package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deployForTest 落地一份部署并写下记录（P15.3 的形状），返回落地时的配置摘要。
//
// 不加辅助函数的话，每个用例都要重复"渲染 → 计划 → 落盘 → 写记录"四步，
// 而重复出来的那四步里最容易写错的是记录里的 ConfigDigest——它必须与盘上那份
// 逐字相同，否则"漂移"这个判据在测试里永远为真，也就永远测不到真现场。
func deployForTest(t *testing.T, a *Applier, configDir, name string) string {
	t.Helper()
	req := applyRequest(t, configDir)
	report, err := a.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("落地部署失败：%v", err)
	}
	def, err := Normalize(req.Plan.Definition)
	if err != nil {
		t.Fatalf("规范化定义失败：%v", err)
	}
	rec := DeploymentRecord{
		Schema:       RecordSchema,
		DeploymentID: DeploymentIDForName(name),
		Name:         name,
		Definition:   def,
		ConfigDigest: report.ConfigDigest,
		Image: ImageRecord{
			Requested:    def.ImageReference,
			Digest:       "sha256:" + strings.Repeat("a", 64),
			ConfigDigest: report.ConfigDigest,
			ResolvedAt:   time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
		},
		UpdatedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	if err := a.WriteRecord(rec); err != nil {
		t.Fatalf("写部署记录失败：%v", err)
	}
	return report.ConfigDigest
}

// TestInspectReadsDeploymentFacts 是主线：盘上三个文件与 systemd 的两个回答都读出来。
func TestInspectReadsDeploymentFacts(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, applier, configDir := newTestService(t, fake, nil)
	digest := deployForTest(t, applier, configDir, "webapp")

	insp, err := svc.Inspect(context.Background(), "webapp")
	if err != nil {
		t.Fatalf("读部署现场失败：%v", err)
	}
	if insp.Name != "webapp" || insp.ProjectName != "webapp" {
		t.Fatalf("名字 = %q / 项目 = %q", insp.Name, insp.ProjectName)
	}
	if insp.DeploymentID != DeploymentIDForName("webapp") {
		t.Fatalf("部署标识 = %q", insp.DeploymentID)
	}
	if insp.UnitName != "upanel-deploy-webapp.service" {
		t.Fatalf("单元名 = %q", insp.UnitName)
	}
	if !insp.ConfigPresent || insp.ConfigDigest != digest {
		t.Fatalf("配置现场 = present:%v digest:%q（期望 %q）", insp.ConfigPresent, insp.ConfigDigest, digest)
	}
	if insp.Version != digest {
		t.Fatalf("版本令牌 = %q，期望与配置摘要相同（%q）", insp.Version, digest)
	}
	if !insp.RecordPresent || insp.Record == nil {
		t.Fatalf("记录没读出来：present=%v err=%q", insp.RecordPresent, insp.RecordError)
	}
	if insp.Record.Image.Digest == "" {
		t.Fatal("记录里的镜像 digest 空了：页面靠它说清“现在跑的是哪一份镜像”")
	}
	if insp.ConfigDrift {
		t.Fatal("刚落地就报漂移：记录里的摘要与盘上那份应当逐字相同")
	}
	if insp.PreviousConfigPresent {
		t.Fatal("只部署过一次，不该有上一版配置")
	}
	// 这一份刚被 Apply 起来过，所以三个读数都该是肯定的：enabled、active、读到了。
	if !insp.Unit.Known || !insp.Unit.Enabled || !insp.Unit.Active || insp.Unit.Reason != "" {
		t.Fatalf("单元读数 = %+v", insp.Unit)
	}
	if insp.UnitPath == "" || !strings.HasSuffix(insp.UnitPath, "upanel-deploy-webapp.service") {
		t.Fatalf("单元路径 = %q", insp.UnitPath)
	}
}

// TestInspectReportsConfigDrift 测"有人在面板之外动过配置"这一格。
func TestInspectReportsConfigDrift(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "active"}
	svc, applier, configDir := newTestService(t, fake, nil)
	digest := deployForTest(t, applier, configDir, "webapp")

	// 直接往渲染出来的配置里加一行：这是"有人手工改过"的最小现场。
	path := filepath.Join(configDir, "webapp.compose.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n# 手改\n")...), 0o600); err != nil {
		t.Fatalf("改配置失败：%v", err)
	}

	insp, err := svc.Inspect(context.Background(), "webapp")
	if err != nil {
		t.Fatalf("读部署现场失败：%v", err)
	}
	if !insp.ConfigDrift {
		t.Fatal("盘上配置变了，却没有报漂移")
	}
	if insp.ConfigDigest == digest {
		t.Fatal("配置摘要没跟着内容变")
	}
	if insp.Record == nil || insp.Record.ConfigDigest != digest {
		t.Fatal("记录里的摘要被一起改了：记录是上一次执行时写下的，不该跟着盘上变")
	}
}

// TestInspectWithoutRecordStillLists 测"配置还在、记录没了"：能看，不能更新。
func TestInspectWithoutRecordStillLists(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, applier, configDir := newTestService(t, fake, nil)
	deployForTest(t, applier, configDir, "webapp")
	if err := applier.RemoveRecord("webapp"); err != nil {
		t.Fatalf("删记录失败：%v", err)
	}

	insp, err := svc.Inspect(context.Background(), "webapp")
	if err != nil {
		t.Fatalf("没有记录也该能读出现场：%v", err)
	}
	if insp.RecordPresent || insp.Record != nil {
		t.Fatalf("记录不该在：present=%v record=%+v", insp.RecordPresent, insp.Record)
	}
	if !insp.ConfigPresent {
		t.Fatal("配置还在，不该报缺失")
	}
}

// TestInspectSurfacesBrokenRecord 测"盘上有记录文件但读不懂"：
// 这与"没有记录"必须分开报，否则人会以为记录是被删了。
func TestInspectSurfacesBrokenRecord(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, applier, configDir := newTestService(t, fake, nil)
	deployForTest(t, applier, configDir, "webapp")
	path := filepath.Join(configDir, "webapp.deployment.json")
	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o640); err != nil {
		t.Fatalf("写坏记录失败：%v", err)
	}

	insp, err := svc.Inspect(context.Background(), "webapp")
	if err != nil {
		t.Fatalf("坏记录不该让整条读不出来：%v", err)
	}
	if insp.RecordError == "" {
		t.Fatal("坏了却没报原因：页面会显示成“没有记录”")
	}
	if insp.RecordPresent || insp.Record != nil {
		t.Fatal("读不懂的记录不能当成读到了")
	}
}

// TestInspectMissingDeployment 测"问一个没有部署的名字"：哨兵错误，不是空结构。
func TestInspectMissingDeployment(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, _, _ := newTestService(t, fake, nil)

	if _, err := svc.Inspect(context.Background(), "nothing"); !errors.Is(err, ErrNoDeployment) {
		t.Fatalf("期望 ErrNoDeployment，实际 %v", err)
	}
	var validation *ValidationError
	if _, err := svc.Inspect(context.Background(), "Bad Name"); !errors.As(err, &validation) {
		t.Fatalf("非法名字期望 ValidationError，实际 %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("什么都没问到就不该调 systemctl：%v", fake.calls)
	}
}

// TestListEnumeratesConfigDir 测列表：两个后缀都收、名字不合规的进 skipped、按名字排序。
func TestListEnumeratesConfigDir(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, applier, configDir := newTestService(t, fake, nil)
	deployForTest(t, applier, configDir, "webapp")

	// 第二份只有记录（配置被人删了）：它必须出现在列表里，并显示成配置缺失。
	rec := DeploymentRecord{
		Schema:       RecordSchema,
		DeploymentID: DeploymentIDForName("orphan"),
		Name:         "orphan",
		Definition:   withName(t, "orphan"),
		ConfigDigest: "sha256:" + strings.Repeat("b", 64),
		Image:        ImageRecord{Requested: "nginx:1.27", ConfigDigest: "sha256:" + strings.Repeat("b", 64)},
		UpdatedAt:    time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	if err := applier.WriteRecord(rec); err != nil {
		t.Fatalf("写孤儿记录失败：%v", err)
	}
	// 一份名字不合规的配置：不能静默吞掉。
	if err := os.WriteFile(filepath.Join(configDir, "Bad_Name.compose.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写非法文件失败：%v", err)
	}
	// 只有 .prev 的条目：那不是部署（配置与记录都不在），不该列。
	if err := os.WriteFile(filepath.Join(configDir, "ghost.compose.yaml.prev"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写孤立 prev 失败：%v", err)
	}
	// 与部署无关的文件：不是我们的，不列也不报跳过。
	if err := os.WriteFile(filepath.Join(configDir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写无关文件失败：%v", err)
	}

	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	got := make([]string, 0, len(list.Deployments))
	for _, item := range list.Deployments {
		got = append(got, item.Name)
	}
	if strings.Join(got, ",") != "orphan,webapp" {
		t.Fatalf("列表 = %v，期望按名字排序的 [orphan webapp]", got)
	}
	if list.Truncated {
		t.Fatal("两条不该报截断")
	}
	if len(list.Skipped) != 1 || list.Skipped[0].Name != "Bad_Name" {
		t.Fatalf("skipped = %+v，期望只有 Bad_Name", list.Skipped)
	}
	orphan := list.Deployments[0]
	if orphan.ConfigPresent || orphan.RecordPresent != true {
		t.Fatalf("孤儿记录现场 = %+v", orphan)
	}
	if orphan.Unit.Known {
		t.Fatalf("没有单元文件时不该有单元读数：%+v", orphan.Unit)
	}
	if orphan.Unit.Reason == "" {
		t.Fatal("没有读数时必须说清为什么")
	}
	if fake.called("is-enabled upanel-deploy-orphan.service") {
		t.Fatal("单元文件不存在的条目不该去问 systemd")
	}
}

// TestListMissingConfigDir 测"还没有任何部署"：空列表，不是错误。
func TestListMissingConfigDir(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, _, configDir := newTestService(t, fake, nil)
	if err := os.RemoveAll(configDir); err != nil {
		t.Fatalf("删配置目录失败：%v", err)
	}
	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("配置目录不存在不该报错：%v", err)
	}
	if len(list.Deployments) != 0 || len(list.Skipped) != 0 || list.Truncated {
		t.Fatalf("期望空列表，实际 %+v", list)
	}
	if list.Deployments == nil {
		t.Fatal("空列表要序列化成 []，不能是 null")
	}
}

// TestListTruncates 测上限：超过 MaxInspections 条时停下并如实说被截断。
func TestListTruncates(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, _, configDir := newTestService(t, fake, nil)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("建配置目录失败：%v", err)
	}
	// 只写记录文件、内容读不懂：这样既不需要真的落地 200 份部署，
	// 也不会触发 systemctl（记录读坏了，单元文件也不在）。
	for i := 0; i <= MaxInspections+4; i++ {
		name := "n" + pad4(i)
		path := filepath.Join(configDir, name+".deployment.json")
		if err := os.WriteFile(path, []byte("{坏"), 0o640); err != nil {
			t.Fatalf("写文件失败：%v", err)
		}
	}
	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if !list.Truncated {
		t.Fatal("超过上限却没有报截断：页面会以为就这么几份")
	}
	if len(list.Deployments) != MaxInspections {
		t.Fatalf("读出的条数 = %d，期望 %d", len(list.Deployments), MaxInspections)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("这些条目没有单元文件，不该调 systemctl：%d 次", len(fake.calls))
	}
}

// TestInspectionJSONShape 钉住 JSON 字段名：前端与 CONTRACTS §8.9 都按这些名字读。
func TestInspectionJSONShape(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "active"}
	svc, applier, configDir := newTestService(t, fake, nil)
	deployForTest(t, applier, configDir, "webapp")
	insp, err := svc.Inspect(context.Background(), "webapp")
	if err != nil {
		t.Fatalf("读部署现场失败：%v", err)
	}
	data := mustJSON(t, insp)
	want := []string{"name", "deployment_id", "project", "unit", "unit_path", "config_path",
		"config_present", "config_digest", "version", "previous_config_present",
		"record_present", "config_drift", "unit_state", "record"}
	for _, key := range want {
		if !strings.Contains(data, `"`+key+`":`) {
			t.Fatalf("JSON 里少了 %q：%s", key, data)
		}
	}
	for _, key := range []string{"known", "enabled", "active"} {
		if !strings.Contains(string(mustJSON(t, insp.Unit)), `"`+key+`":`) {
			t.Fatalf("unit_state 里少了 %q", key)
		}
	}
}

// withName 复制黄金定义并换名字（列表里要有多份不同名字的部署）。
func withName(t *testing.T, name string) Definition {
	t.Helper()
	def := goldenDefinition()
	def.Name = name
	out, err := Normalize(def)
	if err != nil {
		t.Fatalf("规范化定义失败：%v", err)
	}
	return out
}

// mustJSON 把值序列化成字符串（只用于断言字段名）。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	return string(data)
}

func pad4(n int) string {
	s := []byte("0000")
	for i := 3; i >= 0 && n > 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}
