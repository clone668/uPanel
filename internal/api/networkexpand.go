package api

// network.confirm 的服务端展开（P14.2）。
//
// 这个动作的参数里有一个字段**不是**调用方给的：`arrived_on`。它是"浏览器这次是连到
// 哪个地址上来的"，只能从这条连接的 socket 本地地址取（net/http 的
// http.LocalAddrContextKey）。理由写在 CONTRACTS.md §13.6：确认靠的是"你现在从哪个
// 地址连过来"这个每次请求都重新发生的现场事实，而不是一个能被转发、能被日志带走的
// 确认码。前端给的地址不是凭据，是证词。
//
// 所以这层做两件事：
//  1. 严格解码调用方给的字段（只有 interface_id 与 token）。请求体里出现 arrived_on
//     会因为未知字段被拒——不是"忽略掉再覆盖"，而是明确告诉调用方这个字段不归它。
//  2. 把 socket 本地地址填进去。取不到就拒绝：宁可这一次确认失败（120 秒后自动回滚，
//     现场回到原样），也不能在没有凭据的情况下把新配置永久留下。

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/network"
)

// networkConfirmRequest 是调用方可以提供的全部字段。
//
// 刻意**没有** ArrivedOn：见上。多一个字段就等于多一条"我自己说我是从哪来的"的路。
type networkConfirmRequest struct {
	InterfaceID string `json:"interface_id"`
	Token       string `json:"token"`
}

// expandNetworkConfirm 把 arrived_on 填进 network.confirm 的参数。
func (s *Server) expandNetworkConfirm(c *gin.Context, params json.RawMessage) (json.RawMessage, bool) {
	var req networkConfirmRequest
	if len(params) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(params)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"network.confirm 的参数只有 interface_id 与 token 两个字段（到达地址由服务端从连接取）："+err.Error())
			return nil, false
		}
	}
	req.InterfaceID = strings.TrimSpace(req.InterfaceID)
	req.Token = strings.TrimSpace(req.Token)
	if req.InterfaceID == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "network.confirm 需要 interface_id")
		return nil, false
	}
	if !network.ValidRollbackToken(req.Token) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"network.confirm 需要一个回滚标识（network.apply 结果里的 rollback_token）")
		return nil, false
	}

	arrivedOn, err := requestLocalAddress(c)
	if err != nil {
		// 500 而不是 400：400 会说成"你发错了"，而这里错的是我们这一侧——
		// 拿不到连接本地地址就无法证明这次确认来自新地址。
		s.internalError(c, "无法确定确认请求的到达地址", err)
		return nil, false
	}

	out, err := json.Marshal(network.ConfirmParams{
		InterfaceID: req.InterfaceID,
		Token:       req.Token,
		ArrivedOn:   arrivedOn,
	})
	if err != nil {
		s.internalError(c, "生成 network.confirm 参数失败", err)
		return nil, false
	}
	return out, true
}

// requestLocalAddress 取本次请求到达的地址（不带端口、不带 IPv6 zone、IPv4 不再映成 v6）。
//
// 归一化的口径只有一条：**和网卡上那个地址的写法一样**。netplan 写的、`ip addr` 读回来
// 的、代理拿来做比较的都是 `192.168.1.50` 这种点分十进制，所以 `::ffff:192.168.1.50`
// 必须先解成 v4，否则"从新地址确认"会在双栈监听下永远失败——而失败的方向是自动回滚，
// 也就是管理员好不容易在新地址上连上了，却确认不了。
func requestLocalAddress(c *gin.Context) (string, error) {
	addr, ok := c.Request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return "", errors.New("api: 请求上下文里没有本地地址（不是经由 http.Server 进来的连接）")
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		// 极少数情况下 net.Addr 不是 host:port 形状；退回原样字符串，交给代理判等。
		return strings.TrimSpace(addr.String()), nil
	}
	// IPv6 zone（fe80::1%eth0）不进比较：候选地址只可能是 IPv4，而 zone 是链路本地的
	// 概念，带进字符串只会让两边的写法永远对不上。
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if parsed, perr := netip.ParseAddr(host); perr == nil {
		return parsed.Unmap().String(), nil
	}
	return host, nil
}
