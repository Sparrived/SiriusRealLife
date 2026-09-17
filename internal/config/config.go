// Package config 读取运行期配置。
//
// 只从环境变量读（配合 .env，见 .env.example）。配置项刻意保持少：
// 状态表与人格参数硬编码在 Go 里（conventions §1）。
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
	"github.com/Sparrived/SiriusRealLife/internal/llm"
)

// Options 是 Sirius 的进程级配置。
type Options struct {
	Addr         string
	TickInterval time.Duration
	Seed         int64
	StartTick    fsm.Tick
	StaticDir    string
	AllowOps     bool
	LogLevel     slog.Level
	AMKR         llm.Config
}

// FromEnv 读环境变量并做校验。
func FromEnv() (Options, error) {
	opt := Options{
		Addr:         envOr("SIRIUS_ADDR", "127.0.0.1:8080"),
		StaticDir:    envOr("SIRIUS_STATIC_DIR", "web/dist"),
		TickInterval: time.Second,
		Seed:         20240101,
		StartTick:    7 * 60, // 默认从游戏内 07:00 开始，别一上来就是半夜
	}

	if v := strings.TrimSpace(os.Getenv("SIRIUS_TICK_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Options{}, fmt.Errorf("config: SIRIUS_TICK_INTERVAL 不是合法时长 %q: %w", v, err)
		}
		if d <= 0 {
			return Options{}, fmt.Errorf("config: SIRIUS_TICK_INTERVAL 必须为正，得到 %s", d)
		}
		opt.TickInterval = d
	}
	if v := strings.TrimSpace(os.Getenv("SIRIUS_SEED")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Options{}, fmt.Errorf("config: SIRIUS_SEED 不是整数 %q: %w", v, err)
		}
		opt.Seed = n
	}
	if v := strings.TrimSpace(os.Getenv("SIRIUS_START_TICK")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Options{}, fmt.Errorf("config: SIRIUS_START_TICK 不是整数 %q: %w", v, err)
		}
		opt.StartTick = fsm.Tick(n)
	}
	if v := strings.TrimSpace(os.Getenv("SIRIUS_ALLOW_OPS")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Options{}, fmt.Errorf("config: SIRIUS_ALLOW_OPS 不是布尔值 %q: %w", v, err)
		}
		opt.AllowOps = b
	}
	opt.LogLevel = parseLevel(envOr("SIRIUS_LOG_LEVEL", "info"))

	amkr, err := llm.ConfigFromEnv()
	if err != nil {
		return Options{}, err
	}
	opt.AMKR = amkr

	// 安全线（AGENTS.md §4）：Sirius 自身具备鉴权之前，只能绑回环地址。
	// --allow-ops 会放行 AMKR 的宿主机运维接口，更不该对外。
	if !isLoopback(opt.Addr) {
		return Options{}, fmt.Errorf(
			"config: SIRIUS_ADDR=%q 不是回环地址。Sirius 尚无自身鉴权，且 /amkr/ 等同于 AMKR 完整管理权限，禁止对外监听（AGENTS.md §4）",
			opt.Addr)
	}
	return opt, nil
}

// isLoopback 判断监听地址是否只绑本机。
func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
