package storage

// P07.4：受管理卷——导入、挂载/卸载、配置、退出管理。
//
// 这一层解决的是“这块盘归面板管了”之后的所有事，而它的危险点与 P07.2/P07.3 不同：
// 那两步动的是**盘上的字节**，这一步动的是**开机的行为**。/etc/fstab 里多一行写错的
// 条目，代价不是“这次操作失败”，而是“这台机器下次起不来”。所以本文件与 fstab.go
// 的每一条规则都围绕一个目标：写进 fstab 的每一行都必须是可回滚、可验证、且只属于
// 面板自己的那一块区域。
//
// 三条不可让步的边界（PLAN.md 第 9.1、9.3 节）：
//
//  1. **挂载点只能是 /srv/upanel/volumes/<slug>**。挂载点不由用户给定，由 slug 派生；
//     slug 只允许小写字母、数字和中划线。这条约束让“挂载到 /etc 上”这类请求在参数
//     校验阶段就不成立，而不是靠执行阶段的一句 if。
//  2. **fstab 只动面板自己的托管块**。块外的每一个字节都原样保留；块内有外部痕迹
//     （结构不完整、重复条目）就拒绝，而不是猜着修。
//  3. **按 UUID 挂载，不按设备路径**。设备名（/dev/sdb1）会因为插拔顺序、控制器数量
//     而变；UUID 是文件系统自己带的。挂载与 fstab 条目都以 UUID 为准，写入前再读一次
//     超级块确认这个 UUID 还在那块盘上——这是“路径复用”骗不了的一道门。

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 五个卷动作。
const (
	ActionVolumeImport    = "storage.volume.import"
	ActionVolumeMount     = "storage.volume.mount"
	ActionVolumeUnmount   = "storage.volume.unmount"
	ActionVolumeConfigure = "storage.volume.configure"
	ActionVolumeForget    = "storage.volume.forget"
)

// IsVolumeAction 报告 action 是否属于卷管理。
//
// 判决与执行都靠它分流：设备动作走 Assess/performWrite，卷动作走
// AssessVolume/performVolume。分成两套而不是在一套里加分支，是因为两者的“目标”
// 根本不是同一种东西——一个是设备，一个是登记记录加 fstab 的一行。
func IsVolumeAction(action string) bool {
	switch action {
	case ActionVolumeImport, ActionVolumeMount, ActionVolumeUnmount,
		ActionVolumeConfigure, ActionVolumeForget:
		return true
	}
	return false
}

// 卷管理用到的固定路径。
//
// 全部是常量：这些路径不接受配置、不来自 HTTP 参数。改它们要改代码并重新验收，
// 这正是这类“会写开机配置”的路径应有的门槛。
const (
	// DefaultVolumeRoot 是受管理挂载点的父目录（PLAN.md 第 9.1 节）。
	DefaultVolumeRoot = "/srv/upanel/volumes"
	// DefaultFstabPath 是系统挂载表。
	DefaultFstabPath = "/etc/fstab"
	// DefaultFstabBackupDir 是替换前的备份目录。放在代理的数据目录下：
	// 它已经是 root 独占 0700，备份里的内容与 fstab 同级敏感。
	DefaultFstabBackupDir = "/var/lib/upanel-agent/fstab-backups"
	// 挂载与查询工具。Systemd 之外的名字一律不用（busybox 的 mount 不支持 UUID）。
	DefaultMountPath   = "/usr/bin/mount"
	DefaultUmountPath  = "/usr/bin/umount"
	DefaultFindmntPath = "/usr/bin/findmnt"
)

// 卷的观察状态闭集（CONTRACTS.md 第 6 节）。
const (
	VolumeStateMounted   = "mounted"
	VolumeStateUnmounted = "unmounted"
	VolumeStateMissing   = "missing"
	VolumeStateMismatch  = "mismatch"
	VolumeStateError     = "error"
	VolumeStateUnknown   = "unknown"
)

// 卷的期望状态闭集。
const (
	DesiredMounted   = "mounted"
	DesiredUnmounted = "unmounted"
)

// 卷管理的上限与固定参数。
const (
	// MaxSlugLen 是卷名上限。32 字节足够表达用途，也让挂载路径总长可控。
	MaxSlugLen = 32
	// MaxVolumeDisplayNameRunes 是显示名上限（进审计与页面，不进 fstab）。
	MaxVolumeDisplayNameRunes = 64
	// VolumeDeviceTimeoutSeconds 是固定设备等待时间（PLAN.md 第 9.3 节：
	// “固定设备等待时间”，不由用户配置）。它让“盘没插好”变成 30 秒后继续开机，
	// 而不是把整次启动卡死在 local-fs.target 上。
	VolumeDeviceTimeoutSeconds = 30
	// ext4 的 fsck pass 是 2，XFS 是 0（PLAN.md 第 9.3 节）。
	// 写成常量而不是变量：这是“候选文件怎么才算合法”的一部分。
	ext4Pass = 2
	xfsPass  = 0
)

// VolumeConfig 是一卷的挂载配置。它决定 fstab 里那一行的选项。
type VolumeConfig struct {
	// Automount 为真表示开机自动挂载（fstab 条目不带 noauto）。
	Automount bool `json:"automount"`
	// ReadOnly 为真表示只读挂载（ro），否则 rw。
	ReadOnly bool `json:"read_only"`
	// Nodev / Nosuid 是设备与 setuid 屏蔽。数据卷默认开启：
	// 一块可以被普通用户写的数据盘上出现设备节点或 setuid 程序，
	// 就是把提权的梯子递出去。
	Nodev  bool `json:"nodev"`
	Nosuid bool `json:"nosuid"`
}

// DefaultVolumeConfig 是导入时采用的配置：不自动挂载、可读写、带 nodev 与 nosuid。
//
// automount 默认为假，是刻意的：导入不该悄悄改变这台机器的开机行为。要开机挂，
// 用 storage.volume.configure 明确打开，那一步要求再认证。
func DefaultVolumeConfig() VolumeConfig {
	return VolumeConfig{Automount: false, ReadOnly: false, Nodev: true, Nosuid: true}
}

// Validate 检查配置本身是否自洽。
func (c VolumeConfig) Validate() error {
	// 目前四项都是独立的布尔，没有互斥组合。留着这个函数是因为选项集合会长大
	// （PLAN.md 说“初期只提供”，不是“永远只有”），届时约束写在这里。
	return nil
}

// VolumeParams 是“按卷操作”的三个动作（mount / unmount / forget）的参数。
//
// Version 是乐观并发的令牌：它对应卷记录自增的 version，提交时若与库里不一致就
// 说明“你看到的那一版已经不是现在这一版了”，拒绝比猜测安全。
type VolumeParams struct {
	VolumeID string `json:"volume_id"`
	Version  string `json:"version"`
}

// VolumeImportParams 是导入卷的参数（CONTRACTS.md 第 5 节：device_id、slug、display_name）。
//
// 没有挂载点参数：它由 slug 派生。没有 automount 参数：导入不改变开机行为。
type VolumeImportParams struct {
	DeviceID    string `json:"device_id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name,omitempty"`
}

// VolumeConfigureParams 是配置卷的参数。
//
// 四个选项用指针：缺省与显式 false 必须能区分开。nodev/nosuid 的缺省是“开”，
// 若用普通 bool，客户端漏传一个字段就会静默关掉一项加固——那是最不该发生的静默降级。
type VolumeConfigureParams struct {
	VolumeID  string `json:"volume_id"`
	Version   string `json:"version"`
	Automount *bool  `json:"automount,omitempty"`
	ReadOnly  *bool  `json:"read_only,omitempty"`
	Nodev     *bool  `json:"nodev,omitempty"`
	Nosuid    *bool  `json:"nosuid,omitempty"`
}

// VolumeRecord 是代理登记的“这台主机上我接管了哪些卷”。
//
// 它是**代理侧**的事实来源：fstab 的一行与这条记录必须互相对得上；对不上时，
// 记录的 version 与 fstab 的哈希一起构成 StateVersion，执行前逐字比较。
// Web 侧的 volumes 表是它的镜像（CONTRACTS.md 14.2：代理记录最终资源，Web 对账）。
type VolumeRecord struct {
	// ID 形如 vol_<32 位小写十六进制>，导入时由代理生成。
	ID string `json:"id"`
	// DisplayName 只给人看，进 fstab 的只有 UUID、路径、类型与选项。
	DisplayName string `json:"display_name"`
	// Slug 决定挂载路径：<VolumeRoot>/<slug>。
	Slug string `json:"slug"`
	// DeviceID 是内核设备名（sdb1）。它是**会变**的，因此只作线索，
	// 真正的身份是 FilesystemUUID + IdentityFingerprint。
	DeviceID string `json:"device_id"`
	// FilesystemUUID 是挂载与核验的依据。
	FilesystemUUID string `json:"filesystem_uuid"`
	// FilesystemType 是 ext4 或 xfs。
	FilesystemType string `json:"filesystem_type"`
	// IdentityFingerprint 是导入时那台分区的身份指纹（与设备动作同一套算法），
	// 用来抓“换了一块盘，但 UUID 恰好相同”这种极小概率却不该放过的情形。
	IdentityFingerprint string `json:"identity_fingerprint"`
	// Config 是当前生效的挂载配置。
	Config VolumeConfig `json:"config"`
	// DesiredState 是面板希望它处的状态（mounted / unmounted）。
	DesiredState string `json:"desired_state"`
	// Version 每次成功变更后自增。字符串是为了与其它对象的 version 同形。
	Version string `json:"version"`
	// LastOperationID 是最近一次成功修改它的操作（审计与对账用）。
	LastOperationID string    `json:"last_operation_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MountPath 返回这卷的挂载点。
func (r VolumeRecord) MountPath() string { return MountPathOf(r.Slug) }

// FstabEntry 把它渲染成一条 fstab 条目。
func (r VolumeRecord) FstabEntry() FstabEntry {
	return FstabEntry{
		UUID:       r.FilesystemUUID,
		MountPoint: r.MountPath(),
		FSType:     r.FilesystemType,
		Options:    MountOptions(r.Config),
		Dump:       0,
		Pass:       fsckPass(r.FilesystemType),
	}
}

// MountPathOf 由 slug 派生挂载点。
//
// 用 path.Join 而不是字符串相加：多一个斜杠或少一个斜杠都会让 fstab 条目
// 与实际目录不是同一个路径，而这类差异在肉眼检查时几乎看不出来。
func MountPathOf(slug string) string { return path.Join(DefaultVolumeRoot, slug) }

// ValidateSlug 校验卷名。
//
// 规则来自 PLAN.md 第 9.3 节：小写字母、数字、中划线。首字符必须是字母或数字、
// 末字符不能是中划线——中划线在命令行里是选项前缀，在路径里是分隔符，
// 让卷名两侧都可能被误读的字符留在边界上没必要。
func ValidateSlug(slug string) error {
	if slug == "" {
		return errors.New("卷名不能为空")
	}
	if len(slug) > MaxSlugLen {
		return fmt.Errorf("卷名最长 %d 个字符，收到 %d 个", MaxSlugLen, len(slug))
	}
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(slug)-1 {
				return errors.New(`卷名不能以中划线开头或结尾`)
			}
			if slug[i-1] == '-' {
				return errors.New("卷名不能有连续的中划线")
			}
		default:
			return fmt.Errorf("卷名只能用小写字母、数字和中划线，收到 %q", string(rune(c)))
		}
	}
	// 与文件系统标签、内核设备名撞名的卷名没有危害（它只作为目录名出现），
	// 但 "." 和 ".." 会被解析成上级目录——它们在字符集阶段已经不成立，
	// 这里只是把理由记下来。
	if slug == "." || slug == ".." {
		return errors.New(`卷名不能是 "." 或 ".."`)
	}
	return nil
}

// ValidateDisplayName 校验显示名长度（只进审计与页面，不进 fstab）。
func ValidateDisplayName(name string) error {
	if len([]rune(name)) > MaxVolumeDisplayNameRunes {
		return fmt.Errorf("显示名最长 %d 个字符", MaxVolumeDisplayNameRunes)
	}
	// 允许为空：空显示名时页面用 slug。
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("显示名不能包含控制字符")
		}
	}
	return nil
}

// MountOptions 把配置渲染成 fstab 的选项列表。
//
// 顺序固定：ro/rw 在最前（人读 fstab 时第一眼要看的就是读写），其余按固定序列。
// nofail 总是加：一块数据盘不在，不该让这台机器进不了系统。
func MountOptions(c VolumeConfig) []string {
	opts := make([]string, 0, 5)
	if c.ReadOnly {
		opts = append(opts, "ro")
	} else {
		opts = append(opts, "rw")
	}
	if c.Nodev {
		opts = append(opts, "nodev")
	}
	if c.Nosuid {
		opts = append(opts, "nosuid")
	}
	opts = append(opts, "nofail")
	opts = append(opts, "x-systemd.device-timeout="+strconv.Itoa(VolumeDeviceTimeoutSeconds))
	if !c.Automount {
		opts = append(opts, "noauto")
	}
	return opts
}

// fsckPass 给出 fstab 的第六列。
//
// ext4 用 2（非根文件系统的并行 fsck 通道），XFS 用 0——XFS 有自己的日志恢复，
// 让 fsck.xfs 在开机时跑只会得到一句 "nothing to do" 甚至挂住开机。
func fsckPass(filesystem string) int {
	if filesystem == FilesystemXFS {
		return xfsPass
	}
	return ext4Pass
}

// NewVolumeID 生成一个卷标识。
//
// 用随机而不是自增：卷被 forget 之后重建同名卷得到的 id 不同，审计与依赖不会
// 把两段生命周期混成一段。
func NewVolumeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成卷标识失败: %w", err)
	}
	return "vol_" + hex.EncodeToString(b[:]), nil
}

// ValidVolumeID 判断一个卷标识的形状是否合法。
//
// 形状检查在这里，存在性检查在登记表：前者防注入（它会进日志、审计与 URL），
// 后者才是业务判断。
func ValidVolumeID(id string) bool {
	if len(id) != 4+32 || !strings.HasPrefix(id, "vol_") {
		return false
	}
	for i := 4; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NextVersion 返回自增后的版本号（空串视为 0）。
//
// 版本是字符串但对内的语义是单调计数器：它在 fstab 哈希之外额外表达了
// “这条记录被改过几次”，对人排查有用。
func NextVersion(current string) string {
	n, err := strconv.ParseUint(strings.TrimSpace(current), 10, 64)
	if err != nil {
		n = 0
	}
	return strconv.FormatUint(n+1, 10)
}

// Volume 是对外（Web / 页面）的卷视图。
//
// 字段与 CONTRACTS.md 第 6 节的 Volume 一一对应。observed_state 与 capacity 是
// **现场算出来的**，不落库：把“它现在挂着没有”存进数据库，第二天早上就变成假话。
type Volume struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	DisplayName    string `json:"display_name"`
	Slug           string `json:"slug"`
	DeviceID       string `json:"device_id"`
	FilesystemUUID string `json:"filesystem_uuid"`
	FilesystemType string `json:"filesystem_type"`
	MountPath      string `json:"mount_path"`
	Automount      bool   `json:"automount"`
	ReadOnly       bool   `json:"read_only"`
	Nodev          bool   `json:"nodev"`
	Nosuid         bool   `json:"nosuid"`
	DesiredState   string `json:"desired_state"`
	ObservedState  string `json:"observed_state"`
	// Capacity 只在真正挂载且查询成功时存在；null 不是“0 字节可用”。
	Capacity *Capacity `json:"capacity"`
	// Dependencies 是依赖这个卷的应用/共享/备份摘要。P07.4 时这些模块还没实现，
	// 因此恒为空数组——空数组在这里的意思是“没有依赖”，不是“没查”。
	Dependencies []VolumeDependency `json:"dependencies"`
	// ObservedError 说明 observed_state 为什么不是 mounted/unmounted
	// （读不到超级块、UUID 不符……）。正常时不出现。
	ObservedError string    `json:"observed_error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// VolumeDependency 是一条依赖摘要。
type VolumeDependency struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// VolumeBlockers 是 GET /storage/volumes/:id/blockers 的内容：为什么现在卸载不了。
//
// 页面要能指着它说“是这两个进程在用”，而不是一句“设备忙”。
type VolumeBlockers struct {
	VolumeID string `json:"volume_id"`
	// Mounted 报告这卷现在挂着没有：没挂的卷没有“繁忙”可言。
	Mounted bool `json:"mounted"`
	// Mounts 是这块设备当前的挂载点。
	Mounts []string `json:"mounts"`
	// Processes 是打开着这块设备的进程。
	Processes []BusyProcess `json:"processes"`
	// Holders 是消费这台设备的 dm/md 设备。
	Holders []string `json:"holders"`
	// SwapActive 表示这块设备是活动交换区。
	SwapActive bool `json:"swap_active"`
	// Apps / Shares / Backups 是面板侧依赖；P07.4 时三个模块尚未实现，恒为空。
	Apps    []VolumeDependency `json:"apps"`
	Shares  []VolumeDependency `json:"shares"`
	Backups []VolumeDependency `json:"backups"`
	// Busy 是结论：现在卸载会被谁挡住。只有真的有人关着文件（进程）、设备被 dm/md
	// 消费、是活动交换区，或面板侧有应用/共享/备份依赖时才为真。
	// 挂着但没人用**不算**忙：那种卷能干净卸载，预检也会放行，两处口径必须一致。
	Busy bool `json:"busy"`
	// CheckedAt 是这次检查的时间（占用是某一瞬间的快照，必须带上时间）。
	CheckedAt time.Time `json:"checked_at"`
}

// sortVolumes 让列表输出稳定（按挂载路径）。稳定输出不是为了好看：
// 验收脚本与页面 diff 都靠它，顺序随机会让“没有变化”看起来像有变化。
func sortVolumes(vs []Volume) {
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].MountPath != vs[j].MountPath {
			return vs[i].MountPath < vs[j].MountPath
		}
		return vs[i].ID < vs[j].ID
	})
}

// sortRecords 与 sortVolumes 同样的理由，作用于记录列表。
func sortRecords(rs []VolumeRecord) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Slug != rs[j].Slug {
			return rs[i].Slug < rs[j].Slug
		}
		return rs[i].ID < rs[j].ID
	})
}
