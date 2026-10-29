package accounts

import (
	"encoding/json"
	"strings"
	"testing"

	"upanel/internal/shares"
)

func TestNormalizeAppliesDefaults(t *testing.T) {
	got, err := Definition{Name: "alice", Type: TypeRegular}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "alice" {
		t.Fatalf("展示名没补上：%q", got.DisplayName)
	}
	if got.ShellProfile != ProfileDisabled {
		t.Fatalf("默认 profile 是 %q，期望 disabled", got.ShellProfile)
	}
	if got.ShellProfile.Shell() != ShellDisabled {
		t.Fatalf("disabled 的 shell 是 %q", got.ShellProfile.Shell())
	}
	if got.PasswordSecretRef != "" {
		t.Fatalf("没给秘密却冒出来一个：%q", got.PasswordSecretRef)
	}
}

func TestNormalizeRefusals(t *testing.T) {
	cases := []struct {
		name string
		def  Definition
		code string
		want string
	}{
		{"名字为空", Definition{Type: TypeRegular}, NoticeAccountNameInvalid, "不能为空"},
		{"名字大写", Definition{Name: "Alice", Type: TypeRegular}, NoticeAccountNameInvalid, "小写字母开头"},
		{"名字带空格", Definition{Name: "al ice", Type: TypeRegular}, NoticeAccountNameInvalid, "只能包含"},
		{"名字以横线结尾", Definition{Name: "alice-", Type: TypeRegular}, NoticeAccountNameInvalid, "不能以连字符结尾"},
		{"名字太长", Definition{Name: strings.Repeat("a", 33), Type: TypeRegular}, NoticeAccountNameInvalid, "过长"},
		{"保留名 root", Definition{Name: "root", Type: TypeRegular}, NoticeAccountReserved, "保留名"},
		{"面板身份", Definition{Name: "upanel-web", Type: TypeRegular}, NoticeAccountReserved, "保留名"},
		{"终端身份", Definition{Name: "upanel-shell", Type: TypeRegular}, NoticeAccountReserved, "保留名"},
		{"类型为空", Definition{Name: "alice"}, NoticeAccountTypeInvalid, "不能为空"},
		{"类型不在闭集", Definition{Name: "alice", Type: "admin"}, NoticeAccountTypeInvalid, "regular 或 share"},
		{"profile 不在闭集", Definition{Name: "alice", Type: TypeRegular, ShellProfile: "bash"}, NoticeAccountShellProfileInvalid, "disabled 或 restricted"},
		{
			"共享账号不许能登录",
			Definition{Name: "bob", Type: TypeShare, ShellProfile: ProfileRestricted},
			NoticeAccountShellProfileInvalid, "不提供登录",
		},
		{"秘密形状", Definition{Name: "alice", Type: TypeRegular, PasswordSecretRef: "hunter2"}, NoticeAccountSecretInvalid, "形状不对"},
		{"展示名控制字符", Definition{Name: "alice", Type: TypeRegular, DisplayName: "a\nb"}, NoticeAccountDisplayNameInvalid, "控制字符"},
		{"展示名太长", Definition{Name: "alice", Type: TypeRegular, DisplayName: strings.Repeat("字", 65)}, NoticeAccountDisplayNameInvalid, "过长"},
	}
	for _, c := range cases {
		_, err := c.def.Normalize()
		if err == nil {
			t.Fatalf("%s: 期望拒绝，实际通过", c.name)
		}
		if code := codeOf(err); code != c.code {
			t.Fatalf("%s: 码是 %q，期望 %q（消息：%v）", c.name, code, c.code, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 报错里没提到 %q，实际是 %v", c.name, c.want, err)
		}
	}
}

func TestNormalizeAcceptsSecretRef(t *testing.T) {
	got, err := Definition{
		Name:              "alice",
		Type:              TypeRegular,
		PasswordSecretRef: "sec_0123456789abcdef0123456789abcdef",
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordSecretRef == "" {
		t.Fatal("秘密引用被丢掉了")
	}
}

// 展示名全空白在 Normalize 里会被补成账号名（那是“没说”而不是“说错了”），
// 但直接调用校验时必须拒绝——它还会被别的路径用到。
func TestValidateDisplayNameRefusesBlank(t *testing.T) {
	err := ValidateDisplayName("   ")
	if err == nil {
		t.Fatal("全空白应该被拒绝")
	}
	if code := codeOf(err); code != NoticeAccountDisplayNameInvalid {
		t.Fatalf("码是 %q", code)
	}
}

// 与共享账号的那一份对齐：形状规则必须一致，任何一边单独放宽都要在这里红。
//
// 两份校验各有各的上下文（那一份混着 uid < 1000、nologin 这些共享专用前提），但
// “名字长什么样”只能有一种答案——同一个 Linux 名字空间里没有第二种写法。
func TestNameShapeAgreesWithShares(t *testing.T) {
	names := []string{
		"alice", "a", "a1", "a_b", "a-b", "a.b", "Alice", "1alice", "_alice",
		"alice-", "al ice", "al\tice", "", strings.Repeat("a", 32),
		strings.Repeat("a", 33), "root", "nobody", "www-data", "upanel",
		"alice@host", "alicé", "a/b", "a:b",
	}
	for _, name := range names {
		mine := ValidateName(name) == nil
		theirs := shares.ValidateAccountName(name) == nil
		if mine != theirs {
			t.Fatalf("%q: accounts 判 %v，shares 判 %v——两份名字规则漂了", name, mine, theirs)
		}
	}
}

// UID 区间：普通账号 >= 1000，共享账号（--system）在其下。同一张表里靠这一条分开。
func TestUIDRangeSeparatesTypes(t *testing.T) {
	if UIDInRange(999) {
		t.Fatal("999 不该算普通账号区间")
	}
	if !UIDInRange(1000) || !UIDInRange(MaxManagedUID) {
		t.Fatal("区间端点应该算在内")
	}
	if UIDInRange(MaxManagedUID + 1) {
		t.Fatal("上沿之外不该算")
	}
	if UIDInRange(0) {
		t.Fatal("uid 0 永远不算——那是 root")
	}
}

// 定义里没有 uid/gid/groups/home 这些字段，提交它们必须被解码层拒绝（未知字段直接
// 拒绝，CONTRACTS.md 第 5 节）。这里锁住“类型上根本没有它们”。
func TestDefinitionHasNoPrivilegedFields(t *testing.T) {
	raw, err := json.Marshal(Definition{Name: "alice", Type: TypeRegular})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"uid", "gid", "groups", "home", "shell_path"} {
		if strings.Contains(string(raw), `"`+forbidden+`"`) {
			t.Fatalf("定义序列化里出现了 %q：%s", forbidden, raw)
		}
	}
}
