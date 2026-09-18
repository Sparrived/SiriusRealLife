// Package memory 实现注意力门控与记忆分层（docs/memory.md）。
//
// 本包只做 Phase 1 的范围：unread 队列与门控、已读游标、待选区关键词
// 打捞、记忆曲线衰减与 Shadow、升格。向量检索、LLM 整合、自我模型属
// Phase 2（见 docs/memory.md §8）。
//
// 并发：**所有写入都来自 agent 自己的 goroutine**（R1）。Mutex 只为让
// transport 的 SSE 读取不撞上写入，不表示可以有多方写入。
package memory

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// MessageID 是消息在存储中的稳定标识。
type MessageID int64

// SessionID 标识"一次翻阅"。
//
// 打捞以它为检索单位：单条消息脱离上下文会失去含义
// （"那你去吧"指什么？），所以打捞返回**整段**（docs/memory.md §5.1）。
type SessionID int64

// Message 是一条收到的消息。
type Message struct {
	ID          MessageID
	Session     SessionID
	From        string
	Text        string
	MentionsMe  bool
	RepliesToMe bool
	// Importance 在入队时确定，用于 unread 溢出时的淘汰（§2.2）
	// 与升格对冲（§5.3）。默认由本地启发式给出，Phase 2 可换成 LLM 打分。
	Importance int
	Tick       fsm.Tick
}

// Entry 是待选区（staging）里的一条记忆。
type Entry struct {
	ID       int64
	Session  SessionID
	Text     string
	Keywords []string
	// Importance 1–10，写待选区时顺手打的分（§5.3）。
	Importance int
	// Strength 是记忆曲线强度 S ∈ [0,1]，初始 1.0，每 tick 衰减，
	// 被打捞则刷新为 1.0（§4）。
	Strength float64
	// Created / LastDredged 都是 tick，不认 wall clock（R8）。
	Created     fsm.Tick
	LastDredged fsm.Tick
	// DredgeTicks 记录被打捞的 tick，用于升格的滑动窗口判定（§5.3）。
	DredgeTicks []fsm.Tick
}

// DredgeCountWithin 返回窗口内被打捞的次数。
func (e *Entry) DredgeCountWithin(now, window fsm.Tick) int {
	n := 0
	for _, t := range e.DredgeTicks {
		if now-t <= window {
			n++
		}
	}
	return n
}

// Event 是升格后的事件记忆。
//
// Phase 1 只落库与计数，检索用的是关键词两路中的关键词那一路；
// 向量那一路在 Phase 2（§5.2）。
type Event struct {
	ID         int64
	Text       string
	Keywords   []string
	Importance int
	PromotedAt fsm.Tick
	// FromSession 保留来源，便于审计"这条事件记忆从哪来"。
	FromSession SessionID
}

// shadowEntry 是沉入 Shadow 的存档（§3.1）。
//
// **绝不进 prompt**，只供人审计。任何把它喂给 LLM 的代码都是 bug。
type shadowEntry struct {
	Kind   string // "staging" / "event"
	Text   string
	Reason string
	At     fsm.Tick
}

// Options 是 Store 的构造参数。
type Options struct {
	// UnreadLimit 是 unread 队列上限（R5）。
	UnreadLimit int
	// DecayPerTick 是待选区强度的每 tick 衰减量。
	// 默认 0.001：从 1.0 衰到 0.1 约 900 tick（15 游戏小时）。
	DecayPerTick float64
	// ShadowThreshold 是沉入 Shadow 的强度阈值（§4，默认 0.1）。
	ShadowThreshold float64
	// PromoteWindow 是升格判定的滑动窗口（§5.3，默认 100 tick）。
	PromoteWindow fsm.Tick
	// PromoteDredges 是升格所需窗口内打捞次数（默认 3）。
	PromoteDredges int
	// PromoteImportance 是高重要性免打捞阈值（默认 7）。
	PromoteImportance int
}

// DefaultOptions 返回文档给出的初值。
func DefaultOptions() Options {
	return Options{
		UnreadLimit:       200,
		DecayPerTick:      0.001,
		ShadowThreshold:   0.1,
		PromoteWindow:     100,
		PromoteDredges:    3,
		PromoteImportance: 7,
	}
}

// Store 持有全部记忆层。
type Store struct {
	mu  sync.RWMutex
	opt Options

	nextMsgID   MessageID
	nextEntryID int64
	nextEventID int64
	nextSessID  SessionID

	// messages 即文档中的 unread 队列（§2.2）：收到的消息都留在这里，
	// 有上限、按重要性淘汰。已读与未读的区分靠 cursor，不靠删除——
	// 否则 Browse 翻不到看过的内容。
	messages []Message
	// cursor 是已读游标（§2.3）。没有它，"最近 N 条"会永远返回同样内容。
	cursor MessageID
	// browseCursor 是翻阅位置，独立于已读游标（§2.3 的两级读取）。
	browseCursor MessageID

	// now 是最近一次 Tick 传入的 tick 序号。
	//
	// 为什么存一份而不是从消息上取：会话分组要的是"她此刻在什么时候
	// 看到这批消息"，而消息自带的 Tick 可能没被上游填（离线推演、
	// 测试直接 Ingest 都是 0）。用 0 分组会让所有消息挤进同一个会话，
	// 打捞于是永远返回一整坨，§5.1 的"以一次翻阅为单位"就失效了。
	// R8：这里存的就是 tick，不认 wall clock。
	now fsm.Tick

	// lastSession / lastSessionAt 让时间上连续的几次读取归入**同一次翻阅**。
	//
	// 没有它，Pump 每 tick 捞到一条就开一个新会话，于是"整段上下文"
	// 每段只有一句话——§5.1 想避免的正是这个（单条脱离上下文会失去含义）。
	lastSession   SessionID
	lastSessionAt fsm.Tick

	// staging 是待选区（§3）。
	staging []*Entry
	// events 是升格后的事件记忆。
	events []Event
	// shadow 是终态存档，LLM 不可读（§3.1）。
	shadow []shadowEntry

	// dropped 统计因溢出被丢弃的条数，供观测。
	dropped int
}

// New 构造一个 Store。
func New(opt Options) *Store {
	if opt.UnreadLimit <= 0 {
		opt.UnreadLimit = 200
	}
	if opt.DecayPerTick <= 0 {
		opt.DecayPerTick = 0.001
	}
	if opt.ShadowThreshold <= 0 {
		opt.ShadowThreshold = 0.1
	}
	if opt.PromoteWindow <= 0 {
		opt.PromoteWindow = 100
	}
	if opt.PromoteDredges <= 0 {
		opt.PromoteDredges = 3
	}
	if opt.PromoteImportance <= 0 {
		opt.PromoteImportance = 7
	}
	return &Store{opt: opt}
}

// Ingest 收一条消息进 unread 队列。
//
// 只负责入队与打分，**不**改状态——状态只能由 agent 自己的
// goroutine 改（R1）。
//
// 曾经返回"该不该打断"，并驱动 @ 抢占状态。那个语义是错的：
// QQ 里 @ 只是提醒更显眼（弹通知），不掐断你手上的事。显眼程度
// 现在作用在 Importance（淘汰优先级）上，不作用在控制流
// （docs/memory.md §2.1）。
func (s *Store) Ingest(m Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextMsgID++
	m.ID = s.nextMsgID
	if m.Importance == 0 {
		m.Importance = defaultImportance(m)
	}

	s.messages = append(s.messages, m)
	s.evictUnreadLocked()
}

// defaultImportance 是入队时的本地启发式打分。
//
// 不等 LLM：unread 溢出淘汰需要一个立即可用的重要性，
// 而 LLM 打分发生在写入待选区时（§5.3）。两者用途不同。
func defaultImportance(m Message) int {
	switch {
	case m.MentionsMe:
		return 8
	case m.RepliesToMe:
		return 7
	default:
		return 2
	}
}

// evictUnreadLocked 在上限之外**按重要性丢弃**，不是丢最旧的（§2.2）。
func (s *Store) evictUnreadLocked() {
	for len(s.messages) > s.opt.UnreadLimit {
		// 找重要性最低的一条；相同则丢最旧的。
		victim := 0
		for i := 1; i < len(s.messages); i++ {
			if s.messages[i].Importance < s.messages[victim].Importance {
				victim = i
			}
		}
		s.messages = append(s.messages[:victim], s.messages[victim+1:]...)
		s.dropped++
	}
}

// UnreadCount 返回未读计数。它是心境输入（99+ 推高"烦躁"，§2.2）。
//
// 只数游标之后的，不是数存储总量：消息读过后仍留着（供 Browse 翻旧账），
// 但它们不该继续推高"烦躁"。
func (s *Store) UnreadCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.messages {
		if m.ID > s.cursor {
			n++
		}
	}
	return n
}

// UnreadMentions 返回未读里"提到我"的条数（@我 或回复我）。
//
// 与 UnreadCount 一样**不受可见性门控**：这是"手机上有一条在叫我"
// 这个事实，不是消息内容。它必须能在没看手机时被知道——否则模型只
// 看到"还有 5 条没看"，看不出其中一条是专门叫她的，"要不要去看看"
// 就少了一半依据（memory.md §2.1 的"更显眼"）。
//
// 这两类在**看到内容时**还会被 formatMessage 标出来；本方法是它们
// 在**没看到内容时**的唯一痕迹。
func (s *Store) UnreadMentions() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.messages {
		if m.ID > s.cursor && (m.MentionsMe || m.RepliesToMe) {
			n++
		}
	}
	return n
}

// Dropped 返回因溢出被丢弃的条数。
func (s *Store) Dropped() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dropped
}

// scanMessages 模拟"解锁手机扫一眼"：返回**最近** n 条未读消息，并推进游标（§2.3）。
//
// 是"最近"而不是"最旧"：看手机看到的是屏幕底下那几条新消息。
// 已读游标是关键——没有它，退出再进入"看 QQ"会返回同样内容、原地空转。
// 返回按时间正序，便于直接拼进 prompt。
//
// ponytail: 单个单调游标，若未读超过 n，被跳过的中间那几条会被记成"已读"
// 而不再出现在扫描里（仍可用翻页读到）。上限是"扫一眼只扫最新几条"；
// 升级路径是改成逐条已读位图。
func (s *Store) scanMessages(n int) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n <= 0 {
		return nil
	}
	// 从最新往回收集未读，凑够 n 条。
	var picked []Message
	for i := len(s.messages) - 1; i >= 0 && len(picked) < n; i-- {
		m := s.messages[i]
		if m.ID <= s.cursor {
			continue
		}
		picked = append(picked, m)
	}
	if len(picked) == 0 {
		return nil
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].ID < picked[j].ID })
	// 游标推进到这次看到的最新一条。
	s.cursor = picked[len(picked)-1].ID
	return picked
}

// Cursor 返回当前已读游标（供测试与观测）。
func (s *Store) Cursor() MessageID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cursor
}

// browseMessages 主动向前翻页（§2.3 的翻阅工具）。
//
// 与 scanMessages 的区别：那是"解锁手机扫一眼"（拿未读、推进已读游标）；
// 这是"往上翻聊天记录"（拿更早的**已读**内容，回退浏览位置）。
// 两者各有自己的游标，否则"扫一眼"与"翻旧账"会互相打架。
func (s *Store) browseMessages(n int) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n <= 0 {
		return nil
	}
	// 浏览位置初始化为当前已读游标：从"我刚看到的地方"往旧翻。
	if s.browseCursor == 0 {
		s.browseCursor = s.cursor
	}

	// 收集比浏览位置更旧的消息，从新到旧。
	var picked []Message
	for i := len(s.messages) - 1; i >= 0; i-- {
		m := s.messages[i]
		if s.browseCursor != 0 && m.ID >= s.browseCursor {
			continue
		}
		picked = append(picked, m)
		if len(picked) >= n {
			break
		}
	}
	if len(picked) == 0 {
		return nil
	}
	// 浏览位置回退到最旧一条，下次继续往旧翻。
	s.browseCursor = picked[len(picked)-1].ID

	// 返回时按时间正序，便于直接拼进 prompt。
	sort.Slice(picked, func(i, j int) bool { return picked[i].ID < picked[j].ID })
	return picked
}

// WriteStaging 直接写入一条待选区条目，返回新条目。
//
// 生产路径走 rememberSeen（"看到的都写"），本方法是**显式写入**的
// 接缝：测试用它摆出任意待选区状态；将来的工具层（send_message 之类
// 需要记下"我说了什么"）也用它。keywords 会被切成词元，与查询侧
// 走同一条 tokenize，否则又会出现"两侧各写各的、合起来不命中"。
//
// now 为 0 表示"用存储当前的 tick"：显式写入的调用方常常没有 tick
// 可给，而**不能**让 0 覆盖掉存储时钟——那会把后续 Scan 的会话分组
// 全部拽回 tick 0，同一批翻阅被误判成"隔了很久"。
func (s *Store) WriteStaging(session SessionID, text string, keywords []string, importance int, now fsm.Tick) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	if session == 0 {
		s.nextSessID++
		session = s.nextSessID
	}
	at := now
	if at == 0 {
		at = s.now
	}
	return s.appendEntryLocked(session, text, keywords, importance, at)
}

// appendEntryLocked 是待选区唯一的追加点（调用方须持锁）。
func (s *Store) appendEntryLocked(session SessionID, text string, keywords []string, importance int, at fsm.Tick) *Entry {
	if importance < 1 {
		importance = 1
	}
	if importance > 10 {
		importance = 10
	}
	s.nextEntryID++
	e := &Entry{
		ID:         s.nextEntryID,
		Session:    session,
		Text:       text,
		Keywords:   tokensOf(keywords),
		Importance: importance,
		Strength:   1.0,
		Created:    at,
	}
	s.staging = append(s.staging, e)
	return e
}

// Record 把"她说过的话"写入待选区（§5.1：说出去的话也进记忆）。
//
// 与 Scan/Browse 的自动写入不同，这是**显式**写入：内容不来自外部消息，
// 而来自她自己。由 fsm 的 Recorder 缝口调用。
//
// 会话沿用**当前那次翻阅**：她看到的话与她回的话属于同一段情节记忆，
// 拆成两段会让"那你去吧"式的指代重新失去上下文——而"整段返回"的整个
// 理由就是防止这种指代失义。
//
// 不接收 tick 参数：本包的时钟只由 Tick 驱动（R8），从外面塞一个 tick
// 进来会与衰减/升格用的时间对不上。
func (s *Store) Record(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendEntryLocked(s.sessionForLocked(), text, []string{text}, ownWordsImportance, s.now)
}

// ownWordsImportance 是"自己说过的话"的重要性。
//
// 6：高于普通灌水（2），低于 @ 我（8）。她说的话比水群值得记，
// 但不该压过别人专门叫她——那才是更该被留住的事。
const ownWordsImportance = 6

// NewSession 分配一个新的翻阅会话 ID。
func (s *Store) NewSession() SessionID {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSessID++
	return s.nextSessID
}

// rememberSeen 把"刚看到的一批消息"写入待选区（§3 的入口）。
//
// 这是整条记忆链路唯一的写入方。没有它，staging 恒为空，于是
// 打捞永远捞不到东西、升格永不触发、Shadow 永不增长——四层记忆
// 全部有实现有测试，却在真实运行中一层都不动。
//
// 两个设计决定：
//
//  1. **会话按时间邻近合并。** 同一次"看手机"里陆续扫到的几条消息
//     属于同一次翻阅，而不是各成一段。§5.1 要的检索单位是"那天翻到的
//     那一段"：Pump 每 tick 捞到一条就开新会话的话，每段只有一句话，
//     恰好退化成它想避免的"孤立单条"。
//  2. **关键词由本地词元化产出**，不调 LLM。用户明确"看手机时看到的
//     都写"，写入是自动的，因此不能挂在一次工具调用上；而 §5.1 原写
//     "存储时由 LLM 生成关键词"需要额外一次调用，与"写入自动"冲突。
//     ponytail: 本地 2/3-gram 的召回不如 LLM 生成的关键词准（同义词、
//     代词仍然匹配不上）。升级路径就是 §5.1 写的查询扩展——真要接
//     时，让独白顺带产出关键词即可（独白本来就在回看刚发生的事），
//     不必新增调用点。
func (s *Store) rememberSeen(msgs []Message) {
	if len(msgs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	session := s.sessionForLocked()

	for _, m := range msgs {
		// 关键词基于 messageBody：不含"（提到了你）"这类给模型看的标记。
		s.appendEntryLocked(session, formatMessage(m), []string{messageBody(m)}, m.Importance, s.now)
	}
}

// sessionForLocked 返回本批写入应当归属的会话 ID。
//
// 与上一批写入在 sessionGap 内则复用，否则新开一次翻阅。
func (s *Store) sessionForLocked() SessionID {
	if s.lastSession != 0 && s.now-s.lastSessionAt <= sessionGap {
		s.lastSessionAt = s.now
		return s.lastSession
	}
	s.nextSessID++
	s.lastSession = s.nextSessID
	s.lastSessionAt = s.now
	return s.lastSession
}

// sessionGap 是"同一次翻阅"的最大 tick 间隔。
//
// 30 tick = 半小时游戏时间。取值的理由：一次刷手机通常连着看几条，
// 而两次独立的"看手机"之间会隔着别的状态（干活、发呆），远不止半小时。
// 太小会把一次翻阅切碎，太大则把隔了很久的两段揉成一段。
const sessionGap fsm.Tick = 30

// tokensOf 把一组文本（查询词、关键词）统一切成词元并去重。
//
// **写入侧与查询侧必须走同一个函数**，这是打捞能成立的不变式。
// 之前两侧各有一套：写入存整词（"关键词"），查询也传整句
// （"翻翻昨天聊过的记录打发时间"），于是 matchesAny 拿整句去
// strings.Contains，永远为假——线上【想起的事】恒空，而代码路径
// 看着是通的（Dredge 有生产调用方、有单测覆盖）。
// 单测没发现是因为测试两边都传同一个词，恰好能对上。
func tokensOf(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		for _, t := range tokenize(s) {
			if seen[t] {
				continue
			}
			if len(out) >= maxTokens {
				return out
			}
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Dredge 按查询打捞，返回命中的**整段**内容（§5.1），并刷新其记忆曲线。
//
// query **可以是自然语言**（调用方给的就是意图原句、想法原句），本方法
// 自己切词元。此前这里假设"调用方已展开成关键词集合"，但调用方
// （fsm.dredgeQuery）给的是整句，而整句做子串匹配恒不命中——契约两侧
// 各写各的，谁都没错，合起来就是线上打捞永远为空。
// 现在契约收在这里：**给自然语言，我负责切**。
//
// 不做同义词扩展——那是 LLM 的活（§5.1 的查询扩展），本方法只做匹配。
//
// 返回的是去重后的会话文本列表：以"一次翻阅"为检索单位。
func (s *Store) Dredge(query []string, now fsm.Tick) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := tokensOf(query)
	if len(want) == 0 {
		return nil
	}

	// 词元的文档频率：它出现在待选区多少条里。
	//
	// 这是精度控制的关键，不是优化。打捞有**副作用**（刷新强度、
	// 累计升格次数），所以"什么都捞"不只是 prompt 变吵，还会把无关
	// 内容一路顶成事件记忆，记忆层就废了。反过来，只按固定个数
	// 卡阈值也不行：短条目（"周末去看展吗"）与整句查询通常只共享
	// 一个词，那恰恰是最该命中的情况。
	//
	// 规则：**只在这一条里出现过的词元，一个就够；越常见越需要互相印证。**
	df := make(map[string]int, len(want))
	for _, w := range want {
		for _, e := range s.staging {
			if entryMatches(e, w) {
				df[w]++
			}
		}
	}

	// 查询本身词元就少时无法互相印证，只能降为"有一个算一个"。
	need := minGramHits
	if len(want) < need {
		need = len(want)
	}

	// 先找出命中的会话。
	hitSessions := map[SessionID]bool{}
	for _, e := range s.staging {
		if entryHits(e, want, df, need) {
			e.Strength = 1.0 // 打捞刷新记忆曲线（§4）
			e.LastDredged = now
			e.DredgeTicks = append(e.DredgeTicks, now)
			hitSessions[e.Session] = true
		}
	}
	if len(hitSessions) == 0 {
		return nil
	}

	// 再取这些会话的**全部**条目，按时间正序拼成整段。
	// 单条脱离上下文会失去含义（"那你去吧"指什么？），
	// 所以检索单位是"一次翻阅"而不是单条记忆（§5.1）。
	grouped := map[SessionID][]*Entry{}
	var order []SessionID
	for _, e := range s.staging {
		if !hitSessions[e.Session] {
			continue
		}
		if _, ok := grouped[e.Session]; !ok {
			order = append(order, e.Session)
		}
		grouped[e.Session] = append(grouped[e.Session], e)
	}

	var out []string
	for _, sid := range order {
		entries := grouped[sid]
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		parts := make([]string, 0, len(entries))
		for _, e := range entries {
			parts = append(parts, e.Text)
		}
		out = append(out, strings.Join(parts, "\n"))
	}
	return out
}

// entryMatches 判断一条待选区条目是否含某个词元。
//
// 同时比对写入时算好的 Keywords 与正文：Keywords 是精确集合命中，
// 正文兜住"关键词没覆盖到但字面确实出现"的情况。
func entryMatches(e *Entry, w string) bool {
	for _, k := range e.Keywords {
		if k == w {
			return true
		}
	}
	return strings.Contains(strings.ToLower(e.Text), w)
}

// entryHits 判断一条条目是否算命中查询。
//
// 规则：把条目命中的词元按"稀有度"排序，稀有的一个就够，
// 常见词需要凑够 need 个不同的词元互相印证。
//
// 例：查询"翻翻昨天聊过的看展的事打发时间"，条目"周末去看展吗"只共享
// "看展"——但"看展"在整个待选区里只出现这一次，于是单凭它就命中。
// 若待选区里十条都在聊看展，"看展"就不稀奇了，得再有别的词元佐证。
func entryHits(e *Entry, want []string, df map[string]int, need int) bool {
	hits := 0
	rare := false
	for _, w := range want {
		if !entryMatches(e, w) {
			continue
		}
		hits++
		if df[w] <= 1 {
			rare = true
		}
	}
	return rare || hits >= need
}

// Tick 推进记忆曲线并做升格/遗忘（§4、§5.3）。
//
// 由 agent 的 tick 循环每次调用。全部判定按 tick，不按 wall clock（R8）。
func (s *Store) Tick(now fsm.Tick) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 记住当前 tick：rememberSeen 用它给会话分组、给条目打 Created。
	// 这是本包唯一的"时钟"，只在 Tick 里前进（R8）。
	s.now = now
	s.decayLocked(now)
	s.promoteLocked(now)
}

// decayLocked 衰减待选区强度，低于阈值者沉入 Shadow（§4）。
func (s *Store) decayLocked(now fsm.Tick) {
	kept := s.staging[:0]
	for _, e := range s.staging {
		e.Strength -= s.opt.DecayPerTick
		if e.Strength < s.opt.ShadowThreshold {
			// 沉入 Shadow：数据保留、可审计，但不再参与任何打捞。
			s.shadow = append(s.shadow, shadowEntry{
				Kind: "staging", Text: e.Text,
				Reason: fmt.Sprintf("强度衰减至 %.3f < %.3f", e.Strength, s.opt.ShadowThreshold),
				At:     now,
			})
			continue // 从待选区移除
		}
		kept = append(kept, e)
	}
	s.staging = kept
}

// promoteLocked 判定升格（§5.3），源条目**删除**（§3.2）。
//
// 删除而非保留的理由：保留会导致同一条被反复升格，生成多个相似事件记忆。
func (s *Store) promoteLocked(now fsm.Tick) {
	kept := s.staging[:0]
	for _, e := range s.staging {
		n := e.DredgeCountWithin(now, s.opt.PromoteWindow)
		byFrequency := n >= s.opt.PromoteDredges
		byImportance := e.Importance >= s.opt.PromoteImportance && len(e.DredgeTicks) >= 1
		if !byFrequency && !byImportance {
			kept = append(kept, e)
			continue
		}

		s.nextEventID++
		s.events = append(s.events, Event{
			ID:          s.nextEventID,
			Text:        e.Text,
			Keywords:    e.Keywords,
			Importance:  e.Importance,
			PromotedAt:  now,
			FromSession: e.Session,
		})
		// 源条目删除（§3.2），不放回 kept。
	}
	s.staging = kept
}

// Staging 返回当前待选区条目的快照（只读用途）。
//
// 深拷贝 DredgeTicks：否则调用方拿到的是内部切片的别名，
// 一次 append 就可能改写活数据。
func (s *Store) Staging() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.staging))
	for _, e := range s.staging {
		c := *e
		c.DredgeTicks = append([]fsm.Tick(nil), e.DredgeTicks...)
		c.Keywords = append([]string(nil), e.Keywords...)
		out = append(out, c)
	}
	return out
}

// Events 返回事件记忆快照。
func (s *Store) Events() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// ShadowLen 返回 Shadow 条数。
func (s *Store) ShadowLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.shadow)
}

// Counts 汇总各层条数，供观测。
//
// 存在的理由很具体：这条链路曾经"看着是通的、实际一条都没写"，
// 而且四层全有实现、全有单测。光看代码与测试都发现不了，
// 必须在真实运行中能一眼看出 staging 是不是 0。
type Counts struct {
	Messages int
	Staging  int
	Events   int
	Shadow   int
	Dropped  int
}

// Counts 返回各层条数。
func (s *Store) Counts() Counts {
	return Counts{
		Messages: s.MessagesLen(),
		Staging:  s.StagingLen(),
		Events:   s.EventsLen(),
		Shadow:   s.ShadowLen(),
		Dropped:  s.Dropped(),
	}
}

// MessagesLen 返回 unread 队列条数（含已读，见 §2.2）。
func (s *Store) MessagesLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.messages)
}

// StagingLen 返回待选区条数。
//
// 与 ShadowLen 一样是**基本类型返回**，好让别的包用结构化接口接上
// （transport 只想知道"是不是 0"，不该为此 import 本包）。
func (s *Store) StagingLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.staging)
}

// EventsLen 返回事件记忆条数。
func (s *Store) EventsLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// ShadowAudit 是**给人看的**审计接口（§3.1）。
//
// 它只能被调试/审计路径调用；**绝不可**把结果拼进 prompt。
// 如果需要给 agent 用，那说明该数据不该在 Shadow 里。
func (s *Store) ShadowAudit() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.shadow))
	for _, e := range s.shadow {
		out = append(out, fmt.Sprintf("[%s @tick %d] %s（原因：%s）", e.Kind, e.At, e.Text, e.Reason))
	}
	return out
}
