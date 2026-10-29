package backup

// 容量检测（P12.1）。
//
// 目的只有一个：在管理员按下“初始化仓库”之前，界面能说出“这个位置还有多少空间”，
// 备份运行前也能说出同样的话。读不到容量时**不折成 0**——0 与“不知道”在页面上
// 是两句话，混成一句就会变成“空间充足但其实是没读到”。
//
// 两条路：
//   - 本地仓库：syscall statfs（不经子进程，网络文件系统也走同一条路，与存储模块
//     的 30 秒缓存/隔离进程是两件事：这里只在预检与查询时读一次）；
//   - SFTP 仓库：SFTP 的 statvfs 扩展（由 OpenSSH 的 sftp-server 提供），
//     连不上的机器上如实报告失败。

import (
	"io/fs"
	"time"
)

// DefaultDialTimeout 是 SFTP 连接与握手的默认上限。
//
// 15 秒在真机上够用，但不够覆盖“机器慢但活着”的那一档：P12.3 的 24.04 验收机是
// 单线程 TCG 虚机，OpenSSH 客户端一次握手 + statvfs 实测 11.7 秒；Go 侧还要按指纹
// 逐个试主机密钥算法，单次握手经常贴着 15 秒。2026-10-25 的验收里连续两条
// `Connection closed ... [preauth]` 正好各吃掉 15 秒，随后第三条 11 秒才认证成功，
// 45 秒的预检预算已经见底——一个**连得上**的仓库被报成 `BACKUP_SFTP_UNREACHABLE`。
// 30 秒给慢机器留出余量，同时仍让死连接在预检自己的 75 秒预算内收手（Web 侧
// PreflightAgentTimeout 是 90 秒，先说话的是代理）。
const DefaultDialTimeout = 30 * time.Second

// Capacity 是一次容量读数。
type Capacity struct {
	// Supported 表示这次真的读到了数字。
	Supported bool `json:"supported"`
	// Target 是实际读取的路径（可能是目标的某个存活祖先）。
	Target string `json:"target,omitempty"`
	// TotalBytes / AvailableBytes 是总量与可用量（可用量按非特权用户口径：
	// statvfs 的 bavail，与存储模块 available_bytes 同义）。
	TotalBytes     int64 `json:"total_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
	// CheckedAt 是读取时间（UTC）。
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// Error 是没读到的原因；Supported 为 false 时非空。
	Error string `json:"error,omitempty"`
}

// UsedBytes 返回已用量（Supported 为 false 时无意义）。
func (c Capacity) UsedBytes() int64 {
	if !c.Supported || c.TotalBytes < c.AvailableBytes {
		return 0
	}
	return c.TotalBytes - c.AvailableBytes
}

// errNotExistSentinel 是“路径不存在”的通用形状（os 与 sftp 都会 wrap 它）。
var errNotExistSentinel = fs.ErrNotExist

// nowUTC 是容量读数的时间戳来源。测试里靠它固定 CheckedAt。
var nowUTC = func() time.Time { return time.Now().UTC() }
