package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type PruneRequest struct {
	PolicyID   string
	Policy     Policy
	Repository Repository
	// ExpectedDeleteIDs 是预检冻结的集合；执行重新计算后必须逐字一致。
	ExpectedDeleteIDs []string
	Cancel            <-chan struct{}
}

type PruneAssessment struct {
	Allowed  bool
	Blockers []Reason
	Locks    []string
	Target   Target
	Keep     []Snapshot
	Delete   []Snapshot
}

type PruneResult struct {
	PolicyID     string
	RepositoryID string
	DeletedIDs   []string
	KeptIDs      []string
	Verified     bool
	FinishedAt   time.Time
}

func (o *Ops) AssessPrune(ctx context.Context, req PruneRequest) (PruneAssessment, error) {
	a := PruneAssessment{Allowed: true, Locks: []string{LockName(req.Repository.ID), PolicyLockName(req.PolicyID)}}
	snapshots, err := o.listSnapshots(ctx, SnapshotRequest{RepositoryID: req.Repository.ID,
		Definition: req.Repository.Definition, PolicyID: req.PolicyID}, 0)
	if err != nil {
		return a, err
	}
	selection, err := SelectRetention(snapshots, req.Policy.Retention)
	if err != nil {
		a.Allowed = false
		a.Blockers = append(a.Blockers, Reason{Code: "BACKUP_RETENTION_INVALID", Message: err.Error()})
		return a, nil
	}
	a.Keep, a.Delete = selection.Keep, selection.Delete
	target, err := pruneTarget(req.PolicyID, req.Policy, req.Repository, a.Delete)
	if err != nil {
		return a, err
	}
	a.Target = target
	return a, nil
}

// pruneTarget 生成清理预检的目标绑定。
//
// 身份指纹只描述“这对策略与仓库是谁”，不包含待删快照集合。集合是会变的执行状态：
// 两次新备份就会让它变化，但目标并没有被换掉。把集合算进身份，
// revalidate 会先命中 DEVICE_IDENTITY_CHANGED，把一次普通的预检过期错报成设备被换；
// 契约要求“预检后待删集合变化”一律是 PREFLIGHT_STALE。集合因此只进 StateVersion。
func pruneTarget(policyID string, policy Policy, repo Repository, deletion []Snapshot) (Target, error) {
	ids := snapshotIDs(deletion)
	def, err := repo.Definition.Normalize()
	if err != nil {
		return Target{}, err
	}
	repoFingerprint, err := def.IdentityFingerprint()
	if err != nil {
		return Target{}, err
	}
	policyFingerprint := sha256.Sum256([]byte(policyID))
	return Target{
		DisplayName: policy.Name + " → " + repo.Definition.DisplayName(),
		IdentityFingerprint: "policy:" + hex.EncodeToString(policyFingerprint[:]) +
			"|repo:" + repoFingerprint,
		StateVersion: fmt.Sprintf("backup-policy:%s|delete:%s|repository:%s:v%d",
			policyID, retentionFingerprint(policyID, ids), repo.ID, repo.Version),
	}, nil
}

func (o *Ops) Prune(ctx context.Context, req PruneRequest) (PruneResult, error) {
	out := PruneResult{PolicyID: req.PolicyID, RepositoryID: req.Repository.ID}
	listReq := SnapshotRequest{RepositoryID: req.Repository.ID, Definition: req.Repository.Definition, PolicyID: req.PolicyID}
	snapshots, err := o.listSnapshots(ctx, listReq, 0)
	if err != nil {
		return out, err
	}
	selection, err := SelectRetention(snapshots, req.Policy.Retention)
	if err != nil {
		return out, err
	}
	actual := snapshotIDs(selection.Delete)
	if !equalStrings(actual, req.ExpectedDeleteIDs) {
		return out, fmt.Errorf("PREFLIGHT_STALE：预检后待清理快照集合已变化（原 %d 个，现 %d 个）；没有执行删除", len(req.ExpectedDeleteIDs), len(actual))
	}
	out.KeptIDs = snapshotIDs(selection.Keep)
	if len(actual) == 0 {
		out.Verified, out.FinishedAt = true, o.opts.Now().UTC()
		return out, nil
	}
	if err := ctxCancelled(ctx, req.Cancel); err != nil {
		return out, err
	}
	inv, cleanup, _, err := o.invocationFor(ctx, req.Repository, PruneTimeout)
	if err != nil {
		return out, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	forgetErr := o.opts.Restic.ForgetPrune(ctx, inv, actual)
	remaining, readErr := o.listSnapshots(ctx, listReq, 0)
	if readErr != nil {
		out.DeletedIDs = append([]string(nil), actual...)
		return out, errors.Join(forgetErr, fmt.Errorf("backup: 清理后无法读回快照列表: %w", readErr))
	}
	left := make(map[string]bool, len(remaining))
	for _, snap := range remaining {
		left[snap.ID] = true
	}
	for _, id := range actual {
		if !left[id] {
			out.DeletedIDs = append(out.DeletedIDs, id)
		}
	}
	if forgetErr != nil {
		return out, forgetErr
	}
	for _, id := range actual {
		if left[id] {
			return out, fmt.Errorf("backup: restic 返回成功，但快照 %s 仍在仓库中", id)
		}
	}
	for _, snap := range selection.Keep {
		if !left[snap.ID] {
			return out, fmt.Errorf("backup: 保留策略要求保留的快照 %s 在清理后不存在", snap.ID)
		}
	}
	out.Verified, out.FinishedAt = true, o.opts.Now().UTC()
	return out, nil
}

func snapshotIDs(snapshots []Snapshot) []string {
	out := make([]string, 0, len(snapshots))
	for _, snap := range snapshots {
		out = append(out, snap.ID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func retentionFingerprint(policyID string, ids []string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(policyID))
	for _, id := range ids {
		_, _ = h.Write([]byte("\n" + id))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SortedIDs 仅用于结果展示；执行集合始终保留预检的时间顺序。
func SortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func FormatSnapshotIDs(ids []string) string { return strings.Join(ids, ",") }
