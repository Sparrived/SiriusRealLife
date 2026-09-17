package memory

import "github.com/Sparrived/SiriusRealLife/internal/fsm"

// 下面三个方法让 Store 隐式满足 fsm.Attention（Go 接口是隐式满足的）。
//
// 接口定义在 fsm 侧而不是这里，是为了避免 import 成环：memory 已经
// import fsm（用 fsm.Tick），反向 import 会成环。这也符合"接口由
// 使用方定义"的惯例。
//
// 只返回文本：状态机不需要知道消息的 ID、来源、重要性。

// Scan 实现 fsm.Attention：最近 n 条未读的文本。
func (s *Store) Scan(n int) []string {
	msgs := s.scanMessages(n)
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, formatMessage(m))
	}
	return out
}

// Browse 实现 fsm.Attention：更早的 n 条文本。
func (s *Store) Browse(n int) []string {
	msgs := s.browseMessages(n)
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, formatMessage(m))
	}
	return out
}

// Unread 实现 fsm.Attention：未读条数。
func (s *Store) Unread() int { return s.UnreadCount() }

// formatMessage 把一条消息渲染成给 LLM 看的文本。
//
// 带上发送者：群里"谁说的"是理解上下文的关键。
func formatMessage(m Message) string {
	if m.From != "" {
		return m.From + ": " + m.Text
	}
	return m.Text
}

// 编译期断言：Store 必须满足 fsm.Attention。
var _ fsm.Attention = (*Store)(nil)
