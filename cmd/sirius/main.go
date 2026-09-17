// Command sirius 是人格模拟器的入口。
//
// 只做装配：读配置、建 agent、起 HTTP、起 tick 源。业务逻辑全在 internal/。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sparrived/SiriusRealLife/internal/config"
	"github.com/Sparrived/SiriusRealLife/internal/fsm"
	"github.com/Sparrived/SiriusRealLife/internal/llm"
	"github.com/Sparrived/SiriusRealLife/internal/memory"
	"github.com/Sparrived/SiriusRealLife/internal/tick"
	"github.com/Sparrived/SiriusRealLife/internal/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	opt, err := config.FromEnv()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: opt.LogLevel}))
	slog.SetDefault(logger)

	// 绑在非回环地址上时打一条显眼告警：这条安全线只能靠部署方守
	// （进程无法知道宿主侧的端口映射是否只开了回环）。
	if opt.NonLoopbackAcknowledged {
		logger.Warn("已绑定非回环地址",
			slog.String("addr", opt.Addr),
			slog.String("风险", "Sirius 尚无自身鉴权，/amkr/ 等同于 AMKR 完整管理权限"),
			slog.String("要求", "必须确保端口只映射到宿主回环，例如 127.0.0.1:8080:8080"),
		)
	}

	// AMKR 客户端。连不上不让启动失败：先跑起来，LLM 调用失败会
	// 以事件回到 agent 并记进意识流（R4/R10）。
	amkrClient, err := llm.New(opt.AMKR)
	if err != nil {
		return err
	}
	logger.Info("AMKR 配置",
		slog.String("base_url", opt.AMKR.BaseURL),
		slog.String("model", opt.AMKR.Model),
		slog.String("key", llm.KeyFingerprint(opt.AMKR.APIKey)),
	)

	store := memory.New(memory.DefaultOptions())
	broadcaster := transport.NewBroadcaster()
	chatter := llm.NewChatter(amkrClient, llm.SiteMonologue)

	agent, err := fsm.New(fsm.Options{
		Name:      "sirius",
		States:    fsm.MVPStates(),
		StartTick: opt.StartTick,
		Chatter:   chatter,
		Logger:    logger,
		Attention: store,
		Ticker:    store,               // 让 agent 的 tick 驱动记忆的衰减/升格（R8）
		Observe:   broadcaster.Publish, // 值快照，不共享 agent 内部状态（R1）
		// 外部消息的落点：没有它，unread 队列与待选区永远是空的
		// （记忆链路整条失效）。
		Sink: store,
		// 意识流由 LLM 生成，而不是写死的旁白。异步、可取消（R4）。
		// 间隔可调：它是唯一的 LLM 成本闸门（SIRIUS_MONOLOGUE_EVERY）。
		Monologue:      true,
		MonologueEvery: opt.MonologueEvery,
		// 打捞把长期记忆接回 prompt（memory.md §5.1）。
		Dredge: store.DredgeFor(),
	})
	if err != nil {
		return err
	}
	logger.Info("agent 就绪",
		slog.String("state", string(agent.Current)),
		slog.Int64("tick", int64(agent.Now)),
		slog.Int("hour", fsm.Hour(agent.Now)),
	)

	// /amkr/ 反代：把 AMKR WebUI 挂到 Sirius 页面上，省掉自建管理后台。
	proxy, err := transport.NewAMKRProxy(transport.AMKRProxyOptions{
		Target:   opt.AMKR.BaseURL,
		APIKey:   opt.AMKR.APIKey,
		AllowOps: opt.AllowOps,
	})
	if err != nil {
		return err
	}

	server := transport.NewServer(transport.Options{
		Agent:       agent,
		Broadcaster: broadcaster,
		Logger:      logger,
		AgentID:     "sirius",
		StaticDir:   opt.StaticDir,
		Proxy:       proxy,
		Ready:       func() bool { return amkrReachable(opt.AMKR.BaseURL) },
	})

	srv := &http.Server{
		Addr:              opt.Addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clock := make(chan struct{})
	go tick.Source{Interval: opt.TickInterval}.Run(ctx, clock)

	// agent 循环：一个 agent 一个 goroutine（R1）。
	agentErr := make(chan error, 1)
	go func() { agentErr <- agent.Run(ctx, clock) }()

	go func() {
		logger.Info("HTTP 监听", slog.String("addr", opt.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务退出", slog.String("err", err.Error()))
			stop()
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("收到停止信号，开始关停")
	case err := <-agentErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("agent 循环退出", slog.String("err", err.Error()))
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP 关停超时", slog.String("err", err.Error()))
	}
	logger.Info("已停止", slog.Int64("final_tick", int64(agent.Now)))
	return nil
}

// amkrReachable 探测 AMKR 是否可用（/health 免鉴权）。
func amkrReachable(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
