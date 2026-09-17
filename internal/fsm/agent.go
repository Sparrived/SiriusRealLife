package fsm

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
)

// StreamEntry 是意识流里的一条记录。
//
// 这是 `working` 层的载体：prompt 只读它 + 长期层（R5）。
// 完整分层（staging/event/consolidated/self-model/Shadow）见
// docs/memory.md，由 internal/memory 实现并在 Phase 1 接入。
type StreamEntry struct {
	Seq   Tick
	State StateName
	Text  string
}

// streamLimit 是意识流的硬上限（R5）。超出即丢最旧的。
const streamLimit = 100

// ChatRequest / ChatResponse 是 LLM 调用的最小形状。
// 真实客户端在 internal/llm，只用标准库的 net/http（R9）。
type ChatRequest struct {
	Prompt string
}

// ChatResponse 是一次 LLM 调用的结果。
type ChatResponse struct {
	Text string
}

// Chatter 是 LLM 调用的唯一缝口。
//
// 两个真实实现：internal/llm 的 AMKR 客户端，以及测试用的确定性 fake。
// fsm 不 import internal/llm —— 依赖方向必须是 llm → fsm 的调用方，
// 否则状态机核心会被网络库污染，且无法离线测试。
type Chatter interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// Agent 是一个人格实例。
//
// 并发约定（R1）：所有字段只允许被 Run 所在的那个 goroutine 读写。
// 外部只能通过 Events() 投递事件。
type Agent struct {
	Name string

	// 只被自身 goroutine 访问 —— 见 R1。
	Current   StateName
	Now       Tick
	Mood      Mood
	Stream    []StreamEntry
	Deferred  []Event // uninterruptible 状态下被延迟的事件
	CallCount int     // LLM 调用次数（测试与 SSE 观测用）

	states        map[StateName]State
	order         []State // 稳定顺序，保证分派可复现（R3）
	enteredAt     Tick
	dwellUntil    Tick // 进入时随机决定的换出时刻
	cooldownUntil map[StateName]Tick
	rng           *rand.Rand
	events        chan Event
	chatter       Chatter
	log           *slog.Logger
	lastRecord    DispatchRecord
	thinking      context.CancelFunc // 在途 LLM 调用的取消函数，nil 表示空闲
	observe       func(Snapshot)
	attention     Attention
	ticker        Ticker
	moodRates     MoodRates
}

// Options 是构造 Agent 的参数。种子显式传入使 30-tick 验收可复现（R3）。
type Options struct {
	Name    string
	States  []State
	Seed    int64
	Chatter Chatter
	Logger  *slog.Logger
	// Initial 是初始状态名；省略则用状态表第一个。
	Initial StateName
	// StartTick 是起始 tick（1 tick = 1 游戏分钟，故 540 = 09:00）。
	// 省略则从 0（游戏内 00:00）开始。这是 R8 时钟的唯一入口。
	StartTick Tick
	// Observe 在每次 tick 与事件处理后调用，参数是状态的**值快照**。
	//
	// 存在的理由是 R1：外部（HTTP/SSE）绝不能直接读 agent 字段，
	// 否则就是跨 goroutine 读写竞态。Observe 由 agent 自己的 goroutine
	// 调用，观察者只拿到拷贝。
	Observe func(Snapshot)
	// Attention 提供"看 QQ"所需的读取能力。为 nil 时进入
	// scrolling_phone 不会读到任何消息（离线测试用）。
	Attention Attention
	// Ticker 在每个 tick 后被驱动一次，让记忆层跟着时间前进。
	// 为 nil 时不驱动。见 attention.go 里 Ticker 的说明。
	Ticker Ticker
	// MoodRates 是心境衰减率。零值用 DefaultMoodRates()。
	MoodRates MoodRates
}

// Snapshot 是 agent 状态的值快照。
//
// 传值而非指针：观察者拿到之后与 agent 再无共享，不可能反向改写。
type Snapshot struct {
	Now       Tick
	Current   StateName
	Mood      Mood
	Stream    []StreamEntry
	CallCount int
	Deferred  int
	Thinking  bool
	Last      DispatchRecord
}

// snapshot 取一份当前状态的值拷贝（只能在 agent 自己的 goroutine 里调）。
func (a *Agent) snapshot() Snapshot {
	stream := make([]StreamEntry, len(a.Stream))
	copy(stream, a.Stream)
	return Snapshot{
		Now:       a.Now,
		Current:   a.Current,
		Mood:      a.Mood,
		Stream:    stream,
		CallCount: a.CallCount,
		Deferred:  len(a.Deferred),
		Thinking:  a.IsThinking(),
		Last:      a.lastRecord,
	}
}

// emit 在配置了 Observe 时推送快照。
func (a *Agent) emit() {
	if a.observe != nil {
		a.observe(a.snapshot())
	}
}

// New 构造一个 Agent。返回的 agent 尚未运行，需调用 Run 或 Step。
func New(opt Options) (*Agent, error) {
	if len(opt.States) == 0 {
		return nil, fmt.Errorf("fsm: 状态表为空")
	}
	states := make(map[StateName]State, len(opt.States))
	for _, s := range opt.States {
		if s.MaxTick < s.MinTick {
			return nil, fmt.Errorf("fsm: 状态 %s 的 maxTick(%d) < minTick(%d)", s.Name, s.MaxTick, s.MinTick)
		}
		if _, dup := states[s.Name]; dup {
			return nil, fmt.Errorf("fsm: 状态名重复: %s", s.Name)
		}
		states[s.Name] = s
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	initial := opt.Initial
	if initial == "" {
		initial = opt.States[0].Name
	}
	if _, ok := states[initial]; !ok {
		return nil, fmt.Errorf("fsm: 初始状态不存在: %s", initial)
	}

	rates := opt.MoodRates
	// 三项全为 0 视为"未设置"，用默认值。允许显式传部分为 0
	// （例如只想要烦躁不衰减）。
	if rates == (MoodRates{}) {
		rates = DefaultMoodRates()
	}

	a := &Agent{
		Name:          opt.Name,
		states:        states,
		order:         opt.States,
		Now:           opt.StartTick,
		rng:           rand.New(rand.NewSource(opt.Seed)),
		events:        make(chan Event, 64),
		chatter:       opt.Chatter,
		log:           logger.With("agent", opt.Name),
		cooldownUntil: map[StateName]Tick{},
		observe:       opt.Observe,
		attention:     opt.Attention,
		ticker:        opt.Ticker,
		moodRates:     rates,
		Mood:          Mood{Energy: 80, Annoyed: 0, Curious: 60},
	}
	a.enter(initial, "init")
	return a, nil
}

// Events 返回事件投递通道。这是外部影响 agent 的唯一入口（R1）。
func (a *Agent) Events() chan<- Event { return a.events }

// LastRecord 返回最近一次分派记录（R6 日志的内容）。
func (a *Agent) LastRecord() DispatchRecord { return a.lastRecord }

// PlannedDuration 返回当前状态本次计划的停留时长。
//
// 在 OnEnter 里可用：enter() 先算好 dwellUntil 再调 OnEnter，因此
// 状态的副作用能按"这次要待多久"来定成本，而不是每次进入扣一个
// 与时长无关的固定值。这点很重要——"干活越久越累"才对，
// 而 working 一天要进入几十次，固定扣费会让人格长期精疲力尽。
func (a *Agent) PlannedDuration() Tick { return a.dwellUntil - a.Now }

// enter 进入一个状态：执行 onExit/onEnter、设置停留目标、记冷却。
func (a *Agent) enter(name StateName, reason string) {
	prev := a.Current
	if prev != "" {
		if s, ok := a.states[prev]; ok && s.OnExit != nil {
			s.OnExit(a)
		}
		// 冷却从离开时刻起算。
		if s, ok := a.states[prev]; ok && s.Cooldown > 0 {
			a.cooldownUntil[prev] = a.Now + s.Cooldown
		}
	}
	a.Current = name
	a.enteredAt = a.Now

	s := a.states[name]
	// 停留时长在进入时一次性随机决定：随机只在"该换状态了"这一刻介入。
	span := s.MaxTick - s.MinTick
	var extra Tick
	if span > 0 {
		extra = Tick(a.rng.Int63n(int64(span) + 1))
	}
	a.dwellUntil = a.Now + s.MinTick + extra

	if s.OnEnter != nil {
		s.OnEnter(a)
	}
	if prev != "" {
		a.lastRecord = DispatchRecord{
			From: prev, To: name, Reason: reason, Seq: a.Now,
			Candidates: a.lastRecord.Candidates, Roll: a.lastRecord.Roll, Total: a.lastRecord.Total,
		}
		a.logTransition(a.lastRecord)
	}
}

// logTransition 输出 R6 要求的结构化转移日志。字段不用字符串拼。
//
// 候选项快照用 slog.Any 序列化成 JSON 数组：这是复盘"为什么选了这个
// 状态"的唯一依据，必须与当次抽取的 roll/total 一起出现。
func (a *Agent) logTransition(r DispatchRecord) {
	a.log.Info("state_transition",
		slog.String("from", string(r.From)),
		slog.String("to", string(r.To)),
		slog.String("reason", r.Reason),
		slog.Int64("seq", int64(r.Seq)),
		slog.Float64("roll", r.Roll),
		slog.Float64("total", r.Total),
		slog.Any("candidates", r.Candidates),
	)
}

// appendStream 写一条意识流，维持有界（R5）。
func (a *Agent) appendStream(text string) {
	a.Stream = append(a.Stream, StreamEntry{Seq: a.Now, State: a.Current, Text: text})
	if len(a.Stream) > streamLimit {
		a.Stream = a.Stream[len(a.Stream)-streamLimit:]
	}
}

// decayMood 按 tick 衰减心境（R2：心境自己按时间衰减）。
//
// 速率来自 MoodRates（人格参数，见 mood.go）。这里刻意不写字面量：
// 精力衰减快于"睡觉能补回"的量时，精力会长期钉在 0，
// 人格就失去"累"的层次。那个平衡由 TestMoodEconomyBalanced 守住。
func (a *Agent) decayMood() {
	a.Mood.Energy -= a.moodRates.Energy
	a.Mood.Annoyed -= a.moodRates.Annoyed
	a.Mood.Curious -= a.moodRates.Curious
	a.Mood.Clamp()
}

// Step 前进一个 tick。这是业务逻辑的唯一时间入口（R8）。
//
// 顺序：先处理到达的事件，再推时间，最后判断是否该换状态。
func (a *Agent) Step(ctx context.Context) {
	a.drainEvents(ctx)
	a.Now++
	a.decayMood()
	// 记忆层跟着时间前进（衰减/升格/遗忘全按 tick 判定，R8）。
	// 放在这里而不是 dispatch 之后：即使本次要换状态，记忆也该按
	// 新 tick 结算，两者没有先后依赖。
	if a.ticker != nil {
		a.ticker.Tick(a.Now)
	}

	if a.Now >= a.dwellUntil {
		next, rec, err := a.dispatch()
		if err != nil {
			// 无候选不是致命错误：留在原状态并记录，避免死循环。
			a.log.Warn("dispatch_failed", slog.String("err", err.Error()))
			// 延长停留，否则每个 tick 都会重试。
			a.dwellUntil = a.Now + 1
			a.emit()
			return
		}
		a.lastRecord = rec
		a.enter(next, "timeout")
	}
	a.emit()
}

// drainEvents 非阻塞地处理所有待处理事件。
func (a *Agent) drainEvents(ctx context.Context) {
	for {
		select {
		case ev := <-a.events:
			a.handleEvent(ctx, ev)
		default:
			return
		}
	}
}

// handleEvent 处理单个事件。高优先级事件可抢占当前状态（R3）。
func (a *Agent) handleEvent(ctx context.Context, ev Event) {
	cur := a.states[a.Current]

	// uninterruptible 状态：延迟而非抢占，但必须留下记录。
	if cur.Uninterruptible && ev.Kind.priority() >= 100 {
		a.Deferred = append(a.Deferred, ev)
		a.appendStream(fmt.Sprintf("收到 %s，但现在不能被打断，先记下", ev.Kind))
		return
	}

	switch ev.Kind {
	case EventMention:
		// 被 @ 抢占 → 进入看 QQ。理由见 docs/memory.md §2。
		// 抢占时取消在途 LLM 调用：别让它跑完 30 秒再丢弃（R4）。
		a.cancelThinking()
		a.lastRecord = DispatchRecord{From: a.Current, To: "scrolling_phone", Reason: "preempt", Seq: a.Now}
		a.enter("scrolling_phone", "preempt")
	case EventUserMessage:
		a.appendStream("有人发消息，但没叫我")
	case EventLLMDone:
		a.settleThinking()
		a.CallCount++
		a.appendStream("想完了")
	case EventLLMFailed:
		a.settleThinking()
		a.appendStream("没想出来")
	default:
		a.log.Warn("unknown_event", slog.String("kind", string(ev.Kind)))
	}
	_ = ctx
}

// Run 是 agent 的主循环（R1：一个 agent 一个 goroutine）。
//
// clock 每收到一次值就前进一个 tick —— 真实时间与游戏时间的换算
// 只在 tick 源做一次（R8），业务逻辑里没有 time.Now()。
func (a *Agent) Run(ctx context.Context, clock <-chan struct{}) error {
	// 唤醒被延迟的事件：一旦当前状态允许打断，立刻重放。
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clock:
			a.replayDeferred(ctx)
			a.Step(ctx)
		case ev := <-a.events:
			// 事件立即处理，不必等到下一 tick。
			a.handleEvent(ctx, ev)
			a.emit()
		}
	}
}

// replayDeferred 在当前状态可被打断时，重放之前延迟的事件。
func (a *Agent) replayDeferred(ctx context.Context) {
	if len(a.Deferred) == 0 {
		return
	}
	if a.states[a.Current].Uninterruptible {
		return
	}
	pending := a.Deferred
	a.Deferred = nil
	for _, ev := range pending {
		a.handleEvent(ctx, ev)
	}
}
