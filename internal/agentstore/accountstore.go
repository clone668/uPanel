package agentstore

// 账号公钥与 sudo 规则的仓储（P14.5）。
//
// 与 sharestore.go 分成两个文件而不是一个：那个文件已经装了共享与账号两张表，
// 再塞进来两张会把“账号的哪几列是什么”埋在一千行里。表在这里各自成套，
// 但**账号本身仍只有一份实现**（ShareStore 的那五个方法），因为 managed_accounts
// 只有一张，两套读写就是两套读回核验的起点（DECISIONS Q475）。
//
// 一个约定：公钥在库里存的是**整行原文**（public_key），不是单独的 base64 主体。
// 理由是这一行就是 authorized_keys 里那一行：存原文，读回来的时候它能被同一套
// 解析器再校验一次——形状不对的行不可能悄悄躺在库里。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"upanel/internal/accounts"
)

// ShareStore 同时是两个消费方接口的实现：shares.Store（共享与账号）与
// accounts.Store（账号 + 公钥 + sudo 规则）。账号那五个方法只有一份实现，
// 这是 DECISIONS Q475 的落点：同一张 managed_accounts，一套读回核验。
var _ accounts.Store = (*ShareStore)(nil)

// sshKeyColumns 是 managed_ssh_keys 的读取列（顺序与 scanSSHKey 一致）。
const sshKeyColumns = `id, account_id, type, public_key, fingerprint, comment, created_at`

// sudoRuleColumns 是 managed_sudo_rules 的读取列。
const sudoRuleColumns = `id, account_id, template_id, config_hash, created_at, updated_at`

// ListSSHKeys 返回一个账号的全部公钥（顺序由调用方排序）。
func (s *ShareStore) ListSSHKeys(ctx context.Context, accountID string) ([]accounts.SSHKeyRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx,
		`SELECT `+sshKeyColumns+` FROM managed_ssh_keys WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取账号公钥失败: %w", err)
	}
	defer rows.Close()
	var out []accounts.SSHKeyRecord
	for rows.Next() {
		rec, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取账号公钥失败: %w", err)
	}
	return out, nil
}

// GetSSHKey 取一把公钥；不存在时返回 (零值, false, nil)。
func (s *ShareStore) GetSSHKey(ctx context.Context, accountID, keyID string) (accounts.SSHKeyRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT `+sshKeyColumns+` FROM managed_ssh_keys WHERE account_id = ? AND id = ?`,
		accountID, keyID)
	rec, err := scanSSHKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return accounts.SSHKeyRecord{}, false, nil
	}
	if err != nil {
		return accounts.SSHKeyRecord{}, false, err
	}
	return rec, true, nil
}

// SaveSSHKey 新建或覆盖一把公钥。
func (s *ShareStore) SaveSSHKey(ctx context.Context, rec accounts.SSHKeyRecord) error {
	if err := validateSSHKeyRecord(rec); err != nil {
		return err
	}
	now := millis(rec.CreatedAt)
	_, err := s.db.store().ExecContext(ctx, `
		INSERT INTO managed_ssh_keys (
			id, account_id, type, public_key, fingerprint, comment, version,
			last_operation_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, '1', '', ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			type         = excluded.type,
			public_key   = excluded.public_key,
			fingerprint  = excluded.fingerprint,
			comment      = excluded.comment,
			updated_at   = excluded.updated_at`,
		rec.ID, rec.AccountID, rec.Type, sshKeyLine(rec), rec.Fingerprint, rec.Comment, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("agentstore: 账号 %s 上已经有同一把公钥（%s）：要换注释请替换那条记录: %w",
				rec.AccountID, rec.Fingerprint, err)
		}
		if isForeignKeyViolation(err) {
			return fmt.Errorf("agentstore: 账号 %s 不存在，公钥无处安放: %w", rec.AccountID, err)
		}
		return fmt.Errorf("agentstore: 保存公钥 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// DeleteSSHKey 删除一把公钥；不存在时返回 nil（幂等）。
func (s *ShareStore) DeleteSSHKey(ctx context.Context, accountID, keyID string) error {
	if _, err := s.db.store().ExecContext(ctx,
		`DELETE FROM managed_ssh_keys WHERE account_id = ? AND id = ?`,
		accountID, keyID); err != nil {
		return fmt.Errorf("agentstore: 删除公钥 %s 失败: %w", keyID, err)
	}
	return nil
}

// GetSudoRule 取一个账号上的 sudo 规则。
func (s *ShareStore) GetSudoRule(ctx context.Context, accountID string) (accounts.SudoRuleRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT `+sudoRuleColumns+` FROM managed_sudo_rules WHERE account_id = ?`, accountID)
	rec, err := scanSudoRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return accounts.SudoRuleRecord{}, false, nil
	}
	if err != nil {
		return accounts.SudoRuleRecord{}, false, err
	}
	return rec, true, nil
}

// SaveSudoRule 新建或覆盖一条 sudo 规则。
func (s *ShareStore) SaveSudoRule(ctx context.Context, rec accounts.SudoRuleRecord) error {
	if err := validateSudoRuleRecord(rec); err != nil {
		return err
	}
	_, err := s.db.store().ExecContext(ctx, `
		INSERT INTO managed_sudo_rules (
			id, account_id, template_id, config_hash, version,
			last_operation_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, '1', '', ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET
			template_id = excluded.template_id,
			config_hash = excluded.config_hash,
			version     = excluded.version,
			updated_at  = excluded.updated_at`,
		rec.ID, rec.AccountID, rec.Template, rec.Version,
		millis(rec.CreatedAt), millis(rec.UpdatedAt))
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("agentstore: 账号 %s 不存在，sudo 规则无处安放: %w", rec.AccountID, err)
		}
		return fmt.Errorf("agentstore: 保存 sudo 规则 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// DeleteSudoRule 删除一个账号上的 sudo 规则；不存在时返回 nil（幂等）。
func (s *ShareStore) DeleteSudoRule(ctx context.Context, accountID string) error {
	if _, err := s.db.store().ExecContext(ctx,
		`DELETE FROM managed_sudo_rules WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("agentstore: 删除账号 %s 的 sudo 规则失败: %w", accountID, err)
	}
	return nil
}

// sshKeyLine 拼出 authorized_keys 里那一行原文。
func sshKeyLine(rec accounts.SSHKeyRecord) string {
	line := rec.Type + " " + rec.Blob
	if rec.Comment != "" {
		line += " " + rec.Comment
	}
	return line
}

// scanSSHKey 读一行公钥。
//
// 库里的 public_key 会被**重新解析一次**：一段读不懂的公钥不该被原样交给执行器，
// 那会让 authorized_keys 里出现一行 sshd 会静默忽略的内容，而页面显示“已配置”。
func scanSSHKey(r rowScanner) (accounts.SSHKeyRecord, error) {
	var (
		rec       accounts.SSHKeyRecord
		publicKey string
		createdAt int64
	)
	if err := r.Scan(&rec.ID, &rec.AccountID, &rec.Type, &publicKey,
		&rec.Fingerprint, &rec.Comment, &createdAt); err != nil {
		return accounts.SSHKeyRecord{}, err
	}
	parsed, err := accounts.ParsePublicKey(publicKey)
	if err != nil {
		return accounts.SSHKeyRecord{}, fmt.Errorf(
			"agentstore: 公钥 %s 在库里存成了读不懂的样子（%v）：%s", rec.ID, err, publicKey)
	}
	rec.Type = parsed.Type
	rec.Blob = parsed.BlobText()
	rec.Comment = parsed.Comment
	// 指纹以库里那一列为准（它是唯一索引的依据），但两者不一致要说出来：
	// 那意味着有人绕过接口改过库。
	if parsed.Fingerprint != rec.Fingerprint {
		return accounts.SSHKeyRecord{}, fmt.Errorf(
			"agentstore: 公钥 %s 的指纹与内容不符（库里 %s，算出来 %s）",
			rec.ID, rec.Fingerprint, parsed.Fingerprint)
	}
	rec.CreatedAt = fromMillis(createdAt)
	return rec, nil
}

// scanSudoRule 读一行 sudo 规则。
func scanSudoRule(r rowScanner) (accounts.SudoRuleRecord, error) {
	var (
		rec                  accounts.SudoRuleRecord
		createdAt, updatedAt int64
	)
	if err := r.Scan(&rec.ID, &rec.AccountID, &rec.Template, &rec.Version,
		&createdAt, &updatedAt); err != nil {
		return accounts.SudoRuleRecord{}, err
	}
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

// validateSSHKeyRecord 在落库之前再检查一遍形状。
func validateSSHKeyRecord(rec accounts.SSHKeyRecord) error {
	if !accounts.ValidSSHKeyID(rec.ID) {
		return fmt.Errorf("%w: 公钥标识 %q 不合法", ErrResourceInvalid, rec.ID)
	}
	if !strings.HasPrefix(rec.AccountID, "sac_") || len(rec.AccountID) != 36 {
		return fmt.Errorf("%w: 公钥 %s 的账号标识 %q 不合法", ErrResourceInvalid, rec.ID, rec.AccountID)
	}
	if rec.Type == "" || rec.Blob == "" {
		return fmt.Errorf("%w: 公钥 %s 缺少算法名或主体", ErrResourceInvalid, rec.ID)
	}
	if rec.Fingerprint == "" || len(rec.Fingerprint) < 7 || rec.Fingerprint[:7] != "SHA256:" {
		return fmt.Errorf("%w: 公钥 %s 的指纹 %q 不是 SHA256: 形式", ErrResourceInvalid, rec.ID, rec.Fingerprint)
	}
	if rec.CreatedAt.IsZero() {
		return fmt.Errorf("%w: 公钥 %s 的 created_at 不能为空", ErrResourceInvalid, rec.ID)
	}
	return nil
}

// validateSudoRuleRecord 在落库之前再检查一遍形状。
func validateSudoRuleRecord(rec accounts.SudoRuleRecord) error {
	if !accounts.ValidSudoRuleID(rec.ID) {
		return fmt.Errorf("%w: sudo 规则标识 %q 不合法", ErrResourceInvalid, rec.ID)
	}
	if !strings.HasPrefix(rec.AccountID, "sac_") || len(rec.AccountID) != 36 {
		return fmt.Errorf("%w: 规则 %s 的账号标识 %q 不合法", ErrResourceInvalid, rec.ID, rec.AccountID)
	}
	// 模板闭集在数据库上也有一份 CHECK；这里的差别是它先于 SQL 给出人话。
	known := false
	for _, id := range accounts.SudoTemplateIDs() {
		if id == rec.Template {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("%w: 规则 %s 的模板标识 %q 不在闭集里", ErrResourceInvalid, rec.ID, rec.Template)
	}
	if len(rec.Version) != 64 {
		return fmt.Errorf("%w: 规则 %s 的版本必须是渲染内容的 sha256（64 位十六进制）", ErrResourceInvalid, rec.ID)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: 规则 %s 的时间戳不能为空", ErrResourceInvalid, rec.ID)
	}
	return nil
}
