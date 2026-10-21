package terminal

// Web 侧的终端调用端（P06.3）。
//
// Web 进程从不接触 PTY：它把“谁的会话、哪个 id”交给代理，代理判定归属后再决定
// 给不给连接。这一层做三件事：
//  1. 参数先在本地校验一遍（形态错的 session_id 不该占一次 IPC，也不该进代理日志）；
//  2. 结果自洽性校验（代理返回的会话缺字段时明确报“响应不完整”，不拿零值充数）；
//  3. 把代理的拒绝翻译成 Web 的错误，让接口层按状态码回答，而不是一律 503。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
)

// ErrAgentUnavailable 表示终端不可用是因为代理不可用（连不上、超时、响应不合法）。
var ErrAgentUnavailable = errors.New("terminal: 代理不可用")

// Agent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type Agent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
	CreateStream(ctx context.Context, purpose string, params any) (agentclient.StreamPermit, error)
	DialTerminalStream(ctx context.Context, streamID string, subprotocols []string) (*websocket.Conn, error)
	CloseTerminalSession(ctx context.Context, sessionID string, userID int64, reason string) error
}

// CapabilitiesProbe 是“代理登记了哪些能力”的最小接口（生产实现是 *agentclient.Client）。
//
// 它只用来回答一个问题：这台机器到底有没有终端能力。代理在终端账号不可用（不存在、
// 是 root、shell 是 nologin、home 权限过宽、在特权组里，或者平台没有 PTY）时**不登记**
// terminal.create / terminal.sessions——这是有意的：带着一个注定起不来的 shell 对外宣称
// “有终端”，只会把问题推给用户去猜。
//
// 不探测的话，用户打开终端页看到的是代理那句“未登记的查询 terminal.sessions”（400），
// 它既不像配置问题，也不像权限问题。探测之后这里能明确回答 501 并说明原因。
type CapabilitiesProbe interface {
	Capabilities(ctx context.Context) (agentclient.Capabilities, error)
}

// capabilityProbeTimeout 是一次能力探测的时限。它发生在 Web 启动时，本机 socket，
// 正常情况下是毫秒级；给 5 秒是为了不让一次慢启动把整个 Web 拖住。
const capabilityProbeTimeout = 5 * time.Second

// RemoteError 是代理明确拒绝了一次终端操作。
type RemoteError struct {
	Status  int
	Code    httpx.Code
	Message string
}

// Error 实现 error。
func (e *RemoteError) Error() string { return e.Message }

// SessionsResult 是终端会话列表与终端边界说明。
//
// 把账号、uid 与三个时限一起下发，是为了让界面能**显示**边界而不是复述宣传语：
// “以 upanel-shell（uid 998）运行，15 分钟无输入关闭，最长 1 小时”，
// 每个数字都来自代理的当前配置，改配置页面就跟着变。
type SessionsResult struct {
	Account                string        `json:"account"`
	AccountUID             uint32        `json:"account_uid"`
	IdleTimeoutSeconds     int           `json:"idle_timeout_seconds"`
	MaxDurationSeconds     int           `json:"max_duration_seconds"`
	DisconnectGraceSeconds int           `json:"disconnect_grace_seconds"`
	MaxPerUser             int           `json:"max_per_user"`
	MaxSessions            int           `json:"max_sessions"`
	Sessions               []SessionInfo `json:"sessions"`
}

// Remote 是 Web 侧的终端调用端。
type Remote struct {
	agent  Agent
	logger *slog.Logger
	// disabled 非空表示代理没有登记终端能力（账号不可用 / 本机没有 PTY）。
	// 这不是错误状态，而是“这台机器没有终端”的事实：所有调用都按 501 回答。
	disabled error
}

// NewRemote 构造 Remote。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要运行时用一个永远返回空列表的假实现顶上。
func NewRemote(agent Agent, logger *slog.Logger) (*Remote, error) {
	if agent == nil {
		return nil, errors.New("terminal: Remote 需要代理客户端")
	}
	r := &Remote{agent: agent, logger: logger}
	r.probeCapabilities()
	return r, nil
}

// probeCapabilities 在启动时问一次代理“你登记了终端能力吗”。
//
// 探测失败（代理还没起来、socket 不通）不改变行为：那时终端按“可用”处理，真正的
// 判定留给调用时的 503。只有**代理明确回答**“没有登记”才把终端标记为不可用——
// 把一次连接失败当成“本机没有终端”会让人去查账号，而问题其实在 socket 上。
func (r *Remote) probeCapabilities() {
	probe, ok := r.agent.(CapabilitiesProbe)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), capabilityProbeTimeout)
	defer cancel()
	caps, err := probe.Capabilities(ctx)
	if err != nil {
		r.warn("读取代理能力失败：终端按可用处理，调用时再判定", err)
		return
	}
	if hasName(caps.SupportedQueries, QuerySessions) && hasName(caps.SupportedActions, ActionCreate) {
		return
	}
	r.disabled = fmt.Errorf("%w: 代理没有登记 terminal.create / terminal.sessions"+
		"（终端账号不可用或本机没有 PTY）", ErrDisabled)
	r.warn("终端能力未启用", r.disabled)
}

func hasName(list []string, want string) bool {
	for _, name := range list {
		if name == want {
			return true
		}
	}
	return false
}

// unavailable 在终端能力未启用时返回统一的 501 判决。
func (r *Remote) unavailable() error {
	if r.disabled == nil {
		return nil
	}
	return r.disabled
}

// Sessions 列出某个面板账号的会话。
func (r *Remote) Sessions(ctx context.Context, userID int64, sessionID string) (SessionsResult, error) {
	if err := r.unavailable(); err != nil {
		return SessionsResult{}, err
	}
	params := SessionsParams{UserID: userID, SessionID: sessionID}
	if err := params.Validate(); err != nil {
		return SessionsResult{}, err
	}
	raw, err := r.agent.Query(ctx, QuerySessions, params)
	if err != nil {
		return SessionsResult{}, r.mapError(err, "读取终端会话")
	}
	var result SessionsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return SessionsResult{}, fmt.Errorf("%w: 代理返回的终端会话不是合法 JSON: %v", ErrAgentUnavailable, err)
	}
	if result.Account == "" || result.AccountUID == 0 {
		// 账号字段缺失意味着“代理没有真的启起终端”；这时把空账号显示成 upanel-shell
		// 会让页面撒谎，所以按响应不完整处理。
		return SessionsResult{}, fmt.Errorf("%w: 代理返回的终端信息缺少账号字段", ErrAgentUnavailable)
	}
	if result.Sessions == nil {
		result.Sessions = []SessionInfo{}
	}
	return result, nil
}

// Session 读取单个会话（也用于“这个 id 是不是我的”）。
func (r *Remote) Session(ctx context.Context, userID int64, sessionID string) (SessionInfo, SessionsResult, error) {
	result, err := r.Sessions(ctx, userID, sessionID)
	if err != nil {
		return SessionInfo{}, SessionsResult{}, err
	}
	if len(result.Sessions) == 0 {
		return SessionInfo{}, result, fmt.Errorf("%w: 会话 %s", ErrNotFound, sessionID)
	}
	return result.Sessions[0], result, nil
}

// Close 关闭一个会话。
func (r *Remote) Close(ctx context.Context, userID int64, sessionID, reason string) error {
	if err := r.unavailable(); err != nil {
		return err
	}
	if !IsValidSessionID(sessionID) {
		return fmt.Errorf("%w: session_id 形态不合法", ErrInvalidParams)
	}
	if err := r.agent.CloseTerminalSession(ctx, sessionID, userID, normaliseReason(reason)); err != nil {
		return r.mapError(err, "关闭终端会话")
	}
	return nil
}

// Connect 申请一次性许可并拨出一条到代理的终端 WebSocket。
//
// 许可在代理侧是单次凭据：拨号失败就作废。调用方（Web 的 /connect 处理器）在
// 拨号成功之后才把它转给浏览器，因此浏览器的重连必须重新走一次本方法。
func (r *Remote) Connect(ctx context.Context, userID int64, sessionID string) (*websocket.Conn, error) {
	if err := r.unavailable(); err != nil {
		return nil, err
	}
	params := IOParams{UserID: userID, SessionID: sessionID}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	permit, err := r.agent.CreateStream(ctx, PurposeIO, params)
	if err != nil {
		return nil, r.mapError(err, "申请终端连接许可")
	}
	conn, err := r.agent.DialTerminalStream(ctx, permit.StreamID, []string{ProtocolName})
	if err != nil {
		return nil, r.mapError(err, "连接终端")
	}
	if conn == nil {
		// 没有连接也没有错误：属于调用端违约。绝不能把 nil 当成“连上了”交给上层，
		// 那会在转发循环里变成一次空指针崩溃。
		return nil, fmt.Errorf("%w: 代理没有返回终端连接", ErrAgentUnavailable)
	}
	return conn, nil
}

// mapError 把代理错误翻译成 terminal 包的错误。
//
// 三类分开处理：代理不可用 → 503；代理的明确拒绝 → 原样带出状态码与码；
// 其余 → 如实包装，接口层按 500 处理。
func (r *Remote) mapError(err error, what string) error {
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		r.warn(what+"：代理不可用", err)
		return fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
	}
	var rejected *agentclient.Error
	if errors.As(err, &rejected) {
		if rejected.Status >= 500 {
			r.warn(what+"：代理返回错误", err)
			return fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return &RemoteError{Status: rejected.Status, Code: httpx.Code(rejected.Code), Message: rejected.Message}
	}
	return fmt.Errorf("terminal: %s 失败: %w", what, err)
}

func (r *Remote) warn(msg string, err error) {
	if r.logger != nil {
		r.logger.Warn(msg, "error", err)
	}
}
