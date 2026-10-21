package storage

// 设备占用检查（P07.1）：有没有进程正打开着这块设备。
//
// 为什么不用 lsof：那要执行外部命令，而本包从 P04.2 起就坚持“不调用外部命令”，
// 免得引入命令注入面和一个不一定装了的依赖。内核已经把这些信息披露在 /proc 里：
// 每个进程的每个 fd 都是指向设备的符号链接，stat 一下就有 st_rdev，也就是设备号。
//
// 判据是**设备号**而不是路径：/dev/sdb 可能被 udev 改名、可能被 ln 成别的名字、
// dm/md 的设备号也和名字无关。sysfs 的 dev 属性给了目标设备的权威设备号，
// 两边按 major:minor 比对才不会漏也不会错配。
//
// 读不到就说读不到：代理以 root 运行（deploy/systemd/upanel-agent.service），
// 正常情况下 /proc/*/fd 全部可读。真读不到时返回错误，判决层据此拒绝——
// “看不见”绝不能被当成“没人用”。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// BusyProcess 是一个打开着目标设备的进程。
type BusyProcess struct {
	// PID 是进程号（十进制字符串，便于直接展示）。
	PID string `json:"pid"`
	// Comm 是进程名（/proc/<pid>/comm），读不到时为 "?"。
	Comm string `json:"comm"`
}

// BusyScanner 列出打开着某台设备的进程。
type BusyScanner interface {
	Scan(ctx context.Context, devID string) ([]BusyProcess, error)
}

// maxFDInspect 是一次占用检查最多看多少个 fd。
//
// 一台跑满进程的机器上万个 fd 也就几十毫秒；设上限只是为了不让一次异常
// （例如 /proc 挂的是别的容器视图）把预检拖成几十秒。超过上限算“没查完”，
// 于是拒绝，而不是放行。
const maxFDInspect = 200000

// ProcBusyScanner 从 /proc 与 sysfs 判断设备占用。
type ProcBusyScanner struct {
	// Proc 是 procfs 根（真实值 /proc）。
	Proc string
	// Sys 是 sysfs 根（真实值 /sys），用来看设备自己打开的 fd 属于哪个设备。
	Sys string
	// Self 是本进程 pid，跳过它：代理自己读签名时会短暂打开设备。
	Self int
	// MajorMinor 给出一个 fd 路径对应的块设备号（"8:17"）；普通文件返回空串。
	// nil 时用操作系统的 stat 实现（见 osMajorMinor）。
	MajorMinor func(fdPath string) (string, error)
	// ReadDir 列出一个目录的条目；nil 时用 os.ReadDir。
	//
	// 它和 MajorMinor 一样是**可注入的平台接缝**，而且理由更硬：这里最要紧的性质是
	// “读不到就报错，绝不返回空列表”。这条性质必须能在夹具里被验证，
	// 而不是等到真机上某天权限变了才发现它悄悄放过了占用。
	ReadDir func(path string) ([]os.DirEntry, error)
}

// SystemBusyScanner 返回指向真实 /proc 的占用检查器。
func SystemBusyScanner() BusyScanner {
	return ProcBusyScanner{Proc: "/proc", Sys: "/sys", Self: os.Getpid()}
}

// Scan 返回打开着 devID 的进程（去重、按 pid 升序）。
func (s ProcBusyScanner) Scan(ctx context.Context, devID string) ([]BusyProcess, error) {
	if devID == "" {
		return nil, errors.New("storage: 设备名为空")
	}
	if s.Proc == "" {
		return nil, errors.New("storage: 没有配置 procfs 根")
	}

	target, err := s.deviceNumber(devID)
	if err != nil {
		return nil, err
	}

	entries, err := s.readDir(s.Proc)
	if err != nil {
		return nil, fmt.Errorf("storage: 读 %s 失败: %w", s.Proc, err)
	}

	inspected := 0
	hits := map[string]BusyProcess{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid := entry.Name()
		if !isAllDigits(pid) {
			continue
		}
		if s.Self > 0 && pid == strconv.Itoa(s.Self) {
			continue
		}
		fdDir := joinPath(s.Proc, pid, "fd")
		fds, err := s.readDir(fdDir)
		if err != nil {
			if isVanished(err) {
				continue // 进程在这期间退出了
			}
			return nil, fmt.Errorf("storage: 读 %s 失败（无法确认占用情况）: %w", fdDir, err)
		}
		for _, fd := range fds {
			inspected++
			if inspected > maxFDInspect {
				return nil, fmt.Errorf("storage: 检查了 %d 个 fd 仍未结束，占用检查不可信", maxFDInspect)
			}
			num, err := s.majorMinor(joinPath(fdDir, fd.Name()))
			if err != nil {
				if isVanished(err) {
					continue // fd 在扫描期间关闭了
				}
				return nil, fmt.Errorf("storage: 取 %s 的设备号失败（无法确认占用情况）: %w",
					joinPath(fdDir, fd.Name()), err)
			}
			if num == "" || num != target {
				continue
			}
			if _, ok := hits[pid]; ok {
				continue
			}
			hits[pid] = BusyProcess{PID: pid, Comm: s.comm(pid)}
			break
		}
	}

	out := make([]BusyProcess, 0, len(hits))
	for _, p := range hits {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].PID)
		b, _ := strconv.Atoi(out[j].PID)
		if a != b {
			return a < b
		}
		return out[i].PID < out[j].PID
	})
	return out, nil
}

// deviceNumber 取设备的权威设备号（major:minor）。
func (s ProcBusyScanner) deviceNumber(devID string) (string, error) {
	dir := deviceSysDir(s.Sys, devID)
	if dir == "" {
		return "", fmt.Errorf("storage: 在 sysfs 里找不到 %s，无法确定设备号", devID)
	}
	num, ok := readFile(joinPath(dir, "dev"))
	if !ok {
		return "", fmt.Errorf("storage: %s 没有 dev 属性，无法确定设备号", devID)
	}
	return num, nil
}

func (s ProcBusyScanner) majorMinor(path string) (string, error) {
	if s.MajorMinor != nil {
		return s.MajorMinor(path)
	}
	return osMajorMinor(path)
}

func (s ProcBusyScanner) readDir(path string) ([]os.DirEntry, error) {
	if s.ReadDir != nil {
		return s.ReadDir(path)
	}
	return os.ReadDir(path)
}

// comm 读进程名；读不到不算错（进程可能刚退出），用 "?" 表示不知道。
func (s ProcBusyScanner) comm(pid string) string {
	if name, ok := readFile(joinPath(s.Proc, pid, "comm")); ok {
		return name
	}
	return "?"
}

// isVanished 判断错误是否只是“文件在扫描过程中消失了”（进程退出、fd 关闭）。
//
// 这类错误不算失败：占用检查本来就是某一瞬间的快照。
// 其余错误（权限、I/O）必须上抛——那意味着这次检查没做完。
func isVanished(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, errESRCH)
}

// errESRCH 是“进程不存在”的通用表示，由各平台实现映射（见 rdev_*.go）。
var errESRCH = errors.New("进程不存在")

// fdName 从 /proc/<pid>/fd 的条目名拼出完整路径（便于日志里看清是哪个 fd）。
func fdName(dir, name string) string { return filepath.Join(dir, name) }

// stringsJoinPIDs 把进程列表压成 "123/nginx、456/php-fpm" 这样的短串（供提示文本使用）。
func stringsJoinPIDs(procs []BusyProcess, limit int) string {
	parts := make([]string, 0, len(procs))
	for i, p := range procs {
		if limit > 0 && i == limit {
			parts = append(parts, "等 "+strconv.Itoa(len(procs))+" 个进程")
			break
		}
		parts = append(parts, p.PID+"/"+p.Comm)
	}
	return strings.Join(parts, "、")
}
