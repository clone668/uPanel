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

// TestWithTimeoutLiftsHeaderWait 锁住 WithTimeout 的两处生效点。
//
// 只改 http.Client.Timeout 不够：真正的门槛是 Transport 的 ResponseHeaderTimeout
// （“等第一个字节”）。一次 26 秒才给出响应头的 `apt-cache policy` 在第 10 秒就被
// 拨号层掐断了——软件包事实那条查询（packages.AgentQueryTimeout=45s）正是靠这里
// 才等得到。流客户端仍然不继承：安静的长流不该被任何整体超时管着。
func TestWithTimeoutLiftsHeaderWait(t *testing.T) {
	c, err := New("/run/upanel/agent.sock", WithTimeout(45*time.Second))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if c.http.Timeout != 45*time.Second {
		t.Fatalf("整体超时 = %v，期望 45s", c.http.Timeout)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T", c.http.Transport)
	}
	if tr.ResponseHeaderTimeout != 45*time.Second {
		t.Fatalf("等待响应头超时 = %v，期望跟着一起抬到 45s", tr.ResponseHeaderTimeout)
	}
	st, ok := c.stream.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("流 Transport 类型 = %T", c.stream.Transport)
	}
	if st.ResponseHeaderTimeout != 0 {
		t.Fatalf("流客户端的等待响应头超时 = %v，期望 0", st.ResponseHeaderTimeout)
	}
}

func TestBackupQueryTimeoutOutlivesAgentSnapshotBudget(t *testing.T) {
	const agentSnapshotBudget = 90 * time.Second
	if BackupQueryTimeout <= agentSnapshotBudget {
		t.Fatalf("备份客户端超时 = %v，必须先让代理自己的 %v 预算说话",
			BackupQueryTimeout, agentSnapshotBudget)
	}
	c, err := New("/run/upanel/agent.sock", WithTimeout(BackupQueryTimeout))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if c.http.Timeout != BackupQueryTimeout {
		t.Fatalf("整体超时 = %v，期望 %v", c.http.Timeout, BackupQueryTimeout)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T", c.http.Transport)
	}
	if tr.ResponseHeaderTimeout != BackupQueryTimeout {
		t.Fatalf("等待响应头超时 = %v，期望 %v", tr.ResponseHeaderTimeout, BackupQueryTimeout)
	}
}

// TestNetworkConfigParsesPayload：network.config 的响应能解出来，且未导出的结构
// （panel_file / netplan_files）也能跨包解——JSON 只看字段是不是导出的，不看类型名。
func TestNetworkConfigParsesPayload(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/queries" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "network.config") {
			t.Errorf("请求体没带查询名: %s", body)
		}
		io.WriteString(w, `{"data":{"collected_at":"2026-10-26T12:00:00Z",
			"capability":{"backend":"netplan-networkd","editable":true,"target":"ens4","reasons":[]},
			"interface":"ens4","config_hash":"sha256:abc","mode":"static",
			"ipv4":{"address":"192.168.1.10","prefix_length":24,"gateway":"192.168.1.1","dns":null},
			"panel_file":{"name":"90-upanel-ens4.yaml","sha256":"deadbeef"},
			"netplan_files":[{"name":"50-cloud-init.yaml","sha256":"cafe"}],
			"unsupported":[],
			"pending":{"token":"nrb_0123456789abcdef0123456789abcdef","interface":"ens4","mode":"static",
				"candidate_address":"192.168.1.50","applied_at":"2026-10-26T12:00:00Z",
				"deadline":"2026-10-26T12:02:00Z","seconds_left":90,"state":"pending"},
			"issues":[]},"request_id":"rid"}`)
	}))

	cfg, err := c.NetworkConfig(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if cfg.ConfigHash != "sha256:abc" || cfg.Interface != "ens4" || !cfg.Capability.Editable {
		t.Fatalf("响应解错：%+v", cfg)
	}
	if cfg.PanelFile == nil {
		t.Fatal("panel_file 应当解出来（不是 null）")
	}
	if cfg.Pending == nil || cfg.Pending.SecondsLeft != 90 || cfg.Pending.State != "pending" {
		t.Fatalf("待确认的回滚解错：%+v", cfg.Pending)
	}
}

// TestNetworkConfigRejectsIncompletePayload：形状不全就报 unavailable，不给零值。
func TestNetworkConfigRejectsIncompletePayload(t *testing.T) {
	cases := []string{
		// 缺 collected_at
		`{"data":{"capability":{"backend":"netplan-networkd","editable":false,"reasons":[]},
			"mode":"unknown","panel_file":null,"netplan_files":[],"unsupported":[],"pending":null,
			"issues":[]},"request_id":"rid"}`,
		// 缺 backend：闭集里的"未知"不能用零值表达
		`{"data":{"collected_at":"2026-10-26T12:00:00Z",
			"capability":{"editable":false,"reasons":[]},"mode":"unknown","panel_file":null,
			"netplan_files":[],"unsupported":[],"pending":null,"issues":[]},"request_id":"rid"}`,
		// 说可以改，却没有 config_hash：页面据此提交，缺了它必被拒
		`{"data":{"collected_at":"2026-10-26T12:00:00Z",
			"capability":{"backend":"netplan-networkd","editable":true,"target":"ens4","reasons":[]},
			"mode":"static","panel_file":null,"netplan_files":[],"unsupported":[],"pending":null,
			"issues":[]},"request_id":"rid"}`,
	}
	for _, payload := range cases {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, payload)
		}))
		if _, err := c.NetworkConfig(context.Background()); err == nil {
			t.Fatalf("形状不全的响应应当报错：%s", payload)
		} else {
			var un *UnavailableError
			if !errors.As(err, &un) {
				t.Fatalf("应当是 *UnavailableError，得到 %T: %v", err, err)
			}
		}
	}
}
