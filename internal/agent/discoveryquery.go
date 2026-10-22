package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"upanel/internal/discovery"
)

// 应用发现与健康探测的查询名（P09.2）。
const (
	// DiscoveryQueryName 返回本机候选列表（监听端口 / systemd 单元 / 容器）。
	DiscoveryQueryName = "apps.discovery"
	// ProbeQueryName 对一个发现出来的目标做一次 HTTP 健康探测。
	ProbeQueryName = "apps.probe"
)

// newDiscoveryQuery 登记 apps.discovery：无参数，返回发现快照。
//
// 返回**快照**：采集按 TTL 缓存在代理里，同一窗口内反复打开候选列表拿到的是同一份
// 结果与同一个采集时间。候选列表是给人确认的清单，不是“每次刷新都换一批”。
func newDiscoveryQuery(collector *discovery.Collector) Query {
	return Query{
		Name: DiscoveryQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 没有参数。空结构 + 拒绝未知字段：{"port": 8080} 这种“顺便指定一下”
			// 的请求会被拒绝，而不是被静默忽略。
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return collector.Snapshot(ctx), nil
		},
	}
}

// newProbeQuery 登记 apps.probe：对目标做一次 HTTP 探测。
//
// 目标地址由代理按 target_id 从自己的发现结果里解析，调用方给不了 host：参数里只有
// 端口与路径（CONTRACTS §8.1 第 3 条的同一原则——探测目标只能来自代理发现）。
func newProbeQuery(collector *discovery.Collector) Query {
	return Query{
		Name: ProbeQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params discovery.ProbeParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			params = params.Normalize()
			if err := params.Validate(); err != nil {
				// 参数不合法是调用方的问题：包成 ErrParamsInvalid，接口层回 400。
				// 压成 500 会让 Web 把它当成“代理不可用”，于是配置错误显示成
				// “代理挂了”。
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return params, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(discovery.ProbeParams)
			if !ok {
				return nil, fmt.Errorf("%w: 探测参数类型不匹配", ErrParamsInvalid)
			}
			return collector.Probe(ctx, p)
		},
	}
}
