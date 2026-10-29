package shares

// NFS 导出的写入路径（P14.4）。
//
// 顺序与 Samba 那一半同一个纪律，但每一步的“校验工具”不同：
//
//	渲染 → 内容没变就跳过 → 备份 → 写候选 → rename → fsync 目录
//	     → exportfs -ra（让内核与 etab 都吃到新表）
//	     → exportfs -v 读回**实际生效的导出表**逐条比对
//	     → 任一步失败：还原上一版文件、再 ra 一次、把现场写进错误里
//
// 与 Samba 最大的不同在“校验”这一步：testparm 能在**替换之前**判定候选合不合格，
// 而 exports 没有等价的预校验工具——`exportfs -ra` 既校验又生效，它失败的时候
// 新表可能已经部分生效了。所以这里的兜底不是“先验后换”，而是“换了之后一定要能退回去”：
// 还原文件 + 再刷一次表，然后如实报告“这次没成”。
//
// 三条硬纪律：
//  1. **不用退出码当结论**：`exportfs -ra` 返回 0 只说明它读完了文件，不说明
//     root_squash 真的生效——选项被后面的条目抵掉、内核没吃到新表都发生过。
//     判决一律来自 `exportfs -v` 的实际读数。
//  2. **不碰别人的导出**：只写 `upanel.exports` 一个文件，整份重写时也只渲染
//     面板自己那几条（靠行尾标记认领）。
//  3. **卷不在就跳过**：卷没挂时那条目录只是系统盘上的空目录，导出它等于把根分区
//     共享出去（PLAN.md 第 10 节）。跳过并在报告里点名，不静默。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

const (
	// exportfsTimeout 是一次 `exportfs -ra` 的预算。
	//
	// 大量导出（几百条）时 exportfs 要逐条与内核打交道，实测慢的是 rpc.mountd 的
	// 重读而不是文件解析。30 秒对一台共享服务器是宽裕的，对一台卡住的机器
	// 又不会把任务队列堵死太久。
	exportfsTimeout = 30 * time.Second
)

// ExportsReport 是一次导出应用的结果。
//
// 每个字段都是“真的做了什么”：File 是 written/unchanged，Reload 是
// done/skipped_unchanged/skipped_service_inactive/failed，Served 只有
// “读回比对通过 且 nfs-server 在运行”时才是 true。
type ExportsReport struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	// File 是 written 或 unchanged。
	File string `json:"file"`
	// Reload 是本轮 exportfs -ra 的结果。
	Reload string `json:"reload"`
	// Service 是 nfs-server 的运行状态。
	Service string `json:"service"`
	// Served 是“这份导出此刻真的在被 NFS 服务端提供”的结论。
	Served bool `json:"served"`
	// Exports 是读回后导出表里属于面板的路径（排序）。
	Exports []string `json:"exports"`
	// VolumeOffline 是这一轮被跳过的共享名（卷没挂）。
	VolumeOffline []string `json:"volume_offline_shares"`
	// StartupDeps 是 nfs-server 开机依赖的写入结果（written / unchanged / removed /
	// failed / skipped）。
	//
	// 它单独一项，理由与 SMB 那边相同：导出本身是对的，缺的只是“开机时等卷挂上再起
	// 服务”这一层保护。把它说成失败会让人以为导出坏了，说成通过会让人以为重启顺序
	// 已经被保证——两者都不对。
	StartupDeps string `json:"startup_deps"`
	// StartupPaths 是写进 nfs-server 单元的那些“必须先就位”的路径。
	StartupPaths []string `json:"startup_paths"`
	// Backups 是这次产生的备份文件路径（没写就没有）。
	Backups []string `json:"backups"`
	// Notes 是给人看的补充说明。
	Notes []Blocker `json:"notes"`
}

// exportsState 是动手之前的一次现场快照（只读）。
type exportsState struct {
	Present bool
	Content []byte
	Service string
	// Others 是别的导出文件里的条目（冲突检测用）。
	Others []ExportEntry
	// Unparsed 是别的导出文件里读不懂的行。
	Unparsed []string
}

// exportsApplyResult 是写导出这一段的内部结果（与对外报告分开，便于回滚）。
type exportsApplyResult struct {
	Report ExportsReport
	// Previous 是这次写之前的文件内容；HadPrevious 说明它原本存不存在。
	Previous    []byte
	HadPrevious bool
	Backup      string
	// Changed 报告文件被改过（回滚要还原它）。
	Changed bool
}

// exportsBackupDir 是导出文件备份的落点。
//
// 与 Samba 的备份目录分开：两者的备份是给不同的人看的（一个是 smb.conf 的一份快照，
// 一个是 exports 的一份快照），混在一起会让“上周谁改了什么”变成一个要先分辨格式的问题。
func exportsBackupDir(p ConfigPaths) string {
	return filepath.Join(filepath.Dir(p.Exports), "upanel-backups")
}

// readExportsState 读一次现场：托管文件、别的导出文件、服务状态。
//
// 它只读。读不到托管文件不是错误（第一次应用就是这样），但读不到目录则可能是权限问题，
// 那种情况下继续写只会得到一句更晚、更难懂的失败。
func (e *Engine) readExportsState(ctx context.Context) (exportsState, error) {
	p := e.paths()
	fs := e.fs()

	st := exportsState{}
	if data, err := fs.ReadFile(p.Exports); err == nil {
		st.Present = true
		st.Content = data
	} else if !os.IsNotExist(err) {
		return st, dependencyBlocked("读不到托管导出文件 %s：%v（%s）", p.Exports, err, FailExportsWrite)
	}

	others, unparsed, err := e.readOtherExports()
	if err != nil {
		return st, err
	}
	st.Others = others
	st.Unparsed = unparsed
	st.Service = probeNFSServiceState(ctx, e.opts.Runner)
	return st, nil
}

// readOtherExports 读别的导出文件（export.d 里其它 *.exports 与 /etc/exports）。
//
// 这些文件面板**不碰**，读它们只为回答一个问题：我要导出的这条路径，别人是不是
// 已经在导出了。同一条路径被导出两次时 exportfs 报 duplicated 并以 1 退出，
// 那会让整份导出表的刷新失败——包括别人本来好用的那些。
func (e *Engine) readOtherExports() ([]ExportEntry, []string, error) {
	p := e.paths()
	fs := e.fs()

	var (
		entries  []ExportEntry
		unparsed []string
	)
	// 先读主文件。
	if data, err := fs.ReadFile(p.ExportsMain); err == nil {
		got, bad := ParseExports(data)
		entries = append(entries, got...)
		unparsed = append(unparsed, bad...)
	} else if !os.IsNotExist(err) {
		// 读不到不等于没有导出：不知道就说不确定，由调用方降级成告警。
		return nil, nil, dependencyBlocked(
			"读不到 %s：%v；无法确认这条路径是否已经被别人导出（%s）",
			p.ExportsMain, err, NoticeExportsUnparsable)
	}
	// 再读 exports.d 里的其它文件。
	des, err := fs.ReadDir(p.ExportsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return entries, unparsed, nil
		}
		return nil, nil, dependencyBlocked(
			"读不到导出目录 %s：%v（%s）", p.ExportsDir, err, FailExportsWrite)
	}
	own := filepath.Base(p.Exports)
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || name == own || !strings.HasSuffix(name, exportsFileSuffix) {
			continue
		}
		data, err := fs.ReadFile(filepath.Join(p.ExportsDir, name))
		if err != nil {
			return nil, nil, dependencyBlocked(
				"读不到导出文件 %s：%v；无法确认路径冲突（%s）", name, err, NoticeExportsUnparsable)
		}
		got, bad := ParseExports(data)
		entries = append(entries, got...)
		unparsed = append(unparsed, bad...)
	}
	return entries, unparsed, nil
}

// desiredExports 算出“应用完这一版定义之后，托管文件里应该有哪些导出”。
//
// 与 desiredShares 同形：把登记表里的**所有** nfs 共享（换成新定义的那一条）
// 一起算。整份重写是唯一不会让别人的段悄悄消失的做法。
//
// pendingID 只在新共享上有用，而且必须是**真的**那个 id——行尾标记写的是它。
// 所以执行器先取 id 再渲染（见 executeShareApplyNFS）：先用占位符渲染，等落库时
// 再换 id，会留下一行指向不存在记录的标记。
func (e *Engine) desiredExports(ctx context.Context, newDef ShareDefinition, shareID, pendingID string) ([]ManagedExport, []string, error) {
	records, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]ShareRecord, 0, len(records)+1)
	replaced := false
	for _, rec := range records {
		if shareID != "" && rec.ID == shareID {
			rec.Definition = newDef
			replaced = true
		}
		out = append(out, rec)
	}
	if !replaced {
		out = append(out, ShareRecord{ID: pendingID, Definition: newDef})
	}
	return e.renderExports(ctx, out)
}

// exportsExcept 算出“摘掉某条共享之后”的全集。
func (e *Engine) exportsExcept(ctx context.Context, shareID string) ([]ManagedExport, []string, error) {
	records, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]ShareRecord, 0, len(records))
	for _, rec := range records {
		if rec.ID == shareID {
			continue
		}
		out = append(out, rec)
	}
	return e.renderExports(ctx, out)
}

// renderExports 把记录渲染成导出行。
//
// 卷没挂的记录被跳过并记名（与 SMB 的 renderManagedShares 同一条理由）。
func (e *Engine) renderExports(ctx context.Context, records []ShareRecord) ([]ManagedExport, []string, error) {
	out := make([]ManagedExport, 0, len(records))
	var skipped []string
	for _, rec := range records {
		def := rec.Definition
		if def.Protocol != ProtocolNFS || !def.Enabled {
			continue
		}
		vol, found, err := e.volumeFor(ctx, def.VolumeID)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, dependencyBlocked(
				"共享 %s 引用的卷 %s 已不在受管理卷里：无法计算它的目录，导出表拒绝只写一半（%s）",
				def.Name, def.VolumeID, NoticeVolumeNotFound)
		}
		mountPath := vol.MountPath
		if mountPath == "" {
			mountPath = storage.MountPathOf(vol.Slug)
		}
		gate, _, err := e.mountGateFor(ctx, def.VolumeID)
		if err != nil {
			return nil, nil, err
		}
		if !gate.Mounted {
			skipped = append(skipped, def.Name)
			continue
		}
		dir, err := ShareDir(mountPath, def.RelativePath)
		if err != nil {
			return nil, nil, validationError("%s", err.Error())
		}
		if rec.ID == "" {
			// 行尾标记要写 id。没有 id 就渲染不出一条能被认领的导出，
			// 而这绝不是“等一下会好”——是调用方没按上面的约定先取 id。
			return nil, nil, validationError("共享 %s 没有标识，无法写导出的认领标记", def.Name)
		}
		me := ManagedExport{
			ShareID:   rec.ID,
			Name:      def.Name,
			Dir:       dir,
			MountPath: mountPath,
			ReadOnly:  def.ReadOnly,
		}
		cidrs, err := NormalizeCIDRs(def.AllowedCIDRs)
		if err != nil {
			return nil, nil, validationError("共享 %s 的允许网段不合法：%v", def.Name, err)
		}
		me.CIDRs = cidrs
		line, err := RenderExportLine(me)
		if err != nil {
			return nil, nil, validationError("渲染共享 %s 的导出行失败：%v", def.Name, err)
		}
		me.Line = line
		out = append(out, me)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir < out[j].Dir
		}
		return out[i].Name < out[j].Name
	})
	sort.Strings(skipped)
	return out, skipped, nil
}

// exportPermissions 是托管导出文件的权限。
//
// 0644：里面有允许的网段，没有秘密；而它要能被 exportfs（root）读到，
// 也要能被管理员 `cat` 出来核对——这正是“配置透明”的一部分。
const exportPermissions os.FileMode = 0o644

// applyExports 把 desired 写成托管导出文件并让它生效，然后读回核对。
//
// 调用方必须持有 `shares:config` 锁：整份文件是读-改-写的，两个并发应用会让
// 后一个把前一个的导出覆盖掉。
func (e *Engine) applyExports(ctx context.Context, desired []ManagedExport, offline []string, report func(step string, fields map[string]any)) (exportsApplyResult, error) {
	p := e.paths()
	fs := e.fs()

	st, err := e.readExportsState(ctx)
	if err != nil {
		return exportsApplyResult{}, err
	}

	desiredText := RenderExportsFile(desired)
	res := exportsApplyResult{
		Previous:    st.Content,
		HadPrevious: st.Present,
		Report: ExportsReport{
			Path:          p.Exports,
			Hash:          ConfigHashOf(desiredText),
			File:          ConfigFileUnchanged,
			Reload:        ReloadSkippedUnchanged,
			Service:       st.Service,
			Exports:       exportPathsOf(desired),
			VolumeOffline: append([]string(nil), offline...),
		},
	}
	if len(offline) > 0 {
		res.Report.Notes = append(res.Report.Notes, Blocker{
			Code: NoticeVolumeOffline,
			Message: "这些共享这次没有写进导出表，因为它们绑定的卷此刻没挂载：" +
				strings.Join(offline, "、") + "；卷回来之后要重新应用一次",
		})
	}

	// 冲突最后再确认一次：预检到执行之间，别人可能刚好加了一条同路径的导出。
	if conflicts := ExportConflicts(desired, st.Others); len(conflicts) > 0 {
		return res, validationError("%s（%s）", describeExportConflicts(conflicts), NoticeExportPathConflict)
	}
	// 同目录的两条共享，写下去就是两份同路径导出——exportfs 会整份拒绝。
	// 预检拦过一次（Assessment 的 EXPORT_PATH_CONFLICT），这里再拦一次：
	// 登记表在预检之后也可能被别的请求改。
	if dups := SameDirShares(desired); len(dups) > 0 {
		return res, validationError("%s（%s）", describeSameDirShares(dups), NoticeExportPathConflict)
	}

	// ---- 1. 目录 ----
	if _, err := fs.Stat(p.ExportsDir); err != nil {
		if !os.IsNotExist(err) {
			return res, dependencyBlocked("读不到导出目录 %s：%v（%s）", p.ExportsDir, err, FailExportsWrite)
		}
		if err := fs.MkdirAll(p.ExportsDir, 0o755); err != nil {
			return res, dependencyBlocked("创建导出目录 %s 失败：%v（%s）", p.ExportsDir, err, FailExportsWrite)
		}
		res.Report.Notes = append(res.Report.Notes, Blocker{
			Code:    NoticeExportsNotApplied,
			Message: "已创建导出目录 " + p.ExportsDir,
		})
		if report != nil {
			report("share_exports_dir", map[string]any{"dir": p.ExportsDir})
		}
	}

	// ---- 2. 写候选并替换 ----
	contentChanged := !st.Present || string(st.Content) != desiredText
	if contentChanged {
		candidate := p.Exports + ".upanel-new"
		_ = fs.Remove(candidate) // 上一次失败可能留下临时文件；权限只在创建时生效，所以先删
		if err := fs.WriteFileSync(candidate, []byte(desiredText), exportPermissions); err != nil {
			return res, dependencyBlocked("写候选导出文件 %s 失败：%v（%s）", candidate, err, FailExportsWrite)
		}
		defer func() { _ = fs.Remove(candidate) }()

		if st.Present {
			backup, err := e.backupExports(st.Content)
			if err != nil {
				return res, err
			}
			res.Backup = backup
			res.Report.Backups = append(res.Report.Backups, backup)
		}
		if err := fs.Rename(candidate, p.Exports); err != nil {
			return res, commandFailed("替换导出文件 %s 失败：%v（%s）", p.Exports, err, FailExportsWrite)
		}
		if err := fs.FsyncDir(filepath.Dir(p.Exports)); err != nil {
			// 目录项没落盘时掉电会回到旧文件——那等于导出悄悄消失，所以当场说清楚。
			return res, commandFailed("%s 已替换，但目录 fsync 失败（掉电后可能回退）：%v（%s）",
				p.Exports, err, FailExportsWrite)
		}
		res.Changed = true
		res.Report.File = ConfigFileWritten
		if report != nil {
			report("share_exports_file", map[string]any{"path": p.Exports, "hash": res.Report.Hash})
		}
	}

	// ---- 3. 让导出表生效 ----
	//
	// 内容没变也要问一句“现在真的还是这份表吗”：有人手工 `exportfs -ua` 之后，
	// 文件没变而内核里已经没有导出了。那种情形下幂等重放必须能修好它，
	// 否则“再点一次应用”这个动作在最需要它的时候恰好什么都不做。
	table, verr := e.readExportTable(ctx)
	if contentChanged || verr != nil || !exportTableMatches(table, desired) {
		if err := e.reloadExports(ctx); err != nil {
			return res, withRollbackNote(err, e.restoreExportsAfterFailure(ctx, res))
		}
		res.Report.Reload = ReloadDone
		if report != nil {
			report("share_exports_reload", map[string]any{"exports": res.Report.Exports})
		}
		table, verr = e.readExportTable(ctx)
	}
	if verr != nil {
		return res, withRollbackNote(
			commandFailed("读回导出表失败：%v（%s）", verr, FailExportVerify),
			e.restoreExportsAfterFailure(ctx, res))
	}

	// ---- 4. 读回核对 ----
	var mismatches []string
	for _, want := range desired {
		if why := VerifyExport(table, want); why != "" {
			mismatches = append(mismatches, why)
		}
	}
	// “该消失的消失了”同样要核对：只查“在的都在”，删不掉的导出行就等于没删。
	for _, gone := range staleExportPaths(st, desired) {
		if _, ok := table[gone]; ok {
			mismatches = append(mismatches, gone+" 应该已经不在导出表里，但还在")
		}
	}
	if len(mismatches) > 0 {
		return res, withRollbackNote(
			commandFailed("导出表读回核对不通过：%s（%s）", strings.Join(mismatches, "；"), FailExportVerify),
			e.restoreExportsAfterFailure(ctx, res))
	}

	// ---- 5. 开机依赖：让 nfs-server 等这些挂载点 ----
	//
	// 放在核对通过之后：导出一旦生效，重启顺序就成了“下次开机时那些路径背后是不是
	// 那个文件系统”的唯一保证。它失败不回滚导出——导出本身是对的，缺的只是这层保护。
	dep := e.applyStartupDropin(ctx, p.ExportsStartupDep, "nfs-server", exportMountPaths(desired))
	res.Report.StartupDeps = dep.Status
	res.Report.StartupPaths = dep.Paths
	res.Report.Notes = append(res.Report.Notes, dep.Notes...)
	if dep.Changed && report != nil {
		report("share_exports_startup_deps", map[string]any{
			"path": p.ExportsStartupDep, "status": dep.Status, "paths": dep.Paths,
		})
	}

	// ---- 6. 结论 ----
	res.Report.Service = probeNFSServiceState(ctx, e.opts.Runner)
	res.Report.Served = res.Report.Service == ServiceStateActive
	if !res.Report.Served {
		res.Report.Reload = ReloadSkippedInactive
		res.Report.Notes = append(res.Report.Notes, Blocker{
			Code:    NoticeNFSServiceInactive,
			Message: "导出已经写进配置并且被 exportfs 接受，但 nfs-server 现在不是 active：客户端连不上",
		})
	}
	return res, nil
}

// restoreExportsAfterFailure 把文件与导出表都退回去。
//
// 失败时不留下“文件是新的、表是旧的”这种半截状态：那是最难查的一种——
// 下一次应用会以为“内容没变”而跳过写文件，于是内核里永远停在那张旧表上。
//
// 返回值是“还原本身有没有成功”，**它必须被看见**：还原失败时系统停在哪一步
// （旧文件在不在、内核表刷没刷）决定了人下一步该做什么，把这类信息咽掉就等于
// 让操作者对着一句“动作执行失败”猜现场。写导出失败的那几处只在错误消息末尾附上它，
// 因为主因已经写在前面了。
func (e *Engine) restoreExportsAfterFailure(ctx context.Context, res exportsApplyResult) error {
	if !res.Changed {
		return nil
	}
	p := e.paths()
	fs := e.fs()
	var err error
	if res.HadPrevious {
		err = fs.WriteFileSync(p.Exports, res.Previous, exportPermissions)
	} else {
		err = fs.Remove(p.Exports)
	}
	if err == nil {
		_ = fs.FsyncDir(filepath.Dir(p.Exports))
	}
	// 无论还原成功与否都要再刷一次表：这是让内核回到“文件说的那样”的唯一手段。
	rerr := e.reloadExports(ctx)
	switch {
	case err != nil && rerr != nil:
		return fmt.Errorf("%s 没能还原（%v），导出表也没能重刷（%v）：此刻文件与内核是否一致无法确定",
			p.Exports, err, rerr)
	case err != nil:
		return fmt.Errorf("%s 没能还原（%v）；导出表已按现有文件重刷", p.Exports, err)
	case rerr != nil:
		return fmt.Errorf("%s 已还原，但导出表没能重刷（%v）：内核里可能还留着刚写进去的条目",
			p.Exports, rerr)
	}
	return nil
}

// backupExports 把现有导出文件备份一份（带时间戳与序号）。
func (e *Engine) backupExports(content []byte) (string, error) {
	dir := exportsBackupDir(e.paths())
	fs := e.fs()
	if err := fs.MkdirAll(dir, 0o700); err != nil {
		return "", commandFailed("创建导出备份目录 %s 失败：%v（%s）", dir, err, FailExportsWrite)
	}
	stamp := e.opts.Now().UTC().Format("20060102T150405Z")
	base := "exports-" + stamp
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		target := filepath.Join(dir, name)
		if _, err := fs.Stat(target); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", commandFailed("检查备份路径 %s 失败：%v（%s）", target, err, FailExportsWrite)
		}
		if err := fs.WriteFileSync(target, content, 0o600); err != nil {
			return "", commandFailed("写导出备份 %s 失败：%v（%s）", target, err, FailExportsWrite)
		}
		return target, nil
	}
	return "", commandFailed("导出备份目录里同名文件太多，无法生成唯一备份名（%s）", FailExportsWrite)
}

// reloadExports 跑一次 `exportfs -ra`。
func (e *Engine) reloadExports(ctx context.Context) error {
	t := e.tools(ctx)
	if t.Exportfs == "" {
		return dependencyBlocked("缺少 exportfs：无法让导出表生效（%s）", NoticeNFSUnavailable)
	}
	res, err := e.run(ctx, specExportfsReload(t), exportfsTimeout)
	if err != nil {
		return commandFailed("exportfs -ra 失败：%s（%s）", commandFailureText(res, err), FailExportsReload)
	}
	return nil
}

// readExportTable 读一次实际导出表（`exportfs -v`）。
func (e *Engine) readExportTable(ctx context.Context) (ExportTable, error) {
	t := e.tools(ctx)
	if t.Exportfs == "" {
		return nil, errors.New("缺少 exportfs，读不回实际导出表")
	}
	res, err := e.run(ctx, specExportfsV(t), exportfsTimeout)
	if err != nil {
		return nil, fmt.Errorf("exportfs -v 失败：%s", commandFailureText(res, err))
	}
	return ParseExportfsV(res.Stdout)
}

// commandFailureText 把一次失败的命令结果压成一句能进错误消息的话。
//
// 优先用 stderr 的第一行：exportfs 的报错（duplicated export entries、
// Failed to stat …）就在那里，比“退出码 1”有用得多。
func commandFailureText(res syscmd.Result, err error) string {
	msg := strings.TrimSpace(res.Stderr)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	if msg != "" {
		return msg
	}
	return err.Error()
}

// exportTableMatches 报告实际导出表是不是已经是 desired 的样子。
func exportTableMatches(table ExportTable, desired []ManagedExport) bool {
	if table == nil {
		return false
	}
	for _, want := range desired {
		if why := VerifyExport(table, want); why != "" {
			return false
		}
	}
	return true
}

// staleExportPaths 是“上一个版本里有、这一版里没有”的路径。
//
// 只算面板自己文件里的那些：别人的导出不归这一轮管。
func staleExportPaths(st exportsState, desired []ManagedExport) []string {
	if !st.Present {
		return nil
	}
	old, _ := ParseExports(st.Content)
	now := make(map[string]bool, len(desired))
	for _, d := range desired {
		now[d.Dir] = true
	}
	seen := make(map[string]bool, len(old))
	out := make([]string, 0, len(old))
	for _, o := range old {
		if seen[o.Path] || now[o.Path] {
			continue
		}
		seen[o.Path] = true
		out = append(out, o.Path)
	}
	sort.Strings(out)
	return out
}

// exportMountPaths 收集一组导出用到的卷挂载点（去重、排序）。
//
// 与 sharesMountPaths 分开写而不是抽成一个泛型函数：两者的输入是两种不同的渲染结果，
// 硬凑成一个接口只会让读的人多跳一次。
func exportMountPaths(exports []ManagedExport) []string {
	out := make([]string, 0, len(exports))
	for _, e := range exports {
		if e.MountPath != "" {
			out = append(out, e.MountPath)
		}
	}
	sort.Strings(out)
	return dedupeStrings(out)
}

// exportPathsOf 返回一组导出的目录（排序，去重）。
func exportPathsOf(exports []ManagedExport) []string {
	seen := make(map[string]bool, len(exports))
	out := make([]string, 0, len(exports))
	for _, e := range exports {
		if seen[e.Dir] {
			continue
		}
		seen[e.Dir] = true
		out = append(out, e.Dir)
	}
	sort.Strings(out)
	return out
}

// describeExportConflicts 把冲突摊成一句人读的话。
func describeExportConflicts(conflicts map[string][]string) string {
	paths := make([]string, 0, len(conflicts))
	for p := range conflicts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		parts = append(parts, p+"（共享 "+strings.Join(conflicts[p], "、")+"）")
	}
	return "这些目录已经被别的导出文件导出了：" + strings.Join(parts, "；") +
		"——同一条路径导出两次会让 exportfs 整份刷新失败，请先在那边删掉，或换一个相对路径（" +
		NoticeExportPathConflict + "）"
}

// describeSameDirShares 把“两条共享占同一个目录”摊成一句人读的话。
func describeSameDirShares(dups map[string][]string) string {
	dirs := make([]string, 0, len(dups))
	for d := range dups {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	parts := make([]string, 0, len(dirs))
	for _, d := range dirs {
		parts = append(parts, d+"（共享 "+strings.Join(dups[d], "、")+"）")
	}
	return "这些目录被不止一条面板共享占着：" + strings.Join(parts, "；") +
		"——同一个目录导出两次会让 exportfs 整份刷新失败，请给其中一条换一个相对路径（" +
		NoticeExportPathConflict + "）"
}

// sharesSharingDir 回答“这个目录此刻是不是已经被面板自己的另一条共享占着”。
//
// 判据与执行期同一条（SameDirShares）：把别的共享按同一套渲染规则算出目录再比。
// 不自己走一遍“卷挂没挂 / 相对路径合法不合法”的另一套算法——两套算法迟早会分叉，
// 然后预检说不冲突、执行期说冲突。
//
// 候选共享按**名字**排除自己：预检时新建的共享还没有 id（id 在执行时才分配），
// 名字是那一刻唯一能确定“这条是我”的东西（登记表里名字唯一）。
func (e *Engine) sharesSharingDir(ctx context.Context, def ShareDefinition, dir, shareID string) ([]string, error) {
	records, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil, err
	}
	others := make([]ShareRecord, 0, len(records))
	for _, rec := range records {
		if shareID != "" && rec.ID == shareID {
			continue
		}
		if rec.Definition.Name == def.Name {
			continue
		}
		others = append(others, rec)
	}
	if len(others) == 0 {
		return nil, nil
	}
	exports, _, err := e.renderExports(ctx, others)
	if err != nil {
		return nil, err
	}
	names := []string{def.Name}
	for _, ex := range exports {
		if ex.Dir == dir {
			names = append(names, ex.Name)
		}
	}
	if len(names) == 1 {
		return nil, nil
	}
	sort.Strings(names)
	return names, nil
}

// exportPathConflictBlockers 是预检用的冲突判定。
//
// 与执行期用同一份数据、同一个函数（ExportConflicts）：预检说不冲突、执行说冲突，
// 是用户最没法理解的一种失败。
func (e *Engine) exportPathConflicts(ctx context.Context, desired []ManagedExport) ([]Blocker, []Blocker, error) {
	others, unparsed, err := e.readOtherExports()
	if err != nil {
		return nil, nil, err
	}
	var blockers, warnings []Blocker
	if conflicts := ExportConflicts(desired, others); len(conflicts) > 0 {
		blockers = append(blockers, Blocker{
			Code:    NoticeExportPathConflict,
			Message: describeExportConflicts(conflicts),
		})
	}
	if len(unparsed) > 0 {
		warnings = append(warnings, Blocker{
			Code: NoticeExportsUnparsable,
			Message: fmt.Sprintf("别的导出文件里有 %d 行读不懂（%s）：无法确认这条路径没被别人导出过",
				len(unparsed), strings.Join(tailLines(unparsed, 3), " / ")),
		})
	}
	return blockers, warnings, nil
}

// tailLines 取前 n 行（用于消息里点名，不把整份文件塞进日志）。
func tailLines(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}
