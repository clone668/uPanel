package storage

// 文件系统超级块读取（P07.3）。
//
// 为什么需要它：格式化之后必须**读回来**证明盘上真的变成了请求的那种文件系统。
// 只信 mkfs 的退出码是不够的——退出码 0 只说明程序自己认为成功了。P07.1 的签名探针
// 能回答“这是不是 ext/xfs 家族的魔数”，但它按设计只报种类、不解析字段
// （见 signature.go：返回 Kind + Detail，读不到 UUID 与标签）。于是格式化就没有办法
// 证明“标签落对了、UUID 真的换了”。
//
// 这里直接读设备自己的字节：ext4 的超级块在 1024 字节处、XFS 的在 0 字节处，两处都
// 在第一个 4 KiB 页面之内，一次 ReadAt 就够。不调用 blkid（CONTRACTS.md 第 6 节：
// 本阶段不调用 blkid）、不挂载、不写、不 flush。
//
// 分类的诚实边界：
//   - ext 家族靠魔数 0xEF53 认出来，再用 incompat 里的 EXTENTS 位区分 ext4 与更老的
//     两代。ext4 允许挂载没有 extents 的旧文件系统，但 **mkfs.ext4 生成的**一定带
//     extents，所以这个判断对“我们刚做出来的文件系统”成立，对任意老盘不成立。
//   - 认出来的东西只在**我们自己刚写过的设备**上被当作核验依据；它不参与拓扑采集，
//     也不进 filesystem.type/uuid/label（那几个字段仍然只来自挂载表，未挂载时为 null）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// 超级块里的固定偏移。全部相对**分区设备自己的 0 字节**（不是整盘）。
//
// 这些数字不是猜的：ext4 的布局来自内核 include/linux/ext4_fs.h（超级块 1024 字节处、
// s_uuid 在 +0x68、s_volume_name 在 +0x78），XFS 来自 xfsprogs 的 xfs_sb.h
// （sb_magicnum 在 0、sb_uuid 在 +0x20、sb_fname 在 +0x6c）。实测复核见
// docs/evidence/P07/2026-10-22-p07.3/measurements.txt：mkfs 之后按这些偏移读出来的
// 标签与 mkfs 命令里的 -L 逐字节一致，13 字节的 xfs 标签被 mkfs 直接拒绝。
const (
	// fsProbeBytes 是一次读取的窗口（第一个 4 KiB 页面）。
	fsProbeBytes = 4096

	// ext4：超级块从 0x400 开始，魔数 0xEF53 落在 0x438。
	ext4SuperOffset = 0x400
	ext4MagicOffset = 0x438
	ext4Magic       = 0xEF53
	ext4UUIDOffset  = ext4SuperOffset + 0x68
	ext4LabelOffset = ext4SuperOffset + 0x78
	ext4LabelLen    = MaxExt4LabelBytes
	ext4BlocksLo    = ext4SuperOffset + 0x04 // s_blocks_count_lo（小端 4 字节）
	ext4LogBlockSz  = ext4SuperOffset + 0x18 // s_log_block_size（小端 4 字节，1024<<n）
	ext4Incompat    = ext4SuperOffset + 0x60 // s_feature_incompat（小端 4 字节）
	ext4BlocksHi    = ext4SuperOffset + 0x150
	// ext4FeatureIncompat64Bit 表示 s_blocks_count_hi 有效。
	ext4FeatureIncompat64Bit = 0x0080
	// ext4FeatureIncompatExtents 是区分 ext4 与 ext3/ext2 的关键位。
	ext4FeatureIncompatExtents = 0x0040

	// xfs：超级块在 0 字节处，魔数 "XFSB"，所有多字节字段是**大端**。
	xfsMagicOffset     = 0x00
	xfsBlockSizeOffset = 0x04
	xfsDBlocksOffset   = 0x08
	xfsUUIDOffset      = 0x20
	xfsLabelOffset     = 0x6C
	xfsLabelLen        = MaxXFSLabelBytes
	// xfsMagicString 是 XFS 超级块的四个字节。
	xfsMagicString = "XFSB"
)

// FilesystemIdentity 是一台设备上**现在**的文件系统身份（从超级块字节读出来的）。
//
// 它只描述“字节里写着什么”，不描述“内核认不认”。核验时它与签名探针（另一条读取
// 路径）一起用：两条都对上才算 verified。
type FilesystemIdentity struct {
	// Kind 是识别结果：FilesystemExt4、FilesystemXFS，或者空串（不是这两种）。
	Kind string
	// UUID 是标准 8-4-4-4-12 形式的字符串；读不到时为空。
	UUID string
	// Label 是卷标（已去掉 NUL 填充）；没有卷标时为空字符串。
	Label string
	// BlockSize 与 Blocks 是文件系统自己的几何；Bytes = BlockSize × Blocks。
	BlockSize uint64
	Blocks    uint64
	Bytes     uint64
	// Detail 是给人读的一句补充（例如“ext 家族但没有 extents 位，不是 ext4”）。
	Detail string
}

// String 返回调试用的短描述。
func (f FilesystemIdentity) String() string {
	kind := f.Kind
	if kind == "" {
		kind = "unknown"
	}
	if f.Detail != "" {
		return kind + "(" + f.Detail + ")"
	}
	if f.UUID == "" {
		return kind
	}
	return kind + "(uuid " + f.UUID + ")"
}

// SuperblockProber 从一台设备读取文件系统身份。
//
// 与 SignatureProber 同样的纪律：只读、带超时、不唤醒休眠设备、读失败返回错误
// （调用方据此拒绝，而不是把读不出来当成“空文件系统”）。
type SuperblockProber interface {
	Probe(ctx context.Context, devID string, sizeBytes uint64) (FilesystemIdentity, error)
}

// DeviceSuperblockProber 直接读设备节点的前 4 KiB。
//
// Dev 与 Sys 可以指向夹具目录——单元测试把一个普通文件当作设备（与
// DeviceSignatureProber 同一套夹具约定）。
type DeviceSuperblockProber struct {
	// Dev 是设备目录（真实值 /dev）。
	Dev string
	// Sys 是 sysfs 根（真实值 /sys），只用来判断设备是否在休眠。
	Sys string
	// Timeout 是单次读取的超时。0 时用 DefaultDiskReadTimeout。
	Timeout time.Duration
}

// SystemSuperblockProber 返回指向真实设备的生产读取器。
func SystemSuperblockProber() SuperblockProber {
	return DeviceSuperblockProber{Dev: DefaultDevDir, Sys: "/sys", Timeout: DefaultDiskReadTimeout}
}

// Probe 读取设备前 4 KiB 并解析出文件系统身份。
func (p DeviceSuperblockProber) Probe(ctx context.Context, devID string, sizeBytes uint64) (FilesystemIdentity, error) {
	if devID == "" {
		return FilesystemIdentity{}, errors.New("storage: 设备名为空")
	}
	if p.Dev == "" {
		return FilesystemIdentity{}, errors.New("storage: 没有配置设备目录")
	}
	if p.Sys != "" {
		if dir := deviceSysDir(p.Sys, devID); dir != "" && sleeping(dir) {
			return FilesystemIdentity{}, fmt.Errorf("storage: %s 处于休眠，未读取超级块（不强制唤醒磁盘）", devID)
		}
	}

	path := filepath.Join(p.Dev, devID)
	f, err := os.Open(path)
	if err != nil {
		return FilesystemIdentity{}, fmt.Errorf("storage: 打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	size := sizeBytes
	if size == 0 {
		fi, err := f.Stat()
		if err != nil {
			return FilesystemIdentity{}, fmt.Errorf("storage: 取 %s 的大小失败: %w", path, err)
		}
		if fi.Mode().IsRegular() {
			size = uint64(fi.Size())
		}
	}
	if size < fsProbeBytes && size > 0 {
		return FilesystemIdentity{}, fmt.Errorf("storage: %s 只有 %d 字节，放不下一个超级块窗口", devID, size)
	}

	head := make([]byte, fsProbeBytes)
	n, err := readAtTimeout(ctx, f, head, 0, p.timeout())
	if err != nil {
		return FilesystemIdentity{}, fmt.Errorf("storage: 读 %s 的超级块窗口失败: %w", devID, err)
	}
	if n < len(head) {
		// 短读：把没读到的部分留成零，但要知道这是短读——解析器只看固定偏移，
		// 落在窗口内的字段仍然有效，落在外面的（ext4 的 blocks_hi）会读成 0。
		head = head[:n]
	}
	return parseFilesystemIdentity(head)
}

func (p DeviceSuperblockProber) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultDiskReadTimeout
}

// parseFilesystemIdentity 按魔数分派到两个解析器。
//
// 顺序与签名探针一致：先 XFS（魔数在 0 字节，位置固定）、再 ext（0x438）。
// 两者同时命中的情况在真实盘上不存在（偏移不同），真出现就按“认不出来”处理。
func parseFilesystemIdentity(head []byte) (FilesystemIdentity, error) {
	if len(head) < 512 {
		return FilesystemIdentity{}, fmt.Errorf("storage: 头部只有 %d 字节，不足一个扇区", len(head))
	}
	if len(head) >= xfsMagicOffset+4 && string(head[xfsMagicOffset:xfsMagicOffset+4]) == xfsMagicString {
		return parseXFSHeader(head), nil
	}
	if len(head) >= ext4MagicOffset+2 && le16(head[ext4MagicOffset:]) == ext4Magic {
		return parseExt4Superblock(head), nil
	}
	return FilesystemIdentity{}, nil
}

// parseExt4Superblock 解析 ext 家族超级块。
func parseExt4Superblock(head []byte) FilesystemIdentity {
	id := FilesystemIdentity{Kind: SignatureExt}
	if len(head) < ext4SuperOffset+0x200 {
		id.Detail = "超级块不完整"
		return id
	}
	incompat := le32(head[ext4Incompat:])
	logBlockSize := le32(head[ext4LogBlockSz:])
	if logBlockSize > 6 {
		// 1024 << 6 = 64 KiB，再大就不可能是 ext4 的块大小；按认不出来处理，
		// 免得一个坏字段把「文件系统大小」算成天文数字。
		id.Detail = "块大小字段异常（1024<<" + strconv.FormatUint(uint64(logBlockSize), 10) + "）"
		return id
	}
	id.BlockSize = uint64(1024) << logBlockSize
	blocks := uint64(le32(head[ext4BlocksLo:]))
	if incompat&ext4FeatureIncompat64Bit != 0 && len(head) >= ext4BlocksHi+4 {
		blocks |= uint64(le32(head[ext4BlocksHi:])) << 32
	}
	id.Blocks = blocks
	id.Bytes = id.BlockSize * blocks
	id.UUID = formatUUID(head[ext4UUIDOffset : ext4UUIDOffset+16])
	id.Label = trimNul(head[ext4LabelOffset : ext4LabelOffset+ext4LabelLen])
	// ext2/ext3 没有 extents。mkfs.ext4 生成的一定有，因此“有 extents”才是 ext4。
	if incompat&ext4FeatureIncompatExtents != 0 {
		id.Kind = FilesystemExt4
	} else {
		id.Detail = "ext 家族，但没有 extents 位（不是 mkfs.ext4 生成的文件系统）"
	}
	return id
}

// parseXFSHeader 解析 XFS 超级块。
func parseXFSHeader(head []byte) FilesystemIdentity {
	id := FilesystemIdentity{Kind: FilesystemXFS}
	if len(head) < xfsLabelOffset+xfsLabelLen {
		id.Detail = "超级块不完整"
		return id
	}
	id.BlockSize = uint64(be32(head[xfsBlockSizeOffset:]))
	id.Blocks = be64(head[xfsDBlocksOffset:])
	if id.BlockSize > 0 {
		id.Bytes = id.BlockSize * id.Blocks
	}
	id.UUID = formatUUID(head[xfsUUIDOffset : xfsUUIDOffset+16])
	id.Label = trimNul(head[xfsLabelOffset : xfsLabelOffset+xfsLabelLen])
	return id
}

// formatUUID 把 16 字节按 8-4-4-4-12 写出来；全零返回空串（“没有”而不是“全零的 UUID”）。
func formatUUID(b []byte) string {
	if len(b) < 16 {
		return ""
	}
	allZero := true
	for _, c := range b[:16] {
		if c != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i := 0; i < 16; i++ {
		switch i {
		case 4, 6, 8, 10:
			out = append(out, '-')
		}
		out = append(out, hex[b[i]>>4], hex[b[i]&0x0f])
	}
	return string(out)
}

// trimNul 去掉定长字段尾部的 NUL 填充。
//
// 只去尾部、不动中间：标签里允许有空格（"data disk" 是合法卷标），
// 真实的 NUL 只出现在填充区。
func trimNul(b []byte) string {
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	return string(b[:end])
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func be64(b []byte) uint64 { return uint64(be32(b))<<32 | uint64(be32(b[4:])) }
