package backup

// 仓库定义的镜像记录（P12.1）。
//
// 动作的参数是冻结的 `repository_id` + `version`（CONTRACTS.md 第 5 节），所以定义
// 不能随请求传进来：代理必须能从**自己**的库里按 id 找到它，并在执行前核对版本。
// 这样“预检时答应的是 A、执行时做的是 B”就不可能发生——版本不够就拒绝。

import "context"

// Repository 是代理镜像里的一条仓库定义。
type Repository struct {
	ID         string
	Name       string
	Definition Definition
	// Version 是乐观并发令牌；动作参数里的 version 必须与它相等。
	Version int64
}

// RepositorySource 提供仓库定义镜像（生产实现是 agentstore.BackupRepositoryStore）。
type RepositorySource interface {
	GetRepository(ctx context.Context, id string) (Repository, bool, error)
}
