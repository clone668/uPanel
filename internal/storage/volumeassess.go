package storage

// 卷管理动作的判决（P07.4）。
//
// 与设备动作的判决分开写，理由在 volume.go 顶部：目标不是同一种东西。设备动作的目标
// 是“一台设备”，卷动作的目标是“一条登记记录 + fstab 里的一行 + 现场挂载状态”三者的
// 组合。把它们塞进同一个 switch 会让 assess.go 里到处是“这个动作没有设备”的分支。
//
// 这一层**不写任何东西**（CONTRACTS.md 4.1：预检不做变更）。它只读：拓扑快照、
// 超级块、fstab、登记表、/proc 占用。所有“不允许”的理由都必须是可展示的事实，
// 因为拒绝的人看到的第一句话就是这里的 message。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// 卷管理专用的阻塞码。码是闭集，测试按码断言，message 才给人读。
const (
	// NoticeVolumeNotFound 表示登记表里没有这个卷。
	NoticeVolumeNotFound = "VOLUME_NOT_FOUND"
	// NoticeVolumeVersionMismatch 表示调用方手上的 vol 版本已经不是现在这一版。
	NoticeVolumeVersionMismatch = "VOLUME_VERSION_MISMATCH"
	// NoticeVolumeSlugTaken 表示卷名（也就是挂载路径）已经被另一个卷占用。
	NoticeVolumeSlugTaken = "VOLUME_SLUG_TAKEN"
	// NoticeVolumeUUIDTaken 表示这个文件系统 UUID 已经被另一个卷占用。
	NoticeVolumeUUIDTaken = "VOLUME_UUID_TAKEN"
	// NoticeFilesystemUnsupported 表示设备上的文件系统不是 ext4/XFS，本阶段不接管。
	NoticeFilesystemUnsupported = "FILESYSTEM_UNSUPPORTED"
	// NoticeFilesystemUnreadable 表示读不到文件系统身份（超级块读失败或没有 UUID）。
	NoticeFilesystemUnreadable = "FILESYSTEM_UNREADABLE"
	// NoticeFilesystemMismatch 表示设备上的 UUID 与登记的 UUID 不一致。
	NoticeFilesystemMismatch = "FILESYSTEM_UUID_MISMATCH"
	// NoticeFstabUnavailable 表示 fstab 读不了或结构不完整。
	NoticeFstabUnavailable = "FSTAB_UNAVAILABLE"
	// NoticeFstabExternalEntry 表示 fstab 里已经有一条**不属于面板托管块**的条目
	// 指向这个文件系统或挂载点。不自动接管外部挂载（PLAN.md 第 9.1 节）。
	NoticeFstabExternalEntry = "FSTAB_EXTERNAL_ENTRY"
	// NoticeFstabDrift 表示托管块里的条目与登记记录不一致（有人手工改了块内内容）。
	NoticeFstabDrift = "FSTAB_DRIFT"
	// NoticeVolumeNotMounted 表示要卸载的卷本来就没挂着。
	NoticeVolumeNotMounted = "VOLUME_NOT_MOUNTED"
	// NoticeVolumeMounted 表示要挂载/退出管理的卷当前挂载着。
	NoticeVolumeMounted = "VOLUME_MOUNTED"
	// NoticeVolumeMountedElsewhere 表示文件系统挂在别处（不是面板的挂载点）。
	NoticeVolumeMountedElsewhere = "VOLUME_MOUNTED_ELSEWHERE"
	// NoticeVolumeBusy 表示卷上有进程正在使用，不能卸载。
	NoticeVolumeBusy = "VOLUME_BUSY"
	// NoticeMountPointUnsafe 表示挂载点路径上有符号链接、非目录或权限不对的成分。
	NoticeMountPointUnsafe = "MOUNT_POINT_UNSAFE"
	// NoticeVolumeOptionsNeedUnmount 表示要改的挂载选项必须在卸载之后才能生效。
	NoticeVolumeOptionsNeedUnmount = "VOLUME_OPTIONS_NEED_UNMOUNT"
	// NoticeVolumeConfigInvalid 表示配置本身不自洽（选项组合不成立）。
	NoticeVolumeConfigInvalid = "VOLUME_CONFIG_INVALID"
	// NoticeVolumeHasDependencies 表示还有面板对象依赖这个卷。
	NoticeVolumeHasDependencies = "VOLUME_HAS_DEPENDENCIES"
	// NoticeVolumeInUseByShare 表示有共享绑在这个卷上，卸载/格式化会被它挡住。
	//
	// 与 VOLUME_HAS_DEPENDENCIES 分开记：那条说的是“退出管理会丢绑定”（任何依赖都算），
	// 这条说的是“这块卷正在对外提供文件访问”。两者的处理方式不同——卸载是“先停共享再卸”，
	// 退出管理是“先删共享再退”——所以不能在报告里合成一条。
	NoticeVolumeInUseByShare = "VOLUME_IN_USE_BY_SHARE"
	// NoticeVolumeDependenciesUnknown 表示问不出面板侧依赖。
	//
	// 这是**阻塞**项，与“没有依赖”不是一回事：读不到登记表就证明不了没人依赖它，
	// 而卸载/格式化是不可逆方向的破坏（格式化尤其）。宁可拒绝，也不能靠“大概没人用”动手。
	NoticeVolumeDependenciesUnknown = "VOLUME_DEPENDENCIES_UNKNOWN"

	// NoticeMountPointNotEmpty 是告警：挂载点目录里已经有文件，挂载后会被遮住。
	NoticeMountPointNotEmpty = "MOUNT_POINT_NOT_EMPTY"
)

// ImpactConfigChange 表示这次动作会改写 /etc/fstab 的托管块。
const ImpactConfigChange = "config_change"

// VolumeStore 是受管理卷的持久登记表（生产实现见 agentstore）。
//
// 接口定义在消费方，实现放在数据库层：判决与执行都要读它，但两者都不该知道表结构。
type VolumeStore interface {
	// ListVolumes 返回全部登记记录（顺序不保证，调用方自己排序）。
	ListVolumes(ctx context.Context) ([]VolumeRecord, error)
	// GetVolume 取一条记录；第二个返回值为 false 表示不存在（不是错误）。
	GetVolume(ctx context.Context, id string) (VolumeRecord, bool, error)
	// SaveVolume 新建或覆盖一条记录。
	SaveVolume(ctx context.Context, rec VolumeRecord) error
	// DeleteVolume 删除一条记录；不存在时返回 nil（幂等）。
	DeleteVolume(ctx context.Context, id string) error
}

// VolumeEnv 是卷判决与执行需要的外部输入。
//
// 与 Env 一样，探针可注入；与 Env 不同的是它多了登记表与 fstab——因为卷动作的
// 判决本来就依赖这两样东西，没有它们就没有“这是不是我管的卷”这个问题。
type VolumeEnv struct {
	// Store 是登记表；nil 表示本机没有卷管理能力（判决会明确拒绝）。
	Store VolumeStore
	// Fstab 是 fstab 的读写器（判决只读）。
	Fstab FstabStore
	// Superblocks 读文件系统身份（UUID/类型/卷标）。
	Superblocks SuperblockProber
	// Busy 列出打开设备的进程（卸载前的繁忙检查）。
	Busy BusyScanner
	// Dependents 列出面板块依赖这个卷的共享（卸载/退出管理的依赖检查）。
	//
	// 卷这一侧按 VolumeID 问；为 nil 或返回错误时阻塞，理由同 Env.Dependents。
	Dependents DependencyProber
	// Root 是受管理挂载点的父目录；空时用 DefaultVolumeRoot。
	Root string
	// Lstat / ReadDir 是文件系统接缝（挂载点安全与“目录里有没有东西”）。
	Lstat   func(path string) (os.FileInfo, error)
	ReadDir func(path string) ([]os.DirEntry, error)
	// Now 提供时间（可注入）。
	Now func() time.Time
}

func (e VolumeEnv) root() string {
	if strings.TrimSpace(e.Root) == "" {
		return DefaultVolumeRoot
	}
	return e.Root
}

func (e VolumeEnv) lstat(path string) (os.FileInfo, error) {
	if e.Lstat != nil {
		return e.Lstat(path)
	}
	return os.Lstat(path)
}

func (e VolumeEnv) readDir(path string) ([]os.DirEntry, error) {
	if e.ReadDir != nil {
		return e.ReadDir(path)
	}
	return os.ReadDir(path)
}

func (e VolumeEnv) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// mountPathOf 派生挂载点（沿用 env 的 root）。
func (e VolumeEnv) mountPathOf(slug string) string {
	return path.Join(e.root(), slug)
}

// AssessVolume 是卷动作的判决入口。
func AssessVolume(ctx context.Context, topo Topology, action string, params any, env VolumeEnv) (Assessment, error) {
	a := &Assessment{Action: action, NormalizedParams: params}
	if env.Store == nil {
		a.block(NoticeTopologyUnavailable, "本机没有配置受管理卷的登记表，卷管理不可用")
		a.finish()
		return *a, nil
	}
	// 拓扑读不到时，所有卷动作都不成立：判断“哪台设备、挂着没有”都要它。
	if topo.Status == StatusError {
		a.block(NoticeTopologyUnavailable, "存储拓扑不可用："+topo.Error)
		a.finish()
		return *a, nil
	}
	index := indexTopology(topo)
	switch p := params.(type) {
	case VolumeImportParams:
		assessVolumeImport(ctx, a, topo, index, p, env)
	case VolumeParams:
		switch action {
		case ActionVolumeMount:
			assessVolumeMount(ctx, a, topo, index, p, env)
		case ActionVolumeUnmount:
			assessVolumeUnmount(ctx, a, topo, index, p, env)
		case ActionVolumeForget:
			assessVolumeForget(ctx, a, topo, index, p, env)
		default:
			return Assessment{}, fmt.Errorf("storage: %s 不是卷动作", action)
		}
	case VolumeConfigureParams:
		assessVolumeConfigure(ctx, a, topo, index, p, env)
	default:
		return Assessment{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
	}
	a.finish()
	return *a, nil
}

// volumeTarget 是卷动作里“找到并确认了记录”的结果。
type volumeTarget struct {
	rec      VolumeRecord
	fstab    FstabState
	entry    FstabEntry
	hasEntry bool
	obs      VolumeObservation
}

// loadVolume 取出记录并做三项所有卷动作都要做的事：版本核对、fstab 读取、现场观察。
//
// 三项都做，是为了让“为什么不允许”只有一个出口：任何一个不成立都会留下阻塞项，
// 调用方只负责补动作自己的检查。
func loadVolume(ctx context.Context, a *Assessment, topo Topology, index topologyIndex,
	volumeID, version string, env VolumeEnv) (volumeTarget, bool) {
	t := volumeTarget{}
	rec, found, err := env.Store.GetVolume(ctx, volumeID)
	if err != nil {
		a.block(NoticeTopologyUnavailable, "读取卷登记失败："+err.Error())
		return t, false
	}
	if !found {
		a.block(NoticeVolumeNotFound, fmt.Sprintf("卷 %s 不在受管理卷列表里", volumeID))
		return t, false
	}
	t.rec = rec
	a.TargetID = rec.ID
	a.DisplayName = volumeDisplayName(rec)
	if disk := diskOf(index, rec.DeviceID); disk != "" {
		a.DiskIDs = []string{disk}
	}
	if version != "" && version != rec.Version {
		a.block(NoticeVolumeVersionMismatch, fmt.Sprintf(
			"卷 %s 的版本是 %s，请求里带的是 %s：页面上的那份已经过期，请刷新后再操作",
			rec.Slug, rec.Version, version))
	}
	if err := validateSlugForRecord(env, rec.Slug); err != nil {
		a.block(NoticeMountPointUnsafe, err.Error())
	}
	fst, err := env.Fstab.Read()
	switch {
	case err != nil:
		a.block(NoticeFstabUnavailable, "读 %s 失败：%v", env.Fstab.fstabPath(), err)
	case fst.Missing:
		// 文件不存在是允许的（我们会在写的时候建）；但“卷登记了、fstab 里没有”
		// 会被下面的漂移检查抓到。
	}
	t.fstab = fst
	t.entry, t.hasEntry = fst.Block.FindEntryByUUID(rec.FilesystemUUID)
	t.obs = observeVolume(ctx, topo, index, rec, env)
	return t, true
}

// validateSlugForRecord 确认记录里的卷名仍然能派生出安全的挂载点。
//
// 记录是面板自己写的，理论上不会变；但数据库是可以被 root 手工改的，
// 而挂载点会进 fstab。这里再算一次代价极小。
func validateSlugForRecord(env VolumeEnv, slug string) error {
	if err := ValidateSlug(slug); err != nil {
		return fmt.Errorf("记录里的卷名 %q 不合法：%v", slug, err)
	}
	if got, want := env.mountPathOf(slug), path.Join(DefaultVolumeRoot, slug); got != want {
		return fmt.Errorf("卷名 %q 派生出的挂载点 %q 不是 %q", slug, got, want)
	}
	return nil
}

// assessVolumeImport 判“能不能把这块分区纳管”。
func assessVolumeImport(ctx context.Context, a *Assessment, topo Topology, index topologyIndex,
	p VolumeImportParams, env VolumeEnv) {
	a.TargetID = p.DeviceID
	a.DisplayName = "分区 " + p.DeviceID

	dev := index.devices[p.DeviceID]
	if dev == nil {
		a.block(NoticeDeviceMissing, "设备 %s 不在当前拓扑里（拔掉了、改名了，或者 id 写错了）", p.DeviceID)
		a.finish()
		return
	}
	// 只接管普通分区：整盘直接挂载会绕开分区表，且与 PLAN.md 第 9.1 节不符。
	if dev.Kind != KindPartition {
		a.block(NoticeUnsupportedKind, "%s 是%s，只有普通分区可以导入为受管理卷", p.DeviceID, kindLabel(dev.Kind))
	}
	chain, disks, ok := index.chain(p.DeviceID)
	if !ok || len(disks) == 0 {
		a.block(NoticeParentUnknown, "找不到 %s 所属的物理盘，无法确认盘锁范围", p.DeviceID)
	} else {
		// 系统盘保护沿整条链判断：分区自己不带这个信息，它所属的盘才带。
		if reasons := protectionReasons(chain); len(reasons) > 0 {
			a.block(NoticeSystemDisk, "目标所在物理盘承载了系统数据：%s", strings.Join(reasons, "；"))
		}
		a.DiskIDs = disks
		for _, disk := range disks {
			a.Locks = append(a.Locks, "disk:"+disk)
		}
	}
	a.Locks = append(a.Locks, "fstab")
	if len(chain) > 0 {
		a.DisplayName = displayNameFor(dev, index)
	}

	// 挂载状态：已经挂着的分区不导入——面板不接管“别人挂着的”东西。
	if mounts := mountsOf(dev); len(mounts) > 0 {
		if mountedAt(mounts, env.mountPathOf(p.Slug)) {
			a.block(NoticeVolumeMountedElsewhere,
				"%s 现在挂在 %s——这正是要导入的挂载点，但面板还没有它的登记。先卸载，或换一个卷名",
				p.DeviceID, env.mountPathOf(p.Slug))
		} else {
			a.block(NoticeVolumeMounted, "%s 现在挂在 %s，先卸载再导入", p.DeviceID, strings.Join(mounts, "、"))
		}
	}
	if !dev.Online {
		a.block(NoticeOffline, "%s 当前不在线，无法确认文件系统身份", p.DeviceID)
	}
	if dev.ReadOnly {
		a.block(NoticeReadOnly, "%s 被标记为只读，不能作为可读写数据卷导入", p.DeviceID)
	}

	// 身份：读超级块。读不到就拒绝——导入的核心就是把 UUID 记下来。
	id, err := env.superblocks().Probe(ctx, p.DeviceID, dev.SizeBytes)
	switch {
	case err != nil:
		a.block(NoticeFilesystemUnreadable, "读 %s 的文件系统身份失败：%v", p.DeviceID, err)
	case id.Kind == "" || id.UUID == "":
		a.block(NoticeFilesystemUnreadable, "%s 上没有读到带 UUID 的文件系统（读到的类型是 %q）",
			p.DeviceID, id.Kind)
	case id.Kind != FilesystemExt4 && id.Kind != FilesystemXFS:
		a.block(NoticeFilesystemUnsupported,
			"%s 上是 %s，本阶段只接管 ext4 与 XFS 的普通分区", p.DeviceID, id.Kind)
	}
	if id.UUID != "" {
		a.IdentityFingerprint = importIdentityFingerprint(dev, index, id)
	}

	// 登记表：卷名与 UUID 都不能重复（两个卷指向同一个挂载点会让 fstab 出现两行，
	// 而 systemd 取哪一行不由我们决定）。
	records, err := env.Store.ListVolumes(ctx)
	if err != nil {
		a.block(NoticeTopologyUnavailable, "读取卷登记失败："+err.Error())
	}
	wantPath := env.mountPathOf(p.Slug)
	for _, rec := range records {
		if strings.EqualFold(rec.Slug, p.Slug) || rec.MountPath() == wantPath {
			a.block(NoticeVolumeSlugTaken, "卷名 %q 已经被卷 %s 占用（挂载点 %s）",
				p.Slug, rec.ID, rec.MountPath())
		}
		if id.UUID != "" && strings.EqualFold(rec.FilesystemUUID, id.UUID) {
			a.block(NoticeVolumeUUIDTaken, "文件系统 UUID %s 已经由卷 %s 管理", id.UUID, rec.ID)
		}
	}

	// fstab：块外不许有指向同一个 UUID 或同一个挂载点的行——那是别人的配置，
	// 面板可以只读展示，但不自动接管（PLAN.md 第 9.1 节）。
	fst, err := env.Fstab.Read()
	if err != nil {
		a.block(NoticeFstabUnavailable, "读 %s 失败：%v", env.Fstab.fstabPath(), err)
	} else {
		for _, ref := range ExternalFstabRefs(fst.Content, id.UUID, wantPath) {
			a.block(NoticeFstabExternalEntry,
				"%s 里已经有一条不属于面板的条目（第 %d 行：%s）。面板不接管外部挂载：先手工删掉它，或换一个卷名",
				env.Fstab.fstabPath(), ref.Line, ref.Text)
		}
	}
	// 挂载点目录本身：符号链接/非目录/父链上有符号链接都不行。
	if msgs := checkMountPointPath(env, wantPath); len(msgs) > 0 {
		for _, msg := range msgs {
			a.block(NoticeMountPointUnsafe, "%s", msg)
		}
	} else if msgs := mountPointContents(env, wantPath); len(msgs) > 0 {
		a.warn(NoticeMountPointNotEmpty, "%s", strings.Join(msgs, "；"))
	}

	a.impact(ImpactConfigChange, fmt.Sprintf(
		"会把 UUID=%s 写进 %s 的受管块（挂载点 %s），块外内容不动；不会格式化，也不会改动分区表",
		id.UUID, env.Fstab.fstabPath(), wantPath))
	// 导入不重写磁盘、不动数据，只是把它登记下来。
	a.StateVersion = volumeStateVersion(topo, VolumeRecord{}, fst, "")
}

// assessVolumeMount 判“能不能挂上”。
func assessVolumeMount(ctx context.Context, a *Assessment, topo Topology, index topologyIndex,
	p VolumeParams, env VolumeEnv) {
	t, ok := loadVolume(ctx, a, topo, index, p.VolumeID, p.Version, env)
	if !ok {
		return
	}
	a.Locks = append(a.Locks, "volume:"+t.rec.ID)
	if disk := diskOf(index, t.rec.DeviceID); disk != "" {
		a.Locks = append(a.Locks, "disk:"+disk)
	}
	a.StateVersion = volumeStateVersion(topo, t.rec, t.fstab, t.obs.stateKey())

	switch t.obs.State {
	case VolumeStateMounted:
		if t.obs.MountedElsewhere {
			a.block(NoticeVolumeMountedElsewhere, "%s", t.obs.Detail)
		} else {
			a.block(NoticeVolumeMounted, "卷 %s 已经挂在 %s 上", t.rec.Slug, t.rec.MountPath())
		}
	case VolumeStateMissing:
		a.block(NoticeDeviceMissing, "%s", t.obs.Detail)
	case VolumeStateMismatch:
		a.block(NoticeFilesystemMismatch, "%s", t.obs.Detail)
	case VolumeStateError, VolumeStateUnknown:
		a.block(NoticeFilesystemUnreadable, "%s", t.obs.Detail)
	}
	// fstab 里必须有一条与登记一致的条目：`mount <挂载点>` 依赖它，
	// 而且这样挂载用的就是 UUID，不是设备路径。
	switch {
	case !t.hasEntry:
		a.block(NoticeFstabDrift,
			"%s 的受管块里没有 UUID=%s 的条目。先执行一次 storage.volume.configure 把它补回来",
			env.Fstab.fstabPath(), t.rec.FilesystemUUID)
	case !t.entry.SameAs(t.rec.FstabEntry()):
		a.block(NoticeFstabDrift, "受管块里的条目与登记不一致：条目是 %q，登记应生成 %q。先执行 configure",
			t.entry.Line(), t.rec.FstabEntry().Line())
	}
	if msgs := checkMountPointPath(env, t.rec.MountPath()); len(msgs) > 0 {
		for _, msg := range msgs {
			a.block(NoticeMountPointUnsafe, "%s", msg)
		}
	}
	if t.obs.DeviceID != "" {
		a.IdentityFingerprint = t.obs.IdentityFingerprint
	}
	a.impact(ImpactConfigChange, "会把 "+t.rec.MountPath()+" 挂上（按 UUID），不改 fstab")
}

// assessVolumeUnmount 判“能不能卸”。
func assessVolumeUnmount(ctx context.Context, a *Assessment, topo Topology, index topologyIndex,
	p VolumeParams, env VolumeEnv) {
	t, ok := loadVolume(ctx, a, topo, index, p.VolumeID, p.Version, env)
	if !ok {
		return
	}
	a.Locks = append(a.Locks, "volume:"+t.rec.ID)
	if disk := diskOf(index, t.rec.DeviceID); disk != "" {
		a.Locks = append(a.Locks, "disk:"+disk)
	}
	// 卸载不重写 fstab，因此版本里不含 fstab 哈希：别人刚改过 fstab 不该让卸载失效。
	a.StateVersion = volumeStateVersion(topo, t.rec, FstabState{}, t.obs.stateKey())

	switch t.obs.State {
	case VolumeStateUnmounted:
		a.block(NoticeVolumeNotMounted, "卷 %s 现在没有挂载，无需卸载", t.rec.Slug)
	case VolumeStateMounted:
		if t.obs.MountedElsewhere {
			// 卸载的目标是“面板的挂载点”。挂在别处的同一块文件系统不归这次操作管：
			// 从别处卸掉它可能直接打断别人的服务。
			a.block(NoticeVolumeMountedElsewhere, "%s", t.obs.Detail)
		}
	case VolumeStateMissing:
		a.block(NoticeDeviceMissing, "%s", t.obs.Detail)
	case VolumeStateMismatch, VolumeStateError, VolumeStateUnknown:
		// 状态不明时仍然允许卸载：卸载是“让它停下来”，比继续挂着安全。
		// 但要说清楚依据不足。
		a.warn(NoticeFilesystemMismatch, "卷的状态没能完全确认（%s），卸载会按挂载点执行", t.obs.Detail)
	}
	if t.obs.State != VolumeStateUnmounted && !t.obs.MountedElsewhere {
		for _, b := range volumeBusyBlockers(ctx, t.rec, t.obs, env) {
			a.appendBlocker(b)
		}
	}
	// 依赖检查在“没挂载”和“挂在别处”之外都要做：挂着的卷正是要卸的那个，
	// 而挂在别处时共享同样绑在这个卷上，撤销共享的事归用户决定，不由卸载顺手带走。
	//
	// 口径只算**启用中**的共享：禁用中的共享此刻没有客户端，数据还在盘上，
	// 卸载不会伤到它，也没必要逼用户先删配置。
	for _, b := range dependencyBlockers(ctx, t.rec, PurposeUnmount, env) {
		a.appendBlocker(b)
	}
	if t.obs.DeviceID != "" {
		a.IdentityFingerprint = t.obs.IdentityFingerprint
	}
	a.impact(ImpactConfigChange, "会卸载 "+t.rec.MountPath()+"，fstab 条目保留（开机行为不变）")
}

// assessVolumeConfigure 判“能不能改配置”。
func assessVolumeConfigure(ctx context.Context, a *Assessment, topo Topology, index topologyIndex,
	p VolumeConfigureParams, env VolumeEnv) {
	t, ok := loadVolume(ctx, a, topo, index, p.VolumeID, p.Version, env)
	if !ok {
		return
	}
	a.Locks = append(a.Locks, "volume:"+t.rec.ID, "fstab")
	a.StateVersion = volumeStateVersion(topo, t.rec, t.fstab, t.obs.stateKey())

	want, err := p.Apply(t.rec.Config)
	if err != nil {
		a.block(NoticeVolumeOptionsNeedUnmount, "%v", err)
		return
	}
	switch t.obs.State {
	case VolumeStateMounted:
		// 挂载选项的改动要重新挂载才生效。本阶段不做 remount：
		// 先卸载再配置，比一次悄悄生效的 remount 更容易解释，也更不容易半途而废。
		if mountOptionsDiffer(t.rec.Config, want) {
			a.block(NoticeVolumeOptionsNeedUnmount,
				"卷 %s 正在挂载，读写/设备/权限选项的变更要先卸载（只有“开机自动挂载”可以随时改）",
				t.rec.Slug)
		}
	case VolumeStateMissing:
		a.block(NoticeDeviceMissing, "%s", t.obs.Detail)
	case VolumeStateMismatch:
		a.block(NoticeFilesystemMismatch, "%s", t.obs.Detail)
	case VolumeStateError, VolumeStateUnknown:
		a.block(NoticeFilesystemUnreadable, "%s", t.obs.Detail)
	}
	if err := want.Validate(); err != nil {
		a.block(NoticeVolumeConfigInvalid, "%v", err)
	}
	if msgs := checkMountPointPath(env, t.rec.MountPath()); len(msgs) > 0 {
		for _, msg := range msgs {
			a.block(NoticeMountPointUnsafe, "%s", msg)
		}
	}
	// 自动挂载为真时要求设备在位：findmnt 的校验会把“开机必需的源找不到”当成错误，
	// 与其在写文件时才失败，不如在这里说清楚。
	if want.Automount && t.obs.State == VolumeStateMissing {
		a.block(NoticeDeviceMissing, "开机自动挂载要求设备在开机时可达；现在读不到它，先修好设备再打开自动挂载")
	}
	// 身份绑定：改配置要写 fstab、要好几次读这块设备，执行端必须能复核“要动的还是预检时那个”。
	// 少了这一句，预检能过但结果是**不合法**的计划（“预检必须绑定一个可复核的目标”），
	// 面板侧于是拿到 500——这正是 P07.4 真机验收在 configure 上第一次撞到的洞。
	if t.obs.DeviceID != "" {
		a.IdentityFingerprint = t.obs.IdentityFingerprint
	}
	a.impact(ImpactConfigChange, fmt.Sprintf("会把 %s 的受管块改成 %s（块外内容不动）",
		env.Fstab.fstabPath(), DescribeOptions(MountOptions(want))))
}

// assessVolumeForget 判“能不能退出管理”。
func assessVolumeForget(ctx context.Context, a *Assessment, topo Topology, index topologyIndex,
	p VolumeParams, env VolumeEnv) {
	t, ok := loadVolume(ctx, a, topo, index, p.VolumeID, p.Version, env)
	if !ok {
		return
	}
	a.Locks = append(a.Locks, "volume:"+t.rec.ID, "fstab")
	a.StateVersion = volumeStateVersion(topo, t.rec, t.fstab, t.obs.stateKey())

	switch t.obs.State {
	case VolumeStateMounted:
		a.block(NoticeVolumeMounted, "卷 %s 还挂着，先卸载再退出管理（面板不会替你卸一个还有人在用的卷）", t.rec.Slug)
	}
	// 面板侧依赖：退出管理会删掉绑定关系，因此**任何**共享都算（禁用的也算）。
	for _, b := range dependencyBlockers(ctx, t.rec, PurposeForget, env) {
		a.appendBlocker(b)
	}
	if t.obs.DeviceID != "" {
		a.IdentityFingerprint = t.obs.IdentityFingerprint
	}
	a.impact(ImpactConfigChange, fmt.Sprintf(
		"会从 %s 的受管块里删掉 UUID=%s 的条目并删除登记记录；盘上的数据一个字节都不动",
		env.Fstab.fstabPath(), t.rec.FilesystemUUID))
}

// volumeDependencies 问面板侧依赖（共享/应用/备份）。
//
// 三种结果必须分得开，混淆任何一种都会做出错误判决：
//
//   - 返回空数组、err 为 nil：问过了，确实没有依赖。
//   - 返回错误：**没问成**。调用方必须阻塞——证明不了没人用，就不能卸、不能格式化。
//   - env.Dependents 为 nil：装配层没接线，同样按“没问成”处理。
//
// P07.4 时共享模块还不存在，这里的空数组意思是“没有依赖”；从 P08.3 起它接的是
// 真实的共享查询，空数组才真的意味着没人依赖。
func volumeDependencies(ctx context.Context, rec VolumeRecord, purpose DependencyPurpose, env VolumeEnv) ([]VolumeDependency, error) {
	if env.Dependents == nil {
		return nil, errors.New("本机没有配置面板依赖查询")
	}
	return env.Dependents(ctx, DependencyQuery{VolumeID: rec.ID, Purpose: purpose})
}

// dependencyBlockers 把依赖查询结果翻成阻塞项。
//
// 查询失败不能静默放过：它和“查到了依赖”一样会挡住动作，只是原因不同。
func dependencyBlockers(ctx context.Context, rec VolumeRecord, purpose DependencyPurpose, env VolumeEnv) []Blocker {
	deps, err := volumeDependencies(ctx, rec, purpose, env)
	if err != nil {
		return []Blocker{{
			Code: NoticeVolumeDependenciesUnknown,
			Message: fmt.Sprintf("无法确认卷 %s 上还有没有面板对象依赖它（%v）。不能靠“大概没人用”动手",
				rec.Slug, err),
		}}
	}
	if len(deps) == 0 {
		return nil
	}
	// 同一个查询在卸载和退出管理里的语义不同，报告里的编码也就不同：
	// 卸载说的是“还有共享在对外服务”，退出管理说的是“会丢绑定关系”。
	code := NoticeVolumeHasDependencies
	msg := "卷 %s 还被这些对象依赖：%s"
	if purpose == PurposeUnmount {
		code = NoticeVolumeInUseByShare
		msg = "卷 %s 上还有启用中的共享在对外提供访问（%s）。先停用或删除这些共享再卸载"
	}
	return []Blocker{{Code: code, Message: fmt.Sprintf(msg, rec.Slug, describeDependencies(deps))}}
}

// describeDependencies 把依赖列表压成一句话。
func describeDependencies(deps []VolumeDependency) string {
	parts := make([]string, 0, len(deps))
	for _, d := range deps {
		parts = append(parts, d.Kind+" "+d.Name)
	}
	sort.Strings(parts)
	return strings.Join(parts, "、")
}

// volumeBusyBlockers 收集“谁在占着这个卷”。
//
// 卸载被拒时页面要能说清是哪个挂载点、哪个 pid——这正是 CONTRACTS.md 第 6 节
// GET /storage/volumes/:id/blockers 的内容，也是 P07.4 要求里的“繁忙占用列表”。
func volumeBusyBlockers(ctx context.Context, rec VolumeRecord, obs VolumeObservation, env VolumeEnv) []Blocker {
	// 注意这里**不**把“设备还有挂载点”当成阻塞项：这个函数在判决里被调用时，
	// 卷本来就该是挂着的（那正是要卸载它的理由）。只有 umount 失败之后
	// “挂载点还在”才是证据，那一句由执行器自己拼（volumewrite.go）。
	var out []Blocker
	if env.Busy == nil {
		out = append(out, Blocker{
			Code:    NoticeBusy,
			Message: "无法确认是否有进程在使用这个卷（本机没有配置占用检查）",
		})
		return out
	}
	// 挂载着就先看挂载点：umount 真正会撞上的是“这个文件系统上还有打开的文件”。
	// 设备名读不到不至于让这条检查失效——挂载点上的文件照样能查。
	procs, err := scanVolumeHolders(ctx, obs.DeviceID, obs.Mounts, env.Busy)
	if err != nil {
		out = append(out, Blocker{
			Code: NoticeBusy,
			Message: fmt.Sprintf("检查%s的进程占用失败（无法确认没人在用）：%v",
				holdersTarget(obs.DeviceID, obs.Mounts), err),
		})
		return out
	}
	if len(procs) > 0 {
		out = append(out, Blocker{
			Code: NoticeVolumeBusy,
			Message: fmt.Sprintf("有 %d 个进程正打开着 %s：%s。先停掉它们，或把应用改成从挂载点访问",
				len(procs), holdersTarget(obs.DeviceID, obs.Mounts), stringsJoinPIDs(procs, 5)),
		})
	}
	return out
}

// VolumeObservation 是一次现场观察的结果。
type VolumeObservation struct {
	// State 是 observed_state 闭集里的一个值。
	State string
	// Detail 说明 State 为什么是它（错误消息与页面都用它）。
	Detail string
	// MountedElsewhere 表示这块文件系统挂着，但不在面板的挂载点上。
	//
	// 它不是一个独立的 observed_state：契约里 observed_state 是闭集
	// （mounted/unmounted/missing/mismatch/error/unknown），而“挂在别处”
	// 在事实上仍然是 mounted。把区别放在这个布尔上，既不改契约，
	// 又能在判决里精确拒绝（挂载点之外的东西不归本次操作管）。
	MountedElsewhere bool
	// DeviceID 是拓扑里那台设备现在的名字（记录里的名字可能已经过期）。
	DeviceID string
	// IdentityFingerprint 是设备的身份指纹（换盘检测用）。
	IdentityFingerprint string
	// Filesystem 是读到的文件系统身份。
	Filesystem FilesystemIdentity
	// Mounts 是这块设备当前的挂载点。
	Mounts []string
	// Capacity 只在挂载时存在。
	Capacity *Capacity
}

// observeVolume 现场判断一个卷现在的状态。
//
// 判断顺序是有讲究的：先看设备在不在（不在就没什么可读的），再读超级块
// （UUID 对不上说明路径被复用），最后才看挂载状态。
func observeVolume(ctx context.Context, topo Topology, index topologyIndex, rec VolumeRecord, env VolumeEnv) VolumeObservation {
	obs := VolumeObservation{State: VolumeStateUnknown, Detail: "还没有检查"}
	dev := index.devices[rec.DeviceID]
	if dev == nil {
		obs.State = VolumeStateMissing
		obs.Detail = fmt.Sprintf("设备 %s 不在当前拓扑里（拔掉了、改名了，或者这块盘已经不在了）", rec.DeviceID)
		return obs
	}
	obs.DeviceID = dev.ID
	obs.IdentityFingerprint = targetIdentity(dev, index)
	obs.Mounts = mountsOf(dev)
	if t := dev.Filesystem; t != nil {
		for i := range t.Mounts {
			if t.Mounts[i].Path == rec.MountPath() {
				obs.Capacity = t.Mounts[i].Capacity
			}
		}
	}
	if env.Superblocks == nil {
		obs.State = VolumeStateUnknown
		obs.Detail = "本机没有配置文件系统读取器，无法确认 UUID"
		return obs
	}
	id, err := env.Superblocks.Probe(ctx, dev.ID, dev.SizeBytes)
	switch {
	case err != nil:
		obs.State = VolumeStateError
		obs.Detail = fmt.Sprintf("读 %s 的文件系统身份失败：%v", dev.ID, err)
		return obs
	case id.UUID == "":
		obs.State = VolumeStateError
		obs.Detail = fmt.Sprintf("%s 上没有读到文件系统 UUID", dev.ID)
		return obs
	}
	obs.Filesystem = id
	if !strings.EqualFold(id.UUID, rec.FilesystemUUID) {
		obs.State = VolumeStateMismatch
		obs.Detail = fmt.Sprintf("%s 上的 UUID 是 %s，登记的是 %s——路径被别的文件系统复用了",
			dev.ID, id.UUID, rec.FilesystemUUID)
		return obs
	}
	if mountedAt(obs.Mounts, rec.MountPath()) {
		obs.State = VolumeStateMounted
		obs.Detail = "挂在 " + rec.MountPath()
		return obs
	}
	if len(obs.Mounts) > 0 {
		obs.State = VolumeStateMounted
		obs.MountedElsewhere = true
		obs.Detail = fmt.Sprintf("%s 上的文件系统挂在 %s，不是面板的挂载点 %s",
			dev.ID, strings.Join(obs.Mounts, "、"), rec.MountPath())
		return obs
	}
	obs.State = VolumeStateUnmounted
	obs.Detail = "没有挂载"
	return obs
}

// stateKey 是这个观察结果的版本成分（“挂在别处”与“挂在对的地方”必须算出不同版本）。
func (o VolumeObservation) stateKey() string {
	if o.MountedElsewhere {
		return o.State + ":elsewhere"
	}
	return o.State
}

// volumeStateVersion 把“会影响这次动作的外部事实”压成一个可逐字比较的字符串。
//
// 装进来的三样东西各有分工：
//   - 拓扑版本：结构变了（分区表、挂载、设备集合）——这是“世界变了”的信号；
//   - 记录版本：登记被改过（另一个操作、或者手工改库）；
//   - fstab 哈希：只在会重写 fstab 的动作里算（传零值就不参与）——
//     别人刚改过 fstab 时，覆盖他的改动是这里最该防的事；
//   - 观察状态：挂载状态与 UUID 的一致性（例如“本来没挂，现在挂上了”）。
//
// 时间戳不参与：同样的世界必须算出同样的版本，否则每次预检都“过期”。
func volumeStateVersion(topo Topology, rec VolumeRecord, fst FstabState, observed string) string {
	fstabPart := "fstab:-"
	if fst.Hash != "" {
		fstabPart = "fstab:" + fst.Hash
	}
	return strings.Join([]string{
		"topo:" + topo.SnapshotVersion,
		"vol:" + rec.ID + "@" + rec.Version,
		fstabPart,
		"obs:" + observed,
	}, "|")
}

// volumeDisplayName 是页面与通知里用的名字。
func volumeDisplayName(rec VolumeRecord) string {
	name := rec.DisplayName
	if name == "" {
		name = rec.Slug
	}
	return fmt.Sprintf("%s（%s）", name, rec.MountPath())
}

// mountsOf 取设备的挂载点列表。
func mountsOf(dev *Device) []string {
	if dev == nil || dev.Filesystem == nil {
		return nil
	}
	out := make([]string, 0, len(dev.Filesystem.Mounts))
	for _, m := range dev.Filesystem.Mounts {
		out = append(out, m.Path)
	}
	sort.Strings(out)
	return out
}

// mountedAt 报告挂载点列表里有没有 path。
func mountedAt(mounts []string, path string) bool {
	for _, m := range mounts {
		if m == path {
			return true
		}
	}
	return false
}

// mountOptionsDiffer 报告两份配置的**挂载时**选项（除 automount 外）是否不同。
func mountOptionsDiffer(a, b VolumeConfig) bool {
	return a.ReadOnly != b.ReadOnly || a.Nodev != b.Nodev || a.Nosuid != b.Nosuid
}

// ExternalFstabRef 是 fstab 里一条不属于托管块的引用。
type ExternalFstabRef struct {
	// Line 是行号（从 1 开始）。
	Line int
	// Text 是这一行的内容（截断到 160 字节）。
	Text string
}

// ExternalFstabRefs 找出块外引用了这个 UUID 或这个挂载点的行。
//
// “块外”是关键：托管块里当然会有这个 UUID（那是我们自己写的），它不构成冲突。
// 判据是行首字段等于 `UUID=<uuid>`（大小写不敏感）或第二字段等于 mountPoint，
// 注释行与空行跳过。
func ExternalFstabRefs(content, uuid, mountPoint string) []ExternalFstabRef {
	lines := splitLines(content)
	beginAt, endAt, err := findBlockMarkers(lines)
	var out []ExternalFstabRef
	for i, line := range lines {
		if err == nil && beginAt >= 0 && i >= beginAt && i <= endAt {
			continue // 托管块内部
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		hit := false
		if uuid != "" && len(fields) >= 1 {
			upper := strings.ToUpper(fields[0])
			if strings.HasPrefix(upper, "UUID=") {
				if strings.EqualFold(strings.TrimPrefix(upper, "UUID="), uuid) {
					hit = true
				}
			}
		}
		if mountPoint != "" && len(fields) >= 2 && fields[1] == mountPoint {
			hit = true
		}
		if !hit {
			continue
		}
		text := trimmed
		if len(text) > 160 {
			text = text[:160] + "…"
		}
		out = append(out, ExternalFstabRef{Line: i + 1, Text: text})
	}
	return out
}

// checkMountPointPath 检查挂载点路径上有没有危险成分。
//
// 返回的是给人看的理由列表（空表示没问题）。检查的是**从根开始的每一段**：
// 只要 /srv 或 /srv/upanel 是一个符号链接，最终路径就可能落在别处，
// 而 fstab 里写的是那个字面路径——两者不一致时，挂载会落在你没想到的位置。
func checkMountPointPath(env VolumeEnv, mountPoint string) []string {
	var msgs []string
	if !strings.HasPrefix(mountPoint, DefaultVolumeRoot+"/") {
		return []string{fmt.Sprintf("挂载点 %q 不在 %s 下", mountPoint, DefaultVolumeRoot)}
	}
	if clean := path.Clean(mountPoint); clean != mountPoint {
		return []string{fmt.Sprintf("挂载点 %q 不是规范路径（应为 %q）", mountPoint, clean)}
	}
	// 逐段 Lstat：符号链接必须在“穿越之前”被发现，所以不能只 stat 最终路径。
	for _, dir := range pathSegments(mountPoint) {
		fi, err := env.lstat(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// 路径还没建出来是正常的（我们会 mkdir），后面的段也不会存在。
				break
			}
			msgs = append(msgs, fmt.Sprintf("检查 %s 失败：%v", dir, err))
			break
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			msgs = append(msgs, fmt.Sprintf("%s 是符号链接。挂载点路径上不允许符号链接（挂载会落在链接目标上）", dir))
			break
		}
		if !fi.IsDir() {
			msgs = append(msgs, fmt.Sprintf("%s 不是目录（%s），不能作为挂载点的上级", dir, fi.Mode().Type()))
			break
		}
		if dir == DefaultVolumeRoot || dir == env.root() {
			// 受管理挂载点的父目录必须是 root 所有、且不可被普通用户写入：
			// 否则普通用户可以把自己的目录挂到这里，或者塞进一个符号链接。
			if uid, _, ok := fileOwner(fi); ok && uid != 0 {
				msgs = append(msgs, fmt.Sprintf("%s 的属主是 uid %d，不是 root", dir, uid))
			}
			if fi.Mode().Perm()&0o002 != 0 {
				msgs = append(msgs, fmt.Sprintf("%s 允许任何用户写入（%#o）", dir, fi.Mode().Perm()))
			}
		}
	}
	return msgs
}

// pathSegments 把绝对路径拆成从根开始的每一段（不含根）。
func pathSegments(p string) []string {
	// 用 path.Clean 而不是 filepath.Clean：这是**目标主机**上的 POSIX 路径，
	// 不是本进程所在平台的路径。在 Windows 上 filepath.Clean 会把它变成反斜杠。
	p = path.Clean(p)
	if !strings.HasPrefix(p, "/") {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	out := make([]string, 0, len(parts))
	cur := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		cur += "/" + part
		out = append(out, cur)
	}
	return out
}

// mountPointContents 报告挂载点目录里有没有东西（挂载后会被遮住）。
func mountPointContents(env VolumeEnv, mountPoint string) []string {
	fi, err := env.lstat(mountPoint)
	if err != nil || !fi.IsDir() {
		return nil
	}
	entries, err := env.readDir(mountPoint)
	if err != nil {
		return []string{fmt.Sprintf("读不出 %s 里有什么（%v）", mountPoint, err)}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		if len(names) == 5 {
			break
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return []string{fmt.Sprintf("%s 里已经有文件（%s），挂载之后这些文件会被遮住（数据不会丢，但看不到）",
		mountPoint, strings.Join(names, "、"))}
}

// superblocks 返回超级块读取器（nil 时返回一个明确报错的实现）。
func (e VolumeEnv) superblocks() SuperblockProber {
	if e.Superblocks != nil {
		return e.Superblocks
	}
	return missingSuperblockProber{}
}

// missingSuperblockProber 在没有配置读取器时说清楚，而不是返回一个空身份
// （空身份会被当成“没有文件系统”，那是另一个结论）。
type missingSuperblockProber struct{}

func (missingSuperblockProber) Probe(ctx context.Context, devID string, sizeBytes uint64) (FilesystemIdentity, error) {
	return FilesystemIdentity{}, errors.New("本机没有配置文件系统读取器")
}

// Apply 把 configure 的参数叠加到现有配置上（未给的字段保持不变）。
func (p VolumeConfigureParams) Apply(current VolumeConfig) (VolumeConfig, error) {
	out := current
	if p.Automount != nil {
		out.Automount = *p.Automount
	}
	if p.ReadOnly != nil {
		out.ReadOnly = *p.ReadOnly
	}
	if p.Nodev != nil {
		out.Nodev = *p.Nodev
	}
	if p.Nosuid != nil {
		out.Nosuid = *p.Nosuid
	}
	// 一份“什么都没改”的 configure 是允许的：它仍然会重写一遍受管块，
	// 这正是“手工改坏了块内条目”之后的修复手段。
	return out, nil
}

// DescribeConfig 把配置渲染成一句人读的说明（审计摘要用）。
func DescribeConfig(c VolumeConfig) string {
	parts := []string{}
	if c.ReadOnly {
		parts = append(parts, "只读")
	} else {
		parts = append(parts, "读写")
	}
	if c.Automount {
		parts = append(parts, "开机自动挂载")
	} else {
		parts = append(parts, "开机不自动挂载")
	}
	if c.Nodev {
		parts = append(parts, "nodev")
	}
	if c.Nosuid {
		parts = append(parts, "nosuid")
	}
	return strings.Join(parts, "、")
}

// importIdentityFingerprint 是导入动作的身份指纹：设备身份 + 盘上文件系统的身份。
//
// 为什么文件系统也算身份：用户预检时批准的是“接管这台分区上这个 UUID 的 ext4”。
// 执行前如果盘上被重新格式化，设备身份指纹可以一个字都不变，而 fstab 会按**新的**
// UUID 写下去——那等于在用户没同意的情况下换掉了他要接管的东西。把 (类型, UUID)
// 融进指纹之后，performVolume 的身份门就会以 DEVICE_IDENTITY_CHANGED 拒掉这种情形。
//
// 登记记录里存的仍然是纯设备身份（targetIdentity）：之后挂载/卸载/改配置要比的是
// “还是那块盘”，文件系统身份由 FilesystemUUID 单独核对。
func importIdentityFingerprint(dev *Device, index topologyIndex, id FilesystemIdentity) string {
	base := targetIdentity(dev, index)
	if base == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("upanel-volume-fs-v1|" + base + "|" +
		strings.ToLower(id.Kind) + "|" + strings.ToLower(id.UUID)))
	return base + "|fs:" + hex.EncodeToString(sum[:8])
}
