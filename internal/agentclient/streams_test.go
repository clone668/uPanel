package agentclient

// P05.3：受限流客户端的用例。
//
// 三条要验的纪律：
//   - 流客户端**不能**继承单次调用的整体超时（否则一条实时日志流只有 10 秒寿命）；
//   - 帧必须逐行交出来，空行跳过、非法帧报错、超长帧拒绝；
//   - 打开失败要分清“代理不可用”与“代理拒绝”（后者带状态码与错误码）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// streamAgent 是假的代理进程：只认 streams 两个路由。
type streamAgent struct {
	t *testing.T

	createStatus int
	createBody   string
	streamStatus int
	streamBody   string
	lastPurpose  string
	lastParams   map[string]any
	gotStreamID  string
}

func (a *streamAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/streams":
		var body struct {
			Purpose string         `json:"purpose"`
			Params  map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.lastPurpose = body.Purpose
		a.lastParams = body.Params
		w.WriteHeader(a.createStatus)
		_, _ = io.WriteString(w, a.createBody)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/internal/v1/streams/"):
		a.gotStreamID = strings.TrimPrefix(r.URL.Path, "/internal/v1/streams/")
		w.WriteHeader(a.streamStatus)
		_, _ = io.WriteString(w, a.streamBody)
	default:
		a.t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func newStreamClient(t *testing.T, agent *streamAgent) *Client {
	t.Helper()
	agent.t = t
	if agent.createStatus == 0 {
		agent.createStatus = http.StatusCreated
	}
	if agent.streamStatus == 0 {
		agent.streamStatus = http.StatusOK
	}
	return newTestClient(t, agent)
}

// TestStreamClientHasNoOverallTimeout 锁住“流不能套整体超时”这条：
// 客户端的普通请求有 DefaultTimeout，流客户端必须是 0。
func TestStreamClientHasNoOverallTimeout(t *testing.T) {
	c := newTestClient(t, http.NotFoundHandler())
	if c.http.Timeout != DefaultTimeout {
		t.Fatalf("普通客户端超时 = %v，期望 %v", c.http.Timeout, DefaultTimeout)
	}
	if c.stream.Timeout != 0 {
		t.Fatalf("流客户端超时 = %v，期望 0（由服务端决定流的寿命）", c.stream.Timeout)
	}
	base, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("普通客户端 Transport 类型 = %T", c.http.Transport)
	}
	if base.ResponseHeaderTimeout != DefaultTimeout {
		t.Fatalf("普通客户端的等待响应头超时 = %v，期望 %v", base.ResponseHeaderTimeout, DefaultTimeout)
	}
	streamTransport, ok := c.stream.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("流客户端 Transport 类型 = %T", c.stream.Transport)
	}
	// 流不能继承“等待响应头”超时：安静的系统上第一条日志可能几十秒才来，
	// 那个 10 秒计时器会把一条正常的流掐掉（真实 VM 上表现为
	// “timeout awaiting response headers”，代理侧的空闲上限根本没机会生效）。
	if streamTransport.ResponseHeaderTimeout != 0 {
		t.Fatalf("流客户端的等待响应头超时 = %v，期望 0", streamTransport.ResponseHeaderTimeout)
	}
	// 但拨号器必须是同一个：交换 socket 路径或测试注入的 dialer 失效都会在这里暴露。
	if reflect.ValueOf(streamTransport.DialContext).Pointer() != reflect.ValueOf(base.DialContext).Pointer() {
		t.Fatal("流客户端必须复用同一个 DialContext（否则连的不是同一个 socket）")
	}
}

// TestCreateStreamSendsPurposeAndParams 验许可请求的形状。
func TestCreateStreamSendsPurposeAndParams(t *testing.T) {
	agent := &streamAgent{createBody: `{"data":{"stream_id":"abc","purpose":"logs.stream","expires_at":"2026-10-21T03:00:30Z"}}`}
	c := newStreamClient(t, agent)

	permit, err := c.CreateStream(context.Background(), LogsStreamPurpose(),
		map[string]string{"unit": "ssh.service"})
	if err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	if permit.StreamID != "abc" || permit.Purpose != "logs.stream" || permit.ExpiresAt == "" {
		t.Fatalf("许可 = %+v", permit)
	}
	if agent.lastPurpose != "logs.stream" {
		t.Fatalf("用途 = %q", agent.lastPurpose)
	}
	if got := agent.lastParams["unit"]; got != "ssh.service" {
		t.Fatalf("参数没有送出: %v", agent.lastParams)
	}
}

// TestCreateStreamRejectsIncompletePermit 验缺字段的许可不会被当成可用。
func TestCreateStreamRejectsIncompletePermit(t *testing.T) {
	for _, body := range []string{
		`{"data":{"purpose":"logs.stream","expires_at":"2026-10-21T03:00:30Z"}}`,
		`{"data":{"stream_id":"abc","expires_at":"2026-10-21T03:00:30Z"}}`,
		`{"data":{"stream_id":"abc","purpose":"logs.stream"}}`,
	} {
		agent := &streamAgent{createBody: body}
		c := newStreamClient(t, agent)
		if _, err := c.CreateStream(context.Background(), LogsStreamPurpose(), nil); err == nil {
			t.Fatalf("%s 应被拒绝", body)
		}
	}
}

// TestCreateStreamRequiresPurpose 用途为空不发请求：那一定是我们自己的编程错误。
func TestCreateStreamRequiresPurpose(t *testing.T) {
	agent := &streamAgent{}
	c := newStreamClient(t, agent)
	if _, err := c.CreateStream(context.Background(), "", nil); err == nil {
		t.Fatal("空用途必须报错")
	}
	if agent.lastPurpose != "" {
		t.Fatal("空用途不该发出请求")
	}
}

// TestOpenStreamReadsFrames 验逐帧读取：空行跳过、读到 EOF 结束。
func TestOpenStreamReadsFrames(t *testing.T) {
	agent := &streamAgent{streamBody: strings.Join([]string{
		`{"type":"entry","entry":{"cursor":"s=a;i=1;b=b;m=1;t=1;x=1","timestamp":"2026-10-21T03:00:00Z"}}`,
		``,
		`{"type":"end","reason":"duration_limit"}`,
	}, "\n") + "\n"}
	c := newStreamClient(t, agent)

	reader, err := c.OpenStream(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("开流失败: %v", err)
	}
	defer reader.Close()
	if agent.gotStreamID != "abc123" {
		t.Fatalf("流 id = %q", agent.gotStreamID)
	}

	var types []string
	for {
		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读帧失败: %v", err)
		}
		types = append(types, frame.Type)
		if frame.Type == "end" && frame.Reason != "duration_limit" {
			t.Fatalf("end 帧原因 = %q", frame.Reason)
		}
	}
	if len(types) != 2 || types[0] != "entry" || types[1] != "end" {
		t.Fatalf("帧序列 = %v，期望 [entry end]", types)
	}
}

// TestOpenStreamReportsRejection 验打开失败时带出状态码与错误码（而不是一律“不可用”）。
func TestOpenStreamReportsRejection(t *testing.T) {
	agent := &streamAgent{streamStatus: http.StatusNotFound,
		streamBody: `{"error":{"code":"NOT_FOUND","message":"流许可不存在或已失效"}}`}
	c := newStreamClient(t, agent)

	_, err := c.OpenStream(context.Background(), "gone")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误 = %v，期望 *Error", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != "NOT_FOUND" {
		t.Fatalf("错误 = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "流许可不存在") {
		t.Fatalf("错误信息应保留代理给的原因: %v", apiErr)
	}
}

// TestOpenStreamReportsUnavailable 响应不是错误信封时只能说“不可用”，不能编一个错误码。
func TestOpenStreamReportsUnavailable(t *testing.T) {
	agent := &streamAgent{streamStatus: http.StatusBadGateway, streamBody: "bad gateway"}
	c := newStreamClient(t, agent)
	_, err := c.OpenStream(context.Background(), "abc")
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("错误 = %v，期望 *UnavailableError", err)
	}
}

// TestOpenStreamRejectsBadFrames 验三种坏帧：非法 JSON、缺 type、超长。
func TestOpenStreamRejectsBadFrames(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", "{不是 JSON}\n"},
		{"缺 type", `{"reason":"x"}` + "\n"},
		{"超长帧", `{"type":"entry","entry":{"message":"` + strings.Repeat("x", maxStreamFrameBytes) + `"}}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &streamAgent{streamBody: tc.body}
			c := newStreamClient(t, agent)
			reader, err := c.OpenStream(context.Background(), "abc")
			if err != nil {
				t.Fatalf("开流失败: %v", err)
			}
			defer reader.Close()
			var unavailable *UnavailableError
			if _, err := reader.Next(); !errors.As(err, &unavailable) {
				t.Fatalf("错误 = %v，期望 *UnavailableError", err)
			}
		})
	}
}

// TestOpenStreamRequiresID 空流 id 不发请求。
func TestOpenStreamRequiresID(t *testing.T) {
	agent := &streamAgent{}
	c := newStreamClient(t, agent)
	if _, err := c.OpenStream(context.Background(), ""); err == nil {
		t.Fatal("空流 id 必须报错")
	}
	if agent.gotStreamID != "" {
		t.Fatal("空流 id 不该发出请求")
	}
}

// TestStreamReaderCloseIsIdempotent 验重复关闭不报错（defer 与正常路径都会关）。
func TestStreamReaderCloseIsIdempotent(t *testing.T) {
	agent := &streamAgent{streamBody: `{"type":"end","reason":"duration_limit"}` + "\n"}
	c := newStreamClient(t, agent)
	reader, err := c.OpenStream(context.Background(), "abc")
	if err != nil {
		t.Fatalf("开流失败: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("第一次 Close 失败: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("第二次 Close 失败: %v", err)
	}
}

// TestStreamDialFailureIsUnavailable 连不上代理时是 UnavailableError，而不是空流。
func TestStreamDialFailureIsUnavailable(t *testing.T) {
	c, err := New("/run/upanel/agent.sock", WithDialer(
		func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("没有这个 socket")
		}))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if _, err := c.OpenStream(context.Background(), "abc"); err == nil {
		t.Fatal("拨号失败必须报错")
	} else {
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("错误 = %v，期望 *UnavailableError", err)
		}
	}
}

// TestStreamReadRespectsContextCancel 验 ctx 取消会让读取结束：Web 侧断开靠的就是它。
func TestStreamReadRespectsContextCancel(t *testing.T) {
	body := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-body:
		}
	}))
	defer srv.Close()
	defer close(body)
	addr := strings.TrimPrefix(srv.URL, "http://")
	c, err := New("/run/upanel/agent.sock", WithDialer(
		func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := c.OpenStream(ctx, "abc")
	if err != nil {
		t.Fatalf("开流失败: %v", err)
	}
	defer reader.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := reader.Next(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消后应当返回错误")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消 ctx 后读取没有结束：代理侧会留下孤儿进程")
	}
}
