package agent

// 更新前备份的适配层（P15.3）：把 internal/backup 的策略与运行接到 deployment.update 上。
//
// 为什么适配层在这里而不在 internal/deploy：备份那一包要知道仓库镜像、密钥环、restic
// 路径、面板库副本目录与应用控制，这些东西的装配全在代理进程里；部署那一包只该说
// "我要在更新前跑一次这条策略的备份，成不成你告诉我"。两边的边界就是那个接口
// （deploy.BackupCause），不是一方的内部结构。
//
// 一条明确的限制：**只接受 files_only 策略**。
//
//	stop_managed_apps 策略的含义是"备份前把这条策略管的每个应用都停下来"，
//	而"应用 → systemd 单元 / 容器 id"的绑定只有 Web 侧知道（backup.run 的参数里
//	带的是 Web 展开出来的 apps，见 backuprun.go 的说明）。deployment.update 的参数里
//	没有这个字段，代理不能凭空发明一份——少停一个应用，快照照样建得出来，但它会被
//	打上 consistency=stop_managed_apps 的标签而实际是在应用写着的中间态下读的。
//	那是一份骗人的备份，比没有备份糟。所以这种策略在预检里被挡住并说清出路：
//	更新自己会先停掉这套部署（它本来就是这次更新的对象），要一致的数据请选一条
//	只备份目录的 files_only 策略。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"upanel/internal/backup"
	"upanel/internal/deploy"
)

// deployBackupModule 是适配层需要的备份能力（生产实现是 *backup.Module）。
//
// 只列这四个方法：更新前备份用不到初始化仓库、恢复、清理。接口越小，
// 将来给部署换一个备份实现时越容易。
type deployBackupModule interface {
	GetPolicy(ctx context.Context, id string) (backup.PolicyRecord, bool, error)
	GetRepository(ctx context.Context, id string) (backup.Repository, bool, error)
	AssessRun(ctx context.Context, req backup.RunAssessRequest) (backup.RunAssessment, error)
	Run(ctx context.Context, req backup.RunRequest) (backup.RunResult, error)
}

// DeployBackup 把备份模块适配成 deploy.BackupCause。
type DeployBackup struct {
	Module deployBackupModule
}

// NewDeployBackup 构造适配层；模块为 nil 时返回 nil（调用方据此不接备份能力）。
func NewDeployBackup(mod deployBackupModule) *DeployBackup {
	if mod == nil {
		return nil
	}
	return &DeployBackup{Module: mod}
}

// AssessBackup 预检一次"更新前备份"。
func (b *DeployBackup) AssessBackup(ctx context.Context, policyID string) (deploy.BackupAssessment, error) {
	out := deploy.BackupAssessment{PolicyID: policyID}
	if b == nil || b.Module == nil {
		out.Blockers = append(out.Blockers, deploy.Notice{Code: "backup_unavailable",
			Message: "这台机器上没有可用的备份能力"})
		return out, nil
	}
	rec, found, err := b.Module.GetPolicy(ctx, policyID)
	if err != nil {
		return out, err
	}
	if !found {
		out.Blockers = append(out.Blockers, deploy.Notice{Code: "backup_policy_not_found",
			Message: fmt.Sprintf("备份策略 %s 不在代理里（可能已删除）", policyID)})
		return out, nil
	}
	pol := rec.Policy
	if pol.ConsistencyMode == backup.ConsistencyStopManagedApps || len(pol.AppIDs) > 0 {
		out.Blockers = append(out.Blockers, deploy.Notice{Code: "backup_policy_needs_apps",
			Message: "更新前备份只接受 files_only 策略：这条策略要求备份前停掉受管应用，" +
				"而应用绑定只有面板侧知道，deployment.update 的参数里没有它。" +
				"这次更新本身会先停掉这套部署再备份，所以请改选一条只备份目录的策略；" +
				"要连别的应用一起停，请单独跑 backup.run"})
		return out, nil
	}
	repo, found, err := b.Module.GetRepository(ctx, pol.RepositoryID)
	if err != nil {
		return out, err
	}
	if !found {
		out.Blockers = append(out.Blockers, deploy.Notice{Code: backup.CodeRepositoryMissing,
			Message: fmt.Sprintf("策略指向的仓库 %s 不在代理里（可能已删除）", pol.RepositoryID)})
		return out, nil
	}
	out.RepositoryID = repo.ID
	out.Description = fmt.Sprintf("策略「%s」，仓库 %s，%d 条来源，口径 %s",
		rec.Name, repo.ID, len(pol.Sources), pol.ConsistencyMode)
	assessment, err := b.Module.AssessRun(ctx, backup.RunAssessRequest{
		PolicyID:   rec.ID,
		Policy:     pol,
		Repository: repo,
	})
	if err != nil {
		return out, err
	}
	out.Allowed = assessment.Allowed
	for _, blocker := range assessment.Blockers {
		out.Blockers = append(out.Blockers, deploy.Notice{Code: blocker.Code, Message: blocker.Message})
	}
	for _, warning := range assessment.Warnings {
		out.Warnings = append(out.Warnings, deploy.Notice{Code: warning.Code, Message: warning.Message})
	}
	out.Locks = []string{backup.PolicyLockName(rec.ID), backup.LockName(repo.ID)}
	return out, nil
}

// RunBackup 执行一次"更新前备份"。
func (b *DeployBackup) RunBackup(ctx context.Context, policyID, operationID string) (deploy.BackupOutcome, error) {
	out := deploy.BackupOutcome{PolicyID: policyID}
	if b == nil || b.Module == nil {
		return out, fmt.Errorf("这台机器上没有可用的备份能力")
	}
	// 执行前重新取一遍策略与仓库：预检与执行之间策略可能被改过（比如被改成
	// stop_managed_apps，那时就不能按 files_only 的口径跑了）。
	rec, found, err := b.Module.GetPolicy(ctx, policyID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("备份策略 %s 在执行前不见了", policyID)
	}
	if rec.Policy.ConsistencyMode == backup.ConsistencyStopManagedApps || len(rec.Policy.AppIDs) > 0 {
		return out, fmt.Errorf("策略 %s 在执行前被改成了 stop_managed_apps：更新前备份只接受 files_only 口径", policyID)
	}
	repo, found, err := b.Module.GetRepository(ctx, rec.Policy.RepositoryID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("%s：策略指向的仓库 %s 在执行前不见了", backup.CodeRepositoryMissing, rec.Policy.RepositoryID)
	}
	out.RepositoryID = repo.ID
	started := time.Now()
	result, err := b.Module.Run(ctx, backup.RunRequest{
		PolicyID:    rec.ID,
		Policy:      rec.Policy,
		Repository:  repo,
		OperationID: operationID,
	})
	out.Duration = time.Since(started)
	out.SnapshotID = strings.TrimSpace(result.SnapshotID)
	out.Verified = result.Verified
	if out.SnapshotID != "" {
		parts := []string{"快照 " + out.SnapshotID}
		if out.Verified {
			parts = append(parts, "已读回核对")
		} else if result.VerifyError != "" {
			parts = append(parts, "读回核对失败："+result.VerifyError)
		}
		out.Summary = strings.Join(parts, "，")
	}
	return out, err
}
