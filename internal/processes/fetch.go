package processes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
)

// ErrAgentUnavailable 表示读不到进程列表是因为代理不可用（连不上、超时）。
var ErrAgentUnavailable = errors.New("processes: 代理不可用")

// QueryAgent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// Fetcher 是 Web 侧读取进程列表的客户端。
//
// 只读代理最近一次快照，Web 不做第二层缓存：快照的时间戳就是页面上那句
// “采集于……”的来源，多一层缓存只会让那句话变成谎话。
// 进程的写操作不经过这里，走统一的预检 → 任务链路（action=process.signal）。
type Fetcher struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要在运行时用一个永远返回空列表的假实现顶上。
func NewFetcher(agent QueryAgent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("processes: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Snapshot 读取代理最近一次进程快照。
func (f *Fetcher) Snapshot(ctx context.Context) (Snapshot, error) {
	raw, err := f.agent.Query(ctx, QueryListName, nil)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取进程列表失败：代理不可用", "error", err)
			}
			return Snapshot{}, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return Snapshot{}, fmt.Errorf("processes: 代理拒绝了 %s 查询: %w", QueryListName, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("processes: 代理返回的进程列表不是合法 JSON: %w", err)
	}
	// 与 services 同一套规则：没有 collected_at 时必须是“明确失败 + 原因”，
	// 不能是一份看起来正常的空列表。
	if snap.CollectedAt.IsZero() {
		if snap.Status != StatusError {
			return Snapshot{}, errors.New("processes: 代理返回的进程列表缺少 collected_at，且没有标明失败原因")
		}
		if snap.Error == "" {
			return Snapshot{}, errors.New("processes: 代理返回的进程列表标记为失败，却没有给出原因")
		}
	}
	if snap.IntervalMS <= 0 {
		return Snapshot{}, errors.New("processes: 代理返回的进程列表缺少有效的 interval_ms")
	}
	if snap.Processes == nil {
		snap.Processes = []Process{}
	}
	if snap.Total < len(snap.Processes) {
		// 总数比列表还小只可能是代理算错了：不修正成列表长度（那会把一个 bug 藏起来）。
		return Snapshot{}, fmt.Errorf("processes: 代理返回的 total=%d 小于列表长度 %d", snap.Total, len(snap.Processes))
	}
	return snap, nil
}
