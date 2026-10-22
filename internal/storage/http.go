package storage

// 存储写动作的拒绝判决（P07.2）。
//
// 判决只在两处产生：预检（Assessment.Allowed=false + Blockers）与执行期的复核
// （身份/版本/放行结论变了，或者命令与读回不一致）。执行期必须把这几种情形分开说，
// 因为它们对管理员的意义完全不同：
//
//   - DEVICE_IDENTITY_CHANGED（409）：目标不是当初那块盘了，什么都别做，重新预检；
//   - PREFLIGHT_STALE（409）：还是那块盘，但它的状态在预检之后变了，重新预检；
//   - VALIDATION_ERROR（409）：当前状态不允许执行（挂载、占用、保护……），带着阻塞码；
//   - UNSUPPORTED_DEVICE（422）：这块盘的类型/扇区大小不在支持范围内；
//   - DEPENDENCY_MISSING（503）：本机没有 sfdisk，写动作无从执行；
//   - COMMAND_FAILED（500）：命令失败或**读回与预期不一致**——实际状态已写进消息里，
//     既不能说"成功了"，也不能说"什么都没发生"。

import (
	"fmt"
	"net/http"
	"strings"

	"upanel/internal/httpx"
)

// HTTPError 是存储模块给出的判决，代理层按它回状态码与错误码
// （接口见 internal/agent 的 statusCarrier）。
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

// identityChanged 是"换盘了"。
func identityChanged(displayName string) *HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeDeviceIdentityChanged,
		"执行前复核发现目标身份已变化（%s），本次不做任何写入；请重新预检", displayName)
}

// preflightStale 是"还是那块盘，但状态变了"。
func preflightStale(detail string) *HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodePreflightStale,
		"执行前复核发现目标状态在预检之后发生变化（%s），本次不做任何写入；请重新预检", detail)
}

// notAllowed 把判决里的阻塞项翻成拒绝（带码，便于前端与日志定位）。
func notAllowed(blockers []Blocker) *HTTPError {
	codes := make([]string, 0, len(blockers))
	reasons := make([]string, 0, len(blockers))
	for _, b := range blockers {
		codes = append(codes, b.Code)
		reasons = append(reasons, b.Message)
	}
	return newHTTPError(http.StatusConflict, httpx.CodeValidationError,
		"当前状态不允许执行（%s）：%s", strings.Join(codes, ","), strings.Join(reasons, "；"))
}

// unsupportedDevice 是"这块盘不在支持范围内"。
func unsupportedDevice(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusUnprocessableEntity, httpx.CodeUnsupportedDevice, format, args...)
}

// dependencyMissing 是"本机缺少执行该动作所需的程序"。
func dependencyMissing(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusServiceUnavailable, httpx.CodeDependencyMissing, format, args...)
}

// commandFailed 是"命令失败"或"写后读回与预期不一致"。
//
// 它永远带着实际状态：一条只写"命令失败"的失败记录，会让管理员在一个已经动过的
// 分区表前不知道该怎么办。
func commandFailed(format string, args ...any) *HTTPError {
	return newHTTPError(http.StatusInternalServerError, httpx.CodeCommandFailed, format, args...)
}
