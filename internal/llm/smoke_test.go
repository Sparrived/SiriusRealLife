package llm

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSmokeAgainstRealAMKR 打真实 AMKR，验证契约没写错。
//
// 默认跳过：CI 与离线环境不该依赖本机服务，也不该烧 token。
// 显式开启：
//
//	$env:AMKR_SMOKE=1; $env:AMKR_API_KEY="..."; go test ./internal/llm/ -run Smoke -v
//
// key 从环境变量读，不进代码、不进日志。
func TestSmokeAgainstRealAMKR(t *testing.T) {
	if os.Getenv("AMKR_SMOKE") == "" {
		t.Skip("跳过真实 AMKR 冒烟测试（设 AMKR_SMOKE=1 开启）")
	}

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.APIKey == "" {
		t.Fatal("需要 AMKR_API_KEY")
	}
	t.Logf("AMKR=%s model=%s key=%s", cfg.BaseURL, cfg.Model, KeyFingerprint(cfg.APIKey))

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	text, err := c.Complete(ctx, "只回复两个字：收到")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	// 延迟是设计输入：thinking 状态的 maxTick 要按它估（实测 1.8–3.4s）。
	t.Logf("延迟 %v，回复 %q", elapsed, text)
	if text == "" {
		t.Fatal("回复为空")
	}
}
