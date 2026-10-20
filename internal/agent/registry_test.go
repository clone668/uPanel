package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"upanel/internal/monitoring"
)

// stubReader 是按路径前缀返回固定内容的 FileReader：给 monitoring 的采集器做替身。
// 路径不在表里就返回 os.ErrNotExist，这样“来源缺失”的分支也能被测到。
type stubReader struct {
	files map[string]string
}

func (r stubReader) ReadFile(name string) ([]byte, error) {
	if content, ok := r.files[name]; ok {
		return []byte(content), nil
	}
	return nil, os.ErrNotExist
}

func TestRegisterRejectsInvalidNames(t *testing.T) {
	reg := NewRegistry()
	decode := func(raw json.RawMessage) (any, error) { return struct{}{}, nil }
	run := func(ctx context.Context, params any) (any, error) { return struct{}{}, nil }

	for _, name := range []string{
		"", "summary", "System.Summary", "system.", ".summary", "system..summary",
		"system summary", "system/summary", "system.summary;id", "9system.summary",
		"system.summary'--", "system.summary$(id)",
	} {
		if err := reg.RegisterQuery(Query{Name: name, Decode: decode, Run: run}); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("%q 应返回 ErrInvalidName，实际 %v", name, err)
		}
	}
}

func TestRegisterRejectsDuplicatesAndNilHandlers(t *testing.T) {
	reg := NewRegistry()
	decode := func(raw json.RawMessage) (any, error) { return struct{}{}, nil }
	run := func(ctx context.Context, params any) (any, error) { return struct{}{}, nil }

	q := Query{Name: "system.summary", Decode: decode, Run: run}
	if err := reg.RegisterQuery(q); err != nil {
		t.Fatalf("首次登记应成功: %v", err)
	}
	if err := reg.RegisterQuery(q); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("重复登记应返回 ErrDuplicateName，实际 %v", err)
	}
	if err := reg.RegisterQuery(Query{Name: "system.other", Run: run}); !errors.Is(err, ErrNilHandler) {
		t.Fatalf("缺少 Decode 应返回 ErrNilHandler，实际 %v", err)
	}
	if err := reg.RegisterQuery(Query{Name: "system.other", Decode: decode}); !errors.Is(err, ErrNilHandler) {
		t.Fatalf("缺少 Run 应返回 ErrNilHandler，实际 %v", err)
	}
	if err := reg.RegisterAction(Action{Name: "storage.volume.mount", Decode: decode}); err != nil {
		t.Fatalf("动作登记应成功: %v", err)
	}
	if err := reg.RegisterAction(Action{Name: "storage.volume.mount", Decode: decode}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("动作重复登记应返回 ErrDuplicateName，实际 %v", err)
	}
	// 动作与查询是两个独立命名空间：同名不冲突，但都要能被查到。
	if _, ok := reg.Query("system.summary"); !ok {
		t.Fatal("查询未登记成功")
	}
	if _, ok := reg.Action("storage.volume.mount"); !ok {
		t.Fatal("动作未登记成功")
	}
	if _, ok := reg.Query("storage.volume.mount"); ok {
		t.Fatal("动作不应出现在查询表里")
	}
}

func TestProductionRegistryWithoutDepsIsEmpty(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if len(reg.ActionNames()) != 0 {
		t.Fatalf("生产动作表应为空，实际 %v", reg.ActionNames())
	}
	// 没有采集器时也不登记 system.summary：capabilities 不能报告一条永远失败的查询。
	if len(reg.QueryNames()) != 0 {
		t.Fatalf("没有采集器时生产查询表应为空，实际 %v", reg.QueryNames())
	}
	if _, ok := reg.Query("system.summary"); ok {
		t.Fatal("没有采集器时不应登记 system.summary")
	}
	if _, ok := reg.Action("system.power"); ok {
		t.Fatal("P02.3 不应开放任何动作")
	}
}

func TestProductionRegistryRegistersSystemSummaryWithCollector(t *testing.T) {
	collector := monitoring.NewCollector(monitoring.Options{
		Reader: stubReader{files: map[string]string{
			"/etc/os-release":                 "ID=ubuntu\nNAME=Ubuntu\nVERSION_ID=24.04\n",
			"/proc/sys/kernel/hostname":       "testhost\n",
			"/proc/sys/kernel/osrelease":      "6.8.0-test\n",
			"/proc/sys/kernel/random/boot_id": "boot-1\n",
			"/proc/uptime":                    "1234.50 9999.00\n",
			"/proc/stat":                      "cpu  1 2 3 4 5 6 7 8\ncpu0 1 2 3 4 5 6 7 8\n",
			"/proc/meminfo":                   "MemTotal: 1024 kB\nMemFree: 512 kB\nMemAvailable: 768 kB\nBuffers: 1 kB\nCached: 2 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
			"/proc/loadavg":                   "0.10 0.20 0.30 1/100 4242\n",
			"/proc/net/dev":                   "Inter-|   Receive |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n",
		}},
	})
	reg, err := NewProductionRegistry(ProductionDeps{Collector: collector})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if got := strings.Join(reg.QueryNames(), ","); got != "system.summary" {
		t.Fatalf("生产查询表 = %q，期望只有 system.summary", got)
	}
	if len(reg.ActionNames()) != 0 {
		t.Fatalf("登记查询不应带出动作，实际 %v", reg.ActionNames())
	}
}

func TestDecodeParamsStrictness(t *testing.T) {
	type params struct {
		Message string `json:"message"`
		Count   int    `json:"count"`
	}

	var p params
	if err := DecodeParams(nil, &p); err != nil {
		t.Fatalf("缺省参数应按空对象处理: %v", err)
	}
	if err := DecodeParams(json.RawMessage(`{"message":"hi","count":3}`), &p); err != nil {
		t.Fatalf("合法参数应通过: %v", err)
	}
	if p.Message != "hi" || p.Count != 3 {
		t.Fatalf("解析结果 = %+v", p)
	}

	for _, raw := range []string{
		`{"message":"hi","extra":1}`,
		`{"message":"hi"}{"message":"hi"}`,
		`{"message":`,
		`[]`,
	} {
		var q params
		if err := DecodeParams(json.RawMessage(raw), &q); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应返回 ErrParamsInvalid，实际 %v", raw, err)
		}
	}

	big := `{"message":"` + strings.Repeat("x", MaxParamsBytes) + `"}`
	var q params
	if err := DecodeParams(json.RawMessage(big), &q); !errors.Is(err, ErrParamsTooLarge) {
		t.Fatalf("超长参数应返回 ErrParamsTooLarge，实际 %v", err)
	}
}

func TestSortedNamesAreStable(t *testing.T) {
	reg := NewRegistry()
	decode := func(raw json.RawMessage) (any, error) { return struct{}{}, nil }
	run := func(ctx context.Context, params any) (any, error) { return struct{}{}, nil }
	for _, name := range []string{"system.summary", "agent.self", "system.info", "storage.topology"} {
		if err := reg.RegisterQuery(Query{Name: name, Decode: decode, Run: run}); err != nil {
			t.Fatalf("登记 %s 失败: %v", name, err)
		}
	}
	got := strings.Join(reg.QueryNames(), ",")
	want := "agent.self,storage.topology,system.info,system.summary"
	if got != want {
		t.Fatalf("查询名排序 = %q，期望 %q", got, want)
	}
}
