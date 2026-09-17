// Package acceptance 是 roadmap.md §2 验收标准的端到端验证。
//
// 它把真实模块装配起来（fsm + memory + transport），而不是各测各的：
// 单测都过、拼起来失效，是这类项目最常见的问题。装配方式与
// cmd/sirius/main.go 保持一致。
package acceptance

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
	"github.com/Sparrived/SiriusRealLife/internal/memory"
	"github.com/Sparrived/SiriusRealLife/internal/transport"
)

// fakeChatter 是确定性 Chatter（不需要真实 LLM）。
type fakeChatter struct{ reply string }

func (f fakeChatter) Chat(ctx context.Context, _ fsm.ChatRequest) (fsm.ChatResponse, error) {
	return fsm.ChatResponse{Text: f.reply}, nil
}

// blockingChatter 一直阻塞到 release 关闭或被取消，用来模拟慢 LLM。
//
// cancelled 在**被取消**时关闭，让测试能区分"取消"与"正常返回"——
// 只看 IsThinking() 是不行的：抢占会立刻为新状态再发起一次独白，
// 于是在途标记又变成非空。
//
// 用带缓冲的 channel 而不是 sync.Once：Chatter 按值传递（接口约定），
// 值里放 sync.Once 会被 go vet 判为复制锁。
type blockingChatter struct {
	release   chan struct{}
	cancelled chan struct{}
}

func (b blockingChatter) Chat(ctx context.Context, _ fsm.ChatRequest) (fsm.ChatResponse, error) {
	select {
	case <-b.release:
		return fsm.ChatResponse{Text: "ok"}, nil
	case <-ctx.Done():
		if b.cancelled != nil {
			// 非阻塞发送：可能已被取消过一次（同一个 chatter 被复用）。
			select {
			case b.cancelled <- struct{}{}:
			default:
			}
		}
		return fsm.ChatResponse{}, ctx.Err()
	}
}

// logCapture 收集 R6 结构化转移日志。
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, string(p))
	return len(p), nil
}

// transitions 解析出全部 state_transition 日志。
func (c *logCapture) transitions() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, line := range c.lines {
		for _, part := range strings.Split(strings.TrimSpace(line), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(part), &m); err != nil {
				continue
			}
			if m["msg"] == "state_transition" {
				out = append(out, m)
			}
		}
	}
	return out
}

type harness struct {
	agent     *fsm.Agent
	store     *memory.Store
	broadcast *transport.Broadcaster
	server    *httptest.Server
	logs      *logCapture
	logger    *slog.Logger
}

// newHarness 按与 cmd/sirius 相同的方式装配。
//
// initial 显式传入：MVPStates() 的第一个是 scrolling_phone，
// 若默认取第一个，QQ 门控测试会失去意义（它本来就看得见）。
func newHarness(t *testing.T, initial fsm.StateName, chatter fsm.Chatter) *harness {
	t.Helper()
	logs := &logCapture{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))

	store := memory.New(memory.DefaultOptions())
	broadcast := transport.NewBroadcaster()

	agent, err := fsm.New(fsm.Options{
		Name:      "sirius",
		States:    fsm.MVPStates(),
		Seed:      20240101,
		StartTick: 7 * 60, // 07:00
		Initial:   initial,
		Chatter:   chatter,
		Attention: store, // Store 隐式满足 fsm.Attention
		Ticker:    store, // 并驱动记忆的衰减/升格（R8）
		Observe:   broadcast.Publish,
		Logger:    logger,
		// 与 cmd/sirius 一致：消息入记忆层、独白真的被调用、打捞接回 prompt。
		// 缺任何一项，"单测各自通过、装起来整条链路是空的"都不会被发现。
		//
		// 独白不节流（-1）：测试要的是"每次进入都发起"这个确定性，
		// 节流会让断言依赖 tick 的相对位置。
		Sink:           store,
		Monologue:      true,
		MonologueEvery: -1,
		Dredge:         store.DredgeFor(),
	})
	if err != nil {
		t.Fatalf("fsm.New: %v", err)
	}

	srv := transport.NewServer(transport.Options{
		Agent:       agent,
		Broadcaster: broadcast,
		Logger:      logger,
		AgentID:     "sirius",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &harness{
		agent: agent, store: store, broadcast: broadcast,
		server: ts, logs: logs, logger: logger,
	}
}

// TestAcceptance30Ticks 跑 30 tick，核对验收 1/2/4：
// 不自锁、不死循环、同状态不连续进入、每次转移都有完整结构化日志。
func TestAcceptance30Ticks(t *testing.T) {
	h := newHarness(t, "idle", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	prevState := h.agent.Current
	transitions := 0
	for i := 0; i < 30; i++ {
		h.agent.Step(ctx)
		if h.agent.Current != prevState {
			transitions++
			prevState = h.agent.Current
		}
	}

	if want := fsm.Tick(30 + 7*60); h.agent.Now != want {
		t.Fatalf("30 tick 后 Now = %d, 期望 %d", h.agent.Now, want)
	}
	if transitions == 0 {
		t.Fatal("30 tick 内一次状态转移都没有（可能自锁）")
	}

	// 验收 2：转移日志字段齐全（R6）。
	recs := h.logs.transitions()
	if len(recs) == 0 {
		t.Fatal("没有 R6 转移日志")
	}
	for i, r := range recs {
		for _, key := range []string{"from", "to", "reason", "roll", "total", "candidates", "seq"} {
			if _, ok := r[key]; !ok {
				t.Errorf("第 %d 条转移日志缺少 %s: %v", i, key, r)
			}
		}
		if r["from"] == r["to"] {
			t.Errorf("第 %d 条日志 from==to==%v", i, r["to"])
		}
	}

	// 验收 4：同一状态不连续进入两次（R3）——"from==to" 已在日志层排除。
	// 这里再核对意识流有界（R5）。
	if len(h.agent.Stream) > 100 {
		t.Errorf("意识流应有界，实际 %d 条", len(h.agent.Stream))
	}
}

// TestAcceptanceDeterministic 验证 R3：同种子同输入必须可复现。
func TestAcceptanceDeterministic(t *testing.T) {
	run := func() []string {
		h := newHarness(t, "idle", fakeChatter{reply: "嗯"})
		ctx := context.Background()
		var path []string
		for i := 0; i < 200; i++ {
			h.agent.Step(ctx)
			path = append(path, string(h.agent.Current))
		}
		return path
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("两次运行长度不同: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("第 %d 步状态不同: %s vs %s（R3 要求可复现）", i, a[i], b[i])
		}
	}
}

// TestAcceptanceLLMDoesNotBlockTick 验收 3：LLM 调用期间状态机不被阻塞，
// 且仍能收事件、仍能被抢占（R4）。
//
// 走真实路径：独白由 enter() 自动发起（不再手工调 Think），
// 这样测的就是生产代码实际会走的那条路。
func TestAcceptanceLLMDoesNotBlockTick(t *testing.T) {
	release := make(chan struct{})
	cancelled := make(chan struct{}, 4)
	h := newHarness(t, "working", blockingChatter{release: release, cancelled: cancelled})
	ctx := context.Background()

	// 构造时进入初始状态即已发起独白。
	if !h.agent.IsThinking() {
		t.Fatal("进入状态应自动发起独白")
	}

	// 调用在途时连推 5 个 tick：必须立即返回。
	start := time.Now()
	for i := 0; i < 5; i++ {
		h.agent.Step(ctx)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("tick 被 LLM 阻塞: 5 tick 用了 %v", elapsed)
	}

	// 在途期间仍能收事件并被抢占（R3/R4）。
	h.agent.Events() <- fsm.Event{Kind: fsm.EventMention}
	h.agent.Step(ctx)
	if got := h.agent.Current; got != "scrolling_phone" {
		t.Fatalf("调用期间被 @ 应能抢占，实际状态 %s", got)
	}
	// 旧的调用必须已被取消。此时可能有一个**新**的独白在途——
	// 那是在新状态里正常发起的，不是被抢占的那个。
	select {
	case <-cancelled:
		// 旧调用确实收到了取消。
	case <-time.After(500 * time.Millisecond):
		t.Error("抢占后旧的在途调用没有被取消（R4）")
	}

	// 被取消的调用不该在意识流里留下"失败"记录：那不是故障。
	close(release)
	time.Sleep(20 * time.Millisecond)
	h.agent.Step(ctx)
	if got := streamText(h.agent); strings.Contains(got, "没想出来") {
		t.Errorf("被抢占取消的调用不应记为失败:\n%s", got)
	}
}

// TestAcceptanceQQGating 验收 5：不在看 QQ 时消息不进意识流；
// 被 @ 能打断；sleeping 时不打断但有记录。
func TestAcceptanceQQGating(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	// 普通消息：静默入队，不打断。
	h.agent.Events() <- fsm.NewMessageEvent(fsm.IncomingMessage{From: "小明", Text: "秘密内容不该泄露"})
	h.agent.Step(ctx)
	if got := h.agent.Current; got == "scrolling_phone" {
		t.Error("普通消息不应把 agent 拉到看 QQ 状态")
	}

	// 消息内容不该出现在意识流里（不在看 QQ）。
	if got := streamText(h.agent); strings.Contains(got, "秘密内容不该泄露") {
		t.Fatalf("不在看 QQ 时消息内容不应进意识流：%s", got)
	}
	// 但应当留下"有未读"这个事实。
	h.agent.ReadPhone(5) // working 状态：受可见性门控，不读内容
	if got := streamText(h.agent); strings.Contains(got, "秘密内容不该泄露") {
		t.Fatalf("working 状态下 ReadPhone 不应读出内容：%s", got)
	}
	if h.store.Cursor() != 0 {
		t.Error("不可见时不应推进已读游标")
	}

	// 被 @ 打断。
	h.agent.Events() <- fsm.Event{Kind: fsm.EventMention}
	h.agent.Step(ctx)
	if got := h.agent.Current; got != "scrolling_phone" {
		t.Fatalf("被 @ 应打断到 scrolling_phone，实际 %s", got)
	}
	// 这次应当读到内容了（可见性生效，OnEnter 触发读取）。
	if got := streamText(h.agent); !strings.Contains(got, "秘密内容不该泄露") {
		t.Errorf("进入看 QQ 后应能读到消息：%s", got)
	}
}

// TestAcceptanceSleepingDefersMention 验收 5 的另一半：
// sleeping 时 @ 不打断，但留下记录，且事件被延迟而非丢弃。
func TestAcceptanceSleepingDefersMention(t *testing.T) {
	h := newHarness(t, "sleeping", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	before := len(h.agent.Stream)
	h.agent.Events() <- fsm.Event{Kind: fsm.EventMention}
	h.agent.Step(ctx)

	if got := h.agent.Current; got != "sleeping" {
		t.Errorf("sleeping 时不应被 @ 打断，实际 %s", got)
	}
	if len(h.agent.Stream) <= before {
		t.Error("sleeping 时收到 @ 应留下记录（延迟量是人格的一部分）")
	}
	if len(h.agent.Deferred) == 0 {
		t.Error("事件应被延迟而不是丢弃")
	}
}

// TestAcceptanceMemoryDecaysToShadow 验收 6：
// 有记忆沉入 Shadow，且 Shadow 内容不出现在意识流（LLM 读不到）。
func TestAcceptanceMemoryDecaysToShadow(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})

	// 强度 1.0，每 tick 衰减 0.001，阈值 0.1 → 约 900 tick 沉底。
	h.store.WriteStaging(h.store.NewSession(), "很久以前的事", []string{"很久"}, 3, 0)
	if h.store.ShadowLen() != 0 {
		t.Fatal("刚开始不应有 Shadow")
	}
	for i := 1; i <= 1000; i++ {
		h.store.Tick(fsm.Tick(i))
	}
	if h.store.ShadowLen() == 0 {
		t.Fatal("应有记忆沉入 Shadow")
	}

	// 关键：Shadow 内容绝不能出现在意识流里。
	h.agent.Current = "scrolling_phone" // 可见 QQ，确保门控不是"拦"住它的原因
	h.agent.ReadPhone(5)
	if got := streamText(h.agent); strings.Contains(got, "很久以前的事") {
		t.Fatalf("Shadow 内容不应进意识流：%s", got)
	}
	// 但审计接口取得到（存档而非删除）。
	audit := strings.Join(h.store.ShadowAudit(), "\n")
	if !strings.Contains(audit, "很久以前的事") {
		t.Error("审计接口应能读到 Shadow 内容")
	}
}

// TestAcceptanceTickDrivesMemory 验证装配缺口：agent 的 tick 循环
// 真的驱动了记忆层的衰减/遗忘，而不是只靠测试手动调 Store.Tick。
func TestAcceptanceTickDrivesMemory(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	h.store.WriteStaging(h.store.NewSession(), "会过期的事", []string{"过期"}, 3, 0)
	if h.store.ShadowLen() != 0 {
		t.Fatal("刚开始不应有 Shadow")
	}

	// 只推 agent 的 tick，**不**手动调 store.Tick。
	for i := 0; i < 1000; i++ {
		h.agent.Step(ctx)
	}
	if h.store.ShadowLen() == 0 {
		t.Fatal("agent 前进 1000 tick 后记忆应已沉入 Shadow——" +
			"若失败说明 Ticker 没接上，记忆在真实运行中永不遗忘")
	}
}

// TestAcceptanceHTTPIntegration 验证 HTTP 层与 agent 真的接上了。
func TestAcceptanceHTTPIntegration(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		h.agent.Step(ctx)
	}

	resp, err := http.Get(h.server.URL + "/api/v1/agents/sirius")
	if err != nil {
		t.Fatalf("GET 状态: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		State string `json:"state"`
		Tick  int64  `json:"tick"`
		Mood  struct {
			Energy float64 `json:"energy"`
		} `json:"mood"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if got.State != string(h.agent.Current) {
		t.Errorf("HTTP 状态 %q != agent 状态 %q", got.State, h.agent.Current)
	}
	if got.Tick != int64(h.agent.Now) {
		t.Errorf("HTTP tick %d != agent tick %d", got.Tick, h.agent.Now)
	}
	if got.Mood.Energy == 0 {
		t.Error("心境应当被带上")
	}

	// 投递事件后应能被 agent 处理。
	body := strings.NewReader(`{"text":"@我 在吗","mentions_me":true}`)
	resp2, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events", "application/json", body)
	if err != nil {
		t.Fatalf("POST 事件: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("投递事件状态码 = %d", resp2.StatusCode)
	}
	h.agent.Step(ctx)
	if h.agent.Current != "scrolling_phone" {
		t.Errorf("HTTP 投递的 @ 事件应触发抢占，实际 %s", h.agent.Current)
	}
}

// TestAcceptanceSSEStreams 验证 SSE 能把真实状态推出去。
func TestAcceptanceSSEStreams(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	h.agent.Step(context.Background()) // 先产生一条快照

	resp, err := http.Get(h.server.URL + "/api/v1/agents/sirius/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "event: state") {
		t.Errorf("首个 SSE 事件应为 state: %q", string(buf[:n]))
	}
}

// TestAcceptanceRunLoop 验证 Run 主循环真的按 clock 前进并能优雅退出。
func TestAcceptanceRunLoop(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- h.agent.Run(ctx, clock) }()

	start := h.agent.Now
	for i := 0; i < 10; i++ {
		clock <- struct{}{}
	}
	// 等 agent 追上（它是异步消费 clock 的）。
	deadline := time.After(2 * time.Second)
	for h.agent.Now < start+10 {
		select {
		case <-deadline:
			t.Fatalf("Run 未跟上 clock：Now=%d, 期望 %d", h.agent.Now, start+10)
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 Run 未退出")
	}
}

// TestAcceptanceHTTPMessageReachesMemory 验证消息链路端到端：
// POST /events → agent 事件 → memory.Store unread 队列。
//
// 这条测试存在的理由：Ingest 曾经没有任何生产调用方，于是整套记忆
// 机制（unread → 待选区 → 打捞 → 升格 → Shadow）在真实运行中永远是
// 空的，而所有单测照样通过。**必须走 HTTP 入口**，不能直接调
// store.Ingest——否则测的还是那条断掉的链路。
func TestAcceptanceHTTPMessageReachesMemory(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	if got := h.store.UnreadCount(); got != 0 {
		t.Fatalf("起始未读 = %d, 期望 0", got)
	}

	body := `{"kind":"user_message","from":"张三","text":"在吗"}`
	resp, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	// 事件通过 channel 异步进入 agent，等它被消费。
	deadline := time.Now().Add(2 * time.Second)
	for h.store.UnreadCount() == 0 && time.Now().Before(deadline) {
		h.agent.Step(ctx)
		time.Sleep(time.Millisecond)
	}

	// 普通消息静默入队：未读 +1，且不打断当前状态。
	if got := h.store.UnreadCount(); got != 1 {
		t.Fatalf("未读 = %d, 期望 1（消息没有进入记忆层——Sink 没接上）", got)
	}
	if got := h.agent.Current; got == "scrolling_phone" {
		t.Error("普通消息不应把 agent 拉到看 QQ 状态")
	}

	// 再发一条 @我的：必须即时打断。
	body = `{"kind":"mention","from":"张三","text":"在吗","mentions_me":true}`
	resp2, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST events: %v", err)
	}
	defer resp2.Body.Close()

	deadline = time.Now().Add(2 * time.Second)
	for h.agent.Current != "scrolling_phone" && time.Now().Before(deadline) {
		h.agent.Step(ctx)
		time.Sleep(time.Millisecond)
	}
	if got := h.agent.Current; got != "scrolling_phone" {
		t.Fatalf("被 @ 后状态 = %s, 期望 scrolling_phone", got)
	}
	// 进入看 QQ 会读消息，正文这才合法出现——同时反证它确实存进了记忆层。
	if got := streamText(h.agent); !strings.Contains(got, "在吗") {
		t.Errorf("进入看 QQ 后应读到消息正文:\n%s", got)
	}
}

// TestAcceptanceMonologueIsLLMGenerated 验证意识流由 LLM 生成，而不是
// 写死的旁白：进入状态会真的调用 Chatter，结果变成带类型的记录。
//
// 修复前 CallCount 恒为 0——Think 从未被生产代码调用，整个"意识流"
// 只是四条硬编码字符串。跑起来看不出来，因为它照样在动。
func TestAcceptanceMonologueIsLLMGenerated(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "想: 有点无聊\n打算: 去写点东西"})
	ctx := context.Background()

	// 构造时已进入初始状态并发起过一次独白；把它落定。
	settle := func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			h.agent.Step(ctx)
			if !h.agent.IsThinking() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("独白一直没落定")
	}
	settle()

	if got := h.agent.CallCount; got == 0 {
		t.Fatal("CallCount = 0：独白没有真正调用 LLM")
	}
	if got := h.agent.Intent(); got != "去写点东西" {
		t.Fatalf("意图 = %q, 期望来自 LLM 回复", got)
	}

	// 意识流里必须真的出现模型产出的内容，且类型正确。
	var kinds []fsm.Kind
	for _, e := range h.agent.Stream {
		if e.Text == "有点无聊" || e.Text == "去写点东西" {
			kinds = append(kinds, e.Kind)
		}
	}
	if len(kinds) != 2 {
		t.Fatalf("未在意识流中找到 LLM 产出的记录: %+v", h.agent.Stream)
	}
	if kinds[0] != fsm.KindThought || kinds[1] != fsm.KindIntent {
		t.Errorf("类型 = %v, 期望 [thought intent]", kinds)
	}
}

// TestAcceptanceContextCarriesStreamAndIntent 验证 prompt 上下文真的
// 带上了"最近在想/刚发生/打算"，而不是只有当前状态。
func TestAcceptanceContextCarriesStreamAndIntent(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	for i := 0; i < 40; i++ {
		h.agent.Step(ctx)
	}
	h.agent.Events() <- fsm.NewMessageEvent(fsm.IncomingMessage{
		From: "张三", Text: "吃了吗", MentionsMe: true,
	})
	h.agent.Step(ctx)

	got := h.agent.Context(fsm.SiteMonologue, fsm.ContextOptions{})
	for _, want := range []string{"【现在】", "【心境】", "【刚发生】", "【适合做】"} {
		if !strings.Contains(got, want) {
			t.Errorf("上下文缺少 %s:\n%s", want, got)
		}
	}
	// R5：prompt 不得塞全量意识流（上限 100 条）。
	if n := strings.Count(got, "\n"); n > 12 {
		t.Errorf("上下文 %d 行，疑似塞进了全量意识流:\n%s", n, got)
	}
}

// TestAcceptanceMessageContentStaysGated 验证消息正文不会因接线而
// 旁路 QQ 门控：@我 能把 agent 拉到看手机状态，但正文只在那个状态
// 由 ReadPhone 走可见性门控读出。
func TestAcceptanceMessageContentStaysGated(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()
	h.store.WriteStaging(0, "不该被看见的内容", []string{"秘密"}, 5, h.agent.Now)

	h.agent.Events() <- fsm.NewMessageEvent(fsm.IncomingMessage{
		From: "小明", Text: "不该被看见的内容", MentionsMe: true,
	})
	h.agent.Step(ctx)

	// 抢占发生在 ingestMessage 之后，此时状态仍是 working。
	// 事件处理顺序：先把消息写进意识流（不含正文）→ 再抢占进看手机。
	// 进入看手机后 OnEnter 会读消息，正文这才合法出现。
	if got := h.agent.Context(fsm.SiteMonologue, fsm.ContextOptions{}); !strings.Contains(got, "不该被看见的内容") {
		t.Logf("进入看 QQ 后正文未出现（可接受）：\n%s", got)
	}
	// 关键断言：正文只可能来自 KindObservation（受门控的路径），
	// 不能作为"看到消息"以外的形式出现。
	for _, e := range h.agent.Stream {
		if strings.Contains(e.Text, "不该被看见的内容") && e.Kind != fsm.KindObservation {
			t.Errorf("正文以 %v 类型出现，应只经受门控的观察路径", e.Kind)
		}
	}
}

func streamText(a *fsm.Agent) string {
	var b strings.Builder
	for _, e := range a.Stream {
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	return b.String()
}
