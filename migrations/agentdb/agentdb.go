// Package agentdb 嵌入代理数据库的 SQL 迁移文件。
//
// 代理数据库（PLAN.md 第 4 节：/var/lib/upanel-agent/agent.db）与 Web 数据库是两个
// 独立文件，彼此不是分布式事务：代理先落最终资源与结果，Web 按 operation_id 对账。
//
// 迁移机制与 Web 侧完全相同（internal/store 的显式 SQL 迁移、内容校验和、不可修改），
// 但**文件集合不同**：代理表的列定义由代理自己负责，Web 进程永远不打开这个库。
//
// P02.4 只建 secrets 表。CONTRACTS.md 14.2 要求的其余表（operations、operation_events、
// preflight_plans、managed_resources、config_backups、pending_network_rollbacks）
// 随定义它们的阶段一起落地，不在这里提前猜列定义（见 docs/DECISIONS.md Q43）。
package agentdb

import "embed"

// FS 是嵌入的代理数据库迁移文件集合。
//
//go:embed *.sql
var FS embed.FS
