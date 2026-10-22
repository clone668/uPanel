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
	if parsed.Type == TableGPT {
		// 读到 GPT 就一定给出这两个键：条目表是空的时候给空数组，不给“没有这个键”。
		// “读到了，零条”与“没读到”对写后核验是完全不同的两件事（后者要报错）。
		table.DiskGUID = parsed.DiskGUID
		entries := parsed.Entries
		if entries == nil {
			entries = []GPTEntry{}
		}
		table.DiskEntries = &entries
	}

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
	// DiskGUID 是 GPT 头的 Disk GUID（字符串形态，只用来说明与核验，不参与判断）。
	DiskGUID string
	// Entries 是逐条解出来的 GPT 条目。采集侧不用它（只要数量与空闲区），
	// P07.2 的写后核验要拿它逐条比对（起点、大小、名字、类型）。
	Entries []GPTEntry
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
	entries := make([]GPTEntry, 0, 8)
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
		// 条目号就是内核给分区的编号：GPT 条目表里的第 i 个槽位（从 1 开始）。
		// sfdisk 的 `--delete <dev> <n>` 也按这个编号删，且删掉中间一条**不会**重排，
		// 因此编号是核验“删的是不是那一条”的唯一依据。
		entries = append(entries, GPTEntry{
			Number:     int(i) + 1,
			StartBytes: first * sectorSize,
			SizeBytes:  (last + 1 - first) * sectorSize,
			TypeGUID:   formatGPTGUID(entry[0:16]),
			Name:       gptEntryName(entry),
		})
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

	return partitionTableResult{
		Type:        TableGPT,
		Count:       count,
		FreeRegions: free,
		DiskGUID:    formatGPTGUID(hdr[56:72]),
		Entries:     entries,
	}, nil
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

// gptEntryName 把 GPT 条目里的 UTF-16LE 名字解出来。
//
// 采集侧只把它用于核验（P07.2 写后读回比对名字）：名字是用户输入，必须逐字确认
// 落到了盘上的是同一串，而不是"看起来写进去了"。
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

// GPTEntry 是一条 GPT 分区条目（从盘上的条目表解出来的原始形态）。
//
// 它与 Partition（来自 sysfs 的内核视图）是**两个来源**：写动作的结果核验必须两边
// 都对上才算 verified——只看盘上的字节，会漏掉"内核根本没重读分区表"这种情况；
// 只看内核，会漏掉"盘上写坏了但内核还拿着旧表"。
type GPTEntry struct {
	// Number 是条目表里的槽位号（从 1 开始），也就是内核给分区的编号。
	Number int `json:"number"`
	// StartBytes / SizeBytes 是分区起点与大小（字节，512 字节扇区换算）。
	StartBytes uint64 `json:"start_bytes,string"`
	SizeBytes  uint64 `json:"size_bytes,string"`
	// TypeGUID 是分区类型 GUID 的规范大写形态（例如 Linux filesystem 的 0FC63DAF-…）。
	TypeGUID string `json:"type_guid"`
	// Name 是条目里 UTF-16LE 的分区名（空串表示名字为空，不是"解析失败"——
	// 解析失败由调用方按"名字不符"处理，宁可判成不一致）。
	Name string `json:"name"`
}

// gptTypeLinuxFilesystem 是 Linux 文件系统分区的类型 GUID（sfdisk 的 `type=linux`）。
//
// 固定在代码里而不是让调用方指定：P07.2 只允许建这一类分区，参数里不提供类型选择——
// 有选项就有"把分区标成 EFI 系统分区"这种用法，而那会改变引导行为。
const gptTypeLinuxFilesystem = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"

// formatGPTGUID 把 GPT 的 16 字节 GUID（混合字节序）转成规范文本形态。
//
// GPT 存的是 UEFI 的混合字节序：前三段小端、后两段按原顺序。直接按大端打印会得到
// 一个"看起来像 GUID 但永远对不上"的字符串，所以必须按规范拼。
func formatGPTGUID(b []byte) string {
	if len(b) < 16 {
		return ""
	}
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		binary.LittleEndian.Uint32(b[0:4]),
		binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]),
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}
