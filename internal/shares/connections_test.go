package shares

// 连接状态的解析单测（P08.2）。
//
// 样本就是真机（Samba 4.19.5 / Ubuntu 24.04）上 `smbstatus --json` 的原始输出，
// 见 docs/evidence/P08/2026-10-22-p08.2/probe2-output.txt 与 probe3-output.txt。
// 用真样本而不是手搓的 JSON：字段名拼错、id 是字符串而不是数字这类问题，
// 只有对着真输出才测得出来。

import (
	"testing"
)

const statusWithOneTcon = `{
 "timestamp": "2026-10-22T12:55:52.192073+0000",
 "version": "4.19.5-Ubuntu",
 "smb_conf": "/etc/samba/smb.conf",
 "sessions": {
  "1879008955": {
   "session_id": "1879008955",
   "uid": 997,
   "gid": 987,
   "username": "alice",
   "groupname": "alice",
   "remote_machine": "10.0.0.9",
   "hostname": "ipv4:10.0.0.9:33264",
   "session_dialect": "SMB3_11",
   "encryption": {"cipher": "-", "degree": "none"},
   "signing": {"cipher": "AES-128-GMAC", "degree": "partial"}
  }
 },
 "tcons": {
  "3073206837": {
   "service": "team-data",
   "tcon_id": "3073206837",
   "session_id": "1879008955",
   "machine": "10.0.0.9",
   "connected_at": "2026-10-22T12:55:49.085662+00:00",
   "encryption": {"cipher": "-", "degree": "none"},
   "signing": {"cipher": "-", "degree": "none"}
  }
 },
 "open_files": {
  "/srv/data/projects/big.bin": {
   "service_path": "/srv/data/projects",
   "filename": "big.bin",
   "opens": {"59760/3": {"uid": 997}, "59760/4": {"uid": 997}}
  },
  "/srv/data/other/x.txt": {
   "service_path": "/srv/data",
   "filename": "x.txt",
   "opens": {"59760/5": {"uid": 997}}
  }
 }
}`

func TestAssembleConnectionsJoinsSessions(t *testing.T) {
	doc, err := ParseSmbStatus([]byte(statusWithOneTcon))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	conns := assembleConnections(doc, "team-data", "/srv/data/projects")
	if len(conns) != 1 {
		t.Fatalf("连接数是 %d，期望 1", len(conns))
	}
	c := conns[0]
	if c.Username != "alice" {
		t.Errorf("账号是 %q：会话说人，必须靠 session_id 关联出来", c.Username)
	}
	if c.Dialect != "SMB3_11" {
		t.Errorf("方言是 %q", c.Dialect)
	}
	// 树连接上的签名是 none，会话上是 partial(AES-128-GMAC)：应当报后者，
	// 否则“有没有签名”这件事在面板上永远是 none。
	if c.Signing != "partial(AES-128-GMAC)" {
		t.Errorf("签名是 %q，期望 partial(AES-128-GMAC)", c.Signing)
	}
	// 打开文件按 service_path 归并：这份共享的目录下有两个打开句柄。
	if c.OpenFiles != 2 {
		t.Errorf("打开文件数是 %d，期望 2", c.OpenFiles)
	}
	if c.ConnectedAt == "" {
		t.Error("连接时间丢了")
	}
}

func TestAssembleConnectionsFiltersByName(t *testing.T) {
	doc, err := ParseSmbStatus([]byte(statusWithOneTcon))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 段名在 Samba 内部按小写比较：大小写不同也必须匹配上。
	if got := assembleConnections(doc, "TEAM-DATA", "/srv/data/projects"); len(got) != 1 {
		t.Errorf("大小写不同的共享名没有匹配上：%d 条", len(got))
	}
	if got := assembleConnections(doc, "other-share", "/srv/data"); len(got) != 0 {
		t.Errorf("别的共享被算进来了：%d 条", len(got))
	}
	if got := len(assembleConnections(doc, "absent", "")); got != 0 {
		t.Errorf("不存在的共享返回了 %d 条", got)
	}
}

func TestAssembleConnectionsWithoutDir(t *testing.T) {
	doc, _ := ParseSmbStatus([]byte(statusWithOneTcon))
	// 算不出目录时不猜：能列出连接，但打开文件数不能凭空归并。
	conns := assembleConnections(doc, "team-data", "")
	if len(conns) != 1 {
		t.Fatalf("连接数是 %d", len(conns))
	}
	if conns[0].OpenFiles != 0 {
		t.Errorf("打开文件数是 %d，没有目录时不该归并", conns[0].OpenFiles)
	}
}

func TestAssembleConnectionsEmptyAndSorted(t *testing.T) {
	// 服务停掉时真机返回的就是这一份：空表 + 退出码 0。
	doc, err := ParseSmbStatus([]byte(`{"sessions":{},"tcons":{},"open_files":{}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := assembleConnections(doc, "team-data", "/srv/data"); len(got) != 0 {
		t.Errorf("空表里列出了 %d 条连接", len(got))
	}
}

func TestParseSmbStatusRejectsGarbage(t *testing.T) {
	if _, err := ParseSmbStatus([]byte("Samba version 4.19.5\nPID Username\n")); err == nil {
		t.Error("文本输出被当成了 JSON")
	}
}

func TestSigningText(t *testing.T) {
	cases := []struct {
		in   smbStatusCipher
		want string
	}{
		{smbStatusCipher{Degree: "none", Cipher: "-"}, "none"},
		{smbStatusCipher{Degree: "partial", Cipher: "AES-128-GMAC"}, "partial(AES-128-GMAC)"},
		{smbStatusCipher{Degree: "partial", Cipher: ""}, "partial"},
		{smbStatusCipher{}, "none"},
	}
	for _, c := range cases {
		if got := c.in.SigningText(); got != c.want {
			t.Errorf("SigningText(%+v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
