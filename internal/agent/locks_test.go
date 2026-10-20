package agent

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

func TestLockManagerAcquireRelease(t *testing.T) {
	m := newLockManager()
	ctx := context.Background()
	locks := []string{"disk:dev_a", "disk:dev_b"}

	release, err := m.acquire(ctx, "op_1", locks, time.Second)
	if err != nil {
		t.Fatalf("获取锁失败: %v", err)
	}
	for _, name := range locks {
		if owner, ok := m.holder(name); !ok || owner != "op_1" {
			t.Fatalf("%s 的持有者 = %q, %v", name, owner, ok)
		}
	}
	release()
	if n := m.heldCount(); n != 0 {
		t.Fatalf("释放后仍持有 %d 把锁", n)
	}
}

func TestLockManagerReentrantForSameOperation(t *testing.T) {
	m := newLockManager()
	ctx := context.Background()
	release, err := m.acquire(ctx, "op_1", []string{"a"}, time.Second)
	if err != nil {
		t.Fatalf("获取锁失败: %v", err)
	}
	defer release()
	// 同一个操作再取同一把锁不应自锁。
	release2, err := m.acquire(ctx, "op_1", []string{"a"}, time.Second)
	if err != nil {
		t.Fatalf("同一操作重复获取失败: %v", err)
	}
	release2()
	if owner, _ := m.holder("a"); owner != "op_1" {
		t.Fatalf("持有者 = %q", owner)
	}
}

func TestLockManagerBusyTimesOut(t *testing.T) {
	m := newLockManager()
	ctx := context.Background()
	release, err := m.acquire(ctx, "op_1", []string{"a"}, time.Second)
	if err != nil {
		t.Fatalf("获取锁失败: %v", err)
	}
	defer release()

	start := time.Now()
	if _, err := m.acquire(ctx, "op_2", []string{"a"}, 80*time.Millisecond); !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("错误 = %v，期望 ErrResourceBusy", err)
	}
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("排队只等了 %s，说明没有真的等待", elapsed)
	}
}

func TestLockManagerWaitsThenSucceeds(t *testing.T) {
	m := newLockManager()
	ctx := context.Background()
	release, err := m.acquire(ctx, "op_1", []string{"a"}, time.Second)
	if err != nil {
		t.Fatalf("获取锁失败: %v", err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		release()
	}()
	release2, err := m.acquire(ctx, "op_2", []string{"a"}, 2*time.Second)
	if err != nil {
		t.Fatalf("等锁后获取失败: %v", err)
	}
	release2()
}

func TestLockManagerLeavesNoPartialHold(t *testing.T) {
	m := newLockManager()
	ctx := context.Background()
	release, err := m.acquire(ctx, "op_1", []string{"a"}, time.Second)
	if err != nil {
		t.Fatalf("获取锁失败: %v", err)
	}
	defer release()

	// op_2 想要 a 与 b：a 被占，整组都拿不到，b 也不能被它留下。
	if _, err := m.acquire(ctx, "op_2", []string{"a", "b"}, 50*time.Millisecond); !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("错误 = %v，期望 ErrResourceBusy", err)
	}
	if owner, ok := m.holder("b"); ok && owner == "op_2" {
		t.Fatal("失败的操作残留了部分锁")
	}
	if owner, ok := m.holder("b"); ok {
		t.Fatalf("b 被 %q 占着", owner)
	}
}

func TestLockManagerHonorsContext(t *testing.T) {
	m := newLockManager()
	if _, err := m.acquire(context.Background(), "op_1", []string{"a"}, time.Second); err != nil {
		t.Fatalf("获取锁失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.acquire(ctx, "op_2", []string{"a"}, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
}

func TestLockManagerRequiresSortedLocks(t *testing.T) {
	m := newLockManager()
	unsorted := []string{"disk:dev_b", "disk:dev_a"}
	if sort.StringsAreSorted(unsorted) {
		t.Fatal("测试数据本身是排好序的")
	}
	if _, err := m.acquire(context.Background(), "op_1", unsorted, time.Second); err == nil {
		t.Fatal("未排序的锁列表必须被拒绝（排序是死锁避免的前提）")
	}
}

func TestLockManagerEmptyLocks(t *testing.T) {
	m := newLockManager()
	release, err := m.acquire(context.Background(), "op_1", nil, time.Second)
	if err != nil {
		t.Fatalf("无锁获取失败: %v", err)
	}
	release()
	if n := m.heldCount(); n != 0 {
		t.Fatalf("持有 %d 把锁", n)
	}
}
