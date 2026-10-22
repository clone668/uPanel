package containers

import (
	"net/http"

	"upanel/internal/httpx"
)

// 这一层把容器模块的判决翻译成 HTTP 状态码与错误码（CONTRACTS.md 第 1 节）。
//
// 代理与 Web 两侧都用同一份：代理的查询/动作层通过 statusCarrier 拿到判决，
// Web 的接口层把代理回的码原样带出。判决只在**一个地方**定义，两处才不会漂。

// HTTPStatus 实现代理侧的 statusCarrier。
func (e *NotFoundError) HTTPStatus() (int, httpx.Code) {
	return http.StatusNotFound, httpx.CodeContainerNotFound
}

// HTTPStatus 把“Docker 不可用”与“代理不可用”分开：
// 前者是这台机器上没装或没跑 Docker，503 但码是 DOCKER_UNAVAILABLE。
func (e *UnavailableError) HTTPStatus() (int, httpx.Code) {
	return http.StatusServiceUnavailable, httpx.CodeDockerUnavailable
}

// LogsUnsupported 是“容器在，但它的日志驱动不保存日志”：
// 请求本身没问题，是这个容器给不出历史输出，所以是 422 而不是 500。
type LogsUnsupported struct{ ContainerID string }

func (e *LogsUnsupported) Error() string {
	return "这个容器的日志驱动不保存日志，读不到它的输出（可以把日志驱动改成 json-file 或 journald）"
}

func (e *LogsUnsupported) Is(target error) bool { return target == ErrLogsUnsupported }

func (e *LogsUnsupported) HTTPStatus() (int, httpx.Code) {
	return http.StatusUnprocessableEntity, httpx.CodeValidationError
}

// StaleError 表示预检之后容器的状态变了（重启过、被重建、被停掉）。
//
// 用 PREFLIGHT_STALE 而不是笼统的 409：前端的处理是“重新预检”，
// 而“容器不存在”是另一种结局（要重新选目标）。
type StaleError struct{ Detail string }

func (e *StaleError) Error() string {
	if e.Detail == "" {
		return "容器状态在预检之后发生了变化，请重新预检"
	}
	return "容器状态在预检之后发生了变化（" + e.Detail + "），请重新预检"
}

func (e *StaleError) Is(target error) bool { return target == ErrStateChanged }

func (e *StaleError) HTTPStatus() (int, httpx.Code) {
	return http.StatusConflict, httpx.CodePreflightStale
}

// ValidationError 表示参数形状不对（容器 ID 不是 64 位十六进制等）。
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func (e *ValidationError) HTTPStatus() (int, httpx.Code) {
	return http.StatusBadRequest, httpx.CodeValidationError
}
