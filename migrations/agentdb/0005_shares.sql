-- 代理数据库第 5 条迁移：共享定义与受管理账号（CONTRACTS.md 7、14.2；P08.1）。
--
-- 两张表都放在**代理**库，而不是 Web 库。理由与 P07.4 的受管理卷一样：真正要在系统上
-- 生效的东西由代理写、由代理读回、由代理在重启后对账；Web 侧只是它的一张视图。
-- 放进 Web 库就会出现“Web 以为共享是这样、代理做出来是那样”的第二份真相。
--
-- shares 列语义：
--   id                  共享标识 shr_<32 位小写十六进制>；对外只暴露它
--   name                共享名（也是 Samba 段名）。UNIQUE：同名共享不允许两条
--   protocol            协议；目前只有 'smb'（闭集，由 CHECK 保证）
--   volume_id           绑定的受管理卷；外键指向 managed_resources(id)
--   relative_path       卷内相对路径（空串 = 卷根）；不允许绝对路径与 '..'（由应用层守）
--   enabled             1 = 定义处于启用状态（P08.1 只表示“定义启用”，不代表已对外提供）
--   read_only           1 = 只读共享；read_only 会覆盖掉所有 write 授权
--   definition_json     ShareDefinition 的完整序列化（allowed_cidrs / smb_users 在里面）
--   config_hash         渲染结果的 sha256 = 配置版本。P08.1 只算不写盘
--   directory_prepared  1 = 目录已建、ACL 已应用并读回核验
--   config_applied      1 = 已写进 Samba 配置并通过校验。**P08.1 恒为 0**
--   version             乐观并发令牌：每次成功变更自增
--   last_operation_id   最近一次成功修改它的 operation_id
--   created_at          创建时间（Unix 毫秒，UTC）
--   updated_at          最后修改时间（Unix 毫秒，UTC）
--
-- 为什么外键指向 managed_resources：共享只能建在受管理卷里。删卷时如果还有共享引用它，
-- 数据库直接拒绝——这比“删完卷之后共享指向一个不存在的卷”安全得多。P08.3 会在判决层
-- 给出可展示的阻断理由，这里的约束是最后一道，不是唯一一道。
--
-- managed_accounts 列语义：
--   id                  账号标识 sac_<32 位小写十六进制>
--   name                系统用户名（<=32，小写下划线数字短横）。UNIQUE：一个系统账号一条记录
--   type                'share'（面板创建的共享账号）/ 'regular'（预留给以后）
--   managed             恒为 1：这张表里只有面板创建的账号。字段保留是为了与契约 DTO 对齐
--   disabled            1 = Samba passdb 里的 D 标志
--   has_password        1 = passdb 里已经设过密码（**不存密码，也不存哈希**）
--   uid / gid           创建时读回来的系统 id（对账用：账号被删了 uid 就对不上）
--   shell / home        创建时读回来的 shell 与 home
--   secret_ref          指向代理秘密存储里的密文；解引用才能拿到明文
--   version / last_operation_id / created_at / updated_at  同上
--
-- 这里**没有**任何存放密码或密码哈希的列，而且永远不会有：明文只在
-- smbpasswd 的标准输入上经过一次，随后被清零；passdb 由 Samba 自己管。

CREATE TABLE shares (
    id                 TEXT    PRIMARY KEY CHECK (length(id) = 36),
    name               TEXT    NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 64),
    protocol           TEXT    NOT NULL CHECK (protocol IN ('smb')),
    volume_id          TEXT    NOT NULL REFERENCES managed_resources (id),
    relative_path      TEXT    NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    read_only          INTEGER NOT NULL CHECK (read_only IN (0, 1)),
    definition_json    TEXT    NOT NULL,
    config_hash        TEXT    NOT NULL DEFAULT '' CHECK (length(config_hash) IN (0, 64)),
    directory_prepared INTEGER NOT NULL CHECK (directory_prepared IN (0, 1)),
    config_applied     INTEGER NOT NULL CHECK (config_applied IN (0, 1)),
    version            TEXT    NOT NULL DEFAULT '1' CHECK (length(version) > 0),
    last_operation_id  TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL CHECK (created_at > 0),
    updated_at         INTEGER NOT NULL CHECK (updated_at >= created_at)
);

CREATE INDEX idx_shares_volume ON shares (volume_id, updated_at DESC);
CREATE INDEX idx_shares_name ON shares (name);

CREATE TABLE managed_accounts (
    id                TEXT    PRIMARY KEY CHECK (length(id) = 36),
    name              TEXT    NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 32),
    type              TEXT    NOT NULL CHECK (type IN ('share', 'regular')),
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
    updated_at        INTEGER NOT NULL CHECK (updated_at >= created_at)
);

CREATE INDEX idx_managed_accounts_disabled ON managed_accounts (disabled, name);
