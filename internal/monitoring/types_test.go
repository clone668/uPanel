package monitoring

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSummaryJSONUsesDecimalStrings 固定线上格式：字节数与计数器是十进制字符串，
// 百分比与速率是数字，未知值是 null。前端按这三条写解析，改动就是破坏契约。
func TestSummaryJSONUsesDecimalStrings(t *testing.T) {
	collected := time.Date(2026, 10, 20, 8, 0, 5, 123456789, time.UTC)
	total := uint64(4009864 * 1024)
	used := uint64(37240)
	usage := 12.5
	rate := 0.0
	sum := Summary{
		CollectedAt: collected,
		IntervalMS:  5000,
		Host:        HostSection{Section: Section{Status: StatusOK}},
		CPU: CPUSection{Section: Section{Status: StatusOK},
			LogicalCores: 2, UsagePercent: &usage},
		Memory: MemorySection{Section: Section{Status: StatusOK},
			TotalBytes: &total, UsedBytes: &used},
		Load: LoadSection{Section: Section{Status: StatusOK}},
		Network: NetworkSection{
			Section: Section{Status: StatusOK},
			Interfaces: []NetInterface{{
				Name: "ens4", RXBytes: 402930897, RXPackets: 283011,
				RXBytesPerSecond: &rate,
			}},
		},
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	out := string(raw)

	for _, want := range []string{
		`"total_bytes":"` + strconv.FormatUint(total, 10) + `"`,
		`"used_bytes":"37240"`,
		`"rx_bytes":"402930897"`,
		`"rx_packets":"283011"`,
		`"usage_percent":12.5`,
		`"logical_cores":2`,
		`"rx_bytes_per_second":0`,
		`"collected_at":"2026-10-20T08:00:05.123456789Z"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("响应里缺少 %s\n实际: %s", want, out)
		}
	}

	// 未知值一律 null，不是 0。
	sum.CPU.UsagePercent = nil
	sum.Memory.TotalBytes = nil
	sum.Network.Interfaces[0].RXBytesPerSecond = nil
	raw, err = json.Marshal(sum)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	out = string(raw)
	for _, want := range []string{
		`"usage_percent":null`, `"total_bytes":null`, `"rx_bytes_per_second":null`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("未知值应为 null，缺少 %s\n实际: %s", want, out)
		}
	}
}
