package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"upanel/internal/services"
)

// servicesSource 是服务采集器在 agent 层需要的最小接口。
//
// 用接口而不是 *services.Collector：agent 层只关心“取快照 / 读单元 / 预检 / 执行”，
// 这样这一层的参数校验与映射逻辑可以用假实现验证，不必真的有一台 systemd 主机。
// 生产里唯一的实现就是 *services.Collector（见 registry.go）。
type servicesSource interface {
	Snapshot() services.Snapshot
	Unit(ctx context.Context, name string) (services.Unit, bool, error)
	AssessControl(ctx context.Context, unit, op string) (services.Assessment, error)
	ExecuteControl(ctx context.Context, unit, op string, progress func(step string, fields map[string]any)) (services.ControlResult, error)
}

// newServicesListQuery 登记 services.list：无参数，返回采集器最近一次快照。
//
// 快照而不是现场枚举：10 秒一次的采集足够页面刷新，查询不会在每次刷新时拉起
// systemctl；详情页与预检走 services.unit，现场读取。
func newServicesListQuery(src servicesSource) Query {
	return Query{
		Name: services.QueryListName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 这一条没有参数。空结构 + DisallowUnknownFields 让任何多余字段
			// （包括 {"command":"systemctl stop sshd"}）都被拒绝。
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

// servicesUnitParams 是 services.unit 的参数。
type servicesUnitParams struct {
	Unit string `json:"unit"`
}

// newServicesUnitQuery 登记 services.unit：现场读取一个单元（不读缓存）。
func newServicesUnitQuery(src servicesSource) Query {
	return Query{
		Name: services.QueryUnitName,
		Decode: func(raw json.RawMessage) (any, error) {
			var p servicesUnitParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.Unit = strings.TrimSpace(p.Unit)
			if err := services.ValidateUnitName(p.Unit); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(servicesUnitParams)
			if !ok {
				return nil, fmt.Errorf("参数类型不匹配: %T", params)
			}
			unit, found, err := src.Unit(ctx, p.Unit)
			if err != nil {
				return nil, err
			}
			resp := services.UnitResponse{Found: found}
			if found {
				resp.Unit = &unit
			}
			return resp, nil
		},
	}
}
