package operations

// 按目标统计最近的尝试次数（P10.3）。
//
// 用途只有一处：ACME 签发有速率限额（Let's Encrypt 对同一域名每周 5 张），而
// 失败的签发**同样**消耗配额。所以"一小时之内不许对同一个域名再试一次"这条规则
// 必须基于"有人试过"这个事实，而不是基于"成功了所以有记录"。
//
// 为什么查任务表而不是证书表：第一次签发失败时，证书表里**没有**这条域名的记录
// （见 internal/certificates/ledger.go 的文件头）。把限流放在证书表上，恰好会在
// 最需要它的时候（反复失败）失效。任务表里却有每一次尝试：失败的任务也在这里。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// attemptRow 是查询出来的一行候选。
type attemptRow struct {
	action     string
	targetJSON string
}

// RecentAttempts 统计 actions 里的动作在 since 之后、面向 target 的尝试次数。
//
// target 与预检记录里的 display_name 比较（证书动作的 display_name 就是域名）。
// 这里不用 SQL 的 json_extract：那一行 JSON 的形状由本包写、由本包读，在 Go 里
// 解析比在 SQL 里再实现一遍解析规则更容易看懂，而候选行的量级是"一小时内的任务数"。
func (s *Service) RecentAttempts(ctx context.Context, actions []string, target string, since time.Time) (int, error) {
	if len(actions) == 0 || strings.TrimSpace(target) == "" {
		return 0, nil
	}
	placeholders := make([]string, len(actions))
	args := make([]any, 0, len(actions)+1)
	for i, action := range actions {
		placeholders[i] = "?"
		args = append(args, action)
	}
	args = append(args, formatTime(since.UTC()))

	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT j.action, p.target_json
		  FROM jobs j JOIN preflights p ON p.id = j.preflight_id
		 WHERE j.action IN (`+strings.Join(placeholders, ", ")+`)
		   AND j.created_at >= ?`, args...)
	if err != nil {
		return 0, fmt.Errorf("operations: 统计最近尝试失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		var row attemptRow
		if err := rows.Scan(&row.action, &row.targetJSON); err != nil {
			return 0, fmt.Errorf("operations: 读取最近尝试失败: %w", err)
		}
		var decoded struct {
			DisplayName string `json:"display_name"`
		}
		if err := json.Unmarshal([]byte(row.targetJSON), &decoded); err != nil {
			// 一条解析不出来的目标不能当成"没试过"——但也不能当成试过。
			// 记 0 会让限流失效，记 1 会误伤；这里选择跳过并继续，
			// 因为同一域名的其它尝试行仍然会被数到。
			continue
		}
		if decoded.DisplayName == target {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("operations: 读取最近尝试失败: %w", err)
	}
	return count, nil
}
