package api

// 受管理账号路由的 HTTP 层测试（P14.5）。
//
// 这一层只钉四件事：
//   1. 权限：GET /accounts 与 GET /accounts/:id/ssh-keys 整组 admin；
//   2. 形状：能力与列表一起返回、空公钥列表是 []、标识先过形状检查；
//   3. 错误翻译：代理拒绝原样带出（404 是 404）、代理不可用是 503；
//   4. 密钥体不出这个边界——Blob 在 DTO 上就是 json:"-"。
//
// 账号判决与执行由 internal/accounts 与 internal/agent 的测试覆盖；useradd、
// chpasswd、visudo、sudo -l 的真实行为只在独立 Ubuntu 虚拟机上验证。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/accounts"
	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/shares"
	"upanel/internal/version"
)

const (
	// apiRegularAccountID 是一个普通账号（type=regular），不是共享账号。
	apiRegularAccountID = "sac_00112233445566778899aabbccddeeff"
	// apiKeyID / apiKeyFingerprint 是同一把公钥的登记标识与指纹。
	apiKeyID          = "ssk_0102030405060708090a0b0c0d0e0f10"
	apiKeyFingerprint = "SHA256:Zm9vYmFyYmF6cXV1eGNvcmdlZ3JhdWx0Z2FycGx5"
)

// stubAccounts 是 AccountsReader 的测试替身。
type stubAccounts struct {
	capability accounts.Capability
	records    []shares.AccountRecord
	keys       []accounts.SSHKeyRecord

	listErr error
	keysErr error

	// lastKeyAccountID 记录最后一次公钥查询收到的账号标识：形状不对的标识
	// 不该打扰读取端，这条断言需要一个落脚点。
	lastKeyAccountID string
	keyCalls         int
}

func (s *stubAccounts) Accounts(context.Context) (accounts.Capability, []shares.AccountRecord, error) {
	if s.listErr != nil {
		return accounts.Capability{}, nil, s.listErr
	}
	return s.capability, s.records, nil
}

// availableAccountCapability 是一份“账号工具齐全”的能力报告。
//
// 它**不是** availableCapability()（那一份是 Samba 的）：账号页读的判断依据是
// useradd/getent/chpasswd/两档壳/visudo，两个工具集不相交。
func availableAccountCapability() accounts.Capability {
	return accounts.Capability{
		Status: accounts.CapabilityAvailable,
		Tools: accounts.AccountTools{
			Useradd:         "/usr/sbin/useradd",
			Usermod:         "/usr/sbin/usermod",
			Userdel:         "/usr/sbin/userdel",
			Getent:          "/usr/bin/getent",
			Passwd:          "/usr/bin/passwd",
			Chpasswd:        "/usr/sbin/chpasswd",
			Visudo:          "/usr/sbin/visudo",
			RestrictedShell: "/bin/rbash",
			DisabledShell:   "/usr/sbin/nologin",
		},
		SudoRuleWrite: "available",
		ShellProfiles: []string{"disabled", "restricted"},
		CollectedAt:   time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
	}
}

func (s *stubAccounts) SSHKeys(_ context.Context, accountID string) ([]accounts.SSHKeyRecord, error) {
	s.keyCalls++
	s.lastKeyAccountID = accountID
	if s.keysErr != nil {
		return nil, s.keysErr
	}
	return s.keys, nil
}

// accountsReaderStub 构造一个“可用、有一个普通账号、有一把公钥”的替身。
func accountsReaderStub() *stubAccounts {
	return &stubAccounts{
		capability: availableAccountCapability(),
		records:    []shares.AccountRecord{sampleRegularAccount()},
		keys:       []accounts.SSHKeyRecord{sampleSSHKey()},
	}
}

// sampleRegularAccount 是一个普通账号：有口令、有 /bin/bash、躺在 /home 下。
func sampleRegularAccount() shares.AccountRecord {
	rec := sampleAccount()
	rec.ID = apiRegularAccountID
	rec.Name = "alice"
	rec.DisplayName = "Alice"
	rec.Type = shares.AccountTypeRegular
	rec.UID = 1000
	rec.GID = 1000
	rec.Shell = "/bin/bash"
	rec.ShellProfile = "restricted"
	rec.Home = "/home/alice"
	rec.SecretRef = ""
	return rec
}

// sampleSSHKey 是一把已登记的公钥。
func sampleSSHKey() accounts.SSHKeyRecord {
	return accounts.SSHKeyRecord{
		ID:          apiKeyID,
		AccountID:   apiRegularAccountID,
		Type:        "ssh-ed25519",
		Blob:        "AAAAC3NzaC1lZDI1NTE5AAAAIB0examplekeyblob",
		Comment:     "alice@laptop",
		Fingerprint: apiKeyFingerprint,
		CreatedAt:   time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
	}
}

// accountsFixture 构造一个接上账号读端的服务。
func accountsFixture(t *testing.T, reader AccountsReader) (*Server, *http.Cookie, string, *auth.Service) {
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
		AllowedOrigins:    testOrigins(),
		Accounts:          reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return srv, cookie, csrf, srv.opts.Auth
}

// TestAccountRoutesRequireSessionAndAdmin 两条读路由都要登录，并且都只给 admin。
func TestAccountRoutesRequireSessionAndAdmin(t *testing.T) {
	srv, cookie, csrf, authSvc := accountsFixture(t, accountsReaderStub())

	paths := []string{
		"/api/v1/accounts",
		"/api/v1/accounts/" + apiRegularAccountID + "/ssh-keys",
	}
	for _, path := range paths {
		if rec := sharesDo(t, srv, nil, "", http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: 未登录状态码 = %d，期望 401", path, rec.Code)
		}
	}

	if _, err := authSvc.CreateUser(t.Context(), auth.CreateUserInput{
		Username: "bob", DisplayName: "bob", Role: auth.RoleViewer, Password: "bob-horse-9-ok",
	}); err != nil {
		t.Fatalf("建 viewer 失败: %v", err)
	}
	viewerCookie, viewerCSRF := loginAsUser(t, srv, "bob", "bob-horse-9-ok")
	for _, path := range paths {
		rec := sharesDo(t, srv, viewerCookie, viewerCSRF, http.MethodGet, path, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: viewer 状态码 = %d，期望 403（%s）", path, rec.Code, rec.Body.String())
		}
	}

	// 同一批地址 admin 能拿到内容（否则上面的 403 也可能只是路由写错了）。
	for _, path := range paths {
		if rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("%s: admin 状态码 = %d，期望 200（%s）", path, rec.Code, rec.Body.String())
		}
	}
}

// TestAccountListShape 列表带能力报告，并且普通账号的两列是新的（display_name/shell_profile）。
func TestAccountListShape(t *testing.T) {
	srv, cookie, csrf, _ := accountsFixture(t, accountsReaderStub())
	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data accountListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Capability.Available() {
		t.Errorf("能力 = %+v", body.Data.Capability)
	}
	if len(body.Data.Accounts) != 1 {
		t.Fatalf("账号数 = %d", len(body.Data.Accounts))
	}
	got := body.Data.Accounts[0]
	if got.ID != apiRegularAccountID || got.Name != "alice" {
		t.Errorf("账号字段不对：%+v", got)
	}
	if got.Type != shares.AccountTypeRegular || got.DisplayName != "Alice" || got.ShellProfile != "restricted" {
		t.Errorf("普通账号字段不对：type=%q display_name=%q shell_profile=%q",
			got.Type, got.DisplayName, got.ShellProfile)
	}
}

// TestAccountSSHKeysShape 公钥列表只带指纹与算法名，不带密钥体。
func TestAccountSSHKeysShape(t *testing.T) {
	srv, cookie, csrf, _ := accountsFixture(t, accountsReaderStub())
	path := "/api/v1/accounts/" + apiRegularAccountID + "/ssh-keys"
	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "AAAAC3NzaC1lZDI1NTE5") {
		t.Fatalf("响应里出现了密钥体：%s", rec.Body.String())
	}
	var body struct {
		Data accountKeysResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.AccountID != apiRegularAccountID {
		t.Errorf("account_id = %q", body.Data.AccountID)
	}
	if len(body.Data.SSHKeys) != 1 {
		t.Fatalf("公钥数 = %d", len(body.Data.SSHKeys))
	}
	if body.Data.SSHKeys[0].Fingerprint != apiKeyFingerprint {
		t.Errorf("指纹 = %q", body.Data.SSHKeys[0].Fingerprint)
	}
}

// TestAccountSSHKeysEmptyIsArray 空列表是 [] 而不是 null——“没配”不能被读成“不知道”。
func TestAccountSSHKeysEmptyIsArray(t *testing.T) {
	reader := accountsReaderStub()
	reader.keys = nil
	srv, cookie, csrf, _ := accountsFixture(t, reader)
	path := "/api/v1/accounts/" + apiRegularAccountID + "/ssh-keys"
	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	raw, ok := body.Data["ssh_keys"]
	if !ok {
		t.Fatal("响应缺少 ssh_keys 字段")
	}
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("ssh_keys = %s，期望 []", strings.TrimSpace(string(raw)))
	}
}

// TestAccountSSHKeysRejectsBadID 形状不对的标识连读取端都不该被打扰。
func TestAccountSSHKeysRejectsBadID(t *testing.T) {
	reader := accountsReaderStub()
	srv, cookie, csrf, _ := accountsFixture(t, reader)
	for _, id := range []string{"alice", "SAC_00112233445566778899aabbccddeeff", "sac_short"} {
		rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/accounts/"+id+"/ssh-keys", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d，期望 400（%s）", id, rec.Code, rec.Body.String())
		}
	}
	if reader.keyCalls != 0 {
		t.Errorf("形状不对的标识打到了读取端 %d 次", reader.keyCalls)
	}
}

// TestAccountRoutesTranslateErrors 代理拒绝原样带出，代理不可用是 503。
func TestAccountRoutesTranslateErrors(t *testing.T) {
	path := "/api/v1/accounts/" + apiRegularAccountID + "/ssh-keys"

	notFound := accountsReaderStub()
	notFound.keysErr = &shares.RemoteError{
		Status: http.StatusNotFound, Code: httpx.CodeNotFound, Message: "账号不存在",
	}
	srv, cookie, csrf, _ := accountsFixture(t, notFound)
	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, path, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("配置了 404 的读取端：状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}

	down := accountsReaderStub()
	down.keysErr = errors.Join(accounts.ErrAgentUnavailable, errors.New("socket closed"))
	srv, cookie, csrf, _ = accountsFixture(t, down)
	rec = sharesDo(t, srv, cookie, csrf, http.MethodGet, path, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("代理不可用：状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}

	// 未配置代理时是 503，不是空列表。
	srv, cookie, csrf, _ = accountsFixture(t, nil)
	rec = sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/accounts", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未配置代理：状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
}
