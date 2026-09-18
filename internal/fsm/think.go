package fsm

import (
	"context"
	"encoding/json"
	"fmt"
)

// thinkKind 区分一次 LLM 调用是干什么的。
//
// 必须区分：结果回来后要走**完全不同**的处理（独白只写意识流，
// 决策要改状态）。不带上这个标记，一次决策的回复就会被当成
// 一段内心活动丢掉，人格于是永远不换状态。
type thinkKind string

const (
	thinkMonologue thinkKind = "monologue"
	thinkDecision  thinkKind = "decision"
	// thinkToolRead 是工具结果轮：把刚读到的内容连同打捞回来的记忆
	// 一起交给模型，让它读懂并想好怎么回应。
	//
	// 它**不改状态**（不带状态工具），只写意识流；随后框架会重新问
	// 一次状态决策（memory.md §5.1）。
	thinkToolRead thinkKind = "tool_read"
)

// llmResult 是一次 LLM 调用回来的结果（事件负载）。
type llmResult struct {
	// Kind 是调用种类，决定结果怎么处理。
	Kind thinkKind `json:"kind"`
	// Text 是模型的叙述。决策调用也会有（模型边叙述边调工具）。
	Text string `json:"text"`
	// Calls 是模型决定调用的工具。
	Calls []ToolCall `json:"calls"`
}

// think 起一次异步 LLM 调用（R4）。
//
// 关键约束：调用**不能**挡住 tick 循环，也**不能**让调用方直接改状态。
// 因此这里只做两件事：
//  1. 起一个 goroutine 去调用
//  2. 把结果作为事件投回 agent 自己的 channel
//
// 返回 error 只表示"起不来"（无 chatter、已有在途调用），不表示调用失败——
// 调用失败会以 EventLLMFailed 事件回来。
//
// 只能在 agent 自己的 goroutine 里调用（R1）。
func (a *Agent) think(ctx context.Context, req ChatRequest, kind thinkKind) error {
	if a.chatter == nil {
		return fmt.Errorf("fsm: 未配置 Chatter")
	}
	if a.thinking != nil {
		return fmt.Errorf("fsm: 已有在途 LLM 调用（state=%s）", a.Current)
	}

	callCtx, cancel := context.WithCancel(ctx)
	a.thinking = cancel
	// 记下这次调用为哪个状态发起：结果回来时可能已经换了状态，
	// 归错账会让意识流失真（"在刷手机时想的事"记成"干活时想的事"）。
	a.inFlightState = a.Current

	go func() {
		resp, err := a.chatter.Chat(callCtx, req)
		// 被取消（关停）时直接丢弃：这不是"失败"，不该往意识流里
		// 记一条"没想出来"，也不该触发降级决策。
		if callCtx.Err() != nil {
			return
		}
		// 结果通过事件回到 agent 的 goroutine（R1）。此处绝不直接改状态。
		var ev Event
		if err != nil {
			payload, _ := json.Marshal(map[string]string{"error": err.Error(), "kind": string(kind)})
			ev = Event{Kind: EventLLMFailed, Data: payload}
		} else {
			payload, _ := json.Marshal(llmResult{Kind: kind, Text: resp.Text, Calls: resp.ToolCalls})
			ev = Event{Kind: EventLLMDone, Data: payload}
		}
		select {
		case a.events <- ev:
		case <-callCtx.Done():
			// 已被取消，丢弃结果。
		}
	}()

	return nil
}

// cancelThinking 取消在途调用（若有）。
func (a *Agent) cancelThinking() {
	if a.thinking != nil {
		a.thinking()
		a.thinking = nil
	}
}

// settleThinking 在一次调用落定后清空在途标记，使下次 Think 可以发起。
func (a *Agent) settleThinking() { a.thinking = nil }

// IsThinking 报告是否有在途 LLM 调用。
func (a *Agent) IsThinking() bool { return a.thinking != nil }
