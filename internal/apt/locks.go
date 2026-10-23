package apt

// APT 系统锁的探测（P11.2）。
//
// STAGES §13 的验收条款第一条：**外部 apt 正在运行时不删锁、不并发**。
// 这里做的正是这句话的前半段——"只探测，从不删除"。锁文件是 apt/dpkg 自己的协议：
// 它们用 `flock` 加排他锁，锁在，说明有人正在改包数据库。删掉锁文件既不会让那个人
// 停下来，还会让下一个进程以为没人占用——两个 apt 同时写 dpkg 数据库的后果是
// 一个装到一半的包和一台需要手工救援的机器。
//
// 所以探测方式是"自己也去 flock 一次，拿不到就说明有人占着"，而且**立即释放**。
// 探测本身会短暂持有排他锁（微秒级），比读一个字节再猜安全得多。

import "strings"

// LockState 是一个锁文件的探测结论。
type LockState struct {
	// Path 是锁文件路径。
	Path string `json:"path"`
	// Held=true 表示有别的进程持有这把锁（正在跑 apt/dpkg）。
	Held bool `json:"held"`
	// Error 是探测失败的原因（权限、文件系统不支持 flock 等）。
	//
	// 它不等于 busy：读不到不等于有人占着。调用方必须把两者分开报告，
	// 否则一次权限问题会被说成"apt 正在运行"。
	Error string `json:"error,omitempty"`
}

// LockPaths 返回要探测的锁文件（顺序固定，便于日志与测试比对）。
func (p Paths) LockPaths() []string {
	out := make([]string, 0, 4)
	for _, path := range []string{p.DpkgLockFrontend, p.DpkgLock, p.AptListsLock, p.AptArchivesLock} {
		if strings.TrimSpace(path) != "" {
			out = append(out, path)
		}
	}
	return out
}

// ProbeLocks 探测全部锁文件。
//
// 单个锁探测失败不会中断其余探测：真实场景里最常见的是"这个文件在这个发行版上不存在"，
// 那只是这一把锁没有意义，不是"整台机器不能装包"。
func ProbeLocks(paths []string) []LockState {
	out := make([]LockState, 0, len(paths))
	for _, path := range paths {
		held, err := probeLock(path)
		state := LockState{Path: path, Held: held}
		if err != nil {
			state.Error = err.Error()
		}
		out = append(out, state)
	}
	return out
}

// BusyLocks 过滤出真正被持有的锁。
func BusyLocks(states []LockState) []LockState {
	out := make([]LockState, 0, len(states))
	for _, state := range states {
		if state.Held {
			out = append(out, state)
		}
	}
	return out
}
