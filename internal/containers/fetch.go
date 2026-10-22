// Web 侧的读取端（P09.3）。
//
// Web 进程**不读 docker.sock、不执行任何命令**：所有读取与启停都在代理里完成，
// 这里只把结构化查询交给代理、把结果校验后交给接口层。这一层不缓存——容器状态是
// “此刻的事实”，缓存只会让页面上的时间和实际读到的状态对不上。

package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
)

// timeNow 可替换时钟（测试用）。
var timeNow = func() time.Time { return time.Now().UTC() }

// ErrAgentUnavailable 表示读不到容器信息是因为代理不可用（连不上、超时、响应不合法）。
//
// 它与“这台机器上没有 Docker”是两件事：后者是 200 + available=false，
// 后者等一会儿会好。接口层把前者映射成 503。
var ErrAgentUnavailable = errors.New("containers: 代理不可用")

// ErrStreamInterrupted 表示流在没有结束标记的情况下断了。
var ErrStreamInterrupted = errors.New("containers: 容器日志流在没有结束标记的情况下中断")

// Agent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type Agent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
	CreateStream(ctx context.Context, purpose string, params any) (agentclient.StreamPermit, error)
	OpenStream(ctx context.Context, streamID string) (*agentclient.StreamReader, error)
}

// RejectedError 是“代理明确拒绝了这次调用”的载体（例如容器不存在）。
type RejectedError struct {
	Status  int
	Code    string
	Message string
}

func (e *RejectedError) Error() string { return e.Message }

// Fetcher 是 Web 侧的容器读取端。
type Fetcher struct {
	agent  Agent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要运行时用一个永远返回空列表的假实现顶上。
func NewFetcher(agent Agent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("containers: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// List 读取容器清单。
func (f *Fetcher) List(ctx context.Context) (ListResponse, error) {
	var out ListResponse
	if err := f.call(ctx, QueryList, nil, &out); err != nil {
		return ListResponse{}, err
	}
	if out.Containers == nil {
		out.Containers = []Container{}
	}
	return out, nil
}

// Get 读取单个容器。
func (f *Fetcher) Get(ctx context.Context, id string) (Container, error) {
	if !ValidContainerID(id) {
		return Container{}, &RejectedError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR",
			Message: "container_id 必须是完整的 64 位容器 ID"}
	}
	var out Container
	if err := f.call(ctx, QueryGet, map[string]string{"container_id": id}, &out); err != nil {
		return Container{}, err
	}
	return out, nil
}

// Logs 读取一页容器日志。
func (f *Fetcher) Logs(ctx context.Context, params LogParams) (LogPage, error) {
	if err := params.Validate(timeNow()); err != nil {
		return LogPage{}, &RejectedError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	var out LogPage
	if err := f.call(ctx, QueryLogs, params, &out); err != nil {
		return LogPage{}, err
	}
	if out.Entries == nil {
		out.Entries = []LogEntry{}
	}
	return out, nil
}

// Images 读取镜像清单。
func (f *Fetcher) Images(ctx context.Context) (ImagesResponse, error) {
	var out ImagesResponse
	if err := f.call(ctx, QueryImages, nil, &out); err != nil {
		return ImagesResponse{}, err
	}
	if out.Images == nil {
		out.Images = []Image{}
	}
	return out, nil
}

// Networks 读取网络清单。
func (f *Fetcher) Networks(ctx context.Context) (NetworksResponse, error) {
	var out NetworksResponse
	if err := f.call(ctx, QueryNetworks, nil, &out); err != nil {
		return NetworksResponse{}, err
	}
	if out.Networks == nil {
		out.Networks = []Network{}
	}
	return out, nil
}

// Volumes 读取数据卷清单。
func (f *Fetcher) Volumes(ctx context.Context) (VolumesResponse, error) {
	var out VolumesResponse
	if err := f.call(ctx, QueryVolumes, nil, &out); err != nil {
		return VolumesResponse{}, err
	}
	if out.Volumes == nil {
		out.Volumes = []Volume{}
	}
	return out, nil
}

// Projects 读取 compose 项目清单（只读）。
func (f *Fetcher) Projects(ctx context.Context) (ProjectsResponse, error) {
	var out ProjectsResponse
	if err := f.call(ctx, QueryProjects, nil, &out); err != nil {
		return ProjectsResponse{}, err
	}
	if out.Projects == nil {
		out.Projects = []Project{}
	}
	return out, nil
}

// StreamWithReason 跟随一条容器日志流。
func (f *Fetcher) StreamWithReason(ctx context.Context, params LogParams, emit func(LogEntry) error) (string, error) {
	if err := params.Validate(timeNow()); err != nil {
		return "", &RejectedError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	permit, err := f.agent.CreateStream(ctx, StreamPurposeLogs, params)
	if err != nil {
		return "", f.mapError(err, "申请容器日志流许可")
	}
	reader, err := f.agent.OpenStream(ctx, permit.StreamID)
	if err != nil {
		return "", f.mapError(err, "打开容器日志流")
	}
	defer func() { _ = reader.Close() }()

	for {
		frame, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", ErrStreamInterrupted
			}
			var unavailable *agentclient.UnavailableError
			if errors.As(err, &unavailable) {
				return "", fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
			}
			return "", fmt.Errorf("containers: 读取容器日志流失败: %w", err)
		}
		switch frame.Type {
		case "entry":
			var entry LogEntry
			if len(frame.Entry) == 0 {
				return "", fmt.Errorf("%w: entry 帧没有内容", ErrStreamInterrupted)
			}
			if err := json.Unmarshal(frame.Entry, &entry); err != nil {
				return "", fmt.Errorf("%w: entry 帧无法解析: %v", ErrStreamInterrupted, err)
			}
			if entry.Timestamp.IsZero() {
				return "", fmt.Errorf("%w: entry 帧缺少 timestamp", ErrStreamInterrupted)
			}
			if err := emit(entry); err != nil {
				return "", err
			}
		case "end":
			if frame.Reason == "" {
				return "", fmt.Errorf("%w: 结束帧没有给出原因", ErrStreamInterrupted)
			}
			return frame.Reason, nil
		case "error":
			return "", fmt.Errorf("containers: 代理报告容器日志流失败: %s %s", frame.Code, frame.Message)
		default:
			return "", fmt.Errorf("%w: 未知的流帧类型 %q", ErrStreamInterrupted, frame.Type)
		}
	}
}

// call 执行一次结构化查询并把错误翻译成 Web 侧的分类。
func (f *Fetcher) call(ctx context.Context, name string, params any, out any) error {
	raw, err := f.agent.Query(ctx, name, params)
	if err != nil {
		return f.mapError(err, name)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: 代理返回的 %s 结果不是合法 JSON: %v", ErrAgentUnavailable, name, err)
	}
	return nil
}

// mapError 把代理错误翻译成容器模块的错误。
//
// 三类分开处理，因为接口层对它们的状态码不同：
//   - 连不上、超时、代理自己不可用：容器子系统此刻不可用 → 503 AGENT_UNAVAILABLE；
//   - 代理回了域内判决（哪怕状态码是 5xx）：如实带出（503 DOCKER_UNAVAILABLE、
//     404 容器不存在、400 参数错）；
//   - 其余：当作代理协议问题。
func (f *Fetcher) mapError(err error, what string) error {
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		return f.unavailable(what, err)
	}
	var rejected *agentclient.Error
	if errors.As(err, &rejected) {
		switch {
		case rejected.Status >= http.StatusInternalServerError && agentNotDomainCode(rejected.Code):
			return f.unavailable(what, err)
		case rejected.Status == http.StatusBadRequest:
			return &RejectedError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: rejected.Message}
		case rejected.Status == http.StatusNotFound:
			return &RejectedError{Status: http.StatusNotFound, Code: "CONTAINER_NOT_FOUND", Message: rejected.Message}
		default:
			return &RejectedError{Status: rejected.Status, Code: rejected.Code, Message: rejected.Message}
		}
	}
	return fmt.Errorf("containers: %s 被代理拒绝: %w", what, err)
}

// agentNotDomainCode 判断代理给的这个错误码是不是“代理这一层自己”的。
//
// 为什么不能只看状态码：代理在说“这台机器上的 Docker 不可用”时也是 503，
// 但码是 DOCKER_UNAVAILABLE。把它压成 AGENT_UNAVAILABLE，页面就会告诉用户
// “等代理一会儿”，而真正该做的是把 dockerd 起起来——两种 5xx 的处置完全不同。
// 通用码（空、AGENT_UNAVAILABLE、INTERNAL_ERROR）说明请求根本没进到域逻辑。
func agentNotDomainCode(code string) bool {
	switch httpx.Code(code) {
	case "", httpx.CodeAgentUnavailable, httpx.CodeInternalError:
		return true
	}
	return false
}

// unavailable 记录并包装“代理不可用”。
func (f *Fetcher) unavailable(what string, err error) error {
	if f.logger != nil {
		f.logger.Warn(what+"：代理不可用", "error", err)
	}
	return fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
}
