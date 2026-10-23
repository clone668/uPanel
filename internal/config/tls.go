package config

// 面板自己的 HTTPS 材料（P10.4）。
//
// 为什么面板必须能开 TLS：会话 Cookie 是 __Host- 前缀（CONTRACTS §2 要求 Secure、Path=/、
// 不带 Domain），浏览器在明文 HTTP 下根本不会回传它；PLAN §4 也写明“密码只能通过 HTTPS 提交”。
// 所以“监听 127.0.0.1 + SSH 隧道”只是一个更安全的部署方式，它并不取消 TLS：
// 隧道尽头仍然是一个 https://127.0.0.1:9443，本机进程偷看明文的口子也一起堵上。
//
// 证书从哪来：0.1 的安装脚本生成一张自签名证书并当众打印指纹（安装输出 + check-config），
// 用户核对指纹后接受。要换成受信任证书，直接把两个路径指向新文件再重启即可——
// 面板不存证书、不做 ACME 客户端、不碰系统信任库，这三件事都不该由管理面板替主机决定。

import (
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"upanel/internal/tlscert"
)

const (
	// PanelTLSMinVersion 是面板接受的最低 TLS 版本（TLS 1.2；1.0/1.1 已不该出现在管理面上）。
	PanelTLSMinVersion = tls.VersionTLS12
	// PanelCertExpiringWindow 是启动告警窗：剩余不足这么久就在日志/check-config 里提醒换证书。
	// 与证书模块的 21 天一致，运维只需要记一个数字。
	PanelCertExpiringWindow = 21 * 24 * time.Hour
)

// TLSConfigured 表示配置里给了证书路径（两个字段之一非空即算“想开 TLS”）。
//
// 成对校验在 Web.Validate 里做：只配一半是配置错误，不是“没配”。
func (w Web) TLSConfigured() bool {
	return strings.TrimSpace(w.TLSCertPath) != "" || strings.TrimSpace(w.TLSKeyPath) != ""
}

// PanelTLS 是加载并校验过的面板 HTTPS 材料。
type PanelTLS struct {
	// Config 直接交给 tls.NewListener；只放这一张证书，不引导学生握手去别处找。
	Config *tls.Config
	// Info 是叶证书的解析结果（指纹、有效期、域名），供日志与 check-config 展示。
	Info tlscert.Info
	// Warnings 是“能用但该看一眼”的事实，由调用方决定怎么显示。
	Warnings []string
}

// LoadTLS 读取证书与私钥，返回可直接使用的 TLS 配置。
//
// 失败一律返回面向运维的中文说明：这几个文件是安装脚本生成的，出错时用户需要知道
// 是“文件不在”“权限不对”还是“证书和私钥不是一对”。
func (w Web) LoadTLS() (*PanelTLS, error) {
	certPath := strings.TrimSpace(w.TLSCertPath)
	keyPath := strings.TrimSpace(w.TLSKeyPath)
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("web.tls_cert_path 与 web.tls_key_path 必须同时配置（当前 %q / %q）", certPath, keyPath)
	}

	for _, item := range []struct{ field, value string }{
		{"web.tls_cert_path", certPath},
		{"web.tls_key_path", keyPath},
	} {
		info, err := os.Stat(item.value)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%s 指向的文件不存在: %s", item.field, item.value)
			}
			return nil, fmt.Errorf("%s 无法读取 %s: %w", item.field, item.value, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s 指向的是目录而不是文件: %s", item.field, item.value)
		}
	}

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("加载证书/私钥失败（cert=%s key=%s）：%w；"+
			"最常见的原因是两者不是一对、私钥被加了口令，或 Web 账号没有读权限", certPath, keyPath, err)
	}
	if len(pair.Certificate) == 0 {
		return nil, fmt.Errorf("证书文件里没有证书: %s", certPath)
	}

	info, err := tlscert.ParseCertificate(encodePEM("CERTIFICATE", pair.Certificate[0]))
	if err != nil {
		return nil, fmt.Errorf("解析证书失败 %s: %w", certPath, err)
	}

	now := time.Now().UTC()
	if info.NotAfter.Before(now) {
		return nil, fmt.Errorf("面板证书已于 %s 过期（%s）；"+
			"换一张证书（或重跑安装脚本的证书生成步骤）再启动，不要用过期证书对外提供服务",
			info.NotAfter.Format(time.RFC3339), info.Subject)
	}
	if info.NotBefore.After(now.Add(5 * time.Minute)) {
		return nil, fmt.Errorf("面板证书要到 %s 才生效，当前时间 %s：先核对系统时间（NTP），再检查证书",
			info.NotBefore.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	result := &PanelTLS{
		Config: &tls.Config{
			Certificates: []tls.Certificate{pair},
			MinVersion:   PanelTLSMinVersion,
			// 不设 NextProtos：面板走 HTTP/1.1。SSE 与终端 WebSocket 都在这一层跑，
			// 先把一条路径上的行为钉死；HTTP/2 是以后单独决定的事。
		},
		Info: info,
	}
	if info.NotAfter.Sub(now) < PanelCertExpiringWindow {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"面板证书将在 %s 到期（剩 %s），届时浏览器会拒绝连接：现在就该安排换证书",
			info.NotAfter.Format(time.RFC3339), info.NotAfter.Sub(now).Truncate(time.Hour)))
	}
	if info.SelfSigned {
		result.Warnings = append(result.Warnings, "面板证书是自签名的：浏览器会提示不受信任，"+
			"请按安装说明核对指纹后再接受，或换成受信任证书")
	}
	if st, err := os.Stat(keyPath); err == nil {
		// 私钥要能被 Web 账号读到，所以组读是允许的（安装脚本给 root:upanel-web 0640）；
		// 但“其他用户可读”没有任何正当用途，直接报出来。
		if perm := st.Mode().Perm(); perm&0o007 != 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"私钥文件 %s 的权限是 %04o，其他用户可读：改成 0640（属主 root、属组 upanel-web）",
				keyPath, perm))
		}
	}
	return result, nil
}

// CheckTransport 判定“监听地址 + TLS 配置”这个组合能不能接受。
//
// 两条规则：
//   - 非回环监听必须有 TLS。面板一旦绑到管理网 IP，明文 HTTP 就等于把管理员口令
//     和会话令牌摊在同网段上；Cookie 是 Secure 的，浏览器也不会回传，登录会直接坏掉。
//   - 回环监听可以不开 TLS：那是“只经 SSH 隧道访问”的部署方式，传输已由 SSH 加密。
//     但这也是唯一允许明文的情形，因此本函数不把它的选择藏起来——调用方会记日志。
func CheckTransport(listenAddress string, tlsConfigured bool) error {
	if tlsConfigured {
		return nil
	}
	if !IsLoopbackAddress(listenAddress) {
		return fmt.Errorf("监听地址 %s 不是回环地址，必须配置 web.tls_cert_path 与 web.tls_key_path："+
			"会话 Cookie 带 __Host- 前缀（只在 HTTPS 下回传），明文管理网络没有任何可接受的用法", listenAddress)
	}
	return nil
}

// validateTLSPaths 校验两个路径的写法（不读文件：check-config 之外还要能校验未安装的配置）。
func (w Web) validateTLSPaths() error {
	certPath := strings.TrimSpace(w.TLSCertPath)
	keyPath := strings.TrimSpace(w.TLSKeyPath)
	if (certPath == "") != (keyPath == "") {
		return fmt.Errorf("web.tls_cert_path 与 web.tls_key_path 必须同时配置或同时留空（当前 %q / %q）", certPath, keyPath)
	}
	for _, item := range []struct{ field, value string }{
		{"web.tls_cert_path", certPath},
		{"web.tls_key_path", keyPath},
	} {
		if item.value == "" {
			continue
		}
		if !path.IsAbs(item.value) {
			return fmt.Errorf("%s 必须是 POSIX 绝对路径，当前为 %q", item.field, item.value)
		}
		if strings.ContainsAny(item.value, "?#") {
			return fmt.Errorf("%s 不能包含 '?' 或 '#'，当前为 %q", item.field, item.value)
		}
	}
	return nil
}

// encodePEM 把 DER 重新包成 PEM，好交给 tlscert 统一解析。
// 走这一圈而不是自己解 x509，是为了让“面板证书的指纹”与证书模块里那个指纹
// 用同一套约定（sha256(叶证书 DER) 小写十六进制）。
func encodePEM(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
