package accounts

import (
	"path"
	"strings"
	"testing"
)

func TestSudoTemplatesAreFixed(t *testing.T) {
	if len(sudoTemplates) == 0 {
		t.Fatal("模板表是空的")
	}
	for _, tpl := range sudoTemplates {
		if tpl.ID == "" || tpl.DisplayName == "" || tpl.Purpose == "" {
			t.Fatalf("模板 %q 少了说明字段", tpl.ID)
		}
		if len(tpl.Commands) == 0 {
			t.Fatalf("模板 %q 没有命令", tpl.ID)
		}
		for _, cmd := range tpl.Commands {
			if err := validateSudoCommand(cmd); err != nil {
				t.Fatalf("模板 %q 的命令不合格：%v", tpl.ID, err)
			}
			// “不接受任意 sudoers 文本”的底线：不能出现 ALL 命令、不能出现 shell。
			upper := strings.ToUpper(cmd)
			for _, bad := range []string{" ALL", "/BIN/SH", "/BIN/BASH", "SUDO "} {
				if strings.Contains(upper, bad) {
					t.Fatalf("模板 %q 的命令里有 %q：%s", tpl.ID, bad, cmd)
				}
			}
		}
	}
}

func TestLookupSudoTemplateRefusesUnknown(t *testing.T) {
	if _, err := LookupSudoTemplate("all"); err == nil {
		t.Fatal("all 这种明显的万能模板必须拒绝")
	} else if code := codeOf(err); code != NoticeSudoTemplateInvalid {
		t.Fatalf("码是 %q", code)
	}
	if _, err := LookupSudoTemplate(""); err == nil {
		t.Fatal("空模板标识必须拒绝")
	}
	if _, err := LookupSudoTemplate("system_status"); err != nil {
		t.Fatalf("闭集里的模板被拒了：%v", err)
	}
}

func TestValidateSudoCommandRefusesWildcards(t *testing.T) {
	cases := []string{
		"/usr/bin/journalctl -u *",
		"/usr/bin/systemctl status ?",
		"/usr/bin/systemctl status [a-z]",
		"/bin/sh -c id",
		"/usr/bin/systemctl status --no-pager; id",
		"/usr/bin/systemctl status\n/bin/sh",
		"systemctl status",
	}
	for _, cmd := range cases {
		if err := validateSudoCommand(cmd); err == nil {
			t.Fatalf("%q 应该被拒绝", cmd)
		}
	}
}

func TestRenderSudoersShape(t *testing.T) {
	rule := SudoRule{
		ID:          "sru_0123456789abcdef0123456789abcdef",
		AccountName: "alice",
		Template:    mustTemplate(t, "system_status"),
	}
	content, hash, err := RenderSudoers(rule)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.HasPrefix(text, "# 由 uPanel 生成") {
		t.Fatalf("第一行不是说明：%q", strings.SplitN(text, "\n", 2)[0])
	}
	if !strings.Contains(text, claimMarker+rule.ID) {
		t.Fatalf("没有认领标记：\n%s", text)
	}
	if !strings.Contains(text, "alice ALL = (root) NOPASSWD: /usr/bin/systemctl status --no-pager") {
		t.Fatalf("命令行长这样：\n%s", text)
	}
	if strings.Contains(text, "NOPASSWD: ALL") {
		t.Fatalf("渲染出了万能规则：\n%s", text)
	}
	if len(hash) != 64 {
		t.Fatalf("哈希长度 %d", len(hash))
	}
	// 同一份规则渲染两次必须逐字节相同（哈希就是规则版本）。
	again, hash2, err := RenderSudoers(rule)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != text || hash2 != hash {
		t.Fatal("同一份规则渲染出了不同结果")
	}
	// 换模板就换版本。
	other := rule
	other.Template = mustTemplate(t, "journal_recent")
	if _, hash3, err := RenderSudoers(other); err != nil {
		t.Fatal(err)
	} else if hash3 == hash {
		t.Fatal("换了模板哈希却没变")
	}
}

func TestRenderSudoersRefusesBadInput(t *testing.T) {
	good := SudoRule{
		ID:          "sru_0123456789abcdef0123456789abcdef",
		AccountName: "alice",
		Template:    mustTemplate(t, "system_status"),
	}
	cases := []struct {
		name string
		rule SudoRule
		want string
	}{
		{"账号名非法", SudoRule{ID: good.ID, AccountName: "Alice; id", Template: good.Template}, "小写字母开头"},
		{"规则标识没有前缀", SudoRule{ID: "x", AccountName: "alice", Template: good.Template}, "sru_"},
		{"模板不在闭集", SudoRule{ID: good.ID, AccountName: "alice", Template: SudoTemplate{ID: "all"}}, "不在受支持的清单里"},
	}
	for _, c := range cases {
		if _, _, err := RenderSudoers(c.rule); err == nil {
			t.Fatalf("%s: 期望拒绝", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 报错里没提到 %q：%v", c.name, c.want, err)
		}
	}
}

func TestParseSudoersRoundTrips(t *testing.T) {
	for _, id := range SudoTemplateIDs() {
		rule := SudoRule{
			ID:          "sru_0123456789abcdef0123456789abcdef",
			AccountName: "bob",
			Template:    mustTemplate(t, id),
		}
		content, _, err := RenderSudoers(rule)
		if err != nil {
			t.Fatal(err)
		}
		gotRule, gotTpl, err := ParseSudoers(content)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if gotRule != rule.ID || gotTpl != id {
			t.Fatalf("%s: 读回来 rule=%q tpl=%q", id, gotRule, gotTpl)
		}
	}
}

func TestParseSudoersRefusesForeignContent(t *testing.T) {
	foreign := []byte("# 我自己写的\nbob ALL = (ALL) NOPASSWD: ALL\n")
	_, _, err := ParseSudoers(foreign)
	if err == nil {
		t.Fatal("没有认领标记的文件必须拒绝")
	}
	if code := codeOf(err); code != NoticeSudoFileDamaged {
		t.Fatalf("码是 %q", code)
	}

	missingTemplate := []byte(claimMarker + "srule_0123456789abcdef0123456789abcdef\nbob ALL = (root) NOPASSWD: /bin/true\n")
	if _, _, err := ParseSudoers(missingTemplate); err == nil {
		t.Fatal("读不出模板标识的规则文件必须拒绝")
	}

	staleTemplate := []byte(claimMarker + "srule_0123456789abcdef0123456789abcdef\n# 模板：all（万能）——x\nbob ALL = (root) NOPASSWD: ALL\n")
	if _, _, err := ParseSudoers(staleTemplate); err == nil {
		t.Fatal("模板不在闭集时必须拒绝")
	} else if code := codeOf(err); code != NoticeSudoTemplateInvalid {
		t.Fatalf("码是 %q，期望 %q", code, NoticeSudoTemplateInvalid)
	}
}

func TestSudoersPathUnderDir(t *testing.T) {
	if got := SudoersPath("/etc/sudoers.d", "alice"); got != "/etc/sudoers.d/upanel-alice" {
		t.Fatalf("路径是 %q", got)
	}
	if got := SudoersPath("/etc/sudoers.d/", "alice"); got != "/etc/sudoers.d/upanel-alice" {
		t.Fatalf("带斜杠时路径是 %q", got)
	}
	if got := SudoersPath("", "alice"); got != "/etc/sudoers.d/upanel-alice" {
		t.Fatalf("默认目录时路径是 %q", got)
	}
	// sudoers.d 会忽略带点的文件名，所以**文件名**里不能有点。
	if base := path.Base(SudoersPath("", "alice")); strings.Contains(base, ".") {
		t.Fatalf("文件名 %q 里出现了点", base)
	}
}

func mustTemplate(t *testing.T, id string) SudoTemplate {
	t.Helper()
	tpl, err := LookupSudoTemplate(id)
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}
