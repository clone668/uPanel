package httpx

import "testing"

func TestNormalizeOrigin(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://127.0.0.1:9443", "http://127.0.0.1:9443"},
		{"http://127.0.0.1:9443/", "http://127.0.0.1:9443"},
		{"HTTP://LOCALHOST:9443", "http://localhost:9443"},
		{"https://panel.example.com", "https://panel.example.com"},
		{"https://panel.example.com:443", "https://panel.example.com"},
		{"http://panel.example.com:80", "http://panel.example.com"},
		{"  http://127.0.0.1:18881  ", "http://127.0.0.1:18881"},
		{"http://[::1]:9443", "http://[::1]:9443"},
		{"https://面板.example.com", "https://面板.example.com"},
	}
	for _, c := range cases {
		got, err := NormalizeOrigin(c.in)
		if err != nil {
			t.Fatalf("NormalizeOrigin(%q) 报错: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("NormalizeOrigin(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeOriginRejects(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"127.0.0.1:9443",           // 没有协议
		"ftp://127.0.0.1",          // 非 http(s)
		"javascript:alert(1)",      // 伪协议
		"http://user:pass@host:80", // 带 userinfo
		"http://127.0.0.1:9443/path",
		"http://127.0.0.1:9443?x=1",
		"http://127.0.0.1:9443#frag",
		"http:///nohost",
		"http://127.0.0.1:99999",
		"null",
	}
	for _, in := range cases {
		if got, err := NormalizeOrigin(in); err == nil {
			t.Fatalf("NormalizeOrigin(%q) 应报错，实际得到 %q", in, got)
		}
	}
}

func TestOriginSetContains(t *testing.T) {
	set, err := NewOriginSet([]string{
		"http://127.0.0.1:9443",
		"HTTP://127.0.0.1:9443/", // 重复项，规范化后去重
		"https://panel.example.com",
	})
	if err != nil {
		t.Fatalf("构造 OriginSet 失败: %v", err)
	}
	list := set.List()
	if len(list) != 2 {
		t.Fatalf("去重后应有 2 项，实际 %v", list)
	}

	yes := []string{"http://127.0.0.1:9443", "http://127.0.0.1:9443/", "HTTPS://PANEL.EXAMPLE.COM"}
	for _, origin := range yes {
		if !set.Contains(origin) {
			t.Fatalf("%q 应在允许集合内", origin)
		}
	}
	no := []string{
		"",
		"http://127.0.0.1:9444",             // 端口不同
		"https://127.0.0.1:9443",            // 协议不同
		"http://evil.example.com",           // 完全不同的源
		"http://127.0.0.1:9443.evil.com",    // 后缀混淆
		"http://panel.example.com",          // 少了 https
		"http://127.0.0.1:9443, http://x.y", // 拼接头
		"null",
	}
	for _, origin := range no {
		if set.Contains(origin) {
			t.Fatalf("%q 不应在允许集合内", origin)
		}
	}
}

func TestOriginSetEmptyMatchesNothing(t *testing.T) {
	var nilSet *OriginSet
	if !nilSet.Empty() {
		t.Fatal("nil 集合应视为空")
	}
	if nilSet.Contains("http://127.0.0.1:9443") {
		t.Fatal("nil 集合不应匹配任何源")
	}

	empty, err := NewOriginSet(nil)
	if err != nil {
		t.Fatalf("空输入不应报错: %v", err)
	}
	if !empty.Empty() || empty.Contains("http://127.0.0.1:9443") {
		t.Fatal("空集合不应匹配任何源")
	}
}

func TestNewOriginSetRejectsInvalidEntry(t *testing.T) {
	if _, err := NewOriginSet([]string{"http://127.0.0.1:9443", "not-a-url"}); err == nil {
		t.Fatal("含非法项时必须整体报错，而不是跳过该项")
	}
}
