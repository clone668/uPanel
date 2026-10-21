package agent

// P06.3：终端字节通道与关闭入口。
//
// 字节通道复用 P05.3 的一次性许可：Web 先用 terminal.io 用途申请许可（参数里带
// 会话 id 与面板账号 id），再用这个 id 拨一条 WebSocket。许可在**升级之前**就被
// 消费掉，因此“握手失败还能重试同一个 id”是不存在的——重试必须重新申请。
//
// 为什么是 WebSocket 而不是继续用 NDJSON：
//   - 交互终端是双向的。NDJSON 只能“服务端往下推”，输入得另开一条请求，
//     而那意味着每条输入都要重新鉴权一次、也意味着两次输入之间的顺序无法保证；
//   - 帧要能承载 resize 这类控制消息，并且客户端只能发两类消息——这需要一个
//     明确的“现在开始不再是 HTTP”的时刻。WebSocket 的握手就是那个时刻。
//
// 代理侧不开 ping/pong：这条连接的另一端是 upanel-web（本机 socket，UID 已核验），
// 不是浏览器。浏览器的死活由 Web 用 ping 检测（见 internal/api/terminal.go），
// 代理只关心“Web 还连着没有”——socket 断开就是断开。

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"upanel/internal/httpx"
	"upanel/internal/terminal"
)

const (
	// terminalHandshakeTimeout 是 WebSocket 握手的时限。
	terminalHandshakeTimeout = 5 * time.Second
	// terminalReadBuffer / writeBuffer 是单条连接的缓冲区。终端流量是小块文本，
	// 4 KiB 足够；这里刻意不放大——大缓冲区会让“一帧 64 KiB”的上限失去意义。
	terminalReadBuffer  = 4096
	terminalWriteBuffer = 4096
	// terminalOutputChunk 是一次从 pty 读出的字节数。
	terminalOutputChunk = 32 << 10
)

// serveTerminalStream 把一条已核验的流许可升级成终端 WebSocket。
func (s *Server) serveTerminalStream(w http.ResponseWriter, r *http.Request, permit streamPermit) {
	if s.opts.Terminal == nil {
		httpx.WriteError(w, r, http.StatusNotImplemented, httpx.CodeNotImplemented,
			"代理未启用终端：没有 PTY 会话层")
		return
	}
	params, ok := permit.Params.(terminal.IOParams)
	if !ok {
		s.opts.Logger.Error("终端流许可的参数类型不对", "stream_id", permit.ID)
		httpx.WriteError(w, r, http.StatusInternalServerError,
			httpx.CodeInternalError, "流许可参数与用途不一致")
		return
	}

	// 先附加再升级：附加失败（不存在、不是你的、已被占用）要能回一个正常的
	// 错误信封，而不是升级之后在帧里说——那时浏览器已经拿到 101，
	// 它会把“连上了”当真。
	att, err := s.opts.Terminal.Attach(params.SessionID, params.UserID)
	if err != nil {
		s.writeTerminalError(w, r, err, "附加终端会话", "session_id", params.SessionID)
		return
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout: terminalHandshakeTimeout,
		ReadBufferSize:   terminalReadBuffer,
		WriteBufferSize:  terminalWriteBuffer,
		Subprotocols:     []string{terminal.ProtocolName},
		// 对端不是浏览器：它是 socket 后面的 upanel-web，UID 已经由 gate 核验过
		// （只允许 upanel-web 的 UID）。Origin 头在这一跳上没有任何意义，
		// 浏览器的 Origin 校验在 Web 那一侧做——那里才是唯一面向浏览器的地方。
		CheckOrigin: func(*http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// 升级失败时连接已经不在我们手里，只能记日志；会话要立刻回到
		// “未连接”，否则它会被一条从未存在的连接占住整整一个宽限期。
		att.Close()
		s.opts.Logger.Warn("终端 WebSocket 升级失败",
			"session_id", params.SessionID, "error", err, "request_id", httpx.RequestID(r))
		return
	}

	startedAt := time.Now()
	info := att.Info()
	s.opts.Logger.Info("终端已连接",
		"session_id", info.SessionID,
		"owner_user_id", info.UserID,
		"account", info.Account,
		"account_uid", info.AccountUID,
		"request_id", httpx.RequestID(r))

	s.pumpTerminal(conn, att, info)

	duration := time.Since(startedAt)
	s.opts.Logger.Info("终端连接结束",
		"session_id", info.SessionID,
		"owner_user_id", info.UserID,
		"duration_ms", duration.Milliseconds(),
		"request_id", httpx.RequestID(r))
}

// pumpTerminal 在一条已升级的连接上跑双向泵，直到任一侧结束。
func (s *Server) pumpTerminal(conn *websocket.Conn, att *terminal.Attachment, info terminal.SessionInfo) {
	// 兜底：无论从哪条路出去，连接最后都会被关掉。优雅路径是下面的 closeWith，
	// 它会先把关闭码发出去；这里只负责不留下一条半开的 socket。
	defer func() { _ = conn.Close() }()
	// 断开连接（不是结束会话）：会话进入宽限期，重连能接上同一个 PTY。
	defer att.Close()

	// 关闭码只能由一处发：泵有四个出口（写失败、读失败、协议拒绝、会话结束），
	// 谁先到谁定，后来的不覆盖。不给浏览器留一帧 close 的话，它只能看到
	// “连接异常关闭”（1006），把协议违规、超速、到期、掉线混成同一个现象。
	var closeOnce sync.Once
	closeWith := func(code int) {
		closeOnce.Do(func() {
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(code, ""),
				time.Now().Add(2*time.Second))
			_ = conn.Close()
		})
	}

	var writeMu sync.Mutex
	write := func(f terminal.Frame) bool {
		body, err := terminal.EncodeFrame(f)
		if err != nil {
			return false
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		// 每帧给一个写截止时间：对端进程卡死时不能把这条 goroutine 永久挂住。
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, body) == nil
	}
	if !write(terminal.Frame{
		Type:       terminal.FrameReady,
		SessionID:  info.SessionID,
		Account:    info.Account,
		AccountUID: info.AccountUID,
		Cols:       info.Cols,
		Rows:       info.Rows,
		ServerTime: time.Now().UTC().Format(time.RFC3339),
	}) {
		return
	}

	// 输出泵。读错误有两种含义：子进程退出（io.EOF）与 master 被关掉（会话结束）。
	// 两者都只意味着“不再有输出”，退出码由 Done 那条路负责发出去。
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buf := make([]byte, terminalOutputChunk)
		for {
			n, err := att.Read(buf)
			if n > 0 {
				if !write(terminal.NewOutputFrame(buf[:n])) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// 会话结束（子进程退出、空闲、超时、被关闭）：先把 exit 帧发出去，再断连接。
	// 关连接是为了让下面阻塞在 ReadMessage 的主循环醒过来——否则它会一直等到对端
	// 自己断开，而那可能永远不会发生。
	go func() {
		<-att.Done()
		exit, reason, hasExit := att.Exit()
		frame := terminal.Frame{Type: terminal.FrameExit, Reason: reason}
		if hasExit {
			if exit.HasCode {
				code := exit.Code
				frame.ExitCode = &code
			}
			frame.Signal = exit.Signal
		}
		write(frame)
		closeWith(terminal.CloseCodeNormal)
	}()

	limiter := terminal.NewInputLimiter()
readLoop:
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			// 对端先走了（关页、掉线、被 Web 掐断）：没有对端可通知，直接收尾。
			break
		}
		frame, err := terminal.DecodeClientFrame(payload)
		if err != nil {
			// 协议级拒绝：告诉对端为什么，然后断开。继续读只会让一个说错协议
			// 的客户端不断产生错误帧。
			write(terminal.Frame{Type: terminal.FrameError,
				Code: terminal.CodeFrameInvalid, Message: err.Error()})
			closeWith(terminal.CloseCodeFrameInvalid)
			break
		}
		switch frame.Type {
		case terminal.FrameInput:
			data, err := frame.InputData()
			if err != nil {
				write(terminal.Frame{Type: terminal.FrameError,
					Code: terminal.CodeFrameInvalid, Message: err.Error()})
				closeWith(terminal.CloseCodeFrameInvalid)
				break readLoop
			}
			if !limiter.Consume(len(data), time.Now().UnixNano()) {
				write(terminal.Frame{Type: terminal.FrameError,
					Code: terminal.CodeRateLimited,
					Message: fmt.Sprintf("输入速率超过上限（%d 字节/秒、%d 帧/秒），连接已断开",
						terminal.MaxInputBytesPerSecond, terminal.MaxInputFramesPerSecond)})
				closeWith(terminal.CloseCodeRateLimited)
				break readLoop
			}
			if _, err := att.Write(data); err != nil {
				// tty 已经没了（会话刚结束）：不必再说，exit 帧会跟上。
				closeWith(terminal.CloseCodeNormal)
				break readLoop
			}
		case terminal.FrameResize:
			// 调不动就算了：窗口尺寸变化不是致命错误，客户端会再发一次。
			_ = att.Resize(frame.Cols, frame.Rows)
		}
	}

	// 等输出泵收尾，避免“连接关了但还有 goroutine 在往它写”的竞态。
	// 会话已经结束时 Read 会立刻返回；会话还在时 master 由 att.Close 关掉。
	att.Close()
	select {
	case <-outputDone:
	case <-time.After(2 * time.Second):
		// 子进程还在往 tty 写（例如一个卡住的 cat）：不再等它，连接的寿命已经结束。
	}
}

// handleTerminalSessionByID 处理代理内部的关闭入口：
// DELETE /internal/v1/terminal/sessions/{id}?user_id=&reason=
//
// 这是代理内部接口，不是公开 API：Web 已经核对过“这个会话属于当前登录账号”，
// 代理再核对一次 user_id 与会话属主——两道都在，谁也不会因为对方“应该查过了”
// 而放宽。
func (s *Server) handleTerminalSessionByID(w http.ResponseWriter, r *http.Request) {
	if s.opts.Terminal == nil {
		httpx.WriteError(w, r, http.StatusNotImplemented, httpx.CodeNotImplemented,
			"代理未启用终端：没有 PTY 会话层")
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 DELETE")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/internal/v1/terminal/sessions/")
	if id == "" || strings.Contains(id, "/") {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "会话不存在")
		return
	}
	query := r.URL.Query()
	userID, err := strconv.ParseInt(query.Get("user_id"), 10, 64)
	if err != nil || userID <= 0 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, "user_id 必须是正整数")
		return
	}
	reason := query.Get("reason")
	if reason == "" {
		reason = terminal.ReasonUser
	}
	if !terminal.ValidReason(reason) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			fmt.Sprintf("reason %q 不在允许的结束原因里", reason))
		return
	}
	if err := s.opts.Terminal.Close(terminal.CloseInput{
		SessionID: id, Owner: userID, Reason: reason,
	}); err != nil {
		s.writeTerminalError(w, r, err, "关闭终端会话", "session_id", id)
		return
	}
	s.opts.Logger.Info("关闭终端会话",
		"session_id", id, "owner_user_id", userID, "reason", reason,
		"request_id", httpx.RequestID(r))
	httpx.WriteData(w, r, http.StatusOK, map[string]any{"session_id": id, "closed": true})
}

// writeTerminalError 把会话层的错误写成 HTTP 响应（仅用于尚未写响应头时）。
func (s *Server) writeTerminalError(w http.ResponseWriter, r *http.Request, err error, what string, kv ...any) {
	if mapped, ok := terminal.HTTPStatusOf(err); ok {
		attrs := append([]any{"error", err, "request_id", httpx.RequestID(r)}, kv...)
		s.opts.Logger.Warn(what, attrs...)
		httpx.WriteError(w, r, mapped.Status, mapped.Code, mapped.Message)
		return
	}
	if errors.Is(err, ErrStreamPermitNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "流许可不存在或已失效")
		return
	}
	attrs := append([]any{"error", err, "request_id", httpx.RequestID(r)}, kv...)
	s.opts.Logger.Error(what, attrs...)
	httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, what)
}
