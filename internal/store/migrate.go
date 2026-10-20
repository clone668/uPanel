package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// migrationFile 是一个已加载的迁移文件。
type migrationFile struct {
	version  int
	name     string
	content  string
	checksum string
}

// Migration 描述一条迁移及其应用状态。
type Migration struct {
	Version   int
	Name      string
	Checksum  string
	Applied   bool
	AppliedAt time.Time
}

// MigrateResult 描述一次迁移执行的结果。
type MigrateResult struct {
	// From 是执行前的 schema 版本（新库为 0）。
	From int
	// To 是执行后的 schema 版本。
	To int
	// Applied 是本次实际应用的迁移（按版本升序）。
	Applied []Migration
}

// AppliedVersions 返回本次实际应用的版本号，便于日志与 CLI 输出。
func (r MigrateResult) AppliedVersions() []int {
	out := make([]int, 0, len(r.Applied))
	for _, m := range r.Applied {
		out = append(out, m.Version)
	}
	return out
}

// 迁移文件名：<序号>_<小写名称>.sql，序号至少 4 位。
var migrationNamePattern = regexp.MustCompile(`^(\d{4,})_([a-z0-9_]+)\.sql$`)

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`

// Migrate 按版本顺序应用尚未执行的迁移。
//
// 规则（每一条都对应“迁移失败必须停止启动”的要求）：
//   - 每条迁移在独立事务中执行，失败则整条回滚并返回错误，调用方必须拒绝启动
//   - 已应用的迁移不允许被修改：内容校验和不一致直接报错，而不是静默重跑
//   - 数据库中的版本高于程序内置最高版本时拒绝启动（二进制降级会写坏数据）
//   - 迁移表由本函数创建，不从 SQL 文件里建，避免出现“迁移表本身也需要迁移”的循环
func (db *DB) Migrate(ctx context.Context, fsys fs.FS) (MigrateResult, error) {
	res := MigrateResult{}

	files, err := loadMigrations(fsys)
	if err != nil {
		return res, err
	}

	if _, err := db.sql.ExecContext(ctx, createSchemaMigrations); err != nil {
		return res, fmt.Errorf("store: 创建 schema_migrations 表失败: %w", err)
	}

	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return res, err
	}

	byVersion := make(map[int]migrationFile, len(files))
	maxAvailable := 0
	for _, f := range files {
		byVersion[f.version] = f
		if f.version > maxAvailable {
			maxAvailable = f.version
		}
	}

	maxApplied := 0
	for _, a := range applied {
		if a.Version > maxApplied {
			maxApplied = a.Version
		}
		if f, ok := byVersion[a.Version]; !ok {
			return res, fmt.Errorf("store: 数据库已应用迁移 %04d，但程序中不存在该迁移（疑似二进制降级或迁移文件被删除）", a.Version)
		} else if f.checksum != a.Checksum {
			return res, fmt.Errorf("store: 迁移 %04d_%s 的内容与已应用版本不一致（校验和不同）；已应用的迁移不可修改，请新增一条迁移",
				a.Version, a.Name)
		}
	}
	if maxApplied > maxAvailable {
		return res, fmt.Errorf("store: 数据库 schema 版本为 %d，高于程序内置的最高版本 %d；请升级程序而不是降级数据库", maxApplied, maxAvailable)
	}
	res.From = maxApplied

	for _, f := range files {
		if f.version <= maxApplied {
			continue
		}
		appliedAt, err := db.applyMigration(ctx, f)
		if err != nil {
			return res, err
		}
		db.logger.Info("已应用数据库迁移",
			"version", f.version, "name", f.name, "applied_at", appliedAt)
		res.Applied = append(res.Applied, Migration{
			Version:   f.version,
			Name:      f.name,
			Checksum:  f.checksum,
			Applied:   true,
			AppliedAt: appliedAt,
		})
	}

	res.To = maxApplied
	if len(res.Applied) > 0 {
		res.To = res.Applied[len(res.Applied)-1].Version
	}
	// 迁移会写库，WAL/SHM 附属文件可能到这一步才出现，因此再收紧一次权限。
	if err := db.tightenFileModes(); err != nil {
		return res, err
	}
	return res, nil
}

// Migrations 返回全部迁移及其应用状态，不执行任何 DDL。供 CLI 与诊断使用。
func (db *DB) Migrations(ctx context.Context, fsys fs.FS) ([]Migration, error) {
	files, err := loadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	if _, err := db.sql.ExecContext(ctx, createSchemaMigrations); err != nil {
		return nil, fmt.Errorf("store: 创建 schema_migrations 表失败: %w", err)
	}
	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return nil, err
	}
	appliedByVersion := make(map[int]Migration, len(applied))
	for _, a := range applied {
		appliedByVersion[a.Version] = a
	}

	out := make([]Migration, 0, len(files))
	for _, f := range files {
		m := Migration{Version: f.version, Name: f.name, Checksum: f.checksum}
		if a, ok := appliedByVersion[f.version]; ok {
			m.Applied = true
			m.AppliedAt = a.AppliedAt
		}
		out = append(out, m)
	}
	return out, nil
}

// SchemaVersion 返回当前已应用的最高版本（新库为 0）。
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	if _, err := db.sql.ExecContext(ctx, createSchemaMigrations); err != nil {
		return 0, fmt.Errorf("store: 创建 schema_migrations 表失败: %w", err)
	}
	var version sql.NullInt64
	err := db.sql.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("store: 读取 schema 版本失败: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

func (db *DB) applyMigration(ctx context.Context, f migrationFile) (time.Time, error) {
	appliedAt := time.Now().UTC().Truncate(time.Second)
	err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, f.content); err != nil {
			return fmt.Errorf("store: 执行迁移 %04d_%s 失败: %w", f.version, f.name, err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			f.version, f.name, f.checksum, appliedAt.Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("store: 记录迁移 %04d_%s 失败: %w", f.version, f.name, err)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return appliedAt, nil
}

func (db *DB) appliedMigrations(ctx context.Context) ([]Migration, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取已应用迁移失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Migration
	for rows.Next() {
		var m Migration
		var appliedAt string
		if err := rows.Scan(&m.Version, &m.Name, &m.Checksum, &appliedAt); err != nil {
			return nil, fmt.Errorf("store: 解析迁移记录失败: %w", err)
		}
		t, err := time.Parse(time.RFC3339, appliedAt)
		if err != nil {
			return nil, fmt.Errorf("store: 迁移 %04d 的时间格式非法 %q: %w", m.Version, appliedAt, err)
		}
		m.Applied = true
		m.AppliedAt = t
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历迁移记录失败: %w", err)
	}
	return out, nil
}

func loadMigrations(fsys fs.FS) ([]migrationFile, error) {
	if fsys == nil {
		return nil, errors.New("store: 迁移文件集合为空")
	}
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("store: 枚举迁移文件失败: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("store: 没有找到任何迁移文件")
	}

	files := make([]migrationFile, 0, len(names))
	seen := make(map[int]string, len(names))
	for _, name := range names {
		m := migrationNamePattern.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("store: 迁移文件名不合规 %q（应为 0001_name.sql）", name)
		}
		version, err := strconv.Atoi(m[1])
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("store: 迁移文件名中的序号非法 %q", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: 迁移序号重复 %d（%s 与 %s）", version, other, name)
		}
		seen[version] = name

		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("store: 读取迁移文件 %s 失败: %w", name, err)
		}
		if len(content) == 0 {
			return nil, fmt.Errorf("store: 迁移文件 %s 为空", name)
		}
		sum := sha256.Sum256(content)
		files = append(files, migrationFile{
			version:  version,
			name:     m[2],
			content:  string(content),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}
