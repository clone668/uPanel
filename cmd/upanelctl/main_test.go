package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/store"
	"upanel/migrations"
)

func runCtl(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := runCtl("version")
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(out, "upanelctl") || !strings.Contains(out, "0.0.0-dev") {
		t.Fatalf("输出 = %q", out)
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, errOut := runCtl("version", "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	for _, key := range []string{"version", "commit", "build_date", "go_version"} {
		if _, ok := info[key]; !ok {
			t.Fatalf("缺少字段 %q: %v", key, info)
		}
	}
}

func TestCheckConfigWithValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upanel.json")
	content := `{"web":{"mode":"production","listen_address":"127.0.0.1:19443"},"agent":{"socket_path":"/run/upanel/agent.sock"},"log":{"level":"debug"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCtl("check-config", "--config", path)
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	for _, want := range []string{"127.0.0.1:19443", "/run/upanel/agent.sock", "debug", "production"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestCheckConfigJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upanel.json")
	if err := os.WriteFile(path, []byte(`{"web":{"mode":"mock"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCtl("check-config", "--config", path, "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	var report struct {
		Mode            string `json:"mode"`
		WebListen       string `json:"web_listen"`
		WebLoopbackOnly bool   `json:"web_loopback_only"`
		Platform        struct {
			OS string `json:"os"`
		} `json:"platform"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	if report.Mode != "mock" {
		t.Fatalf("mode = %q", report.Mode)
	}
	// mock 模式即使配置文件未写监听地址，也必须解析为固定的回环地址。
	if report.WebListen != "127.0.0.1:18881" {
		t.Fatalf("web_listen = %q", report.WebListen)
	}
	if !report.WebLoopbackOnly {
		t.Fatal("mock 必须只绑定回环地址")
	}
	if report.Platform.OS == "" {
		t.Fatal("platform.os 不能为空")
	}
}

func TestCheckConfigRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"log":{"level":"verbose"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCtl("check-config", "--config", path)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(errOut, "log.level") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestCheckConfigMissingFileIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	code, _, errOut := runCtl("check-config", "--config", missing)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(errOut, "配置错误") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	code, _, errOut := runCtl("frobnicate")
	if code != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d", code, exitUsage)
	}
	if !strings.Contains(errOut, "未知子命令") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestPlannedSubcommandIsNotImplemented(t *testing.T) {
	// scheduler 是 P12 的子命令，0.1 阶段必须明确返回“未实现”，不能返回成功。
	code, _, errOut := runCtl("scheduler")
	if code != exitNotImplemented {
		t.Fatalf("退出码 = %d，期望 %d（未实现不得返回成功）", code, exitNotImplemented)
	}
	if !strings.Contains(errOut, "尚未实现") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestNoArgumentsShowsUsage(t *testing.T) {
	code, _, errOut := runCtl()
	if code != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d", code, exitUsage)
	}
	if !strings.Contains(errOut, "用法") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeMigrateReport(t *testing.T, out string) migrateReport {
	t.Helper()
	var report migrateReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	return report
}

func TestMigrateFreshDatabaseThenIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")

	code, out, errOut := runCtl("migrate", "--db", dbPath, "--json")
	if code != 0 {
		t.Fatalf("首次迁移退出码 = %d，stderr = %q", code, errOut)
	}
	first := decodeMigrateReport(t, out)
	if first.DatabasePath != dbPath {
		t.Fatalf("database_path = %q", first.DatabasePath)
	}
	if first.FromVersion != 0 {
		t.Fatalf("新库执行前版本 = %d，期望 0", first.FromVersion)
	}
	// 迁移数量随阶段增长：断言“非零且从 1 开始连续”，并把 schema_version 与最后一条对齐。
	if len(first.AppliedVersions) == 0 || first.AppliedVersions[0] != 1 {
		t.Fatalf("applied_versions = %v，期望从 1 开始", first.AppliedVersions)
	}
	for i, v := range first.AppliedVersions {
		if v != i+1 {
			t.Fatalf("applied_versions 不连续: %v", first.AppliedVersions)
		}
	}
	if first.SchemaVersion != first.AppliedVersions[len(first.AppliedVersions)-1] {
		t.Fatalf("schema_version = %d，与最后一条迁移 %v 不一致", first.SchemaVersion, first.AppliedVersions)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("数据库文件未创建: %v", err)
	}

	// 重复执行必须是幂等的：不再应用任何迁移，版本不变。
	code, out, errOut = runCtl("migrate", "--db", dbPath, "--json")
	if code != 0 {
		t.Fatalf("重复迁移退出码 = %d，stderr = %q", code, errOut)
	}
	second := decodeMigrateReport(t, out)
	if second.SchemaVersion != first.SchemaVersion {
		t.Fatalf("重复执行后 schema_version = %d，期望与首次一致（%d）", second.SchemaVersion, first.SchemaVersion)
	}
	if len(second.AppliedVersions) != 0 {
		t.Fatalf("重复执行不应再应用迁移，实际 %v", second.AppliedVersions)
	}
}

// embeddedVersions 返回嵌入的迁移版本（升序），让断言跟着迁移文件走而不是写死数字。
func embeddedVersions(t *testing.T) []int {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("读取嵌入迁移失败: %v", err)
	}
	versions := make([]int, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err != nil {
			t.Fatalf("迁移文件名无法解析版本: %s", e.Name())
		}
		versions = append(versions, v)
	}
	return versions
}

func TestMigrateTextOutput(t *testing.T) {
	versions := embeddedVersions(t)
	if len(versions) == 0 {
		t.Fatal("没有嵌入迁移，测试前提不成立")
	}
	wantSchema := fmt.Sprintf("schema 版本: %d", versions[len(versions)-1])
	wantApplied := make([]string, 0, len(versions))
	for _, v := range versions {
		wantApplied = append(wantApplied, fmt.Sprintf("%04d", v))
	}

	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	code, out, errOut := runCtl("migrate", "--db", dbPath)
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	if !strings.Contains(out, wantSchema) {
		t.Fatalf("输出 = %q，期望包含 %q", out, wantSchema)
	}
	if !strings.Contains(out, "本次应用: "+strings.Join(wantApplied, ", ")) {
		t.Fatalf("输出未说明应用的迁移: %q", out)
	}

	code, out, _ = runCtl("migrate", "--db", dbPath)
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(out, "本次应用: 无") {
		t.Fatalf("重复执行应报告无需应用: %q", out)
	}
}

func TestMigrateRequiresDatabasePath(t *testing.T) {
	// 显式把 database.path 置空、也不传 --db：必须拒绝，不能默默挑一个路径。
	dir := t.TempDir()
	cfg := writeConfig(t, dir, "empty.json", `{"web":{"mode":"mock"},"database":{"path":""}}`)
	code, _, errOut := runCtl("migrate", "--config", cfg)
	if code != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d（stderr=%q）", code, exitUsage, errOut)
	}
	if !strings.Contains(errOut, "未指定数据库路径") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestMigrateRejectsUnusablePaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "相对路径", path: "data/upanel.db"},
		{name: "目录不存在", path: filepath.Join(t.TempDir(), "absent", "upanel.db")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := runCtl("migrate", "--db", tc.path)
			if code != 1 {
				t.Fatalf("退出码 = %d，期望 1（stderr=%q）", code, errOut)
			}
			if !strings.Contains(errOut, "迁移失败") {
				t.Fatalf("stderr = %q", errOut)
			}
			if strings.Contains(errOut, "schema 版本") {
				t.Fatalf("失败时不得输出成功信息: %q", errOut)
			}
		})
	}
}

func TestMigrateRefusesDatabaseAheadOfBinary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	code, _, errOut := runCtl("migrate", "--db", dbPath)
	if code != 0 {
		t.Fatalf("准备数据库失败: %d %q", code, errOut)
	}

	// 手工写入一个高于本程序内置版本的迁移记录。
	db, err := store.Open(context.Background(), store.Options{
		Path:   dbPath,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	if _, err := db.SQL().ExecContext(context.Background(),
		`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (9999, 'future', 'x', '2030-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("写入未来版本失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	code, _, errOut = runCtl("migrate", "--db", dbPath)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（stderr=%q）", code, errOut)
	}
	if !strings.Contains(errOut, "降级") {
		t.Fatalf("拒绝原因未说明降级: %q", errOut)
	}
}

func TestCheckConfigReportsDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg := writeConfig(t, dir, "db.json",
		`{"web":{"mode":"mock"},"database":{"path":"/var/lib/upanel/upanel.db","busy_timeout_ms":2500}}`)
	code, out, errOut := runCtl("check-config", "--config", cfg, "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	var report struct {
		DatabasePath          string `json:"database_path"`
		DatabaseBusyTimeoutMS int    `json:"database_busy_timeout_ms"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	if report.DatabasePath != "/var/lib/upanel/upanel.db" {
		t.Fatalf("database_path = %q", report.DatabasePath)
	}
	if report.DatabaseBusyTimeoutMS != 2500 {
		t.Fatalf("database_busy_timeout_ms = %d", report.DatabaseBusyTimeoutMS)
	}
}

// withStdin 临时替换 init-admin 的输入来源。
func withStdin(t *testing.T, input string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(input)
	t.Cleanup(func() { stdin = old })
}

func TestInitAdminCreatesFirstAdmin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	const password = "correct-horse-9"
	withStdin(t, password+"\n"+password+"\n")

	code, out, errOut := runCtl("init-admin", "--db", dbPath, "--username", "Admin", "--display-name", "管理员")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	if !strings.Contains(out, "已创建管理员: admin") {
		t.Fatalf("输出 = %q", out)
	}
	// 输出与 stderr 都不得包含密码。
	if strings.Contains(out, password) || strings.Contains(errOut, password) {
		t.Fatal("输出中出现了明文密码")
	}

	// 数据库里应有一个 admin 账号，密码以 Argon2id 哈希保存。
	db, err := store.Open(context.Background(), store.Options{Path: dbPath, Logger: quietTestLogger()})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	var username, hash, role string
	if err := db.SQL().QueryRow(
		`SELECT username, password_hash, role FROM users`).Scan(&username, &hash, &role); err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if username != "admin" || role != "admin" {
		t.Fatalf("账号 = %s（%s）", username, role)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("密码哈希格式 = %q", hash)
	}
	if hash == password {
		t.Fatal("库里不能保存明文密码")
	}

	// 第二次执行必须拒绝，且不改变已有账号。
	withStdin(t, "another-password-1\nanother-password-1\n")
	code, _, errOut = runCtl("init-admin", "--db", dbPath, "--username", "second")
	if code == 0 {
		t.Fatal("已有账号时 init-admin 必须失败")
	}
	if !strings.Contains(errOut, "只用于首次初始化") {
		t.Fatalf("stderr = %q", errOut)
	}
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("统计账号失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("账号数 = %d，期望 1", count)
	}
}

func TestInitAdminRejectsMismatchedOrWeakPassword(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")

	withStdin(t, "correct-horse-9\ncorrect-horse-8\n")
	code, _, errOut := runCtl("init-admin", "--db", dbPath)
	if code == 0 || !strings.Contains(errOut, "不一致") {
		t.Fatalf("两行不一致时退出码 = %d，stderr = %q", code, errOut)
	}

	withStdin(t, "short\nshort\n")
	code, _, errOut = runCtl("init-admin", "--db", dbPath)
	if code == 0 || !strings.Contains(errOut, "密码") {
		t.Fatalf("弱密码时退出码 = %d，stderr = %q", code, errOut)
	}

	// 单独一行（缺确认行）也必须失败。
	withStdin(t, "correct-horse-9\n")
	code, _, errOut = runCtl("init-admin", "--db", dbPath)
	if code == 0 || !strings.Contains(errOut, "两行") {
		t.Fatalf("缺少确认行时退出码 = %d，stderr = %q", code, errOut)
	}

	// 以上失败都不应留下账号。
	db, err := store.Open(context.Background(), store.Options{Path: dbPath, Logger: quietTestLogger()})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("统计账号失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("失败路径不应创建账号，实际 %d 个", count)
	}
}

func TestInitAdminJSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	const password = "correct-horse-9"
	withStdin(t, password+"\n"+password+"\n")

	code, out, errOut := runCtl("init-admin", "--db", dbPath, "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	var report struct {
		DatabasePath  string `json:"database_path"`
		UserID        int64  `json:"user_id"`
		Username      string `json:"username"`
		Role          string `json:"role"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	if report.Username != "admin" || report.Role != "admin" || report.UserID == 0 {
		t.Fatalf("报告内容 = %+v", report)
	}
	if report.SchemaVersion < 2 {
		t.Fatalf("schema_version = %d，应至少包含 users/sessions 迁移", report.SchemaVersion)
	}
	if strings.Contains(out, password) || strings.Contains(out, "password_hash") {
		t.Fatal("JSON 输出不得包含密码或哈希")
	}
}

func TestInitAdminRejectsBadUsername(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	withStdin(t, "correct-horse-9\ncorrect-horse-9\n")
	code, _, errOut := runCtl("init-admin", "--db", dbPath, "--username", "1bad name")
	if code != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d（stderr=%q）", code, exitUsage, errOut)
	}
	if !strings.Contains(errOut, "用户名不合法") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// quietTestLogger 返回丢弃输出的日志器（store.Open 要求非空）。
func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writePanelCertForCtl 在临时目录里生成一对自签名证书/私钥（P10.4 的 check-config 用例）。
func writePanelCertForCtl(t *testing.T, dir string) (certPath, keyPath string, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(3003),
		Subject:               pkix.Name{CommonName: "upanel.test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"upanel.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	certPath = filepath.Join(dir, "panel.pem")
	keyPath = filepath.Join(dir, "panel.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return certPath, keyPath, hex.EncodeToString(sum[:])
}

func TestCheckConfigRejectsNonLoopbackWithoutTLS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upanel.json")
	content := `{"web":{"mode":"production","listen_address":"0.0.0.0:9443","allowed_origins":["https://panel.example.com"]},` +
		`"agent":{"socket_path":"/run/upanel/agent.sock"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCtl("check-config", "--config", path)
	if code == 0 {
		t.Fatal("非回环监听且没有 TLS 时必须让 check-config 失败")
	}
	if !strings.Contains(errOut, "tls_cert_path") {
		t.Fatalf("stderr 应指出缺的是 TLS 配置，实际: %q", errOut)
	}
}

func TestCheckConfigPrintsTLSFingerprint(t *testing.T) {
	dir := t.TempDir()
	if !strings.HasPrefix(dir, "/") {
		// 产品按 POSIX 语义校验路径（安装目标永远是 Ubuntu），宿主临时目录在 Windows 上
		// 是 C:\...，check-config 会（正确地）拒绝它。这条用例要读真实文件，没法用假路径，
		// 因此在非 POSIX 宿主上跳过——VM 上的验收脚本会覆盖同一件事。
		t.Skip("宿主临时目录不是 POSIX 绝对路径，这条用例只在 Linux 上跑")
	}
	certPath, keyPath, fingerprint := writePanelCertForCtl(t, dir)
	path := filepath.Join(dir, "upanel.json")
	content := fmt.Sprintf(`{"web":{"mode":"production","listen_address":"127.0.0.1:19443",`+
		`"tls_cert_path":%q,"tls_key_path":%q},"agent":{"socket_path":"/run/upanel/agent.sock"}}`,
		certPath, keyPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCtl("check-config", "--config", path)
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	if !strings.Contains(out, "面板 HTTPS: 已启用") {
		t.Fatalf("输出应说明 HTTPS 已启用: %q", out)
	}
	if !strings.Contains(out, fingerprint) {
		t.Fatalf("输出应打印证书指纹 %s（安装时供用户核对）: %q", fingerprint, out)
	}
	if !strings.Contains(out, "https://127.0.0.1:19443") {
		t.Fatalf("开了 TLS 时推导出来的允许源必须是 https: %q", out)
	}

	code, out, errOut = runCtl("check-config", "--config", path, "--json")
	if code != 0 {
		t.Fatalf("--json 退出码 = %d，stderr = %q", code, errOut)
	}
	var report struct {
		WebTLS *struct {
			Fingerprint string `json:"fingerprint_sha256"`
			SelfSigned  bool   `json:"self_signed"`
		} `json:"web_tls"`
		AllowedOrigins []string `json:"allowed_origins"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	if report.WebTLS == nil || report.WebTLS.Fingerprint != fingerprint {
		t.Fatalf("web_tls 指纹 = %+v，期望 %s", report.WebTLS, fingerprint)
	}
	if !report.WebTLS.SelfSigned {
		t.Fatal("自签名证书应在报告里标明 self_signed=true")
	}
	if len(report.AllowedOrigins) == 0 || !strings.HasPrefix(report.AllowedOrigins[0], "https://") {
		t.Fatalf("允许源 = %v，期望 https 前缀", report.AllowedOrigins)
	}
}

// reset-password 是本机管理员恢复口令的入口，测试要证明四件事：
// 口令真的换了（旧口令失效、新口令可用）、既有会话被作废、审计留痕、
// 以及“不存在的账号/不一致的密码”被明确拒绝。
func TestResetPasswordChangesPasswordAndRevokesSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	const oldPassword = "correct-horse-9"
	const newPassword = "battery-staple-42"

	withStdin(t, oldPassword+"\n"+oldPassword+"\n")
	if code, _, errOut := runCtl("init-admin", "--db", dbPath, "--username", "admin"); code != 0 {
		t.Fatalf("init-admin 退出码 = %d，stderr = %q", code, errOut)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: dbPath, Logger: quietTestLogger()})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	svc, err := auth.NewService(db, auth.Options{})
	if err != nil {
		t.Fatalf("初始化认证服务失败: %v", err)
	}
	issued, err := svc.Login(ctx, auth.LoginInput{Username: "admin", Password: oldPassword})
	if err != nil {
		t.Fatalf("用初始口令登录失败: %v", err)
	}

	withStdin(t, newPassword+"\n"+newPassword+"\n")
	code, out, errOut := runCtl("reset-password", "--db", dbPath, "--username", "admin")
	if code != 0 {
		t.Fatalf("reset-password 退出码 = %d，stderr = %q", code, errOut)
	}
	if !strings.Contains(out, "已重置账号 admin") {
		t.Fatalf("输出 = %q", out)
	}
	if strings.Contains(out, newPassword) || strings.Contains(errOut, newPassword) {
		t.Fatal("输出中出现了明文密码")
	}

	// 旧口令必须失效、新口令必须可用，且重置前的会话必须已经被作废。
	// 先验会话作废（此时还没发生任何新的登录，不会被 Login 自身的轮换掩盖）。
	if _, err := svc.Authenticate(ctx, issued.Token); err == nil {
		t.Fatal("重置口令前的会话在重置后仍然有效")
	}
	if _, err := svc.Login(ctx, auth.LoginInput{Username: "admin", Password: oldPassword}); err == nil {
		t.Fatal("旧口令在重置后仍能登录")
	}
	if _, err := svc.Login(ctx, auth.LoginInput{Username: "admin", Password: newPassword}); err != nil {
		t.Fatalf("新口令无法登录: %v", err)
	}

	// 审计：一条 user.password.reset，actor 是本地 CLI（actor_id 为 NULL）。
	var action string
	var actorID *int64
	if err := db.SQL().QueryRow(
		`SELECT action, actor_id FROM audit_events WHERE action = ? ORDER BY id DESC LIMIT 1`,
		audit.ActionUserPasswordReset).Scan(&action, &actorID); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if actorID != nil {
		t.Fatalf("本地 CLI 的审计 actor_id 应为 NULL，实际 %d", *actorID)
	}
}

func TestResetPasswordRejectsUnknownUserAndMismatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	const password = "correct-horse-9"
	withStdin(t, password+"\n"+password+"\n")
	if code, _, errOut := runCtl("init-admin", "--db", dbPath, "--username", "admin"); code != 0 {
		t.Fatalf("init-admin 退出码 = %d，stderr = %q", code, errOut)
	}

	// 不存在的账号：不能在库里凭空造出一个账号来。
	withStdin(t, "whatever-password-1\nwhatever-password-1\n")
	code, _, errOut := runCtl("reset-password", "--db", dbPath, "--username", "ghost")
	if code == 0 {
		t.Fatal("对不存在的账号 reset-password 必须失败")
	}
	if !strings.Contains(errOut, "没有账号") {
		t.Fatalf("stderr = %q", errOut)
	}

	// 两行密码不一致：拒绝，不改库。
	withStdin(t, "one-password-11\nanother-password-2\n")
	code, _, errOut = runCtl("reset-password", "--db", dbPath, "--username", "admin")
	if code == 0 {
		t.Fatal("两行密码不一致时必须失败")
	}
	if !strings.Contains(errOut, "不一致") {
		t.Fatalf("stderr = %q", errOut)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: dbPath, Logger: quietTestLogger()})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	svc, err := auth.NewService(db, auth.Options{})
	if err != nil {
		t.Fatalf("初始化认证服务失败: %v", err)
	}
	if _, err := svc.Login(ctx, auth.LoginInput{Username: "admin", Password: password}); err != nil {
		t.Fatalf("原口令应仍然有效: %v", err)
	}
}
