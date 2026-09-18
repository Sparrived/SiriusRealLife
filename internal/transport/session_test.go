package transport

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// login 提交一次登录，返回响应与拿到的会话 cookie。
func login(t *testing.T, srv http.Handler, user, pass, next string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	form := url.Values{"user": {user}, "password": {pass}}
	if next != "" {
		form.Set("next", next)
	}
	req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return rec, c
		}
	}
	return rec, nil
}

// TestLoginPageIsReachableWithoutAuth 验证登录页在门外。
//
// 它必须在门外，否则就是个死循环：要登录先得登录。
func TestLoginPageIsReachableWithoutAuth(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, loginPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("登录页 = %d, 期望 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`name="user"`, `name="password"`, `method="post"`} {
		if !strings.Contains(body, want) {
			t.Errorf("登录页缺少 %s", want)
		}
	}
	// 不该出现 Basic 的质询：我们要的是页面，不是浏览器弹窗。
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("登录页不该带 WWW-Authenticate（那会触发浏览器原生弹窗）")
	}
}

// TestLoginSetsSessionThatAuthorizesRequests 验证登录之后 cookie 能过鉴权。
func TestLoginSetsSessionThatAuthorizesRequests(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")

	rec, ck := login(t, srv, "sirius", "s3cret", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("登录成功应 303 跳转，实际 %d", rec.Code)
	}
	if ck == nil {
		t.Fatal("登录成功但没有下发会话 cookie")
	}
	if !ck.HttpOnly {
		t.Error("会话 cookie 必须是 HttpOnly（JS 不该读得到）")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, 期望 Lax", ck.SameSite)
	}
	if ck.Path != "/" {
		t.Errorf("Path = %q, 期望 /", ck.Path)
	}

	// 带上 cookie 就该放行——包括 API 与 /amkr/。
	for _, p := range []string{"/api/v1/agents/default", "/amkr/ui/"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("带会话 cookie 访问 %s 仍被拒", p)
		}
	}
}

// TestExistingBearerHeaderDoesNotBreakSession 是**单点登录 AMKR 的核心回归**。
//
// AMKR 的 WebUI 会给每个请求带上自己的 `Authorization: Bearer `（空），
// 那是给上游 AMKR 的，不是用来登录 Sirius 的。早期实现用 r.BasicAuth()
// 判"凭据合法"，见到非 Basic 就 401 —— 于是"登录成功却打不开 /amkr/"。
func TestExistingBearerHeaderDoesNotBreakSession(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	_, ck := login(t, srv, "sirius", "s3cret", "")
	if ck == nil {
		t.Fatal("登录失败，拿不到会话")
	}

	for _, h := range []string{"Bearer ", "Bearer something-else", "Basic "} {
		req := httptest.NewRequest(http.MethodGet, "/amkr/ui/", nil)
		req.AddCookie(ck)
		req.Header.Set("Authorization", h)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("带会话 + Authorization:%q 时被拒——AMKR 的 WebUI 正是这样发请求的", h)
		}
	}
}

// TestNonBasicHeaderAloneIsNotEnough 验证反过来也成立：
// 只有 AMKR 那个 Bearer 头、没有会话，仍然进不去。
func TestNonBasicHeaderAloneIsNotEnough(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/default", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("只有 Bearer 头应当 401，实际 %d", rec.Code)
	}
}

// TestBrowserIsSentToLogin 验证浏览器导航被送去登录页，而不是弹窗。
func TestBrowserIsSentToLogin(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	req := httptest.NewRequest(http.MethodGet, "/amkr/ui/dashboard", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("浏览器未登录时 = %d, 期望 303 送去登录页", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, loginPath) {
		t.Fatalf("Location = %q, 期望指向 %s", loc, loginPath)
	}
	// 必须带回跳地址，否则登录完只会掉到首页。
	if !strings.Contains(loc, "next=") || !strings.Contains(loc, "amkr") {
		t.Errorf("Location = %q, 应带上原始路径", loc)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("不该带 WWW-Authenticate——那会让浏览器弹原生窗口")
	}
}

// TestAPIGets401NotRedirect 验证 fetch/EventSource 拿到 401 而不是 HTML 登录页。
//
// 这对前端是必须的：它据此跳转登录，而不是把登录页的 HTML 当 JSON 解析
// 然后报一个 "Unexpected token '<'"。
func TestAPIGets401NotRedirect(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	for _, p := range []string{"/api/v1/agents/default", "/api/v1/agents/default/stream"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Accept", "*/*")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, 期望 401", p, rec.Code)
		}
	}
}

// TestBadPasswordIsRejectedAndExplained 验证错误密码。
func TestBadPasswordIsRejectedAndExplained(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	rec, ck := login(t, srv, "sirius", "wrong", "")
	if ck != nil {
		t.Fatal("密码错却下发了会话")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("密码错 = %d, 期望 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "不对") {
		t.Error("登录页应当说明失败原因，而不是静默重绘")
	}
}

// TestNextCannotRedirectOffSite 验证开放重定向被挡住。
//
// 不挡的话，/login?next=//evil.example 会把刚登录的人送去钓鱼页，
// 而地址栏那一瞬看着是我们自己的域名。
func TestNextCannotRedirectOffSite(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	for _, evil := range []string{"//evil.example", "https://evil.example", "/\\evil.example", "evil.example"} {
		rec, ck := login(t, srv, "sirius", "s3cret", evil)
		if ck == nil {
			t.Fatalf("next=%q 时登录失败", evil)
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Errorf("next=%q 应回落到 /，实际 %q", evil, loc)
		}
	}
	// 站内路径要照常保留。
	rec, ck := login(t, srv, "sirius", "s3cret", "/amkr/ui/")
	if ck == nil {
		t.Fatal("登录失败")
	}
	if loc := rec.Header().Get("Location"); loc != "/amkr/ui/" {
		t.Errorf("站内 next 应保留，实际 %q", loc)
	}
}

// TestExpiredOrTamperedSessionIsRejected 验证签名与过期时间真的在起作用。
func TestExpiredOrTamperedSessionIsRejected(t *testing.T) {
	s := newSessionSigner("sirius", "s3cret")

	// 自己签一个已过期的
	expired := s.issue("sirius", time.Now().Add(-time.Minute))
	if _, ok := s.verify(expired); ok {
		t.Error("过期的会话不该通过校验")
	}
	// 改载荷（不动签名）
	valid := s.issue("sirius", time.Now().Add(time.Hour))
	if _, ok := s.verify(valid); !ok {
		t.Fatal("刚签发的会话应当有效")
	}
	tampered := strings.Replace(valid, "c2ly", "ZXZpbA", 1)
	if _, ok := s.verify(tampered); ok {
		t.Error("被改过的会话不该通过校验")
	}
	// 换个用户名重签，签名必须跟着变（否则改用户名能提权）。
	if s.issue("someone-else", time.Now().Add(time.Hour)) == valid {
		t.Error("不同用户名的签名不该相同")
	}
	// 换了密码，旧会话必须失效（密钥由密码派生）。
	other := newSessionSigner("sirius", "another-password")
	if _, ok := other.verify(valid); ok {
		t.Error("密码变了之后旧会话应当失效")
	}
}

// TestLogoutClearsSession 验证登出真的清掉 cookie。
func TestLogoutClearsSession(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")
	req := httptest.NewRequest(http.MethodPost, logoutPath, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("登出 = %d, 期望 303", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("登出应当把会话 cookie 置为过期")
	}
}

// TestCookieSecureFollowsForwardedProto 验证 cookie 的 Secure 属性看的是真实链路。
//
// 生产是 Cloudflare Tunnel → 宿主回环，源站看到的是明文 HTTP（TLS 在边缘
// 终止）。只认 r.TLS 会让 cookie 永远缺 Secure；反过来无条件加 Secure
// 又会让本机 http:// 调试"登录成功但立刻又要求登录"。
func TestCookieSecureFollowsForwardedProto(t *testing.T) {
	srv := newAuthServer(t, "sirius", "s3cret")

	form := url.Values{"user": {"sirius"}, "password": {"s3cret"}}
	mk := func(proto string) *http.Cookie {
		req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookieName {
				return c
			}
		}
		return nil
	}

	if c := mk("https"); c == nil || !c.Secure {
		t.Error("X-Forwarded-Proto: https 时 cookie 应当带 Secure")
	}
	if c := mk(""); c == nil || c.Secure {
		t.Error("明文直连时 cookie 不该带 Secure，否则浏览器根本不回传")
	}
}
