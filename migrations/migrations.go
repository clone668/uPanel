// Package migrations 嵌入 Web 数据库的 SQL 迁移文件。
//
// 迁移是显式的、按文件名序号执行的 SQL 文件（PLAN.md 第 3 节选定“显式 SQL 迁移”，
// 不使用 ORM 自动建表）。文件名格式：<4 位以上序号>_<小写名称>.sql。
//
// 已应用的迁移不可修改：store 会比对内容校验和，改动会导致启动失败而不是悄悄漂移。
package migrations

import "embed"

// FS 是嵌入的迁移文件集合。
//
//go:embed *.sql
var FS embed.FS
