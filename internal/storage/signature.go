package storage

// 设备签名识别（P07.1）。
//
// 为什么需要它：`parsePartitionTable` 只看最前面 34 个扇区，而“直接对整盘 mkfs”
// 的做法**不会**在 LBA0 留下任何东西——ext4 的超级块在 1024 字节处，LBA0 依然是全零。
// 于是“首扇区全零”只能说明没有分区表，不能说明盘上没数据。PLAN.md 第 9.1 节要求
// “GPT 初始化只允许没有分区、文件系统签名及其他占用的空白盘”，所以必须有这一层。
//
// 规则：
//   - 只认文件系统/卷管理层写下的**固定魔数**，不做内容猜测。认不出来的非零内容
//     报 unknown（“有东西，但我不认识”），绝不报 blank。
//   - 只读头部 68 KiB 与尾部 16 KiB。整盘扫描既慢又没有必要：所有已知的签名都在这两个位置。
//   - 不唤醒休眠设备。判断由 DeviceSignatureProber 先做（与分区表探测同一原则）。
//   - 读不出来就是读不出来：返回错误，调用方据此拒绝，而不是当成“空的”。

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
)

// 签名种类闭集。
const (
	// SignatureBlank 表示读到的头部与尾部全零：没有分区表、没有文件系统签名、没有卷管理层。
	SignatureBlank = "blank"
	// SignatureExt 是 ext2/ext3/ext4 共用的超级块魔数（0xEF53）。
	SignatureExt = "ext"
	// SignatureXFS 是 XFS 的魔数（"XFSB"）。
	SignatureXFS = "xfs"
	// SignatureBtrfs 是 Btrfs 的魔数（"_BHRfS_M"，超级块在 64 KiB 处）。
	SignatureBtrfs = "btrfs"
	// SignatureVFAT / SignatureNTFS / SignatureExFAT 是三种常见 Windows 文件系统。
	SignatureVFAT  = "vfat"
	SignatureNTFS  = "ntfs"
	SignatureExFAT = "exfat"
	// SignatureSwap 是交换区签名（"SWAPSPACE2"/"SWAP-SPACE"）。
	// 位置是**第一个页面的末 10 字节**，不是盘尾：2026-10-21 在验收机上实测，
	// 整盘 mkswap 之后 SWAPSPACE2 落在偏移 4086（= 4096-10），盘尾 64 KiB 全零；
	// libblkid 报的也是 offset 0xff6。早期实现只扫尾部，于是把交换区报成“无法识别”。
	SignatureSwap = "swap"
	// SignatureLUKS 是 LUKS 加密头（"LUKS\xba\xbe"）。
	SignatureLUKS = "luks"
	// SignatureLVM2 是 LVM2 物理卷标签（"LABELONE"，在 512 字节处）。
	SignatureLVM2 = "lvm2"
	// SignatureMD 是 Linux 软 RAID 超级块（magic 0xA92B4EFC）。
	SignatureMD = "md"
	// SignatureUnknown 表示有非零内容，但不属于任何已知签名。
	SignatureUnknown = "unknown"
)

// 签名魔数。偏移都是相对这台设备自己的 0 字节（分区设备读到的是它自己的起点）。
var (
	magicLUKS  = []byte("LUKS\xba\xbe")
	magicLVM2  = []byte("LABELONE")
	magicXFS   = []byte("XFSB")
	magicBtrfs = []byte("_BHRfS_M")
	magicNTFS  = []byte("NTFS    ")
	magicExFAT = []byte("EXFAT   ")
	// magicSwapV1 是 2.6 之后的内核写下的签名；magicSwapV0 是更老的一代。
	magicSwapV1 = []byte("SWAPSPACE2")
	magicSwapV0 = []byte("SWAP-SPACE")
	// magicMDRaid 是 md 1.x 超级块的魔数（小端 0xA92B4EFC）。
	magicMDRaid = []byte{0xFC, 0x4E, 0x2B, 0xA9}
)

// 读取窗口。
const (
	// headProbeBytes 覆盖：LUKS(0)、md1.x(0/0x1000/0x2000/0x8000/0x10000)、LVM2(0x200)、
	// ext(0x438)、btrfs(0x10040)、NTFS/exFAT(3)。0x11000 = 68 KiB。
	headProbeBytes = 0x11000
	// tailProbeBytes 覆盖：md 1.0 与 0.90 的尾部超级块。
	// 交换区**不在**这里（它在第一个页面的末 10 字节，见 swapMagic4K），但尾部扫描
	// 保留着：旧工具与个别文件型交换区仍然会把副本写到盘尾，多看一眼不花什么代价。
	tailProbeBytes = 16 * 1024
)

// 交换区魔数的两个位置：4 KiB 页面的末 10 字节与 64 KiB 页面的末 10 字节。
// 两台机器上“同一个”交换区落在哪个位置取决于内核页大小，两个都要查（都在头部窗口内）。
const (
	swapMagic4K  = 4096 - 10
	swapMagic64K = 65536 - 10
)

// Signature 是一次签名识别的结论。
type Signature struct {
	// Kind 是闭集里的一个值。
	Kind string `json:"kind"`
	// Detail 是给人读的补充（例如 LUKS 版本、md 元数据版本）。
	Detail string `json:"detail,omitempty"`
}

// String 返回调试用的短描述。
func (s Signature) String() string {
	if s.Detail == "" {
		return s.Kind
	}
	return s.Kind + "(" + s.Detail + ")"
}

// SignatureProber 读取一台设备上的文件系统/卷签名。
//
// 实现必须是**只读**的：不写、不 flush、不挂载，只用 ReadAt 取字节。
// devID 是内核设备名；sizeBytes 是采集到的容量（0 表示调用方不知道，由实现自行判断）。
type SignatureProber interface {
	Probe(ctx context.Context, devID string, sizeBytes uint64) (Signature, error)
}

// classifySignature 从头部与尾部字节判断签名。sizeBytes 用于判断设备是否小到
// 取不到尾部窗口（那时尾部检查跳过，但头部检查照做）。
//
// 顺序是有讲究的：先认**卷管理层**（LUKS/LVM2/md），再认文件系统。加密盘内部可能有
// 自己的文件系统，但外面那层才是“这块盘属于谁”的事实；反过来判会给出误导性的结论。
func classifySignature(head, tail []byte, sizeBytes uint64) Signature {
	if len(head) < 512 {
		return Signature{Kind: SignatureUnknown, Detail: "头部不足一个扇区，无法判断"}
	}

	// 1) LUKS：偏移 0，魔数后 2 字节是版本（大端）。
	if bytes.HasPrefix(head, magicLUKS) {
		detail := "LUKS"
		if len(head) >= 8 {
			detail = fmt.Sprintf("LUKS v%d", binary.BigEndian.Uint16(head[6:8]))
		}
		return Signature{Kind: SignatureLUKS, Detail: detail}
	}

	// 2) LVM2 物理卷标签：512 字节处。
	if len(head) >= 0x200+len(magicLVM2) && bytes.Equal(head[0x200:0x200+len(magicLVM2)], magicLVM2) {
		return Signature{Kind: SignatureLVM2, Detail: "LVM2 PV"}
	}

	// 3) md 超级块。1.1 在 0、1.2 在 0x1000，另有几处历史偏移；
	//    1.0 与 0.90 在设备尾部，由下面的尾部检查处理。
	for _, off := range []int{0, 0x1000, 0x2000, 0x8000, 0x10000} {
		if len(head) >= off+4 && bytes.Equal(head[off:off+4], magicMDRaid) {
			return Signature{Kind: SignatureMD, Detail: fmt.Sprintf("md 1.x @0x%x", off)}
		}
	}

	// 4) 文件系统。
	if bytes.HasPrefix(head, magicXFS) {
		return Signature{Kind: SignatureXFS, Detail: "XFS"}
	}
	if len(head) >= 0x10000+0x48 && bytes.Equal(head[0x10040:0x10048], magicBtrfs) {
		return Signature{Kind: SignatureBtrfs, Detail: "Btrfs"}
	}
	// ext 超级块在 1024 字节处，magic 是它内部的第 56 字节（小端 0xEF53）。
	if len(head) >= 0x438+2 && head[0x438] == 0x53 && head[0x439] == 0xEF {
		return Signature{Kind: SignatureExt, Detail: "ext2/3/4"}
	}
	if len(head) >= 11 && (bytes.Equal(head[3:11], magicNTFS)) {
		return Signature{Kind: SignatureNTFS, Detail: "NTFS"}
	}
	if len(head) >= 11 && (bytes.Equal(head[3:11], magicExFAT)) {
		return Signature{Kind: SignatureExFAT, Detail: "exFAT"}
	}
	if isFATBootSector(head) {
		return Signature{Kind: SignatureVFAT, Detail: "FAT12/16/32"}
	}

	// 4.5) 交换区：魔数在第一个页面的末 10 字节。
	// 必须按固定偏移查，不能在整个头部窗口里盲扫：那会把“某个文件里恰好有
	// SWAPSPACE2 这十个字符”的数据盘误判成交换区。
	for _, off := range []int{swapMagic4K, swapMagic64K} {
		if len(head) < off+len(magicSwapV1) {
			continue
		}
		if bytes.Equal(head[off:off+len(magicSwapV1)], magicSwapV1) {
			return Signature{Kind: SignatureSwap, Detail: "swap v1"}
		}
		if bytes.Equal(head[off:off+len(magicSwapV0)], magicSwapV0) {
			return Signature{Kind: SignatureSwap, Detail: "swap v0"}
		}
	}

	// 5) 尾部：交换区副本（旧工具写的）与尾部放置的 md 超级块。
	if idx := bytes.Index(tail, magicSwapV1); idx >= 0 {
		return Signature{Kind: SignatureSwap, Detail: "swap v1"}
	}
	if bytes.Contains(tail, magicSwapV0) {
		return Signature{Kind: SignatureSwap, Detail: "swap v0"}
	}
	for off := 0; off+4 <= len(tail); off += 4 {
		if bytes.Equal(tail[off:off+4], magicMDRaid) {
			return Signature{Kind: SignatureMD, Detail: "md 1.0/0.90 @tail"}
		}
	}

	// 6) 什么都不认得：全零才是 blank，否则是 unknown（有东西，但不认识）。
	if allZero(head) && allZero(tail) {
		return Signature{Kind: SignatureBlank, Detail: "头尾全零"}
	}
	return Signature{Kind: SignatureUnknown, Detail: "有非零内容，但不属于已知签名"}
}

// isFATBootSector 判断头扇区是否像一个 FAT 引导扇区。
//
// 只看 0x55AA 是不够的：任何带分区表的盘（包括 GPT 的保护性 MBR）都有它，
// 那样的盘会被误判成“上面有 FAT 文件系统”。所以要同时看 BIOS 参数块：
// 每扇区字节数 ∈ {512,1024,2048,4096}、每簇扇区数是 2 的幂、保留扇区数 > 0、
// FAT 表个数是 1 或 2。NTFS/exFAT 的 BPB 里 FAT 表个数是 0，因此不会被算进来。
func isFATBootSector(head []byte) bool {
	if len(head) < 64 {
		return false
	}
	if head[510] != 0x55 || head[511] != 0xAA {
		return false
	}
	bytesPerSector := binary.LittleEndian.Uint16(head[11:13])
	switch bytesPerSector {
	case 512, 1024, 2048, 4096:
	default:
		return false
	}
	sectorsPerCluster := head[13]
	if sectorsPerCluster == 0 || sectorsPerCluster&(sectorsPerCluster-1) != 0 {
		return false
	}
	reserved := binary.LittleEndian.Uint16(head[14:16])
	if reserved == 0 {
		return false
	}
	fatCount := head[16]
	if fatCount != 1 && fatCount != 2 {
		return false
	}
	// 介质描述符合法值（0xF0 的软盘与 0xF8..0xFF 的硬盘）。
	switch head[21] {
	case 0xF0, 0xF8, 0xF9, 0xFA, 0xFB, 0xFC, 0xFD, 0xFE, 0xFF:
		return true
	}
	return false
}

// signatureBlocksFormat 判断这个签名是否说明“里面有别的层在管着”。
//
// LUKS/LVM2/md 的分区不是“普通数据分区”：格式化或删除会破坏加密头、卷组元数据或阵列成员。
func signatureBlocksFormat(kind string) bool {
	switch kind {
	case SignatureLUKS, SignatureLVM2, SignatureMD:
		return true
	}
	return false
}

// signatureHasData 判断这个签名是否意味着“盘上已有数据”。
func signatureHasData(kind string) bool {
	switch kind {
	case SignatureBlank:
		return false
	}
	return true
}
