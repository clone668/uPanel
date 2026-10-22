package storage

// 卷读取（storage.volumes / storage.volume.blockers）在 Web 侧的测试（P07.4）。
//
// 这一层只做三件事：把代理的响应解成结构体、把空数组补成 []、把代理的拒绝
// 分成“不可用（503）”与“明确说不（4xx，原样带出）”。卷的事实由代理回答，
// 这里不重算任何状态。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
)

// volumeAgent 是能按查询名返回不同响应的替身。
type volumeAgent struct {
	calls  []string
	params any
	byName map[string]json.RawMessage
	err    error
}

func (a *volumeAgent) Query(_ context.Context, name string, params any) (json.RawMessage, error) {
	a.calls = append(a.calls, name)
	a.params = params
	if a.err != nil {
		return nil, a.err
	}
	raw, ok := a.byName[name]
	if !ok {
		return nil, &agentclient.Error{Status: 400, Code: "VALIDATION_ERROR", Message: "未登记的查询 " + name}
	}
	return raw, nil
}

// sampleVolumeView 是一条来自代理的卷视图。
func sampleVolumeView() Volume {
	return Volume{
		ID:             "vol_0123456789abcdef0123456789abcdef",
		Version:        "2",
		DisplayName:    "数据卷",
		Slug:           "data",
		DeviceID:       "sdb1",
		FilesystemUUID: "5f1c9a3e-2b64-4c7d-8e10-9a3b5c7d1e2f",
		FilesystemType: FilesystemExt4,
		MountPath:      MountPathOf("data"),
		Automount:      true,
		Nodev:          true,
		Nosuid:         true,
		DesiredState:   DesiredMounted,
		ObservedState:  VolumeStateMounted,
		Dependencies:   []VolumeDependency{},
		UpdatedAt:      time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC),
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("构造响应失败: %v", err)
	}
	return raw
}

func TestFetcherVolumesDecodes(t *testing.T) {
	want := sampleVolumeView()
	agent := &volumeAgent{byName: map[string]json.RawMessage{
		VolumesQueryName: mustJSON(t, map[string]any{"volumes": []Volume{want}}),
	}}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	got, err := f.Volumes(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("卷数 = %d", len(got))
	}
	if got[0].ID != want.ID || got[0].FilesystemUUID != want.FilesystemUUID {
		t.Errorf("卷 = %+v", got[0])
	}
	// 观察到的状态必须原样带过来：它不是能重算出来的字段。
	if got[0].ObservedState != VolumeStateMounted || got[0].DesiredState != DesiredMounted {
		t.Errorf("observed/desired = %q/%q", got[0].ObservedState, got[0].DesiredState)
	}
	if agent.calls[0] != VolumesQueryName || agent.params != nil {
		t.Errorf("调用 = %v params=%#v", agent.calls, agent.params)
	}
}

func TestFetcherVolumesEmptyIsNotNil(t *testing.T) {
	agent := &volumeAgent{byName: map[string]json.RawMessage{
		VolumesQueryName: mustJSON(t, map[string]any{}),
	}}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	got, err := f.Volumes(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// nil 会被序列化成 null，页面会把它当成“未知”而不是“没有卷”。
	if got == nil {
		t.Fatal("空列表返回了 nil")
	}
	if len(got) != 0 {
		t.Errorf("卷数 = %d", len(got))
	}
}

func TestFetcherVolumePicksByID(t *testing.T) {
	want := sampleVolumeView()
	other := want
	other.ID = "vol_fedcba9876543210fedcba9876543210"
	other.Slug = "logs"
	other.MountPath = MountPathOf("logs")
	agent := &volumeAgent{byName: map[string]json.RawMessage{
		VolumesQueryName: mustJSON(t, map[string]any{"volumes": []Volume{other, want}}),
	}}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}

	got, found, err := f.Volume(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !found || got.Slug != "data" {
		t.Fatalf("found=%v slug=%q", found, got.Slug)
	}

	// 不存在的卷：found=false 且不是错误（调用方按 404 处理）。
	_, found, err = f.Volume(context.Background(), "vol_00000000000000000000000000000000")
	if err != nil || found {
		t.Fatalf("未知卷: found=%v err=%v，期望 found=false 且无错误", found, err)
	}
}

func TestFetcherVolumeBlockersPassesIDAndFillsArrays(t *testing.T) {
	agent := &volumeAgent{byName: map[string]json.RawMessage{
		// 代理只回标量与 null：空数组要由这一层补成 []。
		VolumeBlockersQueryName: mustJSON(t, map[string]any{
			"volume_id": "vol_0123456789abcdef0123456789abcdef",
			"mounted":   true,
			"busy":      true,
		}),
	}}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	got, err := f.VolumeBlockers(context.Background(), "vol_0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	params, ok := agent.params.(map[string]string)
	if !ok || params["volume_id"] != "vol_0123456789abcdef0123456789abcdef" {
		t.Fatalf("参数 = %#v", agent.params)
	}
	for name, isNil := range map[string]bool{
		"mounts":    got.Mounts == nil,
		"processes": got.Processes == nil,
		"holders":   got.Holders == nil,
		"apps":      got.Apps == nil,
		"shares":    got.Shares == nil,
		"backups":   got.Backups == nil,
	} {
		if isNil {
			t.Errorf("%s 是 nil，应该补成空数组（页面据此区分“没有占用”与“未知”）", name)
		}
	}
	if !got.Busy || !got.Mounted {
		t.Errorf("busy/mounted 丢了：%+v", got)
	}
}

func TestFetcherQueryMapsRemoteRefusal(t *testing.T) {
	// 代理明确拒绝：状态码与码必须原样上到接口层（404 不能压成 500/503）。
	agent := &volumeAgent{err: &agentclient.Error{
		Status: http.StatusNotFound, Code: string(httpx.CodeNotFound), Message: "卷不存在：vol_x",
	}}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	_, err = f.Volumes(context.Background())
	if errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("代理拒绝被当成了不可用: %v", err)
	}
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("错误 = %v，期望 *RemoteError", err)
	}
	if remote.Status != http.StatusNotFound || remote.Code != httpx.CodeNotFound {
		t.Errorf("remote = %+v", remote)
	}
	if remote.Error() == "" {
		t.Error("RemoteError 的 Error() 是空的：日志里会看不出代理说了什么")
	}
}

func TestFetcherVolumeCallsMapUnavailable(t *testing.T) {
	// 不可用的翻译对两条卷查询都要成立：它们共用同一个 query。
	for _, call := range []struct {
		name string
		run  func(*Fetcher) error
	}{
		{"storage.volumes", func(f *Fetcher) error { _, err := f.Volumes(context.Background()); return err }},
		{"storage.volume.blockers", func(f *Fetcher) error {
			_, err := f.VolumeBlockers(context.Background(), "vol_0123456789abcdef0123456789abcdef")
			return err
		}},
		{"storage.volume.detail", func(f *Fetcher) error {
			_, _, err := f.Volume(context.Background(), "vol_0123456789abcdef0123456789abcdef")
			return err
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			agent := &volumeAgent{err: &agentclient.UnavailableError{Err: errors.New("socket 不存在")}}
			f, err := NewFetcher(agent, nil)
			if err != nil {
				t.Fatalf("构造 Fetcher 失败: %v", err)
			}
			if err := call.run(f); !errors.Is(err, ErrAgentUnavailable) {
				t.Fatalf("错误 = %v，期望 ErrAgentUnavailable", err)
			}
		})
	}
}

func TestFetcherRejectsMalformedVolumePayload(t *testing.T) {
	agent := &volumeAgent{byName: map[string]json.RawMessage{
		VolumesQueryName:        json.RawMessage(`"不是对象"`),
		VolumeBlockersQueryName: json.RawMessage(`[]`),
	}}
	f, err := NewFetcher(agent, nil)
	if err != nil {
		t.Fatalf("构造 Fetcher 失败: %v", err)
	}
	if _, err := f.Volumes(context.Background()); err == nil {
		t.Fatal("不合法的卷列表没报错")
	}
	// 块设备占用那份响应是数组而不是对象，同样要拒绝而不是静默给空。
	if _, err := f.VolumeBlockers(context.Background(), "vol_0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("不合法的占用信息没报错")
	}
}
