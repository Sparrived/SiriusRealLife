package fsm

// StateName 是状态的稳定标识，会进日志与 SSE。
type StateName string

// Channel 标识一条"信息通道"。
//
// 状态通过**订阅**通道来决定自己能持续看到什么。这是"信息可见性"
// 的载体（R7 修订）：被门控的是信息，不是工具。
type Channel string

const (
	// ChanQQ 是 QQ 消息通道。只有订阅它的状态读得到消息正文。
	ChanQQ Channel = "qq"
)

// State 是一个状态的定义。
//
// 状态 = 一段有明确出口的过程：目标 + 订阅的信息 + 退出条件。
// 状态**不携带情绪**（R2）：`ANGRY_WORKING` 是错的。
type State struct {
	Name StateName

	// Weight 是基础权重，分派时参与加权抽取（R3）。
	Weight float64
	// WeightMod 是可选的权重修正，返回一个乘数（省略视为 1）。
	// 这是 R2「心境 → 分派权重」的显式落点：想让某个状态在夜里更
	// 或更不容易被选中，写在这里，而不是把条件塞进 Guard。
	// Guard 管"能不能进"，WeightMod 管"多想去"。
	WeightMod func(*Agent) float64
	// Guard 是前置条件，返回 false 则不进入候选。
	// 为 nil 视为恒真。
	Guard func(*Agent) bool
	// MinTick / MaxTick 是持续时长区间（闭区间语义：达到 MinTick 后才可能换出）。
	MinTick Tick
	MaxTick Tick
	// Cooldown 是冷却期：刚离开该状态后，这么多 tick 内不再被选中。
	Cooldown Tick

	// Channels 是该状态订阅的信息通道。
	//
	// 订阅在**整个驻留期间**生效，由 Step 每 tick 泵一次
	// （见 Agent.PumpInputs），而不是只在进入时读一次。
	//
	// 为什么做成订阅而不是"在 OnEnter 里读一把"：写在 OnEnter 里的
	// 读取只发生一次，于是"刷手机时来了新消息"看不见——除非为它再写
	// 一次读取，漏写就静默失效。订阅把策略（看哪条通道）与机制
	// （什么时候泵）分开，机制集中在 Step，状态作者漏不掉。
	Channels []Channel
	// FeedLimit 是单次泵入的条数上限（R5：上下文必须有界）。
	// 零值走 defaultFeedLimit。
	FeedLimit int

	// Suggests 是建议动作，进 prompt 提示"现在适合做什么"。
	// **不是白名单**：模型可在意外情境下用别的工具（R7 修订）。
	Suggests []string
	// Blocks 是例外：明确要硬封锁的工具，须有安全/一致性理由。
	Blocks []string

	// OnEnter / OnExit 是进入/退出时的副作用。
	// 只允许改心境与写意识流，不准改状态机结构（R2）。
	OnEnter func(*Agent)
	OnExit  func(*Agent)
}

// Subscribes 报告该状态是否订阅了某条通道。
func (s State) Subscribes(c Channel) bool {
	for _, x := range s.Channels {
		if x == c {
			return true
		}
	}
	return false
}

// moodWeight 返回该状态在当前心境下的实际权重（R2：心境 → 权重，单向）。
//
// 只做两件事：应用 WeightMod（若有），再按精力做统一衰减。
// 具体人格的取舍写在状态表的 WeightMod 里，不藏在这里。
func (s State) moodWeight(a *Agent) float64 {
	w := s.Weight
	if s.WeightMod != nil {
		w *= s.WeightMod(a)
	}
	// 精力低于 30 时，权重按比例衰减：越累越不容易进入高耗状态。
	// 这是对"所有状态"的统一约束，而不是某个状态的个性。
	if a.Mood.Energy < 30 {
		w *= 0.5
	}
	return w
}
