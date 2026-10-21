package files

// 错误到 HTTP 判决的映射（P06.1）。
//
// 这段映射放在 files 里，而不是在代理或 Web 各写一份：同一个“路径越界”在
// 代理的查询接口、代理的流接口、Web 的公开接口里必须是同一个状态码与同一个 code，
// 否则前端要按调用路径猜，而验收脚本也没法只断言一处。
//
// 为什么要区分这几种拒绝：
//   - PATH_OUTSIDE_ROOT（422）不是“参数写错了”，而是“这个路径不允许”；
//     把它说成 400 会让人反复改参数重试；
//   - FILE_NOT_TEXT（422）与 VERSION_CONFLICT（409）都是调用方可以自己处理的确定条件；
//   - NOT_IMPLEMENTED（501）是平台能力缺失，不该被当成“服务坏了”（503）。
//
// 状态码不在这里发明：除“体积超限用 413”这一处（与本项目其它超限处理一致，
// 且 413 在契约允许的状态码里），其余一律取 httpx.StatusForCode，
// 让同一个 code 在代理与 Web 两边对应同一个状态码。
//
// 错误码只新增了两个（ROOT_UNAVAILABLE、FILE_NOT_TEXT）：契约枚举是封闭的，
// 其余情形都能落到已有的码上——“有多个硬链接”就是 PATH_OUTSIDE_ROOT
// （这个 inode 的另一个名字可能在根外），“超过上限”就是 413 的 VALIDATION_ERROR。

import (
	"errors"
	"fmt"
	"net/http"

	"upanel/internal/httpx"
)

// HTTPError 是一次拒绝的完整判决：状态码、错误码与给人看的说明。
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

// HTTPStatusOf 把 files 的错误翻成 HTTP 判决。
//
// 未分类的错误返回 (nil, false)：调用方按自己的策略处理（代理侧记日志回 500，
// Web 侧记日志回 500）。这里**不**给任何错误一个兜底的 200 或 400——
// 一个没被分类的失败应该看起来像失败。
func HTTPStatusOf(err error) (*HTTPError, bool) {
	if err == nil {
		return nil, false
	}
	var existing *HTTPError
	if errors.As(err, &existing) {
		return existing, true
	}
	switch {
	case errors.Is(err, ErrInvalidParams), errors.Is(err, ErrNotDirectory),
		errors.Is(err, ErrNotRegular):
		return reject(httpx.CodeValidationError, err), true
	case errors.Is(err, ErrTooLarge):
		return &HTTPError{Status: http.StatusRequestEntityTooLarge, Code: httpx.CodeValidationError,
			Message: err.Error()}, true
	case errors.Is(err, ErrPathRejected), errors.Is(err, ErrHardLinked):
		// 硬链接归到 PATH_OUTSIDE_ROOT：拒绝它的理由正是“这个 inode 在根外
		// 可能还有别的名字”，与越界是同一件事。
		return reject(httpx.CodePathOutsideRoot, err), true
	case errors.Is(err, ErrUnknownRoot), errors.Is(err, ErrNotFound):
		return reject(httpx.CodeNotFound, err), true
	case errors.Is(err, ErrRootUnavailable):
		return reject(httpx.CodeRootUnavailable, err), true
	case errors.Is(err, ErrNotText):
		return reject(httpx.CodeFileNotText, err), true
	case errors.Is(err, ErrVersionConflict):
		return reject(httpx.CodeVersionConflict, err), true
	case errors.Is(err, ErrUnsupported):
		return reject(httpx.CodeNotImplemented, err), true
	}
	return nil, false
}

// AsHTTPError 把任意错误包成 *HTTPError（未分类时按 500 处理并保留原文）。
//
// 代理用它把查询实现的拒绝变成 HTTP 响应；未分类错误因此不会“看起来像 200”。
func AsHTTPError(err error) *HTTPError {
	if err == nil {
		return nil
	}
	if mapped, ok := HTTPStatusOf(err); ok {
		return mapped
	}
	return &HTTPError{
		Status:  http.StatusInternalServerError,
		Code:    httpx.CodeInternalError,
		Message: fmt.Sprintf("文件操作失败: %v", err),
	}
}
