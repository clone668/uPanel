package terminal

// 错误到 HTTP 判决的映射。
//
// 与 internal/files/http.go 同一个理由：同一个“会话已被占用”在代理的查询接口、
// 流接口、Web 的公开接口里必须是同一个状态码与同一个 code，否则前端要按调用路径
// 猜，验收脚本也没法只断言一处。
//
// 错误码全部落在契约已有的枚举里（不新增码）：会话不存在是 NOT_FOUND，
// 不属于你是 FORBIDDEN，数量到顶或已被占用是 RESOURCE_BUSY，参数不对是
// VALIDATION_ERROR，没有 PTY 支持是 NOT_IMPLEMENTED。

import (
	"errors"
	"net/http"

	"upanel/internal/httpx"
)

// HTTPError 是一次拒绝的完整判决。
type HTTPError struct {
	Status  int
	Code    httpx.Code
	Message string
}

// Error 实现 error。
func (e *HTTPError) Error() string { return e.Message }

// HTTPStatus 让代理层按判决回响应（接口见 internal/agent 的 statusCarrier）。
func (e *HTTPError) HTTPStatus() (int, httpx.Code) { return e.Status, e.Code }

// reject 用错误码的默认状态码构造判决。
func reject(code httpx.Code, err error) *HTTPError {
	return &HTTPError{Status: httpx.StatusForCode(code), Code: code, Message: err.Error()}
}

// HTTPStatusOf 把终端的错误翻成 HTTP 判决。
//
// 未分类的错误返回 (nil, false)：调用方记日志回 500。这里不给任何错误一个兜底
// 判决——一个没被分类的失败应该看起来像失败。
func HTTPStatusOf(err error) (*HTTPError, bool) {
	if err == nil {
		return nil, false
	}
	var existing *HTTPError
	if errors.As(err, &existing) {
		return existing, true
	}
	switch {
	case errors.Is(err, ErrInvalidParams):
		return reject(httpx.CodeValidationError, err), true
	case errors.Is(err, ErrAccountInvalid):
		// 终端账号不可用是本机的配置/安装问题，不是调用方的错：501 让面板显示
		// “本机没有可用的终端”，而不是让管理员反复重试一次注定失败的创建。
		return &HTTPError{Status: http.StatusNotImplemented, Code: httpx.CodeNotImplemented,
			Message: err.Error()}, true
	case errors.Is(err, ErrUnsupported):
		return &HTTPError{Status: http.StatusNotImplemented, Code: httpx.CodeNotImplemented,
			Message: err.Error()}, true
	case errors.Is(err, ErrDisabled):
		// 代理没有登记终端能力：这是本机的安装/配置问题。501 让界面显示
		// “本机没有可用的终端”并给出原因，而不是让管理员反复重试一次注定失败的创建。
		return &HTTPError{Status: http.StatusNotImplemented, Code: httpx.CodeNotImplemented,
			Message: err.Error()}, true
	case errors.Is(err, ErrNotFound):
		return reject(httpx.CodeNotFound, err), true
	case errors.Is(err, ErrNotOwner):
		return reject(httpx.CodeForbidden, err), true
	case errors.Is(err, ErrLimitReached), errors.Is(err, ErrAlreadyAttached), errors.Is(err, ErrClosed):
		// 409 而不是 429：这不是“你请求太快”，而是“此刻的状态不允许”——
		// 已经有一个会话/连接占着位置。重试要等它们结束，不是等一会儿。
		return reject(httpx.CodeResourceBusy, err), true
	}
	return nil, false
}
