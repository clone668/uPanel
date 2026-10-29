-- 代理数据库第 10 条迁移：受管理计划任务及其触发记录（P12.4；CONTRACTS.md 第 12、14.2 节）。
--
-- 为什么计划存在这里而不是 Web 那边：定时触发时**没有**管理员在场，也没有会话。
-- 代理要回答的是"这条计划此刻允许跑什么"，而预检里的 normalized_params 是被冻结的
-- （CONTRACTS §15：从代理已批准配置取动作）。定义只有存在代理自己的库里，才不会被
-- 一次请求替换掉——同一份理由已经用在 backup_repositories（0007）与 backup_policies
-- （0008）上。
--
-- 列语义：
--   id              计划标识 sch_<32 位小写十六进制>；同时是单元名的来源
--                   （upanel-sch_<id>.service/.timer）
--   name            显示名；UNIQUE——两条同名的计划在审计里分不清谁是谁
--   kind            backup / certificate_renewal / user_script（闭集）
--   target_id       被定时的对象 id（pol_/crt_/scr_）；"命令文本"在这里根本存不进来，
--                   校验在 internal/schedules，形态在数据库上再核一次
--   definition_json 规范化后的定义（名称、种类、目标、日历、时区、错过执行策略、
--                   最长运行时长、启用意图）；不含任何秘密
--   definition_key  定义指纹：只有它变了才重写单元，避免每次保存都 daemon-reload
--   agent_timer_ref 已落地的定时器单元名；空串表示这台机器上此刻没有这条计划
--   enabled         意图。它**不是**"已生效"：已生效由 agent_timer_ref 与
--                   systemctl is-enabled/is-active 的事实给出
--   version         乐观并发令牌：每次成功变更自增
--
-- 一处刻意的约束：UNIQUE (kind, target_id)。一条备份策略只该被一条计划定时；
-- 允许两条会产生两个同时写同一个仓库的定时器，而"不重叠"规则管不到它们之间。

CREATE TABLE schedules (
    id               TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    kind             TEXT    NOT NULL CHECK (kind IN ('backup', 'certificate_renewal', 'user_script')),
    target_id        TEXT    NOT NULL,
    definition_json  TEXT    NOT NULL,
    definition_key   TEXT    NOT NULL,
    agent_timer_ref  TEXT    NOT NULL DEFAULT '',
    enabled          INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    version          INTEGER NOT NULL CHECK (version >= 1),
    created_at       INTEGER NOT NULL CHECK (created_at > 0),
    updated_at       INTEGER NOT NULL CHECK (updated_at >= created_at),

    CHECK (length(id) = 36),
    CHECK (substr(id, 1, 4) = 'sch_'),
    CHECK (length(name) BETWEEN 1 AND 80),
    CHECK (length(target_id) = 36),
    CHECK (length(definition_json) BETWEEN 2 AND 8192),
    CHECK (length(definition_key) = 64)
);

CREATE UNIQUE INDEX ux_schedules_name ON schedules (name);
CREATE UNIQUE INDEX ux_schedules_target ON schedules (kind, target_id);
CREATE INDEX idx_schedules_enabled ON schedules (enabled, updated_at DESC);

-- schedule_triggers：每一次触发一行。
--
-- 这张表就是"稳定触发 id"的落地处：UNIQUE (schedule_id, slot_at) 让"同一分钟内
-- 第二次触发"在数据库层失败，于是 systemd 的重试、手工 systemctl start、甚至
-- 代理崩溃后重跑的同一次触发，都只会产生一个 operation。
--
-- operation_id 不是外键：代理产生的定时操作可能被 TTL 清理掉，而"这条计划在
-- 2026-10-25T03:30Z 触发过一次、对应哪个操作"必须比操作本身活得更久——
-- 它是审计链路的一环（CONTRACTS §15：定时触发须审计，不能只存在于 journal）。
--
-- schedule_id 也**不建外键**（因此没有级联）：删掉一条计划时，它过去的触发记录要
-- 留下来。级联删掉它们会让"这条计划昨晚跑过没有"随着一次删除永久消失，而这正是
-- 出了事第一个要查的东西。
CREATE TABLE schedule_triggers (
    trigger_id   TEXT    PRIMARY KEY,
    schedule_id  TEXT    NOT NULL,
    slot_at      TEXT    NOT NULL,
    action       TEXT    NOT NULL,
    operation_id TEXT    NOT NULL,
    -- outcome 记录触发**当时**的判决：triggered / blocked / duplicate / failed。
    -- 它不是操作结果（那是 operations 表的事），而是"这次触发有没有开出任务"。
    outcome      TEXT    NOT NULL DEFAULT 'triggered'
        CHECK (outcome IN ('triggered', 'blocked', 'duplicate', 'failed')),
    reason       TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL,

    CHECK (length(trigger_id) = 36),
    CHECK (substr(trigger_id, 1, 4) = 'trg_')
);

CREATE UNIQUE INDEX ux_schedule_triggers_slot ON schedule_triggers (schedule_id, slot_at);
CREATE INDEX idx_schedule_triggers_created ON schedule_triggers (created_at DESC);
CREATE INDEX idx_schedule_triggers_operation ON schedule_triggers (operation_id);

-- 触发记录只追加：改写历史会让"这一晚到底跑过没有"失去意义。
CREATE TRIGGER schedule_triggers_append_only_update
BEFORE UPDATE ON schedule_triggers
BEGIN
    SELECT RAISE(ABORT, 'schedule_triggers 只追加');
END;

CREATE TRIGGER schedule_triggers_append_only_delete
BEFORE DELETE ON schedule_triggers
BEGIN
    SELECT RAISE(ABORT, 'schedule_triggers 只追加');
END;
