package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// preflightActor 是测试用的操作者上下文。
func preflightActor() ActorContext {
	return ActorContext{UserID: 7, SessionIDHash: strings.Repeat("ab", 32), RequestID: "rid-preflight"}
}

func TestCreatePreflightSendsContractFields(t *testing.T) {
	var got map[string]json.RawMessage
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/preflights" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("请求体不是 JSON 对象: %v", err)
		}
		// 代理对预检返回 201：客户端必须接受任意 2xx，而不是只认 200。
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"data":{"preflight_ref":"pf_0123456789abcdef0123456789abcdef",
			"action":"storage.filesystem.format",
			"normalized_params":{"device":"/dev/sdb","label":"data"},
			"target":{"display_name":"/dev/sdb","identity_fingerprint":"fp","state_version":"7"},
			"expires_at":"2026-10-20T09:02:00Z","allowed":true,
			"blockers":[],"warnings":[{"code":"data_loss","message":"数据会丢失"}],
			"impacts":[{"kind":"data_loss","description":"分区表被重写"}],
			"requires_reauth":true,"requires_phrase":true,
			"confirmation_phrase":"FORMAT K7M2QX","cancellable":false,
			"locks":["disk:/dev/sdb"]},"request_id":"rid"}`)
	}))

	plan, err := c.CreatePreflight(context.Background(), PreflightInput{
		Action: "storage.filesystem.format",
		Params: json.RawMessage(`{"device":"/dev/sdb","label":"data"}`),
		Actor:  preflightActor(),
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	for _, key := range []string{"action", "params", "actor_context"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("请求体缺少 %s: %v", key, got)
		}
	}
	if string(got["action"]) != `"storage.filesystem.format"` {
		t.Fatalf("action = %s", got["action"])
	}
	// 参数必须原样转发：Web 不做规范化，哈希由代理按它自己产出的字节计算。
	if string(got["params"]) != `{"device":"/dev/sdb","label":"data"}` {
		t.Fatalf("params = %s", got["params"])
	}
	var actor map[string]any
	if err := json.Unmarshal(got["actor_context"], &actor); err != nil {
		t.Fatalf("actor_context 不是对象: %v", err)
	}
	if actor["user_id"] != float64(7) || actor["session_id_hash"] != strings.Repeat("ab", 32) {
		t.Fatalf("actor_context = %v", actor)
	}

	if plan.Ref != "pf_0123456789abcdef0123456789abcdef" || plan.Action != "storage.filesystem.format" {
		t.Fatalf("计划 = %+v", plan)
	}
	if string(plan.NormalizedParams) != `{"device":"/dev/sdb","label":"data"}` {
		t.Fatalf("规范化参数 = %s", plan.NormalizedParams)
	}
	if plan.Target.StateVersion != "7" || plan.Target.IdentityFingerprint != "fp" {
		t.Fatalf("目标 = %+v", plan.Target)
	}
	if plan.ConfirmationPhrase != "FORMAT K7M2QX" || !plan.RequiresPhrase || !plan.RequiresReauth {
		t.Fatalf("策略字段 = %+v", plan)
	}
	if plan.Cancellable {
		t.Fatal("cancellable 应为 false")
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "disk:/dev/sdb" {
		t.Fatalf("资源锁 = %v", plan.Locks)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "data_loss" {
		t.Fatalf("告警 = %+v", plan.Warnings)
	}
}

func TestCreatePreflightSendsEmptyObjectWhenParamsNil(t *testing.T) {
	var params string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("请求体不是 JSON: %v", err)
		}
		params = string(got.Params)
		io.WriteString(w, `{"data":{"preflight_ref":"pf_0123456789abcdef0123456789abcdef",
			"action":"acceptance.ping","normalized_params":{},
			"target":{"display_name":"x","identity_fingerprint":"fp","state_version":"1"},
			"expires_at":"2026-10-20T09:02:00Z","allowed":true,
			"blockers":[],"warnings":[],"impacts":[],"locks":[]},"request_id":"rid"}`)
	}))

	if _, err := c.CreatePreflight(context.Background(), PreflightInput{
		Action: "acceptance.ping",
		Actor:  preflightActor(),
	}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if params != "{}" {
		t.Fatalf("缺省参数应发送 {}，实际 %q", params)
	}
}

func TestCreatePreflightRejectsEmptyAction(t *testing.T) {
	c, err := New("/run/upanel/agent.sock")
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if _, err := c.CreatePreflight(context.Background(), PreflightInput{}); err == nil {
		t.Fatal("空 action 必须在本地就拒绝，不能发到代理")
	}
}

func TestCreatePreflightSurfacesBusinessError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"VALIDATION_ERROR","message":"未登记的动作 \"x.y\""},"request_id":"rid"}`)
	}))

	_, err := c.CreatePreflight(context.Background(), PreflightInput{Action: "x.y", Actor: preflightActor()})
	var agentErr *Error
	if !errors.As(err, &agentErr) {
		t.Fatalf("错误类型 = %T，期望 *Error", err)
	}
	if agentErr.Code != "VALIDATION_ERROR" || agentErr.Status != http.StatusBadRequest {
		t.Fatalf("错误 = %+v", agentErr)
	}
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) {
		t.Fatal("业务拒绝不能被当成代理不可用")
	}
}

func TestCreatePreflightRejectsIncompleteResponse(t *testing.T) {
	cases := map[string]string{
		"缺 preflight_ref": `{"data":{"action":"a.b","normalized_params":{},"expires_at":"2026-10-20T09:02:00Z"}}`,
		"缺 action":        `{"data":{"preflight_ref":"pf_x","normalized_params":{},"expires_at":"2026-10-20T09:02:00Z"}}`,
		"缺规范化参数":          `{"data":{"preflight_ref":"pf_x","action":"a.b","expires_at":"2026-10-20T09:02:00Z"}}`,
		"缺到期时间":           `{"data":{"preflight_ref":"pf_x","action":"a.b","normalized_params":{}}}`,
		"data 不是对象":       `{"data":[]}`,
		"没有 data":         `{"request_id":"rid"}`,
		"响应不是 JSON":       `not json`,
		"错误信封但缺 code":     `{"error":{"message":"x"}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			status := http.StatusOK
			if name == "错误信封但缺 code" {
				status = http.StatusBadGateway
			}
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, payload)
			}))
			_, err := c.CreatePreflight(context.Background(), PreflightInput{Action: "a.b", Actor: preflightActor()})
			if err == nil {
				t.Fatal("不完整的响应必须报错")
			}
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("错误类型 = %T，期望 *UnavailableError（%v）", err, err)
			}
		})
	}
}

func TestPutSecretAcceptsCreatedStatus(t *testing.T) {
	// 回归：代理对 POST /internal/v1/secrets 返回 201，客户端曾只认 200，
	// 导致真实配对时 PutSecret 永远失败。
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/secrets" {
			t.Errorf("意外请求 %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"data":{"secret_ref":"sec_0123456789abcdef0123456789abcdef",
			"purpose":"smb_password","state":"temporary","expires_at":"2026-10-20T09:10:00Z"},"request_id":"rid"}`)
	}))
	ref, err := c.PutSecret(context.Background(), PutSecretInput{Purpose: "smb_password", Value: "x"})
	if err != nil {
		t.Fatalf("201 必须被接受: %v", err)
	}
	if ref.Ref == "" || ref.State != "temporary" {
		t.Fatalf("响应 = %+v", ref)
	}
}

func TestPreflightReportsUnavailableWhenAgentDown(t *testing.T) {
	c, err := New("/run/upanel/agent.sock", WithDialer(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	_, err = c.CreatePreflight(context.Background(), PreflightInput{Action: "a.b", Actor: preflightActor()})
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("错误类型 = %T，期望 *UnavailableError", err)
	}
}
