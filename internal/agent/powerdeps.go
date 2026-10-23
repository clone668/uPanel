package agent

// 把“哪些写操作正在跑”借给 system.power。
//
// 装配顺序上有一个环：动作登记表在 `agent.New` **之前**就建好了（P11.2 的四个包动作
// 也一样），而“谁在跑”只有 Server 知道。这里沿用 P08.3 的 ShareDependents 那套做法：
// 先把一个空的持有者交给执行器，等 Server 建好之后再 Bind。
//
// 没接线之前 InFlight 返回错误（而不是空列表）。这个选择是这一层唯一重要的地方：
// **查不到不等于没有**。一次重启把别人的 package.remove 断在 dpkg 写库的中间，
// 代价是那台机器下次要人工修——比“这次重启被拒绝”贵得多。

import (
	"errors"
	"sync"

	"upanel/internal/power"
)

// PowerDependents 实现 power.InFlightSource。
type PowerDependents struct {
	mu  sync.RWMutex
	srv *Server
}

// NewPowerDependents 返回一个待接线的持有者。
func NewPowerDependents() *PowerDependents { return &PowerDependents{} }

// Bind 接线。装配层在 agent.New 之后调用一次。
func (d *PowerDependents) Bind(s *Server) {
	d.mu.Lock()
	d.srv = s
	d.mu.Unlock()
}

// InFlight 列出在跑的写操作，排除 excludeOperationID（电源动作自己）。
func (d *PowerDependents) InFlight(excludeOperationID string) ([]power.InFlight, error) {
	if d == nil {
		return nil, errors.New("电源依赖没有接线")
	}
	d.mu.RLock()
	srv := d.srv
	d.mu.RUnlock()
	if srv == nil || srv.runner == nil {
		return nil, errors.New("代理还没接管执行器，无法确认在跑的任务")
	}
	ops := srv.runner.runningOperations(excludeOperationID)
	out := make([]power.InFlight, 0, len(ops))
	for _, op := range ops {
		out = append(out, power.InFlight{OperationID: op.ID, Action: op.Action})
	}
	return out, nil
}
