package shares

// 运行时脱盘（P08.3）。
//
// 开机顺序由 systemd 的 RequiresMountsFor 管住（startupdeps.go），但那只覆盖“开机那一次”。
// 运行中的盘会以另外几种方式离开：虚拟盘被摘掉、iSCSI/USB 断链、有人手工 umount、
// 卷进入 error 状态。这些时刻 smbd 不会自己停下来，它仍然拿着那个路径——而路径背后
// 已经不是那个文件系统了。
//
// 这里的做法是**收缩暴露面**：发现某条共享绑定的卷不在挂载状态，就把它的段从托管
// 配置里整份重写掉（连同重载），并在登记表上记下这次撤销。客户端拿到的是
// “共享不存在”，而不是“一个空文件夹”。
//
// 三条刻意的取舍：
//
//  1. **只撤不恢复**。卷回来之后不会自动写回配置——写配置是需要人点头的动作。
//     自动写回等于把一次外部故障变成一次静默变更，而静默变更正是这个面板最不该有的
//     东西。撤销时在记录里留了现场，页面据此告诉用户“重新应用一次”。
//  2. **撤销是安全的单向操作**。少一份共享是可见、可重放、可修的；多一份指向空目录的
//     共享是静默误导。两者代价不对称，所以观测不到“挂着”就按没挂处理。
//  3. **不自己拿锁**。调用方必须持有 `samba:config`（见 CONTRACTS）：撤销写的是整份
//     托管文件，与一次人工应用是同一种写入，必须和它们串行。

import (
	"context"
	"os"
	"strings"
	"time"
)

// RevokedShare 是一条被运行时撤销的共享及其现场。
type RevokedShare struct {
	ShareID string `json:"share_id"`
	// Name 是共享名（段名），报告里点名用。
	Name string `json:"name"`
	// Revocation 是撤销的现场（时间、卷、当时的状态、原因）。
	Revocation RuntimeRevocation `json:"revocation"`
}

// ReconcileReport 是一次运行时对账的结果。
type ReconcileReport struct {
	// Checked 是这次看过的启用共享数。
	Checked int `json:"checked"`
	// Revoked 是这次被撤销的共享；空表示没有卷离线，配置一个字节都没动。
	Revoked []RevokedShare `json:"revoked"`
	// ConfigWrites 报告这次有没有真的重写托管配置。
	ConfigWrites bool `json:"config_written"`
	// Reload / Service 是这次写入之后的重载与服务状态（没写就是空）。
	Reload  string `json:"reload,omitempty"`
	Service string `json:"service,omitempty"`
	// Served 是“剩下的那些段此刻在被 Samba 提供”的结论。
	Served bool `json:"served"`
	// ExportsWritten 报告这次有没有真的重写托管导出文件（NFS，P14.4）。
	ExportsWritten bool `json:"exports_written"`
	// ExportsReload / ExportsServed 是导出这一半的结果（没写就是空）。
	ExportsReload string `json:"exports_reload,omitempty"`
	ExportsServed bool   `json:"exports_served"`
	// Notes 是给人看的补充说明。
	Notes []Blocker `json:"notes"`
	// CheckedAt 是这次对账的时间。占用/挂载都是某一瞬间的事实，必须带时间。
	CheckedAt time.Time `json:"checked_at"`
}

// ReconcileVolumes 检查每条启用共享的卷，把卷已离线的共享从配置里摘掉。
//
// 幂等：卷一直离线时，第二次调用发现“配置里已经没这几段、记录上也已经记过”，
// 于是不写文件、不重载、不产生新备份。
func (e *Engine) ReconcileVolumes(ctx context.Context) (ReconcileReport, error) {
	now := e.opts.Now().UTC()
	out := ReconcileReport{Revoked: []RevokedShare{}, CheckedAt: now}

	records, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return out, err
	}

	type pending = offlineShare
	var offline []pending
	for _, rec := range records {
		if !rec.Definition.Enabled {
			continue
		}
		out.Checked++
		gate, found, err := e.mountGateFor(ctx, rec.Definition.VolumeID)
		if err != nil {
			return out, err
		}
		if !found {
			// 卷已经不在受管理卷里了。那不是“挂着”，是一份指向不存在目标的共享：
			// 同样要撤销，理由要写清楚是登记表层面的丢失。
			gate = mountGate{
				State:      "not_managed",
				VolumeName: rec.Definition.VolumeID,
				Notices: []Blocker{{
					Code:    NoticeVolumeNotFound,
					Message: "共享 " + rec.Definition.Name + " 引用的卷已不在受管理卷里，无法确认它的挂载状态",
				}},
			}
		}
		if gate.Mounted {
			continue
		}
		offline = append(offline, pending{rec: rec, gate: gate})
	}
	if len(offline) == 0 {
		return out, nil
	}

	// 这些共享的段现在还在配置里吗？不在的话就没有可撤的东西，只剩登记表要补齐。
	present, err := e.managedSections(ctx)
	if err != nil {
		return out, err
	}
	inConfig := make([]pending, 0, len(offline))
	offlineNames := make([]string, 0, len(offline))
	for _, p := range offline {
		offlineNames = append(offlineNames, p.rec.Definition.Name)
		if present[strings.ToLower(p.rec.Definition.Name)] {
			inConfig = append(inConfig, p)
		}
	}

	for _, p := range offline {
		out.Revoked = append(out.Revoked, revokeRecord(p.rec, p.gate, now))
	}
	offlineIDs := make(map[string]bool, len(offline))
	for _, p := range offline {
		offlineIDs[p.rec.ID] = true
	}
	if len(inConfig) == 0 {
		// 配置里已经没有了（上一次撤销过，或者从来没写进去过）：只补登记表。
		// 不写文件、不重载——没有变化就没有写入，这条规则与人工应用是同一条。
		return out, e.reconcileExports(ctx, &out, records, offlineIDs, offline, now)
	}

	// 整份重写：renderManagedShares 会把所有离线的都跳过，不只是这几条。
	//
	// 但这里要先把“卷已经不在受管理卷里”的那几条从入参里去掉。渲染层遇到这种共享会
	// 直接报错（“拒绝只写一半”）——那条规则是为**人工应用**设的：应用 B 时不能顺手把
	// A 悄悄摘掉。撤销这一侧相反：我们正是要把它摘掉，并且会在报告和登记表里点名。
	// 若照原样传下去，一条悬空共享会让整轮对账失败，托管配置里那个指向空目录的段
	// 就会一直留着——那是这一层最不该发生的结果。
	renderable := make([]ShareRecord, 0, len(records))
	for _, rec := range records {
		if _, offline := offlineIDs[rec.ID]; offline {
			continue
		}
		renderable = append(renderable, rec)
	}
	desired, skipped, err := e.renderManagedShares(ctx, renderable)
	if err != nil {
		return out, err
	}
	out.Notes = append(out.Notes, Blocker{
		Code:    NoticeShareRevokedOffline,
		Message: "这些共享的卷当前没有挂载，它们的段已从托管配置里摘掉：" + strings.Join(offlineNames, "、"),
	})
	cfg, err := e.applyManagedConfig(ctx, desired, skipped, nil)
	if err != nil {
		return out, err
	}
	e.opts.Prober.Invalidate()
	out.ConfigWrites = true
	out.Reload = cfg.Report.Reload
	out.Service = cfg.Report.Service
	out.Served = cfg.Report.Served
	out.Notes = append(out.Notes, cfg.Report.Notes...)
	// SMB 已经处理完，导出这一半仍然要看一遍：同一次故障可能是两块互不相干的盘，
	// 也可能共享与导出各自绑着不同的卷。跳过它等于把 NFS 的暴露面留到下一个 30 秒。
	return out, e.reconcileExports(ctx, &out, records, offlineIDs, offline, now)
}

// reconcileExports 是运行时撤销的 NFS 那一半。
//
// 它单独一段而不是与 SMB 混在一起，因为两边的“在不在配置里”用的是完全不同的判据：
// 一边是段名，另一边是导出表里的目录行（而且只有带行尾标记的那些才算面板的）。
//
// 撤销只撤这一次观测到离线的：托管导出文件是整份重写的，所以“应用 A”会顺带把 B 的
// 行重写一遍——B 的卷要是此刻没挂，renderExports 会跳过它，这条跳过是安全的（少一份
// 导出比多一份指向空目录的导出好），报告里点名。
func (e *Engine) reconcileExports(ctx context.Context, out *ReconcileReport, records []ShareRecord, offlineIDs map[string]bool, offline []offlineShare, now time.Time) error {
	present, err := e.managedExportIDs()
	if err != nil {
		return err
	}
	var nfsOffline []offlineShare
	for _, p := range offline {
		if p.rec.Definition.Protocol != ProtocolNFS {
			continue
		}
		nfsOffline = append(nfsOffline, p)
	}
	if len(nfsOffline) > 0 {
		// 这些行现在还在托管导出文件里吗？不在就没有可撤的东西，只剩登记表要补齐。
		inFile := false
		for _, p := range nfsOffline {
			if present[p.rec.ID] {
				inFile = true
				break
			}
		}
		if !inFile {
			return e.saveRevoked(ctx, offline, now)
		}
		renderable := make([]ShareRecord, 0, len(records))
		for _, rec := range records {
			if offlineIDs[rec.ID] {
				continue
			}
			renderable = append(renderable, rec)
		}
		desired, skipped, err := e.renderExports(ctx, renderable)
		if err != nil {
			return err
		}
		cfg, err := e.applyExports(ctx, desired, skipped, nil)
		if err != nil {
			return err
		}
		e.opts.Prober.Invalidate()
		names := make([]string, 0, len(nfsOffline))
		for _, p := range nfsOffline {
			names = append(names, p.rec.Definition.Name)
		}
		out.Notes = append(out.Notes, Blocker{
			Code: NoticeShareRevokedOffline,
			Message: "这些 NFS 共享的卷当前没有挂载，它们的导出行已从 " + cfg.Report.Path +
				" 摘掉：" + strings.Join(names, "、"),
		})
		out.ExportsWritten = true
		out.ExportsReload = cfg.Report.Reload
		out.ExportsServed = cfg.Report.Served
		out.Notes = append(out.Notes, cfg.Report.Notes...)
	}
	return e.saveRevoked(ctx, offline, now)
}

// managedExportIDs 读托管导出文件里带行尾标记的共享 id。
//
// 文件不存在就是空集：还没写过导出的机器上没有任何东西可撤。
func (e *Engine) managedExportIDs() (map[string]bool, error) {
	data, err := e.fs().ReadFile(e.paths().Exports)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	entries, _ := ParseExports(data)
	out := make(map[string]bool, len(entries))
	for _, en := range entries {
		if en.Managed() {
			out[en.ManagedID] = true
		}
	}
	return out, nil
}

// offlineShare 是一条“卷不在挂载状态”的共享与它的闸门状态。
type offlineShare struct {
	rec  ShareRecord
	gate mountGate
}

// revokeRecord 把一条离线共享折算成报告与记录用的撤销现场。
func revokeRecord(rec ShareRecord, gate mountGate, at time.Time) RevokedShare {
	reason := "卷 " + gate.VolumeName + " 当前不在挂载状态（observed_state=" + gate.State + "）"
	if len(gate.Notices) > 0 {
		reason = gate.Notices[0].Message
	}
	return RevokedShare{
		ShareID: rec.ID,
		Name:    rec.Definition.Name,
		Revocation: RuntimeRevocation{
			At:          at,
			VolumeID:    rec.Definition.VolumeID,
			VolumeName:  gate.VolumeName,
			VolumeState: gate.State,
			Reason:      reason,
		},
	}
}

// managedSections 读托管文件里现存的段名（小写）。
//
// 文件不存在就是空集：还没写过配置的机器上没有任何东西可撤。
func (e *Engine) managedSections(ctx context.Context) (map[string]bool, error) {
	data, err := e.fs().ReadFile(e.paths().Managed)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	out := make(map[string]bool, 8)
	for _, name := range SectionNames(data) {
		out[strings.ToLower(name)] = true
	}
	return out, nil
}

// saveRevoked 把撤销写回登记表（ConfigApplied 归 false，记下现场）。
//
// 重复执行不产生变化：已经记过同一条卷状态的记录不再改 UpdatedAt，
// 否则每次对账都会把“最后更新时间”推到当下，那这个字段就不再说明任何事情。
//
// 已经处于撤销状态的共享**保留第一次撤销的现场**：第一次才是故障发生的时刻，
// 后面的对账只是在重复确认它还没回来。
func (e *Engine) saveRevoked(ctx context.Context, offline []offlineShare, at time.Time) error {
	for _, p := range offline {
		rec := p.rec
		rev := revokeRecord(rec, p.gate, at)
		changed := false
		if rec.ConfigApplied {
			rec.ConfigApplied = false
			changed = true
		}
		if rec.RuntimeRevoked == nil {
			r := rev.Revocation
			rec.RuntimeRevoked = &r
			changed = true
		}
		if !changed {
			continue
		}
		rec.UpdatedAt = at
		if err := e.opts.Store.SaveShare(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}
