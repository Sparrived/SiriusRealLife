package fsm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// StreamEntry 是意识流里的一条记录。
//
// 这是 `working` 层的载体：prompt 只读它 + 长期层（R5）。
// 完整分层（staging/event/consolidated/self-model/Shadow）见
// docs/memory.md，由 internal/memory 实现并在 Phase 1 接入。
//
// Kind 让它成为**带类型的日志**而非流水账：装配器据此回答
// "最近在想什么 / 做了什么 / 接下来打算做什么"（见 stream.go）。
type StreamEntry struct {
	Seq   Tick
	Kind  Kind
	State StateName
	Text  string
}

// streamLimit 是意识流的硬上限（R5）。超出即丢最旧的。
const streamLimit = 100

// ChatRequest / ChatResponse 是 LLM 调用的最小形状。
// 真实客户端在 internal/llm，只用标准库的 net/http（R9）。
type ChatRequest struct {
	Prompt string
	// Schema 非空时请求上游返回符合它的 JSON（OpenAI 的
	// response_format=json_schema, strict）。
	//
	// 这是**尽力而为**，不是保证：实测同一条 prompt 下，AMKR 的某些
	// 路由（如 wb2api）会静默忽略 response_format 并照旧返回散文，
	// 既不遵守也不报错。因此解析侧必须同时接受 JSON 与前缀两种形态，
	// 见 parseMonologue。客户端不能假设路由支持它——路由是 AMKR 侧
	// 决定的，可以随时切（R9）。
	Schema *ResponseSchema
	// Tools 是本次调用允许的工具（空 = 纯文本调用）。
	//
	// 用"每次调用显式给出"而不是"全局注册表"：工具可见性随状态变化
	// （R7 修订），而全局注册表只能表达"所有调用看到同一套"。
	Tools []ToolSpec
}

// ResponseSchema 描述一次结构化输出的期望形状。
//
// JSON Schema 用 json.RawMessage 而不是强类型结构：它是上游协议的
// 形状，不是 Sirius 的领域模型，没必要在 Go 里再建模一遍。
type ResponseSchema struct {
	// Name 是 schema 名，strict 模式要求提供。
	Name string
	// Schema 是 JSON Schema 本体（object 根）。
	Schema json.RawMessage
}

// ChatResponse 是一次 LLM 调用的结果。
type ChatResponse struct {
	Text string
	// ToolCalls 是模型决定调用的工具（可能为空）。
	//
	// 与 Text 可以同时非空：实测模型会一边叙述（"有点无聊，看看手机"）
	// 一边给出工具调用。叙述进意识流，工具调用才改变世界——两者都要，
	// 不能二选一。
	ToolCalls []ToolCall
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
	CallCount int // LLM 调用次数（测试与 SSE 观测用）

	states    map[StateName]State
	order     []State // 稳定顺序，保证候选清单与日志顺序一致
	enteredAt Tick
	// decideAt 是下次该问 LLM"要不要换"的时刻。
	//
	// 这是"检查点"机制的载体：**由 LLM 自己指定**（stay/enter_state 的
	// for_ticks），而不是框架在区间里随机取一个。它回答的正是
	// "这次问我，下次什么时候再问"。
	decideAt Tick
	// pending 是攒下的"为什么现在问你"（见 decide.go 的 Trigger）。
	// 决策完成后清空；期间新到的理由会累加，让模型一次看到全部。
	pending       []Trigger
	cooldownUntil map[StateName]Tick
	events        chan Event
	chatter       Chatter
	log           *slog.Logger
	lastRecord    DispatchRecord
	thinking      context.CancelFunc // 在途 LLM 调用的取消函数，nil 表示空闲
	observe       func(Snapshot)
	attention     Attention
	ticker        Ticker
	moodRates     MoodRates
	sink          MessageSink
	// inFlightState 记录"哪次独白是为哪个状态发起的"。
	// 与 thinking 区分：thinking 只管有没有在途调用，这个管结果该
	// 归给谁。没有它，一次独白的结果可能在换状态之后才回来，
	// 从而被误记到新状态名下。
	inFlightState StateName
	// monologueOn 为真时，进入状态会异步发起一次内心独白。
	monologueOn bool
	// lastMonologueAt 是上次发起独白的 tick，用于节流。
	//
	// 配一个 bool 而不是拿 0 当"没发过"：tick 0 是完全合法的起始
	// 时刻（StartTick 可为 0），用 0 当哨兵会让节流在最开始失效。
	lastMonologueAt Tick
	monologueSent   bool
	// monologueEvery 是两次独白之间的最小间隔（tick）。
	monologueEvery Tick
	// ctxLimits 是上下文装配的条数上限。
	ctxLimits ContextLimits
	// selfModel 是常驻的自我认知（memory.md §6.2）。
	selfModel []string
	// dredge 按查询词打捞记忆（memory.md §5.1）。
	dredge func(query []string, now Tick) []string
}

// Options 是构造 Agent 的参数。
type Options struct {
	Name    string
	States  []State
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
	// Sink 收下外部消息，返回是否即时打断。为 nil 时消息只走
	// 意识流、不进记忆层（离线测试用）。
	//
	// 由 memory.Store 隐式满足（见 message.go）。装配了它，消息才会
	// 真正进入 unread 队列与待选区——否则记忆链路整条是空的。
	Sink MessageSink
	// Monologue 为真时，每次进入状态异步发起一次内心独白，
	// 让意识流由 LLM 生成而不是写死的旁白。
	//
	// 注意它不改变 R4：独白是异步的，结果以事件回到 agent，
	// 期间状态机照常前进、照常可被抢占。
	Monologue bool
	// MonologueEvery 是两次独白之间的最小 tick 间隔（成本闸门）。
	//
	// 0 = 用默认间隔（defaultMonologueEvery）；负值 = 不节流
	// （每次进入状态都调用，调试用）。状态平均只停留几 tick，
	// 不节流会变成每秒一次 LLM 调用。
	MonologueEvery Tick
	// ContextLimits 是上下文各段条数上限。零值用 DefaultContextLimits()。
	ContextLimits ContextLimits
	// SelfModel 是常驻 prompt 的自我认知（memory.md §6.2）。
	SelfModel []string
	// Dredge 按查询词打捞记忆（memory.md §5.1）。为 nil 则不打捞。
	//
	// now 是当前 tick：打捞要刷新记忆曲线，而记忆只认 tick（R8）。
	Dredge func(query []string, now Tick) []string
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
		Name:           opt.Name,
		states:         states,
		order:          opt.States,
		Now:            opt.StartTick,
		events:         make(chan Event, 64),
		chatter:        opt.Chatter,
		log:            logger.With("agent", opt.Name),
		cooldownUntil:  map[StateName]Tick{},
		observe:        opt.Observe,
		attention:      opt.Attention,
		ticker:         opt.Ticker,
		moodRates:      rates,
		sink:           opt.Sink,
		monologueOn:    opt.Monologue,
		monologueEvery: opt.MonologueEvery,
		ctxLimits:      opt.ContextLimits,
		selfModel:      opt.SelfModel,
		dredge:         opt.Dredge,
		Mood:           Mood{Energy: 80, Annoyed: 0, Curious: 60},
	}
	if a.ctxLimits == (ContextLimits{}) {
		a.ctxLimits = DefaultContextLimits()
	}
	// 初始状态由配置指定，不走决策：开局就问 LLM 会让启动依赖网络，
	// 而她"醒来时在哪"本来就该是人格设定的一部分。
	a.enter(initial, "init", "初始状态", a.states[initial].MinTick)
	return a, nil
}

// Events 返回事件投递通道。这是外部影响 agent 的唯一入口（R1）。
func (a *Agent) Events() chan<- Event { return a.events }

// LastRecord 返回最近一次分派记录（R6 日志的内容）。
func (a *Agent) LastRecord() DispatchRecord { return a.lastRecord }

// PlannedDuration 返回当前状态本次计划的停留时长。
//
// 在 OnEnter 里可用：enter() 先算好 decideAt 再调 OnEnter，因此
// 状态的副作用能按"这次要待多久"来定成本，而不是每次进入扣一个
// 与时长无关的固定值。
//
// 注意它只是**计划**：LLM 可以中途改主意（`stay` 会重设 decideAt，
// 退出条件成立会提前问）。因此逐 tick 计费的 EnergyPerTick 才是
// 主渠道，这里的值只适合"进入时的一次性成本"。
func (a *Agent) PlannedDuration() Tick { return a.decideAt - a.Now }

// enter 进入一个状态：执行 onExit/onEnter、设置检查点、记冷却。
//
// `why` 与 `forTicks` 由 LLM 给出（forTicks 已夹紧）。**不含随机**：
// 停留多久由模型决定，框架只保证它落在 [MinTick, MaxTick] 内。
func (a *Agent) enter(name StateName, reason, why string, forTicks Tick) {
	prev := a.Current
	// 候选快照必须在改动 Current **之前**取：survey 是拿 a.Current
	// 去判断"哪个是当前状态"的，改完再取会把新状态标成"就是现在待着的"。
	cands := a.survey()
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
	a.decideAt = a.Now + forTicks

	if s := a.states[name]; s.OnEnter != nil {
		s.OnEnter(a)
	}
	// 独白在 OnEnter **之后**发起：这样上下文里已经包含"我刚进入
	// 这个状态"这条动作记录，模型才知道自己正在做什么。
	// 它不阻塞：think 只起 goroutine，结果以事件回来（R4）。
	//
	// 位置很关键——它必须是**进入状态**时的一次性动作，不能挪到
	// Step 里逐 tick 调用。后者在节流关掉时（MonologueEvery 为负）
	// 会变成每 tick 一次 LLM 调用：既贵，又会让在途通道永远占满，
	// 决策再也插不进去，人格于是卡死在初始状态。
	a.monologue()

	// prev=="" 是构造时的初始进入：那不是一次"转移"，没有复盘价值，
	// 因此不写记录（否则日志里会出现一条 from="" 的假转移）。
	if prev != "" {
		a.lastRecord = DispatchRecord{
			From: prev, To: name, Reason: reason, Why: why,
			ForTicks: forTicks, Candidates: cands, Seq: a.Now,
		}
		a.logDecision(a.lastRecord)
	}
}

// monologue 在启用时异步发起一次内心独白。
//
// 调用时机是**进入状态**（由 enter 调用），不是每 tick：独白回答的是
// "刚进入这个状态时我在想什么"，逐 tick 问既贵又会把在途通道占满，
// 让决策永远插不进来。
//
// 失败静默：独白是锦上添花，起不来（已有在途调用、未配 Chatter）
// 不该影响状态机。LLM 调用本身的失败以 EventLLMFailed 回到意识流。
//
// 与决策共用同一条在途通道（a.thinking）：一次只允许一个 LLM 调用在途。
// 决策优先——`consider` 发现已有在途调用时会留着理由下个 tick 再问，
// 而独白发现被占用就放弃（它只是锦上添花）。
func (a *Agent) monologue() {
	if !a.monologueOn || a.chatter == nil {
		return
	}
	// 节流：状态平均只停留几 tick，逐次独白会变成每秒一次 LLM 调用
	// ——既贵又没意义（上下文几乎没变）。0 = 用默认间隔，负数 = 不节流。
	every := a.monologueEvery
	if every == 0 {
		every = defaultMonologueEvery
	}
	if every > 0 && a.monologueSent && a.Now-a.lastMonologueAt < every {
		return
	}
	// context.Background()：独白不绑定某次 tick，生命周期由
	// cancelThinking 管（取消在 Run 里，见 R4）。
	err := a.think(context.Background(), ChatRequest{
		Prompt: a.Context(SiteMonologue, a.contextOptions()),
		Schema: &monologueSchema,
	}, thinkMonologue)
	if err != nil {
		// 起不来（多半是上一次调用还在途）：**绝不能**改写
		// inFlightState —— 那会把在途结果错记到刚进入的这个状态名下。
		return
	}
	a.lastMonologueAt = a.Now
	a.monologueSent = true
}

// defaultMonologueEvery 是两次独白之间的最小间隔。
//
// 30 游戏分钟 = 按 tick 算的半小时。理由：够让上下文真的发生变化
// （刷了几次手机、干了一会活），又把调用量压到一天几十次这个量级。
// 想更密/更疏就调它，或给 Options.MonologueEvery 传值。
const defaultMonologueEvery Tick = 30

// contextOptions 组装 Context 的参数。
func (a *Agent) contextOptions() ContextOptions {
	return ContextOptions{
		Limits:    a.ctxLimits,
		SelfModel: a.selfModel,
		Dredge:    a.dredge,
	}
}

// logDecision 输出 R6 要求的结构化决策日志。
//
// 内容与旧的 logTransition 不同：**没有随机数**（不再有抽取），取而代之
// 的是 LLM 说的话（why）与当时摆给它的完整清单（含被挡掉的及原因）。
// 这才是现在唯一能复盘"为什么换了这个状态"的依据——决定不在框架里，
// 在模型的理由里。因此 why 必须落盘，不能只进意识流。
func (a *Agent) logDecision(r DispatchRecord) {
	a.log.Info("state_decision",
		slog.String("from", string(r.From)),
		slog.String("to", string(r.To)),
		slog.String("reason", r.Reason),
		slog.String("why", r.Why),
		slog.Int64("for_ticks", int64(r.ForTicks)),
		slog.Int64("seq", int64(r.Seq)),
		slog.Any("candidates", r.Candidates),
	)
}

// appendStream 写一条意识流，维持有界（R5）。
//
// 默认类型是 KindThought：老的调用点（状态旁白）语义上都是"心里闪过
// 一句"，用 Thought 最贴近。需要别的类型用 appendStreamKind。
func (a *Agent) appendStream(text string) {
	a.appendStreamKind(KindThought, text)
}

// appendStreamKind 写一条指定类型的意识流，维持有界（R5）。
func (a *Agent) appendStreamKind(kind Kind, text string) {
	a.Stream = append(a.Stream, StreamEntry{
		Seq: a.Now, Kind: kind, State: a.Current, Text: text,
	})
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
	// 未读堆积是烦躁的**来源**（memory.md §2.2），紧接衰减之后叠加：
	// 先消气再看红点，语义是"这一刻手机上有多少条未读让我多烦"。
	a.reactToUnread()
}

// Step 前进一个 tick。这是业务逻辑的唯一时间入口（R8）。
//
// 顺序：先处理到达的事件，再推时间，泵一次订阅，最后看是否该问 LLM
// "要不要换"。**随机不再介入任何一步**：该不该换、换成什么、待多久
// 全部由模型决定，框架只负责夹紧与挡掉不可选项。
func (a *Agent) Step(ctx context.Context) {
	a.drainEvents(ctx)
	a.Now++
	a.decayMood()
	a.chargeState()
	// 记忆层跟着时间前进（衰减/升格/遗忘全按 tick 判定，R8）。
	// 放在这里而不是决策之后：即使本次要换状态，记忆也该按
	// 新 tick 结算，两者没有先后依赖。
	if a.ticker != nil {
		a.ticker.Tick(a.Now)
	}
	// 订阅泵入（"状态持续"的机制落点）。放在决策**之前**：
	// 这样本 tick 到达的消息能参与这次是否该换状态的判断。
	// 它必须每 tick 都跑，否则"刷手机时来了新消息"就看不见了。
	a.Pump()

	// 决策点：到点了就问一次 LLM。它异步发起，不阻塞本 tick（R4）。
	a.consider(ctx)
	a.emit()
}

// chargeState 按当前状态逐 tick 收精力成本。
//
// 逐 tick 而不是进入时一次扣清：停留时长由 LLM 决定，"stay 三次续了
// 很长"与"待了很久"应当一样累。见 State.EnergyPerTick。
func (a *Agent) chargeState() {
	s := a.states[a.Current]
	if s.EnergyPerTick == 0 {
		return
	}
	a.Mood.Energy -= s.EnergyPerTick
	a.Mood.Clamp()
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

// handleEvent 处理单个事件。
//
// **没有任何事件能抢占状态。** @我 曾经硬编码切到 scrolling_phone，
// 那是错的：QQ 里 @ 的语义是"提醒更显眼"，不是"掐断你手上的事"。
// 现在消息只做两件事——进记忆层、留一条观察——是否因此做事由状态
// 自己的订阅与退出条件决定（docs/memory.md §2.1）。
func (a *Agent) handleEvent(ctx context.Context, ev Event) {
	switch ev.Kind {
	case EventUserMessage:
		// 先入记忆层（unread 队列的起点），再留一条**不含正文**的观察。
		// 是否读得到正文取决于当前状态是否订阅 QQ：订阅了由 Step 的
		// Pump 泵入，没订阅就只是红点数字。
		a.ingestMessage(ev)
	case EventLLMDone:
		a.settleThinking()
		a.CallCount++
		a.absorbLLM(ev.Data)
	case EventLLMFailed:
		a.settleThinking()
		// 失败原因必须进结构化日志，**不能**只留在意识流里。
		// 意识流是 LLM 读的，那里只能写"话到嘴边没想出来"这种人话；
		// 而排障需要的是上游原文。曾经因为把错误吞掉，容器里所有独白
		// 静默失败、只看到一堆"没想出来"，只能去翻 AMKR 的日志文件
		// 才知道是 unified-model 解析不到（R10：不重试，但要能看见）。
		var p struct {
			Kind string `json:"kind"`
		}
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &p)
		}
		a.appendStreamKind(KindThought, "话到嘴边没想出来")
		a.log.Warn("llm_failed",
			slog.String("kind", p.Kind),
			slog.String("state", string(a.Current)),
			slog.String("err", llmError(ev.Data)),
		)
		// 决策调用失败要**降级**，否则这次询问就永远丢了：没人再问，
		// 人格会卡在这个状态里出不去。降级成"原样再待一会"，下次再问。
		if p.Kind == string(thinkDecision) {
			a.commitStay(defaultStayTick, "没想出来，先按原样待着")
		}
	default:
		a.log.Warn("unknown_event", slog.String("kind", string(ev.Kind)))
	}
	_ = ctx
}

// ingestMessage 把外部消息交给记忆层。
//
// 这是记忆链路的起点：没有这一步，unread 队列、待选区、打捞、
// Shadow 在真实运行中永远是空的。
//
// **刻意不把消息正文写进意识流**：§2.2 规定内容只在订阅了 QQ 的
// 状态里被读取（由 Pump / ReadPhone → Scan 走可见性门控）。这里若顺手
// 把正文记下来，QQ 门控就被旁路掉了——不论在哪个状态，消息内容都会
// 出现在 prompt 里。因此这里只留一条**不含内容**的提示。
//
// @我 与回复我的差别只体现在**这条提示的显眼程度**上：真人手机上
// @ 是弹通知、普通群消息只是红点数字，但两者都不打断你正在做的事。
func (a *Agent) ingestMessage(ev Event) {
	m := decodeMessage(ev.Data)
	m.Tick = a.Now
	// 状态只能由 agent 自己的 goroutine 改（R1），入队是记忆层的事。
	if a.sink != nil {
		a.sink.Accept(m)
	}
	if m.MentionsMe || m.RepliesToMe {
		a.appendStreamKind(KindObservation, "有人叫我")
		return
	}
	a.appendStreamKind(KindObservation, "有新消息，但没叫我")
}

// llmError 从 EventLLMFailed 的负载里取出错误原文。
//
// 负载可能为空或非法（测试里直接投 Event{Kind: EventLLMFailed}），
// 此时给一个占位串而不是空字符串，免得日志里出现空的 err 字段。
func llmError(data json.RawMessage) string {
	var p struct {
		Error string `json:"error"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &p)
	}
	if p.Error == "" {
		return "(无错误详情)"
	}
	return p.Error
}

// absorbLLM 按调用种类分发一次 LLM 结果。
//
// 分类在这里而不是在 think 里：结果必须回到 agent 自己的 goroutine
// 才能改状态（R1），分发是 agent 的事。
func (a *Agent) absorbLLM(data json.RawMessage) {
	var res llmResult
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res)
	}
	// 模型常常一边叙述一边调工具。叙述先落进意识流，**两种调用都这样**：
	// 这条叙述是"她当时在想什么"的直接证据，丢了人格就只剩状态跳变。
	if strings.TrimSpace(res.Text) != "" {
		a.absorbNarration(res.Text)
	}
	switch res.Kind {
	case thinkDecision:
		a.applyDecision(res)
	case thinkMonologue:
		// 独白只写意识流，不改状态。已经由 absorbNarration 处理。
		if strings.TrimSpace(res.Text) == "" {
			a.appendStreamKind(KindObservation, "想了半天，没想出什么")
		}
	default:
		a.log.Warn("llm_result_unknown_kind", slog.String("kind", string(res.Kind)))
	}
}

// absorbNarration 把一段叙述解析成带类型的意识流记录。
//
// 解析在此进行而不是在 LLM 侧：类型是**状态机**的概念（它决定
// 上下文怎么装配），LLM 只负责产出带前缀的文本。
func (a *Agent) absorbNarration(text string) {
	entries := parseMonologue(text)
	if len(entries) == 0 {
		return
	}
	// 结果可能晚于状态切换才回来：回填发起时的状态，否则"在刷手机时
	// 想的事"会被记成"干活时想的事"，意识流会失真。
	state := a.inFlightState
	if state == "" {
		state = a.Current
	}
	for _, e := range entries {
		a.Stream = append(a.Stream, StreamEntry{
			Seq: a.Now, Kind: e.Kind, State: state, Text: e.Text,
		})
	}
	if len(a.Stream) > streamLimit {
		a.Stream = a.Stream[len(a.Stream)-streamLimit:]
	}
}

// Run 是 agent 的主循环（R1：一个 agent 一个 goroutine）。
//
// clock 每收到一次值就前进一个 tick —— 真实时间与游戏时间的换算
// 只在 tick 源做一次（R8），业务逻辑里没有 time.Now()。
func (a *Agent) Run(ctx context.Context, clock <-chan struct{}) error {
	for {
		select {
		case <-ctx.Done():
			// 关停时取消在途调用，别让它跑完 30 秒再丢弃（R4）。
			a.cancelThinking()
			return ctx.Err()
		case <-clock:
			a.Step(ctx)
		case ev := <-a.events:
			// 事件立即处理，不必等到下一 tick。
			a.handleEvent(ctx, ev)
			a.emit()
		}
	}
}
