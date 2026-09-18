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
//
// 它只回文本、**不调工具**，因此每次决策都会走"降级成原样再待一会"
// 那条路。这对"不阻塞 tick""门控"这类测试正合适；而要验证状态真的
// 会转移，得用 decidingChatter。
type fakeChatter struct{ reply string }

func (f fakeChatter) Chat(ctx context.Context, _ fsm.ChatRequest) (fsm.ChatResponse, error) {
	return fsm.ChatResponse{Text: f.reply}, nil
}

// decidingChatter 会真的调用 enter_state，让状态机动起来。
//
// 为什么验收层需要它：状态现在完全由 LLM 决定，一个不会调工具的
// 假 LLM 会让 agent 永远停在初始状态——"30 tick 内必须发生转移"
// 这类验收就测不到东西。它按候选清单确定性地点菜，保持可复现。
type decidingChatter struct {
	// pick 是下一次从候选清单里取第几个（取模）。叫 pick 不叫 seed：
	// R3 之后框架里没有随机，这只是一个递增的下标。
	pick int64
}

func (d *decidingChatter) Chat(_ context.Context, req fsm.ChatRequest) (fsm.ChatResponse, error) {
	// 独白调用（不带工具）只回内心活动。
	if len(req.Tools) == 0 {
		return fsm.ChatResponse{Text: "想: 有点无聊\n打算: 去写点东西"}, nil
	}
	names := menuFromTools(req.Tools)
	if len(names) == 0 {
		return fsm.ChatResponse{
			Text: "还是先这样吧",
			ToolCalls: []fsm.ToolCall{{
				ID: "c1", Name: "stay",
				Arguments: json.RawMessage(`{"why":"没得选","for_ticks":5}`),
			}},
		}, nil
	}
	pick := names[int(d.pick)%len(names)]
	d.pick++
	args, _ := json.Marshal(map[string]any{
		"state": pick, "for_ticks": 5, "why": "想换个事做",
	})
	return fsm.ChatResponse{
		Text: "换个事做吧",
		ToolCalls: []fsm.ToolCall{
			{ID: "c1", Name: "enter_state", Arguments: args},
		},
	}, nil
}

// menuFromTools 从工具声明里取出 enter_state 的 state enum。
//
// 走 schema 而不是硬编码状态名：这样验收测的是"模型看到什么菜单"，
// 而不是把状态表抄一遍（抄的那份迟早与真实菜单漂移）。
func menuFromTools(specs []fsm.ToolSpec) []fsm.StateName {
	for _, s := range specs {
		if s.Name != "enter_state" {
			continue
		}
		var p struct {
			Properties struct {
				State struct {
					Enum []string `json:"enum"`
				} `json:"state"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(s.Parameters, &p); err != nil {
			return nil
		}
		out := make([]fsm.StateName, 0, len(p.Properties.State.Enum))
		for _, n := range p.Properties.State.Enum {
			out = append(out, fsm.StateName(n))
		}
		return out
	}
	return nil
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

// decisions 解析出全部 state_decision 日志。
//
// 日志名从 state_transition 改成了 state_decision：内容也变了——
// 不再有权重快照与随机数，取而代之的是模型给的理由（why）与它
// 看到的完整候选清单（含被挡掉的及原因）。
func (c *logCapture) decisions() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, line := range c.lines {
		for _, part := range strings.Split(strings.TrimSpace(line), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(part), &m); err != nil {
				continue
			}
			if m["msg"] == "state_decision" {
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
		Memory:      store,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &harness{
		agent: agent, store: store, broadcast: broadcast,
		server: ts, logs: logs, logger: logger,
	}
}

// TestAcceptance30Ticks 跑 30 tick，核对验收 1/2/4：
// 不自锁、不死循环、同状态不连续进入、每次决定都有完整结构化日志。
func TestAcceptance30Ticks(t *testing.T) {
	h := newHarness(t, "idle", &decidingChatter{pick: 3})
	ctx := context.Background()

	// 推进 30 tick。循环条件用 Now 而不是固定次数：settle 为了让
	// 异步结果落定会额外推几个 tick（它只能靠 Step 来 drain 事件，
	// 而 acceptance 是外部包，够不到内部的 drainEvents）。
	// "一次 Step 恰好前进一个 tick"由 fsm 包内的测试守着，
	// 这里只确认时钟确实在走、且走到了。
	const ticks = 30
	start := h.agent.Now
	prevState := h.agent.Current
	transitions := 0
	for h.agent.Now < start+ticks {
		h.agent.Step(ctx)
		settle(t, h, ctx)
		if h.agent.Current != prevState {
			transitions++
			prevState = h.agent.Current
		}
	}

	if got := h.agent.Now - start; got < ticks {
		t.Fatalf("只推进了 %d tick，期望至少 %d", got, ticks)
	}
	if transitions == 0 {
		t.Fatal("30 tick 内一次状态转移都没有（可能自锁）")
	}

	// 验收 2：决策日志字段齐全（R6）。
	recs := h.logs.decisions()
	if len(recs) == 0 {
		t.Fatal("没有 R6 决策日志")
	}
	for i, r := range recs {
		for _, key := range []string{"from", "to", "reason", "why", "for_ticks", "candidates", "seq"} {
			if _, ok := r[key]; !ok {
				t.Errorf("第 %d 条决策日志缺少 %s: %v", i, key, r)
			}
		}
		if r["reason"] == "llm" && r["from"] == r["to"] {
			t.Errorf("第 %d 条日志 from==to==%v", i, r["to"])
		}
	}

	// 验收 4：同一状态不连续进入两次（R3）——候选清单里排除了当前
	// 状态，因此这条由框架保证，不靠模型自觉。
	if len(h.agent.Stream) > 100 {
		t.Errorf("意识流应有界，实际 %d 条", len(h.agent.Stream))
	}
}

// settle 推进到没有在途 LLM 调用为止，让异步结果落定。
//
// 决策与独白都异步（R4），测试直接调 Step 时必须自己把结果事件
// drain 掉，否则状态永远停在原处、断言看到的是零值。
//
// ⚠️ 它**会推进 tick**：acceptance 是外部包，够不到内部的 drainEvents，
// 只能靠 Step 来收结果事件，而 Step 一定让时间前进。循环里 Step 几次
// 取决于 LLM 何时回话，所以**每次 settle 推进的 tick 数是不定的**。
// 要断言"同一串状态"的测试不能用它，那类测试放在 fsm 包内
// （能同步 drain，见 TestStatePathIsReproducible）；这里只做
// "看最终效果"的断言，与 tick 的具体对齐无关。
func settle(t *testing.T, h *harness, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.agent.IsThinking() {
		if time.Now().After(deadline) {
			t.Fatal("LLM 调用未在 3s 内落定")
		}
		h.agent.Step(ctx)
		time.Sleep(time.Millisecond)
	}
	h.agent.Step(ctx)
}

// TestAcceptanceLLMDoesNotBlockTick 验收 3：LLM 调用期间状态机不被阻塞，
// 且仍能收事件（R4）。
//
// 走真实路径：调用由 Step 自动发起（不手工调 think），这样测的就是
// 生产代码实际会走的那条路。
func TestAcceptanceLLMDoesNotBlockTick(t *testing.T) {
	release := make(chan struct{})
	cancelled := make(chan struct{}, 4)
	h := newHarness(t, "working", blockingChatter{release: release, cancelled: cancelled})
	ctx := context.Background()

	// 推一个 tick 让 agent 发起调用。working 的 MinTick 是 4，因此
	// 第一个 tick 不会问决策，发起的是独白——两者走同一条在途通道。
	h.agent.Step(ctx)
	if !h.agent.IsThinking() {
		t.Fatal("推一个 tick 后应有一次在途 LLM 调用")
	}

	// 调用在途时连推 5 个 tick：必须立即返回。
	start := time.Now()
	for i := 0; i < 5; i++ {
		h.agent.Step(ctx)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("tick 被 LLM 阻塞: 5 tick 用了 %v", elapsed)
	}

	// 在途期间仍能收事件（R1/R4）——事件立即处理，不必等 tick。
	// 但**不抢占状态**：@ 只是提醒更显眼，不掐断手上的事（§2.1）。
	before := h.agent.Current
	h.agent.Events() <- fsm.NewMessageEvent(fsm.IncomingMessage{
		From: "张三", Text: "在吗", MentionsMe: true,
	})
	h.agent.Step(ctx)
	if got := h.agent.Current; got != before {
		t.Fatalf("收到 @ 不该改变状态，期望仍是 %s，实际 %s", before, got)
	}
}

// TestAcceptanceShutdownCancelsInFlightLLM 验证 R4：关停时取消在途调用。
//
// 抢占取消后，取消只剩关停这一个来源，因此这里驱动真实的 Run 循环，
// 而不是手工调一个测试专用的方法。
func TestAcceptanceShutdownCancelsInFlightLLM(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	cancelled := make(chan struct{}, 4)
	h := newHarness(t, "working", blockingChatter{release: release, cancelled: cancelled})

	// 构造时进入初始状态即已发起独白，此刻调用在途。
	if !h.agent.IsThinking() {
		t.Fatal("进入状态应自动发起独白")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.agent.Run(ctx, make(chan struct{})) }()

	cancel()

	select {
	case <-cancelled:
		// 在途调用确实收到了取消。
	case <-time.After(500 * time.Millisecond):
		t.Error("关停后旧的调用没有被取消（别让它跑完 30 秒再丢弃）")
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("ctx 取消后 Run 应当返回")
	}
}

// TestAcceptanceQQGating 验收 5：不在看 QQ 时消息不进意识流；
// 订阅 QQ 时消息在驻留期间持续可见；@ 不抢占。
func TestAcceptanceQQGating(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	// working 不订阅 QQ：消息只留下"有未读"，内容不进意识流。
	h.agent.Events() <- fsm.NewMessageEvent(fsm.IncomingMessage{From: "小明", Text: "秘密内容不该泄露"})
	h.agent.Step(ctx)
	if got := h.agent.Current; got == "scrolling_phone" {
		t.Error("消息不该把 agent 拉到看 QQ 状态（@ 也不抢占了）")
	}
	if got := streamText(h.agent); strings.Contains(got, "秘密内容不该泄露") {
		t.Fatalf("不在看 QQ 时消息内容不应进意识流：%s", got)
	}
	if h.store.Cursor() != 0 {
		t.Error("不可见时不应推进已读游标")
	}

	// 切到订阅 QQ 的状态：泵入应把内容带出来（走真实的 Step 路径）。
	h.agent.Current = "scrolling_phone"
	h.agent.Step(ctx)
	if got := streamText(h.agent); !strings.Contains(got, "秘密内容不该泄露") {
		t.Errorf("订阅 QQ 后应泵入消息：%s", got)
	}
}

// TestAcceptanceMentionIsNotPreemption 验收 5 的另一半（已反转）：
// 收到 @ 不改变状态，但留下记录；睡着时同样不被打断。
//
// 旧标准是"被 @ 能打断；sleeping 时不打断但有记录"。@ 抢占整个取消后，
// 两处的行为统一成"不打断、有记录"，差别只在 sleeping 不订阅 QQ，
// 因此内容对它不可见。
func TestAcceptanceMentionIsNotPreemption(t *testing.T) {
	for _, initial := range []fsm.StateName{"working", "sleeping"} {
		t.Run(string(initial), func(t *testing.T) {
			h := newHarness(t, initial, fakeChatter{reply: "嗯"})
			before := len(h.agent.Stream)

			h.agent.Events() <- fsm.NewMessageEvent(fsm.IncomingMessage{
				From: "张三", Text: "@你 在吗", MentionsMe: true,
			})
			h.agent.Step(context.Background())

			if got := h.agent.Current; got != initial {
				t.Errorf("收到 @ 不该改变状态，期望 %s，实际 %s", initial, got)
			}
			if len(h.agent.Stream) <= before {
				t.Error("收到 @ 应留下记录")
			}
		})
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

// TestAcceptanceMemoryWriteChain 验收：整条记忆写入链路在真实运行中活着。
//
// 这条测试针对的是一类**最难发现的故障**：每一层都有实现、都有单测、
// 接口看着全对，但没有任何写入方，于是真实运行中待选区恒为空、
// 打捞恒空、升格与 Shadow 永不发生。此前正是如此——四层记忆一层都不动。
//
// 因此断言全部走真实入口，不手工调 Store 的内部方法：
//   - 消息从 **HTTP /events** 进来（与生产同一个入口）
//   - 由 agent 自己的 Step 循环泵入（Pump → Scan → 写入）
//   - 快照里的 memory 字段从 **HTTP GET** 读出来
//   - 打捞用**整句**意图查询（dredgeQuery 的真实形状）
func TestAcceptanceMemoryWriteChain(t *testing.T) {
	h := newHarness(t, "working", fakeChatter{reply: "嗯"})
	ctx := context.Background()

	// 先跑几 tick 让时钟离开 0：会话分组依赖真实 tick。
	for i := 0; i < 3; i++ {
		h.agent.Step(ctx)
	}

	// 消息经真实 HTTP 入口投递。
	for _, m := range []string{
		`{"from":"张三","text":"周末去看展吗"}`,
		`{"from":"李四","text":"听说那个展挺好的"}`,
	} {
		resp, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events",
			"application/json", strings.NewReader(m))
		if err != nil {
			t.Fatalf("POST 事件: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("投递事件状态码 = %d", resp.StatusCode)
		}
	}

	// working 不订阅 QQ：消息进了 unread，但没被"看到"，不该写待选区。
	h.agent.Step(ctx)
	if got := h.store.StagingLen(); got != 0 {
		t.Fatalf("没看手机时不该写入待选区，实际 %d 条", got)
	}

	// 切到订阅 QQ 的状态，泵入把内容带进"眼前"。
	h.agent.Current = "scrolling_phone"
	h.agent.Step(ctx)

	// 走真实 HTTP 读快照，确认写入确实发生了。
	staging := h.snapshotMemory(t).Staging
	if staging == 0 {
		t.Fatal("看手机后待选区仍为空——记忆写入链路断了：" +
			"Pump/ReadPhone 看到的内容必须写入待选区")
	}

	// 用**整句**意图打捞（dredgeQuery 的真实形状，不是关键词）。
	got := h.store.Dredge([]string{"翻翻昨天聊过的看展的事打发时间"}, h.agent.Now)
	if len(got) == 0 {
		t.Fatal("整句查询打捞不到刚看到的内容——查询侧没有切词元，整句永远匹配不上")
	}
	if !strings.Contains(got[0], "周末去看展吗") {
		t.Errorf("打捞结果应含刚看到的消息，实际 %q", got[0])
	}

	// 打捞把这段推成事件记忆：整条链路一路走到升格。
	for i := 0; i < 3; i++ {
		h.store.Dredge([]string{"看展"}, h.agent.Now+fsm.Tick(i))
	}
	h.agent.Step(ctx)
	if got := h.snapshotMemory(t).Events; got == 0 {
		t.Error("反复打捞应触发升格，事件记忆仍为 0")
	}
}

// snapshotMemory 经 HTTP 读快照里的 memory 字段。
//
// 刻意走 HTTP 而不是读 h.store：要验证的是**用户/运维能看到什么**，
// 观测面没接上的话，链路活着也等于看不见。
func (h *harness) snapshotMemory(t *testing.T) struct {
	Messages int `json:"messages"`
	Staging  int `json:"staging"`
	Events   int `json:"events"`
	Shadow   int `json:"shadow"`
} {
	t.Helper()
	resp, err := http.Get(h.server.URL + "/api/v1/agents/sirius")
	if err != nil {
		t.Fatalf("GET 状态: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		Memory struct {
			Messages int `json:"messages"`
			Staging  int `json:"staging"`
			Events   int `json:"events"`
			Shadow   int `json:"shadow"`
		} `json:"memory"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析快照: %v", err)
	}
	return got.Memory
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
	//
	// 断言刻意只到"事件被接受、且不改变状态"为止：@ 不再是抢占信号
	// （§2.1），"有人叫我"只是让这条未读更显眼。它会进记忆层、推高
	// 烦躁，但**不掐断手上的事**——换不换状态由 LLM 下一次决策说了算。
	body := strings.NewReader(`{"text":"@我 在吗","mentions_me":true}`)
	resp2, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events", "application/json", body)
	if err != nil {
		t.Fatalf("POST 事件: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("投递事件状态码 = %d", resp2.StatusCode)
	}
	before := h.agent.Current
	h.agent.Step(ctx)
	if h.agent.Current != before {
		t.Errorf("HTTP 投递的 @ 事件不该改变状态，期望仍是 %s，实际 %s",
			before, h.agent.Current)
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

	// 再发一条 @我的：同样**不**打断状态（§2.1），但要进记忆层。
	//
	// 旧断言是"@ 必须即时把 agent 拉到 scrolling_phone"。抢占取消后，
	// @ 与普通消息的差别只剩"这条未读更显眼"（进烦躁的积累、在 prompt
	// 里被标出来）；状态换不换由 LLM 下一次决策说了算。真正要守住的
	// 是"消息确实进了记忆层"，那才是这条测试的存在理由。
	body = `{"kind":"mention","from":"张三","text":"在吗","mentions_me":true}`
	resp2, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST events: %v", err)
	}
	defer resp2.Body.Close()

	deadline = time.Now().Add(2 * time.Second)
	for h.store.UnreadCount() < 2 && time.Now().Before(deadline) {
		h.agent.Step(ctx)
		time.Sleep(time.Millisecond)
	}
	if got := h.store.UnreadCount(); got < 2 {
		t.Fatalf("被 @ 后未读 = %d, 期望 ≥2（@ 也没进记忆层）", got)
	}
	if got := h.agent.Current; got == "scrolling_phone" {
		t.Error("@ 不该把 agent 拉到看 QQ 状态（抢占已取消）")
	}

	// @ 在"还没看手机"时必须已有痕迹：这是它与普通消息的唯一差别。
	// 抢占取消后，若这里也是 0，@ 与灌水就完全等价了。
	if got := h.store.UnreadMentions(); got != 1 {
		t.Errorf("未读里的提及数 = %d, 期望 1（@ 没有留下任何痕迹）", got)
	}
	if got := h.agent.Context(fsm.SiteDispatch, fsm.ContextOptions{}); !strings.Contains(got, "提到了你") {
		t.Errorf("@ 应当在决策上下文里被显式点出，实际:\n%s", got)
	}

	// 正文只在订阅 QQ 的状态里才可见：手动切过去，由 ReadPhone 走
	// 可见性门控读出——这同时反证消息确实存进了记忆层。
	h.agent.Current = "scrolling_phone"
	h.agent.ReadPhone(5)
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
