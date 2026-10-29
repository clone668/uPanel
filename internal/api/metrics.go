package api

// P13.1：历史监控查询与状态（CONTRACTS.md 第 3 节的 GET /system/metrics）。
//
// 两条纪律：
//
//  1. **没有历史不等于没有数据，也不等于服务器出故障**。演示模式下这个进程根本不记
//     历史，此时接口回 200 + enabled=false + 一句原因，而不是 503：界面应当显示
//     “未开启历史监控”，而不是让用户对着一个红色错误框以为坏了。生产模式下
//     reader 一定存在，真出问题时它会回 500 或 400，不会装作正常。
//
//  2. **缺口的形状要说清楚**。响应里每个点只有“它在”的时刻，没有值的地方就是没有点，
//     外加每个序列的 missing_points 计数。绝不补零——PLAN.md 第 18 节、ALERT-01
//     都要求丢失的数据不能当零。
//
// 权限是 V（任何已登录用户），与 GET /system/summary 一致：历史曲线里没有秘密，
// 它与首页那几块实时指标是同一类信息。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/metrics"
	"upanel/internal/platform"
)

// MetricsReader 是历史监控的读取端（P13.1）。生产实现是 *metrics.Reader。
//
// 允许为 nil：演示模式下没有采集器也没有历史查询，接口据此回 enabled=false，
// 而不是给一条看起来正常的空曲线。
type MetricsReader interface {
	Query(ctx context.Context, params metrics.QueryParams) (metrics.Result, error)
	Status(ctx context.Context) (metrics.Status, error)
}

// metricsDisabledReason 是“这个进程没有在记历史”时给人看的一句话。
//
// 演示模式与“装配时没接上”用同一句话不好：前者是设计，后者是错误。这里分开写，
// 但都通过 enabled=false 表达，不靠 HTTP 状态码猜。
func (s *Server) metricsDisabledReason() string {
	if s.opts.Mode == platform.ModeMock {
		return "演示模式不采集历史：这里的数字都是假的，记下来只会变成假的过去"
	}
	return "历史监控未启用"
}

// handleMetrics 处理 GET /api/v1/system/metrics。
func (s *Server) handleMetrics(c *gin.Context) {
	reader := s.opts.Metrics
	if reader == nil {
		httpx.WriteData(c.Writer, c.Request, http.StatusOK, metrics.Result{
			Enabled:     false,
			Reason:      s.metricsDisabledReason(),
			MaxPoints:   metrics.MaxPoints,
			Series:      []metrics.Series{},
			GeneratedAt: time.Now().UTC(),
		})
		return
	}

	params, ok := parseMetricParams(c)
	if !ok {
		return
	}
	res, err := reader.Query(c.Request.Context(), params)
	if err != nil {
		var invalid *metrics.InvalidQueryError
		if errors.As(err, &invalid) {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, invalid.Message)
			return
		}
		s.opts.Logger.Error("读取历史监控失败",
			"error", err, "scope", params.Scope, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "读取历史监控失败")
		return
	}
	if res.Series == nil {
		res.Series = []metrics.Series{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handleMetricsStatus 处理 GET /api/v1/system/metrics/status。
//
// 它回答“为什么图是空的”：采集循环在不在跑、最后一次成功是什么时候、最近一次错误
// 是什么、库里各粒度有多少行。查询接口自己不回答这个问题——一条空曲线什么也说明不了。
func (s *Server) handleMetricsStatus(c *gin.Context) {
	reader := s.opts.Metrics
	if reader == nil {
		st := metrics.Status{
			Enabled:     false,
			Reason:      s.metricsDisabledReason(),
			Samples:     map[string]int64{"total": 0},
			Metrics:     metrics.Definitions(),
			Series:      map[string][]metrics.MetricSeriesRef{},
			GeneratedAt: time.Now().UTC(),
		}
		httpx.WriteData(c.Writer, c.Request, http.StatusOK, st)
		return
	}
	st, err := reader.Status(c.Request.Context())
	if err != nil {
		s.opts.Logger.Error("读取历史监控状态失败",
			"error", err, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "读取历史监控状态失败")
		return
	}
	if st.Series == nil {
		st.Series = map[string][]metrics.MetricSeriesRef{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, st)
}

// parseMetricParams 把查询串解析成 metrics.QueryParams。
func parseMetricParams(c *gin.Context) (metrics.QueryParams, bool) {
	params := metrics.QueryParams{
		Scope:     strings.TrimSpace(c.Query("scope")),
		TargetIDs: splitQueryList(c.QueryArray("target_id")),
		Metrics:   splitQueryList(c.QueryArray("metric")),
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"from 必须是 RFC 3339 时间（例如 2026-10-25T08:00:00Z）")
			return params, false
		}
		params.From = ts
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"to 必须是 RFC 3339 时间（例如 2026-10-25T09:00:00Z）")
			return params, false
		}
		params.To = ts
	}
	if raw := strings.TrimSpace(c.Query("step")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || n > int64(metrics.MaxRange/time.Second) {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"step 必须是正整数秒，且不超过 "+strconv.Itoa(int(metrics.MaxRange/time.Second)))
			return params, false
		}
		params.StepSeconds = n
	}
	return params, true
}

// splitQueryList 把重复出现的查询参数与逗号分隔的列表合并成一个切片。
//
// 两种写法都接受（`?metric=a&metric=b` 与 `?metric=a,b`），因为审核日志/手写 URL
// 时两种都自然会出现；空项丢掉，不会让 `?metric=` 变成“一个名字为空的指标”。
func splitQueryList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
