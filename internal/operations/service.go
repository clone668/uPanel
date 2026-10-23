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
	// MaxParamsBytes 是 Web 侧的**传输**上限：信封必须容得下最大的逐动作参数上限。
	//
	// 逐动作的严格限制在代理登记表里（Action.ParamsLimitBytes），由代理在解码前执行：
	// file.write_text 的参数可以带 1 MiB 内容，传输层因此放宽到 2 MiB，其余动作
	// 仍是 64 KiB（CONTRACTS.md 第 567 行）。Web 不复制那张动作表——两个说法迟早会
	// 对不上，而“保存一个 200 KiB 的配置文件”会变成只有一条链路上说得清的事。
	MaxParamsBytes = 2 << 20

	// PreflightTTL 是预检有效期（CONTRACTS.md 4.1：120 秒）。
	//
	// Web 拿代理返回的 expires_at 作为唯一真值，这里只用于校验代理没有给出离谱的值。
	PreflightTTL = 120 * time.Second

	// preflightIDPrefix 是 Web 侧预检 id 的前缀。
	preflightIDPrefix = "pf_"

	// 代理时钟与本机时钟允许的偏差：只用于拒绝“明显不合规”的到期时间。
	clockSkewAllowance = 5 * time.Second
)

// PreflightAgentTimeout 是 Web 调用代理**生成计划**（预检与提交时的执行前复核）时的等待上限。
//
// 比 agentclient.DefaultTimeout（10 秒）宽得多，因为预检里有一条**真的慢**：
// 软件包动作的预检要跑 `apt-get -s`。实测（24.04 验收机，apt 2.8.3）：
// `apt-get -s --only-upgrade install zlib1g` 15.6 秒，普通 install 模拟 4～10 秒。
// 用 10 秒的通用上限，一次**正常**的升级预检会被切成 AGENT_UNAVAILABLE——
// 这是假话，比慢更糟（P11.2 验收第一轮就是这么失败的：duration_ms 10162，
// 代理那边正跑到一半被 context canceled 掐掉）。
//
// 它比代理自己的模拟预算（internal/agent 的 PlanBudget，60 秒）长，所以真超时
// 的时候先说话的是代理："预检超过 60s 未完成…稍后重试"，而不是 Web 猜的"代理不可用"。
// 90 秒也是这条链路的实际天花板：预检有效期只有 120 秒，等更久没有意义。
//
// 同一个上限管提交：POST /jobs 在代理侧也要复核一次计划（跑同一个 Plan），
// 验收机上实测这一次复核 38.7 秒。两条路用同一个数，是因为慢的原因完全一样。
//
// 超时只是上限，不是等待时间：代理正常时预检照样是几百毫秒到十几秒返回。
const PreflightAgentTimeout = 90 * time.Second

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
	// Gate 是 Web 侧的额外判决（P09.4 的启动依赖门禁）。
	//
	// 它在代理**已经**给出计划之后被调用，而且只能收紧：Gate 返回任何阻塞项，
	// 这条预检就落库为 allowed=0，POST /jobs 随即得到 409 DEPENDENCY_BLOCKED。
	// 反过来永远不成立——Gate 说没事不能让一条代理拒绝过的预检变成可执行。
	//
	// 为什么需要它：依赖卷和应用之间的绑定关系只存在 Web 库里，代理根本不知道
	// “这个容器吃哪个卷”，也就没法在自己的预检里算这条。允许为 nil（没有应用模块
	// 的部署）：那时没有依赖关系可守，门禁不介入。
	Gate Gate
	// OnJobFinished 在任务**刚刚**进入终态时被调用一次（P10.3 引入）。
	//
	// 它存在的理由：任务结果是代理给的，但“这次任务之后证书表该长什么样”只有 Web
	// 知道——证书行、站点绑定、到期时间都躺在 Web 库里。没有这个回调，panel 就只能
	// 在客户端刷新页面时顺手补写，而任务中心关掉标签页的那一刻真相就丢了。
	//
	// 调用时机严格收紧在**状态迁移到终态的那一次**：对账会反复看到一个已经 succeeded
	// 的操作，若每次都对账都回调，证书的 updated_at 会被无意义地刷新，版本号也会漂移。
	//
	// 回调在事务提交之后、用脱离取消的 context 执行（请求被取消不该让账本少写一笔）；
	// 回调里的错误只能记日志，不能反过来影响任务状态——任务已经结束了，它不是失败。
	//
	// params 是这次任务执行用的**规范化参数**（来自它消费的那份预检记录），不是请求体
	// 原文：代理执行的就是这一份。取不到时为空切片，回调必须能处理这种情况——一份
	// 没有参数的结果依然值得写进账本（至少"它失败了"是事实）。
	// 允许为 nil（测试、或没有证书模块的部署）。
	OnJobFinished func(ctx context.Context, job Job, params json.RawMessage)
}

// Gate 是 Web 侧对预检的追加判决。
//
// 参数是代理规范化之后的动作名与参数（不是前端原样上传的那份）：门禁按代理
// 实际要执行的那份参数判断，避免“代理执行 A、门禁检查 B”。
type Gate interface {
	// Blockers 返回要追加的阻塞项。空切片表示放行。
	// 返回 error 只用于内部故障（数据库），不用于“读不到依赖卷状态”——
	// 后者是一种要如实告知的判决，不是 500。
	Blockers(ctx context.Context, action string, params json.RawMessage) ([]Notice, error)
}

// Service 生成并保存预检记录。
type Service struct {
	db     *store.DB
	agent  Agent
	logger *slog.Logger
	now    func() time.Time
	gate   Gate
	// onJobFinished 见 Options.OnJobFinished：只在任务迁移到终态时触发一次。
	onJobFinished func(ctx context.Context, job Job, params json.RawMessage)

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
		gate:     opts.Gate,
		inflight: make(map[string]int),

		onJobFinished: opts.OnJobFinished,
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

	// Web 侧门禁（P09.4）：代理不知道应用与依赖卷的关系，那半边由这里补。
	// 它只能收紧——这一点写死在下面：只有 extra 非空时才改 allowed，永远不会把它翻回 true。
	allowed := plan.Allowed
	blockers := notices(plan.Blockers)
	gated := false
	if s.gate != nil {
		extra, err := s.gate.Blockers(ctx, plan.Action, canonical)
		if err != nil {
			// 门禁自己坏了（数据库读不出依赖关系）：不做“当它没拦”的处理，
			// 否则一次内部故障就会让本该被拦的启动悄悄放行。
			return Preflight{}, fmt.Errorf("operations: 预检门禁失败: %w", err)
		}
		if len(extra) > 0 {
			gated = true
			allowed = false
			blockers = mergeNotices(blockers, extra)
		}
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
		plan.Target.StateVersion, plan.Ref, boolInt(allowed), boolInt(plan.RequiresReauth),
		boolInt(plan.RequiresPhrase), boolInt(plan.Cancellable), formatTime(createdAt), formatTime(expiresAt))
	if err != nil {
		return Preflight{}, fmt.Errorf("operations: 保存预检记录失败: %w", err)
	}

	s.logger.Info("已登记预检",
		"preflight_id", id,
		"agent_ref", plan.Ref,
		"action", plan.Action,
		"user_id", in.UserID,
		"allowed", allowed,
		"gated", gated,
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
		Allowed:            allowed,
		Blockers:           blockers,
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

// mergeNotices 把门禁追加的阻塞项接在代理给的阻塞项后面，按 (code, message) 去重。
//
// 去重不是洁癖：代理与门禁对同一件事各说一句时（比如都提到卷没挂），两句一字不差的
// 重复提示只会让人以为有两个问题。
func mergeNotices(base, extra []Notice) []Notice {
	out := make([]Notice, 0, len(base)+len(extra))
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, n := range append(append([]Notice{}, base...), extra...) {
		key := n.Code + "\x00" + n.Message
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, n)
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
