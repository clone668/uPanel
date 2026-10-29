package deploy

// 更新一份受管部署（P15.3；CONTRACTS 第 5 节 `deployment.update`、PLAN 第 12 节）。
//
// 更新能改的只有一件事：**镜像引用**。端口、挂载、环境变量、资源上限都不动——
// 换端口要重新做端口冲突与锁，换挂载要重新核对卷与文件根，那些是一次新的部署，
// 不是"升级"。这一条是契约表的形状决定的（参数只有 image_reference）。
//
// 顺序不是随手排的，每一步都在回答"现在反悔还来得及吗"：
//
//	1. 停单元（只有选了备份才停）   停完才有一份安静的数据可以拷。
//	2. 跑那次备份                   备份失败 → 配置没动、镜像没换、把单元起回来。
//	3. 拉新镜像并读回 digest        拉不到 → 同上，单元还在旧版本上跑。
//	4. 把旧配置原样留一份，再写新的  从这一步起"旧配置还在盘上"，回切有字节可依。
//	5. restart 单元                 配置换了字节、单元文件一个字没变，只有 restart 会让
//	                                ExecStart 重跑（`start` 对 active 的 oneshot 是空操作）。
//	6. 面板侧健康探测                失败 → 回切到第 4 步留下的字节、restart、再探一次。
//	7. 写记录                       镜像 digest、上一版镜像、更新时间。
//
// 回切的边界（PLAN 第 12 节的明文要求，这里逐条落地）：
//
//	能回切   配置字节（原样还原）与镜像（旧镜像还在本地，restart 就用回去）。
//	不能回切 数据。容器新版本可能已经跑过数据库迁移；把旧镜像起回来不等于把数据
//	         也变回去。所以回切结论里永远带着这一句，也不提供"一键回滚数据"。
//	不自动回切的情形 备份失败、拉镜像失败——那时什么都没变，回切的说法会让人以为
//	         发生过一次失败的更新。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// backupPolicyIDPattern 是备份策略标识的形状（pol_ 加 32 位小写十六进制）。
var backupPolicyIDPattern = regexp.MustCompile(`^pol_[0-9a-f]{32}$`)

// 两个"回切成功"的拒绝码。
//
// 它们和别的拒绝码不一样：现场已经回到上一版、单元也起来了，需要有人去查的只剩
// "新版本为什么没成"。调用方（代理动作层）据此把结果记成 rolled_back 而不是一次
// 什么都不留下的失败——失败任务的结果是空的，而这里最有价值的信息恰恰是读回值。
const (
	CodeUpdateFailedRolledBack = "update_failed_rolled_back"
	CodeHealthFailedRolledBack = "health_failed_rolled_back"
)

// 更新的时间预算。
//
// 操作本身有 10 分钟的总预算（agent.OperationTimeout，主进程侧的硬线），更新要在这个
// 数字里把自己切成几段，否则一次"备份 + 拉镜像 + 重启 + 探测"会在最后一刻被整条掐断，
// 而掐断点上正好是"配置写了、单元还没起"这种最难看清的状态。
const (
	// UpdateBudget 是一次更新的总预算。
	UpdateBudget = 9 * time.Minute
	// UpdateBackupBudget 是更新前那次备份的上限。
	//
	// 与备份动作动辄数小时的上限不同：这里必须收窄。超过预算的备份停掉，本次更新
	// **什么都不改**地失败，并明确告诉人"先用 backup.run 单独跑一次"。一个安静地跑了
	// 四十分钟、最后因为操作超时把应用留在停止状态的更新，比一次诚实的拒绝糟得多。
	UpdateBackupBudget = 360 * time.Second
	// UpdateStopBudget 是停单元的上限（单元的 ExecStop 时限是 300 秒）。
	UpdateStopBudget = ComposeDownTimeoutSeconds*time.Second + 60*time.Second
	// UpdateRollbackReserve 是留给回切的预算：写回旧字节 + restart + 再探一次。
	UpdateRollbackReserve = 120 * time.Second
	// UpdateMinStep 是一步至少需要的时间，不够就不开始这一步。
	UpdateMinStep = 5 * time.Second
)

// BackupCause 是"更新前备份"（CONTRACTS 第 5 节 `deployment.update` 的 backup_policy_id）。
//
// 为什么是一个接口而不是直接依赖 internal/backup：备份那一包的装配要用到仓库镜像、
// 密钥环、restic 路径与应用控制，全在代理侧；部署这一包只该说"我要在更新前跑一次
// 这条策略的备份，成不成你告诉我"。接口由代理实现，两边都不必知道对方的内部结构。
type BackupCause interface {
	// AssessBackup 只读地判断这条策略现在能不能作为"更新前备份"跑。
	AssessBackup(ctx context.Context, policyID string) (BackupAssessment, error)
	// RunBackup 执行一次备份。SnapshotID 是唯一算数的证据：没有它就不算备过。
	// operationID 用于临时文件命名（SFTP 私钥落在 tmpfs 上时要一个可追溯的名字）。
	RunBackup(ctx context.Context, policyID, operationID string) (BackupOutcome, error)
}

// BackupAssessment 是"更新前备份"的预检结论。
type BackupAssessment struct {
	PolicyID     string
	RepositoryID string
	// Description 是这条策略备份什么（给人看，不含路径以外的秘密）。
	Description string
	Allowed     bool
	Blockers    []Notice
	Warnings    []Notice
	// Locks 是这次备份要占的资源锁（仓库/策略），更新会一起持有。
	Locks []string
}

// BackupOutcome 是一次"更新前备份"的结果。
type BackupOutcome struct {
	PolicyID     string
	RepositoryID string
	// SnapshotID 是快照标识；空表示没建成。
	SnapshotID string
	// Verified 表示快照被读回核对过（restic stats）。没有它就不是"备份成功"。
	Verified bool
	// Summary 是一句人话（快照 id、耗时、口径）。
	Summary  string
	Duration time.Duration
}

// UpdateParams 是 deployment.update 的参数（CONTRACTS 第 5 节）。
type UpdateParams struct {
	DeploymentID string `json:"deployment_id"`
	// Version 是调用方看到的部署版本（配置摘要）。
	Version string `json:"version"`
	// ImageReference 是新的镜像引用。
	ImageReference string `json:"image_reference"`
	// BackupPolicyID 可空：给了就先按这条策略做一次备份，再换镜像。
	BackupPolicyID *string `json:"backup_policy_id,omitempty"`
}

// BackupPolicy 返回规范化之后的备份策略标识（空表示不备份）。
func (p UpdateParams) BackupPolicy() string {
	if p.BackupPolicyID == nil {
		return ""
	}
	return strings.TrimSpace(*p.BackupPolicyID)
}

// UpdateAssessment 是一次更新预检的结论。
type UpdateAssessment struct {
	DeploymentID string
	ProjectName  string
	UnitName     string
	// Definition 是**换过镜像之后**的规范化定义（预检绑定的就是它）。
	Definition Definition
	// CurrentImage 是记录里原来的镜像引用；TargetImage 是这次要换成的。
	CurrentImage string
	TargetImage  string
	// CurrentImageDigest 是记录里上一版的镜像 digest（读不到时为空）。
	CurrentImageDigest string
	// CurrentConfigDigest 是盘上那份配置的摘要；ConfigDigest 是这次要落盘的。
	CurrentConfigDigest string
	ConfigDigest        string
	// ConfigChanged 表示这次真的会写出不同的字节（false 时这次更新等于"重新拉起"）。
	ConfigChanged bool
	// StateVersion 是执行前复核用的版本令牌（= 盘上配置的摘要）。
	StateVersion string
	// BackupPolicyID / BackupRequired：这次更新是否要先做一次备份。
	BackupPolicyID string
	BackupRequired bool
	// HealthCheckEnabled 表示成功之后会做一次面板侧探测。
	HealthCheckEnabled bool
	Allowed            bool
	Blockers           []Notice
	Warnings           []Notice
	// Claims 是这份部署占用的端口（锁与冲突检测用，与 apply 同一份）。
	Claims []Claim
	// MountedSources 是容器路径 → 宿主路径（展示用）。
	MountedSources map[string]string
	// VolumeMountPoints 是这次部署依赖的卷挂载点（展示用）。
	VolumeMountPoints []string
	// BackupLocks 是这次备份要额外占的锁。
	BackupLocks []string
}

// Locks 返回一次更新要持有的资源锁。
//
// 与 apply 同一批（compose 项目 + 端口 + 卷），再加备份那两个：更新期间同一个仓库上
// 不该同时跑一次 backup.run——那会让两份快照在同一个 restic 进程池里抢锁，
// 而运维看到的是"备份莫名其妙失败"。
func (a UpdateAssessment) Locks() []string {
	set := make(map[string]struct{}, 8)
	add := func(name string) {
		if name != "" {
			set[name] = struct{}{}
		}
	}
	if a.ProjectName != "" {
		add(LockName(a.ProjectName))
	}
	for _, c := range a.Claims {
		add(PortLockName(c))
	}
	for _, id := range a.Definition.VolumeDependencies {
		add(VolumeLockName(id))
	}
	for _, name := range a.BackupLocks {
		add(name)
	}
	locks := make([]string, 0, len(set))
	for name := range set {
		locks = append(locks, name)
	}
	// 锁名必须有序：代理侧的锁管理器按固定顺序取锁来避免死锁。
	sort.Strings(locks)
	return locks
}

// UpdateReport 是一次更新的读回事实。
type UpdateReport struct {
	DeploymentID string
	ProjectName  string
	UnitName     string
	ConfigPath   string
	// PreviousConfigDigest / ConfigDigest 是换之前与换之后的配置摘要（版本）。
	PreviousConfigDigest string
	ConfigDigest         string
	// Image 是换过之后读回来的镜像事实；ImageDigestKnown=false 表示没读到。
	Image            ImageRecord
	ImageDigestKnown bool
	// UnitStopped 表示在备份之前停过单元（现在它应该已经起来了）。
	UnitStopped bool
	// Backup 是这次更新前的备份（没选备份时 SnapshotID 为空）。
	Backup BackupOutcome
	// Health 是更新之后的健康探测（没启用健康检查时 Checked=false）。
	Health HealthProbe
	// RolledBack 表示健康探测失败、已经回切到上一版配置。
	RolledBack bool
	// RollbackHealth 是回切之后的探测结论。
	RollbackHealth HealthProbe
	// RollbackVerified 表示回切之后单元 active 且（若启用）探测通过。
	RollbackVerified bool
	// Enabled / Active / Restarted 来自 Apply 的读回。
	Enabled   bool
	Active    bool
	Restarted bool
}

// UpdateOutcome 是一次更新的执行结果。
type UpdateOutcome struct {
	Report      UpdateReport
	Definition  Definition
	ProjectName string
	// Idempotent 表示盘上的配置与这次要写的一模一样（这次只是重新拉起）。
	Idempotent bool
	Duration   int64
	// Warnings 是执行期的告警（记录写不进去、digest 读不到……）：它们不改变结论，
	// 但必须说出来，否则"更新成功"会连带的把"其实没记下 digest"一起藏起来。
	Warnings []string
}

// currentRecordImage 从记录里取当前镜像。
func currentRecordImage(rec DeploymentRecord) ImageRecord { return rec.Image }

// AssessUpdate 预检一次更新。只读：不拉镜像、不停单元、不写文件。
func (s *Service) AssessUpdate(ctx context.Context, p UpdateParams) (UpdateAssessment, error) {
	assessment := UpdateAssessment{DeploymentID: strings.TrimSpace(p.DeploymentID)}
	id := assessment.DeploymentID
	if id == "" {
		return assessment, &ValidationError{Field: "deployment_id", Code: CodeMissing,
			Message: "更新部署必须给出部署标识"}
	}
	// 镜像引用先校验形状：它进 argv 也进配置，形状不对时后面几步都没意义。
	ref, err := parseImageReference(p.ImageReference)
	if err != nil {
		return assessment, err
	}
	assessment.TargetImage = ref.String()

	name, err := s.applier.Find(id)
	if err != nil {
		assessment.Blockers = append(assessment.Blockers,
			Notice{Code: "no_such_deployment", Message: err.Error()})
		return assessment, nil
	}
	assessment.ProjectName = name
	assessment.UnitName = mustUnitName(name)

	rec, err := s.applier.requireRecord(name)
	if err != nil {
		assessment.Blockers = append(assessment.Blockers,
			Notice{Code: "no_definition_record", Message: err.Error()})
		return assessment, nil
	}
	assessment.CurrentImage = rec.Image.Requested
	assessment.CurrentImageDigest = rec.Image.Digest

	def := rec.Definition
	def.ImageReference = assessment.TargetImage
	normalized, err := Normalize(def)
	if err != nil {
		return assessment, err
	}
	assessment.Definition = normalized
	assessment.Claims = normalized.Claims()

	state, err := s.applier.Inspect(name)
	if err != nil {
		return assessment, err
	}
	if !state.ConfigExists {
		// 单元还在而配置没了：更新没有可以替换的东西。
		assessment.Blockers = append(assessment.Blockers, Notice{Code: "deployment_incomplete",
			Message: "单元还在，但渲染配置已经不在盘上了：这份部署处于半残状态，" +
				"只能 deployment.remove 之后重新 apply"})
		return assessment, nil
	}
	assessment.CurrentConfigDigest = state.ConfigDigest
	assessment.StateVersion = deploymentStateVersion(state, "")
	if want := strings.TrimSpace(p.Version); want != "" && want != state.ConfigDigest {
		assessment.Blockers = append(assessment.Blockers, Notice{Code: "stale",
			Message: fmt.Sprintf("盘上是 %s，请求里是 %s；期间有人改过这份部署", state.ConfigDigest, want)})
	}
	// 记录里说的那一版与盘上那份对不上：有人动过配置文件，或者记录是旧的。
	// 这时拿记录里的定义去渲染新配置，会顺手把别人的改动覆盖掉。
	if rec.ConfigDigest != "" && rec.ConfigDigest != state.ConfigDigest {
		assessment.Blockers = append(assessment.Blockers, Notice{Code: "record_mismatch",
			Message: fmt.Sprintf("记录里是 %s，盘上是 %s：配置文件被改过（或记录没跟上）。"+
				"更新会覆盖盘上的字节，所以先停下来——确认改动无关后删掉记录再更新，或重新 apply 重建",
				rec.ConfigDigest, state.ConfigDigest)})
	}

	env, err := s.env(ctx)
	if err != nil {
		return assessment, err
	}
	if err := CheckHost(normalized, env); err != nil {
		return assessment, err
	}
	for _, problem := range CheckVolumesReady(normalized, env) {
		assessment.Blockers = append(assessment.Blockers, Notice{Code: "volume_not_ready", Message: problem})
	}

	rendered, err := Render(ctx, normalized, RenderEnv{Host: env, Resolver: s.resolver})
	if err != nil {
		if errors.Is(err, ErrSecretUnavailable) {
			assessment.Blockers = append(assessment.Blockers, Notice{Code: "secret_unavailable",
				Message: "环境变量引用的秘密读不到，整份配置不产出：" + err.Error()})
			return assessment, nil
		}
		return assessment, err
	}
	assessment.ConfigDigest = rendered.ConfigDigest
	assessment.MountedSources = rendered.MountedSources
	assessment.ConfigChanged = rendered.ConfigDigest != state.ConfigDigest

	plan, err := PlanUnitAt(normalized, rendered, env, s.applier.ConfigDir())
	if err != nil {
		return assessment, err
	}
	assessment.VolumeMountPoints = plan.VolumeMountPoints

	assessment.HealthCheckEnabled = normalized.HealthCheck.Enabled
	if !assessment.ConfigChanged {
		assessment.Warnings = append(assessment.Warnings, Notice{Code: "config_unchanged",
			Message: "盘上的配置与这次要写的字节完全一致：这次更新只会把单元重新拉起一次" +
				"（例如上一次更新在写盘之后、拉起之前被打断）"})
	}

	// 更新前备份（可选）。
	if policyID := p.BackupPolicy(); policyID != "" {
		assessment.BackupPolicyID = policyID
		assessment.BackupRequired = true
		if s.backup == nil {
			assessment.Blockers = append(assessment.Blockers, Notice{Code: "backup_unavailable",
				Message: "这台机器上没有可用的备份能力（restic 或配置缺失），不能把" +
					"backup_policy_id 当成已经备份过；去掉备份再更新，或先配好备份仓库"})
			return assessment, nil
		}
		if !ValidBackupPolicyID(policyID) {
			return assessment, &ValidationError{Field: "backup_policy_id", Code: CodeMalformed,
				Message: fmt.Sprintf("备份策略标识 %q 形状不合法（pol_ 加 32 位小写十六进制）", policyID)}
		}
		backup, err := s.backup.AssessBackup(ctx, policyID)
		if err != nil {
			return assessment, err
		}
		assessment.BackupLocks = backup.Locks
		for _, b := range backup.Blockers {
			assessment.Blockers = append(assessment.Blockers,
				Notice{Code: b.Code, Message: "更新前备份被拒绝：" + b.Message})
		}
		for _, w := range backup.Warnings {
			assessment.Warnings = append(assessment.Warnings, Notice{Code: w.Code, Message: w.Message})
		}
		if backup.Allowed {
			assessment.Warnings = append(assessment.Warnings, Notice{Code: "backup_before_update",
				Message: "更新前会先停掉这套部署、按策略 " + policyID + " 做一次备份（" +
					backup.Description + "），再拉镜像并起新版本；备份没建成就不换镜像"})
		}
	}

	if assessment.HealthCheckEnabled {
		assessment.Warnings = append(assessment.Warnings, Notice{Code: "health_probe_after_update",
			Message: fmt.Sprintf("更新之后会从宿主侧探测 http/tcp %s:%d，失败则回切到上一版配置"+
				"并重新拉起（数据不回切）", normalized.HostBindAddress, normalized.HostPort)})
	}
	assessment.Allowed = len(assessment.Blockers) == 0
	return assessment, nil
}

// ExecuteUpdate 执行一次更新。
//
// 每一步的时间都由 budget 收窄（见 UpdateBudget 的说明）：预算不够时**在动手之前**
// 就失败，而不是做到一半被操作超时掐断。
func (s *Service) ExecuteUpdate(ctx context.Context, p UpdateParams, expectState, operationID string,
	progress func(step string, fields map[string]any)) (UpdateOutcome, error) {
	out := UpdateOutcome{}
	started := time.Now()
	deadline := operationDeadline(ctx, UpdateBudget, started)

	// 执行前复核：预检那一套全部重跑。镜像 tag 是别人的东西、卷可能刚被卸下、
	// 记录可能刚被人改过——三种都答"现在不行"。
	assessment, err := s.AssessUpdate(ctx, p)
	if err != nil {
		return out, err
	}
	if !assessment.Allowed {
		return out, &ServiceError{Code: "blocked", Message: firstBlockerMessage(assessment.Blockers)}
	}
	if strings.TrimSpace(expectState) != "" && expectState != assessment.StateVersion {
		return out, &ServiceError{Code: "stale",
			Message: fmt.Sprintf("预检时盘上是 %s，现在是 %s；期间有人改过这份部署",
				orNone(expectState), orNone(assessment.StateVersion))}
	}

	env, err := s.env(ctx)
	if err != nil {
		return out, err
	}
	def := assessment.Definition
	rendered, err := Render(ctx, def, RenderEnv{Host: env, Resolver: s.resolver})
	if err != nil {
		return out, err
	}
	if rendered.ConfigDigest != assessment.ConfigDigest {
		return out, &ServiceError{Code: "stale", Message: "秘密在预检与执行之间变了，" +
			"渲染出来的配置摘要不同；请重新预检"}
	}
	plan, err := PlanUnitAt(def, rendered, env, s.applier.ConfigDir())
	if err != nil {
		return out, err
	}

	report := UpdateReport{
		DeploymentID:         assessment.DeploymentID,
		ProjectName:          assessment.ProjectName,
		UnitName:             assessment.UnitName,
		PreviousConfigDigest: assessment.CurrentConfigDigest,
		ConfigDigest:         assessment.ConfigDigest,
	}
	// 1. 要先备份就先停单元：容器还在写的时候拷出来的目录，只是一份看起来像备份的东西。
	if assessment.BackupRequired {
		stepCtx, cancel, err := stepBudget(ctx, deadline, UpdateStopBudget, UpdateRollbackReserve)
		if err != nil {
			return out, err
		}
		reportStep(progress, "stopping", map[string]any{"unit": report.UnitName,
			"reason": "更新前备份需要一份安静的数据"})
		err = s.applier.StopUnit(stepCtx, report.UnitName)
		cancel()
		if err != nil {
			return out, err
		}
		report.UnitStopped = true

		// 2. 备份。没建成就不换镜像——那才叫"选备份"。
		stepCtx, cancel, err = stepBudget(ctx, deadline, UpdateBackupBudget, UpdateRollbackReserve)
		if err != nil {
			s.restartQuietly(ctx, &report)
			return out, err
		}
		reportStep(progress, "backup", map[string]any{"policy_id": assessment.BackupPolicyID})
		backup, backupErr := s.backup.RunBackup(stepCtx, assessment.BackupPolicyID, operationID)
		cancel()
		report.Backup = backup
		if backupErr != nil || backup.SnapshotID == "" || !backup.Verified {
			reason := "更新前备份没有建成"
			if backupErr != nil {
				reason = "更新前备份失败：" + backupErr.Error()
			} else if backup.SnapshotID == "" {
				reason = "更新前备份没有产出快照"
			} else {
				reason = "更新前备份写出了快照 " + backup.SnapshotID + "，但没有读回核对（" +
					"restic stats 失败）：它不算一份可信的备份"
			}
			// 什么都没改：配置没写、镜像没换，把单元按原样起回来。
			s.restartQuietly(ctx, &report)
			return out, &ServiceError{Code: "backup_failed", Message: reason +
				"；本次更新没有换镜像、没有改配置，单元已按原样起回。" +
				"先用 backup.run 单独跑一次这条策略，或者去掉备份再更新"}
		}

		// 3. 拉镜像。
		if err := s.pullImage(ctx, deadline, def.ImageReference, progress); err != nil {
			s.restartQuietly(ctx, &report)
			return out, err
		}
	} else {
		// 不备份就不用先停：restart 会自己 down 再 up。
		if err := s.pullImage(ctx, deadline, def.ImageReference, progress); err != nil {
			return out, err
		}
	}

	// 4. 拉到的镜像要真的是定义里钉住的那一份（定义写了 @sha256:… 时）。
	facts, factsErr := s.applier.ImageFacts(ctx, def.ImageReference)
	report.Image = s.imageRecordOf(def, rendered, facts, factsErr)
	report.ImageDigestKnown = factsErr == nil && facts.ID != ""
	if factsErr != nil {
		out.Warnings = append(out.Warnings, "读不到镜像 digest："+factsErr.Error()+
			"；这次更新照常进行，但记录里没有镜像指纹")
	} else if want := rendered.ExpectedImageDigest; want != "" {
		if facts.ID != want {
			out.Report = report
			if assessment.BackupRequired {
				s.restartQuietly(ctx, &report)
			}
			return out, &ServiceError{Code: "image_digest_mismatch",
				Message: fmt.Sprintf("定义里钉住的 digest 是 %s，docker 拉回来的是 %s；"+
					"镜像被动过或 tag 指向了别的字节，拒绝换上", want, facts.ID)}
		}
	}

	// 5. 留一份旧字节，写新配置。从这一步起回切有据可依。
	prevBytes, prevDigest, prevExists, err := s.applier.ReadConfig(assessment.ProjectName)
	if err != nil {
		return out, err
	}
	if !prevExists || prevDigest != assessment.CurrentConfigDigest {
		return out, &ServiceError{Code: "stale",
			Message: "配置在预检之后被改过（盘上的摘要对不上）；请重新预检"}
	}
	if err := s.applier.SavePreviousConfig(assessment.ProjectName, prevBytes); err != nil {
		return out, err
	}
	reportStep(progress, "writing", map[string]any{"unit": report.UnitName,
		"config_digest": assessment.ConfigDigest})
	report, err = s.applyNewConfig(ctx, deadline, plan, rendered, report)
	if err != nil {
		// 配置可能已经写了、单元没起来。这时最可靠的动作是回切到刚才那份字节。
		probe, rollbackErr := s.rollback(ctx, deadline, plan, assessment, prevBytes, progress)
		report.RolledBack = rollbackErr == nil || probe.Checked
		report.RollbackHealth = probe
		report.RollbackVerified = rollbackErr == nil && (!probe.Checked || probe.Healthy)
		if rollbackErr != nil {
			out.Report = report
			return out, &ServiceError{Code: "update_failed_rollback_failed",
				Message: "换上之后起不来：" + err.Error() + "；回切也失败了：" + rollbackErr.Error() +
					"。上一版配置留在 " + previousHint(assessment.ProjectName) + "，数据没有动，需要人工处理"}
		}
		out.Report = report
		return out, &ServiceError{Code: CodeUpdateFailedRolledBack,
			Message: "换上之后起不来：" + err.Error() + "；已经回切到上一版配置（" +
				assessment.CurrentConfigDigest + "）并重新拉起。数据没有回切——如果新版本已经动过数据，" +
				"旧版本可能读不了它"}
	}

	// 6. 健康探测。没启用就没什么可探的——不拿"单元 active"冒充"应用能应答"。
	if def.HealthCheck.Enabled {
		reportStep(progress, "probing", map[string]any{"port": def.HostPort, "kind": def.HealthCheck.Kind})
		probe := s.probeHealth(ctx, deadline, def)
		report.Health = probe
		if !probe.Healthy {
			oldProbe, rollbackErr := s.rollback(ctx, deadline, plan, assessment, prevBytes, progress)
			report.RolledBack = rollbackErr == nil
			report.RollbackHealth = oldProbe
			report.RollbackVerified = rollbackErr == nil && (!oldProbe.Checked || oldProbe.Healthy)
			if rollbackErr != nil {
				out.Report = report
				out.Warnings = append(out.Warnings, rollbackErr.Error())
				return out, &ServiceError{Code: "health_failed_rollback_failed",
					Message: "新版本健康探测失败（" + probeSummary(probe) + "），回切也失败了：" +
						rollbackErr.Error() + "；上一版配置留在 " + previousHint(assessment.ProjectName) +
						"，数据没有动"}
			}
			out.Report = report
			return out, &ServiceError{Code: CodeHealthFailedRolledBack,
				Message: "新版本健康探测失败（" + probeSummary(probe) + "）；已回切到上一版配置并重新拉起" +
					"（" + rollbackSummary(oldProbe) + "）。数据没有回切：如果新版本跑过" +
					"数据库迁移，旧版本可能读不了这份数据——这不是一次无损回滚"}
		}
	}

	// 7. 写记录：镜像 digest、上一版、更新时间。
	rec := DeploymentRecord{
		Schema:       RecordSchema,
		DeploymentID: assessment.DeploymentID,
		Name:         assessment.ProjectName,
		Definition:   def,
		ConfigDigest: assessment.ConfigDigest,
		Image:        report.Image,
		Previous: &ImageRecord{Requested: assessment.CurrentImage,
			Digest:       assessment.CurrentImageDigest,
			ConfigDigest: assessment.CurrentConfigDigest,
			ResolvedAt:   time.Now().UTC().Truncate(time.Second)},
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.applier.WriteRecord(rec); err != nil {
		out.Warnings = append(out.Warnings, "部署记录没写进去："+err.Error()+
			"；部署本身已经起来了，但下一次更新会因为没有记录被拒绝（重新 apply 可补上记录）")
	}

	out.Report = report
	out.Definition = def
	out.ProjectName = assessment.ProjectName
	out.Idempotent = !assessment.ConfigChanged
	out.Duration = time.Since(started).Milliseconds()
	return out, nil
}

// pullImage 在预算内拉一次镜像。
func (s *Service) pullImage(ctx context.Context, deadline time.Time, reference string,
	progress func(step string, fields map[string]any)) error {
	stepCtx, cancel, err := stepBudget(ctx, deadline, ImagePullTimeout, UpdateRollbackReserve)
	if err != nil {
		return err
	}
	defer cancel()
	reportStep(progress, "pulling", map[string]any{"image": reference})
	return s.applier.PullImage(stepCtx, reference)
}

// applyNewConfig 写新配置并让单元重跑 ExecStart（restart）。
func (s *Service) applyNewConfig(ctx context.Context, deadline time.Time, plan UnitPlan,
	rendered Rendered, report UpdateReport) (UpdateReport, error) {
	stepCtx, cancel, err := stepBudget(ctx, deadline, systemctlTimeout, UpdateRollbackReserve)
	if err != nil {
		return report, err
	}
	defer cancel()
	applied, err := s.applier.Apply(stepCtx, ApplyRequest{Plan: plan, Rendered: rendered,
		MustRun: true, ReplaceExisting: true})
	report.Enabled, report.Active, report.Restarted = applied.Enabled, applied.Active, applied.Restarted
	report.ConfigPath = applied.ConfigPath
	return report, err
}

// rollback 把上一版配置原样写回并重新拉起，然后再探一次健康。
//
// "原样"很重要：重新渲染会读一次此刻的秘密，秘密被轮换过的话，渲染出来的就不是
// 上一版了。回切要还原的是当时真正跑过的那份字节。
func (s *Service) rollback(ctx context.Context, deadline time.Time, plan UnitPlan,
	assessment UpdateAssessment, previous []byte, progress func(step string, fields map[string]any)) (HealthProbe, error) {
	var probe HealthProbe
	if len(previous) == 0 {
		return probe, errors.New("没有上一版配置的字节，无法回切")
	}
	configPath, err := s.applier.configFile(assessment.ProjectName)
	if err != nil {
		return probe, err
	}
	prevRendered := Rendered{
		Config:       previous,
		ConfigDigest: RenderedDigest(previous),
		ProjectName:  assessment.ProjectName,
	}
	if prevRendered.ConfigDigest != assessment.CurrentConfigDigest {
		return probe, fmt.Errorf("上一版配置的摘要 %s 与预检时记录的 %s 不一致：%s 上的字节被动过",
			prevRendered.ConfigDigest, assessment.CurrentConfigDigest, configPath)
	}
	reportStep(progress, "rolling_back", map[string]any{"config_digest": assessment.CurrentConfigDigest})
	if err := writeFileAtomic(configPath, previous, configFileMode); err != nil {
		return probe, err
	}
	stepCtx, cancel, err := stepBudget(ctx, deadline, systemctlTimeout, 0)
	if err != nil {
		return probe, err
	}
	defer cancel()
	applied, err := s.applier.Apply(stepCtx, ApplyRequest{Plan: plan, Rendered: prevRendered, MustRun: true})
	if err != nil {
		return probe, err
	}
	if !applied.Enabled || !applied.Active {
		return probe, fmt.Errorf("回切之后单元 %s 不是 active（enabled=%t）", applied.UnitName, applied.Enabled)
	}
	if assessment.Definition.HealthCheck.Enabled {
		// 更新只换镜像：健康检查的 kind/路径/端口都属于定义里没动的部分，所以上一版
		// 与这一版探的是同一个地址。
		probe = s.probeHealth(ctx, deadline, assessment.Definition)
	}
	return probe, nil
}

// restartQuietly 在"什么都没改"的失败路径上把单元按原样起回来。
//
// 只在停过单元的情况下需要（备份失败、拉镜像失败）。起不回来不吞掉：报告里
// UnitStopped 留着 true，调用方据此告诉人"应用可能还是停着的"。
//
// 用 RestartUnit 而不是 Apply：这一刻盘上的配置一个字节都没动，而这次要写的新字节
// 与它不同——走 apply 会被"不许覆盖别人的部署"那条判据挡住，于是"起回来"这一步
// 静默失败，而报告里 UnitStopped 还会被以为已经复位。单测第一次跑就撞上了。
func (s *Service) restartQuietly(ctx context.Context, report *UpdateReport) {
	if report == nil || !report.UnitStopped {
		return
	}
	restartCtx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	if err := s.applier.RestartUnit(restartCtx, report.UnitName); err != nil {
		return
	}
	report.UnitStopped = false
}

// imageRecordOf 把镜像事实折成记录用的形状。
func (s *Service) imageRecordOf(def Definition, rendered Rendered, facts ImageFacts, err error) ImageRecord {
	rec := ImageRecord{
		Requested:    def.ImageReference,
		ConfigDigest: rendered.ConfigDigest,
		ResolvedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err != nil {
		return rec
	}
	rec.Digest = facts.ID
	rec.RepoDigests = facts.RepoDigests
	if !facts.InspectedAt.IsZero() {
		rec.ResolvedAt = facts.InspectedAt
	}
	return rec
}

// probeHealth 在预算内做一次健康探测。
func (s *Service) probeHealth(ctx context.Context, deadline time.Time, def Definition) HealthProbe {
	target, err := healthTargetOf(def)
	if err != nil {
		return HealthProbe{Checked: false, LastError: err.Error()}
	}
	prober := s.prober
	if prober == nil {
		prober = DefaultHealthProber{}
	}
	want := HealthProbeBudget
	if left := time.Until(deadline); left < want {
		want = left
	}
	if want < HealthProbeMinAttemptTimeout {
		want = HealthProbeMinAttemptTimeout
	}
	return prober.Probe(ctx, target, want)
}

// ValidBackupPolicyID 判断备份策略标识的形状（pol_ 加 32 位小写十六进制）。
//
// 与 agentstore.ValidBackupPolicyID 同形；在部署包里再写一遍是**故意的**：
// 更新预检要在没有数据库的单元测试里跑。
func ValidBackupPolicyID(id string) bool { return backupPolicyIDPattern.MatchString(id) }

// previousHint 指出回切字节在哪儿（错误消息里给人看的路径）。
func previousHint(name string) string {
	path, err := PreviousConfigPath(name)
	if err != nil {
		return "（路径算不出来）"
	}
	return path
}

// probeSummary 把一次探测结论说成一句人话。
func probeSummary(p HealthProbe) string {
	if !p.Checked {
		return "没有探测：" + p.LastError
	}
	parts := []string{fmt.Sprintf("%d 次尝试", p.Attempts)}
	if p.StatusCode != nil {
		parts = append(parts, fmt.Sprintf("最后状态码 %d", *p.StatusCode))
	}
	if p.LastError != "" {
		parts = append(parts, p.LastError)
	}
	if p.Truncated {
		parts = append(parts, "预算不足，没有按定义探满")
	}
	if p.URL != "" {
		parts = append(parts, p.URL)
	}
	return strings.Join(parts, "；")
}

// rollbackSummary 是回切之后那句结论。
func rollbackSummary(p HealthProbe) string {
	if !p.Checked {
		return "回切之后没有探测"
	}
	if p.Healthy {
		return "上一版重新探测通过"
	}
	return "上一版重新探测也没通过：" + probeSummary(p)
}

// operationDeadline 是这次操作的时间底线：ctx 的截止时间与 fallback 取更早的那个。
func operationDeadline(ctx context.Context, fallback time.Duration, now time.Time) time.Time {
	deadline := now.Add(fallback)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		return dl
	}
	return deadline
}

// stepBudget 给一步收窄后的 ctx。
//
// want 是这一步通常需要的上限，reserve 是要留给后面（尤其是回切）的时间。
// 剩下的预算不够 reserve + UpdateMinStep 时**直接失败**：这一步一开始就注定做不完，
// 做到一半被操作超时掐断只会留下更难解释的状态。
func stepBudget(ctx context.Context, deadline time.Time, want, reserve time.Duration) (context.Context, context.CancelFunc, error) {
	left := time.Until(deadline) - reserve
	if left < UpdateMinStep {
		return ctx, func() {}, &ServiceError{Code: "budget_exhausted",
			Message: fmt.Sprintf("这次更新剩下的时间不够这一步（剩 %s，还要给回切留 %s）："+
				"已经完成的部分不会被撤销，但没有开始的部分不会再开始。请重新提交这次更新",
				time.Until(deadline).Truncate(time.Second), reserve.Truncate(time.Second))}
	}
	if want <= 0 || want > left {
		want = left
	}
	stepCtx, cancel := context.WithTimeout(ctx, want)
	return stepCtx, cancel, nil
}
