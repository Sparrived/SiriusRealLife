package transport

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// newFakeAMKR 起一个假的 AMKR，记录它收到的路径与 Authorization 头。
func newFakeAMKR(t *testing.T) (*httptest.Server, *captured) {
	t.Helper()
	cap := &captured{done: make(chan struct{}, 32)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("amkr-ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type captured struct {
	paths []string
	auths []string
	done  chan struct{}
}

func (c *captured) record(r *http.Request) {
	c.paths = append(c.paths, r.URL.Path)
	c.auths = append(c.auths, r.Header.Get("Authorization"))
	select {
	case c.done <- struct{}{}:
	default:
	}
}

// TestProxyStripsMountPrefix 锁住第一条硬约束：转发前剥掉 /amkr 前缀。
//
// 独立运行的 AMKR 在**根路径**提供服务（/health、/ui/、/api/*）；
// 实测 /amkr/ui/index.html 返回 404。因此上游必须收到剥掉前缀的路径。
// 同时浏览器侧**不能**改 URL——WebUI 靠 location.pathname 推导 API 基址。
func TestProxyStripsMountPrefix(t *testing.T) {
	fake, cap := newFakeAMKR(t)
	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: fake.URL, APIKey: "real-key"})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/amkr/", proxy)

	cases := []struct{ browser, upstream string }{
		{"/amkr/ui/index.html", "/ui/index.html"},
		{"/amkr/health", "/health"},
		{"/amkr/api/settings", "/api/settings"},
		{"/amkr/", "/"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.browser, nil))
		<-cap.done
		if got := cap.paths[len(cap.paths)-1]; got != c.upstream {
			t.Errorf("%s 转发到上游的路径 = %q, 期望 %q", c.browser, got, c.upstream)
		}
	}
}

// TestProxyWithTargetSubPath 验证 Target 自带路径前缀时不出现双斜杠。
func TestProxyWithTargetSubPath(t *testing.T) {
	fake, cap := newFakeAMKR(t)
	proxy, err := NewAMKRProxy(AMKRProxyOptions{
		Target: fake.URL + "/base", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/amkr/", proxy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/amkr/health", nil))
	<-cap.done
	if got := cap.paths[0]; got != "/base/health" {
		t.Fatalf("上游路径 = %q, 期望 /base/health", got)
	}
	if strings.Contains(cap.paths[0], "//") {
		t.Errorf("路径出现双斜杠: %q", cap.paths[0])
	}
}

// TestProxyInjectsAuthBySetNotAdd 锁住第二条硬约束。
//
// AMKR 的 WebUI 总是发送 Authorization（localStorage 为空时是空的
// "Bearer "），而 Starlette 的 headers.get 只读**第一个**头。
// 若用 Add 追加，浏览器那个空凭据在前、注入的有效凭据在后 —— 全部 401。
func TestProxyInjectsAuthBySetNotAdd(t *testing.T) {
	fake, cap := newFakeAMKR(t)
	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: fake.URL, APIKey: "real-key"})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/amkr/", proxy)

	// 模拟 WebUI：带一个空凭据的 Authorization（首次访问就是这个样子）。
	req := httptest.NewRequest(http.MethodGet, "/amkr/api/settings", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	<-cap.done

	if len(cap.auths) != 1 {
		t.Fatalf("AMKR 收到 %d 个请求", len(cap.auths))
	}
	if got := cap.auths[0]; got != "Bearer real-key" {
		t.Fatalf("AMKR 收到的 Authorization = %q, 期望被覆盖为 Bearer real-key", got)
	}
	// 关键：不能是追加后的多值（Go 侧会是 "Bearer , Bearer real-key"）。
	if strings.Contains(cap.auths[0], ",") {
		t.Fatalf("Authorization 被追加成了多值: %q（Starlette 只读第一个，会 401）", cap.auths[0])
	}
}

// TestProxyBlocksOpsPaths 锁住第三条硬约束：运维接口必须 403。
//
// 这些接口能操作宿主机（读日志、启停服务、改写本机 Agent 配置），
// 反代一旦放行就等于把宿主机操作权交给任何能访问 Sirius 的人。
func TestProxyBlocksOpsPaths(t *testing.T) {
	fake, cap := newFakeAMKR(t)
	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: fake.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/amkr/", proxy)

	blocked := []string{
		"/amkr/api/logs",
		"/amkr/api/tool",
		"/amkr/api/service/start",
		"/amkr/api/integrations/claude",
	}
	for _, p := range blocked {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 状态码 = %d, 期望 403", p, rec.Code)
		}
	}
	// 一条都不该到达 AMKR。
	if len(cap.paths) != 0 {
		t.Fatalf("被拦的运维请求仍到达了 AMKR: %v", cap.paths)
	}
}

// TestProxyAllowsNormalManagementPaths 验证只拦运维接口，不误伤正常管理 API。
func TestProxyAllowsNormalManagementPaths(t *testing.T) {
	fake, cap := newFakeAMKR(t)
	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: fake.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/amkr/", proxy)

	allowed := []string{"/amkr/api/settings", "/amkr/api/providers", "/amkr/ui/app.js", "/amkr/health"}
	for _, p := range allowed {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		<-cap.done
		if rec.Code != http.StatusOK {
			t.Errorf("%s 状态码 = %d, 期望 200（不应被误拦）", p, rec.Code)
		}
	}
	if len(cap.paths) != len(allowed) {
		t.Errorf("到达 AMKR 的请求数 = %d, 期望 %d", len(cap.paths), len(allowed))
	}
}

// TestOpsPathBoundaryMatching 验证前缀匹配按路径段边界，不误伤形似路径。
func TestOpsPathBoundaryMatching(t *testing.T) {
	cases := []struct {
		path    string
		blocked bool
	}{
		{"/amkr/api/logs", true},
		{"/amkr/api/logs/", true},
		{"/amkr/api/logs/123", true},
		{"/amkr/api/logs2", false},   // 形似但非运维接口
		{"/amkr/api/logsxyz", false}, // 同上
		{"/amkr/api/settings", false},
		{"/amkr/api/service", true},
		{"/amkr/api/services", false},
	}
	for _, c := range cases {
		_, got := blockedOpsPath(c.path, "/amkr")
		if got != c.blocked {
			t.Errorf("blockedOpsPath(%q) = %v, 期望 %v", c.path, got, c.blocked)
		}
	}
}

// TestProxyAllowOpsBypassesBlock 验证 AllowOps 开关（用于本机调试）。
func TestProxyAllowOpsBypassesBlock(t *testing.T) {
	fake, cap := newFakeAMKR(t)
	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: fake.URL, APIKey: "k", AllowOps: true})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/amkr/", proxy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/amkr/api/logs", nil))
	<-cap.done
	if rec.Code != http.StatusOK {
		t.Fatalf("AllowOps=true 时应放行，实际 %d", rec.Code)
	}
}

// TestStaticAndProxyCoexist 回归测试：同时挂静态目录与 /amkr/ 反代。
//
// 这个组合曾经让服务启动即 panic：
//
//	panic: pattern "/amkr/" conflicts with pattern "GET /"
//	  (/amkr/ matches more methods than GET /, but has a more specific path pattern)
//
// Go 1.22 的 ServeMux 在判定冲突时，两种模式"各有一方面更具体"就冲突。
// 之前的单元测试分别验证了两者，却没有任何一条同时设置 StaticDir 与
// Proxy —— 而生产配置（cmd/sirius）恰好两者都开。只有真正跑二进制才暴露。
func TestStaticAndProxyCoexist(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<!doctype html><title>sirius</title>"), 0o644); err != nil {
		t.Fatalf("写静态文件: %v", err)
	}
	fake, cap := newFakeAMKR(t)
	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: fake.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}

	b := NewBroadcaster()
	agent, err := fsm.New(fsm.Options{
		Name: "t", States: fsm.MVPStates(), Seed: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("fsm.New: %v", err)
	}
	s := NewServer(Options{
		Agent: agent, Broadcaster: b, AgentID: "sirius",
		StaticDir: dir, Proxy: proxy,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// 注册本身不能 panic —— 这就是这个测试的核心。
	var h http.Handler
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("同时挂 StaticDir 与 Proxy 时注册路由 panic: %v", r)
			}
		}()
		h = s.Handler()
	}()

	// 静态首页走本地文件。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET / 状态码 = %d, 期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sirius") {
		t.Errorf("GET / 应返回静态首页，实际 %q", rec.Body.String())
	}

	// /amkr/ 走反代，而不是被静态目录吃掉。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/amkr/health", nil))
	<-cap.done
	if rec.Code != http.StatusOK {
		t.Errorf("GET /amkr/health 状态码 = %d, 期望 200", rec.Code)
	}
	if got := cap.paths[len(cap.paths)-1]; got != "/health" {
		t.Errorf("/amkr/ 应走反代（上游 /health），实际上游路径 %q", got)
	}

	// /amkr（无尾斜杠）应被重定向，而不是落到静态目录的 404。
	// ServeMux 的子树重定向在这个组合下是 307（Temporary Redirect）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/amkr", nil))
	if rec.Code != http.StatusTemporaryRedirect &&
		rec.Code != http.StatusMovedPermanently &&
		rec.Code != http.StatusFound {
		t.Errorf("GET /amkr 状态码 = %d, 期望重定向", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "/amkr/") {
		t.Errorf("重定向目标 = %q, 期望指向 /amkr/", loc)
	}

	// API 路由仍然可用，没被 "/" 抢走。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/health 状态码 = %d, 期望 200", rec.Code)
	}
}

// TestUnreachableProxyReturns502 验证 AMKR 不可达时如实报 502。
func TestUnreachableProxyReturns502(t *testing.T) {
	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: "http://127.0.0.1:1", APIKey: "k"})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/amkr/", proxy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/amkr/health", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "amkr_unreachable") {
		t.Errorf("错误形状不符: %s", rec.Body.String())
	}
}
