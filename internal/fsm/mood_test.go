package fsm

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// newQuietAgent 造一个不打印日志的 agent（统计用）。
func newQuietAgent(t *testing.T, rates MoodRates) *Agent {
	t.Helper()
	a, err := New(Options{
		Name: "stats", States: MVPStates(),
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
// 与旧版的区别：状态不再随机抽取，因此"睡不睡"取决于驱动它的假 LLM。
// 这条测试因此断言的是**精力收支的可行性**（能回得去、不会被钉死），
// 而不是某个抽取分布下的频率。频率合不合理只能靠看日志。
func TestMoodEconomyBalanced(t *testing.T) {
	const days = 7

	a := newDecidingAgent(t, 20240101)
	a.moodRates = DefaultMoodRates()
	// 从早上 7 点开始跑，覆盖一个完整的清醒日 + 夜间。
	a.Now = 7 * 60
	a.enteredAt = a.Now
	a.decideAt = a.Now
	ctx := context.Background()

	var zeroTicks, total, maxZeroRun int
	run := 0
	for i := 0; i < int(TicksPerDay)*days; i++ {
		stepSync(t, a, ctx)
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
	}

	zeroPct := float64(zeroTicks) * 100 / float64(total)
	if zeroPct > 5.0 {
		t.Errorf("精力为 0 的时间占比 %.1f%%，超过 5%%：说明衰减快于回复，"+
			"人格会长期精疲力尽（调整 DefaultMoodRates 或 sleepEnergyPerTick）", zeroPct)
	}
	if maxZeroRun > 300 {
		t.Errorf("最长连续精力为 0 达 %d tick（%.1f 游戏小时），超过 5 小时："+
			"精力一旦见底就再也缓不过来", maxZeroRun, float64(maxZeroRun)/60)
	}
}

// TestSleepRecoversFasterThanDecay 是上面那条测试的**根因**版本。
//
// 收支平衡的算术前提只有一条：睡觉的净回复必须为正，且能在 MaxTick
// 之内从 Guard 的下限睡到 Until 的上限。这条等式一旦坏掉，精力会
// 单调下滑到 0，而"占比"那类统计断言可能因为跑得不够久而漏掉。
// 因此这里直接算，不靠跑。
func TestSleepRecoversFasterThanDecay(t *testing.T) {
	var sleep State
	for _, s := range MVPStates() {
		if s.Name == "sleeping" {
			sleep = s
		}
	}
	decay := DefaultMoodRates().Energy
	net := -sleep.EnergyPerTick - decay // EnergyPerTick 为负有符号：净回复
	if net <= 0 {
		t.Fatalf("睡觉净回复 %.4f/tick 不为正：精力会一路掉到 0 再也回不来", net)
	}

	// 从 Guard 的下限（精力 35）睡到 Until 的上限（90）需要多少 tick。
	need := Tick(55 / net)
	if need > sleep.MaxTick {
		t.Errorf("睡满一次需要 %d tick，超过 MaxTick=%d：还没睡够就被叫醒了，"+
			"精力永远到不了 Until 的门槛，睡醒条件形同虚设", need, sleep.MaxTick)
	}
	if need < sleep.MinTick {
		t.Errorf("睡满一次只要 %d tick，短于 MinTick=%d：Until 会在下限之前就成立，"+
			"MinTick 失去意义", need, sleep.MinTick)
	}
}

// TestWorkCostIsPerTick 验证"干活越久越累"。
//
// 逐 tick 计费取代了旧的"进入时按计划时长扣一次"。原因是计划时长
// 现在只是**计划**：LLM 可以中途改主意，也可能被 Until 提前叫停。
// 逐 tick 累加对"实际待了多久"天然正确。
func TestWorkCostIsPerTick(t *testing.T) {
	var work State
	for _, s := range MVPStates() {
		if s.Name == "working" {
			work = s
		}
	}
	if work.EnergyPerTick <= 0 {
		t.Fatal("working 应当声明正的 EnergyPerTick（干活要额外耗精力）")
	}

	drain := func(ticks int) float64 {
		a := newQuietAgent(t, MoodRates{})
		ctx := context.Background()
		a.Current = "working"
		a.Mood.Energy = 100
		for i := 0; i < ticks; i++ {
			a.Step(ctx)
		}
		return 100 - a.Mood.Energy
	}
	short, long := drain(10), drain(40)
	if long <= short {
		t.Fatalf("干 40 tick 消耗 %.3f 不大于干 10 tick 的 %.3f", long, short)
	}
}

// TestNightMoodDoesNotChangeGuard 验证"夜里更想睡"不再由框架算术表达。
//
// 旧实现用 WeightMod 抬高 sleeping 的权重。加权抽取删掉后，"夜里更想睡"
// 改由 prompt 里的事实（时刻 + 精力）交给 LLM 权衡，框架只留两个硬
// 约束：Guard（进不进得去）与 Until（该不该醒）。
//
// 这条测试锁住那个**边界**：夜里精力充足时 Guard 仍不满足，因此
// "到点必睡"没有偷偷回来——睡觉始终是 LLM 的选择，不是框架的强制。
func TestNightMoodDoesNotChangeGuard(t *testing.T) {
	var sleep State
	for _, s := range MVPStates() {
		if s.Name == "sleeping" {
			sleep = s
		}
	}
	a := newQuietAgent(t, MoodRates{})

	// 深夜但精力充足：23:30 不在"凌晨兜底"区间（01:00–05:00）内，
	// 因此守卫不该满足——夜里该由 LLM 权衡，不是强制。
	a.Now = 23*60 + 30
	a.Mood.Energy = 90
	if sleep.Guard(a) {
		t.Error("深夜精力充足时不该满足睡眠守卫——夜里该由 LLM 权衡，不是强制")
	}
	// 精力低：该满足，无论在什么时刻。
	a.Now = 14 * 60
	a.Mood.Energy = 20
	if !sleep.Guard(a) {
		t.Error("精疲力尽时应当满足睡眠守卫")
	}
	// 凌晨兜底仍然生效：连着不睡到 01:00 之后必须允许睡（否则通宵）。
	a.Now = 2 * 60
	a.Mood.Energy = 90
	if !sleep.Guard(a) {
		t.Error("凌晨（01:00–05:00）仍醒着时应当满足睡眠守卫")
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

	// 未读在免烦阈值内：不增长。
	a.Mood.Annoyed = 0
	a.decayMood()
	if got := a.Mood.Annoyed; got > 0 {
		t.Errorf("未读在免烦阈值内不该推高烦躁，实际 %.3f", got)
	}

	// 越过阈值：应当持续堆积（把注意力接口换成大队列）。
	a.attention = &fakeAttention{msgs: make([]string, 20)}
	a.Mood.Annoyed = 0
	for i := 0; i < 10; i++ {
		a.decayMood()
	}
	if got := a.Mood.Annoyed; got <= 0 {
		t.Error("未读远超阈值时烦躁应当堆积（§2.2：99+ 推高烦躁）")
	}
}

// TestMoodRatesInjectable 验证衰减率是人格参数、可注入。
func TestMoodRatesInjectable(t *testing.T) {
	fast := newQuietAgent(t, MoodRates{Energy: 1.0})
	slow := newQuietAgent(t, MoodRates{Energy: 0.0001})
	for i := 0; i < 10; i++ {
		fast.Step(context.Background())
		slow.Step(context.Background())
	}
	if fast.Mood.Energy >= slow.Mood.Energy {
		t.Fatalf("衰减快的 agent 精力应更低: fast=%.2f slow=%.2f",
			fast.Mood.Energy, slow.Mood.Energy)
	}
	// 零值应当落到默认值，而不是"完全不衰减"。
	def := newQuietAgent(t, MoodRates{})
	if def.moodRates.Energy != DefaultMoodRates().Energy {
		t.Errorf("零值应使用默认衰减率，得到 %v", def.moodRates.Energy)
	}
}
