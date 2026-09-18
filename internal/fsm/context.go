package fsm

import (
	"fmt"
	"strings"
)

// CallSite 标识"这次 LLM 调用是干什么的"。
//
// 为什么不是一个统一的大 prompt：三个调用点要的东西不同。
// 状态决策只要"我在哪、我什么状态、我能做什么"；内心独白要
// "我最近想了什么、刚发生了什么"；工具解读要"工具返回了什么"。
// 全塞进去只会烧 token 并让模型失焦。共享的是**上下文**（本文件），
// 各自的追问由调用点追加。
//
// 定义在 fsm 而不是 llm：fsm 不 import internal/llm（依赖方向单向），
// 而 Context 需要知道自己被谁调用。llm.CallSite 是它的镜像，
// 由 main 装配处用 adapter 接起来。
type CallSite string

const (
	// SiteMonologue 内心独白：写意识流。
	SiteMonologue CallSite = "monologue"
	// SiteDispatch 状态决策：决定继续待着还是换个状态。
	//
	// 名字保留为 dispatch，但语义已经变了：它不再是"框架抽取一个状态"，
	// 而是"问人格要不要换"。任务是同一个（同一个 AMKR 任务名），
	// 因此不改名以免动部署配置。
	SiteDispatch CallSite = "dispatch"
	// SiteToolRead 工具结果解读。
	SiteToolRead CallSite = "tool_read"
)

// TaskEnv 把调用点映射到它的环境变量名。
//
// 调用点 → 任务名的映射放在这张显式表里（v1 硬编码，允许 env 覆盖单条）。
// 定义在这里而不是 llm：枚举归属 fsm，映射表跟着枚举走，两处才不会漂移。
// 真实模型名不出现（R9）。
func (c CallSite) TaskEnv() string {
	switch c {
	case SiteDispatch:
		return "AMKR_TASK_DISPATCH"
	case SiteMonologue:
		return "AMKR_TASK_MONOLOGUE"
	case SiteToolRead:
		return "AMKR_TASK_TOOL_READ"
	default:
		return ""
	}
}

// ContextLimits 是上下文各段落的条数上限。
//
// 每段都必须有上限：这是 R5（意识流必须有界）在 prompt 侧的落点，
// 且直接影响 token 体积与延迟。零值走 DefaultContextLimits。
type ContextLimits struct {
	// Thoughts 是"最近在想"取的条数。
	Thoughts int
	// Recent 是"刚发生了什么"（观察 + 动作）取的条数。
	Recent int
	// Dredged 是打捞结果条数上限。
	Dredged int
}

// DefaultContextLimits 是初值。
//
// 取值理由：独白要的是"刚才那几句"，5 条足够且便宜；Recent 给到 8 条
// 是因为它混合了观察与动作，条数少会让"我刚干了什么"断片。两者都远
// 小于意识流上限 100 —— prompt 绝不该读到全量（R5）。
func DefaultContextLimits() ContextLimits {
	return ContextLimits{Thoughts: 5, Recent: 8, Dredged: 5}
}

// ContextOptions 是装配参数。
type ContextOptions struct {
	// Limits 是各段条数上限。
	Limits ContextLimits
	// SelfModel 是常驻的自我认知（memory.md §6.2，≤20 条）。
	// 由 memory 层提供；为空则整段省略。
	SelfModel []string
	// Dredge 按查询词打捞记忆（memory.md §5.1）。为 nil 则不打捞。
	//
	// 带上 now：打捞要刷新记忆曲线，而记忆只认 tick（R8）。
	// 让调用方传而不是在装配处闭包捕获，是因为 agent 构造时
	// 还没有自己的 tick —— 闭包捕获会拿到一个永远为 0 的值。
	Dredge func(query []string, now Tick) []string
}

// Context 按调用点组装 LLM 的上下文。
//
// 这是"意识流是唯一贯穿人格活动的 prompt"的落点：所有调用点共享
// 同一个装配结果，再各自追加追问。段落组成（每段可省略）：
//
//	【现在】时刻 / 状态 / 心境
//	【手机】还有几条没看（有未读时才出现）
//	【我是谁】自我模型（常驻）
//	【最近在想】KindThought
//	【刚发生】KindObservation + KindAction
//	【打算】最近一条 KindIntent
//	【想起的事】打捞结果
//	【适合做】当前状态的 Suggests
//
// 只能在 agent 自己的 goroutine 里调用（R1）。
func (a *Agent) Context(site CallSite, opt ContextOptions) string {
	if opt.Limits == (ContextLimits{}) {
		opt.Limits = DefaultContextLimits()
	}
	var b strings.Builder

	fmt.Fprintf(&b, "【现在】%02d:%02d，第 %d 天，正在「%s」。\n",
		Hour(a.Now), int(a.Now%60), int(a.Now/TicksPerDay)+1, a.label())

	fmt.Fprintf(&b, "【心境】精力 %.0f，烦躁 %.0f，好奇 %.0f。\n",
		a.Mood.Energy, a.Mood.Annoyed, a.Mood.Curious)

	// 未读：**作为事实陈述，不作为触发条件**（R12）。
	//
	// 为什么必须有这一段：意识流里"有人叫我"那条只在**到达时**写一次，
	// 而【刚发生】只取最近 8 条。一条 20 tick 前到的消息早就被挤出窗口，
	// 于是模型再也看不到"还有没看的消息"——它就无法产生"要不要去看看
	// 手机"这个念头，只能靠碰巧又来了新消息。未读数是**持续存在的事实**，
	// 必须每轮都告知（对应"如果 QQ 来消息了，应当在提示词告知"）。
	//
	// 订阅了 QQ 的状态（如刷手机）里，Pump 已经在 Step 里把消息泵走、
	// 游标随之推进，因此这里自然为 0、整段不出现——不必特殊处理。
	if n := a.Unread(); n > 0 {
		fmt.Fprintf(&b, "【手机】还有 %d 条消息没看。", n)
		// @我 与回复我单独点出来：这两种在 QQ 里会弹通知，普通群消息
		// 只有一个红点数字。它们不抢占状态（R12），因此"更显眼"只能
		// 落在**理由**上——不说这一句，@ 在没看手机时就等于不存在。
		if m := a.UnreadMentions(); m > 0 {
			fmt.Fprintf(&b, "其中 %d 条提到了你。", m)
		}
		b.WriteString("\n")
	}

	if len(opt.SelfModel) > 0 {
		fmt.Fprintf(&b, "【我是谁】%s\n", strings.Join(opt.SelfModel, "；"))
	}

	// 最近在想：只要 thought。
	if lines := a.recentLines(KindThought, opt.Limits.Thoughts); len(lines) > 0 {
		fmt.Fprintf(&b, "【最近在想】%s\n", strings.Join(lines, " / "))
	}

	// 刚发生：观察与动作合并按时间正序，保持因果顺序。
	if lines := a.recentMixedLines(opt.Limits.Recent); len(lines) > 0 {
		fmt.Fprintf(&b, "【刚发生】%s\n", strings.Join(lines, " / "))
	}

	// 打算：最近一条意图。跨状态存活，见 stream.go 的说明。
	if intent := a.Intent(); intent != "" {
		fmt.Fprintf(&b, "【打算】%s\n", intent)
	}

	// 想起的事：按调用点的查询词打捞（memory.md §5.1）。
	// 查询词用当前状态名 + 意图 + 最近的词，让打捞与"此刻"相关。
	if opt.Dredge != nil {
		if hit := opt.Dredge(a.dredgeQuery(), a.Now); len(hit) > 0 {
			if len(hit) > opt.Limits.Dredged {
				hit = hit[:opt.Limits.Dredged]
			}
			fmt.Fprintf(&b, "【想起的事】%s\n", strings.Join(hit, " / "))
		}
	}

	if s := a.states[a.Current]; len(s.Suggests) > 0 {
		fmt.Fprintf(&b, "【适合做】%s\n", strings.Join(s.Suggests, "、"))
	}

	// 追问：每个调用点的差异只在这里。
	switch site {
	case SiteMonologue:
		// 明确要求 JSON，并把字段名写出来：schema 被路由忽略时
		// （实测 wb2api 就是静默忽略），prompt 是唯一还在起作用的约束。
		// 字段名必须与 monologueSchema 一致，改动要同步。
		b.WriteString("\n以 JSON 回这一段内心活动，只要这三个字段：\n")
		b.WriteString(`{"thought":"此刻在想什么","action":"正在做的动作，没有就填空串","intent":"接下来打算做什么，没有就填空串"}`)
		b.WriteString("\n第一人称、口语、一两句就够。不要复述上面的设定，不要解释，不要客套。")
	case SiteDispatch:
		// 决策点：把"为什么现在问你"和"你能怎么选"讲清楚，然后要求
		// **必须调用工具**。
		//
		// 为什么强调必须调工具：**支持与否逐路由不同**，无视 tools 参数的
		// 路由会让模型只回一段散文。那种回复无法驱动状态，框架会降级为
		// "原样再待一会"——所以 prompt 里再要一次，让支持工具的路由真的
		// 给出工具调用。（实测 wb2api 支持 tools，见 docs/deploy-local.md §8.1。）
		if why := a.whyNowText(); why != "" {
			fmt.Fprintf(&b, "\n【为什么现在问你】%s\n", why)
		}
		b.WriteString("\n【你可以做的选择】\n")
		b.WriteString(a.menuText())
		b.WriteString("\n请**调用一个工具**来表达决定，不要只用文字回答。")
		b.WriteString("先说你此刻在想什么（一两句、第一人称、口语），再调工具。")
		b.WriteString("\n只有你自己能做这个决定：不想换就调 stay，觉得该做别的了就调 enter_state。")
	case SiteToolRead:
		b.WriteString("\n把上面的结果用一句话说成人话。")
	}

	return b.String()
}

// label 返回当前状态的中文名。状态表没有标签字段，用名字兜底。
func (a *Agent) label() string {
	return stateLabel(a.Current)
}

// stateLabel 把状态名映射成给 LLM 看的中文。
//
// 放在这里而不是 State 结构里：状态名是稳定标识（进日志与 SSE），
// 中文只是渲染。加状态时若忘了加标签，退化成名字而不是崩。
func stateLabel(n StateName) string {
	switch n {
	case "scrolling_phone":
		return "刷手机"
	case "idle":
		return "发呆"
	case "working":
		return "干活"
	case "sleeping":
		return "睡觉"
	default:
		return string(n)
	}
}

// recentLines 取最近 n 条指定类型的记录文本（时间正序）。
func (a *Agent) recentLines(k Kind, n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	for i := len(a.Stream) - 1; i >= 0 && len(out) < n; i-- {
		if a.Stream[i].Kind == k {
			out = append(out, a.Stream[i].Text)
		}
	}
	return reverse(out)
}

// recentMixedLines 取最近的观察与动作（时间正序）。
//
// 两者混在一起的理由：对"我刚做了什么、发生了什么"这个问题，
// 看到消息和起身去倒水是同一串因果，拆成两段反而读不出顺序。
func (a *Agent) recentMixedLines(n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	for i := len(a.Stream) - 1; i >= 0 && len(out) < n; i-- {
		switch a.Stream[i].Kind {
		case KindObservation, KindAction:
			out = append(out, a.Stream[i].Kind.label()+a.Stream[i].Text)
		}
	}
	return reverse(out)
}

// dredgeQuery 组装打捞查询词。
//
// 只用意图与最近的想法：它们最贴近"此刻在想什么"，用它们去捞旧记忆
// 才是联想。刻意不用状态名——"刷手机"这种词捞不出有意义的往事。
//
// 传的是**自然语言整句**（不是切好的关键词）：切词元是记忆层的事
// （memory.Dredge 自己 tokenize），因为只有它知道待选区里存的是什么。
// 这里曾经假设"调用方负责展开成关键词"，于是整句被拿去做子串匹配，
// 恒不命中——契约两侧各写各的，合起来线上【想起的事】永远是空的。
func (a *Agent) dredgeQuery() []string {
	var q []string
	if intent := a.Intent(); intent != "" {
		q = append(q, intent)
	}
	q = append(q, a.recentLines(KindThought, 2)...)
	return q
}

// reverse 原地反转字符串切片。
func reverse(s []string) []string {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	return s
}
