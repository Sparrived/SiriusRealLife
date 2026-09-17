package fsm

import "encoding/json"

// IncomingMessage 是一条从外部进来的消息。
//
// 为什么在 fsm 侧定义而不是直接用 memory.Message：memory 已经 import
// fsm，反向 import 会成环。这里只放**状态机需要知道的字段**——消息的
// 存储细节（ID、Session、Strength）是记忆层自己的事。
type IncomingMessage struct {
	From        string
	Text        string
	MentionsMe  bool
	RepliesToMe bool
	// Tick 是收到它的游戏时刻。记忆的时间轴只认 tick（R8）。
	Tick Tick
}

// MessageSink 收下一条外部消息。
//
// 与 Attention 同样定义在 fsm 侧、由 memory.Store 隐式满足：
// 依赖方向必须是 memory → fsm，接口由使用方定义，因此这里不需要
// memory 声明"我实现了它"。
//
// 为什么必须存在：消息入口是记忆链路的**起点**。没有它，unread 队列、
// 待选区、打捞、Shadow 在真实运行中永远为空——单元测试各自通过，
// 装起来却没有任何消息进来，是很容易漏掉的一类缺口。
//
// 刻意不返回"该不该打断"：@ 不抢占状态（docs/memory.md §2.1），
// 是否因此做事由状态自己的退出条件决定，不由消息类型决定。
type MessageSink interface {
	Accept(m IncomingMessage)
}

// messagePayload 是消息事件的 JSON 负载形状。
//
// 由本包独占定义：负载怎么编码是 fsm 的事，装配方（transport）
// 只管调 NewMessageEvent。否则字段名会在两处各写一遍，加字段时漏改。
type messagePayload struct {
	From        string `json:"from"`
	Text        string `json:"text"`
	MentionsMe  bool   `json:"mentions_me"`
	RepliesToMe bool   `json:"replies_to_me"`
}

// NewMessageEvent 把一条外部消息包成投给 agent 的事件。
//
// MentionsMe/RepliesToMe 必须进负载：它们决定记忆层的即时重要性
// （@我 得 8 分，普通消息 2 分）以及提示文案的显眼程度，丢了就只剩文本。
func NewMessageEvent(m IncomingMessage) Event {
	payload, _ := json.Marshal(messagePayload{
		From: m.From, Text: m.Text,
		MentionsMe: m.MentionsMe, RepliesToMe: m.RepliesToMe,
	})
	return Event{Kind: EventUserMessage, Data: payload}
}

// decodeMessage 从事件负载还原消息。
//
// 负载可能为空或非法（测试里直接投 Event{Kind: EventUserMessage}），
// 此时返回零值而不是报错：一条没有内容的消息不该让 agent 崩溃。
func decodeMessage(data json.RawMessage) IncomingMessage {
	var p messagePayload
	if len(data) > 0 {
		_ = json.Unmarshal(data, &p)
	}
	return IncomingMessage{
		From: p.From, Text: p.Text,
		MentionsMe: p.MentionsMe, RepliesToMe: p.RepliesToMe,
	}
}
