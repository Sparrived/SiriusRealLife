package memory

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

func newStore() *Store { return New(DefaultOptions()) }

// TestMentionScoresHigherThanChat 验证 §2.1 取消"打断"后的新落点：
// @我/回复我的差别体现在**重要性打分**（淘汰优先级），而不是抢占。
//
// 旧行为是 Ingest 返回 bool 供 agent 决定抢不抢占。抢占取消后，
// 这个区别唯一的去处就是 Importance——它必须仍然生效，否则 @ 的
// 重要消息会因为和水群同分而先被淘汰掉。
func TestMentionScoresHigherThanChat(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want int
	}{
		{"@我", Message{Text: "@我 在吗", MentionsMe: true}, 8},
		{"回复我", Message{Text: "同意", RepliesToMe: true}, 7},
		{"普通群消息", Message{Text: "今天天气不错"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore()
			s.Ingest(c.msg)
			if got := s.messages[0].Importance; got != c.want {
				t.Errorf("重要性 = %d, 期望 %d", got, c.want)
			}
		})
	}
}

// TestUnreadQueueIsBoundedAndDropsByImportance 验证 §2.2/R5：
// 队列有上限，溢出时**按重要性丢弃**而不是丢最旧的。
func TestUnreadQueueIsBoundedAndDropsByImportance(t *testing.T) {
	opt := DefaultOptions()
	opt.UnreadLimit = 5
	s := New(opt)

	// 一条高重要性的"@我"最先到，随后灌入大量低重要性消息。
	// 若按"丢最旧"实现，这条会被丢掉——这正是本测试要抓的。
	s.Ingest(Message{Text: "重要的@", MentionsMe: true})
	for i := 0; i < 20; i++ {
		s.Ingest(Message{Text: fmt.Sprintf("水群 %d", i)})
	}

	if got := s.UnreadCount(); got > 5 {
		t.Fatalf("未读计数 = %d, 超过上限 5", got)
	}
	if s.Dropped() == 0 {
		t.Error("应当记录丢弃计数")
	}
	// 高重要性那条必须还在（它是队列里最旧的一条）。
	found := false
	for _, m := range s.messages {
		if m.MentionsMe {
			found = true
		}
	}
	if !found {
		t.Error("溢出后高重要性的 @我 被丢掉了（应当丢重要性最低的，而不是最旧的）")
	}
	// 反过来确认：丢掉的是低重要性的水群消息。
	for _, m := range s.messages {
		if m.Importance < 2 {
			t.Errorf("应当优先丢掉低重要性消息，却留下了 importance=%d 的 %q", m.Importance, m.Text)
		}
	}
}

// TestScanAdvancesCursor 验证 §2.3：已读游标必须推进，
// 否则退出再进入"看 QQ"会返回同样内容、原地空转。
func TestScanAdvancesCursor(t *testing.T) {
	s := newStore()
	for i := 0; i < 6; i++ {
		s.Ingest(Message{Text: fmt.Sprintf("msg %d", i)})
	}

	first := s.scanMessages(3)
	if len(first) != 3 {
		t.Fatalf("首次扫一眼返回 %d 条, 期望 3", len(first))
	}
	// 扫一眼看到的是**最近**的：ID 4,5,6。
	if first[2].ID != 6 {
		t.Errorf("扫一眼应拿到最新的消息（ID 6），实际最后一条 ID=%d", first[2].ID)
	}
	if c := s.Cursor(); c != 6 {
		t.Errorf("游标 = %d, 期望 6", c)
	}

	// 再来新消息，扫一眼应拿到新的，而不是重复旧的。
	for i := 0; i < 2; i++ {
		s.Ingest(Message{Text: fmt.Sprintf("新消息 %d", i)})
	}
	second := s.scanMessages(3)
	if len(second) != 2 {
		t.Fatalf("第二次扫一眼返回 %d 条, 期望 2（只剩 2 条新的）", len(second))
	}
	for _, a := range first {
		for _, b := range second {
			if a.ID == b.ID {
				t.Fatalf("两次扫一眼返回了同一条消息 %d（游标没推进）", a.ID)
			}
		}
	}
	// 没有新消息时再扫应当为空。
	if got := s.scanMessages(3); len(got) != 0 {
		t.Errorf("没有新消息时应返回空，得到 %d 条", len(got))
	}
}

// TestUnreadCountOnlyCountsAfterCursor 验证读过的不再推高"烦躁"。
func TestUnreadCountOnlyCountsAfterCursor(t *testing.T) {
	s := newStore()
	for i := 0; i < 5; i++ {
		s.Ingest(Message{Text: "x"})
	}
	if got := s.UnreadCount(); got != 5 {
		t.Fatalf("未读计数 = %d, 期望 5", got)
	}
	s.scanMessages(5)
	if got := s.UnreadCount(); got != 0 {
		t.Fatalf("扫完后未读计数 = %d, 期望 0", got)
	}
}

// TestBrowsePagesBackwards 验证 §2.3 的第二级：翻页能往更旧翻，且不与
// 已读游标打架（两级读取各有自己的位置）。
func TestBrowsePagesBackwards(t *testing.T) {
	s := newStore()
	for i := 0; i < 10; i++ {
		s.Ingest(Message{Text: fmt.Sprintf("msg %d", i)})
	}
	s.scanMessages(3) // 看了最新 3 条（ID 8,9,10）

	page1 := s.browseMessages(3)
	if len(page1) != 3 {
		t.Fatalf("翻页返回 %d 条, 期望 3", len(page1))
	}
	// 翻到的必须比已读游标更旧。
	for _, m := range page1 {
		if m.ID >= s.Cursor() {
			t.Errorf("翻到 ID=%d, 不应 >= 游标 %d", m.ID, s.Cursor())
		}
	}
	page2 := s.browseMessages(3)
	// 第二页不能与第一页重叠。
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Fatalf("翻页重叠：ID %d 出现两次", a.ID)
			}
		}
	}
	// 正序返回，便于拼进 prompt。
	for i := 1; i < len(page2); i++ {
		if page2[i-1].ID > page2[i].ID {
			t.Error("翻页结果应按时间正序返回")
		}
	}
}

// TestDredgeReturnsWholeSession 验证 §5.1：打捞返回**整段**翻阅上下文，
// 而不是孤立的一条——单条会失去含义。
func TestDredgeReturnsWholeSession(t *testing.T) {
	s := newStore()
	sess := s.NewSession()
	// 同一会话的三条：只有第一条含关键词，另两条是它的上下文。
	s.WriteStaging(sess, "周末去看展吗", []string{"看展", "周末"}, 5, 1)
	s.WriteStaging(sess, "好啊", nil, 3, 1)
	s.WriteStaging(sess, "那你去吧", nil, 3, 1)
	// 另一个会话，不该被带出来。
	other := s.NewSession()
	s.WriteStaging(other, "记得交周报", []string{"周报"}, 6, 1)

	got := s.Dredge([]string{"看展"}, 10)
	if len(got) != 1 {
		t.Fatalf("命中会话数 = %d, 期望 1", len(got))
	}
	seg := got[0]
	for _, want := range []string{"周末去看展吗", "好啊", "那你去吧"} {
		if !strings.Contains(seg, want) {
			t.Errorf("整段里缺少 %q；实际为 %q", want, seg)
		}
	}
	if strings.Contains(seg, "周报") {
		t.Error("不该把别的会话带出来")
	}
}

// TestDredgeRefreshesStrength 验证 §4：打捞行为本身刷新记忆曲线。
func TestDredgeRefreshesStrength(t *testing.T) {
	s := newStore()
	s.WriteStaging(s.NewSession(), "内容", []string{"关键词"}, 5, 1)

	// 衰减一阵子。
	for i := 0; i < 300; i++ {
		s.Tick(fsm.Tick(i))
	}
	before := s.Staging()[0].Strength
	if before >= 1.0 {
		t.Fatalf("应当已衰减，实际强度 %v", before)
	}

	s.Dredge([]string{"关键词"}, 400)
	after := s.Staging()[0].Strength
	if after != 1.0 {
		t.Fatalf("打捞后强度 = %v, 期望刷新为 1.0", after)
	}
}

// TestDecayToShadow 验证 §4 与验收 6：长期不打捞的记忆沉入 Shadow，
// 且 Shadow 内容不再可被打捞（LLM 读不到）。
func TestDecayToShadow(t *testing.T) {
	s := newStore()
	s.WriteStaging(s.NewSession(), "会被忘掉的事", []string{"秘密关键词"}, 3, 0)

	// 跑足够久让它衰减到阈值以下。
	for i := 0; i < 1200; i++ {
		s.Tick(fsm.Tick(i))
	}
	if s.ShadowLen() == 0 {
		t.Fatal("长期不打捞的记忆应当沉入 Shadow")
	}
	if len(s.Staging()) != 0 {
		t.Fatal("沉入 Shadow 后不应留在待选区")
	}
	// 关键：Shadow 里的东西**打捞不到**（LLM 不可读）。
	if got := s.Dredge([]string{"秘密关键词"}, 1300); len(got) != 0 {
		t.Fatalf("Shadow 内容不应被打捞出来，却得到 %v", got)
	}
	// 但审计接口能拿到（给人看，§3.1）。
	if len(s.ShadowAudit()) == 0 {
		t.Fatal("审计接口应当能读到 Shadow")
	}
	// 审计内容里应当有原文，证明是"存档"而非"删除"。
	if !strings.Contains(strings.Join(s.ShadowAudit(), "\n"), "会被忘掉的事") {
		t.Error("Shadow 应保留原文以供审计")
	}
}

// TestPromoteByFrequencyDeletesSource 验证 §5.3 + §3.2：
// 高频打捞触发升格，且**源条目删除**（防重复升格产生相似事件记忆）。
func TestPromoteByFrequencyDeletesSource(t *testing.T) {
	s := newStore()
	s.WriteStaging(s.NewSession(), "被反复想起的事", []string{"反复"}, 5, 0)

	// 窗口内打捞 3 次（PromoteDredges 默认 3）。
	for i := 0; i < 3; i++ {
		s.Dredge([]string{"反复"}, fsm.Tick(10+i))
	}
	s.Tick(20)

	if len(s.Events()) != 1 {
		t.Fatalf("事件记忆数 = %d, 期望 1", len(s.Events()))
	}
	if len(s.Staging()) != 0 {
		t.Fatal("升格后源条目必须删除（否则会被反复升格）")
	}

	// 再跑若干 tick，不应产生第二条相同的事件记忆。
	for i := 0; i < 200; i++ {
		s.Tick(fsm.Tick(21 + i))
	}
	if n := len(s.Events()); n != 1 {
		t.Fatalf("事件记忆数变成了 %d，源条目未被删除", n)
	}
}

// TestPromoteByImportanceWithSingleDredge 验证 §5.3 的重要性对冲：
// 高 importance 只需被打捞一次即可升格，避免"被 @ 的重要消息因没人翻到而消失"。
func TestPromoteByImportanceWithSingleDredge(t *testing.T) {
	s := newStore()
	s.WriteStaging(s.NewSession(), "很重要的事", []string{"重要"}, 8, 0)

	s.Dredge([]string{"重要"}, 5)
	s.Tick(6)

	if len(s.Events()) != 1 {
		t.Fatalf("importance=8 且被打捞一次应当升格，事件数 = %d", len(s.Events()))
	}
}

// TestLowImportanceNeedsRepeatedDredge 验证低重要性不会被单次打捞升格。
func TestLowImportanceNeedsRepeatedDredge(t *testing.T) {
	s := newStore()
	s.WriteStaging(s.NewSession(), "琐事", []string{"琐事"}, 2, 0)

	s.Dredge([]string{"琐事"}, 5)
	s.Tick(6)

	if len(s.Events()) != 0 {
		t.Fatal("低重要性且只打捞一次不应升格")
	}
}

// TestDredgeWindowExpiry 验证滑动窗口：久远的打捞不计入升格判定。
func TestDredgeWindowExpiry(t *testing.T) {
	s := newStore()
	s.WriteStaging(s.NewSession(), "很久以前被翻过", []string{"久远"}, 2, 0)

	// 三次打捞都发生在窗口之外（窗口默认 100 tick）。
	for i := 0; i < 3; i++ {
		s.Dredge([]string{"久远"}, fsm.Tick(i))
	}
	// 推进到窗口已过期。
	for t := fsm.Tick(200); t < 260; t++ {
		s.Tick(t)
	}
	if len(s.Events()) != 0 {
		t.Fatal("窗口外的打捞不应触发升格")
	}
}

// TestDredgeEmptyQueryReturnsNothing 验证空查询不误命中。
func TestDredgeEmptyQueryReturnsNothing(t *testing.T) {
	s := newStore()
	s.WriteStaging(s.NewSession(), "任何内容", []string{"k"}, 5, 0)
	if got := s.Dredge(nil, 1); len(got) != 0 {
		t.Fatalf("空查询应当不命中，却得到 %v", got)
	}
	if got := s.Dredge([]string{"  "}, 1); len(got) != 0 {
		t.Fatalf("空白查询应当不命中，却得到 %v", got)
	}
}

// TestEmphasisOnlyEntryNotDredgedByOtherSession 验证会话隔离：
// 命中一个会话不会把另一个会话的相似内容带出来。
func TestSessionIsolation(t *testing.T) {
	s := newStore()
	a := s.NewSession()
	s.WriteStaging(a, "聊看展", []string{"看展"}, 5, 0)
	b := s.NewSession()
	s.WriteStaging(b, "另一个会话也提了看展", []string{"看展"}, 5, 0)

	got := s.Dredge([]string{"看展"}, 5)
	if len(got) != 2 {
		t.Fatalf("两个会话都含关键词，应返回 2 段，得到 %d", len(got))
	}
}

// TestKeywordsNormalized 验证关键词归一化（大小写、空白、去重）。
func TestKeywordsNormalized(t *testing.T) {
	s := newStore()
	e := s.WriteStaging(s.NewSession(), "text", []string{"  看展  ", "看展", "KANZHAN", ""}, 5, 0)
	got := e.Keywords
	if len(got) != 2 {
		t.Fatalf("归一化后关键词 = %v, 期望 2 个", got)
	}
	// 小写化后应当能命中。
	if hit := s.Dredge([]string{"kanzhan"}, 1); len(hit) == 0 {
		t.Error("关键词应大小写不敏感")
	}
}

// TestImportanceClamped 验证 importance 被夹到 1–10。
func TestImportanceClamped(t *testing.T) {
	s := newStore()
	lo := s.WriteStaging(s.NewSession(), "a", nil, -5, 0)
	if lo.Importance != 1 {
		t.Errorf("importance = %d, 期望夹到 1", lo.Importance)
	}
	hi := s.WriteStaging(s.NewSession(), "b", nil, 99, 0)
	if hi.Importance != 10 {
		t.Errorf("importance = %d, 期望夹到 10", hi.Importance)
	}
}

// TestScanWritesStaging 验证"看手机时看到的都写"：Scan 有副作用，
// 把看到的消息写入待选区。
//
// 这条是整条记忆链路的**存在性测试**。没有它，staging 恒为空，
// 于是打捞、升格、Shadow 三层在真实运行中永不发生——而它们各自
// 都有单测、都能通过。测试写的是"看手机"这个真实入口（Scan 是
// Pump 与 ReadPhone 的共同收口），不是 WriteStaging。
func TestScanWritesStaging(t *testing.T) {
	s := newStore()
	s.Tick(100)

	for i := 0; i < 3; i++ {
		s.Ingest(Message{From: "张三", Text: fmt.Sprintf("第 %d 条", i)})
	}
	if got := len(s.Staging()); got != 0 {
		t.Fatalf("还没看手机，待选区应为空，实际 %d 条", got)
	}

	s.Scan(5)
	got := s.Staging()
	if len(got) != 3 {
		t.Fatalf("看到 3 条消息应写入 3 条待选区条目，实际 %d", len(got))
	}
	// 记的是"她看到什么"：带发送者，便于日后回忆"谁说过"。
	if !strings.Contains(got[0].Text, "张三") || !strings.Contains(got[0].Text, "第 0 条") {
		t.Errorf("待选区正文应含发送者与内容，实际 %q", got[0].Text)
	}
	// Created 用存储时钟，不是消息自带的 Tick（可能没人填）。
	if got[0].Created != 100 {
		t.Errorf("Created = %d, 期望存储当前 tick 100", got[0].Created)
	}
	// 初始强度满值，此后才开始衰减（§4）。
	if got[0].Strength != 1.0 {
		t.Errorf("初始强度 = %v, 期望 1.0", got[0].Strength)
	}
}

// TestBrowseWritesStaging 验证翻旧账看到的同样写入待选区。
func TestBrowseWritesStaging(t *testing.T) {
	s := newStore()
	for i := 0; i < 6; i++ {
		s.Ingest(Message{Text: fmt.Sprintf("旧账 %d", i)})
	}
	s.Scan(6) // 先全部看过，游标走到头
	before := len(s.Staging())

	s.Browse(3)
	if got := len(s.Staging()) - before; got != 3 {
		t.Fatalf("翻 3 条旧账应新增 3 条待选区条目，实际 %d", got)
	}
}

// TestScanGroupsOneBrowsingSession 验证会话分组：同一次"看手机"里
// 陆续看到的几条属于同一次翻阅，而不是各成一段。
//
// 为什么要紧：打捞的检索单位是"一次翻阅"（§5.1），返回整段上下文。
// 若 Pump 每 tick 捞到一条就开一个新会话，每段只剩一句话，
// 恰好退化成 §5.1 想避免的"孤立单条"（"那你去吧"指什么？）。
func TestScanGroupsOneBrowsingSession(t *testing.T) {
	s := newStore()
	s.Tick(10)
	s.Ingest(Message{Text: "周末去看展吗"})
	s.Scan(1)

	// 隔几 tick 又来一条——仍算同一次翻阅（间隔远小于 sessionGap）。
	s.Tick(12)
	s.Ingest(Message{Text: "那你去吧"})
	s.Scan(1)

	got := s.Staging()
	if len(got) != 2 {
		t.Fatalf("应有 2 条待选区条目，实际 %d", len(got))
	}
	if got[0].Session != got[1].Session {
		t.Errorf("相隔 2 tick 的两条应属同一次翻阅，会话 %d != %d",
			got[0].Session, got[1].Session)
	}
	// 整段返回两条：这才是"翻阅"的粒度。
	seg := s.Dredge([]string{"看展"}, 20)
	if len(seg) != 1 || !strings.Contains(seg[0], "那你去吧") {
		t.Fatalf("打捞应返回含上下文的整段，得到 %v", seg)
	}

	// 隔很久之后再看手机，是新的一次翻阅。
	s.Tick(fsm.Tick(12) + sessionGap + 1)
	s.Ingest(Message{Text: "另一天的事"})
	s.Scan(1)
	all := s.Staging()
	if all[len(all)-1].Session == got[0].Session {
		t.Error("隔了 sessionGap 以上应算新的一次翻阅")
	}
}

// TestDredgeMatchesNaturalLanguageQuery 是这条链路最关键的回归测试。
//
// 调用方（fsm.dredgeQuery）给的是**整句**意图与想法，不是关键词。
// 旧实现的契约写的是"调用方已展开成关键词集合"，于是拿整句去
// strings.Contains，永远为假：线上【想起的事】恒不出现，而 Dredge
// 有生产调用方、有单测覆盖、看着完全正常。
//
// 本测试刻意只走真实入口：Scan 写入（不手工塞关键词），
// 再用整句查询。旧的子串匹配在这里必然失败。
func TestDredgeMatchesNaturalLanguageQuery(t *testing.T) {
	s := newStore()
	s.Tick(100)
	s.Ingest(Message{From: "张三", Text: "周末去看展吗"})
	s.Ingest(Message{From: "我", Text: "好啊"})
	s.Scan(5)

	// 整句意图——正是 dredgeQuery 会传进来的形状。
	got := s.Dredge([]string{"翻翻昨天聊过的看展的事打发时间"}, 110)
	if len(got) == 0 {
		t.Fatal("整句查询应能打捞出含相关内容的翻阅段落——" +
			"若失败说明查询侧没有切词元，整句永远匹配不上")
	}
	if !strings.Contains(got[0], "周末去看展吗") {
		t.Errorf("打捞结果应含相关消息，实际 %q", got[0])
	}

	// 反向：查询里没有的词元不该命中，否则等于"什么都捞"。
	if got := s.Dredge([]string{"明天记得交周报"}, 110); len(got) != 0 {
		t.Errorf("无关查询不应命中，却得到 %v", got)
	}
}

// TestTokenizeBasics 钉住词元化的形状：中文出 2/3-gram，拉丁整词。
func TestTokenizeBasics(t *testing.T) {
	got := tokenize("看展 kanzhan")
	want := map[string]bool{"看展": true, "kanzhan": true}
	for w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("词元里缺少 %q，实际 %v", w, got)
		}
	}
	// 单字不成词元（太泛），标点不产出词元。
	for _, g := range tokenize("的，。") {
		if len([]rune(g)) < 2 {
			t.Errorf("不应产出单字词元 %q", g)
		}
	}
	// 虚词组合被滤掉。
	for _, g := range tokenize("什么") {
		if g == "什么" {
			t.Error("纯功能词不应作为词元")
		}
	}
}
