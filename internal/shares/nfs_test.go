package shares

// NFS 导出的纯函数单测（P14.4）。
//
// 这一层只测“算得对不对”，不碰文件与命令。期望值全部按真机行为写：
// 选项闭集、行尾标记、`exportfs -v` 的输出形状，都是在 22.04 虚拟机上看过之后
// 才固定下来的（见 docs/evidence/P14）。

import (
	"strings"
	"testing"
)

func nfsDefinition() ShareDefinition {
	return ShareDefinition{
		Name:         "media",
		Protocol:     ProtocolNFS,
		VolumeID:     "vol_0123456789abcdef0123456789abcdef",
		RelativePath: "media",
		Enabled:      true,
		AllowedCIDRs: []string{"10.0.2.0/24"},
		NFSOptions:   &NFSOptions{RootSquash: true, Sync: true},
	}
}

func mustLine(t *testing.T, e ManagedExport) string {
	t.Helper()
	line, err := RenderExportLine(e)
	if err != nil {
		t.Fatalf("渲染导出行失败: %v", err)
	}
	return line
}

func TestRenderExportLineIsDeterministic(t *testing.T) {
	// 网段顺序不同、有重复，渲染出来的字节必须一样：配置哈希就是这份定义的版本标识，
	// 顺序抖一下，“没改”就变成了“改了”。
	a := ManagedExport{
		ShareID: "shr_11111111111111111111111111111111",
		Name:    "media",
		Dir:     "/srv/upanel/volumes/data/media",
		CIDRs:   []string{"10.0.2.0/24", "192.168.1.0/24", "10.0.2.0/24"},
	}
	b := ManagedExport{
		ShareID: "shr_11111111111111111111111111111111",
		Name:    "media",
		Dir:     "/srv/upanel/volumes/data/media",
		CIDRs:   []string{"192.168.1.0/24", "10.0.2.0/24"},
	}
	la, lb := mustLine(t, a), mustLine(t, b)
	if la != lb {
		t.Fatalf("同一组网段、不同顺序渲染出不同的行：\n%s\n%s", la, lb)
	}
	want := "/srv/upanel/volumes/data/media " +
		"10.0.2.0/24(rw,sync,root_squash,no_subtree_check) " +
		"192.168.1.0/24(rw,sync,root_squash,no_subtree_check) # upanel:shr_11111111111111111111111111111111"
	if la != want {
		t.Fatalf("导出行不对：\n得到 %s\n期望 %s", la, want)
	}
}

func TestRenderExportLineReadOnly(t *testing.T) {
	ro := mustLine(t, ManagedExport{
		ShareID:  "shr_11111111111111111111111111111111",
		Dir:      "/srv/x",
		CIDRs:    []string{"10.0.2.0/24"},
		ReadOnly: true,
	})
	if !strings.Contains(ro, "(ro,sync,root_squash,no_subtree_check)") {
		t.Fatalf("只读导出应该写 ro：%s", ro)
	}
	if strings.Contains(ro, "rw") {
		t.Fatalf("只读导出里出现了 rw：%s", ro)
	}
	// no_root_squash / async 不是“关掉的选项”，是代码里根本不存在的取值。
	for _, banned := range []string{"no_root_squash", "async", "insecure", "all_squash"} {
		if strings.Contains(ro, banned) {
			t.Fatalf("导出行里出现了不该存在的选项 %q：%s", banned, ro)
		}
	}
}

func TestRenderExportLineRejectsBadInput(t *testing.T) {
	base := ManagedExport{
		ShareID: "shr_11111111111111111111111111111111",
		Dir:     "/srv/x",
		CIDRs:   []string{"10.0.2.0/24"},
	}
	cases := map[string]func(*ManagedExport){
		"空网段":     func(e *ManagedExport) { e.CIDRs = nil },
		"网段写错":    func(e *ManagedExport) { e.CIDRs = []string{"10.0.2.0/33"} },
		"网段带空串":   func(e *ManagedExport) { e.CIDRs = []string{"10.0.2.0/24", ""} },
		"目录是相对路径": func(e *ManagedExport) { e.Dir = "srv/x" },
		"目录是根":    func(e *ManagedExport) { e.Dir = "/" },
		"目录带空白":   func(e *ManagedExport) { e.Dir = "/srv/a b" },
		"目录带井号":   func(e *ManagedExport) { e.Dir = "/srv/a#b" },
		"目录带引号":   func(e *ManagedExport) { e.Dir = `/srv/a"b` },
		"共享标识不合法": func(e *ManagedExport) { e.ShareID = "shr_XYZ" },
	}
	for name, mutate := range cases {
		e := base
		mutate(&e)
		if _, err := RenderExportLine(e); err == nil {
			t.Errorf("%s：应该被拒，却渲染成功了", name)
		}
	}
}

func TestRenderExportsFileHeaderAndBody(t *testing.T) {
	line := mustLine(t, ManagedExport{
		ShareID: "shr_11111111111111111111111111111111",
		Dir:     "/srv/x",
		CIDRs:   []string{"10.0.2.0/24"},
	})
	text := RenderExportsFile([]ManagedExport{{Line: line}})
	for _, want := range []string{
		"由 uPanel 维护",
		"upanel:<共享 id>",
		"另建一个 .exports 文件",
		line,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("托管文件里缺少 %q：\n%s", want, text)
		}
	}
	if !strings.HasSuffix(text, "\n") {
		t.Fatal("托管文件必须以换行结尾：exports 的最后一行没有换行是另一个故事")
	}
	// 空集合也要是一份合法文件（只有注释）：那代表“面板此刻不导出任何东西”。
	if _, unparsed := ParseExports([]byte(RenderExportsFile(nil))); len(unparsed) != 0 {
		t.Fatalf("空导出文件被解析出读不懂的行：%v", unparsed)
	}
}

func TestParseExports(t *testing.T) {
	data := []byte(`# 注释行
/srv/panel 10.0.2.0/24(rw,sync,root_squash,no_subtree_check) # upanel:shr_11111111111111111111111111111111
/srv/other 192.168.0.0/16(ro,sync,root_squash)

/srv/third 10.0.0.0/8(rw,sync) # 普通注释
garbage line without path
`)
	entries, unparsed := ParseExports(data)
	if len(entries) != 3 {
		t.Fatalf("应该解析出 3 条导出，实际 %d：%+v", len(entries), entries)
	}
	if entries[0].Path != "/srv/panel" || entries[0].ManagedID != "shr_11111111111111111111111111111111" {
		t.Fatalf("第一条解析错了：%+v", entries[0])
	}
	if !entries[0].Managed() {
		t.Fatal("带行尾标记的条目应该被认作面板的")
	}
	if entries[1].Managed() {
		t.Fatalf("没有标记的条目不该被认领：%+v", entries[1])
	}
	if len(entries[1].Specs) != 1 || entries[1].Specs[0] != "192.168.0.0/16(ro,sync,root_squash)" {
		t.Fatalf("客户端部分解析错了：%+v", entries[1])
	}
	if entries[2].Managed() {
		t.Fatal("普通行尾注释不是面板标记，不该被认领")
	}
	// 读不懂的行必须被交出来：靠它才能说出“无法确认有没有冲突”。
	if len(unparsed) != 1 || !strings.HasPrefix(unparsed[0], "garbage") {
		t.Fatalf("读不懂的行没有交出来：%v", unparsed)
	}
}

func TestParseExportsHandlesHashInsidePath(t *testing.T) {
	// `#` 只有前面是空白时才是注释开始。真机的 exports 也是这么判的。
	entries, unparsed := ParseExports([]byte("/srv/a#b 10.0.0.0/8(rw,sync)\n"))
	if len(unparsed) != 0 {
		t.Fatalf("不该有读不懂的行：%v", unparsed)
	}
	if len(entries) != 1 || entries[0].Path != "/srv/a#b" {
		t.Fatalf("路径里的 # 被当成注释了：%+v", entries)
	}
}

func TestParseExportfsV(t *testing.T) {
	// 真机输出（VM 上抓的）：路径一行，缩进的客户端行跟在后面；有的路径没有客户端行。
	out := `/srv/t1
		10.0.2.0/24(sync,wdelay,hide,no_subtree_check,sec=sys,rw,secure,root_squash,no_all_squash)
/srv/upanel-nfs
		<world>(async,wdelay,hide,no_subtree_check,sec=sys,ro,secure,root_squash,no_all_squash)
`
	table, err := ParseExportfsV(out)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if paths := table.ExportTablePaths(); len(paths) != 2 || paths[0] != "/srv/t1" {
		t.Fatalf("解析出的路径不对：%v", paths)
	}
	got := table["/srv/t1"]
	if len(got) != 1 || got[0].Client != "10.0.2.0/24" {
		t.Fatalf("客户端解析错了：%+v", got)
	}
	if !strings.Contains(got[0].Options, "root_squash") {
		t.Fatalf("选项解析错了：%q", got[0].Options)
	}
	if len(table["/srv/upanel-nfs"]) != 1 {
		t.Fatalf("<world> 那一条没解出来：%+v", table["/srv/upanel-nfs"])
	}
}

func TestParseExportfsVEmptyAndBroken(t *testing.T) {
	table, err := ParseExportfsV("")
	if err != nil {
		t.Fatalf("空输出不该报错：%v", err)
	}
	if len(table) != 0 {
		t.Fatalf("空输出应该得到空表：%+v", table)
	}
	if _, err := ParseExportfsV("\t\t10.0.0.0/8(rw)\n"); err == nil {
		t.Fatal("第一行就是客户端行时应该报错（没有路径可挂）")
	}
	// 缩进的一行一定是客户端条目：它没有括号就说明输出不是我们认识的那种，
	// 这时报错比继续解析好——继续解析会把后面的路径挂到错的条目上。
	if _, err := ParseExportfsV("/srv/x\n\t\tgarbage-without-options\n"); err == nil {
		t.Fatal("客户端条目没有括号时应该报错")
	}
}

func TestVerifyExport(t *testing.T) {
	want := ManagedExport{Dir: "/srv/x", CIDRs: []string{"10.0.2.0/24"}}
	ok := ExportTable{"/srv/x": {{Client: "10.0.2.0/24",
		Options: "sync,wdelay,hide,no_subtree_check,sec=sys,rw,secure,root_squash,no_all_squash"}}}
	if why := VerifyExport(ok, want); why != "" {
		t.Fatalf("完全一致的导出被判成不符：%s", why)
	}

	cases := map[string]struct {
		table ExportTable
		want  string
	}{
		"路径不在表里": {ExportTable{}, "没有 /srv/x"},
		"客户端不在": {ExportTable{"/srv/x": {{Client: "192.168.1.0/24",
			Options: "sync,rw,root_squash"}}}, "没有对 10.0.2.0/24 的导出"},
		"实际 no_root_squash": {ExportTable{"/srv/x": {{Client: "10.0.2.0/24",
			Options: "sync,rw,no_root_squash"}}}, "no_root_squash"},
		"实际没有 root_squash": {ExportTable{"/srv/x": {{Client: "10.0.2.0/24",
			Options: "sync,rw"}}}, "没有 root_squash"},
		"实际 async": {ExportTable{"/srv/x": {{Client: "10.0.2.0/24",
			Options: "async,rw,root_squash"}}}, "async"},
		"实际没有 sync": {ExportTable{"/srv/x": {{Client: "10.0.2.0/24",
			Options: "rw,root_squash"}}}, "没有 sync"},
		"要可写却是 ro": {ExportTable{"/srv/x": {{Client: "10.0.2.0/24",
			Options: "sync,ro,root_squash"}}}, "不是 rw"},
	}
	for name, c := range cases {
		why := VerifyExport(c.table, want)
		if why == "" {
			t.Errorf("%s：应该判成不符，却通过了", name)
			continue
		}
		if !strings.Contains(why, c.want) {
			t.Errorf("%s：说法里没有点出关键（%q）：%s", name, c.want, why)
		}
	}

	ro := ManagedExport{Dir: "/srv/x", CIDRs: []string{"10.0.2.0/24"}, ReadOnly: true}
	if why := VerifyExport(ExportTable{"/srv/x": {{Client: "10.0.2.0/24",
		Options: "sync,ro,root_squash"}}}, ro); why != "" {
		t.Fatalf("只读定义对 ro 的实际导出被判成不符：%s", why)
	}
}

func TestExportConflicts(t *testing.T) {
	desired := []ManagedExport{{Name: "media", Dir: "/srv/x"}}
	others := []ExportEntry{
		{Path: "/srv/x", Specs: []string{"10.0.0.0/8(rw)"}}, // 别人的，冲突
		{Path: "/srv/y"}, // 别人的，不冲突
		{Path: "/srv/x", ManagedID: "shr_11111111111111111111111111111111"}, // 面板自己的，不算冲突
	}
	got := ExportConflicts(desired, others)
	if len(got) != 1 {
		t.Fatalf("应该只报一条冲突，实际 %v", got)
	}
	if names := got["/srv/x"]; len(names) != 1 || names[0] != "media" {
		t.Fatalf("冲突里没有点名共享：%v", got)
	}
	if msg := describeExportConflicts(got); !strings.Contains(msg, "duplicated") &&
		!strings.Contains(msg, "整份刷新失败") {
		t.Fatalf("冲突说明没讲清代价：%s", msg)
	}
}

func TestSameDirShares(t *testing.T) {
	desired := []ManagedExport{
		{Name: "media", Dir: "/srv/x"},
		{Name: "same-path", Dir: "/srv/x"},   // 同目录：冲突
		{Name: "docs", Dir: "/srv/y"},        // 独占：不冲突
		{Name: "media-ro", Dir: "/srv/x/ro"}, // 子目录不算同目录：exportfs 认的是完整路径
	}
	got := SameDirShares(desired)
	if len(got) != 1 {
		t.Fatalf("应该只报一条冲突，实际 %v", got)
	}
	names := got["/srv/x"]
	if len(names) != 2 || names[0] != "media" || names[1] != "same-path" {
		t.Fatalf("冲突里没有点名两条共享：%v", got)
	}
	if msg := describeSameDirShares(got); !strings.Contains(msg, NoticeExportPathConflict) ||
		!strings.Contains(msg, "整份刷新失败") {
		t.Fatalf("冲突说明没讲清代价：%s", msg)
	}

	// 多网段把同一条共享渲染成多行时不算冲突——去重是按“目录 + 名字”做的。
	same := SameDirShares([]ManagedExport{
		{Name: "media", Dir: "/srv/x"},
		{Name: "media", Dir: "/srv/x"},
	})
	if len(same) != 0 {
		t.Fatalf("同一条共享被当成了冲突：%v", same)
	}
}

func TestValidateNFSDefinition(t *testing.T) {
	if err := ValidateDefinition(nfsDefinition()); err != nil {
		t.Fatalf("合法的 nfs 定义被拒：%v", err)
	}

	cases := map[string]struct {
		mutate func(*ShareDefinition)
		code   string
	}{
		"没有 nfs_options":    {func(d *ShareDefinition) { d.NFSOptions = nil }, NoticeNFSOptionsInvalid},
		"root_squash=false": {func(d *ShareDefinition) { d.NFSOptions.RootSquash = false }, NoticeNFSOptionsInvalid},
		"sync=false":        {func(d *ShareDefinition) { d.NFSOptions.Sync = false }, NoticeNFSOptionsInvalid},
		"带了 smb_users": {func(d *ShareDefinition) {
			d.SMBUsers = []ShareUser{testUser("sac_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AccessRead)}
		}, NoticeNFSUsersNotAllowed},
		"没有网段": {func(d *ShareDefinition) { d.AllowedCIDRs = nil }, NoticeShareNoCIDRs},
		"向全世界导出": {func(d *ShareDefinition) {
			d.AllowedCIDRs = []string{"0.0.0.0/0"}
		}, NoticeNFSOpenToWorld},
	}
	for name, c := range cases {
		def := nfsDefinition()
		c.mutate(&def)
		err := ValidateDefinition(def)
		if err == nil {
			t.Errorf("%s：应该被拒，却通过了", name)
			continue
		}
		de, ok := err.(*DefinitionError)
		if !ok {
			t.Errorf("%s：错误应该是 *DefinitionError，实际 %T", name, err)
			continue
		}
		if de.Code != c.code {
			t.Errorf("%s：阻塞码是 %s，期望 %s", name, de.Code, c.code)
		}
	}
}

func TestValidateNFSDefinitionRejectsSMBFieldsOnSMB(t *testing.T) {
	// 反向的一条：smb 定义里带 nfs_options 同样要被拒——两种协议的定义不许混着写，
	// 否则“这条共享的选项到底哪一半在生效”就没法回答了。
	def := baseDefinition()
	def.NFSOptions = &NFSOptions{RootSquash: true, Sync: true}
	if err := ValidateDefinition(def); err == nil {
		t.Fatal("smb 定义带 nfs_options 却通过了")
	}
}
