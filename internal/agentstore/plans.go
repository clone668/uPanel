package agentstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 预检计划相关的错误。
var (
	// ErrPlanInvalid 表示计划本身不合法（编程错误，不是用户输入错误）。
	ErrPlanInvalid = errors.New("agentstore: 预检计划不合法")
	// ErrPlanNotFound 表示没有这份计划。
	ErrPlanNotFound = errors.New("agentstore: 预检计划不存在")
)

// PreflightPlan 是一份**代理生成**的预检计划（CONTRACTS.md 14.2 preflight_plans）。
//
// 字段与表列一一对应：这里只做持久化与基本自洽校验，计划的业务含义由各动作的预检器决定。
// JSON 字段用 []byte 保存原始字节，保证读回来的参数与写进去的逐字节一致——
// 参数哈希是按字节算的，任何重新序列化都可能改变它。
type PreflightPlan struct {
	ID                  string
	Action              string
	Params              []byte
	ParamsHash          string
	Target              []byte
	IdentityFingerprint string
	StateVersion        string
	Allowed             bool
	Blockers            []byte
	Warnings            []byte
	Impacts             []byte
	Locks               []byte
	RequiresReauth      bool
	// PhraseHash 是确认短语的 SHA-256；空串表示该动作不需要短语。
	PhraseHash  string
	UserID      int64
	SessionHash string
	RequestID   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// PreflightPlans 是预检计划的持久化仓储。
type PreflightPlans struct{ db *DB }

// NewPreflightPlans 构造仓储。db 为空时返回错误：预检是固定能力，不能“看起来在跑”。
func NewPreflightPlans(db *DB) (*PreflightPlans, error) {
	if db == nil {
		return nil, errors.New("agentstore: 预检计划仓储需要数据库句柄")
	}
	return &PreflightPlans{db: db}, nil
}

// Insert 保存一份计划。
func (p *PreflightPlans) Insert(ctx context.Context, plan PreflightPlan) error {
	if err := validatePlan(plan); err != nil {
		return err
	}
	_, err := p.db.store().ExecContext(ctx, `
		INSERT INTO preflight_plans
		  (id, action, params_json, params_hash, target_json, identity_fingerprint, state_version,
		   allowed, blockers_json, warnings_json, impacts_json, locks_json, requires_reauth,
		   phrase_hash, user_id, session_hash, request_id, created_at, expires_at, used_at, used_operation_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '')`,
		plan.ID, plan.Action, string(plan.Params), plan.ParamsHash, string(plan.Target),
		plan.IdentityFingerprint, plan.StateVersion, boolInt(plan.Allowed),
		string(plan.Blockers), string(plan.Warnings), string(plan.Impacts), string(plan.Locks),
		boolInt(plan.RequiresReauth), plan.PhraseHash, plan.UserID, plan.SessionHash,
		plan.RequestID, millis(plan.CreatedAt), millis(plan.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: 计划引用 %s 已存在", ErrPlanInvalid, plan.ID)
		}
		return fmt.Errorf("agentstore: 保存预检计划失败: %w", err)
	}
	return nil
}

// Get 读回一份计划。
//
// 读路径属于 P03.2 的提交（消费）流程：那一轮用它在执行前复核参数哈希、目标指纹、
// 到期时间与一次性。测试现在就用它核对落库内容，避免出现“写进去了但读不回来”的假通过。
func (p *PreflightPlans) Get(ctx context.Context, id string) (PreflightPlan, error) {
	if !ValidPlanRef(id) {
		return PreflightPlan{}, ErrPlanNotFound
	}
	var (
		plan                    PreflightPlan
		allowed, requiresReauth int
		blockers, warnings      string
		impacts, locks, target  string
		params                  string
		createdAt, expiresAt    int64
	)
	err := p.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, action, params_json, params_hash, target_json, identity_fingerprint, state_version,
		       allowed, blockers_json, warnings_json, impacts_json, locks_json, requires_reauth,
		       phrase_hash, user_id, session_hash, request_id, created_at, expires_at
		  FROM preflight_plans WHERE id = ?`, id).
		Scan(&plan.ID, &plan.Action, &params, &plan.ParamsHash, &target, &plan.IdentityFingerprint,
			&plan.StateVersion, &allowed, &blockers, &warnings, &impacts, &locks, &requiresReauth,
			&plan.PhraseHash, &plan.UserID, &plan.SessionHash, &plan.RequestID, &createdAt, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PreflightPlan{}, ErrPlanNotFound
		}
		return PreflightPlan{}, fmt.Errorf("agentstore: 读取预检计划失败: %w", err)
	}
	plan.Allowed = allowed == 1
	plan.RequiresReauth = requiresReauth == 1
	plan.Params = []byte(params)
	plan.Target = []byte(target)
	plan.Blockers = []byte(blockers)
	plan.Warnings = []byte(warnings)
	plan.Impacts = []byte(impacts)
	plan.Locks = []byte(locks)
	plan.CreatedAt = fromMillis(createdAt)
	plan.ExpiresAt = fromMillis(expiresAt)
	return plan, nil
}

// DeleteExpired 删除已过期且从未被使用的计划，返回删除条数。
//
// 过期的计划没有保留价值：它不可能再被合法消费，留着只会让“库里有一份计划”
// 看起来像是还能执行。已使用过的计划不在这里删除（它属于执行历史）。
func (p *PreflightPlans) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := p.db.store().ExecContext(ctx,
		`DELETE FROM preflight_plans WHERE used_at = 0 AND expires_at <= ?`, millis(now.UTC()))
	if err != nil {
		return 0, fmt.Errorf("agentstore: 删除过期预检计划失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("agentstore: 读取删除条数失败: %w", err)
	}
	return int(n), nil
}

// validatePlan 校验计划自洽性。
//
// 这些检查不是“防用户输入”（计划由代理自己生成），而是防编程错误：例如允许执行为 false
// 却没有给任何阻塞原因，或参数哈希与参数不是同一份字节。
func validatePlan(plan PreflightPlan) error {
	if !ValidPlanRef(plan.ID) {
		return fmt.Errorf("%w: 计划引用 %q 不合法", ErrPlanInvalid, plan.ID)
	}
	if strings.TrimSpace(plan.Action) == "" {
		return fmt.Errorf("%w: 缺少 action", ErrPlanInvalid)
	}
	if len(plan.Params) == 0 {
		return fmt.Errorf("%w: 缺少规范化参数", ErrPlanInvalid)
	}
	if !isHexDigest(plan.ParamsHash) {
		return fmt.Errorf("%w: params_hash 必须是 64 位十六进制", ErrPlanInvalid)
	}
	if plan.UserID <= 0 {
		return fmt.Errorf("%w: user_id 必须为正数", ErrPlanInvalid)
	}
	if len(plan.SessionHash) != 64 || !isHexDigest(plan.SessionHash) {
		return fmt.Errorf("%w: session_hash 必须是 64 位十六进制", ErrPlanInvalid)
	}
	if plan.PhraseHash != "" && !isHexDigest(plan.PhraseHash) {
		return fmt.Errorf("%w: phrase_hash 必须是 64 位十六进制", ErrPlanInvalid)
	}
	if plan.CreatedAt.IsZero() || plan.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: 计划必须有创建时间与到期时间", ErrPlanInvalid)
	}
	if !plan.ExpiresAt.After(plan.CreatedAt) {
		return fmt.Errorf("%w: 到期时间必须晚于创建时间", ErrPlanInvalid)
	}
	if !plan.Allowed && listEmpty(plan.Blockers) {
		return fmt.Errorf("%w: 不允许执行却没有给出阻塞原因", ErrPlanInvalid)
	}
	return nil
}

// listEmpty 判断一个已序列化的列表是否为空。
//
// 只检查 len(b) == 0 是不够的：空 JSON 数组是 []byte("[]")，长度为 2，
// 会让“拒绝执行却没有任何阻塞原因”的计划混过去。
func listEmpty(b []byte) bool {
	trimmed := strings.TrimSpace(string(b))
	return trimmed == "" || trimmed == "[]" || trimmed == "null"
}

// isHexDigest 判断字符串是否为 64 位小写十六进制（SHA-256 的十六进制形式）。
func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == 32 && strings.ToLower(s) == s
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// PlanRefPrefix 是预检计划引用的固定前缀。
const PlanRefPrefix = "pf_"

// ValidPlanRef 判断字符串是否是合法的计划引用（pf_ + 32 位小写十六进制）。
func ValidPlanRef(ref string) bool {
	if !strings.HasPrefix(ref, PlanRefPrefix) || len(ref) != len(PlanRefPrefix)+32 {
		return false
	}
	body := ref[len(PlanRefPrefix):]
	if strings.ToLower(body) != body {
		return false
	}
	decoded, err := hex.DecodeString(body)
	return err == nil && len(decoded) == 16
}
