package shares

// 让判决层与执行层自己给出 HTTP 判决。
//
// 与 internal/storage 的做法一致：代理的查询/执行层不按错误文本猜状态码，只问
// “你这次拒绝对应哪个状态码”。把“共享不存在”说成 500 会让页面把一条没了的记录
// 显示成“面板出故障了”，然后所有人都去查错地方。

import (
	"fmt"
	"net/http"

	"upanel/internal/httpx"
)

// HTTPError 是一次带判决的拒绝。
type HTTPError struct {
	Status  int
	Code    httpx.Code
	Message string
}

// Error 实现 error。
func (e *HTTPError) Error() string { return e.Message }

// HTTPStatus 让代理层按判决回响应。
func (e *HTTPError) HTTPStatus() (int, httpx.Code) { return e.Status, e.Code }

func newHTTPError(status int, code httpx.Code, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// dependencyMissing 表示本机缺依赖（工具没装、登记表没接）。
func dependencyMissing(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusServiceUnavailable, httpx.CodeDependencyMissing, format, args...)
}

// dependencyBlocked 表示前置条件不满足（卷没挂、账号被引用……）。
func dependencyBlocked(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeDependencyBlocked, format, args...)
}

// validationError 表示参数/状态不允许这次操作。
func validationError(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusBadRequest, httpx.CodeValidationError, format, args...)
}

// versionConflict 表示乐观并发令牌不符。
func versionConflict(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeVersionConflict, format, args...)
}

// identityChanged 表示目标身份变了（例如卷挂着的已经不是登记的那个文件系统）。
func identityChanged(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeDeviceIdentityChanged, format, args...)
}

// commandFailed 表示命令没有按预期完成，或读回核验不通过。
func commandFailed(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusInternalServerError, httpx.CodeCommandFailed, format, args...)
}

// notFound 表示登记表里没有这个对象。
func notFound(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusNotFound, httpx.CodeNotFound, format, args...)
}
