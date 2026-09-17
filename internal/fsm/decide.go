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
func (a *Agent) due() []Trigger {
	s := a.states[a.Current]
	if a.Now < a.enteredAt+s.MinTick {
		return nil
	}
	var out []Trigger
	if a.Now >= a.decideAt {
		out = append(out, Trigger{
			Kind: TriggerDwell,
			Text: fmt.Sprintf("上次说好 %d tick 后再问你，现在到点了。", a.decideAt-a.enteredAt),
			At:   a.Now,
		})
	}
	if s.Until != nil && s.Until(a) {
		out = append(out, Trigger{
			Kind: TriggerUntil,
			Text: untilHint + "。",
			At:   a.Now,
		})
	}
	if a.Now >= a.enteredAt+s.MaxTick {
		out = append(out, Trigger{
			Kind: TriggerMax,
			Text: fmt.Sprintf("已经在「%s」待了 %d tick，到上限了，必须做个决定。",
				stateLabel(a.Current), a.Now-a.enteredAt),
			At: a.Now,
		})
	}
	return out
}

// consider 在决策点问一次 LLM（R4：异步，不阻塞 tick）。
//
// 调用期间人格**继续待在原状态**、继续泵入订阅的消息——这正是
// "状态是一段订阅、不是一次跳跃"的落点。
func (a *Agent) consider(ctx context.Context) {
	triggers := a.due()
	if len(triggers) == 0 {
		return
	}
	// 攒着而不是覆盖：多个入口同 tick 成立时，模型应当一次看到全部理由。
	a.pending = append(a.pending, triggers...)
	if a.thinking != nil {
		// 已有在途调用：理由留着，等它落定后下个 tick 再问。
		return
	}
	req := ChatRequest{
		Prompt: a.Context(SiteDispatch, a.contextOptions()),
		Tools:  decisionTools(a.Current, a.survey()),
	}
	if err := a.think(ctx, req, thinkDecision); err != nil {
		// 起不来（没配 Chatter、或已有在途调用）。降级为"原样再待一会"，
		// 理由保留到下次。
		a.log.Warn("decision_not_started", "err", err.Error())
		a.commitStay(defaultStayTick, "没想出来，先按原样待着")
	}
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
	a.decideAt = a.Now + ticks
	a.pending = nil
	a.lastRecord = a.stayRecord(why, ticks)
	a.logDecision(a.lastRecord)
}

// commitEnter 决定进入新状态。
//
// 返回错误时**不改动任何状态**：调用方负责降级（见 applyDecision）。
// 校验放在这里而不是只靠 schema 的 enum：模型仍可能回一个不在
// 候选里的名字（路由忽略 schema 时必然如此），那时必须挡住而不是照做。
func (a *Agent) commitEnter(name StateName, want Tick, why string) error {
	if _, ok := a.states[name]; !ok {
		return fmt.Errorf("fsm: 状态不存在: %s", name)
	}
	if !a.inMenu(name) {
		return fmt.Errorf("fsm: 状态 %s 此刻不可进入", name)
	}
	ticks := a.clampTicks(name, want)
	a.pending = nil
	a.enter(name, "llm", why, ticks)
	return nil
}

// applyDecision 处理一次决策调用回来的结果。
//
// **只有第一次被识别的工具调用生效。** 模型偶尔会一次回多个调用，
// 挑一个执行比合并执行可预测得多；合并会让"最后到底进了哪个状态"
// 取决于遍历顺序。
func (a *Agent) applyDecision(res llmResult) {
	for _, c := range res.Calls {
		switch c.Name {
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
	// 一个可识别的调用都没有：模型可能只回了散文（路由静默忽略 tools
	// 时就是这样）。降级为原样再待一会，**绝不**随机挑一个状态。
	a.log.Warn("decision_unusable", "calls", len(res.Calls))
	a.commitStay(defaultStayTick, "没想清楚要做什么，先按原样待着")
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
	return b.String()
}
