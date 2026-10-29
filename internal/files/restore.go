package files

import "upanel/internal/backup"

// 类型由 backup 拥有，避免 backup → files → agentclient → backup 的装配环。
// 文件模块只提供这些固定类型的句柄级实现，不接受任何额外路径或命令。
type RestoreParams = backup.RestoreParams
type RestoreProbe = backup.RestoreProbe
type RestoreEntry = backup.RestoreEntry
type RestoreReceipt = backup.RestoreReceipt

var EqualRestoreEntries = backup.EqualRestoreEntries
