package storage

// P07.4：受管理卷的执行器——导入、挂载、卸载、改配置、退出管理。
//
// 与 P07.2/P07.3 的写原语共用**同一套复核流程**（重扫拓扑 → 重新判决 → 比身份指纹 →
// 比状态版本 → 看判决是否允许），因为卷动作同样会改变这台机器的状态。区别在于
// 它改的不是盘上的字节，而是 /etc/fstab 里属于面板的那一块，以及当前的挂载状态。
//
// 三个必须成立的次序：
//
//  1. **先核身份，再写 fstab**。写之前直接读一次超级块：UUID 与类型必须与预检时一致。
//     缓存里的 UUID 不算数——fstab 里写错一个 UUID，代价是这台机器下次起不来。
//  2. **先写候选、校验、备份，再 rename**。这一步在 FstabStore.Write 里，本文件只负责
//     在它之后接上 `systemctl daemon-reload`：util-linux 2.39 的 mount 会代理到 systemd，
//     不 reload 的系统仍按旧 fstab 生成挂载单元（实测会有 "fstab has been modified" 警告）。
//  3. **任何一步失败都要回到原样**。fstab 已经替换之后失败（daemon-reload 失败、登记表
//     写失败、读回核验不符）→ 用备份恢复；卷已经挂上之后登记失败 → 卸回去。恢复失败时
//     必须把备份路径写进错误消息，让人能手工接手。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// 卷管理用到的固定程序与时序。
const (
	// DefaultSystemctlPath 用来 `daemon-reload` 与查挂载单元的 LoadState。
	DefaultSystemctlPath = "/usr/bin/systemctl"
	// VolumeCommandTimeout 是 mount/umount/findmnt/systemctl 单条命令的时限。
	// 比 sfdisk 长：装了 nofail 与 30 秒设备超时的条目在设备慢时会更久才返回。
	VolumeCommandTimeout = 90 * time.Second
)

// 卷动作的进度步骤（稳定枚举，进 job_events 的 payload.step）。
const (
	VolumeStepFstab  = "fstab"
	VolumeStepReload = "daemon_reload"
	VolumeStepMount  = "mount"
	VolumeStepUmount = "umount"
	VolumeStepVerify = "verify"
)

// CheckVolumeDependencies 检查卷管理的固定程序是否就位。
//
// 只用于启动日志：缺一个程序不阻止代理启动（拓扑与设备动作仍然可用），卷动作会在
// 预检/执行时以 DEPENDENCY_MISSING 明确拒绝。这里主动报出来，是为了让“为什么导入卷
// 按钮点了没反应”在启动日志里就有答案。
func CheckVolumeDependencies() error {
	return requireFiles([]programNeed{
		{name: "mount", path: DefaultMountPath},
		{name: "umount", path: DefaultUmountPath},
		{name: "findmnt", path: DefaultFindmntPath},
		{name: "systemctl", path: DefaultSystemctlPath},
	})
}

// requireVolumeDeps 检查执行卷动作需要的外部依赖。
//
// 缺登记表就没有“我管了哪些卷”这件事；缺 fstab 读写器就不能写；缺命令执行器就既不能
// 校验候选文件也不能 daemon-reload。三者缺一都明确拒绝，而不是跳过某一步。
func (w *writer) requireVolumeDeps() error {
	if w.runner == nil {
		return dependencyMissing("没有配置命令执行器，本机不能执行卷管理动作")
	}
	if w.scanner == nil {
		return dependencyMissing("没有配置存储拓扑扫描，本机不能执行卷管理动作")
	}
	if w.store == nil {
		return dependencyMissing("没有配置受管理卷的登记表，本机不能执行卷管理动作")
	}
	if w.superblocks == nil {
		return dependencyMissing("没有配置文件系统读取器，本机不能执行卷管理动作")
	}
	return nil
}

// volumeEnv 组装判决与执行共用的卷环境。
//
// 与预检用的是**同一组**探针（超级块、占用）：预检与执行拿两套判断互相打架，是这类
// 动作最容易出的事故。
func (w *writer) volumeEnv() VolumeEnv {
	fst := w.fstab
	if fst.Runner == nil {
		fst.Runner = w.runner
	}
	if strings.TrimSpace(fst.Findmnt) == "" {
		fst.Findmnt = w.findmnt
	}
	env := VolumeEnv{
		Store:       w.store,
		Fstab:       fst,
		Superblocks: w.superblocks,
		Busy:        w.env.Busy,
		Dependents:  w.env.Dependents,
		Root:        w.volumeRoot,
		Lstat:       w.lstat,
		ReadDir:     w.readDir,
		Now:         w.now,
	}
	return env
}

// executeVolume 是一次卷动作的全过程，并负责把中断归类。
//
// 中断的判定与盘写动作同样重要：mount/umount 被 SIGKILL 打断时，内核可能已经完成
// 了挂载——那既不是“没做”，也不是“做完了”，只能报 interrupted 让人核实。
func (w *writer) executeVolume(ctx context.Context, req WriteRequest) (WriteReport, error) {
	report, err := w.performVolume(ctx, req)
	if err == nil {
		return report, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return WriteReport{}, interruptedVolume(ctxErr, targetVolumeID(req.Params))
	}
	return WriteReport{}, err
}

// targetVolumeID 从参数里取卷标识（中断消息用）。
func targetVolumeID(params any) string {
	switch p := params.(type) {
	case VolumeParams:
		return p.VolumeID
	case VolumeConfigureParams:
		return p.VolumeID
	case VolumeImportParams:
		return "（新卷 " + p.Slug + "）"
	}
	return ""
}

// interruptedVolume 是“上下文结束、实际状态未知”的统一错误。
func interruptedVolume(ctxErr error, volumeID string) error {
	return commandFailed("任务上下文在卷 %s 的操作过程中结束（%v），实际状态未知。重新提交之前先在主机上核实挂载状态与 %s",
		volumeID, ctxErr, DefaultFstabPath)
}

// performVolume 是 executeVolume 的主体。
func (w *writer) performVolume(ctx context.Context, req WriteRequest) (WriteReport, error) {
	if err := w.requireVolumeDeps(); err != nil {
		return WriteReport{}, err
	}
	env := w.volumeEnv()
	topo, err := w.scanner.ScanNow(ctx)
	if err != nil {
		return WriteReport{}, commandFailed("执行前重新扫描存储拓扑失败：%v", err)
	}
	if topo.Status == StatusError {
		return WriteReport{}, commandFailed("执行前重新扫描存储拓扑失败：%s", topo.Error)
	}
	assessment, err := AssessVolume(ctx, topo, req.Action, req.Params, env)
	if err != nil {
		return WriteReport{}, err
	}
	// 身份指纹为空表示“现场认不出这台设备”（拔了、读不到）。这时不做换盘判定：
	// 卷动作真正的身份门是超级块里的 UUID（observeVolume 已经比对过，不一致就拒绝）。
	// 强求指纹一致会让“设备不在了的卷”永远无法退出管理，而退出管理恰恰是清理动作。
	if assessment.IdentityFingerprint != "" && assessment.IdentityFingerprint != req.ExpectedIdentity {
		return WriteReport{}, identityChanged(assessment.DisplayName)
	}
	if assessment.StateVersion != req.ExpectedStateVersion {
		return WriteReport{}, preflightStale("卷的状态版本是 " + assessment.StateVersion)
	}
	if !assessment.Allowed {
		return WriteReport{}, notAllowed(assessment.Blockers)
	}
	progress(req, WriteStepRecheck, map[string]any{
		"volume_id": assessment.TargetID,
		"version":   assessment.StateVersion,
	})
	index := indexTopology(topo)
	switch req.Action {
	case ActionVolumeImport:
		return w.volumeImport(ctx, req, env, index)
	case ActionVolumeMount:
		return w.volumeMount(ctx, req, env, index)
	case ActionVolumeUnmount:
		return w.volumeUnmount(ctx, req, env, index)
	case ActionVolumeConfigure:
		return w.volumeConfigure(ctx, req, env, index)
	case ActionVolumeForget:
		return w.volumeForget(ctx, req, env, index)
	}
	return WriteReport{}, fmt.Errorf("%w: %s", ErrNoWritePrimitive, req.Action)
}

// volumeImport 把一个既有分区纳管：写 fstab 受管块 → daemon-reload → 存登记 → 两源核验。
func (w *writer) volumeImport(ctx context.Context, req WriteRequest, env VolumeEnv, index topologyIndex) (WriteReport, error) {
	p, ok := req.Params.(VolumeImportParams)
	if !ok {
		return WriteReport{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", req.Action, req.Params)
	}
	dev := index.devices[p.DeviceID]
	if dev == nil {
		return WriteReport{}, preflightStale(p.DeviceID + " 不在当前拓扑里")
	}
	// 写之前最后一次直接读设备：UUID 与类型以盘上的字节为准。
	id, err := env.superblocks().Probe(ctx, dev.ID, dev.SizeBytes)
	if err != nil {
		return WriteReport{}, commandFailed("写 %s 之前读 %s 的文件系统身份失败：%v", DefaultFstabPath, dev.ID, err)
	}
	if id.UUID == "" || (id.Kind != FilesystemExt4 && id.Kind != FilesystemXFS) {
		return WriteReport{}, commandFailed("%s 上的文件系统与预检时不同（uuid=%q type=%q），拒绝写入 %s",
			dev.ID, id.UUID, id.Kind, DefaultFstabPath)
	}
	volumeID, err := NewVolumeID()
	if err != nil {
		return WriteReport{}, err
	}
	now := env.now()
	rec := VolumeRecord{
		ID:                  volumeID,
		DisplayName:         strings.TrimSpace(p.DisplayName),
		Slug:                p.Slug,
		DeviceID:            dev.ID,
		FilesystemUUID:      id.UUID,
		FilesystemType:      id.Kind,
		IdentityFingerprint: targetIdentity(dev, index),
		Config:              DefaultVolumeConfig(),
		DesiredState:        DesiredUnmounted,
		Version:             "1",
		LastOperationID:     req.OperationID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if rec.DisplayName == "" {
		rec.DisplayName = rec.Slug
	}
	// 挂载点目录：fstab 不建目录，但挂载要用它。先建（0755，root）——建不出来就什么都不写。
	if err := w.ensureMountPoint(rec.MountPath()); err != nil {
		return WriteReport{}, err
	}
	entry := rec.FstabEntry()
	backup, err := w.applyFstab(ctx, req, &entry, "")
	if err != nil {
		return WriteReport{}, err
	}
	if err := w.store.SaveVolume(ctx, rec); err != nil {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("写入登记记录失败：%v", err))
	}
	return w.verifyVolumeRecord(ctx, req, env, rec, backup, map[string]any{
		"operation": "import",
		"backup":    backup,
	})
}

// volumeMount 挂载一个已登记的卷。
func (w *writer) volumeMount(ctx context.Context, req WriteRequest, env VolumeEnv, index topologyIndex) (WriteReport, error) {
	p, ok := req.Params.(VolumeParams)
	if !ok {
		return WriteReport{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", req.Action, req.Params)
	}
	rec, found, err := w.store.GetVolume(ctx, p.VolumeID)
	if err != nil {
		return WriteReport{}, commandFailed("读取卷 %s 的登记失败：%v", p.VolumeID, err)
	}
	if !found {
		return WriteReport{}, preflightStale("卷 " + p.VolumeID + " 已经不在登记表里")
	}
	// 挂载前直接读一次超级块：mount 用 UUID 匹配，但“这个 UUID 现在还在不在那块盘上”
	// 只有读盘上的字节才能回答。
	if rec.DeviceID != "" {
		if dev := index.devices[rec.DeviceID]; dev != nil {
			id, probeErr := env.superblocks().Probe(ctx, dev.ID, dev.SizeBytes)
			if probeErr != nil {
				return WriteReport{}, commandFailed("挂载 %s 之前读 %s 的文件系统身份失败：%v", rec.Slug, dev.ID, probeErr)
			}
			if !strings.EqualFold(id.UUID, rec.FilesystemUUID) {
				return WriteReport{}, preflightStale(fmt.Sprintf("%s 上的 UUID 现在是 %s，登记的是 %s",
					dev.ID, id.UUID, rec.FilesystemUUID))
			}
			rec.DeviceID = dev.ID
		}
	}
	mountPoint := rec.MountPath()
	if err := w.ensureMountPoint(mountPoint); err != nil {
		return WriteReport{}, err
	}
	progress(req, VolumeStepMount, map[string]any{"mount_point": mountPoint, "uuid": rec.FilesystemUUID})
	res, runErr := w.runFixed(ctx, w.mount, mountPoint)
	if runErr != nil {
		return WriteReport{}, commandFailed("执行 mount %s 失败：%v", mountPoint, runErr)
	}
	if res.ExitCode != 0 {
		return WriteReport{}, commandFailed("mount %s 退出码 %d：%s", mountPoint, res.ExitCode, commandDetail(res))
	}
	// 核验来源一：findmnt 读到的 UUID 必须是登记的那一个。
	// 来源二：重新扫描的拓扑里，挂载点必须出现。两个来源都对上才算 verified。
	progress(req, VolumeStepVerify, nil)
	fst, ferr := w.readMountFacts(ctx, mountPoint)
	scanErr := w.verifyMountedByScan(ctx, mountPoint)
	switch {
	case errors.Is(ferr, errNotMounted):
		return WriteReport{}, w.compensateUmount(ctx, mountPoint, rec,
			fmt.Sprintf("mount 返回 0，但 %s 不在挂载表里", mountPoint))
	case ferr != nil:
		return WriteReport{}, w.compensateUmount(ctx, mountPoint, rec, ferr.Error())
	case scanErr != nil:
		return WriteReport{}, w.compensateUmount(ctx, mountPoint, rec, scanErr.Error())
	}
	if !strings.EqualFold(fst.uuid, rec.FilesystemUUID) {
		return WriteReport{}, w.compensateUmount(ctx, mountPoint, rec, fmt.Sprintf(
			"挂载点上读到的 UUID 是 %q，登记的是 %q", fst.uuid, rec.FilesystemUUID))
	}
	rec.DesiredState = DesiredMounted
	updated, err := w.saveTransition(ctx, rec, req.OperationID)
	if err != nil {
		return WriteReport{}, w.compensateUmount(ctx, mountPoint, rec,
			fmt.Sprintf("卷已挂载，但更新登记记录失败：%v", err))
	}
	return WriteReport{
		Observed: map[string]any{
			"volume_id":   updated.ID,
			"mount_path":  mountPoint,
			"source":      fst.source,
			"filesystem":  fst.fstype,
			"uuid":        fst.uuid,
			"options":     fst.options,
			"mounted":     true,
			"checked_via": []string{"findmnt", "topology_scan"},
		},
		Data: map[string]any{
			"volume_id":  updated.ID,
			"slug":       updated.Slug,
			"mount_path": mountPoint,
			"version":    updated.Version,
		},
	}, nil
}

// volumeUnmount 卸载一个已登记的卷。
func (w *writer) volumeUnmount(ctx context.Context, req WriteRequest, env VolumeEnv, index topologyIndex) (WriteReport, error) {
	p, ok := req.Params.(VolumeParams)
	if !ok {
		return WriteReport{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", req.Action, req.Params)
	}
	rec, found, err := w.store.GetVolume(ctx, p.VolumeID)
	if err != nil {
		return WriteReport{}, commandFailed("读取卷 %s 的登记失败：%v", p.VolumeID, err)
	}
	if !found {
		return WriteReport{}, preflightStale("卷 " + p.VolumeID + " 已经不在登记表里")
	}
	mountPoint := rec.MountPath()
	progress(req, VolumeStepUmount, map[string]any{"mount_point": mountPoint})
	res, runErr := w.runFixed(ctx, w.umount, mountPoint)
	if runErr != nil {
		return WriteReport{}, commandFailed("执行 umount %s 失败：%v", mountPoint, runErr)
	}
	if res.ExitCode != 0 {
		// 常见情形是“target is busy”：把占用者再列一遍，页面上就不用自己猜了。
		busy := ""
		if topo := mustTopology(ctx, w); len(topo.Nodes) > 0 {
			obs := observeVolume(ctx, topo, indexTopology(topo), rec, env)
			parts := make([]string, 0, 2)
			// umount 失败之后“挂载点还在”才是证据：说明有东西占着它。
			if len(obs.Mounts) > 0 {
				parts = append(parts, "挂载点仍然存在："+strings.Join(obs.Mounts, "、"))
			}
			if blockers := volumeBusyBlockers(ctx, rec, obs, env); len(blockers) > 0 {
				parts = append(parts, blockers[0].Message)
			}
			if len(parts) > 0 {
				busy = " 当前占用：" + strings.Join(parts, "；")
			}
		}
		return WriteReport{}, commandFailed("umount %s 退出码 %d：%s。%s", mountPoint, res.ExitCode, commandDetail(res), busy)
	}
	progress(req, VolumeStepVerify, nil)
	// 核验：挂载点必须已经不在挂载表里（findmnt 找不到），并且拓扑里也没有这个挂载点。
	if facts, ferr := w.readMountFacts(ctx, mountPoint); ferr == nil && facts.mounted {
		return WriteReport{}, commandFailed("umount 返回 0，但 %s 仍在挂载表里（source=%s）。核实之后才能重新提交",
			mountPoint, facts.source)
	} else if ferr != nil && !errors.Is(ferr, errNotMounted) {
		return WriteReport{}, commandFailed("umount 之后没能确认 %s 的状态：%v", mountPoint, ferr)
	}
	if err := w.verifyUnmountedByScan(ctx, mountPoint); err != nil {
		return WriteReport{}, commandFailed("umount 返回 0，但重新扫描的拓扑里 %s 仍然挂着：%v", mountPoint, err)
	}
	rec.DesiredState = DesiredUnmounted
	if rec.DeviceID != "" {
		if dev := index.devices[rec.DeviceID]; dev != nil {
			rec.DeviceID = dev.ID
		}
	}
	updated, err := w.saveTransition(ctx, rec, req.OperationID)
	if err != nil {
		// 卸载已经生效。这里不自动挂回去：把一个刚被要求卸载的卷重新挂上，
		// 比让登记记录晚一步更新更危险（可能正有人在备份/迁移）。
		return WriteReport{}, commandFailed(
			"卷 %s 已卸载，但更新登记记录失败：%v。下一次预检会按现场状态重新判断，不需要重新执行卸载",
			rec.Slug, err)
	}
	return WriteReport{
		Observed: map[string]any{
			"volume_id":   updated.ID,
			"mount_path":  mountPoint,
			"mounted":     false,
			"checked_via": []string{"findmnt", "topology_scan"},
		},
		Data: map[string]any{
			"volume_id":  updated.ID,
			"slug":       updated.Slug,
			"mount_path": mountPoint,
			"version":    updated.Version,
		},
	}, nil
}

// volumeConfigure 改写一个卷的挂载配置（fstab 受管块里那一条）。
func (w *writer) volumeConfigure(ctx context.Context, req WriteRequest, env VolumeEnv, index topologyIndex) (WriteReport, error) {
	p, ok := req.Params.(VolumeConfigureParams)
	if !ok {
		return WriteReport{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", req.Action, req.Params)
	}
	rec, found, err := w.store.GetVolume(ctx, p.VolumeID)
	if err != nil {
		return WriteReport{}, commandFailed("读取卷 %s 的登记失败：%v", p.VolumeID, err)
	}
	if !found {
		return WriteReport{}, preflightStale("卷 " + p.VolumeID + " 已经不在登记表里")
	}
	cfg, err := p.Apply(rec.Config)
	if err != nil {
		return WriteReport{}, commandFailed("配置不成立：%v", err)
	}
	// 设备身份再确认一次：配置要写进 fstab，UUID 写错就是开机起不来。
	if rec.DeviceID != "" {
		if dev := index.devices[rec.DeviceID]; dev != nil {
			id, probeErr := env.superblocks().Probe(ctx, dev.ID, dev.SizeBytes)
			if probeErr != nil {
				return WriteReport{}, commandFailed("改配置之前读 %s 的文件系统身份失败：%v", dev.ID, probeErr)
			}
			if !strings.EqualFold(id.UUID, rec.FilesystemUUID) {
				return WriteReport{}, preflightStale(fmt.Sprintf("%s 上的 UUID 现在是 %s，登记的是 %s",
					dev.ID, id.UUID, rec.FilesystemUUID))
			}
		}
	}
	rec.Config = cfg
	entry := rec.FstabEntry()
	backup, err := w.applyFstab(ctx, req, &entry, "")
	if err != nil {
		return WriteReport{}, err
	}
	updated, err := w.saveTransition(ctx, rec, req.OperationID)
	if err != nil {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("更新登记记录失败：%v", err))
	}
	return w.verifyVolumeRecord(ctx, req, env, updated, backup, map[string]any{
		"operation": "configure",
		"backup":    backup,
	})
}

// volumeForget 让一个卷退出管理：从受管块里删掉条目、删掉登记记录。盘上数据不动。
func (w *writer) volumeForget(ctx context.Context, req WriteRequest, env VolumeEnv, index topologyIndex) (WriteReport, error) {
	p, ok := req.Params.(VolumeParams)
	if !ok {
		return WriteReport{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", req.Action, req.Params)
	}
	rec, found, err := w.store.GetVolume(ctx, p.VolumeID)
	if err != nil {
		return WriteReport{}, commandFailed("读取卷 %s 的登记失败：%v", p.VolumeID, err)
	}
	if !found {
		return WriteReport{}, preflightStale("卷 " + p.VolumeID + " 已经不在登记表里")
	}
	// nil 表示“删掉这个 UUID 的条目”。
	// 传 nil 条目 + 要删的 UUID：forget 是“减一行”，不是“把块清空”。
	backup, err := w.applyFstab(ctx, req, nil, rec.FilesystemUUID)
	if err != nil {
		return WriteReport{}, err
	}
	if err := w.store.DeleteVolume(ctx, rec.ID); err != nil {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("删除登记记录失败：%v", err))
	}
	// 核验：受管块里不能再有这个 UUID 的条目；挂载单元不能还停在 loaded（有条目时才会生成）。
	st, err := w.fstab.Read()
	if err != nil {
		return WriteReport{}, commandFailed("写完之后读不回 %s：%v", DefaultFstabPath, err)
	}
	if _, ok := st.Block.FindEntryByUUID(rec.FilesystemUUID); ok {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("%s 的受管块里仍然有 UUID=%s 的条目", DefaultFstabPath, rec.FilesystemUUID))
	}
	return WriteReport{
		Observed: map[string]any{
			"volume_id":   rec.ID,
			"slug":        rec.Slug,
			"mount_path":  rec.MountPath(),
			"registered":  false,
			"fstab_entry": false,
			"checked_via": []string{"fstab_reread"},
		},
		Data: map[string]any{
			"volume_id": rec.ID,
			"slug":      rec.Slug,
			"backup":    backup,
		},
	}, nil
}

// applyFstab 把“当前受管块 + 目标条目”写进 fstab，然后让 systemd 重读。
//
// want 非 nil：确保这个 UUID 的条目就是这样一行（同 UUID 的旧行被覆盖）。
// want 为 nil：删掉 dropUUID 那一行（forget）。两个参数都不会放宽任何检查——
// 条目集合最后仍然整份交给 checkFstabEntrySet 过一遍。
// 返回的备份路径用于回滚。
func (w *writer) applyFstab(ctx context.Context, req WriteRequest, want *FstabEntry, dropUUID string) (string, error) {
	env := w.volumeEnv()
	st, entries, err := w.volumeEntries(env, want, dropUUID)
	if err != nil {
		return "", err
	}
	progress(req, VolumeStepFstab, map[string]any{
		"path":    w.fstab.fstabPath(),
		"entries": len(entries),
	})
	backup, err := w.fstab.Write(ctx, st, entries)
	if err != nil {
		if errors.Is(err, ErrFstabChanged) {
			// 读与写之间有人改了 fstab：这是“预检过期”，不是磁盘故障。
			return "", preflightStale(err.Error())
		}
		return "", commandFailed("写 %s 失败：%v", w.fstab.fstabPath(), err)
	}
	progress(req, VolumeStepReload, nil)
	if reloadErr := w.daemonReload(ctx); reloadErr != nil {
		return backup, w.rollbackAfter(ctx, backup, targetVolumeID(req.Params),
			fmt.Sprintf("systemctl daemon-reload 失败：%v", reloadErr))
	}
	return backup, nil
}

// volumeEntries 算出写入受管块的完整条目集合。
//
// 起点是**当前块里的条目**，而不是我们以为的那几条：别的卷的条目必须原样保留，
// 否则“导入第二个卷”会把第一个卷的条目抹掉。
func (w *writer) volumeEntries(env VolumeEnv, want *FstabEntry, dropUUID string) (FstabState, []FstabEntry, error) {
	st, err := env.Fstab.Read()
	if err != nil {
		return st, nil, commandFailed("写之前读 %s 失败：%v", w.fstab.fstabPath(), err)
	}
	entries := make([]FstabEntry, 0, len(st.Block.Entries)+1)
	for _, e := range st.Block.Entries {
		if want != nil && strings.EqualFold(e.UUID, want.UUID) {
			continue // 覆盖同一个 UUID 的旧条目
		}
		if want == nil && dropUUID != "" && strings.EqualFold(e.UUID, dropUUID) {
			continue // 退出管理：只减这一行
		}
		entries = append(entries, e)
	}
	if want != nil {
		entries = append(entries, *want)
	}
	return st, entries, nil
}

// daemonReload 让 systemd 重新读 fstab。
//
// 不做 `mount -a`（PLAN.md 第 9.3 节明确禁止）：daemon-reload 只重建单元，
// 不改变任何文件的挂载状态。
func (w *writer) daemonReload(ctx context.Context) error {
	res, err := w.runFixed(ctx, w.systemctl, "daemon-reload")
	if err != nil {
		return fmt.Errorf("执行 %s daemon-reload 失败：%w", w.systemctl, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s daemon-reload 退出码 %d：%s", w.systemctl, res.ExitCode, commandDetail(res))
	}
	return nil
}

// rollbackAfter 用备份恢复 fstab，并返回一条说明“已恢复/未恢复”的错误。
//
// 它只在该步之前**已经**替换过文件时调用；备份为空表示文件本来就没变，不需要恢复。
func (w *writer) rollbackAfter(ctx context.Context, backup, volumeID, reason string) error {
	if backup == "" {
		return commandFailed("%s（%s 没有改动）", reason, DefaultFstabPath)
	}
	if err := w.fstab.Restore(ctx, backup); err != nil {
		return commandFailed("%s。恢复备份 %s 也失败了（%v）——请手工把 %s 换回这份备份",
			reason, backup, err, DefaultFstabPath)
	}
	// 恢复之后 systemd 还盯着新文件，再 reload 一次让它的视图回到旧内容。
	if err := w.daemonReload(ctx); err != nil {
		return commandFailed("%s。已用 %s 恢复 %s，但恢复后 daemon-reload 失败（%v）：systemd 的视图可能仍是旧的，重启前请核实",
			reason, backup, DefaultFstabPath, err)
	}
	return commandFailed("%s。已用备份 %s 恢复 %s 并 daemon-reload", reason, backup, DefaultFstabPath)
}

// verifyVolumeRecord 是 import/configure 的两源核验：读回 fstab + 读 systemd 的单元状态。
func (w *writer) verifyVolumeRecord(ctx context.Context, req WriteRequest, env VolumeEnv, rec VolumeRecord,
	backup string, extra map[string]any) (WriteReport, error) {
	progress(req, VolumeStepVerify, nil)
	want := rec.FstabEntry()
	st, err := w.fstab.Read()
	if err != nil {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("写完之后读不回 %s：%v", DefaultFstabPath, err))
	}
	got, ok := st.Block.FindEntryByUUID(rec.FilesystemUUID)
	if !ok {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("%s 的受管块里没有 UUID=%s 的条目", DefaultFstabPath, rec.FilesystemUUID))
	}
	if !got.SameAs(want) {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("受管块里的条目与期望不一致：%q，期望 %q", got.Line(), want.Line()))
	}
	// 来源二：systemd 已经把这个挂载点当成一个单元（daemon-reload 之后 LoadState=loaded）。
	// findmnt --verify 只能证明文件结构对，证明不了 systemd 认得它。
	unit := systemdUnitFor(rec.MountPath())
	state, err := w.unitLoadState(ctx, unit)
	if err != nil {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID, err.Error())
	}
	if state != "loaded" {
		return WriteReport{}, w.rollbackAfter(ctx, backup, rec.ID,
			fmt.Sprintf("systemd 里 %s 的 LoadState 是 %q，不是 loaded", unit, state))
	}
	observed := map[string]any{
		"volume_id":     rec.ID,
		"slug":          rec.Slug,
		"mount_path":    rec.MountPath(),
		"uuid":          rec.FilesystemUUID,
		"filesystem":    rec.FilesystemType,
		"fstab_entry":   got.Line(),
		"systemd_unit":  unit,
		"systemd_state": state,
		"automount":     rec.Config.Automount,
		"mounted":       false,
		"checked_via":   []string{"fstab_reread", "systemctl_show"},
	}
	for k, v := range extra {
		observed[k] = v
	}
	return WriteReport{
		Observed: observed,
		Data: map[string]any{
			"volume_id":    rec.ID,
			"slug":         rec.Slug,
			"mount_path":   rec.MountPath(),
			"filesystem":   rec.FilesystemType,
			"uuid":         rec.FilesystemUUID,
			"version":      rec.Version,
			"fstab_backup": backup,
			"systemd_unit": unit,
		},
	}, nil
}

// unitLoadState 查一个挂载单元在 systemd 里的 LoadState。
func (w *writer) unitLoadState(ctx context.Context, unit string) (string, error) {
	res, err := w.runFixed(ctx, w.systemctl, "show", "-p", "LoadState", "--value", unit)
	if err != nil {
		return "", fmt.Errorf("查询 systemd 单元 %s 失败：%w", unit, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("systemd 不认识单元 %s（退出码 %d：%s）", unit, res.ExitCode, commandDetail(res))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// systemdUnitFor 由挂载点算挂载单元名。
//
// 不用 `systemd-escape` 子进程：卷名只允许小写字母、数字与中划线（ValidateSlug），
// 根路径是常量，因此这里只需要把 `/` 换成 `-`。这个前提写在 volume.go 里，
// 改动字符集时这里会跟着失效——所以校验与换算放在同一个包里，由测试盯着。
func systemdUnitFor(mountPoint string) string {
	clean := strings.TrimPrefix(path.Clean(mountPoint), "/")
	return strings.ReplaceAll(clean, "/", "-") + ".mount"
}

// ensureMountPoint 建出挂载点目录（幂等）。
func (w *writer) ensureMountPoint(mountPoint string) error {
	// 建之前再查一遍路径：符号链接、非目录、父目录权限不对都拒绝。
	env := w.volumeEnv()
	if msgs := checkMountPointPath(env, mountPoint); len(msgs) > 0 {
		return notAllowed([]Blocker{{Code: NoticeMountPointUnsafe, Message: strings.Join(msgs, "；")}})
	}
	if err := w.mkdirAllPath(mountPoint, 0o755); err != nil {
		return commandFailed("创建挂载点 %s 失败：%v", mountPoint, err)
	}
	fi, err := env.lstat(mountPoint)
	if err != nil {
		return commandFailed("创建挂载点 %s 之后读不到它：%v", mountPoint, err)
	}
	if !fi.IsDir() {
		return commandFailed("挂载点 %s 不是目录（%s）", mountPoint, fi.Mode().Type())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		// 0755 是上限：普通用户不能往挂载点目录里写东西（未挂载时那些文件会出现在根分区上）。
		if err := w.chmodPath(mountPoint, 0o755); err != nil {
			return commandFailed("把挂载点 %s 的权限改成 0755 失败：%v", mountPoint, err)
		}
	}
	return nil
}

// mkdirAllPath 建目录（平台接缝，默认 os.MkdirAll）。
func (w *writer) mkdirAllPath(path string, perm os.FileMode) error {
	if w.mkdirAll != nil {
		return w.mkdirAll(path, perm)
	}
	return os.MkdirAll(path, perm)
}

// chmodPath 改权限（平台接缝，默认 os.Chmod）。
func (w *writer) chmodPath(path string, mode os.FileMode) error {
	if w.chmod != nil {
		return w.chmod(path, mode)
	}
	return os.Chmod(path, mode)
}

// mountFacts 是 findmnt 读回的挂载事实。
type mountFacts struct {
	mounted bool
	uuid    string
	source  string
	fstype  string
	options string
}

// errNotMounted 表示挂载点当前没有挂载（findmnt 的退出码 1）。
var errNotMounted = errors.New("storage: 挂载点没有挂载")

// readMountFacts 用 findmnt 读一个挂载点的事实。
//
// 判据是退出码：findmnt 找不到挂载点时返回 1，其它非零是真正的错误。把两者混在一起
// 会让“没挂”看起来像“读失败”。
func (w *writer) readMountFacts(ctx context.Context, mountPoint string) (mountFacts, error) {
	res, err := w.runFixed(ctx, w.findmnt,
		"--noheadings", "--output", "UUID,SOURCE,FSTYPE,OPTIONS", mountPoint)
	if err != nil {
		return mountFacts{}, fmt.Errorf("执行 findmnt %s 失败：%w", mountPoint, err)
	}
	if res.ExitCode == 1 {
		return mountFacts{mounted: false}, errNotMounted
	}
	if res.ExitCode != 0 {
		return mountFacts{}, fmt.Errorf("findmnt %s 退出码 %d：%s", mountPoint, res.ExitCode, commandDetail(res))
	}
	fields := strings.Fields(strings.TrimSpace(res.Stdout))
	if len(fields) < 3 {
		return mountFacts{}, fmt.Errorf("findmnt %s 的输出无法解析：%q", mountPoint, excerpt(res.Stdout))
	}
	facts := mountFacts{mounted: true, uuid: fields[0], source: fields[1], fstype: fields[2]}
	if len(fields) > 3 {
		facts.options = strings.Join(fields[3:], " ")
	}
	return facts, nil
}

// verifyMountedByScan 用重新扫描的拓扑确认挂载点真的挂上了（第二来源）。
func (w *writer) verifyMountedByScan(ctx context.Context, mountPoint string) error {
	topo, err := w.scanner.ScanNow(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("挂载后重新扫描拓扑失败：%w", err)
	}
	for i := range topo.Nodes {
		dev := &topo.Nodes[i]
		if dev.Filesystem == nil {
			continue
		}
		for _, m := range dev.Filesystem.Mounts {
			if m.Path == mountPoint {
				return nil
			}
		}
	}
	return fmt.Errorf("扫描结果里 %s 上没有挂载", mountPoint)
}

// verifyUnmountedByScan 用重新扫描的拓扑确认挂载点已经卸掉（第二来源）。
func (w *writer) verifyUnmountedByScan(ctx context.Context, mountPoint string) error {
	topo, err := w.scanner.ScanNow(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("卸载后重新扫描拓扑失败：%w", err)
	}
	for i := range topo.Nodes {
		dev := &topo.Nodes[i]
		if dev.Filesystem == nil {
			continue
		}
		for _, m := range dev.Filesystem.Mounts {
			if m.Path == mountPoint {
				return fmt.Errorf("设备 %s 仍挂在 %s", dev.ID, mountPoint)
			}
		}
	}
	return nil
}

// compensateUmount 在“挂上了但后续核验/登记失败”时把卷卸回去。
//
// 这是补偿，不是回滚：umount 之后盘上的数据没变，只是回到动作之前的状态。
// 补偿失败时必须说清楚现在挂着——不能让人以为什么都没发生。
func (w *writer) compensateUmount(ctx context.Context, mountPoint string, rec VolumeRecord, reason string) error {
	res, err := w.runFixed(ctx, w.umount, mountPoint)
	if err != nil {
		return commandFailed("%s。补偿卸载 %s 时命令执行失败（%v）——卷可能仍然挂着，请手工核实", reason, mountPoint, err)
	}
	if res.ExitCode != 0 {
		return commandFailed("%s。补偿卸载 %s 失败（退出码 %d：%s）——卷可能仍然挂着，请手工核实",
			reason, mountPoint, res.ExitCode, commandDetail(res))
	}
	if _, ferr := w.readMountFacts(ctx, mountPoint); ferr == nil {
		return commandFailed("%s。补偿卸载返回 0，但 %s 仍在挂载表里，请手工核实", reason, mountPoint)
	}
	return commandFailed("%s。已把 %s 卸回未挂载状态（登记记录未改动）", reason, mountPoint)
}

// saveTransition 保存一次状态变更（版本自增）。
func (w *writer) saveTransition(ctx context.Context, rec VolumeRecord, opID string) (VolumeRecord, error) {
	rec.Version = NextVersion(rec.Version)
	rec.LastOperationID = opID
	rec.UpdatedAt = time.Now().UTC()
	if err := w.store.SaveVolume(ctx, rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// runFixed 执行一条固定程序 + 固定参数的命令。
//
// 非零退出**不是**“执行出错”：syscmd 把“起来了但退出码非零”报成 *ExitError，
// 而这里的每个调用方都要按退出码分辨语义（findmnt 的 1 = 没挂载，umount 的
// 32 = target is busy）。所以先把它归一成正常的 Result 交回去，同时保住已经
// 填好的 Stdout/Stderr——退出码里的信息（"target is busy"）全在 stderr 里。
// 只有命令压根没跑起来（找不到程序、超时被杀）才返回 error。
func (w *writer) runFixed(ctx context.Context, program string, args ...string) (syscmd.Result, error) {
	res, err := w.runner.Run(ctx, syscmd.Spec{Program: program, Args: args},
		syscmd.Limits{Timeout: VolumeCommandTimeout})
	var exit *syscmd.ExitError
	if errors.As(err, &exit) {
		return res, nil
	}
	return res, err
}

// commandDetail 从命令结果里取一段可读的失败原因。
func commandDetail(res syscmd.Result) string {
	if res.TimedOut {
		return "命令超时被终止"
	}
	out := excerpt(strings.TrimSpace(res.Stderr))
	if out == "" {
		out = excerpt(strings.TrimSpace(res.Stdout))
	}
	if out == "" {
		return "没有输出"
	}
	return out
}

// mustTopology 取一份拓扑用于“顺便列出占用者”，失败时返回空拓扑。
//
// 只用在错误消息的修饰上：拿不到就不加这句，而不是让原本的错误被覆盖。
func mustTopology(ctx context.Context, w *writer) Topology {
	topo, err := w.scanner.ScanNow(context.WithoutCancel(ctx))
	if err != nil {
		return Topology{}
	}
	return topo
}
