package shares

// 受管理账号的单测（P08.1）。
//
// 三个解析器对的是**真实系统输出**（样本取自验收 VM，Ubuntu 24.04 / Samba 4.19.5）：
// getent passwd 的一行、passwd -S 的一行、pdbedit -L -v -u 的一段。这些输出的列宽与
// 措辞会随版本变，所以解析规则必须“按字段名取”而不是“按第几列取”。
//
// 另外一条重点：密码明文只经过 smbpasswd 的标准输入，任何情况下都不该出现在
// 命令行参数里——这一条在这里用命令行参数构造器来钉住。

import (
	"strings"
	"testing"
	"time"
)

func TestValidateAccountName(t *testing.T) {
	ok := []string{"alice", "a", "svc-backup", "team_2026", "u1", strings.Repeat("a", 32)}
	for _, name := range ok {
		if err := ValidateAccountName(name); err != nil {
			t.Errorf("账号名 %q 应该合法，却被拒：%v", name, err)
		}
	}
	bad := []string{
		"", "Alice", "1alice", "-alice", "_alice", "alice-", "ali ce", "alice/bob",
		"root", "daemon", "nobody", "www-data", "sudo",
		strings.Repeat("a", 33), "账号", "alice\t", "alice:bob", "alice.bob",
	}
	for _, name := range bad {
		if err := ValidateAccountName(name); err == nil {
			t.Errorf("账号名 %q 应该被拒，却通过了", name)
		}
	}
}

// 真实 getent passwd 行：useradd --system --user-group --home-dir /nonexistent
//
//	--shell /usr/sbin/nologin 之后 getent passwd 打印的样子。
const realPasswdLine = "facl-a:x:999:999::/nonexistent:/usr/sbin/nologin"

func TestParsePasswdLine(t *testing.T) {
	entry, err := parsePasswdLine(realPasswdLine)
	if err != nil {
		t.Fatalf("解析真实 passwd 行失败: %v", err)
	}
	if entry.Name != "facl-a" {
		t.Errorf("用户名是 %q", entry.Name)
	}
	if entry.UID != 999 || entry.GID != 999 {
		t.Errorf("uid/gid 是 %d/%d，期望 999/999", entry.UID, entry.GID)
	}
	if entry.Home != DefaultHome {
		t.Errorf("home 是 %q，期望 %q", entry.Home, DefaultHome)
	}
	if entry.Shell != DefaultShell {
		t.Errorf("shell 是 %q，期望 %q", entry.Shell, DefaultShell)
	}
	if _, err := parsePasswdLine("facl-a:x:999"); err == nil {
		t.Fatal("字段数不对却解析成功了")
	}
	if _, err := parsePasswdLine("a:x:notanum:1::/h:/bin/sh"); err == nil {
		t.Fatal("uid 不是数字却解析成功了")
	}
}

// 真实 passwd -S 行：新建的系统账号密码字段是 "!"，状态是 L。
const realPasswdStatus = "facl-a L 10/22/2026 -1 -1 -1 -1\n"

func TestParseLockState(t *testing.T) {
	cases := []struct {
		out  string
		want LockState
	}{
		{realPasswdStatus, LockLocked},
		{"facl-a P 10/22/2026 0 99999 7 -1\n", LockUsable},
		{"facl-a NP 10/22/2026 -1 -1 -1 -1\n", LockNone},
		{"", LockUnknown},
		{"facl-a", LockUnknown},
		{"facl-a X 10/22/2026 -1 -1 -1 -1\n", LockUnknown},
	}
	for _, c := range cases {
		if got := parseLockState(c.out); got != c.want {
			t.Errorf("passwd -S 输出 %q 解析成 %q，期望 %q", c.out, got, c.want)
		}
	}
}

// 真实 pdbedit -L -v -u facl-a 的输出（截取与本包相关的字段，其余原样保留）。
// 注意 "Password last set" 这一行里时间带时区名，且日期用 "Mon, 02 Jan 2006" 格式。
const realPdbedit = `Unix username:        facl-a
NT username:
Account Flags:        [U          ]
User SID:             S-1-5-21-1876403056-1241942168-2725344342-1001
Primary Group SID:    S-1-5-21-1876403056-1241942168-2725344342-513
Full Name:
Home Directory:       \\ACCEPT\facl-a
HomeDir Drive:
Logon Script:
Profile Path:
Domain:               ACCEPT
Account desc:
Workstations:
Munged dial:
Logon time:           0
Logoff time:          never
Kickoff time:         never
Password last set:    Thu, 22 Oct 2026 09:23:39 UTC
Password can change:  Thu, 22 Oct 2026 09:23:39 UTC
Password must change: never
Last bad password   : 0
Bad password count  : 0
Logon hours         : FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF
`

// 禁用之后同一账号的 Account Flags 变成 [DU         ]。
const realPdbeditDisabled = "Unix username:        facl-a\n" +
	"Account Flags:        [DU         ]\n" +
	"Password last set:    Thu, 22 Oct 2026 09:23:39 UTC\n"

func TestParsePdbedit(t *testing.T) {
	acc, err := parsePdbedit(realPdbedit)
	if err != nil {
		t.Fatalf("解析真实 pdbedit 输出失败: %v", err)
	}
	if acc.Name != "facl-a" {
		t.Errorf("用户名是 %q", acc.Name)
	}
	if acc.Disabled {
		t.Error("Account Flags=[U] 却判定为禁用")
	}
	if !acc.PasswordSet {
		t.Fatal("Password last set 有值却判定为未设置密码")
	}
	want := time.Date(2026, 10, 22, 9, 23, 39, 0, time.UTC)
	if !acc.PasswordSetAt.Equal(want) {
		t.Errorf("密码设置时间是 %s，期望 %s", acc.PasswordSetAt, want)
	}
	if strings.Contains(acc.Flags, "D") {
		t.Errorf("Flags 里不该有 D：%q", acc.Flags)
	}
}

func TestParsePdbeditDisabledFlag(t *testing.T) {
	acc, err := parsePdbedit(realPdbeditDisabled)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !acc.Disabled {
		t.Fatal("Account Flags=[DU] 却没判定成禁用")
	}
}

func TestParsePdbeditNoPassword(t *testing.T) {
	// 刚 smbpasswd -a 出来、还没设密码时 pdbedit 打印的是 0 或 "Thu, 01 Jan 1970"。
	for _, line := range []string{"Password last set:    0", "Password last set:    Thu, 01 Jan 1970 00:00:00 UTC"} {
		out := "Unix username:        facl-a\nAccount Flags:        [U          ]\n" + line + "\n"
		acc, err := parsePdbedit(out)
		if err != nil {
			t.Fatalf("解析失败（%q）: %v", line, err)
		}
		if acc.PasswordSet {
			t.Errorf("%q 应该判定为没有设置密码", line)
		}
	}
}

func TestParsePdbeditGarbage(t *testing.T) {
	if _, err := parsePdbedit(""); err == nil {
		t.Fatal("空输出应该报错")
	}
	if _, err := parsePdbedit("no colons here at all\n"); err == nil {
		t.Fatal("没有可识别字段的输出应该报错")
	}
}

func TestSambaPasswordStdin(t *testing.T) {
	got := string(sambaPasswordStdin([]byte("s3cret")))
	// smbpasswd -s 读两行：新密码、确认密码。
	if got != "s3cret\ns3cret\n" {
		t.Fatalf("标准输入是 %q，期望两行同一个密码", got)
	}
}

func TestSambaPasswordNeverOnCommandLine(t *testing.T) {
	tools := ToolPaths{
		Smbd:      "/usr/sbin/smbd",
		Smbpasswd: "/usr/bin/smbpasswd",
		Pdbedit:   "/usr/bin/pdbedit",
		Setfacl:   "/usr/bin/setfacl",
		Getfacl:   "/usr/bin/getfacl",
	}
	secret := "TopSecret-Passw0rd"
	stdin := sambaPasswordStdin([]byte(secret))
	spec := specSambaSetPassword(tools, "facl-a", stdin)
	if spec.Program != "/usr/bin/smbpasswd" {
		t.Fatalf("执行的是 %q，期望绝对路径的 smbpasswd", spec.Program)
	}
	for _, arg := range spec.Args {
		if strings.Contains(arg, secret) {
			t.Fatalf("密码出现在命令行参数里：%q", arg)
		}
	}
	// -s 从标准输入读；没有 -s 的话 smbpasswd 会去开终端交互，那在代理里会挂住。
	if !hasArg(spec.Args, "-s") {
		t.Fatalf("没有 -s（从标准输入读密码）：%v", spec.Args)
	}
	if hasArg(spec.Args, "-w") || hasArg(spec.Args, "-L") {
		t.Fatalf("命令行里出现了会导出哈希的参数：%v", spec.Args)
	}
	if string(spec.Stdin) != "TopSecret-Passw0rd\nTopSecret-Passw0rd\n" {
		t.Fatalf("标准输入不是两行密码")
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
