package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Registry 是代理的**固定登记表**：动作与结构化只读查询都必须先在这里登记，
// 未登记的名称一律拒绝（CONTRACTS.md 第 5、15 节）。不存在“默认分支接受未知动作”，
// 也不存在 query=command 这类字符串入口。
//
// P02.3 只提供机制和只读查询通道：生产登记表里没有任何动作（第一个动作要到 P03 的
// 预检/任务链路才接入），查询表同样为空，因为系统采集属于 P04。capabilities 如实
// 返回空列表，而不是塞占位名字。
type Registry struct {
	actions map[string]Action
	queries map[string]Query
}

// Action 是一个已登记的系统写动作的定义。
//
// 动作的 HTTP 入口是 /internal/v1/preflights（P03.1）与 /internal/v1/operations（P03.2）。
// 登记项分两层：
//   - Decode 是必需的：任何动作都必须能把自己的参数严格解码成 DTO；
//   - Plan 是本动作的预检器。为 nil 表示该动作还没有预检实现，预检接口会明确拒绝
//     （501 NOT_IMPLEMENTED），而不是给出一份看起来合法的空计划。
type Action struct {
	// Name 必须是形如 storage.volume.mount 的固定名称。
	Name string
	// Decode 把请求参数解码成该动作自己的 DTO，必须拒绝未知字段。
	Decode func(raw json.RawMessage) (any, error)
	// Plan 计算预检计划：规范化参数、目标身份指纹与状态版本、影响与阻塞项。
	// 它**不得**写系统：预检不做任何磁盘、服务或软件变更（CONTRACTS.md 4.1）。
	Plan func(ctx context.Context, in PlanInput) (Plan, error)
	// Execute 执行这个动作。为 nil 表示该动作还不能执行，提交任务时明确拒绝
	// （501 NOT_IMPLEMENTED），而不是接受一个永远不会跑的任务。
	//
	// 执行器收到的是**预检里的规范化参数**：提交那一刻不再接受新参数（CONTRACTS.md 4.2）。
	Execute func(ctx context.Context, in ExecuteInput) (Result, error)
	// Policy 是动作策略：再认证、确认短语、可取消。
	Policy ActionPolicy
}

// ActionPolicy 是动作在预检 DTO 里对外声明的策略。
//
// 这些值由服务端（动作登记表）决定，**不能**由前端决定：前端只负责展示，
// 是否要求再认证、是否要求确认短语都由这里给出。
type ActionPolicy struct {
	// RequiresReauth 表示提交该动作的任务时需要 5 分钟内的密码再认证。
	RequiresReauth bool
	// RequiresPhrase 表示必须输入预检生成的确认短语才能提交。
	RequiresPhrase bool
	// PhraseVerb 是确认短语的首个大写词（例如 FORMAT），RequiresPhrase 为 true 时必填。
	PhraseVerb string
	// Cancellable 表示任务开始后允许安全取消。前端不能自行展示无效的取消按钮，
	// 因此这个值随预检一起下发。
	Cancellable bool
}

// ExecuteInput 是执行器的输入。
//
// 除 Params 外都是**上下文**，不是可执行参数：执行器拿不到新的 action/params，
// 也只能通过 Progress 回报进度，不能自行改写阶段。
type ExecuteInput struct {
	// Params 是预检落库的规范化参数解出的 DTO。
	Params any
	// Actor 是提交者上下文（审计用）。
	Actor ActorContext
	// OperationID 是本次操作的稳定标识：日志与结果都按它对齐。
	OperationID string
	// PreflightRef 是被消费掉的预检引用（只用于记录）。
	PreflightRef string
	// Now 是执行开始时间（UTC）。
	Now time.Time
	// Progress 回报一个步骤；可空。字段值只允许可序列化为 JSON 的简单值。
	Progress func(step string, fields map[string]any)
	// Cancel 在收到安全取消请求时关闭。执行器必须在可中断点检查它并尽快返回
	// ErrCanceled：不可取消的动作（Policy.Cancellable=false）不会收到关闭信号。
	Cancel <-chan struct{}
}

// Result 是执行结果（CONTRACTS.md 4.3：结果至少包括 outcome、verified_at、
// observed_state、recovery_hint）。
type Result struct {
	// Outcome 是闭集：verified、accepted、rolled_back、partial、unknown。
	// 只有真正核验过实际结果才能用 verified；只是“命令返回 0”不是核验。
	Outcome string
	// VerifiedAt 是核验时间；Outcome 为 verified 时必填。
	VerifiedAt time.Time
	// ObservedState 是核验时读回的实际状态（键值必须是简单 JSON 值）。
	ObservedState map[string]any
	// RecoveryHint 是失败或部分完成时的恢复提示（给人看的一句话）。
	RecoveryHint string
	// Data 是动作自有的附加信息（例如创建的资源标识）。
	Data map[string]any
}

// 结果 outcome 的闭集。
const (
	OutcomeVerified   = "verified"
	OutcomeAccepted   = "accepted"
	OutcomeRolledBack = "rolled_back"
	OutcomePartial    = "partial"
	OutcomeUnknown    = "unknown"
)

// ValidOutcome 判断 outcome 是否在闭集内。
func ValidOutcome(outcome string) bool {
	switch outcome {
	case OutcomeVerified, OutcomeAccepted, OutcomeRolledBack, OutcomePartial, OutcomeUnknown:
		return true
	}
	return false
}

// 代理信任 Web 已完成用户登录认证，但仍独立实施系统安全边界；这份上下文只用于
// “这份预检属于谁、哪个会话”，不用于授权。
// ActorContext 是操作者上下文（CONTRACTS.md 第 15 节）：供审计关联与预检绑定。
type ActorContext struct {
	UserID int64 `json:"user_id"`
	// SessionIDHash 是会话令牌的 SHA-256 十六进制值：会话轮换后它与旧值不同，
	// 因此绑定到它的预检会在重新登录后自然失效。
	SessionIDHash string `json:"session_id_hash"`
	RequestID     string `json:"request_id"`
	// ReauthAt 是最近一次密码再认证的时间（RFC3339），空串表示窗口内没有再认证。
	ReauthAt string `json:"reauth_at"`
}

// PlanInput 是预检器的输入。
type PlanInput struct {
	// Params 是 Decode 解出的动作参数 DTO（类型由各动作自己定义）。
	Params any
	// Actor 是操作者上下文。
	Actor ActorContext
	// PreflightRef 是本次预检的引用，计划可以在提示文本里引用它。
	PreflightRef string
	// Now 是代理当前时间（UTC）。
	Now time.Time
	// ExpiresAt 是本计划的到期时间（Now + PreflightTTL）。
	ExpiresAt time.Time
}

// Plan 是动作的预检结果。Allowed 为 false 时 Blockers 必须非空——否则“不允许”
// 就没有任何可展示的原因，等于把拒绝变成谜语。
type Plan struct {
	// NormalizedParams 是规范化后的参数：预检绑定它、任务按它执行，
	// 提交时不再接受新的 action/params（CONTRACTS.md 4.2）。
	NormalizedParams any
	// Target 描述操作对象与它的状态版本。
	Target Target
	// Allowed 表示在当前状态下是否允许执行。
	Allowed bool
	// Blockers 是阻止执行的原因（Allowed=false 时非空）。
	Blockers []Notice
	// Warnings 是不阻止执行但必须告知的风险。
	Warnings []Notice
	// Impacts 是这次操作会造成的后果（例如数据丢失）。
	Impacts []Impact
	// Locks 是执行时需要持有的资源锁（规范字符串，例如 disk:dev_xxx）。
	Locks []string
}

// Target 是预检绑定的目标：展示名、身份指纹与状态版本。
type Target struct {
	// DisplayName 是给管理员看的目标名称（例如 “/dev/sdb（ACHI-1234）”）。
	DisplayName string
	// IdentityFingerprint 是设备/对象身份的稳定指纹，执行前必须重新核对。
	IdentityFingerprint string
	// StateVersion 是目标状态的快照版本：预检到执行之间状态变了，
	// 旧预检必须失效（PREFLIGHT_STALE）。
	StateVersion string
}

// Notice 是阻塞项/告警项。
type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Impact 是操作影响。
type Impact struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// Query 是一个已登记的结构化只读查询。
type Query struct {
	// Name 必须是形如 system.summary 的固定名称。
	Name string
	// Decode 把参数解码成该查询自己的 DTO，必须拒绝未知字段。
	Decode func(raw json.RawMessage) (any, error)
	// Run 执行查询。只读语义由实现保证：查询不得启动外部命令做写操作。
	Run func(ctx context.Context, params any) (any, error)
}

// 登记名允许的形态：小写字母开头、至少两段、段内允许数字与下划线。
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*)+$`)

// 确认短语的首词：大写，2—16 位，允许数字（FORMAT、DESTROY、RESTORE 等）。
var phraseVerbPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,15}$`)

// MaxParamsBytes 是单个动作/查询参数的上限（CONTRACTS.md 第 15 节：
// “请求体最大值按动作独立限制”）。
const MaxParamsBytes = 64 << 10

var (
	// ErrInvalidName 表示登记名不符合固定命名规则。
	ErrInvalidName = errors.New("agent: 登记名不合法")
	// ErrDuplicateName 表示重复登记。
	ErrDuplicateName = errors.New("agent: 重复登记")
	// ErrNilHandler 表示登记项缺少解码或执行函数。
	ErrNilHandler = errors.New("agent: 登记项缺少实现")
	// ErrParamsTooLarge 表示参数超过上限。
	ErrParamsTooLarge = errors.New("agent: 参数超过上限")
	// ErrParamsInvalid 表示参数不是合法 JSON 或含未知字段/尾部内容。
	ErrParamsInvalid = errors.New("agent: 参数不合法")
	// ErrUnknownAction 表示没有登记过该动作。
	ErrUnknownAction = errors.New("agent: 未登记的动作")
	// ErrUnknownQuery 表示没有登记过该查询。
	ErrUnknownQuery = errors.New("agent: 未登记的查询")
	// ErrNoPlanner 表示动作已登记但没有预检实现（P03.1 起预检是动作的必需能力）。
	ErrNoPlanner = errors.New("agent: 动作缺少预检实现")
	// ErrInvalidPlan 表示预检器返回的计划自相矛盾，属于编程错误。
	ErrInvalidPlan = errors.New("agent: 预检计划不合法")
	// ErrPlanFailed 表示预检器执行失败（不是业务拒绝，业务拒绝用 Allowed=false + Blockers）。
	ErrPlanFailed = errors.New("agent: 预检执行失败")
	// ErrNoExecutor 表示动作已登记但没有执行器（P03.2 起执行是动作的必需能力）。
	ErrNoExecutor = errors.New("agent: 动作缺少执行实现")
	// ErrExecuteFailed 表示执行器返回了失败（动作自己的错误会包在它里面）。
	ErrExecuteFailed = errors.New("agent: 动作执行失败")
	// ErrCanceled 表示执行器在可中断点收到了安全取消请求。
	ErrCanceled = errors.New("agent: 操作已取消")
	// ErrInvalidResult 表示执行器返回的结果自相矛盾，属于编程错误。
	ErrInvalidResult = errors.New("agent: 执行结果不合法")
)

// NewRegistry 返回空的登记表。生产登记表由 NewProductionRegistry 构造。
func NewRegistry() *Registry {
	return &Registry{actions: map[string]Action{}, queries: map[string]Query{}}
}

// NewProductionRegistry 返回生产登记表。
//
// 当前刻意为空：P02.3 不开放任何系统动作，也没有可用的结构化读取（采集属于 P04）。
// 空表不是“占位实现”——它让 capabilities 与真实能力一致，并让未登记名称的拒绝路径
// 在生产里天然生效。
func NewProductionRegistry() *Registry { return NewRegistry() }

// RegisterAction 登记一个动作。
func (r *Registry) RegisterAction(a Action) error {
	if err := validateName(a.Name); err != nil {
		return err
	}
	if a.Decode == nil {
		return fmt.Errorf("%w: 动作 %s", ErrNilHandler, a.Name)
	}
	if a.Policy.RequiresPhrase && !phraseVerbPattern.MatchString(a.Policy.PhraseVerb) {
		return fmt.Errorf("%w: 动作 %s 要求确认短语，但 PhraseVerb %q 不合法（应为 2—16 位大写字母或数字）",
			ErrNilHandler, a.Name, a.Policy.PhraseVerb)
	}
	if !a.Policy.RequiresPhrase && a.Policy.PhraseVerb != "" {
		return fmt.Errorf("%w: 动作 %s 不要求确认短语，却登记了 PhraseVerb %q",
			ErrNilHandler, a.Name, a.Policy.PhraseVerb)
	}
	if _, ok := r.actions[a.Name]; ok {
		return fmt.Errorf("%w: 动作 %s", ErrDuplicateName, a.Name)
	}
	r.actions[a.Name] = a
	return nil
}

// RegisterQuery 登记一个结构化只读查询。
func (r *Registry) RegisterQuery(q Query) error {
	if err := validateName(q.Name); err != nil {
		return err
	}
	if q.Decode == nil || q.Run == nil {
		return fmt.Errorf("%w: 查询 %s", ErrNilHandler, q.Name)
	}
	if _, ok := r.queries[q.Name]; ok {
		return fmt.Errorf("%w: 查询 %s", ErrDuplicateName, q.Name)
	}
	r.queries[q.Name] = q
	return nil
}

// Query 查找已登记的查询。
func (r *Registry) Query(name string) (Query, bool) {
	q, ok := r.queries[name]
	return q, ok
}

// Action 查找已登记的动作。
func (r *Registry) Action(name string) (Action, bool) {
	a, ok := r.actions[name]
	return a, ok
}

// ActionNames 返回按字典序排列的动作名（供 capabilities 使用，输出稳定）。
func (r *Registry) ActionNames() []string { return sortedKeys(r.actions) }

// QueryNames 返回按字典序排列的查询名。
func (r *Registry) QueryNames() []string { return sortedKeys(r.queries) }

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: %q（应为小写点分名称，例如 system.summary）", ErrInvalidName, name)
	}
	return nil
}

// validateResult 校验执行器给出的结果自洽。
//
// 这里不判断“动作是否真的成功”——那是执行器与验证器的责任。这里只拦自相矛盾的返回：
// outcome 不在闭集、verified 却没有核验时间。放过去会让任务中心显示一个没有证据的“已成功”。
func validateResult(res Result) error {
	if !ValidOutcome(res.Outcome) {
		return fmt.Errorf("%w: outcome %q 不在闭集内", ErrInvalidResult, res.Outcome)
	}
	if res.Outcome == OutcomeVerified && res.VerifiedAt.IsZero() {
		return fmt.Errorf("%w: outcome=verified 必须给出 verified_at", ErrInvalidResult)
	}
	for k := range res.ObservedState {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: observed_state 含空键", ErrInvalidResult)
		}
	}
	return nil
}

// DecodeParams 把原始 JSON 参数严格解码到 dst。
//
// 规则（与公开 API 一致，避免出现两套宽容度）：
//   - 参数为空（缺省）时按空对象处理；
//   - 超过 MaxParamsBytes 直接拒绝；
//   - 未知字段直接拒绝，不静默忽略；
//   - 首个 JSON 值之后还有内容也拒绝。
func DecodeParams(raw json.RawMessage, dst any) error {
	if len(raw) > MaxParamsBytes {
		return fmt.Errorf("%w: %d 字节，上限 %d", ErrParamsTooLarge, len(raw), MaxParamsBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", ErrParamsInvalid, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: 首个 JSON 对象之后仍有内容", ErrParamsInvalid)
	}
	return nil
}
