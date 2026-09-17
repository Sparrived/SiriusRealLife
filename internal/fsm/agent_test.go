package fsm

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

func newTestAgent(t *testing.T, c Chatter) *Agent {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil)) // 测试里静音 R6 日志
	a, err := New(Options{
		Name:    "test",
		States:  MVPStates(),
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
//
// 用 decidingChatter：状态现在完全由 LLM 决定，一个不会调工具的假
// LLM 会让每次决策都降级成"原样再待一会"，这个验收就测不到东西。
func TestThirtyTickAcceptance(t *testing.T) {
	a := newDecidingAgent(t, 7)
	ctx := context.Background()

	moves := 0
	prev := a.Current
	for i := 0; i < 30; i++ {
		stepSync(t, a, ctx)
		if a.Current == prev {
			continue
		}
		moves++
		// 验收 4：真正的转移必须 from != to。候选清单里排除了当前
		// 状态，因此这条由框架保证，不靠模型自觉。
		if rec := a.LastRecord(); rec.From == rec.To {
			t.Fatalf("tick %d: 状态转移 from==to==%s", i, a.Current)
		}
		prev = a.Current
	}

	// 验收 1：不自锁、不死循环 —— 30 tick 内必须发生过转移。
	if moves == 0 {
		t.Fatal("30 tick 内一次状态转移都没有：决策链路没跑通")
	}
	if len(a.Stream) == 0 {
		t.Fatal("意识流为空")
	}
	if a.Now != 30 {
		t.Fatalf("Now = %d, 期望 30", a.Now)
	}
}

// TestDecisionIsReproducibleFromLog 验证可复现性的**新**来源。
//
// 旧实现靠固定种子（R3：agent 持有自己的 *rand.Rand）。现在没有随机，
// 复现性来自"决定是 LLM 说的，理由被记进了日志"——同一个假 LLM
// 给出的同一串决定必须产生同一串状态。这条测试守住"去掉随机之后
// 仍然可复现"，而不是把随机换个地方藏起来。
func TestDecisionIsReproducibleFromLog(t *testing.T) {
	run := func() ([]StateName, []string) {
		a := newDecidingAgent(t, 12345)
		ctx := context.Background()
		seq := []StateName{a.Current}
		var whys []string
		for i := 0; i < 30; i++ {
			stepSync(t, a, ctx)
			seq = append(seq, a.Current)
			whys = append(whys, a.LastRecord().Why)
		}
		return seq, whys
	}
	x, xw := run()
	y, yw := run()
	for i := range x {
		if x[i] != y[i] {
			t.Fatalf("tick %d: 相同决定产生不同状态 %s vs %s", i, x[i], y[i])
		}
	}
	for i := range xw {
		if xw[i] != yw[i] {
			t.Fatalf("tick %d: 理由不同 %q vs %q", i, xw[i], yw[i])
		}
	}
	// 理由必须真的被记下来：它是现在唯一能复盘"为什么换状态"的依据。
	// 注意不能只看第一条——决策最早也要到 MinTick（idle 是 2 tick）
	// 才发生，头一两个 tick 的 LastRecord 仍是零值。
	nonEmpty := 0
	for _, w := range xw {
		if w != "" {
			nonEmpty++
		}
	}
	if nonEmpty == 0 {
		t.Fatal("决策理由从来没有被记进 R6 日志")
	}
}

// TestCurrentStateNotInMenu 验证框架的硬约束：当前状态不进候选清单。
//
// 想继续待着必须调 stay——那是另一个工具、另一条分支。把当前状态
// 留在 enter_state 的菜单里，模型可以选它，于是"换状态"变成自我
// 循环，R6 日志里会出现一堆 from==to 的噪音。
func TestCurrentStateNotInMenu(t *testing.T) {
	a := newDecidingAgent(t, 1)
	for _, c := range a.survey() {
		if c.Name != a.Current {
			continue
		}
		if c.Blocked == "" {
			t.Fatalf("当前状态 %s 不该出现在候选里", a.Current)
		}
	}
	// 走几轮真实决策，确认没有 from==to 的转移。
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		stepSync(t, a, ctx)
		if rec := a.LastRecord(); rec.To != "" && rec.From == rec.To && rec.Reason == "llm" {
			t.Fatalf("tick %d: 出现了自我转移 %s", i, rec.To)
		}
	}
}

// TestCooldownBlocksCandidate 验证冷却是对 LLM 的硬约束。
func TestCooldownBlocksCandidate(t *testing.T) {
	a := newDecidingAgent(t, 1)
	a.Current = "idle"
	a.Now = 100
	// 假装刚从 working 离开，冷却到 110。
	a.cooldownUntil["working"] = 110

	if a.inMenu("working") {
		t.Fatal("冷却期内的状态不该可进入")
	}
	// 必须在候选清单里**带原因**出现：模型看到原因才不会反复尝试。
	var found bool
	for _, c := range a.survey() {
		if c.Name == "working" {
			found = true
			if c.Blocked == "" {
				t.Error("冷却中的状态应当带 Blocked 原因")
			}
		}
	}
	if !found {
		t.Error("冷却中的状态仍应出现在清单里（带原因），否则模型会以为框架坏了")
	}

	a.Now = 110
	if !a.inMenu("working") {
		t.Fatal("冷却结束后应当可以进入")
	}
}

// TestGuardHidesCandidate 验证 Guard 不满足时状态不可选。
func TestGuardHidesCandidate(t *testing.T) {
	a := newDecidingAgent(t, 1)
	a.Current = "working"
	a.Now = 12 * 60
	a.Mood.Energy = 90 // 正午精力充足：睡着不该可选

	if a.inMenu("sleeping") {
		t.Fatal("Guard 不满足时不该可进入")
	}
	a.Mood.Energy = 20 // 精疲力尽
	if !a.inMenu("sleeping") {
		t.Fatal("困了之后 sleeping 应当可进入")
	}
}

// TestClampTicks 验证时长被夹进状态的硬边界，且**不是**随机取值。
//
// 旧实现在 [MinTick, MaxTick] 里随机取一个时长；现在模型说多少就是
// 多少，只在越界时夹紧。同一个输入必须永远得到同一个输出。
func TestClampTicks(t *testing.T) {
	a := newDecidingAgent(t, 1)
	a.Current = "idle"

	cases := []struct {
		state string
		want  Tick
		exp   Tick
	}{
		{"working", 1, 4},    // 低于 MinTick → 抬到下限
		{"working", 8, 8},    // 区间内 → 原样
		{"working", 999, 12}, // 高于 MaxTick → 压到上限
		{"sleeping", 0, 60},
		{"sleeping", 120, 120},
		{"sleeping", 100000, 180},
	}
	for _, c := range cases {
		if got := a.clampTicks(StateName(c.state), c.want); got != c.exp {
			t.Errorf("clampTicks(%s, %d) = %d, 期望 %d", c.state, c.want, got, c.exp)
		}
	}
	// 反复调用必须稳定（没有随机藏在里面）。
	for i := 0; i < 20; i++ {
		if got := a.clampTicks("working", 999); got != 12 {
			t.Fatalf("第 %d 次调用得到 %d，夹紧结果不稳定（有随机？）", i, got)
		}
	}
}

// TestMentionDoesNotPreempt 验证 §2.1：@ 不抢占状态。
//
// 这是对旧行为的**反转**。曾经 @ 会硬编码切到 scrolling_phone，那是
// 把 QQ 的语义搞错了：@ 让人注意到，但不掐断你手上的事。现在它只是
// 负载里的一个标记（影响提示文案与队列重要性），不参与控制流。
func TestMentionDoesNotPreempt(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	ctx := context.Background()
	a.Current = "working"

	a.handleEvent(ctx, Event{Kind: EventUserMessage})

	if a.Current != "working" {
		t.Fatalf("@ 不该改变状态，实际变成 %s", a.Current)
	}
	// 也不该留下任何转移记录。
	if rec := a.LastRecord(); rec.Reason == "preempt" {
		t.Fatal("不该再有 preempt 转移理由")
	}
}

// TestMentionSurvivesSleeping 验证睡着时收到 @ 只留下记录，不改变状态。
//
// 旧实现靠 `Uninterruptible` + 延迟队列实现"不打断但有记录"。现在
// 不需要那套机制：没有抢占，自然就打断不了；"睡着时看不见手机"
// 由 sleeping 不订阅 QQ 表达（被门控的是信息）。
func TestMentionSurvivesSleeping(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	ctx := context.Background()
	a.Current = "sleeping"
	before := len(a.Stream)

	a.handleEvent(ctx, Event{Kind: EventUserMessage})

	if a.Current != "sleeping" {
		t.Fatalf("sleeping 不应被打断，实际变成 %s", a.Current)
	}
	if len(a.Stream) <= before {
		t.Fatal("收到消息应留下记录")
	}
}

// TestPumpFeedsOnlySubscribed 验证"状态 = 一段订阅"：
// 订阅了 QQ 的状态在驻留期间持续收到消息，没订阅的收不到。
//
// 这是"如何持续一个状态"的核心机制。旧实现只在 OnEnter 读一次，
// 于是"刷手机时来了新消息"看不见。
func TestPumpFeedsOnlySubscribed(t *testing.T) {
	a, quit := newAgentWithFakeAttention(t)
	defer quit()

	// 只比较**新增**的记录：意识和流里本来就有初始状态的旁白。
	n := len(a.Stream)

	a.Current = "working" // 不订阅 QQ
	a.Pump()
	if len(a.Stream) != n {
		t.Fatalf("working 不订阅 QQ，不该泵入任何内容，多了：%s", streamTextOf(a))
	}

	a.Current = "scrolling_phone" // 订阅 QQ
	a.Pump()
	if len(a.Stream) <= n {
		t.Fatal("scrolling_phone 订阅了 QQ，应泵入消息")
	}
	if got := streamTextOf(a); !strings.Contains(got, "在吗") {
		t.Fatalf("应泵入消息正文，实际：%s", got)
	}
}

// TestQuietForTracksSilence 验证"安静多久了"能被量出来。
//
// 这条守的是"长时间没收到消息就退出订阅"这个退出条件的前提：
// Until 只看得到心境与时间，若没有 QuietFor，"没人说话"这个**由外部
// 输入推出的**事实根本无法表达，那条退出条件也就写不出来。
func TestQuietForTracksSilence(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	ctx := context.Background()

	// 从来没收到过消息：要能区分出来，而不是报一个巨大的数字。
	if _, ever := a.QuietFor(); ever {
		t.Error("从未收到消息时第二个返回值应为 false")
	}

	a.Events() <- NewMessageEvent(IncomingMessage{From: "张三", Text: "在吗"})
	a.Step(ctx)
	// 事件在 Step 开头被 drain，**之后**才 Now++，因此这一步结束时
	// 已经过去 1 tick。这不是误差，而是 tick 顺序的如实反映。
	if got, ever := a.QuietFor(); !ever || got != 1 {
		t.Fatalf("消息刚到，QuietFor = %d/%v，期望 1/true", got, ever)
	}

	// 再推 5 个 tick 没人说话。
	for i := 0; i < 5; i++ {
		a.Step(ctx)
	}
	if got, _ := a.QuietFor(); got != 6 {
		t.Errorf("又过 5 tick 后 QuietFor = %d，期望 6", got)
	}

	// 又来一条：重新计。
	a.Events() <- NewMessageEvent(IncomingMessage{From: "李四", Text: "喂"})
	a.Step(ctx)
	if got, _ := a.QuietFor(); got != 1 {
		t.Errorf("又来消息后 QuietFor = %d，期望 1（只有本 tick）", got)
	}
}

// TestPhoneLeavesWhenQuiet 验证用户要的那条退出条件真的生效：
// 长时间没人说话时，刷手机这个状态会自己说"该结束的迹象出现了"。
func TestPhoneLeavesWhenQuiet(t *testing.T) {
	a, quit := newAgentWithFakeAttention(t)
	defer quit()
	a.Current = "scrolling_phone"
	a.enteredAt = a.Now
	a.Mood.Energy = 80

	var phone State
	for _, s := range MVPStates() {
		if s.Name == "scrolling_phone" {
			phone = s
		}
	}

	// 刚进入：刷得不久，也没安静多久，不该走。
	if phone.Until(a) {
		t.Fatal("刚进入刷手机时不该说该走了")
	}

	// 安静超过阈值：该走了。
	a.lastMessageAt = a.Now - 25
	a.everGotMessage = true
	if !phone.Until(a) {
		t.Error("安静 25 tick 后，刷手机应当提示该结束了（长时间没人说话）")
	}
}

// TestLLMDoesNotBlockTick 验证 R4：LLM 在途时状态机照常前进。
func TestLLMDoesNotBlockTick(t *testing.T) {
	slow := fakeChatter{delay: 200 * time.Millisecond, reply: "想了很久"}
	a := newTestAgent(t, slow)
	ctx := context.Background()

	if err := a.think(ctx, ChatRequest{Prompt: "思考"}, thinkMonologue); err != nil {
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

// TestCancelInFlightLLM 验证 R4：在途调用可被取消（关停路径）。
//
// 旧版本测的是"抢占会取消在途调用"。抢占取消后，取消只剩关停这一个
// 来源（Run 的 ctx.Done 分支），这里直接验证那台机器本身。
func TestCancelInFlightLLM(t *testing.T) {
	slow := fakeChatter{delay: 5 * time.Second, reply: "never"}
	a := newTestAgent(t, slow)
	ctx := context.Background()

	if err := a.think(ctx, ChatRequest{Prompt: "思考"}, thinkMonologue); err != nil {
		t.Fatalf("Think: %v", err)
	}
	a.cancelThinking()

	if a.IsThinking() {
		t.Fatal("取消后应清空在途标记（别让它跑完 30 秒再丢弃）")
	}
}

// TestSecondThinkRejected 验证并发保护：在途时不允许再起一次。
func TestSecondThinkRejected(t *testing.T) {
	a := newTestAgent(t, fakeChatter{delay: time.Second})
	ctx := context.Background()
	if err := a.think(ctx, ChatRequest{Prompt: "第一次"}, thinkMonologue); err != nil {
		t.Fatalf("第一次 Think 不应失败: %v", err)
	}
	if err := a.think(ctx, ChatRequest{Prompt: "第二次"}, thinkMonologue); err == nil {
		t.Fatal("在途时应拒绝第二次 Think")
	}
}

// TestVisibilityOnlyPhone 验证验收 5 的前半：只有 scrolling_phone 订阅 QQ。
func TestVisibilityOnlyPhone(t *testing.T) {
	for _, s := range MVPStates() {
		want := s.Name == "scrolling_phone"
		if got := s.Subscribes(ChanQQ); got != want {
			t.Errorf("状态 %s 的 QQ 订阅 = %v, 期望 %v", s.Name, got, want)
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
	// 必须用 decidingChatter：状态由 LLM 决定，不会调工具的假 LLM
	// 永远停在初始状态，这条测试就永远失败（好在它是失败的，
	// 更坏的情况是断言写成"不报错"从而静默通过）。
	a := newDecidingAgent(t, 20240101)
	ctx := context.Background()

	seen := map[StateName]bool{a.Current: true}
	// 跑一整个游戏日，覆盖夜间与白天。
	for i := 0; i < int(TicksPerDay); i++ {
		stepSync(t, a, ctx)
		seen[a.Current] = true
	}

	for _, s := range MVPStates() {
		if !seen[s.Name] {
			t.Errorf("状态 %s 跑满一整个游戏日仍未进入（guard 可能永远不满足）", s.Name)
		}
	}
}

// TestSleepGateAndWake 验证 sleeping 的两条正交机制：
// Guard 管"能不能睡"（困了才行），Until 管"该不该醒"（睡饱了）。
//
// 旧实现里"多想想睡"靠 WeightMod 抬高权重——那套连同加权抽取一起删了。
// 现在"夜里更想睡"不再是框架的算术，而是 prompt 里的事实（时刻 + 精力），
// 由 LLM 自己权衡；框架只保留两个硬约束：进不去（Guard）、该醒了（Until）。
func TestSleepGateAndWake(t *testing.T) {
	sleep := MVPStates()[3]
	if sleep.Name != "sleeping" {
		t.Fatalf("状态表顺序变了，取到 %s", sleep.Name)
	}
	a := newTestAgent(t, fakeChatter{})

	// 白天精力充足：不该满足守卫（也谈不上醒不醒——根本没睡）。
	a.Now = 12 * 60
	a.Mood.Energy = 90
	if sleep.Guard(a) {
		t.Error("正午精力 90 时不应满足睡眠守卫")
	}

	// 深夜精力充足：守卫仍不满足（不是"到点必睡"）。
	a.Now = 23*60 + 30
	a.Mood.Energy = 90
	if sleep.Guard(a) {
		t.Error("深夜但精力充足时不应满足守卫——夜里该由 LLM 权衡，不是强制")
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

// TestSleepingUntilWakes 验证"精力恢复了就该醒"——这条以前无法表达。
//
// 旧实现里醒来的唯一机制是随机 MaxTick 到点：睡够时长就醒，**与精力
// 无关**。于是"精力已经满了还在睡"必然发生，除非把时长调到刚好。
// 现在 Until 直接回答"睡饱了吗"，框架把它作为理由递给 LLM。
func TestSleepingUntilWakes(t *testing.T) {
	sleep := MVPStates()[3]
	a := newTestAgent(t, fakeChatter{})
	a.Current = "sleeping"
	a.Now = 100
	a.enteredAt = 60 // 已睡 40 tick，仍在 MinTick(60) 之上会另行判定

	if sleep.Until == nil {
		t.Fatal("sleeping 应当有 Until（否则只能靠时长到点醒来）")
	}

	a.Mood.Energy = 40
	if sleep.Until(a) {
		t.Error("精力只有 40 时不该说睡饱了")
	}
	a.Mood.Energy = 95
	if !sleep.Until(a) {
		t.Error("精力回到 95 时应当说睡饱了")
	}
}

// TestEnergyRecoversWhileSleeping 验证睡觉逐 tick 回复精力。
//
// 逐 tick 而非退出时一次加满：MaxTick 可能被夹紧、也可能被提前叫醒，
// "睡了个短的"就该只补回一点。这个测试同时锁住"净回复为正"——
// 若 sleepEnergyPerTick 小于基础衰减，精力会一路掉到 0 再也缓不过来，
// 而所有别的测试照样通过。
func TestEnergyRecoversWhileSleeping(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	ctx := context.Background()
	a.Current = "sleeping"
	a.Mood.Energy = 20
	before := a.Mood.Energy

	for i := 0; i < 30; i++ {
		a.Step(ctx)
	}
	if a.Mood.Energy <= before {
		t.Fatalf("睡了 30 tick 精力反而没涨：%v → %v（净回复必须为正）",
			before, a.Mood.Energy)
	}
}

// TestWorkingDrainsEnergy 验证干活逐 tick 掉精力，且比发呆快。
func TestWorkingDrainsEnergy(t *testing.T) {
	drain := func(state StateName) float64 {
		a := newTestAgent(t, fakeChatter{})
		ctx := context.Background()
		a.Current = state
		a.Mood.Energy = 80
		before := a.Mood.Energy
		for i := 0; i < 20; i++ {
			a.Step(ctx)
		}
		return before - a.Mood.Energy
	}
	work := drain("working")
	idle := drain("idle")
	if work <= idle {
		t.Errorf("干活(%.2f)应当比发呆(%.2f)更耗精力", work, idle)
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
