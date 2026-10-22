-- 0007：应用、分组、收藏与依赖卷（P09.1；CONTRACTS.md 14.1 app_groups / apps / app_favorites /
-- app_volume_dependencies，8 应用契约，PLAN.md 11）。
--
-- 这四张表只描述“这个应用是什么、绑在哪个对象上、入口在哪”，**不含任何运行时状态**：
-- 容器跑没跑、HTTP 通不通由 P09.2/P09.3 探测后单独返回，绝不写进元数据表。
-- 两者混在一起就会出现“元数据说 running，其实早就停了”这种自相矛盾的记录。
--
-- 列语义：
--   apps.type            link | systemd | docker | compose，判别字段
--   apps.binding_json    按 type 严格判别的绑定对象，规范 JSON：
--                          link     {}
--                          systemd  {"unit":"nginx.service"}
--                          docker   {"container_id":"<64 位小写十六进制>"}
--                          compose  {"deployment_id":"dep_<32 位小写十六进制>"}
--                        规范化（字段顺序固定、无多余键）由 apps 包保证，见下面的唯一索引。
--   apps.public_url      浏览器打开的入口地址；允许为空（内部应用没有对外入口）。
--                        **不是**健康检查或反向代理的目标，两者永远分开存。
--   apps.health_json     健康检查定义；enabled=false 时是一份全零的规范对象。
--   apps.group_id        可空外键；NULL 表示未分组。
--   apps.version         乐观锁；PATCH 用 WHERE id=? AND version=?，零行即 409。
--
-- ── 为什么 app_volume_dependencies 没有指向 volumes 的外键 ───────────────────────
--
-- CONTRACTS.md 14.1 的逻辑模型里写的是“volume_id FK”，但 volumes 是**代理库**里的记录
-- （卷由 root 侧采集与登记，Web 只有只读视图）。Web 库里根本没有 volumes 表，
-- 在这里建一个影子表就等于把代理的现场状态抄一份到 Web——那正是“两边对不上”的来源。
--
-- 所以本表只保存对代理卷标识的**引用**，形状用 CHECK 约束（vol_ + 32 位小写十六进制），
-- 存在性在写入时向代理确认（internal/apps 的 VolumeChecker）；代理不可用时拒绝写入，
-- 而不是先存下一个可能永远对不上的引用。这条偏离记在 docs/DECISIONS.md Q248。

CREATE TABLE app_groups (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 40),
    sort_order INTEGER NOT NULL DEFAULT 0 CHECK (sort_order >= 0),
    version    INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);

CREATE INDEX idx_app_groups_sort ON app_groups (sort_order, id);

CREATE TABLE apps (
    id           TEXT    PRIMARY KEY,
    name         TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    description  TEXT    NOT NULL DEFAULT '' CHECK (length(description) <= 1000),
    icon_key     TEXT    NOT NULL DEFAULT '',
    group_id     TEXT    REFERENCES app_groups (id) ON DELETE RESTRICT,
    type         TEXT    NOT NULL CHECK (type IN ('link', 'systemd', 'docker', 'compose')),
    binding_json TEXT    NOT NULL CHECK (length(binding_json) <= 400),
    public_url   TEXT    NOT NULL DEFAULT '' CHECK (length(public_url) <= 2048),
    health_json  TEXT    NOT NULL CHECK (length(health_json) <= 1000),
    version      INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,
    -- link 没有运行绑定：判别模型在库层也守一道，不指望只有 Go 代码记得。
    CHECK (type <> 'link' OR binding_json = '{}')
);

-- 列表排序键 (name, id)：name 允许重复（CONTRACTS.md 14.1 没有给 apps.name 唯一约束），
-- 因此游标必须带上 id 才能稳定翻页。
CREATE INDEX idx_apps_name ON apps (name, id);
CREATE INDEX idx_apps_group ON apps (group_id);

-- 同一个可绑定对象只属于一个应用：一个容器、一个单元、一个部署各自只能被登记一次。
--
-- 用部分唯一索引而不是 Go 里的“先查再写”：并发两次提交在库里撞一下就是 409，
-- 检查与写入不可能是两个彼此独立、中间还能被插队的步骤。
-- link 被排除在外——它没有绑定对象，所有 link 的 binding_json 都是 '{}'。
CREATE UNIQUE INDEX uq_apps_binding ON apps (type, binding_json) WHERE type <> 'link';

CREATE TABLE app_favorites (
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    app_id     TEXT    NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (user_id, app_id)
);

-- 收藏是“谁的首页”，删账号或删应用时跟着走（CONTRACTS.md 14.1：明确从属对象可 CASCADE），
-- 与应用本身的存在不构成依赖。
CREATE INDEX idx_app_favorites_app ON app_favorites (app_id);

CREATE TABLE app_volume_dependencies (
    app_id    TEXT    NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    volume_id TEXT    NOT NULL CHECK (length(volume_id) = 36 AND substr(volume_id, 1, 4) = 'vol_'),
    position  INTEGER NOT NULL CHECK (position >= 0),
    PRIMARY KEY (app_id, volume_id)
);

CREATE UNIQUE INDEX uq_app_volume_dependencies_pos ON app_volume_dependencies (app_id, position);
