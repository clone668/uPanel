package storage

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

func (f *fakeAgent) Query(_ context.Context, name string, params any) (json.RawMessage, error) {
	f.name = name
	f.params = params
	return f.raw, f.err
}

func sampleTopologyJSON(t *testing.T) []byte {
	t.Helper()
	collected := time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)
	raw, err := json.Marshal(Topology{
		Status:          StatusOK,
		SnapshotVersion: "sha256:abc",
		CollectedAt:     collected,
		IntervalMS:      5000,
		Nodes: []Device{{
			ID: "sda", Kind: KindDisk, Name: "sda", SizeBytes: 40 << 30,
			IdentityConfidence: ConfidenceMedium, IdentityFingerprint: "sha256:disk",
			Health:       Health{Status: HealthUnknown},
			Capabilities: []Capability{},
			Protection:   Protection{SystemDisk: true, Reasons: []string{"承载根文件系统"}, Sources: []string{"sda1"}},
			CollectedAt:  collected,
		}},
		Edges: []Edge{{FromDeviceID: "sda1", ToDeviceID: "sda", Relation: RelationPartitionOf}},
	})
	if err != nil {
		t.Fatalf("构造响应失败: %v", err)
	}
	return raw
}

func TestFetcherDecodesTopology(t *testing.T) {
	agent := &fakeAgent{raw: sampleTopologyJSON(t)}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}

	topo, err := f.Topology(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if agent.name != QueryName {
		t.Fatalf("查询名 = %q，期望 %q", agent.name, QueryName)
	}
	if agent.params != nil {
		t.Fatalf("storage.topology 没有参数，实际传了 %#v", agent.params)
	}
	if topo.SnapshotVersion != "sha256:abc" {
		t.Errorf("snapshot_version = %q", topo.SnapshotVersion)
	}
	if len(topo.Nodes) != 1 || topo.Nodes[0].ID != "sda" {
		t.Fatalf("nodes = %+v", topo.Nodes)
	}
	if !topo.Nodes[0].Protection.SystemDisk {
		t.Errorf("protection 丢了")
	}
}

func TestFetcherMapsUnavailable(t *testing.T) {
	f, err := NewFetcher(&fakeAgent{err: &agentclient.UnavailableError{Err: errors.New("socket 不存在")}}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	_, err = f.Topology(context.Background())
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("错误 = %v，期望 ErrAgentUnavailable", err)
	}
}

func TestFetcherDoesNotHideAgentRefusal(t *testing.T) {
	// 代理明确拒绝（查询没登记、参数被拒）不是“不可用”，不能被翻译成 503。
	f, err := NewFetcher(&fakeAgent{err: errors.New("agent: 未登记的查询")}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	_, err = f.Topology(context.Background())
	if errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("代理明确拒绝却被当成不可用: %v", err)
	}
	if err == nil {
		t.Fatal("代理拒绝却没有错误")
	}
}

func TestFetcherRejectsIncompletePayload(t *testing.T) {
	collected := time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)
	cases := []struct {
		name string
		topo Topology
		want string
	}{
		{
			name: "缺 collected_at 且没说失败",
			topo: Topology{Status: StatusOK, SnapshotVersion: "sha256:a", IntervalMS: 5000, Nodes: []Device{}},
			want: "collected_at",
		},
		{
			name: "标了失败却没给原因",
			topo: Topology{Status: StatusError, IntervalMS: 5000, Nodes: []Device{}},
			want: "原因",
		},
		{
			name: "缺 interval_ms",
			topo: Topology{Status: StatusOK, SnapshotVersion: "sha256:a", CollectedAt: collected, Nodes: []Device{}},
			want: "interval_ms",
		},
		{
			name: "缺 snapshot_version",
			topo: Topology{Status: StatusOK, CollectedAt: collected, IntervalMS: 5000, Nodes: []Device{}},
			want: "snapshot_version",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.topo)
			if err != nil {
				t.Fatalf("构造响应失败: %v", err)
			}
			f, err := NewFetcher(&fakeAgent{raw: raw}, nil)
			if err != nil {
				t.Fatalf("构造 Fetcher 失败: %v", err)
			}
			_, err = f.Topology(context.Background())
			if err == nil {
				t.Fatalf("不完整的响应没报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误 = %q，想要提到 %q", err.Error(), c.want)
			}
		})
	}
}

func TestFetcherAllowsFailedSnapshotWithReason(t *testing.T) {
	// 代理扫描失败但给出了原因与上一次的节点：这是**可展示**的快照，不是错误。
	collected := time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)
	raw, err := json.Marshal(Topology{
		Status: StatusError, Error: "storage: 读不到 /sys/block",
		SnapshotVersion: "sha256:old", CollectedAt: collected, IntervalMS: 5000,
		Nodes: []Device{{ID: "sda", Kind: KindDisk, Name: "sda"}},
	})
	if err != nil {
		t.Fatalf("构造响应失败: %v", err)
	}
	f, err := NewFetcher(&fakeAgent{raw: raw}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	topo, err := f.Topology(context.Background())
	if err != nil {
		t.Fatalf("带原因的失败快照不该被当成错误: %v", err)
	}
	if topo.Status != StatusError || topo.Error == "" {
		t.Errorf("status/error 丢了: %+v", topo)
	}
	if len(topo.Nodes) != 1 {
		t.Errorf("节点丢了: %+v", topo.Nodes)
	}
}

func TestFetcherDevice(t *testing.T) {
	f, err := NewFetcher(&fakeAgent{raw: sampleTopologyJSON(t)}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	dev, topo, found, err := f.Device(context.Background(), "sda")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !found || dev.ID != "sda" {
		t.Fatalf("found=%v device=%+v", found, dev)
	}
	if topo.SnapshotVersion != "sha256:abc" {
		t.Errorf("拓扑没一起返回: %+v", topo)
	}

	// 不存在的设备：found=false 且**不是**错误（调用方按 404 处理）。
	if _, _, found, err := f.Device(context.Background(), "sdz"); err != nil || found {
		t.Fatalf("未知设备: found=%v err=%v，期望 found=false 且无错误", found, err)
	}
}

func TestNewFetcherRequiresAgent(t *testing.T) {
	if _, err := NewFetcher(nil, nil); err == nil {
		t.Fatal("没有代理客户端却构造成功了")
	}
}
