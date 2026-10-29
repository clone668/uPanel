//go:build linux

package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRestorePublishesOnlyVerifiedNewDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("恢复所有权映射的生产身份是 root")
	}
	m, root := newWritableManager(t, nil)
	ctx := context.Background()
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := m.BeginRestore(ctx, RestoreParams{RootID: "etc", Path: "restored"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Abort(context.Background()) }()
	if err := os.MkdirAll(filepath.Join(session.Target(), "srv", "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.Target(), "srv", "app", "a.txt"), []byte("new bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Ubuntu 22.04/24.04 的 restic 会恢复归档 uid/gid；发布前必须归一，而不是依赖
	// 这两个版本并不提供的 --no-owner。
	if err := os.Chown(filepath.Join(session.Target(), "srv"), 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(session.Target(), "srv", "app", "a.txt"), 65534, 65534); err != nil {
		t.Fatal(err)
	}
	entries, err := session.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("恢复清单应为两个目录一个文件，实际 %d", len(entries))
	}
	receipt, err := session.Publish(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.VerifiedAt.IsZero() {
		t.Fatal("发布时间缺失")
	}
	got, err := os.ReadFile(filepath.Join(root, "restored", "srv", "app", "a.txt"))
	if err != nil || string(got) != "new bytes" {
		t.Fatalf("发布后内容 = %q, %v", got, err)
	}
	st, err := os.Stat(filepath.Join(root, "restored", "srv", "app", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Sys().(*syscall.Stat_t).Uid != 0 || st.Sys().(*syscall.Stat_t).Gid != 0 {
		t.Fatalf("归档所有权不应发布到宿主: stat=%v", st.Sys())
	}
	outsideGot, err := os.ReadFile(outside)
	if err != nil || string(outsideGot) != "unchanged" {
		t.Fatalf("范围外文件被改变: %q, %v", outsideGot, err)
	}
}

func TestOverwriteRestoreAtomicallyReplacesWholeDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("恢复所有权映射的生产身份是 root")
	}
	m, root := newWritableManager(t, nil)
	ctx := context.Background()
	target := filepath.Join(root, "app")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := m.BeginRestore(ctx, RestoreParams{RootID: "etc", Path: "app", Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Abort(context.Background()) }()
	if err := os.WriteFile(filepath.Join(session.Target(), "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := session.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Publish(ctx, entries); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("旧目录不应混入新目录: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "new.txt")); err != nil || string(got) != "new" {
		t.Fatalf("新内容 = %q, %v", got, err)
	}
}

func TestRestoreRejectsSymlinkAndLeavesOriginalUntouched(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("恢复所有权映射的生产身份是 root")
	}
	m, root := newWritableManager(t, nil)
	ctx := context.Background()
	target := filepath.Join(root, "app")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := m.BeginRestore(ctx, RestoreParams{RootID: "etc", Path: "app", Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(session.Target(), "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Inspect(ctx); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("符号链接应被拒绝: %v", err)
	}
	if err := session.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "old.txt"))
	if err != nil || string(got) != "old" {
		t.Fatalf("失败不应改变原目录: %q, %v", got, err)
	}
}

func TestRestoreRejectsUnsafeMetadataBeforePublish(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("特殊文件与 setuid 夹具需要 root")
	}
	for _, tc := range []struct {
		name  string
		plant func(*testing.T, string)
	}{
		{name: "setuid", plant: func(t *testing.T, stage string) {
			name := filepath.Join(stage, "setuid")
			if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Chmod(name, 0o4755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", plant: func(t *testing.T, stage string) {
			if err := syscall.Mkfifo(filepath.Join(stage, "fifo"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", plant: func(t *testing.T, stage string) {
			first := filepath.Join(stage, "first")
			if err := os.WriteFile(first, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(first, filepath.Join(stage, "second")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, root := newWritableManager(t, nil)
			target := filepath.Join(root, "app")
			if err := os.Mkdir(target, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			session, err := m.BeginRestore(context.Background(), RestoreParams{RootID: "etc", Path: "app", Overwrite: true})
			if err != nil {
				t.Fatal(err)
			}
			tc.plant(t, session.Target())
			if _, err := session.Inspect(context.Background()); err == nil {
				t.Fatal("危险元数据不应通过发布前检查")
			}
			if err := session.Abort(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(target, "old.txt"))
			if err != nil || string(got) != "old" {
				t.Fatalf("拒绝后原目录改变: %q, %v", got, err)
			}
		})
	}
}

func TestRestorePublishMismatchRollsBackOriginal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("恢复所有权映射的生产身份是 root")
	}
	m, root := newWritableManager(t, nil)
	target := filepath.Join(root, "app")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := m.BeginRestore(context.Background(), RestoreParams{RootID: "etc", Path: "app", Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.Target(), "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := session.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entries[0].SHA256 = "deliberate-post-publish-mismatch"
	if _, err := session.Publish(context.Background(), entries); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("发布后清单不符应触发回滚: %v", err)
	}
	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "old.txt"))
	if err != nil || string(got) != "old" {
		t.Fatalf("回滚后原目录内容 = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(target, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("未通过复核的新目录不应保留: %v", err)
	}
}

func TestDefaultRestoreRejectsNonemptyTarget(t *testing.T) {
	m, root := newWritableManager(t, nil)
	if err := os.Mkdir(filepath.Join(root, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginRestore(context.Background(), RestoreParams{RootID: "etc", Path: "app"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("默认恢复非空目录应拒绝: %v", err)
	}
}
