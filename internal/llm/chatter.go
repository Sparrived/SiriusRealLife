package llm

import (
	"context"
	"fmt"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// Chatter 把 AMKR 客户端适配成 fsm 需要的缝口。
//
// 适配层存在的意义：fsm 不 import internal/llm，依赖方向反过来由这里承担，
// 状态机核心因此可以离线测试（不碰网络）。
type Chatter struct {
	client *Client
	site   CallSite
	// OnDegrade 在 503（没有可用 Key）时被调用一次。
	// 调用方据此进入降级状态并记进意识流，**不重试**（R10）。
	OnDegrade func(error)
}

// NewChatter 构造适配器。site 决定用哪个调用点的任务名。
func NewChatter(client *Client, site CallSite) *Chatter {
	return &Chatter{client: client, site: site}
}

// Chat 实现 fsm.Chatter。
//
// 直接透传 ChatResponse：工具调用的解析在客户端做（那是协议形状），
// 这里不加工——适配层只负责错误归类与依赖方向。
func (c *Chatter) Chat(ctx context.Context, req fsm.ChatRequest) (fsm.ChatResponse, error) {
	resp, err := c.client.Complete(ctx, req)
	if err != nil {
		// 503：当作可降级错误处理，不立刻重试。
		if se, ok := err.(*StatusError); ok && se.Unavailable() && c.OnDegrade != nil {
			c.OnDegrade(err)
		}
		return fsm.ChatResponse{}, fmt.Errorf("llm[%s]: %w", c.site, err)
	}
	return resp, nil
}
