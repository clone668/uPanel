package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
)

// 查询名（与 internal/agent 的 discoveryquery.go 必须逐字一致）。
const (
	// QueryName 返回发现快照。
	QueryName = "apps.discovery"
	// ProbeQueryName 对目标做一次 HTTP 探测。
	ProbeQueryName = "apps.probe"
)

// ErrAgentUnavailable 表示读不到发现结果/探测结果是因为代理不可用。
//
// 它与“本机什么都没有”必须分开：前者等一会儿重试就好，后者会让人去查一台
// 本来就没问题的服务器。接口层把前者映射成 503 AGENT_UNAVAILABLE。
var ErrAgentUnavailable = errors.New("discovery: 代理不可用")

// QueryAgent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// Fetcher 是 Web 侧读取发现结果与探测结果的客户端。
//
// Web 不做第二层缓存：发现快照的缓存由代理按 TTL 管，探测结果落在 Web 库里
// （app_health_results）。两边各缓存一次会出现“页面上的时间比数据库里的新”。
type Fetcher struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时报错：不拿一个永远返回空列表的假实现顶上。
func NewFetcher(agent QueryAgent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("discovery: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Snapshot 读取代理最近一次的发现快照。
func (f *Fetcher) Snapshot(ctx context.Context) (Snapshot, error) {
	raw, err := f.agent.Query(ctx, QueryName, nil)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取应用发现失败：代理不可用", "error", err)
			}
			return Snapshot{}, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return Snapshot{}, fmt.Errorf("discovery: 代理拒绝了 %s 查询: %w", QueryName, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("discovery: 解析发现结果失败: %w", err)
	}
	// Targets 是代理内部的解析表（json:"-"），Web 侧要用它判断“目标还存在吗”，
	// 因此从候选自己重建一份；不要指望代理把它序列化过来。
	snap.Targets = TargetsOf(snap.Candidates)
	return snap, nil
}

// Probe 让代理对一个目标做一次 HTTP 探测。
func (f *Fetcher) Probe(ctx context.Context, params ProbeParams) (ProbeResult, error) {
	raw, err := f.agent.Query(ctx, ProbeQueryName, params)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			return ProbeResult{}, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return ProbeResult{}, fmt.Errorf("discovery: 代理拒绝了 %s 查询: %w", ProbeQueryName, err)
	}
	var result ProbeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ProbeResult{}, fmt.Errorf("discovery: 解析探测结果失败: %w", err)
	}
	return result, nil
}

// TargetExists 判断一个 target_id 此刻是否在发现结果里。
//
// 代理不可用时返回 error（接口层回 503），而不是 false：false 的意思是
// “代理看过了，没有这个东西”，那是 422 的依据。
func (f *Fetcher) TargetExists(ctx context.Context, targetID string) (bool, error) {
	snap, err := f.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	_, ok := snap.Targets[targetID]
	return ok, nil
}
