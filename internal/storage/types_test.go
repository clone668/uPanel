package storage

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTopologyJSONWire 固定线上格式（CONTRACTS.md 第 1 节与第 6 节）：
// 字节数是十进制字符串、利用率是数字、未知是 null、数组是空数组而不是 null。
// 前端按这几条写解析，改动就是破坏契约。
func TestTopologyJSONWire(t *testing.T) {
	collected := time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)
	model := "QEMU HARDDISK"
	fsType := "ext4"
	uuid := "59134bd0-a76f-4ea0-8e4a-770b2e1eafc2"
	topo := Topology{
		Status:          StatusOK,
		SnapshotVersion: "sha256:abc",
		CollectedAt:     collected,
		IntervalMS:      5000,
		Nodes: []Device{{
			ID:                  "sda1",
			Kind:                KindPartition,
			Name:                "sda1",
			SizeBytes:           41874865664,
			LogicalSectorBytes:  512,
			PhysicalSectorBytes: 4096,
			Model:               &model,
			IdentityConfidence:  ConfidenceMedium,
			IdentityFingerprint: "sha256:def",
			Filesystem: &Filesystem{
				Type:  &fsType,
				UUID:  &uuid,
				Label: nil,
				Mounts: []Mount{{
					Path: "/", Source: "/dev/sda1", FSType: "ext4",
					Options: []string{"rw", "relatime"}, ReadOnly: false,
					Capacity: &Capacity{
						TotalBytes: 41874865664, AvailableBytes: 30000000000,
						UsedPercent: 28.3, InodeTotal: 2621440, InodeAvailable: 2500000,
						InodeUsedPercent: 4.6, CollectedAt: collected,
					},
				}},
			},
			PartitionTable: &PartitionTable{
				Type:        TableGPT,
				Partitions:  []Partition{{DeviceID: "sda1", Number: 1, StartBytes: 1048576, SizeBytes: 41874865664}},
				FreeRegions: []FreeRegion{},
				Source:      "device",
			},
			Health: Health{Status: HealthUnknown},
			// Capabilities 是空数组：P04.2 没有登记任何写动作。
			Capabilities: []Capability{},
			Protection:   Protection{SystemDisk: true, Reasons: []string{"承载根文件系统"}, Sources: []string{"sda1"}},
			CollectedAt:  collected,
		}},
		Edges: []Edge{{FromDeviceID: "sda1", ToDeviceID: "sda", Relation: RelationPartitionOf}},
	}

	raw, err := json.Marshal(topo)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	out := string(raw)

	want := []string{
		`"size_bytes":"41874865664"`,
		`"start_bytes":"1048576"`,
		`"total_bytes":"41874865664"`,
		`"available_bytes":"30000000000"`,
		`"inode_total":"2621440"`,
		`"inode_available":"2500000"`,
		`"used_percent":28.3`,
		`"inode_used_percent":4.6`,
		`"logical_sector_bytes":512`,
		`"collected_at":"2026-10-20T17:40:00Z"`,
		`"interval_ms":5000`,
		`"snapshot_version":"sha256:abc"`,
		`"system_disk":true`,
		`"relation":"partition_of"`,
		`"from_device_id":"sda1"`,
		`"to_device_id":"sda"`,
		`"identity_confidence":"medium"`,
		`"health":{"status":"unknown","temperature_celsius":null,"source":"","checked_at":null,"details":null}`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("响应里缺少 %s\n实际: %s", w, out)
		}
	}
}

// TestUnknownIsNullNotZero 钉住“不知道 ≠ 0”：
// 没有文件系统、没有容量、没有温度时必须是 null。
func TestUnknownIsNullNotZero(t *testing.T) {
	dev := Device{
		ID: "sdb", Kind: KindDisk, Name: "sdb",
		Health:       Health{Status: HealthUnknown},
		Capabilities: []Capability{},
		Protection:   Protection{Reasons: []string{}, Sources: []string{}},
	}
	raw, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	out := string(raw)
	for _, w := range []string{
		`"filesystem":null`,
		`"partition_table":null`,
		`"model":null`,
		`"serial":null`,
		`"wwn":null`,
		`"transport":null`,
		`"temperature_celsius":null`,
		`"checked_at":null`,
		`"capabilities":[]`,
		`"reasons":[]`,
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("缺少 %s\n实际: %s", w, out)
		}
	}
	// 没有交换区时字段整条不出现（omitempty），而不是 swap:null 让人以为字段坏了。
	if strings.Contains(out, `"swap"`) {
		t.Errorf("没有交换区时不该出现 swap 字段: %s", out)
	}
	// 尺寸为 0 的字节字段仍然是字符串 "0"，不是数字。
	if !strings.Contains(out, `"size_bytes":"0"`) {
		t.Errorf("0 字节也应写成字符串: %s", out)
	}
}

// TestCapacityPercentOneDecimal 保证百分比按一位小数输出（33.3 而不是 33.333333）。
func TestCapacityPercentOneDecimal(t *testing.T) {
	c := Capacity{
		TotalBytes: 3, AvailableBytes: 2, UsedPercent: 33.3,
		InodeTotal: 0, InodeAvailable: 0, InodeUsedPercent: 0,
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(raw), `"used_percent":33.3`) {
		t.Errorf("容量百分比不是一位小数: %s", raw)
	}
	if !strings.Contains(string(raw), `"total_bytes":"`+strconv.FormatUint(3, 10)+`"`) {
		t.Errorf("total_bytes 不是十进制字符串: %s", raw)
	}
}

// TestRound1 检查四舍五入的边界：0、极小值、恰好半位。
func TestRound1(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-1, 0}, {0, 0}, {0.04, 0}, {0.05, 0.1}, {0.14, 0.1}, {0.15, 0.2},
		{33.33333, 33.3}, {99.95, 100}, {100, 100},
	}
	for _, c := range cases {
		if got := round1(c.in); got != c.want {
			t.Errorf("round1(%v) = %v，想要 %v", c.in, got, c.want)
		}
	}
}
