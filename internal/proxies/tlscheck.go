package proxies

// 站点绑定的证书在执行前的现场检查（P10.3）。
//
// 这一层判的不是“证书好不好”，而是**“把这两条路径写进配置之后，nginx -t 会过、
// 浏览器能打开站点”**。它存在的原因是：证书相关的故障几乎都不表现为配置语法错误。
//
//	证书文件不在          → nginx -t 报 cannot load certificate（晚了一步，但还能回滚）
//	私钥不在              → 同上
//	私钥权限 0644         → nginx 能起来，任何本地用户都能读走私钥
//	证书没覆盖这个域名    → **nginx -t 通过、站点照常启动、浏览器一定报证书错误**
//	证书已过期            → 同上
//	staging 证书          → 同上，而且是“以为上线了其实没有”的那一种
//
// 最后三条是这里的重点：它们是唯一一类“所有自动检查都绿、用户一定打不开”的故障，
// 所以必须在写配置之前用证书本身回答，而不是等用户来报。

import (
	"fmt"
	"time"

	"upanel/internal/tlscert"
)

// 证书相关的阻塞码与告警码（闭集）。
const (
	CodeTLSMaterialMissing  = "PROXY_TLS_MATERIAL_MISSING"
	CodeTLSMaterialInvalid  = "PROXY_TLS_MATERIAL_INVALID"
	CodeTLSKeyExposed       = "PROXY_TLS_KEY_PERMISSIVE"
	CodeTLSKeyUnavailable   = "PROXY_TLS_KEY_UNAVAILABLE"
	CodeTLSDomainUncovered  = "PROXY_TLS_DOMAIN_UNCOVERED"
	CodeTLSCertExpired      = "PROXY_TLS_CERT_EXPIRED"
	CodeTLSCertExpiring     = "PROXY_TLS_CERT_EXPIRING"
	CodeTLSFingerprintDrift = "PROXY_TLS_FINGERPRINT_DRIFT"
	CodeTLSStagingCert      = "PROXY_TLS_STAGING_CERT"
	// CodeTLSServedStale：reload 做过了，443 上却不是磁盘上那张证书。
	// 它与 drift 是两件事：drift 说的是"盘上的文件与面板记录不一致"，
	// 这一条说的是"盘上的文件没问题，但服务没有在用它"。
	CodeTLSServedStale = "PROXY_TLS_SERVED_STALE"
)

// tlsInspection 是一次现场检查的产物：事实、阻塞、告警。
type tlsInspection struct {
	Observed map[string]any
	Blockers []Blocker
	Warnings []Blocker
}

// inspectTLS 检查一份 TLS 材料此刻在磁盘上是什么样。
//
// 它只读证书文件，**不打开私钥文件**：私钥在不在、权限对不对，stat 就够了。
// 读私钥对这个判断没有任何增量，却会让私钥内容出现在代理进程的内存里——
// 而这条流水线里唯一需要私钥明文的地方是 Nginx 自己。
func (a *Applier) inspectTLS(fs ConfigFS, m TLSMaterial, domain string, now time.Time) tlsInspection {
	out := tlsInspection{Observed: map[string]any{
		"certificate_id": m.CertificateID,
		"cert_path":      m.CertPath,
		"key_path":       m.KeyPath,
		"source":         m.Source,
		"staging":        m.Staging,
	}}

	raw, err := fs.ReadFile(m.CertPath)
	if err != nil {
		out.Blockers = append(out.Blockers, Blocker{
			Code: CodeTLSMaterialMissing,
			Message: "证书文件 " + m.CertPath + " 读不到（" + err.Error() +
				"）：站点绑定了证书，Nginx 就会去找这个文件",
		})
		return out
	}
	info, err := tlscert.ParseCertificate(raw)
	if err != nil {
		out.Blockers = append(out.Blockers, Blocker{
			Code:    CodeTLSMaterialInvalid,
			Message: "证书文件 " + m.CertPath + " 解析失败：" + err.Error(),
		})
		return out
	}
	fingerprint := info.Fingerprint
	out.Observed["cert_fingerprint"] = fingerprint
	out.Observed["cert_issuer"] = info.Issuer
	out.Observed["cert_domains"] = info.Domains
	out.Observed["cert_not_after"] = info.NotAfter.UTC().Format(time.RFC3339)
	out.Observed["cert_expired"] = info.Expired(now)
	out.Observed["cert_expiring"] = info.Expiring(now)

	if !tlscert.CoversDomain(info, domain) {
		out.Blockers = append(out.Blockers, Blocker{
			Code: CodeTLSDomainUncovered,
			Message: fmt.Sprintf("证书 %s 不覆盖站点域名 %s（覆盖的是 %v）：配置能通过 nginx -t，但浏览器会拒绝打开",
				m.CertPath, domain, info.Domains),
		})
	}
	switch {
	case info.Expired(now):
		out.Blockers = append(out.Blockers, Blocker{
			Code: CodeTLSCertExpired,
			Message: fmt.Sprintf("证书已于 %s 过期：请先续期或换一张证书再应用站点",
				info.NotAfter.UTC().Format(time.RFC3339)),
		})
	case info.Expiring(now):
		out.Warnings = append(out.Warnings, Blocker{
			Code: CodeTLSCertExpiring,
			Message: fmt.Sprintf("证书将在 %s 到期（不足 %d 天）：续期要靠 P12 的 timer，此刻还没接上",
				info.NotAfter.UTC().Format(time.RFC3339), int(tlscert.ExpiringWindow.Hours()/24)),
		})
	}
	// 磁盘上的证书与面板记录里的指纹不一致：说明这份文件在签发/导入之后被换过。
	// 它不阻塞（文件是真的、域名是对的、没过期），但报告里必须说——否则面板显示的
	// 到期时间与真正生效的那张证书可以长期不是同一张。
	if m.Fingerprint != "" && m.Fingerprint != fingerprint {
		out.Warnings = append(out.Warnings, Blocker{
			Code: CodeTLSFingerprintDrift,
			Message: "磁盘上的证书指纹（" + fingerprint[:16] + "…）与面板记录的（" +
				m.Fingerprint[:16] + "…）不一致：面板显示的到期信息需要刷新",
		})
	}
	if info.Staging || m.Staging {
		out.Warnings = append(out.Warnings, Blocker{
			Code: CodeTLSStagingCert,
			Message: "这是 ACME 测试环境（staging）签发的证书：浏览器不信任它，" +
				"确认流程走通之后要重新用正式签发",
		})
	}

	fi, err := fs.Stat(m.KeyPath)
	if err != nil {
		out.Blockers = append(out.Blockers, Blocker{
			Code: CodeTLSKeyUnavailable,
			Message: "私钥文件 " + m.KeyPath + " 读不到（" + err.Error() +
				"）：证书与私钥必须成对出现",
		})
		return out
	}
	mode := fi.Mode().Perm()
	out.Observed["key_mode"] = fmt.Sprintf("%04o", mode)
	out.Observed["key_size"] = fi.Size()
	if mode&0o077 != 0 {
		out.Warnings = append(out.Warnings, Blocker{
			Code: CodeTLSKeyExposed,
			Message: fmt.Sprintf("私钥文件 %s 的权限是 %04o：同组或其他用户也能读走它（应当是 0600）",
				m.KeyPath, mode),
		})
	}
	return out
}
