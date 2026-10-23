package api

// P10.3 接口层用例：证书上传、列表、详情与“现场读不到”的降级。
//
// 这一层证明的是四件在 HTTP 边界上会出错、一旦出错就没别处可拦的事：
//
//  1. **上传不等于导入**。POST /certificates/uploads 只进暂存区，证书表一行都不写；
//     导入必须走 certificate.import 任务（预检 → 执行 → 代理落盘 → 账本写入）。
//  2. **私钥在这一层就被挡住**。夹带 PRIVATE KEY 的上传直接 400，而不是“忽略多余块”——
//     忽略意味着私钥已经进了进程内存和可能的日志。
//  3. **任何 DTO 都不含私钥路径或内容**。台账里明明存着 cert_path / key_path / secret ref，
//     出站必须一个都不带；这里用“响应体里搜关键字”的办法守住它。
//  4. **读不到现场不等于读不到记录**。CertificateState 为 nil 或代理不可用时，
//     详情仍然是 200，只是 observed.available=false 加一句原因。

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
	"math/big"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/tlscert"
	"upanel/internal/version"
)

// testCertPEM 生成一张真实的自签证书（域名与有效期可定），返回证书 PEM 与私钥 PEM。
//
// 不写死 PEM：有效期写死会让用例在某个日期之后自己变红，而“证书过期”恰好是本模块
// 的核心判据之一，用一张过期的固定证书会把别的问题掩盖掉。
func testCertPEM(t *testing.T, domains []string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	if len(domains) == 0 {
		t.Fatalf("至少要有一个域名")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("生成序列号失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: domains[0], Organization: []string{"uPanel Test"}},
		DNSNames:              domains,
		NotBefore:             notBefore.UTC(),
		NotAfter:              notAfter.UTC(),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发测试证书失败: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// mustCertInfo 把 PEM 解析成证书事实（与生产路径同一个解析器）。
func mustCertInfo(t *testing.T, certPEM []byte) tlscert.Info {
	t.Helper()
	info, err := tlscert.ParseCertificate(certPEM)
	if err != nil {
		t.Fatalf("解析测试证书失败: %v", err)
	}
	return info
}

// postMultipart 发送一次 multipart 上传。
func postMultipart(t *testing.T, srv *Server, path, field, filename string, content []byte,
	cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("构造表单失败: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("写入表单失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("结束表单失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if cookie != nil {
		req.Header.Set("Cookie", cookie.String())
	}
	if csrf != "" {
		req.Header.Set(CSRFTokenHeader, csrf)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// uploadCert 以管理员身份上传一份证书（调用前需已再认证）。
func uploadCert(t *testing.T, f *adminFixture, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	return postMultipart(t, f.srv, "/api/v1/certificates/uploads", uploadFieldName, "cert.pem",
		content, f.cookie, f.csrf)
}

// certPathsInBody 找出响应体里出现的私钥/路径痕迹。返回命中的关键字。
func certPathsInBody(body string) []string {
	keywords := []string{
		"PRIVATE KEY", "privkey", "key_path", "cert_path", "key_secret_ref",
		"secret_ref", "/etc/ssl", "/etc/letsencrypt", ".key",
	}
	var hits []string
	for _, kw := range keywords {
		if strings.Contains(body, kw) {
			hits = append(hits, kw)
		}
	}
	return hits
}

// TestUploadCertificateStagesOnly 上传只进暂存区：证书表一行都不写，响应里没有内容。
func TestUploadCertificateStagesOnly(t *testing.T) {
	f := newAdminFixture(t)
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	now := time.Now().UTC()
	certPEM, _ := testCertPEM(t, []string{"upanel.test"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	rec := uploadCert(t, f, certPEM)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}

	var out certificateUploadResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析上传结果失败: %v", err)
	}
	if !certificates.ValidStageID(out.CertificateUploadID) {
		t.Fatalf("certificate_upload_id = %q，形状不对", out.CertificateUploadID)
	}
	if len(out.Domains) != 1 || out.Domains[0] != "upanel.test" {
		t.Fatalf("覆盖域名 = %v", out.Domains)
	}
	if out.Staging || out.SelfSigned == false {
		t.Fatalf("测试证书应被识别为自签、非 staging：%+v", out)
	}
	expiresAt, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at 不是 RFC3339: %q", out.ExpiresAt)
	}
	if d := time.Until(expiresAt); d <= 0 || d > certificates.StageTTL+time.Minute {
		t.Fatalf("暂存到期时间不合理: %v", d)
	}

	// 内容与路径都不出站。
	if hits := certPathsInBody(rec.Body.String()); len(hits) > 0 {
		t.Fatalf("上传响应里出现了不该有的字段 %v：%s", hits, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("上传响应不应回显证书内容: %s", rec.Body.String())
	}

	// 上传不等于导入：暂存区有 1 条，证书表 0 行。
	if got := f.srv.opts.CertificateStages.Len(); got != 1 {
		t.Fatalf("暂存条目 = %d，期望 1", got)
	}
	var rows int
	if err := f.db.SQL().QueryRow(`SELECT COUNT(*) FROM certificates`).Scan(&rows); err != nil {
		t.Fatalf("查询证书表失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("证书表行数 = %d，期望 0（上传不写库）", rows)
	}
}

// TestUploadCertificateRejectsPrivateKey 夹带私钥的上传直接拒绝，且不留暂存。
func TestUploadCertificateRejectsPrivateKey(t *testing.T) {
	f := newAdminFixture(t)
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	now := time.Now().UTC()
	certPEM, keyPEM := testCertPEM(t, []string{"upanel.test"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))

	// fullchain + privkey 一起粘进上传框是最常见的误操作。
	rec := uploadCert(t, f, append(append([]byte(nil), certPEM...), keyPEM...))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if code := parseResult(t, rec); code.Error == nil || code.Error.Code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %+v，期望 %s", code.Error, httpx.CodeValidationError)
	}
	if got := f.srv.opts.CertificateStages.Len(); got != 0 {
		t.Fatalf("被拒的上传留下了 %d 条暂存", got)
	}
}

// TestUploadCertificateRequiresAdminCSRFAndReauth 三条门禁各挡一次。
func TestUploadCertificateRequiresAdminCSRFAndReauth(t *testing.T) {
	f := newAdminFixture(t)
	now := time.Now().UTC()
	certPEM, _ := testCertPEM(t, []string{"upanel.test"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))

	// 1) 没再认证：403 REAUTH_REQUIRED。
	rec := uploadCert(t, f, certPEM)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未再认证状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if body := parseResult(t, rec); body.Error == nil || body.Error.Code != string(httpx.CodeReauthRequired) {
		t.Fatalf("未再认证错误码 = %+v，期望 %s", body.Error, httpx.CodeReauthRequired)
	}

	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	// 2) 没有 CSRF 头：403。
	rec = postMultipart(t, f.srv, "/api/v1/certificates/uploads", uploadFieldName, "cert.pem",
		certPEM, f.cookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}

	// 3) viewer：403 FORBIDDEN（管理员权限检查在再认证之前）。
	f.createUser(t, "cert-viewer", string(auth.RoleViewer), "viewer-pass-9")
	cookie, csrf := loginAsUser(t, f.srv, "cert-viewer", "viewer-pass-9")
	rec = postMultipart(t, f.srv, "/api/v1/certificates/uploads", uploadFieldName, "cert.pem",
		certPEM, cookie, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if body := parseResult(t, rec); body.Error == nil || body.Error.Code != string(httpx.CodeForbidden) {
		t.Fatalf("viewer 错误码 = %+v，期望 %s", body.Error, httpx.CodeForbidden)
	}
	if got := f.srv.opts.CertificateStages.Len(); got != 0 {
		t.Fatalf("被门禁挡下的上传留下了 %d 条暂存", got)
	}
}

// seedCertificate 直接建一条证书记录（相当于某个任务已经跑完、账本写过一次）。
//
// 尝试时间往前放两天：建记录本身就意味着“刚刚试过一次”，而限流看的就是这个时间戳。
// 需要测限流的用例再自己把 last_attempt_at 改到刚刚。
func seedCertificate(t *testing.T, f *adminFixture, domain string, source certificates.Source,
	notAfter time.Time, email string) certificates.Certificate {
	t.Helper()
	now := time.Now().UTC()
	certPEM, _ := testCertPEM(t, []string{domain}, now.Add(-48*time.Hour), notAfter)
	in := certificates.CreateInput{
		Domain:    domain,
		Info:      mustCertInfo(t, certPEM),
		Source:    source,
		AttemptAt: now.Add(-48 * time.Hour),
	}
	if source == certificates.SourceManual {
		in.KeySecretRef = "sec_00112233445566778899aabbccddeeff"
	} else {
		in.ACMEEmail = email
	}
	cert, err := f.srv.opts.Certificates.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("建证书记录失败: %v", err)
	}
	return cert
}

// TestGetCertificateWithoutStateReader 没有代理读取端时详情仍然 200，observed 说明原因。
func TestGetCertificateWithoutStateReader(t *testing.T) {
	f := newAdminFixture(t)
	cert := seedCertificate(t, f, "upanel.test", certificates.SourceManual,
		time.Now().UTC().Add(90*24*time.Hour), "")

	rec := f.do(t, http.MethodGet, "/api/v1/certificates/"+cert.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var detail certificateDetailResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &detail); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if detail.Certificate.ID != cert.ID || detail.Certificate.Domain != "upanel.test" {
		t.Fatalf("详情 = %+v", detail.Certificate)
	}
	if detail.Certificate.RenewalState != string(certificates.RenewalValid) {
		t.Fatalf("renewal_state = %q，期望 valid", detail.Certificate.RenewalState)
	}
	// 手动导入的私钥在秘密存储里，证书本身不在 certbot 的目录下。
	if detail.Certificate.KeyStorage != "secret_store" {
		t.Fatalf("key_storage = %q，期望 secret_store", detail.Certificate.KeyStorage)
	}
	if detail.Observed == nil || detail.Observed.Available {
		t.Fatalf("observed = %+v，期望 available=false", detail.Observed)
	}
	if !strings.Contains(detail.Observed.Reason, "代理") {
		t.Fatalf("observed.reason 没有说明原因: %q", detail.Observed.Reason)
	}
	if hits := certPathsInBody(rec.Body.String()); len(hits) > 0 {
		t.Fatalf("详情响应里出现了不该有的字段 %v：%s", hits, rec.Body.String())
	}
}

// stubCertState 是可编排的现场读取端。
type stubCertState struct {
	inspected certificates.Inspected
	err       error
	calls     []string
}

func (s *stubCertState) Inspect(_ context.Context, domain, source string) (certificates.Inspected, error) {
	s.calls = append(s.calls, domain+":"+source)
	if s.err != nil {
		return certificates.Inspected{}, s.err
	}
	return s.inspected, nil
}

// newCertificateFixture 构造带现场读取端的装置。
func newCertificateFixture(t *testing.T, state CertificateStateReader) *adminFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:     config.ListenProduction,
		Mode:              platform.ModeProduction,
		Version:           version.Info{Version: "test", GoVersion: "test"},
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        newTestOperations(t, db, nil),
		Apps:              newTestApps(t, db),
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		CertificateState:  state,
		AllowedOrigins:    testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// TestGetCertificateReportsObservedKeyMode 现场事实里只有“在不在、mode 多少”，没有路径。
func TestGetCertificateReportsObservedKeyMode(t *testing.T) {
	state := &stubCertState{inspected: certificates.Inspected{
		Domain:      "upanel.test",
		Source:      string(certificates.SourceManual),
		Present:     true,
		Issuer:      "uPanel Test",
		Domains:     []string{"upanel.test"},
		NotBefore:   "2026-10-20T00:00:00Z",
		NotAfter:    "2027-01-18T00:00:00Z",
		Fingerprint: strings.Repeat("a", 64),
		ChainLength: 1,
		Expiring:    true,
		KeyPresent:  true,
		KeyMode:     "0600",
	}}
	f := newCertificateFixture(t, state)
	cert := seedCertificate(t, f, "upanel.test", certificates.SourceManual,
		time.Now().UTC().Add(90*24*time.Hour), "")

	rec := f.do(t, http.MethodGet, "/api/v1/certificates/"+cert.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var detail certificateDetailResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &detail); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if detail.Observed == nil || !detail.Observed.Available {
		t.Fatalf("observed = %+v，期望 available=true", detail.Observed)
	}
	if !detail.Observed.KeyPresent || detail.Observed.KeyMode != "0600" {
		t.Fatalf("私钥事实 = %+v / %q", detail.Observed.KeyPresent, detail.Observed.KeyMode)
	}
	if len(state.calls) != 1 || state.calls[0] != "upanel.test:manual" {
		t.Fatalf("现场查询参数 = %v", state.calls)
	}
	if hits := certPathsInBody(rec.Body.String()); len(hits) > 0 {
		t.Fatalf("详情响应里出现了不该有的字段 %v：%s", hits, rec.Body.String())
	}
}

// TestGetCertificateDegradesWhenStateFails 代理报错只让 observed 不可用，详情照常。
func TestGetCertificateDegradesWhenStateFails(t *testing.T) {
	state := &stubCertState{err: context.DeadlineExceeded}
	f := newCertificateFixture(t, state)
	cert := seedCertificate(t, f, "upanel.test", certificates.SourceCertbot,
		time.Now().UTC().Add(90*24*time.Hour), "ops@example.com")

	rec := f.do(t, http.MethodGet, "/api/v1/certificates/"+cert.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var detail certificateDetailResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &detail); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if detail.Observed == nil || detail.Observed.Available {
		t.Fatalf("observed = %+v，期望 available=false", detail.Observed)
	}
	// certbot 的私钥由 certbot 自己保管，面板连读都不读。
	if detail.Certificate.KeyStorage != "certbot" {
		t.Fatalf("key_storage = %q，期望 certbot", detail.Certificate.KeyStorage)
	}
	if detail.Certificate.ACMEEmail != "ops@example.com" {
		t.Fatalf("acme_email = %q", detail.Certificate.ACMEEmail)
	}
}

// TestGetCertificateNotFound 形状不对的 id 与不存在的 id 都是 404。
func TestGetCertificateNotFound(t *testing.T) {
	f := newAdminFixture(t)
	for _, id := range []string{"crt_00112233445566778899aabbccddeeff", "not-an-id"} {
		rec := f.do(t, http.MethodGet, "/api/v1/certificates/"+id, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，期望 404；响应 %s", id, rec.Code, rec.Body.String())
		}
		if body := parseResult(t, rec); body.Error == nil || body.Error.Code != string(httpx.CodeNotFound) {
			t.Fatalf("%s 错误码 = %+v", id, body.Error)
		}
	}
}

// TestListCertificatesFiltersByNormalizedDomain 列表按域名过滤，域名为大写也能命中。
func TestListCertificatesFiltersByNormalizedDomain(t *testing.T) {
	f := newAdminFixture(t)
	alpha := seedCertificate(t, f, "alpha.test", certificates.SourceManual,
		time.Now().UTC().Add(90*24*time.Hour), "")
	seedCertificate(t, f, "beta.test", certificates.SourceCertbot,
		time.Now().UTC().Add(90*24*time.Hour), "ops@example.com")

	rec := f.do(t, http.MethodGet, "/api/v1/certificates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var list certificateListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("列表条数 = %d，期望 2", len(list.Items))
	}
	if hits := certPathsInBody(rec.Body.String()); len(hits) > 0 {
		t.Fatalf("列表响应里出现了不该有的字段 %v：%s", hits, rec.Body.String())
	}

	rec = f.do(t, http.MethodGet, "/api/v1/certificates?domain=ALPHA.test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("过滤状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析过滤结果失败: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != alpha.ID {
		t.Fatalf("过滤结果 = %+v", list.Items)
	}
	if list.Items[0].KeyStorage != "secret_store" || list.Items[0].Source != string(certificates.SourceManual) {
		t.Fatalf("列表项 = %+v", list.Items[0])
	}

	// 形状不合法的域名是 400，不是"没找到"。
	rec = f.do(t, http.MethodGet, "/api/v1/certificates?domain="+strings.Repeat("a", 300), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法域名状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
}
