package api

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
)

// requestIDMiddleware 为每个请求确定 request_id 并放进上下文。
//
// 沿用调用方传入的合法 X-Request-ID（便于跨日志关联），否则生成新的。
// 非法值（超长、含非可见字符）直接丢弃重新生成，不截断复用。
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := httpx.InboundRequestID(c.Request)
		inherited := rid != ""
		if !inherited {
			rid = httpx.NewRequestID()
		}
		c.Request = c.Request.WithContext(httpx.WithRequestID(c.Request.Context(), rid))
		c.Header(httpx.HeaderRequestID, rid)
		c.Set(ctxKeyRequestID, rid)
		c.Set(ctxKeyRequestIDInherited, inherited)
		c.Next()
	}
}

// accessLogMiddleware 记录每个请求的结果。
//
// 只记录方法、路径、状态码、耗时、字节数与来源地址；不记录请求体、查询参数值和
// Authorization/Cookie 等秘密字段（CONTRACTS.md 第 2 节：秘密字段不能进入通用日志）。
// 来源地址取连接地址，不信任 X-Forwarded-For（生产默认不信任代理头）。
func accessLogMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			"request_id", c.GetString(ctxKeyRequestID),
			"request_id_inherited", c.GetBool(ctxKeyRequestIDInherited),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"route", c.FullPath(),
			"status", status,
			"bytes", c.Writer.Size(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", c.Request.RemoteAddr,
			"user_agent", c.Request.UserAgent(),
		}
		switch {
		case status >= 500:
			logger.Error("请求处理失败", attrs...)
		case status >= 400:
			logger.Warn("请求被拒绝", attrs...)
		default:
			logger.Info("请求完成", attrs...)
		}
	}
}

// recoveryMiddleware 把 panic 转成 500 错误信封，并保留堆栈到日志。
//
// 堆栈只进日志，绝不出现在响应体里。
func recoveryMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("请求处理发生 panic",
					"request_id", c.GetString(ctxKeyRequestID),
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				if c.Writer.Written() {
					// 已经开始写响应：再写会破坏响应，只能中断连接。
					c.Abort()
					return
				}
				httpx.WriteErrorCode(c.Writer, c.Request, httpx.CodeInternalError, "服务内部错误")
				c.Abort()
			}
		}()
		c.Next()
	}
}

// bodyLimitMiddleware 限制请求体大小，防止无界读取。
//
// 具体动作的严格上限在 P02 之后按契约逐个收紧；这里先给出全局兜底。
func bodyLimitMiddleware(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}

// IsBodyTooLarge 判断错误是否来自请求体超限，供 handler 映射成 413。
func IsBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// allowedMethods 返回 gin 为 405 响应写入的 Allow 头值（已排序、逗号分隔）。
func allowedMethods(c *gin.Context) string {
	if v := c.Writer.Header().Get("Allow"); v != "" {
		return v
	}
	return strings.Join(c.Writer.Header().Values("Allow"), ", ")
}
