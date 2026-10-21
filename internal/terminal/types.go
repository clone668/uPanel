// Package terminal 是受限 Web 终端（P06.3）的协议与会话层。
//
// 拆成两半，边界写在文件名上：
//
//	types.go / frame.go   —— 双方共用的 DTO 与帧编码（Web 与代理都 import）；
//	manager_*.go          —— 代理侧的 PTY 会话层（创建、限额、超时、回收）；
//	remote.go             —— Web 侧的调用端（查询、连接许可、关闭、拨号）。
//
// 为什么放在一个包里：内部/files 已经是这个形态（manager_linux.go 是代理侧，
// fetch.go 是 Web 侧），两侧共用同一份 DTO 才不会出现“代理返回的字段名与 Web
// 期望的字段名各写一遍、改一处漏一处”。两个可执行文件本来就各自链接所需的代码。
//
// 三条不可越过的边界（PLAN.md 第 4、14 节）：
//  1. 终端的字节通道只连到一个**已经降权**的 PTY：代理先 fork/exec，再在子进程里
//     降为 upanel-shell 的 UID/GID 并清空附加组与环境。代理绝不在 root 身份下解释
//     用户输入，也不提供任何“执行命令”的接口——输入只是写进 tty 的字节。
//  2. 会话有数量与时长上限：每个管理员 1 个、全局 2 个、15 分钟无输入关闭、
//     最长 1 小时、断流宽限 60 秒。这些值只允许收紧不允许放宽（见 Limits）。
//  3. 帧是结构化的：客户端只能发 input 与 resize，服务端只发 output、ready、exit、error。
//     没有“第一帧决定后面的协议”这种设计，子协议名在握手时就写死。
package terminal

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 登记名与协议名。这些字符串同时出现在三处（代理登记表、Web 客户端、openapi），
// 因此只能定义一次。
const (
	// ActionCreate 是创建终端会话的动作名（CONTRACTS.md 第 5 节）。
	ActionCreate = "terminal.create"
	// QuerySessions 是终端会话列表的查询名。
	QuerySessions = "terminal.sessions"
	// PurposeIO 是终端字节通道的流用途名（沿用 P05.3 的一次性许可通道）。
	PurposeIO = "terminal.io"
	// ProtocolName 是 WebSocket 子协议名。浏览器↔Web 与 Web↔代理都用它，
	// 于是“这条连接说的是终端帧协议”在握手阶段就确定，不靠第一帧自报。
	ProtocolName = "upanel.term.v1"
	// GrantProtocolPrefix 是连接许可的子协议前缀。
	//
	// 许可必须走子协议而不是 URL 查询参数（CONTRACTS.md 第 10 节）：查询参数会出现在
	// 访问日志、浏览器历史、Referer 与反代的 access log 里，而子协议头不会进任何
	// 标准的访问日志。浏览器无法自定义 WebSocket 请求头，子协议是唯一能带自定义值的
	// 位置。
	GrantProtocolPrefix = "upanel.grant."
)

// 会话状态闭集。只有两种：连着，或者还在断线宽限里。
// “已结束”不是状态而是不存在——会话结束后就从表里消失，页面靠 exit 帧与列表 404
// 知道它没了，而不是读一个永远停在 closed 的死行。
const (
	StateAttached = "attached"
	StateDetached = "detached"
)

// 尺寸与帧限额（CONTRACTS.md 第 10 节：cols/rows 限 1—500、单帧最大 64 KiB）。
const (
	MinDimension = 1
	MaxDimension = 500
	// DefaultCols / DefaultRows 是前端首次创建时的默认窗口。
	DefaultCols = 80
	DefaultRows = 24
	// MaxFramePayload 是单帧终端数据的字节上限（64 KiB）。
	MaxFramePayload = 64 << 10
	// MaxFrameBytes 是单帧 JSON 报文的上限：64 KiB 的 base64 再加信封余量。
	MaxFrameBytes = MaxFramePayload*4/3 + 8<<10
)

// 输入速率上限。终端是交互输入，不是文件上传：正常打字连 1 KiB/s 都到不了，
// 粘贴一大段文本才会接近上限。限额挡住的是“把终端当成文件传输通道”。
const (
	MaxInputBytesPerSecond  = 256 << 10
	MaxInputFramesPerSecond = 200
)

// 契约固定的上限（PLAN.md 第 14 节）。配置只能把它们调**小**，不能调大。
const (
	// ContractIdleTimeout 是“默认 15 分钟无输入关闭”。
	ContractIdleTimeout = 15 * time.Minute
	// ContractMaxDuration 是“最长会话 1 小时”。
	ContractMaxDuration = time.Hour
	// ContractDisconnectGrace 是“网络断开宽限 60 秒后终止”。
	ContractDisconnectGrace = 60 * time.Second
	// ContractMaxPerUser 是“每个管理员最多 1 个会话”。
	ContractMaxPerUser = 1
	// ContractMaxSessions 是“全局最多 2 个”。
	ContractMaxSessions = 2
)

// Limits 是运行时实际生效的会话限额。
//
// 配置只能收紧（见 config.Terminal.Validate）：把 15 分钟改成 5 分钟是运维的自由，
// 改成 2 小时会把“无输入自动关闭”变成一句空话，所以那种值在启动阶段就被拒绝。
type Limits struct {
	// IdleTimeout 是无输入自动关闭的时限。
	IdleTimeout time.Duration
	// MaxDuration 是会话的绝对寿命（从创建算起，与是否有输入无关）。
	MaxDuration time.Duration
	// DisconnectGrace 是断流后保留会话的时间：这段时间内重连能接上同一个 PTY，
	// 超过就终止。0.1 不做 tmux 式持久终端，所以宽限只是“网络抖一下不用重开”。
	DisconnectGrace time.Duration
	// MaxPerUser 是单个用户的会话上限。
	MaxPerUser int
	// MaxSessions 是全局会话上限。
	MaxSessions int
}

// DefaultLimits 返回契约值。
func DefaultLimits() Limits {
	return Limits{
		IdleTimeout:     ContractIdleTimeout,
		MaxDuration:     ContractMaxDuration,
		DisconnectGrace: ContractDisconnectGrace,
		MaxPerUser:      ContractMaxPerUser,
		MaxSessions:     ContractMaxSessions,
	}
}

// Validate 校验限额自洽且不放宽契约。
func (l Limits) Validate() error {
	if l.IdleTimeout <= 0 || l.IdleTimeout > ContractIdleTimeout {
		return fmt.Errorf("终端空闲时限必须在 1 秒—15 分钟之间，当前为 %s", l.IdleTimeout)
	}
	if l.MaxDuration <= 0 || l.MaxDuration > ContractMaxDuration {
		return fmt.Errorf("终端最长会话必须在 1 秒—1 小时之间，当前为 %s", l.MaxDuration)
	}
	if l.DisconnectGrace <= 0 || l.DisconnectGrace > ContractDisconnectGrace {
		return fmt.Errorf("终端断流宽限必须在 1 秒—60 秒之间，当前为 %s", l.DisconnectGrace)
	}
	if l.MaxPerUser <= 0 || l.MaxPerUser > ContractMaxPerUser {
		return fmt.Errorf("终端单用户会话上限必须在 1—%d 之间，当前为 %d", ContractMaxPerUser, l.MaxPerUser)
	}
	if l.MaxSessions <= 0 || l.MaxSessions > ContractMaxSessions {
		return fmt.Errorf("终端全局会话上限必须在 1—%d 之间，当前为 %d", ContractMaxSessions, l.MaxSessions)
	}
	if l.MaxPerUser > l.MaxSessions {
		return fmt.Errorf("终端单用户会话上限 %d 大于全局上限 %d", l.MaxPerUser, l.MaxSessions)
	}
	return nil
}

// Params 是 terminal.create 的动作参数（CONTRACTS.md 第 5 节：cols、rows）。
//
// 两个字段都必须显式给出：给“缺省 80x24”会让一次漏传参数的请求看起来正常，
// 而前端本来就知道它要开多大的窗口。
type Params struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Validate 校验窗口尺寸。
func (p Params) Validate() error {
	if p.Cols < MinDimension || p.Cols > MaxDimension {
		return fmt.Errorf("%w: cols 必须在 %d—%d 之间，当前为 %d",
			ErrInvalidParams, MinDimension, MaxDimension, p.Cols)
	}
	if p.Rows < MinDimension || p.Rows > MaxDimension {
		return fmt.Errorf("%w: rows 必须在 %d—%d 之间，当前为 %d",
			ErrInvalidParams, MinDimension, MaxDimension, p.Rows)
	}
	return nil
}

// CreateInput 是创建会话的输入。
type CreateInput struct {
	// Owner 是会话所属的面板账号 id（来自预检的 actor_context）。
	Owner int64
	// OwnerName 只用于日志与审计的可读归属。代理**不**查面板用户表：
	// 身份由 Web 认证后告知（CONTRACTS.md 第 15 节），代理独立实施的是系统边界。
	OwnerName string
	// Params 是窗口尺寸。
	Params Params
}

// SessionInfo 是会话的对外视图。默认不含终端内容——契约要求记录会话创建/结束与账号，
// 不记录终端全文。
type SessionInfo struct {
	SessionID string `json:"session_id"`
	UserID    int64  `json:"user_id"`
	// Account 是实际运行 shell 的系统账号（正常是 upanel-shell）。
	Account string `json:"account"`
	// AccountUID 是该账号的 uid。把它如实下发，界面才能证明“这不是 root”。
	AccountUID uint32 `json:"account_uid"`
	// State 是 attached 或 detached。
	State string `json:"state"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	// CreatedAt / LastActivityAt 是 RFC3339 UTC。
	CreatedAt      time.Time `json:"created_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	// AttachedAt 为上次连接建立时间；从未连接过时为空。
	AttachedAt *time.Time `json:"attached_at"`
	// IdleDeadline / ExpiresAt 是两条独立的终止线：无输入与绝对寿命。
	IdleDeadline time.Time `json:"idle_deadline"`
	ExpiresAt    time.Time `json:"expires_at"`
	// IdleTimeoutSeconds / MaxDurationSeconds 让界面显示“这个会话还有多久”，
	// 而不是把服务端的时间上限硬编码在页面上。
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	MaxDurationSeconds int `json:"max_duration_seconds"`
}

// ListFilter 是一次会话查询的过滤条件。
//
// UserID 是**必填**的：代理不提供“列出所有人的终端”的查询。Web 从自己的会话里取
// 当前账号 id，所以这里没有“省略等于全部”的分支。
type ListFilter struct {
	UserID    int64
	SessionID string
}

// SessionsParams 是 terminal.sessions 查询的参数 DTO。
//
// 为什么 user_id 由 Web 传而不是代理自己推断：代理只认 socket 对端是 upanel-web，
// 它不知道“现在登录的是谁”——那是 Web 的职责（CONTRACTS.md 第 15 节：代理信任 Web
// 完成用户认证，但独立实施系统边界）。代理由此做的判决是“这个会话属不属于这个
// user_id”，所以别人的会话 id 拿到了也连不上。
type SessionsParams struct {
	UserID    int64  `json:"user_id"`
	SessionID string `json:"session_id"`
}

// Filter 把查询参数转成过滤条件。
func (p SessionsParams) Filter() ListFilter {
	return ListFilter{UserID: p.UserID, SessionID: p.SessionID}
}

// Validate 校验查询参数。
func (p SessionsParams) Validate() error { return p.Filter().Validate() }

// IOParams 是 terminal.io 流许可的参数 DTO。
//
// 它与会话查询带同一组字段：连一条终端和看一眼终端需要同一个归属判定。
type IOParams struct {
	UserID    int64  `json:"user_id"`
	SessionID string `json:"session_id"`
}

// Validate 校验流许可参数。
func (p IOParams) Validate() error {
	return ListFilter{UserID: p.UserID, SessionID: p.SessionID}.Validate()
}

// Validate 校验过滤条件。
func (f ListFilter) Validate() error {
	if f.UserID <= 0 {
		return fmt.Errorf("%w: user_id 必须为正整数", ErrInvalidParams)
	}
	if f.SessionID != "" && !IsValidSessionID(f.SessionID) {
		return fmt.Errorf("%w: session_id 形态不合法", ErrInvalidParams)
	}
	return nil
}

// IsValidSessionID 判断会话 id 的形态：32 位小写十六进制。
//
// 形态检查放在最前面，是因为 id 会进 URL 路径与日志：拒绝一切可疑字符比在十几处
// 分别转义更可靠。
func IsValidSessionID(id string) bool {
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

// CloseInput 是关闭会话的输入。
type CloseInput struct {
	SessionID string
	// Owner 是请求者账号 id：只有会话创建者能关自己的会话。
	Owner int64
	// Reason 是写进日志与 exit 帧的结束原因（user、idle、expired、shutdown、disconnect）。
	Reason string
}

// 结束原因闭集。它会出现在 exit 帧里，前端据此决定“重连还是停”。
const (
	ReasonUser       = "user"
	ReasonIdle       = "idle"
	ReasonExpired    = "expired"
	ReasonDisconnect = "disconnect"
	ReasonShutdown   = "shutdown"
	ReasonExit       = "exit"
	ReasonFailed     = "failed"
)

// 错误。全部是“代理判决”，Web 侧把它们翻成 HTTP 状态码，而不是一律 500。
var (
	// ErrUnsupported 表示本平台没有 PTY 支持（非 Linux）。
	ErrUnsupported = errors.New("terminal: 本平台不支持 PTY")
	// ErrDisabled 表示代理没有登记终端能力（账号不可用、没有 PTY，或本机未配置）。
	// 它与 ErrUnsupported 分开：一个说“这个平台做不到”，一个说“这台机器现在没开”。
	ErrDisabled = errors.New("terminal: 本机没有可用的受限终端")
	// ErrInvalidParams 表示参数不合法。
	ErrInvalidParams = errors.New("terminal: 参数不合法")
	// ErrNotFound 表示会话不存在（已结束、已过期或从未存在）。
	ErrNotFound = errors.New("terminal: 会话不存在")
	// ErrNotOwner 表示会话不属于请求者。
	ErrNotOwner = errors.New("terminal: 会话不属于该用户")
	// ErrLimitReached 表示会话数量已达上限。
	ErrLimitReached = errors.New("terminal: 会话数量已达上限")
	// ErrAlreadyAttached 表示会话已经被一条连接占用。
	ErrAlreadyAttached = errors.New("terminal: 会话已连接")
	// ErrClosed 表示会话或服务已关闭。
	ErrClosed = errors.New("terminal: 会话已结束")
	// ErrAccountInvalid 表示终端账号不可用（不存在、是 root、shell 是 nologin）。
	ErrAccountInvalid = errors.New("terminal: 终端账号不可用")
)

// ValidReason 判断结束原因是否在闭集内。
func ValidReason(reason string) bool {
	switch reason {
	case ReasonUser, ReasonIdle, ReasonExpired, ReasonDisconnect, ReasonShutdown, ReasonExit, ReasonFailed:
		return true
	default:
		return false
	}
}

// normaliseReason 把空值补成 user：只有请求关闭时才会传空。
func normaliseReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if ValidReason(reason) {
		return reason
	}
	return ReasonUser
}
