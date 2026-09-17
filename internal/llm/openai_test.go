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

	if _, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "你好"}); err != nil {
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
	resp, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "你在做什么"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "我在刷手机" {
		t.Fatalf("text = %q", resp.Text)
	}
}

// TestToolsAreSentInOpenAIShape 锁住工具声明的线上形状。
//
// OpenAI 要求多一层 {"type":"function","function":{...}} 包装。
// 少这层不会编译报错，只会在上游得到一个 400——正是需要测试盯住的地方。
func TestToolsAreSentInOpenAIShape(t *testing.T) {
	var got map[string]any
	done := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		close(done)
		okResponse("收到")(w, r)
	})

	_, err := c.Complete(context.Background(), fsm.ChatRequest{
		Prompt: "现在做什么",
		Tools: []fsm.ToolSpec{{
			Name:        "stay",
			Description: "什么都不做",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	<-done

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 应有 1 项，实际 %d（原文 %v）", len(tools), got["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf(`tools[0].type = %v, 期望 "function"`, tool["type"])
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("tools[0].function 缺失或形状不对: %v", tool)
	}
	if fn["name"] != "stay" {
		t.Errorf("function.name = %v", fn["name"])
	}
	if _, present := fn["parameters"]; !present {
		t.Error("function.parameters 应存在")
	}
}

// TestNoToolsFieldWhenAbsent 验证不带工具时**不出现** tools 字段。
//
// 与 response_format 同理：AMKR 有些任务对多余参数敏感，
// 一个空的 tools 数组会把普通调用变成工具调用。
func TestNoToolsFieldWhenAbsent(t *testing.T) {
	var got map[string]any
	done := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		close(done)
		okResponse("收到")(w, r)
	})

	if _, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	<-done
	if _, present := got["tools"]; present {
		t.Errorf("未带工具时不该出现 tools 字段: %v", got["tools"])
	}
}

// TestParseToolCalls 验证工具调用能被解析出来。
//
// 两个易错点：arguments 是**字符串**需要二次解码；content 与 tool_calls
// 可以同时非空（模型边叙述边调工具），两者都不能丢。
func TestParseToolCalls(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "up",
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": "有点无聊，看看手机",
					"tool_calls": []map[string]any{{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "enter_state",
							"arguments": `{"state":"scrolling_phone","for_ticks":20}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	})

	resp, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// 叙述不能被丢掉：它要进意识流。
	if resp.Text != "有点无聊，看看手机" {
		t.Errorf("Text = %q", resp.Text)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("应有 1 次工具调用，实际 %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "enter_state" {
		t.Errorf("Name = %q", tc.Name)
	}
	var args struct {
		State    string `json:"state"`
		ForTicks int    `json:"for_ticks"`
	}
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("Arguments 不是合法 JSON（%s）: %v", tc.Arguments, err)
	}
	if args.State != "scrolling_phone" || args.ForTicks != 20 {
		t.Errorf("参数解析错: %+v", args)
	}
}

// TestEmptyToolArgumentsBecomeEmptyObject 验证空 arguments 补成 "{}"。
//
// 无参工具（stay 的某些形状）上游会回空串。若原样传下去，
// 每个工具都得自己防一次"空 JSON"——在这里补一次更省。
func TestEmptyToolArgumentsBecomeEmptyObject(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "up",
			"choices": []map[string]any{{
				"message": map[string]any{
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": "stay", "arguments": ""},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	})

	resp, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("应有 1 次调用")
	}
	got := string(resp.ToolCalls[0].Arguments)
	if got != "{}" {
		t.Errorf("空 arguments 应补成 {}，实际 %q", got)
	}
}

// TestResponseFormatOnlyWhenSchemaRequested 验证 response_format 的收发边界。
//
// 不请求 schema 时**不能**出现这个字段：AMKR 有些任务对多余参数敏感，
// 而空壳 schema 会让一条普通调用变成结构化调用。
func TestResponseFormatOnlyWhenSchemaRequested(t *testing.T) {
	cases := []struct {
		name  string
		req   fsm.ChatRequest
		want  bool
		check func(*testing.T, map[string]any)
	}{
		{
			name: "未请求 schema",
			req:  fsm.ChatRequest{Prompt: "x"},
			want: false,
		},
		{
			name: "请求 schema",
			req: fsm.ChatRequest{Prompt: "x", Schema: &fsm.ResponseSchema{
				Name:   "monologue",
				Schema: json.RawMessage(`{"type":"object","properties":{"thought":{"type":"string"}}}`),
			}},
			want: true,
			check: func(t *testing.T, got map[string]any) {
				rf, _ := got["response_format"].(map[string]any)
				if rf["type"] != "json_schema" {
					t.Errorf("type = %v, 期望 json_schema（只有 strict 才约束字段名）", rf["type"])
				}
				js, _ := rf["json_schema"].(map[string]any)
				if js["name"] != "monologue" {
					t.Errorf("name = %v", js["name"])
				}
				if js["strict"] != true {
					t.Errorf("strict = %v, 期望 true", js["strict"])
				}
				if _, ok := js["schema"].(map[string]any); !ok {
					t.Errorf("schema 未原样传出: %v", js["schema"])
				}
			},
		},
		{
			name: "schema 为空视为未请求",
			req:  fsm.ChatRequest{Prompt: "x", Schema: &fsm.ResponseSchema{Name: "empty"}},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			done := make(chan struct{})
			client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &got)
				close(done)
				okResponse("{}")(w, r)
			})
			if _, err := client.Complete(context.Background(), c.req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			<-done

			rf, present := got["response_format"]
			if present != c.want {
				t.Fatalf("response_format 出现 = %v, 期望 %v（值 %v）", present, c.want, rf)
			}
			if c.want && c.check != nil {
				c.check(t, got)
			}
		})
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
	if _, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "x"}); err != nil {
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

	_, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "x"})
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
	if _, err := c.Complete(context.Background(), fsm.ChatRequest{Prompt: "x"}); err == nil {
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
	_, err := c.Complete(ctx, fsm.ChatRequest{Prompt: "x"})
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
