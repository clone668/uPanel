package processes

import (
	"strconv"

	"upanel/internal/services"
)

// 进程保护名单（PLAN.md 第 14 节）：禁止终止 PID 1、面板/代理自身、内核线程和
// 受保护系统服务进程。
//
// 判定顺序刻意如此：
//  1. PID 1 与内核线程是“结构上不能杀”，不需要看别的证据；
//  2. 自身（代理进程、upanel-* 可执行文件）是“杀了就没有管理通道”；
//  3. 所属 systemd 单元命中服务保护名单（ssh、systemd 核心、网络、其他模块）
//     → 直接拒绝，理由与类别沿用服务模块那一份，不另编一套；
//  4. 祖先链只向上传播 **self** 这一种类别（代理/面板自己的进程树）。
//
// 为什么第 4 条只传 self：sshd 派生的登录会话进程在 session-*.scope 里，它的祖先
// 才是 sshd。若把“祖先受保护”整体向上传播，每个管理员从 SSH 里启动的进程都会变成
// 不可终止——列表里会一整片红，而真正该拦的 sshd 守护进程本来就已经被第 3 条拦住。
// 受保护的“系统服务进程”指的是那个服务的进程本体，不是它开出来的每个 shell。
//
// 不做“按命令名猜保护”（例如见到 nginx 就当成反代模块的服务）：名字可以改，
// cgroup 里的单元名是内核给的。唯一按文件名判断的是 upanel-agent/upanel-web
// 这两个本程序自己的可执行文件，防的是“没有跑在 systemd 单元里的开发/手工启动”。

// maxAncestorDepth 是向上追溯父进程的最大层数。
//
// 16 层足够覆盖“代理 → 子进程 → 孙进程”这类结构；再多只是在已知不属于
// 面板进程树的树上浪费时间。
const maxAncestorDepth = 16

// selfExecutables 是面板与代理自身可执行文件的基名。
var selfExecutables = map[string]bool{
	"upanel-agent": true,
	"upanel-web":   true,
}

// rowSource 提供按 PID 读取原始信息的能力：快照用内存里的 map，
// 现场读取用 /proc。保护判定因此对两条路径完全一致。
type rowSource interface {
	rowOf(pid int64) (procRow, bool)
}

// mapSource 是快照用的行来源。
type mapSource map[int64]procRow

func (m mapSource) rowOf(pid int64) (procRow, bool) {
	row, ok := m[pid]
	return row, ok
}

// protectionReason 是“某个行本身”的保护判定结果（不含祖先）。
type protectionReason struct {
	category string
	reasons  []string
}

// directProtection 判断一行进程本身是否受保护。
func directProtection(row procRow, selfPID int64) protectionReason {
	var out protectionReason

	// 1) PID 1。
	if row.pid == 1 {
		out.category = ProtectInit
		out.reasons = append(out.reasons,
			"PID 1 是 init（在 Ubuntu 上是 systemd）：终止它等于让整台机器失去初始化进程")
		return out
	}

	// 2) 内核线程。
	if row.kernelThread() {
		out.category = ProtectKernel
		out.reasons = append(out.reasons,
			"内核线程（"+safeComm(row.comm)+"）：它没有用户空间可终止，发信号只会让内核报警")
		return out
	}

	// 3) 代理自身。
	if selfPID > 0 && row.pid == selfPID {
		out.category = ProtectSelf
		out.reasons = append(out.reasons,
			"这是执行本次操作的代理进程本身：终止它等于让面板失去唯一的特权通道")
	}

	// 4) 本程序的可执行文件（没有以 systemd 单元运行的情况）。
	if selfExecutables[row.exeBase] {
		if out.category == "" {
			out.category = ProtectSelf
		}
		out.reasons = append(out.reasons,
			"这是面板/代理自身的进程（"+row.exeBase+"）：终止它会让管理界面失去响应")
	}

	// 5) 所属 systemd 单元命中服务保护名单。
	if row.unit != "" {
		if protection := services.Protect(row.unit); protection.Protected {
			if out.category == "" {
				out.category = protection.Category
			}
			for _, reason := range protection.Reasons {
				out.reasons = append(out.reasons,
					"它属于受保护单元 "+row.unit+"："+reason)
			}
		}
	}

	return out
}

// Protect 计算一个进程的保护判定（含代理进程树的祖先检查）。
//
// source 必须能提供祖先的行；快照用它自己的 map，现场读取用 /proc 的即时读取。
// 祖先读不到就停止向上追溯——不猜、也不把“读不到”当成“受保护”。
func Protect(row procRow, source rowSource, selfPID int64) Protection {
	direct := directProtection(row, selfPID)
	category := direct.category
	reasons := append([]string{}, direct.reasons...)

	seen := map[int64]bool{row.pid: true}
	current := row
	for depth := 0; depth < maxAncestorDepth; depth++ {
		parentPID := current.ppid
		if parentPID <= 0 || seen[parentPID] {
			break
		}
		seen[parentPID] = true
		parent, ok := source.rowOf(parentPID)
		if !ok {
			break
		}
		ancestor := directProtection(parent, selfPID)
		if ancestor.category == ProtectSelf {
			if category == "" {
				category = ProtectSelf
			}
			for _, reason := range ancestor.reasons {
				reasons = append(reasons,
					"它由 PID "+strconv.FormatInt(parent.pid, 10)+"（"+
						commandOrComm(parent)+"）启动，而那个进程受保护："+reason)
			}
			break
		}
		current = parent
	}

	if len(reasons) == 0 {
		return Protection{Protected: false, Reasons: []string{}}
	}
	return Protection{
		Protected: true,
		Category:  category,
		Reasons:   dedupeSorted(reasons),
	}
}

// commandOrComm 给祖先链的理由里用的一句话标识：优先脱敏命令摘要，退化到 comm。
func commandOrComm(row procRow) string {
	command := SanitizeCommand(row.argv, row.comm)
	if len([]rune(command)) > 48 {
		return string([]rune(command)[:48]) + "…"
	}
	return command
}

// safeComm 清掉 comm 里的控制字符，避免把 /proc 的怪字节原样带进界面。
func safeComm(comm string) string {
	cleaned := sanitizePart(comm)
	if cleaned == "" {
		return "未知名称"
	}
	return cleaned
}
