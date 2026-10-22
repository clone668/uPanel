package agentstore

// 共享定义与受管理账号仓储的测试（P08.1，CONTRACTS.md 7、14.2）。
//
// 这一层只有八个方法，但它是“这台主机上到底有哪些共享、哪些受管理账号”的权威记录。
// 测试重点不在 CRUD 的形状，而在四条不变量：
//   1. 定义必须原样往返——smb_users 与 allowed_cidrs 决定渲染出来的 Samba 段；
//   2. 同名共享、同名账号不允许有两条记录（唯一索引是最后一道，不能只靠应用层）；
//   3. 共享只能绑在**存在**的受管理卷上（外键），删卷时会被挡住；
//   4. 不合法的记录写不进库，而且报的是能给人看的话，不是一句 SQL 错误。

import (
	"context"
	"strings"
	"testing"
	"time"

	"upanel/internal/shares"
)

func newTestShares(t *testing.T) (*ShareStore, *ResourceStore) {
	t.Helper()
	db := openTestDB(t)
	ss, err := NewShareStore(db)
	if err != nil {
		t.Fatalf("构造共享仓储失败: %v", err)
	}
	rs, err := NewResourceStore(db)
	if err != nil {
		t.Fatalf("构造受管资源仓储失败: %v", err)
	}
	return ss, rs
}

// seedVolume 先落一条受管理卷：共享有外键指向它，没有卷就存不进共享。
func seedVolume(t *testing.T, rs *ResourceStore, id, slug, uuid string) {
	t.Helper()
	if err := rs.SaveVolume(context.Background(), testVolume(id, slug, uuid)); err != nil {
		t.Fatalf("落测试卷失败: %v", err)
	}
}

func testShare(id, name, volumeID string) shares.ShareRecord {
	created := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	return shares.ShareRecord{
		ID: id,
		Definition: shares.ShareDefinition{
			Name:         name,
			Protocol:     shares.ProtocolSMB,
			VolumeID:     volumeID,
			RelativePath: "projects/team",
			Enabled:      true,
			ReadOnly:     false,
			AllowedCIDRs: []string{"10.0.0.0/8", "192.168.1.0/24"},
			SMBUsers: []shares.ShareUser{
				{AccountID: testAcctID1, Access: shares.AccessRead},
				{AccountID: testAcctID2, Access: shares.AccessWrite},
			},
		},
		ConfigHash:        strings.Repeat("a", 64),
		Version:           "1",
		DirectoryPrepared: true,
		ConfigApplied:     false,
		LastOperationID:   "op_test_1",
		CreatedAt:         created,
		UpdatedAt:         created,
	}
}

const (
	testAcctID1 = "sac_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAcctID2 = "sac_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testShareID = "shr_11111111111111111111111111111111"
	testShare2  = "shr_22222222222222222222222222222222"
	testShare3  = "shr_33333333333333333333333333333333"
)

func testAccount(id, name string) shares.AccountRecord {
	created := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	return shares.AccountRecord{
		ID:          id,
		Name:        name,
		Type:        shares.AccountTypeShare,
		Managed:     true,
		Disabled:    false,
		HasPassword: true,
		UID:         999,
		GID:         999,
		Shell:       shares.DefaultShell,
		Home:        shares.DefaultHome,
		SecretRef:   "sec_0123456789abcdef0123456789abcdef",
		Version:     "1",
		CreatedAt:   created,
		UpdatedAt:   created,
	}
}

func TestNewShareStoreRejectsNilDB(t *testing.T) {
	if _, err := NewShareStore(nil); err == nil {
		t.Fatal("没有数据库句柄却构造成功了")
	}
}

func TestSaveShareRoundTrip(t *testing.T) {
	ss, rs := newTestShares(t)
	seedVolume(t, rs, testVolID, "data", testFsUUID)

	rec := testShare(testShareID, "team-data", testVolID)
	if err := ss.SaveShare(context.Background(), rec); err != nil {
		t.Fatalf("保存共享失败: %v", err)
	}
	got, found, err := ss.GetShare(context.Background(), testShareID)
	if err != nil || !found {
		t.Fatalf("读取共享失败: found=%v err=%v", found, err)
	}
	if got.Definition.Name != "team-data" || got.Definition.VolumeID != testVolID {
		t.Fatalf("标量字段没往返: %+v", got.Definition)
	}
	// smb_users 与 allowed_cidrs 只能从 definition_json 里回来，必须逐项对上。
	if len(got.Definition.SMBUsers) != 2 ||
		got.Definition.SMBUsers[0].AccountID != testAcctID1 ||
		got.Definition.SMBUsers[1].Access != shares.AccessWrite {
		t.Fatalf("smb_users 没往返: %+v", got.Definition.SMBUsers)
	}
	if len(got.Definition.AllowedCIDRs) != 2 || got.Definition.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("allowed_cidrs 没往返: %+v", got.Definition.AllowedCIDRs)
	}
	if got.ConfigHash != rec.ConfigHash {
		t.Errorf("config_hash 是 %q", got.ConfigHash)
	}
	if !got.DirectoryPrepared {
		t.Error("directory_prepared 丢了")
	}
	if got.ConfigApplied {
		t.Error("config_applied 在 P08.1 必须是 false，读回来却成了 true")
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) || !got.UpdatedAt.Equal(rec.UpdatedAt) {
		t.Errorf("时间戳没往返：%s / %s", got.CreatedAt, got.UpdatedAt)
	}
}

func TestSaveShareByNameAndUpdate(t *testing.T) {
	ss, rs := newTestShares(t)
	seedVolume(t, rs, testVolID, "data", testFsUUID)

	rec := testShare(testShareID, "team-data", testVolID)
	if err := ss.SaveShare(context.Background(), rec); err != nil {
		t.Fatalf("保存共享失败: %v", err)
	}
	if _, found, err := ss.GetShareByName(context.Background(), "team-data"); err != nil || !found {
		t.Fatalf("按名字查不到: found=%v err=%v", found, err)
	}
	// 更新：版本自增、禁用。
	rec.Definition.Enabled = false
	rec.Version = "2"
	rec.DirectoryPrepared = false
	if err := ss.SaveShare(context.Background(), rec); err != nil {
		t.Fatalf("更新共享失败: %v", err)
	}
	got, _, err := ss.GetShare(context.Background(), testShareID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Version != "2" || got.Definition.Enabled || got.DirectoryPrepared {
		t.Fatalf("更新没落地: %+v", got)
	}
	all, err := ss.ListShares(context.Background())
	if err != nil || len(all) != 1 {
		t.Fatalf("列表里有 %d 条（err=%v）", len(all), err)
	}
}

func TestSaveShareRejectsDuplicateName(t *testing.T) {
	ss, rs := newTestShares(t)
	seedVolume(t, rs, testVolID, "data", testFsUUID)

	if err := ss.SaveShare(context.Background(), testShare(testShareID, "team-data", testVolID)); err != nil {
		t.Fatalf("保存第一条失败: %v", err)
	}
	err := ss.SaveShare(context.Background(), testShare(testShare2, "team-data", testVolID))
	if err == nil {
		t.Fatal("同名共享写进去了：两个 Samba 段同名会互相覆盖")
	}
	if !strings.Contains(err.Error(), "共享名") {
		t.Fatalf("报错没说清是共享名冲突：%v", err)
	}
}

func TestSaveShareRequiresExistingVolume(t *testing.T) {
	ss, _ := newTestShares(t)
	// 没有落任何卷：外键必须挡住。
	err := ss.SaveShare(context.Background(), testShare(testShareID, "team-data", testVolID))
	if err == nil {
		t.Fatal("卷不存在却把分享存进去了：共享会指向一个不存在的卷")
	}
	if !strings.Contains(err.Error(), "受管理卷") {
		t.Fatalf("报错没说清是卷的问题：%v", err)
	}
}

func TestDeleteVolumeBlockedByShare(t *testing.T) {
	ss, rs := newTestShares(t)
	seedVolume(t, rs, testVolID, "data", testFsUUID)
	if err := ss.SaveShare(context.Background(), testShare(testShareID, "team-data", testVolID)); err != nil {
		t.Fatalf("保存共享失败: %v", err)
	}
	// 还有共享引用这个卷时，删卷必须被数据库挡住——否则共享会指向一个不存在的卷。
	if err := rs.DeleteVolume(context.Background(), testVolID); err == nil {
		t.Fatal("共享还引用着这个卷，卷却被删掉了")
	}
	// 删掉共享之后才能删卷。
	if err := ss.DeleteShare(context.Background(), testShareID); err != nil {
		t.Fatalf("删除共享失败: %v", err)
	}
	if err := rs.DeleteVolume(context.Background(), testVolID); err != nil {
		t.Fatalf("共享删掉之后卷仍删不掉: %v", err)
	}
}

func TestDeleteShareIdempotent(t *testing.T) {
	ss, _ := newTestShares(t)
	if err := ss.DeleteShare(context.Background(), testShareID); err != nil {
		t.Fatalf("删除不存在的共享应该是幂等的: %v", err)
	}
}

func TestSaveShareValidatesShape(t *testing.T) {
	ss, rs := newTestShares(t)
	seedVolume(t, rs, testVolID, "data", testFsUUID)

	cases := map[string]func(*shares.ShareRecord){
		"非法 id":  func(r *shares.ShareRecord) { r.ID = "vol_x" },
		"空名字":    func(r *shares.ShareRecord) { r.Definition.Name = "" },
		"名字带斜杠":  func(r *shares.ShareRecord) { r.Definition.Name = "team/data" },
		"nfs 协议": func(r *shares.ShareRecord) { r.Definition.Protocol = shares.ProtocolNFS },
		"空卷":     func(r *shares.ShareRecord) { r.Definition.VolumeID = "" },
		"路径越界":   func(r *shares.ShareRecord) { r.Definition.RelativePath = "../etc" },
		"空用户名单":  func(r *shares.ShareRecord) { r.Definition.SMBUsers = nil },
		"空版本":    func(r *shares.ShareRecord) { r.Version = "" },
		"零时间":    func(r *shares.ShareRecord) { r.CreatedAt = time.Time{} },
	}
	for name, mutate := range cases {
		rec := testShare(testShare3, "shape-check", testVolID)
		mutate(&rec)
		if err := ss.SaveShare(context.Background(), rec); err == nil {
			t.Errorf("%s：不该写进去，却成功了", name)
		}
	}
}

func TestSaveAccountRoundTrip(t *testing.T) {
	ss, _ := newTestShares(t)
	rec := testAccount(testAcctID1, "alice")
	if err := ss.SaveAccount(context.Background(), rec); err != nil {
		t.Fatalf("保存账号失败: %v", err)
	}
	got, found, err := ss.GetAccount(context.Background(), testAcctID1)
	if err != nil || !found {
		t.Fatalf("读取账号失败: found=%v err=%v", found, err)
	}
	if got.Name != "alice" || !got.Managed || !got.HasPassword || got.Disabled {
		t.Fatalf("账号记录没往返: %+v", got)
	}
	if got.UID != 999 || got.Shell != shares.DefaultShell || got.Home != shares.DefaultHome {
		t.Fatalf("uid/shell/home 没往返: %+v", got)
	}
	if got.SecretRef != rec.SecretRef {
		t.Fatalf("secret_ref 是 %q，期望 %q", got.SecretRef, rec.SecretRef)
	}
	// 按名字查（“系统里已有同名账号”的判决靠它）。
	if _, found, err := ss.GetAccountByName(context.Background(), "alice"); err != nil || !found {
		t.Fatalf("按名字查不到: found=%v err=%v", found, err)
	}
}

func TestSaveAccountRejectsDuplicateNameAndBadShape(t *testing.T) {
	ss, _ := newTestShares(t)
	if err := ss.SaveAccount(context.Background(), testAccount(testAcctID1, "alice")); err != nil {
		t.Fatalf("保存第一条失败: %v", err)
	}
	if err := ss.SaveAccount(context.Background(), testAccount(testAcctID2, "alice")); err == nil {
		t.Fatal("同名账号写进去了：一个 Linux 账号只能有一条受管理记录")
	}

	cases := map[string]func(*shares.AccountRecord){
		"非法 id": func(r *shares.AccountRecord) { r.ID = "shr_x" },
		"大写名字":  func(r *shares.AccountRecord) { r.Name = "Alice" },
		"保留名":   func(r *shares.AccountRecord) { r.Name = "root" },
		"未知类型":  func(r *shares.AccountRecord) { r.Type = "guest" },
		"未受管理":  func(r *shares.AccountRecord) { r.Managed = false },
		"零 uid": func(r *shares.AccountRecord) { r.UID = 0 },
		"空版本":   func(r *shares.AccountRecord) { r.Version = "" },
		"零时间":   func(r *shares.AccountRecord) { r.CreatedAt = time.Time{} },
	}
	for name, mutate := range cases {
		rec := testAccount(testAcctID2, "bob")
		mutate(&rec)
		if err := ss.SaveAccount(context.Background(), rec); err == nil {
			t.Errorf("%s：不该写进去，却成功了", name)
		}
	}
}

func TestDeleteAccountIdempotent(t *testing.T) {
	ss, _ := newTestShares(t)
	if err := ss.DeleteAccount(context.Background(), testAcctID1); err != nil {
		t.Fatalf("删除不存在的账号应该是幂等的: %v", err)
	}
	rec := testAccount(testAcctID1, "alice")
	if err := ss.SaveAccount(context.Background(), rec); err != nil {
		t.Fatalf("保存账号失败: %v", err)
	}
	if err := ss.DeleteAccount(context.Background(), testAcctID1); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}
	if _, found, _ := ss.GetAccount(context.Background(), testAcctID1); found {
		t.Fatal("账号记录没删掉")
	}
}

func TestScanShareDetectsColumnDrift(t *testing.T) {
	ss, rs := newTestShares(t)
	seedVolume(t, rs, testVolID, "data", testFsUUID)
	if err := ss.SaveShare(context.Background(), testShare(testShareID, "team-data", testVolID)); err != nil {
		t.Fatalf("保存共享失败: %v", err)
	}
	// 模拟“这一行被本包之外的途径改过”：只改 name 列，不改 definition_json。
	if _, err := ss.db.store().ExecContext(context.Background(),
		`UPDATE shares SET name = 'renamed' WHERE id = ?`, testShareID); err != nil {
		t.Fatalf("改列失败: %v", err)
	}
	if _, _, err := ss.GetShare(context.Background(), testShareID); err == nil {
		t.Fatal("列与定义不一致却读成功了：这时无法判断哪个才是真的")
	} else if !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("报错没说清列与定义不一致：%v", err)
	}
}
