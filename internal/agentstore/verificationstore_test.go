package agentstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"upanel/internal/backup"
)

func TestBackupVerificationStorePersistsPerSnapshot(t *testing.T) {
	db := openTestDB(t)
	store, err := NewBackupVerificationStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := backup.RestoreVerification{RepositoryID: testPolicyRepo,
		SnapshotID: strings.Repeat("a", 64), VerifiedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		FilesChecked: 2, BytesChecked: 128}
	second := first
	second.SnapshotID = strings.Repeat("b", 64)
	second.VerifiedAt = first.VerifiedAt.Add(time.Hour)
	for _, record := range []backup.RestoreVerification{first, second} {
		if err := store.MarkRestoreVerified(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	got, found, err := store.RestoreVerification(ctx, first.RepositoryID, first.SnapshotID)
	if err != nil || !found {
		t.Fatalf("读取验证记录 = %v, %v", found, err)
	}
	if !got.VerifiedAt.Equal(first.VerifiedAt) || got.FilesChecked != 2 || got.BytesChecked != 128 {
		t.Fatalf("验证记录不一致: %+v", got)
	}
	first.VerifiedAt = first.VerifiedAt.Add(2 * time.Hour)
	if err := store.MarkRestoreVerified(ctx, first); err != nil {
		t.Fatal(err)
	}
	other, found, err := store.RestoreVerification(ctx, second.RepositoryID, second.SnapshotID)
	if err != nil || !found || !other.VerifiedAt.Equal(second.VerifiedAt) {
		t.Fatalf("更新一个快照不应改变另一个: %+v, %v, %v", other, found, err)
	}
}

func TestBackupVerificationStoreRejectsEmptyEvidence(t *testing.T) {
	store, err := NewBackupVerificationStore(openTestDB(t))
	if err != nil {
		t.Fatal(err)
	}
	err = store.MarkRestoreVerified(context.Background(), backup.RestoreVerification{RepositoryID: testPolicyRepo,
		SnapshotID: strings.Repeat("a", 64), VerifiedAt: time.Now()})
	if err == nil {
		t.Fatal("没有文件证据却写入了恢复验证时间")
	}
}
