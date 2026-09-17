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

	// Guard 是前置条件，返回 false 则**不进候选**。
	//
	// 这是框架对 LLM 的硬约束之一：模型看得到的候选只有 Guard 通过的
	// 那些，因此它不可能"选一个此刻根本不能进的状态"。为 nil 视为恒真。
	Guard func(*Agent) bool

	// MinTick / MaxTick 是停留时长的硬边界（对 LLM 的选择做夹紧）。
	//
	// MinTick 是下限：LLM 说"再等 1 tick"也会被抬到 MinTick，
	// 避免状态抖动（一次 3 秒的 LLM 调用只换来 1 tick 的停留很荒唐）。
	// MaxTick 是上限：LLM 想赖着不走会被夹回来，兜住"再也不换状态"。
	//
	// 两者都**不含随机**。旧实现在区间里随机取一个时长，那是"纯随机
	// 会毁掉人格"（R3 旧文）的产物；现在时长完全由 LLM 给出。
	MinTick Tick
	MaxTick Tick

	// Cooldown 是冷却期：刚离开该状态后，这么多 tick 内不进候选。
	//
	// 硬约束，与 Guard 同类。它保证"刚干完的事不会立刻又被选中"，
	// 而不必指望 LLM 记得。
	Cooldown Tick

	// Until 是**建议退出条件**：返回真时，它会作为一条理由注入 prompt，
	// 告诉 LLM"这个状态待够了"。
	//
	// 刻意只是一条提示而不是控制流：状态自己不能决定退出，只有 LLM 能。
	// 因此它返回 bool 而不是"直接切走"——这正是"状态机由 LLM 管理、
	// 框架只做约束"的落点。
	//
	// 有它才谈得上"持续一个状态"：LLM 不必自己算"我刷了多久手机"，
	// 状态把判断结果递到它面前，它只需决定走不走。
	Until func(*Agent) bool

	// EnergyPerTick 是该状态每 tick 的精力成本。
	//
	// 逐 tick 计费而不是进入时一次扣清：停留时长由 LLM 决定，进入那
	// 一刻并不知道最终会待多久；而且"同一状态被 stay 续了三次"在多长
	// 驻留上都该更累。逐 tick 累加天然满足这两点。
	EnergyPerTick float64

	// Channels 是该状态订阅的信息通道。
	//
	// 订阅在**整个驻留期间**生效，由 Step 每 tick 泵一次
	// （见 Agent.Pump），而不是只在进入时读一次。
	//
	// 为什么做成订阅而不是"在 OnEnter 里读一把"：写在 OnEnter 里的
	// 读取只发生一次，于是"刷手机时来了新消息"看不见——除非为它再写
	// 一次读取，漏写就静默失效。订阅把策略（看哪条通道）与机制
	// （什么时候泵）分开，机制集中在 Step，状态作者漏不掉。
	Channels []Channel
	// FeedLimit 是单次泵入的条数上限（R5：上下文必须有界）。
	// 零值走 defaultFeedLimit。
	FeedLimit int

	// Suggests 是**推荐行为**，进 prompt 提示"现在适合做什么"。
	//
	// 这是 R2「心境 → prompt」与"状态给建议"的共同落点：它是给 LLM
	// 的提示而非白名单，模型可在意外情境下做别的选择（R7 修订）。
	Suggests []string

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
