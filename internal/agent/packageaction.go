package agent

// 四个 APT 写动作（P11.2）：package.refresh / package.install / package.remove /
// package.upgrade（CONTRACTS.md 第 5 节，名字已冻结）。
//
// 这一层的职责很窄：**把参数变成一次可审计的意图，把预检结论翻译成 Plan**。
// 所有判断都在 internal/apt 里做——锁、模拟、保护名单、非交互、结果核对。
// 代理不重新实现其中任何一条，也不给它加"跳过检查"的开关。
//
// 四条固定策略（P11.2 的权限设计）：
//
//	refresh  不要再认证、不要短语 —— 它不改变已安装的软件，只是重下索引
//	install  要再认证、不要短语 —— 装东西是可逆的，但会改系统
//	upgrade  要再认证、不要短语 —— 同上
//	remove   要再认证、**要短语** —— 卸载会带走连带依赖，且可能带走远程登录
//
// 四个动作全部**不可取消**：apt 拿到锁开始写 dpkg 数据库之后，"取消"只会让包
// 停在半配置状态。apt 自己不提供安全中断点，面板也不假装有。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"upanel/internal/apt"
)

// 动作名（与 CONTRACTS.md 第 5 节一致，来自 internal/apt 的常量）。
const (
	PackageRefreshAction = apt.ActionRefresh
	PackageInstallAction = apt.ActionInstall
	PackageRemoveAction  = apt.ActionRemove
	PackageUpgradeAction = apt.ActionUpgrade
)

// packagePlanBudget 是软件包动作**同步生成计划**的时间上限（见 Action.PlanBudget：
// 预检与提交前的执行前复核都算）。
//
// 为什么需要它：模拟（`apt-get -s`）比读索引慢得多，而且是唯一慢在系统侧的预检。
// 实测（24.04 验收机，apt 2.8.3）：`apt-get -s --only-upgrade install zlib1g` 15.6 秒，
// 普通 install 模拟 4～10 秒，而 refresh 的模拟 0.09 秒。不给预算的话，先掐断的是
// 连接（代理 15 秒写超时 / Web 10 秒通用客户端），于是"能不能装这个包"被显示成
// "代理不可用"——假话，而且把一次正常的慢判成了故障。
//
// 75 秒的依据是两台验收机的实测最坏值：24.04 上一次 `package.remove` 的连带删除模拟
// 47.2 秒、一次提交前的复核 38.7 秒（同机 2 vCPU、磁盘同时在跑 apt-get update）。
// 它仍比 Web 的等待上限（operations.PreflightAgentTimeout，90 秒）短，所以真超时的
// 时候先说话的**是代理**："预检超过 1m15s 未完成…稍后重试"。
// 执行路径不受这个预算影响（内部 `writeTimeout` 是 20 分钟）。
const packagePlanBudget = 75 * time.Second

// MaxPackageNames 是一次写事务最多点名的软件包数。
//
// 上限不是为了防攻击（参数本身已经按字节限制），而是为了防**误操作**：
// 一次点名几百个包，预检的计划会变成一屏没人能读完的列表，"我确认过了"这句话
// 就失去意义。批量升级整台机器不是这个页面的能力。
const MaxPackageNames = 200

// packageNamePattern 是 Debian 的二进制包名（含可选的多架构限定）。
//
// 规则（Debian Policy 5.6.1 的收紧版）：小写字母/数字开头，只含 `a-z0-9+.-`，
// 至少两个字符；可选后缀 `:架构`（例如 `:amd64`、`:all`、`:arm64`）。
//
// 刻意**不**接受的东西，每一条都对应一种"看起来像包名其实不是"的输入：
//   - 以 `-` 开头（会被 apt 当成选项）、
//   - 含空格或换行（会被拆成多个参数）、
//   - 含 `/` 或 `..`（路径）、
//   - 含 `*` `?` `~` `=版本`（通配、pin、版本限定——面板不提供这些语义）、
//   - 含大写（dpkg 的包名不允许）。
var packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+(:[a-z0-9][a-z0-9-]*)?$`)

// ValidatePackageName 校验一个包名。
func ValidatePackageName(name string) error {
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("包名不能有首尾空白: %q", name)
	}
	if !packageNamePattern.MatchString(name) {
		return fmt.Errorf("包名 %q 不合法（应为小写字母/数字加 . + -，可带 :架构 限定）", name)
	}
	return nil
}

// packageWriteParams 是 install / remove / upgrade 的参数。
//
// 只有一个 packages 数组：没有 params、没有 args、没有 `force`/`now`/`autoremove`
// 这类会被翻译成 apt-get 选项的字段，也没有 `dist_upgrade` 之类的开关
// （CONTRACTS.md：package.upgrade 只做逐包升级，不允许隐式全量升级）。
type packageWriteParams struct {
	Packages []string `json:"packages"`
}

// packageRefreshParams 是 package.refresh 的参数（必须是空对象）。
type packageRefreshParams struct{}

// PackageWrites 是 APT 写事务的实现端（生产实现是 *apt.Writer）。
//
// 用接口而不是直接抓 *apt.Writer：验收与单测要能喂一份"真实的结论"，
// 而生产路径用的仍然是那个真的会跑 apt-get 的实现。
type PackageWrites interface {
	// Assess 计算预检结论（不写系统）。
	Assess(ctx context.Context, op apt.Op, names []string) (apt.Assessment, error)
	// Execute 执行一次写事务并核对结果。
	Execute(ctx context.Context, op apt.Op, names []string, progress func(step string, fields map[string]any)) (apt.ExecResult, error)
	// Refresh 刷新索引。
	Refresh(ctx context.Context, progress func(step string, fields map[string]any)) (apt.ExecResult, error)
}

// RegisterPackageActions 登记四个 APT 写动作。
func RegisterPackageActions(reg *Registry, src PackageWrites) error {
	for _, action := range []Action{
		newPackageRefreshAction(src),
		newPackageInstallAction(src),
		newPackageRemoveAction(src),
		newPackageUpgradeAction(src),
	} {
		if err := reg.RegisterAction(action); err != nil {
			return fmt.Errorf("代理登记 %s 失败: %w", action.Name, err)
		}
	}
	return nil
}

// newPackageRefreshAction 登记 package.refresh。
func newPackageRefreshAction(src PackageWrites) Action {
	return Action{
		Name: PackageRefreshAction,
		// 预检要跑 `apt-get -s update`（实测 0.09 秒，但索引坏掉时会卡在网络上）。
		PlanBudget: packagePlanBudget,
		Decode: func(raw json.RawMessage) (any, error) {
			// 空参数也必须严格解码：多一个字段就拒绝，而不是静默忽略。
			var params packageRefreshParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			assessment, err := src.Assess(ctx, apt.OpRefresh, nil)
			if err != nil {
				return Plan{}, err
			}
			return planFromPackageAssessment(packageRefreshParams{}, assessment), nil
		},
		Policy: ActionPolicy{
			// 刷新索引不改变已安装的软件，只是重新下载元数据。要密码反而会让人
			// 在每天都会做的事上习惯性输入密码。
			RequiresReauth: false,
			RequiresPhrase: false,
			// apt-get update 写到一半被 kill 会让索引处于半更新状态（下次 update
			// 会修复，但那是一个不可知窗口）。所以不可取消。
			Cancellable: false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			if _, ok := in.Params.(packageRefreshParams); !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.Refresh(ctx, in.Progress)
			if err != nil {
				return Result{}, err
			}
			return refreshResult(res), nil
		},
	}
}

// newPackageInstallAction 登记 package.install。
func newPackageInstallAction(src PackageWrites) Action {
	return newPackageWriteAction(src, apt.OpInstall, ActionPolicy{
		// 装包会改系统状态，但可逆（再卸掉就是）。要再认证、不要短语。
		RequiresReauth: true,
		RequiresPhrase: false,
		Cancellable:    false,
	})
}

// newPackageUpgradeAction 登记 package.upgrade。
func newPackageUpgradeAction(src PackageWrites) Action {
	return newPackageWriteAction(src, apt.OpUpgrade, ActionPolicy{
		// 升级不是"可选升级"：它换掉正在运行的软件的二进制。要再认证。
		RequiresReauth: true,
		RequiresPhrase: false,
		Cancellable:    false,
	})
}

// newPackageRemoveAction 登记 package.remove。
func newPackageRemoveAction(src PackageWrites) Action {
	return newPackageWriteAction(src, apt.OpRemove, ActionPolicy{
		RequiresReauth: true,
		// 卸载是唯一要短语的动作：它可能带走连带依赖（真实样本里删 ca-certificates
		// 带走了 26 个包，含 certbot 与 ubuntu-server 元包），而且当前会话不会立刻
		// 断——等你断开才发现 SSH 服务端没了。短语是给"我真的要删"留一次停顿。
		RequiresPhrase: true,
		PhraseVerb:     "REMOVE",
		Cancellable:    false,
	})
}

// newPackageWriteAction 是 install / remove / upgrade 的共同形状。
func newPackageWriteAction(src PackageWrites, op apt.Op, policy ActionPolicy) Action {
	return Action{
		Name: actionForOp(op),
		// 预检要跑 `apt-get -s`（24.04 上升级模拟实测 15.6 秒）。
		PlanBudget: packagePlanBudget,
		Decode: func(raw json.RawMessage) (any, error) {
			var params packageWriteParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			names, err := normalizePackageNames(params.Packages)
			if err != nil {
				return nil, err
			}
			return packageWriteParams{Packages: names}, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(packageWriteParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			assessment, err := src.Assess(ctx, op, params.Packages)
			if err != nil {
				return Plan{}, err
			}
			return planFromPackageAssessment(params, assessment), nil
		},
		Policy: policy,
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(packageWriteParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			if len(params.Packages) == 0 {
				// 走到这里说明预检落库的参数被换过；与其让 apt-get 跑一个没有目标的
				// install，不如明确拒绝。
				return Result{}, fmt.Errorf("%w: 没有指定软件包", ErrParamsInvalid)
			}
			res, err := src.Execute(ctx, op, params.Packages, in.Progress)
			if err != nil {
				// 失败时 apt 层已经把原因写成一句人话（"apt-get update 有错误行：E: …"
				// "apt-get remove 失败（退出码 100）：…"），包一层让 runner 把它放进
				// 任务的 error.message——失败任务的 result 按契约是空对象，那是管理员
				// 唯一能看到原因的地方。
				return partialPackageResult(res), packageFailure{err: err}
			}
			return packageResult(res), nil
		},
	}
}

// actionForOp 把操作映射成冻结的动作名。
func actionForOp(op apt.Op) string {
	switch op {
	case apt.OpInstall:
		return PackageInstallAction
	case apt.OpRemove:
		return PackageRemoveAction
	case apt.OpUpgrade:
		return PackageUpgradeAction
	case apt.OpRefresh:
		return PackageRefreshAction
	}
	return ""
}

// normalizePackageNames 校验并规范化包名列表。
//
// 去重但不排序：保持用户点选的顺序，界面上"我要装的这几个"读起来才对；
// 而预检的身份指纹（apt 侧）本来就会排序，两种写法算同一个意图。
func normalizePackageNames(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: packages 不能为空", ErrParamsInvalid)
	}
	if len(raw) > MaxPackageNames {
		return nil, fmt.Errorf("%w: 一次最多 %d 个软件包（收到 %d 个）",
			ErrParamsInvalid, MaxPackageNames, len(raw))
	}
	names := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if err := ValidatePackageName(name); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

// planFromPackageAssessment 把 apt 的预检结论翻译成代理的 Plan。
//
// 与 proxy 的 planFromAssessment 分开命名：两份结论的字段来源不同，硬合成一个
// 接口会让"这个字段从哪来"只能靠记忆。
//
// 翻译是**逐字段直译**：面板不在这里加减阻塞项，也不把阻塞项降级成告警。
// 唯一的加工是措辞（apt 侧已经是给人看的句子）。
func planFromPackageAssessment(params any, assessment apt.Assessment) Plan {
	blockers := make([]Notice, 0, len(assessment.Blockers))
	for _, b := range assessment.Blockers {
		blockers = append(blockers, Notice{Code: b.Code, Message: b.Message})
	}
	warnings := make([]Notice, 0, len(assessment.Warnings))
	for _, w := range assessment.Warnings {
		warnings = append(warnings, Notice{Code: w.Code, Message: w.Message})
	}
	impacts := make([]Impact, 0, len(assessment.Impacts))
	for _, i := range assessment.Impacts {
		impacts = append(impacts, Impact{Kind: i.Kind, Description: i.Description})
	}
	locks := assessment.Locks
	if len(locks) == 0 {
		locks = []string{"apt-global"}
	}
	return Plan{
		NormalizedParams: params,
		Target: Target{
			DisplayName:         assessment.DisplayName,
			IdentityFingerprint: assessment.Identity,
			StateVersion:        assessment.StateVersion,
		},
		Allowed:  assessment.Allowed,
		Blockers: blockers,
		Warnings: warnings,
		Impacts:  impacts,
		Locks:    locks,
	}
}

// packageResult 把执行结果翻译成代理的 Result。
//
// outcome 只有两种可能：每一条计划步骤都在读回的 dpkg 事实里对上了 → verified，
// 否则 partial。**没有 accepted 这一档**：apt 的退出码不是验收标准，
// "命令跑完了"这件事在包管理器上不构成"做成了"。
func packageResult(res apt.ExecResult) Result {
	outcome := OutcomeVerified
	if !res.Verified {
		outcome = OutcomePartial
	}
	data := map[string]any{
		"operation":    string(res.Op),
		"packages":     res.Names,
		"exit_code":    res.ExitCode,
		"duration_ms":  res.Duration.Milliseconds(),
		"plan_steps":   len(res.Plan.Steps),
		"observed":     len(res.Observed),
		"mismatches":   res.Mismatches,
		"index_before": timeText(res.IndexBefore),
		"index_after":  timeText(res.IndexUpdatedAt),
	}
	if text := strings.TrimSpace(res.Stdout); text != "" {
		data["stdout"] = capText(text, 8<<10)
	}
	if text := strings.TrimSpace(res.Stderr); text != "" {
		data["stderr"] = capText(text, 8<<10)
	}
	return Result{
		Outcome:       outcome,
		VerifiedAt:    res.VerifiedAt,
		ObservedState: observedPackages(res),
		RecoveryHint:  recoveryHint(res),
		Data:          data,
	}
}

// packageFailure 把 apt 已经核实过的原因变成任务的 error.message（见 FailureReasoner）。
//
// 只做两件事：保留原错误链（Unwrap 让 statusCarrier / ErrBusy 那一层照样能被认出来），
// 和把原因压成一行——apt 的输出可以上百行，message 是让人扫一眼的那一格。
type packageFailure struct{ err error }

func (f packageFailure) Error() string { return f.err.Error() }

func (f packageFailure) Unwrap() error { return f.err }

func (f packageFailure) FailureReason() string { return oneLine(f.err.Error(), 300) }

// oneLine 把多行原因压成一行并截断，超出部分明确说明被截断。
func oneLine(text string, limit int) string {
	fields := strings.Fields(text)
	line := strings.Join(fields, " ")
	runes := []rune(line)
	if len(runes) <= limit {
		return line
	}
	return string(runes[:limit]) + "…（已截断）"
}

// partialPackageResult 是失败路径的**返回形状**：执行器把读回的事实一并交回来，
// 由调用方决定用不用。
//
// 注意它落不了库：契约规定失败任务的 result 是空对象（错误在 error 里，不塞进
// result），runner 走失败分支时不会序列化这个 Result。真正到管理员眼前的失败原因
// 是 packageFailure 带出去的那一句——所以这里的事实只服务于本次调用与测试，
// 不能当成"失败现场已经保存在任务里"。
func partialPackageResult(res apt.ExecResult) Result {
	observed := observedPackages(res)
	observed["verified"] = false
	return Result{
		Outcome:       OutcomePartial,
		ObservedState: observed,
		RecoveryHint:  recoveryHint(res),
		Data: map[string]any{
			"operation":   string(res.Op),
			"packages":    res.Names,
			"exit_code":   res.ExitCode,
			"duration_ms": res.Duration.Milliseconds(),
			"mismatches":  res.Mismatches,
		},
	}
}

// observedPackages 把"相关包的安装状态"摊平成键值。
//
// 只放真实读到的字段：status 是 dpkg 的 `ii`/`rc` 原文，version 是已装版本。
// 没有装过的包给"未安装"，不省略——省略会让人以为面板没查。
func observedPackages(res apt.ExecResult) map[string]any {
	names := make([]string, 0, len(res.Observed))
	for name := range res.Observed {
		names = append(names, name)
	}
	sort.Strings(names)
	observed := map[string]any{
		"operation":         string(res.Op),
		"verified":          res.Verified,
		"mismatch_count":    len(res.Mismatches),
		"observed_packages": len(res.Observed),
	}
	for _, name := range names {
		pkg := res.Observed[name]
		if pkg.Status == "" {
			observed["pkg:"+name] = "未安装"
			continue
		}
		observed["pkg:"+name] = strings.TrimSpace(pkg.Status + " " + pkg.Version)
	}
	return observed
}

// refreshResult 是 package.refresh 的结果。
func refreshResult(res apt.ExecResult) Result {
	data := map[string]any{
		"operation":      string(res.Op),
		"exit_code":      res.ExitCode,
		"duration_ms":    res.Duration.Milliseconds(),
		"index_before":   timeText(res.IndexBefore),
		"index_after":    timeText(res.IndexUpdatedAt),
		"refresh_errors": res.RefreshErrors,
		"refresh_warns":  res.RefreshWarnings,
		"index_advanced": advanced(res.IndexBefore, res.IndexUpdatedAt),
	}
	if text := strings.TrimSpace(res.Stdout); text != "" {
		data["stdout"] = capText(text, 8<<10)
	}
	observed := map[string]any{
		"operation":   string(res.Op),
		"verified":    res.Verified,
		"exit_code":   res.ExitCode,
		"errors":      len(res.RefreshErrors),
		"index_after": timeText(res.IndexUpdatedAt),
	}
	return Result{
		Outcome:       OutcomeVerified,
		VerifiedAt:    res.VerifiedAt,
		ObservedState: observed,
		Data:          data,
	}
}

// recoveryHint 给出失败时的下一步（给人看的一句话，不是自动修复）。
//
// 刻意**不**建议 `dpkg --configure -a` 这类自动修复命令：apt 中断的原因往往
// 就在它给出的第一条 `E:` 里，先看原因再决定，比先修一遍再猜要好。
func recoveryHint(res apt.ExecResult) string {
	if res.Verified {
		return ""
	}
	if len(res.Mismatches) > 0 {
		return "apt 报告成功，但读回的软件包状态与预检的计划不一致；请先用 apt.packages 查询现场，再决定是否重试"
	}
	return ""
}

// advanced 报告索引时间是否前进（读不到时不算前进）。
func advanced(before, after *time.Time) bool {
	if before == nil || after == nil {
		return false
	}
	return after.After(*before)
}

// timeText 把可选时间点写成 RFC3339；读不到就是空串（不写"未知"冒充值）。
func timeText(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// capText 截断长文本（保留头部），并说明被截断了。
func capText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "\n…（输出已截断）"
}
