package fsm

import "fmt"

// Ticker 由需要跟随时间前进的组件实现（当前是 memory.Store）。
//
// 与 Attention 同样定义在 fsm 侧以避免 import 成环，由 memory.Store 隐式满足。
//
// 为什么必须存在：记忆的强度衰减、升格、遗忘全按 tick 判定（R8），
// 如果没人驱动它，这些机制在真实运行的系统里根本不会发生——
// 单元测试各自通过、装起来记忆却永不遗忘，是很容易漏掉的一类缺口。
type Ticker interface {
	// Tick 前进到指定 tick 序号。
	Tick(now Tick)
}

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
	// Scan 取最近 n 条**未读**消息的文本并推进已读游标。
	Scan(n int) []string
	// Browse 向前翻页，返回更早的 n 条（§2.3 的两级读取）。
	Browse(n int) []string
	// Unread 返回未读条数。它是心境输入（99+ 推高"烦躁"，§2.2）。
	Unread() int
}

// defaultFeedLimit 是单次泵入的条数上限。
//
// 5 条是初值：再多会挤占 prompt，再少不像真的看了一眼。
// ponytail: 硬编码常量，需要按人格调时挪进状态表字段（FeedLimit）。
const defaultFeedLimit = 5

// Pump 用当前状态订阅的通道喂一次数据：有新消息就写进意识流。
//
// 这是"状态持续"的机制落点。订阅在**整个驻留期间**生效，由 Step
// 每 tick 调一次，因此"刷手机时来了一条消息"会被立刻看见，而不是
// 只在进入那一刻读一把。
//
// 静默：没有新消息时不写任何记录。它在每个 tick 都被调用，若像
// ReadPhone 那样写"没有新消息"，意识流会被这句废话淹没。
//
// 只能在 agent 自己的 goroutine 里调用（R1）。
func (a *Agent) Pump() {
	if a.attention == nil {
		return
	}
	s := a.states[a.Current]
	if !s.Subscribes(ChanQQ) {
		return
	}
	limit := s.FeedLimit
	if limit <= 0 {
		limit = defaultFeedLimit
	}
	for _, m := range a.attention.Scan(limit) {
		// 这里是消息正文进入模型视野的路径之一，可见性门控在上面。
		a.appendStreamKind(KindObservation, m)
	}
}

// ReadPhone 读手机：把最近 n 条未读放进意识流。
//
// 与 Pump 的区别：这是**显式**读一次（进入状态时"解锁扫一眼"，或
// 工具调用），会如实写下"没有新消息"这种观察；Pump 是每 tick 的
// 静默泵入。两者共用可见性门控。
//
// 只能在 agent 自己的 goroutine 里调用（R1）——它由状态表的 OnEnter
// 触发，天然满足。
//
// 若当前状态没订阅 QQ，则不读任何内容：这正是"信息可见性"
// 的落点（R7 修订）。工具本身没有被封锁，被门控的是信息。
func (a *Agent) ReadPhone(n int) {
	if !a.states[a.Current].Subscribes(ChanQQ) {
		// 没在看 QQ：不读，但要留下"有未读"这个事实。
		if a.attention != nil {
			if unread := a.attention.Unread(); unread > 0 {
				a.appendStreamKind(KindObservation, fmt.Sprintf("手机上有 %d 条未读，但现在不想看", unread))
			}
		}
		return
	}
	if a.attention == nil {
		return
	}
	msgs := a.attention.Scan(n)
	if len(msgs) == 0 {
		a.appendStreamKind(KindObservation, "看了一眼手机，没有新消息")
		return
	}
	for _, m := range msgs {
		// 这里是唯一能看到消息正文的路径：可见性门控在方法开头。
		a.appendStreamKind(KindObservation, m)
	}
}

// BrowsePhone 向前翻页（§2.3 的第二级读取）。
//
// 与 ReadPhone 一样，受订阅门控。
func (a *Agent) BrowsePhone(n int) []string {
	if a.attention == nil || !a.states[a.Current].Subscribes(ChanQQ) {
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

// QuietFor 返回"距离上一条消息过了多少 tick"。
//
// 第二个返回值为假表示**从来没收到过消息**：那不是"安静了很久"，
// 而是"还没有人说过话"。两者对人格的含义完全不同（前者是"没人理我"，
// 后者是"世界还没开始"），因此必须能区分，不能拿一个大数字糊过去。
//
// 为什么需要它：状态的 Until 只能看到心境与时间。而"我在这儿等了半天
// 也没人说话"是一个**由外部输入推出的**事实——没有这个访问器，它根本
// 无法被表达，于是"长时间没收到消息就退出订阅去干别的"也就写不出来。
// 这正是用户要的那条退出条件（见 docs/architecture.md §2.1）。
//
// 只能在 agent 自己的 goroutine 里调用（R1）。
func (a *Agent) QuietFor() (Tick, bool) {
	if !a.everGotMessage {
		// 从进入系统到现在都没人说过话，也应当算作"一直很安静"。
		return a.Now, false
	}
	return a.Now - a.lastMessageAt, true
}
