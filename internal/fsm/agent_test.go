package fsm

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// fakeChatter 是确定性 Chatter：可控延迟，用于验证 R4 不阻塞 tick。
type fakeChatter struct {
	delay time.Duration
	reply string
}

func (f fakeChatter) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	select {
	case <-time.After(f.delay):
		return ChatResponse{Text: f.reply}, nil
	case <-ctx.Done():
		return ChatResponse{}, ctx.Err()
	}
}

func newTestAgent(t *testing.T, seed int64, c Chatter) *Agent {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil)) // 测试里静音 R6 日志
	a, err := New(Options{
		Name:    "test",
		States:  MVPStates(),
		Seed:    seed,
		Chatter: c,
		Logger:  logger,
		Initial: "idle",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestThirtyTickAcceptance 是 docs/roadmap.md §2 的验收标准 1/2/4。
func TestThirtyTickAcceptance(t *testing.T) {
	a := newTestAgent(t, 20240101, fakeChatter{reply: "嗯"})
	ctx := context.Background()

	prev := a.Current
	for i := 0; i < 30; i++ {
		a.Step(ctx)
		if a.Current == prev {
			continue
		}
		// 验收 4：同一状态不连续进入两次。
		// enter() 已排除当前状态，这里断言转移确实换了状态。
		if a.LastRecord().From == a.LastRecord().To {
			t.Fatalf("tick %d: 状态转移 from==to==%s", i, a.Current)
		}
		prev = a.Current
	}

	// 验收 1：不自锁、不死循环 —— 30 tick 内必须发生过转移。
	if len(a.Stream) == 0 {
		t.Fatal("意识流为空")
	}
	if a.Now != 30 {
		t.Fatalf("Now = %d, 期望 30", a.Now)
	}
}

// TestDeterministic 验证同种子产生完全相同的状态序列（R3 可复现）。
func TestDeterministic(t *testing.T) {
	run := func() []StateName {
		a := newTestAgent(t, 12345, fakeChatter{})
		ctx := context.Background()
		seq := []StateName{a.Current}
		for i := 0; i < 30; i++ {
			a.Step(ctx)
			seq = append(seq, a.Current)
		}
		return seq
	}
	x, y := run(), run()
	for i := range x {
		if x[i] != y[i] {
			t.Fatalf("tick %d: 同种子结果不同 %s vs %s", i, x[i], y[i])
		}
	}
}

// TestNoConsecutiveSameState 验证 R3：不允许连续进入同一状态。
func TestNoConsecutiveSameState(t *testing.T) {
	for seed := int64(0); seed < 50; seed++ {
		a := newTestAgent(t, seed, fakeChatter{})
		ctx := context.Background()
		last := a.Current
		for i := 0; i < 100; i++ {
			a.Step(ctx)
			if a.Current == last {
				continue // 停留在同一状态是允许的
			}
			// 真正发生转移时，from 与 to 必须不同（enter 已排除，
			// 但 EventMention 抢占走的是另一条路径，这里一并覆盖）。
			rec := a.LastRecord()
			if rec.From == rec.To {
				t.Fatalf("seed=%d tick=%d: 转移到相同状态 %s", seed, i, rec.To)
			}
			last = a.Current
		}
	}
}

// TestMentionPreempts 验证 R3/§2：被 @ 能抢占当前状态。
func TestMentionPreempts(t *testing.T) {
	a := newTestAgent(t, 1, fakeChatter{})
	ctx := context.Background()
	// 先走到一个非 scrolling_phone 的状态。
	if a.Current == "scrolling_phone" {
		a.Step(ctx)
	}
	before := a.Current

	a.handleEvent(ctx, Event{Kind: EventMention})

	if a.Current != "scrolling_phone" {
		t.Fatalf("被 @ 后应进入 scrolling_phone，实际 %s", a.Current)
	}
	if rec := a.LastRecord(); rec.Reason != "preempt" {
		t.Fatalf("转移理由应为 preempt，实际 %s", rec.Reason)
	}
	if before == "scrolling_phone" {
		t.Skip("前置状态恰好是 scrolling_phone，跳过")
	}
}

// TestSleepingDefersMention 验证验收 5：sleeping 时 @ 不打断但有记录。
func TestSleepingDefersMention(t *testing.T) {
	a := newTestAgent(t, 1, fakeChatter{})
	ctx := context.Background()
	a.Current = "sleeping" // 直接置入，避免依赖随机

	a.handleEvent(ctx, Event{Kind: EventMention})

	if a.Current != "sleeping" {
		t.Fatalf("sleeping 不应被 @ 打断，实际变成 %s", a.Current)
	}
	if len(a.Deferred) != 1 {
		t.Fatalf("延迟事件数 = %d, 期望 1", len(a.Deferred))
	}
	// 记录必须留下：意识流应提到这件事。
	if len(a.Stream) == 0 {
		t.Fatal("sleeping 时收到 @ 应留下记录")
	}
}

// TestLLMDoesNotBlockTick 验证 R4：LLM 在途时状态机照常前进。
func TestLLMDoesNotBlockTick(t *testing.T) {
	slow := fakeChatter{delay: 200 * time.Millisecond, reply: "想了很久"}
	a := newTestAgent(t, 7, slow)
	ctx := context.Background()

	if err := a.Think(ctx, ChatRequest{Prompt: "思考"}); err != nil {
		t.Fatalf("Think: %v", err)
	}
	if !a.IsThinking() {
		t.Fatal("发起后应为在途状态")
	}

	// 在途期间连推 5 个 tick，必须立即返回（不被 200ms 阻塞）。
	start := time.Now()
	for i := 0; i < 5; i++ {
		a.Step(ctx)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("tick 被 LLM 阻塞了：5 个 tick 用了 %v", elapsed)
	}
	if !a.IsThinking() {
		t.Fatal("调用未完成前不应清空在途标记")
	}

	// 等结果回到事件通道并被处理。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.drainEvents(ctx)
		if !a.IsThinking() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.IsThinking() {
		t.Fatal("调用完成后应清空在途标记")
	}
}

// TestPreemptCancelsInFlightLLM 验证 R4：抢占会取消在途调用。
func TestPreemptCancelsInFlightLLM(t *testing.T) {
	slow := fakeChatter{delay: 5 * time.Second, reply: "never"}
	a := newTestAgent(t, 3, slow)
	ctx := context.Background()

	if err := a.Think(ctx, ChatRequest{Prompt: "思考"}); err != nil {
		t.Fatalf("Think: %v", err)
	}
	a.handleEvent(ctx, Event{Kind: EventMention})

	if a.IsThinking() {
		t.Fatal("抢占后应取消在途调用（别让它跑完 30 秒再丢弃）")
	}
}

// TestSecondThinkRejected 验证并发保护：在途时不允许再起一次。
func TestSecondThinkRejected(t *testing.T) {
	a := newTestAgent(t, 3, fakeChatter{delay: time.Second})
	ctx := context.Background()
	if err := a.Think(ctx, ChatRequest{Prompt: "第一次"}); err != nil {
		t.Fatalf("第一次 Think 不应失败: %v", err)
	}
	if err := a.Think(ctx, ChatRequest{Prompt: "第二次"}); err == nil {
		t.Fatal("在途时应拒绝第二次 Think")
	}
}

// TestVisibilityOnlyPhone 验证验收 5 的前半：只有 scrolling_phone 可见 QQ。
func TestVisibilityOnlyPhone(t *testing.T) {
	for _, s := range MVPStates() {
		want := s.Name == "scrolling_phone"
		if s.Visibility.QQ != want {
			t.Errorf("状态 %s 的 QQ 可见性 = %v, 期望 %v", s.Name, s.Visibility.QQ, want)
		}
	}
}

// TestEveryStateReachable 是回归检查：每个状态都必须在一个游戏日内可达。
//
// 这条测试来自一次真实的踩坑：初版精力衰减是 0.2/tick，配合 Energy<50 的
// 守卫，精力会在 3.3 游戏小时后跌到 0，并在当天剩余 72% 的时间里一直贴着 0，
// 于是"想睡"几乎整天成立——睡觉从一次性作息退化成高频动作。
// 现在衰减按"一个清醒日"校准，守卫也并入时段条件，睡觉是一天一次的事。
//
// 注意：本测试只断言"可达"，不断言频率。频率是否合理靠 Phase 1 人工看日志。
func TestEveryStateReachable(t *testing.T) {
	a := newTestAgent(t, 20240101, fakeChatter{})
	ctx := context.Background()

	seen := map[StateName]bool{a.Current: true}
	// 跑一整个游戏日，覆盖夜间与白天。
	for i := 0; i < int(TicksPerDay); i++ {
		a.Step(ctx)
		seen[a.Current] = true
	}

	for _, s := range MVPStates() {
		if !seen[s.Name] {
			t.Errorf("状态 %s 跑满一整个游戏日仍未进入（guard 可能永远不满足）", s.Name)
		}
	}
}

// TestSleepGateAndWeight 验证 sleeping 的两条正交机制（R2）：
// Guard 管"能不能睡"（困了才行），WeightMod 管"多想想睡"（夜里更想）。
func TestSleepGateAndWeight(t *testing.T) {
	sleep := MVPStates()[3]
	if sleep.Name != "sleeping" {
		t.Fatalf("状态表顺序变了，取到 %s", sleep.Name)
	}
	a := newTestAgent(t, 1, fakeChatter{})

	// 白天精力充足：既不该满足守卫，权重也不应被抬高。
	a.Now = 12 * 60
	a.Mood.Energy = 90
	if sleep.Guard(a) {
		t.Error("正午精力 90 时不应满足睡眠守卫")
	}
	if w := sleep.moodWeight(a); w != sleep.Weight {
		t.Errorf("白天精力充足时权重 = %v, 期望基础值 %v", w, sleep.Weight)
	}

	// 深夜精力充足：守卫仍不满足（不是"到点必睡"），但权重被抬高。
	a.Now = 23*60 + 30
	a.Mood.Energy = 90
	if sleep.Guard(a) {
		t.Error("深夜但精力充足时不应满足守卫——夜里应当是加权，不是强制")
	}
	if w := sleep.moodWeight(a); w <= sleep.Weight {
		t.Errorf("深夜权重 = %v, 应高于基础值 %v", w, sleep.Weight)
	}

	// 白天精疲力尽：守卫应满足。
	a.Now = 14 * 60
	a.Mood.Energy = 20
	if !sleep.Guard(a) {
		t.Error("白天精疲力尽时应当满足睡眠守卫")
	}

	// 凌晨（01:00–05:00）仍醒着：守卫应满足，兜住"通宵不睡"。
	a.Now = 2 * 60
	a.Mood.Energy = 90
	if !sleep.Guard(a) {
		t.Error("凌晨仍醒着时应当满足睡眠守卫（兜底）")
	}
}

// TestClock 验证 R8 的 tick → 游戏时刻换算，含跨日取模。
func TestClock(t *testing.T) {
	cases := []struct {
		tick Tick
		hour int
	}{
		{0, 0},                  // 00:00
		{9 * 60, 9},             // 09:00
		{12 * 60, 12},           // 12:00
		{22 * 60, 22},           // 22:00
		{23 * 60, 23},           // 23:00
		{TicksPerDay - 1, 23},   // 23:59
		{TicksPerDay, 0},        // 次日 00:00
		{TicksPerDay + 60, 1},   // 次日 01:00
		{7*TicksPerDay + 30, 0}, // 多日后仍取模
		{-1, 23},                // 负 tick 不应 panic
	}
	for _, c := range cases {
		if got := Hour(c.tick); got != c.hour {
			t.Errorf("Hour(%d) = %d, 期望 %d", c.tick, got, c.hour)
		}
	}
}
