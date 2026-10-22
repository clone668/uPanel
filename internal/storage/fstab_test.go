package storage

// fstab 托管块的测试（P07.4）。
//
// 这个文件写坏的后果是“机器起不来”，所以测试的重点不在“成功时写入了正确的行”，
// 而在三件事：
//   - 块外的内容**逐字节**不变（别人的配置不能被我们碰）；
//   - 结构不对劲时**拒绝而不是修**（BEGIN/END 不配对、块里出现非 UUID 行）；
//   - 写入顺序（候选 → fsync → 校验 → 备份 → rename → fsync 目录）里任何一步
//     失败，目标文件都必须保持原样，或者有一份能换回去的备份。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const existingFstab = `# /etc/fstab: static file system information.
UUID=11112222-3333-4444-5555-666677778888 / ext4 errors=remount-ro 0 1
/swap.img none swap sw 0 0
`

// ---------- 解析 ----------

func TestParseFstabWithoutBlock(t *testing.T) {
	block, err := ParseFstab(existingFstab)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if block.HasBlock {
		t.Fatal("没有托管块，HasBlock 却是真")
	}
	if block.Prefix != existingFstab {
		t.Errorf("Prefix 不是原文：\n%q", block.Prefix)
	}
	if len(block.Entries) != 0 {
		t.Errorf("条目数 = %d，想要 0", len(block.Entries))
	}
}

func TestParseFstabWithBlock(t *testing.T) {
	content := existingFstab +
		fstabBeginMarker + "\n" +
		fstabBlockHeader + "\n" +
		"UUID=" + testUUID + " /srv/upanel/volumes/data ext4 rw,nodev,nosuid,nofail,noauto 0 2\n" +
		fstabEndMarker + "\n" +
		"# trailing comment\n"

	block, err := ParseFstab(content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !block.HasBlock {
		t.Fatal("有托管块，HasBlock 却是假")
	}
	if block.Prefix != existingFstab {
		t.Errorf("Prefix 未原样保留：\n%q", block.Prefix)
	}
	if block.Suffix != "# trailing comment\n" {
		t.Errorf("Suffix 未原样保留：%q", block.Suffix)
	}
	if len(block.Entries) != 1 {
		t.Fatalf("条目数 = %d，想要 1", len(block.Entries))
	}
	e := block.Entries[0]
	if e.UUID != testUUID || e.MountPoint != "/srv/upanel/volumes/data" ||
		e.FSType != FilesystemExt4 || e.Pass != 2 || e.Dump != 0 {
		t.Errorf("解析结果不对：%+v", e)
	}
	if got := DescribeOptions(e.Options); got != "rw,nodev,nosuid,nofail,noauto" {
		t.Errorf("选项 = %q", got)
	}
}

func TestParseFstabRejectsMalformedBlock(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "只有起始标记",
			content: "# x\n" + fstabBeginMarker + "\nUUID=" + testUUID + " /srv/upanel/volumes/a ext4 rw 0 2\n",
			want:    "没有结束标记",
		},
		{
			name:    "只有结束标记",
			content: "# x\n" + fstabEndMarker + "\n",
			want:    "没有起始标记",
		},
		{
			name: "两个起始标记",
			content: fstabBeginMarker + "\n" + fstabEndMarker + "\n" +
				fstabBeginMarker + "\n" + fstabEndMarker + "\n",
			want: "多个起始标记",
		},
		{
			name:    "结束在起始之前",
			content: fstabEndMarker + "\n" + fstabBeginMarker + "\n",
			want:    "之前",
		},
		{
			name: "块内用设备路径",
			content: fstabBeginMarker + "\n" +
				"/dev/sdb1 /srv/upanel/volumes/a ext4 rw 0 2\n" + fstabEndMarker + "\n",
			want: "只允许 UUID=… 形式",
		},
		{
			name: "块内挂载点不是绝对路径",
			content: fstabBeginMarker + "\n" +
				"UUID=" + testUUID + " srv/upanel/volumes/a ext4 rw 0 2\n" + fstabEndMarker + "\n",
			want: "不是绝对路径",
		},
		{
			name: "块内字段不足",
			content: fstabBeginMarker + "\n" +
				"UUID=" + testUUID + " /srv/upanel/volumes/a ext4\n" + fstabEndMarker + "\n",
			want: "字段不足",
		},
		{
			name: "块内第六列不是数字",
			content: fstabBeginMarker + "\n" +
				"UUID=" + testUUID + " /srv/upanel/volumes/a ext4 rw 0 x\n" + fstabEndMarker + "\n",
			want: "不是数字",
		},
		{
			name: "块内 UUID 形状不对",
			content: fstabBeginMarker + "\n" +
				"UUID=zzzz /srv/upanel/volumes/a ext4 rw 0 2\n" + fstabEndMarker + "\n",
			want: "UUID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFstab(tc.content)
			if err == nil {
				t.Fatal("期望解析失败，实际成功了")
			}
			if !errors.Is(err, ErrFstabMalformed) {
				t.Errorf("错误类型不对：%v", err)
			}
			mustContain(t, err.Error(), tc.want, "错误消息")
		})
	}
}

// TestParseFstabMarkerNeedsWholeLine 钉住“标记用整行匹配”这件事：
// 注释里提到标记的行不该被当成标记。
func TestParseFstabMarkerNeedsWholeLine(t *testing.T) {
	content := "# " + fstabBeginMarker + " (old, deprecated)\n" + existingFstab
	block, err := ParseFstab(content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if block.HasBlock {
		t.Fatal("注释里的标记被当成了真标记")
	}
}

// ---------- 渲染 ----------

func TestRenderPreservesOutsideBytes(t *testing.T) {
	content := "# head\n\nUUID=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee / xfs defaults 0 0" // 无结尾换行
	block, err := ParseFstab(content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	out := block.Render([]FstabEntry{{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw", "nodev", "nosuid", "nofail", "noauto"}, Pass: 2,
	}})

	if !strings.HasPrefix(out, content) {
		// Prefix 没有结尾换行时必须补一个，否则 BEGIN 会粘到上一行末尾。
		t.Fatalf("块前原文被改动：\n%q", out)
	}
	if !strings.Contains(out, "\n"+fstabBeginMarker+"\n") {
		t.Errorf("起始标记没有独占一行：\n%s", out)
	}
	if !strings.HasSuffix(out, fstabEndMarker+"\n") {
		t.Errorf("结束标记不在末尾：\n%q", out)
	}
}

func TestRenderRoundTrip(t *testing.T) {
	block, err := ParseFstab(existingFstab)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	entries := []FstabEntry{
		{UUID: testUUID2, MountPoint: "/srv/upanel/volumes/zeta", FSType: FilesystemExt4,
			Options: []string{"ro", "nodev", "nosuid", "nofail", "noauto"}, Pass: 2},
		{UUID: testUUID, MountPoint: "/srv/upanel/volumes/alpha", FSType: FilesystemXFS,
			Options: []string{"rw", "nodev", "nosuid", "nofail", "x-systemd.device-timeout=30"}, Pass: 0},
	}
	first := block.Render(entries)
	second := block.Render(entries)
	if first != second {
		t.Fatal("同样的条目渲染出不同字节：顺序不稳定，验收与 diff 会假阳性")
	}

	back, err := ParseFstab(first)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if len(back.Entries) != 2 {
		t.Fatalf("回读条目数 = %d，想要 2", len(back.Entries))
	}
	// 渲染按挂载点排序：alpha 在前。
	if back.Entries[0].MountPoint != "/srv/upanel/volumes/alpha" {
		t.Errorf("条目没有按挂载点排序：%+v", back.Entries)
	}
	if back.Prefix != existingFstab {
		t.Errorf("回读后块前原文不同：%q", back.Prefix)
	}
}

// ---------- 写入 ----------

func TestFstabWriteReplacesBlockAndKeepsBackup(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}

	entry := FstabEntry{UUID: testUUID, MountPoint: "/srv/upanel/volumes/data",
		FSType: FilesystemExt4, Options: []string{"rw", "nodev", "nosuid", "nofail", "noauto"}, Pass: 2}
	backup, err := f.fstab.Write(context.Background(), state, []FstabEntry{entry})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if backup == "" {
		t.Fatal("没有生成备份路径")
	}

	data, err := os.ReadFile(filepath.Join(f.dir, "fstab"))
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	mustContain(t, string(data), "UUID="+testUUID+" /srv/upanel/volumes/data ext4", "新 fstab")
	mustContain(t, string(data), existingFstab, "新 fstab（块外内容）")

	backupData, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("读备份失败: %v", err)
	}
	if string(backupData) != existingFstab {
		t.Errorf("备份内容不是旧内容：\n%s", backupData)
	}

	// findmnt --verify 必须是在 rename 之前跑的（候选文件路径以 .upanel-new 结尾）。
	verifies := f.runner.ran("findmnt")
	if len(verifies) != 1 {
		t.Fatalf("findmnt 调用次数 = %d，想要 1", len(verifies))
	}
	if !strings.HasSuffix(verifies[0].Args[2], ".upanel-new") {
		t.Errorf("findmnt 校验的不是候选文件：%v", verifies[0].Args)
	}
}

func TestFstabWriteRefusesExternalChange(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	// 预检之后别人改了文件。
	changed := existingFstab + "# someone else's line\n"
	if err := os.WriteFile(filepath.Join(f.dir, "fstab"), []byte(changed), 0o644); err != nil {
		t.Fatalf("改文件失败: %v", err)
	}

	_, err = f.fstab.Write(context.Background(), state, nil)
	if !errors.Is(err, ErrFstabChanged) {
		t.Fatalf("期望 ErrFstabChanged，实际 %v", err)
	}
	now, _ := os.ReadFile(filepath.Join(f.dir, "fstab"))
	if string(now) != changed {
		t.Error("拒绝覆盖之后文件被改动了")
	}
}

func TestFstabWriteNoopWhenIdentical(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	entry := FstabEntry{UUID: testUUID, MountPoint: "/srv/upanel/volumes/data",
		FSType: FilesystemExt4, Options: []string{"rw", "nodev", "nosuid", "nofail", "noauto"}, Pass: 2}
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if _, err := f.fstab.Write(context.Background(), state, []FstabEntry{entry}); err != nil {
		t.Fatalf("第一次写入失败: %v", err)
	}
	state2, _ := f.fstab.Read()
	backup, err := f.fstab.Write(context.Background(), state2, []FstabEntry{entry})
	if err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}
	if backup != "" {
		t.Errorf("内容没变却生成了备份 %q", backup)
	}
	if n := len(f.runner.ran("findmnt")); n != 1 {
		t.Errorf("内容没变却重新校验了 %d 次", n)
	}
}

func TestFstabWriteVerifyFailureLeavesTargetUntouched(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	f.runner.fail("findmnt", 1, "unknown option 'rwx'")
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	_, err = f.fstab.Write(context.Background(), state, []FstabEntry{{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw", "nodev", "nosuid", "nofail", "noauto"}, Pass: 2,
	}})
	if !errors.Is(err, ErrFstabVerify) {
		t.Fatalf("期望 ErrFstabVerify，实际 %v", err)
	}
	now, _ := os.ReadFile(filepath.Join(f.dir, "fstab"))
	if string(now) != existingFstab {
		t.Error("校验失败后目标文件被改动了")
	}
	if _, statErr := os.Stat(filepath.Join(f.dir, "fstab.upanel-new")); !os.IsNotExist(statErr) {
		t.Error("校验失败后候选文件没有被清理")
	}
}

func TestFstabWriteRequiresRunner(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	f.fstab.Runner = nil
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	_, err = f.fstab.Write(context.Background(), state, []FstabEntry{{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	}})
	if err == nil {
		t.Fatal("没有校验器时仍然写了")
	}
}

func TestFstabWriteRejectsBadEntrySets(t *testing.T) {
	good := FstabEntry{UUID: testUUID, MountPoint: "/srv/upanel/volumes/data",
		FSType: FilesystemExt4, Options: []string{"rw", "nodev"}, Pass: 2}
	cases := []struct {
		name    string
		entries []FstabEntry
		want    string
	}{
		{"挂载点不在受管目录下", []FstabEntry{{
			UUID: testUUID, MountPoint: "/etc", FSType: FilesystemExt4,
			Options: []string{"rw"}, Pass: 2}}, "不在"},
		{"挂载点不是规范形式", []FstabEntry{{
			UUID: testUUID, MountPoint: "/srv/upanel/volumes/../etc", FSType: FilesystemExt4,
			Options: []string{"rw"}, Pass: 2}}, "不是规范形式"},
		{"UUID 形状不对", []FstabEntry{{
			UUID: "nope", MountPoint: "/srv/upanel/volumes/a", FSType: FilesystemExt4,
			Options: []string{"rw"}, Pass: 2}}, "UUID"},
		{"文件系统类型不支持", []FstabEntry{{
			UUID: testUUID, MountPoint: "/srv/upanel/volumes/a", FSType: "ntfs",
			Options: []string{"rw"}, Pass: 2}}, "不在支持范围"},
		{"没有选项", []FstabEntry{{
			UUID: testUUID, MountPoint: "/srv/upanel/volumes/a", FSType: FilesystemExt4,
			Options: nil, Pass: 2}}, "没有挂载选项"},
		{"选项不在允许集合", []FstabEntry{{
			UUID: testUUID, MountPoint: "/srv/upanel/volumes/a", FSType: FilesystemExt4,
			Options: []string{"rw", "exec"}, Pass: 2}}, "不在允许集合"},
		{"挂载点重复", []FstabEntry{good, {
			UUID: testUUID2, MountPoint: good.MountPoint, FSType: FilesystemExt4,
			Options: []string{"rw"}, Pass: 2}}, "重复"},
		{"UUID 重复", []FstabEntry{good, {
			UUID: testUUID, MountPoint: "/srv/upanel/volumes/other", FSType: FilesystemExt4,
			Options: []string{"rw"}, Pass: 2}}, "重复"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newVolumeFixture(t, existingFstab)
			state, err := f.fstab.Read()
			if err != nil {
				t.Fatalf("读失败: %v", err)
			}
			_, err = f.fstab.Write(context.Background(), state, tc.entries)
			if err == nil {
				t.Fatal("不合法的条目集合被写下去了")
			}
			if !errors.Is(err, ErrUnsafeWrite) {
				t.Errorf("错误类型不对：%v", err)
			}
			mustContain(t, err.Error(), tc.want, "错误消息")
			if n := len(f.runner.ran("findmnt")); n != 0 {
				t.Errorf("条目校验失败却执行了 %d 次 findmnt", n)
			}
		})
	}
}

// TestFstabWriteFsyncFailureAfterRename 钉住一个容易被“简化掉”的分支：
// rename 成功但目录 fsync 失败时，操作不能算失败（盘上已经是新内容），
// 但必须把备份路径交出去，让调用方知道“掉电可能回退”。
func TestFstabWriteFsyncFailureAfterRename(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	f.fstab.fsyncDir = func(string) error { return errors.New("input/output error") }
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	backup, err := f.fstab.Write(context.Background(), state, []FstabEntry{{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	}})
	if err == nil {
		t.Fatal("目录 fsync 失败却完全没有报错")
	}
	if backup == "" {
		t.Error("目录 fsync 失败时应仍然交回备份路径")
	}
	mustContain(t, err.Error(), "fsync", "错误消息")
	now, _ := os.ReadFile(filepath.Join(f.dir, "fstab"))
	mustContain(t, string(now), "UUID="+testUUID, "替换后的 fstab")
}

func TestFstabRestore(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	backup, err := f.fstab.Write(context.Background(), state, []FstabEntry{{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	}})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := f.fstab.Restore(context.Background(), backup); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	now, _ := os.ReadFile(filepath.Join(f.dir, "fstab"))
	if string(now) != existingFstab {
		t.Errorf("回滚后内容不是原文：\n%s", now)
	}
}

func TestFstabReadMissingFile(t *testing.T) {
	dir := t.TempDir()
	s := FstabStore{Path: filepath.Join(dir, "fstab")}
	st, err := s.Read()
	if err != nil {
		t.Fatalf("读不存在的文件应当成功: %v", err)
	}
	if !st.Missing {
		t.Error("Missing 应为真")
	}
	if st.Hash != fstabHash("") {
		t.Errorf("空内容的哈希不对：%s", st.Hash)
	}
}

// ---------- 比较与选项集合 ----------

func TestFstabEntrySameAs(t *testing.T) {
	a := FstabEntry{UUID: testUUID, MountPoint: "/srv/upanel/volumes/data",
		FSType: FilesystemExt4, Options: []string{"rw", "nodev"}, Pass: 2}
	b := a
	if !a.SameAs(b) {
		t.Fatal("同样的条目被判为不同")
	}
	b.Dump = 1 // dump 恒为 0，不参与比较
	if !a.SameAs(b) {
		t.Error("dump 参与了比较")
	}
	b = a
	b.Options = []string{"rw", "nosuid"}
	if a.SameAs(b) {
		t.Error("选项不同却判为相同")
	}
	b = a
	b.UUID = strings.ToUpper(testUUID)
	if !a.SameAs(b) {
		t.Error("UUID 大小写不应影响比较")
	}
	b = a
	b.Pass = 0
	if a.SameAs(b) {
		t.Error("fsck 顺序不同却判为相同")
	}
}

func TestValidMountOption(t *testing.T) {
	ok := []string{"rw", "ro", "nodev", "nosuid", "nofail", "noauto", "defaults",
		"x-systemd.device-timeout=30", "x-systemd.device-timeout=1", "x-systemd.device-timeout=300"}
	for _, opt := range ok {
		if !validMountOption(opt) {
			t.Errorf("%q 应当被接受", opt)
		}
	}
	bad := []string{"exec", "suid", "dev", "auto", "noatime", "x-systemd.device-timeout=0",
		"x-systemd.device-timeout=301", "x-systemd.device-timeout=abc", "x-other=1", ""}
	for _, opt := range bad {
		if validMountOption(opt) {
			t.Errorf("%q 不应当被接受", opt)
		}
	}
}

// TestFstabWriteCallsFindmntWithFixedProgram 钉住“用固定路径的 findmnt”：
// 不读 PATH，也不接受参数拼进来的程序名。
func TestFstabWriteCallsFindmntWithFixedProgram(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	state, _ := f.fstab.Read()
	if _, err := f.fstab.Write(context.Background(), state, []FstabEntry{{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	calls := f.runner.ran("findmnt")
	if len(calls) != 1 {
		t.Fatalf("findmnt 调用次数 = %d", len(calls))
	}
	if calls[0].Program != "/usr/bin/findmnt" {
		t.Errorf("findmnt 路径 = %q，想要固定的 /usr/bin/findmnt", calls[0].Program)
	}
	want := []string{"--verify", "--tab"}
	if len(calls[0].Args) != 3 || calls[0].Args[0] != want[0] || calls[0].Args[1] != want[1] {
		t.Errorf("findmnt 参数 = %v", calls[0].Args)
	}
	// 绝不能出现 mount -a。
	if n := len(f.runner.ran("mount")); n != 0 {
		t.Errorf("写 fstab 执行了 %d 次 mount（禁止 mount -a）", n)
	}
}

// TestFstabWriteFsyncOrder 钉住写入顺序里最容易在重构中被调换的两步：
// rename 之后再 fsync 目录。反过来（先 fsync 目录再 rename）在掉电时丢的是
// 整个替换动作——文件可能回到旧内容，而备份已经生成、调用方以为写成功了。
func TestFstabWriteFsyncOrder(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	var events []string
	f.fstab.rename = func(oldPath, newPath string) error {
		events = append(events, "rename")
		return os.Rename(oldPath, newPath)
	}
	f.fstab.fsyncDir = func(string) error {
		events = append(events, "fsync-dir")
		return nil
	}
	state, err := f.fstab.Read()
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if _, err := f.fstab.Write(context.Background(), state, []FstabEntry{{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if len(events) != 2 || events[0] != "rename" || events[1] != "fsync-dir" {
		t.Errorf("事件顺序 = %v，期望 [rename fsync-dir]", events)
	}
}
