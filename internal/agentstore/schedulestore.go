package agentstore

// 计划任务的持久化（CONTRACTS.md 第 12、14.2 节；P12.4）。
//
// 这一层只做六件事：列、取、建、改、删、记一次触发。**不判断**"这条计划现在能不能跑"——
// 那要读真实现场（策略还在不在、脚本哈希对不对得上、systemd 认不认这个日历），
// 判断在 internal/schedules 与代理动作层。
//
// 与单元的对应关系：`agent_timer_ref` 非空等价于"这台机器上装着这个 timer"。
// 定义存在而单元没装上（启用失败）时 ref 保持为空，界面因此不会把一次失败的
// enable 显示成"已生效"。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"upanel/internal/schedules"
)

// schedulePrefix 是计划 id 的前缀。
const schedulePrefix = "sch_"

// ScheduleRecord 是代理库里的一条计划。
//
// 别名而不是另立一份结构：形状由 internal/schedules 定义，仓储只负责持久化，
// 动作层拿到的与库里存的永远是同一个类型。
type ScheduleRecord = schedules.Record

// TriggerRecord 是一次触发。
type TriggerRecord = schedules.TriggerRecord

// 触发判决的闭集。
const (
	TriggerOutcomeTriggered = schedules.TriggerOutcomeTriggered
	TriggerOutcomeBlocked   = schedules.TriggerOutcomeBlocked
	TriggerOutcomeDuplicate = schedules.TriggerOutcomeDuplicate
	TriggerOutcomeFailed    = schedules.TriggerOutcomeFailed
)

var (
	// ErrScheduleNotFound 表示计划 id 不在库里。
	ErrScheduleNotFound = errors.New("agentstore: 计划任务不存在")
	// ErrScheduleVersion 表示期望版本与当前版本不一致（乐观并发冲突）。
	ErrScheduleVersion = errors.New("agentstore: 计划任务版本冲突")
	// ErrScheduleName 表示名字与另一条计划重复。
	ErrScheduleName = errors.New("agentstore: 计划任务名字已被占用")
	// ErrScheduleTarget 表示同一个对象上已经有另一条计划。
	ErrScheduleTarget = errors.New("agentstore: 这个对象已经被另一条计划任务使用")
)

// ScheduleStore 是计划任务的仓储。
type ScheduleStore struct{ db *DB }

// NewScheduleStore 构造仓储。
func NewScheduleStore(db *DB) (*ScheduleStore, error) {
	if db == nil {
		return nil, errors.New("agentstore: 计划任务仓储需要数据库句柄")
	}
	return &ScheduleStore{db: db}, nil
}

// List 返回全部计划（按名字排序，输出稳定）。
func (s *ScheduleStore) List(ctx context.Context) ([]ScheduleRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx, `
		SELECT id, name, kind, target_id, definition_json, definition_key,
		       agent_timer_ref, enabled, version, created_at, updated_at
		  FROM schedules ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取计划任务列表失败: %w", err)
	}
	defer rows.Close()
	var out []ScheduleRecord
	for rows.Next() {
		rec, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取计划任务列表失败: %w", err)
	}
	return out, nil
}

// Get 取一条计划；不存在时返回 (零值, false, nil)。
func (s *ScheduleStore) Get(ctx context.Context, id string) (ScheduleRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, name, kind, target_id, definition_json, definition_key,
		       agent_timer_ref, enabled, version, created_at, updated_at
		  FROM schedules WHERE id = ?`, id)
	rec, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRecord{}, false, nil
	}
	if err != nil {
		return ScheduleRecord{}, false, err
	}
	return rec, true, nil
}

// GetByTarget 按 (种类, 目标 id) 取计划。删除脚本前要据此判断"还被计划引用吗"。
func (s *ScheduleStore) GetByTarget(ctx context.Context, kind schedules.Kind, targetID string) (ScheduleRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, name, kind, target_id, definition_json, definition_key,
		       agent_timer_ref, enabled, version, created_at, updated_at
		  FROM schedules WHERE kind = ? AND target_id = ?`, string(kind), targetID)
	rec, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRecord{}, false, nil
	}
	if err != nil {
		return ScheduleRecord{}, false, err
	}
	return rec, true, nil
}

// Create 新建一条计划并生成 id。定义必须已经 Normalize + Validate 过（调用方负责）。
func (s *ScheduleStore) Create(ctx context.Context, def schedules.Definition, now time.Time) (ScheduleRecord, error) {
	now = now.UTC()
	raw, err := json.Marshal(def)
	if err != nil {
		return ScheduleRecord{}, fmt.Errorf("agentstore: 序列化计划定义失败: %w", err)
	}
	key, err := def.Key()
	if err != nil {
		return ScheduleRecord{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		id, err := newScheduleID()
		if err != nil {
			return ScheduleRecord{}, err
		}
		rec := ScheduleRecord{
			ID: id, Name: def.Name, Kind: def.Kind, TargetID: def.TargetID,
			Definition: def, DefinitionKey: key, Enabled: def.Enabled,
			Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		_, err = s.db.store().ExecContext(ctx, `
			INSERT INTO schedules (id, name, kind, target_id, definition_json, definition_key,
			                       agent_timer_ref, enabled, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
			rec.ID, rec.Name, string(rec.Kind), rec.TargetID, string(raw), key,
			boolInt(rec.Enabled), rec.Version, millis(now), millis(now))
		if err == nil {
			return rec, nil
		}
		if isUniqueViolation(err) {
			// 唯一约束有两个：名字与 (种类, 目标)。要分清是哪一种——一个是"换个名字"，
			// 另一个是"这个对象已经有人定时了"。
			//
			// 顺序有讲究：名字优先。整份表单被复制一遍时两个约束会同时撞上，
			// 此时先把"已有一条同名计划"说出来，人才知道是重复提交而不是要换个目标。
			if _, found, lookupErr := s.getByName(ctx, def.Name); lookupErr == nil && found {
				return ScheduleRecord{}, fmt.Errorf("%w: %s", ErrScheduleName, def.Name)
			}
			if existing, found, lookupErr := s.GetByTarget(ctx, def.Kind, def.TargetID); lookupErr == nil && found {
				return ScheduleRecord{}, fmt.Errorf("%w: %s", ErrScheduleTarget, existing.Name)
			}
			// 主键撞车：换一个 id 再试一次。
			if attempt == 0 {
				continue
			}
		}
		return ScheduleRecord{}, fmt.Errorf("agentstore: 写入计划任务失败: %w", err)
	}
	return ScheduleRecord{}, errors.New("agentstore: 生成计划任务 id 连续冲突")
}

// Update 按期望版本更新一条计划，成功后 version+1。
func (s *ScheduleStore) Update(ctx context.Context, id string, def schedules.Definition,
	expectedVersion int64, now time.Time) (ScheduleRecord, error) {
	now = now.UTC()
	raw, err := json.Marshal(def)
	if err != nil {
		return ScheduleRecord{}, fmt.Errorf("agentstore: 序列化计划定义失败: %w", err)
	}
	key, err := def.Key()
	if err != nil {
		return ScheduleRecord{}, err
	}
	res, err := s.db.store().ExecContext(ctx, `
		UPDATE schedules
		   SET name = ?, kind = ?, target_id = ?, definition_json = ?, definition_key = ?,
		       enabled = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		def.Name, string(def.Kind), def.TargetID, string(raw), key,
		boolInt(def.Enabled), millis(now), id, expectedVersion)
	if err != nil {
		if isUniqueViolation(err) {
			if existing, found, lookupErr := s.getByName(ctx, def.Name); lookupErr == nil && found && existing.ID != id {
				return ScheduleRecord{}, fmt.Errorf("%w: %s", ErrScheduleName, def.Name)
			}
			if existing, found, lookupErr := s.GetByTarget(ctx, def.Kind, def.TargetID); lookupErr == nil && found && existing.ID != id {
				return ScheduleRecord{}, fmt.Errorf("%w: %s", ErrScheduleTarget, existing.Name)
			}
			return ScheduleRecord{}, fmt.Errorf("%w: %s", ErrScheduleName, def.Name)
		}
		return ScheduleRecord{}, fmt.Errorf("agentstore: 更新计划任务 %s 失败: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ScheduleRecord{}, fmt.Errorf("agentstore: 更新计划任务 %s 失败: %w", id, err)
	}
	if affected == 0 {
		if _, found, err := s.Get(ctx, id); err != nil {
			return ScheduleRecord{}, err
		} else if !found {
			return ScheduleRecord{}, ErrScheduleNotFound
		}
		return ScheduleRecord{}, fmt.Errorf("%w: %s 期望版本 %d", ErrScheduleVersion, id, expectedVersion)
	}
	rec, found, err := s.Get(ctx, id)
	if err != nil {
		return ScheduleRecord{}, err
	}
	if !found {
		return ScheduleRecord{}, ErrScheduleNotFound
	}
	return rec, nil
}

// Delete 按期望版本删除一条计划（幂等：不存在时返回 nil）。
//
// 它只删定义。单元文件的卸载、timer 的 disable 由动作层做——仓储不认识 systemd。
func (s *ScheduleStore) Delete(ctx context.Context, id string, expectedVersion int64) error {
	res, err := s.db.store().ExecContext(ctx,
		`DELETE FROM schedules WHERE id = ? AND version = ?`, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("agentstore: 删除计划任务 %s 失败: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("agentstore: 删除计划任务 %s 失败: %w", id, err)
	}
	if affected > 0 {
		return nil
	}
	if _, found, err := s.Get(ctx, id); err != nil {
		return err
	} else if !found {
		return nil
	}
	return fmt.Errorf("%w: %s 期望版本 %d", ErrScheduleVersion, id, expectedVersion)
}

// SetTimerRef 记录（或清空）已落地的定时器单元名。
//
// 它**不**改 version：单元落地的成败不是定义变更，把它算进乐观并发只会让
// "又一次 enable 失败"把管理员的编辑撞出版本冲突。
func (s *ScheduleStore) SetTimerRef(ctx context.Context, id, ref string, now time.Time) (ScheduleRecord, error) {
	if _, err := s.db.store().ExecContext(ctx,
		`UPDATE schedules SET agent_timer_ref = ?, updated_at = ? WHERE id = ?`,
		ref, millis(now.UTC()), id); err != nil {
		return ScheduleRecord{}, fmt.Errorf("agentstore: 更新计划任务 %s 的单元引用失败: %w", id, err)
	}
	rec, found, err := s.Get(ctx, id)
	if err != nil {
		return ScheduleRecord{}, err
	}
	if !found {
		return ScheduleRecord{}, ErrScheduleNotFound
	}
	return rec, nil
}

// RecordTrigger 记一次触发。
//
// 返回 (已有的 operation_id, true, nil) 表示这条 (计划, 槽) 之前已经触发过——
// "稳定触发 id"就落在 UNIQUE (schedule_id, slot_at) 上：同一个槽里的第二次触发
// 在数据库层插不进去，于是 systemd 的重试与手工 systemctl start 都只会开出一个操作。
func (s *ScheduleStore) RecordTrigger(ctx context.Context, rec TriggerRecord) (string, bool, error) {
	_, err := s.db.store().ExecContext(ctx, `
		INSERT INTO schedule_triggers (trigger_id, schedule_id, slot_at, action, operation_id,
		                              outcome, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.TriggerID, rec.ScheduleID, rec.SlotAt.UTC().Format(time.RFC3339),
		rec.Action, rec.OperationID, rec.Outcome, rec.Reason, rec.CreatedAt.UTC().Format(time.RFC3339))
	if err == nil {
		return rec.OperationID, false, nil
	}
	if !isUniqueViolation(err) {
		return "", false, fmt.Errorf("agentstore: 记录计划触发失败: %w", err)
	}
	var existing string
	row := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT operation_id FROM schedule_triggers WHERE schedule_id = ? AND slot_at = ?`,
		rec.ScheduleID, rec.SlotAt.UTC().Format(time.RFC3339))
	if scanErr := row.Scan(&existing); scanErr != nil {
		return "", false, fmt.Errorf("agentstore: 读取已有触发记录失败: %w", scanErr)
	}
	return existing, true, nil
}

// ListTriggers 返回一条计划最近的触发记录（新的在前）。
func (s *ScheduleStore) ListTriggers(ctx context.Context, scheduleID string, limit int) ([]TriggerRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.store().SQL().QueryContext(ctx, `
		SELECT trigger_id, schedule_id, slot_at, action, operation_id, outcome, reason, created_at
		  FROM schedule_triggers WHERE schedule_id = ?
		 ORDER BY slot_at DESC LIMIT ?`, scheduleID, limit)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取触发记录失败: %w", err)
	}
	defer rows.Close()
	var out []TriggerRecord
	for rows.Next() {
		var (
			rec     TriggerRecord
			slot    string
			created string
		)
		if err := rows.Scan(&rec.TriggerID, &rec.ScheduleID, &slot, &rec.Action,
			&rec.OperationID, &rec.Outcome, &rec.Reason, &created); err != nil {
			return nil, fmt.Errorf("agentstore: 读取触发记录失败: %w", err)
		}
		rec.SlotAt, _ = time.Parse(time.RFC3339, slot)
		rec.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取触发记录失败: %w", err)
	}
	return out, nil
}

// getByName 按名字取一条计划（内部用，判断唯一冲突是哪一种）。
func (s *ScheduleStore) getByName(ctx context.Context, name string) (ScheduleRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, name, kind, target_id, definition_json, definition_key,
		       agent_timer_ref, enabled, version, created_at, updated_at
		  FROM schedules WHERE name = ?`, name)
	rec, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRecord{}, false, nil
	}
	if err != nil {
		return ScheduleRecord{}, false, err
	}
	return rec, true, nil
}

// scanSchedule 扫描一行。
func scanSchedule(row interface{ Scan(...any) error }) (ScheduleRecord, error) {
	var (
		rec       ScheduleRecord
		kind      string
		raw       string
		enabled   int
		createdAt int64
		updatedAt int64
	)
	if err := row.Scan(&rec.ID, &rec.Name, &kind, &rec.TargetID, &raw, &rec.DefinitionKey,
		&rec.TimerRef, &enabled, &rec.Version, &createdAt, &updatedAt); err != nil {
		return ScheduleRecord{}, err
	}
	rec.Kind = schedules.Kind(kind)
	rec.Enabled = enabled != 0
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	if err := json.Unmarshal([]byte(raw), &rec.Definition); err != nil {
		return ScheduleRecord{}, fmt.Errorf("agentstore: 计划 %s 的定义无法解析: %w", rec.ID, err)
	}
	return rec, nil
}

// newScheduleID 生成计划 id。
func newScheduleID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agentstore: 生成计划任务 id 失败: %w", err)
	}
	return schedulePrefix + hex.EncodeToString(b[:]), nil
}
