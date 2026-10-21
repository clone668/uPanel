package processes

import (
	"strings"
	"testing"
)

// 脱敏是“不给每个能打开页面的人发凭据”的那道门，所以这里用真实样例逐字比对。

func TestSanitizeCommandRealSecretArgv(t *testing.T) {
	args := parseCmdline([]byte(fixture(t, "secretcmd.cmdline")))
	got := SanitizeCommand(args, "perl")
	want := "perl -e sleep 15 -- --password=*** --token=***"
	if got != want {
		t.Fatalf("脱敏结果 = %q，期望 %q", got, want)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "abc123def456") {
		t.Fatalf("秘密没有被脱敏：%q", got)
	}
}

func TestSanitizeCommandWithoutArgv(t *testing.T) {
	// 内核线程与僵尸没有命令行：用 [comm] 表示“没有 argv 可看”（与 ps 一致）。
	if got := SanitizeCommand(nil, "pool_workqueue_release"); got != "[pool_workqueue_release]" {
		t.Errorf("内核线程摘要 = %q", got)
	}
	if got := SanitizeCommand([]string{}, "python3"); got != "[python3]" {
		t.Errorf("僵尸摘要 = %q", got)
	}
	if got := SanitizeCommand(nil, ""); got != "[未知进程]" {
		t.Errorf("空 comm 摘要 = %q", got)
	}
}

func TestSanitizeCommandRealArgvShapes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		comm string
		want string
	}{
		{
			name: "sshd 把整条命令塞进 argv[0]",
			args: parseCmdline([]byte(fixture(t, "sshd.cmdline"))),
			comm: "sshd",
			want: "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups",
		},
		{
			name: "argv[0] 是绝对路径时只留基名",
			args: []string{"/usr/bin/systemctl", "show", "cron.service"},
			comm: "systemctl",
			want: "systemctl show cron.service",
		},
		{
			name: "路径里带空格时原样保留（取基名会切出无意义的尾巴）",
			args: parseCmdline([]byte(fixture(t, "trickcomm.cmdline"))),
			comm: "a)b c",
			want: "/tmp/a)b c 15",
		},
		{
			name: "`--flag value` 两个参数形式",
			args: []string{"mysql", "--user", "root", "--password", "hunter2", "--port", "3306"},
			comm: "mysql",
			want: "mysql --user root --password *** --port 3306",
		},
		{
			name: "环境变量形式",
			args: []string{"svc", "API_KEY=abc123", "PORT=8080"},
			comm: "svc",
			want: "svc API_KEY=*** PORT=8080",
		},
		{
			name: "不敏感的长参数不被吞掉",
			args: []string{"java", "-Xmx2g", "-jar", "app.jar"},
			comm: "java",
			want: "java -Xmx2g -jar app.jar",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeCommand(tc.args, tc.comm); got != tc.want {
				t.Fatalf("摘要 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeCommandStripsControlCharsAndTruncates(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := SanitizeCommand([]string{"app", "line1\nline2\x1b[31m", long}, "app")
	if strings.ContainsAny(got, "\n\x1b") {
		t.Fatalf("摘要里仍有控制字符：%q", got)
	}
	runes := []rune(got)
	if len(runes) > maxCommandRunes+1 {
		t.Fatalf("摘要长度 %d 超过上限 %d", len(runes), maxCommandRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("被截断的摘要应以省略号结尾：%q", got)
	}
}

func TestSensitiveNameMatching(t *testing.T) {
	sensitive := []string{"password", "Password", "pwd", "secret", "token", "api-key", "api_key", "apikey", "access_key", "private-key", "credential", "bearer", "auth"}
	for _, name := range sensitive {
		if !sensitiveName.MatchString(name) {
			t.Errorf("%q 应被判定为敏感名字", name)
		}
	}
	notSensitive := []string{"user", "port", "path", "config", "log-level", "total"}
	for _, name := range notSensitive {
		if sensitiveName.MatchString(name) {
			t.Errorf("%q 不该被判定为敏感名字", name)
		}
	}
}
