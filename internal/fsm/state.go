package fsm

// StateName 是状态的稳定标识，会进日志与 SSE。
type StateName string

// Visibility 声明该状态下"哪些信息可见"。
//
// 这是 R7 修订后的核心：状态声明的是**信息可见性**，不是工具白名单。
// 不在"看 QQ"状态时，read_qq 之类工具仍然可用，只是没有 QQ 消息显现。
// 被门控的是信息，不是工具。
type Visibility struct {
	// QQ 为真时该状态能看到 QQ 消息。见 docs/memory.md §2。
	QQ bool
}

// State 是一个状态的定义。
//
// 状态 = 一段有明确出口的过程：目标 + 信息可见性 + 退出条件。
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
	// Uninterruptible 为真时高优先级事件**延迟**而非抢占（如 sleeping）。
	Uninterruptible bool

	// Suggests 是建议动作，进 prompt 提示"现在适合做什么"。
	// **不是白名单**：模型可在意外情境下用别的工具（R7 修订）。
	Suggests []string
	// Blocks 是例外：明确要硬封锁的工具，须有安全/一致性理由。
	Blocks []string
	// Visibility 声明该状态下哪些信息可见。
	Visibility Visibility

	// OnEnter / OnExit 是进入/退出时的副作用。
	// 只允许改心境与写意识流，不准改状态机结构（R2）。
	OnEnter func(*Agent)
	OnExit  func(*Agent)
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
