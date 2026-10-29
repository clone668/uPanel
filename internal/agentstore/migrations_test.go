package agentstore

// 迁移升级路径的测试（P14.4 修正）。
//
// 为什么单独写：其它仓储测试都是"建一个全新的库"（迁移 1..N 跑在空表上），
// 而 0013 是一次**表重建**——建新表、拷数据、删旧表、改名、重建索引。空表上跑它
// 什么都证明不了：真正会坏的是"库里已经有行"的那条路（拷楼拷丢列、索引忘了重建、
// 旧 CHECK 太松让坏值溜过去）。
//
// 22.04 验收 VM 上抓到的正是 0013 缺失时的现场：`constraint failed: CHECK constraint
// failed: protocol IN ('smb')`。所以这里用**真实的迁移文件**（从嵌入的 agentdb.FS 里
// 读出 0001—0012，字节完全相同、校验和因此对得上）先把库升到 12，在旧 schema 上落一条
// smb 共享、并确认 nfs 在旧 schema 上确实写不进去，再跑完整迁移，逐项核对：
//   * 版本从 12 到 13；
//   * 旧行一字不差地活下来（含 definition_json 里的字段）；
//   * nfs 现在能写了；
//   * 两个索引被重建（索引随旧表一起被删掉，忘了重建不会报错，只会慢慢变慢）。

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"upanel/internal/accounts"
	"upanel/internal/shares"
	"upanel/migrations/agentdb"
)

// 0014 用例用的三个标识（形状必须与真实的一致：4 字符前缀 + 32 位小写十六进制）。
const (
	testAccountID  = "sac_22222222222222222222222222222222"
	testSSHKeyID   = "ssk_33333333333333333333333333333333"
	testSudoRuleID = "sru_44444444444444444444444444444444"
)

// migrationsUpTo 返回只含 0001—00<max> 的迁移文件集合（内容与嵌入文件逐字节相同）。
func migrationsUpTo(t *testing.T, max int) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(agentdb.FS, ".")
	if err != nil {
		t.Fatalf("读迁移目录失败: %v", err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(name, "%04d_", &v); err != nil || v > max {
			continue
		}
		data, err := fs.ReadFile(agentdb.FS, name)
		if err != nil {
			t.Fatalf("读迁移 %s 失败: %v", name, err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

// nfsDefinitionForTest 是一条最小可存的 NFS 定义（路径、网段、两个固定选项）。
func nfsDefinitionForTest(volumeID string) shares.ShareDefinition {
	return shares.ShareDefinition{
		Name:         "media",
		Protocol:     shares.ProtocolNFS,
		VolumeID:     volumeID,
		RelativePath: "",
		Enabled:      true,
		ReadOnly:     false,
		AllowedCIDRs: []string{"10.0.2.0/24"},
		NFSOptions:   &shares.NFSOptions{RootSquash: true, Sync: true},
	}
}

func TestMigration0013RebuildsSharesAndKeepsRows(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "agent.db"), Logger: testLogger()})
	if err != nil {
		t.Fatalf("打开代理数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// ---- 先升到 12：这是 0013 出现之前现场里真实的库 ----
	old, err := db.Migrate(ctx, migrationsUpTo(t, 12))
	if err != nil {
		t.Fatalf("迁移到 12 失败: %v", err)
	}
	if old.To != 12 {
		t.Fatalf("迁移到 12 之后版本是 %d", old.To)
	}

	rs, err := NewResourceStore(db)
	if err != nil {
		t.Fatalf("构造受管资源仓储失败: %v", err)
	}
	seedVolume(t, rs, testVolID, "data", testFsUUID)
	ss, err := NewShareStore(db)
	if err != nil {
		t.Fatalf("构造共享仓储失败: %v", err)
	}

	smb := testShare(testShareID, "team-data", testVolID)
	if err := ss.SaveShare(ctx, smb); err != nil {
		t.Fatalf("旧 schema 上保存 smb 共享失败: %v", err)
	}

	// 旧 schema 必须真的拒绝 nfs：否则这条用例证明不了 0013 有任何用处。
	nfsRec := testShare(testShare2, "media", testVolID)
	nfsRec.Definition = nfsDefinitionForTest(testVolID)
	if err := ss.SaveShare(ctx, nfsRec); err == nil {
		t.Fatal("旧 schema（protocol IN ('smb')）竟然接受了 nfs 共享")
	}

	// ---- 再跑完整迁移（含 0013） ----
	res, err := db.Migrate(ctx, agentdb.FS)
	if err != nil {
		t.Fatalf("迁移到最新失败: %v", err)
	}
	// 断言的是“从 12 一路升到嵌入的最新版本”，不是写死的 13：这条用例验的是
	// 0013 的表重建，后面每加一条迁移都会把这个数字顶上去，但“旧行活下来、
	// nfs 能写了”这两件事与终点是哪一版无关。
	if res.From != 12 || res.To != latestMigrationVersion(t) {
		t.Fatalf("迁移是 %d → %d，期望 12 → %d", res.From, res.To, latestMigrationVersion(t))
	}

	// 旧行必须一字不差地活下来。
	got, found, err := ss.GetShare(ctx, testShareID)
	if err != nil || !found {
		t.Fatalf("重建之后旧共享不见了: found=%v err=%v", found, err)
	}
	if got.Definition.Name != "team-data" || got.Definition.Protocol != "smb" ||
		len(got.Definition.SMBUsers) != 2 || got.ConfigHash != smb.ConfigHash {
		t.Fatalf("重建之后旧共享被改动了: %+v", got)
	}

	// nfs 现在写得进去，也读得回来。
	if err := ss.SaveShare(ctx, nfsRec); err != nil {
		t.Fatalf("重建之后仍然存不进 nfs 共享: %v", err)
	}
	if back, found, err := ss.GetShare(ctx, testShare2); err != nil || !found {
		t.Fatalf("nfs 共享存进去了却读不回来: found=%v err=%v", found, err)
	} else if back.Definition.Protocol != "nfs" || back.Definition.NFSOptions == nil {
		t.Fatalf("nfs 共享读回来不对: %+v", back.Definition)
	}

	// 索引必须被重建：DROP TABLE 会连索引一起删掉，忘了重建不会报错。
	for _, idx := range []string{"idx_shares_volume", "idx_shares_name"} {
		var name string
		err := db.store().SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&name)
		if err != nil {
			t.Fatalf("索引 %s 没被重建: %v", idx, err)
		}
	}
}

// latestMigrationVersion 返回嵌入迁移里的最大版本号。
func latestMigrationVersion(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(agentdb.FS, ".")
	if err != nil {
		t.Fatalf("读迁移目录失败: %v", err)
	}
	max := 0
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err == nil && v > max {
			max = v
		}
	}
	if max == 0 {
		t.Fatal("迁移目录里一条迁移都没读到")
	}
	return max
}

// TestMigration0014RebuildsAccountsAndAddsKeyTables 验 0014 的三件事：
//
//  1. 老账号行活下来，且两列按规则回填（display_name := name、shell_profile := 'disabled'）；
//  2. 新的 CHECK 真的成立：share 类型 + restricted 被库拒绝（绕过接口也造不出能登录的共享账号）；
//  3. managed_ssh_keys / managed_sudo_rules 存在，唯一索引与级联删除都在。
//
// 与 0013 的用例同一个骨架：先升到 13，在旧 schema 上写下现场，再跑完整迁移。
func TestMigration0014RebuildsAccountsAndAddsKeyTables(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "agent.db"), Logger: testLogger()})
	if err != nil {
		t.Fatalf("打开代理数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// ---- 先升到 13：这是 0014 出现之前现场里真实的库 ----
	if old, err := db.Migrate(ctx, migrationsUpTo(t, 13)); err != nil {
		t.Fatalf("迁移到 13 失败: %v", err)
	} else if old.To != 13 {
		t.Fatalf("迁移到 13 之后版本是 %d", old.To)
	}
	ss, err := NewShareStore(db)
	if err != nil {
		t.Fatalf("构造共享仓储失败: %v", err)
	}
	// 这里用裸 SQL 而不是 ss.SaveAccount：现在的 SaveAccount 会写 display_name /
	// shell_profile 两列，而 13 版的表里没有它们——这正是“0014 之前的那份代码”
	// 与“0014 之后的这份代码”的差别。要在旧 schema 上落下现场，就得按旧列写。
	seeded := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	if _, err := db.store().ExecContext(ctx, `
		INSERT INTO managed_accounts (
			id, name, type, managed, disabled, has_password, uid, gid,
			shell, home, secret_ref, version, last_operation_id, created_at, updated_at
		) VALUES (?, 'legacy', 'share', 1, 0, 1, 997, 987,
			'/usr/sbin/nologin', '/nonexistent', 'sec_0123456789abcdef0123456789abcdef',
			'1', '', ?, ?)`,
		testAccountID, millis(seeded), millis(seeded)); err != nil {
		t.Fatalf("旧 schema 上写入账号失败: %v", err)
	}

	// ---- 再跑完整迁移（含 0014） ----
	res, err := db.Migrate(ctx, agentdb.FS)
	if err != nil {
		t.Fatalf("迁移到最新失败: %v", err)
	}
	if res.From != 13 || res.To != latestMigrationVersion(t) {
		t.Fatalf("迁移是 %d → %d，期望 13 → %d", res.From, res.To, latestMigrationVersion(t))
	}

	// 1. 旧行活下来，两列按规则回填。
	got, found, err := ss.GetAccount(ctx, testAccountID)
	if err != nil || !found {
		t.Fatalf("重建之后旧账号不见了: found=%v err=%v", found, err)
	}
	if got.Name != "legacy" || got.UID != 997 || got.HasPassword != true {
		t.Fatalf("重建之后旧账号被改动了: %+v", got)
	}
	if got.DisplayName != "legacy" {
		t.Fatalf("display_name 应回填成账号名，实际 %q", got.DisplayName)
	}
	if got.ShellProfile != "disabled" {
		t.Fatalf("shell_profile 应回填成 disabled，实际 %q", got.ShellProfile)
	}

	// 2. 新 CHECK：share + restricted 必须被库拒绝。这条走裸 SQL，绕过所有应用层校验。
	if _, err := db.store().ExecContext(ctx,
		`UPDATE managed_accounts SET shell_profile = 'restricted' WHERE id = ?`,
		testAccountID); err == nil {
		t.Fatal("数据库竟然让共享账号用上了 restricted 登录档")
	}

	// 3. 两张新表与它们的索引。
	for _, idx := range []string{
		"ux_managed_ssh_keys_fingerprint", "idx_managed_ssh_keys_account",
	} {
		var name string
		err := db.store().SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&name)
		if err != nil {
			t.Fatalf("索引 %s 不存在: %v", idx, err)
		}
	}
	for _, tbl := range []string{"managed_ssh_keys", "managed_sudo_rules"} {
		var name string
		err := db.store().SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, tbl).Scan(&name)
		if err != nil {
			t.Fatalf("表 %s 不存在: %v", tbl, err)
		}
	}

	// 级联删除：账号没了，公钥与 sudo 规则一起走（外键必须真的开着）。
	key := accounts.SSHKeyRecord{
		ID: testSSHKeyID, AccountID: testAccountID, Type: "ssh-ed25519",
		Blob:        "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		Fingerprint: "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU", Comment: "legacy@vm",
		CreatedAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	}
	if err := ss.SaveSSHKey(ctx, key); err != nil {
		t.Fatalf("保存公钥失败: %v", err)
	}
	if err := ss.SaveSudoRule(ctx, accounts.SudoRuleRecord{
		ID: testSudoRuleID, AccountID: testAccountID, Template: "system_status",
		Version:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CreatedAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("保存 sudo 规则失败: %v", err)
	}
	if err := ss.DeleteAccount(ctx, testAccountID); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}
	if keys, err := ss.ListSSHKeys(ctx, testAccountID); err != nil {
		t.Fatalf("读公钥失败: %v", err)
	} else if len(keys) != 0 {
		t.Fatalf("账号删掉之后公钥还在 %d 条（级联没生效）", len(keys))
	}
	if _, found, err := ss.GetSudoRule(ctx, testAccountID); err != nil {
		t.Fatalf("读 sudo 规则失败: %v", err)
	} else if found {
		t.Fatal("账号删掉之后 sudo 规则还在（级联没生效）")
	}
}
