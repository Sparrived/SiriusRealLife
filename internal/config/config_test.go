package config

import (
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// TestFromEnvDefaults 验证缺省值合理。
func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("SIRIUS_ADDR", "")
	t.Setenv("SIRIUS_TICK_INTERVAL", "")
	t.Setenv("SIRIUS_SEED", "")
	t.Setenv("SIRIUS_START_TICK", "")

	opt, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if opt.TickInterval != time.Second {
		t.Errorf("默认 tick 间隔 = %v", opt.TickInterval)
	}
	// 默认从白天开始：一上来是半夜会让人格表现怪异。
	if fsm.Hour(opt.StartTick) < 6 || fsm.Hour(opt.StartTick) > 22 {
		t.Errorf("默认起始时刻 %d 点不是白天", fsm.Hour(opt.StartTick))
	}
	if opt.Seed == 0 {
		t.Error("应当有默认种子")
	}
}

// TestRejectsNonLoopbackAddr 验证安全线（AGENTS.md §4）：
// 没有自身鉴权前禁止对外监听，且默认 fail-closed。
func TestRejectsNonLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.5:8080", "example.com:80"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("SIRIUS_ADDR", addr)
			t.Setenv("SIRIUS_ALLOW_NON_LOOPBACK", "")
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("监听 %q 应当被拒绝", addr)
			}
			if !strings.Contains(err.Error(), "回环") {
				t.Errorf("错误信息应说明原因，实际: %v", err)
			}
			// 错误信息里要给出路，否则容器部署会卡住。
			if !strings.Contains(err.Error(), "SIRIUS_ALLOW_NON_LOOPBACK") {
				t.Errorf("错误信息应提示容器部署的开关，实际: %v", err)
			}
		})
	}
}

// TestAllowNonLoopbackEscapeHatch 验证容器部署的显式开关。
//
// 容器里必须绑 0.0.0.0，否则 Docker 的端口映射转发不进容器。
// 进程无法知道宿主侧的映射是否只开了回环，所以这必须是**显式**声明，
// 而且开启后 main 会打告警（见 NonLoopbackAcknowledged）。
func TestAllowNonLoopbackEscapeHatch(t *testing.T) {
	t.Setenv("SIRIUS_ADDR", "0.0.0.0:8080")
	t.Setenv("SIRIUS_ALLOW_NON_LOOPBACK", "1")

	opt, err := FromEnv()
	if err != nil {
		t.Fatalf("显式开启后应当允许绑定非回环地址: %v", err)
	}
	if !opt.NonLoopbackAcknowledged {
		t.Error("NonLoopbackAcknowledged 应为真，以便 main 打告警")
	}

	// 回环地址不应被标记（没有需要告警的事）。
	t.Setenv("SIRIUS_ADDR", "127.0.0.1:8080")
	opt, err = FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if opt.NonLoopbackAcknowledged {
		t.Error("回环地址不应标记为非回环")
	}
}

// TestAcceptsLoopbackAddrs 验证各种回环写法被接受。
func TestAcceptsLoopbackAddrs(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("SIRIUS_ADDR", addr)
			if _, err := FromEnv(); err != nil {
				t.Fatalf("监听 %q 应当被接受: %v", addr, err)
			}
		})
	}
}

// TestRejectsBadValues 验证非法输入报错而不是静默用默认值。
func TestRejectsBadValues(t *testing.T) {
	cases := []struct{ key, val string }{
		{"SIRIUS_TICK_INTERVAL", "abc"},
		{"SIRIUS_TICK_INTERVAL", "0s"},
		{"SIRIUS_TICK_INTERVAL", "-1s"},
		{"SIRIUS_SEED", "not-a-number"},
		{"SIRIUS_START_TICK", "x"},
		{"SIRIUS_ALLOW_OPS", "maybe"},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.val, func(t *testing.T) {
			t.Setenv(c.key, c.val)
			if _, err := FromEnv(); err == nil {
				t.Errorf("%s=%q 应当报错", c.key, c.val)
			}
		})
	}
}

// TestLogLevelParsing 验证日志级别。
func TestLogLevelParsing(t *testing.T) {
	t.Setenv("SIRIUS_LOG_LEVEL", "debug")
	opt, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if opt.LogLevel.String() != "DEBUG" {
		t.Errorf("日志级别 = %s, 期望 DEBUG", opt.LogLevel)
	}
}
