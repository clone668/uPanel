package certificates

// 任务结果 → 证书记录（P10.3）。
//
// 证书事实的**唯一**权威是代理：它才读得到 /etc/letsencrypt 与 /etc/ssl/upanel。
// 面板这边的做法是：动作执行完、任务进入终态时，把代理报告里的 observed_state
// 抄进 certificates 表。于是面板库里那一行的每一个字段都有出处
// （"代理在 X 时刻读回来的"），而不是"面板根据参数猜的"。
//
// 为什么不在预检时就建行：一条证书记录的含义是**这张证书存在**，而预检的回答是
// "它也许可以存在"。所以第一次签发失败不会留下证书记录——那不是遗漏，是实话。
// 失败的痕迹在任务表、任务事件与审计里；而"同一域名一小时内不许再试"这条限流
// 用的是任务表，不是这张表（见 operations.Service.RecentAttempts），所以少了这一行
// 也不会让限流失效。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"upanel/internal/tlscert"
)

// JobFact 是一条已结束的证书任务。
//
// 它刻意不是 operations.Job：certificates 不应该为了一个回调去依赖任务流水线的
// 整套类型。接口层做这一层转换，多写六行，换来两个包之间只有一条单向的事实流。
type JobFact struct {
	ID     string
	Action string
	// Status 是公开状态闭集里的终态（succeeded / failed / canceled / interrupted）。
	Status string
	// Result 是代理报告（outcome / observed_state / data / recovery_hint）。
	Result json.RawMessage
	// Error 是失败时的错误对象（code / message）。
	Error json.RawMessage
}

// jobResult 是代理报告里本文件用得到的部分。
type jobResult struct {
	Outcome       string         `json:"outcome"`
	ObservedState map[string]any `json:"observed_state"`
	Data          map[string]any `json:"data"`
	RecoveryHint  string         `json:"recovery_hint"`
}

// JobApplier 把证书任务的终态写进证书记录表。
//
// 它是线程安全的（Service 自己无状态，只是每次调用一次库），因此可以被对账循环与
// 请求路径同时调用。
type JobApplier struct {
	svc    *Service
	logger *slog.Logger
}

// NewJobApplier 构造回调。svc 与 logger 都必须非空。
func NewJobApplier(svc *Service, logger *slog.Logger) (*JobApplier, error) {
	if svc == nil {
		return nil, errors.New("certificates: 账本需要证书服务")
	}
	if logger == nil {
		return nil, errors.New("certificates: 账本需要 Logger")
	}
	return &JobApplier{svc: svc, logger: logger}, nil
}

// Apply 处理一条刚结束的任务。不是证书动作时什么也不做（返回 nil）。
//
// 返回值只用于日志：账本写不进去不会让任务变成失败，但也绝不能静默——所以在
// 这里返回错误，由调用方（operations 的回调桥）打成 ERROR。
func (a *JobApplier) Apply(ctx context.Context, job JobFact, params json.RawMessage) error {
	switch job.Action {
	case ActionImport:
		return a.applyImport(ctx, job, params)
	case ActionIssue, ActionRenew:
		return a.applyIssue(ctx, job, params)
	default:
		return nil
	}
}

// applyImport 处理手动导入（source=manual）。
func (a *JobApplier) applyImport(ctx context.Context, job JobFact, params json.RawMessage) error {
	var p ImportParams
	if err := decodeParams(params, &p); err != nil {
		return fmt.Errorf("certificates: 导入任务 %s 的参数无法解析: %w", job.ID, err)
	}
	normalized, err := p.Normalize()
	if err != nil {
		return fmt.Errorf("certificates: 导入任务 %s 的参数不合法: %w", job.ID, err)
	}

	outcome, observed, result, err := decodeResult(job)
	if err != nil {
		return fmt.Errorf("certificates: 导入任务 %s 的报告无法解析: %w", job.ID, err)
	}
	if job.Status == "succeeded" && outcome == OutcomeVerified {
		info, err := infoFromObserved(observed)
		if err != nil {
			return fmt.Errorf("certificates: 导入任务 %s 的报告缺字段: %w", job.ID, err)
		}
		return a.upsert(ctx, upsertInput{
			ID:           normalized.CertificateID,
			Domain:       normalized.Domain,
			Info:         info,
			Source:       SourceManual,
			KeySecretRef: normalized.KeySecretRef,
			AttemptAt:    attemptAt(observed),
		})
	}
	return a.recordFailure(ctx, normalized.CertificateID, normalized.Domain,
		attemptAt(observed), failureText(job, result))
}

// applyIssue 处理签发与续期（source=certbot）。
func (a *JobApplier) applyIssue(ctx context.Context, job JobFact, params json.RawMessage) error {
	var p IssueParams
	if err := decodeParams(params, &p); err != nil {
		return fmt.Errorf("certificates: 签发任务 %s 的参数无法解析: %w", job.ID, err)
	}
	normalized, err := p.Normalize()
	if err != nil {
		return fmt.Errorf("certificates: 签发任务 %s 的参数不合法: %w", job.ID, err)
	}
	// 定时续期的参数里没有 crt_ id（触发发生在 systemd 里，那一刻没有面板库可问），
	// 只有域名。按域名把记录对出来——这就是"续的是哪张证书"的唯一答案。
	if normalized.CertificateID == "" {
		rec, gerr := a.svc.GetByDomain(ctx, normalized.Domain)
		if gerr != nil {
			if errors.Is(gerr, ErrNotFound) {
				return fmt.Errorf("certificates: 续期任务 %s 按域名 %s 在账本里找不到记录"+
					"（定时续期只按域名对账：证书记录被删掉、或这条计划指向了一张面板不知道的证书）",
					job.ID, normalized.Domain)
			}
			return fmt.Errorf("certificates: 续期任务 %s 按域名查记录失败: %w", job.ID, gerr)
		}
		normalized.CertificateID = rec.ID
		a.logger.Info("定时续期已按域名对上证书记录",
			"job_id", job.ID, "certificate_id", rec.ID, "domain", normalized.Domain)
	}

	outcome, observed, result, err := decodeResult(job)
	if err != nil {
		return fmt.Errorf("certificates: 签发任务 %s 的报告无法解析: %w", job.ID, err)
	}
	if job.Status == "succeeded" && outcome == OutcomeVerified {
		info, err := infoFromObserved(observed)
		if err != nil {
			return fmt.Errorf("certificates: 签发任务 %s 的报告缺字段: %w", job.ID, err)
		}
		return a.upsert(ctx, upsertInput{
			ID:        normalized.CertificateID,
			Domain:    normalized.Domain,
			Info:      info,
			Source:    SourceCertbot,
			ACMEEmail: normalized.Email,
			AttemptAt: attemptAt(observed),
		})
	}
	return a.recordFailure(ctx, normalized.CertificateID, normalized.Domain,
		attemptAt(observed), failureText(job, result))
}

// upsertInput 是一次"事实落库"的输入。
type upsertInput struct {
	ID           string
	Domain       string
	Info         tlscert.Info
	Source       Source
	KeySecretRef string
	ACMEEmail    string
	AttemptAt    time.Time
}

// upsert 让库里的这条记录与代理刚报告的事实一致。
//
// 两条路径，理由与 Renew 的注释相同：**记录 id 不变**，站点绑定、审计与告警才能
// 一路跟着走。重新导入（同一个域名换了一张手动证书）与 certbot 续期都是这件事。
func (a *JobApplier) upsert(ctx context.Context, in upsertInput) error {
	current, err := a.svc.Get(ctx, in.ID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("certificates: 读取证书记录失败: %w", err)
		}
		if _, cerr := a.svc.Create(ctx, CreateInput{
			ID:           in.ID,
			Domain:       in.Domain,
			Info:         in.Info,
			Source:       in.Source,
			KeySecretRef: in.KeySecretRef,
			ACMEEmail:    in.ACMEEmail,
			AttemptAt:    in.AttemptAt,
		}); cerr != nil {
			// 域名唯一索引撞上了一条**别 id** 的记录：那是"这个域名已经有记录，
			// 但这次任务用的是另一个标识"，正常路径下不该发生（展开时复用同域名的
			// 现有 id）。如实报错，不悄悄合并——合并会改掉站点绑定指向的 id。
			return fmt.Errorf("certificates: 创建证书记录失败: %w", cerr)
		}
		a.logger.Info("证书记录已创建", "certificate_id", in.ID, "domain", in.Domain,
			"source", string(in.Source), "staging", in.Info.Staging)
		return nil
	}
	if _, err := a.svc.Renew(ctx, RenewInput{
		ID:           current.ID,
		Info:         in.Info,
		Source:       in.Source,
		KeySecretRef: in.KeySecretRef,
		ACMEEmail:    in.ACMEEmail,
		AttemptAt:    in.AttemptAt,
	}); err != nil {
		return fmt.Errorf("certificates: 更新证书记录失败: %w", err)
	}
	a.logger.Info("证书记录已更新", "certificate_id", current.ID, "domain", current.Domain,
		"not_after", in.Info.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

// recordFailure 记下一次失败的尝试。
//
// 记录不存在时什么也不写：见文件头的理由——一条失败的首次签发不该生出一条
// "有证书"的记录。限流不靠这张表。
func (a *JobApplier) recordFailure(ctx context.Context, id, domain string, at time.Time, failure string) error {
	if !ValidID(id) {
		return nil
	}
	if _, err := a.svc.Get(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("certificates: 读取证书记录失败: %w", err)
	}
	if err := a.svc.RecordAttempt(ctx, id, at, failure); err != nil {
		return fmt.Errorf("certificates: 记录证书尝试失败: %w", err)
	}
	a.logger.Warn("证书任务未成功，已记录尝试", "certificate_id", id, "domain", domain,
		"reason", failure)
	return nil
}

// decodeParams 解析任务参数。
func decodeParams(raw json.RawMessage, out any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return errors.New("参数为空")
	}
	return json.Unmarshal(raw, out)
}

// decodeResult 解析代理报告。报告为空时返回零值——失败的任务可能没有任何报告。
//
// 报告**存在却解析不了**必须报错，不能静默当成"没有报告"：那会让一个 succeeded 的
// 任务落进失败分支，recordFailure 在一张其实签成了的证书上写下 last_error——账本上
// 同时出现"任务成功"与"证书最后一次尝试失败"两个互相矛盾的事实。
func decodeResult(job JobFact) (string, map[string]any, jobResult, error) {
	var res jobResult
	if len(strings.TrimSpace(string(job.Result))) > 0 {
		if err := json.Unmarshal(job.Result, &res); err != nil {
			return "", nil, jobResult{}, fmt.Errorf("代理报告不是合法 JSON: %w", err)
		}
	}
	if res.ObservedState == nil {
		res.ObservedState = map[string]any{}
	}
	return res.Outcome, res.ObservedState, res, nil
}

// infoFromObserved 从代理报告的 observed_state 里拼出一份证书事实。
//
// 这一层刻意严格：报告的字段不全就返回错误，而不是用零值凑一份"事实"。零值凑出来
// 的 not_after 是 0001 年，写进库里就是一张"已经过期两千年"的证书。
func infoFromObserved(observed map[string]any) (tlscert.Info, error) {
	info := tlscert.Info{
		Issuer:      observedString(observed, "issuer"),
		Subject:     observedString(observed, "subject"),
		Domains:     observedStrings(observed, "domains"),
		Fingerprint: observedString(observed, "fingerprint"),
		Staging:     observedBool(observed, "staging"),
		SelfSigned:  observedBool(observed, "self_signed"),
	}
	if n, ok := observedNumber(observed, "chain_length"); ok {
		info.ChainLength = int(n)
	}
	notBefore, err := observedTime(observed, "not_before")
	if err != nil {
		return tlscert.Info{}, err
	}
	notAfter, err := observedTime(observed, "not_after")
	if err != nil {
		return tlscert.Info{}, err
	}
	info.NotBefore, info.NotAfter = notBefore, notAfter
	if info.Fingerprint == "" {
		return tlscert.Info{}, errors.New("报告里没有 fingerprint")
	}
	if len(info.Fingerprint) != 64 {
		return tlscert.Info{}, fmt.Errorf("报告里的 fingerprint 长度不是 64：%d", len(info.Fingerprint))
	}
	if len(info.Domains) == 0 {
		return tlscert.Info{}, errors.New("报告里没有 domains")
	}
	if info.Issuer == "" {
		return tlscert.Info{}, errors.New("报告里没有 issuer")
	}
	return info, nil
}

// attemptAt 读代理报告里的尝试时间；读不到时返回零值（由库层退化成"现在"）。
func attemptAt(observed map[string]any) time.Time {
	at, err := observedTime(observed, "attempted_at")
	if err != nil {
		return time.Time{}
	}
	return at
}

// failureText 拼一句人能读懂的失败原因。
//
// 顺序是有讲究的：代理的错误对象里已经有 code 与 message 时用它（那是判决理由），
// 否则退到报告里的 recovery_hint（那是恢复建议），再否则用状态本身。
// **stderr_tail 不进这句话**：certbot 的原始输出动辄几十行，塞进 last_error 会把
// 界面撑坏；它已经完整地留在任务详情里了。
func failureText(job JobFact, res jobResult) string {
	var obj struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(strings.TrimSpace(string(job.Error))) > 0 {
		if err := json.Unmarshal(job.Error, &obj); err == nil {
			switch {
			case obj.Message != "" && obj.Code != "":
				return truncateRunes(obj.Code+"："+obj.Message, 2000)
			case obj.Message != "":
				return truncateRunes(obj.Message, 2000)
			case obj.Code != "":
				return obj.Code
			}
		}
	}
	if res.RecoveryHint != "" {
		return truncateRunes(res.RecoveryHint, 2000)
	}
	return "任务结束状态：" + job.Status
}

// ---- 报告字段的类型转换 ----
//
// 报告是 JSON 解出来的 map[string]any，数字是 float64、时间与域名是字符串。
// 每个字段都单独判一次类型：写错类型时宁可得到"缺字段"，也不要得到一份看起来
// 合理却来源不明的值。

func observedString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func observedStrings(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		// 同一份 map 也可能在本地构造（测试、或 Data 里放过 []string）。
		if direct, ok2 := m[key].([]string); ok2 {
			return direct
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func observedBool(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func observedNumber(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func observedTime(m map[string]any, key string) (time.Time, error) {
	raw := observedString(m, key)
	if raw == "" {
		return time.Time{}, fmt.Errorf("报告里没有 %s", key)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("报告里的 %s 不是 RFC3339（%q）", key, raw)
	}
	return t.UTC(), nil
}
