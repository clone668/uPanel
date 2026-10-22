package shares

// P08.3：运行时脱盘。
//
// 三条断言构成这一层的行为契约：
//   - 卷掉线 → 段从托管配置里消失、记录里留下现场；
//   - 反复对账不产生新的写入（幂等，且保留第一次撤销的时间）；
//   - 卷回来**不**自动恢复（撤销是单向的，写配置要人点头）。

import (
	"context"
	"strings"
	"testing"

	"upanel/internal/storage"
)

// appliedShare 造一个“已经应用过一份共享”的小机器，返回 harness 与记录 ID。
func appliedShare(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)
	if _, err := h.applyOnce(t); err != nil {
		t.Fatalf("准备一次成功的应用失败: %v", err)
	}
	if !strings.Contains(h.readManaged(), "["+baseDefinition().Name+"]") {
		t.Fatalf("准备阶段没有写出配置段：\n%s", h.readManaged())
	}
	return h
}

// setVolumeState 改卷的观测状态（模拟脱盘 / 挂回）。
func (h *harness) setVolumeState(state string) {
	v := h.vols.vols[testVolID]
	v.ObservedState = state
	h.vols.vols[testVolID] = v
}

// onlyShare 取登记表里唯一那条共享（share.apply 会自己生成 ID）。
func (h *harness) onlyShare(t *testing.T) ShareRecord {
	t.Helper()
	recs, err := h.store.ListShares(context.Background())
	if err != nil {
		t.Fatalf("读登记失败: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("登记表里有 %d 条共享，期望 1", len(recs))
	}
	return recs[0]
}

// TestReconcileVolumesRevokesOfflineShare 校验脱盘后共享被撤销。
func TestReconcileVolumesRevokesOfflineShare(t *testing.T) {
	h := appliedShare(t)
	h.setVolumeState(storage.VolumeStateUnmounted)
	reloadsBefore := h.reloads

	rep, err := h.engine.ReconcileVolumes(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Checked != 1 {
		t.Errorf("Checked = %d，期望 1", rep.Checked)
	}
	if len(rep.Revoked) != 1 || rep.Revoked[0].Name != baseDefinition().Name {
		t.Fatalf("Revoked = %+v", rep.Revoked)
	}
	if rev := rep.Revoked[0].Revocation; rev.VolumeState != storage.VolumeStateUnmounted ||
		rev.VolumeID != testVolID {
		t.Errorf("撤销现场 = %+v", rev)
	}
	if !rep.ConfigWrites {
		t.Error("没有报告重写托管配置")
	}
	if got := h.readManaged(); strings.Contains(got, "["+baseDefinition().Name+"]") {
		t.Errorf("段还在托管配置里：\n%s", got)
	}
	if h.reloads != reloadsBefore+1 {
		t.Errorf("smbcontrol 重载 %d 次，期望多一次", h.reloads-reloadsBefore)
	}

	rec := h.onlyShare(t)
	if rec.ConfigApplied {
		t.Error("撤销之后 config_applied 仍然是 true")
	}
	if rec.RuntimeRevoked == nil {
		t.Fatal("没有记下运行时撤销")
	}
	if rec.RuntimeRevoked.VolumeState != storage.VolumeStateUnmounted {
		t.Errorf("记录里的卷状态 = %q", rec.RuntimeRevoked.VolumeState)
	}
}

// TestReconcileVolumesIsIdempotent 校验反复对账不产生新的写入，且保留第一次撤销的现场。
func TestReconcileVolumesIsIdempotent(t *testing.T) {
	h := appliedShare(t)
	h.setVolumeState(storage.VolumeStateUnmounted)

	first, err := h.engine.ReconcileVolumes(context.Background())
	if err != nil {
		t.Fatalf("第一次对账失败: %v", err)
	}
	recAfterFirst := h.onlyShare(t)
	reloadsAfterFirst := h.reloads
	managedAfterFirst := h.readManaged()

	second, err := h.engine.ReconcileVolumes(context.Background())
	if err != nil {
		t.Fatalf("第二次对账失败: %v", err)
	}
	// 卷仍然离线，所以“撤销事实”照样报告；但没有东西可写。
	if len(second.Revoked) != 1 {
		t.Errorf("第二次 Revoked = %+v", second.Revoked)
	}
	if second.ConfigWrites {
		t.Error("第二次对账没有变化，却报告重写了配置")
	}
	if h.reloads != reloadsAfterFirst {
		t.Errorf("第二次对账多调用了 %d 次重载", h.reloads-reloadsAfterFirst)
	}
	if h.readManaged() != managedAfterFirst {
		t.Error("第二次对账改了托管文件")
	}
	recAfterSecond := h.onlyShare(t)
	if recAfterSecond.UpdatedAt != recAfterFirst.UpdatedAt {
		t.Errorf("第二次对账改了 UpdatedAt：%v → %v",
			recAfterFirst.UpdatedAt, recAfterSecond.UpdatedAt)
	}
	if !recAfterSecond.RuntimeRevoked.At.Equal(first.Revoked[0].Revocation.At) {
		t.Errorf("记录里的撤销时间被覆盖：%v", recAfterSecond.RuntimeRevoked.At)
	}
}

// TestReconcileVolumesDoesNotRestore 校验卷回来后共享不会自动写回配置。
//
// 这条是整个 P08.3 最重要的一条：撤销是安全的单向动作。自动恢复会把一次外部故障
// 变成一次静默的配置变更，而那正是这个面板最不该有的东西。
func TestReconcileVolumesDoesNotRestore(t *testing.T) {
	h := appliedShare(t)
	h.setVolumeState(storage.VolumeStateUnmounted)
	if _, err := h.engine.ReconcileVolumes(context.Background()); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	reloadsAfterRevoke := h.reloads

	// 卷挂回来了。
	h.setVolumeState(storage.VolumeStateMounted)
	rep, err := h.engine.ReconcileVolumes(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if len(rep.Revoked) != 0 {
		t.Errorf("卷已经挂回来，却仍报告撤销：%+v", rep.Revoked)
	}
	if rep.ConfigWrites {
		t.Error("卷挂回来时自动重写了配置")
	}
	if h.reloads != reloadsAfterRevoke {
		t.Errorf("卷挂回来时触发了 %d 次重载", h.reloads-reloadsAfterRevoke)
	}
	if strings.Contains(h.readManaged(), "["+baseDefinition().Name+"]") {
		t.Error("段被自动恢复了：卷回来之后必须由人重新应用")
	}
	rec := h.onlyShare(t)
	if rec.ConfigApplied {
		t.Error("没有重新应用，config_applied 却变回了 true")
	}
	if rec.RuntimeRevoked == nil {
		t.Error("撤销记录应当保留下来（它说明这条共享需要重新应用）")
	}
}

// TestReconcileVolumesIgnoresDisabledShares 校验禁用中的共享不进入对账范围。
func TestReconcileVolumesIgnoresDisabledShares(t *testing.T) {
	h := newHarness(t)
	def := baseDefinition()
	def.Enabled = false
	h.store.putShare(ShareRecord{ID: testShareI, Definition: def, Version: "1",
		CreatedAt: h.now, UpdatedAt: h.now})
	h.setVolumeState(storage.VolumeStateUnmounted)

	rep, err := h.engine.ReconcileVolumes(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Checked != 0 {
		t.Errorf("Checked = %d，禁用中的共享不该被检查", rep.Checked)
	}
	if len(rep.Revoked) != 0 || rep.ConfigWrites {
		t.Errorf("禁用中的共享触发了撤销：%+v", rep)
	}
}

// TestReconcileVolumesHandlesUnmanagedVolume 校验卷已不在受管理卷里时也撤销，
// 并且把理由写成“登记表层面的丢失”，而不是含糊的“离线”。
func TestReconcileVolumesHandlesUnmanagedVolume(t *testing.T) {
	h := appliedShare(t)
	delete(h.vols.vols, testVolID)

	rep, err := h.engine.ReconcileVolumes(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if len(rep.Revoked) != 1 {
		t.Fatalf("Revoked = %+v", rep.Revoked)
	}
	if rep.Revoked[0].Revocation.VolumeState != "not_managed" {
		t.Errorf("卷状态 = %q，期望 not_managed", rep.Revoked[0].Revocation.VolumeState)
	}
	if !strings.Contains(rep.Revoked[0].Revocation.Reason, "不在受管理卷里") {
		t.Errorf("理由没说清楚：%q", rep.Revoked[0].Revocation.Reason)
	}
}
