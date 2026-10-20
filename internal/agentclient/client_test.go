package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient 把客户端连接到 httptest 服务：开发宿主不能创建 Unix socket，
// 但除了“连接方式”以外，客户端的协议行为（信封、错误码、超时）都是同一份代码。
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	c, err := New("/run/upanel/agent.sock", WithDialer(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return c
}

func TestNewRejectsBadSocketPath(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("空 socket 路径必须报错")
	}
	if _, err := New("run/agent.sock"); err == nil {
		t.Fatal("相对 socket 路径必须报错")
	}
}

func TestCapabilitiesParsesData(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/capabilities" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"data":{"version":"1.2.3","mode":"production",
			"supported_actions":[],"supported_queries":[],"started_at":"2026-10-20T00:00:00Z"},
			"request_id":"rid"}`)
	}))

	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if caps.Version != "1.2.3" || caps.Mode != "production" {
		t.Fatalf("capabilities = %+v", caps)
	}
	if caps.SupportedQueries == nil {
		t.Fatal("空能力列表应解析为 []，不能是 nil")
	}
}

func TestQuerySendsTypedParams(t *testing.T) {
	type params struct {
		DeviceID string `json:"device_id"`
	}
	var received map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/queries" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		io.WriteString(w, `{"data":{"ok":true},"request_id":"rid"}`)
	}))

	data, err := c.Query(context.Background(), "storage.topology", params{DeviceID: "dev-1"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Fatalf("data = %s", data)
	}
	if received["query"] != "storage.topology" {
		t.Fatalf("query = %v", received["query"])
	}
	gotParams, ok := received["params"].(map[string]any)
	if !ok || gotParams["device_id"] != "dev-1" {
		t.Fatalf("params = %v", received["params"])
	}
}

func TestQuerySendsEmptyObjectWhenParamsNil(t *testing.T) {
	var received map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		io.WriteString(w, `{"data":{},"request_id":"rid"}`)
	}))
	if _, err := c.Query(context.Background(), "system.summary", nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	params, ok := received["params"].(map[string]any)
	if !ok || len(params) != 0 {
		t.Fatalf("params 应为空对象，实际 %v", received["params"])
	}
}

func TestQueryReturnsTypedBusinessError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"VALIDATION_ERROR","message":"未登记的查询"},"request_id":"rid"}`)
	}))

	_, err := c.Query(context.Background(), "system.summary", nil)
	var bizErr *Error
	if !errors.As(err, &bizErr) {
		t.Fatalf("应返回 *Error，实际 %v", err)
	}
	if bizErr.Status != http.StatusBadRequest || bizErr.Code != "VALIDATION_ERROR" {
		t.Fatalf("错误 = %+v", bizErr)
	}
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) {
		t.Fatal("业务拒绝不应被当成代理不可用")
	}
}

func TestQueryReportsUnavailableWhenDialFails(t *testing.T) {
	c, err := New("/run/upanel/agent.sock", WithDialer(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	_, callErr := c.Query(context.Background(), "system.summary", nil)
	var unavailable *UnavailableError
	if !errors.As(callErr, &unavailable) {
		t.Fatalf("应返回 *UnavailableError，实际 %v", callErr)
	}
}

func TestDoRejectsMalformedEnvelope(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `not json`)
	}))
	_, err := c.Capabilities(context.Background())
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("应返回 *UnavailableError，实际 %v", err)
	}
}

func TestQueryRejectsEmptyName(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应发出请求")
	}))
	if _, err := c.Query(context.Background(), "", nil); err == nil {
		t.Fatal("空 query 名必须报错")
	}
}

func TestTimeoutSurfacesAsUnavailable(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		io.WriteString(w, `{"data":{},"request_id":"rid"}`)
	}))
	// 覆盖为很短的超时：超时必须报成不可用，而不是返回空数据当成功。
	c.http.Timeout = 50 * time.Millisecond
	_, err := c.Query(context.Background(), "system.summary", nil)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("应返回 *UnavailableError，实际 %v", err)
	}
}
