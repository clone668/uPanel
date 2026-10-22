-- 代理数据库第 4 条迁移：主机实际接管清单（CONTRACTS.md 14.2 managed_resources）。
--
-- 这张表是“这台主机上代理真的接管了什么”的权威记录。P07.4 只有一种资源：受管理卷。
-- 因此列定义按卷的实际需要写死，而不是先做一张泛型 JSON 表——泛型表会把“卷必须有一个
-- UUID、一个挂载名”这类不变量丢给应用层去守，而它们恰恰是最该由数据库挡住的东西。
--
-- 列语义（kind = 'volume' 时）：
--   id                  卷标识 vol_<32 位小写十六进制>；对外只暴露它
--   kind                资源类型；目前只有 'volume'（闭集，由 CHECK 保证）
--   display_name        显示名（只进页面与审计，不进 fstab）
--   slug                卷名，决定挂载点 /srv/upanel/volumes/<slug>；UNIQUE
--   device_id           记录时的内核设备名（会变，只作线索）
--   filesystem_uuid     文件系统 UUID；挂载与核验的依据。同一 UUID 不允许两个卷
--   filesystem_type     ext4 / xfs
--   identity_fingerprint 导入时那台分区的身份指纹（抓“换了盘但 UUID 恰好相同”）
--   config_json         挂载配置 {automount, read_only, nodev, nosuid}
--   desired_state       面板希望的状态 mounted / unmounted
--   version             乐观并发令牌：每次成功变更自增
--   last_operation_id   最近一次成功修改它的 operation_id（审计与对账用）
--   created_at          创建时间（Unix 毫秒，UTC）
--   updated_at          最后修改时间（Unix 毫秒，UTC）
--
-- 为什么把 UNIQUE 放在 (filesystem_uuid) 与 (slug) 上：两个卷指向同一个文件系统、或者
-- 两个卷派生出同一个挂载点，都会让 fstab 受管块里出现两行指向同一处。systemd 取哪一行
-- 不由我们决定——这种“能写进去但行为不确定”的状态必须写不进数据库。
--
-- 部分索引（WHERE kind = 'volume'）而不是列级 UNIQUE：以后新增别的资源类型时，
-- 它们的 filesystem_uuid/slug 可能是空的，不该被卷的唯一性约束误伤。

CREATE TABLE managed_resources (
    id                   TEXT    PRIMARY KEY,
    kind                 TEXT    NOT NULL CHECK (kind IN ('volume')),
    display_name         TEXT    NOT NULL DEFAULT '',
    slug                 TEXT    NOT NULL CHECK (length(slug) BETWEEN 1 AND 32),
    device_id            TEXT    NOT NULL DEFAULT '',
    filesystem_uuid      TEXT    NOT NULL CHECK (length(filesystem_uuid) = 36),
    filesystem_type      TEXT    NOT NULL CHECK (filesystem_type IN ('ext4', 'xfs')),
    identity_fingerprint TEXT    NOT NULL DEFAULT '',
    config_json          TEXT    NOT NULL DEFAULT '{}',
    desired_state        TEXT    NOT NULL CHECK (desired_state IN ('mounted', 'unmounted')),
    version              TEXT    NOT NULL DEFAULT '1' CHECK (length(version) > 0),
    last_operation_id    TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL CHECK (created_at > 0),
    updated_at           INTEGER NOT NULL CHECK (updated_at >= created_at)
);

CREATE UNIQUE INDEX ux_managed_resources_volume_uuid
    ON managed_resources (filesystem_uuid) WHERE kind = 'volume';
CREATE UNIQUE INDEX ux_managed_resources_volume_slug
    ON managed_resources (slug) WHERE kind = 'volume';
CREATE INDEX idx_managed_resources_kind
    ON managed_resources (kind, updated_at DESC);
