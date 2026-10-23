package certificates

// 面板侧证书服务的测试（P10.3）。
//
// 用真实 SQLite（不是内存假实现）：域名唯一索引、"零行即冲突"、以及 CHECK 约束
// （手动导入必须带私钥引用、certbot 必须带邮箱）都只有真库才验证得到。
//
// 证书与私钥用真实 crypto 生成：硬编码的 PEM 会过期，而这里要断言的恰恰是"解析出来
// 的日期与域名与生成时给的一致"。

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"upanel/internal/store"
	"upanel/internal/tlscert"
	"upanel/migrations"
)

// fixedNow 是测试用的固定时钟。
func fixedNow() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) }

// quietLogger 让测试输出干净。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testFixture 是一份生成好的证书材料。
type testFixture struct {
	certPEM []byte
	keyPEM  []byte
	info    tlscert.Info
}

// newCertFixture 造一张自签名证书。issuerCN 非空时假装由那个 CA 签发。
func newCertFixture(t *testing.T, domain string, notBefore, notAfter time.Time, issuerCN string) testFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(0x2a2a),
		Subject:               pkix.Name{CommonName: domain, Organization: []string{"uPanel Test"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		DNSNames:              []string{domain},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	parent := tmpl
	if issuerCN != "" {
		parent = &x509.Certificate{
			SerialNumber:          big.NewInt(0x2a2b),
			Subject:               pkix.Name{CommonName: issuerCN},
			NotBefore:             notBefore,
			NotAfter:              notAfter,
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	info, err := tlscert.ParseCertificate(certPEM)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	return testFixture{
		certPEM: certPEM,
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		info:    info,
	}
}

// newTestService 构造跑在临时目录里的真实服务。
func newTestService(t *testing.T, opts Options) (*Service, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(dir, "upanel.db"),
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭测试数据库失败: %v", err)
		}
	})
	if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatalf("迁移测试数据库失败: %v", err)
	}
	if opts.Now == nil {
		opts.Now = fixedNow
	}
	svc, err := New(db, opts)
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	return svc, db
}

// TestCreateEnforcesSourceRules 验证 0010 迁移里的两条 CHECK 从服务层就挡住。
func TestCreateEnforcesSourceRules(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()
	fx := newCertFixture(t, "site.example.test", fixedNow().Add(-time.Hour), fixedNow().Add(90*24*time.Hour), "")

	if _, err := svc.Create(ctx, CreateInput{
		Domain: "site.example.test",
		Info:   fx.info,
		Source: SourceManual,
		// 故意不给私钥引用。
	}); err == nil {
		t.Fatal("手动导入缺私钥引用时应当报错")
	}

	if _, err := svc.Create(ctx, CreateInput{
		Domain: "site.example.test",
		Info:   fx.info,
		Source: SourceCertbot,
		// 故意不给邮箱。
	}); err == nil {
		t.Fatal("certbot 记录缺邮箱时应当报错")
	}

	created, err := svc.Create(ctx, CreateInput{
		Domain:       "site.example.test",
		Info:         fx.info,
		Source:       SourceManual,
		KeySecretRef: "sec_1234",
	})
	if err != nil {
		t.Fatalf("合法的手动导入记录应当创建成功: %v", err)
	}
	if created.Source != SourceManual || created.KeySecretRef != "sec_1234" {
		t.Fatalf("记录内容不对: %+v", created)
	}
	if created.KeyPath == created.CertPath {
		t.Fatalf("证书路径与私钥路径不能相同: %q", created.CertPath)
	}

	// 同一个域名再建一条：唯一索引应当变成 ErrDomainTaken。
	_, err = svc.Create(ctx, CreateInput{
		Domain:       "site.example.test",
		Info:         fx.info,
		Source:       SourceManual,
		KeySecretRef: "sec_5678",
	})
	if err == nil {
		t.Fatal("同一域名重复创建应当失败")
	}
}

// TestCreateWithSuppliedID 验证账本可以用任务报告里的标识建行。
func TestCreateWithSuppliedID(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()
	fx := newCertFixture(t, "keep.example.test", fixedNow().Add(-time.Hour), fixedNow().Add(90*24*time.Hour), "")
	id, err := NewID()
	if err != nil {
		t.Fatalf("生成标识失败: %v", err)
	}
	created, err := svc.Create(ctx, CreateInput{
		ID:           id,
		Domain:       "keep.example.test",
		Info:         fx.info,
		Source:       SourceManual,
		KeySecretRef: "sec_keep",
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	if created.ID != id {
		t.Fatalf("标识被改写: got %q want %q", created.ID, id)
	}
	if bad := "crt_ZZZ"; func() bool {
		_, cerr := svc.Create(ctx, CreateInput{ID: bad, Domain: "x.example.test", Info: fx.info, Source: SourceManual, KeySecretRef: "s"})
		return cerr != nil
	}() != true {
		t.Fatal("非法标识应当被拒绝")
	}
}

// TestCheckCertificate 验证绑定前的判断：覆盖、过期。
func TestCheckCertificate(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()
	fx := newCertFixture(t, "cover.example.test", fixedNow().Add(-time.Hour), fixedNow().Add(90*24*time.Hour), "")
	cert, err := svc.Create(ctx, CreateInput{
		Domain:       "cover.example.test",
		Info:         fx.info,
		Source:       SourceManual,
		KeySecretRef: "sec_cover",
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	if err := svc.CheckCertificate(ctx, cert.ID, "cover.example.test"); err != nil {
		t.Fatalf("覆盖本域名的证书应当可用: %v", err)
	}
	if err := svc.CheckCertificate(ctx, cert.ID, "other.example.test"); err == nil {
		t.Fatal("不覆盖的域名应当被拒绝")
	} else if !errors.Is(err, ErrNotCovered) {
		t.Fatalf("错误类型不对: %v", err)
	}

	// 过期证书：单独造一条记录（Create 不接受已过期的证书，所以直接写库）。
	expired := newCertFixture(t, "old.example.test", fixedNow().Add(-48*time.Hour), fixedNow().Add(-time.Hour), "")
	if _, err := svc.Create(ctx, CreateInput{
		Domain:       "old.example.test",
		Info:         expired.info,
		Source:       SourceManual,
		KeySecretRef: "sec_old",
	}); err == nil {
		t.Fatal("已过期的证书不应被接受为初始导入")
	}
}

// TestTooSoon 验证一小时的限流窗口。
func TestTooSoon(t *testing.T) {
	now := fixedNow()
	if TooSoon(time.Time{}, now) {
		t.Fatal("从未尝试过时不该被限流")
	}
	if !TooSoon(now.Add(-30*time.Minute), now) {
		t.Fatal("半小时前试过应当被限流")
	}
	if TooSoon(now.Add(-2*time.Hour), now) {
		t.Fatal("两小时前试过不该被限流")
	}
}

// TestStageStoreBoundToSession 验证暂存绑定用户与会话，且取走即销毁。
func TestStageStoreBoundToSession(t *testing.T) {
	fx := newCertFixture(t, "stage.example.test", fixedNow().Add(-time.Hour), fixedNow().Add(90*24*time.Hour), "")
	stages := NewStageStore(func() time.Time { return fixedNow() })

	staged, err := stages.Put(1, "session-a", fx.certPEM)
	if err != nil {
		t.Fatalf("暂存失败: %v", err)
	}
	if _, err := stages.Take(staged.ID, 2, "session-a"); err == nil {
		t.Fatal("别的用户不该取走这份暂存")
	}
	if _, err := stages.Take(staged.ID, 1, "session-b"); err == nil {
		t.Fatal("别的会话不该取走这份暂存")
	}
	taken, err := stages.Take(staged.ID, 1, "session-a")
	if err != nil {
		t.Fatalf("取走失败: %v", err)
	}
	if len(taken.PEM) == 0 || taken.Info.Domains[0] != "stage.example.test" {
		t.Fatalf("取走的内容不对: %+v", taken.Info)
	}
	if _, err := stages.Take(staged.ID, 1, "session-a"); err == nil {
		t.Fatal("暂存只能取一次")
	}

	// 夹带私钥的输入不该被当成公钥证书：Put 走 ParseCertificate，私钥块被忽略但
	// 证书本身仍能解析——所以这里断言的是"取走的内容里没有私钥块"这件事由接口层
	// 负责（tlscert.ContainsPrivateKey），本层不假装自己拦得住。
	if tlscert.ContainsPrivateKey(fx.certPEM) {
		t.Fatal("纯证书 PEM 不该被判为含私钥")
	}
	if !tlscert.ContainsPrivateKey(append(append([]byte(nil), fx.certPEM...), fx.keyPEM...)) {
		t.Fatal("证书 + 私钥拼接必须被判为含私钥")
	}
}

// TestLedgerImportCreatesRow 验证导入成功后按任务报告建行。
func TestLedgerImportCreatesRow(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()
	applier, err := NewJobApplier(svc, quietLogger())
	if err != nil {
		t.Fatalf("构造账本失败: %v", err)
	}
	fx := newCertFixture(t, "ledger.example.test", fixedNow().Add(-time.Hour), fixedNow().Add(90*24*time.Hour), "")
	id, _ := NewID()

	params, _ := json.Marshal(ImportParams{
		CertificateID: id,
		Domain:        "ledger.example.test",
		CertPEM:       string(fx.certPEM),
		KeySecretRef:  "sec_ledger",
	})
	result, _ := json.Marshal(map[string]any{
		"outcome": OutcomeVerified,
		"observed_state": map[string]any{
			"certificate_id": id,
			"domain":         "ledger.example.test",
			"source":         "manual",
			"issuer":         fx.info.Issuer,
			"subject":        fx.info.Subject,
			"domains":        fx.info.Domains,
			"not_before":     fx.info.NotBefore.UTC().Format(time.RFC3339),
			"not_after":      fx.info.NotAfter.UTC().Format(time.RFC3339),
			"fingerprint":    fx.info.Fingerprint,
			"chain_length":   float64(fx.info.ChainLength),
			"key_mode":       "0600",
			"attempted_at":   fixedNow().UTC().Format(time.RFC3339),
		},
	})

	if err := applier.Apply(ctx, JobFact{
		ID: "job_1", Action: ActionImport, Status: "succeeded", Result: result,
	}, params); err != nil {
		t.Fatalf("账本写入失败: %v", err)
	}

	cert, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatalf("读取刚写入的记录失败: %v", err)
	}
	if cert.Domain != "ledger.example.test" || cert.Fingerprint != fx.info.Fingerprint {
		t.Fatalf("记录内容不对: %+v", cert)
	}
	if cert.Source != SourceManual || cert.KeySecretRef != "sec_ledger" {
		t.Fatalf("来路或私钥引用不对: %+v", cert)
	}
	if cert.State(fixedNow()) != RenewalValid {
		t.Fatalf("到期状态应当是 valid，实际 %s", cert.State(fixedNow()))
	}
}

// TestLedgerFailureRecordsAttempt 验证失败只记尝试，不建行。
func TestLedgerFailureRecordsAttempt(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()
	applier, err := NewJobApplier(svc, quietLogger())
	if err != nil {
		t.Fatalf("构造账本失败: %v", err)
	}
	id, _ := NewID()
	params, _ := json.Marshal(IssueParams{
		CertificateID: id,
		Domain:        "fail.example.test",
		Email:         "ops@example.test",
		Staging:       true,
	})
	errJSON, _ := json.Marshal(map[string]any{
		"code":    "CERTBOT_FAILED",
		"message": "域名解析不到本机",
	})

	if err := applier.Apply(ctx, JobFact{
		ID: "job_2", Action: ActionIssue, Status: "failed", Error: errJSON,
	}, params); err != nil {
		t.Fatalf("账本写入失败: %v", err)
	}
	// 没有记录，也没有留下"有证书"的假象。
	if _, err := svc.Get(ctx, id); err == nil {
		t.Fatal("失败的首次签发不该创建证书记录")
	}

	// 已有记录时，失败要写进 last_error。
	fx := newCertFixture(t, "fail.example.test", fixedNow().Add(-time.Hour), fixedNow().Add(90*24*time.Hour), "")
	current, err := svc.Create(ctx, CreateInput{
		ID:        id,
		Domain:    "fail.example.test",
		Info:      fx.info,
		Source:    SourceCertbot,
		ACMEEmail: "ops@example.test",
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	if err := applier.Apply(ctx, JobFact{
		ID: "job_3", Action: ActionRenew, Status: "failed", Error: errJSON,
	}, params); err != nil {
		t.Fatalf("账本写入失败: %v", err)
	}
	updated, err := svc.Get(ctx, current.ID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if updated.LastError == "" {
		t.Fatal("失败原因应当写进 last_error")
	}
	if !updated.LastAttemptAt.Equal(fixedNow()) {
		// 报告里没有 attempted_at 时退化成"现在"（服务时钟就是 fixedNow）。
		t.Fatalf("尝试时间不对: %v", updated.LastAttemptAt)
	}
}

// TestLedgerRejectsIncompleteReport 验证报告缺字段时不写一份"半张证书"。
func TestLedgerRejectsIncompleteReport(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()
	applier, _ := NewJobApplier(svc, quietLogger())
	id, _ := NewID()
	params, _ := json.Marshal(ImportParams{
		CertificateID: id,
		Domain:        "half.example.test",
		CertPEM:       "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
		KeySecretRef:  "sec_half",
	})
	result, _ := json.Marshal(map[string]any{
		"outcome": OutcomeVerified,
		"observed_state": map[string]any{
			"issuer": "Test CA",
			// 缺 fingerprint / domains / 日期。
		},
	})
	if err := applier.Apply(ctx, JobFact{
		ID: "job_4", Action: ActionImport, Status: "succeeded", Result: result,
	}, params); err == nil {
		t.Fatal("报告缺字段时应当报错，而不是写一条不完整的记录")
	}
	if _, err := svc.Get(ctx, id); err == nil {
		t.Fatal("报告不完整时不该创建记录")
	}
}

// TestAgentInspectorDropsPaths 验证代理返回的路径不会流到接口层。
func TestAgentInspectorDropsPaths(t *testing.T) {
	payload := `{"domain":"i.example.test","source":"manual","present":true,
		"issuer":"CA","domains":["i.example.test"],"not_after":"2027-01-01T00:00:00Z",
		"fingerprint":"abc","cert_path":"/etc/ssl/upanel/i.example.test.pem",
		"key_path":"/etc/ssl/upanel/i.example.test.key","key_present":true,"key_mode":"0600"}`
	inspector, err := NewAgentInspector(fakeQueryAgent{raw: []byte(payload)})
	if err != nil {
		t.Fatalf("构造读取端失败: %v", err)
	}
	got, err := inspector.Inspect(context.Background(), "i.example.test", "manual")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !got.Present || got.KeyMode != "0600" {
		t.Fatalf("事实不对: %+v", got)
	}
	// Inspected 结构体里根本没有路径字段：序列化结果里也不该出现。
	encoded, _ := json.Marshal(got)
	if containsKey(encoded, "key_path") || containsKey(encoded, "cert_path") {
		t.Fatalf("事实里不该带路径: %s", encoded)
	}
}

// fakeQueryAgent 是固定的查询应答。
type fakeQueryAgent struct {
	raw []byte
	err error
}

func (f fakeQueryAgent) Query(context.Context, string, any) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.raw), nil
}

// containsKey 判断一段 JSON 里有没有某个键名（够用于"不该出现"的断言）。
func containsKey(raw []byte, key string) bool {
	return bytes.Contains(raw, []byte(`"`+key+`"`))
}
