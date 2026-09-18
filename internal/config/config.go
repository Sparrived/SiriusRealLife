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
	StartTick    fsm.Tick
	StaticDir    string
	AllowOps     bool
	LogLevel     slog.Level
	AMKR         llm.Config

	// MonologueEvery 是两次内心独白之间的最小 tick 间隔。
	// 这是**成本闸门**：独白会真的调用 LLM，0 = 每次进入状态都调用
	// （按 1 秒/tick 折算约每天两千多次），因此必须可调。
	MonologueEvery fsm.Tick

	// AllowNonLoopback 为真时允许绑定非回环地址（容器部署必需）。
	// 由 SIRIUS_ALLOW_NON_LOOPBACK 显式开启，默认关闭。
	AllowNonLoopback bool
	// NonLoopbackAcknowledged 表示本次启动**确实**绑在了非回环地址上，
	// 供 main 打一条显眼的告警。它不是开关，是启动时的自检结果。
	NonLoopbackAcknowledged bool

	// AuthUser / AuthPass 是 HTTP Basic 的凭据。两者都非空才启用鉴权。
	//
	// 存在的理由很具体：AGENTS.md §4 规定"Sirius 自身具备鉴权之前只能绑
	// 127.0.0.1"，因为 /amkr/ 反代等同于 AMKR 的完整管理权限（能读到上游
	// key）。要把它挂到公网（Cloudflare Tunnel），就必须先把这个前提补上。
	//
	// 刻意用 Basic 而不是自建登录：隧道已经在 TLS 之上，Basic 是标准库
	// 一行就能做的事，而自建会话/密码存储是又一份要维护的攻击面。
	// 只设了其中一个视为配置错误——半开的鉴权比没有更危险。
	AuthUser string
	AuthPass string
}

// AuthOn 报告是否启用了 HTTP Basic 鉴权。
func (o Options) AuthOn() bool { return o.AuthUser != "" && o.AuthPass != "" }

// FromEnv 读环境变量并做校验。
func FromEnv() (Options, error) {
	opt := Options{
		Addr:         envOr("SIRIUS_ADDR", "127.0.0.1:8080"),
		StaticDir:    envOr("SIRIUS_STATIC_DIR", "web/dist"),
		TickInterval: time.Second,
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
	if v := strings.TrimSpace(os.Getenv("SIRIUS_ALLOW_NON_LOOPBACK")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Options{}, fmt.Errorf("config: SIRIUS_ALLOW_NON_LOOPBACK 不是布尔值 %q: %w", v, err)
		}
		opt.AllowNonLoopback = b
	}
	opt.LogLevel = parseLevel(envOr("SIRIUS_LOG_LEVEL", "info"))

	// HTTP Basic 凭据。只设一半直接报错：半开的鉴权比没有更危险——
	// 部署方以为"我配了密码"，实际请求根本不校验。
	opt.AuthUser = strings.TrimSpace(os.Getenv("SIRIUS_AUTH_USER"))
	opt.AuthPass = os.Getenv("SIRIUS_AUTH_PASS")
	if (opt.AuthUser == "") != (opt.AuthPass == "") {
		return Options{}, fmt.Errorf(
			"config: SIRIUS_AUTH_USER 与 SIRIUS_AUTH_PASS 必须同时设置（或同时留空以关闭鉴权）")
	}
	if opt.AuthOn() && strings.ContainsAny(opt.AuthUser, ":") {
		// Basic 的用户名里出现冒号会让凭据解析歧义（RFC 7617 禁止）。
		return Options{}, fmt.Errorf("config: SIRIUS_AUTH_USER 不能含冒号")
	}

	// 独白间隔：默认 30 游戏分钟。这是成本闸门——独白会真的调用 LLM。
	//
	// 语义对齐 fsm：正值 = 最小间隔 tick 数；env 传 0 = **不节流**
	// （每次进入状态都调用，调试用，成本很高），在 fsm 侧用 -1 表示。
	// 负数直接拒绝，避免"想更疏"被误写成关掉独白。
	opt.MonologueEvery = 30
	if v := strings.TrimSpace(os.Getenv("SIRIUS_MONOLOGUE_EVERY")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Options{}, fmt.Errorf("config: SIRIUS_MONOLOGUE_EVERY 不是整数 %q: %w", v, err)
		}
		if n < 0 {
			return Options{}, fmt.Errorf("config: SIRIUS_MONOLOGUE_EVERY 不能为负，得到 %d", n)
		}
		if n == 0 {
			n = -1 // 不节流
		}
		opt.MonologueEvery = fsm.Tick(n)
	}

	amkr, err := llm.ConfigFromEnv()
	if err != nil {
		return Options{}, err
	}
	opt.AMKR = amkr

	// 安全线（AGENTS.md §4）：Sirius 自身具备鉴权之前，只能绑回环地址。
	// --allow-ops 会放行 AMKR 的宿主机运维接口，更不该对外。
	//
	// **鉴权开启后**这条线才算有条件解除：`SIRIUS_AUTH_USER/PASS` 会对整个
	// 服务（含 /amkr/）生效。但仍然要求显式声明 SIRIUS_ALLOW_NON_LOOPBACK
	// ——进程无法知道宿主侧的端口映射，是否真的只对可信来源开放只能靠
	// 部署方声明，默认保持 fail-closed。
	if !isLoopback(opt.Addr) {
		if !opt.AllowNonLoopback {
			hint := "Sirius 尚无自身鉴权，且 /amkr/ 等同于 AMKR 完整管理权限"
			if opt.AuthOn() {
				hint = "已配置 HTTP Basic 鉴权，但对外监听仍需显式确认"
			}
			return Options{}, fmt.Errorf(
				"config: SIRIUS_ADDR=%q 不是回环地址。%s，禁止对外监听（AGENTS.md §4）。"+
					"容器部署（必须绑 0.0.0.0）请设 SIRIUS_ALLOW_NON_LOOPBACK=1，"+
					"并确保入口处有鉴权与 TLS（本机回环映射，或隧道 + SIRIUS_AUTH_USER/PASS）",
				opt.Addr, hint)
		}
		opt.NonLoopbackAcknowledged = true
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
