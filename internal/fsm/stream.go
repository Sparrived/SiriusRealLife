package fsm

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Kind 是意识流记录的类型。
//
// 为什么必须有类型：意识流是**日志**，prompt 是从日志里按当前任务
// 组装出来的**视图**。如果每条记录都是一样的字符串，就无法回答
// "最近在想什么 / 做了什么 / 接下来准备做什么"——这三件事在数据
// 结构上必须可区分，否则装配器只能把它们混成一段流水账。
//
// 分层依据 CoALA（arXiv:2309.02427 §4.1）：working memory 存
// "当前情境 + 近期感知 + 目标 + 中间推理结果"，而 episodic 存
// "过去行为的序列"。我们把这些统一记在一条带类型的日志里，
// 由 Context() 按用途取子集。
type Kind uint8

const (
	// KindObservation 看到/收到的东西：消息、工具返回、环境变化。
	// 它是"发生了什么"，不是 agent 自己产生的。
	KindObservation Kind = iota
	// KindThought 内心独白："有点无聊"、"大概写不完了"。
	KindThought
	// KindAction 做了什么："拿起手机刷一刷"、"开始干活"。
	KindAction
	// KindIntent 接下来打算做什么。
	//
	// 意图**必须存在日志里**而不是当成一个字段：字段会被状态切换
	// 清掉，而"我原本打算做什么"恰恰要跨状态存活，否则 agent 每次
	// 换状态都像换了个人（这正是修复前的问题）。
	KindIntent
)

// String 让日志与测试输出可读。
func (k Kind) String() string {
	switch k {
	case KindObservation:
		return "observation"
	case KindThought:
		return "thought"
	case KindAction:
		return "action"
	case KindIntent:
		return "intent"
	default:
		return "unknown"
	}
}

// label 是渲染进 prompt 的中文标签。
func (k Kind) label() string {
	switch k {
	case KindObservation:
		return "看到"
	case KindThought:
		return "想"
	case KindAction:
		return "做"
	case KindIntent:
		return "打算"
	default:
		return "?"
	}
}

// monologueSchema 是独白期望的结构化输出形状。
//
// 为什么优先用 JSON 而不是解析文本前缀：前缀解析要靠模型守格式，
// 而实测同一个 prompt 下，模型会给出 `想:` / `想：`（全角）/
// `**打算：**`（加粗）等写法，每一种都要单独兼容，漏一种就静默降级。
// JSON 由 schema 约束字段名，解析不再依赖标点。
//
// additionalProperties:false + required 三项是关键：实测只有
// json_schema+strict 才真正约束字段名；json_object 只保证"是 JSON"，
// 字段名照样由模型自己起（实测 glm 回了 `answer`、deepseek 回了
// `内心活动`）。因此解析侧仍要有别名兜底，见 monologueFields。
var monologueSchema = ResponseSchema{
	Name: "monologue",
	Schema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"thought": {"type": "string", "description": "此刻内心在想什么，一句话"},
			"action": {"type": "string", "description": "正在做的动作；没有就填空字符串"},
			"intent": {"type": "string", "description": "接下来打算做什么；没有就填空字符串"}
		},
		"required": ["thought", "action", "intent"],
		"additionalProperties": false
	}`),
}

// monologueLabels 是文本前缀解析用的标签。
//
// 只列标签、不列冒号：冒号可能是半角 `:` 也可能是全角 `：`，
// 模型还会加粗。实测同一条 prompt 下 AMKR 的 `auto` 路由回全角、
// `hy3` 回半角——只认半角会让前者的意图被静默降级成普通想法。
var monologueLabels = []struct {
	label string
	kind  Kind
}{
	{"打算", KindIntent},
	{"做", KindAction},
	{"想", KindThought},
}

// monologueFields 是 JSON 形态下三个字段的取值别名。
//
// 为什么需要别名：json_object 模式（以及忽略 schema 的路由）下模型会
// 自起字段名，实测出现过 answer / 内心活动 / content。别名表让这些
// 仍然归到正确类型，而不是整段退化成一条 thought。
var monologueFields = []struct {
	names []string
	kind  Kind
}{
	{[]string{"intent", "next", "plan", "打算", "接下来", "计划"}, KindIntent},
	{[]string{"action", "doing", "act", "做", "动作", "正在做"}, KindAction},
	{[]string{"thought", "thinking", "answer", "content", "想", "想法", "内心活动", "独白"}, KindThought},
}

// parseMonologue 把一次独白回复解析成带类型的记录。
//
// 两种形态都接受，因为上游路由由 AMKR 决定、可以随时切（R9）：
//
//  1. JSON（请求了 schema 且路由支持时）→ 按字段名分类型
//  2. 文本前缀（路由忽略 schema 时）→ 按 `想:` / `做:` / `打算:` 分
//
// 任何一步识别不出都不丢内容：兜底是整段作为一条 thought。
// 空文本返回 nil，调用方据此不写任何记录。
func parseMonologue(text string) []StreamEntry {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if entries, ok := parseMonologueJSON(text); ok {
		return entries
	}
	return parseMonologueText(text)
}

// parseMonologueJSON 尝试按 JSON 解析。第二个返回值表示"确实是可用的 JSON"。
func parseMonologueJSON(text string) ([]StreamEntry, bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(unfence(text)), &raw); err != nil {
		return nil, false
	}
	// 输出顺序固定为 想法→动作→打算（时间因果），与别名表的
	// 优先级顺序无关。
	var out []StreamEntry
	for _, kind := range []Kind{KindThought, KindAction, KindIntent} {
		if v := pickField(raw, kind); v != "" {
			out = append(out, StreamEntry{Kind: kind, Text: v})
		}
	}
	if len(out) == 0 {
		// 是合法 JSON 但没有可用字段（例如只有无关的键）：
		// 不当作 JSON 消费，交给文本兜底，内容才不会被丢掉。
		return nil, false
	}
	return out, true
}

// pickField 按别名表取出某个类型对应的字段值。
func pickField(raw map[string]any, kind Kind) string {
	for _, f := range monologueFields {
		if f.kind != kind {
			continue
		}
		for _, name := range f.names {
			s, ok := raw[name].(string)
			if !ok {
				continue
			}
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

// unfence 去掉 markdown 代码围栏。
//
// 不少模型即使被要求 JSON，也会顺手包成 ```json ... ```。
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// parseMonologueText 按文本前缀解析（路由忽略 schema 时的兜底）。
//
// 容错规则（按优先级）：
//  1. 命中前缀 → 对应类型
//  2. 未命中任何前缀的非空行 → KindThought（模型自由发挥的兜底）
//  3. 全文都没有可识别行 → 整段作为一条 KindThought
func parseMonologueText(text string) []StreamEntry {
	var out []StreamEntry
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kind, body, ok := splitPrefix(line)
		if !ok {
			// 没前缀：整行当想法。宁可类型不准，也不要丢内容。
			out = append(out, StreamEntry{Kind: KindThought, Text: line})
			continue
		}
		if body == "" {
			continue
		}
		out = append(out, StreamEntry{Kind: kind, Text: body})
	}
	if len(out) == 0 {
		return []StreamEntry{{Kind: KindThought, Text: text}}
	}
	return out
}

// splitPrefix 识别一行开头的类型前缀。
//
// 冒号不参与匹配（`:` / `：` / 无冒号都收），两侧的 markdown 强调
// 也先剥掉：实测模型会写 `**打算：** 去写点东西`。
func splitPrefix(line string) (Kind, string, bool) {
	s := strings.TrimSpace(strings.TrimLeft(line, "*_#>-•"))
	for _, c := range monologueLabels {
		if !strings.HasPrefix(s, c.label) {
			continue
		}
		rest := s[len(c.label):]
		if rest == "" {
			return c.kind, "", true
		}
		// 必须按 rune 取首字符：全角 `：` 在 UTF-8 里是 3 字节，
		// 取 rest[0] 只会拿到第一个字节，全角冒号就永远匹配不上。
		r, size := utf8.DecodeRuneInString(rest)
		// 标签后必须紧跟冒号或空白，否则"想做点什么"会被误判成
		// "想" 类型、内容只剩"做点什么"。
		if !strings.ContainsRune(":： \t", r) {
			continue
		}
		if r == ':' || r == '：' {
			rest = rest[size:]
		}
		rest = strings.TrimSpace(rest)
		rest = strings.TrimSpace(strings.Trim(rest, "*_"))
		return c.kind, rest, true
	}
	return 0, "", false
}

// Intent 返回最近一条"打算做什么"。
//
// 这就是"接下来准备做什么"的实现：不新增字段，只查日志里最新的一条
// KindIntent。好处是意图天然跨状态存活——它是一条历史记录，
// 不会因为换了状态就消失。
func (a *Agent) Intent() string {
	for i := len(a.Stream) - 1; i >= 0; i-- {
		if a.Stream[i].Kind == KindIntent {
			return a.Stream[i].Text
		}
	}
	return ""
}
