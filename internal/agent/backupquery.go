package agent

// backup.repository.state（P12.1）：仓库在这台机器上的现状。
//
// 这是一个**只读**查询：不初始化、不写 known_hosts、不落任何文件。它的用途是
// 让面板在预检之前就能回答“这个仓库现在到底存不存在、还有多少空间”，而不是靠
// 管理员反复点一遍初始化来试探。
//
// 读不到与没有是两件事，输出里必须分开：`initialized=false` 表示目标位置确实不是
// 仓库；`repo_config_error` 非空表示我们没读到（口令不对、远端不可达、文件权限不够），
// 那是未知，不是空。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"upanel/internal/backup"
)

// backupStateParams 是 backup.repository.state 的参数。
type backupStateParams struct {
	RepositoryID string `json:"repository_id"`
}

// backupStateBudget 是只读查询的预算：一次 restic version、一次 SFTP 拨号与探测、
// 再用 restic 读一次仓库 config。
//
// 45 秒不是拍的：P12.1 在 24.04 那台虚机（单线程 TCG，CPU 比真机慢一个量级）上实测
// 同一串动作要 30 秒上下——`ssh` 握手 3—9 秒、SFTP 逐次 stat 各一个往返、restic
// `cat config` 5—12 秒。原来给 30 秒，恰好在这台机器上把一次**正常的**查询卡成超时，
// 而超时又被报成"restic 以退出状态 -1 结束"（见 internal/backup/exec.go 的
// ErrBudgetExhausted）。预算按最慢的合理现场定，不按"我这台机器跑得飞快"定；
// 真连不上时先到的是拨号自己的 15 秒上限（最多两轮），不会被这条预算拖住。
const backupStateBudget = 45 * time.Second

// RegisterBackupQueries 登记备份只读查询。
func RegisterBackupQueries(reg *Registry, src backupSource) error {
	if src == nil {
		return nil
	}
	return reg.RegisterQuery(Query{
		Name: backup.QueryRepositoryState,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupStateParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if p.RepositoryID == "" {
				return nil, fmt.Errorf("%w: repository_id 不能为空", ErrParamsInvalid)
			}
			return p, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(backupStateParams)
			if !ok {
				return nil, fmt.Errorf("%w: 参数类型不匹配: %T", ErrParamsInvalid, params)
			}
			ctx, cancel := context.WithTimeout(ctx, backupStateBudget)
			defer cancel()
			repo, found, err := src.GetRepository(ctx, p.RepositoryID)
			if err != nil {
				return nil, err
			}
			if !found {
				// 没登记就是没登记：查询返回一个明确的状态，不回一个空对象。
				return map[string]any{
					"repository_id": p.RepositoryID,
					"registered":    false,
					"checked_at":    time.Now().UTC().Format(time.RFC3339),
				}, nil
			}
			state, err := src.State(ctx, backup.StateRequest{
				RepositoryID: repo.ID,
				Definition:   repo.Definition,
				Version:      repo.Version,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"repository_id": p.RepositoryID,
				"registered":    true,
				"state":         state,
			}, nil
		},
	})
}
