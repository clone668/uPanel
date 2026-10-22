package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// newStorageVolumesQuery 登记 storage.volumes：无参数，返回全部受管理卷。
//
// 返回的是一个对象 {"volumes": [...]} 而不是裸数组：以后要加 snapshot_version 之类的
// 附注时不必改响应形状（裸数组只能靠再加一层包起来，那是破坏性变更）。
func newStorageVolumesQuery(collector *storage.Collector) Query {
	return Query{
		Name: storage.VolumesQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			volumes, err := collector.Volumes(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{"volumes": volumes}, nil
		},
	}
}

// volumeBlockersParams 是 storage.volume.blockers 的参数。
//
// 用具名类型而不是匿名 struct：Decode 与 Run 之间靠类型断言传递，
// 两处各写一遍匿名 struct 时字段顺序或 tag 差一个字就会在运行期变成“参数类型不匹配”。
type volumeBlockersParams struct {
	VolumeID string `json:"volume_id"`
}

// newStorageVolumeBlockersQuery 登记 storage.volume.blockers：按 volume_id 查占用。
func newStorageVolumeBlockersQuery(collector *storage.Collector) Query {
	return Query{
		Name: storage.VolumeBlockersQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params volumeBlockersParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			params.VolumeID = strings.TrimSpace(params.VolumeID)
			if !storage.ValidVolumeID(params.VolumeID) {
				return nil, fmt.Errorf("%w: volume_id 必须是 vol_ 加 32 位小写十六进制",
					ErrParamsInvalid)
			}
			return params, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(volumeBlockersParams)
			if !ok {
				return nil, fmt.Errorf("%w: 参数类型不匹配: %T", ErrParamsInvalid, params)
			}
			out, err := collector.VolumeBlockers(ctx, p.VolumeID)
			if err != nil {
				// “卷不存在”是 404，不是 500：页面要能说“这条记录已经被删掉了”，
				// 而不是让用户以为代理出了故障。
				if errors.Is(err, storage.ErrVolumeNotFound) {
					return nil, storage.NotFoundError("卷不存在：%s", p.VolumeID)
				}
				return nil, err
			}
			return out, nil
		},
	}
}
