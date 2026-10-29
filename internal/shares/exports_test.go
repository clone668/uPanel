package shares

// NFS 导出写入流水线的单测（P14.4）。
//
// 这一层要守的是四件在真机上很难看见、但会直接决定“客户端到底连不连得上”的事：
//   1. 文件写下去了不等于生效了：`exportfs -ra` 的退出码不是证据，`exportfs -v`
//      读回的实际导出表才是；
//   2. 内容没变也可能要重刷：有人手工 `exportfs -ua` 之后，内核里已经没有导出，
//      而文件一个字节没变——幂等重放必须能修好这种状态；
//   3. 刷表失败或“刷新成功但表还是旧的”都必须把文件退回去，不留下
//      “文件是新的、内核是旧的”这种最难查的半截状态；
//   4. 摘一行（停用 / 撤盘）与写一行同等重要：删不掉的导出行等于没删。
//
// 与 SMB 那边一样，这里不假装文件系统：导出文件、备份、rename、还原都真的发生在
// t.TempDir() 里，只有 exportfs / systemctl 是假程序。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"upanel/internal/storage"
)

const (
	testVol2  = "vol_22222222222222222222222222222222"
	testVol2D = "vol_33333333333333333333333333333333"
	// nfsDir 是 NFS 测试里那条共享的目录（卷挂载点 + 相对路径）。
	// 定义本身来自 nfs_test.go 的 nfsDefinition：两半（纯函数与流水线）用的是同一份定义，
	// 免得“纯函数算得对、流水线写出来却是另一条共享”这种偏差溜过去。
	nfsDir = testMount + "/media"
)

// nfsApply 走一遍完整链路：预检 → 执行。
func (h *harness) nfsApply(t *testing.T, def ShareDefinition, id, version *string) WriteReport {
	t.Helper()
	p := ShareApplyParams{Definition: &def, ShareID: id, Version: version}
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检不允许：%+v", a.Blockers)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_test_nfs",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	return report
}

// readExports 读托管导出文件的原文。
func (h *harness) readExports(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.cfg.Exports)
	if err != nil {
		t.Fatalf("读托管导出文件失败: %v", err)
	}
	return string(data)
}

// exportsGone 报告托管导出文件**不存在**（第一次应用失败后回滚的期望状态）。
func (h *harness) exportsGone(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(h.cfg.Exports)
	return os.IsNotExist(err)
}

// onlyNFSShare 取登记表里唯一那条共享并断言它是 NFS。
func (h *harness) onlyNFSShare(t *testing.T) ShareRecord {
	t.Helper()
	rec := h.onlyShare(t)
	if rec.Definition.Protocol != ProtocolNFS {
		t.Fatalf("记录协议是 %q，期望 nfs", rec.Definition.Protocol)
	}
	return rec
}

// ---------- 应用 ----------

// TestExecuteShareApplyNFSWritesExportsAndVerifies 校验应用一侧的完整结果。
func TestExecuteShareApplyNFSWritesExportsAndVerifies(t *testing.T) {
	h := newHarness(t)

	report := h.nfsApply(t, nfsDefinition(), nil, nil)

	// 导出表读回比对通过、nfs-server 在跑、这条共享自己的卷挂着——三条都成立才是 verified。
	if report.Outcome != OutcomeVerified {
		t.Fatalf("结果状态是 %q，期望 %q（observed=%v）", report.Outcome, OutcomeVerified, report.Observed)
	}
	if report.Observed["exports_file"] != ConfigFileWritten {
		t.Errorf("exports_file = %v，期望 %q", report.Observed["exports_file"], ConfigFileWritten)
	}
	if report.Observed["reload"] != ReloadDone {
		t.Errorf("reload = %v，期望 %q", report.Observed["reload"], ReloadDone)
	}
	if report.Observed["served"] != true {
		t.Errorf("served = %v，期望 true", report.Observed["served"])
	}
	// 读写共享的目录必须是 0777：root_squash 之后客户端 uid 原样透传，服务端事先
	// 不知道将来是谁连上来，访问控制只能交给网段（见 DECISIONS）。
	if got := report.Observed["directory_mode"]; got != "-rwxrwxrwx" {
		t.Errorf("directory_mode = %v，期望 -rwxrwxrwx", got)
	}
	if h.exportfs.reloadCalls != 1 {
		t.Errorf("exportfs -ra 被调了 %d 次，期望 1 次", h.exportfs.reloadCalls)
	}

	rec := h.onlyNFSShare(t)
	content := h.readExports(t)
	want := nfsDir + " 10.0.2.0/24(rw,sync,root_squash,no_subtree_check) # upanel:" + rec.ID
	if !strings.Contains(content, want) {
		t.Fatalf("托管导出文件里没有这一行：\n期望：%s\n实际：\n%s", want, content)
	}
	// 认领标记写在行尾，而不是单独一行：单行标记会被一次手工编辑弄错位。
	if len(strings.Split(strings.TrimSpace(content), "\n")) != 4 {
		t.Errorf("导出文件应该有 3 行注释 + 1 行导出，实际：\n%s", content)
	}
	if rec.ConfigApplied != true || rec.DirectoryPrepared != true {
		t.Errorf("记录状态不对：config_applied=%v directory_prepared=%v",
			rec.ConfigApplied, rec.DirectoryPrepared)
	}
	if rec.ConfigHash != report.Observed["config_hash"] {
		t.Errorf("记录里的配置哈希 %q 与任务里的 %v 不一致", rec.ConfigHash, report.Observed["config_hash"])
	}
}

// TestExecuteShareApplyNFSReadOnlyDirMode 校验只读共享两道保证都在：
// 导出选项是 ro，目录是 0755（客户端连写都写不进去，不必去猜 ro 有没有生效）。
func TestExecuteShareApplyNFSReadOnlyDirMode(t *testing.T) {
	h := newHarness(t)
	def := nfsDefinition()
	def.ReadOnly = true

	report := h.nfsApply(t, def, nil, nil)

	if got := report.Observed["directory_mode"]; got != "-rwxr-xr-x" {
		t.Errorf("directory_mode = %v，期望 -rwxr-xr-x", got)
	}
	content := h.readExports(t)
	if !strings.Contains(content, "10.0.2.0/24(ro,sync,root_squash,no_subtree_check)") {
		t.Errorf("导出选项不是 ro：\n%s", content)
	}
	if report.Outcome != OutcomeVerified {
		t.Fatalf("结果状态是 %q，期望 %q", report.Outcome, OutcomeVerified)
	}
}

// TestExecuteShareApplyNFSIdempotentReload 校验幂等重放：内容没变就不重刷导出表。
func TestExecuteShareApplyNFSIdempotentReload(t *testing.T) {
	h := newHarness(t)
	def := nfsDefinition()
	h.nfsApply(t, def, nil, nil)
	if h.exportfs.reloadCalls != 1 {
		t.Fatalf("第一次刷了 %d 次表，期望 1 次", h.exportfs.reloadCalls)
	}

	rec := h.onlyNFSShare(t)
	id, ver := rec.ID, rec.Version
	second := h.nfsApply(t, def, &id, &ver)

	if second.Observed["exports_file"] != ConfigFileUnchanged {
		t.Errorf("exports_file = %v，期望 %q", second.Observed["exports_file"], ConfigFileUnchanged)
	}
	if second.Observed["reload"] != ReloadSkippedUnchanged {
		t.Errorf("reload = %v，期望 %q", second.Observed["reload"], ReloadSkippedUnchanged)
	}
	if h.exportfs.reloadCalls != 1 {
		t.Errorf("幂等重放又刷了表：共 %d 次，期望 1 次", h.exportfs.reloadCalls)
	}
	if second.Outcome != OutcomeVerified {
		t.Errorf("结果状态是 %q，期望 %q", second.Outcome, OutcomeVerified)
	}
}

// TestExecuteShareApplyNFSRepairsClearedTable 校验“文件没变但内核里没了”能被修好。
//
// 真机上这是 `exportfs -ua`（或一次重启后 nfs-server 没起来）造成的状态：
// 托管导出文件一个字节没变，导出表却是空的。只看“内容变没变”就跳过刷表，
// 等于“再点一次应用”在最需要它的时候恰好什么都不做。
func TestExecuteShareApplyNFSRepairsClearedTable(t *testing.T) {
	h := newHarness(t)
	def := nfsDefinition()
	h.nfsApply(t, def, nil, nil)

	// 模拟有人手工清空了导出表：文件不动，表空了。
	h.exportfs.table = ExportTable{}

	rec := h.onlyNFSShare(t)
	id, ver := rec.ID, rec.Version
	second := h.nfsApply(t, def, &id, &ver)

	if second.Observed["exports_file"] != ConfigFileUnchanged {
		t.Errorf("exports_file = %v，期望 %q（文件确实没变）", second.Observed["exports_file"], ConfigFileUnchanged)
	}
	if h.exportfs.reloadCalls != 2 {
		t.Fatalf("exportfs -ra 被调了 %d 次，期望 2 次（表空了就必须重刷）", h.exportfs.reloadCalls)
	}
	if second.Outcome != OutcomeVerified {
		t.Errorf("结果状态是 %q，期望 %q（observed=%v）", second.Outcome, OutcomeVerified, second.Observed)
	}
	if _, ok := h.exportfs.table[nfsDir]; !ok {
		t.Error("导出表里仍然没有这条路径")
	}
}

// TestExecuteShareApplyNFSServiceInactiveAccepted 校验服务没在跑时只报 accepted。
//
// 导出写下去了、exportfs 也接受了，但 nfs-server 不在运行——客户端连不上，
// 这时说 verified 是在替一个没被验证的事实打包票。
func TestExecuteShareApplyNFSServiceInactiveAccepted(t *testing.T) {
	h := newHarness(t)
	h.serviceState = ServiceStateInactive

	report := h.nfsApply(t, nfsDefinition(), nil, nil)

	if report.Outcome != OutcomeAccepted {
		t.Fatalf("结果状态是 %q，期望 %q", report.Outcome, OutcomeAccepted)
	}
	if report.Observed["served"] != false {
		t.Errorf("served = %v，期望 false", report.Observed["served"])
	}
	if report.Observed["reload"] != ReloadSkippedInactive {
		t.Errorf("reload = %v，期望 %q", report.Observed["reload"], ReloadSkippedInactive)
	}
	if !strings.Contains(report.RecoveryHint, "nfs-server") {
		t.Errorf("恢复提示没有点出服务没起：%s", report.RecoveryHint)
	}
	// 文件还是写下去了：服务起来之后导出不需要重写。
	if _, err := os.Stat(h.cfg.Exports); err != nil {
		t.Fatalf("服务没在跑时导出文件也应该写下去：%v", err)
	}
}

// ---------- 失败回滚 ----------

// TestExecuteShareApplyNFSReloadFailureRollsBack 校验 exportfs -ra 失败时文件被退回。
func TestExecuteShareApplyNFSReloadFailureRollsBack(t *testing.T) {
	t.Run("第一次应用失败后不留文件", func(t *testing.T) {
		h := newHarness(t)
		h.exportfs.reloadErr = "exportfs: /etc/exports.d/upanel.exports: duplicated export entries"

		def := nfsDefinition()
		p := ShareApplyParams{Definition: &def}
		a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
			Action:               ActionShareApply,
			Params:               a.NormalizedParams,
			ExpectedIdentity:     a.IdentityFingerprint,
			ExpectedStateVersion: a.StateVersion,
		})
		if err == nil {
			t.Fatal("exportfs -ra 失败却报成功了")
		}
		if !strings.Contains(err.Error(), FailExportsReload) {
			t.Errorf("错误里没有 %s：%v", FailExportsReload, err)
		}
		if !strings.Contains(err.Error(), "duplicated export entries") {
			t.Errorf("错误里没有带上 exportfs 的原话：%v", err)
		}
		// 之前没有文件：回滚就是把它删掉，不留下“写着却没能生效”的导出。
		if !h.exportsGone(t) {
			t.Error("失败之后托管导出文件还在")
		}
	})

	t.Run("改一版失败后回到旧内容", func(t *testing.T) {
		h := newHarness(t)
		h.nfsApply(t, nfsDefinition(), nil, nil)
		before := h.readExports(t)

		rec := h.onlyNFSShare(t)
		h.exportfs.reloadErr = "exportfs: /srv/upanel/volumes/data/media: Invalid argument"

		def := nfsDefinition()
		def.ReadOnly = true
		id, ver := rec.ID, rec.Version
		p := ShareApplyParams{Definition: &def, ShareID: &id, Version: &ver}
		a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		if !a.Allowed {
			t.Fatalf("预检不允许：%+v", a.Blockers)
		}
		if _, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
			Action:               ActionShareApply,
			Params:               a.NormalizedParams,
			ExpectedIdentity:     a.IdentityFingerprint,
			ExpectedStateVersion: a.StateVersion,
		}); err == nil {
			t.Fatal("exportfs -ra 失败却报成功了")
		}
		if got := h.readExports(t); got != before {
			t.Fatalf("文件没有回到上一版：\n期望：\n%s\n实际：\n%s", before, got)
		}
	})
}

// TestExecuteShareApplyNFSStaleTableRollsBack 校验“刷新返回 0 但表还是旧的”。
//
// 这是最容易骗过实现的一种失败：命令成功、文件也对，只是内核没吃到。面板的处置是
// 读回核对不通过 → 报错 → 文件退回。少了这一步，页面会显示“已应用”，
// 而客户端连的仍然是上一版导出。
func TestExecuteShareApplyNFSStaleTableRollsBack(t *testing.T) {
	h := newHarness(t)
	h.exportfs.stale = true

	def := nfsDefinition()
	p := ShareApplyParams{Definition: &def}
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
	})
	if err == nil {
		t.Fatal("导出表没更新却报成功了")
	}
	if !strings.Contains(err.Error(), FailExportVerify) {
		t.Errorf("错误里没有 %s：%v", FailExportVerify, err)
	}
	if !h.exportsGone(t) {
		t.Error("失败之后托管导出文件还在")
	}
	if _, ok := h.exportfs.table[nfsDir]; ok {
		t.Error("导出表里居然有这条路径：假 exportfs 的 stale 没有生效")
	}
}

// ---------- 停用 ----------

// TestExecuteShareRefNFSDisableRemovesExportLine 校验停用把导出行摘掉并刷表。
func TestExecuteShareRefNFSDisableRemovesExportLine(t *testing.T) {
	h := newHarness(t)
	h.nfsApply(t, nfsDefinition(), nil, nil)
	rec := h.onlyNFSShare(t)

	p := ShareRefParams{ShareID: rec.ID, Version: rec.Version}
	a, err := h.engine.Assess(context.Background(), ActionShareDisable, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检不允许：%+v", a.Blockers)
	}
	// 预检不能说“ACL 会被清空”：NFS 没有 POSIX ACL 这一层。
	for _, im := range a.Impacts {
		if strings.Contains(im.Description, "ACL") {
			t.Errorf("NFS 的影响面里出现了 ACL：%s", im.Description)
		}
	}
	// 锁必须是导出文件那把：取 samba:config 管不住整份重写。
	if !hasLock(a.Locks, ExportsLockName) {
		t.Errorf("锁是 %v，期望含 %q", a.Locks, ExportsLockName)
	}

	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareDisable,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if report.Outcome != OutcomeAccepted {
		t.Errorf("结果状态是 %q，期望 %q", report.Outcome, OutcomeAccepted)
	}
	if report.Observed["acl_revoked"] != false {
		t.Errorf("acl_revoked = %v，期望 false（NFS 没有 ACL 可收）", report.Observed["acl_revoked"])
	}
	if strings.Contains(h.readExports(t), nfsDir) {
		t.Errorf("停用之后导出行还在：\n%s", h.readExports(t))
	}
	if _, ok := h.exportfs.table[nfsDir]; ok {
		t.Error("导出表里还留着这条路径")
	}

	after := h.onlyNFSShare(t)
	if after.Definition.Enabled || after.ConfigApplied {
		t.Errorf("记录状态不对：enabled=%v config_applied=%v",
			after.Definition.Enabled, after.ConfigApplied)
	}
}

// TestExecuteShareApplyNFSRollsBackExportsWhenStoreFails 校验“导出生效了但落库失败”时
// 导出行必须被摘回去。
//
// 这一条是 22.04 验收 VM 抓出来的（P14.4）：协议列的 CHECK 还停在只允许 smb 的版本，
// ShareSave 失败，而托管导出文件与内核导出表里已经留下了那一行——**系统上真的在导出一个
// 面板数据库里不存在的共享**。任务终态是 failed，页面说“没成功”，客户端却能连上。
// 那种状态比“什么都没做”更糟：它没有任何一条记录能被撤、被停用、被解释。
func TestExecuteShareApplyNFSRollsBackExportsWhenStoreFails(t *testing.T) {
	h := newHarness(t)
	h.store.saveErr = errors.New("constraint failed: CHECK constraint failed: protocol IN ('smb')")

	def := nfsDefinition()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, ShareApplyParams{Definition: &def})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检不允许：%+v", a.Blockers)
	}
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_test_nfs_store_fail",
	})
	if err == nil {
		t.Fatal("落库失败却报成功了")
	}
	if !strings.Contains(err.Error(), "保存共享记录失败") {
		t.Fatalf("错误里没讲清失败在哪一步：%v", err)
	}
	// 导出文件必须回到“没有这一行”：第一次应用失败就该回到没有文件。
	if !h.exportsGone(t) && strings.Contains(h.readExports(t), nfsDir) {
		t.Fatalf("落库失败之后导出行还留在文件里：\n%s", h.readExports(t))
	}
	// 内核里也不能还留着：还原之后必须再刷一次表。
	if _, ok := h.exportfs.table[nfsDir]; ok {
		t.Error("落库失败之后导出表里还留着这条路径")
	}
	if len(h.store.shares) != 0 {
		t.Errorf("落库失败却留下了 %d 条记录", len(h.store.shares))
	}
}

// TestExecuteShareRefNFSRestoresLineWhenDeleteFails 是上一条的反方向：
// 停用/删除把导出行摘掉了，但记录改不动时，那一行要放回去。
//
// 不放回去，库里就有一条“启用中、却没在导出”的共享：下一次对账或重新应用会把它当成
// 新东西再写一遍，而人看到的是一次失败。
func TestExecuteShareRefNFSRestoresLineWhenDeleteFails(t *testing.T) {
	h := newHarness(t)
	h.nfsApply(t, nfsDefinition(), nil, nil)
	rec := h.onlyNFSShare(t)
	h.store.deleteErr = errors.New("agentstore: 数据库已关闭")

	p := ShareRefParams{ShareID: rec.ID, Version: rec.Version}
	a, err := h.engine.Assess(context.Background(), ActionShareDelete, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检不允许：%+v", a.Blockers)
	}
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareDelete,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_test_nfs_delete_fail",
	})
	if err == nil {
		t.Fatal("删记录失败却报成功了")
	}
	if !strings.Contains(err.Error(), "更新共享记录失败") {
		t.Fatalf("错误里没讲清失败在哪一步：%v", err)
	}
	if !strings.Contains(h.readExports(t), nfsDir) {
		t.Errorf("删记录失败之后导出行没有放回去：\n%s", h.readExports(t))
	}
	if _, ok := h.exportfs.table[nfsDir]; !ok {
		t.Error("删记录失败之后导出表里没有这条路径了——记录还在启用，客户端却连不上")
	}
	if _, found, _ := h.store.GetShare(context.Background(), rec.ID); !found {
		t.Error("删记录失败，记录却不见了")
	}
}

// TestAssessShareRefNFSBlocksWithoutExportfs 校验缺 exportfs 时停用被阻塞。
//
// 没有它就只能改文件、刷不了表，客户端还按旧导出连着——那不是“部分成功”，
// 是“看起来撤销了、实际没撤”。
func TestAssessShareRefNFSBlocksWithoutExportfs(t *testing.T) {
	h := newHarness(t)
	h.nfsApply(t, nfsDefinition(), nil, nil)
	rec := h.onlyNFSShare(t)
	h.exportfsBroken(t)

	p := ShareRefParams{ShareID: rec.ID, Version: rec.Version}
	a, err := h.engine.Assess(context.Background(), ActionShareDisable, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("缺 exportfs 却允许了停用")
	}
	if findBlocker(a.Blockers, NoticeNFSUnavailable) == nil {
		t.Fatalf("没有 %s：%v", NoticeNFSUnavailable, a.Blockers)
	}
}

// ---------- 运行时撤盘 ----------

// TestReconcileVolumesRevokesOfflineNFSExport 校验脱盘后导出行被摘掉。
func TestReconcileVolumesRevokesOfflineNFSExport(t *testing.T) {
	h := newHarness(t)
	h.nfsApply(t, nfsDefinition(), nil, nil)
	if !strings.Contains(h.readExports(t), nfsDir) {
		t.Fatal("准备阶段没有写出导出行")
	}

	h.setVolumeState(storage.VolumeStateUnmounted)
	rep, err := h.engine.ReconcileVolumes(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if !rep.ExportsWritten {
		t.Fatalf("没有报告重写托管导出文件：%+v", rep)
	}
	if len(rep.Revoked) != 1 || rep.Revoked[0].Name != "media" {
		t.Fatalf("Revoked = %+v", rep.Revoked)
	}
	if strings.Contains(h.readExports(t), nfsDir) {
		t.Errorf("脱盘之后导出行还在：\n%s", h.readExports(t))
	}
	if _, ok := h.exportfs.table[nfsDir]; ok {
		t.Error("导出表里还留着这条路径")
	}
	if !hasBlockerCode(rep.Notes, NoticeShareRevokedOffline) {
		t.Errorf("没有点名撤销了哪条：%v", rep.Notes)
	}
}

// ---------- 预检 ----------

// TestAssessShareApplyNFSLocksAndWarnings 校验 NFS 预检的锁与固定告警。
func TestAssessShareApplyNFSLocksAndWarnings(t *testing.T) {
	h := newHarness(t)
	def := nfsDefinition()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, ShareApplyParams{Definition: &def})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("应该允许，阻塞项：%v", a.Blockers)
	}
	// 锁必须是导出文件那把，而不是 samba:config：两者写的是不同文件，
	// 混用只会让“改一份 NFS 共享”白等一次 SMB 应用。
	want := []string{ExportsLockName, "share:media", "volume:" + testVolID}
	if len(a.Locks) != len(want) {
		t.Fatalf("锁是 %v，期望 %v", a.Locks, want)
	}
	for i := range want {
		if a.Locks[i] != want[i] {
			t.Fatalf("锁是 %v，期望 %v", a.Locks, want)
		}
	}
	// 从未应用过的机器：导出表里没有这条路径。
	if !hasBlockerCode(a.Warnings, NoticeExportsNotApplied) {
		t.Errorf("缺少 %s 告警：%v", NoticeExportsNotApplied, a.Warnings)
	}
	// Samba 那条固定告警不该出现在 NFS 上：它与 smb.conf 有关。
	if hasBlockerCode(a.Warnings, NoticeShareConfigNotApplied) {
		t.Errorf("NFS 预检里出现了 SMB 的告警：%v", a.Warnings)
	}
}

// TestAssessShareApplyNFSBlocksPathConflict 校验同路径被别的导出文件占用时被阻塞。
func TestAssessShareApplyNFSBlocksPathConflict(t *testing.T) {
	h := newHarness(t)
	// 别人的导出文件（面板不碰它，只读它）。
	if err := os.MkdirAll(h.cfg.ExportsDir, 0o755); err != nil {
		t.Fatalf("造导出目录失败: %v", err)
	}
	foreign := nfsDir + " 10.0.3.0/24(rw,sync)\n"
	if err := os.WriteFile(filepath.Join(h.cfg.ExportsDir, "other.exports"), []byte(foreign), 0o644); err != nil {
		t.Fatalf("写别人的导出文件失败: %v", err)
	}

	def := nfsDefinition()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, ShareApplyParams{Definition: &def})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("同一路径已经被别人导出却允许了：exportfs 整份刷新会因此失败")
	}
	b := findBlocker(a.Blockers, NoticeExportPathConflict)
	if b == nil {
		t.Fatalf("没有 %s：%v", NoticeExportPathConflict, a.Blockers)
	}
	if !strings.Contains(b.Message, nfsDir) {
		t.Errorf("冲突说明没有点出是哪条路径：%s", b.Message)
	}
}

// TestAssessShareApplyNFSBlocksSameDirAsAnotherShare 校验面板自己的两条共享指着
// 同一个目录时，预检就把它拦下。
//
// 这与“撞上别人的导出文件”是同一个危害的两半：两条共享各渲染一行，同一个目录
// 被导出两次，exportfs 报 duplicated export entries 并以 1 退出——整份导出表
// 刷新失败，别人原本好用的导出也一起停摆。
func TestAssessShareApplyNFSBlocksSameDirAsAnotherShare(t *testing.T) {
	h := newHarness(t)
	// 先真跑一条：登记表里有一条指向卷根目录的共享（相对路径为空）。
	first := nfsDefinition()
	first.RelativePath = ""
	h.nfsApply(t, first, nil, nil)

	def := nfsDefinition()
	def.Name = "same-path"
	def.RelativePath = ""
	a, err := h.engine.Assess(context.Background(), ActionShareApply, ShareApplyParams{Definition: &def})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("同一个目录已经被另一条共享导出，却允许了：exportfs 整份刷新会因此失败")
	}
	b := findBlocker(a.Blockers, NoticeExportPathConflict)
	if b == nil {
		t.Fatalf("没有 %s：%v", NoticeExportPathConflict, a.Blockers)
	}
	for _, want := range []string{testMount, "media", "same-path"} {
		if !strings.Contains(b.Message, want) {
			t.Errorf("冲突说明里缺少 %q：%s", want, b.Message)
		}
	}
}

// TestApplyExportsRefusesDuplicateDirs 直接打执行层：同一目录的两条导出一条都不写。
//
// 预检拦过一次，执行期为什么还要拦：登记表在预检之后也可能被别的请求改。这是纵深
// 防御，判据必须是同一个函数（SameDirShares），否则两处迟早说不一样的话。
func TestApplyExportsRefusesDuplicateDirs(t *testing.T) {
	h := newHarness(t)
	id := func(n string) string { return "shr_" + strings.Repeat(n, 32) }
	mk := func(name, shareID string) ManagedExport {
		e := ManagedExport{
			ShareID: shareID,
			Name:    name,
			Dir:     nfsDir,
			CIDRs:   []string{"10.0.2.0/24"},
		}
		e.Line = mustLine(t, e)
		return e
	}
	dups := []ManagedExport{mk("a", id("1")), mk("b", id("2"))}

	if _, err := h.engine.applyExports(context.Background(), dups, nil, nil); err == nil {
		t.Fatal("同一目录的两条导出被执行层接受了")
	} else if !strings.Contains(err.Error(), NoticeExportPathConflict) {
		t.Errorf("错误里没有点名 %s：%v", NoticeExportPathConflict, err)
	}
	if !h.exportsGone(t) {
		t.Errorf("冲突时不该写出导出文件：%s", h.readExports(t))
	}
}

// TestExecuteShareApplyNFSSkipsOfflineVolume 校验另一条共享的卷没挂时它的行被跳过并点名。
//
// 整份重写意味着“应用 A”会顺带重写 B 的行。B 的卷此刻没挂时跳过它是安全的
// （少一份指向空目录的导出比多一份好），但必须在结果里点名，否则 B 的导出会
// 在某次无关的应用之后悄悄消失。
func TestExecuteShareApplyNFSSkipsOfflineVolume(t *testing.T) {
	h := newHarness(t)
	h.vols.vols[testVol2] = storage.Volume{
		ID:             testVol2,
		Version:        "1",
		DisplayName:    "备份卷",
		Slug:           "backup",
		FilesystemUUID: testVol2D,
		FilesystemType: storage.FilesystemExt4,
		MountPath:      "/srv/upanel/volumes/backup",
		DesiredState:   storage.DesiredMounted,
		ObservedState:  storage.VolumeStateUnmounted,
		ObservedError:  "挂载点下没有挂载任何文件系统",
	}
	h.store.putShare(ShareRecord{
		ID: testShareI,
		Definition: ShareDefinition{
			Name:         "archive",
			Protocol:     ProtocolNFS,
			VolumeID:     testVol2,
			RelativePath: "archive",
			Enabled:      true,
			AllowedCIDRs: []string{"10.0.2.0/24"},
			NFSOptions:   &NFSOptions{RootSquash: true, Sync: true},
		},
		Version:           "1",
		ConfigApplied:     true,
		DirectoryPrepared: true,
		CreatedAt:         h.now,
		UpdatedAt:         h.now,
	})

	report := h.nfsApply(t, nfsDefinition(), nil, nil)

	offline, _ := report.Data["volume_offline"].([]string)
	if len(offline) != 1 || offline[0] != "archive" {
		t.Fatalf("volume_offline = %v，期望 [archive]", report.Data["volume_offline"])
	}
	notes, ok := report.Observed["notes"].([]map[string]any)
	if !ok {
		t.Fatalf("notes 不是预期的形状：%T", report.Observed["notes"])
	}
	if !hasNote(notes, NoticeVolumeOffline) {
		t.Errorf("没有点名被跳过的共享：%v", notes)
	}
	content := h.readExports(t)
	if strings.Contains(content, "/srv/upanel/volumes/backup/archive") {
		t.Errorf("卷没挂的共享被写进了导出表：\n%s", content)
	}
	if !strings.Contains(content, nfsDir) {
		t.Errorf("这条共享自己的行应该写进去：\n%s", content)
	}
}

// ---------- 小工具 ----------

// hasLock 报告锁列表里有没有某把锁。
func hasLock(locks []string, name string) bool {
	for _, l := range locks {
		if l == name {
			return true
		}
	}
	return false
}

// hasNote 报告摊平成 map 的 notes 里有没有某个码。
func hasNote(notes []map[string]any, code string) bool {
	for _, n := range notes {
		if n["code"] == code {
			return true
		}
	}
	return false
}

// exportfsBroken 让本机“没有 exportfs”（模拟 nfs-kernel-server 被卸载）。
func (h *harness) exportfsBroken(t *testing.T) {
	t.Helper()
	broken := testTools
	broken.Exportfs = ""
	h.engine.opts.Prober.opts.Paths = broken
	h.engine.opts.Prober.Invalidate()
}
