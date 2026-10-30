package proxies

// 审计回归用例（第四轮）：回滚不丢权限、目录 fsync 覆盖所有目录项变更。
//
// 这三条对应三个真实缺陷：
//
//  1. 回滚把账号文件按托管配置的 0644 root:root 写回——内容对了，worker 读不到，
//     报告却说"旧配置仍可用"；
//  2. 账号文件的写与删从不 fsync 它所在的目录——掉电丢目录项，配置里引用的
//     htpasswd 凭空消失；
//  3. 配置字节没变、只清了改名遗留的旧文件时，conf.d 不 fsync——同样的掉电窗口。

import (
	"strings"
	"testing"
)

// TestRollbackRestoresAuthFileModeAndOwner 回滚账号文件时必须用它自己的权限与属主。
func TestRollbackRestoresAuthFileModeAndOwner(t *testing.T) {
	fsys := newMemFS()
	fsx := fsys.fs()
	authPath := applyAuthPath()
	oldContent := "alice:$apr1$oldhash"
	fsys.seed(authPath, oldContent)

	rb := newRollback(fsx)
	// 账号文件是 0640（worker 属组在真机上还会 chown，memFS 没有 Chown，这里验权限位）。
	rb.recordWithOwner(authPath, []byte(oldContent), true, 0o640, -1, -1)

	// 模拟新流程已经把它改写成了别的内容。
	if err := fsx.WriteFileSync(authPath, []byte("mallory:$apr1$newhash"), 0o644); err != nil {
		t.Fatalf("模拟改写失败: %v", err)
	}
	if err := rb.apply(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got := fsys.content(authPath); got != oldContent {
		t.Errorf("回滚后的内容 = %q，期望旧内容", got)
	}
	if mode := fsys.modeOf(authPath); mode != 0o640 {
		t.Errorf("回滚后的权限 = %04o，期望 0640（写成 0644/其它值，worker 就可能读不到）", mode)
	}

	// 对照：普通托管文件仍然按 0644 还原。
	confPath := applyConfPath(applyDomain)
	fsys.seed(confPath, "old conf")
	rb2 := newRollback(fsx)
	rb2.record(confPath, []byte("old conf"), true)
	if err := fsx.WriteFileSync(confPath, []byte("new conf"), 0o600); err != nil {
		t.Fatalf("模拟改写失败: %v", err)
	}
	if err := rb2.apply(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if mode := fsys.modeOf(confPath); mode != 0o644 {
		t.Errorf("托管文件回滚后的权限 = %04o，期望 0644", mode)
	}
}

// TestApplyFsyncsAuthDirWhenAuthFileChanges 写账号文件之后必须 fsync 它所在的目录。
func TestApplyFsyncsAuthDirWhenAuthFileChanges(t *testing.T) {
	f := newApplyFixture(t)
	def := applyDefinition(applyDomain)
	def.AccessMode = AccessBasicAuth
	def.BasicAuthUsers = []BasicAuthUser{{
		Name:      "alice",
		SecretRef: "sec_0123456789abcdef0123456789abcdef",
	}}
	f.secrets.values["sec_0123456789abcdef0123456789abcdef"] = "s3cret"

	if _, err := f.execute(applyParamsOf(def, 1)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	authDir := f.paths.AuthDir()
	if idx := f.fsys.opIndex(func(op string) bool { return op == "fsync "+authDir }); idx < 0 {
		t.Errorf("账号文件写完之后没有 fsync %s：%v", authDir, f.fsys.ops)
	}
}

// TestApplyFsyncsConfDirWhenOnlyStaleRemoved 配置字节没变、只清理了改名遗留的旧文件时，
// conf.d 的目录项变了，也要 fsync。
func TestApplyFsyncsConfDirWhenOnlyStaleRemoved(t *testing.T) {
	f := newApplyFixture(t)
	confPath := applyConfPath(applyDomain)

	// 盘上已经是渲染结果（配置不变），但留着一份改名前的旧托管文件。
	rendered, err := RenderSite(applySiteID, applyDefinition(applyDomain), RenderOptions{HtpasswdPath: applyAuthPath()})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	f.fsys.seed(confPath, rendered)
	stalePath := f.paths.ConfDir() + "/upanel-old-name.conf"
	f.fsys.seed(stalePath, ManagedMarker+applySiteID+"\nserver { listen 80; }\n")

	report, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got := report.Data["file_state"]; got != FileStateUnchanged {
		t.Errorf("file_state = %v，期望 %s（配置没变）", got, FileStateUnchanged)
	}
	if f.fsys.exists(stalePath) {
		t.Error("改名遗留的旧文件没有被清掉")
	}
	confDir := f.paths.ConfDir()
	if idx := f.fsys.opIndex(func(op string) bool { return op == "fsync "+confDir }); idx < 0 {
		t.Errorf("只清理旧文件时没有 fsync %s：%v", confDir, f.fsys.ops)
	}
	// 报告里也要看得见这次清理。
	if !strings.Contains(report.RecoveryHint, stalePath) {
		t.Errorf("报告没有提到被清理的旧文件 %s：%q", stalePath, report.RecoveryHint)
	}
}
