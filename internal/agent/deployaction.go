package agent

// 部署动作（P15.2 / P15.3）：deployment.apply、deployment.remove 与 deployment.update。
//
// 三条动作共用一套骨架（参数解码 → 预检 → 执行前复核 → 执行 → 读回），判决与落地都在
// internal/deploy 里：这一层只负责把预检结论翻译成计划、把执行结果翻译成 outcome，
// 并在单元起来之后多问一句“那个 compose 项目下真的有容器吗”。
//
// 策略（CONTRACTS.md 第 5 节 R 列）：
//   - deployment.apply  R=是：写单元、拉起容器、占端口
//   - deployment.remove R=是：停下整套部署
//   - deployment.update R=是：换镜像、重启服务、可能先做一次备份
//
// 三条都不需要确认短语：apply 的对象是一个还没起来的东西（错了就删），remove 保留
// 数据卷与业务目录（CONTRACTS §5 明文），update 在失败时会回切到上一版配置——没有
// 一条是“按下去就收不回”的破坏。三条都不可取消：单元文件已经落盘、容器已经换过之后，
// 中途停下只会留下一个说不清状态的部署。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/containers"
	"upanel/internal/deploy"
)

// DeploySource 是部署动作的实现端（生产实现是 *deploy.Service）。
//
// 六个方法必须一起接：只有评估没有执行的实现会让预检显示“可以部署”、提交后却
// 501；只有执行没有评估的实现连“能不能部署”都答不出来。
type DeploySource interface {
	AssessApply(ctx context.Context, p deploy.ApplyParams) (deploy.ApplyAssessment, error)
	ExecuteApply(ctx context.Context, p deploy.ApplyParams, expectState string,
		progress func(step string, fields map[string]any)) (deploy.ApplyOutcome, error)
	AssessRemove(ctx context.Context, p deploy.RemoveParams) (deploy.RemoveAssessment, error)
	ExecuteRemove(ctx context.Context, p deploy.RemoveParams, expectState string,
		progress func(step string, fields map[string]any)) (deploy.RemoveOutcome, error)
	AssessUpdate(ctx context.Context, p deploy.UpdateParams) (deploy.UpdateAssessment, error)
	ExecuteUpdate(ctx context.Context, p deploy.UpdateParams, expectState, operationID string,
		progress func(step string, fields map[string]any)) (deploy.UpdateOutcome, error)
}

// deployProjectLister 是“按项目看容器”的最小接口（生产实现是 *containers.Client）。
//
// 只有读：部署自己起容器，动作层不去停别人的东西。取这一个方法而不是整个容器接口，
// 是因为这里要的是“核对”，不是“管理”。
type deployProjectLister interface {
	Projects(ctx context.Context) (containers.ProjectsResponse, error)
}

// RegisterDeployActions 登记三条部署动作。
func RegisterDeployActions(reg *Registry, src DeploySource, projects deployProjectLister) error {
	for _, action := range []Action{
		newDeployApplyAction(src, projects),
		newDeployUpdateAction(src, projects),
		newDeployRemoveAction(src),
	} {
		if err := reg.RegisterAction(action); err != nil {
			return fmt.Errorf("代理登记 %s 失败: %w", action.Name, err)
		}
	}
	return nil
}

// deploymentFingerprint 是部署的身份指纹（执行前复核要看到同一个值）。
//
// 用名字派生的部署标识而不是名字：标识别名一致才算“还是那一份部署”，而名字
// 是给人看的——同一个人可能把名字拼错两次，那两次不该被当成同一份。
func deploymentFingerprint(deploymentID string) string {
	return "deployment:" + deploymentID
}

// newDeployApplyAction 登记 deployment.apply。
func newDeployApplyAction(src DeploySource, projects deployProjectLister) Action {
	return Action{
		Name: deploy.ActionApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p deploy.ApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.Version = strings.TrimSpace(p.Version)
			if p.DeploymentID != nil {
				id := strings.TrimSpace(*p.DeploymentID)
				p.DeploymentID = &id
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(deploy.ApplyParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: deployment.apply 的参数类型是 %T", ErrPlanFailed, in.Params)
			}
			assessment, err := src.AssessApply(ctx, p)
			if err != nil {
				return deployPlanFromError(err, in.Params)
			}
			warnings := noticesOfDeploy(assessment.Warnings)
			blockers := noticesOfDeploy(assessment.Blockers)
			impacts := []Impact{}
			if assessment.UnitName != "" {
				impacts = append(impacts,
					Impact{
						Kind: "unit_file",
						Description: "写入包装单元 " + assessment.UnitName +
							"（/etc/systemd/system，0644）并 enable：systemd 成为这套部署唯一的生命周期来源，" +
							"开机会把它拉起",
					},
					Impact{
						Kind: "compose_config",
						Description: "写入渲染后的 Compose 配置 " +
							deploy.DeployConfigPath(assessment.ProjectName) +
							"（0600 root：里面有 app_env 解出来的明文秘密）",
					},
					Impact{
						Kind: "container_start",
						Description: "以 compose 项目名 " + assessment.ProjectName +
							" 拉起容器，占用声明的宿主端口；容器写的是受管理卷上的目录",
					},
				)
			}
			return Plan{
				NormalizedParams: in.Params,
				Target: Target{
					DisplayName:         "部署 " + assessment.ProjectName,
					IdentityFingerprint: deploymentFingerprint(assessment.DeploymentID),
					// 执行前复核用的版本：已有部署时是磁盘上那份配置的摘要，新建时是这一次
					// 将要落盘的摘要（磁盘上还没有旧版本，但令牌不能为空——代理层要求
					// allowed 的预检绑定一个可复核的状态快照）。它同时是"别人刚动过这份
					// 部署"的唯一判据：单元文件是生成的字节，配置摘要是它唯一的内容指纹。
					StateVersion: assessment.StateVersion,
				},
				Allowed:  assessment.Allowed,
				Blockers: blockers,
				Warnings: warnings,
				Impacts:  impacts,
				Locks:    assessment.Locks(),
			}, nil
		},
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(deploy.ApplyParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: deployment.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			out, err := src.ExecuteApply(ctx, p, in.Target.StateVersion, in.Progress)
			if err != nil {
				return Result{}, deployExecuteError(err)
			}
			observed := map[string]any{
				"deployment_id": out.Report.DeploymentID,
				"project":       out.ProjectName,
				"unit":          out.Report.UnitName,
				"unit_path":     out.Report.UnitPath,
				"config_path":   out.Report.ConfigPath,
				"config_digest": out.Report.ConfigDigest,
				"written":       out.Report.Written,
				"idempotent":    out.Idempotent,
				// restart 还是 start：单元本来就在盘上、这次又写了新内容时，只有
				// restart 会让 ExecStart 重跑（start 对 active 的 oneshot 是空操作）。
				"unit_restarted": out.Report.Restarted,
				// 两个读数来自 systemctl 的回读，不是 compose up 的退出码。
				"unit_enabled": out.Report.Enabled,
				"unit_active":  out.Report.Active,
			}
			result := Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC().Truncate(time.Second),
				ObservedState: observed,
				Data: map[string]any{
					"deployment_id": out.Report.DeploymentID,
					"project":       out.ProjectName,
					"unit":          out.Report.UnitName,
					"config_digest": out.Report.ConfigDigest,
				},
			}
			deployRecordHealth(observed, out.Health)
			if len(out.Warnings) > 0 {
				observed["warnings"] = out.Warnings
			}
			verifyDeployProject(ctx, projects, out, observed, &result)
			// 健康探测没探通也降级：单元 active、容器在跑、应用不应答，是管理员必须
			// 现在就知道的一件事。容器那条提示更具体，所以先让 verify 写，再补这条。
			if out.Health.Checked && !out.Health.Healthy && result.Outcome == OutcomeVerified {
				result.Outcome = OutcomePartial
				result.RecoveryHint = "部署已经落地（单元 active、容器在跑），但对公开入口 " +
					out.Health.URL + " 的探测没探通（" + deployHealthError(out.Health) + "）；" +
					"先看容器日志确认应用有没有起来"
			}
			return result, nil
		},
	}
}

// verifyDeployProject 在单元起来之后再问一句“容器真的有吗”。
//
// 三种情况分开处理，因为它们对管理员意味着三件不同的事：
//
//	读不到（没有容器读取端、docker 不可用、查询报错）—— 单元已经 active，部署已经
//	落地；这里只把“容器状态没核对”写进 observed，不把一个成功的部署降级成说不清。
//	  一个成功的部署在审计里被标成 unknown，比标错更糟：人会去查一件没坏的事。
//	项目下 0 个容器 —— 单元 active 但 compose 什么都没起，这是真的不对（配置里至少
//	  有一个服务），降级成 partial 并给出看哪两条日志。
//	有容器 —— 记下总数与运行数。
func verifyDeployProject(ctx context.Context, projects deployProjectLister, out deploy.ApplyOutcome,
	observed map[string]any, result *Result) {
	verifyDeployProjectByID(ctx, projects, out.Report.DeploymentID, out.ProjectName, out.Report.UnitName,
		observed, result)
}

// verifyDeployProjectByID 是 verifyDeployProject 的共用内核（apply 与 update 都用它）。
//
// update 拿不到 ApplyOutcome（它自己的报告是 UpdateReport），但要看的是同一件事：
// 这个部署标识下现在有几个容器、跑着几个。留一份带 ID 的版本，比让两条动作各自
// 复制一遍“读不到怎么办”的分支要好——那两段分支迟早会分叉。
func verifyDeployProjectByID(ctx context.Context, projects deployProjectLister, deploymentID, projectName,
	unitName string, observed map[string]any, result *Result) {
	if projects == nil {
		observed["containers_checked"] = false
		return
	}
	response, err := projects.Projects(ctx)
	if err != nil {
		observed["containers_checked"] = false
		observed["containers_error"] = err.Error()
		return
	}
	if !response.Available {
		observed["containers_checked"] = false
		observed["containers_error"] = strings.TrimSpace(response.Notice)
		return
	}
	observed["containers_checked"] = true
	for _, project := range response.Projects {
		if project.ID != deploymentID {
			continue
		}
		observed["containers_total"] = project.Total
		observed["containers_running"] = project.Running
		if project.Total > 0 {
			return
		}
		break
	}
	if _, seen := observed["containers_total"]; !seen {
		observed["containers_total"] = 0
		observed["containers_running"] = 0
	}
	result.Outcome = OutcomePartial
	result.RecoveryHint = "包装单元已经 active，但 compose 项目 " + projectName +
		" 下没有容器；先看 `docker compose -p " + projectName + " ps` 与 `journalctl -u " +
		unitName + "`，确认容器是被删了还是根本没起来"
}

// newDeployUpdateAction 登记 deployment.update（P15.3）。
//
// 它和 apply 的关系：apply 只能在一个名字**还没有**部署时用，update 只能在一个名字
// **已经有**部署记录时用（记录是 P15.3 新增的 `<name>.deployment.json`）。这条分界
// 是故意的——一份没有记录的部署，我们不知道它当前钉的是哪个镜像摘要，也就说不出
// "回切到的是什么"。
func newDeployUpdateAction(src DeploySource, projects deployProjectLister) Action {
	return Action{
		Name: deploy.ActionUpdate,
		Decode: func(raw json.RawMessage) (any, error) {
			var p deploy.UpdateParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.DeploymentID = strings.TrimSpace(p.DeploymentID)
			p.Version = strings.TrimSpace(p.Version)
			p.ImageReference = strings.TrimSpace(p.ImageReference)
			if p.BackupPolicyID != nil {
				id := strings.TrimSpace(*p.BackupPolicyID)
				p.BackupPolicyID = &id
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(deploy.UpdateParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: deployment.update 的参数类型是 %T", ErrPlanFailed, in.Params)
			}
			assessment, err := src.AssessUpdate(ctx, p)
			if err != nil {
				return deployPlanFromError(err, in.Params)
			}
			display := "更新部署 " + assessment.ProjectName
			if strings.TrimSpace(assessment.TargetImage) != "" {
				display = fmt.Sprintf("更新部署 %s（%s → %s）", assessment.ProjectName,
					orDash(assessment.CurrentImage), assessment.TargetImage)
			}
			impacts := []Impact{}
			if assessment.UnitName != "" {
				if assessment.BackupRequired {
					impacts = append(impacts, Impact{
						Kind: "backup_before_update",
						Description: "先停掉包装单元 " + assessment.UnitName + "，再按备份策略 " +
							assessment.BackupPolicyID + " 做一次备份并读回核对；备份没建成就不换镜像，" +
							"单元按原样起回。备份本身受 360 秒预算约束，超过就整轮放弃",
					})
				}
				if strings.TrimSpace(assessment.TargetImage) != "" {
					impacts = append(impacts, Impact{
						Kind: "image_pull",
						Description: "拉取镜像 " + assessment.TargetImage +
							"；定义里钉了 digest 时会核对拉回来的镜像 id 是否就是它，不符就拒绝换上",
					})
				}
				impacts = append(impacts, Impact{
					Kind: "unit_restart",
					Description: "写入新的 Compose 配置（旧字节留在 /etc/upanel/deploy/" +
						assessment.ProjectName + ".compose.yaml.prev），然后 restart 包装单元 " +
						assessment.UnitName + "：容器先停后起，这段时间端口不提供服务",
				})
				if assessment.HealthCheckEnabled {
					impacts = append(impacts, Impact{
						Kind: "health_probe_rollback",
						Description: "起来之后从面板侧探测公开入口；探不通就回切到上一版配置并重新拉起。" +
							"回切的是配置与镜像，**数据不会跟着回切**",
					})
				}
				impacts = append(impacts, Impact{
					Kind: "deployment_record",
					Description: "更新部署记录 /etc/upanel/deploy/" + assessment.ProjectName +
						".deployment.json（0640，含镜像 digest 与上一版摘要，不含明文秘密）",
				})
			}
			return Plan{
				NormalizedParams: in.Params,
				Target: Target{
					DisplayName:         display,
					IdentityFingerprint: deploymentFingerprint(assessment.DeploymentID),
					// 复核令牌是盘上那份配置的摘要：预检与执行之间有人改过配置（或换了
					// 版本令牌）就答 stale，而不是把别人的改动盖掉。
					StateVersion: assessment.StateVersion,
				},
				Allowed:  assessment.Allowed,
				Blockers: noticesOfDeploy(assessment.Blockers),
				Warnings: noticesOfDeploy(assessment.Warnings),
				Impacts:  impacts,
				Locks:    assessment.Locks(),
			}, nil
		},
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(deploy.UpdateParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: deployment.update 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			out, err := src.ExecuteUpdate(ctx, p, in.Target.StateVersion, in.OperationID, in.Progress)
			if err != nil {
				// 回切成功不是“执行失败”：现场已经回到上一版、单元也起来了，剩下的问题
				// 只有一个“新版本为什么没成”。按失败记会把读回值全部丢掉（失败任务的
				// result 是空对象），于是审计里只剩一句话——连当前跑的是哪个版本都看不出来。
				if deployRolledBackCode(err) {
					result := deployUpdateResult(ctx, projects, out)
					result.Outcome = OutcomeRolledBack
					result.RecoveryHint = deployFailureReason(err)
					return result, nil
				}
				return Result{}, deployExecuteError(err)
			}
			return deployUpdateResult(ctx, projects, out), nil
		},
	}
}

// deployUpdateResult 把一次更新的读回事实翻成代理结果（成功与回切共用）。
func deployUpdateResult(ctx context.Context, projects deployProjectLister,
	out deploy.UpdateOutcome) Result {
	report := out.Report
	observed := map[string]any{
		"deployment_id":          report.DeploymentID,
		"project":                report.ProjectName,
		"unit":                   report.UnitName,
		"config_path":            report.ConfigPath,
		"previous_config_digest": report.PreviousConfigDigest,
		"config_digest":          report.ConfigDigest,
		"config_changed":         report.PreviousConfigDigest != report.ConfigDigest,
		"image":                  report.Image.Requested,
		"image_digest":           report.Image.Digest,
		"image_digest_known":     report.ImageDigestKnown,
		"idempotent":             out.Idempotent,
		// restart 而不是 start：单元文件这次一个字节都没变，只有 restart 会让
		// ExecStart 重跑（start 对 active 的 oneshot 是空操作，Q485）。
		"unit_restarted": report.Restarted,
		"unit_enabled":   report.Enabled,
		"unit_active":    report.Active,
		"unit_stopped":   report.UnitStopped,
		"rolled_back":    report.RolledBack,
	}
	if !report.Image.ResolvedAt.IsZero() {
		observed["image_resolved_at"] = report.Image.ResolvedAt.Format(time.RFC3339)
	}
	if report.Backup.SnapshotID != "" || report.Backup.PolicyID != "" {
		observed["backup_policy_id"] = report.Backup.PolicyID
		observed["backup_repository_id"] = report.Backup.RepositoryID
		observed["backup_snapshot_id"] = report.Backup.SnapshotID
		observed["backup_verified"] = report.Backup.Verified
		observed["backup_summary"] = report.Backup.Summary
	}
	deployRecordHealth(observed, report.Health)
	if report.RolledBack {
		observed["rollback_health_checked"] = report.RollbackHealth.Checked
		observed["rollback_health_ok"] = report.RollbackHealth.Healthy
		observed["rollback_verified"] = report.RollbackVerified
		if report.RollbackHealth.LastError != "" {
			observed["rollback_health_error"] = report.RollbackHealth.LastError
		}
	}
	data := map[string]any{
		"deployment_id": report.DeploymentID,
		"project":       report.ProjectName,
		"unit":          report.UnitName,
		"config_digest": report.ConfigDigest,
		"image":         report.Image.Requested,
	}
	if report.Backup.SnapshotID != "" {
		data["backup_snapshot_id"] = report.Backup.SnapshotID
	}
	result := Result{
		Outcome:       OutcomeVerified,
		VerifiedAt:    time.Now().UTC().Truncate(time.Second),
		ObservedState: observed,
		Data:          data,
	}
	if len(out.Warnings) > 0 {
		// Result 没有 warnings 字段：告警和执行期读回值同属“我看见了什么”，
		// 于是放在 observed_state 里，一起进审计。
		observed["warnings"] = out.Warnings
	}
	verifyDeployProjectByID(ctx, projects, report.DeploymentID, report.ProjectName, report.UnitName,
		observed, &result)
	return result
}

// deployRolledBackCode 判断这次失败是不是“已经回切成功”（现场在上一版，单元已起来）。
func deployRolledBackCode(err error) bool {
	var serviceErr *deploy.ServiceError
	if !errors.As(err, &serviceErr) {
		return false
	}
	switch serviceErr.Code {
	case deploy.CodeUpdateFailedRolledBack, deploy.CodeHealthFailedRolledBack:
		return true
	}
	return false
}

// deployFailureReason 取一句给人看的原因（FailureReasoner 或错误原文）。
func deployFailureReason(err error) string {
	var reasoner interface{ FailureReason() string }
	if errors.As(err, &reasoner) {
		return reasoner.FailureReason()
	}
	var serviceErr *deploy.ServiceError
	if errors.As(err, &serviceErr) {
		return serviceErr.Message
	}
	return err.Error()
}

// deployRecordHealth 把一次健康探测的读数写进 observed。
//
// Checked=false 也要写：`health_checked=false` 是一个事实（没探），不写等于让读的人
// 分不清“探通了”和“没探过”。
func deployRecordHealth(observed map[string]any, probe deploy.HealthProbe) {
	observed["health_checked"] = probe.Checked
	if !probe.Checked {
		if probe.LastError != "" {
			observed["health_error"] = probe.LastError
		}
		return
	}
	observed["health_ok"] = probe.Healthy
	observed["health_attempts"] = probe.Attempts
	observed["health_url"] = probe.URL
	observed["health_duration_ms"] = probe.DurationMS
	if probe.StatusCode != nil {
		observed["health_status_code"] = *probe.StatusCode
	}
	if probe.LastError != "" {
		observed["health_error"] = probe.LastError
	}
	if probe.Truncated {
		observed["health_truncated"] = true
	}
}

// deployHealthError 取一句“为什么没探通”。
func deployHealthError(probe deploy.HealthProbe) string {
	if probe.LastError != "" {
		return probe.LastError
	}
	if probe.StatusCode != nil {
		return fmt.Sprintf("最后一次应答是 %d", *probe.StatusCode)
	}
	return "没有应答"
}

// orDash 把空串显示成一句话（计划文本里“从什么换到什么”不能出现空白）。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（未记录）"
	}
	return s
}

// newDeployRemoveAction 登记 deployment.remove。
func newDeployRemoveAction(src DeploySource) Action {
	return Action{
		Name: deploy.ActionRemove,
		Decode: func(raw json.RawMessage) (any, error) {
			var p deploy.RemoveParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.DeploymentID = strings.TrimSpace(p.DeploymentID)
			p.Version = strings.TrimSpace(p.Version)
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(deploy.RemoveParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: deployment.remove 的参数类型是 %T", ErrPlanFailed, in.Params)
			}
			assessment, err := src.AssessRemove(ctx, p)
			if err != nil {
				return deployPlanFromError(err, in.Params)
			}
			display := "部署 " + assessment.ProjectName
			if strings.TrimSpace(assessment.ProjectName) == "" {
				display = "部署（未找到）"
			}
			impacts := []Impact{}
			if assessment.UnitName != "" {
				impacts = append(impacts,
					Impact{
						Kind: "unit_stop",
						Description: "停掉并 disable 包装单元 " + assessment.UnitName +
							"（ExecStop 跑 compose down：容器被删掉），然后删除单元文件与渲染配置",
					},
					Impact{
						Kind: "data_kept",
						Description: "数据卷与业务目录一个字节都不动：删除部署不等于删除数据，" +
							"要清数据得单独用文件管理与卷管理",
					},
				)
			}
			return Plan{
				NormalizedParams: in.Params,
				Target: Target{
					DisplayName:         display,
					IdentityFingerprint: deploymentFingerprint(assessment.DeploymentID),
					StateVersion:        assessment.StateVersion,
				},
				Allowed:  assessment.Allowed,
				Blockers: noticesOfDeploy(assessment.Blockers),
				Warnings: noticesOfDeploy(assessment.Warnings),
				Impacts:  impacts,
				Locks:    assessment.Locks(),
			}, nil
		},
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(deploy.RemoveParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: deployment.remove 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			out, err := src.ExecuteRemove(ctx, p, in.Target.StateVersion, in.Progress)
			if err != nil {
				return Result{}, deployExecuteError(err)
			}
			observed := map[string]any{
				"deployment_id":  out.Report.DeploymentID,
				"project":        out.ProjectName,
				"unit":           out.Report.UnitName,
				"unit_path":      out.Report.UnitPath,
				"config_path":    out.Report.ConfigPath,
				"config_digest":  out.Report.ConfigDigest,
				"unit_stopped":   out.Report.Stopped,
				"unit_removed":   out.Report.UnitRemoved,
				"config_removed": out.Report.ConfigRemoved,
				"data_kept":      true,
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC().Truncate(time.Second),
				ObservedState: observed,
				Data: map[string]any{
					"deployment_id": out.Report.DeploymentID,
					"project":       out.ProjectName,
					"data_kept":     true,
				},
			}, nil
		},
	}
}

// noticesOfDeploy 把 deploy 的通知翻译成代理的通知（同一个 code，同一句话）。
func noticesOfDeploy(raw []deploy.Notice) []Notice {
	out := make([]Notice, 0, len(raw))
	for _, n := range raw {
		out = append(out, Notice{Code: n.Code, Message: n.Message})
	}
	return out
}

// deployPlanFromError 把 deploy 的两种拒绝翻成预检能表达的东西。
//
//   - ServiceError：这是**判决**，不是故障（"这个版本不做更新"、"秘密读不到"）。
//     它变成一条 blocker，人看见的是"为什么现在不能部署"，而不是一个 500。
//   - ValidationError：定义本身不自洽。它是参数问题，走 400，与其它动作一致。
//   - 其余：真故障（读不到卷表、写不了临时文件……），交给上层记日志。
//
// params 必须原样带进返回的计划：一条 blocked 的计划照样要能回答"你评估的是哪一份
// 参数"。漏掉它，`validatePlanResult` 会在"缺少规范化参数"这一行把判决变成实现错误，
// 用户拿到 500 而不是"这一版不做更新"——真机 P15.2 验收 §7a 撞到的就是它。
func deployPlanFromError(err error, params any) (Plan, error) {
	var serviceErr *deploy.ServiceError
	if errors.As(err, &serviceErr) {
		return Plan{
			NormalizedParams: params,
			Allowed:          false,
			Blockers:         []Notice{{Code: serviceErr.Code, Message: serviceErr.Message}},
		}, nil
	}
	var validation *deploy.ValidationError
	if errors.As(err, &validation) {
		field := validation.Field
		if field == "" {
			field = "definition"
		}
		return Plan{}, fmt.Errorf("%w: %s：%s", ErrParamsInvalid, field, validation.Message)
	}
	return Plan{}, fmt.Errorf("%w: %v", ErrPlanFailed, err)
}

// deployExecuteError 把执行期的拒绝变成任务的 error.message。
//
// 失败任务的 result 按契约是空对象，error.message 是管理员唯一能看到原因的地方；
// 不包这一层，一次"卷在预检之后被卸下"只会显示成"动作执行失败"。
func deployExecuteError(err error) error {
	var serviceErr *deploy.ServiceError
	switch {
	case errors.As(err, &serviceErr):
		return deployFailure{err: err,
			reason: fmt.Sprintf("被拒绝（%s）：%s", serviceErr.Code, serviceErr.Message)}
	case errors.Is(err, deploy.ErrDeploymentExists):
		return deployFailure{err: err,
			reason: "这个名字上已经有一份内容不同的部署；换镜像用 deployment.update，" +
				"换端口/挂载/环境变量要删除重建"}
	case errors.Is(err, deploy.ErrUnitStillActive):
		return deployFailure{err: err,
			reason: "单元停不下来，所以没有删除任何文件；先看 `systemctl status` " +
				"与 `journalctl -u`，确认它为什么还在跑"}
	case errors.Is(err, deploy.ErrNoDeployment):
		return deployFailure{err: err, reason: "找不到这份部署（单元文件已经不在磁盘上）"}
	case errors.Is(err, deploy.ErrVersionMismatch):
		return deployFailure{err: err, reason: "磁盘上那份配置与请求里的版本不一致；请重新预检"}
	}
	return err
}

// deployFailure 保留原错误链，并给出一句给人看的原因（见 FailureReasoner）。
type deployFailure struct {
	err    error
	reason string
}

func (f deployFailure) Error() string { return f.err.Error() }

func (f deployFailure) Unwrap() error { return f.err }

func (f deployFailure) FailureReason() string {
	if strings.TrimSpace(f.reason) != "" {
		return f.reason
	}
	return f.err.Error()
}
