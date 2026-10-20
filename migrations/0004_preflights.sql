-- 0004：预检记录（P03.1；CONTRACTS.md 14.1 preflights、4.1）。
--
-- 这里的行是“Web 侧的预检凭据”：代理生成计划，Web 把计划里需要复核的绑定信息
-- 抄下来，提交任务时按它判断“这份预检还是不是原来那一份、发起人还是不是同一个人”。
-- 计划本体（规范化参数、目标指纹、确认短语哈希）存在代理库里，Web 不重复保存短语。
--
-- 列语义：
--   id                Web 侧的预检 id，形如 pf_<32 位小写十六进制>
--   user_id           发起预检的账号（外键：账号删除后预检一并消失，不会留下无主凭据）
--   session_hash      会话令牌的 SHA-256：重新登录（会话轮换）后旧预检自动失效
--   action            动作登记名
--   params_hash       SHA-256(规范化参数) 十六进制；提交时与代理侧比对
--   normalized_params 规范化参数（代理返回的原始 JSON 文本，逐字节保存）
--   target_json       目标信息 {display_name, identity_fingerprint, state_version}
--   state_version     目标状态版本快照（单独成列，便于执行前复核与查询）
--   agent_ref         代理侧计划引用（preflight_plans.id）
--   allowed           0/1，代理给出的结论；为 0 时提交必然被拒绝
--   requires_reauth   0/1，提交任务时是否需要 5 分钟内的再认证
--   requires_phrase   0/1，提交任务时是否需要确认短语
--   created_at        预检创建时间（RFC3339，UTC；与 Web 其他表一致用文本）
--   expires_at        到期时间（代理给的同一时间戳：创建 + 120 秒）
--   consumed_job_id   消费它的 job_id；NULL 表示尚未使用（一次性由 P03.2 的提交路径执行）
--
-- 为什么不在这里存确认短语：短语是一次性授权凭证，明文只在预检响应里出现一次，
-- 代理存的是它的哈希。Web 复述一遍只会多一个泄漏点。

CREATE TABLE preflights (
    id                TEXT    PRIMARY KEY,
    user_id           INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    session_hash      TEXT    NOT NULL CHECK (length(session_hash) = 64),
    action            TEXT    NOT NULL,
    params_hash       TEXT    NOT NULL CHECK (length(params_hash) = 64),
    normalized_params TEXT    NOT NULL,
    target_json       TEXT    NOT NULL,
    state_version     TEXT    NOT NULL DEFAULT '',
    agent_ref         TEXT    NOT NULL,
    allowed           INTEGER NOT NULL CHECK (allowed IN (0, 1)),
    requires_reauth   INTEGER NOT NULL CHECK (requires_reauth IN (0, 1)),
    requires_phrase   INTEGER NOT NULL CHECK (requires_phrase IN (0, 1)),
    created_at        TEXT    NOT NULL,
    expires_at        TEXT    NOT NULL,
    consumed_job_id   TEXT
);

CREATE INDEX idx_preflights_user_id ON preflights (user_id, created_at DESC);
CREATE INDEX idx_preflights_expires_at ON preflights (expires_at);
