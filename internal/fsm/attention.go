package fsm

import "fmt"

// Attention 是"看 QQ"这个动作需要的全部能力。
//
// 为什么在这里定义接口：memory 包已经 import fsm（它用 fsm.Tick），
// 所以 fsm **不能**反向 import memory，否则成环。Go 的接口是隐式满足的，
// 因此 memory.Store 只要实现下面两个方法就自动成为 Attention ——
// 不需要它声明"我实现了 fsm.Attention"。
//
// 只暴露文本，不暴露消息结构：状态机不需要知道消息的 ID、来源、
// 重要性这些记忆层的概念。**被门控的是信息，不是工具**（R7 修订）。
type Attention interface {
	// Scan 模拟"解锁手机扫一眼"：返回最近 n 条未读消息的文本并推进已读游标。
	Scan(n int) []string
	// Browse 向前翻页，返回更早的 n 条（§2.3 的两级读取）。
	Browse(n int) []string
	// Unread 返回未读条数。它是心境输入（99+ 推高"烦躁"，§2.2）。
	Unread() int
}

// scanOnEnterN 是进入"看 QQ"时扫一眼的条数。
//
// 5 条是初值：再多会挤占 prompt，再少不像真的看了一眼。
// ponytail: 硬编码常量，需要按人格调时挪进状态表字段。
const scanOnEnterN = 5

// ReadPhone 读手机：把最近 n 条未读放进意识流。
//
// 只能在 agent 自己的 goroutine 里调用（R1）——它由状态表的 OnEnter
// 触发，天然满足。
//
// 若当前状态声明了 QQ 不可见，则不读任何内容：这正是"信息可见性"
// 的落点（R7 修订）。工具本身没有被封锁，被门控的是信息。
func (a *Agent) ReadPhone(n int) {
	if !a.states[a.Current].Visibility.QQ {
		// 不在看 QQ 的状态：不读，但要留下"有未读"这个事实。
		if a.attention != nil {
			if unread := a.attention.Unread(); unread > 0 {
				a.appendStream(fmt.Sprintf("手机上有 %d 条未读，但现在不想看", unread))
			}
		}
		return
	}
	if a.attention == nil {
		return
	}
	msgs := a.attention.Scan(n)
	if len(msgs) == 0 {
		a.appendStream("看了一眼手机，没有新消息")
		return
	}
	for _, m := range msgs {
		a.appendStream(m)
	}
}

// BrowsePhone 向前翻页（§2.3 的第二级读取）。
//
// 与 ReadPhone 一样，受 Visibility.QQ 门控。
func (a *Agent) BrowsePhone(n int) []string {
	if a.attention == nil || !a.states[a.Current].Visibility.QQ {
		return nil
	}
	return a.attention.Browse(n)
}

// Unread 返回未读条数。它是心境输入（§2.2），不受可见性门控——
// "手机上有多少条未读"这个数字本身不需要进入模型视野即可影响烦躁。
func (a *Agent) Unread() int {
	if a.attention == nil {
		return 0
	}
	return a.attention.Unread()
}
