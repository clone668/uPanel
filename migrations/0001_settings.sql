-- 0001：初始 schema。
--
-- P01.2 只建立 Web 数据库的第一张真实表（settings）。其余业务表按 STAGES.md 在各阶段
-- 通过新的迁移追加，禁止修改已应用的迁移文件（store 会校验已应用迁移的校验和）。
--
-- 约定：mutable 元数据表包含 version / created_at / updated_at；时间为 RFC3339 UTC 文本。

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_settings_updated_at ON settings (updated_at);
