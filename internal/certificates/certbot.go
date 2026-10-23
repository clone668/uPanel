package certificates

// Certbot 的调用与 HTTP-01 可达性探测（P10.3）。
//
// 三件事在这里被钉死：
//
//  1. **不接受任意 Certbot 参数。** 参数表由 CertbotArgs 生成，程序路径来自配置里
//     的绝对路径，`--webroot` 是唯一允许的方式（不用 nginx 插件：那会让 certbot
//     去改 /etc/nginx，与 P10.2 的托管文件争同一块地方）。
//  2. **staging 是默认，正式要显式选。** 面板只传一个布尔值，实际地址由这里按配置
//     映射——默认是 Let's Encrypt 的 staging 与 production 两个入口。
//  3. **签发之前先证明校验路径可达。** 最常见的失败是域名没指到这台机器或 80 端口
//     不通，而那种失败在 certbot 真的去申请时才暴露，已经消耗了一次请求。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"upanel/internal/proxies"
	"upanel/internal/syscmd"
	"upanel/internal/tlscert"
)

const (
	// MaxSecretRefLen 与面板库 0010 迁移的 CHECK 一致。
	MaxSecretRefLen = 64

	// certbotTimeout 是一次 certbot 调用的上限。
	//
	// ACME 的流程本身是「提交订单 → 等 CA 来取校验文件 → 轮询结果」，正常在
	// 30 秒内结束；两分钟与 CONTRACTS.md 第 4 节的任务期限同一数量级。
	// **超时不是失败的同义词**：certbot 可能已经提交了订单，所以超时之后要按
	// 实际结果去读盘上的证书，而不是直接宣布没发生（见 ExecuteIssue 的读回）。
	certbotTimeout = 120 * time.Second

	// probeTimeout 是一次 HTTP-01 可达性探测的上限。
	probeTimeout = 5 * time.Second

	// acmeChallengeDir 是校验文件相对 webroot 的位置（RFC 8555 固定路径）。
	acmeChallengeDir = ".well-known/acme-challenge"

	// acmeDirMode 是校验目录（webroot 与它的两级子目录）的权限。
	//
	// 必须是 0755：Nginx 的 worker 不是 root，它要能进入这些目录并读到 CA 来取的
	// 校验文件。这也意味着这里**不能**靠进程 umask 的默认值（见 probeHTTP01）。
	acmeDirMode os.FileMode = 0o755
)

// serverURL 把「是不是 staging」映射成 ACME 目录地址。
func (o *Ops) serverURL(staging bool) string {
	if staging {
		if o.opts.StagingDirectoryURL != "" {
			return o.opts.StagingDirectoryURL
		}
		return ACMEDirectoryStaging
	}
	if o.opts.ProductionDirectoryURL != "" {
		return o.opts.ProductionDirectoryURL
	}
	return ACMEDirectoryProduction
}

// certbotPath 返回 certbot 可执行文件的绝对路径。
func (o *Ops) certbotPath() string {
	if o.opts.CertbotPath != "" {
		return o.opts.CertbotPath
	}
	return tlscert.CertbotPath
}

// runCertbot 跑一次 certbot certonly，返回证书与私钥的路径。
//
// 程序与参数全部由代码生成：域名与邮箱经过形状校验（域名是 A-label、邮箱没有
// 空白与 shell 元字符），并且**逐项**传给 exec——不经过任何 shell，所以即使
// 某个值有问题也不会变成一条命令。
func (o *Ops) runCertbot(ctx context.Context, p IssueParams, force bool) (string, string, syscmd.Result, error) {
	program, args := CertbotArgs(o.certbotPath(), o.webroot(), p.Domain, p.Email,
		o.serverURL(p.Staging), force)
	result, err := o.opts.Runner.Run(ctx, syscmd.Spec{Program: program, Args: args},
		syscmd.Limits{Timeout: certbotTimeout})
	certPath, keyPath := o.pathsOf(p.Domain, SourceCertbot)
	if err != nil {
		if errors.Is(err, syscmd.ErrTimeout) {
			// 超时之后仍然按盘上的事实判断：certbot 可能已经把证书写下来了。
			if _, statErr := o.opts.FS.Stat(certPath); statErr == nil {
				return certPath, keyPath, result, nil
			}
			return certPath, keyPath, result,
				fmt.Errorf("certbot 超过 %s 没有结束，且盘上没有证书", certbotTimeout)
		}
		// 非零退出是 syscmd 的 error（*ExitError），但 Result 里带着 certbot 自己的
		// 输出——那才是运维能行动的原因（账号不存在、HTTP-01 打不通、限额）。
		// 只报"以退出状态 1 结束"，面板上就是一个查不下去的失败：真机上遇到的正是
		// 这种情形（测试 ACME 重启后账号没了，certbot 说 accountDoesNotExist，
		// 而界面只显示“以退出状态 1 结束”）。
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return certPath, keyPath, result,
				fmt.Errorf("certbot 退出码 %d：%s", exitErr.ExitCode, certbotReason(result))
		}
		return certPath, keyPath, result, fmt.Errorf("跑 certbot 失败：%w", err)
	}
	if result.ExitCode != 0 {
		return certPath, keyPath, result,
			fmt.Errorf("certbot 退出码 %d：%s", result.ExitCode, certbotReason(result))
	}
	return certPath, keyPath, result, nil
}

// certbotReason 取一段能读的失败原因：stderr 优先，空则 stdout，截至 600 字节；
// 两个都空时明说去哪儿看，而不是留一个空白的"退出码 1"。
// certbot 的 stderr 里没有私钥（私钥从头到尾没到它手里），只有域名、ACME 错误码与服务端 detail。
func certbotReason(result syscmd.Result) string {
	reason := tail(result.Stderr, 600)
	if reason == "" {
		reason = tail(result.Stdout, 600)
	}
	if reason == "" {
		return "certbot 没有输出任何原因（certbot 的完整日志在 /var/log/letsencrypt/）"
	}
	return reason
}

// probeHTTP01 在本机的校验目录里放一个随机文件，再从外部按 URL 取回它。
//
// 它测的正是 ACME 服务器将要走的那条路：DNS → 80 端口 → Nginx 的
// `location ^~ /.well-known/acme-challenge/` → webroot 目录。三者任一不通，
// 这里就失败，而失败发生在 certbot 之前。
func (o *Ops) probeHTTP01(ctx context.Context, domain string) (int, error) {
	dir := path.Join(o.webroot(), acmeChallengeDir)
	if err := o.opts.FS.MkdirAll(dir, acmeDirMode); err != nil {
		return 0, fmt.Errorf("建不了校验目录 %s：%w", dir, err)
	}
	// 这三级的权限必须**显式**设：MkdirAll 的 perm 只对新目录生效，而且先被进程
	// umask 掩一遍。代理单元带 `UMask=0077`，于是校验目录在真机上落成 0700 root：
	// Nginx 的 worker（www-data）连进都进不去，CA 来取校验文件时拿到 403 ——
	// 而 certbot 只有在目录**不存在**时才按自己的 022 umask 建它，目录已经在了
	// 它就原样沿用。P10.3 没暴露是因为那时代理是手工以 root 起的（umask 022）。
	// 与 Q243 同一条教训：权限由代码决定，不由启动代理的 shell 决定。
	for _, d := range []string{o.webroot(), path.Join(o.webroot(), ".well-known"), dir} {
		if err := o.opts.FS.Chmod(d, acmeDirMode); err != nil {
			return 0, fmt.Errorf("设置校验目录 %s 的权限失败：%w", d, err)
		}
	}
	token, err := randomToken()
	if err != nil {
		return 0, err
	}
	target := path.Join(dir, token)
	if err := o.opts.FS.WriteFileSync(target, []byte(token), 0o644); err != nil {
		return 0, fmt.Errorf("写不了校验文件 %s：%w", target, err)
	}
	defer func() { _ = o.opts.FS.Remove(target) }()

	url := "http://" + domain + "/" + acmeChallengeDir + "/" + token
	status, body, err := o.httpGet(ctx, url)
	if err != nil {
		return 0, fmt.Errorf("请求 %s 失败：%w", url, err)
	}
	if strings.TrimSpace(body) != token {
		return status, fmt.Errorf("取回的 %s 内容不是刚写下的那个文件（状态 %d）：80 端口上回答请求的可能不是本机的 Nginx", url, status)
	}
	return status, nil
}

// httpGet 是可达性探测用的 HTTP 客户端（不跟随跳转、不走代理、有超时）。
func (o *Ops) httpGet(ctx context.Context, url string) (int, string, error) {
	if o.opts.HTTPGet != nil {
		return o.opts.HTTPGet(ctx, url)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	transport := &http.Transport{
		// 不用 http.ProxyFromEnvironment：这台机器上的代理环境变量不该决定
		// 「域名是不是指向本机」，而 EXPLICIT 代理会让探测在代理端成功、
		// certbot 在源站失败。
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: (&net.Dialer{
			Timeout: probeTimeout,
		}).DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// 跳转就是不通过：ACME 服务器不会跟随 301，因为它要的是那个文件本身。
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	// 探测文件只有几十字节，多读一点就够判断了。
	buf := make([]byte, 4<<10)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n]), nil
}

// randomToken 生成一个 32 位十六进制的随机名字。
func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成校验文件名失败：%w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// normalizeDomain 用反向代理那一套规则归一化域名。
//
// 复用而不是复制：证书的域名与站点定义的域名必须是同一个字符串，否则
// `proxy.apply` 在参数一致性检查里会把它们判成不一致。两处各自实现一套
// IDNA 归一化，迟早会在某个 Unicode 域名上分出两个答案。
// NormalizeDomainForQuery 归一化一个查询用的域名（接口层的 ?domain= 过滤）。
//
// 与写入路径用的是同一套归一化：否则"按域名过滤"会在大写或末尾带点的时候找不到
// 一条明明存在的记录。
func NormalizeDomainForQuery(raw string) (string, error) { return normalizeDomain(raw) }

func normalizeDomain(raw string) (string, error) {
	d, err := proxies.NormalizeDomain(raw)
	if err != nil {
		return "", err
	}
	return d, nil
}
