package hostcaps

// 结果的缓存（P16.1）。
//
// 一次探测要跑十几条版本命令。它不能挂在 HTTP 请求路径上：代理的 capabilities
// 是一条随时会被读的端点，让每一次读都等 20 个子进程，等于把一个便宜的事实变成
// 一个昂贵的依赖。也不能因此不探——那就回到了“dependencies 永远是空数组”。
//
// 所以：代理启动后**后台**探一次，之后按固定间隔重探；端点只读缓存。
// 缓存里还没有读数时如实报告“还没探完”（Ready=false），而不是给一个空数组——
// 空数组会被读成“这台机器什么依赖都没有”。

import (
	"context"
	"sync"
	"time"
)

// DefaultInterval 是后台重探的间隔。
//
// 十秒与十分钟之间没有中间地带值得挑：装一个包之后人不希望等十分钟才看到它出现，
// 而这条命令的真实触发时机只有一个——启动之后，以及有人刚装完东西。
// 十分钟是“装完稍等就有了”与“别让面板每分每秒 fork 二十次”之间能接受的一档。
const DefaultInterval = 10 * time.Minute

// Cache 是探测结果的缓存。
//
// 零值不可用：用 NewCache 构造。
type Cache struct {
	deps     Deps
	interval time.Duration

	mu       sync.RWMutex
	report   Report
	ready    bool
	probedAt time.Time
	runs     int
}

// NewCache 构造缓存。interval <= 0 时用 DefaultInterval。
func NewCache(deps Deps, interval time.Duration) *Cache {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Cache{deps: deps, interval: interval}
}

// Refresh 同步探一次并替换缓存。它返回这次的结果与错误。
//
// 取消（ctx.Done）不会写进缓存：半份报告比旧报告更误导人。
func (c *Cache) Refresh(ctx context.Context) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	report := Probe(ctx, c.deps)
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	c.mu.Lock()
	c.report = report
	c.ready = true
	c.probedAt = time.Now()
	c.runs++
	c.mu.Unlock()
	return report, nil
}

// Snapshot 返回最近一次探测的结果。
//
// 第二个返回值表示“有没有探过”。false 时第一个返回值是零值 Report
// （Items 为 nil）——调用方必须把这个状态如实说出去，不能渲染成“没有依赖”。
//
// 返回的 Items 切片与缓存共享，调用方**不得修改**。Refresh 每次都构造新的
// Report，从不原地改旧的那一份，所以并发读是安全的。
func (c *Cache) Snapshot() (Report, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.report, c.ready
}

// ProbedAt 返回最近一次成功探测的时刻；没探过时为零值。
func (c *Cache) ProbedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.probedAt
}

// Runs 返回成功探测的次数。供测试核对“缓存有没有真的挡住重复探测”。
func (c *Cache) Runs() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.runs
}

// Start 在后台探一次，然后按间隔重探，直到 ctx 结束。
//
// 它不阻塞调用方：代理启动不该等这十几条命令。探测失败（比如 ctx 被取消）
// 只体现在“没有新读数”上，缓存里旧的那一份继续可用——十秒前的 restic 版本
// 仍然比“不知道”有用，而清空缓存会让界面在下次探测成功前空一片。
func (c *Cache) Start(ctx context.Context) {
	go func() {
		if _, err := c.Refresh(ctx); err != nil {
			return
		}
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 单次失败不退出循环：一次 fork 失败或一次超时不该让
				// “每十分钟刷新一次”永久失效。
				_, _ = c.Refresh(ctx)
			}
		}
	}()
}
