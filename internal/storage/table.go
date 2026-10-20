package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// GPT 的常量。名字按 UEFI 规范第 5 章。
const (
	gptHeaderSignature = "EFI PART"
	gptHeaderLBA       = 1
	sectorSize         = 512
	// 采集脚本读的就是前 34 个扇区：MBR + GPT 头 + 128 项 × 128 字节的条目表。
	maxTableBytes = 34 * sectorSize
)

// probePartitionTable 判断分区表类型并算出空闲区间。
//
// 三条原则：
//
//   - **分区列表来自 sysfs**（内核当前认定的事实），不是从盘上读的。盘上写的和内核认的
//     可能不一致（例如刚被别的系统改过、或者分区表损坏），那种情况用 Warning 说出来，
//     而不是悄悄用盘上的数据覆盖内核的。
//   - 盘在休眠、读失败或超时就不读，Source 标成 skipped。读 /dev 会唤醒盘，
//     那是 PLAN.md 第 8 节明确不允许的行为。
//   - 判不出来就说 unknown。宁可让界面显示“未知”，也不要编一个 gpt。
func probePartitionTable(ctx context.Context, root Root, dir string, dev *Device) {
	table := PartitionTable{
		Type:        TableUnknown,
		Partitions:  []Partition{},
		FreeRegions: []FreeRegion{},
		Source:      "kernel",
	}
	if dev.PartitionTable != nil {
		table.Partitions = dev.PartitionTable.Partitions
	}

	if root.Disks == nil {
		dev.PartitionTable = &table
		return
	}
	if !dev.Online || sleeping(dir) {
		table.Source = "skipped"
		table.Warning = "设备不在运行状态或处于休眠，未读取分区表（不强制唤醒磁盘）"
		dev.PartitionTable = &table
		return
	}

	raw, err := root.Disks.ReadDisk(ctx, dev.Name)
	if err != nil {
		table.Source = "skipped"
		table.Warning = fmt.Sprintf("读取分区表失败：%v", err)
		dev.PartitionTable = &table
		return
	}
	if len(raw) > maxTableBytes {
		raw = raw[:maxTableBytes]
	}

	parsed, err := parsePartitionTable(raw)
	if err != nil {
		table.Source = "skipped"
		table.Warning = fmt.Sprintf("分区表解析失败：%v", err)
		dev.PartitionTable = &table
		return
	}
	table.Type = parsed.Type
	table.Source = "device"
	table.FreeRegions = parsed.FreeRegions

	// 盘上条目数与内核分区数不一致时如实提示：可能是分区表刚改过还没重读，
	// 也可能是内核拒收了某些分区。两种都不该被隐藏。
	if parsed.Type == TableGPT && parsed.Count != len(table.Partitions) {
		table.Warning = fmt.Sprintf("盘上分区表有 %d 个分区，内核当前认可 %d 个（可能刚改过分区表或内核拒绝重读）",
			parsed.Count, len(table.Partitions))
	}
	dev.PartitionTable = &table
}

// partitionTableResult 是解析结果（内部用）。
type partitionTableResult struct {
	Type        string
	Count       int
	FreeRegions []FreeRegion
}

// parsePartitionTable 解析读到的整盘头部字节（至少含 LBA0 与 LBA1）。
func parsePartitionTable(raw []byte) (partitionTableResult, error) {
	if len(raw) < sectorSize {
		return partitionTableResult{}, fmt.Errorf("只读到 %d 字节，不足一个扇区", len(raw))
	}

	// GPT：LBA1 的 8 字节签名。先看它，因为 GPT 盘的 LBA0 是保护性 MBR（type 0xEE），
	// 只看 MBR 会把它误判成 mbr。
	if len(raw) >= gptHeaderLBA*sectorSize+92 &&
		string(raw[gptHeaderLBA*sectorSize:gptHeaderLBA*sectorSize+8]) == gptHeaderSignature {
		return parseGPT(raw)
	}

	// MBR：偏移 510 的 0x55AA。四个 16 字节条目从 446 开始。
	if len(raw) >= 512 && raw[510] == 0x55 && raw[511] == 0xAA {
		count := 0
		for i := 0; i < 4; i++ {
			off := 446 + i*16
			if off+16 > len(raw) {
				break
			}
			if raw[off+4] == 0 { // 分区类型 0 = 空条目
				continue
			}
			count++
		}
		return partitionTableResult{Type: TableMBR, Count: count, FreeRegions: []FreeRegion{}}, nil
	}

	// 全零（新盘/未分区）与“有数据但不是分区表”要分开说。
	if allZero(raw[:512]) {
		return partitionTableResult{Type: TableNone, Count: 0, FreeRegions: []FreeRegion{}}, nil
	}
	return partitionTableResult{Type: TableUnknown, Count: 0, FreeRegions: []FreeRegion{}},
		fmt.Errorf("首扇区既不是 GPT 也不是 MBR，内容也不是全零")
}

// parseGPT 解析 GPT 头与条目表，算出已用区间与空闲区间。
func parseGPT(raw []byte) (partitionTableResult, error) {
	hdr := raw[gptHeaderLBA*sectorSize:]
	if len(hdr) < 92 {
		return partitionTableResult{}, fmt.Errorf("GPT 头不完整（%d 字节）", len(hdr))
	}
	headerSize := binary.LittleEndian.Uint32(hdr[12:16])
	if headerSize < 92 || headerSize > sectorSize {
		return partitionTableResult{}, fmt.Errorf("GPT 头长度异常（%d）", headerSize)
	}
	firstUsable := binary.LittleEndian.Uint64(hdr[40:48])
	lastUsable := binary.LittleEndian.Uint64(hdr[48:56])
	entryLBA := binary.LittleEndian.Uint64(hdr[72:80])
	entryCount := binary.LittleEndian.Uint32(hdr[80:84])
	entrySize := binary.LittleEndian.Uint32(hdr[84:88])
	if entrySize < 128 || entryCount == 0 || entryCount > 1024 {
		return partitionTableResult{}, fmt.Errorf("GPT 条目表参数异常（count=%d size=%d）", entryCount, entrySize)
	}
	if firstUsable == 0 || lastUsable < firstUsable {
		return partitionTableResult{}, fmt.Errorf("GPT 可用区间异常（%d..%d）", firstUsable, lastUsable)
	}

	entryOff := int(entryLBA) * sectorSize
	// used 是“已被分区占用”的扇区区间，end 为开区间（与 GPT 的 inclusive last LBA 分开记，
	// 免得每处都 +1）。
	type span struct{ start, end uint64 }
	used := make([]span, 0, entryCount)
	count := 0
	for i := uint32(0); i < entryCount; i++ {
		off := entryOff + int(i)*int(entrySize)
		if off+int(entrySize) > len(raw) {
			break // 头部没读全：已解析的部分仍然有效
		}
		entry := raw[off : off+int(entrySize)]
		if allZero(entry[0:16]) {
			continue // 空条目
		}
		first := binary.LittleEndian.Uint64(entry[32:40])
		last := binary.LittleEndian.Uint64(entry[40:48])
		if last < first {
			continue
		}
		count++
		used = append(used, span{start: first, end: last + 1})
	}

	// 空闲区间 = 可用区间减去已用区间（补集）。空闲区间的意义是“还能建多大的分区”，
	// 所以必须是按扇区精确的，不能拿整盘大小减一下了事（对齐浪费、GPT 头尾都要减掉）。
	sort.Slice(used, func(i, j int) bool { return used[i].start < used[j].start })
	free := []FreeRegion{}
	cursor := firstUsable
	emit := func(from, to uint64) {
		if to > from {
			free = append(free, FreeRegion{
				StartBytes: from * sectorSize,
				SizeBytes:  (to - from) * sectorSize,
			})
		}
	}
	for _, u := range used {
		if u.start > cursor {
			emit(cursor, u.start)
		}
		if u.end > cursor {
			cursor = u.end
		}
	}
	emit(cursor, lastUsable+1)

	return partitionTableResult{Type: TableGPT, Count: count, FreeRegions: free}, nil
}

// sleeping 判断设备是否处于休眠：runtime_status 为 suspended 时读 /dev 会把它唤醒。
//
// dir 是这块设备在 sysfs 里的目录（分区的目录嵌在整盘下面）。
func sleeping(dir string) bool {
	dev := joinPath(dir, "device")
	if s, ok := readFile(joinPath(dev, "power-status")); ok {
		return strings.EqualFold(s, "suspended")
	}
	if s, ok := readFile(joinPath(dev, "power", "runtime_status")); ok {
		return strings.EqualFold(s, "suspended")
	}
	if s, ok := readFile(joinPath(dev, "state")); ok {
		switch strings.ToLower(s) {
		case "suspended", "sleeping", "standby":
			return true
		}
	}
	return false
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// gptEntryName 把 GPT 条目里的 UTF-16LE 名字解出来（调试与未来展示用）。
// 未使用时不参与任何判断，保留它是为了将来别在别处重新实现一遍。
func gptEntryName(entry []byte) string {
	if len(entry) < 128 {
		return ""
	}
	units := make([]uint16, 0, 36)
	for i := 56; i+1 < 128; i += 2 {
		u := binary.LittleEndian.Uint16(entry[i : i+2])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	s := string(utf16.Decode(units))
	if !utf8.ValidString(s) {
		return ""
	}
	return s
}
