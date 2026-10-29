-- 代理数据库第 14 条迁移：受管理账号补两列，新增 SSH 公钥与 sudo 规则两张表（P14.5）。
--
-- 为什么重建成对而不是 ALTER ADD COLUMN：
--   要加的两列（display_name、shell_profile）都是闭集/长度有约束的。SQLite 的
--   ALTER TABLE ADD COLUMN 虽然接受 CHECK，但它只对**新写入**的行生效，表里已有的行
--   绕过了这条约束；而这一版的语义是“老账号一律是 share 类型、一律 disabled 登录档”，
--   本来就该让新约束对整张表成立。按官方推荐的表重建走（建新表 → 拷数据 → 删旧表 →
--   改名 → 重建索引），形状与 0013 一致。
--
--   迁移文件一经应用即不可修改（internal/store 校验内容哈希），SQLite 也不支持就地
--   修改 CHECK，所以只能重建。整条迁移在 internal/store 的一个事务里执行，这里不写
--   BEGIN/COMMIT（会嵌套报错）。
--
-- 回填规则：
--   display_name  := name（老记录没有展示名，用系统名兜底；页面本来也是这么显示的）
--   shell_profile := 'disabled'（P08 建的全是共享账号，共享账号只能是 disabled）
--
-- 这一版新增的两个 CHECK 是“同一张表里两种类型各守各的规矩”的落点：
--   type='share'   → shell_profile 必须是 'disabled'（共享账号只做 SMB 认证）
--   type='regular' → shell_profile 闭集内即可
-- 应用层（internal/accounts/definition.go）已经拦过一次，这里是最后一道：绕过接口直接
-- 写库也造不出“能登录的共享账号”。
--
-- managed_ssh_keys / managed_sudo_rules 都挂在 managed_accounts 上，ON DELETE CASCADE：
-- 账号没了，它的公钥与 sudo 规则不可能还有意义；而 authorized_keys 与 sudoers.d 里的
-- 实体文件由执行器在删账号时一并清理（库里的级联不能替你改文件系统）。
--
-- has_password 这一列的含义随类型而定，不新开一列：
--   type='share'   → Samba passdb 里设过密码
--   type='regular' → shadow 里设过登录密码
-- 两者回答的是同一个问题（“这个账号有凭据吗”），只是凭据存在不同地方。分两列会让
-- “一共设过几个”变成一次 or 运算，而没有任何一处需要区分。

CREATE TABLE managed_accounts_new (
    id                TEXT    PRIMARY KEY CHECK (length(id) = 36),
    name              TEXT    NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 32),
    display_name      TEXT    NOT NULL DEFAULT '' CHECK (length(display_name) <= 256),
    type              TEXT    NOT NULL CHECK (type IN ('share', 'regular')),
    shell_profile     TEXT    NOT NULL DEFAULT 'disabled' CHECK (shell_profile IN ('disabled', 'restricted')),
    managed           INTEGER NOT NULL CHECK (managed IN (0, 1)),
    disabled          INTEGER NOT NULL CHECK (disabled IN (0, 1)),
    has_password      INTEGER NOT NULL CHECK (has_password IN (0, 1)),
    uid               INTEGER NOT NULL CHECK (uid > 0),
    gid               INTEGER NOT NULL CHECK (gid > 0),
    shell             TEXT    NOT NULL DEFAULT '',
    home              TEXT    NOT NULL DEFAULT '',
    secret_ref        TEXT    NOT NULL DEFAULT '',
    version           TEXT    NOT NULL DEFAULT '1' CHECK (length(version) > 0),
    last_operation_id TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL CHECK (created_at > 0),
    updated_at        INTEGER NOT NULL CHECK (updated_at >= created_at),
    CHECK (type <> 'share' OR shell_profile = 'disabled')
);

INSERT INTO managed_accounts_new (id, name, display_name, type, shell_profile, managed,
                                  disabled, has_password, uid, gid, shell, home, secret_ref,
                                  version, last_operation_id, created_at, updated_at)
SELECT id, name, name, type, 'disabled', managed,
       disabled, has_password, uid, gid, shell, home, secret_ref,
       version, last_operation_id, created_at, updated_at
  FROM managed_accounts;

DROP TABLE managed_accounts;

ALTER TABLE managed_accounts_new RENAME TO managed_accounts;

CREATE INDEX idx_managed_accounts_disabled ON managed_accounts (disabled, name);
CREATE INDEX idx_managed_accounts_type ON managed_accounts (type, name);

-- 一个账号的一把公钥。
--   public_key   完整的授权行（算法名 + base64 + 可选注释），是写进 authorized_keys 的原文
--   fingerprint  SHA256:… —— 同一账号内唯一，去重与展示都靠它（换注释不算换钥匙）
--   version      乐观并发令牌
-- 同一个指纹在两个账号上各存一份是允许的（一把钥匙开两扇门是正常需求），所以唯一索引
-- 是 (account_id, fingerprint) 而不是 fingerprint。
CREATE TABLE managed_ssh_keys (
    id                TEXT    PRIMARY KEY CHECK (length(id) = 36),
    account_id        TEXT    NOT NULL REFERENCES managed_accounts (id) ON DELETE CASCADE,
    type              TEXT    NOT NULL CHECK (type IN ('ssh-ed25519', 'ssh-rsa',
                                                       'ecdsa-sha2-nistp256',
                                                       'ecdsa-sha2-nistp384',
                                                       'ecdsa-sha2-nistp521')),
    public_key        TEXT    NOT NULL CHECK (length(public_key) BETWEEN 32 AND 8192),
    fingerprint       TEXT    NOT NULL CHECK (substr(fingerprint, 1, 7) = 'SHA256:'),
    comment           TEXT    NOT NULL DEFAULT '' CHECK (length(comment) <= 512),
    version           TEXT    NOT NULL DEFAULT '1' CHECK (length(version) > 0),
    last_operation_id TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL CHECK (created_at > 0),
    updated_at        INTEGER NOT NULL CHECK (updated_at >= created_at),
    CHECK (substr(id, 1, 4) = 'ssk_'),
    CHECK (substr(account_id, 1, 4) = 'sac_')
);

CREATE UNIQUE INDEX ux_managed_ssh_keys_fingerprint ON managed_ssh_keys (account_id, fingerprint);
CREATE INDEX idx_managed_ssh_keys_account ON managed_ssh_keys (account_id, created_at);

-- 一个账号最多一条 sudo 规则（文件是 /etc/sudoers.d/upanel-<账号名>，一个账号一个文件）。
-- 模板标识写死成闭集，与 internal/accounts/sudo.go 的 sudoTemplates 一一对应：
-- 库能接受而代码不认识的值，会在下一次读回渲染时变成一个谁也解释不了的状态。
CREATE TABLE managed_sudo_rules (
    id                TEXT    PRIMARY KEY CHECK (length(id) = 36),
    account_id        TEXT    NOT NULL UNIQUE REFERENCES managed_accounts (id) ON DELETE CASCADE,
    template_id       TEXT    NOT NULL CHECK (template_id IN ('system_status', 'journal_recent')),
    config_hash       TEXT    NOT NULL CHECK (length(config_hash) = 64),
    version           TEXT    NOT NULL DEFAULT '1' CHECK (length(version) > 0),
    last_operation_id TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL CHECK (created_at > 0),
    updated_at        INTEGER NOT NULL CHECK (updated_at >= created_at),
    CHECK (substr(id, 1, 4) = 'sru_'),
    CHECK (substr(account_id, 1, 4) = 'sac_')
);
