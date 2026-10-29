package agentstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"upanel/internal/backup"
)

// RestoreVerification 是一次已经完成的抽样恢复证据；不含口令、路径清单或文件内容。
type RestoreVerification = backup.RestoreVerification

// BackupVerificationStore 持久化每个仓库/快照的恢复验证时间。
type BackupVerificationStore struct{ db *DB }

func NewBackupVerificationStore(db *DB) (*BackupVerificationStore, error) {
	if db == nil {
		return nil, errors.New("agentstore: 恢复验证仓储需要数据库句柄")
	}
	return &BackupVerificationStore{db: db}, nil
}

func (s *BackupVerificationStore) MarkRestoreVerified(ctx context.Context, record RestoreVerification) error {
	if record.FilesChecked <= 0 || record.BytesChecked < 0 || record.VerifiedAt.IsZero() {
		return errors.New("agentstore: 恢复验证证据不完整")
	}
	_, err := s.db.store().SQL().ExecContext(ctx, `
		INSERT INTO backup_restore_verifications
		    (repository_id, snapshot_id, verified_at, files_checked, bytes_checked, sample_restore)
		VALUES (?, ?, ?, ?, ?, 1)
		ON CONFLICT(repository_id, snapshot_id) DO UPDATE SET
		    verified_at=excluded.verified_at,
		    files_checked=excluded.files_checked,
		    bytes_checked=excluded.bytes_checked,
		    sample_restore=1`, record.RepositoryID, record.SnapshotID,
		record.VerifiedAt.UTC().UnixMilli(), record.FilesChecked, record.BytesChecked)
	if err != nil {
		return fmt.Errorf("agentstore: 写入恢复验证记录失败: %w", err)
	}
	return nil
}

func (s *BackupVerificationStore) RestoreVerification(ctx context.Context, repositoryID, snapshotID string) (RestoreVerification, bool, error) {
	var record RestoreVerification
	var millis int64
	err := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT repository_id, snapshot_id, verified_at, files_checked, bytes_checked
		  FROM backup_restore_verifications
		 WHERE repository_id=? AND snapshot_id=?`, repositoryID, snapshotID).
		Scan(&record.RepositoryID, &record.SnapshotID, &millis, &record.FilesChecked, &record.BytesChecked)
	if errors.Is(err, sql.ErrNoRows) {
		return RestoreVerification{}, false, nil
	}
	if err != nil {
		return RestoreVerification{}, false, fmt.Errorf("agentstore: 读取恢复验证记录失败: %w", err)
	}
	record.VerifiedAt = time.UnixMilli(millis).UTC()
	return record, true, nil
}
