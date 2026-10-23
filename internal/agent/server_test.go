package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/version"
)

// testWebUID 是单元测试里“Web 服务账号”的 UID。真实值由启动时按账号名解析，
// 这里只是让 gate 有一个明确的允许对象。
const testWebUID uint32 = 4242

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testPeer() PeerPolicy {
	return PeerPolicy{
		WebUser:     "test-web",
		WebUID:      testWebUID,
		SocketGroup: "test-web",
		SocketGID:   testWebUID,
	}
}

func socketPathForTest(t *testing.T) string {
	t.Helper()
	// 仅用于 New 的参数校验；Serve 需要 Linux + root，单独在 VM 中验证。
	return filepath.Join(t.TempDir(), "agent.sock")
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithRegistry(t, nil)
}

func newTestServerWithRegistry(t *testing.T, reg *Registry) *Server {
	t.Helper()
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Registry:        reg,
		Secrets:         newTestSecretStore(t),
		Plans:           newTestPlanStore(t),
		Operations:      newTestOperationStore(t),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

// serveAs 以指定对端 UID 调用路由。gate 只认上下文里的内核身份，
// 单元测试在这里显式注入，等价于 Accept 阶段读到 SO_PEERCRED。
func serveAs(srv *Server, req *http.Request, uid uint32) *httptest.ResponseRecorder {
	ctx := withPeerInfo(req.Context(), peerInfo{UID: uid, PID: 31337})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func TestCapabilitiesShape(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/capabilities", nil)
	req.Header.Set("X-Request-ID", "rid-caps")

	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var body struct {
		Data struct {
			Version          string   `json:"version"`
			Mode             string   `json:"mode"`
			OS               string   `json:"os"`
			SupportedActions []string `json:"supported_actions"`
			SupportedQueries []string `json:"supported_queries"`
			Dependencies     []any    `json:"dependencies"`
			StartedAt        string   `json:"started_at"`
			GoVersion        string   `json:"go_version"`
		} `json:"data"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.RequestID != "rid-caps" {
		t.Fatalf("request_id = %q", body.RequestID)
	}
	if body.Data.Version != "test" {
		t.Fatalf("version = %q", body.Data.Version)
	}
	if body.Data.Mode != "production" {
		t.Fatalf("代理模式必须报告 production，实际 %q", body.Data.Mode)
	}
	// 生产登记表为空：必须如实返回空数组，不能编造占位动作或查询。
	if len(body.Data.SupportedActions) != 0 {
		t.Fatalf("supported_actions 应为空，实际 %v", body.Data.SupportedActions)
	}
	if body.Data.SupportedActions == nil {
		t.Fatal("supported_actions 必须是 []，不能是 null")
	}
	if len(body.Data.SupportedQueries) != 0 {
		t.Fatalf("supported_queries 应为空，实际 %v", body.Data.SupportedQueries)
	}
	if body.Data.SupportedQueries == nil {
		t.Fatal("supported_queries 必须是 []，不能是 null")
	}
	if body.Data.Dependencies == nil {
		t.Fatal("dependencies 必须是 []，不能是 null")
	}
	if body.Data.StartedAt == "" {
		t.Fatal("started_at 不能为空")
	}
}

func TestCapabilitiesRejectsPost(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/capabilities", strings.NewReader("{}"))
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestUnknownAgentPathIs404(t *testing.T) {
	srv := newTestServer(t)
	// 只列出根路径、不存在的接口与尚未实现的接口（终端与文件属后续阶段）；
	// 它们必须 404 而不是 200——未实现的代理接口不得以任何成功状态回话。
	//
	// P05.3 起 /internal/v1/streams 是已实现的路由（GET 建许可不合法，回 405），
	// 所以它从这里移出，改由 streams_test.go 覆盖；这条测试的语义不变。
	for _, path := range []string{"/", "/internal/v1/jobs", "/internal/v1/terminal", "/internal/v1/files", "/internal/v1/operations/not-an-id"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := serveAs(srv, req, testWebUID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: 状态码 = %d，期望 404（未实现的代理接口不得返回 200）", path, rec.Code)
		}
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	base := Options{
		SocketPath:      "/run/upanel/agent.sock",
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
	}
	if _, err := New(Options{SocketPath: base.SocketPath, ShutdownTimeout: time.Second, Peer: base.Peer}); err == nil {
		t.Fatal("缺少 Logger 必须报错")
	}
	if _, err := New(Options{Logger: base.Logger, ShutdownTimeout: time.Second, SocketPath: "relative.sock", Peer: base.Peer}); err == nil {
		t.Fatal("相对 socket 路径必须报错")
	}
	if _, err := New(Options{Logger: base.Logger, ShutdownTimeout: time.Second, Peer: base.Peer}); err == nil {
		t.Fatal("空 socket 路径必须报错")
	}
	if _, err := New(Options{Logger: base.Logger, SocketPath: base.SocketPath, ShutdownTimeout: 0, Peer: base.Peer}); err == nil {
		t.Fatal("停止超时必须为正")
	}
	if _, err := New(Options{Logger: base.Logger, SocketPath: base.SocketPath, ShutdownTimeout: time.Second}); err == nil {
		t.Fatal("缺少 Peer 规则必须报错（不能默认放行任何人）")
	}
	if _, err := New(Options{Logger: base.Logger, SocketPath: base.SocketPath, ShutdownTimeout: time.Second, Peer: base.Peer}); err == nil {
		t.Fatal("缺少秘密存储必须报错（不能启动一个存不了秘密的代理）")
	}
	if _, err := New(Options{
		Logger:          base.Logger,
		SocketPath:      base.SocketPath,
		ShutdownTimeout: time.Second,
		Peer:            base.Peer,
		Secrets:         newTestSecretStore(t),
	}); err == nil {
		t.Fatal("缺少预检计划存储必须报错（不能启动一个签不出预检的代理）")
	}
	rootPeer := base.Peer
	rootPeer.WebUID = 0
	if _, err := New(Options{Logger: base.Logger, SocketPath: base.SocketPath, ShutdownTimeout: time.Second, Peer: rootPeer}); err == nil {
		t.Fatal("WebUID 为 0 必须报错")
	}
}

// TestPrepareSocketPathRefusesNonSocket 验证不会误删同名普通文件或目录。
func TestPrepareSocketPathRefusesNonSocket(t *testing.T) {
	dir := t.TempDir()

	filePath := filepath.Join(dir, "agent.sock")
	if err := os.WriteFile(filePath, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	srv, err := New(Options{
		SocketPath:      filePath,
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Secrets:         newTestSecretStore(t),
		Plans:           newTestPlanStore(t),
		Operations:      newTestOperationStore(t),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := srv.prepareSocketPath(); err == nil {
		t.Fatal("同名普通文件必须拒绝覆盖")
	}
	if _, statErr := os.Stat(filePath); statErr != nil {
		t.Fatalf("普通文件不应被删除: %v", statErr)
	}

	dirPath := filepath.Join(dir, "subdir")
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	srv2, err := New(Options{
		SocketPath:      dirPath,
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),

		Secrets:    newTestSecretStore(t),
		Plans:      newTestPlanStore(t),
		Operations: newTestOperationStore(t),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := srv2.prepareSocketPath(); err == nil {
		t.Fatal("同名目录必须拒绝处理")
	}
	if _, statErr := os.Stat(dirPath); statErr != nil {
		t.Fatalf("目录不应被删除: %v", statErr)
	}
}

// TestPrepareSocketPathRequiresExistingDir 验证 socket 目录不存在时明确失败。
func TestPrepareSocketPathRequiresExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	srv, err := New(Options{
		SocketPath:      filepath.Join(dir, "agent.sock"),
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),

		Secrets:    newTestSecretStore(t),
		Plans:      newTestPlanStore(t),
		Operations: newTestOperationStore(t),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := srv.prepareSocketPath(); err == nil {
		t.Fatal("socket 目录不存在时必须报错，不能隐式创建系统目录")
	}
}

// newTestServerAt 构造一个 socket 路径指定的服务，只用于 prepareSocketPath 的测试。
func newTestServerAt(t *testing.T, socketPath string) *Server {
	t.Helper()
	srv, err := New(Options{
		SocketPath:      socketPath,
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Secrets:         newTestSecretStore(t),
		Plans:           newTestPlanStore(t),
		Operations:      newTestOperationStore(t),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

// TestPrepareSocketPathRefusesLiveSocket 验证不会抢走一个正在服务的代理的 socket。
//
// 这条来自 P11.3 验收现场：上一轮留下的 systemd 单元在 3 秒一次的重启循环里反复
// 启动代理，每次都把脚本手启的那个代理的 socket 文件删掉再 bind；抢输的那个随即
// 退出，退出时又把路径删了。结果代理进程活着，Web 却报
// "dial unix /run/upanel/agent.sock: connect: no such file or directory"。
// 连得上就说明有人在服务，此时必须拒绝启动，而不是删掉它。
func TestPrepareSocketPathRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("准备监听失败: %v", err)
	}
	defer func() { _ = listener.Close() }()

	srv := newTestServerAt(t, path)
	prepErr := srv.prepareSocketPath()
	if prepErr == nil {
		t.Fatal("socket 上已有代理在服务时必须拒绝启动")
	}
	if !strings.Contains(prepErr.Error(), "已经有一个代理在服务") {
		t.Fatalf("错误信息应说清原因，实际: %v", prepErr)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("正在服务的 socket 不能被删除: %v", statErr)
	}
	// 拒绝启动不能对原来的监听者有任何副作用。
	conn, dialErr := net.DialTimeout("unix", path, time.Second)
	if dialErr != nil {
		t.Fatalf("拒绝启动后原代理的 socket 应仍然可连: %v", dialErr)
	}
	_ = conn.Close()
}

// TestPrepareSocketPathReplacesStaleSocket 验证陈旧 socket 仍然会被清掉。
//
// 上一次异常退出的代理会留下 socket 文件而没有人 listen。这种必须能恢复，
// 否则代理一旦崩溃就再也起不来——那是把"不误删"做成了"不能启动"。
func TestPrepareSocketPathReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("准备监听失败: %v", err)
	}
	// 关掉监听但把文件留在原地：这就是崩溃后留下的现场。
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("关闭监听失败: %v", err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("陈旧 socket 应该还在: %v", statErr)
	}

	srv := newTestServerAt(t, path)
	if err := srv.prepareSocketPath(); err != nil {
		t.Fatalf("陈旧 socket 应被清理: %v", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("陈旧 socket 应已被删除，实际: %v", statErr)
	}
}
