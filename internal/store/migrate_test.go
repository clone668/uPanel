package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"testing/fstest"

	"upanel/migrations"
)

func TestMigrateAppliesRealMigrations(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	res, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if res.From != 0 {
		t.Fatalf("新库 From = %d，期望 0", res.From)
	}
	// 迁移条数随阶段增长：这里不写死数量，而是要求“从一开始连续升序”，
	// 且 To 等于最后一条的版本号——写死数量会让每次新增迁移都变成无意义的改测试。
	versions := res.AppliedVersions()
	if len(versions) == 0 {
		t.Fatalf("没有任何迁移被应用: From=%d To=%d", res.From, res.To)
	}
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("迁移版本不连续: %v", versions)
		}
	}
	if res.To != versions[len(versions)-1] {
		t.Fatalf("To = %d，期望等于最后一条迁移版本 %d", res.To, versions[len(versions)-1])
	}

	// 迁移文件里的多语句必须完整执行：表与索引都要存在。
	var name string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'settings'`).Scan(&name); err != nil {
		t.Fatalf("settings 表不存在: %v", err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_settings_updated_at'`).Scan(&name); err != nil {
		t.Fatalf("索引不存在（多语句迁移可能只执行了第一条）: %v", err)
	}
	// 0002 建立的账号与会话表同样必须真的存在（多语句迁移逐条生效）。
	for _, table := range []string{"users", "sessions"} {
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("%s 表不存在: %v", table, err)
		}
	}

	// 再跑一次：幂等，不重复应用。
	again, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
	if len(again.Applied) != 0 || again.From != res.To || again.To != res.To {
		t.Fatalf("重复迁移不应再应用: From=%d To=%d Applied=%v", again.From, again.To, again.AppliedVersions())
	}
}

func TestMigrateSequenceAndChecksum(t *testing.T) {
	ctx := context.Background()

	twoMigrations := fstest.MapFS{
		"0001_first.sql":  {Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_second.sql": {Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY); CREATE INDEX b_id ON b (id);`)},
	}

	db, _ := openTemp(t)
	res, err := db.Migrate(ctx, twoMigrations)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if res.To != 2 || len(res.Applied) != 2 {
		t.Fatalf("应依次应用两条迁移，实际 From=%d To=%d Applied=%v", res.From, res.To, res.AppliedVersions())
	}

	// 修改已应用迁移的内容 -> 必须报错（防止 schema 漂移）。
	changed := fstest.MapFS{
		"0001_first.sql":  {Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT)`)},
		"0002_second.sql": {Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY); CREATE INDEX b_id ON b (id);`)},
	}
	if _, err := db.Migrate(ctx, changed); err == nil {
		t.Fatalf("已应用迁移被修改时应报错")
	} else if !strings.Contains(err.Error(), "校验和") {
		t.Fatalf("错误信息应说明校验和冲突: %v", err)
	}

	// 删除已应用迁移 -> 必须报错。
	removed := fstest.MapFS{
		"0002_second.sql": {Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY); CREATE INDEX b_id ON b (id);`)},
	}
	if _, err := db.Migrate(ctx, removed); err == nil {
		t.Fatalf("已应用迁移被删除时应报错")
	}

	// 数据库版本高于程序 -> 必须报错（二进制降级）。
	db2, _ := openTemp(t)
	if _, err := db2.Migrate(ctx, twoMigrations); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	older := fstest.MapFS{
		"0001_first.sql": {Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
	}
	if _, err := db2.Migrate(ctx, older); err == nil {
		t.Fatalf("数据库版本高于程序时应报错")
	} else if !strings.Contains(err.Error(), "降级") {
		t.Fatalf("错误信息应提示降级风险: %v", err)
	}
}

func TestMigrateFailureStopsAndRollsBack(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	broken := fstest.MapFS{
		"0001_ok.sql": {Data: []byte(`CREATE TABLE ok (id INTEGER PRIMARY KEY)`)},
		"0002_broken.sql": {Data: []byte(`CREATE TABLE broken (id INTEGER PRIMARY KEY);
CREATE TABLE broken (id INTEGER PRIMARY KEY);`)},
	}

	if _, err := db.Migrate(ctx, broken); err == nil {
		t.Fatalf("坏迁移应返回错误")
	} else if !strings.Contains(err.Error(), "0002_broken") {
		t.Fatalf("错误信息应包含失败的迁移名: %v", err)
	}

	// 0001 已提交，0002 整条回滚：既不能留下 broken 表，也不能记录 0002。
	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("读取版本失败: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema 版本 = %d，期望 1（失败迁移不得记录）", version)
	}
	var count int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'broken'`).Scan(&count); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("失败迁移留下了表，说明事务没有回滚")
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	cases := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{"没有文件", fstest.MapFS{}, "没有找到"},
		{"文件名不合规", fstest.MapFS{"init.sql": {Data: []byte("SELECT 1")}}, "不合规"},
		{"序号重复", fstest.MapFS{
			"0001_a.sql": {Data: []byte("SELECT 1")},
			"0001_b.sql": {Data: []byte("SELECT 2")},
		}, "重复"},
		{"空文件", fstest.MapFS{"0001_a.sql": {Data: []byte("")}}, "为空"},
	}
	for _, c := range cases {
		if _, err := loadMigrations(c.fsys); err == nil {
			t.Fatalf("%s: 期望错误", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误信息 = %v，期望包含 %q", c.name, err, c.want)
		}
	}
}

func TestMigrationsReportsStatus(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	list, err := db.Migrations(ctx, migrations.FS)
	if err != nil {
		t.Fatalf("读取迁移列表失败: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("新库应列出待应用的迁移")
	}
	for _, m := range list {
		if m.Applied {
			t.Fatalf("新库不应有已应用的迁移: %+v", m)
		}
	}
	if _, err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	list, err = db.Migrations(ctx, migrations.FS)
	if err != nil {
		t.Fatalf("读取迁移列表失败: %v", err)
	}
	for _, m := range list {
		if !m.Applied || m.AppliedAt.IsZero() {
			t.Fatalf("迁移应标记为已应用并带时间: %+v", m)
		}
	}

	// settings 表可用（后续阶段的仓储会用到这张表）。
	var tx *sql.Tx
	if err := db.WithTx(ctx, func(ctx context.Context, gotx *sql.Tx) error {
		tx = gotx
		_, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value_json, created_at, updated_at) VALUES ('k', '"v"', '2026-10-20T00:00:00Z', '2026-10-20T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("写入 settings 失败: %v", err)
	}
}
