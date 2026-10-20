package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrDiskReadTimeout 表示读整盘头部超时。采集循环据此把分区表标记为 skipped，
// 而不是让整个快照卡在 I/O 上。
var ErrDiskReadTimeout = errors.New("storage: 读盘超时")

// ErrCapacityUnavailable 表示挂载点容量不可读（statfs 失败）。调用方把容量留成 null。
var ErrCapacityUnavailable = errors.New("storage: 容量不可读")

// DiskReader 读出整盘最前面的 34 个扇区（512 字节/扇区）：
// LBA0（MBR/GPT 保护性 MBR）、LBA1（GPT 头）、LBA2..33（GPT 分区条目表，128 项 × 128 字节）。
//
// 实现必须**不强制唤醒**休眠设备：判断由调用方先做（见 scan.go 的磁盘可读性检查），
// 实现只负责在读取失败或超时时尽快返回错误。
type DiskReader interface {
	ReadDisk(ctx context.Context, name string) ([]byte, error)
}

// CapacityReader 读取挂载点的容量。失败时返回错误，调用方保留 null。
//
// fsType 来自挂载表：本地文件系统可以就地 statfs，网络文件系统必须走隔离通道
// （见 capacity.go），否则一次不可达的 NFS 就能把采集循环卡死。
type CapacityReader interface {
	Capacity(ctx context.Context, mountPath, fsType string) (*Capacity, error)
}

// Root 是采集的输入根：真实运行时指向 /sys、/proc、/dev，测试时指向夹具目录。
//
// 把三个根显式拆开（而不是写死 /sys）是为了能拿**真实采集的夹具**跑单元测试：
// 手写的 sysfs 树会把“我以为内核长这样”固化成测试前提，真机上就不成立（DECISIONS Q85）。
type Root struct {
	// Sys 是 sysfs 根（真实值 /sys）。
	Sys string
	// Proc 是 procfs 根（真实值 /proc）。
	Proc string
	// Disks 读整盘头部；nil 表示不做分区表探测（只报 sysfs 已知信息）。
	Disks DiskReader
	// Caps 读挂载点容量；nil 表示容量一律为 null。
	Caps CapacityReader
	// Now 是时钟，便于测试固定时间；nil 时用 time.Now。
	Now func() time.Time

	// ReadTimeout 是单块盘头部读取的超时。0 时用 DefaultDiskReadTimeout。
	ReadTimeout time.Duration
}

// DefaultDiskReadTimeout 是读一块盘头部的超时。17 KiB 的正常读取是毫秒级：
// 超过这个时间说明盘在睡眠、链路有问题或设备卡住，宁可跳过也不要挂着。
const DefaultDiskReadTimeout = 2 * time.Second

func (r Root) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r Root) timeout() time.Duration {
	if r.ReadTimeout > 0 {
		return r.ReadTimeout
	}
	return DefaultDiskReadTimeout
}

// SystemRoot 返回指向真实系统的 Root。
//
// 网络挂载的容量查询通过重新执行本进程（helper 模式）隔离，网络一断也只有
// 子进程会挂住，采集循环照常走。
func SystemRoot() Root {
	return Root{
		Sys:         "/sys",
		Proc:        "/proc",
		Disks:       OSDiskReader{Dev: "/dev", Timeout: DefaultDiskReadTimeout},
		Caps:        &OSCapacityReader{Helper: selfStatfsHelper()},
		ReadTimeout: DefaultDiskReadTimeout,
	}
}

// selfStatfsHelper 返回指向本进程可执行文件的容量 Helper。
// 取不到自身路径时返回 nil：那时网络挂载容量一律为 null，而不是退回直接 statfs
// 把采集循环置于风险之中。
func selfStatfsHelper() StatfsRunner {
	exe, err := os.Executable()
	if err != nil || !filepath.IsAbs(exe) {
		return nil
	}
	return ExecStatfsHelper{Path: exe}
}

// readFile 读取一个 sysfs/procfs 属性文件，去掉首尾空白。
// 第二个返回值是“读到了且不是空”。很多属性文件存在但读起来报 EINVAL（空 wwid 就是），
// 因此缺文件与读失败同等对待：都算没有这个信息。
func readFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", false
	}
	return s, true
}

// readUint 读取一个十进制整数属性文件。
func readUint(path string) (uint64, bool) {
	s, ok := readFile(path)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// linkTarget 读出 /dev/disk/by-* 目录里一个条目的目标路径。
//
// 必须按**符号链接**读（os.Readlink），不能直接 os.ReadFile：
// 真实系统上 /dev/disk/by-uuid/<uuid> 指向块设备，ReadFile 会把它当普通文件一直读到底——
// 一块 4 GiB 的盘就是 4 GiB 内存，代理当场被 OOM 杀掉（P04.2 验收在 24.04 上真的这样死过）。
// 夹具里这些条目是普通文件，内容写着目标路径，所以两条路径都要支持：
// 是符号链接就 Readlink，是普通文件才读内容，并且只接受 /dev/ 开头的绝对路径。
func linkTarget(path string) string {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return ""
		}
		if !filepath.IsAbs(target) {
			// udev 写的是相对链接（../../sda1），按条目所在目录解析。
			target = filepath.Clean(filepath.Join(filepath.Dir(path), target))
		}
		return target
	}
	if s, ok := readFile(path); ok && strings.HasPrefix(s, "/dev/") {
		return s
	}
	return ""
}

// readList 读取一个“一行一个名字”的文件（holders/list、slaves/list）。
func readList(path string) []string {
	s, ok := readFile(path)
	if !ok {
		return nil
	}
	out := make([]string, 0, 4)
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// dirNames 列出目录里的条目名（不递归）。目录不存在时返回 nil。
func dirNames(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// OSDiskReader 从真实的 /dev 读整盘头部。
//
// 读在独立 goroutine 里进行，超时后关闭文件句柄：正常读取是毫秒级，超时说明
// 盘在睡眠或链路卡住，这时**必须**回到采集循环，不能让一个坏盘拖住整个代理。
// 如果内核把线程挂在不可中断的 I/O 上，关闭句柄也可能解不开——那种情况会留下一个
// goroutine，但采集已经拿到错误并继续，界面显示 skipped。
type OSDiskReader struct {
	// Dev 是设备目录（真实值 /dev）。
	Dev string
	// Timeout 是单次读取的超时。
	Timeout time.Duration
}

// ReadDisk 读取 name（例如 sda）的前 34 个扇区。
func (o OSDiskReader) ReadDisk(ctx context.Context, name string) ([]byte, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultDiskReadTimeout
	}
	f, err := os.Open(filepath.Join(o.Dev, name))
	if err != nil {
		return nil, fmt.Errorf("storage: 打开 /dev/%s 失败: %w", name, err)
	}
	defer f.Close()

	buf := make([]byte, 34*512)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := f.ReadAt(buf, 0)
		done <- result{n: n, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		_ = f.Close()
		return nil, ctx.Err()
	case <-timer.C:
		_ = f.Close()
		return nil, fmt.Errorf("%w: /dev/%s 超过 %s", ErrDiskReadTimeout, name, timeout)
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("storage: 读 /dev/%s 失败: %w", name, res.err)
		}
		if res.n < 512 {
			return nil, fmt.Errorf("storage: /dev/%s 只读到 %d 字节，不足一个扇区", name, res.n)
		}
		return buf[:res.n], nil
	}
}

// round1 保留一位小数。容量百分比按一位小数展示，避免 33.33333% 这类噪声。
func round1(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return float64(int64(v*10+0.5)) / 10
}

// joinPath 是 filepath.Join 的短名字：本包里路径拼接出现得很频繁，
// 但每次都写 filepath.Join 会让 sysfs 的层级结构看不清。
func joinPath(parts ...string) string { return filepath.Join(parts...) }

// resolveSymlink 解析符号链接（真实 sysfs 里 /sys/block/sda/device 是链接）。
func resolveSymlink(path string) (string, error) { return filepath.EvalSymlinks(path) }

// relativeTo 求 target 相对 base 的路径，用来把 sysfs 的真实路径变成夹具里那套相对路径。
func relativeTo(base, target string) (string, error) { return filepath.Rel(base, target) }
