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
	// EventUserMessage 收到一条用户消息。
	EventUserMessage EventKind = "user_message"
	// EventMention 消息里 @ 了 agent。优先级高于普通消息。
	EventMention EventKind = "mention"
	// EventLLMDone 一次 LLM 调用完成，带回结果。
	// 这是 R4 的关键：LLM 结果通过事件回到 agent 自己的 goroutine，
	// 而不是让调用方直接改状态。
	EventLLMDone EventKind = "llm_done"
	// EventLLMFailed 一次 LLM 调用失败。
	EventLLMFailed EventKind = "llm_failed"
)

// priority 返回事件的抢占优先级，数值越大越优先。
// 普通消息不抢占；被 @ 才抢占（见 docs/memory.md §2）。
func (k EventKind) priority() int {
	switch k {
	case EventMention:
		return 100
	case EventLLMDone, EventLLMFailed:
		return 50
	case EventUserMessage:
		return 10
	default:
		return 0
	}
}
