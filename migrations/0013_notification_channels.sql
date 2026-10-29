-- 0013：通知通道与投递账（P13.3；CONTRACTS.md §13.3、PLAN.md 第 18 节）。
--
-- 两张表，分工是“配置”与“事实”：
--   notification_channels   用户配了什么（发到哪、怎么发、开没开）；
--   notification_deliveries 每一次要发出去的东西发成没发成。
--
-- 为什么投递要单独一张表，而不是在 alert_events 上挂几列：
-- 一条事件要发给 n 个通道，每个通道各自成功、各自失败、各自重试。挂在事件行上就只能
-- 记住“最后一次”的结果，而“发给 A 成功、发给 B 还在重试”恰恰是最常见的状态。
-- 这也是 P13.2 的迁移里刻意不写 notification_* 列的原因（见 0012 的注释）。
--
-- 投递表同时承担**面板内通知**的收件箱：in_app 通道的投递行一写进来就是 sent
-- （它不需要发到任何地方，出现即到达），P13.4 的告警桌面直接读这张表。

CREATE TABLE notification_channels (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    -- 三种类型，闭集。它们的差别不是“协议不同”，而是“秘密在哪、要不要出门”：
    --   in_app   不出门。事件出现在面板里就是送达，没有秘密、没有重试、没有失败。
    --   smtp     出门发邮件。口令在代理的加密库里，只经 secret_ref 引用。
    --   webhook  出门打 HTTPS。可选 bearer 令牌，同样只经 secret_ref 引用。
    type       TEXT    NOT NULL CHECK (type IN ('in_app','smtp','webhook')),
    -- settings 是按 type 解释的受约束键值，写库前由 internal/notifications 解码成
    -- 结构体再**重新序列化**——库里永远不会有本进程不认识的键，也就不会有“页面
    -- 上改不掉的隐藏字段”。这一条比“建表时校验 JSON”重要得多：前者保证的是
    -- 每一次写，后者只保证第一次。
    settings   TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(settings)),
    -- secret_ref 指向代理加密库里的秘密（用途 notification_password）。
    -- 只有引用，没有明文；in_app 不允许有秘密。
    secret_ref TEXT    CHECK (secret_ref IS NULL OR length(secret_ref) BETWEEN 1 AND 128),
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    -- 失败重试上限（PLAN.md 第 18 节要求“失败重试上限”明确配置）。放在通道上而不是
    -- 全局配置里：一个内网 webhook 值得重试 5 次，一个配错的外部 SMTP 一次都嫌多。
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    version    INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    CHECK (type <> 'in_app' OR secret_ref IS NULL)
);

-- 面板内通知恒有且只有一条。它可以被停用（停用之后事件不再进收件箱），
-- 但不能被删掉、也不能再建第二条——收件箱是这个面板的一部分，不是一种可选集成。
CREATE UNIQUE INDEX idx_notification_channels_inapp ON notification_channels (type)
    WHERE type = 'in_app';

CREATE TABLE notification_deliveries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 通道被删掉时置空而不是级联：这条投递“发成功过”是一个已经发生的事实，
    -- 不因为后来撤销了配置就变成没发生过。快照列（name/type/target）保证还读得懂。
    channel_id   INTEGER REFERENCES notification_channels (id) ON DELETE SET NULL,
    channel_name TEXT    NOT NULL,
    channel_type TEXT    NOT NULL CHECK (channel_type IN ('in_app','smtp','webhook')),
    -- target 是“发到哪”的快照：收件人列表拼接、webhook 的 URL、或 in_app 的空串。
    -- 不存秘密，也不存秘密引用——投递账里出现 secret_ref 只会让人以为这里能解出密码。
    target       TEXT    NOT NULL DEFAULT '' CHECK (length(target) <= 1024),
    -- event_id 为 NULL 表示这是一次**测试发送**（POST /notification-channels/:id/test）
    -- 或者它的事件行已经被删掉了。测试发送不参与去重，因为它本来就可以发很多次。
    event_id     INTEGER REFERENCES alert_events (id) ON DELETE SET NULL,
    -- 事件快照：规则名。事件行被删/规则被改名之后，投递账仍然说得清“这是哪条规则的”。
    event_rule   TEXT    NOT NULL DEFAULT '' CHECK (length(event_rule) <= 120),
    kind         TEXT    NOT NULL CHECK (kind IN ('fired','resolved','test')),
    title        TEXT    NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    body         TEXT    NOT NULL CHECK (length(body) <= 4000),
    -- pending=等着发（或等着下一次重试）/ sent=已送达 / failed=放弃
    state        TEXT    NOT NULL CHECK (state IN ('pending','sent','failed')),
    attempts     INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- max_attempts 是**失败重试上限**（PLAN.md 第 18 节）：发不出去的东西不能无限重试，
    -- 一台配错的 SMTP 会让重试队列永远有活干，日志里天天是同一条错。
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    -- next_attempt_at 是 Unix 秒（投递调度要拿它和 now 比大小），0 表示立刻可发。
    next_attempt_at INTEGER NOT NULL DEFAULT 0 CHECK (next_attempt_at >= 0),
    -- last_error 只留最近一次失败的原因（代理回原话，不含秘密）。
    last_error   TEXT    NOT NULL DEFAULT '' CHECK (length(last_error) <= 500),
    -- last_status 是最近一次的事实回执：SMTP 应答码（250/451/…）或 HTTP 状态码。
    -- 0 表示还没尝试过。
    last_status  INTEGER NOT NULL DEFAULT 0,
    sent_at      TEXT,
    created_at   TEXT    NOT NULL,
    -- 发了就该有 sent_at，没发就不该有。与 alert_events 的 resolved 用同一套写法，
    -- 免得出现“状态是已送达但没有送达时间”这种只能靠人解释的行。
    CHECK ((state = 'sent' AND sent_at IS NOT NULL) OR (state <> 'sent' AND sent_at IS NULL))
);

-- 同一事件在同一通道上，每种类型最多一条。这是“恢复只发一次、未变化不刷屏”在
-- 通知这一层的结构保证：重试是把这一行改状态，不是再插一行。
CREATE UNIQUE INDEX idx_notification_deliveries_once
    ON notification_deliveries (event_id, channel_id, kind) WHERE event_id IS NOT NULL;
-- 调度只取到期的 pending 行。
CREATE INDEX idx_notification_deliveries_due
    ON notification_deliveries (state, next_attempt_at, id);
CREATE INDEX idx_notification_deliveries_event
    ON notification_deliveries (event_id, id);
CREATE INDEX idx_notification_deliveries_created
    ON notification_deliveries (created_at DESC, id DESC);

-- 面板内通知的收件箱。默认开着：PLAN.md 第 18 节“默认提供面板内通知”。
-- 它没有 endpoint、没有秘密、也不需要“先配好才能用”。
INSERT INTO notification_channels (name, type, settings, secret_ref, enabled, max_attempts, version, created_at, updated_at)
VALUES ('面板内通知', 'in_app', '{}', NULL, 1, 1, 1,
        strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));
