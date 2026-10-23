package proxies

// P10.2 渲染层用例。
//
// 渲染是整条写入流水线的幂等判据：同样的定义必须渲染出同样的字节，否则“没变就不写、
// 不备份、不 reload”这条规则会退化成“每次都写一遍、每次都 reload”。所以这里断言的
// 不是“配置里大概有这些行”，而是**逐字节稳定**与**该有的行都在、不该有的行都不在**。
//
// 另一半同样重要：渲染出去的东西会变成 root 拥有的 nginx 配置。它只从已经过形状
// 校验的字段拼字符串，不接受任何“用户提供的配置片段”——下面每一条拒绝用例都在守
// 这条边界。

import (
	"strings"
	"testing"

	"upanel/internal/tlscert"
)

// renderableDefinition 返回一份能渲染的完整定义（public 模式）。
func renderableDefinition() Definition {
	return Definition{
		Domain:             "app.example.com",
		UpstreamTargetID:   "local_3f2a1b",
		UpstreamScheme:     SchemeHTTP,
		UpstreamPort:       8080,
		AccessMode:         AccessPublic,
		MaxBodyBytes:       DefaultMaxBodyBytes,
		ReadTimeoutSeconds: DefaultReadTimeoutSeconds,
		Enabled:            true,
	}
}

const renderTestSiteID = "site_0123456789abcdef0123456789abcdef"

// TestRenderSiteIsByteStable 同样输入两次渲染必须一字不差。
func TestRenderSiteIsByteStable(t *testing.T) {
	def := renderableDefinition()
	first, err := RenderSite(renderTestSiteID, def, RenderOptions{})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	second, err := RenderSite(renderTestSiteID, def, RenderOptions{})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if first != second {
		t.Fatal("同样定义渲染出不同字节：幂等判断会因此失效")
	}
	// 时间戳、随机盐、自增序号都不该出现——它们会让每次渲染都不同。
	for _, banned := range []string{"2024", "2025", "2026", "time.Now"} {
		if strings.Contains(first, banned) {
			t.Errorf("渲染结果里出现了会随时间变化的内容 %q", banned)
		}
	}
}

// TestRenderSiteStructure 逐行核对 public 模式的产物。
func TestRenderSiteStructure(t *testing.T) {
	out, err := RenderSite(renderTestSiteID, renderableDefinition(), RenderOptions{})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	lines := strings.Split(out, "\n")

	// 第一行是标记：清理改名遗留文件、以及“这是面板写的”都靠它。
	if !strings.HasPrefix(lines[0], ManagedMarker+renderTestSiteID) {
		t.Fatalf("第一行必须是托管标记，实际 %q", lines[0])
	}
	must := []string{
		"server {",
		"    listen 80;",
		"    listen [::]:80;",
		"    server_name app.example.com;",
		"    access_log /var/log/nginx/upanel-app.example.com.access.log;",
		"    client_max_body_size 1m;",
		"        proxy_pass http://127.0.0.1:8080;",
		"        proxy_http_version 1.1;",
		"        proxy_set_header Host $host;",
		"        proxy_set_header X-Real-IP $remote_addr;",
		"        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;",
		"        proxy_set_header X-Forwarded-Proto $scheme;",
		"        proxy_connect_timeout 5s;",
		"        proxy_read_timeout 60s;",
		"        proxy_send_timeout 60s;",
	}
	for _, want := range must {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("缺少这一行：%q", want)
		}
	}
	// public 模式既没有网段，也没有账号。
	for _, banned := range []string{"allow ", "deny all", "auth_basic", "Upgrade"} {
		if strings.Contains(out, banned) {
			t.Errorf("public 模式不该出现 %q", banned)
		}
	}
	// 监听端口是常量，不是参数：这里永远只能看到 80（P10.3 才加 443）。
	if strings.Contains(out, "listen 9443") || strings.Contains(out, "listen 443") {
		t.Error("托管站点只能监听 80：管理面板的端口不得成为任何 server 块的监听端口")
	}
	if ManagedListenPort != 80 {
		t.Fatalf("ManagedListenPort = %d，P10.2 只监听 80", ManagedListenPort)
	}
	// 括号必须配对——这是“生成的东西至少是完整的”。
	if strings.Count(out, "{") != strings.Count(out, "}") {
		t.Errorf("花括号不配对：{ %d 个，} %d 个", strings.Count(out, "{"), strings.Count(out, "}"))
	}
}

// TestRenderSiteWebSocketHeaders 只有开启时才有 Upgrade/Connection，且成对出现。
func TestRenderSiteWebSocketHeaders(t *testing.T) {
	def := renderableDefinition()
	def.WebSocketEnabled = true
	out, err := RenderSite(renderTestSiteID, def, RenderOptions{})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	for _, want := range []string{
		"        proxy_set_header Upgrade $http_upgrade;\n",
		"        proxy_set_header Connection \"upgrade\";\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q", want)
		}
	}
	// 只给 Upgrade 不给 Connection，握手会以 400 结束；反过来也一样。
	if strings.Count(out, "Upgrade") != 1 || strings.Count(out, "Connection") != 1 {
		t.Errorf("Upgrade/Connection 必须各出现一次：%q", out)
	}
	// map 只能在 http 上下文里写，托管文件是 conf.d 的 server 块，不能用它。
	if strings.Contains(out, "map ") {
		t.Error("托管文件里不能出现 map 指令")
	}
}

// TestRenderSiteCIDRAccess 网段白名单按给定顺序写，末尾必须有 deny all。
func TestRenderSiteCIDRAccess(t *testing.T) {
	def := renderableDefinition()
	def.AccessMode = AccessCIDR
	def.AllowedCIDRs = []string{"10.0.0.0/8", "192.168.1.0/24"}
	out, err := RenderSite(renderTestSiteID, def, RenderOptions{})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	first := strings.Index(out, "    allow 10.0.0.0/8;\n")
	second := strings.Index(out, "    allow 192.168.1.0/24;\n")
	deny := strings.Index(out, "    deny all;\n")
	if first < 0 || second < 0 || deny < 0 {
		t.Fatalf("allow/deny 行不完整：\n%s", out)
	}
	if !(first < second && second < deny) {
		t.Error("allow 必须按定义顺序排列，deny all 必须在最后（否则后面的 allow 永远匹配不到）")
	}
	if strings.Contains(out, "auth_basic") {
		t.Error("cidr 模式不该出现 auth_basic")
	}
}

// TestRenderSiteBasicAuthAccess 账号模式引用绝对路径的账号文件。
func TestRenderSiteBasicAuthAccess(t *testing.T) {
	def := renderableDefinition()
	def.AccessMode = AccessBasicAuth
	def.BasicAuthUsers = []BasicAuthUser{
		{Name: "alice", SecretRef: "sec_0123456789abcdef0123456789abcdef"},
	}
	const authPath = "/etc/nginx/upanel-auth/site_0123456789abcdef0123456789abcdef.htpasswd"
	out, err := RenderSite(renderTestSiteID, def, RenderOptions{HtpasswdPath: authPath})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(out, "    auth_basic \"uPanel 受保护站点\";\n") {
		t.Errorf("缺少 auth_basic：\n%s", out)
	}
	if !strings.Contains(out, "    auth_basic_user_file "+authPath+";\n") {
		t.Errorf("缺少 auth_basic_user_file：\n%s", out)
	}
	if strings.Contains(out, "deny all") || strings.Contains(out, "allow ") {
		t.Error("basic_auth 模式不该同时带网段规则")
	}
	// 密码是哈希，不是任何形式的明文——配置文件里不该出现账号名以外的账号细节。
	for _, u := range def.BasicAuthUsers {
		if strings.Contains(out, u.Name+":") {
			t.Errorf("配置里不该出现 %q 这样的 htpasswd 行", u.Name)
		}
		if strings.Contains(out, u.SecretRef) {
			t.Error("配置里不该出现秘密引用")
		}
	}
}

// TestRenderSiteRejectsUnrenderable 输入形状不对时必须拒绝，而不是渲染出坏配置。
func TestRenderSiteRejectsUnrenderable(t *testing.T) {
	cases := []struct {
		name   string
		siteID string
		mutate func(*Definition)
		auth   string
	}{
		{name: "站点标识形状", siteID: "site_XYZ"},
		{name: "站点标识大写", siteID: "site_0123456789ABCDEF0123456789abcdef"},
		{name: "域名为空", mutate: func(d *Definition) { d.Domain = "" }},
		{name: "域名带路径", mutate: func(d *Definition) { d.Domain = "../etc/passwd" }},
		{name: "域名带通配符", mutate: func(d *Definition) { d.Domain = "*.example.com" }},
		{name: "上游端口为零", mutate: func(d *Definition) { d.UpstreamPort = 0 }},
		{name: "上游端口越界", mutate: func(d *Definition) { d.UpstreamPort = 65536 }},
		{name: "上游协议不是 http/https", mutate: func(d *Definition) { d.UpstreamScheme = "gopher" }},
		{name: "请求体上限为零", mutate: func(d *Definition) { d.MaxBodyBytes = 0 }},
		{name: "读取超时为零", mutate: func(d *Definition) { d.ReadTimeoutSeconds = 0 }},
		{name: "访问模式不认识", mutate: func(d *Definition) { d.AccessMode = "ip_allowlist" }},
		{name: "public 带网段", mutate: func(d *Definition) {
			d.AllowedCIDRs = []string{"10.0.0.0/8"}
		}},
		{name: "cidr 没有网段", mutate: func(d *Definition) { d.AccessMode = AccessCIDR }},
		{name: "basic_auth 没有账号", mutate: func(d *Definition) { d.AccessMode = AccessBasicAuth }},
		{name: "basic_auth 没有账号文件路径", mutate: func(d *Definition) {
			d.AccessMode = AccessBasicAuth
			d.BasicAuthUsers = []BasicAuthUser{{Name: "alice", SecretRef: "sec_0123456789abcdef0123456789abcdef"}}
		}},
		{name: "账号文件路径是相对路径", mutate: func(d *Definition) {
			d.AccessMode = AccessBasicAuth
			d.BasicAuthUsers = []BasicAuthUser{{Name: "alice", SecretRef: "sec_0123456789abcdef0123456789abcdef"}}
		}, auth: "upanel-auth/x.htpasswd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			siteID := tc.siteID
			if siteID == "" {
				siteID = renderTestSiteID
			}
			def := renderableDefinition()
			if tc.mutate != nil {
				tc.mutate(&def)
			}
			out, err := RenderSite(siteID, def, RenderOptions{HtpasswdPath: tc.auth})
			if err == nil {
				t.Fatalf("非法输入却渲染出了配置：\n%s", out)
			}
			if out != "" {
				t.Errorf("拒绝时必须返回空内容，实际 %q", out)
			}
		})
	}
}

// TestSizeSpec 把字节数写成让人一眼能核对的 nginx 写法。
func TestSizeSpec(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{1024, "1k"},
		{1 << 20, "1m"},
		{8 << 20, "8m"},
		{1 << 30, "1g"},
		{1536, "1536"}, // 不能整除时退回字节数，同样合法
		{1000, "1000"},
	}
	for _, tc := range cases {
		if got := sizeSpec(tc.in); got != tc.want {
			t.Errorf("sizeSpec(%d) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestManagedFileNames 文件名规则：域名拼进 conf.d，站点 id 拼进 auth 目录。
func TestManagedFileNames(t *testing.T) {
	name, err := ManagedFileNameOf("app.example.com")
	if err != nil {
		t.Fatalf("合法域名被拒: %v", err)
	}
	if name != "upanel-app.example.com.conf" {
		t.Errorf("托管文件名 = %q", name)
	}
	if _, err := ManagedFileNameOf("../passwd"); err == nil {
		t.Error("带路径的域名必须被拒绝：它会写到 conf.d 之外")
	}
	if got := AuthFileNameOf(renderTestSiteID); got != renderTestSiteID+".htpasswd" {
		t.Errorf("账号文件名 = %q", got)
	}
}

// TestRenderSiteDomainCannotEscapeConfDir 域名里的任何路径成分都不能让路径跳出 conf.d。
func TestRenderSiteDomainCannotEscapeConfDir(t *testing.T) {
	for _, domain := range []string{
		"../../../etc/cron.d/x",
		"a/b.example.com",
		"app.example.com;",
		"app.example.com\nuser root;",
	} {
		def := renderableDefinition()
		def.Domain = domain
		if _, err := ManagedFileNameOf(def.Domain); err == nil {
			t.Errorf("域名 %q 竟然能当文件名用", domain)
		}
		if out, err := RenderSite(renderTestSiteID, def, RenderOptions{}); err == nil {
			t.Errorf("域名 %q 竟然能渲染出配置：\n%s", domain, out)
		}
	}
}

// ---- P10.3：TLS 与 ACME 校验位置 ----

// renderTestMaterial 返回一份指向测试路径的证书材料。
func renderTestMaterial() *TLSMaterial {
	return &TLSMaterial{
		CertificateID: "crt_0123456789abcdef0123456789abcdef",
		Source:        CertSourceManual,
		Domain:        "app.example.com",
		CertPath:      "/etc/ssl/upanel/app.example.com.pem",
		KeyPath:       "/etc/ssl/upanel/app.example.com.key",
		Fingerprint:   strings.Repeat("ab", 32),
		Issuer:        "uPanel 测试 CA",
		NotAfter:      "2030-01-02T03:04:05Z",
	}
}

// tlsRenderableDefinition 返回一份绑定了证书的定义。
func tlsRenderableDefinition() Definition {
	def := renderableDefinition()
	def.CertificateID = "crt_0123456789abcdef0123456789abcdef"
	return def
}

// TestRenderSiteWithoutCertificateListensOn80Only 没有绑证书时只有 80 这一块。
//
// 这一条是“不假装有 TLS”的判据：只要定义里没有证书标识，产物里就不该出现
// 任何 ssl_ 指令，也不该出现 443——一个没有证书却写着 443 的 server 块会让
// nginx -t 直接失败，而用户看到的是“我什么都没改就坏了”。
func TestRenderSiteWithoutCertificateListensOn80Only(t *testing.T) {
	out, err := RenderSite(renderTestSiteID, renderableDefinition(), RenderOptions{})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if strings.Contains(out, "listen 443") || strings.Contains(out, "ssl_certificate") {
		t.Errorf("没有证书却渲染出了 TLS：\n%s", out)
	}
	if strings.Count(out, "server {") != 1 {
		t.Errorf("应该只有一个 server 块：\n%s", out)
	}
	// ACME 校验位置无论有没有证书都要有：续期发生在证书已经存在的站点上，
	// 但首签发生在它还没有证书的时候（那时站点只监听 80）。
	if !strings.Contains(out, "    location ^~ /.well-known/acme-challenge/ {\n") {
		t.Errorf("缺少 ACME 校验位置：\n%s", out)
	}
	if !strings.Contains(out, "        root "+tlscert.ACMEWebroot+";\n") {
		t.Errorf("ACME 校验位置没有指向默认 webroot：\n%s", out)
	}
	// 只有 80 的站点不该有跳转：它没有 https 可跳。
	if strings.Contains(out, "return 301") {
		t.Errorf("没有证书却写了跳转：\n%s", out)
	}
}

// TestRenderSiteWithCertificate 绑定证书之后是两个 server 块：80 只留校验与跳转，443 是站点。
func TestRenderSiteWithCertificate(t *testing.T) {
	m := renderTestMaterial()
	out, err := RenderSite(renderTestSiteID, tlsRenderableDefinition(), RenderOptions{TLS: m})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if got := strings.Count(out, "server {"); got != 2 {
		t.Fatalf("应该有两个 server 块，实际 %d：\n%s", got, out)
	}
	must := []string{
		"    listen 80;",
		"    listen [::]:80;",
		"    listen 443 ssl;",
		"    listen [::]:443 ssl;",
		"    ssl_certificate     " + m.CertPath + ";",
		"    ssl_certificate_key " + m.KeyPath + ";",
		"    ssl_protocols TLSv1.2 TLSv1.3;",
		"    ssl_session_cache shared:upanel_tls:10m;",
		"    ssl_prefer_server_ciphers off;",
		"        return 301 https://$host$request_uri;",
		"        proxy_pass http://127.0.0.1:8080;",
		"    location ^~ /.well-known/acme-challenge/ {",
	}
	for _, want := range must {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q：\n%s", want, out)
		}
	}
	// 两个反斜杠都要有：证书与私钥各一条，且都指向材料给的路径。
	if strings.Count(out, "ssl_certificate_key ") != 1 {
		t.Error("ssl_certificate_key 应恰好出现一次")
	}
	// 旧协议与 http2 都不该出现。
	for _, banned := range []string{"TLSv1.1", "TLSv1 ", "http2", "SSLv3"} {
		if strings.Contains(out, banned) {
			t.Errorf("不该出现 %q：\n%s", banned, out)
		}
	}
	// 跳转必须在 ACME 校验位置之后：否则 http-01 会被 301 到 https，
	// 而签发那一刻 https 上还没有可用证书。
	if strings.Index(out, "return 301") < strings.Index(out, "acme-challenge") {
		t.Error("301 不能排在 ACME 校验位置之前")
	}
}

// TestRenderSiteACMEWebrootOverride 校验目录可以被覆盖（测试环境用它指向临时目录）。
func TestRenderSiteACMEWebrootOverride(t *testing.T) {
	const webroot = "/tmp/upanel-acme-test"
	out, err := RenderSite(renderTestSiteID, renderableDefinition(),
		RenderOptions{ACMEWebroot: webroot})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(out, "        root "+webroot+";\n") {
		t.Errorf("没有用上覆盖的 webroot：\n%s", out)
	}
	if strings.Contains(out, tlscert.ACMEWebroot) {
		t.Errorf("默认 webroot 不该再出现：\n%s", out)
	}
}

// TestRenderSiteRejectsBadTLSMaterial 证书材料的形状不对时必须拒绝渲染。
func TestRenderSiteRejectsBadTLSMaterial(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TLSMaterial)
	}{
		{name: "证书标识形状", mutate: func(m *TLSMaterial) { m.CertificateID = "crt_XYZ" }},
		{name: "来源不认识", mutate: func(m *TLSMaterial) { m.Source = "selfsign" }},
		{name: "证书路径不是绝对路径", mutate: func(m *TLSMaterial) { m.CertPath = "app.pem" }},
		{name: "证书路径含 ..", mutate: func(m *TLSMaterial) {
			m.CertPath = "/etc/ssl/upanel/../nginx/app.pem"
		}},
		{name: "证书路径不在证书目录", mutate: func(m *TLSMaterial) {
			m.CertPath = "/etc/passwd"
		}},
		{name: "证书文件名不是域名", mutate: func(m *TLSMaterial) {
			m.CertPath = "/etc/ssl/upanel/other.example.com.pem"
		}},
		{name: "证书与私钥同路径", mutate: func(m *TLSMaterial) { m.KeyPath = m.CertPath }},
		{name: "指纹不是 64 位十六进制", mutate: func(m *TLSMaterial) { m.Fingerprint = "zz" }},
		{name: "到期时间不是 RFC3339", mutate: func(m *TLSMaterial) { m.NotAfter = "2030-01-02" }},
		{name: "域名为空", mutate: func(m *TLSMaterial) { m.Domain = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := renderTestMaterial()
			tc.mutate(m)
			if out, err := RenderSite(renderTestSiteID, tlsRenderableDefinition(),
				RenderOptions{TLS: m}); err == nil {
				t.Fatalf("非法材料却渲染出了配置：\n%s", out)
			}
		})
	}
}

// TestApplyParamsCertificateConsistency 定义与材料必须成对出现、且对得上。
func TestApplyParamsCertificateConsistency(t *testing.T) {
	def := tlsRenderableDefinition()
	m := renderTestMaterial()

	// 两半都在、且一致：通过。
	good := ApplyParams{SiteID: renderTestSiteID, Version: 1, Definition: &def, PanelPort: 9443, Certificate: m}
	if _, err := good.Normalize(); err != nil {
		t.Fatalf("一致的材料被拒: %v", err)
	}

	// 定义绑了证书但参数里没有材料。
	half := ApplyParams{SiteID: renderTestSiteID, Version: 1, Definition: &def, PanelPort: 9443}
	if _, err := half.Normalize(); err == nil {
		t.Error("定义绑了证书却没有材料，应当拒绝（否则站点以为绑了证书、实际只监听 80）")
	}

	// 材料有、定义没绑。
	unbound := renderableDefinition()
	other := ApplyParams{SiteID: renderTestSiteID, Version: 1, Definition: &unbound, PanelPort: 9443, Certificate: m}
	if _, err := other.Normalize(); err == nil {
		t.Error("参数带证书材料但定义没绑定，应当拒绝")
	}

	// 标识对不上。
	mismatch := renderTestMaterial()
	mismatch.CertificateID = "crt_ffffffffffffffffffffffffffffffff"
	p := ApplyParams{SiteID: renderTestSiteID, Version: 1, Definition: &def, PanelPort: 9443, Certificate: mismatch}
	if _, err := p.Normalize(); err == nil {
		t.Error("证书标识与定义不一致，应当拒绝")
	}

	// 域名对不上。
	wrongDomain := renderTestMaterial()
	wrongDomain.Domain = "other.example.com"
	wrongDomain.CertPath = "/etc/ssl/upanel/other.example.com.pem"
	wrongDomain.KeyPath = "/etc/ssl/upanel/other.example.com.key"
	p = ApplyParams{SiteID: renderTestSiteID, Version: 1, Definition: &def, PanelPort: 9443, Certificate: wrongDomain}
	if _, err := p.Normalize(); err == nil {
		t.Error("证书域名与站点域名不一致，应当拒绝")
	}
}

// TestTLSOperatorPaths 两条来源各自的路径规则（写进配置的必须是这两个目录之一）。
func TestTLSOperatorPaths(t *testing.T) {
	certbot := TLSMaterial{
		CertificateID: "crt_0123456789abcdef0123456789abcdef",
		Source:        CertSourceCertbot,
		Domain:        "app.example.com",
		CertPath:      "/etc/letsencrypt/live/app.example.com/fullchain.pem",
		KeyPath:       "/etc/letsencrypt/live/app.example.com/privkey.pem",
		Fingerprint:   strings.Repeat("cd", 32),
		NotAfter:      "2030-01-02T03:04:05Z",
		Staging:       true,
	}
	if err := certbot.Validate(); err != nil {
		t.Fatalf("certbot 材料被拒: %v", err)
	}
	// certbot 的文件名是固定的，不是 <域名>.pem。
	bad := certbot
	bad.CertPath = "/etc/letsencrypt/live/app.example.com/cert.pem"
	if err := bad.Validate(); err == nil {
		t.Error("certbot 的证书文件名必须是 fullchain.pem")
	}
	// staging 只能是 certbot 的。
	stagedManual := renderTestMaterial()
	stagedManual.Staging = true
	if err := stagedManual.Validate(); err == nil {
		t.Error("手动导入的证书不该被标成 staging")
	}
}
