package fsm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// recordingSink 记录收到的消息，用于验证记忆链路入口。
type recordingSink struct{ got []IncomingMessage }

func (r *recordingSink) Accept(m IncomingMessage) { r.got = append(r.got, m) }

// fakeAttention 是一个确定性的 Attention：固定队列 + 游标。
//
// 用它而不是 memory.Store：fsm 不能 import memory（会成环），
// 而这里要测的是"泵入受订阅门控"，不是存储实现。
type fakeAttention struct {
	msgs   []string
	cursor int
	// mentions 是"未读里提到我的条数"，供 @ 相关断言使用。
	mentions int
}

func (f *fakeAttention) Scan(n int) []string {
	if n <= 0 || f.cursor >= len(f.msgs) {
		return nil
	}
	end := f.cursor + n
	if end > len(f.msgs) {
		end = len(f.msgs)
	}
	out := f.msgs[f.cursor:end]
	f.cursor = end
	return out
}

func (f *fakeAttention) Browse(n int) []string { return nil }

func (f *fakeAttention) Unread() int { return len(f.msgs) - f.cursor }

// UnreadMentions 让 fake 可指定"其中几条提到了我"。
//
// 默认 0：大多数测试关心的是"未读条数"，不是 @ 的显眼程度。
// 需要验证 @ 的测试显式设置它。
func (f *fakeAttention) UnreadMentions() int { return f.mentions }

// newAgentWithFakeAttention 造一个接了 fakeAttention 的 agent，
// 队列里预置两条消息。返回的清理函数用于消掉后台独白 goroutine。
func newAgentWithFakeAttention(t *testing.T) (*Agent, func()) {
	t.Helper()
	a := newTestAgentWith(t, Options{
		Name:      "pump",
		States:    MVPStates(),
		Initial:   "idle",
		Attention: &fakeAttention{msgs: []string{"张三: 在吗", "李四: 走了"}},
	})
	return a, func() { a.cancelThinking() }
}

// newTestAgentWith 用给定 Options 构造 agent，自动补上静音 logger。
func newTestAgentWith(t *testing.T, opt Options) *Agent {
	t.Helper()
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a, err := New(opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// stepSync 走一个 tick 并等异步 LLM 调用落地。
//
// 为什么需要它：决策与独白都是异步的（R4），结果以事件回到 agent。
// 生产环境里 Run 的 select 会处理这些事件；而测试直接调 Step，
// 不 drain 就永远看不到结果——状态一动不动，测试却可能照样通过。
// 这里显式等到没有在途调用为止。
func stepSync(t *testing.T, a *Agent, ctx context.Context) {
	t.Helper()
	a.Step(ctx)
	waitIdle(t, a, ctx)
}

// stepUntilDecision 推进 tick 直到发生过一次决策（或超时）。
//
// 决策不会在 MinTick 之前发生，而 MinTick 最小的状态也要 2 tick。
// 只调一次 stepSync 的话，大多数测试会在"还没到决策点"时就去断言
// LastRecord——于是拿到零值记录，报出"reason 为空"这种误导性的失败。
// 这个助手把"等到真的做了决定"这件事显式化。
func stepUntilDecision(t *testing.T, a *Agent, ctx context.Context, maxTicks int) {
	t.Helper()
	before := a.LastRecord().Seq
	beforeReason := a.LastRecord().Reason
	for i := 0; i < maxTicks; i++ {
		stepSync(t, a, ctx)
		rec := a.LastRecord()
		if rec.Reason != "" && (rec.Seq != before || rec.Reason != beforeReason) {
			return
		}
	}
	t.Fatalf("%d tick 内没有发生任何决策（MinTick 之后应当问一次）", maxTicks)
}

// waitIdle 轮询直到没有在途 LLM 调用，并把结果事件全部处理掉。
func waitIdle(t *testing.T, a *Agent, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for a.IsThinking() {
		if time.Now().After(deadline) {
			t.Fatal("LLM 调用未在 3s 内落地（测试挂起）")
		}
		time.Sleep(time.Millisecond)
		a.drainEvents(ctx)
	}
	// 再收一次尾部：调用的结果与 settleThinking 是同一步，
	// 但可能还有别的排队事件（消息等）。
	a.drainEvents(ctx)
}

// mustJSON 把值编码成 JSON，仅用于测试。
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// decidingChatter 是一个会**真的做决定**的假 LLM。
//
// 为什么需要它：状态现在完全由 LLM 驱动，一个只回文本的 fake 会让
// 每次决策都降级成"原样再待一会"，于是任何涉及状态转移的测试都
// 测不到东西（永远停在初始状态，却全绿——最坏的一类测试）。
//
// 它按 enter_state 的 schema 里给出的候选清单**确定性地**挑一个
// （用 seed 做下标），因此既走通了真实决策路径，又保持可复现。
// 独白调用照旧只回文本。
//
// 它会**像一个正常人格那样**响应 Until：prompt 里说"该结束的迹象
// 已经出现了"就换状态，否则留在原地。判据刻意取自 **prompt 文本**
// 而不是直接读 agent 字段——Chat 跑在另一个 goroutine 上，读 agent
// 状态就是跨 goroutine 读写（R1）。
//
// 这不是在测框架，而是让假 LLM 表现得合理：否则精力类测试测的只是
// 假 LLM 的任性（比如没睡饱就离开睡觉，然后吃满半天冷却，精力一路
// 掉到 0）。
type decidingChatter struct {
	seed int64
	// turns 记录每次被调用时是决策还是独白，便于断言"确实问了决策"。
	turns []thinkKind
}

func (d *decidingChatter) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if len(req.Tools) == 0 {
		d.turns = append(d.turns, thinkMonologue)
		return ChatResponse{Text: `{"thought":"随便想想","action":"","intent":""}`}, nil
	}
	d.turns = append(d.turns, thinkDecision)

	// 判据全部取自 **prompt 文本**，不读 agent 字段（R1）。
	//
	// 睡觉是特例：真人不会因为"到点了"就离开一张还没睡热的床，
	// 只有状态说"该结束的迹象出现了"（睡饱了）才起来。若照搬"到点
	// 就走"，agent 会在 MinTick 一到就离开睡觉、吃满半天冷却，
	// 于是精力一路掉到 0 —— 精力类测试会全线失败，而根因只是
	// 假 LLM 演得不像人。
	//
	// 判据必须是【现在】那一行的完整措辞：候选清单里也会出现"睡觉"
	// 两个字，用裸词判断会让 agent 在任何列出睡觉的状态里赖着不走。
	if !strings.Contains(req.Prompt, untilHint) && strings.Contains(req.Prompt, "正在「睡觉」") {
		return ChatResponse{
			Text: "还没睡够",
			ToolCalls: []ToolCall{{
				ID: "c1", Name: toolStay,
				Arguments: json.RawMessage(`{"why":"还没睡够","for_ticks":30}`),
			}},
		}, nil
	}

	// 从 enter_state 的 schema 里读出候选，模拟"看着菜单点菜"。
	names := menuFromTools(req.Tools)
	if len(names) == 0 {
		return ChatResponse{
			Text:      "还是先这样吧",
			ToolCalls: []ToolCall{{ID: "c1", Name: toolStay, Arguments: json.RawMessage(`{"why":"没得选","for_ticks":5}`)}},
		}, nil
	}
	pick := names[int(d.seed)%len(names)]
	d.seed++
	args, _ := json.Marshal(map[string]any{"state": pick, "for_ticks": 5, "why": "想换个事做"})
	return ChatResponse{
		Text: "换个事做吧",
		ToolCalls: []ToolCall{
			{ID: "c1", Name: toolEnterState, Arguments: args},
		},
	}, nil
}

// menuFromTools 从工具声明里取出 enter_state 的 state enum。
func menuFromTools(specs []ToolSpec) []StateName {
	for _, s := range specs {
		if s.Name != toolEnterState {
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
		out := make([]StateName, 0, len(p.Properties.State.Enum))
		for _, n := range p.Properties.State.Enum {
			out = append(out, StateName(n))
		}
		return out
	}
	return nil
}

// newDecidingAgent 造一个用 decidingChatter 驱动的 agent。
func newDecidingAgent(t *testing.T, seed int64) *Agent {
	t.Helper()
	return newTestAgentWith(t, Options{
		Name: "decider", States: MVPStates(), Initial: "idle",
		Chatter: &decidingChatter{seed: seed},
	})
}
