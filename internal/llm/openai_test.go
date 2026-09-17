package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// fsmRequest 是构造 fsm 调用请求的小助手。
func fsmRequest(prompt string) fsm.ChatRequest {
	return fsm.ChatRequest{Prompt: prompt}
}

// newTestClient 起一个假的 AMKR，返回客户端与"最后一次收到的请求"。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Config{
		BaseURL: srv.URL,
		APIKey:  "test-local-key",
		Model:   "TASK_000001",
		Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func okResponse(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "some-upstream",
			"choices": []map[string]any{
				{"message": map[string]string{"content": content}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"total_tokens": 42},
		})
	}
}

// TestCompleteSendsForbiddenFreeParams 锁住关键契约：请求体里**不能**出现
// 任务已固定的采样参数，否则 AMKR 直接返回 400（docs/llm-amkr.md §2）。
func TestCompleteSendsForbiddenFreeParams(t *testing.T) {
	var got map[string]any
	done := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		close(done) // 交给测试 goroutine 读取，避免跨 goroutine 竞争
		okResponse("收到")(w, r)
	})

	if _, err := c.Complete(context.Background(), "你好"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	<-done

	forbidden := []string{
		"temperature", "top_p", "top_k",
		"frequency_penalty", "presence_penalty", "seed", "stop",
	}
	for _, k := range forbidden {
		if _, present := got[k]; present {
			t.Errorf("请求体里出现了 %q —— AMKR 会因此返回 400", k)
		}
	}
	// 必需字段要在。
	if got["model"] != "TASK_000001" {
		t.Errorf("model = %v, 期望 TASK_000001", got["model"])
	}
	if got["stream"] != false {
		t.Errorf("stream = %v, 期望 false", got["stream"])
	}
}

// TestCompleteParsesContent 验证正常响应能取出文本。
func TestCompleteParsesContent(t *testing.T) {
	c, _ := newTestClient(t, okResponse("我在刷手机"))
	text, err := c.Complete(context.Background(), "你在做什么")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "我在刷手机" {
		t.Fatalf("text = %q", text)
	}
}

// TestCompleteSendsAuthHeader 验证鉴权头正确。
func TestCompleteSendsAuthHeader(t *testing.T) {
	var auth string
	done := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		close(done)
		okResponse("ok")(w, r)
	})
	if _, err := c.Complete(context.Background(), "x"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	<-done
	if auth != "Bearer test-local-key" {
		t.Fatalf("Authorization = %q", auth)
	}
}

// TestUnavailableIsDegradable 验证 503 被识别为可降级错误（R10：不重试）。
func TestUnavailableIsDegradable(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no usable key"}`))
	})

	_, err := c.Complete(context.Background(), "x")
	if err == nil {
		t.Fatal("503 应当返回错误")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("错误类型 = %T, 期望 *StatusError", err)
	}
	if !se.Unavailable() {
		t.Error("503 应被识别为可降级")
	}
	if calls != 1 {
		t.Fatalf("调用次数 = %d, 期望 1（R10：绝不重试）", calls)
	}
}

// TestServerErrorNotRetried 验证 502 也不重试（R10）。
func TestServerErrorNotRetried(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, err := c.Complete(context.Background(), "x"); err == nil {
		t.Fatal("502 应当返回错误")
	}
	if calls != 1 {
		t.Fatalf("调用次数 = %d, 期望 1（R10：绝不重试）", calls)
	}
}

// TestContextCancelPropagates 验证 R4：调用可被 context 取消。
func TestContextCancelPropagates(t *testing.T) {
	release := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // 卡住直到测试放行
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Complete(ctx, "x")
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("取消未及时生效，耗时 %v", elapsed)
	}
}

// TestConfigRejectsShortTimeout 验证启动期就拦住"比 AMKR 还短"的超时。
func TestConfigRejectsShortTimeout(t *testing.T) {
	_, err := New(Config{BaseURL: "http://x", Model: "m", Timeout: 30 * time.Second})
	if err == nil {
		t.Fatal("30s 超时应当被拒（小于 AMKR 的 60s request_timeout）")
	}
}

// TestConfigRejectsMissing 验证缺关键配置时启动即失败。
func TestConfigRejectsMissing(t *testing.T) {
	if _, err := New(Config{Model: "m", Timeout: time.Minute * 2}); err == nil {
		t.Error("缺 BaseURL 应当报错")
	}
	if _, err := New(Config{BaseURL: "http://x", Timeout: time.Minute * 2}); err == nil {
		t.Error("缺 Model 应当报错")
	}
}

// TestKeyFingerprintHidesKey 验证日志指纹不泄露 key 的任何片段。
func TestKeyFingerprintHidesKey(t *testing.T) {
	const secret = "sk-super-secret-value-12345"
	fp := KeyFingerprint(secret)
	for _, frag := range []string{"super-secret", "sk-", "12345"} {
		if strings.Contains(fp, frag) {
			t.Fatalf("指纹泄露了 key 片段 %q: %q", frag, fp)
		}
	}
	// 同一把 key 指纹稳定，不同 key 指纹不同（这是指纹的唯一用途）。
	if KeyFingerprint(secret) != fp {
		t.Error("同一把 key 的指纹应当稳定")
	}
	if KeyFingerprint(secret+"x") == fp {
		t.Error("不同 key 的指纹应当不同")
	}
	if KeyFingerprint("") != "(none)" {
		t.Error("空 key 应返回 (none)")
	}
}

// TestModelForFallback 验证调用点未单独配置时回落到默认模型。
func TestModelForFallback(t *testing.T) {
	t.Setenv("AMKR_MODEL", "unified-model")
	t.Setenv("AMKR_TASK_DISPATCH", "")
	if got := ModelFor(SiteDispatch); got != "unified-model" {
		t.Errorf("未覆盖时应回落默认模型，得到 %q", got)
	}
	t.Setenv("AMKR_TASK_DISPATCH", "TASK_000042")
	if got := ModelFor(SiteDispatch); got != "TASK_000042" {
		t.Errorf("有覆盖时应取覆盖值，得到 %q", got)
	}
	// 其他调用点不受影响。
	if got := ModelFor(SiteMonologue); got != "unified-model" {
		t.Errorf("其他调用点不应被影响，得到 %q", got)
	}
}

// TestChatterAdaptsToFSM 验证适配器接到 fsm 的缝口上，且 503 触发降级回调。
func TestChatterAdaptsToFSM(t *testing.T) {
	c, _ := newTestClient(t, okResponse("适配成功"))
	ch := NewChatter(c, SiteMonologue)

	resp, err := ch.Chat(context.Background(), fsmRequest("hi"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "适配成功" {
		t.Fatalf("Text = %q", resp.Text)
	}

	// 503 应当触发 OnDegrade，且只触发一次调用。
	degraded := 0
	c2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ch2 := NewChatter(c2, SiteDispatch)
	ch2.OnDegrade = func(error) { degraded++ }
	if _, err := ch2.Chat(context.Background(), fsmRequest("hi")); err == nil {
		t.Fatal("503 应当返回错误")
	}
	if degraded != 1 {
		t.Fatalf("降级回调次数 = %d, 期望 1", degraded)
	}
}
