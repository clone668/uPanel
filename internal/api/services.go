package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/services"
)

// ServicesReader 是服务只读视图的读取端（P05.1）。生产实现是 *services.Fetcher。
//
// 与 Monitor/Storage 一样，接口定义在消费方，测试可以给假实现；生产里没有
// “固定成功”的替身：没配置代理时 Services 为 nil，接口回 503 AGENT_UNAVAILABLE，
// 而不是给一份空列表——空列表会让人以为这台机器上没有任何服务。
type ServicesReader interface {
	Snapshot(ctx context.Context) (services.Snapshot, error)
	Unit(ctx context.Context, name string) (services.Unit, bool, error)
}

// serviceListResponse 是 GET /api/v1/services 的响应体。
//
// 与 /storage/devices 同构：status/error/collected_at/interval_ms 都在外层，
// 页面据此说明“这份列表有多旧、上一次采集成不成功”。
type serviceListResponse struct {
	Status          string          `json:"status"`
	Error           string          `json:"error,omitempty"`
	SnapshotVersion string          `json:"snapshot_version"`
	CollectedAt     time.Time       `json:"collected_at"`
	IntervalMS      int64           `json:"interval_ms"`
	Truncated       bool            `json:"truncated"`
	Units           []services.Unit `json:"units"`
}

// serviceDetailResponse 是 GET /api/v1/services/:unit 的响应体。
type serviceDetailResponse struct {
	Unit services.Unit `json:"unit"`
}

// handleListServices 返回服务单元列表（权限 V：任何已登录用户都能看状态）。
func (s *Server) handleListServices(c *gin.Context) {
	reader := s.opts.Services
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"服务列表不可用：未配置代理连接")
		return
	}
	snap, err := reader.Snapshot(c.Request.Context())
	if err != nil {
		s.writeServicesError(c, err, "读取服务列表失败")
		return
	}
	resp := serviceListResponse{
		Status:          snap.Status,
		Error:           snap.Error,
		SnapshotVersion: snap.SnapshotVersion,
		CollectedAt:     snap.CollectedAt,
		IntervalMS:      snap.IntervalMS,
		Truncated:       snap.Truncated,
		Units:           snap.Units,
	}
	if resp.Units == nil {
		resp.Units = []services.Unit{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleGetService 返回单个服务单元的现场状态。
//
// 现场读取而不是从列表里挑：列表最多 10 秒旧，而“现在能不能 stop”必须看现在的状态。
// 单元不存在返回 404；单元名不合法返回 400（非法名字不进代理，也不进 systemctl）。
func (s *Server) handleGetService(c *gin.Context) {
	reader := s.opts.Services
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"服务详情不可用：未配置代理连接")
		return
	}
	name := c.Param("unit")
	if name == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "服务单元名不能为空")
		return
	}
	if err := services.ValidateUnitName(name); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "服务单元名不合法："+name)
		return
	}
	if services.IsTemplate(name) {
		// 模板单元没有实例：systemctl 不接受这种名字，列表里也不会出现它。
		// 404 而不是 400——名字本身合法，只是这台机器上没有对应的可操作单元。
		writeError(c, http.StatusNotFound, httpx.CodeNotFound,
			"模板单元 "+name+" 需要指定实例（例如 "+strings.Replace(name, "@.service", "@<实例名>.service", 1)+"），面板不列出模板单元")
		return
	}
	unit, found, err := reader.Unit(c.Request.Context(), name)
	if err != nil {
		s.writeServicesError(c, err, "读取服务详情失败")
		return
	}
	if !found {
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "服务单元不存在："+name)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, serviceDetailResponse{Unit: unit})
}

// writeServicesError 把读取错误翻译成 HTTP 状态。
func (s *Server) writeServicesError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, services.ErrAgentUnavailable) {
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法读取服务状态")
		return
	}
	if errors.Is(err, services.ErrSystemctlUnavailable) {
		// systemctl 不存在是“依赖缺失”，不是内部错误：界面要能说清楚装什么。
		writeError(c, http.StatusServiceUnavailable, httpx.CodeDependencyMissing,
			"本机没有可用的 systemctl，无法读取服务状态")
		return
	}
	if errors.Is(err, services.ErrInvalidUnitName) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "服务单元名不合法")
		return
	}
	s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
}
