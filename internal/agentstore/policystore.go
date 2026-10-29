package agentstore

// 备份策略的持久化（CONTRACTS.md 第 12、14.2 节；P12.2）。
//
// 与仓库镜像同一个道理：`backup.run` 的参数是冻结的 `policy_id` + `version`，
// 代理必须能从**自己**的库里按 id 找到策略并核对版本，否则“预检时答应备份这三个目录、
// 执行时换成另三个”就没有东西挡得住。
//
// 这一层只做五件事：列、取、建、改、删。**不判断**“这条策略现在能不能跑”——
// 那要读真实现场（根表、仓库存不存在、应用还登记着没有、restic 在不在），
// 判断在 internal/backup 与代理动作层。
//
// 删策略不删仓库、不删快照（CONTRACTS 第 12 节）。这里连仓库表都不碰。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"upanel/internal/backup"
)

// BackupPolicyRecord 是代理镜像里的一条策略。
type BackupPolicyRecord struct {
	ID        string
	Name      string
	Policy    backup.Policy
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

var (
	// ErrPolicyNotFound 表示策略 id 不在镜像里。
	ErrPolicyNotFound = errors.New("agentstore: 备份策略不存在")
	// ErrPolicyVersion 表示期望版本与当前版本不一致（乐观并发冲突）。
	ErrPolicyVersion = errors.New("agentstore: 备份策略版本冲突")
	// ErrPolicyName 表示名字与另一条策略重复。
	ErrPolicyName = errors.New("agentstore: 备份策略名字已被占用")
)

// policyPrefix 是策略 id 的前缀。
const policyPrefix = "pol_"

// BackupPolicyStore 是策略的仓储。
type BackupPolicyStore struct{ db *DB }

// NewBackupPolicyStore 构造仓储。
func NewBackupPolicyStore(db *DB) (*BackupPolicyStore, error) {
	if db == nil {
		return nil, errors.New("agentstore: 备份策略仓储需要数据库句柄")
	}
	return &BackupPolicyStore{db: db}, nil
}

// 编译期确认它满足 backup 的策略镜像接口。
var _ backup.PolicySource = (*BackupPolicyStore)(nil)

// List 返回全部策略记录（顺序由调用方定）。
func (s *BackupPolicyStore) List(ctx context.Context) ([]BackupPolicyRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx, `
		SELECT id, name, policy_json, version, created_at, updated_at
		  FROM backup_policies`)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取备份策略列表失败: %w", err)
	}
	defer rows.Close()
	var out []BackupPolicyRecord
	for rows.Next() {
		rec, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取备份策略列表失败: %w", err)
	}
	return out, nil
}

// Get 取一条记录；不存在时返回 (零值, false, nil)。
func (s *BackupPolicyStore) Get(ctx context.Context, id string) (BackupPolicyRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, name, policy_json, version, created_at, updated_at
		  FROM backup_policies WHERE id = ?`, id)
	rec, err := scanPolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupPolicyRecord{}, false, nil
	}
	if err != nil {
		return BackupPolicyRecord{}, false, err
	}
	return rec, true, nil
}

// GetPolicy 实现 backup.PolicySource。
func (s *BackupPolicyStore) GetPolicy(ctx context.Context, id string) (backup.PolicyRecord, bool, error) {
	rec, ok, err := s.Get(ctx, id)
	if err != nil || !ok {
		return backup.PolicyRecord{}, ok, err
	}
	return backup.PolicyRecord{ID: rec.ID, Name: rec.Name, Policy: rec.Policy, Version: rec.Version}, true, nil
}

// Create 新建一条记录并生成 id。策略必须已经 Normalize 过（调用方负责）。
func (s *BackupPolicyStore) Create(ctx context.Context, p backup.Policy, now time.Time) (BackupPolicyRecord, error) {
	now = now.UTC()
	for attempt := 0; attempt < 2; attempt++ {
		id, err := newPolicyID()
		if err != nil {
			return BackupPolicyRecord{}, err
		}
		rec := BackupPolicyRecord{ID: id, Name: p.Name, Policy: p, Version: 1, CreatedAt: now, UpdatedAt: now}
		if err := s.insert(ctx, rec); err != nil {
			if errors.Is(err, ErrPolicyName) {
				return BackupPolicyRecord{}, err
			}
			if attempt == 0 && isUniqueViolation(err) {
				continue
			}
			return BackupPolicyRecord{}, err
		}
		return rec, nil
	}
	return BackupPolicyRecord{}, errors.New("agentstore: 生成备份策略 id 连续冲突")
}

// Update 按期望版本更新一条记录，成功后 version+1。
func (s *BackupPolicyStore) Update(ctx context.Context, id string, p backup.Policy,
	expectedVersion int64, now time.Time) (BackupPolicyRecord, error) {
	now = now.UTC()
	raw, err := json.Marshal(p)
	if err != nil {
		return BackupPolicyRecord{}, fmt.Errorf("agentstore: 序列化备份策略失败: %w", err)
	}
	res, err := s.db.store().ExecContext(ctx, `
		UPDATE backup_policies
		   SET name = ?, repository_id = ?, consistency_mode = ?, policy_json = ?,
		       version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		p.Name, p.RepositoryID, string(p.ConsistencyMode), string(raw), millis(now), id, expectedVersion)
	if err != nil {
		if isUniqueViolation(err) {
			return BackupPolicyRecord{}, fmt.Errorf("%w: %s", ErrPolicyName, p.Name)
		}
		return BackupPolicyRecord{}, fmt.Errorf("agentstore: 更新备份策略 %s 失败: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return BackupPolicyRecord{}, fmt.Errorf("agentstore: 更新备份策略 %s 失败: %w", id, err)
	}
	if affected == 0 {
		// 要分清“没有这条记录”（404）与“版本不对”（409）。
		if _, ok, err := s.Get(ctx, id); err != nil {
			return BackupPolicyRecord{}, err
		} else if !ok {
			return BackupPolicyRecord{}, ErrPolicyNotFound
		}
		return BackupPolicyRecord{}, fmt.Errorf("%w: %s 期望版本 %d", ErrPolicyVersion, id, expectedVersion)
	}
	rec, ok, err := s.Get(ctx, id)
	if err != nil {
		return BackupPolicyRecord{}, err
	}
	if !ok {
		return BackupPolicyRecord{}, ErrPolicyNotFound
	}
	return rec, nil
}

// Delete 删除一条记录（幂等：不存在时返回 nil）。
//
// 只删策略，**不删仓库、不删快照**（CONTRACTS.md 第 12 节）。
func (s *BackupPolicyStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.store().ExecContext(ctx,
		`DELETE FROM backup_policies WHERE id = ?`, id); err != nil {
		return fmt.Errorf("agentstore: 删除备份策略 %s 失败: %w", id, err)
	}
	return nil
}

// insert 写一条新记录。
func (s *BackupPolicyStore) insert(ctx context.Context, rec BackupPolicyRecord) error {
	raw, err := json.Marshal(rec.Policy)
	if err != nil {
		return fmt.Errorf("agentstore: 序列化备份策略失败: %w", err)
	}
	_, err = s.db.store().ExecContext(ctx, `
		INSERT INTO backup_policies (id, name, repository_id, consistency_mode, policy_json,
		                             version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Name, rec.Policy.RepositoryID, string(rec.Policy.ConsistencyMode), string(raw),
		rec.Version, millis(rec.CreatedAt), millis(rec.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s", ErrPolicyName, rec.Name)
		}
		return fmt.Errorf("agentstore: 写入备份策略 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// scanPolicy 扫描一行。
func scanPolicy(row interface{ Scan(...any) error }) (BackupPolicyRecord, error) {
	var (
		rec       BackupPolicyRecord
		raw       string
		createdAt int64
		updatedAt int64
	)
	if err := row.Scan(&rec.ID, &rec.Name, &raw, &rec.Version, &createdAt, &updatedAt); err != nil {
		return BackupPolicyRecord{}, err
	}
	if err := json.Unmarshal([]byte(raw), &rec.Policy); err != nil {
		return BackupPolicyRecord{}, fmt.Errorf("agentstore: 策略 %s 的内容无法解析: %w", rec.ID, err)
	}
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

// ListPolicies 实现代理内部镜像的列表。
func (s *BackupPolicyStore) ListPolicies(ctx context.Context) ([]backup.PolicyRecord, error) {
	records, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backup.PolicyRecord, 0, len(records))
	for _, rec := range records {
		out = append(out, policyRecordOf(rec))
	}
	return out, nil
}

// CreatePolicy 实现代理内部镜像的新建。
func (s *BackupPolicyStore) CreatePolicy(ctx context.Context, p backup.Policy, now time.Time) (backup.PolicyRecord, error) {
	rec, err := s.Create(ctx, p, now)
	if err != nil {
		return backup.PolicyRecord{}, err
	}
	return policyRecordOf(rec), nil
}

// UpdatePolicy 实现代理内部镜像的更新（按期望版本）。
func (s *BackupPolicyStore) UpdatePolicy(ctx context.Context, id string, p backup.Policy,
	expectedVersion int64, now time.Time) (backup.PolicyRecord, error) {
	rec, err := s.Update(ctx, id, p, expectedVersion, now)
	if err != nil {
		return backup.PolicyRecord{}, err
	}
	return policyRecordOf(rec), nil
}

// DeletePolicy 实现代理内部镜像的删除（只删策略）。
func (s *BackupPolicyStore) DeletePolicy(ctx context.Context, id string) error {
	return s.Delete(ctx, id)
}

// PoliciesForRepository 返回引用某个仓库的策略（删仓库前要告诉人“谁在用它”）。
func (s *BackupPolicyStore) PoliciesForRepository(ctx context.Context, repositoryID string) ([]backup.PolicyRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx, `
		SELECT id, name, policy_json, version, created_at, updated_at
		  FROM backup_policies WHERE repository_id = ? ORDER BY name`, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取仓库 %s 的策略失败: %w", repositoryID, err)
	}
	defer rows.Close()
	var out []backup.PolicyRecord
	for rows.Next() {
		rec, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, policyRecordOf(rec))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取仓库 %s 的策略失败: %w", repositoryID, err)
	}
	return out, nil
}

// policyRecordOf 去掉记录里的时间戳。
func policyRecordOf(rec BackupPolicyRecord) backup.PolicyRecord {
	return backup.PolicyRecord{ID: rec.ID, Name: rec.Name, Policy: rec.Policy, Version: rec.Version}
}

// newPolicyID 生成 pol_ + 32 位小写十六进制。
func newPolicyID() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("agentstore: 生成备份策略 id 失败: %w", err)
	}
	defer wipe(buf)
	return policyPrefix + hex.EncodeToString(buf), nil
}

// ValidPolicyID 校验策略 id 的形态（供路由层用）。
//
// 形状定义在 internal/backup（backup.ValidPolicyID）：Web 侧展开 backup.run 参数时
// 也要判同一个形状，而 Web 不该为了一个正则把这张表所在的存储层拉进二进制。
// 这里只做转发，保证两处永远是同一个判据。
func ValidPolicyID(id string) bool { return backup.ValidPolicyID(id) }
