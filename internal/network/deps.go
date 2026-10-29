package network

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"
)

// Runner 执行一条**只读**的系统命令并把标准输出交回来。
//
// 这一包只会用它跑三个程序：`ip`（地址与路由）、`ss` 不用（监听从 /proc 读）、
// `ufw`（状态）。它不能接受调用方给的字符串：程序与参数都由本包的常量决定。
//
// 约定：程序根本跑不起来（不在、没有执行权限）时返回 err；程序跑起来了但退出码非零
// 时 err 为 nil、由调用方看 code——"ufw 说它没启用"和"ufw 不在"是两件事。
type Runner interface {
	Run(ctx context.Context, program string, args ...string) (stdout []byte, code int, err error)
}

// ErrProgramMissing 表示要跑的程序不在（或不可执行）。
//
// 它单独一个错误值，是为了让调用方能把它翻译成 `missing` 而不是 `failed`：
// 一台没装 ufw 的机器上，"防火墙状态读不到"的原因是"没有这个程序"，
// 界面上应该显示"未安装"，而不是一句泛泛的读取失败。
var ErrProgramMissing = errors.New("network: 程序不可用")

// FileSystem 是读取 /proc、/sys 与配置目录用到的三个操作。
//
// 不用 processes.FileSystem 同名接口：那会把 internal/network 与 internal/processes
// 绑在一起，而 processes 的读取端又依赖 internal/agentclient，后者要 import 本包
// 来解响应——一个只为复用三行接口声明的循环依赖。三个方法的形状保持一致，
// 于是测试里的假文件系统两边都能用。
type FileSystem interface {
	ReadFile(name string) ([]byte, error)
	// ListDir 只返回名字：判定"目录在不在"与"里面有什么"都不需要类型信息。
	ListDir(dir string) ([]string, error)
	Readlink(name string) (string, error)
}

// Deps 是采集的依赖，全部可替换；测试给假文件系统与假命令，不碰真机。
type Deps struct {
	// FS 是读 /proc、/sys 的最小能力；为零值时用真实文件系统。
	FS FileSystem
	// Runner 跑 `ip` 与 `ufw`；为 nil 时所有需要命令的读取都如实报告 missing。
	Runner Runner
	// ProcRoot 是 procfs 挂载点，默认 /proc。
	ProcRoot string
	// SysRoot 是 sysfs 挂载点，默认 /sys。
	SysRoot string
	// NetplanDir 是 netplan 配置目录，默认 /etc/netplan。
	NetplanDir string
	// RunDir 是运行期状态目录，默认 /run。netplan 渲染出来的产物在它下面。
	RunDir string
	// Timeout 是单次采集里所有外部读取的总上限，默认 8 秒。
	//
	// 8 秒不是随手取的：页面打开就要等这一次采集，而 `ip` 在正常情况下是毫秒级；
	// 超过 8 秒基本意味着内核或网络栈出了问题，那时"读不到"比"再等等"对管理员更有用。
	Timeout time.Duration
	// Now 用于打采集时刻；为零值时用 time.Now。
	Now func() time.Time
}

func (d Deps) fs() FileSystem {
	if d.FS != nil {
		return d.FS
	}
	return osFS{}
}

func (d Deps) procRoot() string {
	if d.ProcRoot != "" {
		return d.ProcRoot
	}
	return "/proc"
}

func (d Deps) sysRoot() string {
	if d.SysRoot != "" {
		return d.SysRoot
	}
	return "/sys"
}

func (d Deps) netplanDir() string {
	if d.NetplanDir != "" {
		return d.NetplanDir
	}
	return "/etc/netplan"
}

func (d Deps) runDir() string {
	if d.RunDir != "" {
		return d.RunDir
	}
	return "/run"
}

func (d Deps) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 8 * time.Second
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// errno 错误到 Issue 码的翻译。放在一处，免得每个读取点各自判一遍 strings.Contains。
func issueCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, ErrProgramMissing):
		// 程序不在和路径不在是同一件事的另一面：这个来源读不到，不是"没有东西"。
		return IssueMissing
	case errors.Is(err, fs.ErrPermission):
		return IssuePermission
	case errors.Is(err, context.DeadlineExceeded):
		return IssueTimeout
	default:
		return IssueFailed
	}
}

// osFS 是真实文件系统的实现。
type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func (osFS) ListDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func (osFS) Readlink(name string) (string, error) { return os.Readlink(name) }
