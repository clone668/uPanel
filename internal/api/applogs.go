package api

// P09.4：应用关联对象的日志（CONTRACTS.md 第 8 节 GET /apps/:id/logs）。
//
// 这个接口不产生新的读取能力：systemd 应用走 journalctl（与 /logs 同一条读取链），
// docker 应用走 docker logs（与 /containers/:id/logs 同一条链）。它的价值是**判别**——
// 页面拿着一个应用标识就能看到它背后那个东西的日志，不需要知道它绑的是什么。
//
// 三条纪律：
//
//  1. **对象来自绑定，不来自请求。** 请求里的 unit/source 一律拒绝，否则这个接口
//     就变成了“按任意单元名读日志”的通用入口，绕过 /logs 自己的边界。
//  2. **admin。** 日志里什么都可能有（口令形态的字符串、内网地址）。
//  3. **限额沿用各自的读取端。** journal 的时间跨度/条数/字节上限由 journal 包执行，
//     docker 的行数与窗口由 containers 包执行；这里不复制一套上限。

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/apps"
	"upanel/internal/containers"
	"upanel/internal/httpx"
	"upanel/internal/journal"
)

// appLogsResponse 是 GET /api/v1/apps/:id/logs 的响应体。
//
// source 是判别式：journal 时带 unit，docker 时带 container_id。page 的字段按 source
// 分别是 /logs 与 /containers/:id/logs 的同一套（日志条目、游标、截断标记）。
type appLogsResponse struct {
	Source      string `json:"source"`
	Unit        string `json:"unit,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	// Page 是 journal.Page 或 containers.LogPage。
	Page any `json:"page"`
}

// handleGetAppLogs 处理 GET /api/v1/apps/:id/logs（admin）。
func (s *Server) handleGetAppLogs(c *gin.Context) {
	app, err := s.opts.Apps.Get(c.Request.Context(), sessionFrom(c).User.ID, c.Param("id"))
	if err != nil {
		s.writeAppError(c, "读取应用失败", err)
		return
	}
	switch app.Type {
	case apps.TypeSystemd:
		s.serveAppJournalLogs(c, app)
	case apps.TypeDocker:
		s.serveAppContainerLogs(c, app)
	case apps.TypeLink:
		writeError(c, http.StatusUnprocessableEntity, httpx.CodeValidationError,
			"这是入口（link）类型的应用：它没有关联的运行时，也就没有日志")
	case apps.TypeCompose:
		writeError(c, http.StatusNotImplemented, httpx.CodeNotImplemented,
			"compose 部署的日志要等部署能力落地（1.0）；此刻请到容器页面按容器查看")
	default:
		writeError(c, http.StatusUnprocessableEntity, httpx.CodeValidationError,
			"这个应用类型没有关联的日志来源")
	}
}

// serveAppJournalLogs 读 systemd 单元的日志。
func (s *Server) serveAppJournalLogs(c *gin.Context, app apps.App) {
	// 请求里出现 unit/source 时明确拒绝，而不是“悄悄用绑定覆盖”：
	// 一个被忽略的参数会让人以为自己在看另一个单元的日志。
	if strings.TrimSpace(c.Query("unit")) != "" || strings.TrimSpace(c.Query("source")) != "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"不接受 unit/source 参数：日志对象由应用的绑定决定（"+app.Binding.Unit+"）")
		return
	}
	// docker 的参数在这里没有意义，静默忽略会让人以为 since 生效了。
	if c.Query("since") != "" || c.Query("until") != "" || c.Query("tail") != "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"systemd 类型的应用只接受 level/from/to/cursor/limit 参数")
		return
	}
	params, ok := s.parseLogParams(c)
	if !ok {
		return
	}
	// 绑定即事实：source 与 unit 由服务端填，调用方无从指定。
	params.Source = journal.SourceJournal
	params.Unit = app.Binding.Unit
	if err := params.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	reader := s.opts.Logs
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"日志不可用：未配置代理连接")
		return
	}
	page, err := reader.Query(c.Request.Context(), params)
	if err != nil {
		s.writeLogsError(c, err, "读取应用日志失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, appLogsResponse{
		Source: "journal",
		Unit:   app.Binding.Unit,
		Page:   page,
	})
}

// serveAppContainerLogs 读容器的日志。
func (s *Server) serveAppContainerLogs(c *gin.Context, app apps.App) {
	if c.Query("unit") != "" || c.Query("source") != "" ||
		c.Query("level") != "" || c.Query("from") != "" || c.Query("to") != "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"docker 类型的应用只接受 since/until/tail/cursor 参数")
		return
	}
	params := containers.LogParams{
		ContainerID: app.Binding.ContainerID,
		Since:       strings.TrimSpace(c.Query("since")),
		Until:       strings.TrimSpace(c.Query("until")),
		Cursor:      strings.TrimSpace(c.Query("cursor")),
	}
	if raw := strings.TrimSpace(c.Query("tail")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > containers.MaxLogLines {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"tail 必须是 1—"+strconv.Itoa(containers.MaxLogLines)+" 的整数")
			return
		}
		params.Tail = n
	}
	if err := params.Validate(time.Now()); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	page, err := reader.Logs(c.Request.Context(), params)
	if err != nil {
		// 容器不存在时这里回 404 CONTAINER_NOT_FOUND，消息由代理给出：
		// 绑定的容器可能已被重建，页面据此要求重新确认绑定。
		s.writeContainersError(c, err, "读取应用日志失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, appLogsResponse{
		Source:      "docker",
		ContainerID: params.ContainerID,
		Page:        page,
	})
}
