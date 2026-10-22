package agentstore

// 受管理卷的持久化（CONTRACTS.md 14.2 managed_resources；P07.4）。
//
// 这一层只做四件事：列、取、存、删。它**不判断**“这个卷能不能导入/挂载/卸载”——那些
// 判断在 storage 的判决层，因为只有那里同时看得到拓扑、超级块、fstab 与占用。
// 把判断搬到存储层，会比现在更难保证“预检与执行看到的是同一份事实”。
//
// 依赖方向是单向的：agentstore → storage（只用它的记录类型）。storage 永远不 import
// agentstore，所以判决与查询可以在没有数据库的情况下被单元测试。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"upanel/internal/storage"
)

// ErrResourceInvalid 表示要保存的记录本身不合法（编程错误，不是用户输入错误）。
var ErrResourceInvalid = errors.New("agentstore: 受管资源记录不合法")

// ResourceStore 是受管理资源的仓储。
type ResourceStore struct{ db *DB }

// NewResourceStore 构造仓储。db 为空时返回错误：卷管理是固定能力，
// “没有数据库也能跑”只会在第一次导入时炸掉。
func NewResourceStore(db *DB) (*ResourceStore, error) {
	if db == nil {
		return nil, errors.New("agentstore: 受管资源仓储需要数据库句柄")
	}
	return &ResourceStore{db: db}, nil
}

// 编译期确认它满足 storage 的接口：方法名或签名写错时在这里就炸，而不是在接线时。
var _ storage.VolumeStore = (*ResourceStore)(nil)

// ListVolumes 返回全部受管理卷（顺序由调用方排序）。
func (s *ResourceStore) ListVolumes(ctx context.Context) ([]storage.VolumeRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx, `
		SELECT id, display_name, slug, device_id, filesystem_uuid, filesystem_type,
		       identity_fingerprint, config_json, desired_state, version,
		       last_operation_id, created_at, updated_at
		  FROM managed_resources
		 WHERE kind = 'volume'`)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取受管理卷失败: %w", err)
	}
	defer rows.Close()
	var out []storage.VolumeRecord
	for rows.Next() {
		rec, err := scanVolume(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取受管理卷失败: %w", err)
	}
	return out, nil
}

// GetVolume 取一条记录；不存在时返回 (零值, false, nil)。
//
// “不存在”不是错误：调用方（判决层）要把它变成一条可展示的阻塞项
// （VOLUME_NOT_FOUND），而不是一个 500。
func (s *ResourceStore) GetVolume(ctx context.Context, id string) (storage.VolumeRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, display_name, slug, device_id, filesystem_uuid, filesystem_type,
		       identity_fingerprint, config_json, desired_state, version,
		       last_operation_id, created_at, updated_at
		  FROM managed_resources
		 WHERE kind = 'volume' AND id = ?`, id)
	rec, err := scanVolume(row)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.VolumeRecord{}, false, nil
	}
	if err != nil {
		return storage.VolumeRecord{}, false, err
	}
	return rec, true, nil
}

// SaveVolume 新建或覆盖一条记录。
//
// 用 UPSERT 而不是“先查再写”：并发导入时先查再写会两边都认为自己是新建的。
// 唯一索引 (filesystem_uuid) 与 (slug) 是最后一道：撞了就是撞了，报错而不是覆盖别人的卷。
func (s *ResourceStore) SaveVolume(ctx context.Context, rec storage.VolumeRecord) error {
	if err := validateVolumeRecord(rec); err != nil {
		return err
	}
	cfg, err := json.Marshal(rec.Config)
	if err != nil {
		return fmt.Errorf("agentstore: 序列化挂载配置失败: %w", err)
	}
	_, err = s.db.store().ExecContext(ctx, `
		INSERT INTO managed_resources (
			id, kind, display_name, slug, device_id, filesystem_uuid, filesystem_type,
			identity_fingerprint, config_json, desired_state, version,
			last_operation_id, created_at, updated_at
		) VALUES (?, 'volume', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			display_name         = excluded.display_name,
			slug                 = excluded.slug,
			device_id            = excluded.device_id,
			filesystem_uuid      = excluded.filesystem_uuid,
			filesystem_type      = excluded.filesystem_type,
			identity_fingerprint = excluded.identity_fingerprint,
			config_json          = excluded.config_json,
			desired_state        = excluded.desired_state,
			version              = excluded.version,
			last_operation_id    = excluded.last_operation_id,
			updated_at           = excluded.updated_at`,
		rec.ID, rec.DisplayName, rec.Slug, rec.DeviceID, rec.FilesystemUUID,
		rec.FilesystemType, rec.IdentityFingerprint, string(cfg), rec.DesiredState,
		rec.Version, rec.LastOperationID, millis(rec.CreatedAt), millis(rec.UpdatedAt))
	if err != nil {
		// 唯一索引冲突时把话说清楚：这两条约束是“两个卷不能指向同一个文件系统/
		// 同一个挂载点”的最终保障，报一句 UNIQUE 约束失败让人自己去猜是没道理的。
		if isUniqueViolation(err) {
			return fmt.Errorf("agentstore: 卷 %s（UUID=%s，挂载名 %s）与已有记录冲突："+
				"同一个文件系统或同一个挂载名不能有两个卷: %w",
				rec.ID, rec.FilesystemUUID, rec.Slug, err)
		}
		return fmt.Errorf("agentstore: 保存卷 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// DeleteVolume 删除一条记录；不存在时返回 nil（幂等）。
func (s *ResourceStore) DeleteVolume(ctx context.Context, id string) error {
	if _, err := s.db.store().ExecContext(ctx,
		`DELETE FROM managed_resources WHERE kind = 'volume' AND id = ?`, id); err != nil {
		return fmt.Errorf("agentstore: 删除卷 %s 失败: %w", id, err)
	}
	return nil
}

// 扫描共用 rowScanner（定义在 operations.go）：本包所有仓储都用同一份扫描契约，
// 少一个接口就少一处“只有某一种查询能编译”的意外。

// scanVolume 读一行。
func scanVolume(r rowScanner) (storage.VolumeRecord, error) {
	var (
		rec       storage.VolumeRecord
		cfgJSON   string
		createdAt int64
		updatedAt int64
	)
	err := r.Scan(&rec.ID, &rec.DisplayName, &rec.Slug, &rec.DeviceID,
		&rec.FilesystemUUID, &rec.FilesystemType, &rec.IdentityFingerprint,
		&cfgJSON, &rec.DesiredState, &rec.Version, &rec.LastOperationID,
		&createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.VolumeRecord{}, err
		}
		return storage.VolumeRecord{}, fmt.Errorf("agentstore: 读取受管理卷失败: %w", err)
	}
	// 配置解析失败不静默：一份读不懂的配置会让 fstab 条目变成猜的。
	if strings.TrimSpace(cfgJSON) != "" {
		if err := json.Unmarshal([]byte(cfgJSON), &rec.Config); err != nil {
			return storage.VolumeRecord{}, fmt.Errorf(
				"agentstore: 卷 %s 的挂载配置无法解析（%v）：%s", rec.ID, err, cfgJSON)
		}
	}
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

// validateVolumeRecord 在落库之前再检查一遍形状。
//
// 与数据库的 CHECK 约束重复是有意的：数据库报的是一句 SQL 错误，而这里的错话能被
// 判决层直接展示（“卷名不合法：…”），也更容易定位是哪一步写错了。
func validateVolumeRecord(rec storage.VolumeRecord) error {
	if !storage.ValidVolumeID(rec.ID) {
		return fmt.Errorf("%w: 卷标识 %q 不合法", ErrResourceInvalid, rec.ID)
	}
	if err := storage.ValidateSlug(rec.Slug); err != nil {
		return fmt.Errorf("%w: %v", ErrResourceInvalid, err)
	}
	if err := storage.ValidateDisplayName(rec.DisplayName); err != nil {
		return fmt.Errorf("%w: %v", ErrResourceInvalid, err)
	}
	if rec.FilesystemUUID == "" || len(rec.FilesystemUUID) != 36 {
		return fmt.Errorf("%w: 文件系统 UUID %q 不合法", ErrResourceInvalid, rec.FilesystemUUID)
	}
	if !storage.ValidFilesystem(rec.FilesystemType) {
		return fmt.Errorf("%w: 文件系统类型 %q 不在 ext4/xfs 内", ErrResourceInvalid, rec.FilesystemType)
	}
	switch rec.DesiredState {
	case storage.DesiredMounted, storage.DesiredUnmounted:
	default:
		return fmt.Errorf("%w: desired_state %q 不合法", ErrResourceInvalid, rec.DesiredState)
	}
	if rec.Version == "" {
		return fmt.Errorf("%w: version 不能为空", ErrResourceInvalid)
	}
	if rec.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at 不能为空", ErrResourceInvalid)
	}
	return nil
}
