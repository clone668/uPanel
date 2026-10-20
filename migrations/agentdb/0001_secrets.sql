-- 代理数据库第 1 条迁移：秘密存储（CONTRACTS.md 14.2）。
--
-- 这张表只有 root 读得到：数据库文件 /var/lib/upanel-agent/agent.db 属主 root、
-- 权限 0600，目录 root 独占（0700）。Web 进程既读不到密文，也读不到 /etc/upanel/agent.key。
--
-- 列语义：
--   id            秘密引用（secret_ref），形如 sec_<32 位小写十六进制>；对外只暴露它
--   purpose       用途枚举（smb_password、tls_private_key…），写入时即固定，不随引用改变
--   ciphertext    AES-256-GCM 密文（含 16 字节认证标签）；销毁时清空，不留材料
--   nonce         每条秘密独立的 12 字节随机 nonce，绝不复用
--   key_version   本机密钥版本，密钥轮换时用于识别旧密文
--   state         temporary（未被配置引用，到期销毁）/ persistent（已被引用）/ destroyed（墓碑）
--   reference     引用这份秘密的配置标识；persistent 时非空
--   created_at    创建时间（Unix 毫秒，UTC）
--   expires_at    temporary 的销毁时间（创建 + 10 分钟）；0 表示不适用
--   referenced_at 转为 persistent 的时间；0 表示未引用
--   destroyed_at  墓碑时间；0 表示未销毁
--
-- 时间列用整数毫秒而不是 RFC3339 文本：到期扫描要按时间比较，文本比较在带小数秒
-- 的情况下不可靠（"01.5" 与 "01.05" 的字典序与时间序不一致）。
--
-- CHECK 约束不是装饰：它们让“销毁但忘了清空密文”“persistent 却没有引用”这类违反
-- 生命周期的写入在数据库层直接失败，而不是等到审计时才发现。
CREATE TABLE secrets (
    id            TEXT    PRIMARY KEY,
    purpose       TEXT    NOT NULL,
    ciphertext    BLOB    NOT NULL,
    nonce         BLOB    NOT NULL,
    key_version   INTEGER NOT NULL,
    state         TEXT    NOT NULL,
    reference     TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL DEFAULT 0,
    referenced_at INTEGER NOT NULL DEFAULT 0,
    destroyed_at  INTEGER NOT NULL DEFAULT 0,

    CHECK (length(id) = 36),
    CHECK (substr(id, 1, 4) = 'sec_'),
    CHECK (length(purpose) BETWEEN 1 AND 64),
    CHECK (key_version >= 1),
    CHECK (created_at > 0),
    CHECK (state IN ('temporary', 'persistent', 'destroyed')),

    -- 销毁后密文与 nonce 必须为空（BLOB 零长度），否则等于留了材料。
    CHECK (state <> 'destroyed' OR (length(ciphertext) = 0 AND length(nonce) = 0)),
    CHECK (state = 'destroyed' OR length(ciphertext) >= 16),
    CHECK (state = 'destroyed' OR length(nonce) = 12),

    CHECK (state <> 'temporary' OR expires_at > 0),
    CHECK (state <> 'persistent' OR length(reference) > 0),
    CHECK (state <> 'persistent' OR referenced_at > 0),
    CHECK (state = 'destroyed' OR destroyed_at = 0),
    CHECK (state <> 'destroyed' OR destroyed_at > 0)
);

-- 到期扫描只查 temporary，索引按 (state, expires_at) 建。
CREATE INDEX secrets_state_expires ON secrets (state, expires_at);
-- 按用途 + 引用查“这份配置用到的秘密”，以及同一引用的唯一性检查。
CREATE INDEX secrets_purpose_reference ON secrets (purpose, reference);
