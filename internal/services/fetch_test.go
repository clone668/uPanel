package services

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

func sampleSnapshotJSON(t *testing.T) []byte {
	t.Helper()
	pid := int64(784)
	mem := "483328"
	raw, err := json.Marshal(Snapshot{
		Status:          StatusOK,
		CollectedAt:     time.Date(2026, 10, 21, 0, 20, 0, 0, time.UTC),
		IntervalMS:      10000,
		SnapshotVersion: "sha256:abc",
		Units: []Unit{{
			Unit:          "cron.service",
			Description:   "Regular background program processing daemon",
			LoadState:     LoadLoaded,
			ActiveState:   ActiveActive,
			SubState:      "running",
			UnitFileState: "enabled",
			Enablement:    EnablementEnabled,
			MainPID:       &pid,
			MemoryBytes:   &mem,
			Protection:    Protection{Reasons: []string{}},
			Capabilities:  []string{OpStop, OpRestart, OpDisable},
		}},
	})
	if err != nil {
		t.Fatalf("构造响应失败: %v", err)
	}
	return raw
}

func TestFetcherDecodesSnapshot(t *testing.T) {
	agent := &fakeAgent{raw: sampleSnapshotJSON(t)}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	snap, err := f.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if agent.name != QueryListName {
		t.Errorf("查询名 = %q，期望 %q", agent.name, QueryListName)
	}
	if agent.params != nil {
		t.Errorf("%s 没有参数，实际传了 %#v", QueryListName, agent.params)
	}
	if snap.Status != StatusOK || snap.IntervalMS != 10000 || snap.SnapshotVersion != "sha256:abc" {
		t.Errorf("快照外层 = %+v", snap)
	}
	if len(snap.Units) != 1 || snap.Units[0].Unit != "cron.service" {
		t.Fatalf("units = %+v", snap.Units)
	}
	if snap.Units[0].MainPID == nil || *snap.Units[0].MainPID != 784 {
		t.Errorf("main_pid 丢了: %v", snap.Units[0].MainPID)
	}
	if strings.Join(snap.Units[0].Capabilities, ",") != "stop,restart,disable" {
		t.Errorf("capabilities = %v", snap.Units[0].Capabilities)
	}
}

// TestFetcherMapsUnavailable 代理连不上 → ErrAgentUnavailable（HTTP 层据此回 503）。
func TestFetcherMapsUnavailable(t *testing.T) {
	f, err := NewFetcher(&fakeAgent{err: &agentclient.UnavailableError{Err: errors.New("socket 不存在")}}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	if _, err := f.Snapshot(context.Background()); !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("Snapshot 错误 = %v，期望 ErrAgentUnavailable", err)
	}
	if _, _, err := f.Unit(context.Background(), "cron.service"); !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("Unit 错误 = %v，期望 ErrAgentUnavailable", err)
	}
}

// TestFetcherDoesNotHideAgentRefusal 代理明确拒绝不是“不可用”。
func TestFetcherDoesNotHideAgentRefusal(t *testing.T) {
	f, err := NewFetcher(&fakeAgent{err: errors.New("agent: 未登记的查询")}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	_, err = f.Snapshot(context.Background())
	if err == nil {
		t.Fatal("代理拒绝却没有错误")
	}
	if errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("代理明确拒绝却被当成不可用: %v", err)
	}
}

// TestFetcherRejectsIncompleteSnapshot 缺字段时必须报错，不能返回一份“看起来正常”的列表。
func TestFetcherRejectsIncompleteSnapshot(t *testing.T) {
	collected := time.Date(2026, 10, 21, 0, 20, 0, 0, time.UTC)
	cases := []struct {
		name string
		snap Snapshot
		want string
	}{
		{"缺 collected_at 且没说失败", Snapshot{Status: StatusOK, IntervalMS: 10000, SnapshotVersion: "sha256:a", Units: []Unit{}}, "collected_at"},
		{"标了失败却没给原因", Snapshot{Status: StatusError, IntervalMS: 10000, Units: []Unit{}}, "原因"},
		{"缺 interval_ms", Snapshot{Status: StatusOK, CollectedAt: collected, SnapshotVersion: "sha256:a", Units: []Unit{}}, "interval_ms"},
		{"缺 snapshot_version", Snapshot{Status: StatusOK, CollectedAt: collected, IntervalMS: 10000, Units: []Unit{}}, "snapshot_version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.snap)
			if err != nil {
				t.Fatalf("构造响应失败: %v", err)
			}
			f, err := NewFetcher(&fakeAgent{raw: raw}, nil)
			if err != nil {
				t.Fatalf("构造 Fetcher 失败: %v", err)
			}
			_, err = f.Snapshot(context.Background())
			if err == nil {
				t.Fatal("不完整的快照必须被拒绝")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误 = %q，期望提到 %q", err.Error(), c.want)
			}
		})
	}
}

// TestFetcherSnapshotNormalizesNilUnits 代理返回 units:null 时归一化为空数组。
func TestFetcherSnapshotNormalizesNilUnits(t *testing.T) {
	raw := []byte(`{"status":"error","error":"尚未完成第一次采集","collected_at":"0001-01-01T00:00:00Z","interval_ms":10000,"units":null}`)
	f, err := NewFetcher(&fakeAgent{raw: raw}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	snap, err := f.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if snap.Units == nil || len(snap.Units) != 0 {
		t.Errorf("units 应为空数组，实际 %#v", snap.Units)
	}
	if snap.Status != StatusError || snap.Error == "" {
		t.Errorf("失败状态丢了: %+v", snap)
	}
}

func TestFetcherUnit(t *testing.T) {
	unit := sampleUnitJSON(t, "cron.service")
	agent := &fakeAgent{raw: json.RawMessage(`{"found":true,"unit":` + string(unit) + `}`)}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	got, found, err := f.Unit(context.Background(), "cron.service")
	if err != nil || !found {
		t.Fatalf("读取失败: found=%v err=%v", found, err)
	}
	if got.Unit != "cron.service" {
		t.Errorf("单元名 = %q", got.Unit)
	}
	if agent.name != QueryUnitName {
		t.Errorf("查询名 = %q", agent.name)
	}
	params, ok := agent.params.(map[string]string)
	if !ok || params["unit"] != "cron.service" {
		t.Errorf("参数 = %#v", agent.params)
	}
}

func sampleUnitJSON(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(Unit{
		Unit:         name,
		LoadState:    LoadLoaded,
		ActiveState:  ActiveActive,
		SubState:     "running",
		Enablement:   EnablementEnabled,
		Protection:   Protection{Reasons: []string{}},
		Capabilities: []string{OpStop, OpRestart, OpDisable},
	})
	if err != nil {
		t.Fatalf("构造单元失败: %v", err)
	}
	return raw
}

// TestFetcherUnitNotFound found=false 不是错误。
func TestFetcherUnitNotFound(t *testing.T) {
	f, err := NewFetcher(&fakeAgent{raw: json.RawMessage(`{"found":false,"unit":null}`)}, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	_, found, err := f.Unit(context.Background(), "ghost.service")
	if err != nil || found {
		t.Fatalf("found=%v err=%v，期望 found=false 且无错误", found, err)
	}
}

// TestFetcherUnitRejectsInconsistentPayload 代理自相矛盾的响应要报错。
func TestFetcherUnitRejectsInconsistentPayload(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"说存在却没给状态", `{"found":true,"unit":null}`, "没有返回状态"},
		{"单元名不一致", `{"found":true,"unit":` + string(sampleUnitJSON(t, "other.service")) + `}`, "不一致"},
		{"不是合法 JSON", `{`, "不是合法 JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := NewFetcher(&fakeAgent{raw: json.RawMessage(c.raw)}, nil)
			if err != nil {
				t.Fatalf("构造 Fetcher 失败: %v", err)
			}
			_, _, err = f.Unit(context.Background(), "cron.service")
			if err == nil {
				t.Fatal("不一致的响应必须报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误 = %q，期望提到 %q", err.Error(), c.want)
			}
		})
	}
}

// TestFetcherUnitRejectsInvalidName Web 侧先挡一层非法名字。
func TestFetcherUnitRejectsInvalidName(t *testing.T) {
	agent := &fakeAgent{raw: json.RawMessage(`{"found":false,"unit":null}`)}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	if _, _, err := f.Unit(context.Background(), "cron.service;reboot"); !errors.Is(err, ErrInvalidUnitName) {
		t.Fatalf("错误 = %v，期望 ErrInvalidUnitName", err)
	}
	if agent.name != "" {
		t.Errorf("非法名字不应发出查询，实际发了 %q", agent.name)
	}
}

// TestNewFetcherRequiresAgent 没有代理客户端时构造即失败。
func TestNewFetcherRequiresAgent(t *testing.T) {
	if _, err := NewFetcher(nil, nil); err == nil {
		t.Fatal("nil 代理必须被拒绝")
	}
}
