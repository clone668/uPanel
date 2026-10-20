package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/version"
)

// newSecretServer 构造一个带真实秘密存储的代理服务，并把存储一并返回以便查库。
func newSecretServer(t *testing.T, opts agentstore.SecretsOptions) (*Server, *agentstore.Secrets) {
	t.Helper()
	secrets := newTestSecretStoreWith(t, opts)
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Secrets:         secrets,
		Plans:           newTestPlanStore(t),
		Operations:      newTestOperationStore(t),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv, secrets
}

// postSecret 发送一次 POST /internal/v1/secrets。
func postSecret(t *testing.T, srv *Server, uid uint32, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/secrets", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	return serveAs(srv, req, uid)
}

var secretRefPattern = regexp.MustCompile(`^sec_[0-9a-f]{32}$`)

func TestPostSecretReturnsRefWithoutPlaintext(t *testing.T) {
	srv, secrets := newSecretServer(t, agentstore.SecretsOptions{})
	const plaintext = "Tr0ub4dor&3-plain"

	rec := postSecret(t, srv, testWebUID,
		`{"purpose":"smb_password","value":"`+plaintext+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), plaintext) {
		t.Fatalf("响应体里出现了明文: %s", rec.Body.String())
	}

	var body struct {
		Data struct {
			SecretRef string `json:"secret_ref"`
			Purpose   string `json:"purpose"`
			State     string `json:"state"`
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !secretRefPattern.MatchString(body.Data.SecretRef) {
		t.Fatalf("secret_ref 形态不合法: %q", body.Data.SecretRef)
	}
	if body.Data.Purpose != "smb_password" {
		t.Fatalf("purpose = %q", body.Data.Purpose)
	}
	if body.Data.State != agentstore.StateTemporary {
		t.Fatalf("未带引用时应为临时秘密，实际 %q", body.Data.State)
	}
	expiresAt, err := time.Parse(time.RFC3339, body.Data.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at 不是 RFC3339: %q", body.Data.ExpiresAt)
	}
	if d := time.Until(expiresAt); d <= 0 || d > agentstore.TemporarySecretTTL+time.Minute {
		t.Fatalf("到期时间不合理: %s（距今 %s）", expiresAt, d)
	}
	if body.RequestID == "" {
		t.Fatal("响应缺少 request_id")
	}

	// 代理内部能解出来，说明密文是真的可用的密文。
	plain, err := secrets.Resolve(reqContext(t), body.Data.SecretRef)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(plain) != plaintext {
		t.Fatalf("解密结果 = %q，期望 %q", string(plain), plaintext)
	}
}

func TestPostSecretWithReferenceIsPersistent(t *testing.T) {
	srv, _ := newSecretServer(t, agentstore.SecretsOptions{})
	rec := postSecret(t, srv, testWebUID,
		`{"purpose":"backup_repository_password","value":"repo-pass","reference":"backup.repository.1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	var raw struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	data := raw.Data
	if data["state"] != agentstore.StatePersistent {
		t.Fatalf("state = %v，期望 %s", data["state"], agentstore.StatePersistent)
	}
	if _, ok := data["expires_at"]; ok {
		t.Fatal("持久秘密不应返回 expires_at")
	}
}

func TestPostSecretRejectsGetAndNoReadRoute(t *testing.T) {
	srv, _ := newSecretServer(t, agentstore.SecretsOptions{})

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/secrets", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态码 = %d，期望 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q，期望 POST", allow)
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
		t.Fatalf("405 应带契约错误码: %s", rec.Body.String())
	}

	// 单条秘密的读取路径根本不存在：不能有“按 ref 读回来”的入口。
	for _, path := range []string{
		"/internal/v1/secrets/sec_0123456789abcdef0123456789abcdef",
		"/internal/v1/secrets/sec_0123456789abcdef0123456789abcdef/value",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := serveAs(srv, req, testWebUID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，期望 404（不存在读取明文的接口）", path, rec.Code)
		}
	}
}

func TestPostSecretValidatesInput(t *testing.T) {
	srv, _ := newSecretServer(t, agentstore.SecretsOptions{})

	cases := []struct {
		name       string
		payload    string
		wantStatus int
		wantText   string
	}{
		{"未登记用途", `{"purpose":"root_shell","value":"x"}`, http.StatusBadRequest, "未登记的用途"},
		{"内部用途", `{"purpose":"totp_seed","value":"JBSWY3DPEHPK3PXP"}`, http.StatusBadRequest, "代理内部流程"},
		{"空明文", `{"purpose":"smb_password","value":""}`, http.StatusBadRequest, "不能为空"},
		{"缺明文", `{"purpose":"smb_password"}`, http.StatusBadRequest, "不能为空"},
		{"未知字段", `{"purpose":"smb_password","value":"x","extra":1}`, http.StatusBadRequest, "unknown field"},
		{"非法引用", `{"purpose":"smb_password","value":"x","reference":"bad\nref"}`, http.StatusBadRequest, "引用标识"},
		{"空请求体", ``, http.StatusBadRequest, "请求体不能为空"},
		{"不是对象", `[]`, http.StatusBadRequest, "参数不合法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postSecret(t, srv, testWebUID, tc.payload)
			if rec.Code != tc.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d；响应 %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantText) {
				t.Fatalf("响应未包含 %q: %s", tc.wantText, rec.Body.String())
			}
		})
	}
}

func TestPostSecretEnforcesSizeLimits(t *testing.T) {
	srv, _ := newSecretServer(t, agentstore.SecretsOptions{})

	// 用途自身的上限（smb_password 4 KiB）。
	tooBig := `{"purpose":"smb_password","value":"` + strings.Repeat("a", 5<<10) + `"}`
	rec := postSecret(t, srv, testWebUID, tooBig)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长明文状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}

	// 请求体本身的上限（远远超过任何合法秘密）。
	huge := `{"purpose":"app_env","value":"` + strings.Repeat("a", maxSecretRequestBytes) + `"}`
	rec = postSecret(t, srv, testWebUID, huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体状态码 = %d，期望 413", rec.Code)
	}
}

func TestPostSecretRequiresJSONContentType(t *testing.T) {
	srv, _ := newSecretServer(t, agentstore.SecretsOptions{})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/secrets",
		strings.NewReader(`{"purpose":"smb_password","value":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("状态码 = %d，期望 415", rec.Code)
	}
}

func TestPostSecretDeniedForOtherUIDs(t *testing.T) {
	srv, _ := newSecretServer(t, agentstore.SecretsOptions{})
	const payload = `{"purpose":"smb_password","value":"x"}`

	// root 也不能走普通接口：它只能用单独限定的专用路由。
	for _, uid := range []uint32{0, testWebUID + 1} {
		rec := postSecret(t, srv, uid, payload)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("UID %d 状态码 = %d，期望 403", uid, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "smb_password") {
			t.Fatalf("拒绝响应不应回显请求内容: %s", rec.Body.String())
		}
	}
}

// secretRefOf 从 POST /internal/v1/secrets 的响应里取出 secret_ref。
func secretRefOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Data struct {
			SecretRef string `json:"secret_ref"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.SecretRef == "" {
		t.Fatalf("响应里没有 secret_ref: %s", rec.Body.String())
	}
	return body.Data.SecretRef
}

func TestSweepExpiredSecrets(t *testing.T) {
	// 真实等待而不注入时钟：验证的是“服务端扫描器用的就是真实时间”。
	//
	// 分成两块而不是一条时间线，是因为这里曾经偶发失败（宿主高负载下重现过一次）：
	// TTL 50ms、POST 之后立刻扫描，断言“一条都没销毁”，可一旦机器忙，POST 到扫描之间
	// 就超过 50ms，秘密在第一次扫描前已到期，断言变成 false。
	// 所以“未到期不销毁”用一条 TTL 1 小时的秘密来断言（差几个毫秒不会翻转），
	// “到期后销毁”另开一条 TTL 50ms 的秘密并等满 80ms。两条合起来仍然覆盖
	// 扫描器的两个方向，而且不依赖机器忙不忙。
	//
	// 第一块：未到期的秘密不能被销毁。
	longSrv, longSecrets := newSecretServer(t, agentstore.SecretsOptions{TTL: time.Hour})
	rec := postSecret(t, longSrv, testWebUID, `{"purpose":"notification_password","value":"keep-me"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	longRef := secretRefOf(t, rec)
	removed, err := longSrv.SweepExpiredSecrets(reqContext(t))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 0 {
		t.Fatalf("未到期就销毁了 %d 条", removed)
	}
	if _, err := longSecrets.Resolve(reqContext(t), longRef); err != nil {
		t.Fatalf("未到期的秘密应当还能解密: %v", err)
	}

	// 第二块：到期之后必须销毁，且只销毁一次。
	srv, secrets := newSecretServer(t, agentstore.SecretsOptions{TTL: 50 * time.Millisecond})
	rec = postSecret(t, srv, testWebUID, `{"purpose":"notification_password","value":"mail-pass"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	body := struct {
		Data struct {
			SecretRef string `json:"secret_ref"`
		} `json:"data"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	time.Sleep(80 * time.Millisecond)

	removed, err = srv.SweepExpiredSecrets(reqContext(t))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("销毁条数 = %d，期望 1", removed)
	}
	if _, err := secrets.Resolve(reqContext(t), body.Data.SecretRef); err == nil {
		t.Fatal("被销毁的秘密不应还能解密")
	}

	removed, err = srv.SweepExpiredSecrets(reqContext(t))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 0 {
		t.Fatalf("重复扫描销毁了 %d 条", removed)
	}
}

func TestSecretsRouteRejectsOtherMethods(t *testing.T) {
	srv, _ := newSecretServer(t, agentstore.SecretsOptions{})
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/internal/v1/secrets", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := serveAs(srv, req, testWebUID)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s 状态码 = %d，期望 405", method, rec.Code)
		}
	}
}
