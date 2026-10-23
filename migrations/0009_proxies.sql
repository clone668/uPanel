-- 0009：反向代理站点（P10.1；CONTRACTS.md §9、PLAN.md 第 13 节）。
--
-- 这张表只描述“哪个域名转到哪个上游、谁能访问”，**不含任何 Nginx 状态**：
-- nginx 装没装、配置有没有冲突由代理现场读（internal/nginx），写配置文件与 reload
-- 是 P10.2 的事。把“我打算这样代理”和“Nginx 现在是这样”分开放，才不会出现
-- “库里说已启用，其实配置文件根本没写下去”这种记录。
--
-- 列语义：
--   proxies.id                  site_ + 32 位小写十六进制（与 app_/grp_/shr_ 同一套形状）
--   proxies.app_id              可空外键：这条代理指向哪个应用。NULL 表示还没关联
--   proxies.domain              **已规范化**的域名：小写 A-label（punycode）。
--                                   唯一索引直接建在它上面——同一个域名出现两条记录，
--                                   P10.2 就会写出两个 server 块，Nginx 只会用第一个。
--   proxies.upstream_target_id  上游目标，形状由 CHECK 收紧成 local_… / cntr_…
--                                   （与健康检查同一个闭集，见 apps.targetIDPattern）。
--                                   host/IP/URL 在这个形状里表达不出来，因此“把面板转到
--                                   169.254.169.254”不是被过滤掉，而是没有地方填。
--   proxies.access_mode         访问模式闭集：public / cidr / basic_auth。
--                                   0.1 一次只支持一种（PLAN.md 第 13 节），不做组合。
--   proxies.allowed_cidrs_json  access_mode=cidr 时的允许网段，规范 JSON 数组（有序、去重）
--   proxies.basic_auth_json     访问模式的账号列表，规范 JSON：[{"name":"…","secret_ref":"sec_…"}]
--                                   只有引用；密码哈希在 P10.2 写 htpasswd 时才生成。
--   proxies.websocket_enabled   是否放行 Upgrade/Connection 头与长连接超时
--   proxies.max_body_bytes      请求体上限（client_max_body_size）
--   proxies.read_timeout_seconds 读超时（proxy_read_timeout）
--   proxies.enabled             用户的意图，不是事实：P10.1 一个字都没写进 /etc/nginx，
--                                   所以它只表示“我希望这个站点生效”。真实生效状态要等 P10.2
--                                   写出配置并通过 nginx -t 才有东西可记（届时加 applied_* 列），
--                                   在那之前**不能**把 enabled=1 读成“已经生效”。
--   proxies.version             乐观锁；PATCH 用 WHERE id=? AND version=?，零行即 409

CREATE TABLE proxies (
    id                     TEXT    PRIMARY KEY,
    app_id                 TEXT    REFERENCES apps (id) ON DELETE RESTRICT,
    domain                 TEXT    NOT NULL CHECK (length(domain) BETWEEN 1 AND 253),
    upstream_target_id     TEXT    NOT NULL CHECK (length(upstream_target_id) BETWEEN 7 AND 64),
    upstream_scheme        TEXT    NOT NULL CHECK (upstream_scheme IN ('http', 'https')),
    upstream_port          INTEGER NOT NULL CHECK (upstream_port BETWEEN 1 AND 65535),
    access_mode            TEXT    NOT NULL CHECK (access_mode IN ('public', 'cidr', 'basic_auth')),
    allowed_cidrs_json     TEXT    NOT NULL DEFAULT '[]' CHECK (length(allowed_cidrs_json) <= 4000),
    basic_auth_json        TEXT    NOT NULL DEFAULT '[]' CHECK (length(basic_auth_json) <= 4000),
    websocket_enabled      INTEGER NOT NULL DEFAULT 0 CHECK (websocket_enabled IN (0, 1)),
    max_body_bytes         INTEGER NOT NULL CHECK (max_body_bytes BETWEEN 1024 AND 1073741824),
    read_timeout_seconds   INTEGER NOT NULL CHECK (read_timeout_seconds BETWEEN 1 AND 3600),
    enabled                INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    version                INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at             TEXT    NOT NULL,
    updated_at             TEXT    NOT NULL,
    -- 上游目标的形状在库层也守一道：apps 包的目标校验与这里必须是同一个闭集。
    -- 改动这里之前先看 internal/proxies/validate.go 的同名规则，两处分叉比一处缺失更难查。
    CHECK (substr(upstream_target_id, 1, 6) = 'local_' OR substr(upstream_target_id, 1, 5) = 'cntr_'),
    -- cidr 模式必须有网段，basic_auth 模式必须有账号；另外两种模式下这两个字段必须是空数组。
    -- “模式说有名单、名单却是空的”会让 P10.2 写出一个谁都能进或谁都进不去的站点，
    -- 而那种错误在页面上看不出来。
    CHECK (access_mode <> 'cidr'       OR allowed_cidrs_json <> '[]'),
    CHECK (access_mode <> 'basic_auth' OR basic_auth_json    <> '[]'),
    CHECK (access_mode =  'cidr'       OR allowed_cidrs_json =  '[]'),
    CHECK (access_mode =  'basic_auth' OR basic_auth_json    =  '[]')
);

-- 一个域名只能属于一条代理记录。用唯一索引而不是“先查后写”：
-- 并发两次提交在库里撞一下就是 409，检查与写入不可能是两个中间还能插队的步骤。
CREATE UNIQUE INDEX uq_proxies_domain ON proxies (domain);

CREATE INDEX idx_proxies_app ON proxies (app_id);
CREATE INDEX idx_proxies_domain_id ON proxies (domain, id);
