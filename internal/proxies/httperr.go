package proxies

// 让写入路径自己给出 HTTP 判决（P10.2）。
//
// 与 internal/shares/http.go 同一套约定：代理的动作执行层不按错误文本猜状态码，
// 只问“这次拒绝对应哪个状态码”。把“回滚也失败了”说成 400，会让任务中心把一次
// 需要人工介入的事故显示成一次参数错误。

import (
	"errors"
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

// dependencyBlocked 表示前置条件不满足（Nginx 不在、域名被占、读不到配置）。
func dependencyBlocked(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeDependencyBlocked, format, args...)
}

// validationError 表示参数/状态不允许这次操作。
func validationError(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusBadRequest, httpx.CodeValidationError, format, args...)
}

// versionConflict 表示绑定的版本与当前记录不一致。
func versionConflict(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeVersionConflict, format, args...)
}

// commandFailed 表示命令没有按预期完成，或读回核验不通过。
func commandFailed(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusInternalServerError, httpx.CodeCommandFailed, format, args...)
}

// paramsRejected 把参数校验错误映射成 400。
//
// 执行层在归一化参数时也可能发现参数不合法（例如参数是别的调用方拼出来的）。那种
// 拒绝是 400，而 *ValidationError 没有自己的 HTTPStatus：不映射就会被代理层当成 500，
// 让一次“参数写错了”看起来像“这台机器坏了”。
func paramsRejected(err error) error {
	var invalid *ValidationError
	if errors.As(err, &invalid) {
		return validationError("%s（字段 %s）", invalid.Message, invalid.Field)
	}
	return err
}
