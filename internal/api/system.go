package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/monitoring"
)

// Monitor 是系统基础资源的读取端（P04.1）。生产实现是 *monitoring.Fetcher。
//
// 接口定义在消费方（api）而不是实现方，测试可以直接给一个假实现；
// 但生产里没有“固定成功”的替身：没有配置代理时 Monitor 为 nil，接口回 503。
type Monitor interface {
	Summary(ctx context.Context) (monitoring.Summary, error)
}

// handleSystemSummary 返回代理最近一次采集的 CPU/内存/负载/网络/主机信息。
//
// 权限是 V（CONTRACTS.md 第 3 节）：任何已登录用户都能看系统概况。
// 这里不做第二层缓存，也不把“没有数据”补成 0——响应里的 collected_at、
// 每段的 status 与 null 值本身就在回答“这份数据有多旧、哪一段没读到”。
func (s *Server) handleSystemSummary(c *gin.Context) {
	if s.opts.Monitor == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"系统信息不可用：未配置代理连接")
		return
	}
	sum, err := s.opts.Monitor.Summary(c.Request.Context())
	if err != nil {
		if errors.Is(err, monitoring.ErrAgentUnavailable) {
			// 代理不可用是运行期状态（代理重启、socket 暂时不在），不是 500。
			s.opts.Logger.Warn("读取系统信息失败：代理不可用",
				"error", err, "request_id", httpx.RequestID(c.Request))
			writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
				"代理不可用，暂时读不到系统信息")
			return
		}
		// 代理明确拒绝了查询，或返回的正文不合法：这是需要人看日志的故障。
		s.opts.Logger.Error("读取系统信息失败",
			"error", err, "request_id", httpx.RequestID(c.Request))
		writeError(c, http.StatusInternalServerError, httpx.CodeCommandFailed,
			"系统信息读取失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, sum)
}
