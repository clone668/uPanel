package agent

// 共享查询的接线测试（P08.1）：share.capability、share.list、share.accounts。
//
// 这一层只负责三件事：参数按形状拒绝、把登记表与能力报告原样返回、以及**不在
// 响应里漏出不该给的东西**（秘密引用、密码形态的字段）。判决与执行在
// shareaction_test.go 与 internal/shares 里各自有测试，这里不重复。
//
// 三条查询都返回能力报告：页面必须同时看到“这条共享的定义是什么”与“它现在有没有
// 被提供”。definition.enabled 是意图，config_applied 是事实。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/shares"
)

// shareQuerySource 给三条查询用的替身：一份“Samba 齐了、配置写入还不支持”的能力。
func shareQuerySource() *fakeShares {
	return &fakeShares{
		capability: shares.Capability{
			Status:         shares.CapabilityAvailable,
			SambaVersion:   "4.19.2-Ubuntu",
			Tools:          shares.ToolPaths{Smbd: "/usr/sbin/smbd", Setfacl: "/usr/bin/setfacl"},
			ConfigPath:     shares.DefaultConfigPath,
			ConfigReadable: true,
			IncludeDir:     shares.DefaultIncludeDir,
			IncludeReady:   false,
			IncludePresent: false,
			ConfigWrite:    shares.ConfigWriteUnsupported,
			Notices: []shares.Blocker{{
				Code:    shares.NoticeIncludeDirMissing,
				Message: "托管 include 目录还不存在（P08.2 会创建它）",
			}},
			CollectedAt: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
		},
		records: []shares.ShareRecord{{
			ID: testShareID,
			Definition: shares.ShareDefinition{
				Name:         "docs",
				Protocol:     shares.ProtocolSMB,
				VolumeID:     "vol_0123456789abcdef0123456789abcdef",
				RelativePath: "docs",
				Enabled:      true,
				AllowedCIDRs: []string{"10.0.0.0/24"},
				SMBUsers:     []shares.ShareUser{{AccountID: testAccountID, Access: shares.AccessWrite}},
			},
			ConfigHash:        strings.Repeat("a", 64),
			Version:           "2",
			DirectoryPrepared: true,
			ConfigApplied:     false,
			CreatedAt:         time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
			UpdatedAt:         time.Date(2026, 11, 4, 9, 5, 0, 0, time.UTC),
		}},
		accounts: []shares.AccountRecord{{
			ID:          testAccountID,
			Name:        "upanel-share-docs",
			Type:        shares.AccountTypeShare,
			Managed:     true,
			HasPassword: true,
			UID:         995,
			GID:         990,
			Shell:       "/usr/sbin/nologin",
			Home:        "/nonexistent",
			SecretRef:   testSecretRef,
			Version:     "2",
			CreatedAt:   time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
			UpdatedAt:   time.Date(2026, 11, 4, 9, 3, 0, 0, time.UTC),
		}},
	}
}

// shareQueryServer 构造一个带共享查询的测试服务。
func shareQueryServer(t *testing.T, src *fakeShares) *Server {
	t.Helper()
	reg, err := NewProductionRegistry(ProductionDeps{Shares: src})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	return newTestServerWithRegistry(t, reg)
}

// TestShareCapabilityQuery 能力查询把“配置写入还不支持”写死返回。
func TestShareCapabilityQuery(t *testing.T) {
	srv := shareQueryServer(t, shareQuerySource())
	rec := postQuery(t, srv, `{"query":"`+shares.QueryCapability+`"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data shares.Capability `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Available() {
		t.Errorf("能力 = %+v", body.Data)
	}
	// 这一条是 P08.1 的诚实性锚点：配置写入还没实现，能力对象必须这么说。
	if body.Data.ConfigWrite != shares.ConfigWriteUnsupported {
		t.Errorf("config_write = %q，P08.1 必须是 unsupported", body.Data.ConfigWrite)
	}
	if body.Data.IncludePresent {
		t.Error("P08.1 还没接 include，include_present 不该是 true")
	}
	if len(body.Data.Notices) != 1 || body.Data.Notices[0].Code != shares.NoticeIncludeDirMissing {
		t.Errorf("notices = %+v", body.Data.Notices)
	}
}

// TestShareListQuery 列表把能力与共享一起返回，且“已启用”与“已生效”分开。
func TestShareListQuery(t *testing.T) {
	srv := shareQueryServer(t, shareQuerySource())
	rec := postQuery(t, srv, `{"query":"`+shares.QueryShares+`"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data shareListView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Capability.Available() {
		t.Errorf("列表没有带能力报告：%+v", body.Data.Capability)
	}
	if len(body.Data.Shares) != 1 {
		t.Fatalf("共享数 = %d", len(body.Data.Shares))
	}
	got := body.Data.Shares[0]
	if got.ID != testShareID || got.Definition.Name != "docs" {
		t.Errorf("共享字段不对：%+v", got)
	}
	if !got.DirectoryPrepared || got.ConfigApplied {
		t.Errorf("directory_prepared/config_applied = %v/%v，P08.1 只能是「目录已备、配置未写」",
			got.DirectoryPrepared, got.ConfigApplied)
	}
	if len(got.ConfigHash) != 64 {
		t.Errorf("config_hash = %q", got.ConfigHash)
	}
}

// TestShareListQueryEmptyIsArray 空列表是 [] 而不是 null。
//
// 页面据此判断“没有共享”；null 会被当成“不知道”，两者在界面上是两张不同的脸。
func TestShareListQueryEmptyIsArray(t *testing.T) {
	src := shareQuerySource()
	src.records = nil
	src.accounts = nil
	srv := shareQueryServer(t, src)

	for _, tc := range []struct{ query, key string }{
		{shares.QueryShares, "shares"},
		{shares.QueryAccounts, "accounts"},
	} {
		rec := postQuery(t, srv, `{"query":"`+tc.query+`"}`, testWebUID)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 状态码 = %d（%s）", tc.query, rec.Code, rec.Body.String())
		}
		var body struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: 响应不是合法 JSON: %v", tc.query, err)
		}
		raw, ok := body.Data[tc.key]
		if !ok {
			t.Fatalf("%s: 响应缺少 %q", tc.query, tc.key)
		}
		if strings.TrimSpace(string(raw)) == "null" {
			t.Errorf("%s: 空列表序列化成了 null：%s", tc.query, rec.Body.String())
		}
	}
}

// TestShareAccountsQueryHidesSecretRef 账号查询不能漏出秘密引用或密码形态字段。
func TestShareAccountsQueryHidesSecretRef(t *testing.T) {
	srv := shareQueryServer(t, shareQuerySource())
	rec := postQuery(t, srv, `{"query":"`+shares.QueryAccounts+`"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, banned := range []string{"secret_ref", "sec_", "password_hash", `"password"`, "smbpasswd"} {
		if strings.Contains(body, banned) {
			t.Errorf("账号响应里出现了 %q：%s", banned, body)
		}
	}
	if !strings.Contains(body, "has_password") {
		t.Errorf("has_password 应该在（它只回答“设过没有”）：%s", body)
	}
}

// TestShareQueriesRejectBadParams 三条查询都不接受参数：带了就是 400，不是静默忽略。
func TestShareQueriesRejectBadParams(t *testing.T) {
	srv := shareQueryServer(t, shareQuerySource())
	for _, raw := range []string{
		`{"query":"` + shares.QueryCapability + `","params":{"refresh":true}}`,
		`{"query":"` + shares.QueryShares + `","params":{"limit":10}}`,
		`{"query":"` + shares.QueryAccounts + `","params":{"include_system":true}}`,
		`{"query":"` + shares.QueryShares + `","params":[]}`,
	} {
		rec := postQuery(t, srv, raw, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", raw, rec.Code, rec.Body.String())
		}
	}
}

// TestShareListQueryStoreErrorIsError 登记表打不开是错误，不是空列表。
func TestShareListQueryStoreErrorIsError(t *testing.T) {
	src := shareQuerySource()
	src.listErr = context.DeadlineExceeded
	srv := shareQueryServer(t, src)
	rec := postQuery(t, srv, `{"query":"`+shares.QueryShares+`"}`, testWebUID)
	if rec.Code == http.StatusOK {
		t.Fatalf("登记表出错却回了 200：%s", rec.Body.String())
	}
}
