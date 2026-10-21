package storage

// 占用检查的测试（P07.1）。
//
// 真实系统上没法让某个进程“正好打开着 /dev/sdb”来跑测试，而创建一个真块设备节点
// 需要 root。所以这里把两件事拆开：
//   - **/proc 的结构**用夹具目录模拟（pid 目录、fd 目录、comm 文件）；
//   - **设备号的取得**用注入的 MajorMinor 函数，从一个表里查。
//
// 判据（按 major:minor 比对，而不是按路径）因此可以被精确验证：夹具里可以让
// 两个不同路径指向同一个设备号，也可以让一个进程的 fd 指向普通文件。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProcs 建一棵 /proc 夹具：pids 是 pid → (comm, fd 名 → 设备号)。
type fakeProcs map[string]struct {
	comm string
	fds  map[string]string
}

func buildProc(t *testing.T, root string, procs fakeProcs) {
	t.Helper()
	for pid, p := range procs {
		dir := filepath.Join(root, pid)
		mustMkdirAll(t, filepath.Join(dir, "fd"))
		mustWriteFile(t, filepath.Join(dir, "comm"), p.comm+"\n")
		for fd := range p.fds {
			// 内容无所谓：设备号来自注入的 MajorMinor，真实实现读的是 st_rdev。
			mustWriteFile(t, filepath.Join(dir, "fd", fd), "")
		}
	}
	// 非 pid 的条目（Linux 的 /proc 里有 self、meminfo 等）必须被忽略。
	mustMkdirAll(t, filepath.Join(root, "sys"))
}

func fakeMajorMinor(procs fakeProcs) func(string) (string, error) {
	return func(fdPath string) (string, error) {
		parts := strings.Split(filepath.ToSlash(fdPath), "/")
		if len(parts) < 3 {
			return "", nil
		}
		pid, fd := parts[len(parts)-3], parts[len(parts)-1]
		if p, ok := procs[pid]; ok {
			return p.fds[fd], nil
		}
		return "", nil
	}
}

// TestProcBusyScannerFindsHolder 校验“有进程打开着这块设备”能被找到，
// 而且同一个进程的多个 fd 只算一次。
func TestProcBusyScannerFindsHolder(t *testing.T) {
	dir := t.TempDir()
	procs := fakeProcs{
		"100": {"dd", map[string]string{"3": "8:16", "4": "8:16"}},
		"200": {"nginx", map[string]string{"0": "8:1", "5": "8:16"}},
		"300": {"idle", map[string]string{"1": ""}},
	}
	buildProc(t, dir, procs)
	sys := filepath.Join(dir, "sys")
	mustMkdirAll(t, filepath.Join(sys, "block", "sdb"))
	mustWriteFile(t, filepath.Join(sys, "block", "sdb", "dev"), "8:16\n")

	scanner := ProcBusyScanner{Proc: dir, Sys: sys, MajorMinor: fakeMajorMinor(procs)}
	got, err := scanner.Scan(context.Background(), "sdb")
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("占用进程 = %+v，期望 2 个（100、200）", got)
	}
	if got[0].PID != "100" || got[1].PID != "200" {
		t.Fatalf("进程没有按 pid 升序去重：%+v", got)
	}
	if got[0].Comm != "dd" {
		t.Errorf("comm = %q，期望 dd", got[0].Comm)
	}
}

// TestProcBusyScannerSkipsSelf 校验代理自己不算占用：
// 它在读签名时会短暂打开设备，把自己算进去会让每次预检都被自己拒绝。
func TestProcBusyScannerSkipsSelf(t *testing.T) {
	dir := t.TempDir()
	procs := fakeProcs{"4242": {"upanel-agent", map[string]string{"7": "8:16"}}}
	buildProc(t, dir, procs)
	sys := filepath.Join(dir, "sys")
	mustMkdirAll(t, filepath.Join(sys, "block", "sdb"))
	mustWriteFile(t, filepath.Join(sys, "block", "sdb", "dev"), "8:16\n")

	scanner := ProcBusyScanner{Proc: dir, Sys: sys, Self: 4242, MajorMinor: fakeMajorMinor(procs)}
	got, err := scanner.Scan(context.Background(), "sdb")
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("把自己算成了占用者：%+v", got)
	}
}

// TestProcBusyScannerClean 校验没人用时返回空列表而不是错误。
func TestProcBusyScannerClean(t *testing.T) {
	dir := t.TempDir()
	procs := fakeProcs{"1": {"systemd", map[string]string{"0": "1:3"}}}
	buildProc(t, dir, procs)
	sys := filepath.Join(dir, "sys")
	mustMkdirAll(t, filepath.Join(sys, "block", "sdc"))
	mustWriteFile(t, filepath.Join(sys, "block", "sdc", "dev"), "8:32\n")

	scanner := ProcBusyScanner{Proc: dir, Sys: sys, MajorMinor: fakeMajorMinor(procs)}
	got, err := scanner.Scan(context.Background(), "sdc")
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空列表期望，实际 %+v", got)
	}
}

// TestProcBusyScannerMissingDevice 校验找不到设备号时报错：
// 不知道设备号就无法判断占用，"看不见"不能被当成"没人用"。
func TestProcBusyScannerMissingDevice(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	mustMkdirAll(t, filepath.Join(sys, "block"))
	scanner := ProcBusyScanner{Proc: dir, Sys: sys}
	if _, err := scanner.Scan(context.Background(), "sdz"); err == nil {
		t.Fatal("找不到设备号却没有报错")
	}
}

// TestProcBusyScannerFailsClosed 校验读 /proc/<pid>/fd 失败时上抛错误，
// 而不是返回"没有人占用"。
//
// 用注入的 ReadDir 制造失败：真机上这对应“权限不足 / 容器视图不完整”，
// 而那正是最需要拒绝的情形。
func TestProcBusyScannerFailsClosed(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	mustMkdirAll(t, filepath.Join(sys, "block", "sdb"))
	mustWriteFile(t, filepath.Join(sys, "block", "sdb", "dev"), "8:16\n")
	mustMkdirAll(t, filepath.Join(dir, "999"))

	scanner := ProcBusyScanner{
		Proc: dir, Sys: sys,
		MajorMinor: func(string) (string, error) { return "", nil },
		ReadDir: func(path string) ([]os.DirEntry, error) {
			if strings.HasSuffix(filepath.ToSlash(path), "/999/fd") {
				return nil, os.ErrPermission
			}
			return os.ReadDir(path)
		},
	}
	_, err := scanner.Scan(context.Background(), "sdb")
	if err == nil {
		t.Fatal("读 /proc/<pid>/fd 失败却返回了空占用列表（把“看不见”当成“没人用”）")
	}
	if !strings.Contains(err.Error(), "占用") {
		t.Fatalf("错误应说明占用检查失败：%v", err)
	}
}

// TestProcBusyScannerProcUnreadable 校验连 /proc 都读不到时报错。
func TestProcBusyScannerProcUnreadable(t *testing.T) {
	scanner := ProcBusyScanner{
		Proc: "/nonexistent-proc",
		Sys:  t.TempDir(),
		ReadDir: func(string) ([]os.DirEntry, error) {
			return nil, os.ErrPermission
		},
		MajorMinor: func(string) (string, error) { return "", nil },
	}
	if _, err := scanner.Scan(context.Background(), "sdb"); err == nil {
		t.Fatal("/proc 读不到却没有报错")
	}
}

// TestProcBusyScannerFdError 校验单个 fd 取不到设备号时上抛错误。
func TestProcBusyScannerFdError(t *testing.T) {
	dir := t.TempDir()
	procs := fakeProcs{"100": {"dd", map[string]string{"3": "8:16"}}}
	buildProc(t, dir, procs)
	sys := filepath.Join(dir, "sys")
	mustMkdirAll(t, filepath.Join(sys, "block", "sdb"))
	mustWriteFile(t, filepath.Join(sys, "block", "sdb", "dev"), "8:16\n")

	scanner := ProcBusyScanner{
		Proc: dir, Sys: sys,
		MajorMinor: func(string) (string, error) { return "", os.ErrPermission },
	}
	if _, err := scanner.Scan(context.Background(), "sdb"); err == nil {
		t.Fatal("fd 取不到设备号却没有报错")
	}
}

// TestIsVanished 校验“进程退出”与“读失败”被区分开。
func TestIsVanished(t *testing.T) {
	if !isVanished(os.ErrNotExist) {
		t.Error("ENOENT 应被当作磁盘上的正常消失")
	}
	if isVanished(errors.New("permission denied")) {
		t.Error("权限错误不能被当成消失")
	}
}
