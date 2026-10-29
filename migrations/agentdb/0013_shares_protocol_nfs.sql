-- 代理数据库第 13 条迁移：shares.protocol 的闭集加上 'nfs'（P14.4 修正）。
--
-- 为什么改：
--   0005 把这一列钉死成 CHECK (protocol IN ('smb'))。P14.4 把 NFS 那一支从判决层一路
--   做到执行层、读写导出、读回核对，样样都过；到落库这一步被这条 CHECK 挡住，
--   报出来的是 `constraint failed: CHECK constraint failed: protocol IN ('smb')`——
--   一条只有读迁移文件的人才看得懂的话。22.04 验收 VM 上就是这么红的：
--   share.apply 先在 /etc/exports.d/upanel.exports 里把导出写好、exportfs 也吃下去了，
--   然后 SaveShare 失败，任务终态 failed。**系统上真的在导出一个面板数据库里不存在的
--   共享**，这比“功能没做”更糟。
--
-- 为什么不改 0005：迁移文件一经应用即不可修改（internal/store 校验内容哈希）。
--   SQLite 也不支持就地修改 CHECK，所以按官方推荐的表重建流程走：
--   建新表 → 拷数据 → 删旧表 → 改名 → 重建索引。整条迁移在 internal/store 的
--   一个事务里执行，因此这里不写 BEGIN/COMMIT（会嵌套报错）。
--
-- 为什么不把 protocol 改成无约束的 TEXT：
--   闭集是这一列的语义。判决层只认 'smb' 与 'nfs'（internal/shares/types.go 的
--   ProtocolSMB / ProtocolNFS），写进第三个值就是“库里有一条谁也读不懂的共享”，
--   而它会在下一次 ListShares 渲染配置时被静默跳过——用户看到的是共享凭空消失。
--   留一条 CHECK，坏值在写入那一刻就炸。
--
-- 为什么 shares 不用管外键：
--   shares 是 managed_resources 的子表，没有别的表引用它（grep migrations/agentdb 里
--   REFERENCES shares 是空的），所以 DROP TABLE shares 只影响自己。foreign_keys 在这个
--   连接上是开的（internal/store/store.go），事务内改不了 PRAGMA——这里不需要改：
--   删子表、建新子表都不触发外键检查。
--
-- 回填时不需要任何映射：旧 CHECK 只允许 'smb'，表里不可能有别的值。definition_json
-- 里存的 Protocol 与列上的值是同一个字段（SaveShare 就是这么写的），所以也不需要
-- 在 JSON 里做改写。

CREATE TABLE shares_new (
    id                 TEXT    PRIMARY KEY CHECK (length(id) = 36),
    name               TEXT    NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 64),
    protocol           TEXT    NOT NULL CHECK (protocol IN ('smb', 'nfs')),
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
    updated_at         INTEGER NOT NULL CHECK (updated_at >= created_at),
    runtime_revoked_json TEXT  NOT NULL DEFAULT ''
);

INSERT INTO shares_new (id, name, protocol, volume_id, relative_path, enabled, read_only,
                        definition_json, config_hash, directory_prepared, config_applied,
                        version, last_operation_id, created_at, updated_at, runtime_revoked_json)
SELECT id, name, protocol, volume_id, relative_path, enabled, read_only,
       definition_json, config_hash, directory_prepared, config_applied,
       version, last_operation_id, created_at, updated_at, runtime_revoked_json
  FROM shares;

DROP TABLE shares;

ALTER TABLE shares_new RENAME TO shares;

-- 索引随旧表一起被删掉了（索引属于表），按 0005 的定义重建：少一个索引不会编译失败，
-- 只会让共享列表在卷多起来之后慢慢变慢。
CREATE INDEX idx_shares_volume ON shares (volume_id, updated_at DESC);
CREATE INDEX idx_shares_name ON shares (name);
