package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/httpx"
)

// 上下文键。只在包内使用，避免与业务键冲突。
const (
	ctxKeyRequestID          = "upanel.request_id"
	ctxKeyRequestIDInherited = "upanel.request_id_inherited"
	// ctxKeyAuditAction 是本路由组的审计动作名。中间件拒绝请求时用它记录“被拒绝”的事件，
	// 因为那时还没有进入 handler，拿不到更精确的动作。
	ctxKeyAuditAction = "upanel.audit_action"
)

// routes 组装路由与中间件。
//
// 路由组织：公共 API 统一挂在 /api/v1 下，按领域分组（health、auth、system、storage……）。
// 本阶段只实现 health 分组；其余分组在对应阶段加入，不在路由表里预埋返回 501 的占位路由。
//
// 未命中的路径分两类处理（P01.4）：
//   - /api 与 /api/... 一律 JSON 404，**绝不**回落到静态页面或 200；
//   - 其余路径交给前端静态处理器（SPA 回退）。没有前端处理器时同样 JSON 404。
func (s *Server) routes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	// 不自动重定向：/api/v1/health/live/ 与 /api/v1/health/live 是不同路径，
	// 重定向会让客户端把 301 当成成功。
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.HandleMethodNotAllowed = true

	r.Use(
		requestIDMiddleware(),
		accessLogMiddleware(s.opts.Logger),
		recoveryMiddleware(s.opts.Logger),
		bodyLimitMiddleware(MaxRequestBodyBytes),
	)

	v1 := r.Group("/api/v1")
	v1.Use(s.originGuard())
	{
		health := v1.Group("/health")
		health.GET("/live", s.handleHealthLive)
		health.HEAD("/live", s.handleHealthLive)

		authGroup := v1.Group("/auth")
		authGroup.POST("/login", s.handleLogin)
		authGroup.GET("/session", s.requireSession(), s.handleSession)
		authGroup.POST("/logout", s.requireSession(), s.requireCSRF(), s.handleLogout)
		authGroup.POST("/reauth", s.auditRoute(audit.ActionAuthReauth),
			s.requireSession(), s.requireAdmin(), s.requireCSRF(), s.handleReauth)

		// 账号管理（P02.2）：整组 admin；改动类操作额外要求 5 分钟内的再认证。
		users := v1.Group("/users", s.auditRoute(audit.ActionUserManage))
		users.Use(s.requireSession(), s.requireAdmin())
		users.GET("", s.handleListUsers)
		users.POST("", s.requireCSRF(), s.handleCreateUser)
		users.PATCH("/:id", s.requireCSRF(), s.requireReauth(), s.handleUpdateUser)
		users.POST("/:id/password", s.requireCSRF(), s.requireReauth(), s.handleSetUserPassword)
		users.DELETE("/:id", s.requireCSRF(), s.requireReauth(), s.handleDeleteUser)

		// 系统只读信息（P04.1）：权限 V，任何已登录用户都能看系统概况。
		// 采集与缓存都在代理侧，这里只透传最近一次快照，不叠加第二层缓存。
		system := v1.Group("/system")
		system.Use(s.requireSession())
		system.GET("/summary", s.handleSystemSummary)

		// 存储只读视图（P04.2）：权限 V（CONTRACTS.md 第 6 节）。
		// 三个路由读的是代理的同一份扫描快照，因此设备列表与拓扑永远一致；
		// 本阶段没有任何写路由——分区/格式化要等 P07 的预检与任务链路。
		st := v1.Group("/storage")
		st.Use(s.requireSession())
		st.GET("/topology", s.handleStorageTopology)
		st.GET("/devices", s.handleListDevices)
		st.GET("/devices/:id", s.handleGetDevice)

		// 受管理卷（P07.4）：列表与详情是 V，占用详情是 admin
		// （CONTRACTS.md 第 6 节：占用里有进程命令行与 PID）。
		// 这些路由**只读**：导入/挂载/卸载/改配置/退出管理都走统一的
		// POST /operations/preflight + POST /jobs（action=storage.volume.*），
		// 不在这里开一个“直接执行”的入口。
		st.GET("/volumes", s.handleListVolumes)
		st.GET("/volumes/:id", s.handleGetVolume)
		st.GET("/volumes/:id/blockers", s.requireAdmin(), s.handleVolumeBlockers)

		// 共享（P08.1）：整组 admin（CONTRACTS.md 第 7 节）。共享定义里有路径、
		// 账号名单与来源网段，合起来就是“谁能访问哪个目录”的完整答案，viewer
		// 只在首页看数量与健康摘要。
		//
		// 三条路由都**只读**，而且都把能力报告一起返回：definition.enabled 是意图，
		// config_applied 是事实，两者分开显示才不会被读成“已生效”。
		// 应用/禁用/删除共享、建删账号都是系统写操作，走 POST /operations/preflight
		// + POST /jobs（action=share.* 与 share.account.*），这里不开“直接执行”的入口。
		sh := v1.Group("/shares")
		sh.Use(s.requireSession(), s.requireAdmin())
		sh.GET("", s.handleListShares)
		sh.GET("/:id", s.handleGetShare)
		// 连接状态（P08.2）：读的是 smbstatus 此刻报出的会话表，不是登记表。
		// 单独一条子路径而不是塞进详情：连接会变，详情不会。
		sh.GET("/:id/connections", s.handleGetShareConnections)

		// 受管理账号（P08.1）：admin。返回的每一条都没有密码、没有哈希，
		// 也没有 secret_ref——DTO 上就没有那些字段。
		sa := v1.Group("/share-accounts")
		sa.Use(s.requireSession(), s.requireAdmin())
		sa.GET("", s.handleListShareAccounts)

		// 秘密通道（P08.1）：admin + CSRF + 5 分钟内再认证，只进不出。
		//
		// 这是唯一一条“写”但不进任务链路的接口：它不改主机状态，只是把明文交给代理
		// 加密保存并拿回引用。真正的系统变更（设 Samba 密码、写共享配置）仍是预检 +
		// 任务，参数里只有 secret_ref。协议里没有读取明文的路由，这里也没有。
		sec := v1.Group("/secrets", s.auditRoute(audit.ActionSecretCreate))
		sec.Use(s.requireSession(), s.requireAdmin())
		sec.POST("", s.requireCSRF(), s.requireReauth(), s.handleCreateSecret)

		// 应用与分组元数据（P09.1）：读 V、写 admin + CSRF（CONTRACTS.md 第 8 节）。
		//
		// 这一组只改元数据，**不碰主机**：不启动容器、不重启服务、不执行 Compose YAML。
		// 因此它走的是“直接写库 + 审计”，而不是预检 → 任务链路；反过来，任何真正
		// 影响运行中的动作（P09.3/P09.4 的启停、日志）仍然必须走 POST /operations/preflight
		// + POST /jobs，这里不会出现“直接执行”的入口。
		//
		// 收藏是当前用户的偏好：PUT 幂等、不需要 version，viewer 也能改自己的星标。
		ap := v1.Group("/apps", s.auditRoute(audit.ActionAppManage))
		ap.Use(s.requireSession())
		ap.GET("", s.handleListApps)
		// 发现候选（P09.2）：admin。静态段必须与 /:id 并存——gin 的树会先匹配静态段，
		// 因此 /apps/discovery 不会被当成 id=discovery 的应用详情。
		ap.GET("/discovery", s.requireAdmin(), s.handleAppDiscovery)
		ap.GET("/:id", s.handleGetApp)
		// 最近一次健康探测结论（P09.2）：V。探测本身由 Web 进程的调度器按
		// interval_seconds 触发，这里只读库里那一行。
		ap.GET("/:id/health", s.handleGetAppHealth)
		ap.POST("", s.requireAdmin(), s.requireCSRF(), s.handleCreateApp)
		ap.PATCH("/:id", s.requireAdmin(), s.requireCSRF(), s.handleUpdateApp)
		ap.DELETE("/:id", s.requireAdmin(), s.requireCSRF(), s.handleDeleteApp)
		ap.PUT("/:id/favorite", s.requireCSRF(), s.handleSetAppFavorite)

		// 应用分组（P09.1）：列表 V，写 admin + CSRF。删除时分组里还有应用会被拒绝
		// （422 DEPENDENCY_BLOCKED），不会顺手把应用删掉或移组。
		ag := v1.Group("/app-groups", s.auditRoute(audit.ActionAppGroupManage))
		ag.Use(s.requireSession())
		ag.GET("", s.handleListAppGroups)
		ag.POST("", s.requireAdmin(), s.requireCSRF(), s.handleCreateAppGroup)
		ag.PATCH("/:id", s.requireAdmin(), s.requireCSRF(), s.handleUpdateAppGroup)
		ag.DELETE("/:id", s.requireAdmin(), s.requireCSRF(), s.handleDeleteAppGroup)

		// 服务与进程（P05.1）：权限 V（CONTRACTS.md 第 10 节）。只读路由在这里，
		// 启停/重启/开机启动是写操作，走统一的预检 → 任务链路（POST /operations/preflight
		// + POST /jobs，action=service.control），不在这里开一个“直接执行”的入口。
		sv := v1.Group("/services")
		sv.Use(s.requireSession())
		sv.GET("", s.handleListServices)
		sv.GET("/:unit", s.handleGetService)

		// 进程列表（P05.2）：权限 admin（CONTRACTS.md 第 10 节）。终止进程是写操作，
		// 走预检 → 任务链路（action=process.signal），而且 TERM 与 KILL 是两次独立操作：
		// KILL 的预检要求再认证与确认短语。这里不提供“直接 kill”的入口。
		pr := v1.Group("/processes")
		pr.Use(s.requireSession(), s.requireAdmin())
		pr.GET("", s.handleListProcesses)

		// 日志（P05.3）：权限 admin（CONTRACTS.md 第 10 节）。原始日志里可能有
		// 口令形态的字符串、内网地址、别人的命令行——脱敏是尽力措施，不能作为
		// 向 viewer 开放日志的依据（PLAN.md 第 14 节），所以整组 requireAdmin。
		//
		// /logs/stream 与任务事件流一样是 GET + SSE：没有 requireCSRF
		// （EventSource 无法自定义请求头），靠会话 Cookie + 同源守卫界定来源。
		lg := v1.Group("/logs")
		lg.Use(s.requireSession(), s.requireAdmin())
		lg.GET("", s.handleLogs)
		lg.GET("/stream", s.handleLogsStream)

		// 文件管理（P06.1）：整组 admin（CONTRACTS.md 第 11 节）。
		// 四个只读路由：列根、列目录、读文本、下载。上传与修改在 P06.2/P06.3，
		// 走预检 → 任务链路，这里不提供“直接写文件”的入口。
		//
		// 下载是 GET（浏览器直接下载，同源 Cookie），与日志流一样不要求 CSRF 头：
		// 它不改状态，且由会话 Cookie + 同源守卫界定来源。
		fm := v1.Group("/files")
		fm.Use(s.requireSession(), s.requireAdmin())
		fm.GET("/roots", s.handleFileRoots)
		fm.GET("/list", s.handleFileList)
		fm.GET("/text", s.handleFileText)
		fm.GET("/download", s.handleFileDownload)
		// 暂存上传（P06.2）：内容收进根内 .upanel/staging，目标路径一个字节都没变。
		// 落地必须走 file.commit_upload 的预检 → 任务；所以这里**没有**“直接写文件”的路由，
		// 只有“把字节放到暂存区”的三条 + “放弃这次上传”一条。
		//
		// PUT 是唯一允许的大请求体入口：它不解析 JSON、不缓存内容，边读边转发给代理。
		fm.POST("/uploads", s.requireCSRF(), s.handleCreateUpload)
		fm.PUT("/uploads/:id/content", s.requireCSRF(), s.handleUploadContent)
		fm.GET("/uploads/:id", s.handleGetUpload)
		fm.DELETE("/uploads/:id", s.requireCSRF(), s.handleDeleteUpload)

		// 受限终端（P06.3）：整组 admin（CONTRACTS.md 第 10 节）。开会话不在路由表里：
		// 那是 system 写操作，走 terminal.create 的预检 → 任务链路。这里只有
		// “看自己的会话、领一次性许可、连、关”四条，没有“直接开一个 shell”的入口。
		//
		// 许可必须由 POST 领取（带 CSRF）：WebSocket 握手是 GET，浏览器无法为它加
		// 自定义请求头，许可因此是这次升级的“CSRF 令牌替身”，30 秒、一次性、
		// 只经子协议传递（见 terminalws.go）。
		//
		// 两个组分开登记审计动作（attach / close），因为中间件拒绝发生在 handler
		// 之前，只有组级别能带上正确的动作名。
		tmAttach := v1.Group("/terminal", s.auditRoute(audit.ActionTerminalAttach))
		tmAttach.Use(s.requireSession(), s.requireAdmin())
		tmAttach.GET("/sessions", s.handleTerminalSessions)
		tmAttach.POST("/sessions/:id/connection-grants", s.requireCSRF(), s.handleCreateTerminalGrant)
		tmAttach.GET("/sessions/:id/connect", s.handleTerminalConnect)

		tmClose := v1.Group("/terminal", s.auditRoute(audit.ActionTerminalClose))
		tmClose.Use(s.requireSession(), s.requireAdmin())
		tmClose.DELETE("/sessions/:id", s.requireCSRF(), s.handleTerminalClose)

		// 系统写操作的统一入口（P03.1）：先预检，再由 POST /jobs 创建任务（P03.2）。
		// 预检只读不写系统；再认证在**提交任务**时校验（按动作登记策略），不在预检阶段。
		ops := v1.Group("/operations", s.auditRoute(audit.ActionOperationPreflight))
		ops.Use(s.requireSession(), s.requireAdmin())
		ops.POST("/preflight", s.requireCSRF(), s.handleCreatePreflight)

		// 任务（P03.2）。读任务允许 viewer（CONTRACTS.md 4.3 的权限列是 V）：
		// 看自己提交的任务状态不需要管理员权限；提交与取消必须 admin + CSRF。
		jobs := v1.Group("/jobs", s.auditRoute(audit.ActionJobManage))
		jobs.Use(s.requireSession())
		jobs.POST("", s.requireAdmin(), s.requireCSRF(), s.handleCreateJob)
		jobs.GET("", s.handleListJobs)
		jobs.GET("/:id", s.handleGetJob)
		jobs.POST("/:id/cancel", s.requireAdmin(), s.requireCSRF(), s.handleCancelJob)
		// 事件流与日志按契约只有 admin 能看（CONTRACTS.md 4.3：GET /jobs/:id/events、/logs 是 admin，
		// 而 GET /jobs 与 GET /jobs/:id 是 V）。viewer 看到的是摘要，看不到流。
		//
		// 注意这里**没有** requireCSRF：SSE 是 GET，EventSource 无法自定义请求头，
		// 靠会话 Cookie + Origin 校验 + 同源守卫（originGuard）来界定来源。
		jobs.GET("/:id/events", s.requireAdmin(), s.handleJobEvents)
		jobs.GET("/:id/logs", s.requireAdmin(), s.handleJobLogs)
	}

	r.NoRoute(s.handleNoRoute)
	r.NoMethod(s.handleMethodNotAllowed)
	return r
}

func (s *Server) handleHealthLive(c *gin.Context) {
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, healthLive{Status: "ok"})
}

// handleNoRoute 处理未注册的路径。
//
// 关键约束（P01 验收）：未知的 API 路径不能返回 index.html，也不能返回 200。
// 因此 API 前缀先判，命中即 JSON 404，不再往下走静态回退。
func (s *Server) handleNoRoute(c *gin.Context) {
	if isAPIPath(c.Request.URL.Path) || s.opts.Frontend == nil {
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "资源不存在")
		return
	}
	s.opts.Frontend.ServeHTTP(c.Writer, c.Request)
}

// isAPIPath 判断路径是否属于 API 命名空间（/api 本身与 /api/... 都算）。
func isAPIPath(urlPath string) bool {
	return urlPath == "/api" || strings.HasPrefix(urlPath, "/api/")
}

// handleMethodNotAllowed 只覆盖“路径已注册、方法不支持”的情况（例如 POST /api/v1/health/live）。
// 未注册路径上的任何方法都走 NoRoute：API 前缀给 JSON 404，页面路径交给静态处理器（它自己回 405）。
func (s *Server) handleMethodNotAllowed(c *gin.Context) {
	writeError(c, http.StatusMethodNotAllowed, httpx.CodeValidationError, "该路径不支持此请求方法")
}
