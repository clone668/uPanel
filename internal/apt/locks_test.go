package apt

// 锁探测（P11.2）。STAGES §13 的第一条验收：外部 apt 正在运行时不删锁、不并发。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLockPathsFixedOrder：四把锁按固定顺序返回（日志与测试要比对）。
func TestLockPathsFixedOrder(t *testing.T) {
	paths := DefaultPaths()
	got := paths.LockPaths()
	want := []string{
		DefaultDpkgLockFrontend,
		DefaultDpkgLock,
		DefaultAptListsLock,
		DefaultAptArchivesLock,
	}
	if len(got) != len(want) {
		t.Fatalf("应有四把锁: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 把锁应为 %s，实际 %s", i, want[i], got[i])
		}
	}
	// 空路径不进列表（配置成空串表示这把锁不探测）。
	empty := Paths{}
	if n := len(empty.LockPaths()); n != 0 {
		t.Fatalf("空 Paths 不该产出锁: %d", n)
	}
}

// TestProbeLocksNeverCreatesFiles：探测一个不存在的路径不能把它建出来，也不能算成 busy。
//
// 这里**不断言 Error 的取值**：文件不存在该不该算"读不到"是平台语义，unix 实现
// （probelock_unix.go）刻意把它当成"这把锁没有意义"，宿主上没有 flock 语义则如实
// 报不支持。原来这条断言的是 Error != ""，那是照着 Windows 宿主的行为写的，
// 于是它在 Linux 上一跑就红——P11.2 的 apt 单测第一次在真 VM 上执行时才暴露出来。
func TestProbeLocksNeverCreatesFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there.lock")
	states := ProbeLocks([]string{missing})
	if len(states) != 1 {
		t.Fatalf("应有一条结论: %v", states)
	}
	state := states[0]
	if state.Held {
		t.Fatal("文件不存在不等于被占用")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("探测不该创建锁文件")
	}
	if busy := BusyLocks(states); len(busy) != 0 {
		t.Fatalf("读不到不能算 busy: %+v", busy)
	}
}

// TestProbeLocksReportsUndecidablePathAsErrorNotBusy：路径长到操作系统都打不开时，
// 结论必须是"读不到"而不是"被占用"——把权限/路径问题说成 apt 正在运行，会让人
// 一直等到一个永远不会结束的"忙"。
//
// 这一条放平台无关文件里是有意的：两个平台的实现都该这样，而它又能在 root 下跑
// （真造 EACCES 得换普通用户，root 做不到）。验收脚本里 APT_LOCK_UNREADABLE 那条
// NOT RUN 说的"分支由 internal/apt/locks_test.go 覆盖"，指的就是这条。
func TestProbeLocksReportsUndecidablePathAsErrorNotBusy(t *testing.T) {
	tooLong := filepath.Join(t.TempDir(), strings.Repeat("x", 5000))
	states := ProbeLocks([]string{tooLong})
	if len(states) != 1 {
		t.Fatalf("应有一条结论: %v", states)
	}
	if states[0].Held {
		t.Fatal("打不开不等于被占用")
	}
	if states[0].Error == "" {
		t.Fatalf("打不开要如实报错，不能静默当成空闲: %+v", states[0])
	}
	if busy := BusyLocks(states); len(busy) != 0 {
		t.Fatalf("读不到不能算 busy: %+v", busy)
	}
}

// TestBusyLocksFilters：只有 Held 为真的才进 busy 列表。
func TestBusyLocksFilters(t *testing.T) {
	states := []LockState{
		{Path: "/a", Held: true},
		{Path: "/b"},
		{Path: "/c", Error: "权限不足"},
		{Path: "/d", Held: true},
	}
	busy := BusyLocks(states)
	if len(busy) != 2 || busy[0].Path != "/a" || busy[1].Path != "/d" {
		t.Fatalf("busy 过滤错: %+v", busy)
	}
}
