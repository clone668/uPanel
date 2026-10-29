package schedules

// 计划任务的动作实现端（P12.4；CONTRACTS.md 第 5、12、15 节）。
//
// 这一层把四件事串成一条可审计的链：
//
//	apply    先让 systemd 认表达式，再落库，再装单元，最后把实际装上的单元名记回库里。
//	         任何一步失败都停在原地并把"哪一步过了、哪一步没过"说清楚，绝不让
//	         一条"看起来保存成功"的计划在凌晨三点安静地什么都不做。
//	delete   先停单元再删记录：反过来会留下一段"库里没有、systemd 还在计时"的时间。
//	trigger  重新算触发 ID（不接受调用方自称的槽），重新解析目标版本，重新走动作翻译。
//	         定时触发不是另一条执行路径，它只是"没有人在键盘前"的同一条路径。
//
// 它不执行动作：动作由代理运行器跑。这里只产出"该跑什么"和"这次触发算不算过"。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SystemUserID 是"系统自己"在操作与任务记录里的操作者 ID（P12.4）。
//
// 它必须是**正数**：代理库的 operations/preflight_plans 与面板库的 jobs/preflights
// 都有 `CHECK (user_id > 0)` / `REFERENCES users(id)`，而那条约束是对的——user_id
// 指向一个真实的账号，0 不是一个账号。所以"系统自己"用的是一个保留值：面板的
// users.id 是自增的，永远到不了 2^62。选它而不是 0 也不是 MaxInt64，是因为它一眼
// 看得出是保留区，同时给自增留足空间（SQLite 的 INTEGER 是 64 位）。
//
// 值只在这里写一次：代理（记录定时操作）与面板（对账导入任务）用的是同一个数，
// 各写一份常量就是两个真相。
const SystemUserID int64 = 1 << 62

// Store 是本模块需要的计划仓储能力（由 agentstore.ScheduleStore 实现）。
type Store interface {
	List(ctx context.Context) ([]Record, error)
	Get(ctx context.Context, id string) (Record, bool, error)
	GetByTarget(ctx context.Context, kind Kind, targetID string) (Record, bool, error)
	Create(ctx context.Context, def Definition, now time.Time) (Record, error)
	Update(ctx context.Context, id string, def Definition, expectedVersion int64, now time.Time) (Record, error)
	Delete(ctx context.Context, id string, expectedVersion int64) error
	SetTimerRef(ctx context.Context, id, ref string, now time.Time) (Record, error)
	RecordTrigger(ctx context.Context, rec TriggerRecord) (string, bool, error)
	ListTriggers(ctx context.Context, scheduleID string, limit int) ([]TriggerRecord, error)
}

// Units 是单元落地端（由 Installer 实现）。
type Units interface {
	ValidateCalendar(ctx context.Context, d Definition) (CalendarCheck, error)
	Ensure(ctx context.Context, id string, d Definition) (Status, error)
	Remove(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (Status, error)
}

// Target 是被计划指向的对象此刻的样子。
type Target struct {
	Name string
	// Version 是被执行动作需要的版本号（备份策略 / 脚本的乐观并发版本）。
	Version int64
	// Ready 为 false 时计划不能保存也跑不了；Note 说明原因。
	Ready bool
	Note  string
}

// TargetLookup 解析计划目标（由代理层接上备份策略、脚本与证书的现场）。
//
// 参数是整个定义而不是 (kind, target_id)：证书续期的目标事实不在某张表里，
// 而在**盘上**（`/etc/letsencrypt/live/<domain>` 在不在），判它需要域名，
// 而域名只在定义的种类参数里。把定义整个交过去，比将来再补第二个参数诚实。
type TargetLookup func(ctx context.Context, def Definition) (Target, bool, error)

// Notice 是一条阻塞项或告警项。
type Notice struct {
	Code    string
	Message string
}

// Impact 是一条影响说明。
type Impact struct {
	Kind        string
	Description string
}

// Assessment 是 apply/delete 的预检结论。
type Assessment struct {
	Allowed             bool
	Blockers            []Notice
	Warnings            []Notice
	Impacts             []Impact
	DisplayName         string
	IdentityFingerprint string
	StateVersion        string
	// NextElapse 是 systemd 认可的下一次触发时刻（apply 预检时就有）。
	NextElapse *time.Time
}

// Module 是计划动作的实现端。
type Module struct {
	store  Store
	units  Units
	lookup TargetLookup
	now    func() time.Time
	// triggerGrace 是触发 ID 的容忍槽数：CLI 算槽与代理收单可能跨过分钟边界。
	triggerGrace int
}

// ModuleOptions 是构造参数。
type ModuleOptions struct {
	Store  Store
	Units  Units
	Lookup TargetLookup
	Now    func() time.Time
	// TriggerGraceSlots 默认 2。
	TriggerGraceSlots int
}

// NewModule 构造实现端。
func NewModule(opts ModuleOptions) (*Module, error) {
	if opts.Store == nil {
		return nil, errors.New("schedules: 计划模块需要仓储")
	}
	if opts.Units == nil {
		return nil, errors.New("schedules: 计划模块需要单元落地端")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	grace := opts.TriggerGraceSlots
	if grace <= 0 {
		grace = 2
	}
	return &Module{store: opts.Store, units: opts.Units, lookup: opts.Lookup, now: now, triggerGrace: grace}, nil
}

// List 返回全部计划。
func (m *Module) List(ctx context.Context) ([]Record, error) { return m.store.List(ctx) }

// Get 返回一条计划。
func (m *Module) Get(ctx context.Context, id string) (Record, bool, error) {
	return m.store.Get(ctx, id)
}

// Triggers 返回一条计划的最近触发记录。
func (m *Module) Triggers(ctx context.Context, id string, limit int) ([]TriggerRecord, error) {
	return m.store.ListTriggers(ctx, id, limit)
}

// ScriptScheduleName 回答"这条脚本还被哪条计划引用"。
//
// 它让脚本模块在删除前能问到引用关系，而不必反向依赖计划仓储。
func (m *Module) ScriptScheduleName(ctx context.Context, scriptID string) (string, bool, error) {
	rec, found, err := m.store.GetByTarget(ctx, KindUserScript, scriptID)
	if err != nil || !found {
		return "", false, err
	}
	return rec.Name, true, nil
}

// ApplyRequest 是 schedule.apply 的参数。
type ApplyRequest struct {
	// ScheduleID 为空表示新建。
	ScheduleID string
	Version    int64
	Definition Definition
}

// AssessApply 生成 schedule.apply 的预检结论。
//
// 它**真的去问 systemd**：日历表达式认不认、下一次什么时候触发。这两件事在
// 管理员按下"保存"之前就该有答案。
func (m *Module) AssessApply(ctx context.Context, req ApplyRequest) (Assessment, error) {
	def := req.Definition.Normalize()
	out := Assessment{Allowed: true, DisplayName: def.Name}
	if err := def.Validate(); err != nil {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_INVALID", Message: err.Error()})
		return out, nil
	}
	out.Impacts = append(out.Impacts, Impact{
		Kind:        "systemd_unit",
		Description: fmt.Sprintf("写入 %s 与 %s", TimerPath("sch_…"), ServicePath("sch_…")),
	})

	// P12.4 在这里拦过"证书续期尚未接线"。P12.5 把那三件事补齐之后闸就拆了：
	// 种类参数进定义（Certificate）、目标按盘上 live 目录判（代理侧）、
	// 参数只带 domain/staging/scheduled（执行侧按定时语义不强制重签）。
	// 留着的唯一阻塞项是下面那条"目标不在盘上"，而它现在对证书有真实含义。
	if check, err := m.units.ValidateCalendar(ctx, def); err != nil {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_CALENDAR_INVALID", Message: err.Error()})
	} else {
		next := check.NextElapse
		out.NextElapse = &next
	}
	if m.lookup != nil {
		target, found, err := m.lookup(ctx, def)
		if err != nil {
			return Assessment{}, err
		}
		switch {
		case !found:
			out.Allowed = false
			out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_TARGET_NOT_FOUND",
				Message: "计划指向的对象不存在"})
		case !target.Ready:
			out.Allowed = false
			out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_TARGET_NOT_READY",
				Message: target.Note})
		}
	}

	// 一个对象只接受一条计划（库里有 UNIQUE(kind, target_id)）。这条约束必须在预检
	// 阶段就说话：让它到执行阶段才炸，就是"预检说允许、提交以后失败"——最难查的一种。
	// 真机现场：给同一份脚本再排一个 UTC 时间窗，预检 `allowed=true`，
	// 提交后操作失败在 `这个对象已经被另一条计划任务使用`。
	if other, found, err := m.store.GetByTarget(ctx, def.Kind, def.TargetID); err != nil {
		return Assessment{}, err
	} else if found && other.ID != req.ScheduleID {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_TARGET_TAKEN",
			Message: fmt.Sprintf("这个对象已经挂在计划「%s」上：一个对象只接受一条计划（同一份定义要排两个时间窗，用一条日历表达式写多个时刻）", other.Name)})
	}
	// 名字同样有 UNIQUE。重名在审计里分不清谁是谁，所以在预检就挡，而不是等 INSERT 失败。
	if list, err := m.store.List(ctx); err != nil {
		return Assessment{}, err
	} else {
		for _, other := range list {
			if other.ID != req.ScheduleID && other.Name == def.Name {
				out.Allowed = false
				out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_NAME_TAKEN",
					Message: fmt.Sprintf("计划名字「%s」已被占用", def.Name)})
				break
			}
		}
	}

	if req.ScheduleID == "" {
		out.IdentityFingerprint = "new-schedule"
		out.StateVersion = "none"
		return out, nil
	}
	rec, found, err := m.store.Get(ctx, req.ScheduleID)
	if err != nil {
		return Assessment{}, err
	}
	if !found {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_NOT_FOUND", Message: "计划任务不存在"})
		out.IdentityFingerprint = req.ScheduleID
		out.StateVersion = "none"
		return out, nil
	}
	out.DisplayName = rec.Name
	out.IdentityFingerprint = rec.ID
	out.StateVersion = strconv.FormatInt(rec.Version, 10)
	if req.Version != rec.Version {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{
			Code:    "VERSION_CONFLICT",
			Message: fmt.Sprintf("计划已被改动过（当前版本 %d，预检基于 %d）", rec.Version, req.Version),
		})
	}
	if rec.Name != def.Name {
		out.Warnings = append(out.Warnings, Notice{Code: "SCHEDULE_RENAMED",
			Message: fmt.Sprintf("名字将从「%s」改成「%s」", rec.Name, def.Name)})
	}
	return out, nil
}

// Apply 执行 schedule.apply：落库 → 装/卸单元 → 把装上的单元名记回库里。
func (m *Module) Apply(ctx context.Context, req ApplyRequest) (Record, error) {
	def := req.Definition.Normalize()
	if err := def.Validate(); err != nil {
		return Record{}, err
	}
	// P12.4 在这里拦过证书续期（"该动作尚未接线"）。P12.5 把三件事补齐了，闸跟着拆：
	// 种类参数进定义、ActionFor 给出 domain/staging/scheduled 的三字段参数、代理侧
	// 定时语义不强制重签。真机验收撞到的正是这道闸——预检放行、提交后失败，是最难
	// 查的一种形状，所以它不能靠"记得删"，得靠 TestApplyAcceptsCertificateRenewal 钉住。
	now := m.now().UTC()

	var rec Record
	var err error
	if req.ScheduleID == "" {
		rec, err = m.store.Create(ctx, def, now)
	} else {
		rec, err = m.store.Update(ctx, req.ScheduleID, def, req.Version, now)
	}
	if err != nil {
		return Record{}, err
	}

	if !def.Enabled {
		// 停用的计划不该留下任何会计时东西。先停单元，再清 ref。
		if err := m.units.Remove(ctx, rec.ID); err != nil {
			return rec, fmt.Errorf("定义已保存（停用），但停不掉定时单元：%w", err)
		}
		return m.store.SetTimerRef(ctx, rec.ID, "", now)
	}
	status, err := m.units.Ensure(ctx, rec.ID, def)
	if err != nil {
		return rec, fmt.Errorf("定义已保存，但定时单元没装上：%w", err)
	}
	if !status.Present || !status.Enabled() {
		return rec, fmt.Errorf("定义已保存，但单元没有真正启用（UnitFileState=%s）", status.UnitFileState)
	}
	return m.store.SetTimerRef(ctx, rec.ID, TimerName(rec.ID), now)
}

// AssessDelete 生成 schedule.delete 的预检结论。
func (m *Module) AssessDelete(ctx context.Context, id string, version int64) (Assessment, error) {
	rec, found, err := m.store.Get(ctx, id)
	if err != nil {
		return Assessment{}, err
	}
	if !found {
		return Assessment{Allowed: false, Blockers: []Notice{{Code: "SCHEDULE_NOT_FOUND", Message: "计划任务不存在"}},
			IdentityFingerprint: id, StateVersion: "none"}, nil
	}
	out := Assessment{
		Allowed:             true,
		DisplayName:         rec.Name,
		IdentityFingerprint: rec.ID,
		StateVersion:        strconv.FormatInt(rec.Version, 10),
		Impacts: []Impact{{Kind: "systemd_unit",
			Description: fmt.Sprintf("停用并删除 %s", TimerName(rec.ID))}},
	}
	if version != rec.Version {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{
			Code:    "VERSION_CONFLICT",
			Message: fmt.Sprintf("计划已被改动过（当前版本 %d，预检基于 %d）", rec.Version, version),
		})
	}
	return out, nil
}

// Delete 执行 schedule.delete：先停单元，再删记录。
//
// 顺序是刻意的：记录还在而单元已经没了，界面上是"启用但没有 timer"这种可见的异常；
// 单元还在而记录已经没了，只有 journal 里一串失败，没人看得见。
func (m *Module) Delete(ctx context.Context, id string, version int64) error {
	rec, found, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := m.units.Remove(ctx, id); err != nil {
		return fmt.Errorf("停不掉定时单元，计划保持不变：%w", err)
	}
	return m.store.Delete(ctx, id, rec.Version)
}

// TriggerDecision 是一次定时触发的判决。
type TriggerDecision struct {
	Schedule    Record
	TriggerID   string
	OperationID string
	Slot        time.Time
	Action      ActionRequest
	Blockers    []Notice
	// Duplicate 表示这个时间槽已经处理过；OperationID 是当时那一次。
	Duplicate bool
}

// Blocked 报告这次触发是否有阻塞项。
func (d TriggerDecision) Blocked() bool { return len(d.Blockers) > 0 }

// DecideTrigger 生成一次定时触发的判决（不写任何东西）。
//
// 触发 ID 由代理自己算：调用方给的 ID 必须与"这条计划在当前时间槽"的哈希一致，
// 否则拒绝。这样"稳定触发 ID"不是调用方的承诺，而是可验证的事实——同一个槽
// 无论谁触发几次，落进 tracing/审计里的都是同一个 ID。
func (m *Module) DecideTrigger(ctx context.Context, scheduleID, triggerID string) (TriggerDecision, error) {
	now := m.now().UTC()
	rec, found, err := m.store.Get(ctx, scheduleID)
	if err != nil {
		return TriggerDecision{}, err
	}
	out := TriggerDecision{Schedule: rec, TriggerID: strings.TrimSpace(triggerID)}
	if !found {
		out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_NOT_FOUND", Message: "计划任务不存在"})
		return out, nil
	}
	slot, ok := m.matchSlot(scheduleID, out.TriggerID, now)
	if !ok {
		return TriggerDecision{}, fmt.Errorf("触发 ID 与当前时间槽不符（%q）；定时触发只能由 upanelctl schedule-trigger 发起", out.TriggerID)
	}
	out.Slot = slot
	out.OperationID = OperationID(out.TriggerID)

	if !rec.Enabled {
		out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_DISABLED", Message: "计划任务已停用"})
		return out, nil
	}
	version := int64(0)
	if m.lookup != nil {
		target, exists, err := m.lookup(ctx, rec.Definition)
		if err != nil {
			return TriggerDecision{}, err
		}
		if !exists {
			out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_TARGET_NOT_FOUND",
				Message: "计划指向的对象已经不存在"})
			return out, nil
		}
		if !target.Ready {
			out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_TARGET_NOT_READY", Message: target.Note})
			return out, nil
		}
		version = target.Version
	}
	action, err := ActionFor(rec.Definition, ResolvedTarget{Version: version})
	if err != nil {
		out.Blockers = append(out.Blockers, Notice{Code: "SCHEDULE_ACTION_UNSUPPORTED", Message: err.Error()})
		return out, nil
	}
	out.Action = action
	return out, nil
}

// matchSlot 判断触发 ID 是否对应最近几个时间槽之一，并返回那个槽。
func (m *Module) matchSlot(scheduleID, triggerID string, now time.Time) (time.Time, bool) {
	if triggerID == "" {
		return time.Time{}, false
	}
	base := SlotAt(now)
	for i := 0; i < m.triggerGrace; i++ {
		slot := base.Add(-time.Duration(i) * time.Minute)
		if TriggerID(scheduleID, slot) == triggerID {
			return slot, true
		}
	}
	return time.Time{}, false
}

// ClaimTrigger 把这个时间槽记进触发表，返回是否重复。
//
// 它同时是幂等门：同一个槽只有第一个调用者拿到 duplicate=false。库里已有那次
// 触发的 operation_id 时原样返回——"这个槽当时跑的是什么"必须可查。
func (m *Module) ClaimTrigger(ctx context.Context, d TriggerDecision, outcome, reason string) (string, bool, error) {
	if d.TriggerID == "" || d.Schedule.ID == "" {
		return "", false, errors.New("schedules: 触发记录缺少触发 ID 或计划")
	}
	return m.store.RecordTrigger(ctx, TriggerRecord{
		TriggerID:   d.TriggerID,
		ScheduleID:  d.Schedule.ID,
		SlotAt:      d.Slot,
		Action:      d.Action.Action,
		OperationID: d.OperationID,
		Outcome:     outcome,
		Reason:      reason,
		CreatedAt:   m.now().UTC(),
	})
}

// View 是一条计划的现场。
type View struct {
	Record Record
	// Units 是 systemd 的事实；Installed=false 表示这台机器上没有它的单元。
	Units Status
	// NextElapse 是下一次触发时刻；读不到时为空。
	NextElapse *time.Time
	// Blockers 是"现在就算触发了也跑不起来"的原因。
	Blockers []Notice
}

// View 返回一条计划的现场（含单元事实与下次触发时刻）。
func (m *Module) View(ctx context.Context, id string) (View, bool, error) {
	rec, found, err := m.store.Get(ctx, id)
	if err != nil || !found {
		return View{}, found, err
	}
	return m.describe(ctx, rec), true, nil
}

// ViewsConcurrency 是列表组装时的并发上限。
//
// 每条计划的现场都要**真的去问 systemd 两次**（`systemd-analyze calendar` 算下次
// 触发、`systemctl show` 读单元事实），两次都是 fork/exec 外部进程。24.04 验收机上
// 一次 calendar 250—330 毫秒、一次 show 同量级；串行组装就让请求时长随计划条数线性
// 长大——P12.4 第三轮验收现场的列表读了 10144 毫秒，正好压过 Web 侧 10 秒的通用上限，
// 于是**一个健康的代理被报成"代理不可用"并回 503**（证据：代理日志
// `GET /internal/v1/schedules 200 duration_ms=10144`，Web 日志同一时刻
// `Client.Timeout exceeded while awaiting headers`）。
//
// 取 4 是因为这些子进程本身在抢同样的两三个核：再高只是把 CPU 争用换成更多的
// 进程启动，墙钟不会跟着降。
const ViewsConcurrency = 4

// Views 返回全部计划的现场。
//
// 顺序与库里一致（按下标回填），并发的只是"问 systemd"这一步。
func (m *Module) Views(ctx context.Context) ([]View, error) {
	recs, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]View, len(recs))
	if len(recs) == 0 {
		return out, nil
	}
	workers := ViewsConcurrency
	if len(recs) < workers {
		workers = len(recs)
	}
	idx := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range idx {
				out[i] = m.describe(ctx, recs[i])
			}
		}()
	}
	for i := range recs {
		idx <- i
	}
	close(idx)
	wg.Wait()
	// 调用方已经走了就别把这些"问不到 systemd"的现场当成事实交上去：describe 会把
	// 取消记成 SCHEDULE_UNIT_STATUS_UNREADABLE，那是在把"我们不等了"说成"单元坏了"。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// describe 组装一条计划的现场。
//
// 列表视图与详情视图走同一条组装。下次触发时刻来自 systemd-analyze（绝对时刻，
// 不带时区歧义）；单元状态来自 systemctl。两者都不是猜的。
func (m *Module) describe(ctx context.Context, rec Record) View {
	v := View{Record: rec}
	status, err := m.units.Status(ctx, rec.ID)
	if err != nil {
		v.Blockers = append(v.Blockers, Notice{Code: "SCHEDULE_UNIT_STATUS_UNREADABLE", Message: err.Error()})
	} else {
		v.Units = status
	}
	if rec.Enabled {
		if check, err := m.units.ValidateCalendar(ctx, rec.Definition); err != nil {
			v.Blockers = append(v.Blockers, Notice{Code: "SCHEDULE_CALENDAR_INVALID", Message: err.Error()})
		} else {
			next := check.NextElapse
			v.NextElapse = &next
		}
	}
	if !rec.Enabled {
		v.Blockers = append(v.Blockers, Notice{Code: "SCHEDULE_DISABLED", Message: "计划任务已停用"})
		return v
	}
	if m.lookup != nil {
		target, found, err := m.lookup(ctx, rec.Definition)
		switch {
		case err != nil:
			v.Blockers = append(v.Blockers, Notice{Code: "SCHEDULE_TARGET_UNREADABLE", Message: err.Error()})
		case !found:
			v.Blockers = append(v.Blockers, Notice{Code: "SCHEDULE_TARGET_NOT_FOUND",
				Message: "计划指向的对象已经不存在"})
		case !target.Ready:
			v.Blockers = append(v.Blockers, Notice{Code: "SCHEDULE_TARGET_NOT_READY", Message: target.Note})
		}
	}
	return v
}
