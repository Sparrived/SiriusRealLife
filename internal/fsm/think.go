package fsm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Think 起一次异步 LLM 调用（R4）。
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
func (a *Agent) Think(ctx context.Context, prompt string) error {
	if a.chatter == nil {
		return fmt.Errorf("fsm: 未配置 Chatter")
	}
	if a.thinking != nil {
		return fmt.Errorf("fsm: 已有在途 LLM 调用（state=%s）", a.Current)
	}

	callCtx, cancel := context.WithCancel(ctx)
	a.thinking = cancel
	a.appendStream("开始思考")

	go func() {
		resp, err := a.chatter.Chat(callCtx, ChatRequest{Prompt: prompt})
		// 结果通过事件回到 agent 的 goroutine（R1）。此处绝不直接改状态。
		var ev Event
		if err != nil {
			payload, _ := json.Marshal(map[string]string{"error": err.Error()})
			ev = Event{Kind: EventLLMFailed, Data: payload}
		} else {
			payload, _ := json.Marshal(map[string]string{"text": resp.Text})
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

// cancelThinking 取消在途调用（若有）。由 agent 自己在抢占时调用。
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
