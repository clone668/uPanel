-- 0012：告警规则、告警事件与维护静默（P13.2；CONTRACTS.md §13.2、PLAN.md 第 18 节）。
--
-- 三张表一起建，因为它们是同一件事的三个面：
--   alert_rules    规则是什么，以及这条规则**现在处于哪个状态**（状态机就落在规则行上）；
--   alert_events   已经发生过的那一段超限。一个“还在持续”的事件就是还没写 resolved_at 的那一行；
--   alert_silence  维护静默窗口。它是全局的、按时间段的，不是按规则的。
--
-- 为什么状态机落在 alert_rules 行上而不是单独一张表：一条规则的当前状态就是它自己的一部分，
-- 拆开以后“读到一条规则、再读到它半秒前被改过的状态”这种撕裂会变成可能，而状态机的每次
-- 迁移都是一次 UPDATE ... WHERE version = ? 的乐观并发（见 internal/alerts）。一张表一次写，
-- 半行新半行旧的问题从根上不存在。
--
-- 时间列分成两种口径，是刻意的：
--   created_at / updated_at / started_at / resolved_at  是 **RFC3339 文本**，给人读、给接口返回；
--   state_since / recover_since / cooldown_until / last_eval_at  是 **Unix 秒整数**，给状态机算。
-- 状态机每个评估周期都要拿它们和 now 比大小、做减法；拿文本去做这些事要么每行解析一次，
-- 要么就得在 SQL 里拼字符串。同一张表里两种口径必须一眼分得清，所以列名不重复、注释不省略。
--
-- 注意这里**没有** notification_* 列：发通知（P13.3）要记的是“这条事件通知过几次、上次
-- 失败为什么”，那是通知自己的账，塞进事件表会让“事件是否还在持续”这个最常读的判断
-- 混进一堆不相关的字段。

CREATE TABLE alert_rules (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    -- name 是给人看的短名。同一台机器上允许重名——靠 id 定位，不靠名字。
    name                 TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    -- scope 由 metric 决定（metric 在哪个 scope 下登记，scope 就是什么），不接受调用方自选：
    -- 让调用方同时给 scope 和 metric，就多了一种“两者不匹配”的非法输入要解释。
    scope                TEXT    NOT NULL CHECK (scope IN ('host','interface','filesystem')),
    -- target_id 的含义随 scope 变：host 恒为空串，interface 是网卡名，filesystem 是挂载点路径。
    -- 空串而不是 NULL：和 metric_samples 一样，NULL 在唯一性/比较里不参与判断。
    target_id            TEXT    NOT NULL DEFAULT '' CHECK (length(target_id) <= 512),
    metric               TEXT    NOT NULL CHECK (length(metric) BETWEEN 1 AND 64),
    -- comparison 是闭集，符号要拼进统计 SQL（见 store.comparisonExpr）。
    comparison           TEXT    NOT NULL CHECK (comparison IN ('gt','gte','lt','lte')),
    -- threshold 的量纲由 metric 决定（percent / bytes / bytes_per_sec / count），
    -- 库里只存数字，单位跟着指标走，不额外存一列单位——两处写同一件事迟早会不一致。
    threshold            REAL    NOT NULL,
    -- 空间双阈值（PLAN.md 第 18 节：“使用率 + 剩余 GiB、任一满足/全部满足”）。
    --
    -- 原契约里只有一个 secondary_threshold，表达不了 secondary_threshold 是哪个指标的量纲：
    -- “85% 或剩 10 GiB”里的第二个数是 available_bytes，不是 used_percent。所以这里必须有
    -- secondary_metric 和 secondary_comparison，三个字段要么全有、要么全无（见下面的 CHECK）。
    secondary_metric     TEXT    CHECK (secondary_metric IS NULL OR (length(secondary_metric) BETWEEN 1 AND 64)),
    secondary_comparison TEXT    CHECK (secondary_comparison IS NULL OR secondary_comparison IN ('gt','gte','lt','lte')),
    secondary_threshold  REAL,
    -- combination 只在有次级口径时起作用：any=任一满足就触发，all=全部满足才触发。
    combination          TEXT    NOT NULL DEFAULT 'any' CHECK (combination IN ('any','all')),
    -- duration_seconds 是“持续多久才算数”。下限 30 秒：低于 30 秒就是在报瞬时抖动，
    -- 那还不如没有告警。上限 86400 秒（24 小时）= raw 粒度的保留窗口，超过它就没有
    -- 原始点可数了（见 store.MetricWindowStats 的注释）。
    duration_seconds     INTEGER NOT NULL CHECK (duration_seconds BETWEEN 30 AND 86400),
    -- recovery_threshold 是“回落到多少才算恢复”。它必须落在告警线的另一侧（由代码校验方向），
    -- 两条线之间那条带子是**既不告警也不恢复**的死区，靠它挡住阈值附近来回抖导致的刷屏。
    recovery_threshold   REAL    NOT NULL,
    -- cooldown_seconds 是恢复之后多久之内不再重新触发。0 表示不冷却。
    cooldown_seconds     INTEGER NOT NULL DEFAULT 300 CHECK (cooldown_seconds BETWEEN 0 AND 86400),
    enabled              INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    -- version 是乐观并发：每次改规则（包括状态迁移以外的用户改动）都 +1，
    -- 客户端提交时必须带上它读到的那个值，不匹配就是 409。
    version              INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    -- 状态机：ok（没事）/ pending（已经超限但还没持续够）/ firing（已触发，事件还没恢复）。
    --
    -- “数据缺失”不进状态机——它不是第四个状态，而是一次“这一轮不做判断”。
    -- 把缺失写成一种状态，就会在采集停掉的那一刻把“不知道”变成结论。
    state                TEXT    NOT NULL DEFAULT 'ok' CHECK (state IN ('ok','pending','firing')),
    -- state_since：进入当前状态的时刻（Unix 秒）。pending 用它算“持续了多久”，firing 用它做显示。
    state_since          INTEGER NOT NULL DEFAULT 0 CHECK (state_since >= 0),
    -- recover_since：firing 状态下“从什么时候开始已经回到恢复线以内”。
    -- 0 表示这一轮还在限外。恢复也要持续 duration_seconds，靠它计时。
    recover_since        INTEGER NOT NULL DEFAULT 0 CHECK (recover_since >= 0),
    -- cooldown_until：冷却到期时刻（Unix 秒）。为 0 或已过，都不冷却。
    cooldown_until       INTEGER NOT NULL DEFAULT 0 CHECK (cooldown_until >= 0),
    last_eval_at         INTEGER NOT NULL DEFAULT 0 CHECK (last_eval_at >= 0),
    -- last_value 是最近一次评估看到的 peak（朝告警方向最极端的值），用来在列表里显示
    -- “现在大概多少”，不是精确的当前值。NULL 表示还没评估过。
    last_value           REAL,
    -- last_reason 记录最近一次“这一轮为什么没判断”（数据缺失等），上限 200 字。
    -- 它不改变状态，只是让人能回答“规则为什么一直不响”。
    last_reason          TEXT    NOT NULL DEFAULT '' CHECK (length(last_reason) <= 200),
    created_at           TEXT    NOT NULL,
    updated_at           TEXT    NOT NULL,
    -- 双阈值要么三个字段全给，要么一个都不给。半套规则没有意义：
    -- 有 metric 没 threshold 是空条件，有 threshold 没 metric 不知道量纲。
    CHECK ((secondary_metric IS NULL AND secondary_comparison IS NULL AND secondary_threshold IS NULL)
        OR (secondary_metric IS NOT NULL AND secondary_comparison IS NOT NULL AND secondary_threshold IS NOT NULL)),
    -- 次级口径必须和主口径在同一个 scope 下（同一张表的同一行数据才在同一个窗口里可比）；
    -- 具体哪两个指标能配对由代码校验（internal/alerts 的 scope 闭集），SQL 里只挡住
    -- 明摆着的“主次同名”。
    CHECK (secondary_metric IS NULL OR secondary_metric <> metric)
);

-- 评估循环每轮只取 enabled = 1 的规则，按 id 稳定顺序。
CREATE INDEX idx_alert_rules_enabled ON alert_rules (enabled, id);

CREATE TABLE alert_events (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    -- rule_id 是触发它的规则。删除规则时**置空而不是级联删除**：
    -- “这台机器上周磁盘写满过一次”是一条历史，规则后来被删掉不改变这件事发生过。
    -- 事件行里本来就存了规则快照（下面几列），置空之后照样读得懂。
    -- 置空也可能发生在事件还没恢复的时候，所以删除规则的动作会先把打开的事件收口
    -- （见 internal/alerts 的 Delete），不留一条永远 firing 的孤儿行。
    rule_id          INTEGER REFERENCES alert_rules (id) ON DELETE SET NULL,
    -- 下面这些列是**触发那一刻的快照**：规则被改名、改阈值、甚至删掉之后，
    -- 这条历史事件仍然要能说清“当时是什么规则、什么条件、什么目标”。
    -- 显示用的字段一律读这里的快照，不回 join 规则表——join 出去的值会随规则
    -- 改动而“穿越”，让一条旧事件看起来像是按新阈值触发的。
    rule_name        TEXT    NOT NULL,
    scope            TEXT    NOT NULL,
    target_id        TEXT    NOT NULL DEFAULT '',
    metric           TEXT    NOT NULL,
    comparison       TEXT    NOT NULL,
    threshold        REAL    NOT NULL,
    -- 状态：firing（还在持续）/ resolved（已经恢复）。一个事件周期只有一行——
    -- 恢复是把这一行原地写上 resolved_at，不是再插一条“恢复事件”。
    -- 这样“恢复只发一次、未变化不刷屏”是结构保证，不是逻辑保证。
    state            TEXT    NOT NULL CHECK (state IN ('firing','resolved')),
    started_at       TEXT    NOT NULL,
    resolved_at      TEXT,
    -- duration_seconds 是实际持续时长（恢复时算好写死），不是规则上的配置值。
    duration_seconds INTEGER,
    -- peak_value 是这一整个事件期间最极端的值（恢复时按 [started_at, resolved_at] 重算）。
    peak_value       REAL,
    -- silenced 表示这条事件触发时正处在维护静默窗口里。事件照记，只是通知（P13.3）不发。
    -- 静默不改变规则的判定状态：机器确实超限了，这是事实，不该因为“正在维护”就假装没发生。
    silenced         INTEGER NOT NULL DEFAULT 0 CHECK (silenced IN (0,1)),
    -- 确认（ack）是 P13.4 的界面动作，列在这里是因为它属于事件本身而不是另一件事；
    -- 接口按 P13.4 再开放，先留着列免得为两个可空字段再迁一次库。
    acked_at         TEXT,
    acked_by         TEXT,
    created_at       TEXT    NOT NULL,
    -- resolved 的行必须有 resolved_at，反之亦然。数据库层面把这条不变量钉死，
    -- 免得出现“状态是 resolved 但没有恢复时间”这种只能靠人解释的行。
    CHECK ((state = 'firing' AND resolved_at IS NULL) OR (state = 'resolved' AND resolved_at IS NOT NULL))
);

-- 一条规则同一时刻最多只有一个未恢复的事件（去重的结构保证）。
CREATE UNIQUE INDEX idx_alert_events_open ON alert_events (rule_id) WHERE state = 'firing';
-- 事件列表按时间倒序翻页。
CREATE INDEX idx_alert_events_started ON alert_events (started_at DESC, id DESC);
CREATE INDEX idx_alert_events_rule ON alert_events (rule_id, started_at DESC);

CREATE TABLE alert_silence (
    -- 单行表：维护静默是**全局**的一个时间窗，不是每规则一份。
    -- PLAN.md 第 18 节要的是“维护期间不吵”，而维护是整台机器的维护——
    -- 一台机器上二十条规则各自设窗口，只会让人在维护开始前忘掉其中三条。
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    -- starts_at / ends_at 为 NULL 表示“没有静默窗口”。两端都用 RFC3339 文本：
    -- 它们要原样回给界面显示。
    starts_at  TEXT,
    ends_at    TEXT,
    reason     TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 200),
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    CHECK ((starts_at IS NULL AND ends_at IS NULL) OR (starts_at IS NOT NULL AND ends_at IS NOT NULL))
);

INSERT INTO alert_silence (id, starts_at, ends_at, reason, updated_at, updated_by)
VALUES (1, NULL, NULL, '', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), '');
