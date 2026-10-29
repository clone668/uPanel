-- 代理数据库第 8 条迁移：备份策略镜像（P12.2；CONTRACTS.md 第 12、14.2 节）。
--
-- 为什么策略也存代理一份：`backup.run` 的参数和 `backup.repository.initialize` 一样
-- 被冻结成 `policy_id` + `version`（CONTRACTS.md 第 5 节），代理必须能从**自己**的库里
-- 按 id 找到策略，并在执行前核对版本——否则“预检时答应备份这三个目录、执行时换成另三个”
-- 就没有任何东西挡得住。
--
-- 库与仓库镜像的关系：policy_json 里只有 repository_id，没有仓库位置。仓库口令、SFTP
-- 私钥、主机名一个都不在这张表里；删掉一条策略不会删仓库、也不会删快照
-- （CONTRACTS 第 12 节）。
--
-- 列语义：
--   id               策略标识 pol_<32 位小写十六进制>；与 Web 侧同名
--   name             显示名；UNIQUE（两条同名策略会让审计记录分不清谁是谁）
--   repository_id    指向 backup_repositories.id；不建外键：仓库定义先被删掉时，
--                    策略要留下来让预检报 BACKUP_REPOSITORY_MISSING，而不是被级联删掉
--   consistency_mode files_only / stop_managed_apps（闭集）
--   policy_json      规范化后的策略（来源、应用名单、保留），无明文秘密
--   version          乐观并发令牌：每次成功变更自增；动作参数里的 version 与它比对
--   created_at       创建时间（Unix 毫秒，UTC）
--   updated_at       最后修改时间（Unix 毫秒，UTC）
--
-- 为什么 repository_id 与 consistency_mode 单独成列：界面要按仓库分组、要按口径筛，
-- 而且“一条 files_only 策略带着 app_ids”这类错位在数据库层就能被人看见。

CREATE TABLE backup_policies (
    id               TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    repository_id    TEXT    NOT NULL,
    consistency_mode TEXT    NOT NULL CHECK (consistency_mode IN ('files_only', 'stop_managed_apps')),
    policy_json      TEXT    NOT NULL,
    version          INTEGER NOT NULL CHECK (version >= 1),
    created_at       INTEGER NOT NULL CHECK (created_at > 0),
    updated_at       INTEGER NOT NULL CHECK (updated_at >= created_at),

    CHECK (length(id) = 36),
    CHECK (substr(id, 1, 4) = 'pol_'),
    CHECK (length(name) BETWEEN 1 AND 64),
    CHECK (length(repository_id) = 36),
    CHECK (substr(repository_id, 1, 4) = 'rep_'),
    CHECK (length(policy_json) BETWEEN 2 AND 65536)
);

CREATE UNIQUE INDEX ux_backup_policies_name ON backup_policies (name);
CREATE INDEX idx_backup_policies_repository ON backup_policies (repository_id);
CREATE INDEX idx_backup_policies_updated ON backup_policies (updated_at DESC);

-- 仓库被删掉时策略要留下：没有这条触发器，删仓库就顺手把“我们答应备份什么”一起删了，
-- 而管理员的预期是“仓库没了，策略还在，预检告诉我仓库没了”。这里只加注释，不加外键，
-- 也不加级联。
