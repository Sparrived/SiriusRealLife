package fsm

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// newQuietAgent 造一个不打印日志的 agent（统计用）。
func newQuietAgent(t *testing.T, seed int64, rates MoodRates) *Agent {
	t.Helper()
	a, err := New(Options{
		Name: "stats", States: MVPStates(), Seed: seed,
		StartTick: 7 * 60, Initial: "working",
		MoodRates: rates,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestMoodEconomyBalanced 锁住心境的"收支平衡"。
//
// 这是人格参数里最容易悄悄坏掉的地方：精力唯一的回复手段是睡觉，
// 若日衰减超过睡觉能补回的量，精力会长期钉在 0，人格失去"累"的
// 层次——所有单测照样通过，只有跑一整天数据才看得出来。
//
// 曾经的缺陷：working.OnEnter 每次扣固定 5 点，而 working 一天要
// 进入几十次，于是精力有 46.5% 的时间为 0、最长连续 10.4 游戏小时。
// 修法是把成本改成与**本次计划时长**成正比（见 PlannedDuration）。
//
// 这个测试跑 20 个种子 × 7 天，断言统计意义上的行为，不看单次轨迹。
func TestMoodEconomyBalanced(t *testing.T) {
	const seeds, days = 20, 7

	var zeroTicks, total, sleeps, maxZeroRun int
	for seed := int64(1); seed <= seeds; seed++ {
		a := newQuietAgent(t, seed, MoodRates{})
		run := 0
		prev := a.Current
		for i := 0; i < int(TicksPerDay)*days; i++ {
			a.Step(context.Background())
			total++
			if a.Mood.Energy <= 0.01 {
				zeroTicks++
				run++
				if run > maxZeroRun {
					maxZeroRun = run
				}
			} else {
				run = 0
			}
			if prev != "sleeping" && a.Current == "sleeping" {
				sleeps++
			}
			prev = a.Current
		}
	}

	zeroPct := float64(zeroTicks) * 100 / float64(total)
	if zeroPct > 2.0 {
		t.Errorf("精力为 0 的时间占比 %.1f%%，超过 2%%：说明衰减快于回复，"+
			"人格会长期精疲力尽（调整 DefaultMoodRates 或 workEnergyPerTick）", zeroPct)
	}
	if maxZeroRun > 180 {
		t.Errorf("最长连续精力为 0 达 %d tick（%.1f 游戏小时），超过 3 小时："+
			"精力一旦见底就再也缓不过来", maxZeroRun, float64(maxZeroRun)/60)
	}

	// 睡觉必须真的发生，否则"精力平衡"是靠不消耗换来的。
	perDay := float64(sleeps) / float64(seeds*days)
	if perDay < 1.0 {
		t.Errorf("日均睡眠 %.2f 次，少于 1 次：精力可能是靠衰减太慢平衡的", perDay)
	}
	if perDay > 3.0 {
		t.Errorf("日均睡眠 %.2f 次，多于 3 次：睡得太频繁，不像人", perDay)
	}
}

// TestWorkCostScalesWithDuration 验证"干活越久越累"。
//
// 定成本（每次进入扣固定值）与时长无关，是曾经的缺陷来源：
// working 一天进入几十次，固定扣费等于按"次数"收钱。
func TestWorkCostScalesWithDuration(t *testing.T) {
	cost := func(minTick, maxTick Tick) float64 {
		states := MVPStates()
		for i := range states {
			if states[i].Name == "working" {
				states[i].MinTick, states[i].MaxTick = minTick, maxTick
			}
		}
		a, err := New(Options{
			Name: "w", States: states, Seed: 3, Initial: "working",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		a.Mood.Energy = 100
		// 进入 working 的 OnEnter 已由 New 触发过一次；这里手工调一次
		// 以比较不同时长下的成本。
		a.Current = "idle"
		a.Now = 0
		a.enter("working", "test")
		return 100 - a.Mood.Energy
	}

	short := cost(2, 2)
	long := cost(60, 60)
	if long <= short {
		t.Fatalf("干 60 tick 的成本 %.3f 不大于干 2 tick 的 %.3f——"+
			"成本必须与时长成正比", long, short)
	}
	// 比例关系：60/2 = 30 倍（允许浮点误差）。
	if ratio := long / short; ratio < 25 || ratio > 35 {
		t.Errorf("成本比例 %.1f，期望约 30（60 tick / 2 tick）", ratio)
	}
}

// TestNightAndEnergyRaiseSleepWeight 验证 R2 的"心境 → 权重"：
// 夜里更想睡、精力低更想睡，但不改变 Guard（能不能睡）。
func TestNightAndEnergyRaiseSleepWeight(t *testing.T) {
	a := newQuietAgent(t, 5, MoodRates{})
	var sleep State
	for _, s := range MVPStates() {
		if s.Name == "sleeping" {
			sleep = s
		}
	}
	if sleep.WeightMod == nil {
		t.Fatal("sleeping 应当有 WeightMod")
	}

	// 白天 + 精力充足：基准。
	a.Now = 14 * 60
	a.Mood.Energy = 90
	dayRested := sleep.moodWeight(a)

	// 深夜：权重应当更高。
	a.Now = 2 * 60
	nightRested := sleep.moodWeight(a)

	// 白天 + 精力低：权重也应当更高。
	a.Now = 14 * 60
	a.Mood.Energy = 30
	dayTired := sleep.moodWeight(a)

	if nightRested <= dayRested {
		t.Errorf("深夜权重 %.1f 应大于白天 %.1f", nightRested, dayRested)
	}
	if dayTired <= dayRested {
		t.Errorf("精力低时权重 %.1f 应大于精力足时 %.1f", dayTired, dayRested)
	}
}

// TestUnreadDrivesAnnoyed 验证 memory.md §2.2 的"未读推高烦躁"真的接上了。
//
// 这是本轮补上的缺口：此前 Annoyed **只有衰减、没有任何增长**，
// 文档里那句话是空话。当时反应通道是 @ 抢占，掩盖了它；抢占取消后
// 积累成了唯一的反应通道——没有它，消息再多人格也毫无反应。
func TestUnreadDrivesAnnoyed(t *testing.T) {
	a, _ := newAgentWithFakeAttention(t) // 队列里 2 条，低于免烦阈值
	a.Current = "working"                // 不订阅 QQ：烦躁不该依赖"看没看"

	annoy := func() float64 { return a.Mood.Annoyed }

	// 未读在免烦阈值内：不增长。
	a.Mood.Annoyed = 0
	a.decayMood()
	if got := annoy(); got > 0 {
		t.Errorf("未读在免烦阈值内不该推高烦躁，实际 %.3f", got)
	}

	// 越过阈值：应当持续堆积（把注意力接口换成大队列）。
	a.attention = &fakeAttention{msgs: make([]string, 20)}
	a.Mood.Annoyed = 0
	for i := 0; i < 10; i++ {
		a.decayMood()
	}
	if got := annoy(); got <= 0 {
		t.Error("未读远超阈值时烦躁应当堆积（§2.2：99+ 推高烦躁）")
	}
}

// TestMoodRatesInjectable 验证衰减率是人格参数、可注入。
func TestMoodRatesInjectable(t *testing.T) {
	fast := newQuietAgent(t, 1, MoodRates{Energy: 1.0})
	slow := newQuietAgent(t, 1, MoodRates{Energy: 0.0001})
	for i := 0; i < 10; i++ {
		fast.Step(context.Background())
		slow.Step(context.Background())
	}
	if fast.Mood.Energy >= slow.Mood.Energy {
		t.Fatalf("衰减快的 agent 精力应更低: fast=%.2f slow=%.2f",
			fast.Mood.Energy, slow.Mood.Energy)
	}
	// 零值应当落到默认值，而不是"完全不衰减"。
	def := newQuietAgent(t, 1, MoodRates{})
	if def.moodRates.Energy != DefaultMoodRates().Energy {
		t.Errorf("零值应使用默认衰减率，得到 %v", def.moodRates.Energy)
	}
}
