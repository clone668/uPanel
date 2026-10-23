package proxies

import (
	"errors"
	"strings"
	"testing"
)

// validDefinition 返回一份形状合法的站点定义。
func validDefinition() Definition {
	return Definition{
		Domain:           "app.example.com",
		UpstreamTargetID: "local_3f2a1b",
		UpstreamPort:     8080,
	}
}

// TestNormalizeDomainAccepts 确认合法域名被归一成入库形态。
func TestNormalizeDomainAccepts(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{"app.example.com", "app.example.com", "原样"},
		{"  App.Example.COM.  ", "app.example.com", "大小写与首尾空白、尾点都归一"},
		{"食狮.com.cn", "xn--85x722f.com.cn", "中文域名转 A-label"},
		{"xn--85x722f.com.cn", "xn--85x722f.com.cn", "已经是 A-label 的不动"},
		{"a-b.example.co.uk", "a-b.example.co.uk", "连字符允许"},
		{"1.example.com", "1.example.com", "数字开头的片段允许"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := NormalizeDomain(tc.in)
			if err != nil {
				t.Fatalf("%s 应该通过：%v", tc.why, err)
			}
			if got != tc.want {
				t.Fatalf("%s：期望 %q，实际 %q", tc.why, tc.want, got)
			}
		})
	}
}

// TestNormalizeDomainRejects 确认不合法的输入被明确拒绝。
//
// 每一条都带“为什么不能接受”——域名会变成文件名和 server_name，
// 悄悄剪掉多余部分比拒绝更危险。
func TestNormalizeDomainRejects(t *testing.T) {
	cases := []struct{ in, wantMsg, why string }{
		{"", "不能为空", "空域名"},
		{"  ", "不能为空", "只有空白"},
		{"*.example.com", "通配符", "通配符暂不支持"},
		{"https://example.com", "协议", "带协议"},
		{"example.com/app", "路径", "带路径"},
		{"example.com?x=1", "查询串", "带查询串"},
		{"example.com:8080", "端口", "带端口"},
		{"user@example.com", "查询串", "带 @（不能靠剪掉它变成合法域名）"},
		{"exa mple.com", "空白", "带空格"},
		{"192.168.1.10", "IP 地址", "IP 不是域名"},
		{"exa_mple.com", "不合法", "下划线"},
		{"localhost", "至少要有一个点", "单标签名"},
		{"example.123", "顶级域不能全是数字", "数字顶级域（写错的 IP）"},
		{strings.Repeat("a", 64) + ".com", "不合法", "片段超过 63"},
		{strings.Repeat("abcdefghi.", 30) + "com", "太长", "总长度超过 253"},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			got, err := NormalizeDomain(tc.in)
			if err == nil {
				t.Fatalf("%s 应该被拒绝，实际归一成 %q", tc.why, got)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("%s：错误信息应包含 %q，实际 %q", tc.why, tc.wantMsg, err.Error())
			}
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Field != "domain" {
				t.Fatalf("%s：应是以 domain 为字段的校验错误，实际 %T", tc.why, err)
			}
		})
	}
}

// TestNormalizeFillsDefaults 确认缺省值是 nginx 自己的默认，而不是随手编的数字。
func TestNormalizeFillsDefaults(t *testing.T) {
	def := Definition{
		Domain:           "app.example.com",
		UpstreamTargetID: "local_3f2a1b",
		UpstreamPort:     8080,
	}
	got, err := def.Normalize(NormalizeOptions{PanelPort: 9443})
	if err != nil {
		t.Fatalf("应该通过：%v", err)
	}
	if got.UpstreamScheme != SchemeHTTP {
		t.Fatalf("协议默认应为 http，实际 %q", got.UpstreamScheme)
	}
	if got.AccessMode != AccessPublic {
		t.Fatalf("访问模式默认应为 public，实际 %q", got.AccessMode)
	}
	if got.MaxBodyBytes != 1<<20 {
		t.Fatalf("请求体上限默认应为 1 MiB（nginx 的 1m），实际 %d", got.MaxBodyBytes)
	}
	if got.ReadTimeoutSeconds != 60 {
		t.Fatalf("读取超时默认应为 60 秒（nginx 的默认），实际 %d", got.ReadTimeoutSeconds)
	}
	if got.WebSocketEnabled {
		t.Fatal("WebSocket 默认应关闭：它要显式选择，不能替用户打开长连接")
	}
	if len(got.AllowedCIDRs) != 0 || len(got.BasicAuthUsers) != 0 {
		t.Fatal("public 模式下不该有白名单或账号")
	}
}

// TestNormalizeRejectsPanelPort 确认管理面板自己的端口不能当上游。
func TestNormalizeRejectsPanelPort(t *testing.T) {
	def := validDefinition()
	def.UpstreamPort = 9443
	_, err := def.Normalize(NormalizeOptions{PanelPort: 9443})
	if err == nil {
		t.Fatal("9443 应该被拒绝")
	}
	if !strings.Contains(err.Error(), "管理面板自己的端口") {
		t.Fatalf("错误信息应说明是面板端口，实际 %q", err.Error())
	}
	// 不表示面板端口时（测试环境）不检查——这条规则由配置提供，不写死在代码里。
	if _, err := def.Normalize(NormalizeOptions{}); err != nil {
		t.Fatalf("未指定面板端口时不该拒绝：%v", err)
	}
}

// TestNormalizeRejectsBadUpstream 确认上游目标的形状限制。
func TestNormalizeRejectsBadUpstream(t *testing.T) {
	cases := []struct {
		target  string
		port    int
		wantMsg string
	}{
		{"", 8080, "必须指定上游目标"},
		{"127.0.0.1", 8080, "不能填主机名"},
		{"http://127.0.0.1:8080", 8080, "不能填主机名"},
		{"169.254.169.254", 8080, "不能填主机名"},
		{"local_", 8080, "不能填主机名"},
		{"cntr_0123456789ABCDEF", 8080, "不能填主机名"},
		{"local_ok", 0, "1 到 65535"},
		{"local_ok", 70000, "1 到 65535"},
	}
	for _, tc := range cases {
		def := validDefinition()
		def.UpstreamTargetID = tc.target
		def.UpstreamPort = tc.port
		_, err := def.Normalize(NormalizeOptions{})
		if err == nil {
			t.Fatalf("%q/%d 应该被拒绝", tc.target, tc.port)
		}
		if !strings.Contains(err.Error(), tc.wantMsg) {
			t.Fatalf("%q：错误信息应包含 %q，实际 %q", tc.target, tc.wantMsg, err.Error())
		}
	}
}

// TestNormalizeCIDRMode 确认白名单模式：必填、收敛成网络地址、拒绝“假装受限”。
func TestNormalizeCIDRMode(t *testing.T) {
	def := validDefinition()
	def.AccessMode = AccessCIDR

	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("cidr 模式没有网段应该被拒绝")
	}

	def.AllowedCIDRs = []string{"192.168.1.7/24", "10.0.0.0/8", "192.168.1.0/24", " 10.0.1.0/16 "}
	got, err := def.Normalize(NormalizeOptions{})
	if err != nil {
		t.Fatalf("应该通过：%v", err)
	}
	want := []string{"10.0.0.0/16", "10.0.0.0/8", "192.168.1.0/24"}
	if len(got.AllowedCIDRs) != len(want) {
		t.Fatalf("应收敛并去重为 %v，实际 %v", want, got.AllowedCIDRs)
	}
	for i := range want {
		if got.AllowedCIDRs[i] != want[i] {
			t.Fatalf("应排序去重为 %v，实际 %v", want, got.AllowedCIDRs)
		}
	}

	// 0.0.0.0/0 顶着“受限”的名字做全开放的事，拒绝比存下来好。
	def.AllowedCIDRs = []string{"0.0.0.0/0"}
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("0.0.0.0/0 应该被拒绝")
	} else if !strings.Contains(err.Error(), "public") {
		t.Fatalf("错误信息应指路 public，实际 %q", err.Error())
	}

	def.AllowedCIDRs = []string{"不是网段"}
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("非法网段应该被拒绝")
	}
}

// TestNormalizeBasicAuthMode 确认账号模式：必填、名字与密码引用都要有形状。
func TestNormalizeBasicAuthMode(t *testing.T) {
	ref := "sec_0123456789abcdef0123456789abcdef"
	def := validDefinition()
	def.AccessMode = AccessBasicAuth

	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("basic_auth 模式没有账号应该被拒绝")
	}

	// 首尾空白收掉之后名字才是有效的。
	def.BasicAuthUsers = []BasicAuthUser{{Name: " guest ", SecretRef: ref}}
	got, err := def.Normalize(NormalizeOptions{})
	if err != nil {
		t.Fatalf("应该通过：%v", err)
	}
	if len(got.BasicAuthUsers) != 1 || got.BasicAuthUsers[0].Name != "guest" {
		t.Fatalf("名字应收成 guest，实际 %+v", got.BasicAuthUsers)
	}

	// 同名账号重复是错误而不是“帮你合并”：两个同名账号在 htpasswd 里只有一个能生效，
	// 静默去重会让用户以为自己有两个账号。
	def.BasicAuthUsers = []BasicAuthUser{{Name: "guest", SecretRef: ref}, {Name: "guest", SecretRef: ref}}
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("重名账号应该被拒绝")
	}

	badCases := []struct {
		user BasicAuthUser
		why  string
	}{
		{BasicAuthUser{Name: "Guest", SecretRef: ref}, "大写账号名"},
		{BasicAuthUser{Name: "", SecretRef: ref}, "空账号名"},
		{BasicAuthUser{Name: "-x", SecretRef: ref}, "连字符开头"},
		{BasicAuthUser{Name: "a b", SecretRef: ref}, "带空格"},
		{BasicAuthUser{Name: "guest", SecretRef: ""}, "没有密码引用"},
		{BasicAuthUser{Name: "guest", SecretRef: "secret"}, "密码引用形状不对"},
		{BasicAuthUser{Name: "guest", SecretRef: "sec_XYZ"}, "秘密引用不是 32 位十六进制"},
	}
	for _, tc := range badCases {
		def.BasicAuthUsers = []BasicAuthUser{tc.user}
		if _, err := def.Normalize(NormalizeOptions{}); err == nil {
			t.Fatalf("%s 应该被拒绝", tc.why)
		}
	}
}

// TestNormalizeRejectsCrossModeFields 确认“模式说 public 却塞了白名单”这种半吊子输入被拒。
func TestNormalizeRejectsCrossModeFields(t *testing.T) {
	def := validDefinition()
	def.AccessMode = AccessPublic
	def.AllowedCIDRs = []string{"10.0.0.0/8"}
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("public 模式不该接受白名单")
	}

	def = validDefinition()
	def.AccessMode = AccessCIDR
	def.AllowedCIDRs = []string{"10.0.0.0/8"}
	def.BasicAuthUsers = []BasicAuthUser{{Name: "guest", SecretRef: "sec_0123456789abcdef0123456789abcdef"}}
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("cidr 模式不该接受账号：库里只允许一种访问模式有内容")
	}
}

// TestNormalizeRanges 确认两个数值字段的边界。
func TestNormalizeRanges(t *testing.T) {
	for _, tc := range []struct {
		body    int64
		timeout int
		why     string
	}{
		{1, 60, "请求体小于 1 KiB"},
		{1 << 31, 60, "请求体大于 1 GiB"},
		{1 << 20, 0, "读取超时为 0（会被填默认值，不该报错）"},
		{1 << 20, 3601, "读取超时大于 3600"},
		{1 << 20, -1, "读取超时为负"},
	} {
		def := validDefinition()
		def.MaxBodyBytes = tc.body
		def.ReadTimeoutSeconds = tc.timeout
		_, err := def.Normalize(NormalizeOptions{})
		if tc.timeout == 0 {
			if err != nil {
				t.Fatalf("%s：不该报错，实际 %v", tc.why, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s 应该被拒绝", tc.why)
		}
	}
}
