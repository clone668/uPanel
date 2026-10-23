package proxies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"upanel/internal/agentclient"
	"upanel/internal/nginx"
)

// NginxStateQueryName 是代理侧 Nginx 现状查询的名字（与 internal/agent 逐字一致）。
const NginxStateQueryName = "nginx.state"

// QueryAgent 是 AgentFacts 需要的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// nginxStateParams 是 nginx.state 的参数。字段名与代理侧一致。
type nginxStateParams struct {
	Fresh bool `json:"fresh"`
}

// AgentFacts 是 FactsSource 的生产实现：每次调用都向代理要 nginx.state。
//
// 为什么 Web 侧不自己缓存事实：缓存已经在代理里（默认 5 秒），Web 再存一层就会
// 出现“页面上写着 10:00:03 采的，实际是 09:59:58 采的”这种说不清的时间差。
//
// Invalidate 的语义见方法注释：它不向主机发任何请求，只把“下一次展示读取必须绕过
// 代理缓存”记成一个待办。写操作之后那一次读取必须看见刚写下去的那份配置，
// 而之后的读取又可以回到缓存——这正好是写操作对缓存提出的全部要求。
type AgentFacts struct {
	agent  QueryAgent
	logger *slog.Logger

	mu sync.Mutex
	// mustBeFresh 为 true 表示下一次 Snapshot 不吃代理缓存。
	mustBeFresh bool
}

// NewAgentFacts 构造 AgentFacts。agent 为 nil 时报错：不拿一个“永远没有冲突”的
// 假事实源顶上——那正是最危险的那句谎话。
func NewAgentFacts(agent QueryAgent, logger *slog.Logger) (*AgentFacts, error) {
	if agent == nil {
		return nil, errors.New("proxies: AgentFacts 需要代理客户端")
	}
	return &AgentFacts{agent: agent, logger: logger}, nil
}

// Snapshot 读取展示用的 Nginx 现状（代理侧有缓存）。
//
// 写操作之后的第一调用会用 fresh=true 绕过缓存，随后自动恢复走缓存。
func (f *AgentFacts) Snapshot(ctx context.Context) (nginx.Facts, error) {
	f.mu.Lock()
	fresh := f.mustBeFresh
	f.mustBeFresh = false
	f.mu.Unlock()
	return f.fetch(ctx, fresh)
}

// Collect 读取判断用的 Nginx 现状：**永远**不吃缓存。
//
// 写操作前的冲突判断必须看见用户刚刚手工加进去的 server_name。缓存窗口里的
// “没有冲突”是这条链路上唯一一句会直接导致覆盖别人配置的话。
func (f *AgentFacts) Collect(ctx context.Context) (nginx.Facts, error) {
	return f.fetch(ctx, true)
}

// Invalidate 让下一次展示读取绕过代理缓存。
//
// 它不发请求：写操作刚刚落盘，紧接着的展示读取才是需要看见新内容的那一次，
// 而在这里先跑一遍完整采集（最多读 16 MiB 配置）只是把同一件事做两遍。
func (f *AgentFacts) Invalidate() {
	f.mu.Lock()
	f.mustBeFresh = true
	f.mu.Unlock()
}

// fetch 向代理要一次 nginx.state，并把错误说成人话。
func (f *AgentFacts) fetch(ctx context.Context, fresh bool) (nginx.Facts, error) {
	raw, err := f.agent.Query(ctx, NginxStateQueryName, nginxStateParams{Fresh: fresh})
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取 Nginx 现状失败：代理不可用", "error", err)
			}
			return nginx.Facts{}, fmt.Errorf("%w: %v", ErrFactsUnavailable, err)
		}
		return nginx.Facts{}, fmt.Errorf("proxies: 代理拒绝了 %s 查询: %w", NginxStateQueryName, err)
	}
	var facts nginx.Facts
	if err := json.Unmarshal(raw, &facts); err != nil {
		return nginx.Facts{}, fmt.Errorf("proxies: 解析 Nginx 现状失败: %w", err)
	}
	return facts, nil
}
