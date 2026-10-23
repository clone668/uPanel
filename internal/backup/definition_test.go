package backup

import (
	"errors"
	"strings"
	"testing"
)

// validLocal 返回一条合法的本地定义。
func validLocal() Definition {
	return Definition{
		Name:              "夜间根文件",
		Backend:           BackendLocal,
		Location:          Location{RootID: "root1", RelativePath: "backups/nightly"},
		PasswordSecretRef: "sec_0123456789abcdef0123456789abcdef",
	}
}

// validSFTP 返回一条合法的 SFTP 定义。
func validSFTP() Definition {
	return Definition{
		Name:    "远端站点",
		Backend: BackendSFTP,
		Location: Location{
			Host:               "backup.example.com",
			Username:           "upanel",
			RemotePath:         "/srv/restic/site",
			HostKeyFingerprint: "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
		},
		PasswordSecretRef:   "sec_0123456789abcdef0123456789abcdef",
		CredentialSecretRef: "sec_fedcba9876543210fedcba9876543210",
	}
}

func TestNormalizeLocalRejectsSFTPFields(t *testing.T) {
	def := validLocal()
	def.Location.Host = "example.com"
	_, err := def.Normalize()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("期望 ErrInvalid，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "host") {
		t.Fatalf("错误里应该指出是哪个字段：%v", err)
	}
}

func TestNormalizeSFTPRequiresCredential(t *testing.T) {
	def := validSFTP()
	def.CredentialSecretRef = ""
	if _, err := def.Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SFTP 后端缺私钥引用必须被拒绝，得到 %v", err)
	}
}

func TestNormalizeLocalRejectsCredential(t *testing.T) {
	def := validLocal()
	def.CredentialSecretRef = "sec_fedcba9876543210fedcba9876543210"
	if _, err := def.Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("本地后端带私钥引用必须被拒绝（它不会被用到，说明定义是拼凑的），得到 %v", err)
	}
}

func TestNormalizeRejectsParentTraversal(t *testing.T) {
	for _, rel := range []string{"../etc", "backups/../../etc", "a//b", "a/./b", `a\b`} {
		def := validLocal()
		def.Location.RelativePath = rel
		if _, err := def.Normalize(); err == nil {
			t.Fatalf("相对路径 %q 必须被拒绝", rel)
		}
	}
}

func TestNormalizeAcceptsAndTrims(t *testing.T) {
	def := validLocal()
	def.Name = "  夜间根文件  "
	def.Location.RelativePath = " backups/nightly "
	out, err := def.Normalize()
	if err != nil {
		t.Fatalf("合法定义被拒绝: %v", err)
	}
	if out.Name != "夜间根文件" {
		t.Fatalf("名字没有去空白：%q", out.Name)
	}
	if out.Location.RelativePath != "backups/nightly" {
		t.Fatalf("相对路径没有去空白：%q", out.Location.RelativePath)
	}
}

func TestNormalizeSFTPDefaultsPort(t *testing.T) {
	out, err := validSFTP().Normalize()
	if err != nil {
		t.Fatalf("合法 SFTP 定义被拒绝: %v", err)
	}
	if out.Location.Port != DefaultSFTPPort {
		t.Fatalf("端口默认值应为 %d，得到 %d", DefaultSFTPPort, out.Location.Port)
	}
}

func TestIdentityFingerprintTracksLocation(t *testing.T) {
	a, err := validSFTP().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.Location.HostKeyFingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

	fpA, err := a.IdentityFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fpA, "sha256:") || len(fpA) != len("sha256:")+64 {
		t.Fatalf("指纹形状不对：%q", fpA)
	}
	fpB, err := b.IdentityFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fpA == fpB {
		t.Fatal("换了主机密钥指纹，身份指纹必须跟着变")
	}
	// 同一个定义两次必须一致（否则预检永远失效）。
	again, err := a.IdentityFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if again != fpA {
		t.Fatalf("同一份定义的身份指纹不稳定：%q != %q", again, fpA)
	}
}

func TestResticRepository(t *testing.T) {
	def, err := validLocal().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got := def.resticRepository("/srv/data/backups/nightly"); got != "/srv/data/backups/nightly" {
		t.Fatalf("本地仓库地址应为绝对路径，得到 %q", got)
	}
	sftp, err := validSFTP().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	got := sftp.resticRepository("/srv/restic/site")
	want := "sftp:upanel@backup.example.com:/srv/restic/site"
	if got != want {
		t.Fatalf("SFTP 仓库地址应为 %q，得到 %q", want, got)
	}
	// 端口**不**进仓库地址：restic 的 `sftp:user@host:path` 里没有端口字段，
	// 端口由 SFTPCommand 的 `-p` 决定（两份事实只能有一个来源）。
	sftp.Location.Port = 2222
	got = sftp.resticRepository("/srv/restic/site")
	if strings.Contains(got, "2222") {
		t.Fatalf("仓库地址不该带端口（端口在 sftp.command 里），得到 %q", got)
	}
	if !strings.Contains(SFTPCommand("/usr/bin/ssh", "/tmp/kh", "/tmp/key", "host", 2222, "u"), "-p 2222") {
		t.Fatal("sftp.command 必须带上端口")
	}
}

func TestDisplayNameOmitsSecretRefs(t *testing.T) {
	def, err := validSFTP().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	name := def.DisplayName()
	for _, secret := range []string{def.PasswordSecretRef, def.CredentialSecretRef} {
		if strings.Contains(name, secret) {
			t.Fatalf("展示名里不能出现秘密引用：%q", name)
		}
	}
}
