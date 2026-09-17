package fsm

// workEnergyPerTick 是"干活"每个 tick 的精力成本。
//
// 与状态的基础衰减叠加：基础衰减 100/1440 表示"活着就消耗"，
// 这里表示"干活额外消耗"。取 0.02/1 时，一天干约 540 tick 的活
// 额外消耗约 11 点——量级刻意小于基础衰减，因为 MVP 的"工作"
// 只是坐在那里，不该比活着本身贵好几倍。
//
// 这个数字与 DefaultMoodRates().Energy 共同决定精力能否收支平衡
// （唯一回复手段是睡觉）。调任何一边都要重跑 TestMoodEconomyBalanced。
const workEnergyPerTick = 0.02

// sleepEnergyPerTick 是睡觉每 tick 恢复的精力（毛值，基础衰减另算）。
//
// 净回复 = 0.5 - 100/1440 ≈ 0.43/tick，因此从 Guard 的下限 35 睡到
// Until 的 90 约需 128 tick，落在 [MinTick 60, MaxTick 180] 内。
// 这是"睡觉真的补得回来"的全部依据：若净回复 <= 0，精力会钉在 0，
// 人格失去"累"的层次，而所有单测照样通过。
const sleepEnergyPerTick = 0.5

// MVPStates 返回 MVP 的四个状态（见 docs/roadmap.md §2）。
//
// 每个状态在这里声明三件事，也就是框架对 LLM 的**全部**约束：
//   - Guard/Cooldown：能不能进（模型看不到不满足的选项）
//   - MinTick/MaxTick：能待多久（模型给的时长被夹进这个区间）
//   - Until：什么时候"该走了"（作为理由注入 prompt，不替模型做决定）
//
// 除此之外的一切——换成什么状态、待多久、要不要继续——都由 LLM 决定。
// 状态表里**没有权重**：不再有"按概率被抽中"这回事（R3 修订）。
func MVPStates() []State {
	return []State{
		{
			Name:     "scrolling_phone",
			MinTick:  3,
			MaxTick:  10,
			Cooldown: 2,
			Suggests: []string{"刷手机", "看 QQ", "翻看之前的聊天"},
			// 订阅 QQ：看的到消息，**且在整个驻留期间持续看到**
			// （Step 每 tick 泵一次）。这是"状态 = 一段订阅"的落点。
			Channels: []Channel{ChanQQ},
			// 刷太久就该干点别的了。这条判断交给状态做（它知道"刷了
			// 多久"），是否真的走交给 LLM —— 见 State.Until。
			Until: func(a *Agent) bool {
				return a.Now-a.enteredAt >= 8
			},
			OnEnter: func(a *Agent) {
				a.appendStreamKind(KindAction, "拿起手机刷一刷")
				// 进入时"解锁扫一眼"：显式读一次，会如实写下
				// "没有新消息"。之后靠 Pump 静默泵入增量。
				a.ReadPhone(defaultFeedLimit)
			},
		},
		{
			Name:     "idle",
			MinTick:  2,
			MaxTick:  6,
			Cooldown: 1,
			Suggests: []string{"发呆", "什么都不做"},
			// 发呆本就不该久留，但没有"待够"的硬道理——所以不给 Until，
			// 让检查点（LLM 自己定的 for_ticks）来问它。
			OnEnter: func(a *Agent) {
				a.appendStream("有点无聊")
			},
		},
		{
			Name:     "working",
			MinTick:  4,
			MaxTick:  12,
			Cooldown: 2,
			Suggests: []string{"工作", "写东西"},
			// 干活越久越累：逐 tick 计费，而不是进入时扣一个固定值。
			// 固定值曾经让精力长期为 0（一天进入几十次，每次扣 5），
			// 与"实际干了多久"毫无关系。见 TestMoodEconomyBalanced。
			EnergyPerTick: workEnergyPerTick,
			// 累了就该歇着。这是"心境 → 提示"的落点（R2 单向）。
			Until: func(a *Agent) bool {
				return a.Mood.Energy < 30
			},
			OnEnter: func(a *Agent) {
				a.appendStreamKind(KindAction, "开始干活")
			},
		},
		{
			Name: "sleeping",
			// 睡觉不能靠"想去"：全靠 Guard（困了）与 Until（睡饱了）约束。
			MinTick:  60,
			MaxTick:  180,
			Cooldown: TicksPerDay / 2, // 睡过之后半天内不再想睡
			// 刻意**不订阅** QQ：睡着时手机响了也不看。这比旧的
			// `Uninterruptible` 更准确——被门控的是信息，不是抢占规则。
			Guard: func(a *Agent) bool {
				// 作息：真的困了才睡（精力 < 35），或已过午夜仍醒着。
				// 不要把"夜里"写成无条件的真值：那会让守卫在整个夜间
				// 恒为真，睡醒后立刻又想睡（曾经一天睡 12 次）。
				return a.Mood.Energy < 35 || (Hour(a.Now) >= 1 && Hour(a.Now) < 5)
			},
			// 睡饱了就该醒。这条 Until 取代了旧实现里"靠随机 MaxTick
			// 到点"的醒来方式——那时"精力恢复了"根本无法表达。
			Until: func(a *Agent) bool {
				return a.Mood.Energy >= 90
			},
			// 逐 tick 回复，而不是退出时一次加满：MaxTick 可能被夹紧、
			// 也可能到点被叫醒，"睡了个短的"就该只补回一点。
			EnergyPerTick: -sleepEnergyPerTick,
			OnEnter: func(a *Agent) {
				a.appendStreamKind(KindAction, "困了，睡一会")
			},
		},
	}
}
