package storage

// 签名识别与读取的测试（P07.1）。
//
// 两类输入：
//   - **手工构造的字节**：真实系统上没法让 mkfs 产出每一种签名，也没法让一块真盘
//     同时是 btrfs 和 swap。分类器的每个分支都要有一个精确到偏移的用例，
//     因为错一个字节就会把“有数据”的盘判成空白盘。
//   - **写进临时文件的设备映像**：读取器（含尾部窗口、超时、休眠检查）必须能在
//     不碰真实 /dev 的前提下验证。

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 各签名的构造器：都返回“一台 64 MiB 设备”的头/尾内容。
const testDeviceBytes = uint64(64 << 20)

func headOf(off int, magic []byte) []byte {
	buf := make([]byte, headProbeBytes)
	copy(buf[off:], magic)
	return buf
}

func TestClassifySignature(t *testing.T) {
	tailEnd := make([]byte, tailProbeBytes)
	copy(tailEnd[len(tailEnd)-10:], magicSwapV1)

	cases := []struct {
		name string
		head []byte
		tail []byte
		kind string
	}{
		{"全零 = 空白", make([]byte, headProbeBytes), make([]byte, tailProbeBytes), SignatureBlank},
		{"LUKS1", headOf(0, append(append([]byte{}, magicLUKS...), 0x00, 0x01)), nil, SignatureLUKS},
		{"LUKS2", headOf(0, append(append([]byte{}, magicLUKS...), 0x00, 0x02)), nil, SignatureLUKS},
		{"LVM2 PV", headOf(0x200, magicLVM2), nil, SignatureLVM2},
		{"XFS", headOf(0, magicXFS), nil, SignatureXFS},
		{"Btrfs", headOf(0x10040, magicBtrfs), nil, SignatureBtrfs},
		{"md 1.2", headOf(0x1000, magicMDRaid), nil, SignatureMD},
		{"md 1.1", headOf(0, magicMDRaid), nil, SignatureMD},
		{"swap v1 在最末尾", make([]byte, headProbeBytes), tailEnd, SignatureSwap},
		// 真实现场：整盘 mkswap 把 SWAPSPACE2 写在第一个页面的末 10 字节（4096-10），
		// 盘尾全零。这一条就是 2026-10-21 在验收机上漏判的那个现场。
		{"swap v1 在 4 KiB 页面末（整盘 mkswap 的真实位置）",
			headOf(swapMagic4K, magicSwapV1), nil, SignatureSwap},
		{"swap v1 在 64 KiB 页面末", headOf(swapMagic64K, magicSwapV1), nil, SignatureSwap},
		{"老式 swap v0 在 4 KiB 页面末", headOf(swapMagic4K, magicSwapV0), nil, SignatureSwap},
		// 反例：同样十个字节出现在别处（数据盘里恰好存了这个字符串）不能算交换区，
		// 否则“盲扫头部窗口”的实现会被这条抓住。
		{"SWAPSPACE2 出现在非页面边界", headOf(0x3000, magicSwapV1), nil, SignatureUnknown},
		{"NTFS", headOf(3, magicNTFS), nil, SignatureNTFS},
		{"exFAT", headOf(3, magicExFAT), nil, SignatureExFAT},
		{"ext4 超级块", extHead(0xEF53), nil, SignatureExt},
		{"认不出的非零内容", headOf(12345, []byte("random bytes")), nil, SignatureUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := tc.tail
			if tail == nil {
				tail = make([]byte, tailProbeBytes)
			}
			got := classifySignature(tc.head, tail, testDeviceBytes)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q（detail=%q），期望 %q", got.Kind, got.Detail, tc.kind)
			}
		})
	}
}

// extHead 造一个带给定魔数的 ext 超级块（超级块在 1024，魔数是它内部的第 56 字节）。
func extHead(magic uint16) []byte {
	buf := make([]byte, headProbeBytes)
	binary.LittleEndian.PutUint16(buf[0x438:0x43a], magic)
	return buf
}

// TestClassifySignatureExtMagicOnly 校验魔数位置精确：魔数错开一个字节就不能再判成 ext。
// 这条断言存在的理由很直接——GPT 的分区条目表就在 512..17408 之间，把 ext 魔数
// 放宽到“某处出现 0x53EF”会让一块 GPT 盘被误判成 ext4，进而挡住合法操作。
func TestClassifySignatureExtMagicOnly(t *testing.T) {
	buf := make([]byte, headProbeBytes)
	binary.LittleEndian.PutUint16(buf[0x439:0x43b], 0xEF53)
	if got := classifySignature(buf, make([]byte, tailProbeBytes), testDeviceBytes); got.Kind == SignatureExt {
		t.Fatalf("魔数偏移错了却被判成 ext：%+v", got)
	}
}

// TestClassifySignatureVATNeedsBPB 校验 FAT 判定不只看 0x55AA：
// 带分区表的盘（含 GPT 的保护性 MBR）都有 0x55AA，它们不是 FAT 文件系统。
func TestClassifySignatureVFATNeedsBPB(t *testing.T) {
	// GPT 保护性 MBR：0x55AA + 一条 type=0xEE 的条目，但 BIOS 参数块是空的。
	mbr := make([]byte, headProbeBytes)
	mbr[510], mbr[511] = 0x55, 0xAA
	mbr[446+4] = 0xEE
	if got := classifySignature(mbr, make([]byte, tailProbeBytes), testDeviceBytes); got.Kind == SignatureVFAT {
		t.Fatalf("GPT 保护性 MBR 被判成 FAT：%+v", got)
	}

	// 真正的 FAT16 引导扇区：参数块齐全。
	fat := make([]byte, headProbeBytes)
	binary.LittleEndian.PutUint16(fat[11:13], 512) // 每扇区字节
	fat[13] = 4                                    // 每簇扇区
	binary.LittleEndian.PutUint16(fat[14:16], 4)   // 保留扇区
	fat[16] = 2                                    // FAT 表个数
	fat[21] = 0xF8                                 // 介质描述符
	fat[510], fat[511] = 0x55, 0xAA
	if got := classifySignature(fat, make([]byte, tailProbeBytes), testDeviceBytes); got.Kind != SignatureVFAT {
		t.Fatalf("完整 FAT 引导扇区没被判成 vfat：%+v", got)
	}
}

// TestClassifySignatureTailWinsOverHead 校验放在尾部的签名能被发现：
// 整盘 mkfs 的 ext4 头在 1024 字节处（LBA0 全零），而交换区签名只在最后一页。
func TestClassifySignatureTailWinsOverHead(t *testing.T) {
	tail := make([]byte, tailProbeBytes)
	copy(tail[len(tail)-10:], magicSwapV1)
	got := classifySignature(make([]byte, headProbeBytes), tail, testDeviceBytes)
	if got.Kind != SignatureSwap {
		t.Fatalf("尾部签名没被发现：%+v", got)
	}
}

// writeImage 把一个字节切片写成一个“设备文件”，不足的部分补零。
func writeImage(t *testing.T, dir, name string, size int64, writes map[int64][]byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建设备映像失败: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("设置映像大小失败: %v", err)
	}
	for off, data := range writes {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatalf("写入映像失败: %v", err)
		}
	}
	return path
}

func TestDeviceSignatureProber(t *testing.T) {
	dir := t.TempDir()
	const size = int64(4 << 20)

	writeImage(t, dir, "blank", size, nil)
	writeImage(t, dir, "ext4", size, map[int64][]byte{
		0x438: {0x53, 0xEF},
	})
	writeImage(t, dir, "swap", size, map[int64][]byte{
		size - 10: magicSwapV1,
	})
	// mkswap 在真实块设备上的写法：第一页末 10 字节一处，盘尾什么都不写。
	writeImage(t, dir, "realswap", size, map[int64][]byte{
		1024:        make([]byte, 16), // 交换区头部的零散字段，制造“头部非零”
		swapMagic4K: magicSwapV1,
	})
	writeImage(t, dir, "md0", size, map[int64][]byte{
		0x1000: magicMDRaid,
	})

	prober := DeviceSignatureProber{Dev: dir, Timeout: 2 * time.Second}
	cases := map[string]string{
		"blank":    SignatureBlank,
		"ext4":     SignatureExt,
		"swap":     SignatureSwap,
		"realswap": SignatureSwap,
		"md0":      SignatureMD,
	}
	for name, want := range cases {
		got, err := prober.Probe(context.Background(), name, uint64(size))
		if err != nil {
			t.Fatalf("%s: 读取失败: %v", name, err)
		}
		if got.Kind != want {
			t.Errorf("%s: kind = %q，期望 %q", name, got.Kind, want)
		}
	}
}

// TestDeviceSignatureProberShortDevice 校验比探测窗口短的设备也能读（小分区）。
func TestDeviceSignatureProberShortDevice(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, dir, "tiny", 4096, map[int64][]byte{0x438: {0x53, 0xEF}})
	prober := DeviceSignatureProber{Dev: dir}
	got, err := prober.Probe(context.Background(), "tiny", 4096)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Kind != SignatureExt {
		t.Fatalf("kind = %q，期望 ext", got.Kind)
	}
}

// TestDeviceSignatureProberTooSmall 校验不足一个扇区的目标直接报错，
// 而不是返回“空白”——那会让一块读不到的设备看起来可以初始化。
func TestDeviceSignatureProberTooSmall(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, dir, "empty", 0, nil)
	prober := DeviceSignatureProber{Dev: dir}
	if _, err := prober.Probe(context.Background(), "empty", 0); err == nil {
		t.Fatal("零字节设备没有报错")
	}
}

// TestDeviceSignatureProberSleeping 校验休眠设备根本不会被打开：
// 读 /dev 会唤醒盘，PLAN.md 第 8 节不允许。
func TestDeviceSignatureProberSleeping(t *testing.T) {
	dir := t.TempDir()
	// 设备节点故意不存在：如果实现先尝试打开，报的会是“打开失败”，
	// 只有在检查休眠之后才打开，才会得到“处于休眠”这个错误。
	sys := filepath.Join(dir, "sys")
	mustMkdirAll(t, filepath.Join(sys, "block", "sdz", "device"))
	mustWriteFile(t, filepath.Join(sys, "block", "sdz", "device", "state"), "suspended\n")

	prober := DeviceSignatureProber{Dev: filepath.Join(dir, "dev"), Sys: sys}
	_, err := prober.Probe(context.Background(), "sdz", 1<<20)
	if err == nil {
		t.Fatal("休眠设备没有报错")
	}
	if !strings.Contains(err.Error(), "休眠") {
		t.Fatalf("错误应说明是休眠导致的：%v", err)
	}
}

// TestDeviceSysDirPartition 校验分区目录能被找到（分区目录嵌在整盘下面）。
func TestDeviceSysDirPartition(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "block", "sdb", "sdb1"))
	mustMkdirAll(t, filepath.Join(dir, "block", "sdc"))
	if got := deviceSysDir(dir, "sdb1"); got == "" || !strings.HasSuffix(got, filepath.Join("sdb", "sdb1")) {
		t.Fatalf("deviceSysDir(sdb1) = %q", got)
	}
	if got := deviceSysDir(dir, "sdc"); got == "" || !strings.HasSuffix(got, "sdc") {
		t.Fatalf("deviceSysDir(sdc) = %q", got)
	}
	if got := deviceSysDir(dir, "nope"); got != "" {
		t.Fatalf("不存在的设备返回了 %q", got)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("建目录 %s 失败: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s 失败: %v", path, err)
	}
}
