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
