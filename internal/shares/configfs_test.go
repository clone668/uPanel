package shares

// 配置写入用的真实文件系统实现的测试（P08.3）。
//
// 这里只测一件事，但它是真机抓出来的：**文件权限不能由启动代理的那个 shell 的 umask 决定**。
// 生产的代理单元带 `UMask=0077`，于是 `os.OpenFile(..., 0o644)` 在真机上落成 0600，
// 而契约里写的是 0644——两边都没写错，只是权限被 umask 静默掩掉了，代码里看不出来。
// 这条测试把两种情形都钉住：新建（umask 掩掉 perm）与旧文件（上一版留下的更紧权限）。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileSyncEnforcesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 的权限位只有只读/可写两态，os.Chmod 也只动这一位；这条断言在那里没有意义。
		t.Skip("Windows 上不存在 POSIX 权限位，跳过")
	}
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.conf")
	if err := writeFileSync(fresh, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("写新文件失败: %v", err)
	}
	if got := modeOf(t, fresh); got != 0o644 {
		t.Fatalf("新文件权限是 %04o，期望 0644：perm 被 umask 掩掉了", got)
	}

	// 上一版代理留下的 0600 文件：再写一次必须被纠正，而不是保持原样。
	// “只在创建时生效”是这个缺陷的第二种形态：文件一旦落成 0600，之后每次重写都是 0600。
	stale := filepath.Join(dir, "stale.conf")
	if err := os.WriteFile(stale, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("造旧文件失败: %v", err)
	}
	if err := os.Chmod(stale, 0o600); err != nil {
		t.Fatalf("把旧文件改成 0600 失败: %v", err)
	}
	if err := writeFileSync(stale, []byte("new\n"), 0o644); err != nil {
		t.Fatalf("重写旧文件失败: %v", err)
	}
	if got := modeOf(t, stale); got != 0o644 {
		t.Fatalf("重写之后权限是 %04o，期望 0644：已有的更紧权限没有被纠正", got)
	}
	// 内容也换了（重写是整份覆盖，不是追加）。
	if data, err := os.ReadFile(stale); err != nil || string(data) != "new\n" {
		t.Fatalf("重写之后内容是 %q（err=%v）", data, err)
	}

	// 反过来同样要成立：本该 0600 的东西（备份、秘密）不会被这条改动放松。
	tight := filepath.Join(dir, "tight.conf")
	if err := writeFileSync(tight, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("写 0600 文件失败: %v", err)
	}
	if got := modeOf(t, tight); got != 0o600 {
		t.Fatalf("0600 的文件落成了 %04o", got)
	}
}

func modeOf(t *testing.T, name string) os.FileMode {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatalf("stat %s 失败: %v", name, err)
	}
	return info.Mode().Perm()
}
