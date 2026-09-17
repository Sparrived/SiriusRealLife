// Package transport 提供 HTTP 路由、SSE 推送与 AMKR WebUI 反代。
//
// 约定见 docs/conventions.md §3（API）与 docs/llm-amkr.md §4（反代）。
package transport

import (
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
	return mux
}

// stateResponse 是状态快照的 JSON 形状。字段 snake_case（conventions §3）。
type stateResponse struct {
	AgentID   string         `json:"agent_id"`
	Tick      int64          `json:"tick"`
	State     string         `json:"state"`
	Mood      moodResponse   `json:"mood"`
	Thinking  bool           `json:"thinking"`
	Deferred  int            `json:"deferred"`
	CallCount int            `json:"call_count"`
	Stream    []streamEntry  `json:"stream"`
	Last      *dispatchEntry `json:"last_dispatch,omitempty"`
}

type moodResponse struct {
	Energy  float64 `json:"energy"`
	Annoyed float64 `json:"annoyed"`
	Curious float64 `json:"curious"`
}

type streamEntry struct {
	Seq   int64  `json:"seq"`
	State string `json:"state"`
	Text  string `json:"text"`
}

type dispatchEntry struct {
	From   string           `json:"from"`
	To     string           `json:"to"`
	Reason string           `json:"reason"`
	Roll   float64          `json:"roll"`
	Total  float64          `json:"total"`
	Cands  []candidateEntry `json:"candidates"`
}

type candidateEntry struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// toResponse 把内部快照转成 API 形状。
func (s *Server) toResponse(snap fsm.Snapshot) stateResponse {
	out := stateResponse{
		AgentID:   s.opt.AgentID,
		Tick:      int64(snap.Now),
		State:     string(snap.Current),
		Thinking:  snap.Thinking,
		Deferred:  snap.Deferred,
		CallCount: snap.CallCount,
		Mood: moodResponse{
			Energy:  snap.Mood.Energy,
			Annoyed: snap.Mood.Annoyed,
			Curious: snap.Mood.Curious,
		},
	}
	for _, e := range snap.Stream {
		out.Stream = append(out.Stream, streamEntry{
			Seq: int64(e.Seq), State: string(e.State), Text: e.Text,
		})
	}
	if snap.Last.To != "" {
		d := dispatchEntry{
			From: string(snap.Last.From), To: string(snap.Last.To),
			Reason: snap.Last.Reason, Roll: snap.Last.Roll, Total: snap.Last.Total,
		}
		for _, c := range snap.Last.Candidates {
			d.Cands = append(d.Cands, candidateEntry{Name: string(c.Name), Weight: c.Weight})
		}
		out.Last = &d
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

	kind := fsm.EventUserMessage
	switch {
	case req.MentionsMe:
		kind = fsm.EventMention
	case req.RepliesMe:
		kind = fsm.EventMention // 回复我同样即时打断（docs/memory.md §2.1）
	}
	payload, _ := json.Marshal(map[string]string{"text": req.Text, "from": req.From})

	select {
	case s.opt.Agent.Events() <- fsm.Event{Kind: kind, Data: payload}:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "kind": string(kind)})
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
