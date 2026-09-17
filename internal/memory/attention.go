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

// Accept 实现 fsm.MessageSink：把一条外部消息收进 unread 队列。
//
// 这是记忆链路的**起点**。之前 Ingest 没有任何生产调用方，导致
// 整套记忆机制（unread → 待选区 → 打捞 → 升格 → Shadow）在真实
// 运行中永远是空的——单测各自通过，装起来却没有一条消息进来。
//
// 刻意不返回"该不该打断"：@ 不抢占状态（docs/memory.md §2.1），
// 收了就是收了。
func (s *Store) Accept(m fsm.IncomingMessage) {
	s.Ingest(Message{
		From:        m.From,
		Text:        m.Text,
		MentionsMe:  m.MentionsMe,
		RepliesToMe: m.RepliesToMe,
		Tick:        m.Tick,
	})
}

// DredgeFor 返回一个把 tick 绑进打捞的查询函数（§5.1）。
//
// 单独一层适配而不是让 fsm 直接调 Dredge：Dredge 需要一个"现在是
// 第几 tick"的参数以刷新记忆曲线（R8），而 prompt 装配侧的调用点
// 只管"我想到什么词"。签名与 fsm.ContextOptions.Dredge 对齐。
func (s *Store) DredgeFor() func([]string, fsm.Tick) []string {
	return func(query []string, now fsm.Tick) []string { return s.Dredge(query, now) }
}

// 编译期断言：Store 必须满足 fsm.Attention、fsm.Ticker 与 fsm.MessageSink。
//
// Ticker 就是 Store.Tick —— 让 agent 的 tick 循环驱动记忆的
// 衰减/升格/遗忘。没有这条断言，很容易出现"单测都过、装起来
// 记忆永不遗忘"的缺口；MessageSink 同理（消息永不入队）。
var (
	_ fsm.Attention   = (*Store)(nil)
	_ fsm.Ticker      = (*Store)(nil)
	_ fsm.MessageSink = (*Store)(nil)
)
