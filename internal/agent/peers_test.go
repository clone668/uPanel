package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeErrorCode 取出错误信封里的 code。
func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, body)
	}
	return env.Error.Code
}

func TestGateRejectsRequestWithoutPeerIdentity(t *testing.T) {
	srv := newTestServer(t)
	// 直接调用 Handler（模拟“连接身份未知”）：必须失败关闭，不能当作放行。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/capabilities", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "FORBIDDEN" {
		t.Fatalf("错误码 = %q，期望 FORBIDDEN", code)
	}
}

func TestGateDeniesRootOnNormalRoutes(t *testing.T) {
	srv := newTestServer(t)
	// PLAN.md 第 4 节：普通 API 仅允许 upanel-web；root 只能走专用调度/恢复路由。
	// P02.3 还没有专用路由，所以 root 访问任何现有接口都必须被拒绝。
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/internal/v1/capabilities", ""},
		{http.MethodPost, "/internal/v1/queries", `{"query":"system.summary"}`},
		{http.MethodGet, "/", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := serveAs(srv, req, 0)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("root %s %s: 状态码 = %d，期望 403", tc.method, tc.path, rec.Code)
		}
		if code := decodeErrorCode(t, rec.Body.Bytes()); code != "FORBIDDEN" {
			t.Fatalf("root %s %s: 错误码 = %q", tc.method, tc.path, code)
		}
	}
}

func TestGateDeniesOtherUIDs(t *testing.T) {
	srv := newTestServer(t)
	for _, uid := range []uint32{1, 33, 1000, 9999} {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/capabilities", nil)
		rec := serveAs(srv, req, uid)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("UID %d: 状态码 = %d，期望 403", uid, rec.Code)
		}
		// 错误信息里不能带上允许的 UID 等内部细节。
		if body := rec.Body.String(); strings.Contains(body, "4242") {
			t.Fatalf("UID %d: 错误响应泄漏了允许的 UID: %s", uid, body)
		}
	}
}

func TestGateAllowsWebUID(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/capabilities", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("Web UID: 状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
}

func TestGateRootOnlyRouting(t *testing.T) {
	// 临时登记一个“只允许 root”的专用前缀，验证分类逻辑本身是对的。
	// 生产列表在 P02.3 为空（见 peer.go 的 rootOnlyPrefixes 注释）。
	old := rootOnlyPrefixes
	rootOnlyPrefixes = []string{"/internal/v1/schedules/"}
	defer func() { rootOnlyPrefixes = old }()

	srv := newTestServer(t)
	if !isRootOnlyPath("/internal/v1/schedules/abc/trigger") {
		t.Fatal("专用前缀未被识别")
	}
	if isRootOnlyPath("/internal/v1/capabilities") {
		t.Fatal("普通接口不应被识别为专用路由")
	}
	if !srv.peerAllowed(0, "/internal/v1/schedules/abc/trigger") {
		t.Fatal("专用路由应允许 root")
	}
	if srv.peerAllowed(testWebUID, "/internal/v1/schedules/abc/trigger") {
		t.Fatal("专用路由不能允许 Web 账号")
	}
	if srv.peerAllowed(0, "/internal/v1/capabilities") {
		t.Fatal("普通接口不能允许 root")
	}
	if !srv.peerAllowed(testWebUID, "/internal/v1/capabilities") {
		t.Fatal("普通接口应允许 Web 账号")
	}
}

func TestPeerPolicyValidate(t *testing.T) {
	if err := (PeerPolicy{}).validate(); err == nil {
		t.Fatal("空 WebUser 必须报错")
	}
	if err := (PeerPolicy{WebUser: "upanel-web"}).validate(); err == nil {
		t.Fatal("WebUID 为 0 必须报错")
	}
	if err := (PeerPolicy{WebUser: "upanel-web", WebUID: 1001}).validate(); err != nil {
		t.Fatalf("合法规则不应报错: %v", err)
	}
}

func TestResolvePeerPolicyRejectsUnknownUser(t *testing.T) {
	if _, err := ResolvePeerPolicy("upanel-no-such-user-xyz", ""); err == nil {
		t.Fatal("不存在的账号必须报错，不能退化成放行")
	} else if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误信息应说明账号不存在: %v", err)
	}
	if _, err := ResolvePeerPolicy("", ""); err == nil {
		t.Fatal("空账号名必须报错")
	}
}
