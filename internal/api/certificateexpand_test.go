package api

// P10.3 接口层用例：证书三个动作的预检参数展开。
//
// 展开是这套设计里“谁能决定参数”的分界线。调用方只给**意图**（域名、上传标识、
// 私钥引用、staging 选择），落进系统的参数由面板算：
//
//	certificate_id  按域名复用既有记录（站点绑定指向它），来路不同就 409
//	cert_pem        来自暂存区，不是请求体
//	email           来自记录或请求，两者都空就是 400
//	staging         必须显式给，缺字段就是 400（默认值会烧掉正式环境的速率限额）
//
// 还有一条横跨三者的规则：同一个域名一小时内只允许一次 ACME 尝试。这里用两种
// 来源各证明一次——任务表里的历史尝试，以及记录上的最后一次尝试时间。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/proxies"
	"upanel/internal/version"
)

// expandFixture 造一套带假代理的装置：展开成功时预检会真的走到代理。
func expandFixture(t *testing.T) (*adminFixture, *stubAgent) {
	t.Helper()
	agent := &stubAgent{plan: stubPlan()}
	return newPreflightFixture(t, agent), agent
}

// preflightPost 发一次预检，返回响应记录与错误码（没有错误时为空串）。
func preflightPost(t *testing.T, f *adminFixture, action, params string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", preflightBody(action, params))
	code := ""
	if body := parseResult(t, rec); body.Error != nil {
		code = body.Error.Code
	}
	return rec, code
}

// expandIssue 发一次 certificate.issue 预检并断言成功，返回展开后的参数。
func expandIssue(t *testing.T, agent *stubAgent) certificates.IssueParams {
	t.Helper()
	if len(agent.calls) != 1 {
		t.Fatalf("代理收到 %d 次预检，期望 1", len(agent.calls))
	}
	return decodeIssue(t, agent.calls[0].Params)
}

func decodeIssue(t *testing.T, raw json.RawMessage) certificates.IssueParams {
	t.Helper()
	var out certificates.IssueParams
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("展开后的签发参数无法解析: %v（%s）", err, raw)
	}
	return out
}

func decodeImport(t *testing.T, raw json.RawMessage) certificates.ImportParams {
	t.Helper()
	var out certificates.ImportParams
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("展开后的导入参数无法解析: %v（%s）", err, raw)
	}
	return out
}

// TestExpandIssueRequiresExplicitStaging 缺 staging 直接 400，且不惊动代理。
func TestExpandIssueRequiresExplicitStaging(t *testing.T) {
	f, agent := expandFixture(t)
	rec, code := preflightPost(t, f, certificates.ActionIssue,
		`{"domain":"issue.test","email":"ops@example.com"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %q，期望 %s", code, httpx.CodeValidationError)
	}
	if !strings.Contains(rec.Body.String(), "staging") {
		t.Fatalf("错误信息没有指出缺的是 staging：%s", rec.Body.String())
	}
	// 参数不对就不该生成预检：代理一次都不该被调用。
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// TestExpandIssueSendsServerComputedParams 展开后的参数由面板填好再交给代理。
func TestExpandIssueSendsServerComputedParams(t *testing.T) {
	f, agent := expandFixture(t)
	rec, code := preflightPost(t, f, certificates.ActionIssue,
		`{"domain":"Issue.TEST.","email":"ops@example.com","staging":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	got := expandIssue(t, agent)
	if got.Domain != "issue.test" {
		t.Fatalf("域名没有归一化: %q", got.Domain)
	}
	if got.Email != "ops@example.com" || !got.Staging {
		t.Fatalf("展开后的参数 = %+v", got)
	}
	// 还没有记录时标识由面板生成，形状必须是 crt_ 加 32 位小写十六进制。
	if !certificates.ValidID(got.CertificateID) {
		t.Fatalf("certificate_id = %q，形状不对", got.CertificateID)
	}
	// 响应里不得回显展开后的证书参数：那是给代理的，不是给前端的。
	if strings.Contains(rec.Body.String(), "ops@example.com") {
		t.Fatalf("预检响应不应回显邮箱：%s", rec.Body.String())
	}
}

// TestExpandIssueRejectsUnknownField 参数里多一个字段就拒绝，不猜意图。
func TestExpandIssueRejectsUnknownField(t *testing.T) {
	f, agent := expandFixture(t)
	rec, code := preflightPost(t, f, certificates.ActionIssue,
		`{"domain":"issue.test","email":"ops@example.com","staging":true,"server":"https://acme.example/directory"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %q", code)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// TestExpandIssueRejectsManualRecord 签发不能顶掉一条手动导入的记录。
func TestExpandIssueRejectsManualRecord(t *testing.T) {
	f, agent := expandFixture(t)
	seedCertificate(t, f, "taken.test", certificates.SourceManual,
		time.Now().UTC().Add(90*24*time.Hour), "")

	rec, code := preflightPost(t, f, certificates.ActionIssue,
		`{"domain":"taken.test","email":"ops@example.com","staging":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %q", code)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// TestExpandIssueReusesCertbotRecordIdentity 同一域名再次签发复用记录标识与邮箱。
func TestExpandIssueReusesCertbotRecordIdentity(t *testing.T) {
	f, agent := expandFixture(t)
	existing := seedCertificate(t, f, "reuse.test", certificates.SourceCertbot,
		time.Now().UTC().Add(20*24*time.Hour), "ops@example.com")

	rec, code := preflightPost(t, f, certificates.ActionIssue,
		`{"domain":"reuse.test","staging":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	got := expandIssue(t, agent)
	if got.CertificateID != existing.ID {
		t.Fatalf("certificate_id = %q，期望复用 %q", got.CertificateID, existing.ID)
	}
	if got.Email != "ops@example.com" {
		t.Fatalf("email = %q，期望沿用记录里的邮箱", got.Email)
	}
	if got.Staging {
		t.Fatalf("staging = true，请求里选的是正式环境")
	}
}

// TestExpandIssueRateLimitedByRecord 记录上的最后一次尝试时间在一小时内 → 429。
func TestExpandIssueRateLimitedByRecord(t *testing.T) {
	f, agent := expandFixture(t)
	existing := seedCertificate(t, f, "cooldown.test", certificates.SourceCertbot,
		time.Now().UTC().Add(90*24*time.Hour), "ops@example.com")
	// 模拟“刚试过一次失败”：限流依据是这条时间戳，不是任务表里的行。
	if _, err := f.db.SQL().Exec(
		`UPDATE certificates SET last_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), existing.ID); err != nil {
		t.Fatalf("准备尝试时间失败: %v", err)
	}

	rec, code := preflightPost(t, f, certificates.ActionRenew, `{"domain":"cooldown.test","staging":true}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeResourceBusy) {
		t.Fatalf("错误码 = %q，期望 %s", code, httpx.CodeResourceBusy)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("被限流时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// TestExpandRenewRequiresRecord 续一张不存在的证书没有意义。
func TestExpandRenewRequiresRecord(t *testing.T) {
	f, agent := expandFixture(t)
	rec, code := preflightPost(t, f, certificates.ActionRenew, `{"domain":"missing.test","staging":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeNotFound) {
		t.Fatalf("错误码 = %q", code)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// TestExpandRenewRejectsManualCertificate certbot 续不了手动导入的证书。
func TestExpandRenewRejectsManualCertificate(t *testing.T) {
	f, agent := expandFixture(t)
	seedCertificate(t, f, "manual.test", certificates.SourceManual,
		time.Now().UTC().Add(90*24*time.Hour), "")

	rec, code := preflightPost(t, f, certificates.ActionRenew, `{"domain":"manual.test","staging":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %q", code)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// TestExpandImportUsesStagedUpload 导入的公钥证书来自暂存区，私钥只以引用形式出现。
func TestExpandImportUsesStagedUpload(t *testing.T) {
	f, agent := expandFixture(t)
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	now := time.Now().UTC()
	certPEM, keyPEM := testCertPEM(t, []string{"import.test"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	upload := uploadCert(t, f, certPEM)
	if upload.Code != http.StatusCreated {
		t.Fatalf("上传失败: %d %s", upload.Code, upload.Body.String())
	}
	var staged certificateUploadResponse
	if err := json.Unmarshal(parseResult(t, upload).Data, &staged); err != nil {
		t.Fatalf("解析上传结果失败: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"domain":                 "Import.TEST",
		"certificate_upload_id":  staged.CertificateUploadID,
		"private_key_secret_ref": "sec_00112233445566778899aabbccddeeff",
		"staging":                false,
	})
	rec, code := preflightPost(t, f, certificates.ActionImport, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	if len(agent.calls) != 1 {
		t.Fatalf("代理收到 %d 次预检，期望 1", len(agent.calls))
	}
	got := decodeImport(t, agent.calls[0].Params)
	if got.Domain != "import.test" {
		t.Fatalf("域名没有归一化: %q", got.Domain)
	}
	if !certificates.ValidID(got.CertificateID) {
		t.Fatalf("certificate_id = %q", got.CertificateID)
	}
	if !strings.Contains(got.CertPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("展开后的 cert_pem 不是 PEM 证书")
	}
	if strings.Contains(got.CertPEM, "PRIVATE KEY") || strings.Contains(string(agent.calls[0].Params), string(keyPEM)) {
		t.Fatalf("展开后的参数里出现了私钥")
	}
	if got.KeySecretRef != "sec_00112233445566778899aabbccddeeff" {
		t.Fatalf("key_secret_ref = %q", got.KeySecretRef)
	}
	// 暂存是一次性的：用它生成过一次计划之后就没了。
	if n := f.srv.opts.CertificateStages.Len(); n != 0 {
		t.Fatalf("暂存里还剩 %d 条", n)
	}
}

// TestExpandImportReusesExistingRecord 同一域名再导入一次，标识不变（站点绑定跟着它）。
func TestExpandImportReusesExistingRecord(t *testing.T) {
	f, agent := expandFixture(t)
	existing := seedCertificate(t, f, "again.test", certificates.SourceManual,
		time.Now().UTC().Add(60*24*time.Hour), "")
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	now := time.Now().UTC()
	certPEM, _ := testCertPEM(t, []string{"again.test"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	upload := uploadCert(t, f, certPEM)
	if upload.Code != http.StatusCreated {
		t.Fatalf("上传失败: %d %s", upload.Code, upload.Body.String())
	}
	var staged certificateUploadResponse
	if err := json.Unmarshal(parseResult(t, upload).Data, &staged); err != nil {
		t.Fatalf("解析上传结果失败: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"domain":                 "again.test",
		"certificate_upload_id":  staged.CertificateUploadID,
		"private_key_secret_ref": "sec_00112233445566778899aabbccddeeff",
	})
	rec, code := preflightPost(t, f, certificates.ActionImport, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	if got := decodeImport(t, agent.calls[0].Params); got.CertificateID != existing.ID {
		t.Fatalf("certificate_id = %q，期望复用 %q", got.CertificateID, existing.ID)
	}
}

// TestExpandImportRejectsNonCoveringCertificate 证书不覆盖目标域名 → 400。
func TestExpandImportRejectsNonCoveringCertificate(t *testing.T) {
	f, agent := expandFixture(t)
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	now := time.Now().UTC()
	certPEM, _ := testCertPEM(t, []string{"other.test"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	upload := uploadCert(t, f, certPEM)
	if upload.Code != http.StatusCreated {
		t.Fatalf("上传失败: %d %s", upload.Code, upload.Body.String())
	}
	var staged certificateUploadResponse
	if err := json.Unmarshal(parseResult(t, upload).Data, &staged); err != nil {
		t.Fatalf("解析上传结果失败: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"domain":                 "wanted.test",
		"certificate_upload_id":  staged.CertificateUploadID,
		"private_key_secret_ref": "sec_00112233445566778899aabbccddeeff",
	})
	rec, code := preflightPost(t, f, certificates.ActionImport, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %q", code)
	}
	if !strings.Contains(rec.Body.String(), "other.test") {
		t.Fatalf("错误信息没有说明这张证书覆盖什么：%s", rec.Body.String())
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// TestExpandImportRequiresUploadAndKeyRef 缺上传标识或私钥引用都是 400，且不消费暂存。
func TestExpandImportRequiresUploadAndKeyRef(t *testing.T) {
	f, agent := expandFixture(t)
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	now := time.Now().UTC()
	certPEM, _ := testCertPEM(t, []string{"need.test"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	upload := uploadCert(t, f, certPEM)
	if upload.Code != http.StatusCreated {
		t.Fatalf("上传失败: %d %s", upload.Code, upload.Body.String())
	}
	var staged certificateUploadResponse
	if err := json.Unmarshal(parseResult(t, upload).Data, &staged); err != nil {
		t.Fatalf("解析上传结果失败: %v", err)
	}

	// 缺私钥引用。
	body, _ := json.Marshal(map[string]any{
		"domain":                "need.test",
		"certificate_upload_id": staged.CertificateUploadID,
	})
	rec, code := preflightPost(t, f, certificates.ActionImport, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺私钥引用状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %q", code)
	}

	// 缺上传标识。
	body, _ = json.Marshal(map[string]any{
		"domain":                 "need.test",
		"private_key_secret_ref": "sec_00112233445566778899aabbccddeeff",
	})
	rec, _ = preflightPost(t, f, certificates.ActionImport, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺上传标识状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}

	// 两次都没走到 Take：暂存还在，用户补上字段就能继续。
	if n := f.srv.opts.CertificateStages.Len(); n != 1 {
		t.Fatalf("暂存条目 = %d，期望 1（不该被失败的前置检查吃掉）", n)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

// newBindFixture 造一套“能建站点、能调代理、有证书服务”的装置。
//
// 为什么不用现成的夹具：绑定证书的站点需要三样东西同时在位——代理事实（否则建站点
// 直接 503）、代理调用（否则预检 503）、证书服务（否则 certificate_id 校验 503）。
// 这条用例要走的恰好是三者交叉的那一格。
func newBindFixture(t *testing.T) (*adminFixture, *stubAgent) {
	t.Helper()
	fake := &proxyFactsFake{facts: proxyFacts(nil)}
	authSvc, aud, db := newTestServicesWithDB(t)
	certSvc := newTestCertificates(t, db)
	proxySvc, err := proxies.New(db, proxies.Options{
		Facts:     fake,
		Apps:      newTestApps(t, db),
		Certs:     certSvc,
		Normalize: proxies.NormalizeOptions{PanelPort: 9443},
	})
	if err != nil {
		t.Fatalf("构造代理站点服务失败: %v", err)
	}
	agent := &stubAgent{plan: stubPlan()}
	srv, err := New(Options{
		ListenAddress:     config.ListenProduction,
		Mode:              platform.ModeProduction,
		Version:           version.Info{Version: "test", GoVersion: "test"},
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        newTestOperations(t, db, agent),
		Apps:              newTestApps(t, db),
		Proxies:           proxySvc,
		NginxFacts:        fake,
		PanelPort:         9443,
		Certificates:      certSvc,
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}, agent
}

// TestExpandProxyApplyAttachesCertificateMaterial 站点绑了证书时，apply 参数带上材料。
func TestExpandProxyApplyAttachesCertificateMaterial(t *testing.T) {
	f, agent := newBindFixture(t)
	cert := seedCertificate(t, f, "site.test", certificates.SourceManual,
		time.Now().UTC().Add(90*24*time.Hour), "")
	body := proxyBody("site.test")
	body["certificate_id"] = cert.ID
	site := mustCreateProxy(t, f, body)

	rec, code := preflightPost(t, f, "proxy.apply", `{"site_id":"`+site.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	if len(agent.calls) != 1 {
		t.Fatalf("代理收到 %d 次预检，期望 1", len(agent.calls))
	}
	// 材料里必须有证书与私钥的**引用**，但展开后的参数不经过 HTTP 回给前端。
	raw := string(agent.calls[0].Params)
	if !strings.Contains(raw, `"certificate"`) || !strings.Contains(raw, cert.ID) {
		t.Fatalf("apply 参数里没有挂上证书材料：%s", raw)
	}
}
