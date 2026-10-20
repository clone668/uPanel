package api

// P03.3 接口层用例：任务事件流（SSE）与日志分页。
//
// 这里用真实 httptest.Server 而不是 ResponseRecorder：SSE 的正确性有一半在**传输层**
// （chunked 刷新、写超时、连接是否真的断开），用内存 recorder 验不到。代价是每个用例
// 都要真的开连接、真的读帧、真的关连接——这正是要验的东西。

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
)

// streamFixture 是一个已提交的任务 + 一个真实监听中的 HTTP 服务。
type streamFixture struct {
	*jobFixture
	url    string
	jobID  string
	server *httptest.Server
}

// newStreamFixture 提交任务，并把代理侧事件准备好（进度 + 结束）。
func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	f := newJobFixture(t, nil)
	rec := f.submit(t, "key-stream", jobTestPhrase)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("提交任务失败: 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out jobSubmitResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析提交结果失败: %v", err)
	}
	if !strings.HasSuffix(out.EventsURL, "/events") {
		t.Fatalf("events_url = %q，期望以 /events 结尾", out.EventsURL)
	}

	f.agent.op.Stage = "succeeded"
	f.agent.op.Step = "finished"
	f.agent.op.StartedAt = "2026-10-20T09:00:01Z"
	f.agent.op.FinishedAt = "2026-10-20T09:00:03Z"
	// 对账走的是 GetOperation，用的是 stub 的 getOp；op 只是提交时 CreateOperation 的返回值。
	// 两个视图必须是同一份事实，否则事件流看到的是“代理从没前进过”。
	f.agent.getOp = f.agent.op
	f.agent.events = []agentclient.OperationEvent{
		{Seq: 1, Kind: "progress", Payload: json.RawMessage(`{"step":"ping_done","slices":3}`)},
		{Seq: 2, Kind: "finished", Payload: json.RawMessage(`{"outcome":"verified"}`)},
	}

	server := httptest.NewServer(f.srv.Handler())
	t.Cleanup(server.Close)
	return &streamFixture{jobFixture: f, url: server.URL, jobID: out.JobID, server: server}
}

// sseClient 是读帧的小工具：单个后台 goroutine 持续把流按空行切帧，
// nextFrame 只从 channel 取。每条流只有一个读者——多个 goroutine 抢同一个
// bufio.Reader 会把帧切碎，读出来的“证据”就不作数了。
type sseClient struct {
	resp   *http.Response
	frames chan string
	cancel context.CancelFunc
}

// openStream 打开一条事件流；lastEventID 为空表示从头开始。
func (f *streamFixture) openStream(t *testing.T, path, lastEventID string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+path, nil)
	if err != nil {
		cancel()
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", f.cookie.String())
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("打开事件流失败: %v", err)
	}
	client := &sseClient{resp: resp, frames: make(chan string, 64), cancel: cancel}
	go func() {
		defer close(client.frames)
		reader := bufio.NewReader(resp.Body)
		var b strings.Builder
		for {
			line, rerr := reader.ReadString('\n')
			b.WriteString(line)
			if rerr != nil {
				if b.Len() > 0 {
					client.frames <- b.String()
				}
				return
			}
			if line == "\n" || line == "\r\n" {
				client.frames <- b.String()
				b.Reset()
			}
		}
	}()
	return client
}

func (c *sseClient) close() {
	c.cancel()
	//nolint:errcheck // 关闭失败不影响用例结论
	c.resp.Body.Close()
}

// nextFrame 取一帧（以空行结尾）；超过 timeout 返回空串。
func (c *sseClient) nextFrame(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case frame, ok := <-c.frames:
		if !ok {
			return ""
		}
		return frame
	case <-time.After(timeout):
		return ""
	}
}

// TestJobEventsStreamsFrames 验帧格式：id、event、data 三段齐全，且内容来自真实事件。
func TestJobEventsStreamsFrames(t *testing.T) {
	f := newStreamFixture(t)
	stream := f.openStream(t, "/api/v1/jobs/"+f.jobID+"/events", "")
	defer stream.close()

	if ct := stream.resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}
	if cc := stream.resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", cc)
	}

	var frames []string
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		frame := stream.nextFrame(t, 3*time.Second)
		if frame == "" {
			break
		}
		frames = append(frames, frame)
		// 等到状态帧出现即可收工：那是这条流的终点。
		if strings.Contains(frame, "event: status") && strings.Contains(frame, `"succeeded"`) {
			break
		}
	}
	joined := strings.Join(frames, "---")
	for _, want := range []string{"event: progress", "event: status", `"ping_done"`, `"succeeded"`, "id: "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("事件流里没有 %q。收到的帧：\n%s", want, joined)
		}
	}
	// data 必须是合法 JSON，且 seq 与 id 一致：客户端靠 id 重连，靠 seq 排序。
	for _, frame := range frames {
		var id string
		var data string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" {
			continue
		}
		var payload struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("data 不是合法 JSON: %v（%s）", err, data)
		}
		if id != "" && id != itoaInt(payload.Seq) {
			t.Fatalf("帧的 id=%q 与 data.seq=%d 不一致", id, payload.Seq)
		}
	}
}

// TestJobEventsResumeSkipsDelivered 验接续：带 Last-Event-ID 重连不会重发已经看过的帧。
func TestJobEventsResumeSkipsDelivered(t *testing.T) {
	f := newStreamFixture(t)
	first := f.openStream(t, "/api/v1/jobs/"+f.jobID+"/events", "")
	var lastID string
	var frames []string
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		frame := first.nextFrame(t, 3*time.Second)
		if frame == "" {
			break
		}
		frames = append(frames, frame)
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "id: ") {
				lastID = strings.TrimPrefix(line, "id: ")
			}
		}
		if strings.Contains(frame, `"succeeded"`) {
			break
		}
	}
	first.close()
	if lastID == "" {
		t.Fatalf("第一轮没有收到任何带 id 的帧：\n%s", strings.Join(frames, "---"))
	}

	resumed := f.openStream(t, "/api/v1/jobs/"+f.jobID+"/events", lastID)
	defer resumed.close()
	if resumed.resp.StatusCode != http.StatusOK {
		t.Fatalf("重连状态码 = %d，期望 200", resumed.resp.StatusCode)
	}
	// 任务已经结束、没有新事件：重连后应当**一帧业务事件都没有**（心跳是注释行，不占帧）。
	if frame := resumed.nextFrame(t, 1500*time.Millisecond); strings.Contains(frame, "event: ") {
		t.Fatalf("重连后收到了已经看过的帧：\n%s", frame)
	}
}

// TestJobEventsRejectsBadLastEventID 验输入校验：非法位置直接 400，而不是开一条流然后乱补发。
func TestJobEventsRejectsBadLastEventID(t *testing.T) {
	f := newStreamFixture(t)
	req, err := http.NewRequest(http.MethodGet, f.url+"/api/v1/jobs/"+f.jobID+"/events", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Cookie", f.cookie.String())
	req.Header.Set("Last-Event-ID", "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

// TestJobEventsRequiresAdmin 验权限：viewer 能看摘要，但看不了流。
func TestJobEventsRequiresAdmin(t *testing.T) {
	f := newStreamFixture(t)
	f.createUser(t, "watcher1", "viewer", "viewer-pass-123")
	cookie, _ := loginAsUser(t, f.srv, "watcher1", "viewer-pass-123")

	req, err := http.NewRequest(http.MethodGet, f.url+"/api/v1/jobs/"+f.jobID+"/events", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Cookie", cookie.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer 打开事件流的状态码 = %d，期望 403", resp.StatusCode)
	}
}

// TestJobEventsGapAfterPrune 验历史清理后的重连：发一帧 gap 让客户端重新拉状态，
// 而不是假装“没有新事件”把页面冻在旧进度上。
//
// 不能靠删除来伪造“已清理”——job_events 的 DELETE 触发器就是用来禁止这件事的。
// 所以这里造一个**事件从 seq=5 开始**的任务，等价于 seq 1..4 已被容量上限回收。
func TestJobEventsGapAfterPrune(t *testing.T) {
	f := newStreamFixture(t)
	synthID := "job_0000000000000000000000000000gap1"
	var userID int64
	if err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT id FROM users WHERE username = ?`, testUser).Scan(&userID); err != nil {
		t.Fatalf("读取测试账号失败: %v", err)
	}
	now := "2026-10-20T09:00:00Z"
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO jobs (id, operation_id, preflight_id, user_id, action, status,
		                   step, created_at, updated_at, needs_sync)
		 VALUES (?, 'op_00000000000000000000000000gap01', ?, ?, 'acceptance.probe',
		         'running', 'ping', ?, ?, 1)`,
		synthID, f.preflightID, userID, now, now); err != nil {
		t.Fatalf("插入合成任务失败: %v", err)
	}
	for _, seq := range []int64{5, 6} {
		if _, err := f.db.ExecContext(context.Background(),
			`INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
			 VALUES (?, ?, 'status', '{"status":"running"}', ?)`,
			synthID, seq, now); err != nil {
			t.Fatalf("插入合成事件失败: %v", err)
		}
	}

	stream := f.openStream(t, "/api/v1/jobs/"+synthID+"/events", "1")
	defer stream.close()
	// 第一帧永远是 ": connected" 注释（客户端据此知道流已建立），gap 在它之后。
	var frame string
	for i := 0; i < 3; i++ {
		frame = stream.nextFrame(t, 2*time.Second)
		if frame == "" {
			break
		}
		if strings.Contains(frame, "event: gap") {
			break
		}
	}
	if !strings.Contains(frame, "event: gap") || !strings.Contains(frame, "history_pruned") {
		t.Fatalf("期望收到 gap 帧，实际：\n%s", frame)
	}
}

// TestJobEventsConnectionLimit 验并发连接上限：第 5 条流返回 429，而不是把 fd 耗光。
func TestJobEventsConnectionLimit(t *testing.T) {
	f := newStreamFixture(t)
	path := "/api/v1/jobs/" + f.jobID + "/events"

	var open []*sseClient
	for i := 0; i < sseMaxConnsPerUser; i++ {
		c := f.openStream(t, path, "")
		if c.resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 条流的状态码 = %d，期望 200", i+1, c.resp.StatusCode)
		}
		open = append(open, c)
	}
	defer func() {
		for _, c := range open {
			c.close()
		}
	}()

	req, err := http.NewRequest(http.MethodGet, f.url+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Cookie", f.cookie.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第 %d 条流的状态码 = %d，期望 429", sseMaxConnsPerUser+1, resp.StatusCode)
	}
}

// TestJobLogsEndpoint 验日志分页接口的形状与游标校验。
func TestJobLogsEndpoint(t *testing.T) {
	f := newStreamFixture(t)
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
		 SELECT ?, COALESCE(MAX(seq),0) + 1, 'log', '{"message":"第一行"}', '2026-10-20T09:00:05Z'
		   FROM job_events WHERE job_id = ?`, f.jobID, f.jobID); err != nil {
		t.Fatalf("插入日志失败: %v", err)
	}

	rec := f.do(t, http.MethodGet, "/api/v1/jobs/"+f.jobID+"/logs?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取日志失败: 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Logs []struct {
			Seq     int64           `json:"seq"`
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		} `json:"logs"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(parseResult(t, rec).Data, &body); err != nil {
		t.Fatalf("解析日志失败: %v", err)
	}
	if len(body.Logs) != 1 || body.Logs[0].Kind != "log" {
		t.Fatalf("日志 = %+v，期望 1 条 log", body.Logs)
	}
	if !strings.Contains(string(body.Logs[0].Payload), "第一行") {
		t.Fatalf("日志内容 = %s", body.Logs[0].Payload)
	}
	if body.NextCursor != nil {
		t.Fatalf("只有一条日志时 next_cursor 应为 null，实际 %v", *body.NextCursor)
	}

	// 游标是 base64url("log:<seq>")。这里给一个能解码、但前缀不对的值：
	// 只验“解不开 base64”是不够的，那会把“接受任意 base64”也放过去。
	bad := f.do(t, http.MethodGet, "/api/v1/jobs/"+f.jobID+"/logs?cursor=bm90LWEtY3Vyc29y", "")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("非法游标的状态码 = %d，期望 400；响应 %s", bad.Code, bad.Body.String())
	}
	if got := parseResult(t, bad).Error.Code; got != "VALIDATION_ERROR" {
		t.Fatalf("非法游标的错误码 = %q，期望 VALIDATION_ERROR", got)
	}
}

func itoaInt(v int64) string {
	return strconv.FormatInt(v, 10)
}
