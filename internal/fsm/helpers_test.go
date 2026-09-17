package fsm

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

// recordingSink 记录收到的消息，用于验证记忆链路入口。
type recordingSink struct{ got []IncomingMessage }

func (r *recordingSink) Accept(m IncomingMessage) { r.got = append(r.got, m) }

// fakeAttention 是一个确定性的 Attention：固定队列 + 游标。
//
// 用它而不是 memory.Store：fsm 不能 import memory（会成环），
// 而这里要测的是"泵入受订阅门控"，不是存储实现。
type fakeAttention struct {
	msgs   []string
	cursor int
}

func (f *fakeAttention) Scan(n int) []string {
	if n <= 0 || f.cursor >= len(f.msgs) {
		return nil
	}
	end := f.cursor + n
	if end > len(f.msgs) {
		end = len(f.msgs)
	}
	out := f.msgs[f.cursor:end]
	f.cursor = end
	return out
}

func (f *fakeAttention) Browse(n int) []string { return nil }

func (f *fakeAttention) Unread() int { return len(f.msgs) - f.cursor }

// newAgentWithFakeAttention 造一个接了 fakeAttention 的 agent，
// 队列里预置两条消息。返回的清理函数用于消掉后台独白 goroutine。
func newAgentWithFakeAttention(t *testing.T) (*Agent, func()) {
	t.Helper()
	a := newTestAgentWith(t, Options{
		Name:      "pump",
		States:    MVPStates(),
		Initial:   "idle",
		Attention: &fakeAttention{msgs: []string{"张三: 在吗", "李四: 走了"}},
	})
	return a, func() { a.cancelThinking() }
}

// newTestAgentWith 用给定 Options 构造 agent，自动补上静音 logger。
func newTestAgentWith(t *testing.T, opt Options) *Agent {
	t.Helper()
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a, err := New(opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// mustJSON 把值编码成 JSON，仅用于测试。
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
