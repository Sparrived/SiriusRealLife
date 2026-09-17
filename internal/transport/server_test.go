package transport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

func newTestServer(t *testing.T) (*Server, *Broadcaster) {
	t.Helper()
	b := NewBroadcaster()
	a, err := fsm.New(fsm.Options{
		Name: "t", States: fsm.MVPStates(), Seed: 1, Initial: "idle",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("fsm.New: %v", err)
	}
	return NewServer(Options{
		Agent:       a,
		Broadcaster: b,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AgentID:     "default",
	}), b
}

// TestStateEndpoint 验证 GET 状态快照。
func TestStateEndpoint(t *testing.T) {
	s, b := newTestServer(t)
	b.Publish(fsm.Snapshot{
		Now: 42, Current: "working",
		Mood:   fsm.Mood{Energy: 55, Annoyed: 3, Curious: 60},
		Stream: []fsm.StreamEntry{{Seq: 42, Kind: fsm.KindIntent, State: "working", Text: "干活"}},
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents/default", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var got stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if got.Tick != 42 || got.State != "working" {
		t.Errorf("tick=%d state=%s", got.Tick, got.State)
	}
	// 字段必须是 snake_case（conventions §3）。
	if !strings.Contains(rec.Body.String(), `"agent_id"`) {
		t.Error("响应缺少 snake_case 的 agent_id")
	}
	if !strings.Contains(rec.Body.String(), `"call_count"`) {
		t.Error("响应缺少 snake_case 的 call_count")
	}
	// 意识流必须带 kind：前端据它区分"在想/在做/打算做"。
	if !strings.Contains(rec.Body.String(), `"kind":"intent"`) {
		t.Errorf("响应缺少意识流的 kind 字段: %s", rec.Body.String())
	}
}

// TestStreamKindReachesClient 验证 Kind 真的穿过 API 到达客户端。
//
// 后端加了 Kind 而传输层忘了带上，是最容易漏的一环：Go 侧测试
// 全绿，界面却仍然只有一团文本。
func TestStreamKindReachesClient(t *testing.T) {
	s, b := newTestServer(t)
	kinds := []fsm.Kind{fsm.KindObservation, fsm.KindThought, fsm.KindAction, fsm.KindIntent}
	want := []string{"observation", "thought", "action", "intent"}

	var stream []fsm.StreamEntry
	for i, k := range kinds {
		stream = append(stream, fsm.StreamEntry{Seq: fsm.Tick(i), Kind: k, State: "idle", Text: "x"})
	}
	b.Publish(fsm.Snapshot{Now: 4, Current: "idle", Stream: stream})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents/default", nil))

	var got stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got.Stream) != len(want) {
		t.Fatalf("流条数 = %d, 期望 %d", len(got.Stream), len(want))
	}
	for i, w := range want {
		if got.Stream[i].Kind != w {
			t.Errorf("第 %d 条 kind = %q, 期望 %q", i, got.Stream[i].Kind, w)
		}
	}
}

// TestStateEndpointBeforeFirstTick 验证还没跑过 tick 时返回空快照而非 404。
func TestStateEndpointBeforeFirstTick(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents/default", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("未跑 tick 时应返回 200，实际 %d", rec.Code)
	}
}

// TestEventEndpointQueues 验证外部只能投事件（R1），不能直接改状态。
func TestEventEndpointQueues(t *testing.T) {
	s, _ := newTestServer(t)

	body := `{"kind":"user_message","text":"@我 在吗","mentions_me":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/default/events", strings.NewReader(body))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d, 期望 202；body=%s", rec.Code, rec.Body.String())
	}
	// @ 不另立事件类型：它只是负载里的标记，不参与控制流（§2.1）。
	if !strings.Contains(rec.Body.String(), string(fsm.EventUserMessage)) {
		t.Errorf("应投递 user_message 事件，实际 %s", rec.Body.String())
	}

	// 事件确实进了 agent，但**不该**改变状态：@ 不抢占（§2.1）。
	// （Events() 是 send-only，正是 R1 的体现——测试也只能通过效果观察。）
	before := s.opt.Agent.Current
	s.opt.Agent.Step(context.Background())
	if got := s.opt.Agent.Current; got == "scrolling_phone" && before != "scrolling_phone" {
		t.Errorf("@ 不该把 agent 抢到 scrolling_phone，实际 %s", got)
	}
}

// TestEventEndpointRejectsBadInput 验证输入校验。
func TestEventEndpointRejectsBadInput(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		name string
		body string
		code int
	}{
		{"非法 JSON", `{`, http.StatusBadRequest},
		{"空文本", `{"text":""}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/default/events", strings.NewReader(c.body))
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != c.code {
				t.Errorf("状态码 = %d, 期望 %d", rec.Code, c.code)
			}
			// 错误形状按 conventions §3。
			if !strings.Contains(rec.Body.String(), `"error"`) {
				t.Errorf("错误响应缺少 error 字段: %s", rec.Body.String())
			}
		})
	}
}

// TestSSEStreamDeliversSnapshot 验证 SSE 推送（conventions §3：SSE 而非 WebSocket）。
func TestSSEStreamDeliversSnapshot(t *testing.T) {
	s, b := newTestServer(t)
	b.Publish(fsm.Snapshot{Now: 7, Current: "idle"})

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/agents/default/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	// 连接建立后应立即补一条当前状态。
	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("读取流: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "event: state") {
		t.Errorf("首个事件应为 state，实际 %q", got)
	}
	if !strings.Contains(got, `"tick":7`) {
		t.Errorf("应带上当前快照，实际 %q", got)
	}
}

// TestBroadcasterDropsSlowSubscriber 验证慢订阅者被丢帧而不是拖住 agent。
func TestBroadcasterDropsSlowSubscriber(t *testing.T) {
	b := NewBroadcaster()
	ch, cancel := b.subscribe()
	defer cancel()

	// 灌入远超缓冲容量的快照：不应阻塞。
	for i := 0; i < 100; i++ {
		b.Publish(fsm.Snapshot{Now: fsm.Tick(i)})
	}
	if b.Dropped() == 0 {
		t.Error("应当记录被丢弃的推送（慢订阅者）")
	}
	// 订阅者仍能拿到**最新**的快照，而不是卡在旧数据。
	last := fsm.Snapshot{}
	for len(ch) > 0 {
		last = <-ch
	}
	if last.Now == 0 {
		t.Error("订阅者应至少拿到部分快照")
	}
}

// TestUnsubscribeClosesChannel 验证退订后 channel 关闭，SSE 循环能退出。
func TestUnsubscribeClosesChannel(t *testing.T) {
	b := NewBroadcaster()
	ch, cancel := b.subscribe()
	if b.Subscribers() != 1 {
		t.Fatalf("订阅者数 = %d, 期望 1", b.Subscribers())
	}
	cancel()
	if b.Subscribers() != 0 {
		t.Fatalf("退订后订阅者数 = %d, 期望 0", b.Subscribers())
	}
	if _, open := <-ch; open {
		t.Error("退订后 channel 应已关闭")
	}
}

// TestHealthEndpoint 验证健康检查反映 AMKR 可用性。
func TestHealthEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	s.opt.Ready = func() bool { return false }

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ready":false`) {
		t.Errorf("应反映 AMKR 不可用: %s", rec.Body.String())
	}
}
