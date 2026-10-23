package proxies

// Nginx 配置的渲染（P10.2）。
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

import (
	"fmt"
	"path"
	"strings"

	"upanel/internal/nginx"
)

const (
	// ManagedMarker 是托管文件第一行的标记，带上站点标识。
	//
	// 它有两个用处：给人看（“这是面板写的，别手工改”），给程序看（P10.2 清理改名
	// 之后留下的旧文件时，靠它判断“这份文件属于哪个站点”）。没有它，域名改名之后
	// 旧文件只能靠猜。
	ManagedMarker = "# 由 uPanel 管理：站点 "

	// ManagedListenPort 是托管站点监听的端口。
	//
	// **它不是参数。** 0.1 的站点只监听 80（HTTPS 在 P10.3 加 443）。把监听端口
	// 从参数里来，就等于允许把管理面板的 9443 写成一份 server 块——那正是 P10.1
	// 用“上游端口不得等于面板端口”挡住的那种事故的另一条入口。
	ManagedListenPort = 80

	// AuthDirName 是 htpasswd 文件所在的目录（相对配置根目录）。
	AuthDirName = "upanel-auth"

	// maxUpstreamPort / minUpstreamPort 与定义校验一致。
	minUpstreamPort = 1
	maxUpstreamPort = 65535
)

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
// htpasswdPath 只在 access_mode=basic_auth 时被写进配置；其它模式传空串即可。
// 定义本身必须已经过 Normalize（本函数不重复做 IDNA 归一化之类的事，只做渲染前的
// 最后一道形状检查——渲染是最后一步能廉价地拒绝怪东西的地方）。
func RenderSite(siteID string, def Definition, htpasswdPath string) (string, error) {
	if err := validateRenderable(siteID, def, htpasswdPath); err != nil {
		return "", err
	}

	var b strings.Builder
	// 头三行是标记与告警。它们不参与幂等判断之外的任何逻辑，因此可以稳定地写在最前。
	b.WriteString(ManagedMarker + siteID + "\n")
	b.WriteString("# 这个文件会被面板整份重写：在这里手工修改的内容，下一次应用就没了。\n")
	b.WriteString("# 要改访问方式请改面板里的站点定义，不要在 Nginx 里改。\n")
	b.WriteString("\n")
	b.WriteString("server {\n")
	b.WriteString(fmt.Sprintf("    listen %d;\n", ManagedListenPort))
	b.WriteString("    listen [::]:" + fmt.Sprint(ManagedListenPort) + ";\n")
	b.WriteString("    server_name " + def.Domain + ";\n")
	b.WriteString("\n")
	// 访问日志分开写：出事时要能只翻某一个站点的日志，而不是在整机 access.log 里捞。
	b.WriteString("    access_log /var/log/nginx/upanel-" + def.Domain + ".access.log;\n")
	b.WriteString("    error_log  /var/log/nginx/upanel-" + def.Domain + ".error.log warn;\n")
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("    client_max_body_size %s;\n", sizeSpec(def.MaxBodyBytes)))
	b.WriteString("\n")

	// 访问策略。三种模式互斥，渲染也互斥——定义里不可能出现“既 basic_auth 又有网段”。
	switch def.AccessMode {
	case AccessCIDR:
		for _, cidr := range def.AllowedCIDRs {
			b.WriteString("    allow " + cidr + ";\n")
		}
		b.WriteString("    deny all;\n")
	case AccessBasicAuth:
		b.WriteString("    auth_basic \"uPanel 受保护站点\";\n")
		b.WriteString("    auth_basic_user_file " + htpasswdPath + ";\n")
	}

	b.WriteString("\n    location / {\n")
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
	b.WriteString("}\n")
	return b.String(), nil
}

// validateRenderable 检查渲染需要的字段。
//
// 它不重复 Normalize 的全部工作：那一步在入库前已经做过，而且它要做的事（IDNA
// 转换、去重、默认值）会改变字节，重做一遍只会让“渲染出来的东西”与“库里存的东西”
// 出现微妙的差别。这里只挡住“渲染出来会变成一段坏配置”的输入。
func validateRenderable(siteID string, def Definition, htpasswdPath string) error {
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
		if htpasswdPath == "" {
			return fmt.Errorf("basic_auth 模式的账号文件路径为空")
		}
		if !path.IsAbs(htpasswdPath) {
			return fmt.Errorf("账号文件路径必须是绝对路径：%q", htpasswdPath)
		}
	default:
		return fmt.Errorf("访问模式 %q 不在 public/cidr/basic_auth 里", def.AccessMode)
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
