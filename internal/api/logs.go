package api

// P05.3：日志查询与实时流（CONTRACTS.md 第 10 节）。
//
// 三条纪律：
//
//  1. **只有 admin**。原始日志里什么都可能有：口令形态的字符串、内网地址、别人的
//     命令行。PLAN.md 第 14 节写得很清楚——“过滤秘密是尽力措施，不能作为向 viewer
//     开放日志的依据”。所以 viewer 在这里得到 403，而不是“脱敏后的日志”。
//
//  2. **限额在服务端**。时间范围、条数、字节、单条消息长度都由代理侧强制，
//     前端传什么都不改变这些上限；本层再做一次参数校验只是为了给 400 而不是 500。
//
//  3. **流断开就释放**。SSE 的写入失败或客户端断开会让请求 ctx 结束，
//     该 ctx 是一直传到代理的 http 请求上的；代理看到连接断开即杀掉 journalctl。
//     P05.3 验收里那条“结束日志订阅不会残留无限 journalctl 子进程”验的就是这条链路。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/journal"
	"upanel/internal/logs"
)

// LogsReader 是日志读取端（P05.3）。生产实现是 *logs.Fetcher。
type LogsReader interface {
	Query(ctx context.Context, params journal.Params) (journal.Page, error)
	StreamWithReason(ctx context.Context, params journal.Params, emit func(journal.Entry) error) (journal.StreamEnd, error)
}

// logsResponse 是 GET /api/v1/logs 的响应体。
//
// 直接复用 journal.Page 的字段而不是再造一份 DTO：代理返回的、Web 返回的与
// 前端消费的必须是同一套字段名（CONTRACTS.md 第 16 节）。
type logsResponse = journal.Page

// handleLogs 处理 GET /api/v1/logs。
func (s *Server) handleLogs(c *gin.Context) {
	params, ok := s.parseLogParams(c)
	if !ok {
		return
	}
	reader := s.opts.Logs
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"日志不可用：未配置代理连接")
		return
	}
	page, err := reader.Query(c.Request.Context(), params)
	if err != nil {
		s.writeLogsError(c, err, "读取日志失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, logsResponse(page))
}

// handleLogsStream 处理 GET /api/v1/logs/stream（SSE，仅 admin）。
func (s *Server) handleLogsStream(c *gin.Context) {
	params, ok := s.parseLogParams(c)
	if !ok {
		return
	}
	// 断线续传：EventSource 会把上一次的 id 作为 Last-Event-ID 带回来，
	// 我们发出去的 id 就是 journal 游标，于是“续传”等于“从这个游标之后接着发”。
	if cursor := strings.TrimSpace(c.GetHeader("Last-Event-ID")); cursor != "" && params.Cursor == "" {
		params.Cursor = cursor
	}
	if err := params.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	reader := s.opts.Logs
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"日志不可用：未配置代理连接")
		return
	}

	sess := sessionFrom(c)
	// 日志流与任务事件流共用同一份额度：都是“一个管理员能同时开几条实时连接”。
	release, allowed := s.streams.acquire(sess.User.ID)
	if !allowed {
		writeError(c, http.StatusTooManyRequests, httpx.CodeValidationError,
			"同一个账号同时打开的流过多，请先关闭多余的标签页")
		return
	}
	defer release()

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	stream := &sseWriter{w: c.Writer, rc: http.NewResponseController(c.Writer)}
	if !stream.comment("connected") {
		return
	}

	ctx := c.Request.Context()
	// 代理侧一条流的时长/空闲上限是分钟级（10 分钟 / 60 秒），这里的心跳必须
	// 比它短得多，否则安静系统上页面会先看到“连接没有动静”。
	entries := make(chan journal.Entry, 64)
	done := make(chan struct{})
	var (
		reason journal.StreamEnd
		err    error
	)
	go func() {
		defer close(done)
		reason, err = reader.StreamWithReason(ctx, params, func(entry journal.Entry) error {
			select {
			case entries <- entry:
				return nil
			case <-ctx.Done():
				// 写不动了：立刻让上层收流，代理那边会跟着释放子进程。
				return ctx.Err()
			}
		})
	}()

	beat := time.NewTicker(sseHeartbeatInterval)
	defer beat.Stop()

	writeEntry := func(entry journal.Entry) bool {
		return stream.eventWithID(entry.Cursor, "entry", entry)
	}

	for {
		select {
		case <-ctx.Done():
			// 客户端走了。等 goroutine 自己收尾（它拿的是同一个 ctx），
			// 但不在这里阻塞：代理侧的资源由它释放。
			return
		case entry := <-entries:
			if !writeEntry(entry) {
				return
			}
		case <-beat.C:
			if !stream.comment("keepalive") {
				return
			}
		case <-done:
			// 收尾：把缓冲里剩下的条目写完，再写结束原因。
			for {
				select {
				case entry := <-entries:
					if !writeEntry(entry) {
						return
					}
				default:
					if err != nil && !errors.Is(err, context.Canceled) {
						s.logStreamFailure(sess.User.ID, params, err)
						stream.eventWithID("", "error", map[string]any{
							"code":    "STREAM_FAILED",
							"message": err.Error(),
						})
						return
					}
					stream.eventWithID("", "end", map[string]any{"reason": string(reason)})
					return
				}
			}
		}
	}
}

// logParamsFromRequest 把查询串读成 journal.Params。
//
// 只认 CONTRACTS.md 第 10 节列出的字段（外加 PLAN.md 第 14 节要求的 level）：
// 多出来的查询参数不影响结果，但拼错的字段名也不会被悄悄忽略——它们在 Validate
// 里没有对应项，最终表现为“没有按那个条件过滤”，这正是我们要避免的静默失败。
// 因此这里对每个允许的键都用精确名字读取，别的键一律不看也不猜。
func (s *Server) parseLogParams(c *gin.Context) (journal.Params, bool) {
	params := journal.Params{
		Source: strings.TrimSpace(c.Query("source")),
		Unit:   strings.TrimSpace(c.Query("unit")),
		Level:  strings.TrimSpace(c.Query("level")),
		From:   strings.TrimSpace(c.Query("from")),
		To:     strings.TrimSpace(c.Query("to")),
		Cursor: strings.TrimSpace(c.Query("cursor")),
	}
	// Go 1.17 起 url.ParseQuery 把未转义的 ';' 当成非法分隔符：它会把那对参数丢掉
	// 并返回错误，而 Gin 的 c.Query 只看解析出来的 map。结果就是
	// “cursor=s=…;i=…;x=…” 这种没转义的游标被当成“没给游标”，查询退回第一页，
	// 翻页拿着同一个 next_cursor 原地打转（真实验收里撞到过）。
	// 静默忽略比报错更难查，所以这里直接把不合法的查询串说成 400。
	if _, err := url.ParseQuery(c.Request.URL.RawQuery); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"查询串格式不合法：特殊字符必须 percent-encode（游标里的 ';' 应写成 %3B）")
		return journal.Params{}, false
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		// 显式的 0 不是“用默认值”：契约里 limit 的下界是 1，给了就必须在范围内。
		// Params.Limit 的零值表示“没给”，两者不能混为一谈。
		if err != nil || n < 1 || n > journal.MaxLimit {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"limit 必须是 1—"+strconv.Itoa(journal.MaxLimit)+" 的整数")
			return journal.Params{}, false
		}
		params.Limit = n
	}
	if err := params.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return journal.Params{}, false
	}
	return params, true
}

// writeLogsError 把读取错误翻译成 HTTP 状态。
func (s *Server) writeLogsError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	switch {
	case errors.Is(err, journal.ErrInvalidParams):
		// 参数问题（时间格式、跨度、级别）不是服务端故障。
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
	case errors.Is(err, journal.ErrCursorRejected):
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"游标无效：请重新查询第一页，不要手工构造 cursor")
	case errors.Is(err, logs.ErrAgentUnavailable):
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法读取日志")
	default:
		s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
	}
}

// logStreamFailure 记录流的异常结束。断在日志里必须留痕，否则“页面不动了”无从排查。
func (s *Server) logStreamFailure(userID int64, params journal.Params, err error) {
	if err == nil {
		return
	}
	s.opts.Logger.Warn("日志流中断",
		"user_id", userID,
		"source", params.Source,
		"unit", params.Unit,
		"error", err)
}
