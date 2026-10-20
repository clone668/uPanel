-- 代理数据库第 2 条迁移：预检计划（CONTRACTS.md 14.2 preflight_plans、4.1）。
--
-- 预检由**代理**生成：Web 只负责把管理员的请求和身份上下文转发过来，计划里的
-- 规范化参数、目标身份指纹、状态版本、影响与阻塞项都由代理计算并落库。这样做的
-- 理由很实际：如果 Web 能自己造一份“看起来合法”的计划，那么执行前的复核就失去意义。
--
-- 列语义：
--   id                   计划引用，形如 pf_<32 位小写十六进制>；对外只暴露它
--   action               动作登记名（形如 storage.filesystem.format）
--   params_json          规范化参数（canonical JSON：无多余空白、键有序）
--   params_hash          SHA-256(params_json) 十六进制；提交时按它比对，不再接受新参数
--   target_json          目标信息 {display_name, identity_fingerprint, state_version}
--   identity_fingerprint 目标身份指纹（单独成列：执行前必须重新核对）
--   state_version        目标状态快照版本（单独成列：状态变了就失效）
--   allowed              0/1，当前状态下是否允许执行
--   blockers_json        阻止原因 [{code, message}]
--   warnings_json        告警 [{code, message}]
--   impacts_json         影响 [{kind, description}]
--   locks_json           执行时需要持有的锁（规范字符串数组）
--   requires_reauth      0/1，提交任务时是否要求 5 分钟内的再认证
--   phrase_hash          确认短语的 SHA-256；空串表示该动作不需要短语。
--                        **只存哈希**：短语明文只在预检响应里出现一次
--   user_id              发起预检的账号 id（Web 侧 users.id，供对账与审计）
--   session_hash         发起预检的会话令牌哈希：重新登录后旧预检自动失效
--   request_id           发起预检的 request_id，便于与日志对齐
--   created_at           创建时间（Unix 毫秒，UTC）
--   expires_at           到期时间（created_at + 120_000）
--   used_at              被任务消费的时间；0 表示尚未使用
--   used_operation_id    消费它的 operation_id；空串表示尚未使用
--
-- 一次性由数据库层把住：只用 used_at = 0 且未过期的计划才能被任务消费（P03.2）。
-- 过期计划由代理的扫描器删除（未使用的计划没有保留价值），而不是留在库里假装还能用。

CREATE TABLE preflight_plans (
    id                   TEXT    PRIMARY KEY,
    action               TEXT    NOT NULL,
    params_json          TEXT    NOT NULL,
    params_hash          TEXT    NOT NULL,
    target_json          TEXT    NOT NULL,
    identity_fingerprint TEXT    NOT NULL DEFAULT '',
    state_version        TEXT    NOT NULL DEFAULT '',
    allowed              INTEGER NOT NULL CHECK (allowed IN (0, 1)),
    blockers_json        TEXT    NOT NULL DEFAULT '[]',
    warnings_json        TEXT    NOT NULL DEFAULT '[]',
    impacts_json         TEXT    NOT NULL DEFAULT '[]',
    locks_json           TEXT    NOT NULL DEFAULT '[]',
    requires_reauth      INTEGER NOT NULL CHECK (requires_reauth IN (0, 1)),
    phrase_hash          TEXT    NOT NULL DEFAULT '',
    user_id              INTEGER NOT NULL CHECK (user_id > 0),
    session_hash         TEXT    NOT NULL CHECK (length(session_hash) = 64),
    request_id           TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL CHECK (created_at > 0),
    expires_at           INTEGER NOT NULL CHECK (expires_at > created_at),
    used_at              INTEGER NOT NULL DEFAULT 0,
    used_operation_id    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_preflight_plans_expires_at ON preflight_plans (expires_at);
CREATE INDEX idx_preflight_plans_user_id ON preflight_plans (user_id, created_at DESC);

-- 用过的计划不能再被改回未使用：一次性是安全属性，不能让一条 UPDATE 把它抹掉。
CREATE TRIGGER preflight_plans_used_once
BEFORE UPDATE OF used_at ON preflight_plans
WHEN OLD.used_at > 0 AND NEW.used_at = 0
BEGIN
    SELECT RAISE(ABORT, 'preflight_plans 已使用的计划不能恢复为未使用');
END;
