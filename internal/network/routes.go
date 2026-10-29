package network

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Routes 采集路由表（IPv4 与 IPv6 各查一次）。
//
// 两个家族分开查而不是查一次，是因为 `ip route show` 默认只给 IPv4：把 IPv6 的默认
// 网关漏掉，页面上会显示"这台机器没有默认网关"，而它其实走的是 IPv6——那种错会让
// 管理员去改本不该动的 IPv4 配置。
func Routes(ctx context.Context, deps Deps) RouteList {
	out := RouteList{
		CollectedAt: deps.now().Format(time.RFC3339),
		Routes:      []Route{},
		Gateways:    []Gateway{},
		Issues:      []Issue{},
	}
	for _, family := range []string{FamilyIPv4, FamilyIPv6} {
		routes, issues := readRoutes(ctx, deps, family)
		out.Issues = append(out.Issues, issues...)
		out.Routes = append(out.Routes, routes...)
	}

	// 排序固定在代码里：先默认路由，再按家族、目标、优先级。页面顺序不跟着 `ip`
	// 的输出顺序走——两次刷新顺序不同会让人以为路由变了。
	sort.SliceStable(out.Routes, func(i, j int) bool {
		a, b := out.Routes[i], out.Routes[j]
		if a.Default != b.Default {
			return a.Default
		}
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		return a.Metric < b.Metric
	})

	seen := map[string]bool{}
	for _, route := range out.Routes {
		if !route.Default || route.Gateway == "" {
			continue
		}
		key := route.Family + "|" + route.Gateway + "|" + route.Device
		if seen[key] {
			continue
		}
		seen[key] = true
		out.Gateways = append(out.Gateways, Gateway{
			Family: route.Family, Address: route.Gateway, Device: route.Device, Metric: route.Metric,
		})
	}
	return out
}

// ipRoute 是 `ip -j route show` 的一个元素。
type ipRoute struct {
	Dst      string `json:"dst"`
	Gateway  string `json:"gateway"`
	Dev      string `json:"dev"`
	Protocol string `json:"protocol"`
	Scope    string `json:"scope"`
	PrefSrc  string `json:"prefsrc"`
	Metric   *int   `json:"metric"`
}

func readRoutes(ctx context.Context, deps Deps, family string) ([]Route, []Issue) {
	if deps.Runner == nil {
		return nil, []Issue{{Source: "ip", Code: IssueMissing, Detail: "没有配置命令执行端"}}
	}
	args := []string{"-j"}
	if family == FamilyIPv6 {
		args = append(args, "-6")
	}
	args = append(args, "route", "show")

	runCtx, cancel := context.WithTimeout(ctx, deps.timeout())
	defer cancel()
	stdout, code, err := deps.Runner.Run(runCtx, "ip", args...)
	if err != nil {
		return nil, []Issue{{Source: "ip", Code: issueCode(err),
			Detail: "跑 ip route 失败（" + family + "）：" + err.Error()}}
	}
	if code != 0 {
		return nil, []Issue{{Source: "ip", Code: IssueFailed,
			Detail: fmt.Sprintf("ip route show（%s）退出码 %d", family, code)}}
	}
	var raw []ipRoute
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, []Issue{{Source: "ip", Code: IssueUnparsable,
			Detail: "ip route 的输出不是预期 JSON（" + family + "）：" + err.Error()}}
	}

	routes := make([]Route, 0, len(raw))
	for _, item := range raw {
		route := Route{
			Family:   family,
			Gateway:  item.Gateway,
			Device:   item.Dev,
			Protocol: item.Protocol,
			Scope:    item.Scope,
		}
		if item.Dst == "" || item.Dst == "default" {
			route.Default = true
			if family == FamilyIPv6 {
				route.Destination = "::/0"
			} else {
				route.Destination = "0.0.0.0/0"
			}
		} else if strings.Contains(item.Dst, "/") {
			route.Destination = item.Dst
		} else {
			// 主机路由在 `ip -j route` 里只给地址不给前缀（`10.0.2.2`、`::1`），
			// 而契约说 destination 一律写成网段：一个字段两种写法，读的人就得记住
			// "没有斜杠时按单地址解释"。补齐 /32 与 /128，让解析方只处理一种形状。
			if family == FamilyIPv6 {
				route.Destination = item.Dst + "/128"
			} else {
				route.Destination = item.Dst + "/32"
			}
		}
		// metric 缺省是"内核没写"，不是 0：0 也是一个合法的优先级。
		if item.Metric != nil {
			route.Metric = *item.Metric
			route.PrefSet = true
		}
		routes = append(routes, route)
	}
	return routes, nil
}
