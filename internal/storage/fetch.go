package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
)

// QueryName 是存储拓扑查询在代理登记表里的名字（CONTRACTS.md 第 15 节）。
const QueryName = "storage.topology"

// VolumesQueryName / VolumeBlockersQueryName 是 P07.4 的两条卷查询。
//
// 卷的 observed_state 与 capacity 是现场算出来的（读超级块、读挂载表），因此必须由
// 代理回答——Web 侧没有 /dev 也没有超级块读取器，任何“Web 自己算一份状态”的写法
// 都会变成第二个事实来源。
const (
	VolumesQueryName        = "storage.volumes"
	VolumeBlockersQueryName = "storage.volume.blockers"
)

// RemoteError 是代理明确拒绝（4xx）时带到 Web 层的错误。
//
// 与 ErrAgentUnavailable 分开：那是“代理此刻联系不上”，这是“代理听见了并且说不”。
// 两者的 HTTP 判决不同（503 与 404/400），混成一个会让页面去提示用户重试一件
// 重试也不会成功的事。
type RemoteError struct {
	Status  int
	Code    httpx.Code
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("storage: 代理拒绝（%d %s）：%s", e.Status, e.Code, e.Message)
}

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

// Volumes 读取代理侧的受管理卷列表（含现场观察到的状态）。
//
// 代理没有配置登记表时返回 agentclient 的“查询不存在”类错误，调用方按 503 处理：
// 一台没有卷管理能力的机器不该在页面上显示一个空列表假装“没有卷”。
func (f *Fetcher) Volumes(ctx context.Context) ([]Volume, error) {
	raw, err := f.query(ctx, VolumesQueryName, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Volumes []Volume `json:"volumes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("storage: 代理返回的卷列表不是合法 JSON: %w", err)
	}
	if payload.Volumes == nil {
		payload.Volumes = []Volume{}
	}
	return payload.Volumes, nil
}

// VolumeBlockers 读取一个卷当前的占用情况（为什么现在卸载不了）。
func (f *Fetcher) VolumeBlockers(ctx context.Context, volumeID string) (VolumeBlockers, error) {
	raw, err := f.query(ctx, VolumeBlockersQueryName, map[string]string{"volume_id": volumeID})
	if err != nil {
		return VolumeBlockers{}, err
	}
	var out VolumeBlockers
	if err := json.Unmarshal(raw, &out); err != nil {
		return VolumeBlockers{}, fmt.Errorf("storage: 代理返回的占用信息不是合法 JSON: %w", err)
	}
	// 空数组而不是 null：页面据此判断“没有占用”，null 会被当成“未知”。
	if out.Mounts == nil {
		out.Mounts = []string{}
	}
	if out.Processes == nil {
		out.Processes = []BusyProcess{}
	}
	if out.Holders == nil {
		out.Holders = []string{}
	}
	if out.Apps == nil {
		out.Apps = []VolumeDependency{}
	}
	if out.Shares == nil {
		out.Shares = []VolumeDependency{}
	}
	if out.Backups == nil {
		out.Backups = []VolumeDependency{}
	}
	return out, nil
}

// Volume 读取单个卷的视图。
//
// 走的是列表查询再挑一条：CONTRACTS.md 第 15 节只登记了 storage.volumes 与
// storage.volume.blockers 两条卷查询，为详情页再加一条“按 id 查”会让代理多一个
// 只差一个过滤条件的入口。代价是详情页也会触发一次全量观察（每个卷读一次超级块），
// 卷数上去之后要连“按 id 查”一起重新设计，而不是在这里偷偷加缓存。
func (f *Fetcher) Volume(ctx context.Context, id string) (Volume, bool, error) {
	volumes, err := f.Volumes(ctx)
	if err != nil {
		return Volume{}, false, err
	}
	for _, v := range volumes {
		if v.ID == id {
			return v, true, nil
		}
	}
	return Volume{}, false, nil
}

// query 是三条读取共用的代理调用与错误归一。
func (f *Fetcher) query(ctx context.Context, name string, params any) (json.RawMessage, error) {
	raw, err := f.agent.Query(ctx, name, params)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取存储数据失败：代理不可用", "query", name, "error", err)
			}
			return nil, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		// 代理的明确拒绝：带着它的状态码与码往上走，接口层据此回 404/400，
		// 而不是一律压成 503“代理不可用”。
		var rejected *agentclient.Error
		if errors.As(err, &rejected) {
			return nil, &RemoteError{
				Status:  rejected.Status,
				Code:    httpx.Code(rejected.Code),
				Message: rejected.Message,
			}
		}
		return nil, fmt.Errorf("storage: 代理拒绝了 %s 查询: %w", name, err)
	}
	return raw, nil
}
