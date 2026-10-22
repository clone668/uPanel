package shares

// 托管配置的解析/渲染/校验单测（P08.2）。
//
// 这些函数不碰系统：它们是纯的，所以每条断言都可以指着一个具体的行为差别。
// 真机上的行为（testparm 真的怎么回、reload 真的有没有生效）在
// docs/evidence/P08/2026-10-22-p08.2/ 里有记录，不在这里假装。

import (
	"os"
	"strings"
	"testing"

	"upanel/internal/syscmd"
)

func TestParseSmbConf(t *testing.T) {
	conf := ParseSmbConf([]byte(`# 注释里的 include = /etc/samba/upanel.d/upanel.conf 不算
[global]
   workgroup = WORKGROUP
   include = /etc/samba/upanel.d/upanel.conf
[Data]
   path = /srv/data
`))
	if !conf.IncludePresent {
		t.Error("生效的 include 没有被认出来")
	}
	if conf.TrailingNewline != true {
		t.Error("末尾换行没有被认出来")
	}
	names := []string{}
	for _, s := range conf.Sections {
		names = append(names, strings.ToLower(s))
	}
	want := []string{"global", "data"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("段名是 %v，期望 %v", conf.Sections, want)
	}

	// 被注释掉的 include 不算接入：这一条如果反过来，页面会显示“已接入”而实际没接。
	commented := ParseSmbConf([]byte("[global]\n;include = " + ManagedConfigPath + "\n"))
	if commented.IncludePresent {
		t.Error("被注释掉的 include 被当成了接入")
	}

	// 指向别处的 include 不算。
	other := ParseSmbConf([]byte("[global]\ninclude = /etc/samba/other.conf\n"))
	if other.IncludePresent {
		t.Error("别的 include 被当成了托管接入")
	}
}

func TestPlanIncludeIsMinimalAndIdempotent(t *testing.T) {
	current := []byte("[global]\n   workgroup = WORKGROUP\n")
	candidate, changed := PlanInclude(current, ParseSmbConf(current))
	if !changed {
		t.Fatal("没有接入时应该改")
	}
	// 最小改动：原有内容原样保留在整个文件的最前面。
	if !strings.HasPrefix(string(candidate), string(current)) {
		t.Fatalf("原有内容被改动了：\n%s", candidate)
	}
	added := strings.TrimPrefix(string(candidate), string(current))
	if strings.Count(added, "include = ") != 1 {
		t.Fatalf("新增的不止一行 include：\n%q", added)
	}
	if !strings.Contains(added, ManagedConfigPath) {
		t.Errorf("include 没有指向托管文件：%q", added)
	}

	// 已接入时一个字节都不动。
	again, changedAgain := PlanInclude(candidate, ParseSmbConf(candidate))
	if changedAgain {
		t.Error("已接入还说改了")
	}
	if string(again) != string(candidate) {
		t.Error("已接入时内容被改动了")
	}

	// 末尾没有换行时不能把两行粘成一行。
	noNewline := []byte("[global]\n   workgroup = WORKGROUP")
	fixed, _ := PlanInclude(noNewline, ParseSmbConf(noNewline))
	if !strings.Contains(string(fixed), "WORKGROUP\n\n#") && !strings.Contains(string(fixed), "WORKGROUP\n#") {
		t.Errorf("追加后两行粘在一起了：\n%q", fixed)
	}
}

func TestRenderManagedConfigDeterministic(t *testing.T) {
	shares := []ManagedShare{
		{Name: "zeta", Block: "[zeta]\n   path = /srv/zeta"},
		{Name: "Alpha", Block: "[Alpha]\n   path = /srv/alpha"},
	}
	first := RenderManagedConfig(shares)
	second := RenderManagedConfig([]ManagedShare{shares[1], shares[0]})
	if first != second {
		t.Fatal("同一组定义渲染出两份不同的字节：配置哈希因此失去意义")
	}
	// 段名按小写排序：Alpha 在 zeta 前面。
	if strings.Index(first, "[Alpha]") > strings.Index(first, "[zeta]") {
		t.Errorf("段没有按名字排序：\n%s", first)
	}
	// [global] 段必须先出现，且带着三项加固。
	globalIdx := strings.Index(first, "[global]")
	if globalIdx < 0 || globalIdx > strings.Index(first, "[Alpha]") {
		t.Errorf("[global] 位置不对：\n%s", first)
	}
	for _, kv := range managedGlobalSettings {
		if !strings.Contains(first, kv.Key+" = "+kv.Value) {
			t.Errorf("缺少全局项 %s = %s", kv.Key, kv.Value)
		}
	}
	// 空共享列表也要渲染出可用的文件（只有 [global]）：摘掉最后一份共享时就是这样。
	empty := RenderManagedConfig(nil)
	if !strings.Contains(empty, "[global]") || strings.Contains(empty, "[zeta]") {
		t.Errorf("空配置渲染不对：\n%s", empty)
	}
}

func TestSectionNamesSkipsGlobal(t *testing.T) {
	names := SectionNames([]byte("[global]\n[a]\n[b]\n"))
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("段名是 %v", names)
	}
}

func TestConfiguredSectionsFollowsIncludes(t *testing.T) {
	files := map[string]string{
		"/etc/samba/smb.conf":        "[global]\ninclude = /etc/samba/conf.d/one.conf\n",
		"/etc/samba/conf.d/one.conf": "[One]\n   path = /srv/one\n",
	}
	read := func(name string) ([]byte, error) {
		if body, ok := files[name]; ok {
			return []byte(body), nil
		}
		return nil, os.ErrNotExist
	}
	sections, unresolved := ConfiguredSections([]byte(files["/etc/samba/smb.conf"]), read)
	if _, ok := sections["one"]; !ok {
		t.Errorf("include 里的段没有被算进来：%v", sections)
	}
	if len(unresolved) != 0 {
		t.Errorf("不该有解析不了的 include：%v", unresolved)
	}

	// 解析不了的 include（% 变量）要报出来，而不是当成“没有冲突”。
	withVar, unresolved2 := ConfiguredSections([]byte("[global]\ninclude = /etc/samba/%G.conf\n"), read)
	if len(unresolved2) != 1 {
		t.Errorf("含变量的 include 没有被列为无法解析：%v（%v）", unresolved2, withVar)
	}

	// 托管文件自己不算“既有配置”：否则每次应用都会与自己的段撞名。
	own := map[string]string{
		"/etc/samba/smb.conf": "[global]\ninclude = " + ManagedConfigPath + "\n",
		ManagedConfigPath:     "[mine]\n   path = /srv/mine\n",
	}
	sections3, _ := ConfiguredSections([]byte(own["/etc/samba/smb.conf"]), func(name string) ([]byte, error) {
		if body, ok := own[name]; ok {
			return []byte(body), nil
		}
		return nil, os.ErrNotExist
	})
	if _, ok := sections3["mine"]; ok {
		t.Error("托管文件里的段被当成了既有配置")
	}
}

func TestTestparmVerdict(t *testing.T) {
	ok := syscmd.Result{ExitCode: 0, Stderr: "Loaded services file OK.\n"}
	if err := TestparmVerdict(ok, nil); err != nil {
		t.Errorf("合格的输出被判成失败：%v", err)
	}

	// 未知参数：退出码是 0，但配置被静默忽略——必须判失败。
	unknownKey := syscmd.Result{
		ExitCode: 0,
		Stderr:   "Loaded services file OK.\nUnknown parameter encountered: \"foo\"\nIgnoring unknown parameter \"foo\"\n",
	}
	if err := TestparmVerdict(unknownKey, nil); err == nil {
		t.Error("未知参数被放过了：静默忽略的配置会被当成校验通过")
	}

	// 没有成功标记。
	silent := syscmd.Result{ExitCode: 0}
	if err := TestparmVerdict(silent, nil); err == nil {
		t.Error("没有成功标记却被当成通过")
	}

	// 非零退出。
	broken := syscmd.Result{ExitCode: 1, Stderr: "Error loading services."}
	if err := TestparmVerdict(broken, &syscmd.ExitError{ExitCode: 1}); err == nil {
		t.Error("退出码 1 被当成通过")
	}
}

func TestOnlyRemoval(t *testing.T) {
	old := RenderManagedConfig([]ManagedShare{
		{Name: "keep", Block: "[keep]\n   path = /srv/keep"},
		{Name: "gone", Block: "[gone]\n   path = /srv/gone"},
	})
	// 只摘掉 gone：允许。
	if !onlyRemoval([]ManagedShare{{Name: "keep", Block: "[keep]\n   path = /srv/keep"}}, old) {
		t.Error("纯摘除被判定为不是摘除")
	}
	// 新增一段：不允许。
	if onlyRemoval([]ManagedShare{{Name: "new", Block: "[new]\n   path = /srv/new"}}, old) {
		t.Error("新增被判定成了摘除")
	}
	// 改动已有段的路径：不允许。
	if onlyRemoval([]ManagedShare{{Name: "keep", Block: "[keep]\n   path = /srv/other"}}, old) {
		t.Error("改动被判定成了摘除")
	}
	// 不是面板写的文件（开头对不上）：不允许。
	if onlyRemoval(nil, "[global]\n   workgroup = X\n") {
		t.Error("别人的 smb.conf 被当成了托管文件")
	}
}

func TestSameUserList(t *testing.T) {
	cases := []struct {
		got  string
		want []string
		ok   bool
	}{
		{"alice,bob", []string{"alice", "bob"}, true},
		{"alice, bob", []string{"bob", "alice"}, true},
		{"alice", []string{"alice", "bob"}, false},
		{"alice,bob,carol", []string{"alice", "bob"}, false},
		{"", nil, true},
	}
	for _, c := range cases {
		if got := sameUserList(c.got, c.want); got != c.ok {
			t.Errorf("sameUserList(%q, %v) = %v，期望 %v", c.got, c.want, got, c.ok)
		}
	}
}

func TestConfigDrift(t *testing.T) {
	records := []ShareRecord{
		{Definition: ShareDefinition{Name: "applied", Enabled: true}, ConfigApplied: true},
		{Definition: ShareDefinition{Name: "missing", Enabled: true}, ConfigApplied: true},
		{Definition: ShareDefinition{Name: "extra", Enabled: false}},
	}
	drift := configDrift(records, []string{"applied", "extra"})
	if len(drift) != 2 || drift[0] != "extra" || drift[1] != "missing" {
		t.Errorf("漂移是 %v，期望 [extra missing]", drift)
	}
}
