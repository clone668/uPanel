package nginx

import (
	"strings"
	"testing"
)

// TestParseConfigReadsRealShape 用一份形状真实的配置走一遍解析器。
//
// 这份配置抄的是 Ubuntu 上 nginx.conf 的样子：main 上下文里 include 模块，http 块里
// include 目录，server 块里有 listen、server_name、location，还夹着注释和引号。
// 解析器要能把这些原样分开，而不是把它们当成一串词。
func TestParseConfigReadsRealShape(t *testing.T) {
	const src = `# 顶层注释
user www-data;
worker_processes auto;
include /etc/nginx/modules-enabled/*.conf;

events {
    worker_connections 768;
}

http {
    include /etc/nginx/mime.types;
    sendfile on;

    server {
        listen 80 default_server;
        listen [::]:80 default_server;
        server_name _;
        location / {
            return 404;
        }
    }

    server {
        listen 443 ssl;
        server_name example.com www.example.com;
        ssl_certificate "/etc/letsencrypt/live/example.com/fullchain.pem";
        location /api/ {
            proxy_pass http://127.0.0.1:8080;
        }
    }
}
`
	nodes, err := parseConfig(src)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(nodes) != 5 {
		t.Fatalf("顶层指令应有 5 条，实际 %d 条", len(nodes))
	}
	if nodes[0].Name != "user" || len(nodes[0].Args) != 1 || nodes[0].Args[0] != "www-data" {
		t.Fatalf("第一条指令解析错了：%+v", nodes[0])
	}
	if nodes[0].Line != 2 {
		t.Fatalf("user 指令应在第 2 行，实际 %d", nodes[0].Line)
	}

	var http *directive
	for i := range nodes {
		if nodes[i].Name == "http" {
			http = &nodes[i]
		}
	}
	if http == nil || !http.HasBlock {
		t.Fatal("没找到 http 块")
	}

	var servers []directive
	for _, child := range http.Block {
		if child.Name == "server" {
			servers = append(servers, child)
		}
	}
	if len(servers) != 2 {
		t.Fatalf("http 里应有 2 个 server 块，实际 %d 个", len(servers))
	}

	// 第二个 server 的 server_name 是两个名字，引号里的证书路径不能被空格切开。
	var names []string
	var certs []string
	for _, child := range servers[1].Block {
		switch child.Name {
		case "server_name":
			names = child.Args
		case "ssl_certificate":
			certs = child.Args
		}
	}
	if len(names) != 2 || names[0] != "example.com" || names[1] != "www.example.com" {
		t.Fatalf("server_name 解析错了：%v", names)
	}
	if len(certs) != 1 || certs[0] != "/etc/letsencrypt/live/example.com/fullchain.pem" {
		t.Fatalf("带引号的参数解析错了：%v", certs)
	}
}

// TestParseConfigRejectsBrokenInput 确认畸形配置是明确的失败，而不是半棵树。
//
// 半个配置树比没有更危险：它看起来像全的，冲突判定会据此说“这个域名没人用”。
func TestParseConfigRejectsBrokenInput(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"缺分号", "http { server_name a.com }", "指令没有结束"},
		{"块没闭合", "http {\n  server {\n    listen 80;\n  }\n", "块没有闭合"},
		{"多余的右括号", "server {\n  listen 80;\n}\n}\n", "多余的 }"},
		{"引号没闭合", "server_name \"a.com;\n", "引号没有闭合"},
		{"嵌套太深", strings.Repeat("a {", 40) + strings.Repeat("}", 40), "嵌套太深"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := parseConfig(tc.src)
			if err == nil {
				t.Fatalf("应该报错，实际解析出 %d 条指令", len(nodes))
			}
			if nodes != nil {
				t.Fatalf("出错时不应返回指令树，实际返回 %d 条", len(nodes))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应包含 %q，实际 %q", tc.want, err.Error())
			}
		})
	}
}

// TestParseConfigIgnoresCommentsAndBlankSpace 确认注释与空白不会变成参数。
func TestParseConfigIgnoresCommentsAndBlankSpace(t *testing.T) {
	nodes, err := parseConfig("\n\n# 只有注释\n\t  server_name\ta.com\t;\n# 尾巴\n")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("应有 1 条指令，实际 %d 条", len(nodes))
	}
	if nodes[0].Name != "server_name" || len(nodes[0].Args) != 1 || nodes[0].Args[0] != "a.com" {
		t.Fatalf("解析错了：%+v", nodes[0])
	}
}

// TestClassifyNames 确认各种 server_name 写法被定的类和归一化的值。
func TestClassifyNames(t *testing.T) {
	cases := []struct {
		raw    string
		kind   NameKind
		host   string
		errCmt string
	}{
		{"example.com", NameExact, "example.com", "普通域名"},
		{"Example.COM.", NameExact, "example.com", "大写与尾点都归一化"},
		{"127.0.0.1", NameExact, "127.0.0.1", "IPv4 是普通主机名"},
		{"*.example.com", NameWildcard, "example.com", "子域通配"},
		{".example.com", NameWildcardApex, "example.com", "带自己的通配"},
		{"~^www\\d+\\.example\\.com$", NameRegex, "", "正则"},
		{"_", NamePlaceholder, "", "占位名"},
		{"www.example.*", NameWildcardTail, "www.example", "尾部通配"},
		{"exa mple.com", NameOther, "", "空格不是主机名的一部分"},
		{"-bad.example.com", NameExact, "-bad.example.com", "连字符开头仍然是被写下的名字"},
		{"", NameOther, "", "空名"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := Classify(tc.raw)
			if got.Kind != tc.kind || got.Host != tc.host {
				t.Fatalf("%s：期望 %s/%q，实际 %s/%q（%s）",
					tc.raw, tc.kind, tc.host, got.Kind, got.Host, tc.errCmt)
			}
			if got.Raw != tc.raw {
				t.Fatalf("原文应原样保留，实际 %q", got.Raw)
			}
		})
	}
}

// TestVerdict 确认判定语义：谁占住了域名、谁只是路过、谁判不了。
func TestVerdict(t *testing.T) {
	cases := []struct {
		raw  string
		host string
		want Verdict
	}{
		{"example.com", "example.com", Claimed},
		{"example.com", "www.example.com", NotClaimed},
		{"*.example.com", "www.example.com", Claimed},
		{"*.example.com", "a.b.example.com", Claimed},
		{"*.example.com", "example.com", NotClaimed}, // nginx 的 *. 不含裸域
		{".example.com", "example.com", Claimed},     // nginx 的 . 含裸域
		{".example.com", "www.example.com", Claimed},
		{"*.example.com", "notexample.com", NotClaimed}, // 后缀拼接必须带点，不能靠字符串尾匹配
		{"~^www\\d+\\.example\\.com$", "example.com", Undecidable},
		{"www.example.*", "www.example.com", Claimed},
		{"www.example.*", "example.com", NotClaimed},
		{"www.example.*", "www.example.co.uk", Claimed},
		{"_", "example.com", NotClaimed},
	}
	for _, tc := range cases {
		t.Run(tc.raw+"→"+tc.host, func(t *testing.T) {
			if got := Classify(tc.raw).Verdict(tc.host); got != tc.want {
				t.Fatalf("%s 对 %s 的判定应为 %d，实际 %d", tc.raw, tc.host, tc.want, got)
			}
		})
	}
}

// TestParseListen 确认 listen 的地址、端口与两个关键参数。
func TestParseListen(t *testing.T) {
	cases := []struct {
		args    []string
		address string
		port    int
		def     bool
		ssl     bool
	}{
		{nil, "*", 80, false, false},
		{[]string{"80"}, "*", 80, false, false},
		{[]string{"80", "default_server"}, "*", 80, true, false},
		{[]string{"443", "ssl"}, "*", 443, false, true},
		{[]string{"127.0.0.1:8080"}, "127.0.0.1", 8080, false, false},
		{[]string{"[::]:443", "ssl", "default_server"}, "::", 443, true, true},
		{[]string{"localhost:9000"}, "localhost", 9000, false, false},
		{[]string{"unix:/run/nginx.sock"}, "unix:/run/nginx.sock", 0, false, false},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			got := ParseListen(tc.args)
			if got.Address != tc.address || got.Port != tc.port || got.DefaultServer != tc.def || got.SSL != tc.ssl {
				t.Fatalf("解析错了：地址 %q 端口 %d default=%v ssl=%v",
					got.Address, got.Port, got.DefaultServer, got.SSL)
			}
		})
	}
}

// TestIsFollowedInclude 确认哪些 include 算已经扫过、哪些要报成漏网。
func TestIsFollowedInclude(t *testing.T) {
	cases := map[string]bool{
		"/etc/nginx/conf.d/*.conf":          true,
		"conf.d/*.conf":                     true,
		"/etc/nginx/sites-enabled/*":        true,
		"sites-enabled/*":                   true,
		"/etc/nginx/mime.types":             false,
		"/etc/nginx/modules-enabled/*.conf": false,
		"snippets/ssl.conf":                 false,
		"/opt/app/nginx/*.conf":             false,
	}
	for target, want := range cases {
		if got := isFollowedInclude(target); got != want {
			t.Fatalf("%s：应算%s，实际%s", target, map[bool]string{true: "已扫过", false: "漏网"}[want],
				map[bool]string{true: "已扫过", false: "漏网"}[got])
		}
	}
}

// TestManagedFileName 确认面板文件名的生成与拒绝规则。
func TestManagedFileName(t *testing.T) {
	name, err := ManagedFileName("example.com")
	if err != nil || name != "upanel-example.com.conf" {
		t.Fatalf("应为 upanel-example.com.conf，实际 %q err=%v", name, err)
	}
	if !IsManagedName(name) {
		t.Fatal("自己生成的名字应该被认成面板文件")
	}
	// 域名不能把路径分隔符带进文件名。
	for _, bad := range []string{"../etc/passwd", "a/b.com", "", ".", "..", "a..b"} {
		if _, err := ManagedFileName(bad); err == nil {
			t.Fatalf("%q 不该生成文件名", bad)
		}
	}
	if IsManagedName("example.com.conf") {
		t.Fatal("没有 upanel- 前缀的文件不是面板的")
	}
	if IsManagedName("upanel-x.txt") {
		t.Fatal("upanel- 开头但不是 .conf 的文件不是面板的")
	}
}
