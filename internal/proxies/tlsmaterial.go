package proxies

// 站点绑定的 TLS 材料（P10.3）。
//
// 一个站点要不要 443，只取决于它有没有绑定证书（Definition.CertificateID）。
// 绑定之后渲染需要的是**四个事实**：证书文件在哪、私钥文件在哪、这份证书覆盖哪个
// 域名、以及它属于哪个证书记录。前两个是 Nginx 指令的参数，第三个是执行层的
// 最后一道校验（渲染出来的 server_name 必须被证书覆盖，否则浏览器会报证书错误
// ——那是唯一一种“配置语法正确、nginx -t 通过、站点照常启动但用户一定打不开”的
// 故障），第四个让报告里能说清“生效的是哪一张证书”。
//
// 这里**没有私钥内容，也永远不会有**。私钥在 agent 的秘密存储里，由 agent 在
// 写文件那一刻取出、写完即弃；面板与参数里只有路径。

import (
	"fmt"
	"path"
	"regexp"
	"time"

	"upanel/internal/tlscert"
)

// 证书来源闭集（与 certificates.source、0010 迁移的 CHECK 一致）。
const (
	CertSourceCertbot = "certbot"
	CertSourceManual  = "manual"
)

var (
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// certFilePattern 是域名形状的文件名。证书文件名来自域名（A-label），
	// 这里再挡一次，防止有人把 ../ 之类的东西塞进路径。
	certFilePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
)

// TLSMaterial 是渲染 443 需要的一切（除定义之外的）。
//
// 字段全部来自面板库里那条证书记录，在预检时固化进参数：这样“预检时看到的是哪张
// 证书”和“执行时写进配置的是哪张证书”是同一个值，不受两次点击之间换绑证书的影响。
type TLSMaterial struct {
	CertificateID string `json:"certificate_id"`
	// Source 是 certbot 或 manual。它决定私钥在哪：
	// certbot 的私钥在 /etc/letsencrypt/live 下，面板从头到尾不碰；
	// manual 的私钥在 agent 秘密存储里，导入时写一次。
	Source string `json:"source"`
	// Domain 是这张证书为之签发的那个域名（= 站点域名）。
	Domain      string `json:"domain"`
	CertPath    string `json:"cert_path"`
	KeyPath     string `json:"key_path"`
	Fingerprint string `json:"fingerprint"`
	Issuer      string `json:"issuer"`
	// NotAfter 是到期时刻（RFC3339）。执行层用它判断“这张证书在渲染这一刻
	// 是不是已经过期了”——过期证书能让 nginx 启动，但没人能打开站点。
	NotAfter string `json:"not_after"`
	// Staging 表示测试环境签发（浏览器不认）。它不改变渲染结果，只进报告。
	Staging bool `json:"staging"`
}

// Validate 检查渲染与执行需要的形状。
func (m TLSMaterial) Validate() error {
	if !certificateIDPattern.MatchString(m.CertificateID) {
		return fmt.Errorf("证书标识 %q 形状不合法", m.CertificateID)
	}
	switch m.Source {
	case CertSourceCertbot, CertSourceManual:
	default:
		return fmt.Errorf("证书来源 %q 只能是 certbot 或 manual", m.Source)
	}
	if m.Domain == "" {
		return fmt.Errorf("证书域名不能为空")
	}
	if !certFilePattern.MatchString(m.Domain) {
		return fmt.Errorf("证书域名 %q 不能用于文件路径", m.Domain)
	}
	if err := checkMaterialPath(m.CertPath, m.Source, m.Domain, ".pem"); err != nil {
		return fmt.Errorf("证书文件路径：%w", err)
	}
	if err := checkMaterialPath(m.KeyPath, m.Source, m.Domain, ".key"); err != nil {
		return fmt.Errorf("私钥文件路径：%w", err)
	}
	// 同一个文件当证书又当私钥，是把私钥写进 0644 的位置的另一种说法。
	if path.Clean(m.CertPath) == path.Clean(m.KeyPath) {
		return fmt.Errorf("证书与私钥不能是同一个文件")
	}
	if !fingerprintPattern.MatchString(m.Fingerprint) {
		return fmt.Errorf("指纹必须是小写十六进制的 SHA-256")
	}
	if _, err := time.Parse(time.RFC3339, m.NotAfter); err != nil {
		return fmt.Errorf("到期时间不是 RFC3339：%q", m.NotAfter)
	}
	if m.Staging && m.Source != CertSourceCertbot {
		return fmt.Errorf("只有 certbot 签发的证书才可能是 staging")
	}
	return nil
}

// checkMaterialPath 检查一条路径确实落在该来源的目录里，且文件名是域名。
//
// 为什么要在形状层挡住“任意路径”：这两个值会原样进 Nginx 配置。它们由面板生成、
// 正常情况下不会跑偏，但执行层的入口是参数，而“上游生成得对”不能替参数担保——
// 一条被改过的参数不该让 Nginx 去读一个任意文件。
func checkMaterialPath(p, source, domain, ext string) error {
	if p == "" {
		return fmt.Errorf("不能为空")
	}
	if !path.IsAbs(p) {
		return fmt.Errorf("必须是绝对路径：%q", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("必须是规范路径（不能含 . 或 ..）：%q", p)
	}
	file := path.Base(p)
	switch source {
	case CertSourceManual:
		if path.Dir(p) != tlscert.CertDir {
			return fmt.Errorf("手动导入的证书必须落在 %s 下：%q", tlscert.CertDir, p)
		}
		if file != domain+ext {
			return fmt.Errorf("文件名必须是 %s%s：%q", domain, ext, file)
		}
	case CertSourceCertbot:
		// certbot 的布局是 live/<域名>/fullchain.pem 与 privkey.pem。
		if path.Dir(p) != path.Join(tlscert.LiveDir, domain) {
			return fmt.Errorf("Certbot 证书必须落在 %s/%s 下：%q", tlscert.LiveDir, domain, p)
		}
		want := map[string]string{".pem": "fullchain.pem", ".key": "privkey.pem"}[ext]
		if file != want {
			return fmt.Errorf("文件名必须是 %s：%q", want, file)
		}
	}
	return nil
}

// Expired / Expiring 让预检能直接说“这张证书什么时候不能用”。
// 解析失败按“过期”处理是错的——那会把一条坏数据变成一次阻塞；这里交给
// Validate 挡（NotAfter 必须能解析，否则参数根本进不来）。
func (m TLSMaterial) Expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, m.NotAfter)
	if err != nil {
		return true
	}
	return !now.Before(t)
}

// Expiring 报告证书是否进入临期窗口。
func (m TLSMaterial) Expiring(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, m.NotAfter)
	if err != nil {
		return true
	}
	return !now.Add(tlscert.ExpiringWindow).Before(t)
}

// BindingOf 从参数里取出证书绑定（没有绑定时返回 nil）。
func (p ApplyParams) BindingOf() *TLSMaterial { return p.Certificate }
