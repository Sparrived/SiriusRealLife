package memory

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// newAgentWithStore 接上真实的 memory.Store 作为 fsm 的 Attention。
//
// 这是"集成"测试：验证 attention.go 的适配真的被 fsm 用上了，
// 而不是各自单测通过、拼起来失效。
func newAgentWithStore(t *testing.T, initial fsm.StateName) (*fsm.Agent, *Store) {
	t.Helper()
	store := New(DefaultOptions())
	a, err := fsm.New(fsm.Options{
		Name:      "t",
		States:    fsm.MVPStates(),
		Initial:   initial,
		Attention: store, // Store 隐式满足 fsm.Attention
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("fsm.New: %v", err)
	}
	return a, store
}

// streamText 把意识流拼成一段文本，便于断言。
func streamText(a *fsm.Agent) string {
	var b strings.Builder
	for _, e := range a.Stream {
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestQQVisibleOnlyInScrollingPhone 是验收标准 5 的集成验证：
// 只有 scrolling_phone 状态能看到 QQ 消息，其他状态不进意识流。
func TestQQVisibleOnlyInScrollingPhone(t *testing.T) {
	a, store := newAgentWithStore(t, "working")
	store.Ingest(Message{From: "小明", Text: "周末去看展吗"})

	// 在 working（QQ 不可见）：消息不应出现在意识流里。
	a.ReadPhone(5)
	if got := streamText(a); strings.Contains(got, "周末去看展吗") {
		t.Fatalf("working 状态下 QQ 消息不应可见，实际意识流：%s", got)
	}
	// 但应当留下"有未读"这个事实（烦躁的来源）。
	if !strings.Contains(streamText(a), "未读") {
		t.Errorf("应当记下「有未读」这个事实，实际：%s", streamText(a))
	}
	// 消息没被消费，游标未动。
	if store.Cursor() != 0 {
		t.Errorf("不可见时不应推进已读游标，实际 %d", store.Cursor())
	}

	// 切到 scrolling_phone（QQ 可见）：这次应当读得到。
	a.Current = "scrolling_phone"
	a.ReadPhone(5)
	if got := streamText(a); !strings.Contains(got, "周末去看展吗") {
		t.Fatalf("scrolling_phone 状态下应能看到 QQ 消息，实际意识流：%s", got)
	}
	if store.Cursor() == 0 {
		t.Error("可见时应当推进已读游标")
	}
}

// TestEnteringScrollingPhoneReadsQQ 验证"状态 = 一段订阅"：
// 进入订阅了 QQ 的状态后，泵入会把未读消息带进意识流。
func TestEnteringScrollingPhoneReadsQQ(t *testing.T) {
	a, store := newAgentWithStore(t, "working")
	store.Ingest(Message{From: "小红", Text: "在吗"})

	// 走真实路径：先推一个 tick 让它进入订阅状态，再泵一次。
	a.Current = "scrolling_phone"
	a.Pump()

	if got := streamText(a); !strings.Contains(got, "在吗") {
		t.Fatalf("进入订阅状态后应泵入未读消息，实际意识流：%s", got)
	}
}

// TestUnreadDrivesMoodInput 验证 §2.2：未读计数可被读取，作为心境输入。
func TestUnreadDrivesMoodInput(t *testing.T) {
	a, store := newAgentWithStore(t, "working")
	if a.Unread() != 0 {
		t.Fatal("初始未读应为 0")
	}
	for i := 0; i < 7; i++ {
		store.Ingest(Message{Text: "水群"})
	}
	if got := a.Unread(); got != 7 {
		t.Fatalf("未读 = %d, 期望 7", got)
	}
	// 未读计数不受可见性门控：它本身不该进模型视野就能影响烦躁。
	if got := streamText(a); strings.Contains(got, "水群") {
		t.Errorf("未读数不该把内容带进意识流：%s", got)
	}
}

// TestBrowsePhoneGatedByVisibility 验证翻页同样受可见性门控。
func TestBrowsePhoneGatedByVisibility(t *testing.T) {
	a, store := newAgentWithStore(t, "working")
	for i := 0; i < 10; i++ {
		store.Ingest(Message{Text: "消息"})
	}
	store.scanMessages(3) // 先看几条，制造"已读"

	// working：不可见，翻不到东西。
	if got := a.BrowsePhone(3); len(got) != 0 {
		t.Errorf("working 状态下不应能翻页，得到 %v", got)
	}

	// scrolling_phone：可见，能翻。
	a.Current = "scrolling_phone"
	if got := a.BrowsePhone(3); len(got) != 3 {
		t.Errorf("scrolling_phone 状态下应能翻到 3 条，得到 %d", len(got))
	}
}

// TestReadPhoneWithNoMessages 验证空手机不报错、留下合理记录。
func TestReadPhoneWithNoMessages(t *testing.T) {
	a, _ := newAgentWithStore(t, "scrolling_phone")
	a.ReadPhone(5)
	if !strings.Contains(streamText(a), "没有新消息") {
		t.Errorf("没有未读时应如实记录，实际：%s", streamText(a))
	}
}

// TestMentionAndReplyAreMarkedInContent 验证"显式提醒有人提及你"。
//
// 这是用户明确要求复刻的 QQ 语义：**@ 与回复不打断她手上的事**，
// 但在她看手机时，这两类会**显眼地被标出来**——现实里 QQ 给这两类
// 弹通知、带红字，普通群消息只有一个红点数字（R12 / memory.md §2.1）。
//
// 差别落在**内容**上，不落在控制流上：`formatMessage` 是唯一把消息
// 渲染给模型看的地方，因此标记加在这里天然覆盖 Scan / Browse /
// Pump / ReadPhone 所有可见路径，不会漏。
//
// 这条守的是"文档说了但代码没做"：§2.1 写了"前两类在 prompt 里被
// 显式标出来"，而 formatMessage 曾经只拼 `发送者: 正文`，
// MentionsMe/RepliesToMe 完全没被读过——于是"更显眼"只剩重要性打分，
// 而模型在正文里看不出这条是在叫她。
func TestMentionAndReplyAreMarkedInContent(t *testing.T) {
	a, store := newAgentWithStore(t, "scrolling_phone")
	store.Ingest(Message{From: "张三", Text: "在吗", MentionsMe: true})
	store.Ingest(Message{From: "李四", Text: "怎么说", RepliesToMe: true})
	store.Ingest(Message{From: "王五", Text: "今天天气不错"})

	a.ReadPhone(5)
	got := streamText(a)

	// @ 与回复必须显式标出"提到了你 / 回复了你"。
	if !strings.Contains(got, "提到") {
		t.Errorf("@我的消息应当在内容里标出『提到』，实际：%s", got)
	}
	if !strings.Contains(got, "回复") {
		t.Errorf("回复我的消息应当在内容里标出『回复』，实际：%s", got)
	}
	// 普通消息不该被标记：两者都标就等于没标。
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "天气") && (strings.Contains(line, "提到") || strings.Contains(line, "回复")) {
			t.Errorf("普通消息不该带提及标记：%s", line)
		}
	}
	// 正文本身仍要原样保留（标记是补充，不是替换）。
	if !strings.Contains(got, "今天天气不错") {
		t.Errorf("正文应当原样保留，实际：%s", got)
	}
}

// TestAgentRunsWithoutAttention 验证未接 Attention 时不 panic（离线场景）。
func TestAgentRunsWithoutAttention(t *testing.T) {
	a, err := fsm.New(fsm.Options{
		Name: "no-attention", States: fsm.MVPStates(), Initial: "working",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("fsm.New: %v", err)
	}
	for i := 0; i < 10; i++ {
		a.Step(context.Background())
	}
	if a.Unread() != 0 {
		t.Error("未接 Attention 时未读应为 0")
	}
	if got := a.BrowsePhone(3); len(got) != 0 {
		t.Error("未接 Attention 时翻页应返回空")
	}
}
