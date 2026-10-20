-- 0002：账号与会话（P02.1）。
--
-- 依据 CONTRACTS.md 第 14.1 节的逻辑表：
--   users    id、username UNIQUE、password_hash、role、disabled
--   sessions id_hash UNIQUE、user_id FK、csrf_hash、created_at、last_seen_at、expires_at、reauth_until
--
-- 约定补充（记录在 DECISIONS Q23）：
--   - username 用 COLLATE NOCASE：登录名不因大小写产生两个账号；
--   - display_name 由 P02.2 的账号管理使用，P02.1 创建首个管理员时留空字符串；
--   - sessions 只保存令牌的 SHA-256 十六进制约值，明文令牌只存在于 Cookie 中，数据库泄露也拿不到会话；
--   - 绝对超时 = created_at + 12 小时（代码计算），expires_at 是滑动空闲超时的截止时间（30 分钟）；
--     reauth_until 属于 P02.2 的再认证，先建列以免后续改表；
--   - 登录失败限速在进程内实现，不建表：永久锁定会把管理员锁死，限速状态也不需要跨重启保留。

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL COLLATE NOCASE UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    disabled      INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    version       INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX idx_users_role ON users (role);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    id_hash      TEXT NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_hash    TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    reauth_until TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);
