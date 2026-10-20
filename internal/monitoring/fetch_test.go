package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
)

// fakeAgent 记录调用并返回预设结果。
type fakeAgent struct {
	name   string
	params any
	raw    json.RawMessage
	err    error
}

func (f *fakeAgent) Query(ctx context.Context, name string, params any) (json.RawMessage, error) {
	f.name = name
	f.params = params
	return f.raw, f.err
}

func TestFetcherDecodesSummary(t *testing.T) {
	collected := time.Date(2026, 10, 20, 8, 0, 5, 0, time.UTC)
	payload, err := json.Marshal(Summary{
		CollectedAt: collected,
		IntervalMS:  5000,
		Host:        HostSection{Section: Section{Status: StatusOK, CollectedAt: &collected}, Hostname: "upanel-2404"},
		CPU:         CPUSection{Section: Section{Status: StatusOK}, LogicalCores: 2},
		Memory:      MemorySection{Section: Section{Status: StatusOK}},
		Load:        LoadSection{Section: Section{Status: StatusOK}},
		Network:     NetworkSection{Section: Section{Status: StatusOK}},
	})
	if err != nil {
		t.Fatalf("构造响应失败: %v", err)
	}
	agent := &fakeAgent{raw: payload}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}

	sum, err := f.Summary(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if agent.name != QueryName {
		t.Fatalf("查询名 = %q，期望 %q", agent.name, QueryName)
	}
	if agent.params != nil {
		t.Fatalf("system.summary 没有参数，实际传了 %#v", agent.params)
	}
	if !sum.CollectedAt.Equal(collected) {
		t.Fatalf("collected_at = %v", sum.CollectedAt)
	}
	if sum.Host.Hostname != "upanel-2404" {
		t.Fatalf("主机名 = %q", sum.Host.Hostname)
	}
	// degraded 由代理给出，Fetcher 不改写它。
	if sum.Degraded {
		t.Fatal("代理没标 degraded，Fetcher 不应自己算一个")
	}
}

func TestFetcherMapsUnavailable(t *testing.T) {
	agent := &fakeAgent{err: &agentclient.UnavailableError{Err: errors.New("dial unix: connection refused")}}
	f, _ := NewFetcher(agent, nil)

	_, err := f.Summary(context.Background())
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("代理连不上时应返回 ErrAgentUnavailable，实际 %v", err)
	}
}

func TestFetcherDoesNotHideAgentRefusal(t *testing.T) {
	agent := &fakeAgent{err: &agentclient.Error{Status: 400, Code: "VALIDATION_ERROR", Message: "未登记的查询"}}
	f, _ := NewFetcher(agent, nil)

	_, err := f.Summary(context.Background())
	if err == nil {
		t.Fatal("代理拒绝时不能当成成功")
	}
	if errors.Is(err, ErrAgentUnavailable) {
		t.Fatal("代理明确拒绝不是“代理不可用”，两者不能混为一谈")
	}
	if !strings.Contains(err.Error(), "未登记的查询") {
		t.Fatalf("错误应保留代理的说明，实际 %q", err.Error())
	}
}

func TestFetcherRejectsIncompletePayload(t *testing.T) {
	cases := map[string]string{
		"不是 JSON":        `{`,
		"缺 collected_at": `{"interval_ms":5000}`,
		"缺 interval_ms":  `{"collected_at":"2026-10-20T08:00:00Z"}`,
	}
	for name, payload := range cases {
		f, _ := NewFetcher(&fakeAgent{raw: json.RawMessage(payload)}, nil)
		if _, err := f.Summary(context.Background()); err == nil {
			t.Fatalf("%s: 应该报错", name)
		}
	}
}

func TestNewFetcherRequiresAgent(t *testing.T) {
	if _, err := NewFetcher(nil, nil); err == nil {
		t.Fatal("没有代理客户端时必须报错，不能给一个空实现")
	}
}
