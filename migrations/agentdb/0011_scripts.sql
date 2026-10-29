-- 代理数据库第 11 条迁移：受管理脚本（P12.4；CONTRACTS.md 第 5、12、14.2 节）。
--
-- 这张表回答的问题只有一条：**"此刻机器上那份脚本，是不是管理员批准过的那一份"**。
-- 所以内容与内容哈希都在这里，两者由同一次事务写入；落盘文件的字节永远从这里派生，
-- 运行前再核一次文件哈希（CONTRACTS §12：运行前核对已批准内容哈希，以避免终端用户
-- 修改已获授权的定时脚本）。
--
-- 为什么不把内容只写到文件里：文件会被别人的手改动，而"批准的是哪一份"不能跟着改。
-- 也不只存哈希：GET /scripts/:id 要把内容还给管理员（那是他自己的脚本），
-- 而且哈希对不上时要能重新铺一份正确的。
--
-- 列语义：
--   id             脚本标识 scr_<32 位小写十六进制>
--   name           显示名；UNIQUE
--   content        脚本正文（UTF-8，最多 64 KiB）；无秘密，是管理员自己上传的文本
--   content_hash   content 的 SHA-256 十六进制：运行前与磁盘上的字节比对
--   agent_script_ref 落盘路径（固定目录下的 <id>.sh）；不接收调用方给的路径
--   version        乐观并发令牌：script.run 的参数里带它
--   interpreter    固定解释器标识（当前只有 sh）；不接受任意可执行文件路径
--
-- 一行刻意写下的限制：content 里可以有任意 shell 语法，但它以 upanel-shell 身份运行，
-- 永远不获得 root（CONTRACTS §5 script.run 那一行）。真正的边界在降权执行那一步，
-- 不在这张表上——把"脚本内容看起来安全"当成边界才是危险的。

CREATE TABLE scripts (
    id               TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    content          TEXT    NOT NULL,
    content_hash     TEXT    NOT NULL,
    agent_script_ref TEXT    NOT NULL,
    interpreter      TEXT    NOT NULL DEFAULT 'sh' CHECK (interpreter IN ('sh')),
    version          INTEGER NOT NULL CHECK (version >= 1),
    created_at       INTEGER NOT NULL CHECK (created_at > 0),
    updated_at       INTEGER NOT NULL CHECK (updated_at >= created_at),

    CHECK (length(id) = 36),
    CHECK (substr(id, 1, 4) = 'scr_'),
    CHECK (length(name) BETWEEN 1 AND 80),
    CHECK (length(content) BETWEEN 1 AND 65536),
    CHECK (length(content_hash) = 64),
    CHECK (length(agent_script_ref) BETWEEN 1 AND 512)
);

CREATE UNIQUE INDEX ux_scripts_name ON scripts (name);
CREATE INDEX idx_scripts_updated ON scripts (updated_at DESC);
