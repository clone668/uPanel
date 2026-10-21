package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/processes"
)

// ProcessesReader 是进程只读视图的读取端（P05.2）。生产实现是 *processes.Fetcher。
//
// 与 Monitor/Storage/Services 一致：接口定义在消费方，测试可以给假实现；生产里没有
// “固定成功”的替身——没配置代理时 Processes 为 nil，接口回 503 AGENT_UNAVAILABLE，
// 而不是给一份空列表。空列表会让人以为这台机器上没有进程。
type ProcessesReader interface {
	Snapshot(ctx context.Context) (processes.Snapshot, error)
}

// processListResponse 是 GET /api/v1/processes 的响应体。
//
// 与 /services 同构：status/error/collected_at/interval_ms 在外层，页面据此说明
// “这份列表有多旧、上一次采集成不成功”。没有 snapshot_version——见 processes.Snapshot
// 的说明：进程快照每秒都在变，用版本去绑预检只会让每次提交都无谓失效。
type processListResponse struct {
	Status      string              `json:"status"`
	Error       string              `json:"error,omitempty"`
	CollectedAt time.Time           `json:"collected_at"`
	IntervalMS  int64               `json:"interval_ms"`
	Total       int                 `json:"total"`
	Truncated   bool                `json:"truncated"`
	Processes   []processes.Process `json:"processes"`
}

// handleListProcesses 返回进程列表。权限 admin（CONTRACTS.md 第 10 节）：
// 命令摘要里可能有路径、参数与服务名，属于主机内部信息，不对 viewer 开放。
func (s *Server) handleListProcesses(c *gin.Context) {
	reader := s.opts.Processes
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"进程列表不可用：未配置代理连接")
		return
	}
	snap, err := reader.Snapshot(c.Request.Context())
	if err != nil {
		s.writeProcessesError(c, err, "读取进程列表失败")
		return
	}
	resp := processListResponse{
		Status:      snap.Status,
		Error:       snap.Error,
		CollectedAt: snap.CollectedAt,
		IntervalMS:  snap.IntervalMS,
		Total:       snap.Total,
		Truncated:   snap.Truncated,
		Processes:   snap.Processes,
	}
	if resp.Processes == nil {
		resp.Processes = []processes.Process{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// writeProcessesError 把读取错误翻译成 HTTP 状态。
func (s *Server) writeProcessesError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, processes.ErrAgentUnavailable) {
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法读取进程状态")
		return
	}
	s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
}
