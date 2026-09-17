package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveAMKRProxy 对**真实** AMKR 端到端验证反代。
//
// 默认跳过（离线环境不该依赖本机服务）。开启：
//
//	$env:AMKR_SMOKE=1; $env:AMKR_LIVE_URL="http://127.0.0.1:28881"; go test ./internal/transport/ -run LiveAMKR -v
//
// 这个测试存在的理由：路径前缀的处理方式**只能靠实测确认**。
// 文档曾写"1:1 透传"，但独立运行的 AMKR 在根路径提供服务，
// 实测 /amkr/ui/index.html 返回 404 —— 必须剥前缀。这条测试把它钉住。
func TestLiveAMKRProxy(t *testing.T) {
	if os.Getenv("AMKR_SMOKE") == "" {
		t.Skip("跳过真实 AMKR 反代测试（设 AMKR_SMOKE=1 开启）")
	}
	live := os.Getenv("AMKR_LIVE_URL")
	if live == "" {
		live = "http://127.0.0.1:28881"
	}
	key := os.Getenv("AMKR_API_KEY")

	proxy, err := NewAMKRProxy(AMKRProxyOptions{Target: live, APIKey: key})
	if err != nil {
		t.Fatalf("NewAMKRProxy: %v", err)
	}
	srv := httptest.NewServer(func() http.Handler {
		mux := http.NewServeMux()
		mux.Handle("/amkr/", proxy)
		return mux
	}())
	defer srv.Close()

	client := &http.Client{Timeout: 10 * time.Second}

	get := func(path string) (int, string) {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, string(b)
	}

	// 1. /amkr/health 免鉴权，应当 200（前缀被正确剥掉）。
	code, body := get("/amkr/health")
	if code != http.StatusOK {
		t.Fatalf("/amkr/health 状态码 = %d, 期望 200（前缀未正确剥掉？）body=%s", code, trunc(body))
	}
	if !strings.Contains(body, `"status"`) {
		t.Errorf("/amkr/health 响应不像 AMKR 的 health: %s", trunc(body))
	}

	// 2. /amkr/ui/index.html 应当 200：证明 WebUI 反代可用。
	code, body = get("/amkr/ui/index.html")
	if code != http.StatusOK {
		t.Fatalf("/amkr/ui/index.html 状态码 = %d, 期望 200；body=%s", code, trunc(body))
	}
	if !strings.Contains(strings.ToLower(body), "<!doctype html") {
		t.Errorf("UI 首页不像 HTML: %s", trunc(body))
	}

	// 3. 管理 API：注入有效 key 后应当能通（验证 Set 覆盖生效）。
	//    没有 key 时应 401，也不能是 500/404。
	code, body = get("/amkr/api/settings")
	if key == "" {
		if code != http.StatusUnauthorized {
			t.Errorf("无 key 时 /amkr/api/settings 状态码 = %d, 期望 401；body=%s", code, trunc(body))
		}
	} else {
		if code != http.StatusOK {
			t.Fatalf("带 key 时 /amkr/api/settings 状态码 = %d, 期望 200（注入未生效？）body=%s", code, trunc(body))
		}
	}

	// 4. 运维接口必须被拦，不转发。
	code, _ = get("/amkr/api/logs")
	if code != http.StatusForbidden {
		t.Errorf("/amkr/api/logs 状态码 = %d, 期望 403（运维接口未拦）", code)
	}
}

func trunc(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
