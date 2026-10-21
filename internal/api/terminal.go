package api

// P06.3：受限 Web 终端 API（CONTRACTS.md 第 10 节）。
//
// 四条纪律：
//
//  1. **整组 admin**。终端能读服务器上的几乎一切：配置文件里的凭据、别人的数据、
//     应用日志。PLAN.md 第 15 节把它列为管理员能力，viewer 在这里得到 403，
//     而不是“只读终端”——一个只读终端并不存在。
//
//  2. **创建不在这里**。开会话是 system 写操作，走 terminal.create 的
//     预检 → 任务链路（CONTRACTS.md 第 5 节）；本文件的接口只管“看一眼自己的会话、
//     领一次性许可、连上去、关掉”。没有“直接开一个 shell”的快捷路径。
//
//  3. **归属由代理判定**。user_id 从服务端会话里取，从不从请求参数取；代理再按
//     user_id 核对会话归属。别人的 session_id 拿到了也是 404/403，不是 200 空列表。
//
//  4. **许可走子协议**。连接许可由带 CSRF 的 POST 签发，30 秒、一次性、只经
//     WebSocket 子协议传递；永不进 URL 查询参数（会落访问日志、浏览器历史、Referer）。
//     见 terminalgrant.go 与 terminalws.go。
//
// 会话内容一律不进审计：审计只记会话的元数据（谁、何时、哪个账号、多少字节），
// 不记终端全文（CONTRACTS.md 第 10 节）。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/terminal"
)

// TerminalReader 是 Web 的终端调用端（生产实现是 *terminal.Remote）。
//
// 用接口而不是直接依赖 *terminal.Remote：测试可以注入只回答“代理不可用”的实现，
// 用来证明 503 分支与“许可不发给不存在的会话”，而不必起一个真代理。
type TerminalReader interface {
	Sessions(ctx context.Context, userID int64, sessionID string) (terminal.SessionsResult, error)
	Session(ctx context.Context, userID int64, sessionID string) (terminal.SessionInfo, terminal.SessionsResult, error)
	Close(ctx context.Context, userID int64, sessionID, reason string) error
	Connect(ctx context.Context, userID int64, sessionID string) (*websocket.Conn, error)
}

// terminalUnavailable 是“没有配置代理”时的统一回答。
const terminalUnavailable = "终端不可用：未配置代理连接"

// terminalGrantResponse 是 POST /terminal/sessions/:id/connection-grants 的返回体。
//
// 三个协议字段（protocol、grant_protocol、connect_path）一起下发，是为了让前端
// 不必自己拼字符串：子协议名与路径里任何一处写错，握手都会以“浏览器报一个
// 没头没尾的 WebSocket 错误”结束，而错误原因在服务端日志里。
type terminalGrantResponse struct {
	SessionID string `json:"session_id"`
	// Grant 是一次性连接许可，30 秒内有效、只能用一次。
	Grant string `json:"grant"`
	// ExpiresAt 是许可到期时间（RFC3339 UTC）。
	ExpiresAt string `json:"expires_at"`
	// Protocol 是握手必须声明的子协议名。
	Protocol string `json:"protocol"`
	// GrantProtocol 是携带许可的子协议名（upanel.grant.<id>）。
	GrantProtocol string `json:"grant_protocol"`
	// ConnectPath 是调用时要拨的路径，**相对 API 基址**（与 /files 的下载地址同一套规则），
	// 不含查询参数：许可不放进 URL，前端只把它拼到基址后面。
	ConnectPath string `json:"connect_path"`
}

// handleTerminalSessions 处理 GET /api/v1/terminal/sessions。
//
// 返回当前账号的会话与终端边界（账号、uid、三个时限、两个数量上限）。
// 把边界一起下发，是为了让界面**显示**“以 upanel-shell（uid 998）运行、
// 15 分钟无输入关闭、最长 1 小时”，而不是把这些数字硬编码在页面上——
// 运维改了配置，页面跟着变。
func (s *Server) handleTerminalSessions(c *gin.Context) {
	reader, ok := s.terminalReader(c)
	if !ok {
		return
	}
	sess := sessionFrom(c)
	result, err := reader.Sessions(c.Request.Context(), sess.User.ID, "")
	if err != nil {
		s.writeTerminalError(c, err, "列出终端会话失败")
		return
	}
	if result.Sessions == nil {
		// 契约要求 sessions 永远是数组。null 会让页面渲染直接失败，而“按加载失败处理”
		// 又是另一种谎话——这里没有会话，就是空数组。
		result.Sessions = []terminal.SessionInfo{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, result)
}

// handleCreateTerminalGrant 处理 POST /api/v1/terminal/sessions/:id/connection-grants。
//
// 先问代理“这个会话是不是你的”（不存在或不是你的都是 404），再签发许可。
// 顺序不能反：先发许可再校验归属，等于给未授权的一方发了一张门票，
// 只是门票后面还有一道锁——多余的攻击面。
func (s *Server) handleCreateTerminalGrant(c *gin.Context) {
	reader, ok := s.terminalReader(c)
	if !ok {
		return
	}
	sess := sessionFrom(c)
	id, ok := terminalSessionID(c)
	if !ok {
		return
	}
	if _, _, err := reader.Session(c.Request.Context(), sess.User.ID, id); err != nil {
		s.writeTerminalError(c, err, "申请终端连接许可失败")
		return
	}
	grant, expiresAt, err := s.terminalGrants.issue(sess.User.ID, id)
	if err != nil {
		if errors.Is(err, errGrantStoreFull) {
			// 到顶只可能是客户端在刷：这不是“服务器忙”，但状态上确实是“此刻不给新的”。
			writeError(c, http.StatusConflict, httpx.CodeResourceBusy,
				"终端连接许可数量已达上限，请稍后重试")
			return
		}
		s.internalError(c, "签发终端连接许可失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, terminalGrantResponse{
		SessionID:     id,
		Grant:         grant,
		ExpiresAt:     expiresAt.UTC().Format(time.RFC3339),
		Protocol:      terminal.ProtocolName,
		GrantProtocol: terminal.GrantProtocolPrefix + grant,
		ConnectPath:   "/terminal/sessions/" + id + "/connect",
	})
}

// handleTerminalClose 处理 DELETE /api/v1/terminal/sessions/:id。
//
// 关闭是幂等的：会话已经不在了（自然退出、到期、被回收）也回同样的成功体，
// 因为调用方要的结果是“它现在不在跑了”。代理侧的 Close 对“找不到”返回成功，
// 所以这里不需要额外分支——但审计会如实记下 reason。
func (s *Server) handleTerminalClose(c *gin.Context) {
	reader, ok := s.terminalReader(c)
	if !ok {
		return
	}
	sess := sessionFrom(c)
	id, ok := terminalSessionID(c)
	if !ok {
		return
	}
	reason := strings.TrimSpace(c.Query("reason"))
	if reason == "" {
		reason = terminal.ReasonUser
	}
	if !terminal.ValidReason(reason) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "reason 不在允许的取值内")
		return
	}
	target := "terminal:" + id
	if err := reader.Close(c.Request.Context(), sess.User.ID, id, reason); err != nil {
		s.writeTerminalError(c, err, "关闭终端会话失败")
		return
	}
	if s.opts.Audit != nil {
		ev := s.auditEvent(c, sess, audit.ActionTerminalClose, target, audit.OutcomeSuccess,
			map[string]any{"session_id": id, "reason": reason})
		if err := s.opts.Audit.Record(c.Request.Context(), ev); err != nil {
			// 会话已经关掉了；审计失败只影响可追溯性，不能让前端以为关闭失败而去重试。
			s.opts.Logger.Error("终端关闭审计写入失败", "error", err,
				"session_id", id, "request_id", c.GetString(ctxKeyRequestID))
		}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, gin.H{"session_id": id, "closed": true})
}

// terminalReader 取终端调用端；未配置时直接写 503 并返回 false。
func (s *Server) terminalReader(c *gin.Context) (TerminalReader, bool) {
	if s.opts.Terminal == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, terminalUnavailable)
		return nil, false
	}
	// *terminal.Remote 满足 TerminalReader；显式转换让“接口满足关系”在编译期就被检查。
	var reader TerminalReader = s.opts.Terminal
	return reader, true
}

// terminalSessionID 解析并校验 :id。形态不对一律“会话不存在”，不泄漏内部细节。
func terminalSessionID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if !terminal.IsValidSessionID(id) {
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "会话不存在")
		return "", false
	}
	return id, true
}

// writeTerminalError 把终端错误翻成 HTTP 响应。
func (s *Server) writeTerminalError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var remote *terminal.RemoteError
	if errors.As(err, &remote) {
		// 代理的明确拒绝：状态码与码原样转出。别人的会话不能因为经过 Web 就变成 500。
		writeError(c, remote.Status, remote.Code, remote.Message)
		return
	}
	// 本地校验与代理判决走**同一张**映射表（terminal.HTTPStatusOf）：
	// 同一个“会话已被占用”在 Web 层被拦下和在代理层被拦下必须是同一个状态码。
	if mapped, found := terminal.HTTPStatusOf(err); found {
		writeError(c, mapped.Status, mapped.Code, mapped.Message)
		return
	}
	switch {
	case errors.Is(err, terminal.ErrAgentUnavailable):
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法完成终端操作")
	default:
		s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
	}
}

// recordTerminalAttach 记录一次连接尝试的结果。
//
// 成功的连接与在握手中途被拒绝的连接都记：后者是“有人试图连一个不属于他的会话”
// 的唯一痕迹。阶段名（origin / protocol / grant / agent）让事后能分辨是配置问题
// 还是越权尝试。
func (s *Server) recordTerminalAttach(c *gin.Context, sess auth.Session, sessionID, stage,
	outcome string, details map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	details["session_id"] = sessionID
	details["stage"] = stage
	ev := s.auditEvent(c, sess, audit.ActionTerminalAttach, "terminal:"+sessionID, outcome, details)
	if err := s.opts.Audit.Record(c.Request.Context(), ev); err != nil {
		s.opts.Logger.Error("终端连接审计写入失败", "error", err,
			"session_id", sessionID, "stage", stage, "request_id", c.GetString(ctxKeyRequestID))
	}
}
