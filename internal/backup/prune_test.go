package backup

import (
	"strings"
	"testing"
	"time"
)

// 待删集合是执行状态，不是目标身份。两次新备份让它变化时，预检必须报
// PREFLIGHT_STALE（状态版本变了），而不是 DEVICE_IDENTITY_CHANGED（换了一台/一个仓库）。
func TestPruneTargetIdentityIgnoresDeletionSet(t *testing.T) {
	def := validLocal()
	repo := Repository{ID: "rep_0123456789abcdef0123456789abcdef", Name: "本地", Definition: def, Version: 3}
	policy := Policy{Name: "夜间备份", RepositoryID: repo.ID, Retention: Retention{KeepLast: 1}}
	older := Snapshot{ID: strings.Repeat("a", 64), Time: time.Unix(100, 0).UTC()}
	newer := Snapshot{ID: strings.Repeat("b", 64), Time: time.Unix(200, 0).UTC()}

	before, err := pruneTarget("pol_0123456789abcdef0123456789abcdef", policy, repo, []Snapshot{older})
	if err != nil {
		t.Fatal(err)
	}
	after, err := pruneTarget("pol_0123456789abcdef0123456789abcdef", policy, repo, []Snapshot{older, newer})
	if err != nil {
		t.Fatal(err)
	}

	if before.IdentityFingerprint != after.IdentityFingerprint {
		t.Fatalf("待删集合变化不应改变目标身份: %q → %q", before.IdentityFingerprint, after.IdentityFingerprint)
	}
	if before.StateVersion == after.StateVersion {
		t.Fatalf("待删集合变化必须改变状态版本，否则过期预检不会被拒绝")
	}
	if before.DisplayName != after.DisplayName {
		t.Fatalf("目标显示名应稳定: %q → %q", before.DisplayName, after.DisplayName)
	}
}

// 换仓库定义（同一仓库 id、不同位置）必须改变身份：那才是“目标被换掉”。
func TestPruneTargetIdentityTracksRepositoryDefinition(t *testing.T) {
	policy := Policy{Name: "夜间备份", Retention: Retention{KeepLast: 1}}
	left := Repository{ID: "rep_0123456789abcdef0123456789abcdef", Version: 1,
		Definition: Definition{Name: "本地", Backend: BackendLocal,
			Location:          Location{RootID: "root1", RelativePath: "backups/left"},
			PasswordSecretRef: "sec_0123456789abcdef0123456789abcdef"}}
	right := left
	right.Definition.Location.RelativePath = "backups/right"

	a, err := pruneTarget("pol_0123456789abcdef0123456789abcdef", policy, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pruneTarget("pol_0123456789abcdef0123456789abcdef", policy, right, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.IdentityFingerprint == b.IdentityFingerprint {
		t.Fatal("仓库位置变化后身份指纹必须变化")
	}
}
