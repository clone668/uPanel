package agent

// P09.3：容器、镜像、网络、数据卷与 compose 项目清单的只读查询（CONTRACTS.md 第 8 节）。
//
// 这些查询与文件模块同一套写法：参数按查询自己的 DTO 严格解码（未知字段直接拒绝），
// 实现只做转交，判决（容器不存在、Docker 没在跑、日志驱动不支持读取）由
// internal/containers 给出，走 statusCarrier 变成准确的 HTTP 状态码。
//
// 与发现模块（P09.2）的区别：那边回答“这台机器上有什么在监听”，这里回答
// “容器现在是什么状态、有哪些镜像/网络/卷、日志写了什么、能不能启停”。

import (
	"context"
	"encoding/json"
	"time"

	"upanel/internal/containers"
)

// StreamPurposeContainerLogs 是容器日志实时流的用途名。
//
// 与 logs.stream 分开：两条流的参数 DTO、限额与实现完全不同（journald 游标 vs
// docker logs --follow），共用一个用途名会让许可里的参数类型变成二选一。
const StreamPurposeContainerLogs = containers.StreamPurposeLogs

// ContainersSource 是容器读取端在代理层需要的最小接口（生产实现是 *containers.Client）。
//
// 导出是因为 cmd/upanel-agent 要把它塞进 Options 与 ProductionDeps；
// 方法集仍然只有“列、取、读日志、列镜像/网络/卷、列项目、跟一条流”这几件事。
type ContainersSource interface {
	List(ctx context.Context) (containers.ListResponse, error)
	Get(ctx context.Context, id string) (containers.Container, error)
	Logs(ctx context.Context, params containers.LogParams) (containers.LogPage, error)
	Images(ctx context.Context) (containers.ImagesResponse, error)
	Networks(ctx context.Context) (containers.NetworksResponse, error)
	Volumes(ctx context.Context) (containers.VolumesResponse, error)
	Projects(ctx context.Context) (containers.ProjectsResponse, error)
	Stream(ctx context.Context, params containers.LogParams, opts containers.StreamOptions,
		emit func(containers.LogEntry) error) (string, error)
	// 启停是同一批数据上的写操作：预检要读容器，执行要读回状态。
	// 放在同一个接口里，是因为它们共用同一个 “docker 客户端” 依赖；
	// 分成两个接口只会让接线多一个可以配错、配漏的地方。
	AssessControl(ctx context.Context, params containers.ControlParams) (containers.ControlAssessment, error)
	ExecuteControl(ctx context.Context, params containers.ControlParams, before containers.Container,
		progress func(step string, fields map[string]any)) (containers.ControlResult, error)
}

// 查询名由 internal/containers 定义：Web 侧要用同一批字符串去调用查询。
const (
	ContainersListQueryName     = containers.QueryList
	ContainersGetQueryName      = containers.QueryGet
	ContainersLogsQueryName     = containers.QueryLogs
	ContainersImagesQueryName   = containers.QueryImages
	ContainersNetworksQueryName = containers.QueryNetworks
	ContainersVolumesQueryName  = containers.QueryVolumes
	ContainersProjectsQueryName = containers.QueryProjects
)

// newContainersListQuery 登记 containers.list：无参数，返回全部容器（含已停止）。
func newContainersListQuery(src ContainersSource) Query {
	return Query{
		Name: ContainersListQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 空结构 + 严格解码：任何多余字段（包括 {"command":"ps"}）都被拒绝。
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.List(ctx)
		},
	}
}

// containerIDParams 是“只有一个容器 ID”的查询参数。
type containerIDParams struct {
	ContainerID string `json:"container_id"`
}

// newContainersGetQuery 登记 containers.get：按完整 ID 取一个容器。
func newContainersGetQuery(src ContainersSource) Query {
	return Query{
		Name: ContainersGetQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params containerIDParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if !containers.ValidContainerID(params.ContainerID) {
				return nil, &containers.ValidationError{
					Message: "container_id 必须是完整的 64 位容器 ID（不接受短 ID 与名字）",
				}
			}
			return params, nil
		},
		Run: func(ctx context.Context, decoded any) (any, error) {
			params, ok := decoded.(containerIDParams)
			if !ok {
				return nil, ErrParamsInvalid
			}
			return src.Get(ctx, params.ContainerID)
		},
	}
}

// newContainersLogsQuery 登记 containers.logs：读一页容器日志（从新到旧）。
func newContainersLogsQuery(src ContainersSource) Query {
	return Query{
		Name: ContainersLogsQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params containers.LogParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if err := params.Validate(time.Now().UTC()); err != nil {
				// 参数错误是调用方的问题，必须是 400：代理回 500 会被 Web 当成
				// “代理不可用”，把用户的参数写错报成 503。
				return nil, &containers.ValidationError{Message: err.Error()}
			}
			return params, nil
		},
		Run: func(ctx context.Context, decoded any) (any, error) {
			return src.Logs(ctx, decoded.(containers.LogParams))
		},
	}
}

// newContainersImagesQuery 登记 containers.images。
func newContainersImagesQuery(src ContainersSource) Query {
	return Query{
		Name: ContainersImagesQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.Images(ctx)
		},
	}
}

// newContainersNetworksQuery 登记 containers.networks。
func newContainersNetworksQuery(src ContainersSource) Query {
	return Query{
		Name: ContainersNetworksQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.Networks(ctx)
		},
	}
}

// newContainersVolumesQuery 登记 containers.volumes。
func newContainersVolumesQuery(src ContainersSource) Query {
	return Query{
		Name: ContainersVolumesQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.Volumes(ctx)
		},
	}
}

// newContainersProjectsQuery 登记 containers.projects（compose 项目只读清单）。
func newContainersProjectsQuery(src ContainersSource) Query {
	return Query{
		Name: ContainersProjectsQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.Projects(ctx)
		},
	}
}
