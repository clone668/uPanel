package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ActionRestore = "backup.restore"
	ActionVerify  = "backup.verify"
	ActionPrune   = "backup.prune"

	RestoreTimeout = 6 * time.Hour
	VerifyTimeout  = 30 * time.Minute
	PruneTimeout   = 6 * time.Hour

	VerifySampleMaxFiles            = 8
	VerifySampleMaxFileBytes  int64 = 8 << 20
	VerifySampleMaxTotalBytes int64 = 32 << 20
)

// RestoreFilesystem 是 internal/files 的句柄级恢复能力，不是一条字符串路径写接口。
type RestoreFilesystem interface {
	ProbeRestore(context.Context, RestoreParams) (RestoreProbe, error)
	BeginRestore(context.Context, RestoreParams) (RestoreSession, error)
}

type RestoreVerification struct {
	RepositoryID string
	SnapshotID   string
	VerifiedAt   time.Time
	FilesChecked int
	BytesChecked int64
}

type VerificationStore interface {
	MarkRestoreVerified(context.Context, RestoreVerification) error
	RestoreVerification(context.Context, string, string) (RestoreVerification, bool, error)
}

type RestoreRequest struct {
	RepositoryID string
	Repository   Repository
	SnapshotID   string
	TargetRootID string
	RelativePath string
	Overwrite    bool
	Apps         []AppTarget
	Controller   AppController
	Cancel       <-chan struct{}
	Progress     func(string, map[string]any)
}

type RestoreAssessment struct {
	Allowed   bool
	Blockers  []Reason
	Warnings  []Reason
	Impacts   []Impact
	Locks     []string
	Target    Target
	Snapshot  Snapshot
	NodeCount int
	Bytes     int64
	Apps      []AppRuntime
}

type RestoreResult struct {
	RepositoryID string
	SnapshotID   string
	TargetRootID string
	RelativePath string
	Overwrite    bool
	Published    bool
	Verified     bool
	Files        int
	Directories  int
	Bytes        int64
	FinishedAt   time.Time
	Apps         []AppAction
	AppsRestored bool
	Warnings     []string
	CleanupError string
}

func (o *Ops) invocationFor(ctx context.Context, repo Repository, timeout time.Duration) (Invocation, func(), Definition, error) {
	def, err := repo.Definition.Normalize()
	if err != nil {
		return Invocation{}, nil, Definition{}, err
	}
	inv, cleanup, err := o.readInvocation(ctx, def)
	if err != nil {
		return Invocation{}, nil, Definition{}, err
	}
	inv.Timeout = timeout
	return inv, cleanup, def, nil
}

func validateSnapshotNodes(nodes []RestoreNode) (filesCount, dirsCount int, bytes int64, err error) {
	for _, n := range nodes {
		switch n.Type {
		case "file":
			if n.Size < 0 {
				return 0, 0, 0, fmt.Errorf("backup: 快照文件 %s 的大小为负数", n.Path)
			}
			filesCount++
			if n.Size > int64(^uint64(0)>>1)-bytes {
				return 0, 0, 0, errors.New("backup: 快照总大小超过计算上限")
			}
			bytes += n.Size
		case "dir":
			dirsCount++
		default:
			return 0, 0, 0, fmt.Errorf("backup: 快照节点 %s 是 %s；符号链接与特殊文件禁止恢复", n.Path, n.Type)
		}
		if n.Mode&0o6000 != 0 {
			return 0, 0, 0, fmt.Errorf("backup: 快照节点 %s 含 setuid/setgid 权限，拒绝恢复", n.Path)
		}
	}
	return filesCount, dirsCount, bytes, nil
}

func (o *Ops) AssessRestore(ctx context.Context, req RestoreRequest) (RestoreAssessment, error) {
	a := RestoreAssessment{Allowed: true, Locks: []string{LockName(req.RepositoryID),
		"file:" + req.TargetRootID + ":/" + req.RelativePath}}
	if o.opts.Restores == nil {
		a.Allowed = false
		a.Blockers = append(a.Blockers, Reason{Code: "BACKUP_RESTORE_UNAVAILABLE", Message: "代理没有接入句柄级安全恢复能力"})
		return a, nil
	}
	inv, cleanup, def, err := o.invocationFor(ctx, req.Repository, SnapshotQueryTimeout)
	if err != nil {
		return a, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	raw, err := o.opts.Restic.Snapshots(ctx, inv, nil)
	if err != nil {
		return a, err
	}
	found := false
	for _, item := range raw {
		if item.ID == req.SnapshotID {
			a.Snapshot, err = snapshotOf(item)
			found = err == nil
			break
		}
	}
	if !found {
		a.Allowed = false
		a.Blockers = append(a.Blockers, Reason{Code: "BACKUP_SNAPSHOT_NOT_FOUND", Message: "指定快照不在该仓库中"})
		return a, nil
	}
	nodes, err := o.opts.Restic.ListNodes(ctx, inv, req.SnapshotID)
	if err != nil {
		return a, err
	}
	_, _, bytes, nodeErr := validateSnapshotNodes(nodes)
	a.NodeCount = len(nodes)
	a.Bytes = bytes
	if nodeErr != nil {
		a.Allowed = false
		a.Blockers = append(a.Blockers, Reason{Code: "BACKUP_RESTORE_TREE_UNSUPPORTED", Message: nodeErr.Error()})
	}
	probe, probeErr := o.opts.Restores.ProbeRestore(ctx, RestoreParams{RootID: req.TargetRootID, Path: req.RelativePath, Overwrite: req.Overwrite})
	if probeErr != nil {
		a.Allowed = false
		a.Blockers = append(a.Blockers, Reason{Code: "BACKUP_RESTORE_TARGET_BLOCKED", Message: probeErr.Error()})
	} else if probe.CapacityKnown && uint64(bytes)+64<<20 > probe.AvailableBytes {
		a.Allowed = false
		a.Blockers = append(a.Blockers, Reason{Code: CodeNoSpace,
			Message: fmt.Sprintf("恢复暂存需要 %d 字节并预留 64 MiB，目标卷只剩 %d 字节", bytes, probe.AvailableBytes)})
	} else if !probe.CapacityKnown {
		a.Warnings = append(a.Warnings, Reason{Code: CodeCapacityUnknown,
			Message: "读不到目标卷可用空间；暂存写入仍会处理 ENOSPC，失败时不发布"})
	}
	repoFingerprint, err := def.IdentityFingerprint()
	if err != nil {
		return a, err
	}
	a.Target = Target{DisplayName: req.TargetRootID + ":/" + req.RelativePath,
		IdentityFingerprint: req.SnapshotID + "|" + repoFingerprint,
		StateVersion: fmt.Sprintf("snapshot:%s|target:%s|repository:%s:v%d",
			req.SnapshotID, probe.TargetVersion, req.Repository.ID, req.Repository.Version)}
	a.Impacts = append(a.Impacts, Impact{Kind: "restore_files", Description: fmt.Sprintf("恢复 %d 个节点、%d 字节到同卷暂存，逐文件核对后原子发布", len(nodes), bytes)})
	if req.Overwrite {
		a.Impacts = append(a.Impacts, Impact{Kind: "overwrite", Description: "原子替换整个目标目录；验证通过后才删除旧目录"})
		if len(req.Apps) > 0 {
			if req.Controller == nil {
				a.Allowed = false
				a.Blockers = append(a.Blockers, Reason{Code: CodePolicyAppsUnavailable, Message: "覆盖恢复需要停止依赖应用，但代理没有应用控制能力"})
			} else {
				runtimes, err := req.Controller.Inspect(ctx, req.Apps)
				if err != nil {
					return a, err
				}
				a.Apps = runtimes
				for _, rt := range runtimes {
					if !rt.Found || len(rt.Blockers) > 0 {
						a.Allowed = false
						a.Blockers = append(a.Blockers, Reason{Code: CodePolicyAppsUnavailable, Message: fmt.Sprintf("依赖应用 %s 不存在或不能安全停止", rt.Target.Label)})
					}
				}
				a.Impacts = append(a.Impacts, Impact{Kind: ImpactAvailability, Description: fmt.Sprintf("覆盖恢复期间停止 %d 个依赖应用，结束后按原状态拉起", len(req.Apps))})
			}
		}
	} else {
		a.Impacts = append(a.Impacts, Impact{Kind: "create_directory", Description: "只发布到不存在或已经为空的目录，不覆盖任何已有文件"})
	}
	return a, nil
}

func (o *Ops) Restore(ctx context.Context, req RestoreRequest) (out RestoreResult, retErr error) {
	out = RestoreResult{RepositoryID: req.RepositoryID, SnapshotID: req.SnapshotID,
		TargetRootID: req.TargetRootID, RelativePath: req.RelativePath, Overwrite: req.Overwrite}
	if o.opts.Restores == nil {
		return out, errors.New("backup: 没有安全恢复能力")
	}
	progress := req.Progress
	if progress == nil {
		progress = func(string, map[string]any) {}
	}
	inv, cleanup, _, err := o.invocationFor(ctx, req.Repository, RestoreTimeout)
	if err != nil {
		return out, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	nodes, err := o.opts.Restic.ListNodes(ctx, inv, req.SnapshotID)
	if err != nil {
		return out, err
	}
	if _, _, _, err := validateSnapshotNodes(nodes); err != nil {
		return out, err
	}
	session, err := o.opts.Restores.BeginRestore(ctx, RestoreParams{RootID: req.TargetRootID, Path: req.RelativePath, Overwrite: req.Overwrite})
	if err != nil {
		return out, err
	}
	defer func() {
		if err := session.Abort(context.WithoutCancel(ctx)); err != nil && retErr == nil {
			retErr = fmt.Errorf("backup: 恢复暂存清理失败: %w", err)
		}
	}()
	var stopped []AppRuntime
	appReq := RunRequest{Apps: req.Apps, Controller: req.Controller}
	appOut := RunResult{}
	if req.Overwrite && len(req.Apps) > 0 {
		defer func() {
			restoreAppsAfterRun(context.WithoutCancel(ctx), appReq, stopped, &appOut, &out.Warnings, progress)
			out.Apps = append(out.Apps, appOut.Apps...)
			out.AppsRestored = appOut.AppsRestored
			if !out.AppsRestored && retErr == nil {
				retErr = errors.New("backup: 恢复目录已处理，但依赖应用没有全部确认拉起")
			}
		}()
		if err := stopAppsForRun(ctx, appReq, &appOut, &out.Warnings, progress, &stopped); err != nil {
			return out, err
		}
	} else {
		out.AppsRestored = true
	}
	if err := ctxCancelled(ctx, req.Cancel); err != nil {
		return out, err
	}
	progress("restore", map[string]any{"snapshot_id": req.SnapshotID})
	if err := o.opts.Restic.Restore(ctx, inv, req.SnapshotID, session.Target(), nil); err != nil {
		return out, err
	}
	actual, err := session.Inspect(ctx)
	if err != nil {
		return out, err
	}
	if err := o.verifyRestoredNodes(ctx, inv, req.SnapshotID, nodes, actual); err != nil {
		return out, err
	}
	progress("publish", map[string]any{"files": len(actual)})
	receipt, err := session.Publish(ctx, actual)
	if err != nil {
		return out, err
	}
	out.Published, out.Verified = true, true
	out.Bytes = receipt.Bytes
	out.FinishedAt = receipt.VerifiedAt
	out.CleanupError = receipt.CleanupError
	for _, entry := range actual {
		if entry.Type == "file" {
			out.Files++
		} else if entry.Type == "dir" {
			out.Directories++
		}
	}
	return out, nil
}

func (o *Ops) verifyRestoredNodes(ctx context.Context, inv Invocation, snapshotID string, nodes []RestoreNode, actual []RestoreEntry) error {
	expected := append([]RestoreNode(nil), nodes...)
	sort.Slice(expected, func(i, j int) bool { return expected[i].Path < expected[j].Path })
	got := append([]RestoreEntry(nil), actual...)
	sort.Slice(got, func(i, j int) bool { return got[i].Path < got[j].Path })
	if len(expected) != len(got) {
		return fmt.Errorf("backup: 恢复节点数不一致（快照 %d，暂存 %d）", len(expected), len(got))
	}
	for i, n := range expected {
		rel := strings.TrimPrefix(n.Path, "/")
		entry := got[i]
		if entry.Path != rel || entry.Type != n.Type || entry.Mode != n.Mode&0o777 || (n.Type == "file" && entry.Size != n.Size) {
			return fmt.Errorf("backup: 恢复节点 %s 的路径、类型、权限或大小与快照不一致", n.Path)
		}
		if n.Type != "file" {
			continue
		}
		sum, size, err := o.opts.Restic.DumpSHA256(ctx, inv, snapshotID, n.Path)
		if err != nil {
			return err
		}
		if size != entry.Size || sum != entry.SHA256 {
			return fmt.Errorf("backup: 恢复文件 %s 的 SHA-256 与仓库字节不一致", n.Path)
		}
	}
	return nil
}

type VerifyRequest struct {
	RepositoryID  string
	Repository    Repository
	SnapshotID    string
	SampleRestore bool
}

type VerifyResult struct {
	RepositoryID  string
	SnapshotID    string
	Checked       bool
	SampleRestore bool
	Verified      bool
	FilesChecked  int
	BytesChecked  int64
	VerifiedAt    *time.Time
}

func (o *Ops) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	out := VerifyResult{RepositoryID: req.RepositoryID, SnapshotID: req.SnapshotID, SampleRestore: req.SampleRestore}
	inv, cleanup, _, err := o.invocationFor(ctx, req.Repository, VerifyTimeout)
	if err != nil {
		return out, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	if !req.SampleRestore {
		if err := o.opts.Restic.Check(ctx, inv); err != nil {
			return out, err
		}
		out.Checked = true
		return out, nil
	}
	if o.opts.Verifications == nil {
		return out, errors.New("backup: 没有恢复验证记录存储")
	}
	nodes, err := o.opts.Restic.ListNodes(ctx, inv, req.SnapshotID)
	if err != nil {
		return out, err
	}
	selected := selectVerifySample(nodes)
	if len(selected) == 0 {
		return out, errors.New("backup: 快照里没有符合 8 MiB/文件、32 MiB 总额抽样限额的普通文件")
	}
	dir, err := os.MkdirTemp(o.opts.Paths.StateDir, "restore-verify-")
	if err != nil {
		return out, fmt.Errorf("backup: 创建验证临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Chmod(dir, 0o700); err != nil {
		return out, err
	}
	includes := make([]string, 0, len(selected))
	for _, n := range selected {
		includes = append(includes, n.Path)
	}
	if err := o.opts.Restic.Restore(ctx, inv, req.SnapshotID, dir, includes); err != nil {
		return out, err
	}
	if err := checkVerifyTree(dir, len(selected)); err != nil {
		return out, err
	}
	for _, n := range selected {
		name := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(n.Path, "/")))
		st, err := os.Lstat(name)
		if err != nil || !st.Mode().IsRegular() || st.Size() != n.Size {
			return out, fmt.Errorf("backup: 抽样恢复文件 %s 缺失、类型错误或大小不一致", n.Path)
		}
		f, err := os.Open(name)
		if err != nil {
			return out, err
		}
		h := sha256.New()
		copied, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil || copied != n.Size {
			return out, fmt.Errorf("backup: 读取抽样恢复文件 %s 失败", n.Path)
		}
		sum, size, err := o.opts.Restic.DumpSHA256(ctx, inv, req.SnapshotID, n.Path)
		if err != nil {
			return out, err
		}
		if size != n.Size || sum != hex.EncodeToString(h.Sum(nil)) {
			return out, fmt.Errorf("backup: 抽样恢复文件 %s 的 SHA-256 与仓库字节不一致", n.Path)
		}
		out.FilesChecked++
		out.BytesChecked += n.Size
	}
	now := o.opts.Now().UTC()
	if err := o.opts.Verifications.MarkRestoreVerified(ctx, RestoreVerification{RepositoryID: req.RepositoryID,
		SnapshotID: req.SnapshotID, VerifiedAt: now, FilesChecked: out.FilesChecked, BytesChecked: out.BytesChecked}); err != nil {
		return out, err
	}
	out.Checked, out.Verified, out.VerifiedAt = true, true, &now
	return out, nil
}

func checkVerifyTree(root string, wantFiles int) error {
	var files, total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("backup: 抽样暂存含符号链接或特殊文件 %s", entry.Name())
		}
		files++
		total += info.Size()
		if files > int64(wantFiles) || total > VerifySampleMaxTotalBytes {
			return errors.New("backup: 抽样恢复超出文件数或 32 MiB 空间限额")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if files != int64(wantFiles) {
		return fmt.Errorf("backup: 抽样恢复文件数不一致（期望 %d，实际 %d）", wantFiles, files)
	}
	return nil
}

func selectVerifySample(nodes []RestoreNode) []RestoreNode {
	out := make([]RestoreNode, 0, VerifySampleMaxFiles)
	var total int64
	for _, n := range nodes {
		if n.Type != "file" || n.Size < 0 || n.Size > VerifySampleMaxFileBytes || total+n.Size > VerifySampleMaxTotalBytes {
			continue
		}
		out = append(out, n)
		total += n.Size
		if len(out) == VerifySampleMaxFiles {
			break
		}
	}
	return out
}
