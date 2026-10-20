package storage

import (
	"encoding/json"
	"strings"
	"testing"
)

// 网络挂载在快照里是**独立列表**（PLAN.md 第 8 节），不是塞进某个设备的挂载点。
// 原因很实在：NFS/CIFS 的 major:minor 是伪设备号，/sys/dev/block 里没有对应的
// 块设备，硬要塞进 nodes 就得编一个不存在的设备出来。
//
// 这组用例全部用合成的 mountRecord：真实夹具里没有网络挂载，而“有没有网络挂载”
// 与机器型号无关，是纯解析问题。

func TestSelectNetworkMountsKeepsOnlyBlocklessNetworkFS(t *testing.T) {
	byDev := map[string]string{
		"8:1":  "sda1",
		"0:24": "", // 没有块设备
	}
	mounts := []mountRecord{
		{majorMinor: "8:1", path: "/", fstype: "ext4", source: "/dev/sda1"},
		{majorMinor: "0:24", path: "/run", fstype: "tmpfs", source: "tmpfs"},
		{majorMinor: "0:44", path: "/mnt/upanel-nfs", fstype: "nfs4", source: "10.0.0.5:/export"},
		{majorMinor: "0:45", path: "/srv/smb", fstype: "cifs", source: "//10.0.0.6/share", readOnly: true},
		{majorMinor: "0:46", path: "/mnt/local", fstype: "9p", source: "hostshare"},
	}
	got := selectNetworkMounts(mounts, byDev)
	if len(got) != 3 {
		t.Fatalf("应当只收网络挂载，实际得到 %d 项：%+v", len(got), got)
	}
	// 按路径排序：界面顺序必须稳定，不能跟着 mountinfo 行序漂。
	want := []string{"/mnt/local", "/mnt/upanel-nfs", "/srv/smb"}
	for i, w := range want {
		if got[i].Path != w {
			t.Fatalf("第 %d 项路径是 %q，期望 %q", i, got[i].Path, w)
		}
	}
	if got[1].Source != "10.0.0.5:/export" || got[1].FSType != "nfs4" {
		t.Fatalf("NFS 挂载的 source/fstype 没带出来：%+v", got[1])
	}
	if !got[2].ReadOnly {
		t.Fatalf("cifs 挂载是 ro，read_only 没带出来：%+v", got[2])
	}
	// 刚进快照时还没有容量，可达性必须是“未查过”而不是 false。
	if got[1].Capacity != nil || got[1].Reachable != nil {
		t.Fatalf("未采集容量前不该给出结论：%+v", got[1])
	}
}

func TestSelectNetworkMountsDedupesPathAndHandlesEmpty(t *testing.T) {
	byDev := map[string]string{}
	mounts := []mountRecord{
		{majorMinor: "0:44", path: "/mnt/nfs", fstype: "nfs4", source: "a:/x"},
		{majorMinor: "0:44", path: "/mnt/nfs", fstype: "nfs4", source: "a:/x"},
	}
	if got := selectNetworkMounts(mounts, byDev); len(got) != 1 {
		t.Fatalf("同一路径的重复行应当只留一条，实际 %d 条", len(got))
	}
	empty := selectNetworkMounts(nil, byDev)
	if empty == nil {
		t.Fatal("没有网络挂载时要返回空切片，不能是 nil（JSON 会是 null）")
	}
	if len(empty) != 0 {
		t.Fatalf("空输入不该产出条目：%+v", empty)
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("空列表要序列化成 []，实际 %s", raw)
	}
}

func TestNetworkMountJSONShape(t *testing.T) {
	topo := Topology{
		Status:      StatusOK,
		CollectedAt: fixedNow,
		Nodes:       []Device{},
		Edges:       []Edge{},
		NetworkMounts: []NetworkMount{
			{Path: "/mnt/nfs", Source: "10.0.0.5:/export", FSType: "nfs4"},
		},
	}
	raw, err := json.Marshal(topo)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded struct {
		NetworkMounts []struct {
			Path      string    `json:"path"`
			Source    string    `json:"source"`
			FSType    string    `json:"fstype"`
			ReadOnly  bool      `json:"read_only"`
			Reachable *bool     `json:"reachable"`
			Capacity  *Capacity `json:"capacity"`
		} `json:"network_mounts"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(decoded.NetworkMounts) != 1 {
		t.Fatalf("network_mounts 丢了：%s", raw)
	}
	nm := decoded.NetworkMounts[0]
	if nm.Path != "/mnt/nfs" || nm.FSType != "nfs4" || nm.Source != "10.0.0.5:/export" {
		t.Fatalf("字段被改写：%+v", nm)
	}
	if nm.Capacity != nil || nm.Reachable != nil {
		t.Fatalf("没有容量与结论时必须是 null：%s", raw)
	}
}

// 网络挂载的可达性不参与指纹：statfs 每 30 秒都可能变，放进去会让 P07 的预检永远过期；
// 但挂载点本身消失必须让指纹变——那是真的配置变化。
func TestSnapshotVersionIgnoresNetworkMountObservations(t *testing.T) {
	nodes := []Device{{ID: "sda", Kind: KindDisk, Name: "sda"}}
	edges := []Edge{}
	base := []NetworkMount{{Path: "/mnt/nfs", FSType: "nfs4", Source: "a:/x"}}
	v1, err := snapshotVersion(nodes, edges, base)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}

	yes := true
	observed := []NetworkMount{{
		Path:          "/mnt/nfs",
		FSType:        "nfs4",
		Source:        "a:/x",
		Reachable:     &yes,
		Capacity:      &Capacity{TotalBytes: 1000, CollectedAt: fixedNow},
		CapacityError: "查询超时：storage: 容量查询超时",
	}}
	v2, err := snapshotVersion(nodes, edges, observed)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if v1 != v2 {
		t.Fatal("容量与可达性是观测值，不该改变指纹")
	}

	if _, err := snapshotVersion(nodes, edges, nil); err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	v3, err := snapshotVersion(nodes, edges, []NetworkMount{})
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if v3 == v1 {
		t.Fatal("挂载点消失必须改变指纹：这是真的配置变化")
	}
}

func TestCloneTopologyCopiesNetworkMounts(t *testing.T) {
	yes := true
	topo := Topology{NetworkMounts: []NetworkMount{{
		Path:      "/mnt/nfs",
		FSType:    "nfs4",
		Reachable: &yes,
		Capacity:  &Capacity{TotalBytes: 42},
	}}}
	clone := cloneTopology(topo)
	*clone.NetworkMounts[0].Reachable = false
	clone.NetworkMounts[0].Capacity.TotalBytes = 7
	if !*topo.NetworkMounts[0].Reachable || topo.NetworkMounts[0].Capacity.TotalBytes != 42 {
		t.Fatal("克隆改动了原始快照：Reachable/Capacity 没有深拷贝")
	}
}

func TestCapacityReasonText(t *testing.T) {
	// 这两种失败对运维的含义完全不同：一个是远端不可达，一个是文件系统在但问不出数字。
	if !strings.Contains(capacityReason(ErrCapacityTimeout), "超时") {
		t.Fatalf("超时说明没提到超时：%s", capacityReason(ErrCapacityTimeout))
	}
	if !strings.Contains(capacityReason(ErrCapacityUnavailable), "无法读取") {
		t.Fatalf("不可读说明不清楚：%s", capacityReason(ErrCapacityUnavailable))
	}
}
