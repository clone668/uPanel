package backup

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// restoreTarget 给出一个「像 files 层交出来的」暂存目录 target。
//
// 产品在 Unix 上会把 `/proc/self/fd/N` 里的 N **复制一份**交给子进程（`dupRestoreFD`），
// 所以这个 N 必须是真开着的描述符；Windows 上 `dupRestoreFD` 是桩，字符串只走个过场。
// 原先这里硬编码 `/proc/self/fd/8`——Windows 上照样过（桩不碰 fd），Linux 上必然 EBADF。
// 这条用例只在 Windows 上被跑过，直到 2026-10-28 的 P16 全量 `-race` 第一次在 Linux 上
// 执行它才暴露（Q518）。
func restoreTarget(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "/proc/self/fd/8"
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开暂存目录失败: %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	return "/proc/self/fd/" + strconv.Itoa(int(dir.Fd()))
}

func retentionSnapshot(id byte, at time.Time) Snapshot {
	return Snapshot{ID: strings.Repeat(string(id), 64), Time: at}
}

func TestSelectRetentionUsesUnionOfRules(t *testing.T) {
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	snaps := []Snapshot{
		retentionSnapshot('a', base),
		retentionSnapshot('b', base.Add(-2*time.Hour)), // same day, not keep_last
		retentionSnapshot('c', base.AddDate(0, 0, -1)),
		retentionSnapshot('d', base.AddDate(0, 0, -2)),
		retentionSnapshot('e', base.AddDate(0, 0, -8)),
	}
	selection, err := SelectRetention(snaps, Retention{KeepLast: 1, KeepDaily: 2, KeepWeekly: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := snapshotIDs(selection.Keep)
	want := []string{strings.Repeat("a", 64), strings.Repeat("c", 64), strings.Repeat("e", 64)}
	if !equalStrings(got, want) {
		t.Fatalf("保留集合 = %v，想要 %v", got, want)
	}
	if len(selection.Delete) != 2 {
		t.Fatalf("待删应为 2，实际 %d", len(selection.Delete))
	}
}

func TestSelectRetentionRejectsUnconfiguredOrInvalidRules(t *testing.T) {
	for _, retention := range []Retention{{}, {KeepLast: -1}, {KeepDaily: MaxRetentionCount + 1}} {
		if _, err := SelectRetention(nil, retention); err == nil {
			t.Fatalf("规则 %+v 应被拒绝", retention)
		}
	}
}

func TestResticRecoveryArgumentsAreFixedAndLiteral(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var specs []ExecSpec
	r := NewRestic(Paths{Restic: "/usr/bin/restic"}, func(_ context.Context, spec ExecSpec) (ExecResult, error) {
		specs = append(specs, spec)
		if spec.StdoutWriter != nil {
			_, _ = spec.StdoutWriter.Write([]byte("payload"))
		}
		return ExecResult{Program: spec.Program}, nil
	})
	inv := Invocation{Repository: "/srv/repo", Password: "secret"}
	if err := r.Restore(context.Background(), inv, id, restoreTarget(t), []string{"/srv/a*?[x].txt"}); err != nil {
		t.Fatal(err)
	}
	restoreArgs := strings.Join(specs[0].Args, " ")
	for _, forbidden := range []string{"secret", "--include /srv/a*?[x].txt"} {
		if strings.Contains(restoreArgs, forbidden) {
			t.Fatalf("argv 含秘密或未转义通配符: %s", restoreArgs)
		}
	}
	if !strings.Contains(restoreArgs, `--include /srv/a\*\?\[x].txt`) || strings.Contains(restoreArgs, "--no-owner") ||
		!strings.Contains(restoreArgs, "--target /proc/self/fd/3") || len(specs[0].ExtraFiles) != 1 {
		t.Fatalf("恢复 argv 或显式 fd 传递不符合固定边界: %s, files=%d", restoreArgs, len(specs[0].ExtraFiles))
	}
	sum, size, err := r.DumpSHA256(context.Background(), inv, id, "/srv/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if size != 7 || sum != "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5" {
		t.Fatalf("dump 流式结果 = %q/%d", sum, size)
	}
	if err := r.ForgetPrune(context.Background(), inv, []string{id}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(specs[len(specs)-1].Args, " ")
	if !strings.Contains(joined, "forget --prune "+id) || strings.Contains(joined, "secret") {
		t.Fatalf("清理 argv 不正确: %s", joined)
	}
}

func TestSelectVerifySampleEnforcesCaps(t *testing.T) {
	nodes := []RestoreNode{{Path: "/large", Type: "file", Size: VerifySampleMaxFileBytes + 1}}
	for i := 0; i < VerifySampleMaxFiles+2; i++ {
		nodes = append(nodes, RestoreNode{Path: string(rune('a' + i)), Type: "file", Size: 4 << 20})
	}
	selected := selectVerifySample(nodes)
	if len(selected) > VerifySampleMaxFiles {
		t.Fatalf("抽样文件数超限: %d", len(selected))
	}
	var total int64
	for _, n := range selected {
		total += n.Size
	}
	if total > VerifySampleMaxTotalBytes {
		t.Fatalf("抽样字节超限: %d", total)
	}
}
