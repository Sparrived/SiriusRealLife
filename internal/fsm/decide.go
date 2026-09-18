package fsm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TriggerKind 标识"为什么现在要问你"。
//
// 为什么要有这个类型：状态的决策时机有**多个入口**——检查点到点、
// 状态自己的退场条件成立、硬上限兜底，将来还会有"计划被卡住""工具
// 回来了"。它们不该各写一条控制流去直接改状态，而应当都只往
// `pending` 里追加一条理由，最后由同一个决策点一并渲染给 LLM。
// 这样加一个新入口不需要碰状态机，只需多一个 TriggerKind。
type TriggerKind string

const (
	// TriggerDwell 到了上次约定的检查点（LLM 自己说的"下次什么时候问我"）。
	TriggerDwell TriggerKind = "dwell"
	// TriggerUntil 状态自己的退场条件成立（State.Until 返回真）。
	TriggerUntil TriggerKind = "until"
	// TriggerMax 到了硬上限，必须做决定。
	TriggerMax TriggerKind = "max"
)

// Trigger 是一条"为什么现在问你"的理由。
type Trigger struct {
	Kind TriggerKind
	// Text 是给人格看的一句话（进 prompt），中文、第一人称语境。
	Text string
	// At 是产生它的 tick，便于排障时看清时序。
	At Tick
}

// 决策工具名。写成常量而不是散落的字面量：这些字符串同时出现在
// 工具声明、解析分支与 prompt 里，写错一处不会编译报错。
const (
	toolStay       = "stay"
	toolEnterState = "enter_state"
	// toolReadApp 是"先看看手机上的内容"。
	//
	// 它**不改状态**：调用它等于对框架说"我信息不够，先看一眼再决定"。
	// 框架于是读内容 → 写入意识流与待选区 → 用刚读到的内容打捞记忆
	// → 发起一轮工具结果调用（SiteToolRead，只负责读懂与回应）
	// → 然后**重新问一次**状态决策。
	//
	// 为什么读的回合要单独一次调用：那一轮才需要记忆。回复一条消息
	// 依赖"之前聊过什么"，而这只有在内容已经摆在眼前时才说得清
	// （memory.md §5.1 的"打捞只在要做动作的调用点发生"）。
	toolReadApp = "read_app"
)

// maxReadApps 是**一次决策里**最多允许读几次 app。
//
// 必须有界：模型可以一直说"再翻一点"，而每读一次要多花两次
// LLM 调用（工具结果轮 + 重问决策）。超出后 read_app 被忽略，
// 模型只能从剩下的选择里挑，或降级为 stay。
const maxReadApps = 2

// 单次翻页的条数：默认值与硬上限。
const (
	defaultReadAppLimit = 10
	maxReadAppLimit     = 30
)

// untilHint 是"状态该结束了"这条理由里固定出现的一句话。
//
// 抽成常量是因为测试要按 prompt 文本判断"这句话出现了没有"——而
// 测试的假 LLM 跑在另一个 goroutine 上，不能直接读 agent 字段（R1）。
// 两处引用同一个常量，改文案不会让测试静默失效。
const untilHint = "该结束的迹象已经出现了"

// defaultStayTick 是"打算不出来时"默认再待多久。
//
// 只在**失败降级**时使用（LLM 调用失败、回复里没有可识别的工具）。
// 它必须存在：没有它，一次失败就意味着永远不再问，人格会卡死在
// 一个状态里。降级到"原样再待一会然后重问"，而不是随便挑一个状态
// ——旧实现用随机数兜底，那是"纯随机会毁掉人格"的老毛病。
const defaultStayTick Tick = 3

// due 返回本 tick 应当追加的决策理由（可能为空）。
//
// 三种入口在这里汇合。MinTick 是硬下限：期间一律不问，避免一次
// 3 秒的 LLM 调用只换来 1 tick 的停留。MaxTick 是硬上限：到点必须问，
// 否则一次失败的降级就可能让人格永远赖在一个状态里。
//
// **条件型入口只在上升沿报一次**（untilReported / maxReported）。
// 这是必须的：Until 与 MaxTick 一旦成立就持续成立，逐 tick 重报会让
// 决策点连轴转，模型填的 for_ticks 形同虚设。实测线上每 3 tick 问一次，
// 而模型填的是 12。检查点（decideAt）本身就是"过一次就消失"的，
// 靠 commitStay/commitEnter 推进它，因此不需要额外标记。
func (a *Agent) due() []Trigger {
	s := a.states[a.Current]
	if a.Now < a.enteredAt+s.MinTick {
		return nil
	}
	var out []Trigger
	// 检查点也是**条件**，不是事件：decideAt 要等 commitStay/commitEnter
	// 才推进，而一次决策要跨好几个 tick（LLM 得先回话）。这期间它每个
	// tick 都成立，因此同样按上升沿去重——否则 pending 里会攒下十几条
	// 一模一样的话，模型看到一堆重复的"到点了"。
	if risingEdge(a.Now >= a.decideAt, &a.dwellReported) {
		out = append(out, Trigger{
			Kind: TriggerDwell,
			Text: fmt.Sprintf("上次说好 %d tick 后再问你，现在到点了。", a.decideAt-a.enteredAt),
			At:   a.Now,
		})
	}

	if risingEdge(s.Until != nil && s.Until(a), &a.untilReported) {
		// 理由要说**具体**，不能只说"该结束了"：模型得知道是什么迹象，
		// 才能判断该不该当真。而 Until 只是一个 bool，说不出原因，因此
		// 这里补上框架**确实知道**的事实——待了多久、安静了多久。
		// 不说状态的内部逻辑（那是状态自己的事），只说外部可观测的量。
		elapsed := a.Now - a.enteredAt
		text := fmt.Sprintf("%s「%s」已经 %d tick 了。",
			untilHint, stateLabel(a.Current), elapsed)
		if quiet, ever := a.QuietFor(); ever && quiet > elapsed {
			text += fmt.Sprintf("而且最近 %d tick 都没有新消息。", quiet)
		}
		out = append(out, Trigger{Kind: TriggerUntil, Text: text, At: a.Now})
	}

	if risingEdge(a.Now >= a.enteredAt+s.MaxTick, &a.maxReported) {
		out = append(out, Trigger{
			Kind: TriggerMax,
			Text: fmt.Sprintf("已经在「%s」待了 %d tick，到上限了，必须做个决定。",
				stateLabel(a.Current), a.Now-a.enteredAt),
			At: a.Now,
		})
	}
	return out
}

// risingEdge 实现"条件由假变真时报一次"：cond 为真且上次不为真时返回
// true 并记下已报；cond 为假时重新武装，下次再变真还能报。
//
// 抽成一个函数是因为这条规则对**每个**决策入口都一样，而漏掉任何一个
// 的后果都很重（见 Agent 里三个 Reported 字段的说明）。R11 说"加一个
// 入口只准往 pending 追加一条理由"——这个函数就是那条理由该怎么追加。
func risingEdge(cond bool, reported *bool) bool {
	if !cond {
		*reported = false
		return false
	}
	if *reported {
		return false
	}
	*reported = true
	return true
}

// consider 在决策点问一次 LLM（R4：异步，不阻塞 tick）。
//
// 调用期间人格**继续待在原状态**、继续泵入订阅的消息——这正是
// "状态是一段订阅、不是一次跳跃"的落点。
func (a *Agent) consider(ctx context.Context) {
	// 先把本 tick 新成立的上升沿收进 pending。
	a.pending = append(a.pending, a.due()...)
	// **由 pending 决定要不要问**，而不是由"本 tick 有没有新理由"。
	// 这一点很关键：due() 现在只在上升沿产出理由，若上一次要问时
	// 正好有别的调用在途，理由会被攒下来而 due() 不再产出——按
	// "本 tick 有没有新理由"判断就会把它永远搁置，模型再也收不到
	// 那条"该走了"。
	if len(a.pending) == 0 {
		return
	}
	if a.thinking != nil {
		// 已有在途调用（多半是独白）：理由留着，等它落定后下个 tick 再问。
		return
	}
	req := ChatRequest{
		Prompt: a.Context(SiteDispatch, a.contextOptions()),
		Tools:  a.decisionTools(),
	}
	if err := a.think(ctx, req, thinkDecision); err != nil {
		// 起不来（没配 Chatter）。降级为"原样再待一会"。
		a.log.Warn("decision_not_started", "err", err.Error())
		a.commitStay(defaultStayTick, "没想出来，先按原样待着")
	}
}

// decisionTools 组装一个状态决策点可用的全部工具。
//
// 候选状态与 read_app 在两个地方分别构造：前者只依赖状态表，
// 后者还依赖"当前状态看不看得见东西"，混在一起会让纯函数变脏。
func (a *Agent) decisionTools() []ToolSpec {
	tools := decisionTools(a.Current, a.survey())
	if t := a.readAppTool(); t != nil {
		tools = append(tools, *t)
	}
	return tools
}

// decisionTools 声明决策可用的工具。
//
// 候选状态写进 state 的 enum：这是**框架约束**的落点——模型在协议
// 层面就无法选到一个此刻不能进的状态，而不必在 prompt 里写"请不要选
// 睡觉"。约束在 schema 里生效比在散文里恳求可靠得多。
//
// current 显式传入而不是从 survey 里反查：survey 的 Blocked 是给人看的
// 中文句子，拿它做字符串匹配来判断"谁是当前状态"是脆的——改一句文案
// 就会静默失效。
func decisionTools(current StateName, survey []Candidate) []ToolSpec {
	names := make([]string, 0, len(survey))
	var blocked []string
	for _, c := range survey {
		if c.Blocked == "" {
			names = append(names, string(c.Name))
			continue
		}
		blocked = append(blocked, fmt.Sprintf("%s：%s", stateLabel(c.Name), c.Blocked))
	}
	enumJSON, _ := json.Marshal(names)

	tickParam := `"for_ticks":{"type":"integer","description":"你希望我下次什么时候再问你，单位 tick（1 tick = 1 游戏分钟）"}`

	stayParams := fmt.Sprintf(
		`{"type":"object","properties":{"why":{"type":"string","description":"为什么还想继续待着"}%s},"required":["why","for_ticks"],"additionalProperties":false}`,
		", "+tickParam)

	enterDesc := "换一个状态。可选：" + strings.Join(names, "、")
	if len(names) == 0 {
		enterDesc = "换一个状态。此刻没有可选的状态。"
	}
	if len(blocked) > 0 {
		enterDesc += "。不合适的：" + strings.Join(blocked, "；")
	}
	enterParams := fmt.Sprintf(
		`{"type":"object","properties":{"state":{"type":"string","enum":%s,"description":"要进入的状态"}%s,"why":{"type":"string","description":"为什么想换过去"}},"required":["state","for_ticks","why"],"additionalProperties":false}`,
		string(enumJSON), ", "+tickParam)

	return []ToolSpec{
		{
			Name: toolStay,
			Description: fmt.Sprintf("继续留在「%s」，什么都不换。想接着做手上的事、或觉得还不到换的时候，用这个。",
				stateLabel(current)),
			Parameters: json.RawMessage(stayParams),
		},
		{
			Name:        toolEnterState,
			Description: enterDesc,
			Parameters:  json.RawMessage(enterParams),
		},
	}
}

// readAppTool 声明"翻开手机先看看"这个工具，**仅当当前状态看得到东西时**。
//
// 返回 nil 表示不提供它——例如睡觉时不订阅任何通道，翻开手机也没内容可看。
// 这是 R7 修订的落点：被门控的是**信息可见性**，不是工具白名单；
// 但看不见信息的场合下把这个工具摆出来，只会诱使模型点一个空动作。
func (a *Agent) readAppTool() *ToolSpec {
	apps := a.visibleApps()
	if len(apps) == 0 {
		return nil
	}
	enumJSON, _ := json.Marshal(apps)
	params := fmt.Sprintf(
		`{"type":"object","properties":{"app":{"type":"string","enum":%s,"description":"看哪个 app"},"n":{"type":"integer","description":"看多少条，默认 %d"}},"required":["app"],"additionalProperties":false}`,
		string(enumJSON), defaultReadAppLimit)
	return &ToolSpec{
		Name: toolReadApp,
		Description: "翻开手机上的 app 看内容（往上翻更早的聊天）。" +
			"信息不够、想先看一眼再决定的时候用它；我会把内容摆到你眼前，" +
			"并把你以前的相关记忆一起想起来，然后重新问你要做什么。",
		Parameters: json.RawMessage(params),
	}
}

// visibleApps 返回当前状态能读到的 app 名（就是它订阅的通道）。
func (a *Agent) visibleApps() []string {
	var out []string
	for _, c := range a.states[a.Current].Channels {
		out = append(out, string(c))
	}
	return out
}

// clampTicks 把 LLM 给的时长夹进状态的硬边界。
//
// 非随机：这是框架的**约束**，不是"在区间里挑一个"。模型说 1 会被抬到
// MinTick（免得状态抖成碎片），说 99999 会被压到 MaxTick（免得一次
// 决定就让人格再也不动）。夹紧结果如实记进日志，便于发现模型老想越界。
func (a *Agent) clampTicks(name StateName, want Tick) Tick {
	s := a.states[name]
	if want < s.MinTick {
		return s.MinTick
	}
	if s.MaxTick > 0 && want > s.MaxTick {
		return s.MaxTick
	}
	return want
}

// inMenu 报告某状态此刻是否可进（Guard 通过、不在冷却、不是当前状态）。
func (a *Agent) inMenu(name StateName) bool {
	for _, c := range a.survey() {
		if c.Name == name && c.Blocked == "" {
			return true
		}
	}
	return false
}

// commitStay 决定继续留在当前状态，并定下下次询问的时刻。
func (a *Agent) commitStay(want Tick, why string) {
	ticks := a.clampTicks(a.Current, want)
	// 不需要手动清 dwellReported：decideAt 一定被推到未来（ticks >= MinTick），
	// 条件随即变假，risingEdge 会在下个 tick 自动重新武装。
	a.decideAt = a.Now + ticks
	a.pending = nil
	a.endReadEpisode()
	a.lastRecord = a.stayRecord(why, ticks)
	a.logDecision(a.lastRecord)
}

// commitEnter 决定进入新状态。
//
// 返回错误时**不改动任何状态**：调用方负责降级（见 applyDecision）。
// 校验放在这里而不是只靠 schema 的 enum：模型仍可能回一个不在
// 候选里的名字（路由忽略 schema 时就会如此），那时必须挡住而不是照做。
func (a *Agent) commitEnter(name StateName, want Tick, why string) error {
	if _, ok := a.states[name]; !ok {
		return fmt.Errorf("fsm: 状态不存在: %s", name)
	}
	if !a.inMenu(name) {
		return fmt.Errorf("fsm: 状态 %s 此刻不可进入", name)
	}
	ticks := a.clampTicks(name, want)
	a.pending = nil
	a.endReadEpisode()
	a.enter(name, "llm", why, ticks)
	return nil
}

// endReadEpisode 清掉"先看一眼"这一轮留下的痕迹。
//
// 一次决策在这里真正结束时才调用（commitStay / commitEnter）：
// 读的次数预算与刚读到的内容都属于**这一次询问**，跨决策留着
// 会让下次的 read_app 预算凭空少一次、打捞查询词带上过期内容。
func (a *Agent) endReadEpisode() {
	a.readApps = 0
	a.pendingRead = nil
}

// applyDecision 处理一次决策调用回来的结果。
//
// **只有第一次被识别的工具调用生效。** 模型偶尔会一次回多个调用，
// 挑一个执行比合并执行可预测得多；合并会让"最后到底进了哪个状态"
// 取决于遍历顺序。
func (a *Agent) applyDecision(ctx context.Context, res llmResult) {
	for _, c := range res.Calls {
		switch c.Name {
		case toolReadApp:
			// "先看一眼再决定"：读完**不改状态**，也不清 pending。
			// 等工具结果轮落定后，Step 里的 consider 会发现 pending
			// 仍非空、且已无在途调用，于是自动重新问一次决策——
			// 那时内容和记忆都已经在意识流里了。
			if a.readApps >= maxReadApps {
				a.log.Warn("read_app_over_budget", "used", a.readApps)
				continue
			}
			if err := a.beginReadApp(ctx, c.Arguments); err != nil {
				// 读不成（app 看不见、参数坏了）：当成"没识别到这个调用"，
				// 交给后面的调用或降级分支，而不是把这次询问丢掉。
				a.log.Warn("read_app_failed", "err", err.Error())
				continue
			}
			a.readApps++
			return
		case toolStay:
			var p struct {
				Why      string `json:"why"`
				ForTicks Tick   `json:"for_ticks"`
			}
			if err := json.Unmarshal(c.Arguments, &p); err != nil {
				a.log.Warn("decision_bad_args", "tool", c.Name, "err", err.Error())
				continue
			}
			a.commitStay(p.ForTicks, p.Why)
			return
		case toolEnterState:
			var p struct {
				State    StateName `json:"state"`
				ForTicks Tick      `json:"for_ticks"`
				Why      string    `json:"why"`
			}
			if err := json.Unmarshal(c.Arguments, &p); err != nil {
				a.log.Warn("decision_bad_args", "tool", c.Name, "err", err.Error())
				continue
			}
			if err := a.commitEnter(p.State, p.ForTicks, p.Why); err != nil {
				a.log.Warn("decision_rejected", "state", string(p.State), "err", err.Error())
				continue
			}
			return
		}
	}
	// 一个可识别的调用都没有：模型可能只回了散文（路由不支持 tools
	// 时就是这样，且**支持与否逐路由不同**）。降级为原样再待一会，
	// **绝不**随机挑一个状态。
	a.log.Warn("decision_unusable", "calls", len(res.Calls))
	a.commitStay(defaultStayTick, "没想清楚要做什么，先按原样待着")
}

// beginReadApp 执行一次 read_app：读内容 → 进意识流与待选区 → 发起工具结果轮。
//
// 三件事的顺序是有意的：
//  1. **先读**。读到的内容进意识流（【刚发生】），也由记忆层自动写入
//     待选区——"看到的都写"，与 Pump 走同一条路。
//  2. **再打捞**。工具结果轮的 prompt 里会出现【想起的事】，查询词就是
//     刚读到的内容（见 dredgeQuery）。这是整个设计里打捞唯一的触发点。
//  3. **最后发问**，且那一轮**不带状态工具**：它只负责读懂与回应，
//     状态决策仍由下一次 SiteDispatch 单独完成（一次决定只改一次状态）。
//
// 返回 error 表示"这一次读不成"，调用方据此降级；它**不**表示读取内容为空
// （翻到尽头是正常结果，会如实写进意识流）。
func (a *Agent) beginReadApp(ctx context.Context, args json.RawMessage) error {
	var p struct {
		App string `json:"app"`
		N   int    `json:"n"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return fmt.Errorf("fsm: read_app 参数无法解析: %w", err)
	}
	// 信息可见性门控：看不见的 app 不给读（R7 修订）。
	if !a.states[a.Current].Subscribes(Channel(p.App)) {
		return fmt.Errorf("fsm: 当前状态看不到 app %q", p.App)
	}
	if a.attention == nil {
		return fmt.Errorf("fsm: 未配置 Attention")
	}

	n := p.N
	if n <= 0 {
		n = defaultReadAppLimit
	}
	if n > maxReadAppLimit {
		n = maxReadAppLimit
	}

	// ponytail: 只有 QQ 一个通道，翻页仍走 BrowsePhone（它自己按 ChanQQ
	// 门控）。多通道时这里要改成按 Channel 分派到各自的历史。
	lines := a.BrowsePhone(n)
	if len(lines) == 0 {
		a.appendStreamKind(KindObservation, "往上翻了翻，没有更早的内容了")
	} else {
		for _, l := range lines {
			a.appendStreamKind(KindObservation, l)
		}
	}
	// 留给 dredgeQuery：打捞要拿"刚读到的内容"当查询词。
	a.pendingRead = lines

	return a.think(ctx, ChatRequest{
		Prompt: a.Context(SiteToolRead, a.contextOptions()),
	}, thinkToolRead)
}

// whyNowText 渲染【为什么现在问你】。
func (a *Agent) whyNowText() string {
	if len(a.pending) == 0 {
		return ""
	}
	lines := make([]string, 0, len(a.pending))
	for _, t := range a.pending {
		lines = append(lines, t.Text)
	}
	return strings.Join(lines, " ")
}

// menuText 渲染【你可以做的选择】。
//
// 被挡掉的状态也写出来并附原因：模型看到"睡觉：此刻不满足进入条件"
// 比看不到它更好——后者会让它反复尝试一个不可能的选项，或者以为
// 框架出了故障。
func (a *Agent) menuText() string {
	survey := a.survey()
	var b strings.Builder
	b.WriteString(fmt.Sprintf("- 继续留在「%s」：调 %s，for_ticks 填你希望我下次什么时候再问你。\n",
		stateLabel(a.Current), toolStay))
	var avail, blocked []string
	for _, c := range survey {
		if c.Blocked == "" {
			avail = append(avail, fmt.Sprintf("%s（%s）", stateLabel(c.Name), c.Name))
		} else if c.Name != a.Current {
			blocked = append(blocked, fmt.Sprintf("%s：%s", stateLabel(c.Name), c.Blocked))
		}
	}
	if len(avail) > 0 {
		b.WriteString(fmt.Sprintf("- 换个状态：调 %s，state 从这些里选：%s。\n",
			toolEnterState, strings.Join(avail, "、")))
	} else {
		b.WriteString("- 此刻没有别的状态可换，只能继续待着。\n")
	}
	if len(blocked) > 0 {
		b.WriteString(fmt.Sprintf("（这些现在不合适：%s）\n", strings.Join(blocked, "；")))
	}
	// 信息不够时可以"先看一眼"：读了之后框架会把内容与相关记忆摆出来，
	// 再重新问一次。只在看得见东西时列出它。
	if apps := a.visibleApps(); len(apps) > 0 {
		b.WriteString(fmt.Sprintf("- 想先看看手机上的内容再决定：调 %s，app 填 %s。\n",
			toolReadApp, strings.Join(apps, "、")))
	}
	return b.String()
}
