package fsm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// transitionRecord 是 R6 日志的一条解析结果。
type transitionRecord struct {
	Msg    string
	From   string
	To     string
	Reason string
	Seq    int64
	Roll   float64
	Total  float64
	Cands  []string
}

// captureLogs 跑 n 个 tick 并收集结构化转移日志。
func captureLogs(t *testing.T, seed int64, n int) []transitionRecord {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	a, err := New(Options{
		Name: "test", States: MVPStates(), Seed: seed,
		Chatter: fakeChatter{}, Logger: logger, Initial: "idle",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < n; i++ {
		a.Step(ctx)
	}

	var out []transitionRecord
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("日志不是合法 JSON: %q: %v", line, err)
		}
		rec := transitionRecord{
			Msg:    str(raw["msg"]),
			From:   str(raw["from"]),
			To:     str(raw["to"]),
			Reason: str(raw["reason"]),
			Seq:    int64(num(raw["seq"])),
			Roll:   num(raw["roll"]),
			Total:  num(raw["total"]),
		}
		// candidates 是数组，逐项取出名字，供 TestTransitionLogFields 使用。
		if arr, ok := raw["candidates"].([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					rec.Cands = append(rec.Cands, str(m["Name"]))
				}
			}
		} else {
			t.Fatalf("candidates 不是数组: %T", raw["candidates"])
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

// TestTransitionLogFields 验证 R6：每次转移都留下 from/to/reason + 权重快照 + 随机值。
func TestTransitionLogFields(t *testing.T) {
	recs := captureLogs(t, 20240101, 60)
	if len(recs) == 0 {
		t.Fatal("60 tick 内没有任何转移日志")
	}
	for i, r := range recs {
		if r.Msg != "state_transition" {
			t.Errorf("第 %d 条日志 msg = %q, 期望 state_transition", i, r.Msg)
		}
		if r.From == "" || r.To == "" {
			t.Errorf("第 %d 条日志缺少 from/to: %+v", i, r)
		}
		if r.From == r.To {
			t.Errorf("第 %d 条日志 from==to==%s", i, r.To)
		}
		if r.Reason == "" {
			t.Errorf("第 %d 条日志缺少 reason", i)
		}
		if r.Total <= 0 {
			t.Errorf("第 %d 条日志 total = %v, 应大于 0", i, r.Total)
		}
		if r.Roll < 0 || r.Roll >= r.Total {
			t.Errorf("第 %d 条日志 roll = %v 越界 [0,%v)", i, r.Roll, r.Total)
		}
		if len(r.Cands) == 0 {
			t.Errorf("第 %d 条日志没有候选项快照", i)
		}
		// 选中的状态必须在候选快照里，否则日志无法复盘抽取过程。
		var found bool
		for _, c := range r.Cands {
			if c == r.To {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("第 %d 条日志选中的 %s 不在候选 %v 中", i, r.To, r.Cands)
		}
	}
}

// TestTransitionLogHasCandidateSnapshot 验证权重快照确实落进了日志。
//
// R6 要求"当时候选状态的权重快照"——这是出问题时唯一能复盘
// "为什么选了这个状态"的东西，不能只有 from/to。
func TestTransitionLogHasCandidateSnapshot(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	a, err := New(Options{
		Name: "test", States: MVPStates(), Seed: 20240101,
		Chatter: fakeChatter{}, Logger: logger, Initial: "idle",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		a.Step(ctx)
	}

	logs := buf.String()
	// candidates 是 JSON 数组，元素含 name/weight。
	if !strings.Contains(logs, `"candidates"`) {
		t.Fatal("转移日志里没有 candidates 快照（R6 要求权重快照）")
	}
	if !strings.Contains(logs, `"Name"`) || !strings.Contains(logs, `"Weight"`) {
		t.Fatal("candidates 快照里缺少 Name/Weight 字段（R6 要求权重快照）")
	}
}

// TestSleepIsNotChronic 验证睡眠频率是个"作息"而不是高频动作。
//
// 上一条只保证可达；这条盯住实际频率：一个游戏日内睡觉次数应有上限。
// 若精力衰减被调得过快（曾经是 0.2/tick），这条会失败。
func TestSleepIsNotChronic(t *testing.T) {
	a := newTestAgent(t, 20240101, fakeChatter{})
	ctx := context.Background()

	sleeps := 0
	wasSleeping := a.Current == "sleeping"
	for i := 0; i < int(TicksPerDay); i++ {
		a.Step(ctx)
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
	if sleeps == 0 {
		t.Error("一个游戏日内一次都没睡，作息特征没体现")
	}
}
