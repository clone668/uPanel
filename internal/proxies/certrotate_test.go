package proxies

// 证书换了、配置没变：reload 判据（P10.3）。
//
// 这三条用例守的是同一个事实：**配置的字节不等于服务的内容**。
// 续期只换证书文件，渲染结果一字不差，按"配置有没有变"决定要不要 reload
// 就会得到 skipped_unchanged——新证书躺在盘上，443 上还是旧的，而报告说一切正常。
//
// 判据不能靠记状态（库里没有"上次 reload 时证书是哪一张"这一列，见 Q286），
// 所以只能现场问 443 一句。这些用例把那一问注入成桩：真握手太依赖 443 端口，
// 但"问了之后怎么判、判完怎么报"这部分必须能被普通测试覆盖。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path"
	"strings"
	"testing"
	"time"

	"upanel/internal/tlscert"
)

// seedTLSMaterial 在内存文件系统里放一对真证书/私钥，并返回与它们一致的证书材料。
//
// 私钥内容不参与任何判断（inspectTLS 只 stat 它），放什么字符串都行；
// 证书必须是真的：指纹、域名覆盖、到期都要从证书本身算出来。
func seedTLSMaterial(t *testing.T, f *applyFixture, domain string, serial int64, notAfter time.Time) (*TLSMaterial, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: domain},
		NotBefore:             applyFixedNow().Add(-time.Hour),
		NotAfter:              notAfter,
		DNSNames:              []string{domain},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发测试证书失败: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	info, err := tlscert.ParseCertificate(certPEM)
	if err != nil {
		t.Fatalf("解析测试证书失败: %v", err)
	}

	certPath := path.Join(tlscert.CertDir, domain+".pem")
	keyPath := path.Join(tlscert.CertDir, domain+".key")
	f.fsys.seed(certPath, string(certPEM))
	f.fsys.seed(keyPath, "-----BEGIN PRIVATE KEY-----\n（测试里不读私钥内容）\n-----END PRIVATE KEY-----\n")

	return &TLSMaterial{
		CertificateID: "crt_" + strings.Repeat("1", 32),
		Source:        CertSourceManual,
		Domain:        domain,
		CertPath:      certPath,
		KeyPath:       keyPath,
		Fingerprint:   info.Fingerprint,
		Issuer:        info.Issuer,
		NotAfter:      info.NotAfter.UTC().Format(time.RFC3339),
	}, info.Fingerprint
}

// boundParams 构造一份绑定了证书的应用参数。
func boundParams(domain string, version int, m *TLSMaterial) ApplyParams {
	def := applyDefinition(domain)
	def.CertificateID = m.CertificateID
	p := applyParamsOf(def, version)
	p.Certificate = m
	return p
}

// TestApplyReloadsWhenCertificateRotated 续期之后"再应用一次"必须真的 reload。
//
// 这是本组用例里最重要的一条：它把"配置字节没变、证书换了"与"什么都没变"
// 分开。少了这一条，续期链路在报告上是全绿的，而浏览器拿到的还是旧证书。
func TestApplyReloadsWhenCertificateRotated(t *testing.T) {
	f := newApplyFixture(t)
	domain := applyDomain
	notAfter := applyFixedNow().Add(90 * 24 * time.Hour)

	first, firstFP := seedTLSMaterial(t, f, domain, 0x1001, notAfter)
	served := firstFP
	f.applier.opts.ServedCert = func(_ context.Context, d string, port int) (string, error) {
		if d != domain {
			t.Errorf("握手用的域名 = %q，期望 %q（SNI 必须是被问的那个站点）", d, domain)
		}
		if port != ManagedTLSPort {
			t.Errorf("握手端口 = %d，期望 %d", port, ManagedTLSPort)
		}
		return served, nil
	}

	// 第一次：写配置 + reload。
	if _, err := f.execute(boundParams(domain, 1, first)); err != nil {
		t.Fatalf("第一次应用失败: %v", err)
	}
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 1 {
		t.Fatalf("第一次应用的 reload 次数 = %d，期望 1", got)
	}

	// 续期：同一对路径上换了一张新证书（真实世界里 certbot 改的是 live/ 指向的
	// archive 文件，路径不变、内容变），面板记录里的指纹也跟着更新。
	rotated, rotatedFP := seedTLSMaterial(t, f, domain, 0x1002, notAfter)
	if rotatedFP == firstFP {
		t.Fatal("两次签发的指纹相同，用例的前提不成立")
	}
	served = firstFP
	// Nginx 换一代 worker 之后才读新证书：reload 之前问到的永远是旧的。
	f.runner.afterReload = func() { served = rotatedFP }
	f.runner.reset()

	report, err := f.execute(boundParams(domain, 2, rotated))
	if err != nil {
		t.Fatalf("续期之后的应用失败: %v", err)
	}

	if got := report.Data["file_state"]; got != FileStateUnchanged {
		t.Errorf("file_state = %v，期望 %s（渲染结果本来就不该变）", got, FileStateUnchanged)
	}
	if got := report.Data["reload"]; got != ReloadDone {
		t.Errorf("reload = %v，期望 %s：证书换了就必须 reload", got, ReloadDone)
	}
	if got := report.Observed["reload_reason"]; got != ReloadReasonCertRotated {
		t.Errorf("reload_reason = %v，期望 %q", got, ReloadReasonCertRotated)
	}
	if got := report.Observed["served_cert_fingerprint"]; got != firstFP {
		t.Errorf("reload 之前服务端指纹 = %v，期望 %s", got, firstFP)
	}
	if got := report.Observed["served_cert_after_reload"]; got != rotatedFP {
		t.Errorf("reload 之后服务端指纹 = %v，期望 %s", got, rotatedFP)
	}
	// 桩是"reload 一返回就换了"，所以第一次问就该对上：不必等。
	if got := report.Observed["served_cert_probe_attempts"]; got != 1 {
		t.Errorf("reload 之后的探测次数 = %v，期望 1", got)
	}
	if strings.Contains(report.RecoveryHint, "仍然不是磁盘上这张证书") {
		t.Errorf("reload 之后已经换成新证书了，却报了陈旧告警：%s", report.RecoveryHint)
	}
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 1 {
		t.Errorf("reload 次数 = %d，期望 1", got)
	}
	if report.Outcome != OutcomeVerified {
		t.Errorf("outcome = %q，期望 verified", report.Outcome)
	}
}

// TestApplyReportsServedCertStillStaleAfterReload reload 做了，443 上却不是那张证书。
//
// 这件事必须出现在报告里：它是"续期成功、面板全绿、用户仍然拿到旧证书"的唯一线索。
// 常见成因是前面还有一层代理，或这个域名没有命中本面板的 server 块。
func TestApplyReportsServedCertStillStaleAfterReload(t *testing.T) {
	f := newApplyFixture(t)
	domain := applyDomain
	notAfter := applyFixedNow().Add(90 * 24 * time.Hour)

	first, firstFP := seedTLSMaterial(t, f, domain, 0x2001, notAfter)
	served := firstFP
	f.applier.opts.ServedCert = func(context.Context, string, int) (string, error) {
		return served, nil
	}
	if _, err := f.execute(boundParams(domain, 1, first)); err != nil {
		t.Fatalf("第一次应用失败: %v", err)
	}

	rotated, rotatedFP := seedTLSMaterial(t, f, domain, 0x2002, notAfter)
	// served 一直是旧指纹：reload 之后也没换过去（一层前置代理的典型表现）。
	f.runner.reset()

	report, err := f.execute(boundParams(domain, 2, rotated))
	if err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if got := report.Data["reload"]; got != ReloadDone {
		t.Errorf("reload = %v，期望 %s", got, ReloadDone)
	}
	if got := report.Observed["served_cert_after_reload"]; got != firstFP {
		t.Errorf("reload 之后服务端指纹 = %v，期望仍是旧的 %s", got, firstFP)
	}
	if !strings.Contains(report.RecoveryHint, "仍然不是磁盘上这张证书") {
		t.Errorf("reload 之后服务端还是旧证书，报告里必须说：%q", report.RecoveryHint)
	}
	if rotatedFP == firstFP {
		t.Fatal("两次签发的指纹相同，用例的前提不成立")
	}
}

// TestApplyProbeFailureFallsBackToByteComparison 问不到 443 就退回按配置字节判断。
//
// "问不到"不能当成"没变化"以外的任何结论：配置已经过了 nginx -t，reload 也修不好
// 一个握不上手的 443。但报告里要留下痕迹，否则将来查"为什么续期没生效"时，
// 没人知道当时根本没问成。
func TestApplyProbeFailureFallsBackToByteComparison(t *testing.T) {
	f := newApplyFixture(t)
	domain := applyDomain
	notAfter := applyFixedNow().Add(90 * 24 * time.Hour)

	f.applier.opts.ServedCert = func(context.Context, string, int) (string, error) {
		return "", context.DeadlineExceeded
	}
	first, _ := seedTLSMaterial(t, f, domain, 0x3001, notAfter)
	if _, err := f.execute(boundParams(domain, 1, first)); err != nil {
		t.Fatalf("第一次应用失败: %v", err)
	}
	rotated, _ := seedTLSMaterial(t, f, domain, 0x3002, notAfter)
	f.runner.reset()

	report, err := f.execute(boundParams(domain, 2, rotated))
	if err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if got := report.Data["reload"]; got != ReloadSkippedNoChange {
		t.Errorf("reload = %v，期望 %s（问不到就不改结论）", got, ReloadSkippedNoChange)
	}
	probe, _ := report.Observed["served_cert_probe"].(string)
	if !strings.HasPrefix(probe, "failed: ") {
		t.Errorf("served_cert_probe = %q，期望以 \"failed: \" 开头（没测到要说出来）", probe)
	}
	if _, ok := report.Observed["served_cert_fingerprint"]; ok {
		t.Error("握手失败了却写入了服务端指纹")
	}
}
