package llm

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// CallSite 是调用点的稳定标识。
//
// 定义在 fsm 侧并在此别名：上下文装配（fsm.Context）要按调用点决定
// 往 prompt 里放什么，所以调用点的归属是状态机；llm 只是使用者。
// 各写一份枚举会让两边悄悄漂移——加调用点时忘改一边，装配出的
// prompt 就会落进 default 分支。
//
// 分开的理由：换模型、调温度不需要改 Sirius 代码，只要在 AMKR WebUI 里
// 改对应任务的配置。真实模型名**不出现在代码里**（R9）。
type CallSite = fsm.CallSite

const (
	// SiteDispatch 状态决策：决定下一步做什么。
	SiteDispatch = fsm.SiteDispatch
	// SiteMonologue 内心独白：写意识流。
	SiteMonologue = fsm.SiteMonologue
	// SiteToolRead 工具结果解读：把工具返回翻译成人话。
	SiteToolRead = fsm.SiteToolRead
)

// ConfigFromEnv 从环境变量读连接信息。
//
// AMKR_MODEL 是所有调用点的默认值（AMKR 尚未配 TASK_* 时用 unified-model）；
// 每个调用点可用 AMKR_TASK_<SITE> 单独覆盖。等 AMKR 侧建好任务后再逐个替换，
// 不必改代码。
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL: envOr("AMKR_BASE_URL", "http://127.0.0.1:8000"),
		APIKey:  os.Getenv("AMKR_API_KEY"),
		Model:   envOr("AMKR_MODEL", "unified-model"),
	}
	if v := strings.TrimSpace(os.Getenv("AMKR_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("llm: AMKR_TIMEOUT 不是合法时长 %q: %w", v, err)
		}
		cfg.Timeout = d
	}
	return cfg, nil
}

// ModelFor 返回某个调用点应当使用的 model 名。
//
// 优先取该调用点的 env 覆盖，否则回落到默认模型。
func ModelFor(site CallSite) string {
	if name := site.TaskEnv(); name != "" {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return envOr("AMKR_MODEL", "unified-model")
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func resolveTimeout(cfg *Config) (int, error) { return 0, nil }

// KeyFingerprint 返回 key 的短指纹，用于日志标识。
//
// **绝不打明文 key**（docs/llm-amkr.md §1），也不回显它的任何片段：
// 只用 SHA-256 前 4 字节。指纹足够回答"是不是同一把 key"，
// 且无法从中还原原文。
func KeyFingerprint(key string) string {
	if key == "" {
		return "(none)"
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("fp=%x", sum[:4])
}
