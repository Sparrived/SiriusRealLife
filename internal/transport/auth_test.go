package transport

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// newAuthServer 造一个开了 HTTP Basic 的服务，并挂一个假的 /amkr/ 后端。
//
// 假 /amkr/ 是必须的：这条鉴权最需要挡住的就是那个反代（它等同于 AMKR 的
// 完整管理权限），只测 API 路由会漏掉真正危险的那个路径。
func newAuthServer(t *testing.T, user, pass string) http.Handler {
	t.Helper()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := fsm.New(fsm.Options{
		Name: "t", States: fsm.MVPStates(), Initial: "idle", Logger: silent,
	})
	if err != nil {
		t.Fatalf("fsm.New: %v", err)
	}
	amkr := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "AMKR 管理台")
	})
	return NewServer(Options{
		Agent:       a,
		Broadcaster: NewBroadcaster(),
		Logger:      silent,
		AgentID:     "default",
		Proxy:       amkr,
		AuthUser:    user,
		AuthPass:    pass,
	}).Handler()
}

// TestBasicAuthProtectsEverything 验证鉴权包住全部路由**包括 /amkr/**。
//
// 这不是可选的加固：/amkr/ 反代等于 AMKR 的完整管理权限，能读到上游 key。
// 把服务挂到公网（隧道/反代）之前，必须有东西挡住它——AGENTS.md §4 那条
// "没有鉴权只能绑回环"的安全线，前提就是这条。
func TestBasicAuthProtectsEverything(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")

	cases := []struct {
		name string
		path string
		// longLived 为真时不测"凭据正确"那一支：SSE 是长连接，
		// ServeHTTP 在连接关闭前不会返回，会把测试挂死。
		// 拒绝路径仍然要测——那正是鉴权拦在前面的地方。
		longLived bool
	}{
		{"状态快照", "/api/v1/agents/default", false},
		{"SSE 流", "/api/v1/agents/default/stream", true},
		{"投递消息", "/api/v1/agents/default/events", false},
		{"AMKR 管理台", "/amkr/ui/", false},
		{"静态前端", "/", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 无凭据
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("无凭据访问 %s = %d, 期望 401", c.path, rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Errorf("%s 的 401 缺少 WWW-Authenticate，浏览器不会弹登录框", c.path)
			}

			// 密码错
			rec = httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			req.SetBasicAuth("sirius", "wrong")
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("密码错访问 %s = %d, 期望 401", c.path, rec.Code)
			}

			// 用户名错
			rec = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodGet, c.path, nil)
			req.SetBasicAuth("nobody", "s3cret")
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("用户名错访问 %s = %d, 期望 401", c.path, rec.Code)
			}

			// 凭据正确
			if c.longLived {
				return
			}
			rec = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodGet, c.path, nil)
			req.SetBasicAuth("sirius", "s3cret")
			srv.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Errorf("凭据正确访问 %s 仍被拒（401）", c.path)
			}
		})
	}
}

// TestHealthStaysOpen 验证健康检查不要求凭据。
//
// 刻意留空：它只报"AMKR 通不通"，不含任何私人内容，而监控与容器探针
// 不该为了探活去存一份密码。
func TestHealthStaysOpen(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("健康检查 = %d, 期望 200（它不该要求凭据）", rec.Code)
	}
}

// TestNoAuthWhenUnset 验证没配凭据时不启用鉴权。
//
// 本机开发与既有测试都依赖这一点：默认不改变行为，配了才生效。
func TestNoAuthWhenUnset(t *testing.T) {
	srv := newAuthServer(t, "", "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents/default", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Error("没配凭据时不该要求登录")
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/amkr/ui/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("没配凭据时 /amkr/ 应照常放行，实际 %d", rec.Code)
	}
}
