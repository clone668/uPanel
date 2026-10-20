// 挂载点容量的读取与隔离（P04.3）。
//
// 本地文件系统的 statfs 是微秒级的系统调用，直接在采集 goroutine 里调用即可。
// **网络挂载不是**：对一台已消失的 NFS/CIFS 服务器做 statfs，进程会卡在不可中断的
// 内核 I/O 里，信号杀不掉、超时也回不来——整个 5 秒采集循环就此停摆
// （PLAN.md 第 8 节：网络挂载查询在隔离子进程中执行、限制并发；超时后标记 unavailable）。
//
// 因此网络挂载的 statfs 走一条独立通道：
//
//   - 在子进程里做（代理二进制自己的 helper 模式），父进程只等有限的时间；
//   - 超时就放弃等待并**隔离这个挂载点**一段时间。子进程可能还挂在 D 状态里杀不掉，
//     隔离保证我们不会每 30 秒再派一个新的上去，把进程表堆满。
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"upanel/internal/syscmd"
)

// ErrCapacityTimeout 表示容量查询在时限内没有返回。
// 它与“statfs 返回错误”不同：后者说明文件系统还在，只是问不出数字。
var ErrCapacityTimeout = errors.New("storage: 容量查询超时")

// CapacityQuarantine 是超时之后不再重试的时长。
//
// 选 5 分钟不是估值：它足够让一次网络抖动或远端重启过去，又短于 SMART 的 15 分钟，
// 使“网络恢复后页面多久能自愈”有一个可预期的上限。
const CapacityQuarantine = 5 * time.Minute

// DefaultNetworkCapacityTimeout 是网络挂载容量查询的时限。
// 正常的 NFS/CIFS statfs 是毫秒级；超过 10 秒说明远端已经不可达。
const DefaultNetworkCapacityTimeout = 10 * time.Second

// StatfsHelperFlag 是把代理二进制切到 helper 模式的命令行开关。
//
// 它不是给运维用的功能入口：只接受一个路径，只做 statfs，不读任何文件内容，
// 也不返回目录项。存在的唯一理由是让 statfs 能在一个可以被父进程放弃的子进程里执行。
const StatfsHelperFlag = "--statfs-helper"

// IsNetworkFS 判断一个挂载类型是否属于“远端可能不可达”的文件系统。
//
// 判断依据是 /proc/self/mountinfo 里的 fstype，不看挂载点路径写法
// （/mnt/nfs 是命名习惯，不是协议）。
func IsNetworkFS(fsType string) bool {
	switch strings.ToLower(fsType) {
	case "nfs", "nfs4", "cifs", "smb3", "smbfs", "fuse.sshfs", "sshfs",
		"9p", "ceph", "glusterfs", "afs", "ncpfs", "davfs":
		return true
	}
	return false
}

// StatfsRunner 在“可以被放弃”的地方对挂载点做一次 statfs。
// 生产实现是 ExecStatfsHelper（子进程）；测试可以注入一个会超时的假实现。
type StatfsRunner interface {
	Statfs(ctx context.Context, mountPath string) (*Capacity, error)
}

// OSCapacityReader 是生产用的容量读取器：本地挂载直接 statfs，网络挂载交给隔离通道。
type OSCapacityReader struct {
	// Helper 是隔离子进程；nil 时网络挂载也直接 statfs（仅用于测试与非 Linux 构建）。
	Helper StatfsRunner
	// Timeout 是网络挂载查询的时限；0 时用 DefaultNetworkCapacityTimeout。
	Timeout time.Duration
	// Now 是时钟，测试可以固定；nil 时用 time.Now。
	Now func() time.Time

	mu         sync.Mutex
	quarantine map[string]time.Time // 挂载点 → 解禁时间
}

func (o *OSCapacityReader) now() time.Time {
	if o != nil && o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

func (o *OSCapacityReader) timeout() time.Duration {
	if o != nil && o.Timeout > 0 {
		return o.Timeout
	}
	return DefaultNetworkCapacityTimeout
}

// Capacity 读取一个挂载点的容量。
//
// fsType 决定走哪条通道：网络文件系统走隔离子进程，其余在本进程内 statfs。
func (o *OSCapacityReader) Capacity(ctx context.Context, mountPath, fsType string) (*Capacity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o == nil || o.Helper == nil || !IsNetworkFS(fsType) {
		capv, err := statfsCapacity(mountPath)
		if err != nil {
			return nil, err
		}
		capv.CollectedAt = o.now()
		return capv, nil
	}

	if until, blocked := o.quarantined(mountPath); blocked {
		return nil, fmt.Errorf("%w: %s 在 %s 之前不再重试（上一次查询没有返回）",
			ErrCapacityTimeout, mountPath, until.Format(time.RFC3339))
	}

	runCtx, cancel := context.WithTimeout(ctx, o.timeout())
	defer cancel()
	capv, err := o.Helper.Statfs(runCtx, mountPath)
	if err != nil {
		if errors.Is(err, ErrCapacityTimeout) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			o.quarantineUntil(mountPath, o.now().Add(CapacityQuarantine))
			return nil, fmt.Errorf("%w: %s 超过 %s 没有返回，%s 内不再重试",
				ErrCapacityTimeout, mountPath, o.timeout(), CapacityQuarantine)
		}
		return nil, err
	}
	capv.CollectedAt = o.now()
	return capv, nil
}

func (o *OSCapacityReader) quarantined(path string) (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	until, ok := o.quarantine[path]
	if !ok {
		return time.Time{}, false
	}
	if !o.now().Before(until) {
		// 解禁：下一次重新尝试。失败会再次进入隔离。
		delete(o.quarantine, path)
		return time.Time{}, false
	}
	return until, true
}

func (o *OSCapacityReader) quarantineUntil(path string, until time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.quarantine == nil {
		o.quarantine = map[string]time.Time{}
	}
	o.quarantine[path] = until
}

// ExecStatfsHelper 通过重新执行代理二进制（helper 模式）来做 statfs。
//
// 为什么是“自己”而不是找一个小工具：系统里没有哪个命令能保证“只做 statfs 且输出机器可读”，
// 调 stat/df 要解析本地化文本（PLAN.md 第 8 节明确不允许），而重新执行自己是零依赖的。
type ExecStatfsHelper struct {
	// Path 是代理可执行文件的绝对路径。
	Path string
	// Timeout 是子进程的运行时限；0 时用 DefaultNetworkCapacityTimeout。
	Timeout time.Duration
	// Run 是执行入口，测试可以注入；nil 时用 syscmd.Run。
	Run func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// helperOutput 是 helper 子进程的输出。
type helperOutput struct {
	Capacity *Capacity `json:"capacity"`
	Error    string    `json:"error"`
}

func (h ExecStatfsHelper) Statfs(ctx context.Context, mountPath string) (*Capacity, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultNetworkCapacityTimeout
	}
	run := h.Run
	if run == nil {
		run = syscmd.Run
	}
	res, err := run(ctx, syscmd.Spec{Program: h.Path, Args: []string{StatfsHelperFlag, mountPath}},
		syscmd.Limits{Timeout: timeout, MaxStdout: 64 << 10})

	// 超时优先判断：这时子进程可能还挂在内核 I/O 里，输出无从谈起。
	if errors.Is(err, syscmd.ErrTimeout) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: statfs %s", ErrCapacityTimeout, mountPath)
	}
	if err != nil {
		var exitErr *syscmd.ExitError
		if !errors.As(err, &exitErr) {
			// 启动失败（helper 路径不对、权限不足）：这是配置问题，要如实报出来。
			return nil, fmt.Errorf("storage: 启动容量 Helper 失败: %w", err)
		}
	}
	var out helperOutput
	if parseErr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); parseErr != nil {
		return nil, fmt.Errorf("storage: 容量 Helper 输出不是合法 JSON（退出状态 %d）: %v",
			res.ExitCode, parseErr)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrCapacityUnavailable, out.Error)
	}
	if out.Capacity == nil {
		return nil, fmt.Errorf("%w: 容量 Helper 没有返回容量", ErrCapacityUnavailable)
	}
	return out.Capacity, nil
}

// RunStatfsHelper 是 helper 子进程的入口：对 argv 里的一个路径做 statfs，
// 把结果以 JSON 打到 stdout。
//
// 返回值用作进程退出码：0 表示成功，2 表示 statfs 失败（错误信息在 JSON 里）。
// 它不读目录、不读文件内容、不跟随符号链接做事，唯一的系统调用是 statfs。
func RunStatfsHelper(args []string) int {
	writeErr := func(msg string) int {
		b, _ := json.Marshal(helperOutput{Error: msg})
		_, _ = os.Stdout.Write(append(b, '\n'))
		return 2
	}
	if len(args) != 1 {
		return writeErr("需要且只需要一个挂载点路径")
	}
	path := args[0]
	if !strings.HasPrefix(path, "/") {
		return writeErr("挂载点必须是绝对路径")
	}
	capv, err := statfsCapacity(path)
	if err != nil {
		return writeErr(err.Error())
	}
	b, err := json.Marshal(helperOutput{Capacity: capv})
	if err != nil {
		return writeErr("序列化容量失败: " + err.Error())
	}
	_, _ = os.Stdout.Write(append(b, '\n'))
	return 0
}
