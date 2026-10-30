package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
)

func TestResolveBackupAppsPreservesRequestedPolicyVersion(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	pol := agentclient.BackupPolicy{ID: testPolicyID, Version: 7}
	out, ok := (&Server{}).resolveBackupApps(c, pol, 3)
	if !ok {
		t.Fatal("files-only policy should expand")
	}
	if out.Version != 3 {
		t.Fatalf("expanded version = %d, want caller's stale version 3", out.Version)
	}
}
