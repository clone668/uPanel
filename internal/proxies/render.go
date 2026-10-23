package proxies

// Nginx 配置的渲染（P10.2、P10.3）。
//
// 渲染是一个**纯函数**：同样的站点定义必须渲染出同样的字节。这条要求不是为了好看，
// 而是整条写入流水线的幂等判据——渲染结果与磁盘上的内容逐字节相同就不写、不备份、
// 不 reload。所以这里：
//
//   - 不写时间戳、不写随机盐、不写自增序号；
//   - 不读系统状态（上游地址由定义里的端口决定，不取决于此刻谁在监听）；
//   - 不排序任何来自用户的列表之外的东西（CIDR 与账号在入库前已排序）。
//
// 只管理一件事：一个独立域名 → 一个明确上游。子路径、rewrite、多 upstream、
// 负载均衡都不在这里（PLAN.md 第 13 节：0.1 不做）。文件里不允许出现用户提供的
// 原始 Nginx 片段——所有会进配置的值都来自已经过形状校验的字段。
//
// P10.3 加了两种形状：没有证书时只监听 80；绑定证书时 80 只留 ACME 校验与跳转，
// 443 才是站点。两种形状都保留 /.well-known/acme-challenge/ 那条 location——续期
// 要能在证书还没换新的那一刻被访问到，配置里就必须一直留着这条路。

import (
	"fmt"
	"path"
	"strings"

	"upanel/internal/nginx"
	"upanel/internal/tlscert"
)

const (
	// ManagedMarker 是托管文件第一行的标记，带上站点标识。
	//
	// 它有两个用处：给人看（“这是面板写的，别手工改”），给程序看（P10.2 清理改名
	// 之后留下的旧文件时，靠它判断“这份文件属于哪个站点”）。没有它，域名改名之后
	// 旧文件只能靠猜。
	ManagedMarker = "# 由 uPanel 管理：站点 "

	// ManagedListenPort 是托管站点监听的 HTTP 端口。
	//
	// **它不是参数。** 0.1 的站点只监听 80 与 443（后者需要绑定证书）。把监听端口
	// 从参数里来，就等于允许把管理面板的 9443 写成一份 server 块——那正是 P10.1
	// 用“上游端口不得等于面板端口”挡住的那种事故的另一条入口。
	ManagedListenPort = 80

	// ManagedTLSPort 是绑定证书之后的监听端口。
	ManagedTLSPort = 443

	// AuthDirName 是 htpasswd 文件所在的目录（相对配置根目录）。
	AuthDirName = "upanel-auth"

	// acmeLocationPath 是 HTTP-01 校验的固定路径（Let's Encrypt 只用这一条）。
	acmeLocationPath = "/.well-known/acme-challenge/"

	// sslSessionZone 是 TLS 会话缓存的共享区名。多个托管文件用同一个名字是**有意的**：
	// 同名同尺寸的 shared 区在 nginx 里是同一个区，会话复用才能跨站点生效。
	sslSessionZone = "shared:upanel_tls:10m"

	// maxUpstreamPort / minUpstreamPort 与定义校验一致。
	minUpstreamPort = 1
	maxUpstreamPort = 65535
)

// RenderOptions 是渲染需要的全部输入（除定义本身）。
type RenderOptions struct {
	// HtpasswdPath 是 basic_auth 的账号文件路径（其它模式传空串）。
	HtpasswdPath string
	// TLS 非空时渲染 443 的 server 块。
	TLS *TLSMaterial
	// ACMEWebroot 是 HTTP-01 校验文件的根目录；空串走默认。
	ACMEWebroot string
}

// ManagedFileNameOf 返回一个站点对应的托管文件名（`upanel-<domain>.conf`）。
//
// 文件名来自域名而不是站点 id：人打开 /etc/nginx/conf.d 时要能一眼看出这是哪个
// 域名。域名在入库前已归一化成 A-label，nginx.ManagedFileName 再做一次白名单检查。
func ManagedFileNameOf(domain string) (string, error) {
	return nginx.ManagedFileName(domain)
}

// AuthFileNameOf 返回一个站点的 htpasswd 文件名。
//
// 用站点 id 而不是域名：域名会改，账号文件不该跟着改一次名（改名的每一步都要重写
// 配置、重新校验、重新 reload，多一个会动的文件名就多一段可能半途而废的流程）。
func AuthFileNameOf(siteID string) string { return siteID + ".htpasswd" }

// RenderSite 把一个站点定义渲染成托管文件的内容。
//
// 定义本身必须已经过 Normalize（本函数不重复做 IDNA 归一化之类的事，只做渲染前的
// 最后一道形状检查——渲染是最后一步能廉价地拒绝怪东西的地方）。
func RenderSite(siteID string, def Definition, opts RenderOptions) (string, error) {
	if err := validateRenderable(siteID, def, opts); err != nil {
		return "", err
	}
	webroot := opts.ACMEWebroot
	if webroot == "" {
		webroot = tlscert.ACMEWebroot
	}

	var b strings.Builder
	// 头三行是标记与告警。它们不参与幂等判断之外的任何逻辑，因此可以稳定地写在最前。
	b.WriteString(ManagedMarker + siteID + "\n")
	b.WriteString("# 这个文件会被面板整份重写：在这里手工修改的内容，下一次应用就没了。\n")
	b.WriteString("# 要改访问方式或换证书请改面板里的站点定义，不要在 Nginx 里改。\n")
	b.WriteString("\n")

	if opts.TLS != nil {
		writeHTTPToHTTPSBlock(&b, def, webroot)
		b.WriteString("\n")
		writeTLSServerBlock(&b, def, opts, webroot)
		return b.String(), nil
	}
	writeHTTPServerBlock(&b, def, opts, webroot)
	return b.String(), nil
}

// writeHTTPServerBlock 渲染“只有 80”的站点（没有绑定证书）。
func writeHTTPServerBlock(b *strings.Builder, def Definition, opts RenderOptions, webroot string) {
	b.WriteString("server {\n")
	b.WriteString(fmt.Sprintf("    listen %d;\n", ManagedListenPort))
	b.WriteString("    listen [::]:" + fmt.Sprint(ManagedListenPort) + ";\n")
	b.WriteString("    server_name " + def.Domain + ";\n")
	b.WriteString("\n")
	writeLogs(b, def)
	writeBodyLimit(b, def)
	writeAccessPolicy(b, def, opts.HtpasswdPath)
	writeACMELocation(b, webroot)
	writeProxyLocation(b, def)
	b.WriteString("}\n")
}

// writeHTTPToHTTPSBlock 渲染 80 端口上的那一半：只留 ACME 校验，其余跳转。
//
// 为什么跳转而不是“两边都服务”：一个域名两种协议会让 Cookie、HSTS 与相对跳转
// 各自有一套行为，而站点管理员没有第二份配置去表达“我要的是哪个”。301 是明确
// 的一句话说清楚这件事。ACME 校验必须留在 80 且**在跳转之前**——续期的校验请求
// 走的是 http://<域名>/.well-known/…，如果被 301 到 https，签发那一刻证书还是旧的
// 或者没有，校验必然失败。
func writeHTTPToHTTPSBlock(b *strings.Builder, def Definition, webroot string) {
	b.WriteString("server {\n")
	b.WriteString(fmt.Sprintf("    listen %d;\n", ManagedListenPort))
	b.WriteString("    listen [::]:" + fmt.Sprint(ManagedListenPort) + ";\n")
	b.WriteString("    server_name " + def.Domain + ";\n")
	b.WriteString("\n")
	writeLogs(b, def)
	writeACMELocation(b, webroot)
	b.WriteString("\n    location / {\n")
	b.WriteString("        return 301 https://$host$request_uri;\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")
}

// writeTLSServerBlock 渲染 443 上的站点。
func writeTLSServerBlock(b *strings.Builder, def Definition, opts RenderOptions, webroot string) {
	b.WriteString("server {\n")
	b.WriteString(fmt.Sprintf("    listen %d ssl;\n", ManagedTLSPort))
	b.WriteString("    listen [::]:" + fmt.Sprint(ManagedTLSPort) + " ssl;\n")
	b.WriteString("    server_name " + def.Domain + ";\n")
	b.WriteString("\n")
	b.WriteString("    ssl_certificate     " + opts.TLS.CertPath + ";\n")
	b.WriteString("    ssl_certificate_key " + opts.TLS.KeyPath + ";\n")
	// TLSv1.2 起：1.0/1.1 已在所有主流浏览器里停用，留着只会被扫出“弱协议”。
	// 没有 http2 指令：`listen 443 ssl http2` 在 1.25 起废弃、`http2 on` 在 1.24
	// 上直接报错，而 0.1 不承诺 HTTP/2（WebSocket 走的是 HTTP/1.1 升级）。
	b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n")
	b.WriteString("    ssl_session_cache " + sslSessionZone + ";\n")
	b.WriteString("    ssl_session_timeout 10m;\n")
	b.WriteString("    ssl_prefer_server_ciphers off;\n")
	b.WriteString("\n")
	writeLogs(b, def)
	writeBodyLimit(b, def)
	writeAccessPolicy(b, def, opts.HtpasswdPath)
	writeACMELocation(b, webroot)
	writeProxyLocation(b, def)
	b.WriteString("}\n")
}

func writeLogs(b *strings.Builder, def Definition) {
	// 访问日志分开写：出事时要能只翻某一个站点的日志，而不是在整机 access.log 里捞。
	b.WriteString("    access_log /var/log/nginx/upanel-" + def.Domain + ".access.log;\n")
	b.WriteString("    error_log  /var/log/nginx/upanel-" + def.Domain + ".error.log warn;\n")
	b.WriteString("\n")
}

func writeBodyLimit(b *strings.Builder, def Definition) {
	b.WriteString(fmt.Sprintf("    client_max_body_size %s;\n", sizeSpec(def.MaxBodyBytes)))
	b.WriteString("\n")
}

// writeAccessPolicy 写访问策略。三种模式互斥，渲染也互斥——定义里不可能出现
// “既 basic_auth 又有网段”。
func writeAccessPolicy(b *strings.Builder, def Definition, htpasswdPath string) {
	switch def.AccessMode {
	case AccessCIDR:
		for _, cidr := range def.AllowedCIDRs {
			b.WriteString("    allow " + cidr + ";\n")
		}
		b.WriteString("    deny all;\n")
		b.WriteString("\n")
	case AccessBasicAuth:
		b.WriteString("    auth_basic \"uPanel 受保护站点\";\n")
		b.WriteString("    auth_basic_user_file " + htpasswdPath + ";\n")
		b.WriteString("\n")
	}
}

// writeACMELocation 写 HTTP-01 校验的那条 location。
//
// `^~` 前缀让它在正则匹配之前生效：校验路径必须永远由本地目录回答，不能落到
// 上游应用手里（有些框架会把 /.well-known/ 重写成 404）。
func writeACMELocation(b *strings.Builder, webroot string) {
	b.WriteString("    location ^~ " + acmeLocationPath + " {\n")
	b.WriteString("        root " + webroot + ";\n")
	b.WriteString("        default_type text/plain;\n")
	b.WriteString("        try_files $uri =404;\n")
	b.WriteString("    }\n")
	b.WriteString("\n")
}

func writeProxyLocation(b *strings.Builder, def Definition) {
	b.WriteString("    location / {\n")
	b.WriteString("        proxy_pass " + def.UpstreamScheme + "://127.0.0.1:" +
		fmt.Sprint(def.UpstreamPort) + ";\n")
	b.WriteString("        proxy_http_version 1.1;\n")
	b.WriteString("        proxy_set_header Host $host;\n")
	b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString("        proxy_set_header X-Forwarded-Proto $scheme;\n")
	if def.WebSocketEnabled {
		// Upgrade 与 Connection 是一对：只给一个，握手会以 400 结束。
		// 这里**不用 map $http_upgrade $connection_upgrade**——map 只能写在 http
		// 上下文，托管文件是 conf.d 里的 server 块，写 map 就得改主配置。而改主配置
		// 正是“不碰发行版文件”这条规则禁止的事。
		b.WriteString("        proxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("        proxy_set_header Connection \"upgrade\";\n")
	}
	b.WriteString("        proxy_connect_timeout 5s;\n")
	// 读超时同时是 WebSocket 的“空闲多久断开”。默认 60 秒对一个挂着不动的前端
	// 页面是够的，对需要长连接的场景要用户在定义里调大（上限 3600 秒）。
	b.WriteString(fmt.Sprintf("        proxy_read_timeout %ds;\n", def.ReadTimeoutSeconds))
	b.WriteString(fmt.Sprintf("        proxy_send_timeout %ds;\n", def.ReadTimeoutSeconds))
	b.WriteString("    }\n")
}

// validateRenderable 检查渲染需要的字段。
//
// 它不重复 Normalize 的全部工作：那一步在入库前已经做过，而且它要做的事（IDNA
// 转换、去重、默认值）会改变字节，重做一遍只会让“渲染出来的东西”与“库里存的东西”
// 出现微妙的差别。这里只挡住“渲染出来会变成一段坏配置”的输入。
func validateRenderable(siteID string, def Definition, opts RenderOptions) error {
	if !siteIDPattern.MatchString(siteID) {
		return fmt.Errorf("站点标识 %q 形状不合法，无法生成配置", siteID)
	}
	if def.Domain == "" {
		return fmt.Errorf("域名为空，无法生成配置")
	}
	if _, err := nginx.ManagedFileName(def.Domain); err != nil {
		return fmt.Errorf("域名 %q 不能当文件名用：%w", def.Domain, err)
	}
	if def.UpstreamPort < minUpstreamPort || def.UpstreamPort > maxUpstreamPort {
		return fmt.Errorf("上游端口 %d 不在 1—65535 之间", def.UpstreamPort)
	}
	if def.UpstreamScheme != SchemeHTTP && def.UpstreamScheme != SchemeHTTPS {
		return fmt.Errorf("上游协议 %q 只能是 http 或 https", def.UpstreamScheme)
	}
	if def.MaxBodyBytes <= 0 {
		return fmt.Errorf("请求体上限必须是正数")
	}
	if def.ReadTimeoutSeconds <= 0 {
		return fmt.Errorf("读取超时必须是正数")
	}
	switch def.AccessMode {
	case AccessPublic:
		if len(def.AllowedCIDRs) > 0 || len(def.BasicAuthUsers) > 0 {
			return fmt.Errorf("public 模式不能带网段或账号")
		}
	case AccessCIDR:
		if len(def.AllowedCIDRs) == 0 {
			return fmt.Errorf("cidr 模式必须有来源网段")
		}
	case AccessBasicAuth:
		if len(def.BasicAuthUsers) == 0 {
			return fmt.Errorf("basic_auth 模式必须有账号")
		}
		if opts.HtpasswdPath == "" {
			return fmt.Errorf("basic_auth 模式的账号文件路径为空")
		}
		if !path.IsAbs(opts.HtpasswdPath) {
			return fmt.Errorf("账号文件路径必须是绝对路径：%q", opts.HtpasswdPath)
		}
	default:
		return fmt.Errorf("访问模式 %q 不在 public/cidr/basic_auth 里", def.AccessMode)
	}
	if opts.TLS != nil {
		if err := opts.TLS.Validate(); err != nil {
			return err
		}
	}
	if opts.ACMEWebroot != "" && !path.IsAbs(opts.ACMEWebroot) {
		return fmt.Errorf("ACME 校验目录必须是绝对路径：%q", opts.ACMEWebroot)
	}
	return nil
}

// sizeSpec 把字节数写成 nginx 的大小写法（1m、512k、1g）。
//
// 用 1m 而不是 1048576：人打开文件时要能一眼核对面板上显示的“1 MiB”。
// 只接受能用整数单位表达的数值（不可整除时退回字节数，那同样合法）。
func sizeSpec(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dg", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dm", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dk", n>>10)
	default:
		return fmt.Sprint(n)
	}
}
