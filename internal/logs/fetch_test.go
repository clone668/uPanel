package logs

// P05.3：Web 侧日志读取端的用例。
//
// 这里不造假代理接口，而是让**真实的 agentclient** 对着一个 httptest 服务说话：
// 两进程之间真正的契约就是那一层 HTTP 信封与 NDJSON 帧，用真客户端验才作数。
// （开发宿主不能建 Unix socket，但 WithDialer 只换连接方式，协议代码是同一份。）

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/journal"
)

// quietLogger 让用例输出只剩失败信息。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeAgent 是一个“代理进程”：按路径分发 queries / streams。
type fakeAgent struct {
	t *testing.T

	queryStatus int
	queryBody   string

	createStatus int
	createBody   string

	streamBody  string
	streamCode  int
	streamFrame string

	queryCalls  int
	streamCalls int
	lastParams  map[string]any
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	return &fakeAgent{t: t, queryStatus: http.StatusOK, createStatus: http.StatusCreated, streamCode: http.StatusOK}
}

func (f *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/queries":
		f.queryCalls++
		var body struct {
			Query  string         `json:"query"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastParams = body.Params
		if body.Query != journal.QueryName {
			f.t.Errorf("查询名 = %q，期望 %q", body.Query, journal.QueryName)
		}
		w.WriteHeader(f.queryStatus)
		_, _ = io.WriteString(w, f.queryBody)
	case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/streams":
		f.streamCalls++
		w.WriteHeader(f.createStatus)
		_, _ = io.WriteString(w, f.createBody)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/internal/v1/streams/"):
		f.streamCalls++
		w.WriteHeader(f.streamCode)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, f.streamBody)
	default:
		f.t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// newFetcherAgainst 把真实的 agentclient 指到假的代理进程上。
func newFetcherAgainst(t *testing.T, handler http.Handler) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	client, err := agentclient.New("/run/upanel/agent.sock",
		agentclient.WithDialer(func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}))
	if err != nil {
		t.Fatalf("构造 agentclient 失败: %v", err)
	}
	fetcher, err := NewFetcher(client, quietLogger())
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	return fetcher
}

func envelope(data string) string {
	return `{"data":` + data + `,"request_id":"rid-test"}`
}

func errorEnvelope(code, message string) string {
	raw, _ := json.Marshal(map[string]string{"code": code, "message": message})
	return `{"error":` + string(raw) + `}`
}

func samplePageJSON() string {
	return `{
		"source":"journal",
		"unit":"ssh.service",
		"from":"2026-10-21T02:00:00Z",
		"to":"2026-10-21T03:00:00Z",
		"entries":[{"cursor":"s=a;i=1;b=b;m=1;t=1;x=1","timestamp":"2026-10-21T02:59:00Z",
		            "level":"warning","message":"sshd: 收到连接","unit":"ssh.service","redacted":true}],
		"next_cursor":"s=a;i=0;b=b;m=1;t=1;x=0",
		"truncated":false,"scanned":2,"duration_ms":6
	}`
}

// TestQueryReturnsPage 验成功路径：参数原样送出去，页原样解析回来。
func TestQueryReturnsPage(t *testing.T) {
	fake := newFakeAgent(t)
	fake.queryBody = envelope(samplePageJSON())
	fetcher := newFetcherAgainst(t, fake)

	page, err := fetcher.Query(context.Background(), journal.Params{
		Unit: "ssh.service", Level: "warning", Limit: 50,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Source != journal.SourceJournal || len(page.Entries) != 1 {
		t.Fatalf("页不对: %+v", page)
	}
	if !page.Entries[0].Redacted || page.Entries[0].Message == "" {
		t.Fatalf("条目字段丢失: %+v", page.Entries[0])
	}
	if page.NextCursor == nil || *page.NextCursor == "" {
		t.Fatal("next_cursor 应透出")
	}
	if got := fake.lastParams["unit"]; got != "ssh.service" {
		t.Fatalf("unit 没有送到代理: %v", got)
	}
	if got := fake.lastParams["limit"]; got != float64(50) {
		t.Fatalf("limit 没有送到代理: %v", got)
	}
}

// TestQueryRejectsBadParamsLocally 参数错了不发请求：Web 层能判的就不该让代理判。
func TestQueryRejectsBadParamsLocally(t *testing.T) {
	fake := newFakeAgent(t)
	fake.queryBody = envelope(samplePageJSON())
	fetcher := newFetcherAgainst(t, fake)

	_, err := fetcher.Query(context.Background(), journal.Params{Unit: "ssh*"})
	if !errors.Is(err, journal.ErrInvalidParams) {
		t.Fatalf("错误 = %v，期望 ErrInvalidParams", err)
	}
	if fake.queryCalls != 0 {
		t.Fatalf("非法参数不该发出 %d 次请求", fake.queryCalls)
	}
}

// TestQueryMapsAgentRejection 代理回 400 时要变成 ErrInvalidParams（接口层据此回 400），
// 而不是被当成内部故障。
func TestQueryMapsAgentRejection(t *testing.T) {
	fake := newFakeAgent(t)
	fake.queryStatus = http.StatusBadRequest
	fake.queryBody = errorEnvelope("VALIDATION_ERROR", "to 超过当前时间，请检查时区")
	fetcher := newFetcherAgainst(t, fake)

	_, err := fetcher.Query(context.Background(), journal.Params{To: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if !errors.Is(err, journal.ErrInvalidParams) {
		t.Fatalf("错误 = %v，期望 ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "超过当前时间") {
		t.Fatalf("错误信息应保留代理给出的原因，实际 %v", err)
	}
	// 而且只保留代理那一句：再叠一层 “journal: 查询参数不合法: ” 会让用户看到
	// 三层前缀（真机载荷 err-future.json 记过这个），把一句参数错误说成内部故障。
	if got, want := err.Error(), "to 超过当前时间，请检查时区"; got != want {
		t.Fatalf("错误消息 = %q，期望原样透传代理的消息 %q", got, want)
	}
}

// TestQueryMapsUnavailable 代理连不上/报 503 时要变成 ErrAgentUnavailable（接口层回 503）。
func TestQueryMapsUnavailable(t *testing.T) {
	fake := newFakeAgent(t)
	fake.queryStatus = http.StatusServiceUnavailable
	fake.queryBody = errorEnvelope("AGENT_UNAVAILABLE", "journalctl 不可用")
	fetcher := newFetcherAgainst(t, fake)

	_, err := fetcher.Query(context.Background(), journal.Params{})
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("错误 = %v，期望 ErrAgentUnavailable", err)
	}
}

// TestQueryRejectsIncoherentPages 验对“看似正常但不自洽”的页的拒绝。
//
// 一个缺时间窗口或缺游标的页必须报错：页面会把它渲染成“这段时间没有日志”，
// 于是用户看到的是“一切正常”，而实际是代理没说清楚。
func TestQueryRejectsIncoherentPages(t *testing.T) {
	cases := []struct {
		name string
		page string
		want string
	}{
		{"缺时间窗口", `{"source":"journal","entries":[]}`, "时间窗口"},
		{"数据源不在白名单", `{"source":"dmesg","from":"2026-10-21T02:00:00Z","to":"2026-10-21T03:00:00Z","entries":[]}`, "数据源"},
		{"unit 不一致", `{"source":"journal","unit":"cron.service","from":"2026-10-21T02:00:00Z",
			"to":"2026-10-21T03:00:00Z","entries":[]}`, "unit"},
		{"条目缺游标", `{"source":"journal","from":"2026-10-21T02:00:00Z","to":"2026-10-21T03:00:00Z",
			"entries":[{"timestamp":"2026-10-21T02:59:00Z","level":"info","message":"x"}]}`, "cursor"},
		{"next_cursor 为空串", `{"source":"journal","from":"2026-10-21T02:00:00Z","to":"2026-10-21T03:00:00Z",
			"entries":[],"next_cursor":""}`, "next_cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAgent(t)
			fake.queryBody = envelope(tc.page)
			fetcher := newFetcherAgainst(t, fake)
			_, err := fetcher.Query(context.Background(), journal.Params{Unit: "ssh.service"})
			if !errors.Is(err, ErrAgentUnavailable) {
				t.Fatalf("错误 = %v，期望 ErrAgentUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应提到 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// TestStreamDeliversEntriesAndReason 验实时流：逐条交出去，结束原因如实返回。
func TestStreamDeliversEntriesAndReason(t *testing.T) {
	fake := newFakeAgent(t)
	fake.createBody = envelope(`{"stream_id":"abc123","purpose":"logs.stream","expires_at":"2026-10-21T03:00:30Z"}`)
	fake.streamBody = strings.Join([]string{
		`{"type":"entry","entry":{"cursor":"s=a;i=1;b=b;m=1;t=1;x=1","timestamp":"2026-10-21T03:00:00Z","level":"info","message":"第一条"}}`,
		`{"type":"entry","entry":{"cursor":"s=a;i=2;b=b;m=1;t=1;x=2","timestamp":"2026-10-21T03:00:01Z","level":"error","message":"第二条"}}`,
		`{"type":"end","reason":"idle_timeout"}`,
	}, "\n") + "\n"
	fetcher := newFetcherAgainst(t, fake)

	var got []journal.Entry
	reason, err := fetcher.StreamWithReason(context.Background(), journal.Params{Unit: "ssh.service"},
		func(entry journal.Entry) error {
			got = append(got, entry)
			return nil
		})
	if err != nil {
		t.Fatalf("流失败: %v", err)
	}
	if reason != journal.StreamEndIdle {
		t.Fatalf("结束原因 = %q，期望 %q", reason, journal.StreamEndIdle)
	}
	if len(got) != 2 || got[1].Level != journal.LevelError || got[0].Message != "第一条" {
		t.Fatalf("条目不对: %+v", got)
	}
	if fake.streamCalls != 2 {
		t.Fatalf("应当先申请许可再开流（共 2 次调用），实际 %d", fake.streamCalls)
	}
}

// TestStreamRequiresEndFrame 验“没有结束标记”与“正常结束”必须分开。
//
// 少了这一条，页面会在代理崩溃时显示“已结束”，用户以为日志看完了。
func TestStreamRequiresEndFrame(t *testing.T) {
	fake := newFakeAgent(t)
	fake.createBody = envelope(`{"stream_id":"abc123","purpose":"logs.stream","expires_at":"2026-10-21T03:00:30Z"}`)
	fake.streamBody = `{"type":"entry","entry":{"cursor":"s=a;i=1;b=b;m=1;t=1;x=1","timestamp":"2026-10-21T03:00:00Z"}}` + "\n"
	fetcher := newFetcherAgainst(t, fake)

	_, err := fetcher.StreamWithReason(context.Background(), journal.Params{},
		func(journal.Entry) error { return nil })
	if !errors.Is(err, ErrStreamInterrupted) {
		t.Fatalf("错误 = %v，期望 ErrStreamInterrupted", err)
	}
}

// TestStreamRejectsBadFrames 验各种不合规帧都不会被静默跳过。
func TestStreamRejectsBadFrames(t *testing.T) {
	cases := []struct {
		name   string
		frames string
		want   error
	}{
		{"end 没有原因", `{"type":"end"}`, ErrStreamInterrupted},
		{"未知帧类型", `{"type":"progress"}`, ErrStreamInterrupted},
		{"entry 缺时间", `{"type":"entry","entry":{"cursor":"s=a;i=1;b=b;m=1;t=1;x=1"}}`, ErrStreamInterrupted},
		{"entry 没有内容", `{"type":"entry"}`, ErrStreamInterrupted},
		{"空行不是帧", "\n\n" + `{"type":"end","reason":"duration_limit"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAgent(t)
			fake.createBody = envelope(`{"stream_id":"abc123","purpose":"logs.stream","expires_at":"2026-10-21T03:00:30Z"}`)
			fake.streamBody = tc.frames + "\n"
			fetcher := newFetcherAgainst(t, fake)
			_, err := fetcher.StreamWithReason(context.Background(), journal.Params{},
				func(journal.Entry) error { return nil })
			if tc.want == nil {
				if err != nil {
					t.Fatalf("不该报错，实际 %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误 = %v，期望 %v", err, tc.want)
			}
		})
	}
}

// TestStreamReportsAgentErrorFrame 代理报告失败时要把原因带出来，而不是安静结束。
func TestStreamReportsAgentErrorFrame(t *testing.T) {
	fake := newFakeAgent(t)
	fake.createBody = envelope(`{"stream_id":"abc123","purpose":"logs.stream","expires_at":"2026-10-21T03:00:30Z"}`)
	fake.streamBody = `{"type":"error","code":"STREAM_FAILED","message":"journalctl 退出码 1"}` + "\n"
	fetcher := newFetcherAgainst(t, fake)

	_, err := fetcher.StreamWithReason(context.Background(), journal.Params{},
		func(journal.Entry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "journalctl 退出码 1") {
		t.Fatalf("错误应带出代理的原因，实际 %v", err)
	}
}

// TestStreamPropagatesEmitError 上层写不动了要立刻收流：这是释放资源的上游信号。
func TestStreamPropagatesEmitError(t *testing.T) {
	fake := newFakeAgent(t)
	fake.createBody = envelope(`{"stream_id":"abc123","purpose":"logs.stream","expires_at":"2026-10-21T03:00:30Z"}`)
	fake.streamBody = `{"type":"entry","entry":{"cursor":"s=a;i=1;b=b;m=1;t=1;x=1","timestamp":"2026-10-21T03:00:00Z"}}` + "\n"
	fetcher := newFetcherAgainst(t, fake)

	boom := errors.New("客户端走了")
	_, err := fetcher.StreamWithReason(context.Background(), journal.Params{}, func(journal.Entry) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，期望原样返回 %v", err, boom)
	}
}

// TestStreamPermitRejected 许可被拒（400）要映射成参数错误；其它状态是普通拒绝。
func TestStreamPermitRejected(t *testing.T) {
	bad := newFakeAgent(t)
	bad.createStatus = http.StatusBadRequest
	bad.createBody = errorEnvelope("VALIDATION_ERROR", "unit 不是合法的单元名")
	fetcher := newFetcherAgainst(t, bad)
	if _, err := fetcher.StreamWithReason(context.Background(), journal.Params{},
		func(journal.Entry) error { return nil }); !errors.Is(err, journal.ErrInvalidParams) {
		t.Fatalf("错误 = %v，期望 ErrInvalidParams", err)
	}

	unknown := newFakeAgent(t)
	unknown.createStatus = http.StatusNotImplemented
	unknown.createBody = errorEnvelope("VALIDATION_ERROR", "未登记的流用途")
	fetcher = newFetcherAgainst(t, unknown)
	if _, err := fetcher.StreamWithReason(context.Background(), journal.Params{},
		func(journal.Entry) error { return nil }); err == nil ||
		errors.Is(err, journal.ErrInvalidParams) {
		t.Fatalf("501 不该被当成参数错误，实际 %v", err)
	}
}

// TestNewFetcherRequiresAgent 宁可构造时报错，也不要运行时用假实现顶上。
func TestNewFetcherRequiresAgent(t *testing.T) {
	if _, err := NewFetcher(nil, quietLogger()); err == nil {
		t.Fatal("agent 为 nil 时必须报错")
	}
}
