package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/totp"
	"upanel/internal/version"
)

// newTotpServer 构造一个接线了二次验证的代理服务。
//
// Secrets 同时充当 Secrets 与 Totp：生产也是这么接的（同一个 *agentstore.Secrets），
// 而 TotpSecrets 是单独声明的接口，接线处必须显式赋值——这正是要测的形状。
func newTotpServer(t *testing.T, opts agentstore.SecretsOptions) (*Server, *agentstore.Secrets) {
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
		Totp:            secrets,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv, secrets
}

// totpEnvelope 是成功响应的信封。用泛型是为了每个接口拿到自己的 data 类型。
type totpEnvelope[T any] struct {
	Data      T      `json:"data"`
	RequestID string `json:"request_id"`
}

// postTotp 以 Web 账号身份调用一个 totp 接口。
func postTotp(t *testing.T, srv *Server, uid uint32, path, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	return serveAs(srv, req, uid)
}

// enrollTotp 走一次登记，返回引用与明文种子（种子从 otpauth URI 里取回来——
// 响应里除了这条 URI 没有别的地方能拿到它，这个测试也顺便钉住这一点）。
func enrollTotp(t *testing.T, srv *Server, userID int64, username string) (ref, seed string) {
	t.Helper()
	payload := `{"user_id":` + itoa(int(userID)) + `,"username":"` + username + `"}`
	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/enroll", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enroll 状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	var env totpEnvelope[totpEnrollResponse]
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("enroll 响应不是合法 JSON: %v", err)
	}
	if !secretRefPattern.MatchString(env.Data.SecretRef) {
		t.Fatalf("secret_ref 形态不合法: %q", env.Data.SecretRef)
	}
	parsed, err := url.Parse(env.Data.OTPAuthURI)
	if err != nil {
		t.Fatalf("otpauth_uri 不是合法 URI: %v", err)
	}
	return env.Data.SecretRef, parsed.Query().Get("secret")
}

// wrongCode 返回一个与正确码必然不同的 6 位码，用来测“码不对但不是畸形输入”。
func wrongCode(t *testing.T, seed string) string {
	t.Helper()
	correct, err := totp.Code(seed, time.Now().UTC())
	if err != nil {
		t.Fatalf("计算正确码失败: %v", err)
	}
	if correct == "000000" {
		return "111111"
	}
	return "000000"
}

func TestTotpEnrollCreatesTemporarySeedAndShowsItOnlyInTheURI(t *testing.T) {
	srv, secrets := newTotpServer(t, agentstore.SecretsOptions{})

	payload := `{"user_id":7,"username":"alice"}`
	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/enroll", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}

	// 响应里除了 otpauth_uri，不许有任何字段装着种子。逐键核对，而不是"没看见就算没有"。
	var raw struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	want := map[string]bool{
		"secret_ref": true, "otpauth_uri": true, "issuer": true,
		"account": true, "digits": true, "period": true, "expires_at": true,
	}
	for key := range raw.Data {
		if !want[key] {
			t.Fatalf("响应里出现了未登记的字段 %q：%s", key, rec.Body.String())
		}
	}
	for key := range want {
		if _, ok := raw.Data[key]; !ok {
			t.Fatalf("响应缺少字段 %q：%s", key, rec.Body.String())
		}
	}

	var env totpEnvelope[totpEnrollResponse]
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if env.Data.Issuer != totpIssuer {
		t.Fatalf("issuer = %q，期望 %q", env.Data.Issuer, totpIssuer)
	}
	if env.Data.Account != "alice" {
		t.Fatalf("account = %q", env.Data.Account)
	}
	if env.Data.Digits != totp.Digits || env.Data.Period != int(totp.Period.Seconds()) {
		t.Fatalf("digits/period = %d/%d", env.Data.Digits, env.Data.Period)
	}

	// 未确认 = 临时秘密：没有引用，带到期时间，10 分钟内自己消失。
	purpose, reference, found, err := secrets.Binding(t.Context(), env.Data.SecretRef)
	if err != nil || !found {
		t.Fatalf("刚写进去的种子查不到：found=%v err=%v", found, err)
	}
	if purpose != totpPurpose {
		t.Fatalf("用途 = %q，期望 %q", purpose, totpPurpose)
	}
	if reference != "" {
		t.Fatalf("未确认的种子不该有引用，实际 %q", reference)
	}
	expiresAt, err := time.Parse(time.RFC3339, env.Data.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at 不是 RFC3339: %q", env.Data.ExpiresAt)
	}
	if d := time.Until(expiresAt); d <= 0 || d > agentstore.TemporarySecretTTL+time.Minute {
		t.Fatalf("到期时间不合理: %s（距今 %s）", expiresAt, d)
	}

	// URI 里那个种子必须真的就是这个引用解出来的种子——否则扫码扫进去的是另一把钥匙。
	seed, err := secrets.Resolve(t.Context(), env.Data.SecretRef)
	if err != nil {
		t.Fatalf("解密种子失败: %v", err)
	}
	parsed, err := url.Parse(env.Data.OTPAuthURI)
	if err != nil {
		t.Fatalf("otpauth_uri 不是合法 URI: %v", err)
	}
	if got, want := parsed.Query().Get("secret"), totp.Normalize(string(seed)); got != want {
		t.Fatalf("URI 里的种子与代理存的种子不一致")
	}
	// URI 必须让 authenticator 认出类型与参数。
	if parsed.Scheme != "otpauth" || parsed.Host != "totp" {
		t.Fatalf("URI scheme/host = %s://%s", parsed.Scheme, parsed.Host)
	}
	if parsed.Query().Get("digits") != "6" || parsed.Query().Get("period") != "30" {
		t.Fatalf("URI 缺少 digits/period: %s", env.Data.OTPAuthURI)
	}
}

func TestTotpEnrollValidatesItsInput(t *testing.T) {
	srv, _ := newTotpServer(t, agentstore.SecretsOptions{})

	cases := []struct {
		name    string
		payload string
	}{
		{"缺 user_id", `{"username":"alice"}`},
		{"user_id 为 0", `{"user_id":0,"username":"alice"}`},
		{"user_id 为负", `{"user_id":-3,"username":"alice"}`},
		{"username 为空", `{"user_id":1,"username":"   "}`},
		{"username 带控制字符", `{"user_id":1,"username":"ali\nce"}`},
		{"未知字段", `{"user_id":1,"username":"alice","issuer":"evil"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/enroll", tc.payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTotpConfirmTurnsTheSeedPersistentAndVerifyThenAccepts(t *testing.T) {
	srv, secrets := newTotpServer(t, agentstore.SecretsOptions{})
	ref, seed := enrollTotp(t, srv, 7, "alice")

	// 确认之前 verify 必须拒绝：临时登记不能用来登录。
	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/verify",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+wrongCode(t, seed)+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未确认就 verify 的状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}

	code, err := totp.Code(seed, time.Now().UTC())
	if err != nil {
		t.Fatalf("计算码失败: %v", err)
	}
	rec = postTotp(t, srv, testWebUID, "/internal/v1/totp/confirm",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm 状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var confirmed totpEnvelope[totpCodeResponse]
	if err := json.Unmarshal(rec.Body.Bytes(), &confirmed); err != nil {
		t.Fatalf("confirm 响应不是合法 JSON: %v", err)
	}
	if !confirmed.Data.OK {
		t.Fatalf("confirm 没通过：%s", rec.Body.String())
	}
	if confirmed.Data.Step <= 0 {
		t.Fatalf("confirm 没有返回时间步，Web 无法做重放防护：%s", rec.Body.String())
	}

	// 确认 = 转持久：引用挂上用户，到期时间被取消。
	purpose, reference, found, err := secrets.Binding(t.Context(), ref)
	if err != nil || !found {
		t.Fatalf("确认后查不到种子：found=%v err=%v", found, err)
	}
	if purpose != totpPurpose {
		t.Fatalf("用途 = %q", purpose)
	}
	if reference != totpBindingRef(7) {
		t.Fatalf("确认后的引用 = %q，期望 %q", reference, totpBindingRef(7))
	}

	// 确认之后 verify 通过，并回报同一个时间步。
	rec = postTotp(t, srv, testWebUID, "/internal/v1/totp/verify",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify 状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var verified totpEnvelope[totpCodeResponse]
	if err := json.Unmarshal(rec.Body.Bytes(), &verified); err != nil {
		t.Fatalf("verify 响应不是合法 JSON: %v", err)
	}
	if !verified.Data.OK {
		t.Fatalf("已确认的种子验码失败：%s", rec.Body.String())
	}
	if verified.Data.Step != confirmed.Data.Step {
		t.Fatalf("verify 的时间步 = %d，confirm 的是 %d", verified.Data.Step, confirmed.Data.Step)
	}
}

func TestTotpConfirmWithWrongCodeLeavesTheSeedTemporary(t *testing.T) {
	srv, secrets := newTotpServer(t, agentstore.SecretsOptions{})
	ref, seed := enrollTotp(t, srv, 7, "alice")

	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/confirm",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+wrongCode(t, seed)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("码不对应当是正常结果（200 + ok=false），实际 %d；响应 %s", rec.Code, rec.Body.String())
	}
	var env totpEnvelope[totpCodeResponse]
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if env.Data.OK || env.Data.Reason != totpReasonCodeMismatch {
		t.Fatalf("码不对时的返回 = %+v，期望 ok=false/reason=%s", env.Data, totpReasonCodeMismatch)
	}
	// 重点：码错了，这条种子必须还是临时的——错误尝试不能让"未确认"变成"已确认"。
	_, reference, found, err := secrets.Binding(t.Context(), ref)
	if err != nil || !found {
		t.Fatalf("查不到种子：found=%v err=%v", found, err)
	}
	if reference != "" {
		t.Fatalf("码错之后种子变成了 %q，未确认的种子不该有引用", reference)
	}
}

func TestTotpRejectsAnotherUsersSeed(t *testing.T) {
	srv, _ := newTotpServer(t, agentstore.SecretsOptions{})
	ref, seed := enrollTotp(t, srv, 7, "alice")

	code, err := totp.Code(seed, time.Now().UTC())
	if err != nil {
		t.Fatalf("计算码失败: %v", err)
	}
	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/confirm",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm 状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}

	// 别人的引用：能解密、用途也对，但归属不是他。这一层必须挡住。
	// disable 的请求体没有 code 字段，单独拼一份。
	cases := []struct{ path, payload string }{
		{"/internal/v1/totp/verify", `{"user_id":8,"secret_ref":"` + ref + `","code":"` + code + `"}`},
		{"/internal/v1/totp/confirm", `{"user_id":8,"secret_ref":"` + ref + `","code":"` + code + `"}`},
		{"/internal/v1/totp/disable", `{"user_id":8,"secret_ref":"` + ref + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := postTotp(t, srv, testWebUID, tc.path, tc.payload)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTotpRejectsRefsThatAreNotSeeds(t *testing.T) {
	srv, _ := newTotpServer(t, agentstore.SecretsOptions{})
	// 拿一条普通秘密（SMB 口令）的引用冒充 TOTP 种子。
	rec := postSecret(t, srv, testWebUID, `{"purpose":"smb_password","value":"Tr0ub4dor&3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("准备一条普通秘密失败：%d %s", rec.Code, rec.Body.String())
	}
	var env totpEnvelope[struct {
		SecretRef string `json:"secret_ref"`
	}]
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if env.Data.SecretRef == "" {
		t.Fatalf("没有拿到引用：%s", rec.Body.String())
	}

	rec = postTotp(t, srv, testWebUID, "/internal/v1/totp/confirm",
		`{"user_id":7,"secret_ref":"`+env.Data.SecretRef+`","code":"123456"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "不是二次验证种子") {
		t.Fatalf("错误信息没说清原因：%s", rec.Body.String())
	}
}

func TestTotpVerifyAfterExpiryAsksForANewEnrollment(t *testing.T) {
	// 用一个会"往前走"的时钟：登记之后把时间推过 TTL，临时种子就到期了。
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := now
	srv, _ := newTotpServer(t, agentstore.SecretsOptions{
		TTL: 10 * time.Minute,
		Now: func() time.Time { return clock },
	})
	ref, seed := enrollTotp(t, srv, 7, "alice")

	clock = now.Add(11 * time.Minute)
	code, err := totp.Code(seed, clock)
	if err != nil {
		t.Fatalf("计算码失败: %v", err)
	}
	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/confirm",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+code+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "过期") {
		t.Fatalf("错误信息没说清是过期：%s", rec.Body.String())
	}
}

func TestTotpDisableDestroysTheSeedAndIsIdempotent(t *testing.T) {
	srv, secrets := newTotpServer(t, agentstore.SecretsOptions{})
	ref, seed := enrollTotp(t, srv, 7, "alice")
	code, err := totp.Code(seed, time.Now().UTC())
	if err != nil {
		t.Fatalf("计算码失败: %v", err)
	}
	if rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/confirm",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+code+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("confirm 状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}

	// 两次都 200（关闭是幂等的），但只有第一次真的销毁了东西：第二次如实报
	// destroyed:false——"目标状态已经成立"和"我刚刚关掉了它"是两件事，别混成一句。
	wantDestroyed := []bool{true, false}
	for attempt, want := range wantDestroyed {
		rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/disable",
			`{"user_id":7,"secret_ref":"`+ref+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 disable 状态码 = %d；响应 %s", attempt+1, rec.Code, rec.Body.String())
		}
		var env totpEnvelope[totpDisableResponse]
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		if env.Data.Destroyed != want {
			t.Fatalf("第 %d 次 disable 的 destroyed = %v，期望 %v", attempt+1, env.Data.Destroyed, want)
		}
	}

	// 销毁之后：引用查不到，验证报不存在，解密也解不开（密文被清空）。
	if _, _, found, err := secrets.Binding(t.Context(), ref); err != nil || found {
		t.Fatalf("销毁后仍查得到：found=%v err=%v", found, err)
	}
	if _, err := secrets.Resolve(t.Context(), ref); err == nil {
		t.Fatal("销毁后仍解得出明文")
	}
	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/verify",
		`{"user_id":7,"secret_ref":"`+ref+`","code":"`+code+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("销毁后 verify 状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
	}
}

func TestTotpRoutesAreAbsentWhenNotWired(t *testing.T) {
	// 没接线就是 404：不是"接口在、但每次都拒绝"。
	srv := newTestServer(t)
	for _, path := range []string{
		"/internal/v1/totp/enroll", "/internal/v1/totp/confirm",
		"/internal/v1/totp/verify", "/internal/v1/totp/disable",
	} {
		t.Run(path, func(t *testing.T) {
			rec := postTotp(t, srv, testWebUID, path, `{"user_id":1,"username":"alice"}`)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTotpEnforcesMethodContentTypeAndPeer(t *testing.T) {
	srv, _ := newTotpServer(t, agentstore.SecretsOptions{})

	// 只收 POST。
	get := httptest.NewRequest(http.MethodGet, "/internal/v1/totp/enroll", nil)
	rec := serveAs(srv, get, testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态码 = %d，期望 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q", allow)
	}

	// 只收 application/json。
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/totp/enroll",
		strings.NewReader(`{"user_id":1,"username":"alice"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec = serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain 状态码 = %d，期望 415；响应 %s", rec.Code, rec.Body.String())
	}

	// 只有 Web 账号能调：root 拿不到这条路由（种子的生成与校验都不该有 root 旁路）。
	payload := `{"user_id":1,"username":"alice"}`
	if rec := postTotp(t, srv, 0, "/internal/v1/totp/enroll", payload); rec.Code != http.StatusForbidden {
		t.Fatalf("root 调用状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if rec := postTotp(t, srv, 9999, "/internal/v1/totp/enroll", payload); rec.Code != http.StatusForbidden {
		t.Fatalf("陌生 UID 调用状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
}

func TestTotpVerifyRejectsEmptyCode(t *testing.T) {
	srv, _ := newTotpServer(t, agentstore.SecretsOptions{})
	ref, _ := enrollTotp(t, srv, 7, "alice")
	rec := postTotp(t, srv, testWebUID, "/internal/v1/totp/verify",
		`{"user_id":7,"secret_ref":"`+ref+`","code":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空码状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
}
