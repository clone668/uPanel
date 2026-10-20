package agent

// 资源锁（CONTRACTS.md 第 15 节：锁统一由代理维护）。
//
// 这是**进程内**的互斥，只解决“同一个代理里两个操作同时改同一块资源”。它不是完整的
// 锁方案：外部 apt、systemctl、配置文件编辑器不认这把锁，所以真实动作在 P04 起还必须
// 检测系统锁与配置哈希。P03.2 先把机制建好并如实标注边界。
//
// 死锁避免靠**顺序**：获取前把锁名排序（normalizeLocks 已排序），任何操作都按同一顺序
// 拿锁，因此不存在“A 持锁 1 等锁 2、B 持锁 2 等锁 1”。等不到就在 30 秒后返回
// RESOURCE_BUSY（CONTRACTS.md 4.2：锁冲突可在 30 秒内排队，不能无限占用 worker）。

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// LockQueueTimeout 是等待资源锁的上限（CONTRACTS.md 4.2：30 秒）。
const LockQueueTimeout = 30 * time.Second

// lockRetryInterval 是排队期间的重试间隔。
//
// 不用条件变量：锁的持有者可能崩溃或被取消，等待方轮询一个固定间隔比维护唤醒列表
// 更难出错；30 秒上限意味着最多几百次尝试，代价可以忽略。
const lockRetryInterval = 25 * time.Millisecond

// ErrResourceBusy 表示在排队时限内没能拿到全部资源锁。
var ErrResourceBusy = errors.New("agent: 资源被占用，排队超时")

// lockManager 维护“锁名 → 持有者 operation_id”的映射。
type lockManager struct {
	mu   sync.Mutex
	held map[string]string
}

func newLockManager() *lockManager {
	return &lockManager{held: make(map[string]string)}
}

// acquire 按给定的**已排序**锁列表依次获取，返回释放函数。
//
// 要么全部拿到，要么一把都不留：任何一把在时限内拿不到，已经把拿到的部分释放掉再返回
// ErrResourceBusy。残留半套锁会让别的操作永远等一个已经放弃的人。
func (m *lockManager) acquire(ctx context.Context, operationID string, locks []string, timeout time.Duration) (func(), error) {
	if len(locks) == 0 {
		return func() {}, nil
	}
	if !sort.StringsAreSorted(locks) {
		// 排序是死锁避免的前提，不排序就别拿锁。
		return nil, errors.New("agent: 资源锁列表必须先排序再获取")
	}
	if timeout <= 0 {
		timeout = LockQueueTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		if held, ok := m.tryLockAll(operationID, locks); ok {
			return func() { m.release(operationID, held) }, nil
		}
		if time.Now().After(deadline) {
			return nil, ErrResourceBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockRetryInterval):
		}
	}
}

// tryLockAll 一次性尝试获取全部锁；失败时不留下任何部分持有。
//
// 返回的是**本次真正拿到的**锁：同一个操作重复获取同一把锁时，那把锁不算新的，
// 因此先释放后拿到的那一份不会把别人还需要的锁提前放掉。
func (m *lockManager) tryLockAll(operationID string, locks []string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range locks {
		if owner, taken := m.held[name]; taken && owner != operationID {
			return nil, false
		}
	}
	fresh := make([]string, 0, len(locks))
	for _, name := range locks {
		if _, taken := m.held[name]; taken {
			continue
		}
		m.held[name] = operationID
		fresh = append(fresh, name)
	}
	return fresh, true
}

// release 释放该操作持有的锁。重复释放或释放别人的锁都不做任何事。
func (m *lockManager) release(operationID string, locks []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range locks {
		if m.held[name] == operationID {
			delete(m.held, name)
		}
	}
}

// holder 返回某把锁当前的持有者（供测试与诊断）。
func (m *lockManager) holder(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.held[name]
	return owner, ok
}

// heldCount 返回当前持有的锁数量（供测试与优雅停机的断言使用）。
func (m *lockManager) heldCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held)
}
