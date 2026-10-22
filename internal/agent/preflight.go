package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
)

// PreflightTTL 是预检的有效期（CONTRACTS.md 4.1：120 秒）。
//
// 期限从**代理**生成计划的那一刻算起，由代理写进计划行并返回给 Web；Web 存的是同一个
// 时间戳，不自己再加一次。两端各自计算“120 秒”只会让到期边界出现两个真值。
const PreflightTTL = 120 * time.Second

// maxPreflightRequestBytes 是 POST /internal/v1/preflights 的请求体上限：
// **最大**的动作参数上限加上 action / actor_context / JSON 转义的余量。
//
// 为什么不是默认的 64 KiB：file.write_text 的参数里带着最多 1 MiB 的文件内容，
// 传输层按 64 KiB 截断会让这条动作永远无法预检。逐动作的上限仍然存在
// （Action.ParamsLimitBytes），由 DecodeParamsLimit 在解码时执行。
const maxPreflightRequestBytes = MaxActionParamsBytes + 8<<10

// planSweepInterval 是过期预检计划的扫描间隔。
const planSweepInterval = 30 * time.Second

// PlanStore 是代理需要的预检计划存储能力。
//
// Get 是 P03.2 消费预检的读路径：提交时要把计划读回来复核绑定关系与一次性。
type PlanStore interface {
	Insert(ctx context.Context, plan agentstore.PreflightPlan) error
	Get(ctx context.Context, id string) (agentstore.PreflightPlan, error)
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}

// preflightRequest 是 POST /internal/v1/preflights 的请求体（CONTRACTS.md 第 15 节）。
//
// 只有动作名、参数和操作者上下文：没有命令字符串、没有可选的“跳过检查”开关，
// 也没有让 Web 自带计划的字段——计划只能由代理生成。
type preflightRequest struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params"`
	Actor  ActorContext    `json:"actor_context"`
}

// preflightTargetResponse 是计划里目标的部分。state_version 属于内部协议：
// 公开 DTO 只暴露 display_name 与 identity_fingerprint，状态版本用于执行前复核。
type preflightTargetResponse struct {
	DisplayName         string `json:"display_name"`
	IdentityFingerprint string `json:"identity_fingerprint"`
	StateVersion        string `json:"state_version"`
}

// preflightResponse 是计划的完整内容（内部协议）。
//
// confirmation_phrase 只在需要短语时出现，且明文只出现这一次：代理保存的是它的哈希。
type preflightResponse struct {
	PreflightRef       string                  `json:"preflight_ref"`
	Action             string                  `json:"action"`
	NormalizedParams   json.RawMessage         `json:"normalized_params"`
	Target             preflightTargetResponse `json:"target"`
	ExpiresAt          string                  `json:"expires_at"`
	Allowed            bool                    `json:"allowed"`
	Blockers           []Notice                `json:"blockers"`
	Warnings           []Notice                `json:"warnings"`
	Impacts            []Impact                `json:"impacts"`
	RequiresReauth     bool                    `json:"requires_reauth"`
	RequiresPhrase     bool                    `json:"requires_phrase"`
	ConfirmationPhrase string                  `json:"confirmation_phrase,omitempty"`
	Cancellable        bool                    `json:"cancellable"`
	Locks              []string                `json:"locks"`
}

// handlePreflights 生成一份预检计划并落库。
//
// 这一层**不做任何系统写操作**：只解码参数、调用动作的预检器、保存计划并返回。
// 真正的执行在 POST /internal/v1/operations（P03.2），那一步会重新复核这里的绑定。
func (s *Server) handlePreflights(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 POST")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return
	}

	body, ok := readLimitedBody(w, r, maxPreflightRequestBytes)
	if !ok {
		return
	}
	var req preflightRequest
	// 信封按“最大的逐动作上限”解码：具体的动作还没解析出来，用默认的 64 KiB
	// 会把 file.write_text 的 1 MiB 内容挡在门外，让那条动作永远无法预检。
	if err := decodeStrictObjectLimit(body, &req, maxPreflightRequestBytes); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	if strings.TrimSpace(req.Action) == "" {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, "action 不能为空")
		return
	}
	if err := validateActorContext(req.Actor); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	action, found := s.registry.Action(req.Action)
	if !found {
		// 未登记的动作不存在“默认分支”：这是登记表的意义所在。
		s.opts.Logger.Warn("拒绝未登记动作的预检",
			"action", req.Action, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			fmt.Sprintf("未登记的动作 %q", req.Action))
		return
	}
	if action.Plan == nil {
		s.opts.Logger.Error("动作缺少预检实现", "action", req.Action, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusNotImplemented, httpx.CodeNotImplemented,
			fmt.Sprintf("动作 %q 尚未实现预检", req.Action))
		return
	}

	// 逐动作的参数上限在解码之前校验：一个 1 MiB 的 content 允许进入
	// file.write_text，但同样的体积打给 service.control 会被当场拒绝。
	if limit := action.paramsLimit(); len(req.Params) > limit {
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, httpx.CodeValidationError,
			fmt.Sprintf("动作 %q 的参数上限是 %d 字节，收到 %d 字节", req.Action, limit, len(req.Params)))
		return
	}
	params, err := action.Decode(req.Params)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	// 策略在**生成预检**时确定，而不是在提交时：随参数变化的门槛（例如 TERM 与 KILL）
	// 必须在这一刻就落到预检记录里，之后任何人都改不了它。
	policy, err := action.ResolvePolicy(params)
	if err != nil {
		if errors.Is(err, ErrParamsInvalid) {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
		s.internalError(w, r, "动作策略不合法", err)
		return
	}

	ref, err := newPlanRef()
	if err != nil {
		s.internalError(w, r, "生成预检引用失败", err)
		return
	}
	// 截断到秒：线上格式是 RFC3339（秒级），落库精度必须与下发的值一致，
	// 否则 Web 存到的到期时间会比代理库里的早几百毫秒，边界上会判出两个“真值”。
	now := s.nowUTC().Truncate(time.Second)
	expiresAt := now.Add(PreflightTTL)

	plan, err := action.Plan(r.Context(), PlanInput{
		Params:       params,
		Actor:        req.Actor,
		PreflightRef: ref,
		Now:          now,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		// 预检器返回错误属于实现失败（业务拒绝应当用 Allowed=false + Blockers 表达）。
		s.internalError(w, r, "预检计算失败", fmt.Errorf("%w: action=%s: %v", ErrPlanFailed, req.Action, err))
		return
	}
	if verr := validatePlanResult(plan); verr != nil {
		s.internalError(w, r, "预检结果不合法", fmt.Errorf("%w: action=%s: %v", ErrInvalidPlan, req.Action, verr))
		return
	}

	normalized, err := json.Marshal(plan.NormalizedParams)
	if err != nil {
		s.internalError(w, r, "规范化参数无法序列化", err)
		return
	}
	normalized, err = canonicalJSON(normalized)
	if err != nil {
		s.internalError(w, r, "规范化参数不是合法 JSON", err)
		return
	}
	paramsHash := hashHex(normalized)

	locks := normalizeLocks(plan.Locks)
	targetBytes, err := json.Marshal(preflightTargetResponse{
		DisplayName:         plan.Target.DisplayName,
		IdentityFingerprint: plan.Target.IdentityFingerprint,
		StateVersion:        plan.Target.StateVersion,
	})
	if err != nil {
		s.internalError(w, r, "目标信息无法序列化", err)
		return
	}
	blockersBytes, err := marshalList(plan.Blockers)
	if err != nil {
		s.internalError(w, r, "阻塞项无法序列化", err)
		return
	}
	warningsBytes, err := marshalList(plan.Warnings)
	if err != nil {
		s.internalError(w, r, "告警项无法序列化", err)
		return
	}
	impactsBytes, err := marshalList(plan.Impacts)
	if err != nil {
		s.internalError(w, r, "影响项无法序列化", err)
		return
	}
	locksBytes, err := json.Marshal(locks)
	if err != nil {
		s.internalError(w, r, "资源锁无法序列化", err)
		return
	}

	phrase := ""
	phraseHash := ""
	if policy.RequiresPhrase {
		phrase, err = newConfirmationPhrase(policy.PhraseVerb)
		if err != nil {
			s.internalError(w, r, "生成确认短语失败", err)
			return
		}
		phraseHash = hashHex([]byte(phrase))
	}

	record := agentstore.PreflightPlan{
		ID:                  ref,
		Action:              action.Name,
		Params:              normalized,
		ParamsHash:          paramsHash,
		Target:              targetBytes,
		IdentityFingerprint: plan.Target.IdentityFingerprint,
		StateVersion:        plan.Target.StateVersion,
		Allowed:             plan.Allowed,
		Blockers:            blockersBytes,
		Warnings:            warningsBytes,
		Impacts:             impactsBytes,
		Locks:               locksBytes,
		RequiresReauth:      policy.RequiresReauth,
		PhraseHash:          phraseHash,
		UserID:              req.Actor.UserID,
		SessionHash:         req.Actor.SessionIDHash,
		RequestID:           httpx.RequestID(r),
		CreatedAt:           now,
		ExpiresAt:           expiresAt,
	}
	if err := s.opts.Plans.Insert(r.Context(), record); err != nil {
		s.internalError(w, r, "保存预检计划失败", err)
		return
	}

	s.opts.Logger.Info("已生成预检计划",
		"action", record.Action,
		"ref", record.ID,
		"user_id", record.UserID,
		"allowed", record.Allowed,
		"blockers", len(plan.Blockers),
		"warnings", len(plan.Warnings),
		"locks", len(locks),
		"requires_reauth", record.RequiresReauth,
		"requires_phrase", record.PhraseHash != "",
		"expires_at", expiresAt.Format(time.RFC3339),
		"params_hash", paramsHash)

	httpx.WriteData(w, r, http.StatusCreated, preflightResponse{
		PreflightRef:       ref,
		Action:             record.Action,
		NormalizedParams:   normalized,
		Target:             preflightTargetResponse{DisplayName: plan.Target.DisplayName, IdentityFingerprint: plan.Target.IdentityFingerprint, StateVersion: plan.Target.StateVersion},
		ExpiresAt:          expiresAt.Format(time.RFC3339),
		Allowed:            plan.Allowed,
		Blockers:           nonNilNotices(plan.Blockers),
		Warnings:           nonNilNotices(plan.Warnings),
		Impacts:            nonNilImpacts(plan.Impacts),
		RequiresReauth:     policy.RequiresReauth,
		RequiresPhrase:     policy.RequiresPhrase,
		ConfirmationPhrase: phrase,
		Cancellable:        policy.Cancellable,
		Locks:              locks,
	})
}

// validateActorContext 校验操作者上下文。
//
// user_id 与 session_hash 是预检绑定的一半（另一半是动作、参数与目标状态）：
// 缺了它们，这份预检就不属于任何具体的人与会话，提交时也就无法判断“是不是同一个人”。
func validateActorContext(a ActorContext) error {
	if a.UserID <= 0 {
		return errors.New("actor_context.user_id 必须为正整数")
	}
	if len(a.SessionIDHash) != 64 || !isLowerHex(a.SessionIDHash) {
		return errors.New("actor_context.session_id_hash 必须是 64 位小写十六进制")
	}
	if a.ReauthAt != "" {
		if _, err := time.Parse(time.RFC3339, a.ReauthAt); err != nil {
			return errors.New("actor_context.reauth_at 必须是 RFC3339 时间")
		}
	}
	return nil
}

// validatePlanResult 校验预检器给出的结果自洽。
func validatePlanResult(plan Plan) error {
	if plan.NormalizedParams == nil {
		return errors.New("缺少规范化参数")
	}
	// 目标绑定只在**打算执行**时才要求。
	//
	// 身份指纹与状态版本存在的唯一理由是让执行端复核“我要动的还是预检时那个东西”；
	// 对一条 allowed=false 的预检，这条安全义务不存在——提交端在 `!plan.Allowed`
	// 那一步就拒了（operations.go），它根本不可能被执行。无条件要求绑定会把最普通
	// 的一种拒绝变成实现错误：真机 P07.3 验收 §13 撞到的就是它——对一个已经从拓扑里
	// 消失的分区做预检，判决层正确地给出 DEVICE_NOT_FOUND，但“消失的设备”没有任何
	// 东西可以绑定，于是这里判“预检结果不合法”，用户拿到 500 内部错误，而不是
	// “这台设备已经不在了”。（Q190）
	if plan.Allowed {
		if strings.TrimSpace(plan.Target.IdentityFingerprint) == "" {
			return errors.New("缺少目标身份指纹：预检必须绑定一个可复核的目标")
		}
		if strings.TrimSpace(plan.Target.StateVersion) == "" {
			return errors.New("缺少目标状态版本：预检必须绑定目标状态快照")
		}
	}
	if !plan.Allowed && len(plan.Blockers) == 0 {
		return errors.New("不允许执行却没有给出阻塞项")
	}
	for _, n := range plan.Blockers {
		if strings.TrimSpace(n.Code) == "" || strings.TrimSpace(n.Message) == "" {
			return errors.New("阻塞项必须同时有 code 与 message")
		}
	}
	for _, n := range plan.Warnings {
		if strings.TrimSpace(n.Code) == "" || strings.TrimSpace(n.Message) == "" {
			return errors.New("告警项必须同时有 code 与 message")
		}
	}
	for _, i := range plan.Impacts {
		if strings.TrimSpace(i.Kind) == "" || strings.TrimSpace(i.Description) == "" {
			return errors.New("影响项必须同时有 kind 与 description")
		}
	}
	return nil
}

// normalizeLocks 排序并去重资源锁。
//
// 执行时多个资源按规范字符串顺序获取以避免死锁（CONTRACTS.md 第 15 节）；
// 顺序在这里定下来，计划与执行看到的就是同一份列表。
func normalizeLocks(locks []string) []string {
	seen := make(map[string]struct{}, len(locks))
	out := make([]string, 0, len(locks))
	for _, l := range locks {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// canonicalJSON 把 JSON 压成无多余空白的规范形式。
//
// json.Marshal 对 map 会按键排序，对结构体按字段顺序输出，因此“解到 any 再编码”
// 得到的字节是稳定的——参数哈希才能跨进程、跨重启复现。
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

func isLowerHex(s string) bool {
	if strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// newPlanRef 生成计划引用（pf_ + 16 字节随机值的十六进制）。
func newPlanRef() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agent: 生成预检引用失败: %w", err)
	}
	return agentstore.PlanRefPrefix + hex.EncodeToString(b[:]), nil
}

// phraseAlphabet 去掉了容易看错的 I/O/0/1：确认短语要被人手抄一遍。
const phraseAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// phraseCodeLength 是随机码长度：6 位 32 进制约 30 位熵，配合一次性与 120 秒期限足够。
const phraseCodeLength = 6

// newConfirmationPhrase 生成确认短语，形如 "FORMAT K7M2QX"。
//
// 短语由服务端（代理）生成，前端只能展示与回传，不能自定。它是一次性的授权凭证，
// 落库时只存哈希。
func newConfirmationPhrase(verb string) (string, error) {
	buf := make([]byte, phraseCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("agent: 生成确认短语失败: %w", err)
	}
	code := make([]byte, phraseCodeLength)
	for i, b := range buf {
		code[i] = phraseAlphabet[int(b)%len(phraseAlphabet)]
	}
	return verb + " " + string(code), nil
}

func marshalList[T any](items []T) ([]byte, error) {
	if items == nil {
		items = []T{}
	}
	return json.Marshal(items)
}

func nonNilNotices(v []Notice) []Notice {
	if v == nil {
		return []Notice{}
	}
	return v
}

func nonNilImpacts(v []Impact) []Impact {
	if v == nil {
		return []Impact{}
	}
	return v
}

// internalError 记录真实错误并返回不泄漏细节的 500。
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	s.opts.Logger.Error(msg, "error", err, "request_id", httpx.RequestID(r), "path", r.URL.Path)
	httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, msg)
}

// sweepExpiredPlans 删除过期计划并记录结果。
func (s *Server) sweepExpiredPlans(ctx context.Context, now time.Time) (int, error) {
	removed, err := s.opts.Plans.DeleteExpired(ctx, now)
	if err != nil {
		s.opts.Logger.Error("清理过期预检计划失败", "error", err)
		return removed, err
	}
	if removed > 0 {
		s.opts.Logger.Info("已清理过期预检计划", "count", removed, "at", now.UTC().Format(time.RFC3339))
	}
	return removed, nil
}

// SweepExpiredPlans 立即扫描一次（供运维与测试调用）。
func (s *Server) SweepExpiredPlans(ctx context.Context) (int, error) {
	return s.sweepExpiredPlans(ctx, time.Now())
}

// runPlanSweeper 周期性清理过期计划，直到 ctx 结束。
func (s *Server) runPlanSweeper(ctx context.Context) {
	ticker := time.NewTicker(planSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_, _ = s.sweepExpiredPlans(ctx, now)
		}
	}
}
