package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
)

// ErrAgentUnavailable 表示读不到服务状态是因为代理不可用（连不上、超时）。
var ErrAgentUnavailable = errors.New("services: 代理不可用")

// QueryAgent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// UnitResponse 是 services.unit 查询的返回体。
//
// found=false 时 unit 为 null：那表示“这台机器上没有这个单元”，
// 与“代理读失败”（error）和“单元存在但没有状态”（unit 里有对象）都不同。
type UnitResponse struct {
	Found bool  `json:"found"`
	Unit  *Unit `json:"unit"`
}

// Fetcher 是 Web 侧读取服务状态的客户端。
//
// 列表读代理最近一次快照；详情与操作前的检查现场读取（services.unit），
// 避免“页面显示 10 秒前的状态”。Web 自己不做第二层缓存。
type Fetcher struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要在运行时用一个永远返回空列表的假实现顶上。
func NewFetcher(agent QueryAgent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("services: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Snapshot 读取代理最近一次服务快照。
func (f *Fetcher) Snapshot(ctx context.Context) (Snapshot, error) {
	raw, err := f.agent.Query(ctx, QueryListName, nil)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取服务列表失败：代理不可用", "error", err)
			}
			return Snapshot{}, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return Snapshot{}, fmt.Errorf("services: 代理拒绝了 %s 查询: %w", QueryListName, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("services: 代理返回的服务列表不是合法 JSON: %w", err)
	}
	// 与 storage 同一套规则：没有 collected_at 时必须是“明确失败 + 原因”，
	// 不能是一份看起来正常的空列表。
	if snap.CollectedAt.IsZero() {
		if snap.Status != StatusError {
			return Snapshot{}, errors.New("services: 代理返回的服务列表缺少 collected_at，且没有标明失败原因")
		}
		if snap.Error == "" {
			return Snapshot{}, errors.New("services: 代理返回的服务列表标记为失败，却没有给出原因")
		}
	}
	if snap.IntervalMS <= 0 {
		return Snapshot{}, errors.New("services: 代理返回的服务列表缺少有效的 interval_ms")
	}
	if snap.Status != StatusError && snap.SnapshotVersion == "" {
		return Snapshot{}, errors.New("services: 代理返回的服务列表缺少 snapshot_version")
	}
	if snap.Units == nil {
		snap.Units = []Unit{}
	}
	return snap, nil
}

// Unit 现场读取单个服务单元。found=false 时调用方按 404 处理。
func (f *Fetcher) Unit(ctx context.Context, name string) (Unit, bool, error) {
	if err := ValidateUnitName(name); err != nil {
		return Unit{}, false, err
	}
	raw, err := f.agent.Query(ctx, QueryUnitName, map[string]string{"unit": name})
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			return Unit{}, false, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return Unit{}, false, fmt.Errorf("services: 代理拒绝了 %s 查询: %w", QueryUnitName, err)
	}
	var resp UnitResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Unit{}, false, fmt.Errorf("services: 代理返回的单元状态不是合法 JSON: %w", err)
	}
	if !resp.Found {
		return Unit{}, false, nil
	}
	if resp.Unit == nil {
		return Unit{}, false, errors.New("services: 代理说单元存在，却没有返回状态")
	}
	if resp.Unit.Unit != name {
		return Unit{}, false, fmt.Errorf("services: 代理返回的单元名 %q 与请求的 %q 不一致", resp.Unit.Unit, name)
	}
	return *resp.Unit, true, nil
}
