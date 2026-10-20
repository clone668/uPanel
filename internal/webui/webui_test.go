package webui

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const testIndex = `<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="UTF-8" />
    <title>uPanel</title>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/assets/index-abcdef12.js"></script>
  </body>
</html>
`

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte(testIndex)},
		"assets/index-abcdef12.js": {
			Data: []byte(`console.log("uPanel")`),
		},
		"assets/index-abcdef12.css": {Data: []byte(`body{margin:0}`)},
		"favicon.svg":               {Data: []byte(`<svg/>`)},
	}
}

func newTestHandler(t *testing.T, fsys fs.FS, mode string) *Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := NewHandler(FromFS(fsys), Options{Logger: logger, Mode: mode})
	if err != nil {
		t.Fatalf("NewHandler 失败: %v", err)
	}
	return h
}

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	h.ServeHTTP(rec, req)
	return rec
}

func TestAssetsAvailability(t *testing.T) {
	if got := FromFS(testFS()).Available(); !got {
		t.Fatal("有 index.html 时应为可用")
	}
	if FromFS(testFS()).Reason() != "" {
		t.Fatal("可用时不应带原因")
	}

	empty := FromFS(fstest.MapFS{".keep": {Data: []byte("placeholder")}})
	if empty.Available() {
		t.Fatal("没有 index.html 时不应判为可用")
	}
	if !strings.Contains(empty.Reason(), "scripts/build.sh") {
		t.Fatalf("不可用原因应指出构建方式，实际 %q", empty.Reason())
	}

	if FromFS(nil).Available() {
		t.Fatal("nil 文件系统不应可用")
	}
	if !strings.Contains((*Assets)(nil).Reason(), "构建产物") {
		t.Fatal("nil Assets 的 Reason 应说明产物缺失")
	}
}

func TestNewHandlerRequiresLogger(t *testing.T) {
	if _, err := NewHandler(FromFS(testFS()), Options{}); err == nil {
		t.Fatal("缺少 Logger 时必须返回错误")
	}
	if _, err := NewHandler(nil, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Fatal("缺少 Assets 时必须返回错误")
	}
}

func TestUnavailableServesExplicit503(t *testing.T) {
	h := newTestHandler(t, fstest.MapFS{".keep": {Data: []byte("x")}}, "production")

	for _, target := range []string{"/", "/storage", "/assets/index-abcdef12.js"} {
		rec := serve(h, http.MethodGet, target)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: 期望 503，实际 %d", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="app"`) {
			t.Fatalf("%s: 资源缺失时不得返回页面内容", target)
		}
		if !strings.Contains(rec.Body.String(), "前端资源") {
			t.Fatalf("%s: 503 正文应说明缺少前端资源，实际 %q", target, rec.Body.String())
		}
	}
}

func TestIndexInjectsMode(t *testing.T) {
	h := newTestHandler(t, testFS(), "mock")
	rec := serve(h, http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type 应为 html，实际 %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("入口页必须 no-store，实际 %q", cc)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `window.__UPANEL_MODE__="mock"`) {
		t.Fatalf("缺少模式注入: %s", body)
	}
	if !strings.Contains(body, `id="app"`) {
		t.Fatal("入口页结构被破坏")
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP 不完整: %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("脚本策略不允许 unsafe-inline: %q", csp)
	}

	nonce := nonceFromCSP(t, csp)
	if !strings.Contains(body, `nonce="`+nonce+`"`) {
		t.Fatalf("响应里的 nonce 与 CSP 不一致: nonce=%q body=%s", nonce, body)
	}

	// 每次响应都换 nonce，避免被复用到注入的页面。
	second := serve(h, http.MethodGet, "/")
	if nonceFromCSP(t, second.Header().Get("Content-Security-Policy")) == nonce {
		t.Fatal("两个响应的 nonce 不应相同")
	}
}

func nonceFromCSP(t *testing.T, csp string) string {
	t.Helper()
	const marker = "'nonce-"
	idx := strings.Index(csp, marker)
	if idx < 0 {
		t.Fatalf("CSP 中没有 nonce: %q", csp)
	}
	rest := csp[idx+len(marker):]
	end := strings.Index(rest, "'")
	if end <= 0 {
		t.Fatalf("CSP nonce 格式异常: %q", csp)
	}
	return rest[:end]
}

func TestModeInjectionCannotBreakScriptTag(t *testing.T) {
	h := newTestHandler(t, testFS(), `mock</script><script>alert(1)</script>`)
	body := serve(h, http.MethodGet, "/").Body.String()

	if strings.Contains(body, "</script><script>alert(1)") {
		t.Fatalf("模式值未被转义，注入了新的 script 标签: %s", body)
	}
	if !strings.Contains(body, `</script>`) {
		t.Fatalf("期望 < 被转义为 \\u003c，实际: %s", body)
	}
}

func TestIndexWithMalformedHead(t *testing.T) {
	fsys := fstest.MapFS{"index.html": {Data: []byte(`<html><body><div id="app"></div></body></html>`)}}
	h := newTestHandler(t, fsys, "production")
	rec := serve(h, http.MethodGet, "/")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("缺少 </head> 时应报错而不是照常返回，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="app"`) {
		t.Fatal("渲染失败时不得返回半成品页面")
	}
}

func TestStaticAssets(t *testing.T) {
	h := newTestHandler(t, testFS(), "production")

	js := serve(h, http.MethodGet, "/assets/index-abcdef12.js")
	if js.Code != http.StatusOK {
		t.Fatalf("脚本期望 200，实际 %d", js.Code)
	}
	if ct := js.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("脚本 Content-Type 应为 javascript，实际 %q", ct)
	}
	if cc := js.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("带哈希的产物应长期缓存，实际 %q", cc)
	}
	if js.Body.String() != `console.log("uPanel")` {
		t.Fatalf("响应体不符: %q", js.Body.String())
	}

	css := serve(h, http.MethodGet, "/assets/index-abcdef12.css")
	if ct := css.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("样式 Content-Type 应为 css，实际 %q", ct)
	}

	svg := serve(h, http.MethodGet, "/favicon.svg")
	if ct := svg.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("图标 Content-Type 应为 svg，实际 %q", ct)
	}
	if cc := svg.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Fatalf("非哈希资源不应长期缓存，实际 %q", cc)
	}
}

func TestSPAFallback(t *testing.T) {
	h := newTestHandler(t, testFS(), "production")

	for _, target := range []string{"/", "/storage", "/settings/network"} {
		rec := serve(h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 页面路径应回退到 index.html，实际 %d", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Fatalf("%s: 回退内容不是入口页", target)
		}
	}
}

func TestMissingAssetIsNotIndex(t *testing.T) {
	h := newTestHandler(t, testFS(), "production")

	for _, target := range []string{"/assets/missing.js", "/assets/index-abcdef12.js.map", "/v1.2"} {
		rec := serve(h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: 缺少的文件应 404，实际 %d", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="app"`) {
			t.Fatalf("%s: 不得用 index.html 冒充缺失资源", target)
		}
	}
}

func TestPathTraversalRejected(t *testing.T) {
	h := newTestHandler(t, testFS(), "production")

	for _, target := range []string{"/../secret.txt", "/assets/../../secret.txt", "/%2e%2e/secret.txt"} {
		rec := serve(h, http.MethodGet, target)
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "console.log") {
			t.Fatalf("%s: 越界路径返回了文件内容", target)
		}
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently {
			t.Fatalf("%s: 越界路径应被拒绝，实际 %d", target, rec.Code)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newTestHandler(t, testFS(), "production")
	rec := serve(h, http.MethodPost, "/")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 期望 405，实际 %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("Allow 头应为 GET, HEAD，实际 %q", allow)
	}
}

func TestHeadHasNoBody(t *testing.T) {
	h := newTestHandler(t, testFS(), "production")
	rec := serve(h, http.MethodHead, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD 期望 200，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD 不应有正文，实际 %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("HEAD 也应给 Content-Type，实际 %q", ct)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(t, testFS(), "production")

	for _, target := range []string{"/", "/assets/index-abcdef12.js", "/missing.js"} {
		rec := serve(h, http.MethodGet, target)
		header := rec.Header()
		if header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: 缺少 nosniff", target)
		}
		if header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: 缺少 referrer-policy", target)
		}
		if header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: 缺少点击劫持防护", target)
		}
	}
}

func TestEmbeddedHasPlaceholderOnlyByDefault(t *testing.T) {
	// 仓库里 internal/webui/dist 只有占位文件时，Embedded 必须如实报告不可用。
	// 若本地刚跑过 scripts/build.sh（bundle 已同步），此断言反过来检查产物完整性。
	assets := Embedded()
	if !assets.Available() {
		if !strings.Contains(assets.Reason(), "前端资源") {
			t.Fatalf("不可用原因应提到前端资源，实际 %q", assets.Reason())
		}
		return
	}
	if _, err := fs.Stat(embedded, distRoot+"/"+indexFile); err != nil {
		t.Fatalf("声称可用却读不到 index.html: %v", err)
	}
}
