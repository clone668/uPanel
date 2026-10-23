//go:build unix

package apt

// 真 flock 的探测（P11.2）。只在 unix 上跑：Windows 开发宿主没有这个语义，
// 那边的实现刻意返回"读不到"而不是"空闲"。

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestProbeLockDetectsHeldFlock 用一个真实持有的 flock 验证探测结论。
//
// 这是整条并发防线的基础：如果探测在锁被持有时说"空闲"，面板就会和外部 apt
// 同时写 dpkg 数据库；如果它在锁空闲时说"被占用"，面板就永远装不了包。
func TestProbeLockDetectsHeldFlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		t.Fatalf("建锁文件失败: %v", err)
	}
	defer f.Close()

	// 没有别人持有：探测应当报"空闲"且不报错。
	held, err := probeLock(path)
	if err != nil {
		t.Fatalf("空闲文件探测报错: %v", err)
	}
	if held {
		t.Fatal("没人持有时不该报被占用（否则面板永远装不了包）")
	}

	// 模拟一个外部 apt：拿住排他锁不放。
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("拿锁失败: %v", err)
	}
	held, err = probeLock(path)
	if err != nil {
		t.Fatalf("被持有时探测报错: %v", err)
	}
	if !held {
		t.Fatal("锁被持有却报空闲——两个 apt 会同时写 dpkg 数据库")
	}

	// 释放后必须立刻回到"空闲"（探测自己持的那一下要放掉）。
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("放锁失败: %v", err)
	}
	held, err = probeLock(path)
	if err != nil || held {
		t.Fatalf("释放后应报空闲: held=%v err=%v", held, err)
	}

	// 探测不能删除、不能清空锁文件。
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("锁文件被动了: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("探测不该往锁文件里写东西: %d 字节", info.Size())
	}
}
