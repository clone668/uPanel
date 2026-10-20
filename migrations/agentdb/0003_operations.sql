-- 代理数据库第 3 条迁移：执行操作与操作事件（CONTRACTS.md 14.2 operations/operation_events、4.2）。
--
-- 这张表是“一次系统操作的代理侧真相”：Web 建 job 时先落一个稳定的 operation_id，再拿它来提交；
-- 从这里往后，代理只认 operation_id，重启、超时、重复提交都靠它对齐，绝不重新执行。
--
-- 列语义：
--   operation_id        Web 生成的稳定操作标识，形如 op_<32 位小写十六进制>；主键
--   request_hash        SHA-256(动作 + 参数哈希 + 预检引用 + 用户 + 会话)：同一个
--                       operation_id 用不同请求再来一次必须被拒绝，不能把两次意图混成一次
--   action              动作登记名
--   preflight_ref       消费掉的预检引用（UNIQUE：一份预检只能换一个操作）
--   params_json         预检里的**规范化参数**：执行按它来，不再接受新参数（4.2）
--   params_hash         SHA-256(params_json)
--   identity_fingerprint / state_version
--                       预检时的目标身份与状态快照：执行前复核，变了就拒绝
--   target_json         目标的展示信息（display_name、identity_fingerprint）：
--                        预检行会被清理，执行历史必须能独立说明“改的是哪个对象”
--   locks_json          执行时要持有的锁（规范字符串，已排序）
--   user_id             发起人（Web 侧 users.id，供对账与审计）
--   session_hash        发起会话的令牌哈希
--   request_id          提交时的 request_id，便于与日志对齐
--   stage               accepted → running → succeeded / failed / canceled / interrupted
--   step                当前或最后一个步骤的稳定码
--   result_json         结果：outcome（verified/accepted/…）、verified_at、observed_state、recovery_hint
--   error_json          失败信息 {code, message}，不含堆栈与命令原文
--   cancellation_requested
--                       独立的布尔字段（CONTRACTS.md 4.3：它不是“已取消”）
--   created_at / started_at / finished_at / updated_at    Unix 毫秒
--
-- 为什么参数要复制一份而不是只存 preflight_ref：预检行会在过期后被清理，而执行历史
-- 必须能独立解释“当时到底执行了什么”。复制的是规范化参数，不是新参数。

CREATE TABLE operations (
    operation_id           TEXT    PRIMARY KEY,
    request_hash           TEXT    NOT NULL CHECK (length(request_hash) = 64),
    action                 TEXT    NOT NULL,
    preflight_ref          TEXT    NOT NULL,
    params_json            TEXT    NOT NULL,
    params_hash            TEXT    NOT NULL CHECK (length(params_hash) = 64),
    identity_fingerprint   TEXT    NOT NULL,
    state_version          TEXT    NOT NULL,
    target_json            TEXT    NOT NULL DEFAULT '{}',
    locks_json             TEXT    NOT NULL DEFAULT '[]',
    user_id                INTEGER NOT NULL CHECK (user_id > 0),
    session_hash           TEXT    NOT NULL CHECK (length(session_hash) = 64),
    request_id             TEXT    NOT NULL DEFAULT '',
    stage                  TEXT    NOT NULL
        CHECK (stage IN ('accepted', 'running', 'succeeded', 'failed', 'canceled', 'interrupted')),
    step                   TEXT    NOT NULL DEFAULT '',
    result_json            TEXT    NOT NULL DEFAULT '{}',
    error_json             TEXT    NOT NULL DEFAULT '{}',
    cancellation_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancellation_requested IN (0, 1)),
    created_at             INTEGER NOT NULL CHECK (created_at > 0),
    started_at             INTEGER NOT NULL DEFAULT 0,
    finished_at            INTEGER NOT NULL DEFAULT 0,
    updated_at             INTEGER NOT NULL CHECK (updated_at > 0)
);

-- 一份预检只能换一个操作：一次性不仅是业务约定，也让“重放同一份预检”在库层面失败。
CREATE UNIQUE INDEX idx_operations_preflight_ref ON operations (preflight_ref);
CREATE INDEX idx_operations_stage ON operations (stage, updated_at DESC);
CREATE INDEX idx_operations_user_id ON operations (user_id, created_at DESC);

-- 终态是终点：任务中心里“已成功又回到运行中”只会让结果不可信。
CREATE TRIGGER operations_terminal_is_final
BEFORE UPDATE OF stage ON operations
WHEN OLD.stage IN ('succeeded', 'failed', 'canceled', 'interrupted')
 AND NEW.stage NOT IN ('succeeded', 'failed', 'canceled', 'interrupted')
BEGIN
    SELECT RAISE(ABORT, 'operations 终态不能回到运行中');
END;

-- operation_events 是代理侧的状态/日志流水（P03.2 只写状态类事件，日志与 SSE 属 P03.3）。
-- seq 按操作内单调递增，UNIQUE(operation_id, seq) 让“同一事件写两次”直接失败。
--
-- 这里不写 ON DELETE CASCADE：执行历史没有删除路径（下面的触发器已经把删除堵死），
-- 声明级联只会让人以为“删除操作会顺手清掉事件”。
CREATE TABLE operation_events (
    operation_id TEXT    NOT NULL REFERENCES operations (operation_id),
    seq          INTEGER NOT NULL CHECK (seq > 0),
    kind         TEXT    NOT NULL CHECK (kind IN ('accepted', 'started', 'progress', 'log', 'finished', 'canceled', 'error')),
    payload_json TEXT    NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL CHECK (created_at > 0),
    PRIMARY KEY (operation_id, seq)
);

-- 事件只追加：改写历史会让“实际发生了什么”失去意义。
CREATE TRIGGER operation_events_append_only_update
BEFORE UPDATE ON operation_events
BEGIN
    SELECT RAISE(ABORT, 'operation_events 只追加');
END;

CREATE TRIGGER operation_events_append_only_delete
BEFORE DELETE ON operation_events
BEGIN
    SELECT RAISE(ABORT, 'operation_events 只追加');
END;
