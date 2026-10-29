package api

// 网络与防火墙只读接口（P14.1）。
//
// 断言的重点不是"能返回 200"，而是五件事：
//  1. 权限：整组 admin——viewer 连网卡清单都拿不到，未登录 401；
//  2. 没有读取端时回 503，**不是**空列表或空规则表——空表会被读成"这台机器没有网卡"；
//  3. 代理不可用是 503（等一会儿会好），不是 500（面板的错）；
//  4. "能不能改"的判定原样透传：capability.editable 与 reasons 是代理算的，
//     Web 不改写、不补充、不把它翻译成"已支持"；
//  5. issues[] 是数据的一部分：某一条来源读不到时接口仍然是 200，
//     空列表与"读不到"在响应里能分开。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/network"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// fakeNetworkReader 是 NetworkReader 的测试替身：只回放摆好的结果。
type fakeNetworkReader struct {
	ifaces    network.InterfaceList
	ifacesErr error

	routes    network.RouteList
	routesErr error

	listeners    network.ListenerList
	listenersErr error

	firewall    network.FirewallState
	firewallErr error

	config    network.ConfigState
	configErr error

	firewallConfig    network.FirewallConfigState
	firewallConfigErr error

	calls int
}

func (f *fakeNetworkReader) NetworkInterfaces(context.Context) (network.InterfaceList, error) {
	f.calls++
	return f.ifaces, f.ifacesErr
}

func (f *fakeNetworkReader) NetworkRoutes(context.Context) (network.RouteList, error) {
	f.calls++
	return f.routes, f.routesErr
}

func (f *fakeNetworkReader) NetworkListeners(context.Context) (network.ListenerList, error) {
	f.calls++
	return f.listeners, f.listenersErr
}

func (f *fakeNetworkReader) FirewallState(context.Context) (network.FirewallState, error) {
	f.calls++
	return f.firewall, f.firewallErr
}

func (f *fakeNetworkReader) NetworkConfig(context.Context) (network.ConfigState, error) {
	f.calls++
	return f.config, f.configErr
}

func (f *fakeNetworkReader) FirewallConfig(context.Context) (network.FirewallConfigState, error) {
	f.calls++
	return f.firewallConfig, f.firewallConfigErr
}

// networkPaths 是这一组的四个路径，权限与降级断言都跑同一张表。
var networkPaths = []string{
	"/api/v1/network/interfaces",
	"/api/v1/network/routes",
	"/api/v1/network/listeners",
	"/api/v1/firewall",
	// P14.2：写路径的入口状态。它和上面四条同一组、同一权限——因为它也不改任何字节。
	"/api/v1/network/config",
	// P14.3：防火墙写路径的入口状态，同理。
	"/api/v1/firewall/config",
}

func newNetworkFixture(t *testing.T, reader NetworkReader) *adminFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:     config.ListenProduction,
		Mode:              platform.ModeProduction,
		Version:           version.Info{Version: "test", GoVersion: "test"},
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        newTestOperations(t, db, nil),
		Apps:              newTestApps(t, db),
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
		Network:           reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// sampleInterfaces 是一台 netplan + networkd 的单网卡机器，可改。
func sampleInterfaces() network.InterfaceList {
	carrier := true
	mtu := 1500
	return network.InterfaceList{
		CollectedAt: "2026-10-26T12:00:00Z",
		Interfaces: []network.Interface{{
			Name:     "ens4",
			Index:    2,
			Kind:     network.KindEthernet,
			State:    "up",
			MTU:      mtu,
			MAC:      "52:54:00:aa:bb:cc",
			Physical: true,
			Carrier:  &carrier,
			Addresses: []network.Address{{
				Family: network.FamilyIPv4, Address: "192.168.1.10", PrefixLen: 24, Scope: "global",
			}},
		}},
		Capability: network.Capability{
			Backend:  network.BackendNetplanNetworkd,
			Editable: true,
			Target:   "ens4",
			Reasons:  []string{},
		},
		Issues: []network.Issue{},
	}
}

// TestNetworkRequireAdmin：整组 admin，未登录 401、viewer 403。
func TestNetworkRequireAdmin(t *testing.T) {
	f := newNetworkFixture(t, &fakeNetworkReader{ifaces: sampleInterfaces()})

	for _, path := range networkPaths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未登录 %s 状态码 = %d，期望 401", path, rec.Code)
		}
	}

	f.createUser(t, "alice", "viewer", "alice-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "alice", "alice-horse-9")
	for _, path := range networkPaths {
		rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, path, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s 状态码 = %d，期望 403", path, rec.Code)
		}
	}
}

// TestNetworkUnavailableWithoutReader：没接代理时 503，不是空表。
func TestNetworkUnavailableWithoutReader(t *testing.T) {
	f := newNetworkFixture(t, nil)
	for _, path := range networkPaths {
		rec := f.do(t, http.MethodGet, path, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503", path, rec.Code)
		}
		if code := errorCode(t, rec); code != string(httpx.CodeAgentUnavailable) {
			t.Fatalf("%s 错误码 = %q，期望 AGENT_UNAVAILABLE", path, code)
		}
		// 一份空网卡列表会被读成"这台机器没有网卡"，那是不一样的话。
		if strings.Contains(rec.Body.String(), "interfaces") ||
			strings.Contains(rec.Body.String(), "rules") {
			t.Fatalf("%s 的 503 不该带一份空表: %s", path, rec.Body.String())
		}
	}
}

// TestNetworkAgentUnavailableIs503：代理连不上是 503，不是 500。
func TestNetworkAgentUnavailableIs503(t *testing.T) {
	err := &agentclient.UnavailableError{Err: errors.New("dial unix: connect: no such file or directory")}
	f := newNetworkFixture(t, &fakeNetworkReader{
		ifacesErr: err, routesErr: err, listenersErr: err, firewallErr: err, configErr: err,
		firewallConfigErr: err,
	})
	for _, path := range networkPaths {
		rec := f.do(t, http.MethodGet, path, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503", path, rec.Code)
		}
		if code := errorCode(t, rec); code != string(httpx.CodeAgentUnavailable) {
			t.Fatalf("%s 错误码 = %q，期望 AGENT_UNAVAILABLE", path, code)
		}
	}
}

// TestNetworkInterfacesPassesCapabilityThrough：能不能改是代理算的，Web 不改写。
func TestNetworkInterfacesPassesCapabilityThrough(t *testing.T) {
	f := newNetworkFixture(t, &fakeNetworkReader{ifaces: sampleInterfaces()})
	rec := f.do(t, http.MethodGet, "/api/v1/network/interfaces", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out network.InterfaceList
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("响应无法解析: %v", err)
	}
	if out.CollectedAt != "2026-10-26T12:00:00Z" {
		t.Errorf("collected_at 应原样透传，得到 %q", out.CollectedAt)
	}
	if len(out.Interfaces) != 1 || out.Interfaces[0].Name != "ens4" {
		t.Fatalf("网卡列表读错：%+v", out.Interfaces)
	}
	if !out.Capability.Editable || out.Capability.Target != "ens4" {
		t.Errorf("capability 应原样透传：%+v", out.Capability)
	}
	if out.Capability.Backend != network.BackendNetplanNetworkd {
		t.Errorf("backend 应是 netplan+networkd，得到 %q", out.Capability.Backend)
	}
	// reasons 必须是 []，不能是 null：null 在界面上会变成"没原因也不可改"。
	if out.Capability.Reasons == nil {
		t.Error("capability.reasons 不该是 null")
	}
}

// TestNetworkIssuesAreData：某条来源读不到时仍是 200，issues 说明是哪一条。
func TestNetworkIssuesAreData(t *testing.T) {
	list := sampleInterfaces()
	list.Issues = []network.Issue{{
		Source: "ip", Code: network.IssueMissing,
		Detail: "跑 ip 失败：network: 程序不可用",
	}}
	list.Capability = network.Capability{
		Backend:  network.BackendNetplanNetworkd,
		Editable: false,
		Reasons:  []string{"找不到物理以太网卡，没有可改的目标"},
	}
	f := newNetworkFixture(t, &fakeNetworkReader{ifaces: list})

	rec := f.do(t, http.MethodGet, "/api/v1/network/interfaces", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（部分来源读不到不算接口失败）", rec.Code)
	}
	var out network.InterfaceList
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("响应无法解析: %v", err)
	}
	if len(out.Issues) != 1 || out.Issues[0].Code != network.IssueMissing {
		t.Fatalf("issues 应原样带出来：%+v", out.Issues)
	}
	if out.Capability.Editable {
		t.Error("读不到地址时不能可改")
	}
	if len(out.Capability.Reasons) == 0 {
		t.Error("不可改必须给出原因，不能只留一个 editable=false")
	}
}

// TestFirewallShapeHolds：防火墙响应里 manager 是闭集、rules 不为 null。
func TestFirewallShapeHolds(t *testing.T) {
	f := newNetworkFixture(t, &fakeNetworkReader{firewall: network.FirewallState{
		CollectedAt:     "2026-10-26T12:00:00Z",
		Manager:         network.ManagerUFW,
		ManagerReasons:  []string{"发现 ufw，且没有别的防火墙管理器在"},
		Installed:       true,
		Enabled:         true,
		DefaultIncoming: "deny",
		DefaultOutgoing: "allow",
		DefaultRouted:   "disabled",
		Rules: []network.FirewallRule{{
			Number: 1, To: "22/tcp", Action: "ALLOW", Direction: "IN", From: "Anywhere",
		}},
		Editable: true,
		Reasons:  []string{},
		Issues:   []network.Issue{},
	}})

	rec := f.do(t, http.MethodGet, "/api/v1/firewall", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out network.FirewallState
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("响应无法解析: %v", err)
	}
	if out.Manager != network.ManagerUFW || !out.Enabled || !out.Editable {
		t.Errorf("防火墙状态读错：%+v", out)
	}
	if len(out.Rules) != 1 || out.Rules[0].To != "22/tcp" {
		t.Errorf("规则读错：%+v", out.Rules)
	}
	if out.Rules == nil {
		t.Error("rules 不该是 null")
	}
}

// TestNetworkConfigPassesThrough：写路径的入口状态原样透传。
//
// 断言的三样东西是页面唯一的依据：能不能改（capability）、提交时要带的 config_hash、
// 有没有一条还没确认的回滚（pending，带剩余秒数）。任何一样被 Web 改写，页面就会给出
// 一个提交必被拒的表单，或者干脆不知道机器还悬在回滚窗口里。
func TestNetworkConfigPassesThrough(t *testing.T) {
	cfg := network.ConfigState{
		CollectedAt:  "2026-10-26T12:00:00Z",
		Capability:   network.Capability{Backend: network.BackendNetplanNetworkd, Editable: true, Target: "ens4", Reasons: []string{}},
		Interface:    "ens4",
		PermanentMAC: "52:54:00:aa:bb:cc",
		ConfigHash:   "sha256:abc123",
		Mode:         "static",
		IPv4:         &network.IPv4Setting{Address: "192.168.1.10", PrefixLength: 24, Gateway: "192.168.1.1"},
		Unsupported:  []string{},
		Pending: &network.RollbackView{
			Token: "nrb_0123456789abcdef0123456789abcdef", Interface: "ens4", Mode: "static",
			CandidateAddress: "192.168.1.50", AppliedAt: "2026-10-26T12:00:00Z",
			Deadline: "2026-10-26T12:02:00Z", SecondsLeft: 87, State: "pending",
		},
		Issues: []network.Issue{},
	}
	f := newNetworkFixture(t, &fakeNetworkReader{config: cfg})

	rec := f.do(t, http.MethodGet, "/api/v1/network/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out network.ConfigState
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("响应无法解析: %v", err)
	}
	if out.ConfigHash != cfg.ConfigHash {
		t.Errorf("config_hash 应原样透传，得到 %q", out.ConfigHash)
	}
	if !out.Capability.Editable || out.Capability.Target != "ens4" {
		t.Errorf("capability 应原样透传：%+v", out.Capability)
	}
	if out.Pending == nil || out.Pending.SecondsLeft != 87 || out.Pending.State != "pending" {
		t.Fatalf("待确认的回滚应原样透传：%+v", out.Pending)
	}
	if out.Pending.CandidateAddress != "192.168.1.50" {
		t.Errorf("候选地址应原样透传：%+v", out.Pending)
	}
}

// TestFirewallConfigPassesThrough：面板管理的规则集、指纹与待确认的那条回滚
// 全部原样透传（P14.3）。Web 不补、不改写、不把"读不到"翻译成"没有规则"。
func TestFirewallConfigPassesThrough(t *testing.T) {
	cfg := network.FirewallConfigState{
		CollectedAt: "2026-10-26T12:00:00Z",
		Manager:     "ufw",
		Reasons:     []string{},
		Installed:   true,
		Enabled:     true,
		Editable:    true,
		Rules: []network.ManagedRule{
			{ID: "ssh", Action: network.RuleAllow, Port: 22, Proto: network.ProtoTCP},
			{ID: "web", Action: network.RuleAllow, Port: 443, Proto: network.ProtoTCP, From: "10.0.0.0/8"},
		},
		ConfigHash: "sha256:fw123",
		Pending: &network.FirewallPending{
			Token: "frb_0123456789abcdef0123456789abcdef", Deadline: "2026-10-26T12:02:00Z",
			SecondsLeft: 87, Previous: "退回到：未启用，面板管理的规则为空",
		},
		Issues: []network.Issue{},
	}
	f := newNetworkFixture(t, &fakeNetworkReader{firewallConfig: cfg})

	rec := f.do(t, http.MethodGet, "/api/v1/firewall/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out network.FirewallConfigState
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("响应无法解析: %v", err)
	}
	if out.ConfigHash != cfg.ConfigHash || !out.Editable || !out.Enabled {
		t.Errorf("指纹与两张布尔应原样透传：%+v", out)
	}
	if len(out.Rules) != 2 || out.Rules[1].From != "10.0.0.0/8" {
		t.Fatalf("规则集应原样透传：%+v", out.Rules)
	}
	if out.Pending == nil || out.Pending.SecondsLeft != 87 {
		t.Fatalf("待确认的回滚应原样透传：%+v", out.Pending)
	}
	if out.Pending.Previous == "" {
		t.Error("待确认记录必须带上「退回到什么样」，否则页面只能让人确认一个看不见的改动")
	}
}

// TestFirewallConfigUnavailableWithoutReader：没有代理连接时 503，
// 不给一份"看起来能改"的空规则表。
func TestFirewallConfigUnavailableWithoutReader(t *testing.T) {
	f := newNetworkFixture(t, nil)
	rec := f.do(t, http.MethodGet, "/api/v1/firewall/config", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if code := errorCode(t, rec); code != string(httpx.CodeAgentUnavailable) {
		t.Fatalf("错误码 = %q，期望 AGENT_UNAVAILABLE", code)
	}
	if strings.Contains(rec.Body.String(), "rules") {
		t.Fatalf("503 不该带一份空规则表: %s", rec.Body.String())
	}
}
