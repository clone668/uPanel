package api

// P05.3 接口层用例：日志查询与实时流。
//
// 分两类：
//   - /logs（普通 JSON）用 httptest.ResponseRecorder，关注权限、参数校验与错误映射；
//   - /logs/stream（SSE）用真实 httptest.Server，因为要验的是传输层行为：
//     帧格式、Last-Event-ID 续传、以及**客户端断开后请求 ctx 真的会结束**
//     ——那是“代理侧不会残留 journalctl 子进程”这条链路的 Web 侧端点。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/journal"
	"upanel/internal/logs"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubLogs 是 LogsReader 的测试替身。它不冒充 journalctl：解析与限额由
// internal/journal 的测试覆盖，这里只验 HTTP 层的映射与 ctx 传播。
type stubLogs struct {
	page      journal.Page
	queryErr  error
	entries   []journal.Entry
	reason    journal.StreamEnd
	streamErr error
	// hold 为真时流会一直等到 ctx 结束（模拟一条安静的实时流）。
	hold bool

	params   journal.Params
	paramsCh chan journal.Params
	ctxDone  chan struct{}
}

func newStubLogs() *stubLogs {
	return &stubLogs{paramsCh: make(chan journal.Params, 8), ctxDone: make(chan struct{}, 8)}
}

func (s *stubLogs) Query(_ context.Context, params journal.Params) (journal.Page, error) {
	s.params = params
	if s.queryErr != nil {
		return journal.Page{}, s.queryErr
	}
	return s.page, nil
}

func (s *stubLogs) StreamWithReason(ctx context.Context, params journal.Params, emit func(journal.Entry) error) (journal.StreamEnd, error) {
	s.params = params
	s.paramsCh <- params
	for _, entry := range s.entries {
		if err := emit(entry); err != nil {
			return journal.StreamEndClientClosed, err
		}
	}
	if s.streamErr != nil {
		return "", s.streamErr
	}
	if s.hold {
		<-ctx.Done()
		s.ctxDone <- struct{}{}
		return journal.StreamEndClientClosed, ctx.Err()
	}
	return s.reason, nil
}

func logsFixture(t *testing.T, reader LogsReader) (*Server, *http.Cookie) {
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
		Logs:              reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

// sampleLogPage 是一页真实形状的日志（游标格式与 systemd 255 一致）。
func sampleLogPage() journal.Page {
	next := "s=8a1e0b6f2c4d4f39; i=42;b=8a1e0b6f2c4d4f39;m=5f0c;t=5f0c;x=9b31"
	return journal.Page{
		Source: journal.SourceJournal,
		From:   time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC),
		Entries: []journal.Entry{
			{
				Cursor:     "s=8a1e0b6f2c4d4f39;i=41;b=8a1e0b6f2c4d4f39;m=5f0c;t=5f0c;x=9b30",
				Timestamp:  time.Date(2026, 10, 21, 2, 59, 0, 0, time.UTC),
				Level:      journal.LevelWarning,
				Message:    "sshd: 收到连接",
				Unit:       "ssh.service",
				Identifier: "sshd",
				Redacted:   true,
			},
		},
		NextCursor: &next,
		Scanned:    2,
		DurationMS: 7,
	}
}

func adminGet(t *testing.T, srv *Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", testOrigin)
	if cookie != nil {
		req.Header.Set("Cookie", cookie.String())
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestLogsRequiresSession 未登录一律 401。
func TestLogsRequiresSession(t *testing.T) {
	srv, _ := logsFixture(t, newStubLogs())
	for _, path := range []string{"/api/v1/logs", "/api/v1/logs/stream"} {
		rec := adminGet(t, srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未登录状态码 = %d，期望 401", path, rec.Code)
		}
	}
}

// TestLogsForbiddenForViewer 验 PLAN.md 第 14 节：原始日志仅 admin 可读。
//
// 注意这里连流也一起验：只挡住 /logs 而把 /logs/stream 敞开，等于没挡。
func TestLogsForbiddenForViewer(t *testing.T) {
	srv, _ := logsFixture(t, newStubLogs())
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	cookie, _ := loginAsUser(t, srv, "watcher", "watcher-horse-9")
	for _, path := range []string{"/api/v1/logs", "/api/v1/logs/stream"} {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s viewer 状态码 = %d，期望 403", path, rec.Code)
		}
		if got := parseResult(t, rec).Error; got == nil || got.Code != "FORBIDDEN" {
			t.Fatalf("%s 错误码 = %+v", path, got)
		}
	}
}

// TestLogsRejectsBadParams 参数错误是 400，不是 500：调用方要能分清“我写错了”。
func TestLogsRejectsBadParams(t *testing.T) {
	srv, cookie := logsFixture(t, newStubLogs())
	for _, q := range []string{
		"?limit=abc",
		"?limit=0",
		"?limit=-1",
		"?limit=5000",
		"?level=verbose",
		"?unit=ssh*",
		"?source=dmesg",
		"?from=2026-10-21",
		"?cursor=" + strings.Repeat("a", 600),
		// 未转义的 ';' 会让 url.ParseQuery 丢掉整个 cursor 参数：必须报错，
		// 不能“当成没给游标”悄悄回第一页（翻页会原地打转）。
		"?cursor=s=a;i=1;b=b",
		"?from=2026-10-22T00:00:00Z&to=2026-10-21T00:00:00Z",
	} {
		rec := adminGet(t, srv, cookie, "/api/v1/logs"+q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → 状态码 %d，期望 400；响应 %s", q, rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec).Error; got == nil || got.Code != "VALIDATION_ERROR" {
			t.Fatalf("%s 错误码 = %+v", q, got)
		}
	}
}

// TestLogsUnavailableWithoutAgent 未配置代理时是 503，而不是“空日志”。
//
// 空列表会让页面显示“这段时间没有日志”，把“代理连不上”伪装成“一切正常”。
func TestLogsUnavailableWithoutAgent(t *testing.T) {
	srv, cookie := logsFixture(t, nil)
	for _, path := range []string{"/api/v1/logs", "/api/v1/logs/stream"} {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503", path, rec.Code)
		}
		if got := parseResult(t, rec).Error; got == nil || got.Code != "AGENT_UNAVAILABLE" {
			t.Fatalf("%s 错误码 = %+v", path, got)
		}
	}
}

// TestLogsReturnsPage 验返回体就是契约里的那些字段，且游标原样透出。
func TestLogsReturnsPage(t *testing.T) {
	stub := newStubLogs()
	stub.page = sampleLogPage()
	srv, cookie := logsFixture(t, stub)

	rec := adminGet(t, srv, cookie,
		"/api/v1/logs?source=journal&unit=ssh.service&level=warning&limit=50&cursor=s%3Da%3Bi%3D1")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	var body journal.Page
	if err := json.Unmarshal(parseResult(t, rec).Data, &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body.Source != journal.SourceJournal || len(body.Entries) != 1 {
		t.Fatalf("返回体不对: %+v", body)
	}
	entry := body.Entries[0]
	if entry.Cursor == "" || entry.Level != journal.LevelWarning || !entry.Redacted {
		t.Fatalf("条目字段丢失: %+v", entry)
	}
	if body.NextCursor == nil || *body.NextCursor != *sampleLogPage().NextCursor {
		t.Fatalf("next_cursor 没透出: %v", body.NextCursor)
	}
	// 参数必须原样到读取端：过滤条件被悄悄丢掉，页面就会显示“全部日志”而不自知。
	if stub.params.Unit != "ssh.service" || stub.params.Level != "warning" ||
		stub.params.Limit != 50 || stub.params.Cursor != "s=a;i=1" {
		t.Fatalf("参数没传到读取端: %+v", stub.params)
	}
}

// TestLogsMapsReaderErrors 验错误映射：参数类 400、代理不可用 503、其余 500。
func TestLogsMapsReaderErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"参数", fmt.Errorf("%w: 时间范围超过 24 小时", journal.ErrInvalidParams), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"游标", fmt.Errorf("%w: 游标已失效", journal.ErrCursorRejected), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"代理", fmt.Errorf("dial: %w", logs.ErrAgentUnavailable), http.StatusServiceUnavailable, "AGENT_UNAVAILABLE"},
		{"其它", errors.New("journalctl 退出码 1"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubLogs()
			stub.queryErr = tc.err
			srv, cookie := logsFixture(t, stub)
			rec := adminGet(t, srv, cookie, "/api/v1/logs")
			if rec.Code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d；响应 %s", rec.Code, tc.want, rec.Body.String())
			}
			if got := parseResult(t, rec).Error; got == nil || got.Code != tc.code {
				t.Fatalf("错误码 = %+v，期望 %q", got, tc.code)
			}
		})
	}
}

// openLogStream 打开一条日志 SSE；header 里的 Last-Event-ID 由 lastID 决定。
func openLogStream(t *testing.T, base, path, cookie, lastID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		cancel()
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", cookie)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("打开日志流失败: %v", err)
	}
	return resp, cancel
}

// readFrames 读到 done 帧（或超时）为止，返回收到的全部帧。
func readFrames(t *testing.T, resp *http.Response, want string) []string {
	t.Helper()
	reader := bufio.NewReader(resp.Body)
	var (
		frames []string
		b      strings.Builder
	)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			if b.Len() > 0 {
				frames = append(frames, b.String())
			}
			return frames
		}
		if line == "\n" || line == "\r\n" {
			frame := b.String()
			b.Reset()
			frames = append(frames, frame)
			if want != "" && strings.Contains(frame, want) {
				return frames
			}
		}
	}
	return frames
}

// TestLogsStreamFrames 验帧格式：entry 帧带游标 id，最后是 end 帧带原因。
func TestLogsStreamFrames(t *testing.T) {
	stub := newStubLogs()
	stub.entries = []journal.Entry{
		{Cursor: "s=a;i=1;b=b;m=1;t=1;x=1", Timestamp: time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC),
			Level: journal.LevelInfo, Message: "第一条"},
		{Cursor: "s=a;i=2;b=b;m=1;t=1;x=2", Timestamp: time.Date(2026, 10, 21, 3, 0, 1, 0, time.UTC),
			Level: journal.LevelError, Message: "第二条"},
	}
	stub.reason = journal.StreamEndDuration
	srv, cookie := logsFixture(t, stub)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	resp, cancel := openLogStream(t, server.URL, "/api/v1/logs/stream?unit=ssh.service", cookie.String(), "")
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	frames := readFrames(t, resp, "event: end")
	joined := strings.Join(frames, "---")
	for _, want := range []string{
		": connected",
		"event: entry",
		"id: s=a;i=1;b=b;m=1;t=1;x=1",
		`"message":"第一条"`,
		`"level":"error"`,
		"event: end",
		`"reason":"duration_limit"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("流里没有 %q。收到的帧：\n%s", want, joined)
		}
	}
	if got := <-stub.paramsCh; got.Unit != "ssh.service" {
		t.Fatalf("参数没传到读取端: %+v", got)
	}
}

// TestLogsStreamResumesFromLastEventID 验断线续传：Last-Event-ID 被当成游标传给代理。
func TestLogsStreamResumesFromLastEventID(t *testing.T) {
	stub := newStubLogs()
	stub.reason = journal.StreamEndClientClosed
	srv, cookie := logsFixture(t, stub)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	resp, cancel := openLogStream(t, server.URL, "/api/v1/logs/stream", cookie.String(),
		"s=a;i=7;b=b;m=1;t=1;x=7")
	defer cancel()
	defer resp.Body.Close()
	readFrames(t, resp, "event: end")

	if got := <-stub.paramsCh; got.Cursor != "s=a;i=7;b=b;m=1;t=1;x=7" {
		t.Fatalf("Last-Event-ID 没有变成游标: %+v", got)
	}
}

// TestLogsStreamRejectsBadLastEventID 验非法续传位置直接 400，而不是开流后乱补发。
func TestLogsStreamRejectsBadLastEventID(t *testing.T) {
	srv, cookie := logsFixture(t, newStubLogs())
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	resp, cancel := openLogStream(t, server.URL, "/api/v1/logs/stream", cookie.String(), "不是游标 带空格")
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

// TestLogsStreamReportsError 验异常结束会写 error 帧，而不是安静地断掉。
func TestLogsStreamReportsError(t *testing.T) {
	stub := newStubLogs()
	stub.streamErr = errors.New("journalctl 退出码 1")
	srv, cookie := logsFixture(t, stub)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	resp, cancel := openLogStream(t, server.URL, "/api/v1/logs/stream", cookie.String(), "")
	defer cancel()
	defer resp.Body.Close()

	frames := readFrames(t, resp, "event: error")
	joined := strings.Join(frames, "---")
	if !strings.Contains(joined, "event: error") || !strings.Contains(joined, "STREAM_FAILED") {
		t.Fatalf("应写出 error 帧；收到的帧：\n%s", joined)
	}
}

// TestLogsStreamReleasesOnDisconnect 是 P05.3 那条验收在 Web 侧的一半：
// 客户端断开后，请求 ctx 必须结束——它是传播到代理的唯一信号，
// 代理看到它结束才会杀掉 journalctl。这里用一个“只在 ctx 结束时返回”的替身来证明。
func TestLogsStreamReleasesOnDisconnect(t *testing.T) {
	stub := newStubLogs()
	stub.hold = true
	srv, cookie := logsFixture(t, stub)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	resp, cancel := openLogStream(t, server.URL, "/api/v1/logs/stream", cookie.String(), "")
	// 等流真的开起来（读取端已被调用）。
	select {
	case <-stub.paramsCh:
	case <-time.After(3 * time.Second):
		t.Fatal("流没有开起来")
	}

	cancel()
	resp.Body.Close()

	select {
	case <-stub.ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后请求 ctx 没有结束：代理侧会留下孤儿 journalctl")
	}
}
