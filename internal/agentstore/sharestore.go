package agentstore

// 共享定义与受管理账号的持久化（CONTRACTS.md 7、14.2；P08.1）。
//
// 这一层同样只做四件事：列、取、存、删。判断“这条共享能不能建/能不能删”在 shares 的
// 判决层——那里同时看得到卷的现场视图、目录的 Lstat 结果、系统的 passwd/passdb 读回与
// Samba 能力。把这些判断搬进存储层，就没有地方能保证“预检与执行看到同一份事实”了。
//
// 依赖方向：agentstore → shares（只 import 它的记录类型与形状校验）。
// shares 永远不 import agentstore，所以判决层可以在没有数据库的情况下被单元测试。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"upanel/internal/shares"
)

// ShareStore 是共享与受管理账号的仓储。
type ShareStore struct{ db *DB }

// NewShareStore 构造仓储。db 为空时返回错误：共享管理是固定能力，
// “没有数据库也能跑”只会在第一次保存共享时炸掉。
func NewShareStore(db *DB) (*ShareStore, error) {
	if db == nil {
		return nil, errors.New("agentstore: 共享仓储需要数据库句柄")
	}
	return &ShareStore{db: db}, nil
}

// 编译期确认它满足 shares 的接口：方法名或签名写错时在这里就炸，而不是在接线时。
var _ shares.Store = (*ShareStore)(nil)

// sharesColumns 是两条查询共用的列序。列序在三个地方（SELECT、扫描、INSERT）必须一致，
// 写成一个常量比写三遍靠眼力对齐可靠。
//
// 加字段时这四处（迁移、这个常量、scanShare、SaveShare）是同一件事的四个面：少一面不会
// 编译失败，只会安静地少一个能力——`runtime_revoked_json` 就是这么漏过一轮的（Q240）。
const sharesColumns = `id, name, protocol, volume_id, relative_path, enabled, read_only,
	definition_json, config_hash, directory_prepared, config_applied, version,
	last_operation_id, created_at, updated_at, runtime_revoked_json`

const accountsColumns = `id, name, type, managed, disabled, has_password, uid, gid,
	shell, home, secret_ref, version, last_operation_id, created_at, updated_at`

// ListShares 返回全部共享（顺序由调用方排序）。
func (s *ShareStore) ListShares(ctx context.Context) ([]shares.ShareRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx,
		`SELECT `+sharesColumns+` FROM shares`)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取共享列表失败: %w", err)
	}
	defer rows.Close()
	var out []shares.ShareRecord
	for rows.Next() {
		rec, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取共享列表失败: %w", err)
	}
	return out, nil
}

// GetShare 取一条共享；不存在时返回 (零值, false, nil)。
//
// “不存在”不是错误：判决层要把它变成一条可展示的阻塞项（SHARE_NOT_FOUND），
// 而不是一个 500。
func (s *ShareStore) GetShare(ctx context.Context, id string) (shares.ShareRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT `+sharesColumns+` FROM shares WHERE id = ?`, id)
	return oneShare(row)
}

// GetShareByName 按共享名取。名字在数据库里唯一，所以最多一条。
func (s *ShareStore) GetShareByName(ctx context.Context, name string) (shares.ShareRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT `+sharesColumns+` FROM shares WHERE name = ?`, name)
	return oneShare(row)
}

func oneShare(row rowScanner) (shares.ShareRecord, bool, error) {
	rec, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return shares.ShareRecord{}, false, nil
	}
	if err != nil {
		return shares.ShareRecord{}, false, err
	}
	return rec, true, nil
}

// SaveShare 新建或覆盖一条共享。
//
// 用 UPSERT 而不是“先查再写”：并发保存时先查再写会两边都认为自己是新建的。
// 唯一索引 (name) 与外键 (volume_id) 是最后一道：撞了就是撞了。
func (s *ShareStore) SaveShare(ctx context.Context, rec shares.ShareRecord) error {
	if err := validateShareRecord(rec); err != nil {
		return err
	}
	def, err := json.Marshal(rec.Definition)
	if err != nil {
		return fmt.Errorf("agentstore: 序列化共享定义失败: %w", err)
	}
	// 撤销现场是可选字段：没有撤销过就写空串，而不是写一坨 "null"。
	// 空串是“没有”，读回时据此得到 nil——两者必须一一对应，否则页面会把
	// 一条从没被撤销过的共享显示成“撤销过，原因未知”。
	revoked := ""
	if rec.RuntimeRevoked != nil {
		rv, err := json.Marshal(rec.RuntimeRevoked)
		if err != nil {
			return fmt.Errorf("agentstore: 序列化共享 %s 的撤销现场失败: %w", rec.ID, err)
		}
		revoked = string(rv)
	}
	_, err = s.db.store().ExecContext(ctx, `
		INSERT INTO shares (
			id, name, protocol, volume_id, relative_path, enabled, read_only,
			definition_json, config_hash, directory_prepared, config_applied, version,
			last_operation_id, created_at, updated_at, runtime_revoked_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name               = excluded.name,
			protocol           = excluded.protocol,
			volume_id          = excluded.volume_id,
			relative_path      = excluded.relative_path,
			enabled            = excluded.enabled,
			read_only          = excluded.read_only,
			definition_json    = excluded.definition_json,
			config_hash        = excluded.config_hash,
			directory_prepared = excluded.directory_prepared,
			config_applied     = excluded.config_applied,
			version            = excluded.version,
			last_operation_id  = excluded.last_operation_id,
			updated_at         = excluded.updated_at,
			runtime_revoked_json = excluded.runtime_revoked_json`,
		rec.ID, rec.Definition.Name, rec.Definition.Protocol, rec.Definition.VolumeID,
		rec.Definition.RelativePath, boolInt(rec.Definition.Enabled), boolInt(rec.Definition.ReadOnly),
		string(def), rec.ConfigHash, boolInt(rec.DirectoryPrepared), boolInt(rec.ConfigApplied),
		rec.Version, rec.LastOperationID, millis(rec.CreatedAt), millis(rec.UpdatedAt), revoked)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("agentstore: 共享 %s（名字 %s）与已有记录冲突："+
				"同一个共享名不能有两条定义: %w", rec.ID, rec.Definition.Name, err)
		}
		if isForeignKeyViolation(err) {
			return fmt.Errorf("agentstore: 共享 %s 绑定的卷 %s 不在受管理卷里: %w",
				rec.ID, rec.Definition.VolumeID, err)
		}
		return fmt.Errorf("agentstore: 保存共享 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// DeleteShare 删除一条共享记录；不存在时返回 nil（幂等）。
//
// 只删记录，不删目录：共享的“删除”在任何阶段都只移除导出配置。
func (s *ShareStore) DeleteShare(ctx context.Context, id string) error {
	if _, err := s.db.store().ExecContext(ctx,
		`DELETE FROM shares WHERE id = ?`, id); err != nil {
		return fmt.Errorf("agentstore: 删除共享 %s 失败: %w", id, err)
	}
	return nil
}

// ListAccounts 返回全部受管理账号（顺序由调用方排序）。
func (s *ShareStore) ListAccounts(ctx context.Context) ([]shares.AccountRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx,
		`SELECT `+accountsColumns+` FROM managed_accounts`)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取受管理账号失败: %w", err)
	}
	defer rows.Close()
	var out []shares.AccountRecord
	for rows.Next() {
		rec, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取受管理账号失败: %w", err)
	}
	return out, nil
}

// GetAccount 取一条账号；不存在时返回 (零值, false, nil)。
func (s *ShareStore) GetAccount(ctx context.Context, id string) (shares.AccountRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT `+accountsColumns+` FROM managed_accounts WHERE id = ?`, id)
	return oneAccount(row)
}

// GetAccountByName 按系统用户名取。
func (s *ShareStore) GetAccountByName(ctx context.Context, name string) (shares.AccountRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT `+accountsColumns+` FROM managed_accounts WHERE name = ?`, name)
	return oneAccount(row)
}

func oneAccount(row rowScanner) (shares.AccountRecord, bool, error) {
	rec, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return shares.AccountRecord{}, false, nil
	}
	if err != nil {
		return shares.AccountRecord{}, false, err
	}
	return rec, true, nil
}

// SaveAccount 新建或覆盖一条受管理账号。
func (s *ShareStore) SaveAccount(ctx context.Context, rec shares.AccountRecord) error {
	if err := validateAccountRecord(rec); err != nil {
		return err
	}
	_, err := s.db.store().ExecContext(ctx, `
		INSERT INTO managed_accounts (
			id, name, type, managed, disabled, has_password, uid, gid,
			shell, home, secret_ref, version, last_operation_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name              = excluded.name,
			type              = excluded.type,
			managed           = excluded.managed,
			disabled          = excluded.disabled,
			has_password      = excluded.has_password,
			uid               = excluded.uid,
			gid               = excluded.gid,
			shell             = excluded.shell,
			home              = excluded.home,
			secret_ref        = excluded.secret_ref,
			version           = excluded.version,
			last_operation_id = excluded.last_operation_id,
			updated_at        = excluded.updated_at`,
		rec.ID, rec.Name, rec.Type, boolInt(rec.Managed), boolInt(rec.Disabled),
		boolInt(rec.HasPassword), rec.UID, rec.GID, rec.Shell, rec.Home, rec.SecretRef,
		rec.Version, rec.LastOperationID, millis(rec.CreatedAt), millis(rec.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("agentstore: 账号名 %s 已有受管理记录（%s）："+
				"要改它请更新那条记录，不要新建同名账号: %w", rec.Name, rec.ID, err)
		}
		return fmt.Errorf("agentstore: 保存账号 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// DeleteAccount 删除一条账号记录；不存在时返回 nil（幂等）。
func (s *ShareStore) DeleteAccount(ctx context.Context, id string) error {
	if _, err := s.db.store().ExecContext(ctx,
		`DELETE FROM managed_accounts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("agentstore: 删除账号 %s 失败: %w", id, err)
	}
	return nil
}

// scanShare 读一行共享。
func scanShare(r rowScanner) (shares.ShareRecord, error) {
	var (
		rec                     shares.ShareRecord
		name, protocol          string
		volumeID, relativePath  string
		enabled, readOnly       int
		defJSON                 string
		prepared, configApplied int
		createdAt, updatedAt    int64
		revokedJSON             string
	)
	err := r.Scan(&rec.ID, &name, &protocol, &volumeID, &relativePath, &enabled, &readOnly,
		&defJSON, &rec.ConfigHash, &prepared, &configApplied, &rec.Version,
		&rec.LastOperationID, &createdAt, &updatedAt, &revokedJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shares.ShareRecord{}, err
		}
		return shares.ShareRecord{}, fmt.Errorf("agentstore: 读取共享失败: %w", err)
	}
	// 定义解析失败不静默：一份读不懂的定义会让渲染出来的 Samba 段变成猜的。
	if err := json.Unmarshal([]byte(defJSON), &rec.Definition); err != nil {
		return shares.ShareRecord{}, fmt.Errorf(
			"agentstore: 共享 %s 的定义无法解析（%v）：%s", rec.ID, err, defJSON)
	}
	// 撤销现场同理：读不懂就说读不懂。这是页面用来解释“这条共享为什么不见了”的依据，
	// 猜一个空记录会让它看起来像“没被撤销过”，于是用户找不到重新应用的理由。
	if revokedJSON != "" {
		var rv shares.RuntimeRevocation
		if err := json.Unmarshal([]byte(revokedJSON), &rv); err != nil {
			return shares.ShareRecord{}, fmt.Errorf(
				"agentstore: 共享 %s 的撤销现场无法解析（%v）：%s", rec.ID, err, revokedJSON)
		}
		rec.RuntimeRevoked = &rv
	}
	// 标量列与 JSON 里的副本必须一致。它们由同一次写入产生，不一致说明这一行被
	// 别的路径改过（或者写入中途失败过）——这时**不能**猜哪个是对的。
	if rec.Definition.Name != name || rec.Definition.Protocol != protocol ||
		rec.Definition.VolumeID != volumeID || rec.Definition.RelativePath != relativePath ||
		boolInt(rec.Definition.Enabled) != enabled || boolInt(rec.Definition.ReadOnly) != readOnly {
		return shares.ShareRecord{}, fmt.Errorf(
			"agentstore: 共享 %s 的列与定义不一致（name=%q/%q protocol=%q/%q volume=%q/%q path=%q/%q "+
				"enabled=%d/%d read_only=%d/%d）：这一行被本包之外的途径改过",
			rec.ID, name, rec.Definition.Name, protocol, rec.Definition.Protocol,
			volumeID, rec.Definition.VolumeID, relativePath, rec.Definition.RelativePath,
			enabled, boolInt(rec.Definition.Enabled), readOnly, boolInt(rec.Definition.ReadOnly))
	}
	rec.DirectoryPrepared = prepared == 1
	rec.ConfigApplied = configApplied == 1
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

// scanAccount 读一行账号。
func scanAccount(r rowScanner) (shares.AccountRecord, error) {
	var (
		rec                   shares.AccountRecord
		managed, disabled, hp int
		createdAt, updatedAt  int64
	)
	err := r.Scan(&rec.ID, &rec.Name, &rec.Type, &managed, &disabled, &hp,
		&rec.UID, &rec.GID, &rec.Shell, &rec.Home, &rec.SecretRef, &rec.Version,
		&rec.LastOperationID, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shares.AccountRecord{}, err
		}
		return shares.AccountRecord{}, fmt.Errorf("agentstore: 读取受管理账号失败: %w", err)
	}
	rec.Managed = managed == 1
	rec.Disabled = disabled == 1
	rec.HasPassword = hp == 1
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

// validateShareRecord 在落库之前再检查一遍形状。
//
// 与数据库 CHECK 重复是有意的：数据库报的是一句 SQL 错误，而这里的错话能被直接展示
// （“共享名不合法：…”），也更容易定位是哪一步写错了。
func validateShareRecord(rec shares.ShareRecord) error {
	if !shares.ValidShareID(rec.ID) {
		return fmt.Errorf("%w: 共享标识 %q 不合法", ErrResourceInvalid, rec.ID)
	}
	if err := shares.ValidateName(rec.Definition.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrResourceInvalid, err)
	}
	if rec.Definition.Protocol != shares.ProtocolSMB {
		return fmt.Errorf("%w: 协议 %q 在 P08.1 不受支持（只有 smb）",
			ErrResourceInvalid, rec.Definition.Protocol)
	}
	if rec.Definition.VolumeID == "" {
		return fmt.Errorf("%w: 共享 %s 没有绑定卷", ErrResourceInvalid, rec.ID)
	}
	if err := shares.ValidateRelativePath(rec.Definition.RelativePath); err != nil {
		return fmt.Errorf("%w: %v", ErrResourceInvalid, err)
	}
	if len(rec.Definition.SMBUsers) == 0 {
		return fmt.Errorf("%w: 共享 %s 的 smb_users 为空（空名单会退化成 guest，必须硬拒）",
			ErrResourceInvalid, rec.ID)
	}
	if rec.Version == "" {
		return fmt.Errorf("%w: version 不能为空", ErrResourceInvalid)
	}
	if rec.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at 不能为空", ErrResourceInvalid)
	}
	return nil
}

// validateAccountRecord 同上。
func validateAccountRecord(rec shares.AccountRecord) error {
	if !shares.ValidAccountID(rec.ID) {
		return fmt.Errorf("%w: 账号标识 %q 不合法", ErrResourceInvalid, rec.ID)
	}
	if err := shares.ValidateAccountName(rec.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrResourceInvalid, err)
	}
	switch rec.Type {
	case shares.AccountTypeShare, shares.AccountTypeRegular:
	default:
		return fmt.Errorf("%w: 账号类型 %q 不在 share/regular 内", ErrResourceInvalid, rec.Type)
	}
	if !rec.Managed {
		return fmt.Errorf("%w: 账号 %s 在登记表里却标记为未受管理", ErrResourceInvalid, rec.ID)
	}
	if rec.UID <= 0 {
		return fmt.Errorf("%w: 账号 %s 的 uid 必须为正，收到 %d", ErrResourceInvalid, rec.ID, rec.UID)
	}
	if rec.Version == "" {
		return fmt.Errorf("%w: version 不能为空", ErrResourceInvalid)
	}
	if rec.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at 不能为空", ErrResourceInvalid)
	}
	return nil
}

// isForeignKeyViolation 判断是不是外键约束失败。
func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
