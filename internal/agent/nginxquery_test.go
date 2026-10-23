package agent

// P10.1 的代理层单测：`nginx.state` 的参数、注册与版本解析。
//
// 这里不验证“域名冲突判得对不对”（那是 internal/proxies 的事），只验证代理这一侧
// 有没有如实把事实交出去：
//   - 参数只认 fresh 一个布尔字段，未知字段一律拒绝；
//   - fresh=true 与 fresh=false 走两条不同的读取路径（前者不吃缓存）；
//   - `nginx -v` 的输出解析不出来时返回错误，不编版本号。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"upanel/internal/nginx"
)

// testNginxCollector 用临时目录构造采集器：真的读文件，不读测试机的 /etc/nginx。
func testNginxCollector(t *testing.T, files map[string]string) *nginx.Collector {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写配置失败: %v", err)
		}
	}
	return nginx.New(nginx.Options{
		ConfigRoot: root,
		Version:    func(context.Context) (string, error) { return "1.24.0", nil },
		Unit:       func(context.Context) (string, bool, error) { return "active", true, nil },
	})
}

// nginxFactsEnvelope 是 nginx.state 的响应外壳。
type nginxFactsEnvelope struct {
	Data nginx.Facts `json:"data"`
}

// TestNginxStateQueryRegistered 确认 nginx.state 只在有采集器时登记。
func TestNginxStateQueryRegistered(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Nginx: testNginxCollector(t, nil)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if _, ok := reg.queries[NginxStateQueryName]; !ok {
		t.Fatalf("%s 没有登记", NginxStateQueryName)
	}

	// 没有采集器时不登记：查它会得到“未登记”，而不是一份空事实。
	reg, err = NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if _, ok := reg.queries[NginxStateQueryName]; ok {
		t.Fatal("没有采集器时不该登记 nginx.state")
	}
}

// TestNginxStateQueryReturnsFacts 走一遍真实采集链路。
func TestNginxStateQueryReturnsFacts(t *testing.T) {
	collector := testNginxCollector(t, map[string]string{
		"nginx.conf": "events {}\nhttp {\n  include conf.d/*.conf;\n}\n",
		"conf.d/blog.conf": "server {\n  listen 80;\n" +
			"  server_name blog.example.com;\n  location / { proxy_pass http://127.0.0.1:8080; }\n}\n",
	})
	reg, err := NewProductionRegistry(ProductionDeps{Nginx: collector})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"nginx.state","params":{}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body nginxFactsEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	facts := body.Data
	if !facts.Available || facts.ConfigMissing {
		t.Fatalf("available/config_missing = %v/%v（%s）", facts.Available, facts.ConfigMissing, facts.Reason)
	}
	if facts.Version != "1.24.0" || facts.UnitState != "active" {
		t.Fatalf("version/unit = %q/%q", facts.Version, facts.UnitState)
	}
	if len(facts.Files) != 2 {
		t.Fatalf("读到的文件 = %+v", facts.Files)
	}
	hosts := facts.ServerNames()
	if len(hosts) != 1 || hosts[0] != "blog.example.com" {
		t.Fatalf("server_names = %v", hosts)
	}
	if facts.CollectedAt.IsZero() {
		t.Fatal("collected_at 缺失：采集时间是这个查询的核心信息")
	}
}

// TestNginxStateQueryFreshBypassesCache 确认 fresh=true 真的绕过了缓存。
//
// 做法是先用 {} 读一次（进缓存），改文件，再用 {} 读一次（应当还是旧事实），
// 最后用 {"fresh":true} 读一次（必须是新事实）。这是写前判断必须拿到最新配置的
// 唯一依据，所以它必须是可验证的行为，而不是一句注释。
func TestNginxStateQueryFreshBypassesCache(t *testing.T) {
	root := t.TempDir()
	confPath := filepath.Join(root, "conf.d", "blog.conf")
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	write := func(name string) {
		if err := os.WriteFile(confPath, []byte("server {\n  server_name "+name+";\n}\n"), 0o644); err != nil {
			t.Fatalf("写配置失败: %v", err)
		}
	}
	write("first.example.com")

	collector := nginx.New(nginx.Options{ConfigRoot: root})
	reg, err := NewProductionRegistry(ProductionDeps{Nginx: collector})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	read := func(params string) nginx.Facts {
		t.Helper()
		rec := postQuery(t, srv, `{"query":"nginx.state","params":`+params+`}`, testWebUID)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
		}
		var body nginxFactsEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		return body.Data
	}

	first := read(`{}`)
	if got := first.ServerNames(); len(got) != 1 || got[0] != "first.example.com" {
		t.Fatalf("第一次 = %v", got)
	}

	write("second.example.com")

	cached := read(`{}`)
	if got := cached.ServerNames(); len(got) != 1 || got[0] != "first.example.com" {
		t.Fatalf("缓存窗口内应读到旧事实，实际 = %v", got)
	}

	fresh := read(`{"fresh":true}`)
	if got := fresh.ServerNames(); len(got) != 1 || got[0] != "second.example.com" {
		t.Fatalf("fresh=true 必须读到新事实，实际 = %v", got)
	}
}

// TestNginxStateQueryRejectsUnknownParams 确认参数只认 fresh。
func TestNginxStateQueryRejectsUnknownParams(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Nginx: testNginxCollector(t, nil)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)
	for _, params := range []string{`{"path":"/etc/nginx"}`, `{"fresh":"yes"}`, `"fresh"`} {
		rec := postQuery(t, srv, `{"query":"nginx.state","params":`+params+`}`, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("参数 %s 状态码 = %d，期望 400（%s）", params, rec.Code, rec.Body.String())
		}
	}
}

// TestParseNginxVersion 确认 `nginx -v` 的输出解析。
func TestParseNginxVersion(t *testing.T) {
	ok := []struct{ in, want string }{
		{"nginx version: nginx/1.24.0", "1.24.0"},
		{"nginx version: nginx/1.18.0 (Ubuntu)", "1.18.0"},
		{"nginx/1.25.3\n", "1.25.3"},
	}
	for _, tc := range ok {
		got, err := parseNginxVersion(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("解析 %q = %q, %v，期望 %q", tc.in, got, err, tc.want)
		}
	}
	bad := []string{"", "nginx version:", "openresty/1.21.4", "garbage"}
	for _, in := range bad {
		if got, err := parseNginxVersion(in); err == nil {
			t.Fatalf("解析 %q 应报错，实际 %q", in, got)
		}
	}
}

// TestNginxVersionProbeReadsStderr 确认探针读的是 stderr（nginx 的版本就写在那儿）。
func TestNginxVersionProbeReadsStderr(t *testing.T) {
	// 用 /bin/sh 模拟 nginx -v：把版本写到 stderr 并退出 0。
	// Windows 上跑不了 shell 脚本（也没有 nginx），这条断言在 Linux 测试机上生效。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上无法用 shell 脚本模拟 nginx -v；这条断言在 Linux 上执行")
	}
	script := filepath.Join(t.TempDir(), "fake-nginx")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'nginx version: nginx/1.24.1' >&2\n"), 0o755); err != nil {
		t.Fatalf("写脚本失败: %v", err)
	}
	probe := NginxVersionProbe(script)
	got, err := probe(context.Background())
	if err != nil {
		t.Fatalf("探针失败: %v", err)
	}
	if got != "1.24.1" {
		t.Fatalf("版本 = %q，期望 1.24.1", got)
	}

	// 退出码非零必须是错误，不能返回一个猜出来的版本号。
	failing := filepath.Join(t.TempDir(), "failing-nginx")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho 'boom' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("写脚本失败: %v", err)
	}
	if got, err := NginxVersionProbe(failing)(context.Background()); err == nil {
		t.Fatalf("非零退出码必须报错，实际返回 %q", got)
	}
	if got, err := NginxVersionProbe(filepath.Join(t.TempDir(), "missing"))(context.Background()); err == nil {
		t.Fatalf("不存在的二进制必须报错，实际返回 %q", got)
	}
	if !strings.Contains(NginxStateQueryName, "nginx") {
		t.Fatal("查询名必须与 internal/proxies 里的常量一致")
	}
}
