// Package llm 是 LLM 访问的唯一出口：AMKR 的 OpenAI 兼容接口（R9）。
//
// 手写 net/http 客户端，不引任何供应商 SDK —— AMKR 已把多供应商、
// 多 Key、协议差异吸收掉了，再加一层适配纯属负债。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// responseFormatOf 把 fsm 的 schema 请求翻成 OpenAI 参数。
//
// 为 nil 时返回 nil（omitempty 会让字段整个消失）：不想结构化输出的
// 调用点不该在请求里带上一个空壳，AMKR 有些任务对多余字段敏感。
func responseFormatOf(s *fsm.ResponseSchema) *responseFormat {
	if s == nil || len(s.Schema) == 0 {
		return nil
	}
	return &responseFormat{
		Type: "json_schema",
		JSONSchema: &jsonSchemaSpec{
			Name:   s.Name,
			Strict: true,
			Schema: s.Schema,
		},
	}
}

// Config 是客户端连接信息。只从环境变量读，key 不进代码、不进日志。
type Config struct {
	BaseURL string // 如 http://127.0.0.1:8000
	APIKey  string
	Model   string // 任务名 TASK_XXXXXX，或 AMKR 的逻辑名
	// Timeout 是单次调用的上限，必须**大于** AMKR 自己的预算
	// （request_timeout 60s），否则会把 AMKR 正在重试的请求提前掐死。
	Timeout time.Duration
	// HTTPClient 可注入测试用的 client；省略则按 Timeout 新建。
	HTTPClient *http.Client
}

// Client 是 AMKR 的 OpenAI 兼容客户端。
type Client struct {
	cfg  Config
	http *http.Client
}

// New 构造客户端。缺 BaseURL 或 Model 直接报错：宁可在启动时失败，
// 也不要等到第一次调用才发现配置不对。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("llm: 缺少 AMKR_BASE_URL")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm: 缺少模型/任务名")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.Timeout <= 60*time.Second {
		return nil, fmt.Errorf("llm: Timeout=%s 不大于 AMKR 的 request_timeout(60s)，会把在途重试掐死", cfg.Timeout)
	}
	h := cfg.HTTPClient
	if h == nil {
		h = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{cfg: cfg, http: h}, nil
}

// chatRequest 是 OpenAI Chat Completions 的请求体。
//
// 刻意**不包含** temperature/top_p/top_k/frequency_penalty/presence_penalty/
// seed/stop：任务里已固定的参数若被显式传入，AMKR 会直接返回 400。
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// ResponseFormat 仅在调用方要求结构化输出时出现（omitempty）。
	//
	// 谁能忽略它：AMKR 的路由。实测 wb2api 会**静默忽略**并照旧返回
	// 散文（HTTP 200，不报错），所以调用方必须能接受非 JSON 回复——
	// 我们请求它，但不依赖它（R9：路由由 AMKR 侧决定，可随时切）。
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	// Tools 仅在本次调用带工具时出现。
	Tools []wireTool `json:"tools,omitempty"`
}

// wireTool 是 OpenAI 的工具声明形状：多一层 "type":"function" 包装。
type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// responseFormat 是 OpenAI 的结构化输出参数。
//
// 用 json_schema+strict 而不是 json_object：实测 json_object 只保证
// "是 JSON"，字段名仍由模型自起（同一 prompt 下 glm 回 `answer`、
// deepseek 回 `内心活动`）；只有 strict 才真正约束字段名与必填项。
type responseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *jsonSchemaSpec `json:"json_schema,omitempty"`
}

type jsonSchemaSpec struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// chatMessage 是 OpenAI 的请求消息形状。
//
// 只有 role/content：决策是单轮调用（见 toWireMessages），不需要
// assistant/tool 的历史回放，因此不声明 tool_calls/tool_call_id。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// wireToolCall 是 OpenAI 的工具调用形状。
//
// Arguments 是**字符串**而不是对象——这是协议里最容易写错的一处：
// 它是一段 JSON 文本，需要二次解析。用 string 接收再转 RawMessage，
// 不能直接声明成 RawMessage（否则 json 包会因为类型不符而报错）。
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// toWireMessages 把一句话 prompt 包成线上消息。
//
// 只发一条 user：决策是**单轮**的——模型要么调 stay 要么调
// enter_state，工具的效果在框架内部生效，没有需要回传的工具结果。
// 等真有需要看结果的工具（如 read_app）时再加多轮。
func toWireMessages(prompt string) []chatMessage {
	return []chatMessage{{Role: "user", Content: prompt}}
}

// toWireTools 把工具声明翻成线上形状。
func toWireTools(specs []fsm.ToolSpec) []wireTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, wireTool{
			Type: "function",
			Function: wireFunction{
				Name: s.Name, Description: s.Description, Parameters: s.Parameters,
			},
		})
	}
	return out
}

// Complete 发一次非流式对话请求。
//
// **不重试**（R10）：重试、切 Key、冷却全部由 AMKR 负责，
// 客户端重试等于双倍计费 + 日志噪音。
//
// 参数名用 chat 而不是 req：下面要构造 *http.Request，同名会遮蔽。
func (c *Client) Complete(ctx context.Context, chat fsm.ChatRequest) (fsm.ChatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:          c.cfg.Model,
		Messages:       toWireMessages(chat.Prompt),
		Stream:         false,
		ResponseFormat: responseFormatOf(chat.Schema),
		Tools:          toWireTools(chat.Tools),
	})
	if err != nil {
		return fsm.ChatResponse{}, fmt.Errorf("llm: 序列化请求: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fsm.ChatResponse{}, fmt.Errorf("llm: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fsm.ChatResponse{}, fmt.Errorf("llm: 调用 AMKR: %w", err)
	}
	defer resp.Body.Close()

	// 读有界：错误响应不该把内存吃光。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fsm.ChatResponse{}, fmt.Errorf("llm: 读取响应: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fsm.ChatResponse{}, &StatusError{Code: resp.StatusCode, Body: truncate(string(raw), 500)}
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fsm.ChatResponse{}, fmt.Errorf("llm: 解析响应: %w（原文 %s）", err, truncate(string(raw), 200))
	}
	if len(out.Choices) == 0 {
		return fsm.ChatResponse{}, fmt.Errorf("llm: 响应没有 choices（model=%s）", out.Model)
	}
	msg := out.Choices[0].Message
	return fsm.ChatResponse{
		Text:      msg.Content,
		ToolCalls: decodeToolCalls(msg.ToolCalls),
	}, nil
}

// decodeToolCalls 把线上工具调用翻成 fsm 形状。
//
// 空 arguments 补成 "{}"：上游给空串是常见行为（无参工具），
// 而下游工具拿到空 RawMessage 会解析失败。在这里补一次，
// 好过让每个工具各自防一遍。
func decodeToolCalls(in []wireToolCall) []fsm.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]fsm.ToolCall, 0, len(in))
	for _, c := range in {
		args := c.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, fsm.ToolCall{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: json.RawMessage(args),
		})
	}
	return out
}

// StatusError 是 AMKR 返回非 200 时的错误。
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm: AMKR 返回 %d: %s", e.Code, e.Body)
}

// Unavailable 报告是否为"没有可用 Key"（503）。
//
// 调用方应当据此进入降级状态并记进意识流，**不要**立刻重试（R10）。
func (e *StatusError) Unavailable() bool { return e.Code == http.StatusServiceUnavailable }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
