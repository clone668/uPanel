package api

// P16.3：路由清单守卫。
//
// 这两个用例不判断"哪条路由该要 admin"——那是 CONTRACTS.md 第 4.3 节权限列的活，
// 各个域都有自己的用例（users_test / shares_test / storagevolumes_test …）。
// 它们只问两件对**所有**路由都成立的事，所以新加一条路由忘了带中间件时会自己红：
//
//  1. 没有会话的请求，除三条公开入口外，不可能拿到 401 以外的任何结果；
//  2. 有会话、没有 CSRF 头的写请求，必须是 403 CSRF_FAILED。
//
// 为什么按行为断言而不是扒中间件列表：中间件可以挂在组上也可以挂在路由上，
// 顺序还能调；唯一不会撒谎的是"发一个真实请求看它回什么"。
//
// 这层守卫要的是"漏挂就红"，不是"证明每条路由都安全"。它挡不住
// "挂了 requireSession 但角色判错"，那仍然要靠各域的用例与契约表。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upanel/internal/httpx"
)

// publicRoutes 是唯一允许在没有会话时返回非 401 的入口。
//
// 多一条就要在评审里说清理由：探活必须能被未登录的进程调用，登录本身是为了换会话。
// 增删这里比增删路由更该被追问。
var publicRoutes = map[string]string{
	"GET /api/v1/health/live":  "探活：安装脚本、systemd、负载均衡都要在登录前判活",
	"HEAD /api/v1/health/live": "探活的 HEAD 形态",
	"POST /api/v1/auth/login":  "换会话的入口，本身没有会话",
}

// concretePath 把路由模板换成一条能真正命中的具体路径。
//
// 参数值只要形状合法即可：守卫断言的是"中间件拦在前面"，不是 handler 的业务结果。
func concretePath(t *testing.T, routePath string) string {
	t.Helper()
	replacer := strings.NewReplacer(
		":id", "11111111-1111-1111-1111-111111111111",
		":unit", "nginx.service",
		":name", "demo",
	)
	out := replacer.Replace(routePath)
	if strings.Contains(out, ":") {
		t.Fatalf("路由 %q 里还有没处理过的路径参数，concretePath 需要补一条替换", routePath)
	}
	return out
}

// routeList 取路由表并按 method+path 排序，失败信息才稳定。
func routeList(t *testing.T, srv *Server) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, rt := range srv.routes().Routes() {
		key := rt.Method + " " + rt.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	if len(out) < 100 {
		t.Fatalf("路由表只有 %d 条，明显不对（路由没挂上？）", len(out))
	}
	// 数字进 -v 输出，方便对着契约表点数；守卫生效时它也是"确实枚举到了整张表"的凭据。
	t.Logf("路由表共 %d 条（%d 条公开入口）", len(out), len(publicRoutes))
	return out
}

// doRoute 发一个不带任何凭证的请求。
func doRoute(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	// 不带 Cookie、不带 Origin、不带 CSRF：模拟一个只知道 URL 的人。
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEveryRouteRejectsAnonymousWith401(t *testing.T) {
	f := newAdminFixture(t)

	for _, key := range routeList(t, f.srv) {
		if _, ok := publicRoutes[key]; ok {
			continue
		}
		parts := strings.SplitN(key, " ", 2)
		method, routePath := parts[0], parts[1]
		rec := doRoute(t, f.srv, method, concretePath(t, routePath))

		if rec.Code == http.StatusUnauthorized {
			if got := parseResult(t, rec); got.Error == nil || string(httpx.CodeUnauthenticated) != got.Error.Code {
				t.Fatalf("%s %s 未登录错误码 = %+v，期望 %s", method, routePath, got.Error, httpx.CodeUnauthenticated)
			}
			continue
		}
		// 不是 401 就是漏了 requireSession（或者被别的中间件先拦了）。
		t.Fatalf("%s %s 未登录时返回 %d，期望 401——这条路由没有挂 requireSession？响应：%s",
			method, routePath, rec.Code, routeBody(rec))
	}
}

// routeBody 截断响应体，避免失败信息里塞进整页 HTML。
func routeBody(rec *httptest.ResponseRecorder) string {
	body := strings.TrimSpace(rec.Body.String())
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return "（空）"
	}
	return body
}

func TestEveryWriteRouteRejectsMissingCSRF(t *testing.T) {
	f := newAdminFixture(t)

	// 这些方法不改状态，按契约不要求 CSRF（SSE 与下载都是 GET）。
	exempt := map[string]bool{
		http.MethodGet:     true,
		http.MethodHead:    true,
		http.MethodOptions: true,
	}

	checked := 0
	for _, key := range routeList(t, f.srv) {
		parts := strings.SplitN(key, " ", 2)
		method, routePath := parts[0], parts[1]
		if exempt[method] {
			continue
		}
		if _, ok := publicRoutes[key]; ok {
			continue // 没有会话可绑定的 CSRF，登录本身不做这层校验
		}
		checked++

		req := httptest.NewRequest(method, concretePath(t, routePath), strings.NewReader("{}"))
		req.Header.Set("Cookie", f.cookie.String())
		req.Header.Set("Content-Type", "application/json")
		// 故意不带 X-CSRF-Token。
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 带会话但不带 CSRF 返回 %d，期望 403——这条路由没有挂 requireCSRF？响应：%s",
				method, routePath, rec.Code, routeBody(rec))
		}
		var parsed apiResult
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("%s %s 响应不是合法 JSON: %v", method, routePath, err)
		}
		if parsed.Error == nil || string(httpx.CodeCSRFFailed) != parsed.Error.Code {
			t.Fatalf("%s %s 的错误码 = %+v，期望 %s", method, routePath, parsed.Error, httpx.CodeCSRFFailed)
		}
	}
	if checked < 20 {
		t.Fatalf("只检查了 %d 条写路由，明显不对", checked)
	}
}

// TestNoGenericExecutionRoute 守住产品硬约束：不存在"万能命令执行/任意 root shell"的入口。
//
// 这条不是靠读代码保证的，而是靠路由名扫描——它挡不住有人把执行能力塞进一条
// 名字看不出来的路由，但能让"新增一条 /exec"必须显式改这个用例。
func TestNoGenericExecutionRoute(t *testing.T) {
	f := newAdminFixture(t)

	banned := []string{"exec", "shell", "command", "sudo", "raw", "eval", "sql"}
	for _, key := range routeList(t, f.srv) {
		lower := strings.ToLower(key)
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Fatalf("路由 %q 里出现 %q：这类入口必须先证明它不是一个万能执行面", key, word)
			}
		}
	}
}
