// Package operations 是 Web 侧的操作编排：预检记录的生成、绑定与持久化。
//
// 分工（CONTRACTS.md 4.1）：**预检由代理生成**——规范化参数、目标身份指纹、状态版本、
// 影响与阻塞项、确认短语都来自代理；Web 只负责三件事：
//   - 把请求和操作者上下文转发给代理（身份绑定的一半）；
//   - 把计划里需要复核的绑定信息落进 Web 库（用户、会话、动作、参数哈希、状态版本、到期）；
//   - 把计划原样整理成公开 DTO 返回给前端。
//
// Web **不**自己算“能不能执行”，也不保存确认短语明文：代理存短语哈希，短语只回给
// 管理员一次，提交时再回传由代理核对（P03.2）。
package operations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/store"
)

const (
	// MaxParamsBytes 是单个动作参数的上限：与代理登记表的 MaxParamsBytes 一致，
	// 超限的请求在 Web 就被拒，不必让一个 1 MiB 的请求体走到 socket。
	MaxParamsBytes = 64 << 10

	// PreflightTTL 是预检有效期（CONTRACTS.md 4.1：120 秒）。
	//
	// Web 拿代理返回的 expires_at 作为唯一真值，这里只用于校验代理没有给出离谱的值。
	PreflightTTL = 120 * time.Second

	// preflightIDPrefix 是 Web 侧预检 id 的前缀。
	preflightIDPrefix = "pf_"

	// 代理时钟与本机时钟允许的偏差：只用于拒绝“明显不合规”的到期时间。
	clockSkewAllowance = 5 * time.Second
)

// 本包的固定错误。调用方（api 层）据此映射 HTTP 状态码与契约错误码。
var (
	// ErrAgentUnavailable 表示代理不可用（没有配置客户端，或连不上）。
	ErrAgentUnavailable = errors.New("operations: 代理不可用")
	// ErrInvalidAction 表示动作名不合法（空、过长、含非法字符）。
	ErrInvalidAction = errors.New("operations: 动作名不合法")
	// ErrParamsTooLarge 表示参数超过上限。
	ErrParamsTooLarge = errors.New("operations: 动作参数超过上限")
	// ErrParamsNotObject 表示参数不是 JSON 对象。
	ErrParamsNotObject = errors.New("operations: 动作参数必须是 JSON 对象")
	// ErrInvalidPlan 表示代理返回的计划不合法（协议违约，不是业务拒绝）。
	ErrInvalidPlan = errors.New("operations: 代理返回的预检计划不合法")
)

// Agent 是 Web 需要的代理能力。生产实现是 *agentclient.Client；测试用假实现。
//
// 每个方法都必须有真实实现：预检与操作都不允许用“固定成功”顶替。
type Agent interface {
	CreatePreflight(ctx context.Context, in agentclient.PreflightInput) (agentclient.PreflightPlan, error)
	// CreateOperation 让代理持久接受一次操作（P03.2）。operation_id 由 Web 生成并落库在后。
	CreateOperation(ctx context.Context, in agentclient.OperationInput) (agentclient.Operation, error)
	// GetOperation 读回代理侧的执行记录，用于对账。
	GetOperation(ctx context.Context, operationID string) (agentclient.Operation, error)
	// CancelOperation 请求安全取消；动作是否可取消由代理的策略决定。
	CancelOperation(ctx context.Context, operationID string) (agentclient.Operation, error)
	// OperationEvents 读回代理侧事件流水（P03.3），用于镜像进 job_events。
	//
	// 它和 GetOperation 一样会把“连不上代理”与“代理说没有这条操作”分成两类错误：
	// 前者不能让任务被标成完成，后者不能让任务被标成失败后又重发。
	OperationEvents(ctx context.Context, operationID string) ([]agentclient.OperationEvent, error)
}

// Options 是构造 Service 的可选参数。
type Options struct {
	// Now 覆盖时钟；nil 表示 time.Now。只有测试会改它。
	Now func() time.Time
}

// Service 生成并保存预检记录。
type Service struct {
	db     *store.DB
	agent  Agent
	logger *slog.Logger
	now    func() time.Time

	// sweepMu 让对账扫描串行化：后台循环与显式调用不能同时扫，否则两条路径
	// 会各自向代理要一遍同样的操作，还会互相把对方刚写的 updated_at 当成新变化。
	sweepMu sync.Mutex
	// inflight 记录“已经落库、代理调用还没返回”的任务（P03.4）。
	//
	// 用途只有一个：对账看到代理说 NOT_FOUND 时，先看这条任务是不是正躺在提交途中。
	// 提交还没返回就断言“代理没这条操作”，会把一个正在执行的格式化动作记成失败。
	inflightMu sync.Mutex
	inflight   map[string]int
}

// New 构造服务。db 与 logger 必须非空；agent 允许为空（mock 模式或未配置 socket 时，
// 预检会明确返回“代理不可用”，而不是给出一份假的计划）。
func New(db *store.DB, agent Agent, logger *slog.Logger, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("operations: 数据库句柄不能为空")
	}
	if logger == nil {
		return nil, errors.New("operations: Logger 不能为空")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		db:       db,
		agent:    agent,
		logger:   logger,
		now:      now,
		inflight: make(map[string]int),
	}, nil
}

// Target 是预检绑定的目标。
type Target struct {
	DisplayName         string
	IdentityFingerprint string
	StateVersion        string
}

// Notice 是阻塞项/告警项。
type Notice struct {
	Code    string
	Message string
}

// Impact 是操作影响。
type Impact struct {
	Kind        string
	Description string
}

// Preflight 是 Web 侧的预检结果（不含确认短语以外的秘密材料）。
type Preflight struct {
	ID                 string
	Action             string
	NormalizedParams   json.RawMessage
	Target             Target
	ExpiresAt          time.Time
	CreatedAt          time.Time
	Allowed            bool
	Blockers           []Notice
	Warnings           []Notice
	Impacts            []Impact
	RequiresReauth     bool
	RequiresPhrase     bool
	ConfirmationPhrase string
	Cancellable        bool
	Locks              []string
}

// CreateInput 是创建预检的输入。UserID 与 SessionHash 是身份绑定的一半，
// 缺少它们就没有“这份预检属于谁、哪个会话”可谈。
type CreateInput struct {
	UserID      int64
	SessionHash string
	// ReauthAt 是最近一次密码再认证时间；零值表示当前不在再认证窗口内。
	ReauthAt  time.Time
	RequestID string
	Action    string
	Params    json.RawMessage
}

// CreatePreflight 让代理生成计划，并把绑定信息落进 Web 库。
//
// 返回代理的业务拒绝（*agentclient.Error）或不可用（*agentclient.UnavailableError）时
// 原样上抛，由 API 层区分状态码——这里不把“连不上代理”翻译成“动作不合法”。
func (s *Service) CreatePreflight(ctx context.Context, in CreateInput) (Preflight, error) {
	if err := validateActionName(in.Action); err != nil {
		return Preflight{}, err
	}
	if in.UserID <= 0 {
		return Preflight{}, errors.New("operations: UserID 必须为正数")
	}
	if len(in.SessionHash) != 64 {
		return Preflight{}, errors.New("operations: SessionHash 必须是 64 位十六进制")
	}
	params, err := normalizeParams(in.Params)
	if err != nil {
		return Preflight{}, err
	}
	if s.agent == nil {
		return Preflight{}, fmt.Errorf("%w: 未配置代理 socket", ErrAgentUnavailable)
	}

	actor := agentclient.ActorContext{
		UserID:        in.UserID,
		SessionIDHash: in.SessionHash,
		RequestID:     in.RequestID,
	}
	if !in.ReauthAt.IsZero() {
		at := in.ReauthAt.UTC().Format(time.RFC3339)
		actor.ReauthAt = &at
	}

	plan, err := s.agent.CreatePreflight(ctx, agentclient.PreflightInput{
		Action: in.Action,
		Params: params,
		Actor:  actor,
	})
	if err != nil {
		return Preflight{}, err
	}

	expiresAt, err := time.Parse(time.RFC3339, plan.ExpiresAt)
	if err != nil {
		return Preflight{}, fmt.Errorf("%w: expires_at 不是 RFC3339 时间: %v", ErrInvalidPlan, err)
	}
	expiresAt = expiresAt.UTC()
	now := s.now().UTC()
	// 代理是期限的唯一真值来源，但一个明显超出 120 秒的到期时间说明两端对契约的理解已经不同：
	// 这种计划不能落库，否则“预检 120 秒有效”就变成了一句无人核对的话。
	if expiresAt.Sub(now) > PreflightTTL+clockSkewAllowance {
		return Preflight{}, fmt.Errorf("%w: 到期时间 %s 超出 %s 的有效期上限", ErrInvalidPlan,
			expiresAt.Format(time.RFC3339), PreflightTTL)
	}
	if !expiresAt.After(now) {
		return Preflight{}, fmt.Errorf("%w: 计划在返回时已过期（%s）", ErrInvalidPlan, expiresAt.Format(time.RFC3339))
	}

	canonical, err := canonicalJSON(plan.NormalizedParams)
	if err != nil {
		return Preflight{}, fmt.Errorf("%w: 规范化参数不是合法 JSON: %v", ErrInvalidPlan, err)
	}
	paramsHash := hashHex(canonical)

	targetBytes, err := json.Marshal(map[string]string{
		"display_name":         plan.Target.DisplayName,
		"identity_fingerprint": plan.Target.IdentityFingerprint,
		"state_version":        plan.Target.StateVersion,
	})
	if err != nil {
		return Preflight{}, fmt.Errorf("%w: 目标信息无法序列化: %v", ErrInvalidPlan, err)
	}

	id, err := newPreflightID()
	if err != nil {
		return Preflight{}, err
	}

	createdAt := now
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO preflights
		  (id, user_id, session_hash, action, params_hash, normalized_params, target_json,
		   state_version, agent_ref, allowed, requires_reauth, requires_phrase, cancellable,
		   created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.UserID, in.SessionHash, plan.Action, paramsHash, string(canonical), string(targetBytes),
		plan.Target.StateVersion, plan.Ref, boolInt(plan.Allowed), boolInt(plan.RequiresReauth),
		boolInt(plan.RequiresPhrase), boolInt(plan.Cancellable), formatTime(createdAt), formatTime(expiresAt))
	if err != nil {
		return Preflight{}, fmt.Errorf("operations: 保存预检记录失败: %w", err)
	}

	s.logger.Info("已登记预检",
		"preflight_id", id,
		"agent_ref", plan.Ref,
		"action", plan.Action,
		"user_id", in.UserID,
		"allowed", plan.Allowed,
		"requires_reauth", plan.RequiresReauth,
		"requires_phrase", plan.RequiresPhrase,
		"params_hash", paramsHash,
		"expires_at", expiresAt.Format(time.RFC3339))

	return Preflight{
		ID:                 id,
		Action:             plan.Action,
		NormalizedParams:   canonical,
		Target:             Target(plan.Target),
		ExpiresAt:          expiresAt,
		CreatedAt:          createdAt,
		Allowed:            plan.Allowed,
		Blockers:           notices(plan.Blockers),
		Warnings:           notices(plan.Warnings),
		Impacts:            impacts(plan.Impacts),
		RequiresReauth:     plan.RequiresReauth,
		RequiresPhrase:     plan.RequiresPhrase,
		ConfirmationPhrase: plan.ConfirmationPhrase,
		Cancellable:        plan.Cancellable,
		Locks:              nonNilStrings(plan.Locks),
	}, nil
}

// validateActionName 校验动作名的形状。
//
// 只做“形状”检查：具体有哪些动作由代理登记表决定（Web 不复制一份动作清单，
// 否则两张表迟早会漂移）。这里拦的是空串与明显不可能被登记的名字。
func validateActionName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: 不能为空", ErrInvalidAction)
	}
	if len(name) > 96 {
		return fmt.Errorf("%w: 超过 96 字节", ErrInvalidAction)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' {
			continue
		}
		return fmt.Errorf("%w: 含非法字符 %q", ErrInvalidAction, r)
	}
	if !strings.Contains(name, ".") {
		return fmt.Errorf("%w: 应为小写点分名称（例如 storage.filesystem.format）", ErrInvalidAction)
	}
	return nil
}

// normalizeParams 校验参数并返回原始 JSON（缺省按空对象处理）。
func normalizeParams(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return json.RawMessage("{}"), nil
	}
	if len(trimmed) > MaxParamsBytes {
		return nil, fmt.Errorf("%w: %d 字节，上限 %d 字节", ErrParamsTooLarge, len(trimmed), MaxParamsBytes)
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: 收到 %q", ErrParamsNotObject, trimmed[0])
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: 参数不是合法 JSON", ErrParamsNotObject)
	}
	return raw, nil
}

// canonicalJSON 生成规范 JSON：解到 any 再编码，键有序、无多余空白。
func canonicalJSON(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// hashHex 返回字节的 SHA-256 十六进制值。
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newPreflightID 生成 Web 侧预检 id（pf_ + 16 字节随机值）。
func newPreflightID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("operations: 生成预检 id 失败: %w", err)
	}
	return preflightIDPrefix + hex.EncodeToString(b[:]), nil
}

func notices(in []agentclient.PreflightNotice) []Notice {
	out := make([]Notice, 0, len(in))
	for _, n := range in {
		out = append(out, Notice{Code: n.Code, Message: n.Message})
	}
	return out
}

func impacts(in []agentclient.PreflightImpact) []Impact {
	out := make([]Impact, 0, len(in))
	for _, i := range in {
		out = append(out, Impact{Kind: i.Kind, Description: i.Description})
	}
	return out
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// formatTime 把时间格式化成数据库里的 RFC3339（UTC）。
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
