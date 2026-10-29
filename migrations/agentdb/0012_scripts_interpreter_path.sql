-- 代理数据库第 12 条迁移：scripts.interpreter 存解释器的绝对路径（P12.4 修正）。
--
-- 为什么改：
--   0011 把这一列约束成 'sh'，于是表里存的是基名。同一个名字在契约里是
--   `GET /scripts/:id` 的 interpreter 字段，枚举值是 `/bin/sh`——列名相同、
--   取值不同，读库的人必须知道"这里要换个说法"才看得懂，而 API 读库的人
--   忘了换就会把基名原样回给前端（P12.4 的 24.04 验收就是这么红的：
--   期望 '/bin/sh'，实际 'sh'）。
--
--   这一列回答的问题只有一个：**这台机器被批准用哪个解释器跑这份脚本**。
--   答案是一个绝对路径，不是 PATH 上的某个名字。基名 'sh' 在不同 PATH 下
--   指向不同二进制，把它当"已批准的解释器"本身就是含糊的。
--
-- 为什么不改 0011：迁移文件一经应用即不可修改（internal/store 会校验内容哈希）。
--   SQLite 也不支持就地修改 CHECK 约束，所以这里按官方推荐的表重建流程走：
--   建新表 → 拷数据（顺带把 'sh' 归一成 '/bin/sh'）→ 删旧表 → 改名 → 重建索引。整条迁移在
--   internal/store 的一个事务里执行，因此这里不写 BEGIN/COMMIT（会嵌套报错）。
--
-- 回填规则只有一条：'sh' → '/bin/sh'。除此之外的任何值都不该存在（旧 CHECK 只允许
-- 'sh'），所以不需要更复杂的映射；如果真出现别的值，下面的 CHECK 会在拷贝时炸掉，
-- 而不是悄悄放过。

CREATE TABLE scripts_new (
    id               TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    content          TEXT    NOT NULL,
    content_hash     TEXT    NOT NULL,
    agent_script_ref TEXT    NOT NULL,
    interpreter      TEXT    NOT NULL DEFAULT '/bin/sh' CHECK (interpreter IN ('/bin/sh')),
    version          INTEGER NOT NULL CHECK (version >= 1),
    created_at       INTEGER NOT NULL CHECK (created_at > 0),
    updated_at       INTEGER NOT NULL CHECK (updated_at >= created_at),

    CHECK (length(id) = 36),
    CHECK (substr(id, 1, 4) = 'scr_'),
    CHECK (length(name) BETWEEN 1 AND 80),
    CHECK (length(content) BETWEEN 1 AND 65536),
    CHECK (length(content_hash) = 64),
    CHECK (length(agent_script_ref) BETWEEN 1 AND 512)
);

INSERT INTO scripts_new (id, name, content, content_hash, agent_script_ref, interpreter,
                         version, created_at, updated_at)
SELECT id, name, content, content_hash, agent_script_ref,
       CASE interpreter WHEN 'sh' THEN '/bin/sh' ELSE interpreter END,
       version, created_at, updated_at
  FROM scripts;

DROP TABLE scripts;

ALTER TABLE scripts_new RENAME TO scripts;

CREATE UNIQUE INDEX ux_scripts_name ON scripts (name);
CREATE INDEX idx_scripts_updated ON scripts (updated_at DESC);
