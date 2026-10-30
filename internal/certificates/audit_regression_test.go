package certificates

// 审计回归用例（第四轮）：备份旧证书/私钥之后，内存里的私钥副本必须清零。
//
// backupExisting 自己 ReadFile 出来的旧私钥副本不在调用方 zero(keyPEM) 的覆盖
// 范围内——不清它，"私钥用完即弃"这条纪律就在备份这条路上破了一个洞。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// TestBackupExistingZeroesOldKeyMaterial 备份写完之后，旧私钥的内存副本是全零。
func TestBackupExistingZeroesOldKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	certPath := path.Join(dir, "app.example.com.pem")
	keyPath := path.Join(dir, "app.example.com.key")
	backupDir := path.Join(dir, "backups")

	// fakeFS 持有 ReadFile 返回的同一份切片：backupExisting 清零它，这里看得见。
	certBytes := []byte("old public cert")
	keyBytes := []byte("OLD PRIVATE KEY MATERIAL")
	files := map[string][]byte{certPath: certBytes, keyPath: keyBytes}
	var backups map[string][]byte
	fs := MaterialFS{
		ReadFile: func(name string) ([]byte, error) {
			data, ok := files[name]
			if !ok {
				return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
			}
			return data, nil
		},
		WriteFileSync: func(name string, data []byte, _ os.FileMode) error {
			if backups == nil {
				backups = map[string][]byte{}
			}
			backups[name] = append([]byte(nil), data...)
			return nil
		},
		MkdirAll: func(string, os.FileMode) error { return nil },
	}

	stamp, err := backupExisting(fs, MaterialPaths{BackupDir: backupDir},
		[]string{certPath, keyPath}, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	if stamp == "" {
		t.Fatal("有旧文件却没有产生备份名")
	}
	if len(backups) != 2 {
		t.Fatalf("备份文件数 = %d，期望 2", len(backups))
	}
	// 私钥的内存副本必须已经清零；证书是公开材料，清零它无副作用、一并断言。
	if !bytes.Equal(keyBytes, make([]byte, len(keyBytes))) {
		t.Errorf("备份后旧私钥的内存副本没有清零：%q", keyBytes)
	}
	if !bytes.Equal(certBytes, make([]byte, len(certBytes))) {
		t.Errorf("备份后旧证书的内存副本没有清零：%q", certBytes)
	}
	// 落盘的备份内容必须仍然是原文——清零的是内存，不是备份。
	if got := backups[path.Join(backupDir, "20261024T080000Z-app.example.com.key")]; string(got) != "OLD PRIVATE KEY MATERIAL" {
		t.Errorf("备份内容被改动了：%q", got)
	}
}

// ---- 第五轮 ----

// auditStubSecrets / auditStubRunner 是 NewOps 的必需件；这些用例走不到它们。
type auditStubSecrets struct{}

func (auditStubSecrets) Resolve(context.Context, string) ([]byte, error) {
	return nil, errors.New("这个用例不该解析私钥")
}

type auditStubRunner struct{}

func (auditStubRunner) Run(context.Context, syscmd.Spec, syscmd.Limits) (syscmd.Result, error) {
	return syscmd.Result{}, errors.New("这个用例不该跑命令")
}

// TestDecodeResultRejectsMalformedReport 报告存在却解析不了时必须报错。
//
// 修法前：succeeded 的任务带着一份坏报告，decodeResult 静默当成"没有报告"，于是
// 落进失败分支，recordFailure 在一张其实签成了的证书上写下 last_error——账本上
// 同时出现"任务成功"与"最后一次尝试失败"两个互相矛盾的事实。
func TestDecodeResultRejectsMalformedReport(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	applier, err := NewJobApplier(svc, quietLogger())
	if err != nil {
		t.Fatalf("构造账本回调失败: %v", err)
	}
	params, err := json.Marshal(ImportParams{
		CertificateID: "crt_" + strings.Repeat("a", 32),
		Domain:        "ledger.example.com",
		CertPEM:       "placeholder",
		KeySecretRef:  "sec_test",
	})
	if err != nil {
		t.Fatalf("编码参数失败: %v", err)
	}
	job := JobFact{
		ID:     "job_malformed",
		Action: ActionImport,
		Status: "succeeded",
		Result: json.RawMessage(`{"outcome": "verified",`),
	}
	err = applier.Apply(context.Background(), job, params)
	if err == nil {
		t.Fatal("报告损坏的 succeeded 任务必须报错，不能静默走失败分支")
	}
	if !strings.Contains(err.Error(), "报告无法解析") {
		t.Fatalf("错误没有指出报告解析失败: %v", err)
	}

	// 对照：报告为空（失败任务的常态）仍然走失败分支，不报错。
	job.Result = nil
	job.Status = "failed"
	if err := applier.Apply(context.Background(), job, params); err != nil {
		t.Fatalf("空报告的失败任务不该报错: %v", err)
	}
}

// TestCheckCertificateRejectsUnknownState 事实不完整的记录不能绑给站点。
//
// 修法前：RenewalUnknown（not_after 零值或指纹缺失，数据被手工改坏时出现）被
// CheckCertificate 放行，绑定在面板上"成功"，直到代理渲染参数时才被
// TLSMaterial.Validate 以一句对不上号的"参数不合法"挡下。
func TestCheckCertificateRejectsUnknownState(t *testing.T) {
	svc, db := newTestService(t, Options{})
	ctx := context.Background()
	fx := newCertFixture(t, "unknown.example.test",
		fixedNow().Add(-time.Hour), fixedNow().Add(90*24*time.Hour), "")
	cert, err := svc.Create(ctx, CreateInput{
		Domain:       "unknown.example.test",
		Info:         fx.info,
		Source:       SourceManual,
		KeySecretRef: "sec_test",
	})
	if err != nil {
		t.Fatalf("建证书记录失败: %v", err)
	}
	// 模拟被手工改坏的记录：not_after 变成零值。
	if _, err := db.ExecContext(ctx,
		`UPDATE certificates SET not_after = '0001-01-01T00:00:00Z' WHERE id = ?`, cert.ID); err != nil {
		t.Fatalf("改坏记录失败: %v", err)
	}
	err = svc.CheckCertificate(ctx, cert.ID, "unknown.example.test")
	if !errors.Is(err, ErrStateUnknown) {
		t.Fatalf("状态不明的证书必须被拒绝（ErrStateUnknown），得到: %v", err)
	}
}

// TestAssessIssueBlocksNon200Probe 预检对非 200 探测必须是阻塞项，不是告警。
//
// 修法前：AssessIssue 只给一条"先让它试一次"的告警并放行，而 ExecuteIssue 对
// 非 200 必拒——用户看到的是预检通过、执行必败，告警文案还承诺了一次永远不会
// 发生的尝试。
func TestAssessIssueBlocksNon200Probe(t *testing.T) {
	webroot := t.TempDir()
	ops, err := NewOps(OpsOptions{
		Secrets: auditStubSecrets{},
		Runner:  auditStubRunner{},
		FS:      SystemMaterialFS(),
		Webroot: webroot,
		// 探测文件的内容就是 URL 的最后一段；按 403 把它答回去，让探测得到
		// "内容对、状态不对"——正是 AssessIssue 里 status != 200 的分支。
		HTTPGet: func(_ context.Context, url string) (int, string, error) {
			return 403, url[strings.LastIndex(url, "/")+1:], nil
		},
		CertbotAvailable: func() (bool, string) { return true, "" },
	})
	if err != nil {
		t.Fatalf("构造证书动作失败: %v", err)
	}
	assess, err := ops.AssessIssue(context.Background(), IssueParams{
		CertificateID: "crt_" + strings.Repeat("b", 32),
		Domain:        "probe.example.com",
		Email:         "ops@example.com",
		Staging:       true,
	}, ActionIssue)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if assess.Allowed {
		t.Fatal("探测返回非 200 时预检必须拒绝（执行路径对同一种情况必拒）")
	}
	found := false
	for _, b := range assess.Blockers {
		if b.Code == CodeACMEReachability {
			found = true
		}
	}
	if !found {
		t.Fatalf("阻塞项里缺少 %s：%v", CodeACMEReachability, assess.Blockers)
	}
}
