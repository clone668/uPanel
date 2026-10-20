-- 0005：任务、任务事件与幂等键（P03.2；CONTRACTS.md 14.1 jobs、job_events、idempotency_keys，
-- 4.2 创建任务、4.3 任务状态机）。
--
-- 这三张表解决的是“同一次意图只能执行一次”，而且要跨 HTTP 重试、重复点击、Web 重启都成立：
--
--   jobs             一次提交意图的持久记录。operation_id 是**稳定**标识，在调用代理之前就写好；
--                    网络超时后按它去问代理，绝不生成第二个 operation_id 重新执行。
--   job_events       状态流水（只追加）。P03.2 只写状态类事件，SSE/日志分页属 P03.3。
--   idempotency_keys 幂等键 → 请求体哈希 → job。相同用户 + 相同路由 + 相同键 + 相同请求体
--                    返回原 job；相同键但请求体不同返回 409。
--
-- 列语义：
--   id                     公开 job_id，形如 job_<32 位小写十六进制>
--   operation_id           代理侧操作标识（op_<32 位小写十六进制>），UNIQUE
--   preflight_id           消费的 Web 预检记录（pf_…）
--   user_id                发起人
--   action                 动作登记名（从预检抄下来，便于列表查询与审计）
--   status                 queued → running → succeeded / failed / canceled / interrupted
--   progress_percent       0—100 或 NULL：“不知道”必须是 NULL，不许编一个假的百分比
--   step                   当前或最后一个步骤的稳定码
--   result_json            代理核验后的结果（outcome、verified_at、observed_state、recovery_hint）
--   error_json             失败信息 {code, message}，不含堆栈与命令原文
--   cancellable            动作策略（来自预检）；不可取消的动作不显示取消按钮
--   cancellation_requested 独立的布尔字段：它是“已请求”，不是“已取消”
--   needs_sync             1 表示这个 job 还需要向代理对账（提交超时、或 Web 写库失败留下的标记）
--   created_at / updated_at / started_at / finished_at   RFC3339 UTC 文本，与 Web 其他表一致

CREATE TABLE jobs (
    id                     TEXT    PRIMARY KEY,
    operation_id           TEXT    NOT NULL UNIQUE,
    preflight_id           TEXT    NOT NULL REFERENCES preflights (id) ON DELETE RESTRICT,
    user_id                INTEGER NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    action                 TEXT    NOT NULL,
    status                 TEXT    NOT NULL
        CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled', 'interrupted')),
    progress_percent       INTEGER CHECK (progress_percent IS NULL
        OR (progress_percent >= 0 AND progress_percent <= 100)),
    step                   TEXT    NOT NULL DEFAULT '',
    result_json            TEXT    NOT NULL DEFAULT '{}',
    error_json             TEXT    NOT NULL DEFAULT '{}',
    cancellable            INTEGER NOT NULL DEFAULT 0 CHECK (cancellable IN (0, 1)),
    cancellation_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancellation_requested IN (0, 1)),
    needs_sync             INTEGER NOT NULL DEFAULT 1 CHECK (needs_sync IN (0, 1)),
    created_at             TEXT    NOT NULL,
    updated_at             TEXT    NOT NULL,
    started_at             TEXT    NOT NULL DEFAULT '',
    finished_at            TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_jobs_user_id ON jobs (user_id, created_at DESC);
CREATE INDEX idx_jobs_status ON jobs (status, updated_at DESC);
CREATE INDEX idx_jobs_needs_sync ON jobs (needs_sync, updated_at);

-- 终态是终点：任务中心里“已成功又回到运行中”会让结果彻底不可信（CONTRACTS.md 4.3）。
CREATE TRIGGER jobs_terminal_is_final
BEFORE UPDATE OF status ON jobs
WHEN OLD.status IN ('succeeded', 'failed', 'canceled', 'interrupted')
 AND NEW.status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted')
BEGIN
    SELECT RAISE(ABORT, 'jobs 终态不能回到运行中');
END;

-- job_events：seq 在 job 内单调递增；UNIQUE 让“同一事件写两次”直接失败。
-- 只追加：改写历史会让“实际发生了什么”失去意义。
CREATE TABLE job_events (
    job_id           TEXT    NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq              INTEGER NOT NULL CHECK (seq > 0),
    kind             TEXT    NOT NULL CHECK (kind IN ('status', 'progress', 'log', 'gap')),
    redacted_payload TEXT    NOT NULL DEFAULT '{}',
    created_at       TEXT    NOT NULL,
    PRIMARY KEY (job_id, seq)
);

CREATE TRIGGER job_events_append_only_update
BEFORE UPDATE ON job_events
BEGIN
    SELECT RAISE(ABORT, 'job_events 只追加');
END;

CREATE TRIGGER job_events_append_only_delete
BEFORE DELETE ON job_events
BEGIN
    SELECT RAISE(ABORT, 'job_events 只追加');
END;

-- idempotency_keys：一次意图的重放键。
--
-- 联合唯一约束 (user_id, route, key) 是幂等的**执行**保证：两个并发请求带着同一个键进来，
-- 只有一个能插入成功，另一个必然走“读回原 job”的分支。body_hash 用来区分
-- “同一个键重试同一件事”（返回原 job）与“同一个键换了一件别的事”（409）。
--
-- 保留期（CONTRACTS.md 14.2：至少 24 小时）由 P03.4 的清理任务执行；
-- 清理不能早于关联的执行历史，否则旧键失效会变成重复执行。
CREATE TABLE idempotency_keys (
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    route      TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    body_hash  TEXT    NOT NULL CHECK (length(body_hash) = 64),
    job_id     TEXT    NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL,
    PRIMARY KEY (user_id, route, key)
);

CREATE INDEX idx_idempotency_keys_expires_at ON idempotency_keys (expires_at);
CREATE INDEX idx_idempotency_keys_job_id ON idempotency_keys (job_id);

-- 预检补一列：可取消策略。
--
-- P03.1 的预检表存了绑定信息，但漏了“这个动作能不能安全取消”。取消策略由代理在预检里
-- 下发（Cancellable），Web 必须把它抄下来：任务中心据此决定显不显示取消按钮——
-- 前端不该在提交之后才去问代理“这个能不能取消”，那时操作已经在跑了。
--
-- 默认 0（不可取消）是刻意的保守默认：存量预检都是 P03.1 期间生成的，它们不携带策略；
-- 把未知当“可以取消”会让用户在不可打断的破坏性动作上看到一个假按钮。
ALTER TABLE preflights ADD COLUMN cancellable INTEGER NOT NULL DEFAULT 0
    CHECK (cancellable IN (0, 1));
