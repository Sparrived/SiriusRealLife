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

// Interrupts 报告该消息是否应当即时打断（§2.1）。
//
// @我 与 回复我 都打断；其他静默入队。
func (m Message) Interrupts() bool { return m.MentionsMe || m.RepliesToMe }

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

// Ingest 收一条消息，返回它是否应当即时打断（§2.1）。
//
// 打断与否由调用方（agent）决定：本方法只负责入队与判定，
// 并**不**自己改状态——状态只能由 agent 自己的 goroutine 改（R1）。
func (s *Store) Ingest(m Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextMsgID++
	m.ID = s.nextMsgID
	if m.Importance == 0 {
		m.Importance = defaultImportance(m)
	}

	s.messages = append(s.messages, m)
	s.evictUnreadLocked()
	return m.Interrupts()
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

// WriteStaging 把一次翻阅的内容写入待选区，返回新条目。
//
// keywords 与 importance 由 LLM 在同一次调用里产出（§5.3）：
// 关键词用于打捞，importance 用于升格对冲。
func (s *Store) WriteStaging(session SessionID, text string, keywords []string, importance int, now fsm.Tick) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	if session == 0 {
		s.nextSessID++
		session = s.nextSessID
	}
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
		Keywords:   normalizeKeywords(keywords),
		Importance: importance,
		Strength:   1.0,
		Created:    now,
	}
	s.staging = append(s.staging, e)
	return e
}

// NewSession 分配一个新的翻阅会话 ID。
func (s *Store) NewSession() SessionID {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSessID++
	return s.nextSessID
}

// normalizeKeywords 去空、去重、转小写，保证匹配行为可预期。
func normalizeKeywords(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range in {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// Dredge 按关键词打捞，返回命中的**整段**内容（§5.1），并刷新其记忆曲线。
//
// query 已是调用方（LLM 查询扩展后）展开的关键词集合：本方法不做
// 同义词扩展，只做匹配——扩展是 LLM 的活（§5.1 的补偿手段）。
//
// 返回的是去重后的会话文本列表：以"一次翻阅"为检索单位。
func (s *Store) Dredge(query []string, now fsm.Tick) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := normalizeKeywords(query)
	if len(want) == 0 {
		return nil
	}

	// 先找出命中的会话。
	hitSessions := map[SessionID]bool{}
	for _, e := range s.staging {
		if matchesAny(e.Keywords, e.Text, want) {
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

// matchesAny 做朴素子串匹配。
//
// ponytail: 子串匹配，天花板是同义词/代词匹配不上；升级路径是
// Phase 2 的向量召回（§5.2），关键词那一路保留作精确命中。
func matchesAny(keywords []string, text string, want []string) bool {
	lowText := strings.ToLower(text)
	for _, w := range want {
		for _, k := range keywords {
			if k == w {
				return true
			}
		}
		if strings.Contains(lowText, w) {
			return true
		}
	}
	return false
}

// Tick 推进记忆曲线并做升格/遗忘（§4、§5.3）。
//
// 由 agent 的 tick 循环每次调用。全部判定按 tick，不按 wall clock（R8）。
func (s *Store) Tick(now fsm.Tick) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
