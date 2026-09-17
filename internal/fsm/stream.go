package fsm

import "strings"

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

// 独白回复的解析前缀。
//
// 为什么用前缀而不是 JSON：走 AMKR 的普通 chat 接口，JSON 模式
// 不是所有 route 都支持，而"模型偶尔多写一句解释"会让 JSON 解析
// 整体失败、独白全丢。前缀解析最坏情况只是**退化成一条 thought**，
// 不会丢内容——这是刻意选的"坏得比较轻"的失败方式。
const (
	prefixThought = "想:"
	prefixAction  = "做:"
	prefixIntent  = "打算:"
)

// parseMonologue 把一次独白回复解析成带类型的记录。
//
// 容错规则（按优先级）：
//  1. 命中前缀 → 对应类型
//  2. 未命中任何前缀的非空行 → KindThought（模型自由发挥的兜底）
//  3. 全文都没有可识别行 → 整段作为一条 KindThought
//
// 空文本返回 nil：调用方据此不写任何记录。
func parseMonologue(text string) []StreamEntry {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
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
func splitPrefix(line string) (Kind, string, bool) {
	for _, c := range []struct {
		prefix string
		kind   Kind
	}{
		{prefixIntent, KindIntent},
		{prefixAction, KindAction},
		{prefixThought, KindThought},
	} {
		if strings.HasPrefix(line, c.prefix) {
			return c.kind, strings.TrimSpace(strings.TrimPrefix(line, c.prefix)), true
		}
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
