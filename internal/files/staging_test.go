//go:build linux

package files

// P06.2 流式暂存上传的真实用例（只在 Linux 上跑）。
//
// 这一层要钉住的是“上传 ≠ 落地”这句话：在 commit_upload 之前，目标路径必须
// 一个字节都没变；而提交之后，读回来的必须就是那份字节。
//
//		建暂存 → 送字节（可以失败）→ 目标不动
//		提交 → 原子落位 → 读回核对
//
// 失败路径同样重要：声明大小与实收不符、超限、过期、重复写，都必须把这次上传
// 变成“作废”，而不是留一份半截数据让别人以为传完了。
//
// Windows 宿主上不参与编译；结论由独立 Ubuntu 虚拟机给出（见
// docs/evidence/P06/<日期>-p06.2/）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustCreateUpload(t *testing.T, m *Manager, params CreateUploadParams) UploadInfo {
	t.Helper()
	info, err := m.CreateUpload(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateUpload 失败: %v", err)
	}
	return info
}

// TestUploadLifecycleHappyPath 完整的两步：字节进暂存时目标不动，提交之后才落位。
func TestUploadLifecycleHappyPath(t *testing.T) {
	m, dir := newWritableManager(t, nil)
	ctx := context.Background()

	info := mustCreateUpload(t, m, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 5,
	})
	if info.State != UploadStateOpen || info.ReceivedBytes != 0 {
		t.Fatalf("新建暂存的状态不对: %+v", info)
	}
	if !info.ExpiresAt.After(info.CreatedAt) {
		t.Fatalf("到期时间不在创建之后: %+v", info)
	}
	// 声明时就预留：并发上传之间共享同一份可用空间判断。
	if got := m.ReservedBytes("etc"); got != 5 {
		t.Fatalf("声明之后预留应当是 5 字节，得到 %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("建暂存阶段目标就该不存在: %v", err)
	}

	received, err := m.WriteUploadContent(ctx, info.UploadID, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("WriteUploadContent 失败: %v", err)
	}
	if received.State != UploadStateReceived || received.ReceivedBytes != 5 {
		t.Fatalf("收完之后的状态不对: %+v", received)
	}
	// 字节已经真实占着磁盘，预留应当释放。
	if got := m.ReservedBytes("etc"); got != 0 {
		t.Fatalf("收完之后预留应当清零，得到 %d", got)
	}
	// 关键断言：内容收完了，目标仍然不存在。
	if _, err := os.Stat(filepath.Join(dir, "app.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("送完字节之后目标就出现了（上传不该落地）: %v", err)
	}
	if again, err := m.GetUpload(ctx, info.UploadID); err != nil || again.State != UploadStateReceived {
		t.Fatalf("GetUpload 与写入结果不一致: %+v %v", again, err)
	}

	result, err := m.CommitUpload(ctx, CommitUploadParams{UploadID: info.UploadID})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if result.Version == "" || result.SizeBytes != 5 {
		t.Fatalf("提交的读回结果不完整: %+v", result)
	}
	if got := mustFileContent(t, filepath.Join(dir, "app.conf")); got != "hello" {
		t.Fatalf("落地内容不对: %q", got)
	}
	// 提交之后这次上传就结束了：暂存数据与记录都不该还在。
	if _, err := m.GetUpload(ctx, info.UploadID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("提交之后暂存应当消失，得到 %v", err)
	}
	if left := stagingEntries(t, dir); len(left) != 0 {
		t.Fatalf("提交之后暂存目录还有残留: %v", left)
	}
}

// TestUploadShortContentFails 实收少于声明：这次上传作废，目标不动。
func TestUploadShortContentFails(t *testing.T) {
	m, dir := newWritableManager(t, nil)
	ctx := context.Background()

	info := mustCreateUpload(t, m, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 10,
	})
	if _, err := m.WriteUploadContent(ctx, info.UploadID, strings.NewReader("abc")); !errors.Is(err, ErrUploadState) {
		t.Fatalf("字节不够应当报 ErrUploadState，得到 %v", err)
	}
	failed, err := m.GetUpload(ctx, info.UploadID)
	if err != nil {
		t.Fatalf("GetUpload 失败: %v", err)
	}
	if failed.State != UploadStateFailed || failed.ReceivedBytes != 0 {
		t.Fatalf("失败状态没有记下来: %+v", failed)
	}
	if failed.Error == "" {
		t.Fatal("失败原因必须写清楚")
	}
	if got := m.ReservedBytes("etc"); got != 0 {
		t.Fatalf("失败之后预留应当释放，得到 %d", got)
	}
	// 半份数据必须删掉：留着它只会让人以为“传了一半也存下了”。
	for _, name := range stagingEntries(t, dir) {
		if strings.HasSuffix(name, uploadDataSuffix) {
			t.Fatalf("失败的上传留下了数据文件: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "app.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("失败的上传动了目标: %v", err)
	}
}

// TestUploadTooMuchContentFails 实收多于声明：同样作废（不截断、不将就）。
func TestUploadTooMuchContentFails(t *testing.T) {
	m, dir := newWritableManager(t, nil)
	ctx := context.Background()

	info := mustCreateUpload(t, m, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 3,
	})
	if _, err := m.WriteUploadContent(ctx, info.UploadID, strings.NewReader("toolong")); !errors.Is(err, ErrUploadState) {
		t.Fatalf("字节超过声明应当报 ErrUploadState，得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("失败的上传动了目标: %v", err)
	}
}

// TestUploadContentOnlyOnce 收完之后再写一次：状态不允许，不覆盖已收的字节。
func TestUploadContentOnlyOnce(t *testing.T) {
	m, _ := newWritableManager(t, nil)
	ctx := context.Background()

	info := mustCreateUpload(t, m, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 2,
	})
	if _, err := m.WriteUploadContent(ctx, info.UploadID, strings.NewReader("ab")); err != nil {
		t.Fatalf("第一次写失败: %v", err)
	}
	if _, err := m.WriteUploadContent(ctx, info.UploadID, strings.NewReader("cd")); !errors.Is(err, ErrUploadState) {
		t.Fatalf("重复写应当报 ErrUploadState，得到 %v", err)
	}
}

// TestUploadOverwriteRules 覆盖已存在目标：必须先选 overwrite，且版本要对得上。
func TestUploadOverwriteRules(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "app.conf"), "old\n")
	})
	ctx := context.Background()

	current, err := m.Stat(ctx, "etc", "app.conf")
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}

	// 拿旧版本建暂存：上传还没开始就该被拒绝，别等传完 2 GiB。
	if _, err := m.CreateUpload(ctx, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 3,
		ExpectedVersion: "v1-deadbeef-deadbeef-4-17a0b1c2d3e4",
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本对不上应当报 ErrVersionConflict，得到 %v", err)
	}

	info := mustCreateUpload(t, m, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 4, ExpectedVersion: current.Version,
	})
	if _, err := m.WriteUploadContent(ctx, info.UploadID, strings.NewReader("new\n")); err != nil {
		t.Fatalf("写内容失败: %v", err)
	}

	// 没选覆盖：目标已存在，拒绝。
	if _, err := m.CommitUpload(ctx, CommitUploadParams{UploadID: info.UploadID}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("未选择覆盖时应当报 ErrAlreadyExists，得到 %v", err)
	}
	// 选了覆盖但没给版本：同样拒绝（否则“覆盖”等于“谁先写完算谁”）。
	if _, err := m.CommitUpload(ctx, CommitUploadParams{
		UploadID: info.UploadID, Overwrite: true,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("覆盖时缺少版本应当报 ErrVersionConflict，得到 %v", err)
	}
	if got := mustFileContent(t, filepath.Join(dir, "app.conf")); got != "old\n" {
		t.Fatalf("被拒绝的提交改了文件: %q", got)
	}

	result, err := m.CommitUpload(ctx, CommitUploadParams{
		UploadID: info.UploadID, Overwrite: true, ExpectedVersion: current.Version,
	})
	if err != nil {
		t.Fatalf("覆盖提交失败: %v", err)
	}
	if result.BackupPath == "" {
		t.Fatalf("覆盖必须留下旧版本的备份: %+v", result)
	}
	if got := mustFileContent(t, filepath.Join(dir, "app.conf")); got != "new\n" {
		t.Fatalf("覆盖后内容不对: %q", got)
	}
	if got := mustFileContent(t, filepath.Join(dir, result.BackupPath)); got != "old\n" {
		t.Fatalf("备份内容不是旧版本: %q", got)
	}
}

// TestUploadRejectsOversizeDeclaration 超过单次上限在第一步就拒绝（不留暂存）。
func TestUploadRejectsOversizeDeclaration(t *testing.T) {
	dir := t.TempDir()
	m, err := New([]Root{{ID: "etc", Name: "配置", Path: dir, Writable: true}},
		Options{MaxUploadBytes: 8})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if m.MaxUploadBytes() != 8 {
		t.Fatalf("上限没有按配置生效: %d", m.MaxUploadBytes())
	}
	if _, err := m.CreateUpload(context.Background(), CreateUploadParams{
		RootID: "etc", Path: "a.conf", SizeBytes: 9,
	}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过上限应当报 ErrTooLarge，得到 %v", err)
	}
	if got := m.ReservedBytes("etc"); got != 0 {
		t.Fatalf("被拒绝的声明不该占预留，得到 %d", got)
	}
}

// TestDeleteUploadReleasesReservation 放弃上传：记录消失、预留清零、目标不动。
func TestDeleteUploadReleasesReservation(t *testing.T) {
	m, dir := newWritableManager(t, nil)
	ctx := context.Background()

	info := mustCreateUpload(t, m, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 1024,
	})
	if err := m.DeleteUpload(ctx, info.UploadID); err != nil {
		t.Fatalf("DeleteUpload 失败: %v", err)
	}
	if _, err := m.GetUpload(ctx, info.UploadID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("删除之后应当查不到，得到 %v", err)
	}
	if got := m.ReservedBytes("etc"); got != 0 {
		t.Fatalf("删除之后预留应当清零，得到 %d", got)
	}
	if left := stagingEntries(t, dir); len(left) != 0 {
		t.Fatalf("删除之后暂存目录还有残留: %v", left)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("放弃上传动了目标: %v", err)
	}
}

// TestUploadExpirySweep 过期清理：24 小时之后暂存被扫掉，预留随之释放。
func TestUploadExpirySweep(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	now := base
	m, err := New([]Root{{ID: "etc", Name: "配置", Path: dir, Writable: true}},
		Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx := context.Background()

	info := mustCreateUpload(t, m, CreateUploadParams{
		RootID: "etc", Path: "app.conf", SizeBytes: 64,
	})
	if want := base.Add(UploadTTL); !info.ExpiresAt.Equal(want) {
		t.Fatalf("到期时间应当是 %s，得到 %s", want, info.ExpiresAt)
	}

	// 还没到期：清扫什么都不做。
	if _, err := m.SweepStaging(ctx, base.Add(time.Hour)); err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	if _, err := m.GetUpload(ctx, info.UploadID); err != nil {
		t.Fatalf("未到期的暂存被清扫了: %v", err)
	}

	now = base.Add(UploadTTL + time.Minute)
	cleaned, err := m.SweepStaging(ctx, now)
	if err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("应当清扫 1 张暂存，得到 %d", cleaned)
	}
	if _, err := m.GetUpload(ctx, info.UploadID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("过期之后应当查不到，得到 %v", err)
	}
	if got := m.ReservedBytes("etc"); got != 0 {
		t.Fatalf("过期之后预留应当释放，得到 %d", got)
	}
	if left := stagingEntries(t, dir); len(left) != 0 {
		t.Fatalf("过期之后暂存目录还有残留: %v", left)
	}
}

// TestUploadParamsAndIDShapes 参数与 id 的形状校验：不合法的输入不进暂存目录。
func TestUploadParamsAndIDShapes(t *testing.T) {
	m, _ := newWritableManager(t, nil)
	ctx := context.Background()

	cases := []struct {
		name   string
		params CreateUploadParams
	}{
		{"空路径", CreateUploadParams{RootID: "etc", Path: "", SizeBytes: 1}},
		{"绝对路径", CreateUploadParams{RootID: "etc", Path: "/etc/passwd", SizeBytes: 1}},
		{"越界", CreateUploadParams{RootID: "etc", Path: "../x", SizeBytes: 1}},
		{"保留目录", CreateUploadParams{RootID: "etc", Path: ReservedDirName + "/staging/x", SizeBytes: 1}},
		{"零大小", CreateUploadParams{RootID: "etc", Path: "a", SizeBytes: 0}},
		{"未登记根", CreateUploadParams{RootID: "nope", Path: "a", SizeBytes: 1}},
	}
	for _, item := range cases {
		if _, err := m.CreateUpload(ctx, item.params); err == nil {
			t.Errorf("%s 应当被拒绝", item.name)
		}
	}

	valid := []string{"up_" + strings.Repeat("0", 32)}
	if !ValidUploadID(valid[0]) {
		t.Fatal("合法 id 被拒绝")
	}
	bad := []string{
		"",
		"up_",
		"up_0123",
		"UP_" + strings.Repeat("0", 32),
		"up_" + strings.Repeat("0", 31),
		"up_" + strings.Repeat("g", 32),
		"../up_" + strings.Repeat("0", 32),
	}
	for _, id := range bad {
		if ValidUploadID(id) {
			t.Errorf("非法 id %q 被接受", id)
		}
	}
	for _, id := range bad {
		if _, err := m.GetUpload(ctx, id); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("GetUpload(%q) 应当报 ErrInvalidParams，得到 %v", id, err)
		}
	}
}
