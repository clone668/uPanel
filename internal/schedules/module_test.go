package schedules

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStore 是计划仓储的内存替身。
type fakeStore struct {
	recs      map[string]Record
	order     []string
	seq       int
	trigs     map[string]TriggerRecord // key: scheduleID + "|" + slot
	err       error
	createErr error
	removeErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{recs: map[string]Record{}, trigs: map[string]TriggerRecord{}}
}

func (s *fakeStore) List(context.Context) ([]Record, error) {
	out := make([]Record, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.recs[id])
	}
	return out, nil
}

func (s *fakeStore) Get(_ context.Context, id string) (Record, bool, error) {
	if s.err != nil {
		return Record{}, false, s.err
	}
	rec, ok := s.recs[id]
	return rec, ok, nil
}

func (s *fakeStore) GetByTarget(_ context.Context, kind Kind, targetID string) (Record, bool, error) {
	for _, rec := range s.recs {
		if rec.Kind == kind && rec.TargetID == targetID {
			return rec, true, nil
		}
	}
	return Record{}, false, nil
}

func (s *fakeStore) Create(_ context.Context, def Definition, now time.Time) (Record, error) {
	if s.createErr != nil {
		return Record{}, s.createErr
	}
	s.seq++
	id := "sch_" + strings.Repeat("0", 31) + string(rune('a'+s.seq-1))
	rec := Record{ID: id, Name: def.Name, Kind: def.Kind, TargetID: def.TargetID,
		Definition: def, DefinitionKey: keyOf(def), Enabled: def.Enabled, Version: 1,
		CreatedAt: now, UpdatedAt: now}
	s.recs[id] = rec
	s.order = append(s.order, id)
	return rec, nil
}

func (s *fakeStore) Update(_ context.Context, id string, def Definition, expected int64, now time.Time) (Record, error) {
	rec, ok := s.recs[id]
	if !ok {
		return Record{}, errors.New("计划不存在")
	}
	if rec.Version != expected {
		return Record{}, errors.New("版本冲突")
	}
	rec.Name, rec.Kind, rec.TargetID = def.Name, def.Kind, def.TargetID
	rec.Definition, rec.DefinitionKey = def, keyOf(def)
	rec.Enabled = def.Enabled
	rec.Version++
	rec.UpdatedAt = now
	s.recs[id] = rec
	return rec, nil
}

func (s *fakeStore) Delete(_ context.Context, id string, expected int64) error {
	rec, ok := s.recs[id]
	if !ok {
		return nil
	}
	if rec.Version != expected {
		return errors.New("版本冲突")
	}
	delete(s.recs, id)
	return nil
}

func (s *fakeStore) SetTimerRef(_ context.Context, id, ref string, now time.Time) (Record, error) {
	rec, ok := s.recs[id]
	if !ok {
		return Record{}, errors.New("计划不存在")
	}
	rec.TimerRef = ref
	rec.UpdatedAt = now
	s.recs[id] = rec
	return rec, nil
}

func (s *fakeStore) RecordTrigger(_ context.Context, rec TriggerRecord) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	key := rec.ScheduleID + "|" + rec.SlotAt.Format(time.RFC3339)
	if existing, ok := s.trigs[key]; ok {
		return existing.OperationID, true, nil
	}
	s.trigs[key] = rec
	return rec.OperationID, false, nil
}

func (s *fakeStore) ListTriggers(_ context.Context, scheduleID string, _ int) ([]TriggerRecord, error) {
	var out []TriggerRecord
	for _, rec := range s.trigs {
		if rec.ScheduleID == scheduleID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func keyOf(def Definition) string { return def.Name + def.CalendarExpression + def.Timezone }

// fakeUnits 记录单元调用。
type fakeUnits struct {
	calErr    error
	ensureErr error
	removeErr error
	status    Status
	ensured   []string
	removed   []string
	lastDef   Definition
	calCalls  int
}

func (u *fakeUnits) ValidateCalendar(_ context.Context, d Definition) (CalendarCheck, error) {
	u.calCalls++
	u.lastDef = d
	if u.calErr != nil {
		return CalendarCheck{}, u.calErr
	}
	return CalendarCheck{Normalized: CalendarLine(d),
		NextElapse: time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)}, nil
}

func (u *fakeUnits) Ensure(_ context.Context, id string, d Definition) (Status, error) {
	u.ensured = append(u.ensured, id)
	u.lastDef = d
	if u.ensureErr != nil {
		return Status{}, u.ensureErr
	}
	if u.status.UnitFileState != "" {
		return u.status, nil
	}
	return Status{TimerPath: "/etc/systemd/system/" + TimerName(id), Present: true,
		UnitFileState: "enabled", ActiveState: "active"}, nil
}

func (u *fakeUnits) Remove(_ context.Context, id string) error {
	u.removed = append(u.removed, id)
	return u.removeErr
}

func (u *fakeUnits) Status(_ context.Context, id string) (Status, error) {
	st := u.status
	if st.TimerPath == "" {
		st.TimerPath = "/etc/systemd/system/" + TimerName(id)
	}
	return st, nil
}

func testDef() Definition {
	return Definition{
		Name:               "每晚备份",
		Kind:               KindBackup,
		TargetID:           "pol_0123456789abcdef0123456789abcdef",
		CalendarExpression: "*-*-* 03:30:00",
		Timezone:           "Asia/Shanghai",
		Persistent:         true,
		MaxRuntimeSeconds:  3600,
		Enabled:            true,
	}
}

func newHarness(t *testing.T) (*Module, *fakeStore, *fakeUnits, *time.Time) {
	t.Helper()
	store := newFakeStore()
	units := &fakeUnits{status: Status{Present: true, UnitFileState: "enabled", ActiveState: "active"}}
	now := time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC)
	mod, err := NewModule(ModuleOptions{
		Store: store, Units: units,
		Lookup: func(_ context.Context, def Definition) (Target, bool, error) {
			switch def.Kind {
			case KindBackup:
				if def.TargetID == "pol_0123456789abcdef0123456789abcdef" {
					return Target{Name: "每晚备份策略", Version: 7, Ready: true}, true, nil
				}
			case KindUserScript:
				if def.TargetID == "scr_0123456789abcdef0123456789abcdef" {
					return Target{Name: "清理脚本", Version: 3, Ready: true}, true, nil
				}
			case KindCertificateRenewal:
				// 证书的目标是盘上那张 live 目录；harness 里假装它在。
				if def.Certificate != nil {
					return Target{Name: def.Certificate.Domain, Version: 1, Ready: true}, true, nil
				}
			}
			return Target{}, false, nil
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	return mod, store, units, &now
}

func TestAssessApplyChecksSystemdBeforeSaving(t *testing.T) {
	mod, _, units, _ := newHarness(t)

	out, err := mod.AssessApply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("AssessApply: %v", err)
	}
	if !out.Allowed {
		t.Fatalf("合法定义应当放行: %v", out.Blockers)
	}
	if out.NextElapse == nil {
		t.Fatal("预检就要给出下次触发时刻")
	}
	if units.calCalls != 1 {
		t.Fatalf("没有问 systemd（调用 %d 次）", units.calCalls)
	}
}

func TestAssessApplyBlockers(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Definition)
		code string
	}{
		{"日历被 systemd 拒", func(d *Definition) { d.Timezone = "Asia/Shanghai" }, ""},
		{"目标不存在", func(d *Definition) { d.TargetID = "pol_ffffffffffffffffffffffffffffffff" }, "SCHEDULE_TARGET_NOT_FOUND"},
		{"定义非法", func(d *Definition) { d.Name = "" }, "SCHEDULE_INVALID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod, _, units, _ := newHarness(t)
			def := testDef()
			c.mut(&def)
			if c.name == "日历被 systemd 拒" {
				units.calErr = errors.New("systemd 不认这个日历表达式")
				c.code = "SCHEDULE_CALENDAR_INVALID"
			}
			out, err := mod.AssessApply(context.Background(), ApplyRequest{Definition: def})
			if err != nil {
				t.Fatalf("AssessApply: %v", err)
			}
			if out.Allowed {
				t.Fatal("应当被拦下")
			}
			found := false
			for _, b := range out.Blockers {
				if b.Code == c.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("阻塞项 = %v, 期望含 %s", out.Blockers, c.code)
			}
		})
	}
}

// TestAssessApplyAllowsCertificateRenewal：P12.5 起证书续期计划可以保存——
// 种类参数齐、目标（盘上的 live 目录）在，就没有阻塞项。
func TestAssessApplyAllowsCertificateRenewal(t *testing.T) {
	mod, _, _, _ := newHarness(t)
	def := testDef()
	def.Kind = KindCertificateRenewal
	def.TargetID = "crt_0123456789abcdef0123456789abcdef"
	def.Certificate = &CertificateParams{Domain: "panel.example.com"}
	out, err := mod.AssessApply(context.Background(), ApplyRequest{Definition: def})
	if err != nil {
		t.Fatalf("AssessApply: %v", err)
	}
	if !out.Allowed {
		t.Fatalf("证书续期计划应当可以保存，阻塞项 = %+v", out.Blockers)
	}
	for _, b := range out.Blockers {
		if b.Code == "SCHEDULE_KIND_UNSUPPORTED" {
			t.Fatal("P12.4 的“未接线”闸必须已经拆掉")
		}
	}
}

// TestAssessApplyBlocksCertificateRenewalWhenCertMissing：证书不在盘上时
// 保存被拦下，理由原样来自目标解析（代理侧的 NOT_READY 说法）。
func TestAssessApplyBlocksCertificateRenewalWhenCertMissing(t *testing.T) {
	store := newFakeStore()
	units := &fakeUnits{status: Status{Present: true, UnitFileState: "enabled", ActiveState: "active"}}
	mod, err := NewModule(ModuleOptions{
		Store: store, Units: units,
		Lookup: func(_ context.Context, def Definition) (Target, bool, error) {
			return Target{Name: def.Certificate.Domain, Version: 1,
				Note: "盘上没有 /etc/letsencrypt/live/panel.example.com"}, true, nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	def := testDef()
	def.Kind = KindCertificateRenewal
	def.TargetID = "crt_0123456789abcdef0123456789abcdef"
	def.Certificate = &CertificateParams{Domain: "panel.example.com"}
	out, err := mod.AssessApply(context.Background(), ApplyRequest{Definition: def})
	if err != nil {
		t.Fatalf("AssessApply: %v", err)
	}
	if out.Allowed {
		t.Fatal("证书不在盘上时应当被拦下")
	}
	found := false
	for _, b := range out.Blockers {
		if b.Code == "SCHEDULE_TARGET_NOT_READY" && strings.Contains(b.Message, "live") {
			found = true
		}
	}
	if !found {
		t.Fatalf("阻塞项 = %+v", out.Blockers)
	}
}

func TestApplyInstallsTimerAndRecordsRef(t *testing.T) {
	mod, store, units, _ := newHarness(t)
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(units.ensured) != 1 || units.ensured[0] != rec.ID {
		t.Fatalf("没有装单元: %v", units.ensured)
	}
	if rec.TimerRef != TimerName(rec.ID) {
		t.Fatalf("TimerRef = %q, 期望 %q", rec.TimerRef, TimerName(rec.ID))
	}
	stored, found, _ := store.Get(context.Background(), rec.ID)
	if !found || stored.TimerRef != TimerName(rec.ID) {
		t.Fatalf("库里的 TimerRef = %q（found=%v），下次读出来会看成没装", stored.TimerRef, found)
	}
	if !rec.Enabled {
		t.Fatal("启用的计划在库里应当是 enabled")
	}
}

func TestApplyDisabledRemovesTimer(t *testing.T) {
	mod, _, units, _ := newHarness(t)
	def := testDef()
	def.Enabled = false
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: def})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(units.removed) != 1 {
		t.Fatalf("停用的计划应当卸掉单元: %v", units.removed)
	}
	if len(units.ensured) != 0 {
		t.Fatalf("停用的计划不该装单元: %v", units.ensured)
	}
	if rec.TimerRef != "" {
		t.Fatalf("TimerRef = %q, 期望为空", rec.TimerRef)
	}
}

func TestApplyReportsUnitFailureWithoutLying(t *testing.T) {
	mod, store, units, _ := newHarness(t)
	units.ensureErr = errors.New("systemctl enable --now 失败：Unit file does not exist.")

	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err == nil {
		t.Fatal("装单元失败必须报错")
	}
	if !strings.Contains(err.Error(), "定义已保存") || !strings.Contains(err.Error(), "没装上") {
		t.Fatalf("错误 = %v, 期望说清「定义已保存但单元没装上」", err)
	}
	stored, found, _ := store.Get(context.Background(), rec.ID)
	if !found {
		t.Fatal("定义应当已经落库")
	}
	if stored.TimerRef != "" {
		t.Fatalf("单元没装上，TimerRef 却是 %q", stored.TimerRef)
	}
}

func TestApplyRejectsWhenUnitNotEnabled(t *testing.T) {
	mod, _, _, _ := newHarness(t)
	// Ensure 返回"存在但没启用"：不能把它当成已生效。
	mod.units = &fakeUnits{status: Status{Present: true, UnitFileState: "disabled", ActiveState: "inactive"}}
	if _, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()}); err == nil {
		t.Fatal("单元没启用时必须报错")
	}
}

func TestApplyRejectsEnabledButInactiveTimer(t *testing.T) {
	mod, store, _, _ := newHarness(t)
	mod.units = &fakeUnits{status: Status{Present: true, UnitFileState: "enabled", ActiveState: "inactive"}}
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err == nil || !strings.Contains(err.Error(), "ActiveState=inactive") {
		t.Fatalf("已启用但未运行的 timer 必须报错，实际 %v", err)
	}
	stored, found, getErr := store.Get(context.Background(), rec.ID)
	if getErr != nil || !found {
		t.Fatalf("读取已保存定义 = %v, %v", getErr, found)
	}
	if stored.TimerRef != "" {
		t.Fatalf("未运行的 timer 不应记录安装成功引用：%q", stored.TimerRef)
	}
}

// TestApplyAcceptsCertificateRenewal：证书续期计划要真的能保存下来、真的装上单元。
//
// P12.4 的 Apply 里有一道"该动作尚未接线"的闸，预检却放行——于是界面上是"预检通过、
// 提交后失败"，而 P12.5 的真机验收正是撞在这个形状上。这条测试就是那道闸的看门人：
// 它一旦回来，这里立刻红。
func TestApplyAcceptsCertificateRenewal(t *testing.T) {
	mod, store, units, _ := newHarness(t)
	def := testDef()
	def.Kind = KindCertificateRenewal
	def.TargetID = "crt_0123456789abcdef0123456789abcdef"
	def.Certificate = &CertificateParams{Domain: "panel.example.com", Staging: true}

	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: def})
	if err != nil {
		t.Fatalf("证书续期计划必须能保存：%v", err)
	}
	if rec.ID == "" {
		t.Fatal("记录没有落库")
	}
	if rec.Definition.Certificate == nil || rec.Definition.Certificate.Domain != "panel.example.com" {
		t.Fatalf("种类参数没有跟着定义落库：%+v", rec.Definition.Certificate)
	}
	if len(units.ensured) != 1 {
		t.Fatalf("应当装上恰好一个单元，实际 %d 个", len(units.ensured))
	}
	if got, want := rec.TimerRef, TimerName(rec.ID); got != want {
		t.Fatalf("timer 引用 = %q，期望 %q", got, want)
	}
	if _, ok, _ := store.Get(context.Background(), rec.ID); !ok {
		t.Fatal("库里查不到刚保存的计划")
	}
}

// TestApplyRejectsCertificateRenewalWithoutParams：闸拆了不等于放水——种类参数
// 缺失仍然拒绝（Validate 的口），否则会装上一条续期时拿不到域名的单元。
func TestApplyRejectsCertificateRenewalWithoutParams(t *testing.T) {
	mod, _, units, _ := newHarness(t)
	def := testDef()
	def.Kind = KindCertificateRenewal
	def.TargetID = "crt_0123456789abcdef0123456789abcdef"
	if _, err := mod.Apply(context.Background(), ApplyRequest{Definition: def}); err == nil {
		t.Fatal("缺少证书参数的计划必须拒绝")
	}
	if len(units.ensured) != 0 {
		t.Fatal("被拒绝的计划不该装单元")
	}
}

func TestDeleteRejectsStaleVersionBeforeRemovingUnits(t *testing.T) {
	mod, store, units, _ := newHarness(t)
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := len(units.removed)

	err = mod.Delete(context.Background(), rec.ID, rec.Version-1)
	if err == nil || !strings.Contains(err.Error(), "版本冲突") {
		t.Fatalf("过期版本必须返回冲突，实际 %v", err)
	}
	if got := len(units.removed); got != before {
		t.Fatalf("过期删除不应触碰 live 单元，Remove 调用从 %d 变成 %d", before, got)
	}
	if _, found, _ := store.Get(context.Background(), rec.ID); !found {
		t.Fatal("过期删除不应移除记录")
	}
}

func TestDeleteStopsUnitsBeforeDeletingRecord(t *testing.T) {
	mod, store, units, _ := newHarness(t)
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	units.removeErr = errors.New("Failed to stop unit")
	if err := mod.Delete(context.Background(), rec.ID, rec.Version); err == nil {
		t.Fatal("停不掉单元时必须报错")
	}
	if _, found, _ := store.Get(context.Background(), rec.ID); !found {
		t.Fatal("停不掉单元时记录不该被删")
	}

	units.removeErr = nil
	if err := mod.Delete(context.Background(), rec.ID, rec.Version); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := store.Get(context.Background(), rec.ID); found {
		t.Fatal("记录还在")
	}
}

func TestDecideTriggerRecomputesIDAndAction(t *testing.T) {
	mod, store, _, now := newHarness(t)
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	triggerID := TriggerID(rec.ID, SlotAt(*now))

	dec, err := mod.DecideTrigger(context.Background(), rec.ID, triggerID)
	if err != nil {
		t.Fatalf("DecideTrigger: %v", err)
	}
	if dec.Blocked() {
		t.Fatalf("合法触发被判成阻塞: %v", dec.Blockers)
	}
	if dec.OperationID != OperationID(triggerID) {
		t.Fatalf("operation id = %q", dec.OperationID)
	}
	if !dec.Slot.Equal(SlotAt(*now)) {
		t.Fatalf("slot = %s", dec.Slot)
	}
	if dec.Action.Action != "backup.run" {
		t.Fatalf("action = %q", dec.Action.Action)
	}
	var params map[string]any
	if err := json.Unmarshal(dec.Action.Params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	// 版本来自现场解析，不是来自计划定义：策略改过之后，定时触发要跑的是新版本。
	if params["version"].(float64) != 7 {
		t.Fatalf("params = %v, 期望带上现场版本 7", params)
	}

	// 调用方自称的 ID 一律不接受。
	if _, err := mod.DecideTrigger(context.Background(), rec.ID, "trg_"+strings.Repeat("0", 32)); err == nil {
		t.Fatal("伪造的触发 ID 必须被拒绝")
	}
	_ = store
}

func TestDecideTriggerGraceSlot(t *testing.T) {
	mod, _, _, now := newHarness(t)
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	prev := TriggerID(rec.ID, SlotAt(*now).Add(-time.Minute))
	if _, err := mod.DecideTrigger(context.Background(), rec.ID, prev); err != nil {
		t.Fatalf("上一分钟槽的触发应当接受（跨分钟边界）: %v", err)
	}
	old := TriggerID(rec.ID, SlotAt(*now).Add(-5*time.Minute))
	if _, err := mod.DecideTrigger(context.Background(), rec.ID, old); err == nil {
		t.Fatal("五分钟前的槽不该被接受")
	}
}

func TestDecideTriggerBlocksDisabledAndMissingTarget(t *testing.T) {
	mod, store, _, now := newHarness(t)
	def := testDef()
	def.Enabled = false
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: def})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dec, err := mod.DecideTrigger(context.Background(), rec.ID, TriggerID(rec.ID, SlotAt(*now)))
	if err != nil {
		t.Fatalf("DecideTrigger: %v", err)
	}
	if !dec.Blocked() || dec.Blockers[0].Code != "SCHEDULE_DISABLED" {
		t.Fatalf("停用的计划触发应当被拦: %+v", dec.Blockers)
	}

	// 目标消失：单元还在，目标没了。
	// 定义与展平列是一起写的（definition_json 是权威那份，columns 只是可查询的副本），
	// 所以"计划里记的目标不见了"要把两处一起改，改一处造出来的是不会发生的现场。
	enabled := testDef()
	rec2, err := mod.Apply(context.Background(), ApplyRequest{Definition: enabled})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	delete(store.recs, rec2.ID)
	store.recs[rec2.ID] = func() Record {
		r := rec2
		r.TargetID = "pol_ffffffffffffffffffffffffffffffff"
		r.Definition.TargetID = r.TargetID
		return r
	}()
	dec, err = mod.DecideTrigger(context.Background(), rec2.ID, TriggerID(rec2.ID, SlotAt(*now)))
	if err != nil {
		t.Fatalf("DecideTrigger: %v", err)
	}
	if !dec.Blocked() || dec.Blockers[0].Code != "SCHEDULE_TARGET_NOT_FOUND" {
		t.Fatalf("目标消失时应当被拦: %+v", dec.Blockers)
	}
}

func TestClaimTriggerIsIdempotentPerSlot(t *testing.T) {
	mod, _, _, now := newHarness(t)
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	triggerID := TriggerID(rec.ID, SlotAt(*now))
	dec, err := mod.DecideTrigger(context.Background(), rec.ID, triggerID)
	if err != nil {
		t.Fatalf("DecideTrigger: %v", err)
	}

	op1, dup, err := mod.ClaimTrigger(context.Background(), dec, TriggerOutcomeTriggered, "")
	if err != nil {
		t.Fatalf("ClaimTrigger: %v", err)
	}
	if dup || op1 != dec.OperationID {
		t.Fatalf("第一次认领: op=%q dup=%v", op1, dup)
	}
	op2, dup, err := mod.ClaimTrigger(context.Background(), dec, TriggerOutcomeTriggered, "")
	if err != nil {
		t.Fatalf("ClaimTrigger 第二次: %v", err)
	}
	if !dup || op2 != op1 {
		t.Fatalf("同一槽第二次认领应当返回原 operation id: op=%q dup=%v", op2, dup)
	}
}

func TestScriptScheduleName(t *testing.T) {
	mod, _, _, _ := newHarness(t)
	def := testDef()
	def.Kind = KindUserScript
	def.TargetID = "scr_0123456789abcdef0123456789abcdef"
	if _, err := mod.Apply(context.Background(), ApplyRequest{Definition: def}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	name, used, err := mod.ScriptScheduleName(context.Background(), "scr_0123456789abcdef0123456789abcdef")
	if err != nil || !used || name != "每晚备份" {
		t.Fatalf("引用查询 = %q %v %v", name, used, err)
	}
	if _, used, _ := mod.ScriptScheduleName(context.Background(), "scr_ffffffffffffffffffffffffffffffff"); used {
		t.Fatal("没人引用的脚本被说成被引用")
	}
}

func TestViewReportsFacts(t *testing.T) {
	mod, _, units, _ := newHarness(t)
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: testDef()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	v, found, err := mod.View(context.Background(), rec.ID)
	if err != nil || !found {
		t.Fatalf("View: %v %v", err, found)
	}
	if !v.Units.Enabled() || !v.Units.Present {
		t.Fatalf("单元事实 = %+v", v.Units)
	}
	if v.NextElapse == nil {
		t.Fatal("启用的计划应当给出下次触发时刻")
	}
	if len(v.Blockers) != 0 {
		t.Fatalf("正常计划不该有阻塞项: %v", v.Blockers)
	}

	units.status = Status{Present: false, UnitFileState: "not-found", ActiveState: "unknown"}
	views, err := mod.Views(context.Background())
	if err != nil {
		t.Fatalf("Views: %v", err)
	}
	if len(views) != 1 || views[0].Units.Present {
		t.Fatalf("列表没有如实反映单元状态: %+v", views)
	}
}

func TestNewModuleRequiresParts(t *testing.T) {
	if _, err := NewModule(ModuleOptions{Units: &fakeUnits{}}); err == nil {
		t.Fatal("没有仓储必须报错")
	}
	if _, err := NewModule(ModuleOptions{Store: newFakeStore()}); err == nil {
		t.Fatal("没有单元端必须报错")
	}
}

func TestAssessApplyBlocksSecondScheduleOnSameTarget(t *testing.T) {
	// 库里有 UNIQUE(kind, target_id)，预检必须自己先说出来：否则形状是
	// "预检 allowed=true、提交后操作失败"，那是 P12.4 真机上抓到的一次。
	mod, store, _, _ := newHarness(t)
	first := testDef()
	rec, err := mod.Apply(context.Background(), ApplyRequest{Definition: first})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	second := first
	second.Name = "另一条"
	second.CalendarExpression = "*-*-* 06:00:00"
	out, err := mod.AssessApply(context.Background(), ApplyRequest{Definition: second})
	if err != nil {
		t.Fatalf("AssessApply: %v", err)
	}
	if out.Allowed {
		t.Fatal("同一个对象上的第二条计划必须被预检挡下")
	}
	var codes []string
	for _, b := range out.Blockers {
		codes = append(codes, b.Code)
	}
	if len(codes) != 1 || codes[0] != "SCHEDULE_TARGET_TAKEN" {
		t.Fatalf("阻塞项 = %v, 期望只有 SCHEDULE_TARGET_TAKEN", codes)
	}
	// 更新自己不算"另一个对象占着"：同一条计划改时间窗必须放行。
	second.Name = rec.Name
	out, err = mod.AssessApply(context.Background(), ApplyRequest{ScheduleID: rec.ID, Version: rec.Version, Definition: second})
	if err != nil {
		t.Fatalf("AssessApply(update): %v", err)
	}
	for _, b := range out.Blockers {
		if b.Code == "SCHEDULE_TARGET_TAKEN" || b.Code == "SCHEDULE_NAME_TAKEN" {
			t.Fatalf("改自己不该被挡: %v", out.Blockers)
		}
	}
	if len(store.recs) != 1 {
		t.Fatalf("库里应当只有一条计划，实际 %d", len(store.recs))
	}
}

func TestAssessApplyBlocksDuplicateName(t *testing.T) {
	mod, store, _, _ := newHarness(t)
	first := testDef()
	if _, err := mod.Apply(context.Background(), ApplyRequest{Definition: first}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	second := first
	// 目标换成一个形状合法的另一条策略：这里要验的只有"名字"，目标是否就绪由别的断言管。
	second.TargetID = "pol_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	out, err := mod.AssessApply(context.Background(), ApplyRequest{Definition: second})
	if err != nil {
		t.Fatalf("AssessApply: %v", err)
	}
	if out.Allowed {
		t.Fatal("重名必须被预检挡下")
	}
	found := false
	for _, b := range out.Blockers {
		if b.Code == "SCHEDULE_NAME_TAKEN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("阻塞项 = %v, 期望含 SCHEDULE_NAME_TAKEN", out.Blockers)
	}
	if len(store.recs) != 1 {
		t.Fatalf("不该多出记录: %d", len(store.recs))
	}
}
