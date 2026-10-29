-- 0014：二次验证（P14.6；CONTRACTS.md §2 的 /auth/totp/*、§14.1 的 user_totp 行）。
--
-- 两张表，一张是"这个账号开了二次验证"，一张是"被烧掉的恢复码"。
--
-- 为什么种子不在这里：TOTP 种子的明文只存在代理的加密库里（agentstore 的 secrets 表，
-- 用途 totp_seed）。面板库只保存代理给的 secret_ref。这不是洁癖——面板库是 Web 进程
-- 读得到的，而 Web 进程不是 root；把种子放在这里等于让任何一个面板进程（包括一个被
-- 拿下的面板进程）都能算出用户的下一个验证码。契约 §14.1 的 user_totp 行因此写的是
-- "代理 secret_ref、enabled、恢复码哈希；无明文种子"。
--
-- 恢复码另起一张表而不是在 user_totp 上挂一列 JSON：恢复码是**一次性的**，用过的那一个
-- 必须能被单独标记。塞进一列就得整列重写，而"重写十个哈希"正是最容易写错的那类代码
-- （并发下一个还没被标记的码会被覆盖回去）。

CREATE TABLE user_totp (
    -- 一个账号最多一条。主键直接用 user_id：UNIQUE 与 PK 是同一件事，不必再建一个自增 id。
    user_id    INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- 代理侧那条种子的引用（sec_ 开头）。只读得到"存在哪一条"，解不出明文。
    secret_ref TEXT    NOT NULL CHECK (length(secret_ref) BETWEEN 1 AND 128),
    -- enabled 是这个账号的开关。写入时一律 1（确认通过才算启用，未确认的登记只活在
    -- 代理的临时秘密里，根本不会在这张表留下半条记录）；保留这一列是为了让"停用但
    -- 保留登记"将来有地方表达，而不是为了现在能写 0。
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    -- last_step 是最后一次被接受的时间步（30 秒一格）。重放防护就靠它：
    -- 同一个窗口里同一个码来第二次，step 不比它大，于是被拒。默认 -1 表示"还没有
    -- 任何一个步被用过"——用 0 会让时间步恰好为 0 的码（1970 年）被误判成重放。
    last_step  INTEGER NOT NULL DEFAULT -1,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);

CREATE TABLE user_totp_recovery_codes (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id  INTEGER NOT NULL REFERENCES user_totp (user_id) ON DELETE CASCADE,
    -- 只存哈希（sha256:<hex>）。恢复码本身与口令同级：写库等于把"绕过二次验证的钥匙"
    -- 抄了一份到磁盘上。
    code_hash TEXT   NOT NULL CHECK (length(code_hash) = 71 AND substr(code_hash, 1, 7) = 'sha256:'),
    -- used_at 非空 = 已经用掉了。用了就再也不认——这是一次性的定义，不是"记一笔统计"。
    used_at   TEXT,
    created_at TEXT  NOT NULL
);

-- 一张码只能登记一次（同账号内）。撞哈希在 32^10 的空间里不会发生，
-- 真有那么一天，那也不是"允许两行同哈希"，而是随机源出问题了。
CREATE UNIQUE INDEX idx_user_totp_recovery_codes_hash
    ON user_totp_recovery_codes (user_id, code_hash);
