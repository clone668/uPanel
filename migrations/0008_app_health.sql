-- 0008：应用健康探测结果（P09.2；CONTRACTS.md 8 应用契约、STAGES.md 11）。
--
-- 这张表**只存探测结论**，不存元数据：health_check 的定义在 apps.health_json 里，
-- 这里放的是“上一次真的去看了一眼，看到了什么”。两者分开放的理由和 P09.1 一样——
-- 定义是用户写的，结论是面板观测到的，混在一行里迟早会出现“定义被改成 8080，
-- 而结论还写着 200 OK，其实是上一个端口的结果”。
--
-- 列语义：
--   app_id        应用标识；删应用时结论跟着走（明确从属对象，CASCADE）。
--   checked_at    这次探测的时间（UTC，秒级）。它同时也是调度器的节流依据：
--                 距离上次探测不足 interval_seconds 就不再探。
--   http_state    ok | error | unknown，与 apps 包的同名枚举一致：
--                   ok       拿到了期望集合里的状态码
--                   error    连不上、超时、TLS 失败，状态码不在期望集合里，或目标已经
--                            不在本机的监听/容器列表里（服务停了也是“不在”）
--                   unknown  这一轮没有得出可用结论（例如代理不可达、发现本身没成功，
--                            或装着这个目标的来源这一轮没采成）
--   status_code   拿到的 HTTP 状态码；没拿到时为 NULL（不是 0——0 不是状态码）。
--   duration_ms   探测耗时（毫秒）。
--   target_id / scheme / port / path / address / url
--                 这次探测**实际使用**的目标与地址。address 永远是 127.0.0.1，
--                 url 是完整请求地址；留档是为了让“为什么这条是 error”能被复核，
--                 而不是只看到一个状态词。
--   error         失败原因；成功时为空串。
--
-- 一行一个应用：健康检查看的是“此刻通不通”，history 属于日志与审计，不属于这里。

CREATE TABLE app_health_results (
    app_id      TEXT    PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    checked_at  TEXT    NOT NULL,
    http_state  TEXT    NOT NULL CHECK (http_state IN ('ok', 'error', 'unknown')),
    status_code INTEGER          CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),
    duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
    target_id   TEXT    NOT NULL CHECK (length(target_id) BETWEEN 1 AND 52),
    scheme      TEXT    NOT NULL CHECK (scheme IN ('http', 'https')),
    port        INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    path        TEXT    NOT NULL CHECK (length(path) BETWEEN 1 AND 256),
    -- address / url 是“实际发出去的请求”。代理不可达、根本没发出去时它们是空串，
    -- 而不是填一个 127.0.0.1 假装发过——那会让一条 unknown 记录看起来像探测过。
    address     TEXT    NOT NULL DEFAULT '' CHECK (length(address) <= 64),
    url         TEXT    NOT NULL DEFAULT '' CHECK (length(url) <= 2048),
    error       TEXT    NOT NULL DEFAULT '' CHECK (length(error) <= 1000)
);

-- 调度器按 checked_at 找“该探的下一个”，但候选集合只有带健康检查的应用（通常个位数），
-- 在 Go 侧筛就够了，这里不额外建索引。
