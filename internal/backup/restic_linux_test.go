//go:build linux

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// 这不是 fakeRunner：只有 Ubuntu 上安装的真实 restic 才能跑。它专门钉住
// O_CLOEXEC 暂存目录经 ExtraFiles 传给 restic 的产品路径，而不是只检查 argv。
func TestResticRestoreThroughInheritedDirectoryFD(t *testing.T) {
	if _, err := os.Stat("/usr/bin/restic"); err != nil {
		t.Skip("真实 restic 未安装")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("restic inherited fd restore\n")
	if err := os.WriteFile(filepath.Join(source, "payload.txt"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRestic(Paths{Restic: "/usr/bin/restic", CacheDir: filepath.Join(base, "cache")}, nil)
	inv := Invocation{Repository: filepath.Join(base, "repository"), Password: "linux-restic-test", Timeout: time.Minute}
	if _, err := r.Init(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Backup(ctx, inv, BackupOptions{Paths: []string{source}, Timeout: time.Minute}, nil); err != nil {
		t.Fatal(err)
	}
	snapshots, err := r.Snapshots(ctx, inv, nil)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("快照读回失败: %d, %v", len(snapshots), err)
	}
	if err := os.Mkdir(filepath.Join(base, "stage"), 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(filepath.Join(base, "stage"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := r.Restore(ctx, inv, snapshots[0].ID, "/proc/self/fd/"+strconv.Itoa(fd), nil); err != nil {
		t.Fatal(err)
	}
	// 长恢复会在发布前触发 GC。原实现把原 fd 包进临时 os.File，回收包装
	// 对象时把父会话也关了；这里强制回收后原描述符仍必须有效。
	debug.FreeOSMemory()
	var stageStat unix.Stat_t
	if err := unix.Fstat(fd, &stageStat); err != nil {
		t.Fatalf("restic 返回并触发 GC 后原恢复目录句柄已失效: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(base, "stage", source, "payload.txt"))
	if err != nil || string(got) != string(want) {
		t.Fatalf("经继承 fd 恢复的内容不一致: %q, %v", got, err)
	}
	nodes, err := r.ListNodes(ctx, inv, snapshots[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	files, _, _, err := validateSnapshotNodes(nodes)
	if err != nil || files != 1 {
		t.Fatalf("真实 restic ls 节点不合法: files=%d nodes=%+v err=%v", files, nodes, err)
	}
	sum, size, err := r.DumpSHA256(ctx, inv, snapshots[0].ID, source+"/payload.txt")
	wantSum := sha256.Sum256(want)
	if err != nil || size != int64(len(want)) || sum != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("真实 restic dump 校验失败: sum=%s size=%d err=%v", sum, size, err)
	}
	if err := r.Check(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := r.ForgetPrune(ctx, inv, []string{snapshots[0].ID}); err != nil {
		t.Fatal(err)
	}
	remaining, err := r.Snapshots(ctx, inv, nil)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("真实 restic forget --prune 后快照仍存在: %+v, %v", remaining, err)
	}
}
