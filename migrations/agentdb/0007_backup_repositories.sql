-- 代理数据库第 7 条迁移：备份仓库定义镜像（P12.1；CONTRACTS.md 第 12、14.2 节）。
--
-- 为什么代理要自己存一份：`backup.repository.initialize` 的参数是冻结的
-- `repository_id` + `version`（CONTRACTS.md 第 5 节），代理必须能按 id 找到定义，
-- 而且**自己**去核对路径与主机（“代理验证路径/主机”）。定义只存在 Web 库里的话，
-- 预检就变成了“Web 说什么代理信什么”。
--
-- 这份镜像里没有明文秘密：只存 secret_ref。仓库口令与 SFTP 私钥都在 secrets 表里
-- 加密保存，两条记录之间只靠引用相连。
--
-- 列语义：
--   id             仓库标识 rep_<32 位小写十六进制>；与 Web 侧同名
--   name           显示名；UNIQUE（两个同名仓库会让审计记录分不清谁是谁）
--   backend        local / sftp（闭集）
--   definition_json 规范化后的定义（Location 联合体、secret_ref），无明文
--   version        乐观并发令牌：每次成功变更自增；动作参数里的 version 与它比对
--   created_at     创建时间（Unix 毫秒，UTC）
--   updated_at     最后修改时间（Unix 毫秒，UTC）
--
-- 为什么把 backend 单独成列：界面要按后端分组，而且“一条 local 定义里带着 host”
-- 这类错位最好在数据库层就能被人看见，而不是埋在 JSON 里。

CREATE TABLE backup_repositories (
    id              TEXT    PRIMARY KEY,
    name            TEXT    NOT NULL,
    backend         TEXT    NOT NULL CHECK (backend IN ('local', 'sftp')),
    definition_json TEXT    NOT NULL,
    version         INTEGER NOT NULL CHECK (version >= 1),
    created_at      INTEGER NOT NULL CHECK (created_at > 0),
    updated_at      INTEGER NOT NULL CHECK (updated_at >= created_at),

    CHECK (length(id) = 36),
    CHECK (substr(id, 1, 4) = 'rep_'),
    CHECK (length(name) BETWEEN 1 AND 64),
    CHECK (length(definition_json) BETWEEN 2 AND 65536)
);

CREATE UNIQUE INDEX ux_backup_repositories_name ON backup_repositories (name);
CREATE INDEX idx_backup_repositories_updated ON backup_repositories (updated_at DESC);
