package deploy

// 面板侧健康探测（P15.3）。
//
// 这里验的是“结论诚不诚实”，不是“HTTP 会不会”：探通了要说通，探不通要说清试了几次、
// 最后一次看到什么，预算不够探满时不许假装探满，启动宽限期内的失败不算失败。

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeHealthProber 是探测能力的假件。
//
// 真的去探一个“永远不健康”的端口要几十秒（定义里的间隔是 30 秒级），而更新流程里
// “探测失败 → 回切”是一条必须验的路径。假件按脚本回答，并把收到的目标记下来：
// 探的是不是**宿主端口**，只能靠看它收到了什么。
type fakeHealthProber struct {
	results []HealthProbe
	targets []HealthTarget
	budgets []time.Duration
	calls   int
}

func (f *fakeHealthProber) Probe(_ context.Context, target HealthTarget, budget time.Duration) HealthProbe {
	f.targets = append(f.targets, target)
	f.budgets = append(f.budgets, budget)
	out := HealthProbe{Checked: true, Healthy: true, Attempts: 1, Address: target.Address, URL: target.URL()}
	if len(f.results) > 0 {
		idx := f.calls
		if idx >= len(f.results) {
			idx = len(f.results) - 1
		}
		out = f.results[idx]
		if out.Address == "" {
			out.Address = target.Address
		}
		if out.URL == "" {
			out.URL = target.URL()
		}
		out.Checked = true
	}
	f.calls++
	return out
}

// TestProbeAddressReplacesWildcard：绑通配地址时探回环。
func TestProbeAddressReplacesWildcard(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"0.0.0.0":    "127.0.0.1",
		"::":         "127.0.0.1",
		"127.0.0.1":  "127.0.0.1",
		"192.0.2.11": "192.0.2.11",
	}
	for bind, want := range cases {
		got, err := probeAddress(bind)
		if want == "" {
			if err == nil {
				t.Fatalf("绑定地址为空时应报错，实际得到 %q", got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("probeAddress(%q) 失败：%v", bind, err)
		}
		if got != want {
			t.Fatalf("probeAddress(%q) = %q，期望 %q", bind, got, want)
		}
	}
}

// TestHealthTargetUsesHostPort：探的是宿主端口，不是容器端口。
//
// 容器端口通不通是 Docker 的事；管理员用的是外面那个入口。
func TestHealthTargetUsesHostPort(t *testing.T) {
	def := Definition{
		HostBindAddress: "0.0.0.0",
		HostPort:        18080,
		ContainerPort:   8080,
		HealthCheck:     HealthCheck{Enabled: true, Kind: HealthCheckHTTP, Path: "/healthz"},
	}
	target, err := healthTargetOf(def)
	if err != nil {
		t.Fatalf("算探测目标失败：%v", err)
	}
	if target.Port != 18080 {
		t.Fatalf("探测端口 = %d，期望宿主端口 18080", target.Port)
	}
	if target.Address != "127.0.0.1" {
		t.Fatalf("探测地址 = %q，期望回环", target.Address)
	}
	if got := target.URL(); got != "http://127.0.0.1:18080/healthz" {
		t.Fatalf("探测 URL = %q", got)
	}
	if target.Retries <= 0 || target.Interval <= 0 || target.Timeout <= 0 {
		t.Fatalf("默认节奏要补齐：%+v", target)
	}

	if _, err := healthTargetOf(Definition{HostPort: 80}); err == nil {
		t.Fatal("没开健康检查时不该算出一个可探的目标")
	}
}

// noSleepProber 造一个“等待零成本”的真探测器（时间真的走，但间隔不等）。
func noSleepProber() DefaultHealthProber {
	return DefaultHealthProber{Sleep: func(context.Context, time.Duration) error { return nil }}
}

// TestDefaultProberHTTPHealthy：真打一次 httptest 服务器。
func TestDefaultProberHTTPHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	addr, port := splitHostPort(t, server.URL)

	probe := noSleepProber().Probe(context.Background(), HealthTarget{
		Kind: HealthCheckHTTP, Address: addr, Port: port, Path: "/",
		Timeout: 2 * time.Second, Interval: 10 * time.Millisecond, Retries: 2,
	}, 5*time.Second)
	if !probe.Checked || !probe.Healthy {
		t.Fatalf("应探通：%+v", probe)
	}
	if probe.Attempts != 1 {
		t.Fatalf("第一次就通了，不该重试：%+v", probe)
	}
	if probe.StatusCode == nil || *probe.StatusCode != http.StatusNoContent {
		t.Fatalf("状态码 = %v", probe.StatusCode)
	}
	if probe.URL == "" || probe.Address != addr {
		t.Fatalf("探测地址没记下来：%+v", probe)
	}
}

// TestDefaultProberHTTPStatusFailure：状态码不在 200—399 就是没探通，要记下码与次数。
func TestDefaultProberHTTPStatusFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	addr, port := splitHostPort(t, server.URL)

	probe := noSleepProber().Probe(context.Background(), HealthTarget{
		Kind: HealthCheckHTTP, Address: addr, Port: port, Path: "/",
		Timeout: 2 * time.Second, Interval: 10 * time.Millisecond, Retries: 2,
	}, 5*time.Second)
	if probe.Healthy {
		t.Fatalf("502 不该算通：%+v", probe)
	}
	if probe.Attempts != 3 {
		t.Fatalf("重试 2 次共 3 次尝试，实际 %d", probe.Attempts)
	}
	if probe.StatusCode == nil || *probe.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %v", probe.StatusCode)
	}
	if !strings.Contains(probe.LastError, "502") {
		t.Fatalf("最后一次失败原因 = %q", probe.LastError)
	}
}

// TestDefaultProberFollowsNoRedirect：3xx 不跟随（避免一次探测变成对别处的请求）。
func TestDefaultProberFollowsNoRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/away" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/away", http.StatusFound)
	}))
	defer server.Close()
	addr, port := splitHostPort(t, server.URL)

	probe := noSleepProber().Probe(context.Background(), HealthTarget{
		Kind: HealthCheckHTTP, Address: addr, Port: port, Path: "/",
		Timeout: 2 * time.Second, Interval: 10 * time.Millisecond, Retries: 0,
	}, 5*time.Second)
	if probe.StatusCode == nil || *probe.StatusCode != http.StatusFound {
		t.Fatalf("应记下 302 本身，而不是跟到 /away：%+v", probe.StatusCode)
	}
}

// TestDefaultProberTCPUnhealthy：连不上的端口要说清连不上。
func TestDefaultProberTCPUnhealthy(t *testing.T) {
	port := closedPort(t)
	probe := noSleepProber().Probe(context.Background(), HealthTarget{
		Kind: HealthCheckTCP, Address: "127.0.0.1", Port: port,
		Timeout: 500 * time.Millisecond, Interval: 10 * time.Millisecond, Retries: 1,
	}, 5*time.Second)
	if probe.Healthy {
		t.Fatalf("空端口不该算通：%+v", probe)
	}
	if probe.Attempts != 2 {
		t.Fatalf("尝试次数 = %d，期望 2", probe.Attempts)
	}
	if !strings.Contains(probe.LastError, "连不上") {
		t.Fatalf("失败原因 = %q", probe.LastError)
	}
}

// TestDefaultProberTruncatesByBudget：预算不够按定义探满时如实说明截断。
func TestDefaultProberTruncatesByBudget(t *testing.T) {
	port := closedPort(t)
	probe := noSleepProber().Probe(context.Background(), HealthTarget{
		Kind: HealthCheckTCP, Address: "127.0.0.1", Port: port,
		Timeout: 200 * time.Millisecond, Interval: time.Minute, Retries: 3,
	}, 2*time.Second)
	if !probe.Truncated {
		t.Fatalf("预算(2s)不够等一次间隔(1min)，必须标截断：%+v", probe)
	}
	if probe.Attempts != 1 {
		t.Fatalf("截断后只该试过 1 次，实际 %d", probe.Attempts)
	}
}

// TestDefaultProberStartPeriodRespected：启动宽限期内的失败不算失败。
func TestDefaultProberStartPeriodRespected(t *testing.T) {
	port := closedPort(t)
	probe := noSleepProber().Probe(context.Background(), HealthTarget{
		Kind: HealthCheckTCP, Address: "127.0.0.1", Port: port,
		Timeout: 200 * time.Millisecond, Interval: 10 * time.Millisecond, Retries: 1,
		StartPeriod: time.Hour,
	}, 5*time.Second)
	if !probe.StartPeriodRespected || !probe.Healthy {
		t.Fatalf("宽限期内的失败不该判不健康：%+v", probe)
	}
	if probe.Attempts != 2 {
		t.Fatalf("仍然要如实记下试过几次：%+v", probe)
	}
}

// TestDefaultProberUnknownKind：未知探测类型是“连目标都拼不出来”，要报错而不是瞎探。
func TestDefaultProberUnknownKind(t *testing.T) {
	probe := noSleepProber().Probe(context.Background(), HealthTarget{
		Kind: "grpc", Address: "127.0.0.1", Port: 1, Timeout: 100 * time.Millisecond, Retries: 0,
	}, time.Second)
	if probe.Healthy {
		t.Fatalf("未知类型不该算健康：%+v", probe)
	}
	if !strings.Contains(probe.LastError, "未知的探测类型") {
		t.Fatalf("失败原因 = %q", probe.LastError)
	}
}

// splitHostPort 从 httptest 的 URL 里拆出地址与端口。
func splitHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	trimmed := strings.TrimPrefix(raw, "http://")
	host, portText, err := net.SplitHostPort(trimmed)
	if err != nil {
		t.Fatalf("拆地址 %q 失败：%v", raw, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("端口 %q 不是数字：%v", portText, err)
	}
	return host, port
}

// closedPort 返回一个刚刚关掉的端口（用来造“连不上”的现场）。
func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("找空闲端口失败：%v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("关监听失败：%v", err)
	}
	return port
}
