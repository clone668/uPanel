package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upanel/internal/monitoring"
	"upanel/internal/testsupport"
)

// fixture 读取 tests/fixtures 下的真实采集样本（见同目录 PROVENANCE.md）。
func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := testsupport.Fixture(name)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return string(raw)
}

// testCollector 用真实夹具内容构造一个采集器，不依赖测试机的 /proc。
func testCollector(t *testing.T) *monitoring.Collector {
	t.Helper()
	paths := monitoring.DefaultPaths()
	return monitoring.NewCollector(monitoring.Options{Reader: stubReader{files: map[string]string{
		paths.Stat:                            fixture(t, "stat-cpu-2404.txt"),
		paths.Meminfo:                         fixture(t, "meminfo-2404.txt"),
		paths.Loadavg:                         fixture(t, "loadavg-2404.txt"),
		paths.NetDev:                          fixture(t, "netdev-2404.txt"),
		paths.OSRelease:                       fixture(t, "os-release-2404.txt"),
		paths.Hostname:                        "upanel-2404\n",
		paths.KernelRelease:                   "6.8.0-71-generic\n",
		paths.BootID:                          "3f6c1d92-0000-4000-8000-000000000001\n",
		paths.Uptime:                          fixture(t, "uptime-2404.txt"),
		paths.CPUInfo:                         "processor\t: 0\nmodel name\t: QEMU Virtual CPU version 2.5+\n",
		paths.SysNetClass + "/lo/operstate":   "unknown\n",
		paths.SysNetClass + "/ens4/operstate": "up\n",
	}}})
}

type summaryEnvelope struct {
	Data monitoring.Summary `json:"data"`
}

func TestSystemSummaryQueryReturnsSnapshot(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Collector: testCollector(t)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"system.summary"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body summaryEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	sum := body.Data
	if sum.CollectedAt.IsZero() {
		t.Fatal("collected_at 缺失：采集时间是这个接口的核心信息")
	}
	if sum.Host.Hostname != "upanel-2404" || sum.Host.OSID != "ubuntu" {
		t.Fatalf("主机段 = %+v", sum.Host)
	}
	if sum.CPU.LogicalCores != 2 {
		t.Fatalf("逻辑核 = %d", sum.CPU.LogicalCores)
	}
	if sum.Memory.TotalBytes == nil || *sum.Memory.TotalBytes != 4009864*1024 {
		t.Fatalf("内存总量 = %v", sum.Memory.TotalBytes)
	}
	if len(sum.Network.Interfaces) != 2 {
		t.Fatalf("网卡 = %+v", sum.Network.Interfaces)
	}
	// 首次采样没有前值：使用率必须是 null，不能是 0。
	if sum.CPU.UsagePercent != nil {
		t.Fatalf("首次采样 usage_percent = %v，应为 null", *sum.CPU.UsagePercent)
	}

	// 查询读的是缓存快照：同一时刻连查两次，采集时间必须一致。
	rec2 := postQuery(t, srv, `{"query":"system.summary"}`, testWebUID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("第二次查询状态码 = %d", rec2.Code)
	}
	var body2 summaryEnvelope
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("第二次响应不是合法 JSON: %v", err)
	}
	if !body2.Data.CollectedAt.Equal(sum.CollectedAt) {
		t.Fatalf("两次查询的 collected_at 不同（%v vs %v）：查询不该每次重采",
			sum.CollectedAt, body2.Data.CollectedAt)
	}
}

func TestSystemSummaryQueryRejectsParams(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Collector: testCollector(t)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	// system.summary 没有参数：带任何字段都是协议错误，包括伪装成命令的字段。
	for _, body := range []string{
		`{"query":"system.summary","params":{"command":"id -u"}}`,
		`{"query":"system.summary","params":{"path":"/etc/shadow"}}`,
	} {
		rec := postQuery(t, srv, body, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", body, rec.Code, rec.Body.String())
		}
	}
}

func TestCapabilitiesListsSystemSummary(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Collector: testCollector(t)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/capabilities", nil), testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var body struct {
		Data struct {
			SupportedActions []string `json:"supported_actions"`
			SupportedQueries []string `json:"supported_queries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got := strings.Join(body.Data.SupportedQueries, ","); got != "system.summary" {
		t.Fatalf("supported_queries = %q", got)
	}
	// 查询上线不代表动作上线：写动作仍然一个都没有。
	if len(body.Data.SupportedActions) != 0 {
		t.Fatalf("supported_actions = %v，P04.1 不应开放任何写动作", body.Data.SupportedActions)
	}
}
