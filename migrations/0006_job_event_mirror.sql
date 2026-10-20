-- 0006：任务事件镜像水位 + 日志容量上限（P03.3；CONTRACTS.md 4.3 SSE、job_events）。
--
-- 两件事：
--   1. jobs.agent_event_seq：镜水位，避免每次对账把代理事件重复抄进 job_events；
--   2. job_events 的"只追加"约束开一个**唯一且很窄**的口子：只允许删除 kind='log'。
--
-- ── 为什么守不住"绝对只追加" ────────────────────────────────────────────────
--
-- CONTRACTS.md 4.3 要求"限制日志容量"。日志是唯一会持续增长的事件类型（一条长任务可以
-- 产出上万行），不设上限就等于让一个任务把整块磁盘写满。控制容量只有两条路：
--
--   ① 到上限就不再写新日志 —— 保住最旧的 N 条，丢掉**最新**的。可日志的价值恰恰在末尾
--      （错误通常发生在最后三行），这么做的结果是"出问题时看不到原因"；
--
--   ② 环形保留 —— 写新的、删最旧的。要删就得允许 DELETE。
--
-- 选 ②。但口子必须窄到不能再窄：
--   - **只**允许 DELETE kind='log'；status / progress / gap 三类事件仍然绝对只追加。
--     状态与进度是"这个任务走到哪一步"的唯一证据，删掉它重连的客户端就拼不出状态机；
--     日志是可以截断的，页面上会显示"更早的日志已清理"。
--   - UPDATE 依旧全表禁止。改写历史与回收容量是两件事：前者永远不允许。
--
-- 这条口子把"只追加"从绝对不变式降级成"状态类事件只追加 + 日志有界"。降级本身
-- 记在 DECISIONS Q68 里，因为它确实削弱了一条 P03.2 立下的约束，不该悄悄改。
--
-- ── 关于 agent_event_seq ───────────────────────────────────────────────────
--
-- 代理侧 operation_events 是**事件的唯一产地**（执行器在真正干活时写 progress/finished）。
-- Web 要推 SSE、要分页看日志，就必须把这些事件镜像到 job_events；但每次对账都会重新
-- 读到同一批事件，没有水位线就会把同一条 progress 抄几十遍，前端看到的是进度条来回跳。
--
-- 语义：jobs.agent_event_seq = "已经镜像到 job_events 的、代理侧最大的 event seq"。
-- 镜像时只取 seq > agent_event_seq 的事件，并在**同一个事务**里把水位推到新的最大值：
-- 要么这批事件和它的水位一起落地，要么一起不落地，不存在"抄了事件但水位没动"的半截状态。
--
-- 它和 job_events.seq 不是一回事：后者是 Web 的推送序号（SSE 的 id），前者是代理侧序号。
-- 两者都不能拿来当对方用。

ALTER TABLE jobs ADD COLUMN agent_event_seq INTEGER NOT NULL DEFAULT 0
    CHECK (agent_event_seq >= 0);

DROP TRIGGER job_events_append_only_delete;

-- 新触发器：允许删除日志，其余照旧拒绝。
--
-- WHEN 子句写在 BEFORE DELETE 上：OLD.kind 是即将被删的那一行的类型。
CREATE TRIGGER job_events_append_only_delete
BEFORE DELETE ON job_events
WHEN OLD.kind <> 'log'
BEGIN
    SELECT RAISE(ABORT, 'job_events 只追加（仅日志行可按容量上限回收）');
END;
-- 注：job_events_append_only_update 保持不变（UPDATE 仍然全表禁止）。
