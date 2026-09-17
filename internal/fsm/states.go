package fsm

// workEnergyPerTick 是"干活"每个 tick 的精力成本。
//
// 与状态的基础衰减叠加：基础衰减 100/1200 表示"活着就消耗"，
// 这里表示"干活额外消耗"。取 0.02/1 时，一天干约 540 tick 的活
// 额外消耗约 11 点——量级刻意小于基础衰减，因为 MVP 的"工作"
// 只是坐在那里，不该比活着本身贵好几倍。
//
// 这个数字与 DefaultMoodRates().Energy 共同决定精力能否收支平衡
// （唯一回复手段是睡觉）。调任何一边都要重跑 TestMoodEconomyBalanced。
const workEnergyPerTick = 0.02

// MVPStates 返回 MVP 的四个状态（见 docs/roadmap.md §2）。
//
// QQ 可见性：只有 scrolling_phone 能看到 QQ 消息。其余状态消息静默入队。
// 注意 read_qq 工具在别的状态下**依然可用**——被门控的是信息，不是工具（R7 修订）。
func MVPStates() []State {
	return []State{
		{
			Name:     "scrolling_phone",
			Weight:   30,
			MinTick:  3,
			MaxTick:  10,
			Cooldown: 2,
			Suggests: []string{"刷手机", "看 QQ", "翻看之前的聊天"},
			// 看 QQ 就发生在这里：只有这个状态可见 QQ 消息。
			Visibility: Visibility{QQ: true},
			OnEnter: func(a *Agent) {
				a.appendStream("拿起手机刷一刷")
				// 进入时"解锁扫一眼"：把最近几条未读放进意识流。
				// 注意读的是**信息**，不是工具——read_qq 之类的工具在
				// 别的状态下依然可用，只是没东西显现（R7 修订）。
				a.ReadPhone(scanOnEnterN)
			},
		},
		{
			Name:     "idle",
			Weight:   25,
			MinTick:  2,
			MaxTick:  6,
			Cooldown: 1,
			Suggests: []string{"发呆", "什么都不做"},
			OnEnter: func(a *Agent) {
				a.appendStream("有点无聊")
			},
		},
		{
			Name:     "working",
			Weight:   35,
			MinTick:  4,
			MaxTick:  12,
			Cooldown: 2,
			Suggests: []string{"工作", "写东西"},
			OnEnter: func(a *Agent) {
				a.appendStream("开始干活")
				// 成本与**这次要干多久**成正比，而不是每次扣一个固定值。
				// 固定值曾经让精力长期为 0：working 一天要进入几十次，
				// 每次扣 5 就是几百点，与"实际工作了多久"毫无关系。
				// 见 TestMoodEconomyBalanced。
				cost := workEnergyPerTick * float64(a.PlannedDuration())
				a.Mood.Energy -= cost
				a.Mood.Clamp()
			},
		},
		{
			Name: "sleeping",
			// 基础权重低：睡觉不是靠"想去"，而是靠困。
			Weight:          4,
			MinTick:         60,
			MaxTick:         180,
			Cooldown:        TicksPerDay / 2, // 睡过之后半天内不再想睡
			Uninterruptible: true,
			Guard: func(a *Agent) bool {
				// 作息：真的困了才睡（精力 < 35），或已过午夜仍醒着。
				// 不要把"夜里"写成无条件的真值：那会让守卫在整个夜间
				// 恒为真，睡醒后立刻又想睡（曾经一天睡 12 次）。
				return a.Mood.Energy < 35 || (Hour(a.Now) >= 1 && Hour(a.Now) < 5)
			},
			WeightMod: func(a *Agent) float64 {
				// 越晚越容易犯困；精力越低越想睡。这是 R2 的"心境 → 权重"。
				m := 1.0
				if h := Hour(a.Now); h >= 22 || h < 6 {
					m *= 3
				}
				if a.Mood.Energy < 50 {
					m *= 2
				}
				return m
			},
			OnEnter: func(a *Agent) {
				a.appendStream("困了，睡一会")
			},
			OnExit: func(a *Agent) {
				a.Mood.Energy = 95
				a.Mood.Clamp()
			},
		},
	}
}
