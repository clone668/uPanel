package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/apt"
)

// 代理侧的查询名（与 internal/agent 逐字一致）。
const (
	AptPackagesQueryName = "apt.packages"
	PowerStateQueryName  = "system.power_state"
)

// AgentQueryTimeout 是这两条查询自己的调用上限，比 agentclient.DefaultTimeout 宽。
//
// 为什么必须宽——量出来的，不是猜的：`apt-cache policy` 在 apt 2.8（24.04）上把每个
// 参数都对全部包名做一次模式匹配（这台验收机 87804 个包名），731 个已安装包名冷采
// 一次 26.6 秒；同一台机器 22.04（apt 2.4，676 个包名）0.3 秒。10 秒的通用上限会把
// 一次**正常**的冷采切成"代理不可用"——这是假话，比慢更糟。
//
// 稳态下不会走到这个上限：代理侧按 (索引 mtime, 已安装版本) 缓存候选版本，
// 索引不变时重采只要 0.3 秒（见 internal/apt 的 candidateCache）。只有"代理刚启动"
// 与"apt 索引刚刷新"这两种情况才需要重新算一遍全部候选。
const AgentQueryTimeout = 45 * time.Second

// QueryAgent 是 AgentFacts 需要的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// aptPackagesParams 是 apt.packages 的参数。字段名与代理侧一致。
type aptPackagesParams struct {
	Fresh bool `json:"fresh"`
}

// AgentFacts 是 Source 的生产实现：每次调用都向代理要事实。
//
// 缓存只有一份，在代理里（30 秒快照）。Web 侧不再存一层：两层缓存会让
// "页面上写着 10:00:03 采的，实际是 09:59:58 采的"这种时间差再也说不清。
type AgentFacts struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewAgentFacts 构造 AgentFacts。agent 为 nil 时报错——不拿一个"什么都没有"
// 的假事实源顶上，那会被读成"这台机器上一个包都没装"。
func NewAgentFacts(agent QueryAgent, logger *slog.Logger) (*AgentFacts, error) {
	if agent == nil {
		return nil, errors.New("packages: AgentFacts 需要代理客户端")
	}
	return &AgentFacts{agent: agent, logger: logger}, nil
}

// Packages 向代理要一次包事实。
func (f *AgentFacts) Packages(ctx context.Context, fresh bool) (apt.Snapshot, error) {
	raw, err := f.agent.Query(ctx, AptPackagesQueryName, aptPackagesParams{Fresh: fresh})
	if err != nil {
		return apt.Snapshot{}, wrapAgentError(f.logger, AptPackagesQueryName, err)
	}
	var snap apt.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return apt.Snapshot{}, fmt.Errorf("packages: 解析 %s 响应失败: %w", AptPackagesQueryName, err)
	}
	return snap, nil
}

// PowerState 向代理要一次重启标志。
func (f *AgentFacts) PowerState(ctx context.Context) (apt.PowerState, error) {
	raw, err := f.agent.Query(ctx, PowerStateQueryName, struct{}{})
	if err != nil {
		return apt.PowerState{}, wrapAgentError(f.logger, PowerStateQueryName, err)
	}
	var st apt.PowerState
	if err := json.Unmarshal(raw, &st); err != nil {
		return apt.PowerState{}, fmt.Errorf("packages: 解析 %s 响应失败: %w", PowerStateQueryName, err)
	}
	return st, nil
}

// wrapAgentError 把"代理连不上"翻译成 ErrFactsUnavailable，其余原样带上查询名。
//
// 分开是因为两者的 HTTP 状态不同：连不上代理是 503（页面该说"代理不可用"），
// 代理明确拒绝（比如未登记这条查询）是代理的判决，也得让调用方看见原因。
func wrapAgentError(logger *slog.Logger, query string, err error) error {
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		if logger != nil {
			logger.Warn("读取软件包事实失败：代理不可用", "query", query, "error", err)
		}
		return fmt.Errorf("%w: %v", ErrFactsUnavailable, err)
	}
	return fmt.Errorf("packages: 代理拒绝了 %s 查询: %w", query, err)
}
