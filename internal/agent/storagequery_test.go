package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"upanel/internal/storage"
	"upanel/internal/testsupport"
)

// testStorageCollector 用**真实采集的 sysfs/proc 夹具**构造采集器，
// 不读测试机的 /sys。夹具来自 24.04 测试机（见 docs/evidence/P04）。
func testStorageCollector(t *testing.T) *storage.Collector {
	t.Helper()
	dir, err := testsupport.FixtureDir("storage-2404")
	if err != nil {
		t.Fatalf("%v", err)
	}
	return storage.NewCollector(storage.Options{Root: storage.Root{
		Sys:   filepath.Join(dir, "sys"),
		Proc:  filepath.Join(dir, "proc"),
		Disks: fixtureDiskReader{dir: filepath.Join(dir, "disks")},
		// Caps 为 nil：夹具目录不是真实文件系统，容量留 null。
	}})
}

// fixtureDiskReader 读夹具里的整盘头部（等价于真实 /dev/sdX 的前 34 个扇区）。
type fixtureDiskReader struct{ dir string }

func (f fixtureDiskReader) ReadDisk(_ context.Context, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.dir, name+".bin"))
}

type topologyEnvelope struct {
	Data storage.Topology `json:"data"`
}

func TestStorageTopologyQueryReturnsSnapshot(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Storage: testStorageCollector(t)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"storage.topology"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body topologyEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	topo := body.Data
	if topo.Status != storage.StatusOK {
		t.Fatalf("status = %q（%s），期望 ok", topo.Status, topo.Error)
	}
	if topo.CollectedAt.IsZero() {
		t.Fatal("collected_at 缺失：采集时间是这个接口的核心信息")
	}
	if topo.SnapshotVersion == "" {
		t.Fatal("snapshot_version 缺失")
	}
	if topo.IntervalMS <= 0 {
		t.Fatalf("interval_ms = %d", topo.IntervalMS)
	}
	if len(topo.Nodes) == 0 {
		t.Fatal("没有采集到任何设备")
	}

	// 真实夹具里 sda 整盘必须被判成系统盘：它承载根文件系统。
	var found bool
	for _, d := range topo.Nodes {
		if d.ID == "sda" {
			found = true
			if d.Kind != storage.KindDisk {
				t.Errorf("sda kind = %q", d.Kind)
			}
			if !d.Protection.SystemDisk {
				t.Errorf("sda 没被标成系统盘")
			}
		}
	}
	if !found {
		t.Fatalf("拓扑里没有 sda：%v", topo.Nodes)
	}

	// 连查两次必须拿到同一份快照：查询不重扫，版本与采集时间都不变。
	rec2 := postQuery(t, srv, `{"query":"storage.topology"}`, testWebUID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("第二次查询状态码 = %d", rec2.Code)
	}
	var body2 topologyEnvelope
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("第二次响应不是合法 JSON: %v", err)
	}
	if !body2.Data.CollectedAt.Equal(topo.CollectedAt) {
		t.Fatalf("两次查询的 collected_at 不同（%v vs %v）：查询不该每次重扫",
			topo.CollectedAt, body2.Data.CollectedAt)
	}
	if body2.Data.SnapshotVersion != topo.SnapshotVersion {
		t.Fatalf("两次查询的 snapshot_version 不同（%s vs %s）",
			topo.SnapshotVersion, body2.Data.SnapshotVersion)
	}
}

func TestStorageTopologyQueryRejectsParams(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Storage: testStorageCollector(t)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	// 这条查询没有参数：带任何字段都是协议错误，包括伪装成命令/动作的字段。
	for _, body := range []string{
		`{"query":"storage.topology","params":{"device":"sda"}}`,
		`{"query":"storage.topology","params":{"action":"format"}}`,
		`{"query":"storage.topology","params":{"command":"lsblk"}}`,
	} {
		rec := postQuery(t, srv, body, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", body, rec.Code, rec.Body.String())
		}
	}
}

func TestCapabilitiesListsStorageTopology(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{
		Collector: testCollector(t),
		Storage:   testStorageCollector(t),
	})
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
	if got := strings.Join(body.Data.SupportedQueries, ","); got != "storage.topology,system.summary" {
		t.Fatalf("supported_queries = %q", got)
	}
	// 跨阶段不变量：supported_actions 里只能出现 storage 那四个固定写动作中**已经有
	// 执行器**的那些。P04.2 时它是空的；P07.2 起是三个分区动作；格式化属 P07.3。
	// 这条断言不钉“某个阶段有哪几个”，而是钉“不许出现计划外的名字，也不许报出没有
	// 执行器的动作”——阶段推进时只需要改 expects 这一处。
	allowed := map[string]bool{
		storage.ActionPartitionTableInitialize: true,
		storage.ActionPartitionCreate:          true,
		storage.ActionPartitionDelete:          true,
		storage.ActionFilesystemFormat:         true,
	}
	for _, name := range body.Data.SupportedActions {
		if !allowed[name] && strings.HasPrefix(name, "storage.") {
			t.Errorf("supported_actions 里出现了计划外的存储动作 %s", name)
		}
	}
	// 已经有执行器的三个必须报出来，否则前端不会显示入口。
	got := map[string]bool{}
	for _, name := range body.Data.SupportedActions {
		got[name] = true
	}
	for _, name := range []string{
		storage.ActionPartitionTableInitialize,
		storage.ActionPartitionCreate,
		storage.ActionPartitionDelete,
	} {
		if !got[name] {
			t.Errorf("supported_actions 里缺少已有执行器的 %s：%v", name, body.Data.SupportedActions)
		}
	}
}

func TestStorageQueryNotRegisteredWithoutCollector(t *testing.T) {
	// 没有存储采集器就不登记 storage.topology：capabilities 不能报告一条永远失败的查询。
	reg, err := NewProductionRegistry(ProductionDeps{Collector: testCollector(t)})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if _, ok := reg.Query(storage.QueryName); ok {
		t.Fatalf("没有采集器却登记了 %s", storage.QueryName)
	}
}
