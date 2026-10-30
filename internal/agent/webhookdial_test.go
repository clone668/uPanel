package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestWebhookIPAllowed(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true,
		"::ffff:8.8.8.8":       true,
		"127.0.0.1":            false,
		"::ffff:127.0.0.1":     false,
		"10.0.0.8":             false,
		"172.16.0.8":           false,
		"192.168.1.8":          false,
		"169.254.169.254":      false,
		"100.64.0.8":           false,
		"192.0.2.8":            false,
		"198.18.0.8":           false,
		"0.0.0.0":              false,
		"224.0.0.1":            false,
		"::1":                  false,
		"fc00::1":              false,
		"fe80::1":              false,
		"2001:db8::1":          false,
	}
	for raw, want := range cases {
		if got := webhookIPAllowed(netip.MustParseAddr(raw)); got != want {
			t.Errorf("webhookIPAllowed(%s) = %v，期望 %v", raw, got, want)
		}
	}
}

func TestDialWebhookRejectsDNSRebindToPrivateAddress(t *testing.T) {
	dialed := false
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	}
	_, err := dialWebhookContext(context.Background(), "hooks.example:443", false, lookup, dial)
	if !errors.Is(err, errWebhookTargetDenied) {
		t.Fatalf("错误 = %v，期望目标策略拒绝", err)
	}
	if dialed {
		t.Fatal("DNS 解析到回环后仍尝试了连接")
	}
}

func TestDialWebhookSkipsPrivateAnswersAndDialsOnlyPublicIP(t *testing.T) {
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("10.0.0.8"),
			netip.MustParseAddr("8.8.8.8"),
		}, nil
	}
	var gotNetwork, gotAddress string
	dial := func(_ context.Context, network, address string) (net.Conn, error) {
		gotNetwork, gotAddress = network, address
		return nil, nil
	}
	if _, err := dialWebhookContext(context.Background(), "hooks.example:443", false, lookup, dial); err != nil {
		t.Fatalf("存在公网答案时不该被私网答案拖死: %v", err)
	}
	if gotNetwork != "tcp" || gotAddress != "8.8.8.8:443" {
		t.Fatalf("实际连接 = %s %s，期望只连公网 IP", gotNetwork, gotAddress)
	}
	if strings.Contains(gotAddress, "10.0.0.8") {
		t.Fatalf("连接了私网地址 %s", gotAddress)
	}
}

func TestDialWebhookAllowsPrivateAddressOnlyWhenExplicitlyEnabled(t *testing.T) {
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	var gotAddress string
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		gotAddress = address
		return nil, nil
	}
	if _, err := dialWebhookContext(context.Background(), "hooks.example:443", true, lookup, dial); err != nil {
		t.Fatalf("显式允许内网后仍被拒绝: %v", err)
	}
	if gotAddress != "127.0.0.1:443" {
		t.Fatalf("实际连接 = %q，期望显式配置的回环目标", gotAddress)
	}
}
