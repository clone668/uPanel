package storage

// 超级块读取的测试（P07.3）。
//
// 这一层只做一件事：把设备自己的字节翻译成“这台设备上现在是什么文件系统、
// 卷标是什么、UUID 是什么、有多大”。它是格式化核验唯一的**正面证据**，
// 因此测试用手算好的字节去钉每一个偏移，而不是拿生产代码再算一遍。
//
// 偏移的来源写在 fsprobe.go 的注释里（内核 ext4_fs.h / xfsprogs xfs_sb.h），
// 实测复核见 docs/evidence/P07/2026-10-22-p07.3/measurements.txt。

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// probeFixture 造一个“设备文件”：给定内容的临时文件。
func probeFixture(t *testing.T, content []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sdc1"), content, 0o644); err != nil {
		t.Fatalf("准备假设备失败: %v", err)
	}
	return dir, "sdc1"
}

// probeOnce 用生产读取器读一次夹具。
func probeOnce(t *testing.T, devDir, devID string, sizeBytes uint64) (FilesystemIdentity, error) {
	t.Helper()
	p := DeviceSuperblockProber{Dev: devDir, Timeout: time.Second}
	return p.Probe(context.Background(), devID, sizeBytes)
}

// ---------- 解析 ----------

func TestParseExt4SuperblockWith64BitBlocks(t *testing.T) {
	head := make([]byte, fsProbeBytes)
	binary.LittleEndian.PutUint16(head[ext4MagicOffset:], ext4Magic)
	binary.LittleEndian.PutUint32(head[ext4LogBlockSz:], 2) // 4096 字节
	binary.LittleEndian.PutUint32(head[ext4BlocksLo:], 0x100)
	binary.LittleEndian.PutUint32(head[ext4Incompat:],
		ext4FeatureIncompatExtents|ext4FeatureIncompat64Bit)
	binary.LittleEndian.PutUint32(head[ext4BlocksHi:], 3) // 高位 = 3
	uuid := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	copy(head[ext4UUIDOffset:], uuid)
	copy(head[ext4LabelOffset:], "data disk") // 8 字节，含空格

	id, err := parseFilesystemIdentity(head)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if id.Kind != FilesystemExt4 {
		t.Errorf("Kind = %q，期望 %q", id.Kind, FilesystemExt4)
	}
	if id.UUID != "01234567-89ab-cdef-fedc-ba9876543210" {
		t.Errorf("UUID = %q", id.UUID)
	}
	if id.Label != "data disk" {
		t.Errorf("Label = %q，期望 %q（标签里的空格要保留）", id.Label, "data disk")
	}
	if id.BlockSize != 4096 {
		t.Errorf("BlockSize = %d", id.BlockSize)
	}
	if want := uint64(0x300000100); id.Blocks != want {
		t.Errorf("Blocks = %#x，期望 %#x（64bit 位有效时高低位要拼起来）", id.Blocks, want)
	}
	if id.Bytes != 4096*id.Blocks {
		t.Errorf("Bytes = %d，期望 BlockSize×Blocks", id.Bytes)
	}
}

func TestParseExt4WithoutExtentsIsNotExt4(t *testing.T) {
	// 只有魔数、没有 extents 位：ext2/ext3 也长这样，不能报成 ext4。
	head := make([]byte, fsProbeBytes)
	binary.LittleEndian.PutUint16(head[ext4MagicOffset:], ext4Magic)
	binary.LittleEndian.PutUint32(head[ext4LogBlockSz:], 1)
	binary.LittleEndian.PutUint32(head[ext4BlocksLo:], 1024)

	id, err := parseFilesystemIdentity(head)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if id.Kind != SignatureExt {
		t.Errorf("Kind = %q，期望 %q（ext 家族但不是 ext4）", id.Kind, SignatureExt)
	}
	if !strings.Contains(id.Detail, "extents") {
		t.Errorf("Detail = %q，应当说明缺 extents", id.Detail)
	}
	if !strings.Contains(id.String(), "ext") {
		t.Errorf("String() = %q", id.String())
	}
}

func TestParseExt4RejectsAbsurdBlockSize(t *testing.T) {
	// logBlockSize = 7 意味着 128 KiB 的块：不可能是 ext4，不能拿它算容量。
	head := make([]byte, fsProbeBytes)
	binary.LittleEndian.PutUint16(head[ext4MagicOffset:], ext4Magic)
	binary.LittleEndian.PutUint32(head[ext4LogBlockSz:], 7)
	binary.LittleEndian.PutUint32(head[ext4Incompat:], ext4FeatureIncompatExtents)

	id, err := parseFilesystemIdentity(head)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if id.Kind != SignatureExt {
		t.Errorf("Kind = %q，期望 %q", id.Kind, SignatureExt)
	}
	if id.Bytes != 0 {
		t.Errorf("Bytes = %d，字段异常时不该算出容量", id.Bytes)
	}
	if !strings.Contains(id.Detail, "块大小") {
		t.Errorf("Detail = %q", id.Detail)
	}
}

func TestParseXFSHeader(t *testing.T) {
	head := make([]byte, fsProbeBytes)
	copy(head[xfsMagicOffset:], "XFSB")
	binary.BigEndian.PutUint32(head[xfsBlockSizeOffset:], 4096)
	binary.BigEndian.PutUint64(head[xfsDBlocksOffset:], 262144) // 1 GiB
	uuid := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}
	copy(head[xfsUUIDOffset:], uuid)
	copy(head[xfsLabelOffset:], "xfsvol") // 6 字节

	id, err := parseFilesystemIdentity(head)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if id.Kind != FilesystemXFS {
		t.Errorf("Kind = %q，期望 %q", id.Kind, FilesystemXFS)
	}
	if id.UUID != "aabbccdd-eeff-0011-2233-445566778899" {
		t.Errorf("UUID = %q（XFS 的 UUID 是大端，不带字节序翻转）", id.UUID)
	}
	if id.Label != "xfsvol" {
		t.Errorf("Label = %q", id.Label)
	}
	if id.Bytes != uint64(1)<<30 {
		t.Errorf("Bytes = %d，期望 1 GiB", id.Bytes)
	}
}

func TestParsePrefersXFSMagic(t *testing.T) {
	// 两个魔数同时在（真实盘上不会发生）：按“认不出来”之外的唯一确定答案处理——
	// 0 字节处的 XFSB 更具体，优先。
	head := make([]byte, fsProbeBytes)
	copy(head[xfsMagicOffset:], "XFSB")
	binary.BigEndian.PutUint32(head[xfsBlockSizeOffset:], 4096)
	binary.BigEndian.PutUint64(head[xfsDBlocksOffset:], 100)
	binary.LittleEndian.PutUint16(head[ext4MagicOffset:], ext4Magic)

	id, err := parseFilesystemIdentity(head)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if id.Kind != FilesystemXFS {
		t.Errorf("Kind = %q，期望 %q", id.Kind, FilesystemXFS)
	}
}

func TestParseBlankDeviceIsUnknown(t *testing.T) {
	id, err := parseFilesystemIdentity(make([]byte, fsProbeBytes))
	if err != nil {
		t.Fatalf("空设备不该报错: %v", err)
	}
	if id.Kind != "" || id.UUID != "" || id.Label != "" || id.Bytes != 0 {
		t.Errorf("空设备应当什么都认不出来，得到 %+v", id)
	}
	if id.String() != "unknown" {
		t.Errorf("String() = %q", id.String())
	}
}

func TestParseRejectsTooShortHead(t *testing.T) {
	if _, err := parseFilesystemIdentity(make([]byte, 511)); err == nil {
		t.Fatal("不足一个扇区的头部却解析成功")
	}
}

func TestFormatUUIDAllZeroIsEmpty(t *testing.T) {
	if got := formatUUID(make([]byte, 16)); got != "" {
		t.Errorf("全零 UUID = %q，期望空串（“没有”而不是“全零”）", got)
	}
	if got := formatUUID([]byte{1}); got != "" {
		t.Errorf("短 UUID = %q，期望空串", got)
	}
}

func TestTrimNulKeepsInnerSpaces(t *testing.T) {
	cases := map[string]string{
		"data\x00\x00\x00\x00":     "data",
		"data disk\x00\x00":        "data disk",
		"\x00\x00\x00\x00":         "",
		"all 16 bytes\x00\x00\x02": "all 16 bytes\x00\x00\x02", // 中间的 NUL 不动
	}
	for in, want := range cases {
		if got := trimNul([]byte(in)); got != want {
			t.Errorf("trimNul(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------- 真实读取路径 ----------

func TestDeviceSuperblockProberReadsRealBytes(t *testing.T) {
	head := ext4Head([16]byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		"fixture", 4<<20)
	dir, dev := probeFixture(t, head)

	id, err := probeOnce(t, dir, dev, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if id.Kind != FilesystemExt4 || id.Label != "fixture" {
		t.Errorf("读到 %+v", id)
	}
	if id.UUID != "deadbeef-0102-0304-0506-0708090a0b0c" {
		t.Errorf("UUID = %q", id.UUID)
	}
}

func TestDeviceSuperblockProberXFS(t *testing.T) {
	head := xfsHead([16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		"xv", 1<<20)
	dir, dev := probeFixture(t, head)

	id, err := probeOnce(t, dir, dev, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if id.Kind != FilesystemXFS || id.Label != "xv" {
		t.Errorf("读到 %+v", id)
	}
}

func TestDeviceSuperblockProberRejectsTinyDevice(t *testing.T) {
	dir, dev := probeFixture(t, make([]byte, 100))
	_, err := probeOnce(t, dir, dev, 0)
	if err == nil || !strings.Contains(err.Error(), "放不下") {
		t.Fatalf("错误 = %v，应当明说放不下一个超级块窗口", err)
	}
}

// TestDeviceSuperblockProberHonoursCallerSize 校验调用方给的大小是权威的。
//
// 生产路径上大小来自 sysfs/分区表（分区的真实容量），不是文件长度：夹具文件比它小
// 时按短读处理，而不是把文件长度当成设备容量。
func TestDeviceSuperblockProberHonoursCallerSize(t *testing.T) {
	dir, dev := probeFixture(t, make([]byte, 100))
	_, err := probeOnce(t, dir, dev, 1<<30)
	if err == nil || !strings.Contains(err.Error(), "不足一个扇区") {
		t.Fatalf("错误 = %v，短读之后头部不足一个扇区应当报错", err)
	}
}

// TestDeviceSuperblockProberToleratesShortRead 校验短读时窗口内的字段照样有效。
//
// 真实设备不会短读；但“读到 2048 字节就结束”不该让整次核验失败——
// UUID 与标签都在第一个 2 KiB 里，只有 ext4 的 blocks_hi 会读成 0。
// 调用方给的大小（4096）是权威的，因此这里不会先被大小检查拦下。
func TestDeviceSuperblockProberToleratesShortRead(t *testing.T) {
	head := ext4Head([16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		"short", 4<<20)
	head = head[:2048] // 只留 2 KiB
	dir, dev := probeFixture(t, head)

	id, err := probeOnce(t, dir, dev, fsProbeBytes)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if id.Kind != FilesystemExt4 || id.Label != "short" {
		t.Errorf("读到 %+v", id)
	}
	if id.UUID != "01020304-0506-0708-090a-0b0c0d0e0f10" {
		t.Errorf("UUID = %q", id.UUID)
	}
}

func TestDeviceSuperblockProberMissingDevice(t *testing.T) {
	dir := t.TempDir()
	_, err := probeOnce(t, dir, "sdz9", 0)
	if err == nil || !strings.Contains(err.Error(), "打开") {
		t.Fatalf("错误 = %v，应当报打开失败", err)
	}
}

func TestDeviceSuperblockProberCancelledContext(t *testing.T) {
	head := ext4Head([16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, "x", 1<<20)
	dir, dev := probeFixture(t, head)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := DeviceSuperblockProber{Dev: dir, Timeout: time.Second}
	_, err := p.Probe(ctx, dev, 0)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，应当能被 errors.Is(err, context.Canceled) 认出", err)
	}
}

func TestDeviceSuperblockProberRejectsEmptyDeviceID(t *testing.T) {
	p := DeviceSuperblockProber{Dev: t.TempDir(), Timeout: time.Second}
	if _, err := p.Probe(context.Background(), "", 0); err == nil {
		t.Fatal("空设备名却读取成功")
	}
}
