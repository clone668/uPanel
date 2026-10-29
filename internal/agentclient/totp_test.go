package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

// TestTotpEnrollParsesPayload 登记：请求里带用户标识与用户名，响应里的引用与 URI 都要拿到。
func TestTotpEnrollParsesPayload(t *testing.T) {
	var received map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/totp/enroll" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		io.WriteString(w, `{"data":{"secret_ref":"sec_abc","otpauth_uri":"otpauth://totp/uPanel:admin?secret=X",
			"issuer":"uPanel","account":"admin","digits":6,"period":30,"expires_at":"2026-10-20T00:10:00Z"},
			"request_id":"rid"}`)
	}))

	out, err := c.TotpEnroll(context.Background(), 7, "admin")
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if out.SecretRef != "sec_abc" || out.Digits != 6 || out.Period != 30 {
		t.Fatalf("登记结果 = %+v", out)
	}
	if got := received["user_id"]; got != float64(7) {
		t.Fatalf("user_id = %v，期望 7", got)
	}
	if got := received["username"]; got != "admin" {
		t.Fatalf("username = %v，期望 admin", got)
	}
}

// TestTotpEnrollRejectsIncompletePayload 引用或 URI 缺一不可：缺了就没法画二维码或后续校验。
func TestTotpEnrollRejectsIncompletePayload(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"secret_ref":"sec_abc","otpauth_uri":"","digits":6,"period":30},"request_id":"rid"}`)
	}))

	_, err := c.TotpEnroll(context.Background(), 7, "admin")
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("响应缺 otpauth_uri 应报不可用（协议走形），实际 %v", err)
	}
}

// TestTotpVerifyParsesCheck 校验：通过的码要带回时间步；码错是结果不是错误。
func TestTotpVerifyParsesCheck(t *testing.T) {
	var received map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/totp/verify" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		if received["code"] == "000000" {
			io.WriteString(w, `{"data":{"ok":false,"reason":"code_mismatch"},"request_id":"rid"}`)
			return
		}
		io.WriteString(w, `{"data":{"ok":true,"step":58216330},"request_id":"rid"}`)
	}))

	check, err := c.TotpVerify(context.Background(), 7, "sec_abc", "123456")
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if !check.OK || check.Step != 58216330 {
		t.Fatalf("校验结果 = %+v", check)
	}
	if received["secret_ref"] != "sec_abc" || received["code"] != "123456" {
		t.Fatalf("请求内容 = %+v", received)
	}

	// 码错：OK=false + 闭集 reason，err 为 nil。
	check, err = c.TotpVerify(context.Background(), 7, "sec_abc", "000000")
	if err != nil {
		t.Fatalf("码错不该报错: %v", err)
	}
	if check.OK || check.Reason != "code_mismatch" {
		t.Fatalf("码错结果 = %+v", check)
	}
}

// TestTotpCheckRejectsMissingStep 通过了却没给时间步：面板没法做重放防护，按协议走形处理。
func TestTotpCheckRejectsMissingStep(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"ok":true},"request_id":"rid"}`)
	}))

	_, err := c.TotpConfirm(context.Background(), 7, "sec_abc", "123456")
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("缺 step 应报不可用，实际 %v", err)
	}
}

// TestTotpDisableReportsDestroyed 销毁：代理说"本来就没有"也照样是成功（幂等）。
func TestTotpDisableReportsDestroyed(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/totp/disable" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"data":{"destroyed":false},"request_id":"rid"}`)
	}))

	destroyed, err := c.TotpDisable(context.Background(), 7, "sec_abc")
	if err != nil {
		t.Fatalf("销毁失败: %v", err)
	}
	if destroyed {
		t.Fatalf("代理回 destroyed=false，客户端不该说成 true")
	}
}

// TestTotpEndpointsPreserveBusinessErrors 代理明确拒绝时错误里要带状态码，不能压成"不可用"。
func TestTotpEndpointsPreserveBusinessErrors(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":"FORBIDDEN","message":"这条种子不属于该账号"},"request_id":"rid"}`)
	}))

	_, err := c.TotpVerify(context.Background(), 7, "sec_other", "123456")
	var bizErr *Error
	if !errors.As(err, &bizErr) {
		t.Fatalf("应返回 *Error，实际 %v", err)
	}
	if bizErr.Status != http.StatusForbidden {
		t.Fatalf("错误 = %+v", bizErr)
	}
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) {
		t.Fatal("业务拒绝不应被当成代理不可用")
	}
}
