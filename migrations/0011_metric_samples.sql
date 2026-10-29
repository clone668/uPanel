-- 0011：历史监控样本（P13.1；CONTRACTS.md §14.1 的 metric_samples、PLAN.md 第 18 节）。
--
-- 这张表是**只增的历史**：原始 5 秒样本保留 24 小时、1 分钟聚合 30 天、1 小时聚合
-- 365 天（PLAN.md 第 18 节），由 internal/metrics 的维护循环按时间裁剪。它不是
-- 实时数据的第二来源——当前值仍然只来自代理的采集快照（GET /system/summary），
-- 这里只回答“过去怎么走的”。
--
-- 长格式（一行一个序列一个时间点）而不是宽表：指标集合会随 P13.2 的告警规则扩展，
-- 加一个指标不应该是一次 schema 变更。代价是行数，换来的是“未知的指标只是没有行”。
--
-- 列语义：
--   metric_samples.scope      序列的层级，闭集：host（主机级）、interface（网卡）、
--                                filesystem（挂载点）。scope 决定 target_id 的含义，
--                                因此查询必须指定 scope——不允许“全表扫一遍找名字”。
--   metric_samples.target_id  二级定位：interface=网卡名，filesystem=挂载点路径，
--                                host=空串。空串而不是 NULL：主键里 NULL 不参与唯一性判断，
--                                那会让“同一条主机指标写两遍”变成合法。
--   metric_samples.metric     指标名，闭集由 internal/metrics 登记（见 metrics.Series）；
--                                单位写在名字里（_percent、_bytes_per_sec、_bytes），
--                                CONTRACTS.md 第 1 节的命名规则。
--   metric_samples.resolution 采样粒度，闭集：raw（5 秒）/ minute（1 分钟）/ hour（1 小时）。
--   metric_samples.ts         样本时间，**Unix 秒，UTC**，且已对齐到 resolution 的边界。
--                                这里刻意不用 RFC3339 文本（其它表用文本是为了人读）：
--                                聚合与裁剪要做整除、比较和 GROUP BY，整数才是对的口径；
--                                接口层再翻译回 RFC3339（PLAN.md 第 1 节：存储 UTC）。
--   metric_samples.value      聚合值。raw 行就是那一次采集的值；minute / hour 行是
--                                该桶内所有下钻样本按 sample_count 加权的平均值。
--   metric_samples.min_value / max_value
--                                **同一个桶**里的最小与最大值。只有 value 的话，
--                                “这一分钟里 CPU 冲到 100% 又落回来”在历史里彻底消失；
--                                告警（P13.2）与图表（P13.4）都需要这个区间。
--   metric_samples.sample_count 参与聚合的样本数（raw 恒为 1）。它同时是“这一桶有多少
--                                数据”的证据：缺失的时间段**没有行**，不会被写成 0 分的
--                                平均值——PLAN.md 第 18 节与 ALERT-01 都要求丢失的数据
--                                不能当零。平均时按它加权，避免“一分钟里只有 3 个样本”
--                                和“整整 12 个样本”在下一个粒度里等权。
--
-- 主键就是序列 + 粒度 + 时间：重复采集同一时刻（代理的快照在 5 秒窗口内是同一份）
-- 会撞主键，写入用 INSERT OR IGNORE 变成幂等，聚合用 INSERT OR REPLACE 可以安全重跑
-- ——重启后补算历史不会产生第二份数据。
--
-- 没有 version/updated_at 列，也没有代理侧的对应表：样本不是元数据，它是**事实记录**。
-- 事实不该被版本化地“修改”，只能被保留策略删掉。

CREATE TABLE metric_samples (
    scope        TEXT    NOT NULL CHECK (scope IN ('host', 'interface', 'filesystem')),
    target_id    TEXT    NOT NULL DEFAULT '' CHECK (length(target_id) <= 512),
    metric       TEXT    NOT NULL CHECK (length(metric) BETWEEN 1 AND 64),
    resolution   TEXT    NOT NULL CHECK (resolution IN ('raw', 'minute', 'hour')),
    ts           INTEGER NOT NULL CHECK (ts > 0),
    value        REAL    NOT NULL,
    min_value    REAL    NOT NULL,
    max_value    REAL    NOT NULL,
    sample_count INTEGER NOT NULL DEFAULT 1 CHECK (sample_count > 0),
    -- value 落在 [min, max] 之外说明聚合代码算错了，让库把它挡在写入这一刻。
    CHECK (min_value <= value AND value <= max_value),
    PRIMARY KEY (scope, target_id, metric, resolution, ts)
) WITHOUT ROWID;

-- 保留策略的裁剪按 (resolution, ts) 走：删的是“某个粒度下更早的时间”，
-- 主键前缀从 scope 开始，帮不上这个忙，因此单独一条索引。
CREATE INDEX idx_metric_samples_prune ON metric_samples (resolution, ts);
