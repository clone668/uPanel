package main

// 证书续期计划的目标解析（P12.5）。
//
// 判的是盘：/etc/letsencrypt/live/<domain> 在不在。三种结论对应三种现实——
// 在（能跑）、不在（这条计划已经没有意义）、读不出来（不是"没有"，是错，要报上去）。
// 单测不改宿主机的 /etc，替换 certificateLiveDir 与 certificateLiveStat 两处即可。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"upanel/internal/schedules"
)

func certDefinition(t *testing.T, domain string) schedules.Definition {
	t.Helper()
	def := schedules.Definition{
		Kind:        schedules.KindCertificateRenewal,
		TargetID:    "crt_0123456789abcdef0123456789abcdef",
		Certificate: &schedules.CertificateParams{Domain: domain},
	}
	return def
}

func withLiveDir(t *testing.T, dir string) {
	t.Helper()
	old := certificateLiveDir
	certificateLiveDir = dir
	t.Cleanup(func() { certificateLiveDir = old })
}

func TestCertificateTargetReadyWhenLiveDirExists(t *testing.T) {
	root := t.TempDir()
	withLiveDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, "panel.example.com"), 0o755); err != nil {
		t.Fatalf("建 live 目录: %v", err)
	}
	lookup := scheduleTargetLookup(nil, nil)
	target, found, err := lookup(context.Background(), certDefinition(t, "panel.example.com"))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found {
		t.Fatal("found = false，期望 true：域名叫得出来就说明这条定义有目标")
	}
	if !target.Ready {
		t.Fatalf("Ready = false，Note = %q", target.Note)
	}
	if target.Name != "panel.example.com" || target.Version <= 0 {
		t.Fatalf("target = %+v", target)
	}
}

func TestCertificateTargetNotReadyWhenLiveDirMissing(t *testing.T) {
	withLiveDir(t, t.TempDir())
	lookup := scheduleTargetLookup(nil, nil)
	target, found, err := lookup(context.Background(), certDefinition(t, "panel.example.com"))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found {
		t.Fatal("found = false：证书不在盘上不等于计划没有目标，应当报 NOT_READY 而不是 NOT_FOUND")
	}
	if target.Ready {
		t.Fatal("Ready = true，期望 false")
	}
	if !strings.Contains(target.Note, "panel.example.com") {
		t.Fatalf("Note 里没有域名，管理员看不出是哪张证书: %q", target.Note)
	}
}

func TestCertificateTargetNotReadyWhenPathIsFile(t *testing.T) {
	root := t.TempDir()
	withLiveDir(t, root)
	// 同名文件占位：certbot 写不进去，续期必然失败。这与"没有证书"是两件事，
	// 说法也要不一样——一个叫管理员去清障碍，一个叫他别再续这条。
	if err := os.WriteFile(filepath.Join(root, "panel.example.com"), []byte("x"), 0o644); err != nil {
		t.Fatalf("占位文件: %v", err)
	}
	lookup := scheduleTargetLookup(nil, nil)
	target, found, err := lookup(context.Background(), certDefinition(t, "panel.example.com"))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found || target.Ready {
		t.Fatalf("found = %v, Ready = %v", found, target.Ready)
	}
	if !strings.Contains(target.Note, "不是目录") {
		t.Fatalf("Note = %q", target.Note)
	}
}

func TestCertificateTargetReportsStatError(t *testing.T) {
	withLiveDir(t, filepath.Join(t.TempDir(), "le"))
	want := errors.New("permission denied")
	old := certificateLiveStat
	certificateLiveStat = func(string) (os.FileInfo, error) { return nil, want }
	t.Cleanup(func() { certificateLiveStat = old })

	lookup := scheduleTargetLookup(nil, nil)
	target, found, err := lookup(context.Background(), certDefinition(t, "panel.example.com"))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v，期望把真实读盘错误交上去（伪装成'证书不在'会让管理员去查错方向）", err)
	}
	if found || target.Ready {
		t.Fatalf("found = %v, target = %+v", found, target)
	}
}

func TestCertificateTargetWithoutDomain(t *testing.T) {
	withLiveDir(t, t.TempDir())
	lookup := scheduleTargetLookup(nil, nil)
	// 库里躺着一条没有域名参数的老定义：判不了盘，只能报找不到。
	def := schedules.Definition{Kind: schedules.KindCertificateRenewal, TargetID: "crt_0123456789abcdef0123456789abcdef"}
	if _, found, err := lookup(context.Background(), def); err != nil || found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
}

func TestCertificateTargetIgnoresFileNotExistWrapping(t *testing.T) {
	// fs.ErrNotExist 可能被包在 PathError 里；errors.Is 必须仍然认得出来，
	// 否则一条指向已删证书的计划会变成"读盘失败"。
	withLiveDir(t, t.TempDir())
	old := certificateLiveStat
	certificateLiveStat = func(path string) (os.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	t.Cleanup(func() { certificateLiveStat = old })

	_, found, err := scheduleTargetLookup(nil, nil)(context.Background(), certDefinition(t, "panel.example.com"))
	if err != nil || !found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
}
