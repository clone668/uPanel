package certificates

// certbot 失败的**可诊断性**（P10.4）。
//
// 这条也是真机抓出来的：验收机上测试 ACME（Pebble）重启过，账号只存在它的内存里，
// 于是 certbot 收到 `accountDoesNotExist`。代理把 syscmd 的非零退出当成 error 返回，
// 旧代码只往上面套一句"跑 certbot 失败：syscmd: /usr/bin/certbot 以退出状态 1 结束"，
// certbot 自己那句 detail 被丢掉了——面板上剩下的失败无法行动：看不出是账号、是
// HTTP-01、还是限额，只能登机器翻 /var/log/letsencrypt。
//
// 这里钉两件事：非零退出必须把 certbot 的 stderr 带进错误消息；stderr 为空时明说
// 去哪儿看，而不是留一个空白的退出码。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// exitRunner 模拟"命令跑起来了，但以非零状态退出"，与 syscmd.Run 的真实返回形状一致：
// Result 填好，error 是 *syscmd.ExitError。
type exitRunner struct {
	result syscmd.Result
}

func (r exitRunner) Run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	res := r.result
	res.Program = spec.Program
	return res, &syscmd.ExitError{Program: spec.Program, ExitCode: res.ExitCode}
}

func newCertbotTestOps(t *testing.T, runner Runner) *Ops {
	t.Helper()
	ops, err := NewOps(OpsOptions{
		Secrets:     stubSecrets{},
		Runner:      runner,
		FS:          SystemMaterialFS(),
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		CertbotPath: "/usr/bin/certbot",
		Webroot:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("构造 Ops 失败：%v", err)
	}
	return ops
}

type stubSecrets struct{}

func (stubSecrets) Resolve(context.Context, string) ([]byte, error) {
	return nil, errors.New("测试里不需要秘密")
}

func TestRunCertbotReportsStderrOnExitFailure(t *testing.T) {
	const detail = "acme.messages.Error: urn:ietf:params:acme:error:accountDoesNotExist " +
		":: The request specified an account that does not exist"
	ops := newCertbotTestOps(t, exitRunner{result: syscmd.Result{
		ExitCode: 1,
		Stderr:   "Saving debug log to /var/log/letsencrypt/letsencrypt.log\n" + detail + "\n",
	}})

	_, _, _, err := ops.runCertbot(context.Background(), IssueParams{
		Domain:  "p104.example.test",
		Email:   "ops@example.test",
		Staging: true,
	}, false)
	if err == nil {
		t.Fatal("certbot 非零退出时应当返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "certbot 退出码 1") {
		t.Fatalf("错误消息里应当有退出码，实际：%s", msg)
	}
	if !strings.Contains(msg, "accountDoesNotExist") {
		t.Fatalf("错误消息里应当带上 certbot 自己的原因（accountDoesNotExist），实际：%s", msg)
	}
}

func TestRunCertbotFailsLoudlyWhenReasonMissing(t *testing.T) {
	ops := newCertbotTestOps(t, exitRunner{result: syscmd.Result{ExitCode: 2}})

	_, _, _, err := ops.runCertbot(context.Background(), IssueParams{
		Domain: "p104.example.test",
		Email:  "ops@example.test",
	}, false)
	if err == nil {
		t.Fatal("certbot 非零退出时应当返回错误")
	}
	if !strings.Contains(err.Error(), "/var/log/letsencrypt/") {
		t.Fatalf("没有输出时应当指向 certbot 自己的日志，实际：%s", err.Error())
	}
}
