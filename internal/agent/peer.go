package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/user"
	"strconv"
	"strings"

	"upanel/internal/httpx"
)

// PeerPolicy 描述“谁可以调用代理”的固定规则（PLAN.md 第 4 节）：
//
//   - 普通接口只允许 Web 服务账号（upanel-web）；
//   - root CLI 只能调用单独限定的调度/恢复路由，不能调用普通接口；
//   - 其他 UID 一律拒绝。
//
// 校验依据是 SO_PEERCRED 读到的**实际**对端 UID，不是请求里自称的身份。
type PeerPolicy struct {
	// WebUser 是普通接口唯一允许的对端账号名（只用于日志与错误信息）。
	WebUser string
	// WebUID 是该账号的数字 UID。
	WebUID uint32
	// SocketGroup 是 socket 的属组名（为空时与 WebUser 相同）。
	SocketGroup string
	// SocketGID 是该组的数字 GID。
	SocketGID uint32
}

func (p PeerPolicy) validate() error {
	if strings.TrimSpace(p.WebUser) == "" {
		return errors.New("agent: PeerPolicy.WebUser 不能为空")
	}
	if p.WebUID == 0 {
		// 普通接口允许 root 就等于绕开了整个边界，启动阶段直接拒绝。
		return fmt.Errorf("agent: PeerPolicy.WebUID 不能是 0（%s 必须是专用非 root 账号）", p.WebUser)
	}
	return nil
}

// ResolvePeerPolicy 按账号名解析出数字 UID/GID。只在启动路径调用；
// 账号不存在时明确失败，而不是退化成“允许任何人”。
func ResolvePeerPolicy(webUser, socketGroup string) (PeerPolicy, error) {
	webUser = strings.TrimSpace(webUser)
	if webUser == "" {
		return PeerPolicy{}, errors.New("agent.web_user 不能为空：普通接口只允许专用 Web 账号访问")
	}
	u, err := user.Lookup(webUser)
	if err != nil {
		return PeerPolicy{}, fmt.Errorf("对端账号 %q 不存在，请先按安装步骤创建专用非 root 账号: %w", webUser, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return PeerPolicy{}, fmt.Errorf("账号 %q 的 UID %q 不是数字: %w", webUser, u.Uid, err)
	}
	if uid == 0 {
		return PeerPolicy{}, fmt.Errorf("账号 %q 的 UID 为 0：普通接口不得允许 root 访问", webUser)
	}

	group := strings.TrimSpace(socketGroup)
	if group == "" {
		group = webUser
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return PeerPolicy{}, fmt.Errorf("socket 属组 %q 不存在，请先用 --group 指定或创建该组: %w", group, err)
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return PeerPolicy{}, fmt.Errorf("组 %q 的 GID %q 不是数字: %w", group, g.Gid, err)
	}
	if gid == 0 {
		return PeerPolicy{}, fmt.Errorf("socket 属组 %q 的 GID 为 0：socket 不得归 root 组", group)
	}

	return PeerPolicy{WebUser: webUser, WebUID: uint32(uid), SocketGroup: group, SocketGID: uint32(gid)}, nil
}

// peerInfo 是对端连接的内核身份。
type peerInfo struct {
	UID uint32
	GID uint32
	PID int32
}

type peerContextKey struct{}

// withPeerInfo 把对端身份放进请求上下文（由 http.Server.ConnContext 调用）。
func withPeerInfo(ctx context.Context, info peerInfo) context.Context {
	return context.WithValue(ctx, peerContextKey{}, info)
}

// peerFromContext 取出对端身份。取不到时返回 false，调用方必须据此拒绝请求。
func peerFromContext(ctx context.Context) (peerInfo, bool) {
	info, ok := ctx.Value(peerContextKey{}).(peerInfo)
	return info, ok
}

// rootOnlyPrefixes 是“只允许 root CLI 调用”的专用路由前缀。
//
// P02.3 还没有这类路由：内置定时任务的 trigger 入口属于 P03/P11，登记后加到
// 这个列表即可，普通接口仍然只允许 Web 账号。列表为空意味着当前 root 也会被
// 普通接口拒绝——这是刻意的，root 不能借道普通接口拿到特权能力。
var rootOnlyPrefixes = []string{}

func isRootOnlyPath(path string) bool {
	for _, prefix := range rootOnlyPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// peerAllowed 判断某个 UID 是否可以访问该路径。
func (s *Server) peerAllowed(uid uint32, path string) bool {
	if isRootOnlyPath(path) {
		return uid == 0
	}
	return uid == s.opts.Peer.WebUID
}

// gate 是所有代理接口的统一入口：先验证对端 UID，再进入路由。
//
// 拒绝发生在任何业务处理之前，且返回统一的错误信封；对端身份不明时按失败关闭处理。
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := peerFromContext(r.Context())
		if !ok {
			s.opts.Logger.Error("拒绝无法确认对端身份的请求", "path", r.URL.Path, "request_id", httpx.RequestID(r))
			httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "无法确认连接对端身份")
			return
		}
		if !s.peerAllowed(info.UID, r.URL.Path) {
			s.opts.Logger.Warn("拒绝对端 UID 不在允许范围",
				"uid", info.UID, "pid", info.PID, "path", r.URL.Path,
				"allowed_uid", s.opts.Peer.WebUID, "request_id", httpx.RequestID(r))
			httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "socket 对端 UID 无权调用该接口")
			return
		}
		next.ServeHTTP(w, r)
	})
}
