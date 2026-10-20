// Package agentclient 是 Web 进程调用特权代理的唯一客户端（PLAN.md 第 22 节）。
//
// 固定约定（PLAN.md 第 4 节、CONTRACTS.md 第 15 节）：
//   - 只通过 Unix socket 连接，不监听、也不直连任何 TCP 端口；
//   - 只调用 /internal/v1 下的专用接口，不把公开 HTTP 路由整体转发给代理；
//   - 失败时返回带错误码的类型化错误，调用方必须把“代理不可用”与“业务拒绝”分开，
//     不能把连不上代理当成动作成功。
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"time"
)

// DefaultTimeout 是单次调用的整体超时。
const DefaultTimeout = 10 * time.Second

// maxResponseBytes 限制客户端读取的响应体上限，避免代理异常时把内存吃光。
const maxResponseBytes = 8 << 20

// Client 是固定 Unix socket 客户端。
type Client struct {
	socketPath string
	http       *http.Client
}

// Option 是客户端选项。
type Option func(*Client)

// WithDialer 替换底层连接方式。只能在测试里用（开发宿主无法创建 Unix socket）；
// 生产路径始终是 socketPath 指向的 Unix socket。
func WithDialer(dial func(ctx context.Context, network, address string) (net.Conn, error)) Option {
	return func(c *Client) {
		c.http.Transport = &http.Transport{
			DialContext:           dial,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: DefaultTimeout,
		}
	}
}

// WithTimeout 替换单次调用超时。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// New 构造客户端。socketPath 必须是绝对路径。
func New(socketPath string, opts ...Option) (*Client, error) {
	if socketPath == "" {
		return nil, errors.New("agentclient: socket 路径不能为空")
	}
	if !path.IsAbs(socketPath) {
		return nil, fmt.Errorf("agentclient: socket 路径必须是绝对路径，当前为 %q", socketPath)
	}
	c := &Client{
		socketPath: socketPath,
		http: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
				MaxIdleConns:          4,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: DefaultTimeout,
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// SocketPath 返回客户端使用的 socket 路径。
func (c *Client) SocketPath() string { return c.socketPath }

// Capabilities 读取代理的版本、模式与已登记能力。
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	raw, err := c.do(ctx, http.MethodGet, "/internal/v1/capabilities", nil)
	if err != nil {
		return Capabilities{}, err
	}
	var caps Capabilities
	if err := json.Unmarshal(raw, &caps); err != nil {
		return Capabilities{}, fmt.Errorf("agentclient: capabilities 响应无法解析: %w", err)
	}
	return caps, nil
}

// Query 调用一个已登记的结构化只读查询，返回原始 data。
// params 为 nil 时发送空对象（不许缺省成“任意参数”）。
func (c *Client) Query(ctx context.Context, name string, params any) (json.RawMessage, error) {
	if name == "" {
		return nil, errors.New("agentclient: query 名不能为空")
	}
	body := struct {
		Query  string `json:"query"`
		Params any    `json:"params"`
	}{Query: name, Params: params}
	if params == nil {
		body.Params = struct{}{}
	}
	return c.do(ctx, http.MethodPost, "/internal/v1/queries", body)
}

// PutSecretInput 是写入一条秘密的请求（CONTRACTS.md 第 15 节）。
//
// 只有用途、明文与可选引用：没有类型标签、没有任意键值对，也没有“读回来”的对应接口。
type PutSecretInput struct {
	Purpose string
	Value   string
	// Reference 是引用这份秘密的配置标识；为空时是到达存活期后自动销毁的临时秘密。
	Reference string
}

// SecretRef 是 POST /internal/v1/secrets 的响应：只有引用与生命周期信息。
type SecretRef struct {
	Ref       string `json:"secret_ref"`
	Purpose   string `json:"purpose"`
	State     string `json:"state"`
	ExpiresAt string `json:"expires_at"`
}

// PutSecret 让代理加密保存一条秘密，返回引用。明文只出现在这一次请求体里，
// 客户端不缓存、不回显、不写日志。
func (c *Client) PutSecret(ctx context.Context, in PutSecretInput) (SecretRef, error) {
	body := struct {
		Purpose   string `json:"purpose"`
		Value     string `json:"value"`
		Reference string `json:"reference,omitempty"`
	}{Purpose: in.Purpose, Value: in.Value, Reference: in.Reference}
	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/secrets", body)
	if err != nil {
		return SecretRef{}, err
	}
	var out SecretRef
	if err := json.Unmarshal(raw, &out); err != nil {
		return SecretRef{}, fmt.Errorf("agentclient: 秘密写入响应无法解析: %w", err)
	}
	if out.Ref == "" {
		return SecretRef{}, &UnavailableError{Err: errors.New("代理响应缺少 secret_ref")}
	}
	return out, nil
}

// ActorContext 是操作者上下文（CONTRACTS.md 第 15 节）：预检与任务都带上它。
//
// 代理信任 Web 已完成登录认证，用这份上下文把计划/任务绑定到具体的人与会话。
type ActorContext struct {
	UserID        int64  `json:"user_id"`
	SessionIDHash string `json:"session_id_hash"`
	RequestID     string `json:"request_id"`
	// ReauthAt 是最近一次密码再认证时间（RFC3339）；没有再认证时为 null。
	ReauthAt *string `json:"reauth_at"`
}

// PreflightInput 是请求一份预检计划。
type PreflightInput struct {
	Action string
	// Params 是动作参数（原始 JSON）。为 nil 时发送空对象。
	Params json.RawMessage
	Actor  ActorContext
}

// PreflightTarget 是计划绑定的目标。
type PreflightTarget struct {
	DisplayName         string `json:"display_name"`
	IdentityFingerprint string `json:"identity_fingerprint"`
	StateVersion        string `json:"state_version"`
}

// PreflightNotice 是阻塞项/告警项。
type PreflightNotice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PreflightImpact 是操作影响。
type PreflightImpact struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// PreflightPlan 是代理生成的预检计划。
//
// NormalizedParams 保留代理发来的原始字节：参数哈希是按这份字节算的，
// 任何本地重新序列化都可能让哈希对不上。
type PreflightPlan struct {
	Ref              string            `json:"preflight_ref"`
	Action           string            `json:"action"`
	NormalizedParams json.RawMessage   `json:"normalized_params"`
	Target           PreflightTarget   `json:"target"`
	ExpiresAt        string            `json:"expires_at"`
	Allowed          bool              `json:"allowed"`
	Blockers         []PreflightNotice `json:"blockers"`
	Warnings         []PreflightNotice `json:"warnings"`
	Impacts          []PreflightImpact `json:"impacts"`
	RequiresReauth   bool              `json:"requires_reauth"`
	RequiresPhrase   bool              `json:"requires_phrase"`
	// ConfirmationPhrase 只在需要短语时出现，且只出现这一次（代理只存哈希）。
	ConfirmationPhrase string   `json:"confirmation_phrase"`
	Cancellable        bool     `json:"cancellable"`
	Locks              []string `json:"locks"`
}

// CreatePreflight 让代理生成一份预检计划。
//
// 代理返回的拒绝（未登记动作、参数不合法、缺预检实现）是 *Error；
// 连不上或响应不合法是 *UnavailableError。调用方必须把两者分开。
func (c *Client) CreatePreflight(ctx context.Context, in PreflightInput) (PreflightPlan, error) {
	if in.Action == "" {
		return PreflightPlan{}, errors.New("agentclient: action 不能为空")
	}
	params := in.Params
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage("{}")
	}
	body := struct {
		Action string          `json:"action"`
		Params json.RawMessage `json:"params"`
		Actor  ActorContext    `json:"actor_context"`
	}{Action: in.Action, Params: params, Actor: in.Actor}

	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/preflights", body)
	if err != nil {
		return PreflightPlan{}, err
	}
	var plan PreflightPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return PreflightPlan{}, &UnavailableError{Err: fmt.Errorf("预检响应无法解析: %w", err)}
	}
	if plan.Ref == "" || plan.Action == "" || len(plan.NormalizedParams) == 0 || plan.ExpiresAt == "" {
		return PreflightPlan{}, &UnavailableError{Err: errors.New("预检响应缺少必需字段")}
	}
	return plan, nil
}

// OperationInput 是一次操作提交（CONTRACTS.md 4.2、第 15 节）。
//
// 没有 action、没有 params：提交时不能替换目标，参数由代理从预检里取回。
type OperationInput struct {
	// OperationID 由 Web 生成并**先落库**：网络超时后按它查询，不复用旧 ID 之外的任何东西。
	OperationID  string
	PreflightRef string
	// Confirmation 是确认短语（动作要求短语时必填）。
	Confirmation string
	Actor        ActorContext
}

// OperationTarget 是操作视图里的目标。
type OperationTarget struct {
	DisplayName         string `json:"display_name"`
	IdentityFingerprint string `json:"identity_fingerprint"`
}

// Operation 是代理侧操作视图（内部协议）。
type Operation struct {
	OperationID           string          `json:"operation_id"`
	Action                string          `json:"action"`
	PreflightRef          string          `json:"preflight_ref"`
	Stage                 string          `json:"stage"`
	Step                  string          `json:"step"`
	ProgressPercent       *int            `json:"progress_percent"`
	Cancellable           bool            `json:"cancellable"`
	CancellationRequested bool            `json:"cancellation_requested"`
	Target                OperationTarget `json:"target"`
	Result                json.RawMessage `json:"result"`
	Error                 json.RawMessage `json:"error"`
	CreatedAt             string          `json:"created_at"`
	StartedAt             string          `json:"started_at"`
	FinishedAt            string          `json:"finished_at"`
	UpdatedAt             string          `json:"updated_at"`
	// Duplicate 表示这次提交是重放（代理已有同一 operation_id 与同一请求）。
	Duplicate bool `json:"duplicate"`
}

// CreateOperation 提交一次操作，返回**已被接受并持久化**的记录。
//
// 代理返回的拒绝（预检过期/失效、短语不符、幂等冲突、动作不可执行）是 *Error；
// 连不上或响应不合法是 *UnavailableError。两种都必须区分：前者不能重试，
// 后者需要按 operation_id 重新查询而不是重新执行。
func (c *Client) CreateOperation(ctx context.Context, in OperationInput) (Operation, error) {
	if in.OperationID == "" {
		return Operation{}, errors.New("agentclient: operation_id 不能为空")
	}
	if in.PreflightRef == "" {
		return Operation{}, errors.New("agentclient: preflight_ref 不能为空")
	}
	body := struct {
		OperationID  string       `json:"operation_id"`
		PreflightRef string       `json:"preflight_ref"`
		Confirmation string       `json:"confirmation,omitempty"`
		Actor        ActorContext `json:"actor_context"`
	}{
		OperationID:  in.OperationID,
		PreflightRef: in.PreflightRef,
		Confirmation: in.Confirmation,
		Actor:        in.Actor,
	}
	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/operations", body)
	if err != nil {
		return Operation{}, err
	}
	return decodeOperation(raw)
}

// GetOperation 按 operation_id 读回实际状态。超时之后的唯一正确动作就是它。
func (c *Client) GetOperation(ctx context.Context, operationID string) (Operation, error) {
	if operationID == "" {
		return Operation{}, errors.New("agentclient: operation_id 不能为空")
	}
	raw, err := c.do(ctx, http.MethodGet, "/internal/v1/operations/"+operationID, nil)
	if err != nil {
		return Operation{}, err
	}
	return decodeOperation(raw)
}

// OperationEvent 是代理侧的一条操作事件（内部协议，CONTRACTS.md 第 15 节）。
//
// Seq 在**操作内**单调递增，是 Web 侧镜像到 job_events 的水位线：
// 每次对账只取回比上次镜像更大的 seq，重复对账不会把同一条事件抄两遍。
type OperationEvent struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// OperationEvents 读回代理侧操作的事件流水。
//
// 与 GetOperation 一样：代理回业务错误是 *Error，连不上或响应不合法是 *UnavailableError。
// 调用方不能把“读不到事件”当成“没有事件”——那会把一次网络抖动写成任务已完成。
func (c *Client) OperationEvents(ctx context.Context, operationID string) ([]OperationEvent, error) {
	if operationID == "" {
		return nil, errors.New("agentclient: operation_id 不能为空")
	}
	raw, err := c.do(ctx, http.MethodGet, "/internal/v1/operations/"+operationID+"/events", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		OperationID string           `json:"operation_id"`
		Events      []OperationEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("操作事件响应无法解析: %w", err)}
	}
	if out.OperationID != operationID {
		return nil, &UnavailableError{Err: fmt.Errorf("操作事件响应的 operation_id 不一致：%q", out.OperationID)}
	}
	for i, ev := range out.Events {
		if ev.Seq <= 0 || ev.Kind == "" {
			return nil, &UnavailableError{Err: fmt.Errorf("第 %d 条操作事件缺少 seq 或 kind", i+1)}
		}
	}
	return out.Events, nil
}

// CancelOperation 请求安全取消。不可取消的动作由代理以 409 拒绝（*Error）。
func (c *Client) CancelOperation(ctx context.Context, operationID string) (Operation, error) {
	if operationID == "" {
		return Operation{}, errors.New("agentclient: operation_id 不能为空")
	}
	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/operations/"+operationID+"/cancel", nil)
	if err != nil {
		return Operation{}, err
	}
	return decodeOperation(raw)
}

// decodeOperation 解出并校验操作视图。
func decodeOperation(raw json.RawMessage) (Operation, error) {
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return Operation{}, &UnavailableError{Err: fmt.Errorf("操作响应无法解析: %w", err)}
	}
	if op.OperationID == "" || op.Stage == "" {
		return Operation{}, &UnavailableError{Err: errors.New("操作响应缺少必需字段")}
	}
	return op, nil
}

// Capabilities 是代理 capabilities 的客户端视图。
type Capabilities struct {
	Version          string   `json:"version"`
	Commit           string   `json:"commit"`
	BuildDate        string   `json:"build_date"`
	GoVersion        string   `json:"go_version"`
	Mode             string   `json:"mode"`
	OS               string   `json:"os"`
	Arch             string   `json:"arch"`
	StartedAt        string   `json:"started_at"`
	SupportedActions []string `json:"supported_actions"`
	SupportedQueries []string `json:"supported_queries"`
}

// Error 是代理返回的业务拒绝（带契约错误码）。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("agentclient: 代理返回 %d %s: %s", e.Status, e.Code, e.Message)
}

// UnavailableError 表示代理不可用（连不上、超时、响应不合法）。
// 与 *Error 区分开：业务拒绝不能重试，不可用可能需要稍后重试或报 AGENT_UNAVAILABLE。
type UnavailableError struct {
	Err error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("agentclient: 代理不可用: %v", e.Err)
}
func (e *UnavailableError) Unwrap() error { return e.Err }

// do 发起一次调用并解出 data。
func (c *Client) do(ctx context.Context, method, path string, payload any) (json.RawMessage, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("agentclient: 请求体无法序列化: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return nil, fmt.Errorf("agentclient: 构造请求失败: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &UnavailableError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, maxResponseBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("读取代理响应失败: %w", err)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
			return nil, &UnavailableError{Err: fmt.Errorf("代理返回 %d 且响应不是错误信封", resp.StatusCode)}
		}
		return nil, &Error{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}

	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("代理响应不是合法 JSON: %w", err)}
	}
	if len(env.Data) == 0 {
		return nil, &UnavailableError{Err: errors.New("代理响应缺少 data")}
	}
	return env.Data, nil
}
