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

// alwaysStay 是一个只会调 stay 的假 LLM。
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
