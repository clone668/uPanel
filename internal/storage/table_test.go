package storage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 分区表解析的用例分两类：
//
//  1. 真实采集的整盘头部（disks/sda.bin 等）——证明解析能处理真机字节；
//  2. 手工构造的坏头部——GPT 损坏、截断、MBR、全零、乱数据。
//     坏头部只能手写（真机上造不出来），这是**反例**，不是“我以为的真机样本”。

func fixtureDisk(t *testing.T, fixture, name string) []byte {
	t.Helper()
	dir, err := fixtureDir(fixture)
	if err != nil {
		t.Fatalf("%v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "disks", name+".bin"))
	if err != nil {
		t.Fatalf("读夹具 %s/disks/%s.bin 失败: %v", fixture, name, err)
	}
	return raw
}

func TestParsePartitionTableRealCapture(t *testing.T) {
	raw := fixtureDisk(t, "storage-2404", "sda")
	res, err := parsePartitionTable(raw)
	if err != nil {
		t.Fatalf("解析真实 sda 头部失败: %v", err)
	}
	if res.Type != TableGPT {
		t.Fatalf("Type = %q，想要 gpt", res.Type)
	}
	if res.Count != 4 {
		t.Errorf("盘上条目数 = %d，想要 4", res.Count)
	}
	if len(res.FreeRegions) == 0 {
		t.Errorf("sda 没有被分区占满，应算得出空闲区")
	}

	// 空白盘：前 34 个扇区全零。
	for _, name := range []string{"sdb", "sdc", "sdd"} {
		res, err := parsePartitionTable(fixtureDisk(t, "storage-2404", name))
		if err != nil {
			t.Errorf("%s: 解析全零头部返回错误: %v", name, err)
			continue
		}
		if res.Type != TableNone {
			t.Errorf("%s: Type = %q，想要 none", name, res.Type)
		}
		if res.Count != 0 || len(res.FreeRegions) != 0 {
			t.Errorf("%s: 全零盘不该有分区或空闲区: %+v", name, res)
		}
	}

	// 22.04 的 sda 也是 GPT，但只有 3 个分区（没有独立的 /boot，内核直接放在 / 上）。
	res2204, err := parsePartitionTable(fixtureDisk(t, "storage-2204", "sda"))
	if err != nil {
		t.Fatalf("解析 22.04 sda 头部失败: %v", err)
	}
	if res2204.Type != TableGPT || res2204.Count != 3 {
		t.Errorf("22.04 sda: %+v，想要 gpt 且 3 个分区", res2204)
	}
}

// buildGPT 造一个最小的 GPT 头部：LBA0 保护性 MBR，LBA1 头，LBA2 起条目表。
// parts 是 [first, last] 的扇区区间（GPT 的 last 是 inclusive）。
func buildGPT(totalSectors uint64, parts [][2]uint64, opts ...func(hdr, entries []byte)) []byte {
	raw := make([]byte, 34*512)
	// LBA0：保护性 MBR。
	raw[446+4] = 0xEE
	raw[510], raw[511] = 0x55, 0xAA
	// LBA1：GPT 头。
	hdr := raw[512 : 512+92]
	copy(hdr[0:8], "EFI PART")
	binary.LittleEndian.PutUint32(hdr[12:16], 92)
	binary.LittleEndian.PutUint64(hdr[40:48], 34)              // firstUsable
	binary.LittleEndian.PutUint64(hdr[48:56], totalSectors-34) // lastUsable
	binary.LittleEndian.PutUint64(hdr[72:80], 2)               // entryLBA
	binary.LittleEndian.PutUint32(hdr[80:84], 128)             // entryCount
	binary.LittleEndian.PutUint32(hdr[84:88], 128)             // entrySize
	// LBA2..33：条目表。
	entries := raw[2*512:]
	for i, p := range parts {
		e := entries[i*128 : (i+1)*128]
		copy(e[0:16], []byte{0x11, 0x22, 0x33, 0x44}) // 非空的类型 GUID
		binary.LittleEndian.PutUint64(e[32:40], p[0])
		binary.LittleEndian.PutUint64(e[40:48], p[1])
	}
	for _, o := range opts {
		o(hdr, entries)
	}
	return raw
}

func TestParsePartitionTableSyntheticGPT(t *testing.T) {
	// 一快盘：可用区间 34..966（共 1000 扇区），两个分区把它切成三块空闲区。
	raw := buildGPT(1000, [][2]uint64{{100, 199}, {400, 499}})
	res, err := parsePartitionTable(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if res.Type != TableGPT || res.Count != 2 {
		t.Fatalf("%+v，想要 gpt 且 2 个分区", res)
	}
	want := []FreeRegion{
		{StartBytes: 34 * 512, SizeBytes: (100 - 34) * 512},
		{StartBytes: 200 * 512, SizeBytes: (400 - 200) * 512},
		{StartBytes: 500 * 512, SizeBytes: (966 + 1 - 500) * 512},
	}
	if len(res.FreeRegions) != len(want) {
		t.Fatalf("空闲区 = %+v，想要 %+v", res.FreeRegions, want)
	}
	for i := range want {
		if res.FreeRegions[i] != want[i] {
			t.Errorf("空闲区 #%d = %+v，想要 %+v", i, res.FreeRegions[i], want[i])
		}
	}
}

func TestParsePartitionTableBrokenGPT(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		err  string
	}{
		{
			name: "GPT 头长度异常",
			raw: buildGPT(1000, nil, func(hdr, _ []byte) {
				binary.LittleEndian.PutUint32(hdr[12:16], 8)
			}),
			err: "头长度异常",
		},
		{
			name: "条目表参数异常（条目数为 0）",
			raw: buildGPT(1000, nil, func(hdr, _ []byte) {
				binary.LittleEndian.PutUint32(hdr[80:84], 0)
			}),
			err: "条目表参数异常",
		},
		{
			name: "条目表参数异常（条目尺寸 16 字节）",
			raw: buildGPT(1000, nil, func(hdr, _ []byte) {
				binary.LittleEndian.PutUint32(hdr[84:88], 16)
			}),
			err: "条目表参数异常",
		},
		{
			name: "可用区间反了",
			raw: buildGPT(1000, nil, func(hdr, _ []byte) {
				binary.LittleEndian.PutUint64(hdr[40:48], 900)
				binary.LittleEndian.PutUint64(hdr[48:56], 100)
			}),
			err: "可用区间异常",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parsePartitionTable(c.raw)
			if err == nil {
				t.Fatalf("坏 GPT 头没报错")
			}
			if !strings.Contains(err.Error(), c.err) {
				t.Errorf("错误 = %q，想要包含 %q", err.Error(), c.err)
			}
		})
	}
}

func TestParsePartitionTableMBR(t *testing.T) {
	raw := make([]byte, 512)
	raw[446+4] = 0x83    // Linux
	raw[446+16+4] = 0x82 // swap
	raw[510], raw[511] = 0x55, 0xAA
	res, err := parsePartitionTable(raw)
	if err != nil {
		t.Fatalf("解析 MBR 失败: %v", err)
	}
	if res.Type != TableMBR {
		t.Errorf("Type = %q，想要 mbr", res.Type)
	}
	if res.Count != 2 {
		t.Errorf("Count = %d，想要 2", res.Count)
	}
	// MBR 不解析空闲区（没有可靠的尾部边界），必须是空数组而不是 null。
	if res.FreeRegions == nil {
		t.Errorf("FreeRegions 为 nil，约定给空数组")
	}
}

func TestParsePartitionTableUnreadable(t *testing.T) {
	if _, err := parsePartitionTable(make([]byte, 100)); err == nil {
		t.Errorf("不足一个扇区却没报错")
	}
	// 有数据但不是分区表：type=unknown，并且报错（不猜）。
	raw := make([]byte, 512)
	for i := range raw {
		raw[i] = 0x5A
	}
	raw[510], raw[511] = 0x00, 0x00
	res, err := parsePartitionTable(raw)
	if err == nil {
		t.Errorf("乱数据没报错")
	}
	if res.Type != TableUnknown {
		t.Errorf("Type = %q，想要 unknown", res.Type)
	}
}

// TestProbeSkipsWithoutWaking 钉住“不为了拿信息去唤醒磁盘”：
// 设备不在线或休眠时，必须标记 skipped，且**不能**调用 ReadDisk。
func TestProbeSkipsWithoutWaking(t *testing.T) {
	dr := &fixtureDiskReader{dir: t.TempDir()}
	root := Root{Sys: t.TempDir(), Proc: t.TempDir(), Disks: dr}

	dev := &Device{ID: "sdx", Name: "sdx", Kind: KindDisk, Online: false}
	probePartitionTable(t.Context(), root, t.TempDir(), dev)
	if len(dr.reads) != 0 {
		t.Errorf("设备离线时仍然读了 /dev：%v", dr.reads)
	}
	if dev.PartitionTable == nil || dev.PartitionTable.Source != "skipped" {
		t.Errorf("离线设备的分区表 = %+v，想要 source=skipped", dev.PartitionTable)
	}
	if dev.PartitionTable.Warning == "" {
		t.Errorf("跳过时必须说明原因，warning 为空")
	}

	// 休眠属性在 sysfs 里 → 也不读。
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device", "power-status"), []byte("suspended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dev2 := &Device{ID: "sdy", Name: "sdy", Kind: KindDisk, Online: true}
	probePartitionTable(t.Context(), root, dir, dev2)
	if len(dr.reads) != 0 {
		t.Errorf("设备休眠时仍然读了 /dev：%v", dr.reads)
	}
	if dev2.PartitionTable.Source != "skipped" {
		t.Errorf("休眠设备的分区表 source = %q，想要 skipped", dev2.PartitionTable.Source)
	}

	// 读失败也照样给 skipped + 原因，不能悄悄变成 gpt。
	dev3 := &Device{ID: "sdz", Name: "sdz", Kind: KindDisk, Online: true}
	probePartitionTable(t.Context(), root, t.TempDir(), dev3)
	if dev3.PartitionTable.Source != "skipped" || dev3.PartitionTable.Type != TableUnknown {
		t.Errorf("读失败时 = %+v，想要 type=unknown source=skipped", dev3.PartitionTable)
	}
}
