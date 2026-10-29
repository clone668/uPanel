-- 代理数据库第 9 条迁移：快照恢复验证记录（P12.3）。
-- 只有 backup.verify(sample_restore=true) 完成真实抽样恢复、并逐文件比对仓库字节后，
-- 才由执行端写入。backup.run、restic check、备份成功都不写这张表。
CREATE TABLE backup_restore_verifications (
    repository_id TEXT NOT NULL,
    snapshot_id   TEXT    NOT NULL,
    verified_at   INTEGER NOT NULL CHECK (verified_at > 0),
    files_checked INTEGER NOT NULL CHECK (files_checked > 0),
    bytes_checked INTEGER NOT NULL CHECK (bytes_checked >= 0),
    sample_restore INTEGER NOT NULL CHECK (sample_restore = 1),
    CHECK (length(repository_id) = 36),
    CHECK (substr(repository_id, 1, 4) = 'rep_'),
    CHECK (length(snapshot_id) = 64),
    PRIMARY KEY (repository_id, snapshot_id)
);
CREATE INDEX idx_backup_restore_verifications_snapshot
    ON backup_restore_verifications (snapshot_id);
