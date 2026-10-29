package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"time"

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
// 普通接口仍然只允许 Web 账号：root 不能借道普通接口拿到特权能力。
// 当前没有整段前缀属于 root——见下面的形状表。
var rootOnlyPrefixes = []string{}

// rootOnlyPathPatterns 是“只允许 root CLI 调用”的专用路由**形状**。
//
// 为什么不能只靠前缀（P12.4）：`/internal/v1/schedules/` 这一段同时挂着两条读路由
// （GET 列表与 GET 详情，只给 Web 账号）和一条触发路由（POST /:id/trigger，只给
// root）。按前缀判定会让 root 读不到计划，或者让 Web 触发计划——两种都是把边界
// 装反了。按形状判定，被放行的永远只有那个确切的后缀。
var rootOnlyPathPatterns = []*regexp.Regexp{
	// 定时触发（CONTRACTS §15）：POST /internal/v1/schedules/<id>/trigger
	regexp.MustCompile(`^/internal/v1/schedules/sch_[0-9a-f]{32}/trigger$`),
	// 定时触发的结果读（P12.4）：GET /internal/v1/schedules/<id>/trigger-runs/<op>。
	//
	// `--wait` 要在 root 身份下知道"这一次跑到哪了"，而 /internal/v1/operations/<id>
	// 是 Web 账号的路由——root 调它是 403。把读收窄成"这条计划自己触发的那个操作"
	// 比把整条操作读路由开给 root 小得多：路径里带着计划 id，处理端还会核对操作记录
	// 的操作者与会话哈希是否都由这条计划派生（见 handleScheduleTriggerRun）。
	regexp.MustCompile(`^/internal/v1/schedules/sch_[0-9a-f]{32}/trigger-runs/op_[0-9a-f]{32}$`),
}

func isRootOnlyPath(path string) bool {
	for _, prefix := range rootOnlyPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	for _, pattern := range rootOnlyPathPatterns {
		if pattern.MatchString(path) {
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
		started := time.Now()
		tw := &timingWriter{ResponseWriter: w}
		next.ServeHTTP(tw, r)
		elapsed := time.Since(started)

		// 耗时记录只在**慢**的时候发声。
		//
		// 这不是可选的方便：代理与 Web 之间的调用上限是 10 秒（agentclient.DefaultTimeout），
		// 而“某次预检用了 8 秒”这件事一度只能从 Web 侧的 curl 计时侧推，代理日志里
		// 一个字都没有——查一次就得靠猜。（Q290）一条慢请求日志把代理侧的真实成本
		// 摆在同一行上：路径、状态码、耗时。正常请求（< slowRequestThreshold）不记，
		// 免得把日志淹没在健康检查里。
		if elapsed >= slowRequestThreshold {
			s.opts.Logger.Warn("代理请求耗时偏长",
				"method", r.Method, "path", r.URL.Path,
				"status", tw.statusOrOK(), "duration_ms", elapsed.Milliseconds(),
				"request_id", httpx.RequestID(r))
		}
	})
}

// slowRequestThreshold 是“值得记一笔”的代理请求耗时下限。
//
// 取 500 毫秒：本机 socket 上的调用正常都在个位数毫秒，超过半秒说明这一条路上
// 多了点什么（外部命令、锁等待、大文件），值得留痕迹。
const slowRequestThreshold = 500 * time.Millisecond

// timingWriter 只记状态码，不改变任何写行为。
type timingWriter struct {
	http.ResponseWriter
	status int
}

func (t *timingWriter) WriteHeader(code int) {
	if t.status == 0 {
		t.status = code
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *timingWriter) Write(b []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	return t.ResponseWriter.Write(b)
}

// statusOrOK 返回状态码；处理器一个字都没写时按 200 记。
func (t *timingWriter) statusOrOK() int {
	if t.status == 0 {
		return http.StatusOK
	}
	return t.status
}

// Unwrap 让 http.ResponseController（P05.3 的日志流要用它推截止时间）
// 能穿过这层包装拿到原始 ResponseWriter。
func (t *timingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
