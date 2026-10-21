package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/journal"
	"upanel/internal/version"
)

// fakeLogs 是 LogsSource 的假实现：查询返回预置页，流按预置条目发完后返回预置原因。
type fakeLogs struct {
	page      journal.Page
	queryErr  error
	entries   []journal.Entry
	end       journal.StreamEnd
	streamErr error
	// closed 记录流被调用过（用于验证“流被打开”而不是只建了许可）。
	calls  int
	params journal.Params
	opts   journal.StreamOptions
}

func (f *fakeLogs) Query(_ context.Context, params journal.Params) (journal.Page, error) {
	f.calls++
	f.params = params
	if f.queryErr != nil {
		return journal.Page{}, f.queryErr
	}
	return f.page, nil
}

func (f *fakeLogs) Stream(_ context.Context, params journal.Params, opts journal.StreamOptions,
	emit journal.EmitFunc) (journal.StreamEnd, error) {
	f.calls++
	f.params = params
	f.opts = opts
	if f.streamErr != nil {
		return "", f.streamErr
	}
	for _, entry := range f.entries {
		if err := emit(entry); err != nil {
			return journal.StreamEndClientClosed, nil
		}
	}
	return f.end, nil
}

// newServerWithLogs 构造一个带日志读取端的代理。读取端同时进登记表（logs.query）
// 与 Options.Logs（logs.stream 用途），与生产里 RegisterProductionQueries 的做法一致。
func newServerWithLogs(t *testing.T, logs LogsSource) *Server {
	t.Helper()
	reg := NewRegistry()
	if logs != nil {
		if err := reg.RegisterQuery(newLogsQuery(logs)); err != nil {
			t.Fatalf("登记 logs.query 失败: %v", err)
		}
	}
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
		Logs:            logs,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

func postJSON(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return serveAs(srv, req, testWebUID)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是错误信封: %s", rec.Body.String())
	}
	return env.Error.Code
}

// errorMessage 取出错误信封里的 message，用于断言“用户会看到的那句话”。
func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是错误信封: %s", rec.Body.String())
	}
	return env.Error.Message
}

// TestLogsQueryRejectsUnknownFields 验证 logs.query 的参数形状是**固定**的：
// 任何多余的字段（尤其是像 command 这种）都在解码层被拒绝。
func TestLogsQueryRejectsUnknownFields(t *testing.T) {
	srv := newServerWithLogs(t, &fakeLogs{})
	rec := postJSON(t, srv, "/internal/v1/queries",
		`{"query":"logs.query","params":{"command":"tail -f /etc/shadow"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q", code)
	}
}

// TestLogsQueryBadParamsAre400 验证参数错误是 400 而不是 500。
//
// 差别很重要：400 告诉调用方“你写错了”，500 会让它以为是服务端故障并重试。
func TestLogsQueryBadParamsAre400(t *testing.T) {
	srv := newServerWithLogs(t, &fakeLogs{})
	for _, body := range []string{
		`{"query":"logs.query","params":{"level":"verbose"}}`,
		`{"query":"logs.query","params":{"source":"dmesg"}}`,
		`{"query":"logs.query","params":{"unit":"ssh*"}}`,
		`{"query":"logs.query","params":{"cursor":"a b"}}`,
		`{"query":"logs.query","params":{"limit":5000}}`,
	} {
		rec := postJSON(t, srv, "/internal/v1/queries", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → 状态码 %d，期望 400；body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestLogsQueryParamErrorsFromQueryAre400 验证**执行阶段**的参数错误也是 400。
//
// 时间窗口（未来 to）与游标的有效性只有拿到“现在”和 journalctl 的真实反馈才知道，
// 所以它们出现在 Run 里而不是 Decode 里。如果这里回 500，Web 会把用户的参数错误
// 报成“代理不可用”（实测：?to=+3h 曾返回 503），调用方只会重试，不会去改参数。
func TestLogsQueryParamErrorsFromQueryAre400(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		// detail 是这条错误里**真正有用**的那半句：最终消息必须原样带着它。
		detail string
	}{
		{"窗口不合法", fmt.Errorf("%w: to 超过当前时间", journal.ErrInvalidParams), "to 超过当前时间"},
		{"游标被拒", fmt.Errorf("%w: Failed to seek to cursor", journal.ErrCursorRejected), "Failed to seek to cursor"},
	} {
		fake := &fakeLogs{queryErr: tc.err}
		srv := newServerWithLogs(t, fake)
		rec := postJSON(t, srv, "/internal/v1/queries",
			`{"query":"logs.query","params":{"unit":"ssh.service"}}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s：状态码 = %d，期望 400；body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
			t.Fatalf("%s：错误码 = %q", tc.name, code)
		}
		// 消息里只允许出现一次“参数不合法”。曾经是三层套娃
		//（“journal: 查询参数不合法: agent: 参数不合法: journal: 查询参数不合法: …”），
		// 因为每一层都用 `%w: %v` 又写了一遍自己的说法。
		msg := errorMessage(t, rec)
		if n := strings.Count(msg, "参数不合法"); n > 1 {
			t.Fatalf("%s：错误消息里有 %d 次“参数不合法”，最多 1 次：%q", tc.name, n, msg)
		}
		if !strings.Contains(msg, tc.detail) {
			t.Fatalf("%s：错误消息丢了原始细节 %q：%q", tc.name, tc.detail, msg)
		}
		// 消息里也不该出现代理自己的说法：参数属于 journal，套一层“agent:”只会误导排查。
		if strings.Contains(msg, "agent:") {
			t.Fatalf("%s：错误消息里出现了层名：%q", tc.name, msg)
		}
	}
}

// TestLogsQueryInternalErrorsStay500 验证真实的执行故障没有被顺手报成 400：
// “游标写错了”和“journalctl 挂了”是两回事，前者该改参数，后者该看服务端。
func TestLogsQueryInternalErrorsStay500(t *testing.T) {
	fake := &fakeLogs{queryErr: errors.New("journal: journalctl 以退出状态 1 结束: 磁盘满了")}
	srv := newServerWithLogs(t, fake)
	rec := postJSON(t, srv, "/internal/v1/queries",
		`{"query":"logs.query","params":{"unit":"ssh.service"}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500；body=%s", rec.Code, rec.Body.String())
	}
}

// TestStreamLogsFlushesHeadersBeforeFirstEntry 锁定“开流先出头”这件事。
//
// 安静的系统上第一条日志可能要等几十秒；代理如果等第一条日志才写，调用方的
// “等待响应头”计时器会先到（实测：Web 侧报 timeout awaiting response headers，
// 代理侧的空闲上限根本没机会生效）。
func TestStreamLogsFlushesHeadersBeforeFirstEntry(t *testing.T) {
	fake := &fakeLogs{end: journal.StreamEndIdle}
	srv := newServerWithLogs(t, fake)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/streams/x", nil)
	srv.serveLogStream(w, req, streamPermit{ID: "x", Purpose: StreamPurposeLogs})
	if !w.wroteHeader {
		t.Fatal("应先写出 200 响应头")
	}
	if !w.flushedBeforeWrite {
		t.Fatal("响应头必须在第一条日志之前 flush")
	}
}

// flushRecorder 记录“在第一次写正文之前有没有 flush 过头”。
type flushRecorder struct {
	*httptest.ResponseRecorder
	wroteHeader        bool
	wroteBody          bool
	flushedBeforeWrite bool
}

func (f *flushRecorder) WriteHeader(code int) {
	f.wroteHeader = true
	f.ResponseRecorder.WriteHeader(code)
}

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.wroteBody = true
	return f.ResponseRecorder.Write(p)
}

func (f *flushRecorder) Flush() {
	if !f.wroteBody {
		f.flushedBeforeWrite = true
	}
	f.ResponseRecorder.Flush()
}

// TestLogsQueryReturnsPage 验证成功路径把 journal.Page 原样交出去。
func TestLogsQueryReturnsPage(t *testing.T) {
	fake := &fakeLogs{page: journal.Page{
		Source: journal.SourceJournal,
		From:   time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC),
		Entries: []journal.Entry{{
			Cursor:    "s=a;i=1;b=b;m=1;t=1;x=1",
			Timestamp: time.Date(2026, 10, 21, 2, 30, 0, 0, time.UTC),
			Level:     journal.LevelWarning,
			Message:   "hello",
		}},
	}}
	srv := newServerWithLogs(t, fake)
	rec := postJSON(t, srv, "/internal/v1/queries",
		`{"query":"logs.query","params":{"unit":"ssh.service","level":"warning","limit":10}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data journal.Page `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(env.Data.Entries) != 1 || env.Data.Entries[0].Message != "hello" {
		t.Fatalf("返回的页不对: %+v", env.Data)
	}
	if fake.params.Unit != "ssh.service" || fake.params.Level != "warning" || fake.params.Limit != 10 {
		t.Fatalf("参数没有原样传到读取端: %+v", fake.params)
	}
}

// TestStreamsRejectUnregisteredPurpose 验证用途白名单。
func TestStreamsRejectUnregisteredPurpose(t *testing.T) {
	srv := newServerWithLogs(t, &fakeLogs{})
	rec := postJSON(t, srv, "/internal/v1/streams",
		`{"purpose":"terminal.stream","params":{}}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501；body=%s", rec.Code, rec.Body.String())
	}
}

// TestStreamsRequireLogs 验证没有日志读取端时不接受日志流许可：
// 否则会造出一张“永远打不开”的许可。
func TestStreamsRequireLogs(t *testing.T) {
	srv := newServerWithLogs(t, nil)
	rec := postJSON(t, srv, "/internal/v1/streams",
		`{"purpose":"logs.stream","params":{"unit":"ssh.service"}}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501；body=%s", rec.Code, rec.Body.String())
	}
}

// TestStreamsRejectBadParams 验证许可阶段的参数校验（发头之前就能拒绝）。
func TestStreamsRejectBadParams(t *testing.T) {
	srv := newServerWithLogs(t, &fakeLogs{})
	for _, body := range []string{
		`{"purpose":"logs.stream","params":{"unit":"ssh*"}}`,
		`{"purpose":"logs.stream","params":{"source":"dmesg"}}`,
		`{"purpose":"logs.stream","params":{"unknown":1}}`,
	} {
		rec := postJSON(t, srv, "/internal/v1/streams", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → 状态码 %d，期望 400；body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func createPermit(t *testing.T, srv *Server, body string) string {
	t.Helper()
	rec := postJSON(t, srv, "/internal/v1/streams", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建许可状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data streamResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析许可响应: %v", err)
	}
	if env.Data.StreamID == "" || env.Data.Purpose != StreamPurposeLogs || env.Data.ExpiresAt.IsZero() {
		t.Fatalf("许可响应缺少必需字段: %+v", env.Data)
	}
	return env.Data.StreamID
}

// TestStreamPermitIsSingleUse 验证许可只能用一次：第二次 GET 必须是 404。
func TestStreamPermitIsSingleUse(t *testing.T) {
	fake := &fakeLogs{end: journal.StreamEndDuration}
	srv := newServerWithLogs(t, fake)
	id := createPermit(t, srv, `{"purpose":"logs.stream","params":{}}`)

	first := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if first.Code != http.StatusOK {
		t.Fatalf("第一次 GET 状态码 = %d；body=%s", first.Code, first.Body.String())
	}
	second := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if second.Code != http.StatusNotFound {
		t.Fatalf("第二次 GET 状态码 = %d，期望 404", second.Code)
	}
}

// TestStreamPermitExpires 验证过期许可是 404，而且过期条目会被清掉。
func TestStreamPermitExpires(t *testing.T) {
	srv := newServerWithLogs(t, &fakeLogs{end: journal.StreamEndIdle})
	id := createPermit(t, srv, `{"purpose":"logs.stream","params":{}}`)

	// 把时钟推到 TTL 之后。许可表用的是 srv.streams.now。
	base := srv.streams.now()
	srv.streams.now = func() time.Time { return base.Add(streamPermitTTL + time.Second) }

	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("过期许可状态码 = %d，期望 404", rec.Code)
	}
	if srv.streams.count() != 0 {
		t.Fatalf("过期许可应被清理，剩余 %d", srv.streams.count())
	}
}

// TestStreamLogsWritesNDJSON 验证流的帧格式：entry 若干 + 一个 end。
func TestStreamLogsWritesNDJSON(t *testing.T) {
	fake := &fakeLogs{
		entries: []journal.Entry{
			{Cursor: "s=a;i=1;b=b;m=1;t=1;x=1", Timestamp: time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC),
				Level: journal.LevelInfo, Message: "第一条"},
			{Cursor: "s=a;i=2;b=b;m=1;t=1;x=2", Timestamp: time.Date(2026, 10, 21, 3, 0, 1, 0, time.UTC),
				Level: journal.LevelError, Message: "第二条", Redacted: true},
		},
		end: journal.StreamEndIdle,
	}
	srv := newServerWithLogs(t, fake)
	id := createPermit(t, srv, `{"purpose":"logs.stream","params":{"unit":"ssh.service"}}`)

	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d，期望 3（两条 entry + 一条 end）；body=%s", len(lines), rec.Body.String())
	}
	var first struct {
		Type  string        `json:"type"`
		Entry journal.Entry `json:"entry"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("解析第一行: %v", err)
	}
	if first.Type != "entry" || first.Entry.Cursor != "s=a;i=1;b=b;m=1;t=1;x=1" || first.Entry.Message != "第一条" {
		t.Fatalf("第一行不对: %s", lines[0])
	}
	var last struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatalf("解析结束行: %v", err)
	}
	if last.Type != "end" || last.Reason != string(journal.StreamEndIdle) {
		t.Fatalf("结束行不对: %s", lines[2])
	}
	if fake.params.Unit != "ssh.service" {
		t.Fatalf("流参数没有传到读取端: %+v", fake.params)
	}
}

// TestStreamLogsReportsError 验证代理侧失败会写成 error 帧，而不是安静地结束。
func TestStreamLogsReportsError(t *testing.T) {
	fake := &fakeLogs{streamErr: errors.New("journalctl 挂了")}
	srv := newServerWithLogs(t, fake)
	id := createPermit(t, srv, `{"purpose":"logs.stream","params":{}}`)
	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("应写出 error 帧；body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "journalctl 挂了") {
		t.Fatalf("error 帧应带原因；body=%s", rec.Body.String())
	}
}

// TestStreamsRejectOtherMethods 验证只有 POST 建许可、只有 GET 开流。
func TestStreamsRejectOtherMethods(t *testing.T) {
	srv := newServerWithLogs(t, &fakeLogs{})
	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams", nil), testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /streams 状态码 = %d，期望 405", rec.Code)
	}
	id := createPermit(t, srv, `{"purpose":"logs.stream","params":{}}`)
	rec = serveAs(srv, httptest.NewRequest(http.MethodPut, "/internal/v1/streams/"+id, nil), testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /streams/:id 状态码 = %d，期望 405", rec.Code)
	}
}

// TestStreamsRequireWebUID 验证许可与开流同样受 SO_PEERCRED 约束。
func TestStreamsRequireWebUID(t *testing.T) {
	srv := newServerWithLogs(t, &fakeLogs{})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/streams",
		strings.NewReader(`{"purpose":"logs.stream","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := serveAs(srv, req, 0)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("root 以外的 UID 状态码 = %d，期望 403；body=%s", rec.Code, rec.Body.String())
	}
}
