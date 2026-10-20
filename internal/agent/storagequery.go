package agent

import (
	"context"
	"encoding/json"

	"upanel/internal/storage"
)

// newStorageTopologyQuery 登记 storage.topology：无参数，返回代理最近一次扫描的
// 块设备拓扑快照（设备、关系、版本、采集时间）。
//
// 返回**快照**而不是现场扫描：扫描按 5 秒循环在后台跑，查询只读缓存。
// 这样同一窗口内刷新页面拿到的是同一份拓扑与同一个 snapshot_version，
// 不会出现“两次刷新看到不同的设备集合”，P07 的预检也才能拿版本做一致性判断。
func newStorageTopologyQuery(collector *storage.Collector) Query {
	return Query{
		Name: storage.QueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 这条查询没有参数。空结构 + DisallowUnknownFields 让任何多余字段
			// （例如 {"device":"sda","action":"format"}）被拒绝，而不是被静默忽略。
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return collector.Topology(ctx), nil
		},
	}
}
