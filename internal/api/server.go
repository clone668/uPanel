// Package api 是 Web 服务的 HTTP 入口：路由组织、中间件与 DTO。
//
// 框架按 PLAN.md 第 3 节固定为 Gin。P01.2 建立路由分组、request_id、访问日志、panic
// 恢复、请求体上限与统一的 404/405 行为；认证、CSRF、DTO 校验与业务路由按阶段追加，
// 本包不预置它们的“空实现”。
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/operations"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// MaxRequestBodyBytes 是所有 JSON 请求体的默认上限。
//
// CONTRACTS.md 第 1 节要求按动作分别限制；在具体动作落地前先用一个全局上限兜底，
// 避免任何路由接受无界请求体。
const MaxRequestBodyBytes int64 = 1 << 20 // 1 MiB

// Options 是构造 Web 服务的参数。
type Options struct {
	// ListenAddress 为最终监听地址；由 config.ResolveListen 计算。
	ListenAddress string
	// Mode 为 production 或 mock。
	Mode platform.Mode
	// Version 为构建版本信息。
	Version version.Info
	// Logger 必须非空：服务不允许在无日志的情况下运行。
	Logger *slog.Logger
	// ShutdownTimeout 为收到停止信号后等待在途请求的时间。
	ShutdownTimeout time.Duration
	// SchemaVersion 为启动时已迁移到的数据库 schema 版本，用于启动日志。
	// 0 表示尚未引入数据库（测试场景）。
	SchemaVersion int
	// Frontend 为 SPA 静态资源处理器（internal/webui）。为 nil 时非 API 路径也返回 JSON 404，
	// 与 P01.2 的行为一致（用于测试与不嵌入前端的场景）。
	Frontend http.Handler
	// Auth 是认证与会话服务。P02.1 起为必需依赖：没有它就无法校验登录、会话与 CSRF。
	Auth *auth.Service
	// AllowedOrigins 是允许发起状态变更请求的源（已由 config.ResolveAllowedOrigins 规范化）。
	// 不能为空：空集合等于拒绝所有浏览器请求。
	AllowedOrigins []string
	// Audit 是追加式审计写入器。P02.2 起为必需依赖：账号变更必须留下审计行。
	Audit *audit.Writer
	// Operations 是预检与任务服务（P03.1）。必需依赖：没有它就没有“先看影响再执行”的入口。
	// 代理不可用属于运行时状态（服务返回 503），不是启动条件。
	Operations *operations.Service
}

// Server 是 Web HTTP 服务。
type Server struct {
	opts           Options
	startedAt      time.Time
	engine         *gin.Engine
	http           *http.Server
	allowedOrigins *httpx.OriginSet
}

// healthLive 是 GET /api/v1/health/live 的响应体。
// CONTRACTS.md 第 3 节：最小存活状态，不返回内部路径或版本细节。
type healthLive struct {
	Status string `json:"status"`
}

// New 构造服务实例。参数不合法时返回错误，调用方必须拒绝启动。
func New(opts Options) (*Server, error) {
	if opts.Logger == nil {
		return nil, errors.New("api: Logger 不能为空")
	}
	if !opts.Mode.Valid() {
		return nil, fmt.Errorf("api: 未知运行模式 %q", string(opts.Mode))
	}
	if opts.ListenAddress == "" {
		return nil, errors.New("api: ListenAddress 不能为空")
	}
	if opts.Mode == platform.ModeMock && opts.ListenAddress != config.ListenMock {
		return nil, fmt.Errorf("api: mock 模式只能监听 %s，当前为 %s", config.ListenMock, opts.ListenAddress)
	}
	if opts.ShutdownTimeout <= 0 {
		return nil, errors.New("api: ShutdownTimeout 必须为正数")
	}
	if opts.Auth == nil {
		return nil, errors.New("api: Auth 不能为空")
	}
	if opts.Audit == nil {
		return nil, errors.New("api: Audit 不能为空")
	}
	if opts.Operations == nil {
		return nil, errors.New("api: Operations 不能为空")
	}
	origins, err := httpx.NewOriginSet(opts.AllowedOrigins)
	if err != nil {
		return nil, fmt.Errorf("api: AllowedOrigins 不合法: %w", err)
	}
	if origins.Empty() {
		return nil, errors.New("api: AllowedOrigins 不能为空")
	}

	s := &Server{opts: opts, startedAt: time.Now().UTC(), allowedOrigins: origins}
	s.engine = s.routes()
	s.http = &http.Server{
		Handler:           s.engine,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Handler 返回路由，供测试与嵌入式使用。
func (s *Server) Handler() http.Handler { return s.engine }

// Serve 监听端口并阻塞服务，直到 ctx 结束或监听失败。
//
// 监听在进入 Serve 之前显式完成，端口被占用等问题会作为启动错误返回，
// 不会被当成“服务已就绪”。
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.ListenAddress)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.opts.ListenAddress, err)
	}

	loopback := config.IsLoopbackAddress(ln.Addr().String())
	if !loopback {
		s.opts.Logger.Warn("监听地址不是回环地址，面板将对该网络可达",
			"address", ln.Addr().String(),
			"hint", "默认应为 127.0.0.1，并配合 SSH 隧道访问")
	}

	s.opts.Logger.Info("Web 服务已开始监听",
		"address", ln.Addr().String(),
		"network", ln.Addr().Network(),
		"mode", s.opts.Mode.String(),
		"loopback_only", loopback,
		"version", s.opts.Version.Version,
		"schema_version", s.opts.SchemaVersion,
		"pid", os.Getpid(),
		"started_at", s.startedAt)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		s.opts.Logger.Info("收到停止信号，开始优雅停止", "timeout", s.opts.ShutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.opts.Logger.Error("优雅停止未能在超时内完成", "error", err)
			_ = s.http.Close()
		}
	}()

	err = s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		s.opts.Logger.Info("Web 服务已停止")
		return nil
	}
	return fmt.Errorf("HTTP 服务异常退出: %w", err)
}

// writeError 是包内统一的错误写出入口，保证状态码与错误码一致。
func writeError(c *gin.Context, status int, code httpx.Code, message string) {
	httpx.WriteError(c.Writer, c.Request, status, code, message)
}
