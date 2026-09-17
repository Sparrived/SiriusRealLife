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
