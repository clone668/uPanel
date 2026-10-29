package deploy

// 部署记录（P15.3）：<name>.deployment.json 与 <name>.compose.yaml.prev。
//
// 这两份文件是更新的全部依据：定义从记录里来（Compose 配置反推不出定义），回切字节
// 从 .prev 里来（重新渲染得到的是"此刻的秘密"，不是上一版）。所以这里验的重点是
// **拒绝**：来路不明的记录、对不上名字的记录、不自洽的定义，一律整份不用。

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sampleRecord 造一份能通过校验的记录。
func sampleRecord() DeploymentRecord {
	def := goldenDefinition()
	return DeploymentRecord{
		Schema:       RecordSchema,
		DeploymentID: DeploymentIDForName(def.Name),
		Name:         def.Name,
		Definition:   def,
		ConfigDigest: "sha256:" + strings.Repeat("a", 64),
		Image: ImageRecord{
			Requested:    "nginx:1.27",
			Digest:       "sha256:" + strings.Repeat("b", 64),
			RepoDigests:  []string{"nginx@sha256:" + strings.Repeat("c", 64)},
			ConfigDigest: "sha256:" + strings.Repeat("a", 64),
			ResolvedAt:   time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
		},
		UpdatedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
}

// TestRecordValidateRejectsBadRecords：记录只要有一条不对就整份不用。
func TestRecordValidateRejectsBadRecords(t *testing.T) {
	if err := sampleRecord().Validate(); err != nil {
		t.Fatalf("样例记录本身必须合法：%v", err)
	}
	cases := map[string]func(*DeploymentRecord){
		"格式版本": func(r *DeploymentRecord) { r.Schema = RecordSchema + 1 },
		"名字":   func(r *DeploymentRecord) { r.Name = "../etc/passwd" },
		"标识":   func(r *DeploymentRecord) { r.DeploymentID = DeploymentIDForName("otherapp") },
		"定义名":  func(r *DeploymentRecord) { r.Definition.Name = "otherapp" },
		"定义": func(r *DeploymentRecord) {
			r.Definition.HostPort = 0
		},
	}
	for name, mutate := range cases {
		rec := sampleRecord()
		mutate(&rec)
		if err := rec.Validate(); err == nil {
			t.Fatalf("%s 不对时必须拒绝：%+v", name, rec)
		}
	}
}

// TestRecordRoundTrip：写下去读回来还是那一份，权限只给 root，不带明文秘密。
func TestRecordRoundTrip(t *testing.T) {
	applier, _, configDir := newTestApplier(t, &fakeSystemctl{})
	rec := sampleRecord()
	if err := applier.WriteRecord(rec); err != nil {
		t.Fatalf("写记录失败：%v", err)
	}
	path := filepath.Join(configDir, "webapp.deployment.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("记录没落盘：%v", err)
	}
	// Windows 上 Perm() 恒为 0666：这条只能在 Linux 上有意义，但它是要求的一部分。
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != recordFileMode {
			t.Fatalf("记录权限 = %o，期望 %o", perm, recordFileMode)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读记录失败：%v", err)
	}
	if strings.Contains(string(raw), "pass$word") {
		t.Fatal("记录里不许有明文秘密：它只该记 secret_ref")
	}
	got, found, err := applier.ReadRecord("webapp")
	if err != nil || !found {
		t.Fatalf("读回记录失败：found=%v err=%v", found, err)
	}
	if got.DeploymentID != rec.DeploymentID || got.Image.Digest != rec.Image.Digest {
		t.Fatalf("读回的记录不对：%+v", got)
	}
	if got.Definition.ImageReference != "nginx:1.27" {
		t.Fatalf("定义没存下来：%+v", got.Definition)
	}

	if _, found, err := applier.ReadRecord("otherapp"); err != nil || found {
		t.Fatalf("没有记录时该 found=false：found=%v err=%v", found, err)
	}
}

// TestWriteRecordRefusesInvalid：写之前就校验，别把坏记录落盘。
func TestWriteRecordRefusesInvalid(t *testing.T) {
	applier, _, configDir := newTestApplier(t, &fakeSystemctl{})
	rec := sampleRecord()
	rec.DeploymentID = DeploymentIDForName("otherapp")
	if err := applier.WriteRecord(rec); err == nil {
		t.Fatal("标识与名字不一致的记录必须写不进去")
	}
	if _, err := os.Stat(filepath.Join(configDir, "webapp.deployment.json")); !os.IsNotExist(err) {
		t.Fatalf("被拒绝的记录不该落盘：%v", err)
	}
}

// TestRequireRecordDistinguishesMissingFromBroken：没有记录与记录坏了是两件事。
func TestRequireRecordDistinguishesMissingFromBroken(t *testing.T) {
	applier, _, configDir := newTestApplier(t, &fakeSystemctl{})
	_, err := applier.requireRecord("webapp")
	if !errors.Is(err, ErrRecordMissing) {
		t.Fatalf("没记录时必须是 ErrRecordMissing：%v", err)
	}
	if !strings.Contains(err.Error(), "deployment.remove") {
		t.Fatalf("拒绝要说清出路（删掉重建）：%v", err)
	}

	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("建配置目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "webapp.deployment.json"), []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatalf("造坏记录失败：%v", err)
	}
	if _, err := applier.requireRecord("webapp"); err == nil {
		t.Fatal("坏记录必须拒绝")
	}
}

// TestPreviousConfigRoundTrip：回切字节原样进出，权限 0600（里面有明文秘密）。
func TestPreviousConfigRoundTrip(t *testing.T) {
	applier, _, configDir := newTestApplier(t, &fakeSystemctl{})
	if _, found, err := applier.ReadPreviousConfig("webapp"); err != nil || found {
		t.Fatalf("没有上一版时该 found=false：found=%v err=%v", found, err)
	}
	original := []byte("services:\n  webapp:\n    image: nginx:1.27\n")
	if err := applier.SavePreviousConfig("webapp", original); err != nil {
		t.Fatalf("存上一版失败：%v", err)
	}
	path := filepath.Join(configDir, "webapp.compose.yaml.prev")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("上一版没落盘：%v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != configFileMode {
			t.Fatalf("上一版权限 = %o，期望 %o", perm, configFileMode)
		}
	}
	got, found, err := applier.ReadPreviousConfig("webapp")
	if err != nil || !found {
		t.Fatalf("读上一版失败：found=%v err=%v", found, err)
	}
	if string(got) != string(original) {
		t.Fatalf("回切字节被改过：%q", got)
	}
}

// TestRemoveRecordRemovesBoth：删除部署时记录与上一版一起清掉，别的文件不动。
func TestRemoveRecordRemovesBoth(t *testing.T) {
	applier, _, configDir := newTestApplier(t, &fakeSystemctl{})
	if err := applier.WriteRecord(sampleRecord()); err != nil {
		t.Fatalf("写记录失败：%v", err)
	}
	if err := applier.SavePreviousConfig("webapp", []byte("旧字节")); err != nil {
		t.Fatalf("存上一版失败：%v", err)
	}
	keep := filepath.Join(configDir, "webapp.compose.yaml")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("建配置目录失败：%v", err)
	}
	if err := os.WriteFile(keep, []byte("配置"), configFileMode); err != nil {
		t.Fatalf("造现场失败：%v", err)
	}
	if err := applier.RemoveRecord("webapp"); err != nil {
		t.Fatalf("删记录失败：%v", err)
	}
	for _, gone := range []string{"webapp.deployment.json", "webapp.compose.yaml.prev"} {
		if _, err := os.Stat(filepath.Join(configDir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s 应该被删掉：%v", gone, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("渲染配置不归这一层删：%v", err)
	}
	// 再删一次不报错（幂等）：删除流程重试时不该因为记录已经不在而失败。
	if err := applier.RemoveRecord("webapp"); err != nil {
		t.Fatalf("重复删除必须幂等：%v", err)
	}
}

// TestReadConfigReturnsDigest：读配置时顺手给出与部署版本同一个口径的摘要。
func TestReadConfigReturnsDigest(t *testing.T) {
	applier, _, configDir := newTestApplier(t, &fakeSystemctl{})
	data := []byte("services: {}\n")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("建配置目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "webapp.compose.yaml"), data, configFileMode); err != nil {
		t.Fatalf("造配置文件失败：%v", err)
	}
	got, digest, found, err := applier.ReadConfig("webapp")
	if err != nil || !found {
		t.Fatalf("读配置失败：found=%v err=%v", found, err)
	}
	if string(got) != string(data) || digest != RenderedDigest(data) {
		t.Fatalf("配置读回不对：%q %q", got, digest)
	}
	if _, _, found, err := applier.ReadConfig("otherapp"); err != nil || found {
		t.Fatalf("没有配置时该 found=false：found=%v err=%v", found, err)
	}
}
