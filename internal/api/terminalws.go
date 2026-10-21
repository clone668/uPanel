package api

// Web 终端的 WebSocket 入口（P06.3，CONTRACTS.md 第 10 节）。
//
// 这条连接是**两跳**的中间那一段：
//
//	浏览器 ──WS（子协议 upanel.term.v1 + 一次性许可）──► Web ──WS（同协议）──► 代理 ──PTY(upanel-shell)
//
// Web 在这一跳上不做业务判断：它不认识 input 帧的内容，也不解析 output。
// 它只负责三件事——握手处的来源与许可校验、两个方向的字节转发、以及
// 两个方向的存活探测（心跳）。帧的内容校验、速率限制、窗口尺寸范围全都在
// 代理侧（那才是能看住 PTY 的一侧）；Web 只额外限一个帧上限，避免把
// 一个超大帧读进内存。
//
// 为什么先连代理再升级浏览器：反过来的话，代理拒绝（会话已被占用、许可失效、
// 代理不可用）就只能以“浏览器看到的莫名其妙的连接关闭”呈现。先连代理，
// 失败时还能回一个带错误码的 JSON，前端就能显示“会话已被占用”而不是“网络错误”。

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/terminal"
)

const (
	// terminalPingInterval 是心跳间隔。浏览器对控制帧自动回 pong，
	// 因此这两跳都不需要前端写心跳代码。
	terminalPingInterval = 20 * time.Second
	// terminalPongWait 是“多久收不到 pong 就判定对端已死”。
	// 20 秒一次心跳、60 秒没有回应即断开：允许两次丢包，不允许无限挂着。
	terminalPongWait = 60 * time.Second
	// terminalWriteWait 是单次写入（含数据帧与控制帧）的时限。
	terminalWriteWait = 10 * time.Second
	// terminalHandshakeTimeout 是升级握手的时限。
	terminalHandshakeTimeout = 10 * time.Second
)

// terminalUpgrader 是终端专用的升级器。
//
// 与代理侧同一组参数（缓冲区 4 KiB、子协议只有 upanel.term.v1），区别在
// CheckOrigin：代理侧的对端是 upanel-web（走 Unix socket，没有浏览器语义）；
// 这里的对端是浏览器，来源已经在 handleTerminalConnect 里按配置白名单核对过，
// 因此不再用 Upgrader 的默认策略（默认只比 Host，而允许源是配置里的公开源）。
var terminalUpgrader = websocket.Upgrader{
	HandshakeTimeout: terminalHandshakeTimeout,
	ReadBufferSize:   4096,
	WriteBufferSize:  4096,
	// 只回 upanel.term.v1，不回显 upanel.grant.<id>：许可是一次性凭据，
	// 不该出现在响应头里（它会被浏览器开发者工具与某些代理记录下来）。
	Subprotocols: []string{terminal.ProtocolName},
	CheckOrigin:  func(*http.Request) bool { return true },
}

// handleTerminalConnect 处理 GET /api/v1/terminal/sessions/:id/connect。
//
// 握手阶段依次四道检查，任何一道不过都不升级：
// 会话 id 形态 → Origin 白名单 → 子协议 → 一次性许可。
// 之后连代理，成功才升级浏览器并开始转发。
func (s *Server) handleTerminalConnect(c *gin.Context) {
	reader, ok := s.terminalReader(c)
	if !ok {
		return
	}
	sess := sessionFrom(c)
	sessionID, ok := terminalSessionID(c)
	if !ok {
		return
	}

	// 1. Origin：浏览器发起的 WebSocket 一定带 Origin，缺失或不在白名单都拒绝。
	//    Cookie 上的 SameSite=Strict 挡不住同一站点内的跨窗口调用，这一道必须自己看着。
	origin := c.GetHeader("Origin")
	if origin == "" || !s.allowedOrigins.Contains(origin) {
		s.recordTerminalAttach(c, sess, sessionID, "origin", audit.OutcomeFailure,
			map[string]any{"origin": origin})
		writeError(c, http.StatusForbidden, httpx.CodeCSRFFailed,
			"WebSocket 握手必须携带允许列表内的 Origin")
		return
	}

	// 2. 子协议：必须声明终端协议名。没有它说明对端不是这个前端，
	//    后面收到什么都不该按终端帧去理解。
	protocols := websocket.Subprotocols(c.Request)
	if !hasSubprotocol(protocols, terminal.ProtocolName) {
		s.recordTerminalAttach(c, sess, sessionID, "protocol", audit.OutcomeFailure,
			map[string]any{"origin": origin, "subprotocols": protocols})
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"WebSocket 握手必须声明子协议 "+terminal.ProtocolName)
		return
	}

	// 3. 一次性许可：由带 CSRF 的 POST 签发，绑定用户与会话，30 秒有效。
	//    许可放 URL 查询参数是被契约明令禁止的（会进访问日志与浏览器历史）。
	grantID := grantFromProtocols(protocols)
	if !s.terminalGrants.consume(grantID, sess.User.ID, sessionID) {
		s.recordTerminalAttach(c, sess, sessionID, "grant", audit.OutcomeFailure,
			map[string]any{"origin": origin})
		writeError(c, http.StatusForbidden, httpx.CodeForbidden,
			"连接许可无效、已过期或不属于该会话")
		return
	}

	// 4. 连代理（在升级浏览器之前）。
	agentConn, err := reader.Connect(c.Request.Context(), sess.User.ID, sessionID)
	if err != nil {
		s.recordTerminalAttach(c, sess, sessionID, "agent", audit.OutcomeFailure,
			map[string]any{"origin": origin, "error": err.Error()})
		s.writeTerminalError(c, err, "连接终端失败")
		return
	}

	browserConn, err := terminalUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 失败时它自己已经写过响应（4xx/5xx）。这里只能收掉代理那一侧：
		// 代理会话会因此进入断流宽限期，60 秒内还能重连，不会留下一个假占用的会话。
		_ = agentConn.Close()
		s.opts.Logger.Warn("终端 WebSocket 升级失败",
			"error", err, "session_id", sessionID, "request_id", c.GetString(ctxKeyRequestID))
		return
	}

	s.recordTerminalAttach(c, sess, sessionID, "connected", audit.OutcomeSuccess,
		map[string]any{"origin": origin, "protocol": browserConn.Subprotocol()})
	s.relayTerminal(c, sess, browserConn, agentConn, sessionID, origin)
}

// relayEnd 描述转发结束的原因，用于日志与审计（不含任何终端内容）。
type relayEnd struct {
	// side 是先结束的那一侧：browser 或 agent。
	side string
	// reason 是短原因（client_closed、agent_closed、timeout、protocol、too_large）。
	reason string
}

// relayTerminal 双向转发并维护两个方向的心跳，直到任意一侧断开。
//
// 重要：整个过程只有一个 goroutine 写一条连接（另一个方向是另一个 goroutine），
// 心跳用 WriteControl（gorilla 允许它与写并发），因此不需要为写操作加锁，
// 也就不会出现“锁住一条卡死的连接把另一端也拖死”的情况。
func (s *Server) relayTerminal(c *gin.Context, sess auth.Session, browser, agent *websocket.Conn,
	sessionID, origin string) {
	startedAt := time.Now()
	defer func() { _ = browser.Close() }()
	defer func() { _ = agent.Close() }()

	// 只接受文本帧：终端帧是 JSON，二进制帧不在协议内。
	// 帧上限与代理侧一致，避免一个超大帧被读进内存（写入端还会再判一次）。
	browser.SetReadLimit(terminal.MaxFrameBytes)

	keepalive := func(conn *websocket.Conn) {
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(terminalPongWait))
		})
		_ = conn.SetReadDeadline(time.Now().Add(terminalPongWait))
	}
	keepalive(browser)
	keepalive(agent)

	var browserToAgent, agentToBrowser atomic.Int64
	ended := make(chan relayEnd, 2)

	// 浏览器 → 代理：input 与 resize 原样转发。
	// 这里不解析也不改写：代理是唯一校验帧的一侧，Web 多解析一次只会制造
	// “两处校验不一致”的新问题。速率限制同样在代理侧。
	go func() {
		reason := "client_closed"
		for {
			typ, data, err := browser.ReadMessage()
			if err != nil {
				ended <- relayEnd{side: "browser", reason: reason}
				return
			}
			_ = browser.SetReadDeadline(time.Now().Add(terminalPongWait))
			if typ != websocket.TextMessage {
				reason = "protocol"
				break
			}
			if len(data) > terminal.MaxFrameBytes {
				reason = "too_large"
				break
			}
			_ = agent.SetWriteDeadline(time.Now().Add(terminalWriteWait))
			if err := agent.WriteMessage(websocket.TextMessage, data); err != nil {
				reason = "agent_closed"
				break
			}
			browserToAgent.Add(int64(len(data)))
		}
		ended <- relayEnd{side: "browser", reason: reason}
	}()

	// 代理 → 浏览器：ready、output、exit、error 原样转发。
	// 代理断开时它自己带了一个关闭码（协议违规 4001、超速 4002、正常结束 1000），
	// 这里只把这个码转给浏览器，不改写、不解释——Web 仍然不解析帧内容。
	var agentClose atomic.Int32
	go func() {
		reason := "agent_closed"
		for {
			typ, data, err := agent.ReadMessage()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					agentClose.Store(int32(ce.Code))
				}
				ended <- relayEnd{side: "agent", reason: reason}
				return
			}
			_ = agent.SetReadDeadline(time.Now().Add(terminalPongWait))
			if typ != websocket.TextMessage {
				reason = "protocol"
				break
			}
			_ = browser.SetWriteDeadline(time.Now().Add(terminalWriteWait))
			if err := browser.WriteMessage(websocket.TextMessage, data); err != nil {
				reason = "client_closed"
				break
			}
			agentToBrowser.Add(int64(len(data)))
		}
		ended <- relayEnd{side: "agent", reason: reason}
	}()

	ticker := time.NewTicker(terminalPingInterval)
	defer ticker.Stop()

	var end relayEnd
loop:
	for {
		select {
		case end = <-ended:
			break loop
		case <-ticker.C:
			if err := browser.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(terminalWriteWait)); err != nil {
				end = relayEnd{side: "browser", reason: "timeout"}
				break loop
			}
			if err := agent.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(terminalWriteWait)); err != nil {
				end = relayEnd{side: "agent", reason: "timeout"}
				break loop
			}
		}
	}

	duration := time.Since(startedAt)
	if end.reason == "timeout" || end.reason == "protocol" || end.reason == "too_large" {
		s.opts.Logger.Warn("终端连接异常结束",
			"session_id", sessionID, "side", end.side, "reason", end.reason,
			"duration_ms", duration.Milliseconds(),
			"request_id", c.GetString(ctxKeyRequestID))
	} else {
		s.opts.Logger.Info("终端连接结束",
			"session_id", sessionID, "side", end.side, "reason", end.reason,
			"duration_ms", duration.Milliseconds(),
			"request_id", c.GetString(ctxKeyRequestID))
	}

	// 审计只记元数据：谁、多久、哪个方向多少字节、为什么结束。
	// 终端全文与被输入的命令不在其中（CONTRACTS.md 第 10 节）。
	s.recordTerminalAttach(c, sess, sessionID, "closed", audit.OutcomeSuccess, map[string]any{
		"origin":       origin,
		"end_side":     end.side,
		"end_reason":   end.reason,
		"duration_ms":  duration.Milliseconds(),
		"input_bytes":  browserToAgent.Load(),
		"output_bytes": agentToBrowser.Load(),
	})

	// 给浏览器一帧 close，把“为什么断”说清楚。没有它，浏览器只能报 1006，
	// 于是一次协议违规、一次超速、一次到期和一次掉线在页面上完全一样。
	// 只有 1000—4999 里可发送的码才转：1005/1006/1015 是本地诊断码，发出去
	// 本身就是协议错误。
	browserCode := int(agentClose.Load())
	if !sendableCloseCode(browserCode) {
		browserCode = websocket.CloseNormalClosure
	}
	_ = browser.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(browserCode, ""),
		time.Now().Add(terminalWriteWait))
}

// sendableCloseCode 判断一个关闭码能否由服务端主动发出。
//
// 1005（没有状态码）、1006（异常关闭）、1015（TLS 握手失败）是**本地诊断码**：
// 它们只能由协议栈在本地产生，写进 close 帧本身就是协议错误，gorilla 会直接拒绝。
func sendableCloseCode(code int) bool {
	switch {
	case code == websocket.CloseNoStatusReceived,
		code == websocket.CloseAbnormalClosure,
		code == websocket.CloseTLSHandshake:
		return false
	case code >= 1000 && code <= 4999:
		return true
	default:
		return false
	}
}

// hasSubprotocol 判断子协议列表里是否声明了 name（大小写不敏感，与 gorilla 一致）。
func hasSubprotocol(protocols []string, name string) bool {
	for _, p := range protocols {
		if strings.EqualFold(strings.TrimSpace(p), name) {
			return true
		}
	}
	return false
}

// grantFromProtocols 从子协议里取出唯一一段许可 id。
//
// 多个许可令牌是畸形请求（浏览器只会带一个），这时返回空串让它按“无效许可”失败；
// 形态不对的令牌也一样。许可不放进 URL，因此除了这里没有第二个入口。
func grantFromProtocols(protocols []string) string {
	var found string
	for _, p := range protocols {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, terminal.GrantProtocolPrefix) {
			continue
		}
		if found != "" {
			return ""
		}
		found = strings.TrimPrefix(p, terminal.GrantProtocolPrefix)
	}
	if !validGrantID(found) {
		return ""
	}
	return found
}

// 编译期断言：*terminal.Remote 必须满足 TerminalReader。
// 放在这里而不是 cmd，是为了让“接口满足关系”在被修改的第一个位置就报错。
var _ TerminalReader = (*terminal.Remote)(nil)
