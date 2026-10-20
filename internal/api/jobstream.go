package api

// P03.3：任务的实时事件流（SSE）与脱敏日志分页（CONTRACTS.md 4.3）。
//
// 这里有三条不能商量的纪律，写之前先摆出来：
//
//  1. **流里只有已经发生过的事实**。事件来自 job_events，而 job_events 由代理事件镜像而来。
//     所以“进度未知”就真的什么都不发，绝不用一个定时器把 0 涨到 100 糊住页面。
//
//  2. **重连不重执行**。Last-Event-ID 只决定“从哪一条之后补发”，与任务本身没有任何关系；
//     这条路径上不存在任何会让代理再干一次活的调用（最多是一次只读的 GetJob 对账）。
//
//  3. **慢客户端不拖垮服务**。没有“无限缓冲”。每次写都带写超时，写不动就断开连接；
//     同时限制单个用户的并发连接数——一个页面反复刷新不该把进程的 fd 用光。
//
// 为什么用轮询数据库而不是进程内广播：写操作发生在别的请求里（提交、取消、对账），
// 也可能发生在重启后。广播要么丢事件（订阅建立之前写的），要么要在进程里长期保存状态；
// 而 job_events 是持久、有序、带 seq 的真相来源，按 seq 增量读它永远不会漏。
// 1 秒一次的主键前缀查询对一个单机面板来说是便宜的，换来的是“不漏、不重、可重连”。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/httpx"
	"upanel/internal/operations"
)

const (
	// ssePollInterval 是轮询 job_events 的间隔。
	ssePollInterval = time.Second
	// sseHeartbeatInterval 是注释心跳间隔（CONTRACTS.md 4.3：每 15 秒）。
	sseHeartbeatInterval = 15 * time.Second
	// sseWriteTimeout 是单次写入的时间上限。超时即断开：慢客户端拿不到无限缓冲。
	sseWriteTimeout = 10 * time.Second
	// sseMaxConnsPerUser 是单个用户可同时打开的事件流数量。
	//
	// 4 个够用：一个页面同时看一个任务，多开几个标签页也能容忍；再往上就是脚本在刷，
	// 那种情况应当被挡住而不是让进程陪着耗尽 fd。
	sseMaxConnsPerUser = 4
)

// streamLimiter 统计每个用户当前打开的事件流数量。
//
// 用 mutex + map 而不是原子计数：需要按用户维度分别限额，而且要能安全释放。
type streamLimiter struct {
	mu    sync.Mutex
	conns map[int64]int
}

func newStreamLimiter() *streamLimiter {
	return &streamLimiter{conns: make(map[int64]int)}
}

// acquire 尝试占用一个连接额度；超额返回 false 与 nil 释放函数。
func (l *streamLimiter) acquire(userID int64) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns[userID] >= sseMaxConnsPerUser {
		return nil, false
	}
	l.conns[userID]++
	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return
		}
		released = true
		if l.conns[userID] <= 1 {
			delete(l.conns, userID)
			return
		}
		l.conns[userID]--
	}, true
}

// count 返回某用户当前的连接数（仅测试使用）。
func (l *streamLimiter) count(userID int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conns[userID]
}

// jobEventDTO 是事件在 SSE 与日志接口里的形状。
type jobEventDTO struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// jobLogsResponse 是 GET /api/v1/jobs/:id/logs 的响应体。
//
// next_cursor 为 null 表示没有下一页（CONTRACTS.md 第 1 节：列表统一形态）。
type jobLogsResponse struct {
	Logs       []jobEventDTO `json:"logs"`
	NextCursor *string       `json:"next_cursor"`
}

func jobEventFromStore(ev operations.JobEvent) jobEventDTO {
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return jobEventDTO{
		Seq:       ev.Seq,
		Kind:      ev.Kind,
		Payload:   payload,
		CreatedAt: formatRFC3339(ev.CreatedAt),
	}
}

// handleJobEvents 处理 GET /api/v1/jobs/:id/events（SSE，仅 admin）。
func (s *Server) handleJobEvents(c *gin.Context) {
	sess := sessionFrom(c)
	jobID := c.Param("id")

	// 归属与存在性在**发头之前**判定：一旦写成 200 + text/event-stream，
	// 就没法再用 JSON 告诉客户端“这个任务不是你的”，EventSource 也不会把它当错误。
	if _, err := s.opts.Operations.GetJob(c.Request.Context(), sess.User.ID, jobID); err != nil {
		s.writeJobError(c, &sess, audit.ActionJobSubmit, "job:"+jobID, err)
		return
	}

	after, ok := parseLastEventID(c)
	if !ok {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"Last-Event-ID 必须是 0 或正整数")
		return
	}

	release, allowed := s.streams.acquire(sess.User.ID)
	if !allowed {
		// 429 与登录限速同一套语义（DECISIONS Q24）：不是请求格式错，是"现在别开"。
		writeError(c, http.StatusTooManyRequests, httpx.CodeValidationError,
			"同一个账号同时打开的事件流过多，请先关闭多余的标签页")
		return
	}
	defer release()

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// 反向代理（P10 起可能加）默认会缓冲响应，事件流必须显式关掉。
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	stream := &sseWriter{w: c.Writer, rc: http.NewResponseController(c.Writer)}
	// 立刻刷一次头，让客户端知道流已建立（也顺手验证了写超时链路可用）。
	if !stream.comment("connected") {
		return
	}

	ctx := c.Request.Context()
	lastSent := after
	// 首轮做一次 gap 判定：客户端带来的位置如果已经落在被清理的历史里，
	// 补发是不可能的，唯一诚实的做法是让它重新拉状态。
	if after > 0 {
		bounds, berr := s.opts.Operations.Bounds(ctx, sess.User.ID, jobID)
		if berr != nil {
			s.logStreamError(sess.User.ID, jobID, berr)
			return
		}
		if bounds.MaxSeq > 0 && after < bounds.MinSeq-1 {
			stream.event(0, "gap", map[string]any{
				"reason":  "history_pruned",
				"min_seq": bounds.MinSeq,
				"max_seq": bounds.MaxSeq,
			})
			return
		}
	}
	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()
	beat := time.NewTicker(sseHeartbeatInterval)
	defer beat.Stop()

	for {
		// 先把攒下的事件灌完（一批最多 MaxJobEventPage 条），再等下一次。
		for {
			events, lerr := s.opts.Operations.ListJobEvents(ctx, sess.User.ID, jobID, lastSent, operations.MaxJobEventPage)
			if lerr != nil {
				// 读不到就断开：不重试、不吞掉。重连由客户端负责，
				// 断开比“静默停在一半”更诚实。
				s.logStreamError(sess.User.ID, jobID, lerr)
				return
			}
			if len(events) == 0 {
				break
			}
			for _, ev := range events {
				if !stream.event(ev.Seq, ev.Kind, jobEventFromStore(ev)) {
					return
				}
				lastSent = ev.Seq
			}
			if len(events) < operations.MaxJobEventPage {
				break
			}
		}

		// 每轮顺带对账一次：进度事件是代理写的，只有有人在看的时候它才会被镜像进来。
		// 对账本身是只读的（GetOperation），不会让代理再执行一次任务。
		if _, rerr := s.opts.Operations.GetJob(ctx, sess.User.ID, jobID); rerr != nil {
			var rejected *operations.Rejected
			if errors.As(rerr, &rejected) || errors.Is(rerr, operations.ErrJobNotFound) {
				// 任务被删或归属变化：流没有继续的意义。
				return
			}
			// 其他错误（代理暂不可用）不终止流：任务还在，等下一次轮询。
		}

		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			if !stream.comment("keepalive") {
				return
			}
		case <-poll.C:
		}
	}
}

// handleJobLogs 处理 GET /api/v1/jobs/:id/logs?cursor=&limit=（仅 admin）。
func (s *Server) handleJobLogs(c *gin.Context) {
	sess := sessionFrom(c)
	jobID := c.Param("id")

	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > operations.MaxJobEventPage {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"limit 必须是 1—"+strconv.Itoa(operations.MaxJobEventPage)+" 的整数")
			return
		}
		limit = n
	}

	events, next, err := s.opts.Operations.ListJobLogs(c.Request.Context(), sess.User.ID, jobID, c.Query("cursor"), limit)
	if err != nil {
		if errors.Is(err, operations.ErrInvalidLogCursor) {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "游标无效：请使用上一页返回的 next_cursor")
			return
		}
		s.writeJobError(c, &sess, audit.ActionJobSubmit, "job:"+jobID, err)
		return
	}

	out := make([]jobEventDTO, 0, len(events))
	for _, ev := range events {
		out = append(out, jobEventFromStore(ev))
	}
	resp := jobLogsResponse{Logs: out}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// parseLastEventID 解析 Last-Event-ID。
//
// 头是标准位置（EventSource 重连时自动带上）；查询参数只用于调试与验收脚本，
// 两者都接受，但语义完全相同。空值 = 从头补发（0）。
func parseLastEventID(c *gin.Context) (int64, bool) {
	raw := strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(c.Query("last_event_id"))
	}
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// logStreamError 记录流的异常终止。事件流断在日志里必须留痕，否则“页面不动了”无从排查。
func (s *Server) logStreamError(userID int64, jobID string, err error) {
	if err == nil {
		return
	}
	s.opts.Logger.Warn("任务事件流中断", "user_id", userID, "job_id", jobID, "error", err)
}

// sseWriter 负责按 SSE 帧格式写出一条事件，并给每次写设上限。
//
// 帧格式（严格按规范，字段顺序固定为 id → event → data）：
//
//	id: 12
//	event: status
//	data: {"seq":12,...}
//
//	data 里不放换行以外的东西；JSON 序列化保证不会有裸换行。
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// event 写一条命名事件；失败（写超时、对端关闭）返回 false，调用方据此退出。
func (sw *sseWriter) event(seq int64, kind string, payload any) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	var b strings.Builder
	if seq > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(seq, 10))
		b.WriteString("\n")
	}
	b.WriteString("event: ")
	b.WriteString(kind)
	b.WriteString("\n")
	b.WriteString("data: ")
	b.Write(body)
	b.WriteString("\n\n")
	return sw.write(b.String())
}

// comment 写一条注释行（心跳）。
func (sw *sseWriter) comment(text string) bool {
	return sw.write(": " + text + "\n\n")
}

func (sw *sseWriter) write(frame string) bool {
	if sw.rc != nil {
		// 每次写都刷新一次截止时间：http.Server 的 WriteTimeout 是绝对的，
		// 不刷新的话 30 秒后一条长流就被整体砍断（表现为“页面每 30 秒重连一次”）。
		if err := sw.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
			// 底层不支持设截止时间（例如某些包装 writer）：不因为拿不到能力就拒绝推流。
			_ = err
		}
	}
	if _, err := sw.w.Write([]byte(frame)); err != nil {
		return false
	}
	if f, ok := sw.w.(http.Flusher); ok {
		f.Flush()
	}
	return true
}
