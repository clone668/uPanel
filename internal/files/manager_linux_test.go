//go:build linux

package files

// P06.1 真实文件系统用例（只在 Linux 上跑）。
//
// 这一层是“不能逃逸”这句话唯一能落地的地方：它在一台真的 Linux 上创建真的
// 目录、符号链接、硬链接、FIFO 与超限文件，然后用真的 openat2 去打开它们。
//
// 在开发宿主（Windows）上这份文件不参与编译，因此下面这些用例的结论只能由
// 独立 Ubuntu 测试虚拟机给出：`GOOS=linux go test -c` 交叉编译后上传到
// upanel-2404 执行。哪个用例在 VM 上跑过、结果如何，记录在
// docs/evidence/P06/<日期>-p06.1/ 下——不是“应该能过”。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// newManager 在一个临时目录上登记一个根。t.TempDir 会自己清理。
func newManager(t *testing.T, mutate func(dir string)) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	if mutate != nil {
		mutate(dir)
	}
	m, err := New([]Root{{ID: "etc", Name: "配置", Path: dir}}, Options{})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return m, dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func mustList(t *testing.T, m *Manager, path string) ListPage {
	t.Helper()
	page, err := m.List(context.Background(), ListParams{RootID: "etc", Path: path})
	if err != nil {
		t.Fatalf("List(%q) 失败: %v", path, err)
	}
	return page
}

func namesOf(page ListPage) []string {
	out := make([]string, 0, len(page.Entries))
	for _, entry := range page.Entries {
		out = append(out, entry.Name)
	}
	return out
}

// TestListRoot 列举根目录：名字按字节序、类型来自真实的 lstat、版本可解析。
func TestListRoot(t *testing.T) {
	m, _ := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "b.txt"), "hello")
		writeFile(t, filepath.Join(dir, "sub", "c.txt"), "x")
		if err := os.Symlink("/etc/shadow", filepath.Join(dir, "a-link")); err != nil {
			t.Fatalf("建符号链接失败: %v", err)
		}
	})

	page := mustList(t, m, "")
	if page.RootID != "etc" || page.Path != "" {
		t.Fatalf("page = %+v", page)
	}
	if page.Truncated || page.NextCursor != nil {
		t.Fatalf("小目录不该被截断：%+v", page)
	}
	if got := namesOf(page); len(got) != 3 || got[0] != "a-link" || got[1] != "b.txt" || got[2] != "sub" {
		t.Fatalf("目录项 = %v", got)
	}
	byName := map[string]Entry{}
	for _, entry := range page.Entries {
		byName[entry.Name] = entry
	}
	if byName["b.txt"].Type != TypeFile || byName["b.txt"].SizeBytes != 5 {
		t.Fatalf("b.txt = %+v", byName["b.txt"])
	}
	if byName["sub"].Type != TypeDir {
		t.Fatalf("sub 类型 = %q", byName["sub"].Type)
	}
	// 符号链接就是符号链接：不报告目标、不报告目标的大小。
	if byName["a-link"].Type != TypeSymlink {
		t.Fatalf("a-link 类型 = %q", byName["a-link"].Type)
	}
	for _, entry := range page.Entries {
		if !strings.HasPrefix(entry.Version, "v1-") {
			t.Fatalf("%s 的 version = %q", entry.Name, entry.Version)
		}
		if len(entry.Mode) != 4 || !strings.HasPrefix(entry.Mode, "0") {
			t.Fatalf("%s 的 mode = %q（期望四位八进制，如 0755）", entry.Name, entry.Mode)
		}
	}
}

// TestListSubdir 子目录用根内相对路径列举。
func TestListSubdir(t *testing.T) {
	m, _ := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "sub", "c.txt"), "x")
	})
	page := mustList(t, m, "sub")
	if page.Path != "sub" || len(page.Entries) != 1 || page.Entries[0].Name != "c.txt" {
		t.Fatalf("page = %+v", page)
	}
}

// TestListCursorPaging 分页：页大小固定，游标是上一页最后一项，不重复不漏。
func TestListCursorPaging(t *testing.T) {
	const total = ListPageSize + 37
	m, _ := newManager(t, func(dir string) {
		for i := 0; i < total; i++ {
			writeFile(t, filepath.Join(dir, "f"+pad(i)), "x")
		}
	})

	first := mustList(t, m, "")
	if len(first.Entries) != ListPageSize {
		t.Fatalf("第一页 %d 项，期望 %d", len(first.Entries), ListPageSize)
	}
	if first.NextCursor == nil {
		t.Fatalf("还有更多项时 next_cursor 不能为 null")
	}
	if *first.NextCursor != first.Entries[len(first.Entries)-1].Name {
		t.Fatalf("游标 %q 不是最后一项 %q", *first.NextCursor, first.Entries[len(first.Entries)-1].Name)
	}

	second, err := m.List(context.Background(),
		ListParams{RootID: "etc", Path: "", Cursor: *first.NextCursor})
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	if len(second.Entries) != total-ListPageSize {
		t.Fatalf("第二页 %d 项，期望 %d", len(second.Entries), total-ListPageSize)
	}
	if second.NextCursor != nil {
		t.Fatalf("最后一页不该给游标：%q", *second.NextCursor)
	}
	// 两页拼起来必须是全部且无重复（游标指向的项不再返回）。
	all := append(namesOf(first), namesOf(second)...)
	sorted := append([]string(nil), all...)
	sort.Strings(sorted)
	if len(all) != total {
		t.Fatalf("总共 %d 项，期望 %d", len(all), total)
	}
	for i := 1; i < len(all); i++ {
		if all[i] == all[i-1] {
			t.Fatalf("重复项 %q", all[i])
		}
	}
}

func pad(i int) string {
	s := strconv.Itoa(i)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// TestListRejectsEscape 形状不合法与越界的路径在打开之前就被拒。
func TestListRejectsEscape(t *testing.T) {
	m, _ := newManager(t, nil)
	cases := []struct {
		path string
		want error
	}{
		{"../../etc", ErrPathRejected},
		{"/etc", ErrPathRejected},
		{".", ErrInvalidParams},
		{"sub/../../etc", ErrPathRejected},
	}
	for _, item := range cases {
		if _, err := m.List(context.Background(), ListParams{RootID: "etc", Path: item.path}); !errors.Is(err, item.want) {
			t.Fatalf("List(%q) = %v，期望 %v", item.path, err, item.want)
		}
	}
}

// TestSymlinkIsNeverFollowed 符号链接：列得出来、进不去、读不了。
//
// 这是验收里“符号链接替换无法逃逸”的直接用例：链接指向根外，但面板连跟随都不做。
func TestSymlinkIsNeverFollowed(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "root-only")

	m, dir := newManager(t, nil)
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "secret")); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}

	page := mustList(t, m, "")
	types := map[string]string{}
	for _, entry := range page.Entries {
		types[entry.Name] = entry.Type
	}
	if types["escape"] != TypeSymlink || types["secret"] != TypeSymlink {
		t.Fatalf("类型 = %v", types)
	}

	if _, err := m.List(context.Background(), ListParams{RootID: "etc", Path: "escape"}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("进入符号链接目录 = %v，期望 ErrPathRejected", err)
	}
	if _, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "secret"}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("读取符号链接 = %v，期望 ErrPathRejected", err)
	}
	if _, err := m.OpenDownload(context.Background(), DownloadParams{RootID: "etc", Path: "secret"}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("下载符号链接 = %v，期望 ErrPathRejected", err)
	}
}

// TestMagicLinkRejected /proc/*/fd 这类 magic link 不能成为跳板。
//
// 根内无法直接出现 magic link（它只存在于 procfs 里），所以现实的攻击形态是
// “根内放一个指向 /proc/self/fd/<n> 的符号链接”。RESOLVE_NO_SYMLINKS 先挡住它，
// RESOLVE_NO_MAGICLINKS 是第二道。（VM 验收脚本里另有直接用 /proc/self/fd 当根的用例。）
func TestMagicLinkRejected(t *testing.T) {
	m, dir := newManager(t, nil)
	if err := os.Symlink("/proc/self/fd", filepath.Join(dir, "fd")); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	if _, err := m.List(context.Background(), ListParams{RootID: "etc", Path: "fd"}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("进入 procfs magic link = %v，期望 ErrPathRejected", err)
	}
}

// TestTextReadsContent 文本读取：内容、大小、版本自洽；同版本再读一次仍然成功。
func TestTextReadsContent(t *testing.T) {
	m, _ := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "hostname"), "upanel\n")
	})

	text, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "hostname"})
	if err != nil {
		t.Fatalf("Text 失败: %v", err)
	}
	if text.Content != "upanel\n" || text.SizeBytes != 7 || text.Encoding != "utf-8" {
		t.Fatalf("text = %+v", text)
	}
	if text.Path != "hostname" || text.RootID != "etc" || !strings.HasPrefix(text.Version, "v1-") {
		t.Fatalf("text = %+v", text)
	}

	// 带同一版本再读：成功（版本核验不是一次性的）。
	again, err := m.Text(context.Background(), TextParams{
		RootID: "etc", Path: "hostname", ExpectedVersion: text.Version,
	})
	if err != nil {
		t.Fatalf("带正确版本重读失败: %v", err)
	}
	if again.Version != text.Version {
		t.Fatalf("版本变了: %q → %q", text.Version, again.Version)
	}
}

// TestTextVersionConflict 版本不一致必须是 VERSION_CONFLICT，而不是把新内容当旧的给出去。
func TestTextVersionConflict(t *testing.T) {
	m, dir := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "app.conf"), "v1\n")
	})
	first, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "app.conf"})
	if err != nil {
		t.Fatalf("Text 失败: %v", err)
	}

	// 内容与 mtime 都变（sleep 太慢，直接改内容 + 触发纳秒 mtime 变化）。
	writeFile(t, filepath.Join(dir, "app.conf"), "v2-longer\n")

	if _, err := m.Text(context.Background(), TextParams{
		RootID: "etc", Path: "app.conf", ExpectedVersion: first.Version,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧版本读取 = %v，期望 ErrVersionConflict", err)
	}
	second, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "app.conf"})
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if second.Version == first.Version {
		t.Fatalf("内容变了版本却没有变：%q", second.Version)
	}
	if second.Content != "v2-longer\n" {
		t.Fatalf("新内容 = %q", second.Content)
	}
}

// TestTextTooLarge 超过 1 MiB 的文件不读。
func TestTextTooLarge(t *testing.T) {
	m, _ := newManager(t, func(dir string) {
		big := strings.Repeat("a", MaxTextBytes+1)
		writeFile(t, filepath.Join(dir, "big.log"), big)
	})
	if _, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "big.log"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超限文件 = %v，期望 ErrTooLarge", err)
	}
}

// TestTextRejectsBinary 非法 UTF-8 与含 NUL 的内容都不是文本。
func TestTextRejectsBinary(t *testing.T) {
	m, dir := newManager(t, nil)
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte{0xff, 0xfe, 0x00}, 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "bin"}); !errors.Is(err, ErrNotText) {
		t.Fatalf("二进制 = %v，期望 ErrNotText", err)
	}
	// 合法 UTF-8 但含 NUL：浏览器会在 NUL 处截断，用户会以为文件就到那里。
	if err := os.WriteFile(filepath.Join(dir, "nul"), []byte("abc\x00def"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "nul"}); !errors.Is(err, ErrNotText) {
		t.Fatalf("含 NUL = %v，期望 ErrNotText", err)
	}
}

// TestHardLinkRejected nlink>1 的普通文件不读：它在根外可能还有名字。
func TestHardLinkRejected(t *testing.T) {
	m, dir := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.conf"), "shared\n")
	})
	if err := os.Link(filepath.Join(dir, "a.conf"), filepath.Join(dir, "b.conf")); err != nil {
		t.Fatalf("建硬链接失败: %v", err)
	}
	for _, name := range []string{"a.conf", "b.conf"} {
		if _, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: name}); !errors.Is(err, ErrHardLinked) {
			t.Fatalf("硬链接 %s = %v，期望 ErrHardLinked", name, err)
		}
	}
}

// TestSpecialFileRejected FIFO 这类特殊文件能列出但不读（读它会阻塞）。
func TestSpecialFileRejected(t *testing.T) {
	m, dir := newManager(t, nil)
	fifo := filepath.Join(dir, "pipe")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("建 FIFO 失败: %v", err)
	}

	page := mustList(t, m, "")
	if len(page.Entries) != 1 || page.Entries[0].Type != TypeOther {
		t.Fatalf("FIFO 类型 = %+v", page.Entries)
	}
	if _, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "pipe"}); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("FIFO 读取 = %v，期望 ErrNotRegular", err)
	}
}

// TestTextOnDirectoryRejected 目录不是普通文件。
func TestTextOnDirectoryRejected(t *testing.T) {
	m, _ := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "sub", "c.txt"), "x")
	})
	if _, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "sub"}); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("目录读取 = %v，期望 ErrNotRegular", err)
	}
}

// TestUnknownRoot 未登记的 root_id 是 NOT_FOUND，不会退化成“按路径找找看”。
func TestUnknownRoot(t *testing.T) {
	m, _ := newManager(t, nil)
	if _, err := m.List(context.Background(), ListParams{RootID: "home", Path: ""}); !errors.Is(err, ErrUnknownRoot) {
		t.Fatalf("未登记根 = %v，期望 ErrUnknownRoot", err)
	}
	if _, err := m.Text(context.Background(), TextParams{RootID: "home", Path: "x"}); !errors.Is(err, ErrUnknownRoot) {
		t.Fatalf("未登记根 = %v，期望 ErrUnknownRoot", err)
	}
}

// TestRootIdentityChangeIsUnavailable 根被删掉重建（或换挂载）之后不可用。
//
// 这是“根身份”这条防线唯一能被观察到的行为：目录还在、权限也对、路径同名，
// 但 inode 变了——面板拒绝继续，而不是把另一个目录当成原来那个。
func TestRootIdentityChangeIsUnavailable(t *testing.T) {
	m, dir := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.conf"), "x")
	})

	if _, err := m.List(context.Background(), ListParams{RootID: "etc", Path: ""}); err != nil {
		t.Fatalf("重建之前应可列举: %v", err)
	}

	// 用 rename 换掉根，而不是“删掉再 mkdir 同名目录”：文件系统会把刚释放的
	// inode 立刻复用给下一个新目录（VM 上实测如此），那样 dev/inode 与登记时一致，
	// 这条用例就成了假通过。因此 decoy 必须在删除之前建好——它的 inode 一定与
	// 原来的根不同，`mv` 只是把那个 inode 换个名字。
	decoy := t.TempDir()
	writeFile(t, filepath.Join(decoy, "impostor.txt"), "impostor\n")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("删除根失败: %v", err)
	}
	if err := os.Rename(decoy, dir); err != nil {
		t.Fatalf("把 decoy 换到根位置失败: %v", err)
	}

	if _, err := m.List(context.Background(), ListParams{RootID: "etc", Path: ""}); !errors.Is(err, ErrRootUnavailable) {
		t.Fatalf("重建之后的根 = %v，期望 ErrRootUnavailable", err)
	}
	roots := m.Roots(context.Background())
	if len(roots.Roots) != 1 {
		t.Fatalf("roots = %+v", roots.Roots)
	}
	if roots.Roots[0].Available {
		t.Fatalf("被换掉的根仍然显示为可用")
	}
	if roots.Roots[0].UnavailableReason == "" {
		t.Fatalf("不可用的根必须说明原因")
	}
}

// TestRootPathMustBeCleanAbsoluteNew 登记的路径必须是规范的绝对路径，且不能是 "/"。
//
// 最后一条直接对应验收里的“文件 UI 不能显示可管理整个服务器所有目录”。
func TestRootPathMustBeCleanAbsoluteNew(t *testing.T) {
	for _, rootPath := range []string{"etc", "relative/path", "/etc/../etc", "/"} {
		if _, err := New([]Root{{ID: "x", Path: rootPath}}, Options{}); err == nil {
			t.Fatalf("根路径 %q 应被拒", rootPath)
		}
	}
	if _, err := New([]Root{{ID: "", Path: "/etc"}}, Options{}); err == nil {
		t.Fatalf("缺少 id 的根应被拒")
	}
	if _, err := New([]Root{
		{ID: "a", Path: "/etc"},
		{ID: "a", Path: "/var"},
	}, Options{}); err == nil {
		t.Fatalf("重复 id 应被拒")
	}
}

// TestMissingRootIsReportedNotFatal 登记时就不存在的目录：代理照样启动，这个根不可用。
func TestMissingRootIsReportedNotFatal(t *testing.T) {
	m, err := New([]Root{{ID: "gone", Name: "gone", Path: "/nonexistent-upanel-test"}}, Options{})
	if err != nil {
		t.Fatalf("单个根不可用不该让 New 失败: %v", err)
	}
	roots := m.Roots(context.Background())
	if len(roots.Roots) != 1 || roots.Roots[0].Available {
		t.Fatalf("roots = %+v", roots.Roots)
	}
	if roots.Roots[0].UnavailableReason == "" {
		t.Fatalf("缺少不可用原因")
	}
	if _, err := m.List(context.Background(), ListParams{RootID: "gone", Path: ""}); !errors.Is(err, ErrRootUnavailable) {
		t.Fatalf("不可用根的列举 = %v", err)
	}
}

// TestRootsReportsMountInfo 根列表带上挂载点与文件系统类型（容量判定的依据）。
func TestRootsReportsMountInfo(t *testing.T) {
	m, _ := newManager(t, nil)
	roots := m.Roots(context.Background())
	if len(roots.Roots) != 1 {
		t.Fatalf("roots = %+v", roots.Roots)
	}
	info := roots.Roots[0]
	if !info.Available {
		t.Fatalf("根不可用: %s", info.UnavailableReason)
	}
	if info.MountPoint == "" || info.FSType == "" {
		t.Fatalf("缺少挂载信息: %+v", info)
	}
	// 没有接容量读取器时 capacity 必须是 null + 原因，不能是 0。
	if info.Capacity != nil {
		t.Fatalf("capacity = %+v，期望 nil", info.Capacity)
	}
	if info.CapacityError == "" {
		t.Fatalf("capacity 为 nil 时必须说明原因")
	}
}

// TestDownloadHeaderAndBytes 下载：首行元数据与句柄字节自洽。
func TestDownloadHeaderAndBytes(t *testing.T) {
	content := "Port 22\n"
	m, _ := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "ssh", "sshd_config"), content)
	})

	dl, err := m.OpenDownload(context.Background(), DownloadParams{RootID: "etc", Path: "ssh/sshd_config"})
	if err != nil {
		t.Fatalf("OpenDownload 失败: %v", err)
	}
	defer func() { _ = dl.Close() }()

	if dl.Header.Name != "sshd_config" || dl.Header.SizeBytes != uint64(len(content)) {
		t.Fatalf("header = %+v", dl.Header)
	}
	if !strings.HasPrefix(dl.Header.Version, "v1-") {
		t.Fatalf("version = %q", dl.Header.Version)
	}

	// 版本必须与文本读取时一致：同一个 inode、同一份内容就该是同一个版本串。
	text, err := m.Text(context.Background(), TextParams{RootID: "etc", Path: "ssh/sshd_config"})
	if err != nil {
		t.Fatalf("Text 失败: %v", err)
	}
	if text.Version != dl.Header.Version {
		t.Fatalf("下载与读取的版本不一致: %q vs %q", dl.Header.Version, text.Version)
	}

	buf := make([]byte, len(content))
	if _, err := dl.File.Read(buf); err != nil {
		t.Fatalf("读下载句柄失败: %v", err)
	}
	if string(buf) != content {
		t.Fatalf("字节 = %q", string(buf))
	}
}

// TestDownloadRejectsRestricted 下载同样拒绝越界、目录、特殊文件与版本冲突。
func TestDownloadRejectsRestricted(t *testing.T) {
	content := "abc\n"
	m, dir := newManager(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "a.txt"), content)
	})
	_ = dir

	if _, err := m.OpenDownload(context.Background(), DownloadParams{RootID: "etc", Path: "../x"}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("越界下载 = %v", err)
	}
	if _, err := m.OpenDownload(context.Background(), DownloadParams{RootID: "etc", Path: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在 = %v", err)
	}

	first, err := m.OpenDownload(context.Background(), DownloadParams{RootID: "etc", Path: "a.txt"})
	if err != nil {
		t.Fatalf("OpenDownload 失败: %v", err)
	}
	version := first.Header.Version
	_ = first.Close()

	writeFile(t, filepath.Join(dir, "a.txt"), "abcdef\n")
	if _, err := m.OpenDownload(context.Background(), DownloadParams{
		RootID: "etc", Path: "a.txt", ExpectedVersion: version,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧版本下载 = %v，期望 ErrVersionConflict", err)
	}
}

// TestListTruncatedOnHugeDirectory MaxListScan 之上如实标记 truncated。
//
// 5 万个文件在这个用例里太慢，因此用清单本身的上限做判断：
// 只要目录项少于上限，truncated 就必须是 false（不谎报），
// “真的超过上限”的行为由 VM 验收脚本里的大目录用例覆盖。
func TestListTruncatedOnHugeDirectory(t *testing.T) {
	m, dir := newManager(t, nil)
	for i := 0; i < 10; i++ {
		writeFile(t, filepath.Join(dir, "f"+pad(i)), "x")
	}
	page := mustList(t, m, "")
	if page.Truncated {
		t.Fatalf("10 项的目录被标成 truncated")
	}
}

// TestNestedMountRejectedByXDEV 根内出现另一个挂载点时，RESOLVE_NO_XDEV 让它不可达。
//
// 这个用例需要 mount 权限，所以在开发机上跳过；它在 VM 验收脚本里以
// `mount -t tmpfs tmpfs <root>/submount` 的形式真的跑一次。
func TestNestedMountRejectedByXDEV(t *testing.T) {
	t.Skip("需要 root 执行 mount；由 tests/integration 的 P06.1 验收脚本在 VM 上覆盖")
}
