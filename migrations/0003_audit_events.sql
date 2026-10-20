-- 0003：审计事件（P02.2 起步；完整审计策略在 P02.4）。
--
-- 依据 CONTRACTS.md 第 14.1 节的逻辑表：
--   audit_events  id、actor_id（可空）、actor_snapshot、action、target、request_id、
--                 job_id、outcome、redacted_details、created_at；只追加
--
-- 约定（记录在 DECISIONS Q31）：
--   - actor_id 不加外键：审计必须在账号被删除后仍然可读，删除账号不能改动历史行。
--     账号身份由 actor_snapshot（username + role 的文本快照）保留。
--   - 只追加由触发器强制：任何 UPDATE / DELETE 都 ABORT，代码里没有“改审计”的路径。
--   - redacted_details 只存非秘密字段（用户名、角色、字段变化），口令、令牌、哈希
--     一律不入库；写入前还有一层按键名过滤（internal/audit）。
--   - job_id 为后续任务阶段预留可空列；P02.2 不写。

CREATE TABLE audit_events (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id         INTEGER,
    actor_snapshot   TEXT NOT NULL,
    action           TEXT NOT NULL,
    target           TEXT NOT NULL DEFAULT '',
    request_id       TEXT NOT NULL DEFAULT '',
    job_id           TEXT,
    outcome          TEXT NOT NULL CHECK (outcome IN ('success', 'failure')),
    redacted_details TEXT NOT NULL DEFAULT '{}',
    created_at       TEXT NOT NULL
);

CREATE INDEX idx_audit_events_created_at ON audit_events (created_at DESC, id DESC);
CREATE INDEX idx_audit_events_actor_id ON audit_events (actor_id);
CREATE INDEX idx_audit_events_action ON audit_events (action);

CREATE TRIGGER audit_events_append_only_update
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events 只追加，不允许 UPDATE');
END;

CREATE TRIGGER audit_events_append_only_delete
BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events 只追加，不允许 DELETE');
END;
