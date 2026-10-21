package agentclient

// P06.3：受限终端的两条专用调用。
//
// 为什么不是给 Client 加一个通用的 Do：代理内部的每个接口都应该只被一个**有名字**
// 的方法调用。一个通用的“发任意请求到任意内部路径”的方法，等于给 Web 进程开了一张
// 通往代理全部接口的空白通行证——它今天用不上，明天就会有人拿它绕开某个专门方法里的
// 参数校验。
//
// 这里只有两件事：
//   - 关闭会话（代理内部的 DELETE，Web 已核对过归属）；
//   - 用一次性许可拨出一条 WebSocket（帧协议见 internal/terminal/frame.go）。
//
// 创建会话**不**在这里：它是 terminal.create 动作，走预检 → 任务链路，
// 因此没有“Web 直接让代理开一个 shell”的捷径。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gorilla/websocket"
)

// maxTerminalFrameBytes 是终端单帧的大小上限（与 internal/terminal 的
// MaxFrameBytes 一致：64 KiB 数据 + base64 与信封余量）。客户端侧也要设，
// 否则一条来自代理的异常大帧会把 Web 进程的内存吃掉。
const maxTerminalFrameBytes = 64<<10*4/3 + 8<<10

// CloseTerminalSession 关闭一个终端会话。
//
// 路径里的 session_id 与查询参数都在这里先做形态校验：它们会进入 URL，代理的
// 路由匹配也按路径前缀做，一个带 "/" 的 id 能指到别的接口上去。
func (c *Client) CloseTerminalSession(ctx context.Context, sessionID string, userID int64, reason string) error {
	if !isHexID(sessionID) {
		return fmt.Errorf("agentclient: session_id 形态不合法")
	}
	if userID <= 0 {
		return fmt.Errorf("agentclient: user_id 必须为正整数")
	}
	if reason == "" {
		reason = "user"
	}
	query := url.Values{}
	query.Set("user_id", strconv.FormatInt(userID, 10))
	query.Set("reason", reason)
	path := "/internal/v1/terminal/sessions/" + sessionID + "?" + query.Encode()
	_, err := c.do(ctx, http.MethodDelete, path, nil)
	return err
}

// DialTerminalStream 用一次性许可拨出一条终端 WebSocket。
//
// 许可 id 是单次凭据：这里拨号失败就作废，调用方必须重新申请，不能拿同一个 id 重试。
// 子协议由调用方给（协议名 + 连接许可），浏览器那一侧看到的是同一组名字。
func (c *Client) DialTerminalStream(ctx context.Context, streamID string, subprotocols []string) (*websocket.Conn, error) {
	if !isHexID(streamID) {
		return nil, fmt.Errorf("agentclient: 流 id 形态不合法")
	}
	dialer := websocket.Dialer{
		// 不设整体超时：终端的寿命由代理的会话上限决定，客户端不该替它设闹钟。
		HandshakeTimeout: DefaultTimeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		Subprotocols:     subprotocols,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return c.dial(ctx, "unix", c.socketPath)
		},
	}
	target := "ws://agent/internal/v1/streams/" + streamID
	conn, resp, err := dialer.DialContext(ctx, target, nil)
	if err != nil {
		// 代理在升级之前拒绝了这次拨号（许可不存在、会话已被占用……）。
		// 把它的错误信封读出来，否则调用方只能看到 "bad handshake"。
		if resp != nil && resp.Body != nil {
			defer func() { _ = resp.Body.Close() }()
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
			if readErr == nil {
				if mapped, ok := mapHandshakeFailure(resp.StatusCode, raw); ok {
					return nil, mapped
				}
			}
			return nil, &UnavailableError{Err: fmt.Errorf("终端握手失败: %d", resp.StatusCode)}
		}
		return nil, &UnavailableError{Err: fmt.Errorf("终端握手失败: %w", err)}
	}
	conn.SetReadLimit(maxTerminalFrameBytes)
	return conn, nil
}

// mapHandshakeFailure 把代理的拒绝信封翻成 *Error。
func mapHandshakeFailure(status int, raw []byte) (error, bool) {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
		return nil, false
	}
	return &Error{Status: status, Code: env.Error.Code, Message: env.Error.Message}, true
}

// isHexID 判断 id 是否是 32 位小写十六进制（流许可与会话 id 都是这种形态）。
func isHexID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
