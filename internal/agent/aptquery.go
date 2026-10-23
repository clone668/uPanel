package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"upanel/internal/apt"
)

// 两条查询（CONTRACTS.md 第 12.1 节）：
//
//	apt.packages        装了什么包、装的哪个版本、APT 说候选是哪个、候选来自哪个 suite
//	system.power_state  这台机器需不需要重启（标志文件 + .pkgs + boot_id）
//
// 它们都只回事实。哪个包算"可升级"、哪个算"安全更新"由面板的 internal/packages 判：
// 那些规则会随发行版策略变，而事实不该跟着规则一起变。
const (
	// AptPackagesQueryName 是已安装包与候选版本的查询。
	AptPackagesQueryName = "apt.packages"
	// PowerStateQueryName 是重启标志查询。
	PowerStateQueryName = "system.power_state"
)

// AptFacts 是代理侧对 APT 事实来源的最小要求。
//
// 用接口而不是 *apt.Collector：验收模式要能换成一份抓下来的真实响应，
// 而生产路径用的仍然是那个会真的去跑 dpkg-query 的实现。
type AptFacts interface {
	// Available 报告这台机器上有没有 dpkg-query。
	Available() bool
	// Packages 返回包事实。fresh=true 绕过缓存。
	Packages(ctx context.Context, fresh bool) (apt.Snapshot, error)
	// PowerState 读重启标志与 boot_id（不缓存）。
	PowerState(ctx context.Context) (apt.PowerState, error)
}

// aptPackagesParams 是 apt.packages 的参数。
//
// fresh=true 时绕过 30 秒快照。为什么要有这个口子：用户刚在终端里 apt-get 完，
// 回到面板点刷新，若拿到 30 秒前的缓存，"没有变化"这句话会让他去做一个基于
// 错误前提的决定。写操作前的判断（P11.2）一律用 fresh=true。
type aptPackagesParams struct {
	Fresh bool `json:"fresh"`
}

// newAptPackagesQuery 登记 apt.packages。
func newAptPackagesQuery(facts AptFacts) Query {
	return Query{
		Name: AptPackagesQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params aptPackagesParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(aptPackagesParams)
			if !ok {
				return nil, fmt.Errorf("%w: apt.packages 参数类型不匹配", ErrParamsInvalid)
			}
			return facts.Packages(ctx, p.Fresh)
		},
	}
}

// newPowerStateQuery 登记 system.power_state（无参数）。
func newPowerStateQuery(facts AptFacts) Query {
	return Query{
		Name: PowerStateQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 无参数查询也走严格解码：多一个字段就拒绝，而不是静默忽略。
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			if _, ok := params.(struct{}); !ok {
				return nil, fmt.Errorf("%w: system.power_state 参数类型不匹配", ErrParamsInvalid)
			}
			return facts.PowerState(ctx)
		},
	}
}
