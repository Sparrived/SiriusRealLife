package transport

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// AMKRProxyOptions 是 /amkr/ 反代的构造参数。
//
// 细则与理由见 docs/llm-amkr.md §4。三条硬约束：
//  1. **必须剥掉 /amkr 前缀**再转发：独立运行的 AMKR 在**根路径**提供
//     服务（/health、/ui/、/api/*），实测 /amkr/ui/index.html 返回 404。
//     注意这与"浏览器侧不改 URL"不矛盾——浏览器必须继续看到 /amkr
//     （WebUI 靠它推导 API 基址），只是上游要收到剥掉前缀的路径。
//  2. **Authorization 用 Set 覆盖**，不能用 Add 追加
//  3. **403 掉运维接口**（操作宿主机，容器里语义不成立）
type AMKRProxyOptions struct {
	// Target 是 AMKR 的基址，如 http://127.0.0.1:28881（带 /amkr 前缀则为 http://amkr:8000/amkr）。
	Target string
	// MountPath 是 Sirius 对外暴露的前缀，默认 /amkr。
	// 转发前会被剥掉。
	MountPath string
	// APIKey 由服务端注入，**不下发给浏览器**。
	APIKey string
	// AllowOps 为真时不拦运维接口。
	//
	// 默认应为 false：更稳的做法是在 AMKR 启动时加 --no-ops
	// （写入 config 的 ops_enabled），本反代只是第二道防线。
	AllowOps bool
}

// opsPaths 是访问宿主机能力的运维接口（docs/llm-amkr.md §4）。
//
// 它们都只需本地 key，因此反代一旦放行就等于把宿主机操作权交出去。
var opsPaths = []string{
	"/api/logs",
	"/api/tool",
	"/api/service",
	"/api/integrations",
}

// NewAMKRProxy 构造 /amkr/ 的反向代理。
//
// 浏览器看到的是 /amkr/...（WebUI 靠页面路径推导 API 基址，**不能**改），
// 而上游 AMKR 独立运行时在根路径提供服务，因此转发前剥掉前缀。
func NewAMKRProxy(opt AMKRProxyOptions) (http.Handler, error) {
	target, err := url.Parse(opt.Target)
	if err != nil {
		return nil, err
	}
	mount := opt.MountPath
	if mount == "" {
		mount = "/amkr"
	}
	// Target 自身若已带路径前缀（如 http://amkr:8000/amkr），
	// 拼接时避免出现双斜杠。
	basePath := strings.TrimSuffix(target.Path, "/")

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// 剥掉挂载前缀：/amkr/health → /health。
			trimmed := strings.TrimPrefix(req.URL.Path, mount)
			if trimmed == "" {
				trimmed = "/"
			}
			if !strings.HasPrefix(trimmed, "/") {
				trimmed = "/" + trimmed
			}
			req.URL.Path = basePath + trimmed
			// RawPath 必须同步清掉，否则 Go 会用旧的转义形式发上游。
			req.URL.RawPath = ""
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// 必须用 Set 覆盖，不能用 Add 追加：
			// AMKR 的 WebUI 总是发送 Authorization（首次访问时是空的
			// "Bearer "），而 Starlette 的 headers.get 只读**第一个**。
			// 追加会让空凭据在前、注入的有效凭据在后，所有管理请求 401。
			req.Header.Set("Authorization", "Bearer "+opt.APIKey)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "amkr_unreachable",
				"无法连接 AMKR: "+err.Error())
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !opt.AllowOps {
			if p, blocked := blockedOpsPath(r.URL.Path, mount); blocked {
				writeError(w, http.StatusForbidden, "ops_disabled",
					"运维接口已被 Sirius 反代屏蔽: "+p)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}), nil
}

// blockedOpsPath 判断路径是否落在运维接口下。
//
// 前缀匹配按路径段边界做，避免 /api/logs2 这类"看似命中"的误伤。
func blockedOpsPath(path, mount string) (string, bool) {
	trimmed := strings.TrimPrefix(path, mount)
	for _, p := range opsPaths {
		if trimmed == p || strings.HasPrefix(trimmed, p+"/") {
			return p, true
		}
	}
	return "", false
}
