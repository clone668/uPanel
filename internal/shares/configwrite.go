package shares

// 托管配置的写入路径（P08.2）：写文件、testparm 校验、原子替换、重载、读回。
//
// 顺序是固定的，每一步都在为一件事兜底：
//
//	渲染 → 写候选（fsync）→ testparm 校验候选 → 备份现行文件 → rename → fsync 目录
//	     → 接 include（同样：候选 → testparm → 备份 → rename → fsync）
//	     → testparm 校验合并后的主配置 → 重载 → 再读一次服务状态 → 逐段读回
//
// 三条纪律：
//
//  1. **先校验，后替换。** 候选文件先写出来、先过 testparm，通过了才 rename 到正式
//     位置。失败时正式文件一个字节都没动过。
//  2. **备份先于 rename。** rename 之后再备份，备份的就是新内容，那种备份只在自欺。
//  3. **reload 的退出码不是证据。** 真机验证过：smbd 停着的时候 `smbcontrol all
//     reload-config` 照样返回 0。所以“重载成功”= 请求返回 0 **且**之后 smbd 仍在运行；
//     smbd 没运行就是“没做”，结果只能是 accepted，不能是 verified。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// ConfigPaths 是配置写入用到的五个路径。
//
// 生产接线不传它（用默认值）。它存在只为单元测试：真机上 /etc/samba 是 root 的，
// 而单元测试要能在一小时后的某次改动里重复验证“候选不合格时正式文件不动”。
type ConfigPaths struct {
	Main       string
	Managed    string
	IncludeDir string
	BackupDir  string
	// StartupDep 是 smbd 的开机依赖 drop-in（P08.3）。零值时用 StartupDropinPath。
	//
	// 它在这里，理由与上面四个完全相同：/etc/systemd 也是 root 的，而
	// “内容没变就不重载”“路径非法就不写”这些判断必须在单元测试里能反复跑。
	StartupDep string
	// Exports / ExportsDir / ExportsMain 是 NFS 导出用的三个路径（P14.4）。
	//
	// Exports 是面板独占的那份文件，ExportsDir 是它所在的目录（应用时创建），
	// ExportsMain 是系统主导出文件——**只读**，用来发现“这条路径已经被别人导出了”。
	Exports    string
	ExportsDir string
	// ExportsMain 是系统主导出文件——**只读**，用来发现“这条路径已经被别人导出了”。
	ExportsMain string
	// ExportsStartupDep 是 nfs-server 的开机依赖 drop-in（P14.4）。零值时用
	// ExportsStartupDropinPath。
	ExportsStartupDep string
}

// DefaultConfigPaths 返回生产路径。
func DefaultConfigPaths() ConfigPaths {
	return ConfigPaths{
		Main:        DefaultConfigPath,
		Managed:     ManagedConfigPath,
		IncludeDir:  DefaultIncludeDir,
		BackupDir:   DefaultConfigBackupDir,
		StartupDep:  StartupDropinPath,
		Exports:     DefaultExportsFile,
		ExportsDir:  DefaultExportsDir,
		ExportsMain: DefaultExportsMain,
		// ExportsStartupDep 只给 nfs-server 用；SMB 那一半不看它。
		ExportsStartupDep: ExportsStartupDropinPath,
	}
}

// ConfigFS 是配置文件读写的接缝。
//
// 每一个方法都对应这条流水线上的一步，因此顺序本身可以被断言：
// WriteFileSync 不带 rename（候选必须能被 testparm 先看一眼），rename 单列一步，
// 目录 fsync 单列一步（掉电后 rename 是否持久，取决于它）。
type ConfigFS struct {
	ReadFile      func(name string) ([]byte, error)
	WriteFileSync func(name string, data []byte, perm os.FileMode) error
	Rename        func(oldpath, newpath string) error
	FsyncDir      func(dir string) error
	MkdirAll      func(dir string, perm os.FileMode) error
	Remove        func(name string) error
	Stat          func(name string) (os.FileInfo, error)
	ReadDir       func(name string) ([]os.DirEntry, error)
}

// SystemConfigFS 是真实的文件系统实现。
func SystemConfigFS() ConfigFS {
	return ConfigFS{
		ReadFile:      readConfigFile,
		WriteFileSync: writeFileSync,
		Rename:        os.Rename,
		FsyncDir:      fsyncDir,
		MkdirAll:      func(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) },
		Remove:        os.Remove,
		Stat:          os.Stat,
		ReadDir:       os.ReadDir,
	}
}

// writeFileSync 写文件、把权限钉成 perm、然后 fsync。
//
// 为什么写完还要 Chmod：OpenFile 的 perm 只在**创建**时生效，而且会被进程 umask 掩掉。
// 生产的代理单元照例带 UMask=0077（秘密与备份都该只有 root 能读），于是“0644 的托管配置”
// 在真机上落成 0600，而且这个差别取决于**代理是怎么被启动的**，代码里看不出来。
// 文件权限是产品决定的事，不能由启动它的 shell 决定——所以这里显式钉一遍。
// 已经存在的旧文件（上一版留下的 0600）也会在这一步被纠正。
func writeFileSync(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fsyncDir 把目录项的变化落盘。
//
// rename 成功但目录没 fsync 时，掉电后可能回到旧文件——而“配置回退了”在共享这个
// 场景里意味着共享突然消失，客户端会毫无征兆地断开。
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// fs 返回配置写入用的文件系统（测试注入优先）。
func (e *Engine) fs() ConfigFS {
	if e.opts.ConfigFS.ReadFile != nil {
		return e.opts.ConfigFS
	}
	return SystemConfigFS()
}

// paths 返回配置写入用的路径（测试注入优先）。
func (e *Engine) paths() ConfigPaths {
	p := e.opts.ConfigPaths
	if p.Main == "" {
		p.Main = DefaultConfigPath
	}
	if p.Managed == "" {
		p.Managed = ManagedConfigPath
	}
	if p.IncludeDir == "" {
		p.IncludeDir = DefaultIncludeDir
	}
	if p.BackupDir == "" {
		p.BackupDir = DefaultConfigBackupDir
	}
	if p.StartupDep == "" {
		p.StartupDep = StartupDropinPath
	}
	if p.Exports == "" {
		p.Exports = DefaultExportsFile
	}
	if p.ExportsDir == "" {
		p.ExportsDir = DefaultExportsDir
	}
	if p.ExportsMain == "" {
		p.ExportsMain = DefaultExportsMain
	}
	if p.ExportsStartupDep == "" {
		p.ExportsStartupDep = ExportsStartupDropinPath
	}
	return p
}

// configState 是动手写之前的一次现场快照（只读）。
type configState struct {
	Main    []byte
	Conf    SmbConf
	Managed []byte
	// ManagedPresent 报告托管文件此刻是否存在。
	ManagedPresent bool
	// Sections 是**现有**配置里的段名（小写）与来源文件，不含托管文件。
	Sections map[string]string
	// Unresolved 是展开不了的 include。
	Unresolved []string
	// Service 是 smbd 的运行状态。
	Service string
	// ManagedShares 是托管文件里现有的段名（不含 global）。
	ManagedShares []string
}

// readConfigState 读一次现场。只读：不建目录、不写文件。
func (e *Engine) readConfigState(ctx context.Context) (configState, error) {
	p := e.paths()
	fs := e.fs()

	mainData, err := fs.ReadFile(p.Main)
	if err != nil {
		return configState{}, dependencyBlocked(
			"读不到 Samba 主配置 %s：%v（读不到就不能说这台机器的 Samba 长什么样）", p.Main, err)
	}
	sections, unresolved := ConfiguredSections(mainData, fs.ReadFile)

	st := configState{
		Main:       mainData,
		Conf:       ParseSmbConf(mainData),
		Sections:   sections,
		Unresolved: unresolved,
		Service:    e.serviceState(ctx),
	}
	if data, err := fs.ReadFile(p.Managed); err == nil {
		st.Managed = data
		st.ManagedPresent = true
		st.ManagedShares = SectionNames(data)
	} else if !os.IsNotExist(err) {
		return configState{}, commandFailed("读不到托管配置 %s：%v", p.Managed, err)
	}
	return st, nil
}

// serviceState 读 smbd 的运行状态（active / inactive / unknown）。
//
// 实现放在 service.go：能力探针与这里必须是同一个判断，否则会出现“能力页说在跑、
// 执行结果说没跑”这种自相矛盾的画面。
func (e *Engine) serviceState(ctx context.Context) string {
	return probeServiceState(ctx, e.opts.Runner)
}

// renderManagedShares 把登记表里的共享渲染成托管配置需要的形态。
//
// records 是**这次要写进去的全集**（调用方已经把正在变化的这条替换成新的定义）。
// 渲染不需要卷此刻挂着：挂载点用 `storage.MountPathOf(slug)` 这个规范路径，
// 与“卷现在有没有挂”是两件事。“卷没挂的时候不该提供服务”由 P08.3 的判决层管，
// 而不是靠这里渲染不出来。
//
// 卷没挂的时候**不渲染**（P08.3）：挂载点在那时候只是系统盘上的一个空目录，把段留在
// 配置里等于共享那个空目录。这条判断放在渲染层而不是判决层，是因为托管文件是整份重写的：
// “应用 B”会顺带把 A 的段重写一遍，如果 A 的卷此刻没挂，重新写就是一次回归。
// 跳过的共享名会一起返回，报告里要点名（静默消失的共享比不能用的共享更难查）。
//
// 卷或账号已经不在登记表里时**不出声地跳过**是绝对不行的：那等于把一份共享从配置里
// 悄悄摘掉，客户端会发现共享消失而面板上什么都没说。这里直接报错并点名。
func (e *Engine) renderManagedShares(ctx context.Context, records []ShareRecord) ([]ManagedShare, []string, error) {
	accountRecs, err := e.opts.Store.ListAccounts(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[string]string, len(accountRecs))
	for _, a := range accountRecs {
		names[a.ID] = a.Name
	}

	out := make([]ManagedShare, 0, len(records))
	var skipped []string
	for _, rec := range records {
		if !rec.Definition.Enabled {
			continue
		}
		vol, found, err := e.volumeFor(ctx, rec.Definition.VolumeID)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, dependencyBlocked(
				"共享 %s 引用的卷 %s 已不在受管理卷里：无法计算它的目录，托管配置拒绝只写一半（%s）",
				rec.Definition.Name, rec.Definition.VolumeID, NoticeVolumeNotFound)
		}
		mountPath := vol.MountPath
		if mountPath == "" {
			mountPath = storage.MountPathOf(vol.Slug)
		}
		// 闸门：只有真的观测到“挂着”才渲染。不知道挂没挂同样不渲染（见 mountgate.go）。
		gate, _, err := e.mountGateFor(ctx, rec.Definition.VolumeID)
		if err != nil {
			return nil, nil, err
		}
		if !gate.Mounted {
			skipped = append(skipped, rec.Definition.Name)
			continue
		}
		dir, err := ShareDir(mountPath, rec.Definition.RelativePath)
		if err != nil {
			return nil, nil, validationError("%s", err.Error())
		}
		block, err := rec.Definition.RenderBlock(mountPath, names)
		if err != nil {
			return nil, nil, validationError("渲染共享 %s 的配置失败：%v", rec.Definition.Name, err)
		}
		users := make([]string, 0, len(rec.Definition.SMBUsers))
		for _, u := range rec.Definition.SMBUsers {
			if name := names[u.AccountID]; name != "" {
				users = append(users, name)
			}
		}
		sort.Strings(users)
		out = append(out, ManagedShare{
			Name:       rec.Definition.Name,
			Dir:        dir,
			MountPath:  mountPath,
			ValidUsers: users,
			Block:      block,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	sort.Strings(skipped)
	return out, skipped, nil
}

// configApplyResult 是写配置这一段的内部结果（与对外报告分开，便于回滚时保留现场）。
type configApplyResult struct {
	Report ManagedConfigReport
	// MainBackup / ManagedBackup 是这次写之前那两个文件的备份路径。
	MainBackup    string
	ManagedBackup string
	// MainChanged 报告 smb.conf 被改过（回滚要还原它）。
	MainChanged bool
}

// applyManagedConfig 把 desired 写成托管配置并接入 include、校验、重载、读回。
//
// 调用方必须持有 `samba:config` 锁：这条流水线读的是全量配置、写的是全量文件，
// 两个并发应用会让后一个把前一个的段覆盖掉（它读到的“现状”里没有前一个）。
func (e *Engine) applyManagedConfig(ctx context.Context, desired []ManagedShare, offline []string, report func(step string, fields map[string]any)) (configApplyResult, error) {
	p := e.paths()
	fs := e.fs()

	st, err := e.readConfigState(ctx)
	if err != nil {
		return configApplyResult{}, err
	}

	desiredText := RenderManagedConfig(desired)
	res := configApplyResult{Report: ManagedConfigReport{
		Path:     p.Managed,
		Hash:     ConfigHashOf(desiredText),
		File:     ConfigFileUnchanged,
		Include:  IncludePresent,
		Reload:   ReloadSkippedInactive,
		Service:  st.Service,
		Sections: sectionNamesOf(desired),
		Testparm: "not-run",
		// 这一轮被跳过（卷离线）的共享必须点名：它们是“从配置里消失”的那些，
		// 静默消失比不能用更难查。
		VolumeOffline: append([]string(nil), offline...),
	}}
	res.Report.Overrides = e.globalOverrides(ctx)
	if len(offline) > 0 {
		res.Report.Notes = append(res.Report.Notes, Blocker{
			Code: NoticeVolumeOffline,
			Message: "这些共享这次没有写进配置，因为它们绑定的卷此刻没挂载：" +
				strings.Join(offline, "、") + "；卷回来之后要重新应用一次",
		})
	}

	// ---- 1. 托管目录 ----
	if _, err := fs.Stat(p.IncludeDir); err != nil {
		if !os.IsNotExist(err) {
			return res, dependencyBlocked("读不到托管目录 %s：%v（%s）", p.IncludeDir, err, NoticeConfigNotWritable)
		}
		if err := fs.MkdirAll(p.IncludeDir, managedDirMode); err != nil {
			return res, dependencyBlocked("创建托管目录 %s 失败：%v（%s）", p.IncludeDir, err, NoticeConfigNotWritable)
		}
		res.Report.Notes = append(res.Report.Notes, Blocker{
			Code:    NoticeIncludeDirMissing,
			Message: "已创建托管 include 目录 " + p.IncludeDir,
		})
		if report != nil {
			report("share_config_dir", map[string]any{"dir": p.IncludeDir})
		}
	}

	// ---- 2. 托管文件：候选 → testparm → 备份 → rename → fsync ----
	//
	// 缺 testparm 时有一条例外，而且只有一条：**纯摘除**。
	// 把已有的段删掉不可能引入新的语法错误，而“停用一份共享”是安全方向的操作，
	// 不该因为机器上少了一个校验工具就做不到（那会让人去手工改 smb.conf，
	// 而手工改才是真正没人管的那条路）。新增或改动仍然必须有 testparm。
	testparmTool := e.tools(ctx).Testparm
	removalOnly := testparmTool == "" && st.ManagedPresent && onlyRemoval(desired, string(st.Managed))
	contentChanged := !st.ManagedPresent || string(st.Managed) != desiredText
	if contentChanged {
		if testparmTool == "" && !removalOnly {
			return res, dependencyBlocked(
				"缺少 testparm：不允许把新内容写进 Samba 配置（%s）", NoticeTestparmMissing)
		}
		candidate := p.Managed + ".upanel-new"
		_ = fs.Remove(candidate) // 上一次失败可能留下临时文件；权限只在创建时生效，所以先删
		if err := fs.WriteFileSync(candidate, []byte(desiredText), managedFileMode); err != nil {
			return res, dependencyBlocked("写候选托管配置 %s 失败：%v（%s）",
				candidate, err, NoticeConfigNotWritable)
		}
		defer func() { _ = fs.Remove(candidate) }()

		if removalOnly {
			res.Report.Testparm = "skipped_tool_missing"
			res.Report.Notes = append(res.Report.Notes, Blocker{
				Code:    NoticeTestparmMissing,
				Message: "没有 testparm：这次只做摘除（不引入新内容），因此允许写入，但无法用 Samba 自己的校验确认结果",
			})
		} else if err := e.validateConfig(ctx, candidate); err != nil {
			return res, err
		} else {
			res.Report.Testparm = "candidate-ok"
		}

		if st.ManagedPresent {
			backup, err := e.backupFile(p, p.Managed, st.Managed)
			if err != nil {
				return res, err
			}
			res.ManagedBackup = backup
			res.Report.Backups = append(res.Report.Backups, backup)
		}
		if err := fs.Rename(candidate, p.Managed); err != nil {
			return res, commandFailed("替换托管配置 %s 失败：%v（%s）", p.Managed, err, FailConfigWrite)
		}
		if err := fs.FsyncDir(filepath.Dir(p.Managed)); err != nil {
			return res, commandFailed("%s 已替换，但目录 fsync 失败（掉电后可能回退）：%v（%s）",
				p.Managed, err, FailConfigWrite)
		}
		res.Report.File = ConfigFileWritten
		if report != nil {
			report("share_config_file", map[string]any{
				"path": p.Managed, "hash": res.Report.Hash, "sections": res.Report.Sections,
			})
		}
	}

	// ---- 3. 接入 include（最小改动：注释 + 一行 include） ----
	includeChanged := false
	if !st.Conf.IncludePresent {
		candidateMain, changed := PlanInclude(st.Main, st.Conf)
		if changed {
			candidatePath := p.Main + ".upanel-new"
			_ = fs.Remove(candidatePath)
			if err := fs.WriteFileSync(candidatePath, candidateMain, managedFileMode); err != nil {
				return res, dependencyBlocked("写候选主配置 %s 失败：%v（%s）",
					candidatePath, err, NoticeConfigNotWritable)
			}
			defer func() { _ = fs.Remove(candidatePath) }()

			// 这一条 testparm 看的是**合并后**的配置：候选主配置里那行 include 指向的
			// 就是刚才换上去的托管文件，所以这是“接上去之后 Samba 会读到什么”的真实
			// 预览，而不是拿一个孤立的片段猜。
			if err := e.validateConfig(ctx, candidatePath); err != nil {
				return res, err
			}
			res.Report.Testparm = "candidate-merged-ok"

			backup, err := e.backupFile(p, p.Main, st.Main)
			if err != nil {
				return res, err
			}
			res.MainBackup = backup
			res.Report.Backups = append(res.Report.Backups, backup)

			if err := fs.Rename(candidatePath, p.Main); err != nil {
				return res, commandFailed("把 include 接入 %s 失败：%v（%s）", p.Main, err, FailConfigWrite)
			}
			if err := fs.FsyncDir(filepath.Dir(p.Main)); err != nil {
				return res, commandFailed("%s 已替换，但目录 fsync 失败（掉电后可能回退）：%v（%s）",
					p.Main, err, FailConfigWrite)
			}
			includeChanged = true
			res.MainChanged = true
			res.Report.Include = IncludeInstalled
			if report != nil {
				report("share_config_include", map[string]any{"path": p.Main, "include": p.Managed})
			}
		}
	}

	// ---- 4. 合并后的正式配置校验 ----
	//
	// 这一段**无条件**跑，即使这次什么都没改。两件事靠它：
	//   - 单独看合格的片段拼起来不合格（段重名、include 顺序、全局项冲突）只有整体
	//     校验才能发现；
	//   - 幂等重放时也要能说“此刻 Samba 读到的这份配置是合格的”，否则“内容没变所以
	//     什么都不校验”会把一个早就坏了的配置一直当成没问题。
	if testparmTool == "" {
		// 没有校验工具：上面已经说明过原因，这里不能再“假装校验通过”，
		// 也不能因此否掉一次只做摘除的操作。Testparm 保持 candidate/skipped 状态，
		// Served 因此必然是 false——"没能校验"本来就不该得出“在提供服务”。
		if res.Report.Testparm == "not-run" {
			res.Report.Testparm = "skipped_tool_missing"
		}
	} else {
		if err := e.validateConfig(ctx, p.Main); err != nil {
			if !contentChanged && !includeChanged {
				return res, commandFailed("%v；这次没有写任何文件，故障是既有的", err)
			}
			// 走到这里说明“片段合格、拼起来不合格”。回滚到写之前的状态，
			// 而不是让机器停在一份不能用的配置上。
			restoreErr := e.rollbackConfig(ctx, res, st)
			if restoreErr != nil {
				return res, commandFailed(
					"合并后的 Samba 配置校验失败，且回滚失败（需要人工介入）：%v；回滚：%v", err, restoreErr)
			}
			return res, commandFailed("%v；已回滚到写之前的样子", err)
		}
		res.Report.Testparm = "merged-ok"
	}

	// ---- 5. 重载 ----
	//
	// 只有真的改过文件才重载。幂等重放不重载不是偷懒：重载会让所有客户端重新
	// 走一遍配置解析（已有连接可能被中断），为了“什么都没改”而付这个代价是错的。
	res.Report.Service = e.serviceState(ctx)
	// 服务不健康这件事与“这次改没改文件”无关：哪怕内容没变，也要说清这份配置
	// 此刻没在被提供，否则页面会拿 served=false 配一个没有任何解释的理由。
	switch res.Report.Service {
	case ServiceStateInactive:
		res.Report.Notes = append(res.Report.Notes, Blocker{
			Code: NoticeSMBServiceInactive,
			Message: "smbd 不在运行：配置已经写好并通过校验，但重载无从谈起，" +
				"这份共享此刻不对外提供访问（开机依赖已单独处理，服务要有人启动）",
		})
	case ServiceStateUnknown:
		res.Report.Notes = append(res.Report.Notes, Blocker{
			Code:    NoticeSMBServiceUnknown,
			Message: "问不出 smbd 的运行状态：配置已经写好并通过校验，但“有没有在提供访问”无法确认",
		})
	}
	switch {
	case !contentChanged && !includeChanged:
		res.Report.Reload = ReloadSkippedUnchanged
	case res.Report.Service == ServiceStateActive:
		if smbcontrol := e.tools(ctx).Smbcontrol; smbcontrol == "" {
			// 文件改好了，但没法让它生效。这不是“失败就算了”：调用方必须知道
			// 磁盘上的配置与 Samba 内存里的配置此刻不一致。
			res.Report.Reload = ReloadFailed
			res.Report.Notes = append(res.Report.Notes, Blocker{
				Code:    NoticeConfigNotWritable,
				Message: "没有 smbcontrol：配置已写入文件，但无法重载，Samba 仍在按旧配置服务",
			})
			break
		}
		if _, err := e.run(ctx, syscmd.Spec{
			Program: e.tools(ctx).Smbcontrol,
			Args:    []string{"all", "reload-config"},
		}, reloadTimeout); err != nil {
			res.Report.Reload = ReloadFailed
			res.Report.Notes = append(res.Report.Notes, Blocker{
				Code:    FailConfigReload,
				Message: "重载 Samba 配置失败：" + err.Error(),
			})
		} else if after := e.serviceState(ctx); after != ServiceStateActive {
			// reload 返回 0 不等于 smbd 还活着：真机上 smbd 停着时 smbcontrol 也回 0。
			res.Report.Reload = ReloadFailed
			res.Report.Service = after
			res.Report.Notes = append(res.Report.Notes, Blocker{
				Code:    FailConfigReload,
				Message: "重载请求之后 smbd 不再是 active（" + after + "）：配置写好了，但这一份此刻没有被提供",
			})
		} else {
			res.Report.Reload = ReloadDone
		}
	default:
		// 服务不健康：重载没有意义，前面已经记过原因。
		res.Report.Reload = ReloadSkippedInactive
	}

	// ---- 6. 读回：Samba 自己读出来的东西必须是我们要的那份 ----
	if err := e.readbackShares(ctx, desired, &res.Report); err != nil {
		return res, err
	}

	// ---- 7. 开机依赖：让 smbd 等这些挂载点就位 ----
	//
	// 放在读回之后：配置此刻能不能用（served）与开机顺序好不好是两件事，
	// 前面每一步都成立之后再来照顾“重启之后会不会先服务一个空目录”。
	// 这一步失败不回滚——配置是对的，缺的是额外的保护，报告里说清楚。
	deps := e.applyStartupDeps(ctx, sharesMountPaths(desired))
	res.Report.StartupDeps = deps.Status
	res.Report.StartupPaths = deps.Paths
	res.Report.Notes = append(res.Report.Notes, deps.Notes...)

	// Served 是“这份配置此刻真的在被 Samba 提供”的结论，四个条件同时成立才算：
	// 合并后的配置合格、include 已接入、smbd 在运行、重载的结局是“做了”或“不需要做”。
	// 少任何一条都只能说“写好了”，不能说“在提供服务”。
	res.Report.Served = res.Report.Service == ServiceStateActive &&
		(res.Report.Reload == ReloadDone || res.Report.Reload == ReloadSkippedUnchanged) &&
		res.Report.Testparm == "merged-ok" &&
		(res.Report.Include == IncludePresent || res.Report.Include == IncludeInstalled)
	return res, nil
}

// validateConfig 跑一次 testparm 并判定结果。
func (e *Engine) validateConfig(ctx context.Context, file string) error {
	tool := e.tools(ctx).Testparm
	if tool == "" {
		return dependencyBlocked("缺少 testparm：没有配置校验工具就不允许写 Samba 配置（%s）", NoticeTestparmMissing)
	}
	res, err := e.run(ctx, syscmd.Spec{Program: tool, Args: []string{"-s", file}}, validateTimeout)
	if verdictErr := TestparmVerdict(res, err); verdictErr != nil {
		return commandFailed("Samba 配置校验失败（%s）：%v", file, verdictErr)
	}
	return nil
}

// readbackShares 用 testparm 逐段读回：路径与授权名单必须与写进去的一致。
//
// 为什么再读一次：写文件成功、testparm 通过，都只说明“这份文本是合法的”。
// 段名被 Samba 归一化（大小写）、参数被同名段覆盖、include 顺序导致后写的赢——
// 这些都可能让**实际生效**的东西与我们写的不一样。读回的是 loadparm 算出来的值。
func (e *Engine) readbackShares(ctx context.Context, desired []ManagedShare, report *ManagedConfigReport) error {
	tool := e.tools(ctx).Testparm
	if tool == "" {
		return nil
	}
	p := e.paths()
	for _, sh := range desired {
		gotPath, err := e.readSectionParam(ctx, tool, p.Main, sh.Name, "path")
		if err != nil {
			return commandFailed("读回共享 %s 的 path 失败：%v（%s）", sh.Name, err, FailConfigReadback)
		}
		if strings.TrimSpace(gotPath) != sh.Dir {
			return commandFailed("共享 %s 在 Samba 里的 path 是 %q，写进去的是 %q（%s）",
				sh.Name, strings.TrimSpace(gotPath), sh.Dir, FailConfigReadback)
		}
		gotUsers, err := e.readSectionParam(ctx, tool, p.Main, sh.Name, "valid users")
		if err != nil {
			return commandFailed("读回共享 %s 的 valid users 失败：%v（%s）", sh.Name, err, FailConfigReadback)
		}
		if !sameUserList(gotUsers, sh.ValidUsers) {
			return commandFailed("共享 %s 在 Samba 里的授权名单是 %q，写进去的是 %q（%s）",
				sh.Name, strings.TrimSpace(gotUsers), strings.Join(sh.ValidUsers, ","), FailConfigReadback)
		}
	}
	return nil
}

// readSectionParam 读一个段里某个参数的有效值。
func (e *Engine) readSectionParam(ctx context.Context, tool, file, section, param string) (string, error) {
	res, err := e.run(ctx, syscmd.Spec{
		Program: tool,
		Args:    []string{"-s", "--section-name=" + section, "--parameter-name=" + param, file},
	}, validateTimeout)
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("testparm 退出码 %d（段 %s 不存在？）", exitErr.ExitCode, section)
		}
		return "", err
	}
	return res.Stdout, nil
}

// rollbackConfig 把配置还原到写之前的样子。
//
// 用内存里的旧内容原子写回，而不是去读备份文件：备份文件也可能写坏（虽然不会），
// 而“我们读到的旧字节”就是这次决定改写的依据本身。
func (e *Engine) rollbackConfig(ctx context.Context, res configApplyResult, st configState) error {
	p := e.paths()
	fs := e.fs()
	var problems []string
	if st.ManagedPresent {
		if err := e.restoreFile(p.Managed, st.Managed); err != nil {
			problems = append(problems, "托管配置还原失败："+err.Error())
		}
	} else if res.Report.File == ConfigFileWritten {
		if err := fs.Remove(p.Managed); err != nil && !os.IsNotExist(err) {
			problems = append(problems, "托管配置删除失败："+err.Error())
		}
	}
	if res.MainChanged {
		if err := e.restoreFile(p.Main, st.Main); err != nil {
			problems = append(problems, "主配置还原失败："+err.Error())
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "；"))
	}
	return nil
}

// restoreFile 用 old 内容原子替换 path。
func (e *Engine) restoreFile(target string, old []byte) error {
	fs := e.fs()
	tmp := target + ".upanel-restore"
	_ = fs.Remove(tmp)
	if err := fs.WriteFileSync(tmp, old, managedFileMode); err != nil {
		return err
	}
	if err := fs.Rename(tmp, target); err != nil {
		_ = fs.Remove(tmp)
		return err
	}
	return fs.FsyncDir(filepath.Dir(target))
}

// backupFile 把现行文件复制到备份目录，返回备份路径。
func (e *Engine) backupFile(p ConfigPaths, src string, content []byte) (string, error) {
	fs := e.fs()
	if err := fs.MkdirAll(p.BackupDir, 0o700); err != nil {
		return "", commandFailed("创建备份目录 %s 失败：%v（%s）", p.BackupDir, err, FailConfigWrite)
	}
	now := e.opts.Now().UTC()
	name := fmt.Sprintf("%s-%s.%s", now.Format("20060102T150405Z"),
		ConfigHashOf(string(content))[:12], filepath.Base(src))
	full := filepath.Join(p.BackupDir, name)
	if err := fs.WriteFileSync(full, content, 0o600); err != nil {
		return "", commandFailed("写备份 %s 失败：%v（%s）", full, err, FailConfigWrite)
	}
	e.pruneBackups(p, filepath.Base(src))
	return full, nil
}

// backupKeep 是每类配置文件保留的备份数量。
//
// 一次应用最多产生两份备份，而“应用共享”是可以一天点很多次的动作。留 40 份既能
// 覆盖足够长的历史，又不会让 /var/lib/upanel-agent 在几年里涨成几十万个小文件。
const backupKeep = 40

// pruneBackups 只保留最新的若干份同类备份。清理失败不影响主流程（备份多大不了）。
func (e *Engine) pruneBackups(p ConfigPaths, base string) {
	entries, err := e.fs().ReadDir(p.BackupDir)
	if err != nil {
		return
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "."+base) {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) <= backupKeep {
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // 时间戳前缀，倒序即最新在前
	for _, name := range names[backupKeep:] {
		_ = e.fs().Remove(filepath.Join(p.BackupDir, name))
	}
}

// sectionNamesOf 取出这批共享的段名（渲染顺序即写入顺序）。
func sectionNamesOf(shares []ManagedShare) []string {
	out := make([]string, 0, len(shares))
	for _, s := range shares {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// sameUserList 比较两份用户名清单（逗号或空白分隔，顺序无关）。
//
// testparm 打出来的 valid users 会按它自己的规则拼接（有的版本逗号后带空格），
// 直接比字符串会把“内容相同、空白不同”判成失败——那是断言在挑刺，不是产品的问题。
func sameUserList(got string, want []string) bool {
	fields := strings.FieldsFunc(got, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	gotSet := make(map[string]bool, len(fields))
	for _, f := range fields {
		if f != "" {
			gotSet[f] = true
		}
	}
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	if len(gotSet) != len(wantSet) {
		return false
	}
	for name := range wantSet {
		if !gotSet[name] {
			return false
		}
	}
	return true
}

// configDrift 判断托管文件与登记表是否一致。
//
// 两个方向都要看：登记表说“已应用”但文件里没有这一段（有人手工删了），以及文件里
// 有一段登记表说已经禁用/删掉的（有人手工加回来）。返回漂移的共享名。
func configDrift(records []ShareRecord, managedSections []string) []string {
	present := make(map[string]bool, len(managedSections))
	for _, name := range managedSections {
		present[strings.ToLower(name)] = true
	}
	var drift []string
	for _, rec := range records {
		key := strings.ToLower(rec.Definition.Name)
		switch {
		case rec.Definition.Enabled && rec.ConfigApplied && !present[key]:
			drift = append(drift, rec.Definition.Name)
		case (!rec.Definition.Enabled || !rec.ConfigApplied) && present[key]:
			drift = append(drift, rec.Definition.Name)
		}
	}
	sort.Strings(drift)
	return drift
}

// onlyRemoval 判断新内容相对旧内容是不是“只删不改”。
//
// 判据有两条，缺一不可：
//   - 旧文件的开头就是”头部注释 + [global] 段“的那一份（说明它确实是面板写的，
//     且我们没有改动全局项——改全局项会改变对所有共享生效的行为）；
//   - 新内容里的每一段都逐字出现在旧文件里（说明没有任何一段被新增或改写）。
//
// 它只在一个地方被使用：testparm 缺失时决定“这次写入能不能放行”。放开的是摘除，
// 因为删掉行不可能引入新的语法错误；任何新增或改动仍然必须先过校验。
func onlyRemoval(shares []ManagedShare, oldText string) bool {
	if !strings.HasPrefix(oldText, RenderManagedConfig(nil)) {
		return false
	}
	for _, s := range shares {
		if !strings.Contains(oldText, strings.TrimRight(s.Block, "\n")) {
			return false
		}
	}
	return true
}
