package agentstore

// 备份仓库定义的持久化（CONTRACTS.md 第 12、14.2 节；P12.1）。
//
// 这一层只做五件事：列、取、建、改、删。**不判断**“这个仓库能不能初始化”——
// 那要读真实现场（根表、restic、SFTP 主机密钥、容量），判断在 internal/backup。
// 把判断搬进来，就会出现“数据库说有，机器上说没有”的分裂。
//
// 与 Web 侧 `backup_repositories` 表的关系：Web 存一份（管理界面、审计关联），
// 代理存这一份（执行依据）。两边用同一个 id 与同一个 version；代理**不信** Web 传来的
// 定义，只认自己库里这份——写入必须走 /internal/v1/backup/repositories。

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

// BackupRepositoryRecord 是代理镜像里的一条仓库定义。
type BackupRepositoryRecord struct {
	ID         string
	Name       string
	Definition backup.Definition
	// Version 是乐观并发令牌：每次成功修改自增。
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

var (
	// ErrRepositoryNotFound 表示仓库 id 不在镜像里。
	ErrRepositoryNotFound = errors.New("agentstore: 备份仓库不存在")
	// ErrRepositoryVersion 表示期望版本与当前版本不一致（乐观并发冲突）。
	ErrRepositoryVersion = errors.New("agentstore: 备份仓库版本冲突")
	// ErrRepositoryName 表示名字与另一条仓库重复。
	ErrRepositoryName = errors.New("agentstore: 备份仓库名字已被占用")
)

// refPrefix 之外，仓库 id 的前缀。
const repositoryPrefix = "rep_"

// BackupRepositoryStore 是仓库定义的仓储。
type BackupRepositoryStore struct{ db *DB }

// NewBackupRepositoryStore 构造仓储。
func NewBackupRepositoryStore(db *DB) (*BackupRepositoryStore, error) {
	if db == nil {
		return nil, errors.New("agentstore: 备份仓库仓储需要数据库句柄")
	}
	return &BackupRepositoryStore{db: db}, nil
}

// 编译期确认它满足 backup 的镜像接口。
var _ backup.RepositorySource = (*BackupRepositoryStore)(nil)

// List 返回全部仓库记录（顺序由调用方定）。
func (s *BackupRepositoryStore) List(ctx context.Context) ([]BackupRepositoryRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx, `
		SELECT id, name, definition_json, version, created_at, updated_at
		  FROM backup_repositories`)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取备份仓库列表失败: %w", err)
	}
	defer rows.Close()
	var out []BackupRepositoryRecord
	for rows.Next() {
		rec, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取备份仓库列表失败: %w", err)
	}
	return out, nil
}

// Get 取一条记录；不存在时返回 (零值, false, nil)。
//
// “不存在”不是错误：动作层要把它变成一条可展示的阻塞项，而不是一个 500。
func (s *BackupRepositoryStore) Get(ctx context.Context, id string) (BackupRepositoryRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, name, definition_json, version, created_at, updated_at
		  FROM backup_repositories WHERE id = ?`, id)
	rec, err := scanRepository(row)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupRepositoryRecord{}, false, nil
	}
	if err != nil {
		return BackupRepositoryRecord{}, false, err
	}
	return rec, true, nil
}

// GetRepository 实现 backup.RepositorySource。
func (s *BackupRepositoryStore) GetRepository(ctx context.Context, id string) (backup.Repository, bool, error) {
	rec, ok, err := s.Get(ctx, id)
	if err != nil || !ok {
		return backup.Repository{}, ok, err
	}
	return backup.Repository{
		ID:         rec.ID,
		Name:       rec.Name,
		Definition: rec.Definition,
		Version:    rec.Version,
	}, true, nil
}

// Create 新建一条记录并生成 id。
//
// 定义必须已经 Normalize 过（调用方负责）：这一层只保证“定义能被序列化”与
// “名字不撞车”，形状校验只有一处，就是 internal/backup。
func (s *BackupRepositoryStore) Create(ctx context.Context, def backup.Definition, now time.Time) (BackupRepositoryRecord, error) {
	now = now.UTC()
	for attempt := 0; attempt < 2; attempt++ {
		id, err := newRepositoryID()
		if err != nil {
			return BackupRepositoryRecord{}, err
		}
		rec := BackupRepositoryRecord{
			ID:         id,
			Name:       def.Name,
			Definition: def,
			Version:    1,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := s.insert(ctx, rec); err != nil {
			if errors.Is(err, ErrRepositoryName) {
				return BackupRepositoryRecord{}, err
			}
			if attempt == 0 && isUniqueViolation(err) {
				// 36 字符随机 id 撞车的概率可以忽略，撞了就换一个再试一次。
				continue
			}
			return BackupRepositoryRecord{}, err
		}
		return rec, nil
	}
	return BackupRepositoryRecord{}, errors.New("agentstore: 生成备份仓库 id 连续冲突")
}

// Update 按期望版本更新一条记录，成功后 version+1。
func (s *BackupRepositoryStore) Update(ctx context.Context, id string, def backup.Definition,
	expectedVersion int64, now time.Time) (BackupRepositoryRecord, error) {
	now = now.UTC()
	raw, err := json.Marshal(def)
	if err != nil {
		return BackupRepositoryRecord{}, fmt.Errorf("agentstore: 序列化仓库定义失败: %w", err)
	}
	res, err := s.db.store().ExecContext(ctx, `
		UPDATE backup_repositories
		   SET name = ?, backend = ?, definition_json = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		def.Name, string(def.Backend), string(raw), millis(now), id, expectedVersion)
	if err != nil {
		if isUniqueViolation(err) {
			return BackupRepositoryRecord{}, fmt.Errorf("%w: %s", ErrRepositoryName, def.Name)
		}
		return BackupRepositoryRecord{}, fmt.Errorf("agentstore: 更新备份仓库 %s 失败: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return BackupRepositoryRecord{}, fmt.Errorf("agentstore: 更新备份仓库 %s 失败: %w", id, err)
	}
	if affected == 0 {
		// 要分清“没有这条记录”与“版本不对”：前者是 404，后者是 409。
		if _, ok, err := s.Get(ctx, id); err != nil {
			return BackupRepositoryRecord{}, err
		} else if !ok {
			return BackupRepositoryRecord{}, ErrRepositoryNotFound
		}
		return BackupRepositoryRecord{}, fmt.Errorf("%w: %s 期望版本 %d", ErrRepositoryVersion, id, expectedVersion)
	}
	rec, ok, err := s.Get(ctx, id)
	if err != nil {
		return BackupRepositoryRecord{}, err
	}
	if !ok {
		return BackupRepositoryRecord{}, ErrRepositoryNotFound
	}
	return rec, nil
}

// Delete 删除一条记录（幂等：不存在时返回 nil）。
//
// 只删定义，**不删仓库、不删快照**（CONTRACTS.md 第 12 节：删除策略不删除仓库；
// 仓库数据的清理只能通过显式的 backup.prune）。
func (s *BackupRepositoryStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.store().ExecContext(ctx,
		`DELETE FROM backup_repositories WHERE id = ?`, id); err != nil {
		return fmt.Errorf("agentstore: 删除备份仓库 %s 失败: %w", id, err)
	}
	return nil
}

// insert 写一条新记录。
func (s *BackupRepositoryStore) insert(ctx context.Context, rec BackupRepositoryRecord) error {
	raw, err := json.Marshal(rec.Definition)
	if err != nil {
		return fmt.Errorf("agentstore: 序列化仓库定义失败: %w", err)
	}
	_, err = s.db.store().ExecContext(ctx, `
		INSERT INTO backup_repositories (id, name, backend, definition_json, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Name, string(rec.Definition.Backend), string(raw), rec.Version,
		millis(rec.CreatedAt), millis(rec.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s", ErrRepositoryName, rec.Name)
		}
		return fmt.Errorf("agentstore: 写入备份仓库 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// scanRepository 扫描一行。
func scanRepository(row interface{ Scan(...any) error }) (BackupRepositoryRecord, error) {
	var (
		rec       BackupRepositoryRecord
		raw       string
		createdAt int64
		updatedAt int64
	)
	if err := row.Scan(&rec.ID, &rec.Name, &raw, &rec.Version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BackupRepositoryRecord{}, err
		}
		return BackupRepositoryRecord{}, fmt.Errorf("agentstore: 读取备份仓库失败: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &rec.Definition); err != nil {
		return BackupRepositoryRecord{}, fmt.Errorf("agentstore: 仓库 %s 的定义无法解析: %w", rec.ID, err)
	}
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

// ListRepositories 实现代理内部镜像的列表（返回值类型，不含时间戳）。
func (s *BackupRepositoryStore) ListRepositories(ctx context.Context) ([]backup.Repository, error) {
	records, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backup.Repository, 0, len(records))
	for _, rec := range records {
		out = append(out, repositoryOf(rec))
	}
	return out, nil
}

// CreateRepository 实现代理内部镜像的新建。
func (s *BackupRepositoryStore) CreateRepository(ctx context.Context, def backup.Definition,
	now time.Time) (backup.Repository, error) {
	rec, err := s.Create(ctx, def, now)
	if err != nil {
		return backup.Repository{}, err
	}
	return repositoryOf(rec), nil
}

// UpdateRepository 实现代理内部镜像的更新（按期望版本）。
func (s *BackupRepositoryStore) UpdateRepository(ctx context.Context, id string, def backup.Definition,
	expectedVersion int64, now time.Time) (backup.Repository, error) {
	rec, err := s.Update(ctx, id, def, expectedVersion, now)
	if err != nil {
		return backup.Repository{}, err
	}
	return repositoryOf(rec), nil
}

// DeleteRepository 实现代理内部镜像的删除（只删定义，不动仓库数据）。
func (s *BackupRepositoryStore) DeleteRepository(ctx context.Context, id string) error {
	return s.Delete(ctx, id)
}

// repositoryOf 去掉记录里的时间戳，只留镜像对外的四个字段。
func repositoryOf(rec BackupRepositoryRecord) backup.Repository {
	return backup.Repository{ID: rec.ID, Name: rec.Name, Definition: rec.Definition, Version: rec.Version}
}

// newRepositoryID 生成 rep_ + 32 位小写十六进制。
func newRepositoryID() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("agentstore: 生成备份仓库 id 失败: %w", err)
	}
	defer wipe(buf)
	return repositoryPrefix + hex.EncodeToString(buf), nil
}

// ValidRepositoryID 校验仓库 id 的形态（供路由层用）。
func ValidRepositoryID(id string) bool {
	if len(id) != 36 || len(id) < len(repositoryPrefix) || id[:len(repositoryPrefix)] != repositoryPrefix {
		return false
	}
	for _, r := range id[len(repositoryPrefix):] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
