package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
)

// QueryName 是系统基础资源查询在代理登记表里的名字（CONTRACTS.md 第 15 节）。
const QueryName = "system.summary"

// ErrAgentUnavailable 表示读不到系统信息是因为代理不可用（连不上、超时）。
// 它与“代理明确拒绝”分开：前者是运行期状态，调用方按 AGENT_UNAVAILABLE 处理。
var ErrAgentUnavailable = errors.New("monitoring: 代理不可用")

// QueryAgent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// Fetcher 是 Web 侧读取 system.summary 的客户端。
//
// 它不做第二层缓存：缓存与“多久之前的数”属于采集端（代理），Web 再缓存一次
// 只会让页面上显示的时间比真实采集时间更旧。请求直接透传到代理最近一次快照。
type Fetcher struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要在运行时用一个永远返回空摘要的假实现顶上。
func NewFetcher(agent QueryAgent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("monitoring: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Summary 读取代理最近一次采集的快照。
func (f *Fetcher) Summary(ctx context.Context) (Summary, error) {
	raw, err := f.agent.Query(ctx, QueryName, nil)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取系统信息失败：代理不可用", "error", err)
			}
			return Summary{}, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return Summary{}, fmt.Errorf("monitoring: 代理拒绝了 %s 查询: %w", QueryName, err)
	}
	var sum Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return Summary{}, fmt.Errorf("monitoring: 代理返回的系统信息不是合法 JSON: %w", err)
	}
	// collected_at 是这份数据唯一的时间锚点：缺了它，界面就只能猜数据有多旧。
	if sum.CollectedAt.IsZero() {
		return Summary{}, errors.New("monitoring: 代理返回的系统信息缺少 collected_at")
	}
	if sum.IntervalMS <= 0 {
		return Summary{}, errors.New("monitoring: 代理返回的系统信息缺少有效的 interval_ms")
	}
	return sum, nil
}
