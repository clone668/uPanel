package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/storage"
)

// StorageReader 是存储只读视图的读取端（P04.2）。生产实现是 *storage.Fetcher。
//
// 与 Monitor 一样，接口定义在消费方，测试可以给假实现；但生产里没有“固定成功”的替身：
// 没配置代理时 Storage 为 nil，接口回 503 AGENT_UNAVAILABLE，而不是给一份空拓扑。
type StorageReader interface {
	Topology(ctx context.Context) (storage.Topology, error)
	Device(ctx context.Context, id string) (storage.Device, storage.Topology, bool, error)
}

// deviceListResponse 是 GET /storage/devices 的响应体。
//
// 三个元数据字段与 /storage/topology 保持一致（status/collected_at/interval_ms），
// 页面才能判断“这份设备列表有多旧、上一次扫描成不成功”。
type deviceListResponse struct {
	Status          string           `json:"status"`
	Error           string           `json:"error,omitempty"`
	SnapshotVersion string           `json:"snapshot_version"`
	CollectedAt     time.Time        `json:"collected_at"`
	IntervalMS      int64            `json:"interval_ms"`
	Devices         []storage.Device `json:"devices"`
	Edges           []storage.Edge   `json:"edges"`
}

// deviceDetailResponse 是 GET /storage/devices/:id 的响应体。
//
// device 里已经带了 health、capabilities 与 protection（阻止理由），
// 这里额外给出**与这台设备直接相关**的关系边，详情页不必自己走整张图。
type deviceDetailResponse struct {
	Status          string         `json:"status"`
	Error           string         `json:"error,omitempty"`
	SnapshotVersion string         `json:"snapshot_version"`
	CollectedAt     time.Time      `json:"collected_at"`
	IntervalMS      int64          `json:"interval_ms"`
	Device          storage.Device `json:"device"`
	Edges           []storage.Edge `json:"edges"`
	// Missing 在设备已经不存在时列出它的 id，便于界面把“刚被移除的盘”说清楚。
	Missing []string `json:"missing,omitempty"`
}

// handleStorageTopology 返回块设备拓扑（nodes + edges + snapshot_version）。
//
// 权限是 V（CONTRACTS.md 第 6 节）：任何已登录用户都能看设备树。
func (s *Server) handleStorageTopology(c *gin.Context) {
	reader := s.opts.Storage
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"存储拓扑不可用：未配置代理连接")
		return
	}
	topo, err := reader.Topology(c.Request.Context())
	if err != nil {
		s.writeStorageError(c, err, "读取存储拓扑失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, topo)
}

// handleListDevices 返回设备列表。
//
// 它和 /storage/topology 读的是同一份快照：列表里每一台设备的 protection、
// holders 与 collected_at 都与拓扑里的完全一致，不会出现“列表与图对不上”。
func (s *Server) handleListDevices(c *gin.Context) {
	reader := s.opts.Storage
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"设备列表不可用：未配置代理连接")
		return
	}
	topo, err := reader.Topology(c.Request.Context())
	if err != nil {
		s.writeStorageError(c, err, "读取设备列表失败")
		return
	}
	resp := deviceListResponse{
		Status:          topo.Status,
		Error:           topo.Error,
		SnapshotVersion: topo.SnapshotVersion,
		CollectedAt:     topo.CollectedAt,
		IntervalMS:      topo.IntervalMS,
		Devices:         topo.Nodes,
		Edges:           topo.Edges,
	}
	if resp.Devices == nil {
		resp.Devices = []storage.Device{}
	}
	if resp.Edges == nil {
		resp.Edges = []storage.Edge{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleGetDevice 返回单个设备的详情。
//
// 设备不存在时是 404（不是 500）：设备名在一次启动内唯一，拔盘或改名之后
// 旧链接就该明确失效，而不是让界面显示一个空对象。
func (s *Server) handleGetDevice(c *gin.Context) {
	reader := s.opts.Storage
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"设备详情不可用：未配置代理连接")
		return
	}
	id := c.Param("id")
	if id == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "设备 id 不能为空")
		return
	}
	dev, topo, found, err := reader.Device(c.Request.Context(), id)
	if err != nil {
		s.writeStorageError(c, err, "读取设备详情失败")
		return
	}
	if !found {
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "设备不存在："+id)
		return
	}

	// 只带与这台设备直接相关的边（它作为子设备、或作为底层提供者）。
	edges := make([]storage.Edge, 0, 4)
	for _, e := range topo.Edges {
		if e.FromDeviceID == id || e.ToDeviceID == id {
			edges = append(edges, e)
		}
	}
	resp := deviceDetailResponse{
		Status:          topo.Status,
		Error:           topo.Error,
		SnapshotVersion: topo.SnapshotVersion,
		CollectedAt:     topo.CollectedAt,
		IntervalMS:      topo.IntervalMS,
		Device:          dev,
		Edges:           edges,
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// writeStorageError 把读取错误翻译成 HTTP 状态。
//
// 三种情况分开：代理不可用（503，运行期状态）、代理拒绝或返回不合法（500，要人看日志）、
// 上下文取消（客户端断开，不再写响应）。
func (s *Server) writeStorageError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, storage.ErrAgentUnavailable) {
		s.opts.Logger.Warn(what+"：代理不可用",
			"error", err, "request_id", httpx.RequestID(c.Request))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，暂时读不到存储信息")
		return
	}
	s.opts.Logger.Error(what, "error", err, "request_id", httpx.RequestID(c.Request))
	writeError(c, http.StatusInternalServerError, httpx.CodeCommandFailed, "存储信息读取失败")
}
