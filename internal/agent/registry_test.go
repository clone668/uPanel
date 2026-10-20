package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

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

func TestProductionRegistryIsEmpty(t *testing.T) {
	reg := NewProductionRegistry()
	if len(reg.ActionNames()) != 0 {
		t.Fatalf("生产动作表应为空，实际 %v", reg.ActionNames())
	}
	if len(reg.QueryNames()) != 0 {
		t.Fatalf("生产查询表应为空，实际 %v", reg.QueryNames())
	}
	if _, ok := reg.Query("system.summary"); ok {
		t.Fatal("P02.3 不应开放任何查询")
	}
	if _, ok := reg.Action("system.power"); ok {
		t.Fatal("P02.3 不应开放任何动作")
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
