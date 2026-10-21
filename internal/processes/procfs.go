package processes

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

// FileSystem 是采集进程信息需要的最小文件系统能力。
//
// 三件事：读文件、列目录、读符号链接。用接口而不是直接调 os，是为了让“真实 /proc
// 是什么样”成为一个可以用真机抓下来的字节来验证的输入（见 testdata/），
// 而不是只能靠一台正在跑的主机现场碰运气。
type FileSystem interface {
	ReadFile(name string) ([]byte, error)
	// ListDir 只返回名字：进程扫描只关心数字目录名，不需要类型信息。
	ListDir(dir string) ([]string, error)
	Readlink(name string) (string, error)
}

type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func (osFS) ListDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func (osFS) Readlink(name string) (string, error) { return os.Readlink(name) }

// Paths 是采集读取的全部固定路径。
//
// 全部集中在这里，是为了让“采集读了哪些文件”是一个可审计的常量列表：
// 没有拼接的用户输入，也没有随环境变化的路径。进程号只来自 ListDir(ProcRoot)。
type Paths struct {
	// ProcRoot 是 procfs 挂载点（通常是 /proc）。
	ProcRoot string
	// Passwd 用来把 uid 翻成用户名；读不到时界面显示 uid:<数字>。
	Passwd string
}

// DefaultPaths 返回 Linux 上的标准路径。
func DefaultPaths() Paths {
	return Paths{ProcRoot: "/proc", Passwd: "/etc/passwd"}
}

// valid 判断路径是否已经初始化。
func (p Paths) valid() bool { return p.ProcRoot != "" }

func (p Paths) statFile() string { return path.Join(p.ProcRoot, "stat") }

func (p Paths) pidFile(pid int64, name string) string {
	return path.Join(p.ProcRoot, strconv.FormatInt(pid, 10), name)
}

// statInfo 是 /proc/<pid>/stat 里本模块关心的字段。
type statInfo struct {
	comm       string
	state      string
	ppid       int64
	utime      uint64
	stime      uint64
	startTicks uint64
}

// parseStat 解析 /proc/<pid>/stat。
//
// 格式陷阱（真实内核行为，不是假设）：第二个字段 comm 用**圆括号**包起来，
// 而 comm 里可以含空格和右括号（进程自己 setprctl 就能取到 "a)b c"），
// 所以不能按空格切分、也不能取第一个 ')'——必须取最后一个 ')' 之后再切。
func parseStat(data []byte) (statInfo, error) {
	text := string(data)
	open := strings.IndexByte(text, '(')
	closing := strings.LastIndexByte(text, ')')
	if open < 0 || closing < 0 || closing < open {
		return statInfo{}, errors.New("processes: /proc/<pid>/stat 缺少 comm 字段")
	}
	info := statInfo{comm: text[open+1 : closing]}
	rest := strings.TrimSpace(text[closing+1:])
	fields := strings.Fields(rest)
	// 之后还剩 49 个字段（第 3 到第 51 项）。starttime 是第 22 项 → 下标 19。
	if len(fields) < 20 {
		return statInfo{}, fmt.Errorf("processes: /proc/<pid>/stat 字段不足（%d）", len(fields))
	}
	if len(fields[0]) != 1 {
		return statInfo{}, fmt.Errorf("processes: /proc/<pid>/stat 状态字段异常：%q", fields[0])
	}
	info.state = stateFromLetter(fields[0][0])
	ppid, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return statInfo{}, fmt.Errorf("processes: 解析 ppid 失败: %w", err)
	}
	info.ppid = ppid
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return statInfo{}, fmt.Errorf("processes: 解析 utime 失败: %w", err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return statInfo{}, fmt.Errorf("processes: 解析 stime 失败: %w", err)
	}
	info.utime = utime
	info.stime = stime
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return statInfo{}, fmt.Errorf("processes: 解析 starttime 失败: %w", err)
	}
	info.startTicks = start
	return info, nil
}

// statusInfo 是 /proc/<pid>/status 里本模块关心的字段。
type statusInfo struct {
	uid      int64
	rssBytes *int64
}

// parseStatus 解析 /proc/<pid>/status。
//
// 内核线程与僵尸没有 VmRSS 行：缺就是缺，返回 nil 而不是 0。
func parseStatus(data []byte) statusInfo {
	var out statusInfo
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "Uid":
			// 真实 uid、有效 uid、保存的 uid、文件系统 uid；进程归属看第一个。
			fields := strings.Fields(value)
			if len(fields) == 0 {
				continue
			}
			if uid, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
				out.uid = uid
			}
		case "VmRSS":
			fields := strings.Fields(value)
			if len(fields) < 2 || fields[1] != "kB" {
				continue
			}
			kb, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil || kb < 0 {
				continue
			}
			bytes := kb * 1024
			out.rssBytes = &bytes
		}
	}
	return out
}

// parseCmdline 把 /proc/<pid>/cmdline（NUL 分隔）拆成 argv。
//
// 空文件是正常现象：内核线程与僵尸都没有用户空间命令行。
func parseCmdline(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// cgroupUnit 从 /proc/<pid>/cgroup 的内容里取出所属 systemd 单元。
//
// cgroup v2 只有一行 `0::<path>`；v1 有多行。单元是路径里最后一个以 .service 或
// .scope 结尾的片段。取不到就返回空串——那表示“不在任何单元里”，不是猜测的理由。
func cgroupUnit(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] != "0" || parts[1] != "" {
			// 只看 cgroup v2 的 "0::path" 行；v1 的行没有统一的单元语义。
			continue
		}
		return unitFromCgroupPath(parts[2])
	}
	return ""
}

// unitFromCgroupPath 从 cgroup 路径里取最后一个 .service/.scope 片段。
func unitFromCgroupPath(cgroupPath string) string {
	segments := strings.Split(strings.Trim(cgroupPath, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		segment := segments[i]
		if strings.HasSuffix(segment, ".service") || strings.HasSuffix(segment, ".scope") {
			// 片段里不可能有 shell 元字符：它是内核给的路径；仍然按单元名规则挡一道。
			if strings.ContainsAny(segment, " \t\x00\n") {
				return ""
			}
			return segment
		}
	}
	return ""
}

// procRow 是一个 PID 上读到的全部原始信息（采集与预检共用）。
type procRow struct {
	pid       int64
	ppid      int64
	comm      string
	state     string
	startTick uint64
	utime     uint64
	stime     uint64
	uid       int64
	rssBytes  *int64
	argv      []string
	unit      string
	// exeBase 是 /proc/<pid>/exe 的基名；读不到时为空（权限、内核线程、僵尸）。
	exeBase string
}

// kernelThread 判断是否是内核线程。
//
// 判据是“没有用户空间命令行且还活着”：所有内核线程的 cmdline 都是空文件，
// 僵尸虽然也没有 cmdline，但它不是内核线程（也不是能杀的对象）。
func (r procRow) kernelThread() bool {
	return len(r.argv) == 0 && !exited(r.state)
}

// cpuTicks 返回这个进程消耗的总时钟滴答数（用户 + 内核）。
func (r procRow) cpuTicks() uint64 { return r.utime + r.stime }

// readRow 读取一个 PID 的全部信息。读不到 stat 就当进程不存在（ok=false）。
func readRow(fs FileSystem, paths Paths, pid int64) (procRow, bool) {
	statData, err := fs.ReadFile(paths.pidFile(pid, "stat"))
	if err != nil {
		return procRow{}, false
	}
	info, err := parseStat(statData)
	if err != nil {
		return procRow{}, false
	}
	row := procRow{
		pid:       pid,
		ppid:      info.ppid,
		comm:      info.comm,
		state:     info.state,
		startTick: info.startTicks,
		utime:     info.utime,
		stime:     info.stime,
	}
	if data, err := fs.ReadFile(paths.pidFile(pid, "status")); err == nil {
		status := parseStatus(data)
		row.uid = status.uid
		row.rssBytes = status.rssBytes
	}
	if data, err := fs.ReadFile(paths.pidFile(pid, "cmdline")); err == nil {
		row.argv = parseCmdline(data)
	}
	if data, err := fs.ReadFile(paths.pidFile(pid, "cgroup")); err == nil {
		row.unit = cgroupUnit(data)
	}
	if target, err := fs.Readlink(paths.pidFile(pid, "exe")); err == nil {
		row.exeBase = path.Base(target)
	}
	return row, true
}

// passwdMap 解析 /etc/passwd，返回 uid → 用户名。
//
// 不用 os/user：agent 是静态编译的（CGO_ENABLED=0），而且这里的唯一需求就是
// “uid 对应哪个名字”，读文件比走一遍 NSS 更可预测——读不到就显示 uid:<数字>。
func parsePasswd(data []byte) map[int64]string {
	users := make(map[int64]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		uid, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || uid < 0 {
			continue
		}
		if _, exists := users[uid]; exists {
			continue
		}
		users[uid] = fields[0]
	}
	return users
}

// ownerName 把 uid 翻成名字；未知 uid 显示 uid:<数字>（不假装它没有主人）。
func ownerName(users map[int64]string, uid int64) string {
	if name, ok := users[uid]; ok && name != "" {
		return name
	}
	return "uid:" + strconv.FormatInt(uid, 10)
}
