package storage

// P07.4：受管理卷的读取视图（列表、详情、占用）。
//
// 这一层只读：它把代理侧登记记录 + 现场观察到的事实拼成 CONTRACTS.md 第 6 节的
// Volume 结构。observed_state 与 capacity **不落库**——存进数据库的话，第二天早上
// 那句“它现在挂着没有”就变成假话了。
//
// 代价写在这里，免得以后被当成 bug：列一次卷会给每个卷读一次超级块（几毫秒），
// 因为 UUID 只能从盘上的字节里读出来。卷数量上去之后（十几二十个）应当给这一步加缓存，
// 但那需要与“预检要求现场事实”这条规则一起设计，不是现在顺手能加的东西。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrVolumeNotFound 表示登记表里没有这个卷。
//
// 与 storage.ErrAgentUnavailable 同层：都是“调用方该翻译成 404/503 的已知错误”。
var ErrVolumeNotFound = errors.New("storage: 卷不存在")

// Volumes 返回全部受管理卷的对外视图（按挂载点排序）。
func (c *Collector) Volumes(ctx context.Context) ([]Volume, error) {
	env := c.volumeEnv()
	if env.Store == nil {
		return nil, dependencyMissing("没有配置受管理卷的登记表，本机不能查询卷")
	}
	records, err := env.Store.ListVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: 读取卷登记失败: %w", err)
	}
	topo := c.Topology(ctx)
	index := indexTopology(topo)
	out := make([]Volume, 0, len(records))
	for _, rec := range records {
		out = append(out, volumeView(ctx, rec, observeVolume(ctx, topo, index, rec, env), env))
	}
	sortVolumes(out)
	return out, nil
}

// Volume 返回单个受管理卷的视图。
func (c *Collector) Volume(ctx context.Context, id string) (Volume, error) {
	env := c.volumeEnv()
	if env.Store == nil {
		return Volume{}, dependencyMissing("没有配置受管理卷的登记表，本机不能查询卷")
	}
	rec, found, err := env.Store.GetVolume(ctx, id)
	if err != nil {
		return Volume{}, fmt.Errorf("storage: 读取卷登记失败: %w", err)
	}
	if !found {
		return Volume{}, ErrVolumeNotFound
	}
	topo := c.Topology(ctx)
	return volumeView(ctx, rec, observeVolume(ctx, topo, indexTopology(topo), rec, env), env), nil
}

// VolumeBlockers 列出“为什么现在卸载不了”（CONTRACTS.md 第 6 节）。
//
// 它是页面上一句“设备忙”背后的全部事实：哪个挂载点、哪些进程、有没有 dm/md 消费者、
// 是不是活动交换区。空数组就是空数组——没有的东西不写 null，页面才好判断。
func (c *Collector) VolumeBlockers(ctx context.Context, id string) (VolumeBlockers, error) {
	env := c.volumeEnv()
	if env.Store == nil {
		return VolumeBlockers{}, dependencyMissing("没有配置受管理卷的登记表，本机不能查询占用")
	}
	rec, found, err := env.Store.GetVolume(ctx, id)
	if err != nil {
		return VolumeBlockers{}, fmt.Errorf("storage: 读取卷登记失败: %w", err)
	}
	if !found {
		return VolumeBlockers{}, ErrVolumeNotFound
	}
	topo := c.Topology(ctx)
	index := indexTopology(topo)
	obs := observeVolume(ctx, topo, index, rec, env)

	out := VolumeBlockers{
		VolumeID:  rec.ID,
		Mounted:   obs.State == VolumeStateMounted,
		Mounts:    obs.Mounts,
		Processes: []BusyProcess{},
		Holders:   []string{},
		Apps:      []VolumeDependency{},
		Shares:    []VolumeDependency{},
		Backups:   []VolumeDependency{},
		CheckedAt: env.now().UTC().Truncate(time.Second),
	}
	if out.Mounts == nil {
		out.Mounts = []string{}
	}
	if dev := index.devices[rec.DeviceID]; dev != nil {
		// Swap 非 nil 就是活动交换区：/proc/swaps 里只会列出在用的。
		if dev.Swap != nil {
			out.SwapActive = true
		}
		if len(dev.Holders) > 0 {
			out.Holders = append(out.Holders, dev.Holders...)
		}
	}
	// 进程占用只在挂载时才有意义：没挂着的卷没人能打开它的设备，也没有挂载点可查。
	if out.Mounted {
		if env.Busy == nil {
			// 没有占用检查器不等于“没人在用”。把 busy 置真，页面会给出
			// “无法确认占用”的说明，而不是让人以为可以安全卸载。
			out.Busy = true
		} else if procs, err := scanVolumeHolders(ctx, obs.DeviceID, obs.Mounts, env.Busy); err != nil {
			// 同上：扫描失败也必须算“不能证明没人用”。
			out.Busy = true
		} else if len(procs) > 0 {
			out.Processes = procs
		}
	}
	// 面板侧依赖（共享/应用/备份）。查询失败时和 Busy 一样按“证明不了没人用”处理：
	// 页面上的 Busy 不是装饰，它直接对应卸载按钮是否可用。
	deps, depErr := volumeDependencies(ctx, rec, PurposeView, env)
	if depErr != nil {
		out.Busy = true
	}
	for _, dep := range deps {
		switch dep.Kind {
		case "app":
			out.Apps = append(out.Apps, dep)
		case "share":
			out.Shares = append(out.Shares, dep)
		case "backup":
			out.Backups = append(out.Backups, dep)
		}
	}
	// 结论只看“有人/有东西在用它”，**不**把“有挂载点”算进来：
	// 挂载状态本来就是独立的 mounted/mounts 字段，而且预检里的 volumeBusyBlockers
	// 也明确不把挂载点当阻塞项（挂着但空闲的卷是可以干净卸载的）。
	// 两边口径必须一致，否则页面会在预检放行的地方显示“设备忙”，
	// 用户点了卸载又成功了——那比不给按钮更糟。
	out.Busy = out.Busy || len(out.Processes) > 0 ||
		len(out.Holders) > 0 || out.SwapActive ||
		len(out.Apps) > 0 || len(out.Shares) > 0 || len(out.Backups) > 0
	return out, nil
}

// volumeView 把记录 + 观察结果拼成对外视图。
//
// 依赖查询失败时 dependencies 是空数组而不是 null：这个字段是给页面看的，
// 拼不出内容就如实留空；真正“不能动手”的判断在预检里（VOLUME_DEPENDENCIES_UNKNOWN），
// 那里才有拒绝动作的位置。
func volumeView(ctx context.Context, rec VolumeRecord, obs VolumeObservation, env VolumeEnv) Volume {
	deps, _ := volumeDependencies(ctx, rec, PurposeView, env)
	if deps == nil {
		deps = []VolumeDependency{}
	}
	v := Volume{
		ID:             rec.ID,
		Version:        rec.Version,
		DisplayName:    rec.DisplayName,
		Slug:           rec.Slug,
		DeviceID:       rec.DeviceID,
		FilesystemUUID: rec.FilesystemUUID,
		FilesystemType: rec.FilesystemType,
		MountPath:      rec.MountPath(),
		Automount:      rec.Config.Automount,
		ReadOnly:       rec.Config.ReadOnly,
		Nodev:          rec.Config.Nodev,
		Nosuid:         rec.Config.Nosuid,
		DesiredState:   rec.DesiredState,
		ObservedState:  obs.State,
		Capacity:       obs.Capacity,
		Dependencies:   deps,
		UpdatedAt:      rec.UpdatedAt,
	}
	// 观察到的设备名与登记的不一致时，用现场的：设备名会随插拔顺序变，
	// 页面显示一个已经不存在的 sdb1 只会误导人。
	if obs.DeviceID != "" {
		v.DeviceID = obs.DeviceID
	}
	if obs.State != VolumeStateMounted && obs.State != VolumeStateUnmounted {
		v.ObservedError = obs.Detail
	}
	if v.ObservedState == "" {
		v.ObservedState = VolumeStateUnknown
	}
	if v.DesiredState == "" {
		v.DesiredState = DesiredUnmounted
	}
	return v
}
