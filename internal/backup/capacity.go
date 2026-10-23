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
// 15 秒是 ssh 自己的 ConnectTimeout 常用值：短于它会把一台略慢的机器判成不可达，
// 长于它会让一次预检把 Web 的 10 秒调用上限耗光（预检本身有 PlanBudget 兜底，
// 这里只是不让一条死连接把时间花光）。
const DefaultDialTimeout = 15 * time.Second

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
