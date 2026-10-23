-- 0010：证书（P10.3；CONTRACTS.md §9.3、PLAN.md 第 13 节）。
--
-- 这张表存的是“这张证书是谁、覆盖什么、存在哪儿”，**不存私钥内容**，也**不存
-- “现在是不是有效”这个结论**。私钥只有两条来路：Certbot 自己写在
-- /etc/letsencrypt（root 0700），或者手动导入时经 POST /secrets 存进代理的秘密
-- 存储（AEAD 加密、按用途 tls_private_key），再由代理物化成 0600 root 的文件。
-- 面板这一侧的库里永远只有路径与引用。
--
-- “有没有过期”“是不是临期”“文件还在不在”都是**读的时候算出来的**（Q286 的同一条
-- 原则：存下来的结论会过期，而过期了的结论比没有更糟）。not_after 存下来只是为了让
-- 列表页不必为每一行去问代理一次；详情页的 observed 块才是现场事实。
--
-- 列语义：
--   certificates.id             crt_ + 32 位小写十六进制
--   certificates.domain         这张证书**绑定的**域名（已规范化 A-label）。
--                                   一条记录一个域名：0.1 一个站点一个域名，多域名证书
--                                   可以导入，但只按它的主域名登记；覆盖范围存在
--                                   domains_json 里，导入时按它判断覆盖。
--   certificates.domains_json   证书实际覆盖的域名（SAN + CN，小写、去重、排序的 JSON）
--   certificates.source         来路闭集：certbot（ACME 签发）/ manual（手动导入）
--   certificates.staging        是否 ACME 测试环境签发。staging 证书浏览器不认，
--                                   界面上必须一眼可见——所以它是一个显式列，不靠
--                                   “看 issuer 猜”。
--   certificates.issuer / subject / not_before / not_after / fingerprint
--                               导入或签发时从证书本身解析出来的事实（fingerprint 是
--                                   整张 DER 的 SHA-256，用于判断盘上的文件还是不是
--                                   当初那一张）
--   certificates.cert_path      公钥证书在主机上的绝对路径（nginx ssl_certificate 的值）
--   certificates.key_path       私钥在主机上的绝对路径（nginx ssl_certificate_key 的值）。
--                                   **只有路径**：这张表里没有任何一列能装下私钥内容。
--   certificates.key_secret_ref 手动导入时私钥在秘密存储里的引用；certbot 签发时为空
--                                   （私钥由 certbot 自己保管，面板连读都不读）
--   certificates.acme_email     Certbot 账号邮箱（续期要同一个账号；为空表示手动导入）
--   certificates.last_attempt_at 上一次签发/续期尝试的时间。**存在的唯一理由是限流**：
--                                   同一域名一小时内不允许再次尝试正式签发（PLAN.md 第 13 节：
--                                   不要在循环重试中消耗正式签发限额）
--   certificates.last_error     上一次失败的原因（人可读）。成功后清空。
--   certificates.version        乐观锁，与 proxies/users 同一套
--
-- proxies.certificate_id 是“这个站点用哪张证书”。空表示该站点只监听 80；非空表示
-- 渲染出 443 的 server 块。**不按域名自动匹配**：证书与站点的绑定是一次明确的动作，
-- 自动匹配会让“我换了一张证书”和“我换了绑定”变成同一件事，出了问题分不清。
--
-- 这张表没有 applied_* 列，理由与 proxies 相同（Q286）。

CREATE TABLE certificates (
    id              TEXT    PRIMARY KEY,
    domain          TEXT    NOT NULL CHECK (length(domain) BETWEEN 1 AND 253),
    domains_json    TEXT    NOT NULL DEFAULT '[]' CHECK (length(domains_json) <= 4000),
    source          TEXT    NOT NULL CHECK (source IN ('certbot', 'manual')),
    staging         INTEGER NOT NULL DEFAULT 0 CHECK (staging IN (0, 1)),
    issuer          TEXT    NOT NULL CHECK (length(issuer) <= 253),
    subject         TEXT    NOT NULL DEFAULT '' CHECK (length(subject) <= 253),
    not_before      TEXT    NOT NULL,
    not_after       TEXT    NOT NULL,
    fingerprint     TEXT    NOT NULL CHECK (length(fingerprint) = 64),
    cert_path       TEXT    NOT NULL CHECK (length(cert_path) BETWEEN 1 AND 512),
    key_path        TEXT    NOT NULL CHECK (length(key_path) BETWEEN 1 AND 512),
    key_secret_ref  TEXT    NOT NULL DEFAULT '' CHECK (length(key_secret_ref) <= 64),
    acme_email      TEXT    NOT NULL DEFAULT '' CHECK (length(acme_email) <= 254),
    last_attempt_at TEXT    NOT NULL DEFAULT '',
    last_error      TEXT    NOT NULL DEFAULT '' CHECK (length(last_error) <= 4000),
    version         INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    -- 手动导入必须带私钥引用，certbot 签发必须带邮箱。两者都缺的记录只会在
    -- 代码写错时出现，让库把它挡住，比在续期那一刻才发现要好。
    CHECK (source <> 'manual' OR key_secret_ref <> ''),
    CHECK (source <> 'certbot' OR acme_email <> ''),
    -- 私钥路径不能落到证书路径上：那会让 nginx 把私钥当证书读，报错信息还指不到点子上。
    CHECK (cert_path <> key_path)
);

CREATE UNIQUE INDEX uq_certificates_domain ON certificates (domain);

-- 一个站点最多绑一张证书；一张证书可以给多个站点用（多 SAN 证书的合理用法）。
-- 站点侧不建唯一索引，证书侧也不建——真正的约束是“证书必须覆盖站点的域名”，
-- 那条规则在服务层判断，库层表达不出来。
ALTER TABLE proxies ADD COLUMN certificate_id TEXT REFERENCES certificates (id) ON DELETE RESTRICT;

CREATE INDEX idx_proxies_certificate ON proxies (certificate_id);
