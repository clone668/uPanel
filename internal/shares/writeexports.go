package shares

// NFS 共享的执行路径（P14.4）。
//
// 与 SMB 那一条共用前半段（预检、guard、目录准备），分岔点很明确：NFS 没有账号层，
// 也没有 POSIX ACL 要替换——它的访问控制只有两件东西，网段与 root_squash。
// 所以这里不做 setfacl，取而代之的是一次显式的目录权限设定：那是“客户端到底能不能
// 往里写”的唯一剩余判据。
//
// 为什么目录权限要面板自己定：NFS 走 sec=sys，客户端 uid 原样透传到服务端。面板在
// 应用时并不知道将来是哪个 uid 连上来，也没有一个 ACL 可以把“这些人可写”写清楚
// （那正是 SMB 那一半的做法）。于是只剩两个诚实的选择：读写共享给 0777（把“谁能连”
// 完全交给网段），或者拒绝提供读写共享。选前者，并且把这个取舍写进 DECISIONS。

import (
	"context"
	"os"
)

// NFS 共享目录的权限。
const (
	// nfsDirModeReadOnly 是只读共享的目录权限：属主可写，其他人只能读与进入。
	//
	// 客户端即便能连上、也写不进这个目录——这是只读共享的第二道保证（第一道是
	// 导出选项里的 ro）。两道都在的时候，一个写不进去的只读共享不会让人去猜
	// “是不是 ro 没生效”。
	nfsDirModeReadOnly os.FileMode = 0o755
	// nfsDirModeReadWrite 是读写共享的目录权限：所有人可写。
	//
	// 这不是疏忽。root_squash 之后客户端 root 变成 nobody，而普通用户以**自己的**
	// uid 落盘——服务端事先不知道那个 uid 是谁。要让网段内的人真的能写，目录就必须
	// 对所有人可写；访问控制留给导出里的网段。"谁能连" 与 "连上之后能写什么" 在 NFS
	// 里是两层，面板只能把第一层管死。
	nfsDirModeReadWrite os.FileMode = 0o777
)

// nfsDirMode 返回这条共享应有的目录权限。
func nfsDirMode(readOnly bool) os.FileMode {
	if readOnly {
		return nfsDirModeReadOnly
	}
	return nfsDirModeReadWrite
}

// executeShareApplyNFS 执行一份 NFS 共享的 share.apply。
//
// 顺序：目录 → 目录权限 → 取 id → 写导出并读回核对 → 开机依赖 → 落库。
// 与 SMB 那边一样，目录在前、配置在后：配置写不成功时目录已经准备好，重试从同一步继续。
func (e *Engine) executeShareApplyNFS(ctx context.Context, p ShareApplyParams, dctx definitionContext, req WriteRequest) (WriteReport, error) {
	def := *p.Definition

	// 1. 目录。
	repaired, err := e.ensureShareDir(dctx.MountPath, dctx.Dir)
	if err != nil {
		return WriteReport{}, err
	}
	if req.Progress != nil {
		req.Progress("share_directory", map[string]any{"directory": dctx.Dir})
	}

	// 2. 目录权限，并读回。写下去不等于写对了（umask、挂载选项、文件系统能力
	//    都可能让它落成别的样子），所以这里不看返回值就下结论是不行的。
	mode := nfsDirMode(def.ReadOnly)
	if err := e.opts.Chmod(dctx.Dir, mode); err != nil {
		return WriteReport{}, commandFailed("设置共享目录 %s 的权限为 %#o 失败：%v", dctx.Dir, mode, err)
	}
	fi, err := e.opts.Lstat(dctx.Dir)
	if err != nil {
		return WriteReport{}, commandFailed("读回共享目录 %s 的权限失败：%v", dctx.Dir, err)
	}
	if fi.Mode().Perm() != mode {
		return WriteReport{}, commandFailed(
			"共享目录 %s 的权限读回是 %#o，要的是 %#o：客户端会因此写不进去（或写进去不该写的）",
			dctx.Dir, fi.Mode().Perm(), mode)
	}
	if req.Progress != nil {
		req.Progress("share_directory_mode", map[string]any{"directory": dctx.Dir, "mode": mode.String()})
	}

	// 3. 记录 id。新建时**必须先取**：导出行的行尾标记写的就是它，晚一步取就等于
	//    往文件里写一行指向不存在记录的标记。
	now := e.opts.Now().UTC()
	var rec ShareRecord
	if p.ShareID == nil {
		id, err := NewShareID()
		if err != nil {
			return WriteReport{}, err
		}
		rec = ShareRecord{ID: id, CreatedAt: now, Version: "1"}
	} else {
		existing, found, err := e.opts.Store.GetShare(ctx, *p.ShareID)
		if err != nil {
			return WriteReport{}, err
		}
		if !found {
			return WriteReport{}, notFound("共享 %s 在执行时已不存在", *p.ShareID)
		}
		rec = existing
		rec.Version = nextVersion(existing.Version)
	}
	shareID := ""
	if p.ShareID != nil {
		shareID = *p.ShareID
	}

	// 4. 渲染全集并写导出。
	wantExports, offline, err := e.desiredExports(ctx, def, shareID, rec.ID)
	if err != nil {
		return WriteReport{}, err
	}
	line, err := RenderExportLine(ManagedExport{
		ShareID:   rec.ID,
		Name:      def.Name,
		Dir:       dctx.Dir,
		MountPath: dctx.MountPath,
		CIDRs:     def.AllowedCIDRs,
		ReadOnly:  def.ReadOnly,
	})
	if err != nil {
		return WriteReport{}, validationError("%s", err.Error())
	}
	hash := ConfigHashOf(line + "\n")
	cfg, err := e.applyExports(ctx, wantExports, offline, req.Progress)
	if err != nil {
		return WriteReport{}, err
	}
	// 能力探针的缓存里还存着“托管导出文件不存在”“导出表里没有这条路径”，必须立刻
	// 作废，否则紧随其后的读取会看到一份已经过时的结论（与 SMB 那一步同理）。
	e.opts.Prober.Invalidate()

	// 5. 落库。
	rec.Definition = def
	rec.ConfigHash = hash
	// DirectoryPrepared：目录存在、权限按这一版定义设定并读回核对过。
	rec.DirectoryPrepared = true
	// ConfigApplied：托管导出文件里现在有这一条。禁用的共享不在文件里，所以是 false。
	rec.ConfigApplied = def.Enabled
	rec.RuntimeRevoked = nil
	rec.LastOperationID = req.OperationID
	rec.UpdatedAt = now
	if err := e.opts.Store.SaveShare(ctx, rec); err != nil {
		// 落库失败时导出已经写进文件、exportfs 也已经吃下去了。不退回就是把人留在
		// 一个比“什么都没做”更糟的状态：**系统上真的在导出一个面板数据库里不存在的
		// 共享**，而页面说这次没成功。22.04 验收就是这么抓到它的——协议列被 0005 的
		// CHECK 挡住，SaveShare 失败，托管导出文件里却留下了那一行。
		//
		// 退回的只有我们自己写的东西（这一个文件 + 重刷导出表）；卷里的目录与文件
		// 一个都不动——目录已经建好了，重试从同一步继续。
		rollback := e.restoreExportsAfterFailure(ctx, cfg)
		e.opts.Prober.Invalidate()
		return WriteReport{}, withRollbackNote(
			commandFailed("导出已经写进 %s 并生效，但保存共享记录失败：%v；这条共享没有登记成功，客户端此刻看到的导出不会被面板管理",
				cfg.Report.Path, err), rollback)
	}

	// served 还要再确认一次“这条共享自己的卷此刻确实挂着”。预检到执行之间隔着一个
	// 任务队列，不重新确认就报 verified，等于用几秒前的观测担保现在的事实。
	served := cfg.Report.Served
	if gate, found, err := e.mountGateFor(ctx, def.VolumeID); err != nil {
		return WriteReport{}, err
	} else if !found || !gate.Mounted {
		served = false
	}
	outcome := OutcomeAccepted
	if served {
		outcome = OutcomeVerified
	}

	observed := baseExportsObserved(cfg)
	for k, v := range map[string]any{
		"share_id":           rec.ID,
		"share_name":         def.Name,
		"protocol":           ProtocolNFS,
		"version":            rec.Version,
		"volume_id":          def.VolumeID,
		"mount_path":         dctx.MountPath,
		"directory":          dctx.Dir,
		"directory_created":  true,
		"directory_prepared": true,
		"directory_mode":     fi.Mode().Perm().String(),
		"config_hash":        hash,
	} {
		observed[k] = v
	}
	if len(repaired) > 0 {
		observed["path_traversal_repaired"] = repaired
	}
	return WriteReport{
		Outcome:  outcome,
		Observed: observed,
		Data: map[string]any{
			"share_id":    rec.ID,
			"version":     rec.Version,
			"config_hash": hash,
			// 这一版的导出内容：界面上可以直接展示“写完长什么样”。
			"export_preview":  line,
			"managed_exports": cfg.Report.Path,
			"exports":         cfg.Report.Exports,
			"served":          served,
			"reload":          cfg.Report.Reload,
			"service":         cfg.Report.Service,
			"startup_deps":    cfg.Report.StartupDeps,
			"startup_paths":   cfg.Report.StartupPaths,
			"volume_offline":  cfg.Report.VolumeOffline,
			"directory_mode":  fi.Mode().Perm().String(),
			"allowed_cidrs":   def.AllowedCIDRs,
			"root_squash":     def.NFSOptions != nil && def.NFSOptions.RootSquash,
			"protocol":        ProtocolNFS,
		},
		RecoveryHint: recoveryHintExports(cfg.Report, def),
	}, nil
}

// executeShareRefNFS 执行 NFS 共享的 share.disable / share.delete。
//
// 顺序与 SMB 那边同一条：**先停止暴露，再谈别的**。NFS 这边没有 ACL 要收，
// 所以只有一件事要做——把这一行从托管导出文件里摘掉，并让 exportfs 重新读一遍。
func (e *Engine) executeShareRefNFS(ctx context.Context, action string, rec ShareRecord, req WriteRequest) (WriteReport, error) {
	// 1. 从导出表里摘掉。
	remaining, offline, err := e.exportsExcept(ctx, rec.ID)
	if err != nil {
		return WriteReport{}, err
	}
	cfg, err := e.applyExports(ctx, remaining, offline, req.Progress)
	if err != nil {
		return WriteReport{}, err
	}
	e.opts.Prober.Invalidate()

	now := e.opts.Now().UTC()
	var storeErr error
	if action == ActionShareDisable {
		rec.Definition.Enabled = false
		rec.DirectoryPrepared = false
		rec.ConfigApplied = false
		rec.Version = nextVersion(rec.Version)
		rec.LastOperationID = req.OperationID
		rec.UpdatedAt = now
		storeErr = e.opts.Store.SaveShare(ctx, rec)
	} else {
		storeErr = e.opts.Store.DeleteShare(ctx, rec.ID)
	}
	if storeErr != nil {
		// 与 apply 同一个理由，方向相反：导出行已经摘掉、表也刷过了，但记录还是启用的
		// （删不掉时记录还在）。不把这一行放回去，库里就有一条“启用中、却没在导出”的
		// 共享——下一次对账或重新应用会把它当成新东西再写一遍，而人看到的是一次失败。
		rollback := e.restoreExportsAfterFailure(ctx, cfg)
		e.opts.Prober.Invalidate()
		return WriteReport{}, withRollbackNote(
			commandFailed("导出行已经从 %s 摘掉并重新生效，但更新共享记录失败：%v；记录仍是启用状态，导出这一行已经放回去了",
				cfg.Report.Path, storeErr), rollback)
	}

	observed := baseExportsObserved(cfg)
	for k, v := range map[string]any{
		"share_id":         rec.ID,
		"share_name":       rec.Definition.Name,
		"protocol":         ProtocolNFS,
		"directory_kept":   true,
		"record_retrained": action == ActionShareDisable,
		// NFS 没有 ACL 可收：目录权限是共享时按 read_only 设的，停用共享不会改动它。
		// 这一项显式写 false 而不是留空，是为了让读结果的人知道“不是忘了做”。
		"acl_revoked": false,
		"acl_note": "NFS 共享没有 POSIX ACL：访问控制由导出里的网段与 root_squash 决定，" +
			"摘掉导出行之后网段内也连不上了；目录权限保持原样",
		"served": false,
	} {
		observed[k] = v
	}
	if action == ActionShareDisable {
		observed["version"] = rec.Version
	}
	return WriteReport{
		Outcome:  OutcomeAccepted,
		Observed: observed,
		Data: map[string]any{
			"share_id":        rec.ID,
			"protocol":        ProtocolNFS,
			"directory":       "",
			"managed_exports": cfg.Report.Path,
			"exports":         cfg.Report.Exports,
			"service":         cfg.Report.Service,
			"reload":          cfg.Report.Reload,
			"served":          false,
		},
		RecoveryHint: "导出行已从 " + cfg.Report.Path + " 移除并重新生效；目录与里面的文件没有被删除。" +
			"客户端可能还挂着旧句柄，卸载重挂之后就连不上了",
	}, nil
}

// baseExportsObserved 把导出写入报告摊平成 observed_state 里的键。
//
// 键名与 SMB 那边刻意**不同名**：`samba_config` 描述的是 smb.conf，而这里动的是
// exports 文件。同名会让“这条任务到底改了哪个文件”变成一个要去猜的问题。
func baseExportsObserved(cfg exportsApplyResult) map[string]any {
	r := cfg.Report
	out := map[string]any{
		"managed_exports": r.Path,
		"exports_hash":    r.Hash,
		"exports_file":    r.File,
		"reload":          r.Reload,
		"service":         r.Service,
		"served":          r.Served,
		"startup_deps":    r.StartupDeps,
	}
	if len(r.StartupPaths) > 0 {
		out["startup_paths"] = r.StartupPaths
	}
	if len(r.VolumeOffline) > 0 {
		out["volume_offline_shares"] = r.VolumeOffline
	}
	if len(r.Backups) > 0 {
		out["exports_backups"] = r.Backups
	}
	if len(r.Exports) > 0 {
		out["exported_paths"] = r.Exports
	}
	if len(r.Notes) > 0 {
		notes := make([]map[string]any, 0, len(r.Notes))
		for _, n := range r.Notes {
			notes = append(notes, map[string]any{"code": n.Code, "message": n.Message})
		}
		out["notes"] = notes
	}
	return out
}

// recoveryHintExports 是 NFS 应用之后的恢复提示（与 SMB 的 recoveryHint 对称）。
func recoveryHintExports(r ExportsReport, def ShareDefinition) string {
	if r.Served {
		return "导出已生效：客户端可以先 `showmount -e <本机地址>` 看到 " + def.Name +
			"；客户端 root 会被 squash 成 nobody（这是刻意的）"
	}
	if r.Service != ServiceStateActive {
		return "导出已经写进 " + r.Path + "，但 nfs-server 没有在运行：先把服务起来，导出不需要重写"
	}
	return "导出已写进 " + r.Path + "，但读回核对没能确认它此刻在提供服务；请先看任务日志里的现场读数"
}
