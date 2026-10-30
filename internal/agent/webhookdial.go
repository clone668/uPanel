package agent

// Webhook 的出站边界。
//
// URL 形状合法不代表目标安全：主机名可以在保存后解析到回环或私网地址，DNS 答案
// 也可能在两次查询之间变化。因此策略放在每次实际连接之前，并逐个检查选中的 IP；
// 禁止重定向只能挡住其中一条绕路，不能替代这里的检查。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

var errWebhookTargetDenied = errors.New("webhook 目标不是允许的公网地址")

type webhookLookupFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)
type webhookDialFunc func(ctx context.Context, network, address string) (net.Conn, error)

var webhookDeniedPrefixes = []netip.Prefix{
	// 共享地址空间与运营商/设备侧特殊网段仍可能通向内网服务。
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// 文档与基准测试地址不应成为真实投递目标。
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2001:2::/48"),
}

func webhookIPAllowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() ||
		addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range webhookDeniedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// dialWebhookContext 解析并连接一个明确通过策略的 IP。lookup 与 dial 是参数，
// 让 DNS 重绑定和“校验后实际连了哪个地址”可以用不访问网络的测试固定下来。
// allowPrivate 是单通道的管理员显式授权；它只放宽目标网段，不改变 URL 与 TLS 校验。
func dialWebhookContext(
	ctx context.Context,
	address string,
	allowPrivate bool,
	lookup webhookLookupFunc,
	dial webhookDialFunc,
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("webhook 地址无效: %w", err)
	}

	var candidates []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		candidates = []netip.Addr{addr}
	} else {
		candidates, err = lookup(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("解析 webhook 主机失败: %w", err)
		}
	}

	var lastDialErr error
	for _, candidate := range candidates {
		candidate = candidate.Unmap()
		if !allowPrivate && !webhookIPAllowed(candidate) {
			continue
		}
		conn, err := dial(ctx, "tcp", net.JoinHostPort(candidate.String(), port))
		if err == nil {
			return conn, nil
		}
		lastDialErr = err
	}
	if lastDialErr != nil {
		return nil, lastDialErr
	}
	return nil, fmt.Errorf("%w: %s", errWebhookTargetDenied, host)
}
