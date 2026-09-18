package fsm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolSendMessage 是"把话发出去"。
//
// 它只出现在**工具结果轮**（SiteToolRead）：那一轮读了内容、也拿到了
// 打捞回来的记忆，才谈得上怎么回应。状态决策轮不提供它——一次决定只改
// 一次状态，回复不是改状态（R11）。
const toolSendMessage = "send_message"

// OutgoingMessage 是一条要发出去的消息（她的回复）。
type OutgoingMessage struct {
	// To 是回给谁（会话或人名）。空表示当前会话。
	To string
	// Text 是实际发出去的话，不含"回了谁"这类包装。
	Text string
	// Tick 是发出时刻，只认 tick（R8）。
	Tick Tick
}

// Outbox 把她的回复送出去。
//
// 与 Sink 对称：Sink 收外部进来的消息，Outbox 发出她的话。
// 定义在 fsm 侧以避免 import 成环，由具体传输层隐式满足。
//
// **为 nil 表示还没有出站通道**：那时她的话仍然进意识流与记忆层
// （"我说过什么"是情节记忆的一半），只是发不出去，并留下一条日志。
// 必须留痕：否则"她回复了"会变成一个看不见的谎。
type Outbox interface {
	Send(m OutgoingMessage)
}

// Recorder 记录"她说过的话"。
//
// 与 Attention 分开：那是"读得到什么"，这是"写下什么"。两者恰好都由
// memory.Store 满足，但它们回答的是不同的问题，混在一个接口里会让
// 只想读的地方凭空拿到写的能力。
//
// 定义在 fsm 侧同样是避免 import 成环（memory 已 import fsm）。
type Recorder interface {
	// Record 把一段她自己的话写入记忆。
	Record(text string)
}

// sendMessageTool 声明工具结果轮可用的工具。
//
// 只有 send_message：这一轮不负责状态（用户定的形状），但仍要能回应——
// 读了消息却回不了话，那一轮就白读了。
func sendMessageTool() ToolSpec {
	return ToolSpec{
		Name: toolSendMessage,
		Description: "把一句话发出去（回复某人或群里）。只在你想好要说什么时用；" +
			"不想回就别调它，直接说一句你的想法即可。",
		Parameters: json.RawMessage(
			`{"type":"object","properties":{"to":{"type":"string","description":"回给谁（人名或会话），不填就是当前会话"},"text":{"type":"string","description":"要发出去的话，用你自己的口吻"}},"required":["text"],"additionalProperties":false}`),
	}
}

// toolReadTools 是工具结果轮的完整工具清单。
func (a *Agent) toolReadTools() []ToolSpec {
	return []ToolSpec{sendMessageTool()}
}

// applyToolRead 处理工具结果轮回来的工具调用。
//
// 这一轮**只能发消息，不能改状态**：状态决策由框架随后重新问一次，
// 走的是 applyDecision。把两者分开是为了"一次决定只改一次状态"。
func (a *Agent) applyToolRead(res llmResult) {
	for _, c := range res.Calls {
		if c.Name != toolSendMessage {
			a.log.Warn("toolread_unknown_call", "tool", c.Name)
			continue
		}
		var p struct {
			To   string `json:"to"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(c.Arguments, &p); err != nil {
			a.log.Warn("toolread_bad_args", "tool", c.Name, "err", err.Error())
			continue
		}
		a.sendMessage(p.To, p.Text)
		return
	}
}

// sendMessage 发出一条回复：进意识流、进记忆、再送出去（若有通道）。
//
// 三件事的顺序与理由：
//  1. **进意识流**（KindAction）：她说出口的话是她做过的事，"我刚说了什么"
//     必须能被下一轮看到，否则她会重复同一句。
//  2. **进记忆**（Recorder）：说出去的话也进待选区。用户定过这条——
//     "我说过什么"与"我听到什么"一样是情节记忆，缺了一半，日后想起
//     这段对话就只剩对方在自言自语。
//  3. **发出去**（Outbox）：没有通道时只留日志，不假装成功。
func (a *Agent) sendMessage(to, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	line := "回了句：" + text
	if to != "" {
		line = fmt.Sprintf("回%s：%s", to, text)
	}
	a.appendStreamKind(KindAction, line)

	if a.recorder != nil {
		a.recorder.Record(line)
	}
	if a.outbox != nil {
		a.outbox.Send(OutgoingMessage{To: to, Text: text, Tick: a.Now})
		return
	}
	// 没有出站通道：话进了意识流与记忆，但发不出去。
	a.log.Info("outbox_missing", "to", to, "len", len(text))
}
