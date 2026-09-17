package fsm

import (
	"context"
	"encoding/json"
)

// Tool 是所有工具的统一签名（R7）。工具里不准写业务逻辑或状态机逻辑。
type Tool func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

// Event 是进入 agent 的唯一入口（R1）。其他 goroutine 想影响 agent，
// 只能投递事件，不准直接改状态。
type Event struct {
	Kind EventKind
	// Data 是事件负载，按 Kind 解释。
	Data json.RawMessage
}

// EventKind 标识事件种类。
//
// 事件名是稳定字符串而非 int：R6 日志要能直接读懂。
type EventKind string

const (
	// EventUserMessage 收到一条外部消息。
	//
	// **@我 与回复我也是这个类型**，只是负载里 MentionsMe/RepliesToMe
	// 为真。刻意不区分事件类型：区分会立刻诱导出"按类型决定优先级、
	// 按优先级抢占状态"的控制流，而 QQ 的现实语义不是这样——@ 只是
	// 提醒更显眼（弹通知），并不把用户手上的事掐断（docs/memory.md §2.1）。
	// 显眼程度作用在**内容**（提示文案、队列重要性），不作用在控制流。
	EventUserMessage EventKind = "user_message"
	// EventLLMDone 一次 LLM 调用完成，带回结果。
	// 这是 R4 的关键：LLM 结果通过事件回到 agent 自己的 goroutine，
	// 而不是让调用方直接改状态。
	EventLLMDone EventKind = "llm_done"
	// EventLLMFailed 一次 LLM 调用失败。
	EventLLMFailed EventKind = "llm_failed"
)
