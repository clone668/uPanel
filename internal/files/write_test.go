//go:build linux

package files

// P06.2 写操作的真实文件系统用例（只在 Linux 上跑）。
//
// 与 manager_linux_test.go 同一个立场：这里不 mock 任何东西，真的建目录、真的
// renameat、真的读回来。写路径上有三件事只有真机上才能验：
//
//  1) **落位之后读回来的东西**与预期一致（verified 不是“调用没报错”）；
//  2) **版本冲突不覆盖别人的修改**：拿旧版本改写必须被拒绝，且文件一个字节都不变；
//  3) **保留目录 .upanel 既不出现在列举里，也不接受路径**：暂存与备份不是“服务器上的文件”。
//
// 这些用例在 Windows 宿主上不参与编译，结论只能由独立 Ubuntu 虚拟机给出：
// 交叉编译 `GOOS=linux go test -c` 后到 upanel-2404 执行，证据在
// docs/evidence/P06/<日期>-p06.2/ 下。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// newWritableManager 在一个临时目录上登记一个**可写**根。
//
// 复用 newManager 不行：它的根是只读的，而只读根正是写操作的第一道门槛。
func newWritableManager(t *testing.T, mutate func(dir string)) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	if mutate != nil {
		mutate(dir)
	}
	m, err := New([]Root{{ID: "etc", Name: "配置", Path: dir, Writable: true}}, Options{})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return m, dir
}

func mustFileContent(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(raw)
}

func stagingEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ReservedDirName, stagingSubdir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("读取暂存目录失败: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

// TestMkdirCreatesDirectory 建目录：结果读回来是目录、版本可解析、重复创建被拒绝。
func TestMkdirCreatesDirectory(t *testing.T) {
	m, dir := newWritableManager(t, nil)
	ctx := context.Background()

	result, err := m.Mkdir(ctx, MkdirParams{RootID: "etc", Path: "logs"})
	if err != nil {
		t.Fatalf("Mkdir 失败: %v", err)
	}
	if result.Type != TypeDir || result.Version == "" {
		t.Fatalf("建目录的结果不完整: %+v", result)
	}
	info, err := os.Stat(filepath.Join(dir, "logs"))
	if err != nil || !info.IsDir() {
		t.Fatalf("磁盘上没有这个目录: %v", err)
	}

	// 已存在就拒绝：建目录不能变成“有就跳过”，那会掩盖一次打错的名字。
	if _, err := m.Mkdir(ctx, MkdirParams{RootID: "etc", Path: "logs"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重复建目录应当报 ErrAlreadyExists，得到 %v", err)
	}
}

// TestMkdirRejectsBadPaths 形状不合法的路径在参数层就被拒，不需要等内核。
func TestMkdirRejectsBadPaths(t *testing.T) {
	m, _ := newWritableManager(t, nil)
	cases := []struct {
		path string
		want error
	}{
		{"", ErrInvalidParams},
		{"/abs", ErrPathRejected},
		{"../escape", ErrPathRejected},
		{"a/../b", ErrPathRejected},
		{"a//b", ErrInvalidParams},
		{"./a", ErrInvalidParams},
		{ReservedDirName, ErrPathRejected},
		{ReservedDirName + "/staging", ErrPathRejected},
	}
	for _, item := range cases {
		_, err := m.Mkdir(context.Background(), MkdirParams{RootID: "etc", Path: item.path})
		if !errors.Is(err, item.want) {
			t.Errorf("Mkdir(%q) 期望 %v，得到 %v", item.path, item.want, err)
		}
	}
}

// TestWriteTextCreatesFile 新建文本文件：读回来的大小、类型、版本都对得上磁盘。
func TestWriteTextCreatesFile(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		if err := os.Mkdir(filepath.Join(dir, "conf"), 0o755); err != nil {
			t.Fatalf("建父目录失败: %v", err)
		}
	})
	ctx := context.Background()

	result, err := m.WriteText(ctx, WriteTextParams{
		RootID:  "etc",
		Path:    "conf/app.conf",
		Content: "a=1\n",
	})
	if err != nil {
		t.Fatalf("WriteText 失败: %v", err)
	}
	if result.Type != TypeFile || result.Version == "" {
		t.Fatalf("写入结果不完整: %+v", result)
	}
	if got := mustFileContent(t, filepath.Join(dir, "conf", "app.conf")); got != "a=1\n" {
		t.Fatalf("磁盘内容不对: %q", got)
	}
	if result.BackupPath != "" || result.BackupCount != 0 {
		t.Fatalf("新建文件不该产生备份: %+v", result)
	}
	// 暂存目录必须是干净的：临时文件不能留在根里。
	if left := stagingEntries(t, dir); len(left) != 0 {
		t.Fatalf("写完之后暂存目录还有残留: %v", left)
	}
}

// TestWriteTextOverwriteRequiresVersionAndLeavesBackup 覆盖必须给出当前版本，且旧内容有备份。
func TestWriteTextOverwriteRequiresVersionAndLeavesBackup(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "app.conf"), "old=1\n")
	})
	ctx := context.Background()

	before, err := m.Stat(ctx, "etc", "app.conf")
	if err != nil || !before.Exists {
		t.Fatalf("Stat 失败: %v", err)
	}

	// 不给版本 = “用我手里这份覆盖别人刚改的那份”，必须拒绝。
	if _, err := m.WriteText(ctx, WriteTextParams{
		RootID: "etc", Path: "app.conf", Content: "new=1\n",
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("覆盖时缺少版本应当报 ErrVersionConflict，得到 %v", err)
	}
	if got := mustFileContent(t, filepath.Join(dir, "app.conf")); got != "old=1\n" {
		t.Fatalf("被拒绝的写入改了文件: %q", got)
	}

	result, err := m.WriteText(ctx, WriteTextParams{
		RootID:          "etc",
		Path:            "app.conf",
		ExpectedVersion: before.Version,
		Content:         "new=1\n",
	})
	if err != nil {
		t.Fatalf("带版本的覆盖失败: %v", err)
	}
	if got := mustFileContent(t, filepath.Join(dir, "app.conf")); got != "new=1\n" {
		t.Fatalf("磁盘内容不对: %q", got)
	}
	if result.BackupPath == "" {
		t.Fatalf("覆盖必须留下旧版本的备份: %+v", result)
	}
	if got := mustFileContent(t, filepath.Join(dir, result.BackupPath)); got != "old=1\n" {
		t.Fatalf("备份内容不是旧版本: %q", got)
	}
	if result.BackupCount != 1 {
		t.Fatalf("备份份数应当是 1，得到 %d", result.BackupCount)
	}
}

// TestWriteTextStaleVersionRejected 拿过期版本改写被拒绝，文件与备份都不动。
func TestWriteTextStaleVersionRejected(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "app.conf"), "old=1\n")
	})
	ctx := context.Background()

	stale := "v1-deadbeef-deadbeef-6-17a0b1c2d3e4"
	_, err := m.WriteText(ctx, WriteTextParams{
		RootID: "etc", Path: "app.conf", ExpectedVersion: stale, Content: "evil=1\n",
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本应当报 ErrVersionConflict，得到 %v", err)
	}
	if got := mustFileContent(t, filepath.Join(dir, "app.conf")); got != "old=1\n" {
		t.Fatalf("被拒绝的写入改了文件: %q", got)
	}
	// 备份目录不该出现：拒绝发生在落位之前。
	if _, err := os.Stat(filepath.Join(dir, ReservedDirName, backupSubdir)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(dir, ReservedDirName, backupSubdir))
		if len(entries) != 0 {
			t.Fatalf("被拒绝的写入留下了备份目录: %d 项", len(entries))
		}
	}
}

// TestWriteTextValidateRules 内容本身的规则：超上限、非 UTF-8、含 NUL 都拒绝。
func TestWriteTextValidateRules(t *testing.T) {
	m, dir := newWritableManager(t, nil)
	ctx := context.Background()

	big := strings.Repeat("a", MaxTextBytes+1)
	if _, err := m.WriteText(ctx, WriteTextParams{
		RootID: "etc", Path: "big.txt", Content: big,
	}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过 1 MiB 应当报 ErrTooLarge，得到 %v", err)
	}
	if _, err := m.WriteText(ctx, WriteTextParams{
		RootID: "etc", Path: "nul.txt", Content: "a\x00b",
	}); !errors.Is(err, ErrNotText) {
		t.Fatalf("含 NUL 应当报 ErrNotText，得到 %v", err)
	}
	if _, err := m.WriteText(ctx, WriteTextParams{
		RootID: "etc", Path: "bad.txt", Content: string([]byte{0xff, 0xfe}),
	}); !errors.Is(err, ErrNotText) {
		t.Fatalf("非法 UTF-8 应当报 ErrNotText，得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big.txt")); err == nil {
		t.Fatal("被拒绝的写入在磁盘上留下了文件")
	}
}

// TestWriteTextOntoDirectoryRejected 往目录上写文本：明确报“目标是目录”。
func TestWriteTextOntoDirectoryRejected(t *testing.T) {
	m, _ := newWritableManager(t, func(dir string) {
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	})
	_, err := m.WriteText(context.Background(), WriteTextParams{
		RootID: "etc", Path: "sub", Content: "x",
	})
	if !errors.Is(err, ErrIsDirectory) {
		t.Fatalf("往目录上写文本应当报 ErrIsDirectory，得到 %v", err)
	}
}

// TestDeleteFileAndTree 删除：文件删掉、空目录可删、非空目录要递归、递归后整棵树都没了。
func TestDeleteFileAndTree(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.conf"), "a\n")
		if err := os.MkdirAll(filepath.Join(dir, "tree", "deep"), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		writeFile(t, filepath.Join(dir, "tree", "one.txt"), "1")
		writeFile(t, filepath.Join(dir, "tree", "deep", "two.txt"), "22")
		if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
			t.Fatalf("建空目录失败: %v", err)
		}
	})
	ctx := context.Background()

	stat, err := m.Stat(ctx, "etc", "a.conf")
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	result, err := m.Remove(ctx, DeleteParams{
		RootID: "etc", Path: "a.conf", ExpectedVersion: stat.Version,
	})
	if err != nil {
		t.Fatalf("删除文件失败: %v", err)
	}
	if result.Version != "" {
		t.Fatalf("删除后的结果不该带版本: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("文件还在: %v", err)
	}

	if _, err := m.Remove(ctx, DeleteParams{RootID: "etc", Path: "empty"}); err != nil {
		t.Fatalf("删除空目录失败: %v", err)
	}

	// 非空目录：非递归删除必须失败（不在下面偷偷删一半）。
	if _, err := m.Remove(ctx, DeleteParams{RootID: "etc", Path: "tree"}); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("非递归删除非空目录应当报 ErrInvalidParams，得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tree", "one.txt")); err != nil {
		t.Fatalf("被拒绝的删除动了内容: %v", err)
	}

	treeResult, err := m.Remove(ctx, DeleteParams{RootID: "etc", Path: "tree", Recursive: true})
	if err != nil {
		t.Fatalf("递归删除失败: %v", err)
	}
	// 2 个目录 + 2 个文件 = 4 项（含目录自己）。
	if treeResult.Entries != 4 {
		t.Fatalf("递归删除的条目数应当是 4，得到 %d", treeResult.Entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "tree")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("目录树还在: %v", err)
	}
}

// TestMoveRenamesWithinRoot 同根改名：旧名字消失、新名字有内容、目标已存在时拒绝。
func TestMoveRenamesWithinRoot(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.conf"), "a\n")
		writeFile(t, filepath.Join(dir, "b.conf"), "b\n")
	})
	ctx := context.Background()

	stat, err := m.Stat(ctx, "etc", "a.conf")
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if _, err := m.Move(ctx, CopyParams{
		SourceRootID: "etc", SourcePath: "a.conf",
		TargetRootID: "etc", TargetPath: "b.conf",
		ExpectedVersion: stat.Version,
	}); err == nil {
		t.Fatal("改名到已存在的目标应当被拒绝")
	}
	if got := mustFileContent(t, filepath.Join(dir, "b.conf")); got != "b\n" {
		t.Fatalf("被拒绝的改名改了目标: %q", got)
	}

	result, err := m.Move(ctx, CopyParams{
		SourceRootID: "etc", SourcePath: "a.conf",
		TargetRootID: "etc", TargetPath: "c.conf",
	})
	if err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if result.Version == "" {
		t.Fatalf("改名后应当读到目标的版本: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("改名之后旧名字还在")
	}
	if got := mustFileContent(t, filepath.Join(dir, "c.conf")); got != "a\n" {
		t.Fatalf("改名后内容不对: %q", got)
	}
}

// TestCopyIsIndependent 复制出来的是两份：改一份不影响另一份。
func TestCopyIsIndependent(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.conf"), "a=1\n")
	})
	ctx := context.Background()

	if _, err := m.Copy(ctx, CopyParams{
		SourceRootID: "etc", SourcePath: "a.conf",
		TargetRootID: "etc", TargetPath: "b.conf",
	}); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if got := mustFileContent(t, filepath.Join(dir, "b.conf")); got != "a=1\n" {
		t.Fatalf("复制内容不对: %q", got)
	}

	target, err := m.Stat(ctx, "etc", "b.conf")
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if _, err := m.WriteText(ctx, WriteTextParams{
		RootID: "etc", Path: "b.conf", ExpectedVersion: target.Version, Content: "b=2\n",
	}); err != nil {
		t.Fatalf("改副本失败: %v", err)
	}
	if got := mustFileContent(t, filepath.Join(dir, "a.conf")); got != "a=1\n" {
		t.Fatalf("改副本影响到了源文件: %q", got)
	}
}

// TestChmodChangesPlainBitsOnly 权限位改完读回核对；setuid 之类在参数层就被拒。
func TestChmodChangesPlainBitsOnly(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.conf"), "a\n")
	})
	ctx := context.Background()

	if _, err := m.Chmod(ctx, ChmodParams{
		RootID: "etc", Path: "a.conf", Mode: "4755",
	}); err == nil {
		t.Fatal("setuid 的权限位应当被拒绝")
	}

	result, err := m.Chmod(ctx, ChmodParams{RootID: "etc", Path: "a.conf", Mode: "0600"})
	if err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	if result.Mode != "0600" {
		t.Fatalf("读回的权限位是 %q", result.Mode)
	}
	info, err := os.Stat(filepath.Join(dir, "a.conf"))
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("磁盘上的权限位是 %v", info.Mode().Perm())
	}
}

// TestChownWithoutAccountsRefuses 账号模块没有时 chown 必须拒绝，而不是接受任意 uid。
func TestChownWithoutAccountsRefuses(t *testing.T) {
	m, _ := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.conf"), "a\n")
	})
	_, err := m.Chown(context.Background(), ChownParams{
		RootID: "etc", Path: "a.conf", ManagedAccountID: 1,
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("没有账号模块时 chown 应当报 ErrUnsupported，得到 %v", err)
	}
	if m.AccountsAvailable() {
		t.Fatal("AccountsAvailable 与 Options.Accounts 不一致")
	}
}

// TestReadOnlyRootRefusesWrites 只读根：读照旧，写一律 ErrNotWritable。
func TestReadOnlyRootRefusesWrites(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.conf"), "a\n")
	m, err := New([]Root{{ID: "etc", Name: "配置", Path: dir}}, Options{})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx := context.Background()

	if _, err := m.Text(ctx, TextParams{RootID: "etc", Path: "a.conf"}); err != nil {
		t.Fatalf("只读根应当能读: %v", err)
	}
	if _, err := m.Mkdir(ctx, MkdirParams{RootID: "etc", Path: "logs"}); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("只读根建目录应当报 ErrNotWritable，得到 %v", err)
	}
	if _, err := m.WriteText(ctx, WriteTextParams{
		RootID: "etc", Path: "b.conf", Content: "b",
	}); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("只读根写文本应当报 ErrNotWritable，得到 %v", err)
	}
}

// TestReservedDirIsInvisible 保留目录：列举里看不到，路径上不接受。
func TestReservedDirIsInvisible(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, ReservedDirName, stagingSubdir), 0o700); err != nil {
			t.Fatalf("建保留目录失败: %v", err)
		}
		writeFile(t, filepath.Join(dir, "a.conf"), "a\n")
	})
	ctx := context.Background()

	page := mustList(t, m, "")
	for _, name := range namesOf(page) {
		if name == ReservedDirName {
			t.Fatalf("列举里出现了保留目录: %v", namesOf(page))
		}
	}

	if _, err := m.Stat(ctx, "etc", ReservedDirName); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("Stat 保留目录应当报 ErrPathRejected，得到 %v", err)
	}
	if _, err := m.Text(ctx, TextParams{
		RootID: "etc", Path: ReservedDirName + "/staging/x.part",
	}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("读保留目录里的文件应当报 ErrPathRejected，得到 %v", err)
	}
	// 备注：保留目录本身确实在磁盘上（因为原子落位要求同文件系统），
	// 所以这里断言的是“看不见也进不去”，不是“不存在”。
	if _, err := os.Stat(filepath.Join(dir, ReservedDirName)); err != nil {
		t.Fatalf("保留目录应当在根内: %v", err)
	}
}

// backupFileNames 列出某个文件的备份文件名（按名字排序，即按时间排序）。
func backupFileNames(t *testing.T, dir, rel string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ReservedDirName, backupSubdir, backupKey(rel)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("读取备份目录失败: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestStatTreeCountsDirectories 目录树的统计必须真的走进去数。
//
// 这条用例盯的是一个具体的错误：用 O_PATH 打开目录再 readNames，getdents64 会返回
// EBADF，于是“统计一棵树”永远失败——预检把删除、复制目录统统拒掉，而失败原因看起来
// 像是路径有问题。列表里的数目（含子目录、符号链接、字节数）是唯一的证据。
func TestStatTreeCountsDirectories(t *testing.T) {
	m, _ := newWritableManager(t, func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, "tree", "sub"), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		writeFile(t, filepath.Join(dir, "tree", "app.conf"), "ab")
		writeFile(t, filepath.Join(dir, "tree", "notes.txt"), "c")
		if err := os.Symlink("app.conf", filepath.Join(dir, "tree", "link")); err != nil {
			t.Fatalf("建符号链接失败: %v", err)
		}
	})

	stat, err := m.StatTree(context.Background(), "etc", "tree")
	if err != nil {
		t.Fatalf("StatTree 失败: %v", err)
	}
	if stat.Type != TypeDir || !stat.InTree {
		t.Fatalf("目录树的形状不对: %+v", stat)
	}
	// 2 个文件 + 1 个目录 + 1 个符号链接。
	if stat.TreeEntries != 4 || stat.TreeFiles != 2 || stat.TreeDirs != 1 || stat.TreeSymlinks != 1 {
		t.Fatalf("目录树统计不对: entries=%d files=%d dirs=%d links=%d",
			stat.TreeEntries, stat.TreeFiles, stat.TreeDirs, stat.TreeSymlinks)
	}
	if stat.TreeBytes != 3 {
		t.Fatalf("目录树的字节数应当是 3，得到 %d", stat.TreeBytes)
	}
	if stat.TreeTruncated {
		t.Fatal("这么小的树不该被截断")
	}
}

// TestCopyDirectoryCopiesTree 目录复制：整棵树都落到目标，源一个字节不动。
func TestCopyDirectoryCopiesTree(t *testing.T) {
	m, dir := newWritableManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "src", "one.txt"), "1")
		writeFile(t, filepath.Join(dir, "src", "deep", "two.txt"), "22")
	})
	ctx := context.Background()

	result, err := m.Copy(ctx, CopyParams{
		SourceRootID: "etc", SourcePath: "src",
		TargetRootID: "etc", TargetPath: "copy",
	})
	if err != nil {
		t.Fatalf("复制目录失败: %v", err)
	}
	if result.Type != TypeDir || result.Version == "" {
		t.Fatalf("复制目录的结果不完整: %+v", result)
	}
	if got := mustFileContent(t, filepath.Join(dir, "copy", "one.txt")); got != "1" {
		t.Fatalf("复制后第一层内容不对: %q", got)
	}
	if got := mustFileContent(t, filepath.Join(dir, "copy", "deep", "two.txt")); got != "22" {
		t.Fatalf("复制后第二层内容不对: %q", got)
	}
	if got := mustFileContent(t, filepath.Join(dir, "src", "deep", "two.txt")); got != "22" {
		t.Fatalf("复制改了源: %q", got)
	}
	if left := stagingEntries(t, dir); len(left) != 0 {
		t.Fatalf("复制之后暂存目录还有残留: %v", left)
	}

	// 带符号链接的目录整棵拒绝：不跟进去（那会复制到根外），也不静默跳过（那会
	// 得到一棵“看起来复制成功、实际少了东西”的树）。
	if err := os.Symlink("one.txt", filepath.Join(dir, "src", "link")); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	if _, err := m.Copy(ctx, CopyParams{
		SourceRootID: "etc", SourcePath: "src",
		TargetRootID: "etc", TargetPath: "copy2",
	}); !errors.Is(err, ErrTreeUnsupported) {
		t.Fatalf("带符号链接的目录应当报 ErrTreeUnsupported，得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "copy2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("被拒绝的复制在磁盘上留下了目标")
	}
}

// TestBackupPruneKeepsTen 旧版本只留最近十份：备份目录不是无底洞。
//
// 这条用例盯的是另一个具体错误：备份目录的 fd 用 O_PATH 打开，pruneBackups 里的
// readNames 拿到 EBADF，清理“失败但不报错”——份数越堆越多，而结果里的
// backup_count 永远是 0，界面上写着“已保留备份”却数不出几份。
func TestBackupPruneKeepsTen(t *testing.T) {
	base := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	now := base
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.conf"), "v0\n")
	m, err := New([]Root{{ID: "etc", Name: "配置", Path: dir, Writable: true}},
		Options{Now: func() time.Time {
			// 每次取时间往前走一秒：备份名按纳秒排序，同一纳秒会撞上 O_EXCL，
			// 而“删掉最旧的几份”依赖的正是这个名字顺序。
			now = now.Add(time.Second)
			return now
		}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx := context.Background()

	writes := MaxEditBackups + 2
	var last WriteResult
	for i := 0; i < writes; i++ {
		stat, err := m.Stat(ctx, "etc", "app.conf")
		if err != nil {
			t.Fatalf("第 %d 次 Stat 失败: %v", i+1, err)
		}
		last, err = m.WriteText(ctx, WriteTextParams{
			RootID: "etc", Path: "app.conf", ExpectedVersion: stat.Version,
			Content: fmt.Sprintf("v%d\n", i+1),
		})
		if err != nil {
			t.Fatalf("第 %d 次覆盖失败: %v", i+1, err)
		}
	}

	names := backupFileNames(t, dir, "app.conf")
	if len(names) != MaxEditBackups {
		t.Fatalf("备份应当留 %d 份，磁盘上有 %d 份", MaxEditBackups, len(names))
	}
	if last.BackupCount != MaxEditBackups {
		t.Fatalf("结果里的备份份数应当是 %d，得到 %d", MaxEditBackups, last.BackupCount)
	}
	sort.Strings(names)
	// 12 次覆盖留下 v0..v11；最旧的两份（v0、v1）该被删掉，剩 v2..v11。
	want := make([]string, 0, MaxEditBackups)
	for i := 2; i < writes; i++ {
		want = append(want, fmt.Sprintf("v%d\n", i))
	}
	for i, name := range names {
		if got := mustFileContent(t, filepath.Join(dir, ReservedDirName, backupSubdir,
			backupKey("app.conf"), name)); got != want[i] {
			t.Fatalf("第 %d 份备份是 %q，期望 %q（应保留最近的十份）", i+1, got, want[i])
		}
	}
	if got := mustFileContent(t, filepath.Join(dir, last.BackupPath)); got != fmt.Sprintf("v%d\n", writes-1) {
		t.Fatalf("最新一份备份的内容不对: %q", got)
	}
}
