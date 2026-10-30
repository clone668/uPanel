package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWritePrivateKeyUsesUniqueFiles(t *testing.T) {
	r := NewRestic(Paths{RunDir: t.TempDir()}, nil)
	first, err := r.WritePrivateKey("repository", []byte("first secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer RemovePrivateKey(first)
	second, err := r.WritePrivateKey("repository", []byte("second secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer RemovePrivateKey(second)
	if first == second {
		t.Fatal("concurrent invocations must not share a private-key path")
	}
	got, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first secret" {
		t.Fatalf("first key was overwritten: %q", got)
	}
}

func TestRunReturnsErrorWhenVerifiedBackupCannotRestartApps(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	runner.backupLines = backupOutput(testSnapA)
	runner.stats = `{"total_size":1,"total_file_count":1}`
	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID}
	apps := &fakeApps{
		verifyStop: true,
		startErr:   fmt.Errorf("start failed"),
		runtimes: []AppRuntime{{Target: AppTarget{AppID: testAppID, Kind: AppKindSystemd,
			Object: "upanel-app-a.service", Label: "site"}, Found: true, Running: true}},
	}
	out, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID, Policy: pol,
		Repository: Repository{ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1},
		Apps:       []AppTarget{{AppID: testAppID, Kind: AppKindSystemd, Object: "upanel-app-a.service", Label: "site"}},
		Controller: apps,
	})
	if err == nil {
		t.Fatal("verified snapshot with a failed application restart must not return success")
	}
	if !out.Verified || out.AppsRestored {
		t.Fatalf("result must preserve snapshot verification and restart failure: %+v", out)
	}
}

type incompleteApps struct {
	omitStop  bool
	omitStart bool
}

func (f *incompleteApps) Inspect(_ context.Context, targets []AppTarget) ([]AppRuntime, error) {
	out := make([]AppRuntime, 0, len(targets))
	for _, target := range targets {
		out = append(out, AppRuntime{Target: target, Found: true, Running: true})
	}
	return out, nil
}

func (f *incompleteApps) Stop(_ context.Context, runtimes []AppRuntime) ([]AppAction, error) {
	if f.omitStop {
		return nil, nil
	}
	actions := make([]AppAction, 0, len(runtimes))
	for _, rt := range runtimes {
		actions = append(actions, AppAction{Target: rt.Target, Operation: "stop", Done: true, Verified: true})
	}
	return actions, nil
}

func (f *incompleteApps) Start(_ context.Context, runtimes []AppRuntime) ([]AppAction, error) {
	if f.omitStart {
		return nil, nil
	}
	actions := make([]AppAction, 0, len(runtimes))
	for _, rt := range runtimes {
		actions = append(actions, AppAction{Target: rt.Target, Operation: "start", Done: true, Verified: true})
	}
	return actions, nil
}

func runWithIncompleteApps(t *testing.T, apps *incompleteApps) (RunResult, *runRunner, error) {
	t.Helper()
	ops, runner, _ := newRunOps(t)
	runner.backupLines = backupOutput(testSnapA)
	runner.stats = `{"total_size":1,"total_file_count":1}`
	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID}
	target := AppTarget{AppID: testAppID, Kind: AppKindSystemd, Object: "upanel-app-a.service", Label: "site"}
	out, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID, Policy: pol,
		Repository: Repository{ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1},
		Apps:       []AppTarget{target}, Controller: apps,
	})
	return out, runner, err
}

func TestRunRejectsMissingStopAction(t *testing.T) {
	_, runner, err := runWithIncompleteApps(t, &incompleteApps{omitStop: true})
	if err == nil {
		t.Fatal("a requested stop without an action result must fail closed")
	}
	if len(runner.callsOf("backup")) != 0 {
		t.Fatal("backup must not run while application stop state is unknown")
	}
}

func TestRunRejectsMissingStartAction(t *testing.T) {
	out, _, err := runWithIncompleteApps(t, &incompleteApps{omitStart: true})
	if err == nil {
		t.Fatal("a verified backup with an unreported application restart must fail")
	}
	if !out.Verified || out.AppsRestored {
		t.Fatalf("missing start action must not be reported as restored: %+v", out)
	}
}

func TestAssessPruneUsesSnapshotsBeyondDisplayLimit(t *testing.T) {
	dir := t.TempDir()
	paths := DefaultPaths()
	paths.Restic = filepath.Join(dir, "restic")
	paths.SSH = filepath.Join(dir, "ssh")
	paths.StateDir = filepath.Join(dir, "state")
	paths.CacheDir = filepath.Join(dir, "cache")
	paths.RunDir = filepath.Join(dir, "run")
	for _, name := range []string{paths.Restic, paths.SSH} {
		if err := os.WriteFile(name, []byte("fake"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	raw := make([]ResticSnapshot, SnapshotListLimit+1)
	for i := range raw {
		raw[i] = ResticSnapshot{ID: fmt.Sprintf("%064x", i+1), ShortID: fmt.Sprintf("%08x", i+1),
			Time: time.Unix(int64(i+1), 0).UTC().Format(time.RFC3339), Paths: []string{"/srv"}}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	runner := &runRunner{snapshots: string(encoded)}
	secrets := &fakeSecrets{refs: map[string]fakeSecret{
		"sec_0123456789abcdef0123456789abcdef": {purpose: "backup_repository_password", value: "secret"},
	}}
	ops, err := NewOps(Options{
		Restic:  NewRestic(paths, runner.run),
		Roots:   fakeRoots{paths: map[string]string{"root1": filepath.Join(dir, "data")}},
		Secrets: secrets, Paths: paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := ops.AssessPrune(context.Background(), PruneRequest{
		PolicyID:   testPolicyID,
		Policy:     Policy{Name: "nightly", RepositoryID: "rep_0123456789abcdef0123456789abcdef", Retention: Retention{KeepLast: 1}},
		Repository: Repository{ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(assessment.Keep) != 1 || len(assessment.Delete) != SnapshotListLimit {
		t.Fatalf("retention saw %d keep/%d delete, want 1/%d: %s", len(assessment.Keep), len(assessment.Delete), SnapshotListLimit, strings.Join(snapshotIDs(assessment.Keep), ","))
	}
}
