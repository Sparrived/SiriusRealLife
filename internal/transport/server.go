// Package transport 提供 HTTP 路由、SSE 推送与 AMKR WebUI 反代。
//
// 约定见 docs/conventions.md §3（API）与 docs/llm-amkr.md §4（反代）。
package transport

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// Broadcaster 把一个 agent 的快照扇出给所有 SSE 订阅者。
//
// 它只持有**值拷贝**，不接触 agent 内部状态（R1）。慢订阅者被丢弃
// 而不是阻塞广播：一个卡住的浏览器不该拖住 agent。
type Broadcaster struct {
	mu   sync.Mutex
	subs map[int]chan fsm.Snapshot
	next int
	// latest 保留最近一次快照，供新连接立即拿到当前状态。
	latest  fsm.Snapshot
	hasLast bool
	// dropped 统计因慢而丢弃的推送次数，供观测。
	dropped int
}

// NewBroadcaster 构造一个广播器。
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[int]chan fsm.Snapshot{}}
}

// Publish 由 agent 的 goroutine 调用（通过 fsm.Options.Observe）。
func (b *Broadcaster) Publish(s fsm.Snapshot) {
	b.mu.Lock()
	b.latest = s
	b.hasLast = true
	// 在锁内只做非阻塞发送：满了就丢这次的推送。
	for _, ch := range b.subs {
		select {
		case ch <- s:
		default:
			b.dropped++
		}
	}
	b.mu.Unlock()
}

// Dropped 返回被丢弃的推送次数。
func (b *Broadcaster) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Snapshot 返回最近一次快照（HTTP 快照端点用）。
func (b *Broadcaster) Snapshot() (fsm.Snapshot, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.latest, b.hasLast
}

// Subscribers 返回当前订阅者数量。
func (b *Broadcaster) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// subscribe 注册一个订阅者，返回其 channel 与取消函数。
func (b *Broadcaster) subscribe() (<-chan fsm.Snapshot, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	// 带缓冲：容忍短暂的消费滞后。
	ch := make(chan fsm.Snapshot, 16)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, id)
		close(ch)
	}
}

// Options 是 Server 的构造参数。
type Options struct {
	Agent       *fsm.Agent
	Broadcaster *Broadcaster
	Logger      *slog.Logger
	// AgentID 出现在 API 路径里（/api/v1/agents/{id}/...）。
	AgentID string
	// StaticDir 是构建后的前端目录；为空则不挂静态资源。
	StaticDir string
	// AMKR 反代目标。Proxy 为 nil 时不挂 /amkr/。
	Proxy http.Handler
	// Ready 报告 AMKR 是否可用，用于 /api/v1/health。
	Ready func() bool
	// Memory 暴露记忆各层条数，用于快照里的 memory 字段。
	//
	// 结构化接口而非 import memory 包：transport 只需要几个计数，
	// 不该为此依赖记忆层的类型（也避免 memory → fsm ← transport
	// 之间再多一条边）。为 nil 时快照里不出现 memory 字段。
	Memory MemoryCounts
	// AuthUser / AuthPass 非空时对整个服务启用 HTTP Basic 鉴权。
	//
	// 这是 AGENTS.md §4 那条安全线的前提：Sirius 没有自身鉴权之前只能绑
	// 回环，因为 /amkr/ 反代等同于 AMKR 的完整管理权限（能读到上游 key）。
	// 要把它挂到公网（隧道/反代），先补上这一层。
	AuthUser string
	AuthPass string
}

// MemoryCounts 是记忆各层条数的最小接口。
//
// 由 *memory.Store 隐式满足（与 fsm.Attention 同样的手法）。
type MemoryCounts interface {
	MessagesLen() int
	StagingLen() int
	EventsLen() int
	ShadowLen() int
}

// Server 组装全部 HTTP 路由。
type Server struct {
	opt Options
	log *slog.Logger
}

// NewServer 构造 Server。
func NewServer(opt Options) *Server {
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	if opt.AgentID == "" {
		opt.AgentID = "default"
	}
	return &Server{opt: opt, log: log}
}

// Handler 返回根 handler。
//
// 用 Go 1.22 起的 ServeMux 路由模式，不引第三方路由库（conventions §1）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	base := "/api/v1/agents/" + s.opt.AgentID

	mux.HandleFunc("GET "+base, s.handleState)
	mux.HandleFunc("GET "+base+"/stream", s.handleStream)
	mux.HandleFunc("POST "+base+"/events", s.handleEvent)
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	if s.opt.StaticDir != "" {
		// 刻意**不**用 "GET /"：那会与 "/amkr/" 冲突 —— Go 1.22 的 ServeMux
		// 判定「GET / 方法更具体」而「/amkr/ 路径更具体」，两者都不比对方
		// 更具体，于是注册时直接 panic。用不带方法的 "/" 让它退化为
		// "路径更具体的 /amkr/ 胜出"，语义正确且不冲突。
		//
		// ServeMux 会自动把 /amkr 重定向到 /amkr/（子树模式的既有行为）。
		mux.Handle("/", http.FileServer(http.Dir(s.opt.StaticDir)))
	}
	if s.opt.Proxy != nil {
		// 路径 1:1 透传，不改写任何段（docs/llm-amkr.md §4）。
		mux.Handle("/amkr/", s.opt.Proxy)
	}
	if s.opt.AuthUser == "" || s.opt.AuthPass == "" {
		return mux
	}
	return s.requireAuth(mux)
}

// requireAuth 给整个 handler 套上 HTTP Basic 鉴权。
//
// **包住全部路由**，包括 /amkr/：那个反代等同于 AMKR 的完整管理权限，
// 是这条隧道最需要挡住的东西。健康检查例外——它只报"AMKR 通不通"，
// 不含任何私人内容，留空可以让监控与容器探针不带凭据。
//
// 用标准库的 subtle.ConstantTimeCompare 而不是 ==：比较凭据时不要把
// 长度/前缀差异泄漏成时间差。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	user := []byte(s.opt.AuthUser)
	pass := []byte(s.opt.AuthPass)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		// 两个比较都要做，不能短路——短路会让"用户名对不对"变成时间差。
		userOK := subtle.ConstantTimeCompare([]byte(u), user) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), pass) == 1
		if !ok || !userOK || !passOK {
			// Realm 用中文：浏览器弹出的登录框会直接显示它。
			w.Header().Set("WWW-Authenticate", `Basic realm="Sirius", charset="UTF-8"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// stateResponse 是状态快照的 JSON 形状。字段 snake_case（conventions §3）。
type stateResponse struct {
	AgentID   string         `json:"agent_id"`
	Tick      int64          `json:"tick"`
	State     string         `json:"state"`
	Mood      moodResponse   `json:"mood"`
	Thinking  bool           `json:"thinking"`
	CallCount int            `json:"call_count"`
	Stream    []streamEntry  `json:"stream"`
	Last      *dispatchEntry `json:"last_dispatch,omitempty"`
	// Memory 是记忆各层条数。
	//
	// 加它是因为这条链路出过一次最难发现的故障：四层记忆都有实现、
	// 都有单测，但没有任何写入方，于是 staging 恒为 0、打捞恒空、
	// 升格与 Shadow 永不发生——接口看着全对，实际一层都不动。
	// 观测面必须能直接回答"待选区到底有没有东西"。
	Memory *memoryResponse `json:"memory,omitempty"`
}

// memoryResponse 是记忆各层条数（§3）。
type memoryResponse struct {
	// Messages 是 unread 队列条数（含已读，§2.2）。
	Messages int `json:"messages"`
	// Staging 是待选区条数。**长期为 0 就是故障**：看手机时看到的
	// 内容应当持续写入这里，它为空意味着写入链路断了。
	Staging int `json:"staging"`
	Events  int `json:"events"`
	// Shadow 是终态存档条数（LLM 不可读，§3.1）。
	Shadow int `json:"shadow"`
}

type moodResponse struct {
	Energy  float64 `json:"energy"`
	Annoyed float64 `json:"annoyed"`
	Curious float64 `json:"curious"`
}

type streamEntry struct {
	Seq   int64  `json:"seq"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	Text  string `json:"text"`
}

type dispatchEntry struct {
	From     string           `json:"from"`
	To       string           `json:"to"`
	Reason   string           `json:"reason"`
	Why      string           `json:"why"`
	ForTicks int64            `json:"for_ticks"`
	Cands    []candidateEntry `json:"candidates"`
}

type candidateEntry struct {
	Name string `json:"name"`
	// Blocked 为空表示它是候选；非空是它没进候选的原因。
	//
	// 前端据此把"没被选中"和"根本不能选"分开显示——这个区别是
	// 排障时最常问的问题（"为什么她从来不去睡觉？"）。
	Blocked string `json:"blocked,omitempty"`
}

// toResponse 把内部快照转成 API 形状。
func (s *Server) toResponse(snap fsm.Snapshot) stateResponse {
	out := stateResponse{
		AgentID:   s.opt.AgentID,
		Tick:      int64(snap.Now),
		State:     string(snap.Current),
		Thinking:  snap.Thinking,
		CallCount: snap.CallCount,
		Mood: moodResponse{
			Energy:  snap.Mood.Energy,
			Annoyed: snap.Mood.Annoyed,
			Curious: snap.Mood.Curious,
		},
	}
	for _, e := range snap.Stream {
		out.Stream = append(out.Stream, streamEntry{
			Seq: int64(e.Seq), Kind: e.Kind.String(), State: string(e.State), Text: e.Text,
		})
	}
	if snap.Last.To != "" {
		d := dispatchEntry{
			From: string(snap.Last.From), To: string(snap.Last.To),
			Reason: snap.Last.Reason, Why: snap.Last.Why, ForTicks: int64(snap.Last.ForTicks),
		}
		for _, c := range snap.Last.Candidates {
			d.Cands = append(d.Cands, candidateEntry{Name: string(c.Name), Blocked: c.Blocked})
		}
		out.Last = &d
	}
	if m := s.opt.Memory; m != nil {
		out.Memory = &memoryResponse{
			Messages: m.MessagesLen(),
			Staging:  m.StagingLen(),
			Events:   m.EventsLen(),
			Shadow:   m.ShadowLen(),
		}
	}
	return out
}

// handleState 返回当前状态快照。
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.opt.Broadcaster.Snapshot()
	if !ok {
		// 还没跑过 tick：返回空快照而不是 404，前端不必特判。
		writeJSON(w, http.StatusOK, stateResponse{AgentID: s.opt.AgentID})
		return
	}
	writeJSON(w, http.StatusOK, s.toResponse(snap))
}

// handleStream 是 SSE 实时流（conventions §3：用 SSE，不上 WebSocket）。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "该连接不支持流式推送")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 关掉可能存在的缓冲代理，否则事件会被攒着不发。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.opt.Broadcaster.subscribe()
	defer cancel()

	// 先补一条当前状态，前端不必等下一次 tick。
	if snap, ok := s.opt.Broadcaster.Snapshot(); ok {
		if err := writeSSE(w, "state", s.toResponse(snap)); err != nil {
			return
		}
		flusher.Flush()
	}

	// 心跳：防止中间代理把空闲连接掐掉。
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	// 状态行变化时额外推一条 transition 事件，前端状态图据此高亮。
	var lastState fsm.StateName

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case snap, ok := <-ch:
			if !ok {
				return
			}
			event := "state"
			if lastState != "" && snap.Current != lastState {
				event = "transition"
			}
			lastState = snap.Current
			if err := writeSSE(w, event, s.toResponse(snap)); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// eventRequest 是 POST /events 的请求体。字段 snake_case。
type eventRequest struct {
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	From       string `json:"from"`
	MentionsMe bool   `json:"mentions_me"`
	RepliesMe  bool   `json:"replies_to_me"`
}

// handleEvent 把外部消息投递进 agent（R1：只能投事件，不能直接改状态）。
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	var req eventRequest
	// 限制请求体大小，避免被撑爆。
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "empty_text", "text 不能为空")
		return
	}

	// @我 / 回复我不另立事件类型：它们只是负载里的标记，决定提示文案
	// 的显眼程度与队列重要性，不决定抢占（docs/memory.md §2.1）。
	// 负载由 fsm 侧构造（R1）：字段名只在那边定义一次，
	// 加字段时不会漏改这里。
	ev := fsm.NewMessageEvent(fsm.IncomingMessage{
		From:        req.From,
		Text:        req.Text,
		MentionsMe:  req.MentionsMe,
		RepliesToMe: req.RepliesMe,
	})

	select {
	case s.opt.Agent.Events() <- ev:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "kind": string(ev.Kind)})
	case <-r.Context().Done():
		return
	case <-time.After(2 * time.Second):
		// 通道满说明 agent 卡住了，如实报 503 而不是无限等。
		writeError(w, http.StatusServiceUnavailable, "agent_busy", "事件通道已满，agent 可能卡住")
	}
}

// handleHealth 报告服务与 AMKR 的可用性。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	amkr := true
	if s.opt.Ready != nil {
		amkr = s.opt.Ready()
	}
	body := map[string]any{
		"status":      "ok",
		"amkr":        map[string]any{"ready": amkr},
		"subscribers": s.opt.Broadcaster.Subscribers(),
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 按 conventions §3 的错误形状返回。
func writeError(w http.ResponseWriter, code int, kind, detail string) {
	writeJSON(w, code, map[string]string{"error": kind, "detail": detail})
}

// writeSSE 写一条 SSE 消息。
func writeSSE(w http.ResponseWriter, event string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// SSE 的 data 不能含裸换行，JSON 已转义，直接写一行。
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	return err
}
