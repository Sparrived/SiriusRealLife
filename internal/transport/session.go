package transport

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 会话与登录页（auth 相关全部在这一个文件里，见 server.go 的 requireAuth）。
//
// 为什么需要会话而不是只用 Basic：**浏览器导航没法带 Authorization 头**。
// 一旦要单点进 /amkr/（AMKR 的 WebUI 是同源的另一个前端），就只能靠
// cookie —— 导航过去的请求带不上凭据，Basic 那条路走不通。
// Basic **保留**：curl、脚本与监控用它，不必先登录拿 cookie。
const (
	sessionCookieName = "sirius_session"
	// loginPath / logoutPath 是不需要鉴权的两个路径。
	loginPath  = "/login"
	logoutPath = "/logout"
	// sessionTTL 是会话有效期。7 天：这是个人观测台，不是银行；
	// 但也不是"永不过期"——长期不用的会话应该自己失效。
	sessionTTL = 7 * 24 * time.Hour
	// sessionRefreshAfter 之后重新签发，让常用的人不会被动登出。
	sessionRefreshAfter = 24 * time.Hour
)

// sessionSigner 签发与校验会话 cookie。
//
// cookie 内容 = base64url(用户名|过期Unix秒) + "." + base64url(HMAC)。
// **无状态**：不存服务端会话表，重启不丢（密钥由密码派生），也不需要
// 清理过期条目。签名保证用户不能自己改用户名或过期时间。
type sessionSigner struct{ key []byte }

// newSessionSigner 从凭据派生签名密钥。
//
// 刻意从密码派生而不是每次启动随机：随机会让每次重启（每次部署）都把人
// 踢下线，而安全性并没有更好——密钥的秘密性本来就等于密码的秘密性。
// 反过来，**改密码会自动作废所有已有会话**，这正是想要的。
func newSessionSigner(user, pass string) *sessionSigner {
	mac := hmac.New(sha256.New, []byte("sirius-session-v1"))
	mac.Write([]byte(user))
	mac.Write([]byte{0}) // 分隔符，防 "ab"+"c" 与 "a"+"bc" 撞同一密钥
	mac.Write([]byte(pass))
	return &sessionSigner{key: mac.Sum(nil)}
}

// issue 签发一个到 exp 为止有效的 cookie 值。
func (s *sessionSigner) issue(user string, exp time.Time) string {
	payload := user + "|" + strconv.FormatInt(exp.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.mac(payload)
}

// verify 校验 cookie 值，返回用户名。过期、被改过、格式不对都算无效。
func (s *sessionSigner) verify(v string) (string, bool) {
	dot := strings.LastIndex(v, ".")
	if dot <= 0 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(v[:dot])
	if err != nil {
		return "", false
	}
	payload := string(raw)
	// 先验签再解析内容：不验签就读内容等于让用户自己说了算。
	if subtle.ConstantTimeCompare([]byte(v[dot+1:]), []byte(s.mac(payload))) != 1 {
		return "", false
	}
	sep := strings.LastIndex(payload, "|")
	if sep <= 0 {
		return "", false
	}
	exp, err := strconv.ParseInt(payload[sep+1:], 10, 64)
	if err != nil || time.Now().After(time.Unix(exp, 0)) {
		return "", false
	}
	return payload[:sep], true
}

// mac 计算载荷的签名。
func (s *sessionSigner) mac(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// signer 懒构造并缓存签名器。
//
// 由凭据派生，而凭据在 Server 构造时就固定了，所以只需算一次。
// 惰性是为了让未启用鉴权的部署完全不碰这段代码。
func (s *Server) signer() *sessionSigner {
	if s.sess == nil {
		s.sess = newSessionSigner(s.opt.AuthUser, s.opt.AuthPass)
	}
	return s.sess
}

// cookieSecure 判断该不该给 cookie 打 Secure。
//
// 生产链路是 Cloudflare Tunnel → 宿主回环，**源站看到的是明文 HTTP**
// （TLS 在边缘终止），因此不能只看 r.TLS。两种都认：
//   - r.TLS != nil：直连 TLS（本地反代/自签）
//   - X-Forwarded-Proto: https：隧道或上游反代声明的
//
// 直连 127.0.0.1:8080 的本机调试拿到的是非 Secure cookie——那正是要的，
// 否则 http:// 下浏览器压根不会回传它，人会"登录成功但立刻又要求登录"。
func cookieSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionUser 取出会话里的用户名（无会话或不合法时返回空）。
func (s *Server) sessionUser(r *http.Request) string {
	if !s.authEnabled() {
		return ""
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	user, ok := s.signer().verify(c.Value)
	if !ok {
		return ""
	}
	// 用户名必须与当前配置一致：改了用户名之后旧会话应当失效。
	if subtle.ConstantTimeCompare([]byte(user), []byte(s.opt.AuthUser)) != 1 {
		return ""
	}
	return user
}

// checkBasic 校验 Authorization: Basic 凭据。
//
// 注意**只认 Basic**：AMKR 的 WebUI 会给每个请求带上自己的
// `Authorization: Bearer `，那是给上游用的，不是用来登录 Sirius 的。
// 见到非 Basic 就当"没提供凭据"，继续走 cookie——否则 SSO 进 /amkr/
// 会被自己的鉴权挡在门外。
func (s *Server) checkBasic(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	// 两个比较都要做，不能短路——短路会把"用户名对不对"泄漏成时间差。
	userOK := subtle.ConstantTimeCompare([]byte(u), []byte(s.opt.AuthUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(p), []byte(s.opt.AuthPass)) == 1
	return userOK && passOK
}

// authEnabled 报告是否启用了鉴权。
func (s *Server) authEnabled() bool {
	return s.opt.AuthUser != "" && s.opt.AuthPass != ""
}

// authed 报告这次请求是否已通过鉴权（Basic 或会话 cookie 任一即可）。
func (s *Server) authed(r *http.Request) bool {
	return s.checkBasic(r) || s.sessionUser(r) != ""
}

// exemptPath 是与鉴权无关的路径。
//
// 登录页本身必须在门外（否则死循环），健康检查刻意留空（监控与容器探针
// 不该为了探活存一份密码，而它只报"AMKR 通不通"）。
func exemptPath(p string) bool {
	switch p {
	case loginPath, logoutPath, "/api/v1/health":
		return true
	default:
		return false
	}
}

// wantsHTML 判断这次请求是不是"人在浏览器里点链接"。
//
// 区别对待是必要的：未登录时浏览器应当被**送到登录页**，而 fetch/EventSource
// 应当拿到 401（前端据此跳转，而不是把登录页的 HTML 当成 JSON 解析）。
func wantsHTML(r *http.Request) bool {
	// 只看 Accept，**不按路径特判**。曾经把 /amkr/ 排除在外，理由是
	// "那是 AMKR WebUI 自己发的请求"——但浏览器导航到 /amkr/ 也走同一个
	// 路径，于是人被 401 挡住而不是被送去登录页。
	// 真正的判据是 Accept：导航带 text/html，XHR/EventSource 不带。
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// handleLogin 处理登录页与登录提交。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if r.Method == http.MethodGet {
		if s.authed(r) {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		s.renderLogin(w, next, "")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, next, "提交的内容无法解析")
		return
	}
	// next 优先取**表单域**：登录页把它放在隐藏字段里（页面自身的 URL 是
	// 干净的 /login）。只读 query 会让回跳永远失效——人登录完总是掉回首页，
	// 想去 /amkr/ 还得再点一次。
	if v := r.PostFormValue("next"); v != "" {
		next = safeNext(v)
	}
	u := r.PostFormValue("user")
	p := r.PostFormValue("password")
	userOK := subtle.ConstantTimeCompare([]byte(u), []byte(s.opt.AuthUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(p), []byte(s.opt.AuthPass)) == 1
	if !userOK || !passOK {
		s.log.Warn("login_failed", "user", u, "ip", clientIP(r))
		s.renderLogin(w, next, "用户名或密码不对")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.signer().issue(s.opt.AuthUser, time.Now().Add(sessionTTL)),
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		// Lax：允许从外部链接直接点进来（顶层 GET），但挡住跨站 POST
		// ——本服务的写操作（投递消息、改 AMKR 配置）都是 POST。
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	s.log.Info("login_ok", "user", s.opt.AuthUser, "ip", clientIP(r))
	// 303：POST 之后必须换成 GET，否则刷新会重复提交。
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout 清掉会话并回到登录页。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

// refreshSession 在会话用掉一部分之后重新签发，让常用的人不被被动登出。
func (s *Server) refreshSession(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return
	}
	dot := strings.LastIndex(c.Value, ".")
	if dot <= 0 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	if err != nil {
		return
	}
	payload := string(raw)
	sep := strings.LastIndex(payload, "|")
	if sep <= 0 {
		return
	}
	exp, err := strconv.ParseInt(payload[sep+1:], 10, 64)
	if err != nil {
		return
	}
	if time.Until(time.Unix(exp, 0)) > sessionTTL-sessionRefreshAfter {
		return // 还早，不折腾
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.signer().issue(s.opt.AuthUser, time.Now().Add(sessionTTL)),
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// safeNext 校验登录后跳转的目标，只允许站内路径。
//
// 不校验就是开放重定向：攻击者可以把 /login?next=//evil.example 发给别人，
// 对方登录后被送到钓鱼页，而地址栏那一瞬看着是我们自己的域名。
func safeNext(next string) string {
	if next == "" {
		return "/"
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// clientIP 取客户端 IP 用于日志（隧道下真实来源在 X-Forwarded-For）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}

// loginPageHTML 是登录页。
//
// **内联样式、零外部资源**：它必须在鉴权之外能完整渲染，所以不能依赖
// web/dist 里的任何东西（那些都在门内）。也因此它不跟着前端构建走，
// 设计令牌在这里重抄一份——两边都改了记得对齐。
//
// 视觉沿用观测台那套：近黑冷调 + 单一强调色 + 系统字体栈 + 4/6px 圆角。
var loginPageHTML = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="color-scheme" content="dark" />
<meta name="robots" content="noindex" />
<title>Sirius · 登录</title>
<style>
  :root {
    --bg: #0a0b0d; --bg-raised: #101215; --bg-inset: #0d0f11;
    --line: #1e2126; --line-strong: #2b3038;
    --text: #e6e9ee; --text-dim: #98a0ab; --text-faint: #767f8a;
    --accent: #34d3a6; --warn: #e0a33e;
    --radius: 4px; --radius-lg: 6px;
    --mono: ui-monospace, "JetBrains Mono", "SF Mono", "Cascadia Mono", Menlo, Consolas, monospace;
    --sans: system-ui, -apple-system, "Segoe UI", "Noto Sans SC", "PingFang SC", sans-serif;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100dvh; display: grid; place-items: center;
    background: var(--bg); color: var(--text);
    font-family: var(--sans); font-size: 13px; line-height: 1.5;
    padding: 24px;
  }
  .card { width: 100%; max-width: 320px; }
  .brand { display: flex; align-items: baseline; gap: 8px; margin-bottom: 18px; }
  .mark { width: 7px; height: 7px; border-radius: 50%; background: var(--accent); align-self: center; }
  .title { font-size: 14px; font-weight: 600; letter-spacing: -0.01em; }
  .sub { font-size: 11px; color: var(--text-faint); }
  form { display: grid; gap: 10px; }
  label { display: grid; gap: 5px; }
  .lbl { font-size: 11px; color: var(--text-faint); text-transform: uppercase; letter-spacing: 0.06em; }
  input {
    width: 100%; padding: 8px 10px; font: inherit; font-size: 13px;
    color: var(--text); background: var(--bg-inset);
    border: 1px solid var(--line-strong); border-radius: var(--radius);
  }
  input::placeholder { color: var(--text-faint); }
  input:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  button {
    margin-top: 4px; padding: 8px 12px; font: inherit; font-size: 13px;
    color: var(--bg); background: var(--accent);
    border: 1px solid var(--accent); border-radius: var(--radius); cursor: pointer;
  }
  button:hover { background: color-mix(in srgb, var(--accent) 85%, white); }
  button:active { transform: translateY(1px); }
  button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  .err {
    margin: 0; padding: 7px 9px; font-size: 11px; color: var(--warn);
    background: var(--bg-raised); border: 1px solid var(--line-strong);
    border-radius: var(--radius);
  }
  .foot { margin: 14px 0 0; font-size: 11px; color: var(--text-faint); }
  .foot code { font-family: var(--mono); font-size: 11px; color: var(--text-dim); }
</style>
</head>
<body>
  <div class="card">
    <div class="brand">
      <span class="mark" aria-hidden="true"></span>
      <span class="title">Sirius</span>
      <span class="sub">人格观测台</span>
    </div>
    {{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
    <form method="post" action="{{.LoginPath}}">
      <input type="hidden" name="next" value="{{.Next}}" />
      <label>
        <span class="lbl">用户名</span>
        <input name="user" type="text" autocomplete="username" autofocus
               autocapitalize="none" autocorrect="off" spellcheck="false" required />
      </label>
      <label>
        <span class="lbl">密码</span>
        <input name="password" type="password" autocomplete="current-password" required />
      </label>
      <button type="submit">登录</button>
    </form>
    <p class="foot">登录后可一并进入 <code>/amkr/</code> 管理台。</p>
  </div>
</body>
</html>
`))

// renderLogin 渲染登录页。
func (s *Server) renderLogin(w http.ResponseWriter, next, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 登录页绝不能被缓存：它带着一次性错误提示与 next，缓存住会串台。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(loginStatus(errMsg))
	_ = loginPageHTML.Execute(w, struct {
		Next      string
		Error     string
		LoginPath string
	}{Next: next, Error: errMsg, LoginPath: loginPath})
}

// loginStatus 登录失败时回 401，首次渲染回 200。
func loginStatus(errMsg string) int {
	if errMsg != "" {
		return http.StatusUnauthorized
	}
	return http.StatusOK
}

// loginRedirect 把浏览器送到登录页，并带上回来的路。
func loginRedirect(w http.ResponseWriter, r *http.Request) {
	target := loginPath
	if r.Method == http.MethodGet && r.URL.Path != "/" {
		target += "?next=" + url.QueryEscape(r.URL.RequestURI())
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
