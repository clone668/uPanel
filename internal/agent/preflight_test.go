package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/version"
)

// recordingPlanStore 把写入的计划记下来，同时照常落库。
//
// 用来验证“拒绝了请求就不该留下计划”：这个判断需要一个外部观察点，
// 而 agentstore 不对外暴露原始 SQL 句柄。
type recordingPlanStore struct {
	inner *agentstore.PreflightPlans

	mu       sync.Mutex
	inserted []agentstore.PreflightPlan
	deleted  int
}

func (r *recordingPlanStore) Insert(ctx context.Context, plan agentstore.PreflightPlan) error {
	if err := r.inner.Insert(ctx, plan); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inserted = append(r.inserted, plan)
	return nil
}

func (r *recordingPlanStore) Get(ctx context.Context, id string) (agentstore.PreflightPlan, error) {
	return r.inner.Get(ctx, id)
}

func (r *recordingPlanStore) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	n, err := r.inner.DeleteExpired(ctx, now)
	if err != nil {
		return n, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted += n
	return n, nil
}

// count 返回已落库的计划条数。
func (r *recordingPlanStore) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inserted)
}

// testSessionHash 是一个形态合法的会话令牌摘要（真实值是 Web 侧算的 SHA-256）。
var testSessionHash = strings.Repeat("ab", 32)

// newPreflightServer 构造一个带真实计划存储的代理服务，并返回存储以便查库。
//
// reg 为 nil 时用验收动作表：它是唯一一份**不触碰系统**的预检实现，
// 用真实生产动作来测预检会顺带验证磁盘逻辑，那是 P03.3 之后的事。
func newPreflightServer(t *testing.T, reg *Registry) (*Server, *recordingPlanStore) {
	t.Helper()
	if reg == nil {
		reg = NewAcceptanceRegistry()
	}
	// 预检与执行记录必须落在**同一个**代理库上：P03.2 的接受是一个事务，
	// 消费预检与写入操作记录同生共死，两个库会让这件事在测试里假装成立。
	db := newTestAgentDB(t)
	plans := &recordingPlanStore{inner: newTestPlanStoreOn(t, db)}
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Registry:        reg,
		Secrets:         newTestSecretStore(t),
		Plans:           plans,
		Operations:      newTestOperationStoreOn(t, db),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv, plans
}

// preflightBody 拼一个合法的预检请求体。
func preflightBody(action, params string) string {
	return `{"action":"` + action + `","params":` + params +
		`,"actor_context":{"user_id":7,"session_id_hash":"` + testSessionHash +
		`","request_id":"rid-preflight","reauth_at":"2026-10-20T09:00:00Z"}}`
}

// postPreflight 发送一次 POST /internal/v1/preflights。
func postPreflight(t *testing.T, srv *Server, uid uint32, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/preflights", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "rid-preflight")
	return serveAs(srv, req, uid)
}

// decodePreflight 解出预检响应。
func decodePreflight(t *testing.T, rec *httptest.ResponseRecorder) preflightResponse {
	t.Helper()
	var body struct {
		Data      preflightResponse `json:"data"`
		RequestID string            `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v；响应 %s", err, rec.Body.String())
	}
	if body.RequestID != "rid-preflight" {
		t.Fatalf("request_id = %q", body.RequestID)
	}
	return body.Data
}

// sha256Hex 计算字符串的 SHA-256 十六进制值（与实现侧独立计算）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var planRefPattern = regexp.MustCompile(`^pf_[0-9a-f]{32}$`)
var phrasePattern = regexp.MustCompile(`^PROBE [A-HJ-NP-Z2-9]{6}$`)

func TestPreflightCreatesBoundPlan(t *testing.T) {
	srv, plans := newPreflightServer(t, nil)
	started := time.Now().UTC()

	rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.probe", `{"label":"alpha"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	data := decodePreflight(t, rec)

	if !planRefPattern.MatchString(data.PreflightRef) {
		t.Fatalf("preflight_ref 形态不合法: %q", data.PreflightRef)
	}
	if data.Action != "acceptance.probe" {
		t.Fatalf("action = %q", data.Action)
	}
	// 策略由登记表决定：前端只能展示，不能自己决定要不要再认证或短语。
	if !data.RequiresReauth || !data.RequiresPhrase {
		t.Fatalf("acceptance.probe 必须要求再认证与确认短语，实际 reauth=%v phrase=%v",
			data.RequiresReauth, data.RequiresPhrase)
	}
	if data.Cancellable {
		t.Fatal("acceptance.probe 登记为不可取消")
	}
	if !phrasePattern.MatchString(data.ConfirmationPhrase) {
		t.Fatalf("确认短语形态不合法: %q", data.ConfirmationPhrase)
	}
	if !data.Allowed {
		t.Fatalf("验收动作应当允许执行，阻塞项: %+v", data.Blockers)
	}
	if len(data.Blockers) != 0 {
		t.Fatalf("阻塞项应为空，实际 %+v", data.Blockers)
	}
	if len(data.Warnings) != 1 || data.Warnings[0].Code != "acceptance_only" {
		t.Fatalf("告警不合规: %+v", data.Warnings)
	}
	if len(data.Impacts) != 1 || data.Impacts[0].Kind != "none" {
		t.Fatalf("影响不合规: %+v", data.Impacts)
	}
	if len(data.Locks) != 1 || data.Locks[0] != "acceptance:probe" {
		t.Fatalf("资源锁 = %v", data.Locks)
	}
	if data.Target.IdentityFingerprint == "" || data.Target.StateVersion != "1" {
		t.Fatalf("目标信息不完整: %+v", data.Target)
	}
	if !strings.Contains(data.Target.DisplayName, "alpha") {
		t.Fatalf("展示名没有带上参数标签: %q", data.Target.DisplayName)
	}

	// 期限：120 秒，从代理生成计划的那一刻算起。
	expiresAt, err := time.Parse(time.RFC3339, data.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at 不是 RFC3339: %q", data.ExpiresAt)
	}
	if d := expiresAt.Sub(started); d < PreflightTTL-5*time.Second || d > PreflightTTL+5*time.Second {
		t.Fatalf("到期时间距现在 %s，期望约 %s", d, PreflightTTL)
	}

	// 落库内容必须与响应一致，且绑定字段齐全。
	stored, err := plans.inner.Get(context.Background(), data.PreflightRef)
	if err != nil {
		t.Fatalf("读取计划失败: %v", err)
	}
	if !stored.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("库中到期时间 %s 与响应 %s 不一致", stored.ExpiresAt, expiresAt)
	}
	if stored.ExpiresAt.Sub(stored.CreatedAt) != PreflightTTL {
		t.Fatalf("库中期限 = %s，期望 %s", stored.ExpiresAt.Sub(stored.CreatedAt), PreflightTTL)
	}
	if stored.UserID != 7 || stored.SessionHash != testSessionHash || stored.RequestID != "rid-preflight" {
		t.Fatalf("绑定字段不对: user=%d session=%s rid=%s",
			stored.UserID, stored.SessionHash, stored.RequestID)
	}
	if stored.Action != "acceptance.probe" || !stored.RequiresReauth || !stored.Allowed {
		t.Fatalf("库中策略字段不对: %+v", stored)
	}
	if stored.StateVersion != "1" || stored.IdentityFingerprint == "" {
		t.Fatal("库中缺少状态版本或目标身份指纹：执行前无法复核")
	}
	// 参数哈希按规范化字节算：库里存的是同一份字节，哈希必须能复算出来。
	if want := sha256Hex(string(stored.Params)); stored.ParamsHash != want {
		t.Fatalf("库中 params_hash 与参数不匹配: %s ≠ %s（参数 %s）", stored.ParamsHash, want, stored.Params)
	}
	// 短语只存哈希，明文只在响应里出现一次。
	if stored.PhraseHash != sha256Hex(data.ConfirmationPhrase) {
		t.Fatal("库中 phrase_hash 与返回的确认短语不匹配")
	}
	_ = stored
}

func TestPreflightStoresNoPlaintextPhrase(t *testing.T) {
	srv, plans := newPreflightServer(t, nil)
	rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.probe", `{}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	data := decodePreflight(t, rec)
	stored, err := plans.inner.Get(context.Background(), data.PreflightRef)
	if err != nil {
		t.Fatalf("读取计划失败: %v", err)
	}
	fields := []string{
		stored.ID, stored.Action, string(stored.Params), string(stored.Target),
		stored.PhraseHash, string(stored.Warnings), string(stored.Impacts), stored.RequestID,
	}
	for _, f := range fields {
		if strings.Contains(f, data.ConfirmationPhrase) {
			t.Fatalf("库里出现了确认短语明文: %q", f)
		}
	}
	if stored.PhraseHash == data.ConfirmationPhrase {
		t.Fatal("短语必须以哈希形式保存")
	}
}

func TestPreflightWithoutPhrasePolicyHasNoPhrase(t *testing.T) {
	srv, _ := newPreflightServer(t, nil)
	rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.ping", `{"note":"x"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	data := decodePreflight(t, rec)
	if data.RequiresPhrase || data.ConfirmationPhrase != "" {
		t.Fatalf("不需要短语的动作不应返回短语: %q", data.ConfirmationPhrase)
	}
	if data.RequiresReauth {
		t.Fatal("acceptance.ping 不要求再认证")
	}
	if !data.Cancellable {
		t.Fatal("acceptance.ping 登记为可取消")
	}
}

func TestPreflightParamsHashIsCanonical(t *testing.T) {
	srv, plans := newPreflightServer(t, nil)

	hashOf := func(params string) string {
		t.Helper()
		rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.probe", params))
		if rec.Code != http.StatusCreated {
			t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
		}
		stored, err := plans.inner.Get(context.Background(), decodePreflight(t, rec).PreflightRef)
		if err != nil {
			t.Fatalf("读取计划失败: %v", err)
		}
		return stored.ParamsHash
	}

	compact := hashOf(`{"label":"alpha"}`)
	spaced := hashOf(`{ "label" : "alpha" }`)
	other := hashOf(`{"label":"beta"}`)

	if compact != spaced {
		t.Fatalf("只有空白差异的参数必须得到同一个哈希: %s ≠ %s", compact, spaced)
	}
	if compact == other {
		t.Fatal("参数内容不同却得到相同哈希，绑定失效")
	}
}

func TestPreflightLocksNormalized(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterAction(Action{
		Name: "acceptance.locks",
		Decode: func(raw json.RawMessage) (any, error) {
			var v map[string]any
			if err := DecodeParams(raw, &v); err != nil {
				return nil, err
			}
			return v, nil
		},
		Plan: func(_ context.Context, in PlanInput) (Plan, error) {
			return Plan{
				NormalizedParams: map[string]any{},
				Target:           Target{DisplayName: "锁测试对象", IdentityFingerprint: "fp", StateVersion: "3"},
				Allowed:          true,
				Locks:            []string{"disk:b", " disk:a ", "disk:b", "", "net:x"},
			}, nil
		},
	}); err != nil {
		t.Fatalf("登记动作失败: %v", err)
	}

	srv, plans := newPreflightServer(t, reg)
	rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.locks", `{}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	data := decodePreflight(t, rec)
	want := []string{"disk:a", "disk:b", "net:x"}
	if len(data.Locks) != len(want) {
		t.Fatalf("资源锁 = %v，期望 %v", data.Locks, want)
	}
	for i := range want {
		if data.Locks[i] != want[i] {
			t.Fatalf("资源锁 = %v，期望 %v（必须去重、去空白并排序）", data.Locks, want)
		}
	}
	stored, err := plans.inner.Get(context.Background(), data.PreflightRef)
	if err != nil {
		t.Fatalf("读取计划失败: %v", err)
	}
	if string(stored.Locks) != `["disk:a","disk:b","net:x"]` {
		t.Fatalf("库中资源锁 = %s", stored.Locks)
	}
}

func TestPreflightRejectsInvalidRequests(t *testing.T) {
	srv, plans := newPreflightServer(t, nil)

	badActor := func(actor string) string {
		return `{"action":"acceptance.probe","params":{},"actor_context":` + actor + `}`
	}

	cases := []struct {
		name     string
		payload  string
		wantText string
	}{
		{"空请求体", ``, "请求体不能为空"},
		{"不是对象", `[]`, "参数不合法"},
		{"缺少动作名", `{"params":{},"actor_context":{"user_id":7,"session_id_hash":"` + testSessionHash + `"}}`, "action 不能为空"},
		{"未登记动作", preflightBody("storage.volume.mount", `{}`), "未登记的动作"},
		{"用户为 0", badActor(`{"user_id":0,"session_id_hash":"` + testSessionHash + `"}`), "user_id"},
		{"缺少会话哈希", badActor(`{"user_id":7}`), "session_id_hash"},
		{"会话哈希长度不对", badActor(`{"user_id":7,"session_id_hash":"abc"}`), "session_id_hash"},
		{"会话哈希含大写", badActor(`{"user_id":7,"session_id_hash":"` + strings.ToUpper(testSessionHash) + `"}`), "session_id_hash"},
		{"再认证时间不是 RFC3339", badActor(`{"user_id":7,"session_id_hash":"` + testSessionHash + `","reauth_at":"09:00"}`), "reauth_at"},
		{"参数含未知字段", preflightBody("acceptance.probe", `{"label":"x","force":true}`), "unknown field"},
		{"参数类型不对", preflightBody("acceptance.probe", `{"label":1}`), "参数不合法"},
		// 前端不能自带计划、不能绕过检查：任何计划外字段都必须被拒绝。
		{"请求体夹带计划外字段", `{"action":"acceptance.probe","params":{},"allowed":true,` +
			`"actor_context":{"user_id":7,"session_id_hash":"` + testSessionHash + `"}}`, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postPreflight(t, srv, testWebUID, tc.payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
				t.Fatalf("400 必须带契约错误码: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantText) {
				t.Fatalf("错误信息 = %s，期望包含 %q", rec.Body.String(), tc.wantText)
			}
		})
	}

	// 拒绝的请求不能留下任何计划：否则库里会出现“没人要的预检”。
	if n := plans.count(); n != 0 {
		t.Fatalf("被拒绝的请求不应落库，实际有 %d 条", n)
	}
}

func TestPreflightRequiresPostAndJSON(t *testing.T) {
	srv, _ := newPreflightServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/preflights", nil)
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

	req = httptest.NewRequest(http.MethodPost, "/internal/v1/preflights",
		strings.NewReader(preflightBody("acceptance.ping", `{}`)))
	req.Header.Set("Content-Type", "text/plain")
	rec = serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON Content-Type 状态码 = %d，期望 415", rec.Code)
	}
}

func TestPreflightRejectsOversizedBody(t *testing.T) {
	srv, _ := newPreflightServer(t, nil)
	huge := `{"action":"acceptance.probe","params":{"label":"` +
		strings.Repeat("a", maxPreflightRequestBytes) + `"}}`
	rec := postPreflight(t, srv, testWebUID, huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413；响应 %s", rec.Code, rec.Body.String())
	}
}

func TestPreflightDeniedForOtherUIDs(t *testing.T) {
	srv, _ := newPreflightServer(t, nil)
	rec := postPreflight(t, srv, testWebUID+1, preflightBody("acceptance.ping", `{}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 Web 服务账号状态码 = %d，期望 403", rec.Code)
	}
}

func TestPreflightWithoutPlannerIsNotImplemented(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterAction(Action{
		Name: "acceptance.no_plan",
		Decode: func(raw json.RawMessage) (any, error) {
			var v map[string]any
			if err := DecodeParams(raw, &v); err != nil {
				return nil, err
			}
			return v, nil
		},
	}); err != nil {
		t.Fatalf("登记动作失败: %v", err)
	}
	srv, plans := newPreflightServer(t, reg)

	rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.no_plan", `{}`))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "NOT_IMPLEMENTED") {
		t.Fatalf("501 应带 NOT_IMPLEMENTED: %s", rec.Body.String())
	}
	if n := plans.count(); n != 0 {
		t.Fatalf("没有预检实现的动作不应落库，实际有 %d 条", n)
	}
}

func TestPreflightRejectsInconsistentPlan(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
	}{
		{"拒绝却没有阻塞项", Plan{
			NormalizedParams: map[string]any{},
			Target:           Target{DisplayName: "x", IdentityFingerprint: "fp", StateVersion: "1"},
			Allowed:          false,
		}},
		{"缺少目标身份指纹", Plan{
			NormalizedParams: map[string]any{},
			Target:           Target{DisplayName: "x", StateVersion: "1"},
			Allowed:          true,
		}},
		{"缺少状态版本", Plan{
			NormalizedParams: map[string]any{},
			Target:           Target{DisplayName: "x", IdentityFingerprint: "fp"},
			Allowed:          true,
		}},
		{"缺少规范化参数", Plan{
			Target:  Target{DisplayName: "x", IdentityFingerprint: "fp", StateVersion: "1"},
			Allowed: true,
		}},
		{"阻塞项缺 code", Plan{
			NormalizedParams: map[string]any{},
			Target:           Target{DisplayName: "x", IdentityFingerprint: "fp", StateVersion: "1"},
			Allowed:          false,
			Blockers:         []Notice{{Message: "出了问题"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			plan := tc.plan
			if err := reg.RegisterAction(Action{
				Name: "acceptance.bad_plan",
				Decode: func(raw json.RawMessage) (any, error) {
					var v map[string]any
					if err := DecodeParams(raw, &v); err != nil {
						return nil, err
					}
					return v, nil
				},
				Plan: func(context.Context, PlanInput) (Plan, error) { return plan, nil },
			}); err != nil {
				t.Fatalf("登记动作失败: %v", err)
			}
			srv, plans := newPreflightServer(t, reg)
			rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.bad_plan", `{}`))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("状态码 = %d，期望 500；响应 %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "INTERNAL_ERROR") {
				t.Fatalf("500 应带 INTERNAL_ERROR: %s", rec.Body.String())
			}
			if n := plans.count(); n != 0 {
				t.Fatal("不合法的计划不能落库")
			}
		})
	}
}

func TestPreflightPlannerErrorIsInternal(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterAction(Action{
		Name: "acceptance.broken",
		Decode: func(raw json.RawMessage) (any, error) {
			var v map[string]any
			if err := DecodeParams(raw, &v); err != nil {
				return nil, err
			}
			return v, nil
		},
		Plan: func(context.Context, PlanInput) (Plan, error) {
			return Plan{}, errors.New("设备列表读取失败")
		},
	}); err != nil {
		t.Fatalf("登记动作失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)
	rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.broken", `{}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500；响应 %s", rec.Code, rec.Body.String())
	}
}

func TestPreflightBlockedPlanIsStoredWithBlockers(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterAction(Action{
		Name: "acceptance.blocked",
		Decode: func(raw json.RawMessage) (any, error) {
			var v map[string]any
			if err := DecodeParams(raw, &v); err != nil {
				return nil, err
			}
			return v, nil
		},
		Plan: func(context.Context, PlanInput) (Plan, error) {
			return Plan{
				NormalizedParams: map[string]any{},
				Target:           Target{DisplayName: "系统盘", IdentityFingerprint: "fp", StateVersion: "1"},
				Allowed:          false,
				Blockers:         []Notice{{Code: "system_disk", Message: "目标是系统盘，禁止操作"}},
				Locks:            []string{"disk:/dev/sda"},
			}, nil
		},
		Policy: ActionPolicy{RequiresReauth: true},
	}); err != nil {
		t.Fatalf("登记动作失败: %v", err)
	}
	srv, plans := newPreflightServer(t, reg)

	rec := postPreflight(t, srv, testWebUID, preflightBody("acceptance.blocked", `{}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201（被阻塞也是有效预检）；响应 %s", rec.Code, rec.Body.String())
	}
	data := decodePreflight(t, rec)
	if data.Allowed {
		t.Fatal("计划必须如实报告 allowed=false")
	}
	if len(data.Blockers) != 1 || data.Blockers[0].Code != "system_disk" {
		t.Fatalf("阻塞项 = %+v", data.Blockers)
	}
	stored, err := plans.inner.Get(context.Background(), data.PreflightRef)
	if err != nil {
		t.Fatalf("读取计划失败: %v", err)
	}
	if stored.Allowed {
		t.Fatal("库中必须记录 allowed=false")
	}
	if string(stored.Blockers) != `[{"code":"system_disk","message":"目标是系统盘，禁止操作"}]` {
		t.Fatalf("库中阻塞项 = %s", stored.Blockers)
	}
}

func TestPreflightSweepsExpiredPlans(t *testing.T) {
	srv, plans := newPreflightServer(t, nil)
	ctx := context.Background()

	now := time.Now().UTC()
	expired := agentstore.PreflightPlan{
		ID:                  agentstore.PlanRefPrefix + strings.Repeat("a", 32),
		Action:              "acceptance.probe",
		Params:              []byte(`{}`),
		ParamsHash:          sha256Hex(`{}`),
		Target:              []byte(`{"display_name":"x","identity_fingerprint":"fp","state_version":"1"}`),
		IdentityFingerprint: "fp",
		StateVersion:        "1",
		Allowed:             true,
		Blockers:            []byte(`[]`),
		Warnings:            []byte(`[]`),
		Impacts:             []byte(`[]`),
		Locks:               []byte(`[]`),
		UserID:              7,
		SessionHash:         testSessionHash,
		CreatedAt:           now.Add(-10 * time.Minute),
		ExpiresAt:           now.Add(-8 * time.Minute),
	}
	if err := plans.inner.Insert(ctx, expired); err != nil {
		t.Fatalf("保存过期计划失败: %v", err)
	}
	if _, err := plans.inner.Get(ctx, expired.ID); err != nil {
		t.Fatalf("读取计划失败: %v", err)
	}

	removed, err := srv.SweepExpiredPlans(ctx)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("清理条数 = %d，期望 1", removed)
	}
	if _, err := plans.inner.Get(ctx, expired.ID); !errors.Is(err, agentstore.ErrPlanNotFound) {
		t.Fatalf("过期计划应被删除，实际错误 %v", err)
	}
}

func TestNewConfirmationPhraseUsesUnambiguousAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		phrase, err := newConfirmationPhrase("FORMAT")
		if err != nil {
			t.Fatalf("生成短语失败: %v", err)
		}
		if !strings.HasPrefix(phrase, "FORMAT ") {
			t.Fatalf("短语 = %q，期望以动作词开头", phrase)
		}
		code := strings.TrimPrefix(phrase, "FORMAT ")
		if len(code) != phraseCodeLength {
			t.Fatalf("随机码长度 = %d，期望 %d", len(code), phraseCodeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(phraseAlphabet, r) {
				t.Fatalf("码字符 %q 不在无歧义字母表里", r)
			}
		}
		seen[phrase] = true
	}
	// 64 次抽样全是同一个短语说明随机源坏了（约 30 位熵，撞车概率可忽略）。
	if len(seen) < 60 {
		t.Fatalf("随机性不足：64 次只得到 %d 个不同短语", len(seen))
	}
}

func TestCanonicalJSONSortsKeys(t *testing.T) {
	out, err := canonicalJSON([]byte(`{ "b" : 2, "a" : { "d" : 4, "c" : 3 } }`))
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if string(out) != `{"a":{"c":3,"d":4},"b":2}` {
		t.Fatalf("规范化结果 = %s", out)
	}
}
