package agentstore

// 受管理卷仓储的测试（P07.4，CONTRACTS.md 14.2）。
//
// 这一层只有四个方法，但它是“主机上到底接管了什么”的权威记录，所以测试重点不在
// CRUD 的形状，而在三条不变量：
//   1. 配置（config_json）必须原样往返——它决定 fstab 那一行写成什么；
//   2. 同一 UUID / 同一卷名不允许有两条记录（唯一索引是最后一道，不能只靠应用层）；
//   3. 不合法的记录写不进库，而且报的是能给人看的话，不是一句 SQL 错误。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/storage"
)

const (
	testVolID   = "vol_0123456789abcdef0123456789abcdef"
	testVolID2  = "vol_fedcba9876543210fedcba9876543210"
	testFsUUID  = "5f1c9a3e-2b64-4c7d-8e10-9a3b5c7d1e2f"
	testFsUUIDB = "0a9e6b71-3c8d-4f52-b1a4-7d6e2c8f9b03"
)

func newTestResources(t *testing.T) *ResourceStore {
	t.Helper()
	rs, err := NewResourceStore(openTestDB(t))
	if err != nil {
		t.Fatalf("构造受管资源仓储失败: %v", err)
	}
	return rs
}

// testVolume 造一条形状合法的记录。
func testVolume(id, slug, uuid string) storage.VolumeRecord {
	created := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	return storage.VolumeRecord{
		ID:                  id,
		DisplayName:         "数据卷 " + slug,
		Slug:                slug,
		DeviceID:            "sdb1",
		FilesystemUUID:      uuid,
		FilesystemType:      storage.FilesystemExt4,
		IdentityFingerprint: "sha256:disk-sdb",
		Config:              storage.DefaultVolumeConfig(),
		DesiredState:        storage.DesiredUnmounted,
		Version:             "1",
		LastOperationID:     "op_test_1",
		CreatedAt:           created,
		UpdatedAt:           created,
	}
}

func TestNewResourceStoreRejectsNilDB(t *testing.T) {
	if _, err := NewResourceStore(nil); err == nil {
		t.Fatal("没有数据库句柄却构造成功了")
	}
}

// TestResourceSaveGetRoundTrip 钉住“存的什么就是取的什么”，包括 config_json。
func TestResourceSaveGetRoundTrip(t *testing.T) {
	rs := newTestResources(t)
	ctx := context.Background()

	rec := testVolume(testVolID, "data", testFsUUID)
	// 非默认配置：只有它能证明 config_json 真的往返了（零值配置全对不上也看不出来）。
	rec.Config = storage.VolumeConfig{Automount: true, ReadOnly: true, Nodev: false, Nosuid: true}
	rec.DesiredState = storage.DesiredMounted
	rec.Version = "7"
	if err := rs.SaveVolume(ctx, rec); err != nil {
		t.Fatalf("保存卷失败: %v", err)
	}

	got, found, err := rs.GetVolume(ctx, testVolID)
	if err != nil {
		t.Fatalf("读取卷失败: %v", err)
	}
	if !found {
		t.Fatal("刚写进去的卷读不到")
	}
	if got.Config != rec.Config {
		t.Errorf("config 往返之后变了：%+v，想要 %+v", got.Config, rec.Config)
	}
	if got.Slug != rec.Slug || got.FilesystemUUID != rec.FilesystemUUID ||
		got.FilesystemType != rec.FilesystemType || got.IdentityFingerprint != rec.IdentityFingerprint {
		t.Errorf("记录往返之后变了：%+v", got)
	}
	if got.DesiredState != storage.DesiredMounted || got.Version != "7" ||
		got.LastOperationID != "op_test_1" {
		t.Errorf("状态字段往返之后变了：%+v", got)
	}
	// 时间是 Unix 毫秒：秒级精度必须活下来，毫秒以下的抖动不可能(记录本来就用毫秒)。
	if !got.CreatedAt.Equal(rec.CreatedAt) || !got.UpdatedAt.Equal(rec.UpdatedAt) {
		t.Errorf("时间往返之后变了：%v / %v，想要 %v / %v",
			got.CreatedAt, got.UpdatedAt, rec.CreatedAt, rec.UpdatedAt)
	}
}

// TestResourceGetMissingIsNotAnError 钉住“不存在”与“读失败”是两件事：
// 前者要变成一条 VOLUME_NOT_FOUND 阻塞项，后者才是 500。
func TestResourceGetMissingIsNotAnError(t *testing.T) {
	rs := newTestResources(t)
	got, found, err := rs.GetVolume(context.Background(), testVolID)
	if err != nil {
		t.Fatalf("读不存在的卷返回了错误: %v", err)
	}
	if found {
		t.Error("不存在的卷却报 found")
	}
	if got.ID != "" {
		t.Errorf("不存在的卷返回了非零值: %+v", got)
	}
}

// TestResourceSaveIsUpsert 钉住“同一 id 是覆盖，不是插入第二条”。
func TestResourceSaveIsUpsert(t *testing.T) {
	rs := newTestResources(t)
	ctx := context.Background()

	rec := testVolume(testVolID, "data", testFsUUID)
	if err := rs.SaveVolume(ctx, rec); err != nil {
		t.Fatalf("保存卷失败: %v", err)
	}
	rec.Version = "2"
	rec.DesiredState = storage.DesiredMounted
	rec.DisplayName = "改过的名字"
	rec.UpdatedAt = rec.CreatedAt.Add(time.Minute)
	if err := rs.SaveVolume(ctx, rec); err != nil {
		t.Fatalf("覆盖保存失败: %v", err)
	}

	all, err := rs.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("列出卷失败: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("记录数 = %d，想要 1（UPSERT 不该产生第二条）：%+v", len(all), all)
	}
	if all[0].Version != "2" || all[0].DisplayName != "改过的名字" {
		t.Errorf("覆盖没有生效：%+v", all[0])
	}
}

// TestResourceRejectsDuplicateSlugAndUUID 钉住两条唯一约束。
//
// 它们不是“防御性冗余”：两个卷指向同一个文件系统或同一个挂载点时，fstab 受管块里
// 会出现两行指向同一处，systemd 用哪一行不由我们决定。这种状态必须写不进数据库。
func TestResourceRejectsDuplicateSlugAndUUID(t *testing.T) {
	rs := newTestResources(t)
	ctx := context.Background()
	if err := rs.SaveVolume(ctx, testVolume(testVolID, "data", testFsUUID)); err != nil {
		t.Fatalf("保存第一条失败: %v", err)
	}

	cases := []struct {
		name string
		rec  storage.VolumeRecord
		want string
	}{
		{"同卷名不同 UUID", testVolume(testVolID2, "data", testFsUUIDB), "data"},
		{"同 UUID 不同卷名", testVolume(testVolID2, "backup", testFsUUID), testFsUUID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := rs.SaveVolume(ctx, c.rec)
			if err == nil {
				t.Fatal("重复记录写进去了")
			}
			if !strings.Contains(err.Error(), "冲突") {
				t.Errorf("错误消息没说清是冲突：%v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误消息里没有定位信息 %q：%v", c.want, err)
			}
		})
	}
	// 失败的两次都不该留下记录。
	all, err := rs.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("列出卷失败: %v", err)
	}
	if len(all) != 1 || all[0].ID != testVolID {
		t.Errorf("被拒绝的写入留下了记录：%+v", all)
	}
}

// TestResourceDeleteIsIdempotent 钉住 DELETE 的幂等：重复删不是错误。
func TestResourceDeleteIsIdempotent(t *testing.T) {
	rs := newTestResources(t)
	ctx := context.Background()
	if err := rs.SaveVolume(ctx, testVolume(testVolID, "data", testFsUUID)); err != nil {
		t.Fatalf("保存卷失败: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := rs.DeleteVolume(ctx, testVolID); err != nil {
			t.Fatalf("第 %d 次删除失败: %v", i+1, err)
		}
	}
	if _, found, err := rs.GetVolume(ctx, testVolID); err != nil || found {
		t.Errorf("删除之后还能读到：found=%v err=%v", found, err)
	}
	// 删一个从没存在过的卷也不报错（重试同一份任务时会走到这里）。
	if err := rs.DeleteVolume(ctx, testVolID2); err != nil {
		t.Errorf("删除不存在的卷报错了: %v", err)
	}
}

// TestResourceListReturnsAllSorted 只保证“都在”，顺序交给调用方（判决层排序）。
func TestResourceListReturnsAllSorted(t *testing.T) {
	rs := newTestResources(t)
	ctx := context.Background()
	if err := rs.SaveVolume(ctx, testVolume(testVolID, "data", testFsUUID)); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := rs.SaveVolume(ctx, testVolume(testVolID2, "backup", testFsUUIDB)); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	all, err := rs.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("列出失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("记录数 = %d，想要 2", len(all))
	}
	seen := map[string]bool{}
	for _, r := range all {
		seen[r.ID] = true
	}
	if !seen[testVolID] || !seen[testVolID2] {
		t.Errorf("列出的记录不全：%+v", all)
	}
}

// TestResourceRejectsInvalidRecords 钉住“落库之前先检查形状”。
//
// 与数据库 CHECK 约束重复是有意的：数据库报的是 SQL 错误，这里的错话能直接展示。
func TestResourceRejectsInvalidRecords(t *testing.T) {
	rs := newTestResources(t)
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(*storage.VolumeRecord)
		want string
	}{
		{"卷标识形状不对", func(r *storage.VolumeRecord) { r.ID = "vol_NOTHEX" }, "卷标识"},
		{"卷名带大写", func(r *storage.VolumeRecord) { r.Slug = "Data" }, "卷名"},
		{"卷名以中划线开头", func(r *storage.VolumeRecord) { r.Slug = "-data" }, "中划线"},
		{"显示名超长", func(r *storage.VolumeRecord) {
			r.DisplayName = strings.Repeat("长", 200)
		}, "显示名"},
		{"UUID 长度不对", func(r *storage.VolumeRecord) { r.FilesystemUUID = "5f1c9a3e" }, "UUID"},
		{"文件系统不在支持范围", func(r *storage.VolumeRecord) { r.FilesystemType = "ntfs" }, "ext4/xfs"},
		{"desired_state 不合法", func(r *storage.VolumeRecord) { r.DesiredState = "mounted-somewhere" }, "desired_state"},
		{"version 为空", func(r *storage.VolumeRecord) { r.Version = "" }, "version"},
		{"没有创建时间", func(r *storage.VolumeRecord) { r.CreatedAt = time.Time{} }, "created_at"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := testVolume(testVolID, "data", testFsUUID)
			c.mut(&rec)
			err := rs.SaveVolume(ctx, rec)
			if err == nil {
				t.Fatal("不合法的记录写进去了")
			}
			if !errors.Is(err, ErrResourceInvalid) {
				t.Errorf("错误类型不对：%v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误消息里没有 %q：%v", c.want, err)
			}
		})
	}
	if all, err := rs.ListVolumes(ctx); err != nil || len(all) != 0 {
		t.Errorf("被拒绝的写入留下了记录：%+v err=%v", all, err)
	}
}
