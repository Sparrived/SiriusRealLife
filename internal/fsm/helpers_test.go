package fsm

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

// recordingSink 记录收到的消息，用于验证记忆链路入口。
type recordingSink struct{ got []IncomingMessage }

func (r *recordingSink) Accept(m IncomingMessage) bool {
	r.got = append(r.got, m)
	return m.Interrupts()
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
