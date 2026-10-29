package backup

// Module 把定义镜像与执行端合成一个值（P12.1 的装配形状）。
//
// 代理的动作层需要四件事：按 id 取定义（来自镜像库）、估一次预检、执行一次初始化、
// 读一次现状。前三件分属两个对象——镜像库归 agentstore，其余归 Ops——所以这里只做
// 一次转发，而不是让动作层同时握两个字段并自己记住“哪个方法归谁”。
//
// 为什么不把 GetRepository 也塞进 Ops：Ops 的职责是“看机器”，镜像库的职责是
// “记住 Web 说过什么”。把后者塞进前者，会让 Ops 的每个测试都得先造一张表。

import "context"

// Module 是动作层看到的备份实现端。
type Module struct {
	// Ops 处理预检、执行与现状读取。
	Ops *Ops
	// Repos 提供仓库定义。
	Repos RepositorySource
	// Policies 提供策略定义（P12.2；生产实现是 *agentstore.BackupPolicyStore）。
	Policies PolicySource
	// Apps 是备份期间停/起受管理应用的能力。nil 表示这台机器上做不到
	// （没有 systemctl、没有 docker，或装配时没接）——那时 stop_managed_apps
	// 的策略会在预检里得到 BACKUP_APP_UNAVAILABLE，而不是在备份到一半时才发现。
	Apps AppController
	// PanelDatabasePath 是面板库的源路径。空表示这个代理不知道面板库在哪：
	// 列了 panel_database 来源的策略会因此被挡住，而不是备份出一个空目录。
	PanelDatabasePath string
	// PanelCopyDir 是面板库副本的落盘目录（代理的 StateDir 下，root 独占）。
	PanelCopyDir string
}

// GetRepository 取一条仓库定义。
func (m *Module) GetRepository(ctx context.Context, id string) (Repository, bool, error) {
	return m.Repos.GetRepository(ctx, id)
}

// Assess 生成一次预检结论。
func (m *Module) Assess(ctx context.Context, req AssessRequest) (Assessment, error) {
	return m.Ops.Assess(ctx, req)
}

// Execute 初始化一个仓库。
func (m *Module) Execute(ctx context.Context, req ExecuteRequest) (Execution, error) {
	return m.Ops.Execute(ctx, req)
}

// State 读仓库现状。
func (m *Module) State(ctx context.Context, req StateRequest) (RepositoryState, error) {
	return m.Ops.State(ctx, req)
}

// Paths 返回实际使用的 restic/ssh 路径（装配层打日志用）。
func (m *Module) Paths() Paths { return m.Ops.opts.Paths }

// 以下是 P12.2 的转发：策略、运行与快照查询。
//
// 三个字段（Apps / PanelDatabasePath / PanelCopyDir）由装配决定，动作层只交出
// 策略与仓库——动作参数因此不需要携带“面板库在哪、用哪个目录放副本”这类环境信息，
// 那些是代理自己的配置，不该由 Web 的请求体决定。

// GetPolicy 取一条策略定义。
func (m *Module) GetPolicy(ctx context.Context, id string) (PolicyRecord, bool, error) {
	if m.Policies == nil {
		return PolicyRecord{}, false, nil
	}
	return m.Policies.GetPolicy(ctx, id)
}

// AssessRun 生成一次备份预检结论。
func (m *Module) AssessRun(ctx context.Context, req RunAssessRequest) (RunAssessment, error) {
	req.Controller = m.Apps
	req.PanelDatabasePath = m.PanelDatabasePath
	return m.Ops.AssessRun(ctx, req)
}

// Run 执行一次备份。
func (m *Module) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	req.Controller = m.Apps
	req.PanelDatabasePath = m.PanelDatabasePath
	if req.PanelCopyDir == "" {
		req.PanelCopyDir = m.PanelCopyDir
	}
	return m.Ops.Run(ctx, req)
}

// ListSnapshots 列仓库里的快照（可按策略过滤）。
func (m *Module) ListSnapshots(ctx context.Context, req SnapshotRequest) ([]Snapshot, error) {
	return m.Ops.ListSnapshots(ctx, req)
}

// GetSnapshot 读一条快照的详情。
func (m *Module) GetSnapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	return m.Ops.GetSnapshot(ctx, req)
}

// P12.3：恢复、验证与显式保留清理。应用控制与验证临时目录都由装配注入。
func (m *Module) AssessRestore(ctx context.Context, req RestoreRequest) (RestoreAssessment, error) {
	req.Controller = m.Apps
	return m.Ops.AssessRestore(ctx, req)
}
func (m *Module) Restore(ctx context.Context, req RestoreRequest) (RestoreResult, error) {
	req.Controller = m.Apps
	return m.Ops.Restore(ctx, req)
}
func (m *Module) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	return m.Ops.Verify(ctx, req)
}
func (m *Module) AssessPrune(ctx context.Context, req PruneRequest) (PruneAssessment, error) {
	return m.Ops.AssessPrune(ctx, req)
}
func (m *Module) Prune(ctx context.Context, req PruneRequest) (PruneResult, error) {
	return m.Ops.Prune(ctx, req)
}
