package storage

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// 网络挂载的容量查询是本阶段最“反直觉”的一块：需要验证的不是“能读到容量”，
// 而是**读不到的时候不要把采集器一起拖死**。用例因此集中在隔离、超时与解禁上。
//
// 这些用例用的是假 Helper（真 Helper 要 fork 一个会卡在 statfs 里的子进程，
// 只有真机能造出来）。真机上的表现见 docs/PROGRESS.md 的 P04.3 校验表。

// hangingHelper 是一个永远不返回的 StatfsRunner，只有 ctx 结束才放手。
type hangingHelper struct {
	mu    sync.Mutex
	calls int
}

func (h *hangingHelper) Statfs(ctx context.Context, mountPath string) (*Capacity, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (h *hangingHelper) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// TestOSCapacityReaderNetworkTimeoutQuarantines 是“不可达的 NFS 不会拖垮采集”的核心用例：
// 第一次查询超时后，同一个挂载点在隔离期内不再被查询。
func TestOSCapacityReaderNetworkTimeoutQuarantines(t *testing.T) {
	helper := &hangingHelper{}
	now := fixedNow
	reader := &OSCapacityReader{
		Helper:  helper,
		Timeout: 20 * time.Millisecond,
		Now:     func() time.Time { return now },
	}

	capv, err := reader.Capacity(context.Background(), "/mnt/nfs", "nfs4")
	if capv != nil {
		t.Fatalf("超时却返回了容量 %+v", capv)
	}
	if !errors.Is(err, ErrCapacityTimeout) {
		t.Fatalf("err = %v，想要 ErrCapacityTimeout", err)
	}
	if helper.count() != 1 {
		t.Fatalf("Helper 调用次数 = %d，想要 1", helper.count())
	}

	// 隔离期内：必须直接拒绝，不再派出新的子进程。
	start := time.Now()
	if _, err := reader.Capacity(context.Background(), "/mnt/nfs", "nfs4"); !errors.Is(err, ErrCapacityTimeout) {
		t.Fatalf("隔离期内 err = %v，想要 ErrCapacityTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Errorf("隔离期内还等了 %v：应当在内存里直接拒绝", elapsed)
	}
	if helper.count() != 1 {
		t.Errorf("隔离期内又派了子进程（%d 次）：D 状态的子进程会越堆越多", helper.count())
	}
	if !strings.Contains(err.Error(), "不再重试") {
		t.Errorf("错误信息应说明处于隔离：%v", err)
	}
}

// TestOSCapacityReaderQuarantineExpires 隔离到期后要重新尝试，而不是永久黑名单。
func TestOSCapacityReaderQuarantineExpires(t *testing.T) {
	helper := &hangingHelper{}
	now := fixedNow
	reader := &OSCapacityReader{
		Helper:  helper,
		Timeout: 20 * time.Millisecond,
		Now:     func() time.Time { return now },
	}

	if _, err := reader.Capacity(context.Background(), "/mnt/nfs", "nfs4"); !errors.Is(err, ErrCapacityTimeout) {
		t.Fatalf("第一次 err = %v", err)
	}
	now = now.Add(CapacityQuarantine)
	if _, err := reader.Capacity(context.Background(), "/mnt/nfs", "nfs4"); !errors.Is(err, ErrCapacityTimeout) {
		t.Fatalf("解禁后 err = %v", err)
	}
	if helper.count() != 2 {
		t.Errorf("解禁后 Helper 调用次数 = %d，想要 2（要重新试一次）", helper.count())
	}
}

// TestOSCapacityReaderLocalNeverSpawns 本地文件系统不走子进程：
// ext4 上 statfs 是微秒级的，为它 fork 一个进程纯属浪费。
func TestOSCapacityReaderLocalNeverSpawns(t *testing.T) {
	helper := &fakeStatfsRunner{cap: &Capacity{TotalBytes: 1, AvailableBytes: 1}}
	reader := &OSCapacityReader{Helper: helper, Timeout: time.Second}

	// 本地路径：在 Linux 上会成功，在 Windows（开发宿主）上会返回 ErrCapacityUnavailable。
	// 两种情况都要求同一个断言：Helper 没被调用。
	_, err := reader.Capacity(context.Background(), t.TempDir(), "ext4")
	if err != nil && !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("本地挂载的 err = %v，只允许 ErrCapacityUnavailable", err)
	}
	if helper.calls != 0 {
		t.Errorf("本地挂载派了 %d 次子进程，应当直接 statfs", helper.calls)
	}
}

// TestOSCapacityReaderHelperNilFallsBack：没有配置 Helper 时网络挂载也直接 statfs。
// 这条路径只用于测试与非 Linux 构建，但要保证它不会 panic。
func TestOSCapacityReaderHelperNilFallsBack(t *testing.T) {
	reader := &OSCapacityReader{}
	if _, err := reader.Capacity(context.Background(), "/mnt/nfs", "nfs4"); err == nil {
		t.Log("本机 statfs 成功了（/mnt/nfs 存在）；不影响结论")
	}
}

// TestOSCapacityReaderContextCancelled：调用方已经取消时不该再启动任何东西。
func TestOSCapacityReaderContextCancelled(t *testing.T) {
	helper := &hangingHelper{}
	reader := &OSCapacityReader{Helper: helper, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := reader.Capacity(ctx, "/mnt/nfs", "nfs4"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v，想要 context.Canceled", err)
	}
	if helper.count() != 0 {
		t.Errorf("已取消的 ctx 仍然派出了子进程")
	}
}

// fakeStatfsRunner 返回预设结果，并记录调用次数。
type fakeStatfsRunner struct {
	calls   int
	mu      sync.Mutex
	cap     *Capacity
	err     error
	results []syscmd.Result
	runErrs []error
}

func (f *fakeStatfsRunner) Statfs(ctx context.Context, mountPath string) (*Capacity, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.cap, f.err
}

// TestExecStatfsHelperParsesOutput 校验父进程对 helper 输出的解释：
// 成功、业务错误、坏 JSON、超时、启动失败要分成五类，不能都变成“读不到容量”。
func TestExecStatfsHelperParsesOutput(t *testing.T) {
	cases := []struct {
		name    string
		result  syscmd.Result
		err     error
		wantErr func(error) bool
		reason  string
	}{
		{
			name:   "正常输出",
			result: syscmd.Result{ExitCode: 0, Stdout: `{"capacity":{"total_bytes":"100","available_bytes":"40"}}`},
		},
		{
			name:    "helper 报 statfs 失败",
			result:  syscmd.Result{ExitCode: 2, Stdout: `{"error":"statfs 失败: stale file handle"}`},
			err:     &syscmd.ExitError{Program: "/usr/local/bin/upanel-agent", ExitCode: 2},
			wantErr: func(err error) bool { return errors.Is(err, ErrCapacityUnavailable) },
			reason:  "stale file handle",
		},
		{
			name:    "输出不是 JSON",
			result:  syscmd.Result{ExitCode: 0, Stdout: "df: /mnt/nfs: Stale file handle"},
			wantErr: func(err error) bool { return strings.Contains(err.Error(), "不是合法 JSON") },
			reason:  "JSON",
		},
		{
			name:    "超时",
			result:  syscmd.Result{ExitCode: -1, TimedOut: true},
			err:     syscmd.ErrTimeout,
			wantErr: func(err error) bool { return errors.Is(err, ErrCapacityTimeout) },
			reason:  "超时",
		},
		{
			name:    "helper 启动失败",
			result:  syscmd.Result{ExitCode: -1},
			err:     errors.New("fork/exec: permission denied"),
			wantErr: func(err error) bool { return strings.Contains(err.Error(), "启动容量 Helper 失败") },
			reason:  "permission denied",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{result: tc.result, err: tc.err}
			helper := ExecStatfsHelper{Path: "/usr/local/bin/upanel-agent", Run: runner.run}

			capv, err := helper.Statfs(context.Background(), "/mnt/nfs")
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v，想要成功", err)
				}
				if capv == nil || capv.TotalBytes != 100 || capv.AvailableBytes != 40 {
					t.Fatalf("cap = %+v，想要 100/40", capv)
				}
				return
			}
			if err == nil {
				t.Fatalf("想要错误，得到 cap=%+v", capv)
			}
			if !tc.wantErr(err) {
				t.Errorf("err = %v，不符合预期类别", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("err = %v，应包含 %q", err, tc.reason)
			}
		})
	}
}

// TestExecStatfsHelperArgs 校验调用方式：绝对路径 + helper 开关 + 一个挂载点，
// 不经过 shell，并且带超时。
func TestExecStatfsHelperArgs(t *testing.T) {
	runner := &fakeRunner{result: syscmd.Result{ExitCode: 0, Stdout: `{"capacity":{"total_bytes":"1","available_bytes":"1"}}`}}
	helper := ExecStatfsHelper{Path: "/usr/local/bin/upanel-agent", Timeout: 3 * time.Second, Run: runner.run}

	if _, err := helper.Statfs(context.Background(), "/mnt/nfs share"); err != nil {
		t.Fatalf("err = %v", err)
	}
	spec := runner.specs[0]
	if spec.Program != "/usr/local/bin/upanel-agent" {
		t.Errorf("Program = %q", spec.Program)
	}
	want := []string{StatfsHelperFlag, "/mnt/nfs share"}
	if strings.Join(spec.Args, "|") != strings.Join(want, "|") {
		t.Errorf("Args = %v，想要 %v（路径里的空格应当原样作为参数传递）", spec.Args, want)
	}
	if runner.limits[0].Timeout != 3*time.Second {
		t.Errorf("Timeout = %v，想要 3s", runner.limits[0].Timeout)
	}
	if runner.limits[0].MaxStdout == 0 {
		t.Errorf("helper 的输出要有上限")
	}
}

// TestRunStatfsHelperRejectsBadArgs 校验 helper 入口只接受一个绝对路径。
// 这个入口是安全边界的一部分：它绝不能变成“执行任意东西”的通道。
func TestRunStatfsHelperRejectsBadArgs(t *testing.T) {
	cases := [][]string{
		{},                       // 没有参数
		{"/mnt", "/mnt"},         // 两个参数
		{"relative/path"},        // 相对路径
		{"--statfs-helper", "x"}, // 看着像路径其实不是
	}
	for _, args := range cases {
		if code := RunStatfsHelper(args); code != 2 {
			t.Errorf("RunStatfsHelper(%v) = %d，想要 2", args, code)
		}
	}
}

// TestRunStatfsHelperSucceedsOnRealPath 在 Linux 上对真实目录跑一遍 helper 逻辑。
func TestRunStatfsHelperSucceedsOnRealPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("statfs 只在 Linux 上可用，本条在验收 VM 上执行")
	}
	if code := RunStatfsHelper([]string{t.TempDir()}); code != 0 {
		t.Errorf("RunStatfsHelper(临时目录) = %d，想要 0", code)
	}
	if code := RunStatfsHelper([]string{"/nonexistent-upanel-check"}); code != 2 {
		t.Errorf("不存在的路径 = %d，想要 2", code)
	}
}

// TestCapacityReasonDistinguishes 校验失败原因的人话翻译：
// “超时”与“读不出来”要分开，前者意味着远端不可达。
func TestCapacityReasonDistinguishes(t *testing.T) {
	timeout := ErrCapacityTimeout
	if got := capacityReason(timeout); !strings.Contains(got, "超时") {
		t.Errorf("capacityReason(ErrCapacityTimeout) = %q", got)
	}
	unavailable := ErrCapacityUnavailable
	if got := capacityReason(unavailable); !strings.Contains(got, "无法读取容量") {
		t.Errorf("capacityReason(ErrCapacityUnavailable) = %q", got)
	}
	other := errors.New("stale file handle")
	if got := capacityReason(other); got != "stale file handle" {
		t.Errorf("capacityReason(其他) = %q，想要原文", got)
	}
}
