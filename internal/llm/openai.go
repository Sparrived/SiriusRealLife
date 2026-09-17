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
)

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
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Complete 发一次非流式对话请求。
//
// **不重试**（R10）：重试、切 Key、冷却全部由 AMKR 负责，
// 客户端重试等于双倍计费 + 日志噪音。
func (c *Client) Complete(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:    c.cfg.Model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
		Stream:   false,
	})
	if err != nil {
		return "", fmt.Errorf("llm: 序列化请求: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: 调用 AMKR: %w", err)
	}
	defer resp.Body.Close()

	// 读有界：错误响应不该把内存吃光。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("llm: 读取响应: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{Code: resp.StatusCode, Body: truncate(string(raw), 500)}
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: 解析响应: %w（原文 %s）", err, truncate(string(raw), 200))
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: 响应没有 choices（model=%s）", out.Model)
	}
	return out.Choices[0].Message.Content, nil
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
