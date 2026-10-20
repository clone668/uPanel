package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
)

// QueryName 是存储拓扑查询在代理登记表里的名字（CONTRACTS.md 第 15 节）。
const QueryName = "storage.topology"

// ErrAgentUnavailable 表示读不到存储拓扑是因为代理不可用（连不上、超时）。
// 它与“代理明确拒绝”分开：前者是运行期状态，调用方按 AGENT_UNAVAILABLE 处理。
var ErrAgentUnavailable = errors.New("storage: 代理不可用")

// QueryAgent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// Fetcher 是 Web 侧读取 storage.topology 的客户端。
//
// 它不做第二层缓存：扫描与“数据有多旧”由代理负责，Web 再缓存一次只会让页面
// 显示的时间比真实采集时间更旧。请求直接透传到代理最近一次扫描的快照。
type Fetcher struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要在运行时用一个永远返回空拓扑的假实现顶上。
func NewFetcher(agent QueryAgent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("storage: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Topology 读取代理最近一次扫描的块设备拓扑。
//
// 代理自己扫描失败时返回的是一份 status=error、nodes 保留上一次成功内容的快照，
// 那**不是**错误：页面要照常显示设备，并标出数据时间与失败原因。
// 只有“代理不可用”和“代理明确拒绝”才是错误。
func (f *Fetcher) Topology(ctx context.Context) (Topology, error) {
	raw, err := f.agent.Query(ctx, QueryName, nil)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取存储拓扑失败：代理不可用", "error", err)
			}
			return Topology{}, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		return Topology{}, fmt.Errorf("storage: 代理拒绝了 %s 查询: %w", QueryName, err)
	}

	var topo Topology
	if err := json.Unmarshal(raw, &topo); err != nil {
		return Topology{}, fmt.Errorf("storage: 代理返回的拓扑不是合法 JSON: %w", err)
	}
	// collected_at 是这份数据唯一的时间锚点；没有它，界面只能猜数据有多旧。
	// 扫描一次都没成功过时（代理刚启动且 sysfs 读不到）允许为空，
	// 但那种情况必须带 status=error 与原因，不能是一份“看起来正常”的空拓扑。
	if topo.CollectedAt.IsZero() {
		if topo.Status != StatusError {
			return Topology{}, errors.New("storage: 代理返回的拓扑缺少 collected_at，且没有标明失败原因")
		}
		if topo.Error == "" {
			return Topology{}, errors.New("storage: 代理返回的拓扑标记为失败，却没有给出原因")
		}
	}
	if topo.IntervalMS <= 0 {
		return Topology{}, errors.New("storage: 代理返回的拓扑缺少有效的 interval_ms")
	}
	if topo.Status != StatusError && topo.SnapshotVersion == "" {
		return Topology{}, errors.New("storage: 代理返回的拓扑缺少 snapshot_version")
	}
	if topo.Nodes == nil {
		topo.Nodes = []Device{}
	}
	if topo.Edges == nil {
		topo.Edges = []Edge{}
	}
	return topo, nil
}

// Device 从最近一次拓扑里取一台设备。找不到时第二个返回值为 false
// （调用方按 404 处理，而不是 500）。
func (f *Fetcher) Device(ctx context.Context, id string) (Device, Topology, bool, error) {
	topo, err := f.Topology(ctx)
	if err != nil {
		return Device{}, Topology{}, false, err
	}
	for _, d := range topo.Nodes {
		if d.ID == id {
			return d, topo, true, nil
		}
	}
	return Device{}, topo, false, nil
}
