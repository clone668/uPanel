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
