package agent

import (
	"context"
	"encoding/json"

	"upanel/internal/processes"
)

// processesSource 是进程采集器在 agent 层需要的最小接口。
//
// 与 servicesSource 同理：agent 层只关心“取快照 / 预检 / 执行”，这样参数校验与
// 映射可以用假实现验证。生产里唯一的实现是 *processes.Collector。
type processesSource interface {
	Snapshot() processes.Snapshot
	Assess(ctx context.Context, params processes.SignalParams) (processes.Assessment, error)
	ExecuteSignal(ctx context.Context, params processes.SignalParams, progress func(step string, fields map[string]any)) (processes.SignalResult, error)
}

// newProcessesListQuery 登记 processes.list：无参数，返回采集器最近一次快照。
//
// 与 services.list 一致用快照：5 秒一次的采集足够页面刷新，查询不会在每次刷新时
// 扫描整个 /proc。预检与执行走的是现场读取，不受这个间隔影响。
func newProcessesListQuery(src processesSource) Query {
	return Query{
		Name: processes.QueryListName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 没有参数。空结构 + DisallowUnknownFields：任何多余字段
			// （包括 {"command":"kill -9 1"}）都在这里被拒绝。
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(_ context.Context, _ any) (any, error) {
			return src.Snapshot(), nil
		},
	}
}
