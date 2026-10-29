package agentclient

import (
	"context"
	"encoding/json"
	"fmt"

	"upanel/internal/network"
)

// 网络与防火墙的只读查询（P14.1）。四个方法都只是"把代理给的事实解出来并核对形状"，
// 不做判断、不做缓存：判断在 internal/api，缓存没有——网络状态的变化速度是
// "管理员敲一条 netplan apply"，缓存窗口里的旧地址会被读成"我改的东西没生效"。
//
// 契约（CONTRACTS.md 第 14.1 节）：data 里必须有 collected_at 与对应的列表字段。
// 缺字段时返回 *UnavailableError 而不是给一个空列表——空列表会被读成"没有网卡"。

// NetworkInterfaces 读网卡与地址。
func (c *Client) NetworkInterfaces(ctx context.Context) (network.InterfaceList, error) {
	raw, err := c.Query(ctx, "network.interfaces", nil)
	if err != nil {
		return network.InterfaceList{}, err
	}
	var out network.InterfaceList
	if err := json.Unmarshal(raw, &out); err != nil {
		return network.InterfaceList{}, &UnavailableError{Err: fmt.Errorf("网卡列表响应无法解析: %w", err)}
	}
	if out.CollectedAt == "" {
		return network.InterfaceList{}, &UnavailableError{Err: fmt.Errorf("网卡列表响应缺少 collected_at")}
	}
	return out, nil
}

// NetworkRoutes 读路由表与默认网关。
func (c *Client) NetworkRoutes(ctx context.Context) (network.RouteList, error) {
	raw, err := c.Query(ctx, "network.routes", nil)
	if err != nil {
		return network.RouteList{}, err
	}
	var out network.RouteList
	if err := json.Unmarshal(raw, &out); err != nil {
		return network.RouteList{}, &UnavailableError{Err: fmt.Errorf("路由列表响应无法解析: %w", err)}
	}
	if out.CollectedAt == "" {
		return network.RouteList{}, &UnavailableError{Err: fmt.Errorf("路由列表响应缺少 collected_at")}
	}
	return out, nil
}

// NetworkListeners 读监听端口。
func (c *Client) NetworkListeners(ctx context.Context) (network.ListenerList, error) {
	raw, err := c.Query(ctx, "network.listeners", nil)
	if err != nil {
		return network.ListenerList{}, err
	}
	var out network.ListenerList
	if err := json.Unmarshal(raw, &out); err != nil {
		return network.ListenerList{}, &UnavailableError{Err: fmt.Errorf("监听端口响应无法解析: %w", err)}
	}
	if out.CollectedAt == "" {
		return network.ListenerList{}, &UnavailableError{Err: fmt.Errorf("监听端口响应缺少 collected_at")}
	}
	return out, nil
}

// FirewallState 读防火墙现状。
func (c *Client) FirewallState(ctx context.Context) (network.FirewallState, error) {
	raw, err := c.Query(ctx, "firewall.state", nil)
	if err != nil {
		return network.FirewallState{}, err
	}
	var out network.FirewallState
	if err := json.Unmarshal(raw, &out); err != nil {
		return network.FirewallState{}, &UnavailableError{Err: fmt.Errorf("防火墙状态响应无法解析: %w", err)}
	}
	if out.CollectedAt == "" {
		return network.FirewallState{}, &UnavailableError{Err: fmt.Errorf("防火墙状态响应缺少 collected_at")}
	}
	// manager 是闭集：空串说明代理的响应不完整，"未知"这个词在这里不能靠零值表达。
	if out.Manager == "" {
		return network.FirewallState{}, &UnavailableError{Err: fmt.Errorf("防火墙状态响应缺少 manager")}
	}
	return out, nil
}

// NetworkConfig 读"可改网卡现在是什么样"（P14.2）。
//
// 与另外四条只读查询的区别：它的响应里带着 config_hash 与一条待确认的回滚记录，
// 也就是 network.apply 要带的那两样东西。所以形状检查比别处更严——config_hash 只在
// editable=true 时存在，缺了它，页面会给出一个"看起来能提交、提交必被拒"的表单。
func (c *Client) NetworkConfig(ctx context.Context) (network.ConfigState, error) {
	raw, err := c.Query(ctx, "network.config", nil)
	if err != nil {
		return network.ConfigState{}, err
	}
	var out network.ConfigState
	if err := json.Unmarshal(raw, &out); err != nil {
		return network.ConfigState{}, &UnavailableError{Err: fmt.Errorf("网络配置响应无法解析: %w", err)}
	}
	if out.CollectedAt == "" {
		return network.ConfigState{}, &UnavailableError{Err: fmt.Errorf("网络配置响应缺少 collected_at")}
	}
	// backend 是闭集：空串说明响应不完整，"未知"不能靠零值表达。
	if out.Capability.Backend == "" {
		return network.ConfigState{}, &UnavailableError{Err: fmt.Errorf("网络配置响应缺少 capability.backend")}
	}
	if out.Capability.Editable && out.ConfigHash == "" {
		return network.ConfigState{}, &UnavailableError{Err: fmt.Errorf(
			"网络配置响应说可以改，却没有 config_hash（页面据此提交，缺了它必被拒）")}
	}
	return out, nil
}

// FirewallConfig 读"面板管理的防火墙规则现在是什么样"（P14.3）。
//
// 与 NetworkConfig 同一个口径的形状检查，理由也一样：响应里的 config_hash 是提交
// firewall.apply 时必须原样带回去的指纹，缺了它页面会给出一个"看起来能提交、提交必被拒"
// 的表单。区别是这里的判据挂在 editable 上而不是 capability 上——防火墙没有"部分可改"，
// 只有"UFW 独占且装好了"这一种可以改的情形。
func (c *Client) FirewallConfig(ctx context.Context) (network.FirewallConfigState, error) {
	raw, err := c.Query(ctx, "firewall.config", nil)
	if err != nil {
		return network.FirewallConfigState{}, err
	}
	var out network.FirewallConfigState
	if err := json.Unmarshal(raw, &out); err != nil {
		return network.FirewallConfigState{}, &UnavailableError{Err: fmt.Errorf("防火墙配置响应无法解析: %w", err)}
	}
	if out.CollectedAt == "" {
		return network.FirewallConfigState{}, &UnavailableError{Err: fmt.Errorf("防火墙配置响应缺少 collected_at")}
	}
	// manager 是闭集：空串说明响应不完整，"未知"不能靠零值表达。
	if out.Manager == "" {
		return network.FirewallConfigState{}, &UnavailableError{Err: fmt.Errorf("防火墙配置响应缺少 manager")}
	}
	if out.Editable && out.ConfigHash == "" {
		return network.FirewallConfigState{}, &UnavailableError{Err: fmt.Errorf(
			"防火墙配置响应说可以改，却没有 config_hash（页面据此提交，缺了它必被拒）")}
	}
	// rules 必须是数组而不是 null：null 在页面上会变成一个"没有规则"的列表，
	// 而"没有规则"与"没读到规则"是两件完全不同的事。
	if out.Rules == nil {
		return network.FirewallConfigState{}, &UnavailableError{Err: fmt.Errorf("防火墙配置响应缺少 rules")}
	}
	return out, nil
}
