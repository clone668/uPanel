package certificates

// HTTP-01 可达性探测的目录权限（P10.4）。
//
// 这一条是被**装好的 systemd 单元**抓出来的，不是被单元测试抓出来的：代理单元带
// `UMask=0077`，于是 `MkdirAll(dir, 0o755)` 在真机上落成 `0700 root`，Nginx 的 worker
// （www-data）进不去校验目录，CA 来取文件时拿到 403。P10.3 之所以是绿的，是因为
// 那时代理是手工以 root 起的（umask 022）——同一个程序，两种启动方式，两种结果。
//
// 所以这里不用假文件系统测：假 FS 不受 umask 影响，测了也证明不了什么。测试真的把
// 进程 umask 调成 0077，再用真实实现走一遍，断言三级目录都是 0755（Q243 的同一条
// 教训：权限由代码决定，不由启动它的 shell 决定）。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"context"
)

func TestProbeHTTP01EnforcesChallengeDirMode(t *testing.T) {
	webroot := filepath.Join(t.TempDir(), "acme")
	// 真实地降低 umask：这正是生产单元的行为。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上不存在 POSIX 权限位，umask 也没有意义")
	}
	old := setUmaskForTest(0o077)
	defer setUmaskForTest(old)

	// 直接构造 Ops：这条测试只关心文件权限，不需要秘密存储与命令执行端。
	ops := &Ops{opts: OpsOptions{
		FS:      SystemMaterialFS(),
		Webroot: webroot,
		// 探测端不做网络请求：它读的正是刚写下的那个文件，顺便证明写进去的内容
		// 就是 URL 里那一段（现实中由 Nginx 从同一个目录回答）。
		HTTPGet: func(_ context.Context, url string) (int, string, error) {
			name := url[strings.LastIndex(url, "/")+1:]
			data, readErr := os.ReadFile(filepath.Join(webroot, acmeChallengeDir, name))
			if readErr != nil {
				return 404, "", readErr
			}
			return 200, string(data), nil
		},
	}}

	status, err := ops.probeHTTP01(context.Background(), "p104.example.test")
	if err != nil {
		t.Fatalf("探测失败（umask 0077 下校验目录不可进入）: %v", err)
	}
	if status != 200 {
		t.Fatalf("状态码是 %d，期望 200", status)
	}

	for _, dir := range []string{
		webroot,
		filepath.Join(webroot, ".well-known"),
		filepath.Join(webroot, acmeChallengeDir),
	} {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatalf("目录 %s 不存在: %v", dir, statErr)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Errorf("%s 的权限是 %04o，期望 0755：umask 0077 把 perm 掩掉了，"+
				"Nginx 的 worker 会进不去校验目录", dir, got)
		}
	}

	// 探测文件本身必须是可读的（Nginx 不是它的属主），且用完就删掉。
	if entries, readErr := os.ReadDir(filepath.Join(webroot, acmeChallengeDir)); readErr == nil {
		if len(entries) != 0 {
			t.Errorf("探测结束后校验目录里还留着 %d 个文件（应当删掉自己写的那个）", len(entries))
		}
	}
}
