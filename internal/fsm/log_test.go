package fsm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// decisionRecord 是 R6 日志的一条解析结果。
//
// 与旧版的区别：**没有 roll/total**（不再有抽取），多了 why/for_ticks。
// 复盘"为什么换了这个状态"的依据从"随机数落在哪"变成了"模型说了什么"。
type decisionRecord struct {
	Msg      string
	From     string
	To       string
	Reason   string
	Why      string
	ForTicks int64
	Seq      int64
	Cands    []string
	// Blocked 是被挡掉的状态名（Blocked 字段非空的那些）。
	Blocked []string
}

// captureLogs 跑 n 个 tick 并收集结构化决策日志。
func captureLogs(t *testing.T, n int) []decisionRecord {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	a, err := New(Options{
		Name: "test", States: MVPStates(),
		Chatter: &decidingChatter{seed: 20240101},
		Logger:  logger, Initial: "idle",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < n; i++ {
		stepSync(t, a, ctx)
	}

	var out []decisionRecord
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("日志不是合法 JSON: %q: %v", line, err)
		}
		if str(raw["msg"]) != "state_decision" {
			continue // 这条测试只关心决策日志
		}
		rec := decisionRecord{
			Msg:      str(raw["msg"]),
			From:     str(raw["from"]),
			To:       str(raw["to"]),
			Reason:   str(raw["reason"]),
			Why:      str(raw["why"]),
			ForTicks: int64(num(raw["for_ticks"])),
			Seq:      int64(num(raw["seq"])),
		}
		arr, ok := raw["candidates"].([]any)
		if !ok {
			t.Fatalf("candidates 不是数组: %T", raw["candidates"])
		}
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := str(m["Name"])
			rec.Cands = append(rec.Cands, name)
			if str(m["Blocked"]) != "" {
				rec.Blocked = append(rec.Blocked, name)
			}
		}
		out = append(out, rec)
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// TestDecisionLogFields 验证 R6：每个状态决定都留下完整的可复盘记录。
//
// R6 的**内容**已经变了。旧要求是"当时候选状态的权重快照 + 随机数"；
// 现在没有权重也没有随机，但要求更严了：必须留下**模型给的理由**
// （why）与**它看到的选择**（含被挡掉的及原因）。理由不落盘，事后就
// 只剩"状态跳了"这一个事实，无从复盘。
func TestDecisionLogFields(t *testing.T) {
	recs := captureLogs(t, 60)
	if len(recs) == 0 {
		t.Fatal("60 tick 内没有任何决策日志")
	}
	for i, r := range recs {
		if r.From == "" || r.To == "" {
			t.Errorf("第 %d 条日志缺少 from/to: %+v", i, r)
		}
		if r.From == r.To && r.Reason == "llm" {
			t.Errorf("第 %d 条日志 from==to==%s（LLM 决策不该自我转移）", i, r.To)
		}
		if r.Reason == "" {
			t.Errorf("第 %d 条日志缺少 reason", i)
		}
		if r.Why == "" {
			t.Errorf("第 %d 条日志缺少 why（R6：没有理由就无法复盘）", i)
		}
		if r.ForTicks <= 0 {
			t.Errorf("第 %d 条日志 for_ticks = %d, 应大于 0", i, r.ForTicks)
		}
		if len(r.Cands) == 0 {
			t.Errorf("第 %d 条日志没有候选快照", i)
		}
		// LLM 做出的转移，目标必须在**可选项**里（被挡掉的不算）。
		if r.Reason == "llm" {
			var ok bool
			for _, c := range r.Cands {
				if c == r.To {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("第 %d 条日志选中的 %s 不在候选 %v 中", i, r.To, r.Cands)
			}
			for _, b := range r.Blocked {
				if b == r.To {
					t.Errorf("第 %d 条日志选中了被挡掉的状态 %s", i, r.To)
				}
			}
		}
	}
}

// TestDecisionLogHasCandidateSnapshot 验证候选清单真的落进日志。
//
// 这是排障时唯一能回答"她当时有哪些选择"的东西。特别地，被挡掉的
// 状态与原因也必须记——"为什么她从来不去睡觉"的答案通常就在
// Blocked 里（当时不困），而不是在选中项里。
func TestDecisionLogHasCandidateSnapshot(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	a, err := New(Options{
		Name: "test", States: MVPStates(),
		Chatter: &decidingChatter{seed: 7},
		Logger:  logger, Initial: "idle",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		stepSync(t, a, ctx)
	}

	logs := buf.String()
	if !strings.Contains(logs, `"candidates"`) {
		t.Fatal("决策日志里没有 candidates 快照（R6 要求候选清单）")
	}
	if !strings.Contains(logs, `"Blocked"`) {
		t.Fatal("candidates 里缺少 Blocked 字段：无法复盘'当时为什么不能选它'")
	}
	if !strings.Contains(logs, `"why"`) {
		t.Fatal("决策日志里没有 why（R6 要求留下模型的理由）")
	}
}

// TestStayKeepsState 验证 stay 不改变状态，且重设检查点。
//
// 这是"LLM 可以选择什么都不做"的落点：模型说"再等等"，框架就真的
// 只是把下次询问推后，而不是随便挑一个状态打发过去。
func TestStayKeepsState(t *testing.T) {
	a := newTestAgentWith(t, Options{
		Name: "stay", States: MVPStates(), Initial: "idle",
		Chatter: alwaysStay{},
	})
	ctx := context.Background()
	start := a.Current

	for i := 0; i < 40; i++ {
		stepSync(t, a, ctx)
		if a.Current != start {
			t.Fatalf("tick %d: stay 却换了状态 %s → %s", i, start, a.Current)
		}
	}
	rec := a.LastRecord()
	if rec.Reason != "stay" {
		t.Errorf("reason = %q, 期望 stay", rec.Reason)
	}
	if rec.From != start || rec.To != start {
		t.Errorf("stay 的 from/to 都应是 %s，实际 %s → %s", start, rec.From, rec.To)
	}
	if rec.ForTicks <= 0 {
		t.Errorf("stay 应重设检查点，for_ticks = %d", rec.ForTicks)
	}
}

// TestConditionTriggersFireOnce 验证条件型理由只在**上升沿**报一次。
//
// 这条守的是一个线上实测到的真实缺陷：Until 和 MaxTick 是**条件**，
// 一旦成立就持续成立（"刷够 8 tick 了"不会自己变回去），于是 due()
// 每 tick 都重新报一次，决策点连轴转——模型填的 for_ticks 完全失效。
//
// 实测数据：85 次决策的相邻间隔中位数是 **3**，而模型填的是 for_ticks=12。
// 每次询问都是一次真金白银的 LLM 调用，且人格被反复打断，难以连贯做事。
//
// 直接测 due() 而不是跑端到端：端到端会被 clampTicks 干扰（stay 的
// for_ticks 被夹到 MaxTick，于是 dwell 合法地每 MaxTick 触发一次），
// 那是**另一条**正确行为，混在一起就测不清"条件重复上报"这一条。
func TestConditionTriggersFireOnce(t *testing.T) {
	count := func(kind TriggerKind, ticks int) int {
		a := newTestAgent(t, fakeChatter{})
		// working：Until 看精力（< 30），MaxTick=12。
		a.Current = "working"
		a.enteredAt = a.Now
		// 检查点放在过去：dwell 因此**持续**成立，正好用来验证它也被去重。
		a.decideAt = a.Now
		a.Mood.Energy = 10 // Until 恒真

		n := 0
		for i := 0; i < ticks; i++ {
			a.Now++
			for _, tr := range a.due() {
				if tr.Kind == kind {
					n++
				}
			}
		}
		return n
	}

	// 跑 50 tick（远超 MaxTick=12），三个条件都只该报一次。
	// dwell 是这里最容易漏的一个：decideAt 要等 commit 才推进，而一次
	// 决策要跨好几个 tick，所以它同样会逐 tick 重复成立。
	for _, tc := range []struct {
		kind TriggerKind
		name string
	}{
		{TriggerDwell, "检查点"},
		{TriggerUntil, "Until"},
		{TriggerMax, "MaxTick"},
	} {
		if got := count(tc.kind, 50); got != 1 {
			t.Errorf("%s 在条件持续成立时报了 %d 次，期望 1 次（上升沿）", tc.name, got)
		}
	}
}

// TestDecisionCadenceRespectsForTicks 验证决策间隔真的尊重 for_ticks。
//
// 这是上一条的端到端版本，也是**本来该在 CI 里拦住那个线上缺陷**的测试：
// 只测 due() 的上升沿还不够——只要有人把 due() 的返回直接当"要不要问"
// 用、或把 pending 清早了，线上仍会退化成每 3 tick 问一次。
//
// 做法：让假 LLM 每次都填一个明确的 for_ticks，然后数一段时间里到底
// 被问了几次。
//
// ⚠️ 期望值要按**夹紧后**的 for_ticks 算，不是按 want 本身：框架会把
// for_ticks 夹进 [MinTick, MaxTick]，所以模型说"20 tick 后再问"，若该
// 状态的 MaxTick 只有 12，实际就是 12。第一版测试没考虑这点，算出
// "200 tick 问了 34 次、超过上界 20"而误报失败——那是**正确行为**
// 被测试写错了。
func TestDecisionCadenceRespectsForTicks(t *testing.T) {
	const want = Tick(20)
	const span = Tick(400)

	// 用 working（MinTick=4、MaxTick=12）：它的 Until 看精力，而默认
	// 精力充足时恒假，因此询问频率**只**由夹紧后的 for_ticks 决定，
	// 不会被条件型入口干扰。
	var eff = want
	for _, s := range MVPStates() {
		if s.Name == "working" && eff > s.MaxTick {
			eff = s.MaxTick
		}
	}
	if eff == want {
		t.Fatalf("预期 working 的 MaxTick 会夹紧 for_ticks=%d，但它没有（eff=%d）", want, eff)
	}

	ch := &cadenceChatter{want: want}
	a := newTestAgentWith(t, Options{
		Name: "cadence", States: MVPStates(), Initial: "working",
		Chatter: ch,
	})
	ctx := context.Background()

	start := a.Now
	for a.Now < start+span {
		stepSync(t, a, ctx)
	}

	ideal := int(span / eff)
	got := ch.decisions
	// 给 2 倍余量（首次进入、夹紧边界取整都会少问几次）。缺陷版本是
	// **每 tick** 问一次（~span 次，约 33 倍），2 倍余量足以区分。
	if got > ideal*2 {
		t.Errorf("%d tick 内问了 %d 次决策，理想值 %d（for_ticks 夹紧后为 %d），"+
			"超过 2 倍上界 —— 条件型理由被重复上报了（线上实测过这个缺陷）",
			span, got, ideal, eff)
	}
	if got < ideal/2 {
		t.Errorf("%d tick 内只问了 %d 次决策，少于理想值 %d 的一半 —— 检查点可能没被正确推进",
			span, got, ideal)
	}
}

// cadenceChatter 每次都调 stay 并填固定 for_ticks，用于观察决策频率。
type cadenceChatter struct {
	want      Tick
	decisions int
}

func (c *cadenceChatter) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if len(req.Tools) == 0 {
		return ChatResponse{Text: `{"thought":"嗯","action":"","intent":""}`}, nil
	}
	c.decisions++
	args, _ := json.Marshal(map[string]any{"why": "再待一会", "for_ticks": c.want})
	return ChatResponse{
		Text:      "再待一会",
		ToolCalls: []ToolCall{{ID: "c1", Name: toolStay, Arguments: args}},
	}, nil
}

// 只报一次的前提是"条件真的持续成立"。若她中途做了别的事让条件不再
// 成立（这里是把精力补回去），之后再成立时必须还能提醒她——否则
// 一次提醒之后就永久静音，Until 就废了。
func TestUntilRearmsAfterConditionClears(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	a.Current = "working"
	// 越过 MinTick（working 是 4）：否则 due() 一律返回空，测不到东西。
	a.enteredAt = a.Now - 5
	a.decideAt = a.Now + 1000 // 检查点放远，只让 Until 参与

	a.Mood.Energy = 10
	a.Now++
	if len(a.due()) == 0 {
		t.Fatal("条件首次成立时应产出理由")
	}

	// 条件不再成立：due() 应重新武装。
	a.Mood.Energy = 80
	a.Now++
	a.due()
	// 再次成立：应再报一次。
	a.Mood.Energy = 10
	a.Now++
	if len(a.due()) == 0 {
		t.Error("条件重新成立后应再次提醒（否则 Until 一次提醒后永久静音）")
	}
}

type alwaysStay struct{}

func (alwaysStay) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if len(req.Tools) == 0 {
		return ChatResponse{Text: `{"thought":"再待会","action":"","intent":""}`}, nil
	}
	return ChatResponse{
		Text: "还不着急",
		ToolCalls: []ToolCall{{
			ID: "c1", Name: toolStay,
			Arguments: json.RawMessage(`{"why":"手上的事还没做完","for_ticks":4}`),
		}},
	}, nil
}

// TestInvalidChoiceFallsBackToStay 验证模型选了不可用的状态时**不照做**。
//
// 这是框架的兜底：路由忽略 schema 时模型可能回一个不在候选里的名字，
// 或者回一个此刻被 Guard/冷却挡住的状态。那时必须降级成"原样再待一会"
// 并记日志，**绝不能**盲目执行——否则 Guard 就成了摆设，人格会在
// 不该睡觉的时候睡着。
func TestInvalidChoiceFallsBackToStay(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"状态不存在", `{"state":"teleporting","for_ticks":5,"why":"随便"}`},
		{"当前状态（应在菜单外）", `{"state":"idle","for_ticks":5,"why":"继续发呆"}`},
		{"冷却中的状态", `{"state":"working","for_ticks":5,"why":"接着干活"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAgentWith(t, Options{
				Name: "bad", States: MVPStates(), Initial: "idle",
				Chatter: fixedCall{name: toolEnterState, args: tc.args},
			})
			// working 处于冷却中，覆盖第三种情况。
			a.cooldownUntil["working"] = a.Now + 1000
			ctx := context.Background()

			stepUntilDecision(t, a, ctx, 20)

			if a.Current != "idle" {
				t.Fatalf("不可用的选择不该被执行，状态变成了 %s", a.Current)
			}
			if got := a.LastRecord().Reason; got != "stay" {
				t.Errorf("降级后 reason = %q, 期望 stay", got)
			}
		})
	}
}

// fixedCall 是一个固定回同一个工具调用的假 LLM。
type fixedCall struct {
	name string
	args string
}

func (f fixedCall) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if len(req.Tools) == 0 {
		return ChatResponse{Text: `{"thought":"嗯","action":"","intent":""}`}, nil
	}
	return ChatResponse{
		Text:      "试试",
		ToolCalls: []ToolCall{{ID: "c1", Name: f.name, Arguments: json.RawMessage(f.args)}},
	}, nil
}

// TestNoToolCallFallsBackToStay 验证模型只回散文时降级而不是崩掉。
//
// 这条路径**一定会发生**：实测 wb2api 静默忽略 tools，永远只回散文。
// 那时若不降级，这次询问就永远丢了——没人再问，人格卡死在状态里。
// 降级也不能是"随机挑一个状态"（那是被删掉的老毛病）。
func TestNoToolCallFallsBackToStay(t *testing.T) {
	a := newTestAgentWith(t, Options{
		Name: "prose", States: MVPStates(), Initial: "idle",
		Chatter: fakeChatter{reply: "我觉得今天天气不错"},
	})
	ctx := context.Background()

	stepUntilDecision(t, a, ctx, 20)

	if a.Current != "idle" {
		t.Fatalf("只回散文时不该换状态，实际 %s", a.Current)
	}
	rec := a.LastRecord()
	if rec.Reason != "stay" {
		t.Errorf("降级后 reason = %q, 期望 stay", rec.Reason)
	}
	if rec.ForTicks != defaultStayTick {
		t.Errorf("降级应再待 defaultStayTick=%d，实际 %d", defaultStayTick, rec.ForTicks)
	}
}

// TestSleepIsNotChronic 验证睡眠频率是个"作息"而不是高频动作。
func TestSleepIsNotChronic(t *testing.T) {
	a := newDecidingAgent(t, 20240101)
	ctx := context.Background()

	sleeps := 0
	wasSleeping := a.Current == "sleeping"
	for i := 0; i < int(TicksPerDay); i++ {
		stepSync(t, a, ctx)
		nowSleeping := a.Current == "sleeping"
		if nowSleeping && !wasSleeping {
			sleeps++
		}
		wasSleeping = nowSleeping
	}

	// 一天最多睡 3 次（正常应 1–2 次；留出余量避免脆弱）。
	if sleeps > 3 {
		t.Errorf("一个游戏日内睡了 %d 次，不像作息（精力衰减可能过快）", sleeps)
	}
	// 注意：这里**刻意不断言** sleeps > 0。睡觉现在是 LLM 的选择，
	// 一个假 LLM 完全可能一天都不选它——那不代表框架坏了。
	// 旧版本这条断言依赖随机抽取必然踩到 sleeping，已经失去意义。
}
