package shares

// 定义渲染与配置版本的单测（P08.1）。
//
// 这些用例的重点不是“函数能跑”，而是三件在真机上很难发现、但在配置里很致命的事：
//   1. 渲染结果是**确定的**：同样的定义必须得到逐字节相同的段，否则配置版本会乱跳，
//      P08.2 的“只改自己那一段”就无从判断有没有变化；
//   2. 空 smb_users 必须被拒绝：Samba 里 valid users 为空会让段退化成对 guest 开放；
//   3. 路径解析只做词法拼接且必须留在卷内：'..' 与绝对路径是唯一能让人把共享指到
//      系统盘上的输入。
//
// 期望里的渲染结果与 VM 上的实测输出（docs/evidence/P08）逐字节一致。

import (
	"strings"
	"testing"
)

func testUser(id, access string) ShareUser { return ShareUser{AccountID: id, Access: access} }

func baseDefinition() ShareDefinition {
	return ShareDefinition{
		Name:         "team-data",
		Protocol:     ProtocolSMB,
		VolumeID:     "vol_0123456789abcdef0123456789abcdef",
		RelativePath: "projects/team",
		Enabled:      true,
		ReadOnly:     false,
		SMBUsers: []ShareUser{
			testUser("sac_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AccessWrite),
			testUser("sac_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", AccessRead),
		},
	}
}

func TestValidateName(t *testing.T) {
	ok := []string{"team-data", "a", "data_2026", "A-Z_09", strings.Repeat("x", 64)}
	for _, name := range ok {
		if err := ValidateName(name); err != nil {
			t.Errorf("名字 %q 应该合法，却被拒：%v", name, err)
		}
	}
	bad := []string{
		"", " team", "team ", "te am", "team/data", "team\\data", "team.data",
		"global", "homes", "printers", "print$", "ipc$",
		strings.Repeat("x", 65), "团队", "team\t", "-lead", "team\n",
	}
	for _, name := range bad {
		if err := ValidateName(name); err == nil {
			t.Errorf("名字 %q 应该被拒，却通过了", name)
		}
	}
}

func TestValidateRelativePath(t *testing.T) {
	ok := []string{"", "a", "a/b", "a/b/c", "a-b_c.d"}
	for _, p := range ok {
		if err := ValidateRelativePath(p); err != nil {
			t.Errorf("相对路径 %q 应该合法，却被拒：%v", p, err)
		}
	}
	bad := []string{"/etc", "/", "a/", "a//b", "./a", "a/../b", "..", "a/..", "a/b/../..", strings.Repeat("a", 256)}
	for _, p := range bad {
		if err := ValidateRelativePath(p); err == nil {
			t.Errorf("相对路径 %q 应该被拒，却通过了", p)
		}
	}
}

func TestValidateDefinitionRejectsEmptyUsers(t *testing.T) {
	def := baseDefinition()
	def.SMBUsers = nil
	err := ValidateDefinition(def)
	if err == nil {
		t.Fatal("smb_users 为空却通过了校验：空名单在 Samba 里会退化成 guest")
	}
	if !strings.Contains(err.Error(), "smb_users") {
		t.Fatalf("错误消息没说清是 smb_users 的问题：%v", err)
	}
	var de *DefinitionError
	if !asDefinitionError(err, &de) {
		t.Fatalf("错误必须是 *DefinitionError，实际 %T", err)
	}
	if de.Code != NoticeShareNoUsers {
		t.Fatalf("阻塞码是 %s，期望 %s", de.Code, NoticeShareNoUsers)
	}
}

func TestValidateDefinitionRejectsProtocolAndAccess(t *testing.T) {
	def := baseDefinition()
	def.Protocol = ProtocolNFS
	if err := ValidateDefinition(def); err == nil {
		t.Fatal("protocol=nfs 在 P08.1 应该被拒")
	}

	def = baseDefinition()
	def.SMBUsers[0].Access = "admin"
	if err := ValidateDefinition(def); err == nil {
		t.Fatal("access 不在 read/write 内却通过了")
	}

	def = baseDefinition()
	def.SMBUsers[1].AccountID = def.SMBUsers[0].AccountID
	if err := ValidateDefinition(def); err == nil {
		t.Fatal("同一个账号出现两次却通过了")
	}

	def = baseDefinition()
	def.AllowedCIDRs = []string{"10.0.0.0/33"}
	if err := ValidateDefinition(def); err == nil {
		t.Fatal("非法 CIDR 却通过了")
	}
}

func TestRenderBlockMatchesSambaShape(t *testing.T) {
	def := baseDefinition()
	names := map[string]string{
		"sac_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "alice",
		"sac_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "bob",
	}
	got, err := def.RenderBlock("/srv/upanel/volumes/data", names)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	want := "[team-data]\n" +
		"   path = /srv/upanel/volumes/data/projects/team\n" +
		"   browseable = yes\n" +
		"   guest ok = no\n" +
		"   read only = no\n" +
		"   follow symlinks = no\n" +
		"   wide links = no\n" +
		"   valid users = alice,bob\n" +
		"   write list = alice\n"
	if got != want {
		t.Fatalf("渲染结果与期望不一致\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderBlockReadOnlyHasNoWriteList(t *testing.T) {
	def := baseDefinition()
	def.ReadOnly = true
	names := map[string]string{
		"sac_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "alice",
		"sac_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "bob",
	}
	got, err := def.RenderBlock("/srv/upanel/volumes/data", names)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(got, "read only = yes\n") {
		t.Fatalf("只读共享没有 read only = yes：\n%s", got)
	}
	if strings.Contains(got, "write list") {
		t.Fatalf("只读共享不该出现 write list：\n%s", got)
	}
}

func TestRenderBlockIsDeterministic(t *testing.T) {
	def := baseDefinition()
	def.AllowedCIDRs = []string{"192.168.1.0/24", "10.0.0.0/8", "172.16.0.0/12"}
	names := map[string]string{
		"sac_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "alice",
		"sac_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "bob",
	}
	first, err := def.RenderBlock("/srv/upanel/volumes/data", names)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	// 打乱输入顺序：输出必须逐字节相同，否则“配置版本”会随机变化。
	def.AllowedCIDRs = []string{"172.16.0.0/12", "10.0.0.0/8", "192.168.1.0/24"}
	def.SMBUsers = []ShareUser{def.SMBUsers[1], def.SMBUsers[0]}
	second, err := def.RenderBlock("/srv/upanel/volumes/data", names)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if first != second {
		t.Fatalf("同一份定义换了输入顺序就得到不同渲染结果：\n--- 1 ---\n%s\n--- 2 ---\n%s", first, second)
	}
	if ConfigHashOf(first) != ConfigHashOf(second) {
		t.Fatal("渲染结果相同但配置哈希不同")
	}
	if !strings.Contains(first, "hosts allow = 10.0.0.0/8 172.16.0.0/12 192.168.1.0/24\n") {
		t.Fatalf("hosts allow 没有排序输出：\n%s", first)
	}
}

func TestConfigHashChangesWithContent(t *testing.T) {
	a := "[s]\n   path = /a\n"
	b := "[s]\n   path = /b\n"
	if ConfigHashOf(a) == ConfigHashOf(b) {
		t.Fatal("内容不同却算出同一个配置哈希")
	}
	if len(ConfigHashOf(a)) != 64 {
		t.Fatalf("配置哈希不是 64 位十六进制：%q", ConfigHashOf(a))
	}
}

func TestShareDirStaysInsideVolume(t *testing.T) {
	mount := "/srv/upanel/volumes/data"
	got, err := ShareDir(mount, "projects/team")
	if err != nil {
		t.Fatalf("合法相对路径失败: %v", err)
	}
	if got != mount+"/projects/team" {
		t.Fatalf("拼接结果 %q 不对", got)
	}
	root, err := ShareDir(mount, "")
	if err != nil {
		t.Fatalf("空相对路径（卷根）失败: %v", err)
	}
	if root != mount {
		t.Fatalf("空相对路径应该得到挂载点本身，实际 %q", root)
	}
	for _, bad := range []string{"/etc", "..", "a/../../etc", "../" + "x"} {
		if _, err := ShareDir(mount, bad); err == nil {
			t.Errorf("相对路径 %q 应该被拒，却通过了", bad)
		}
	}
}

func TestValidIDs(t *testing.T) {
	if !ValidShareID("shr_0123456789abcdef0123456789abcdef") {
		t.Fatal("合法共享标识被拒")
	}
	if !ValidAccountID("sac_0123456789abcdef0123456789abcdef") {
		t.Fatal("合法账号标识被拒")
	}
	for _, bad := range []string{
		"", "shr_", "shr_0123456789ABCDEF0123456789ABCDEF",
		"vol_0123456789abcdef0123456789abcdef",
		"shr_0123456789abcdef0123456789abcde",
		"shr_0123456789abcdef0123456789abcdefg",
	} {
		if ValidShareID(bad) {
			t.Errorf("共享标识 %q 应该被拒", bad)
		}
	}
	if ValidAccountID("shr_0123456789abcdef0123456789abcdef") {
		t.Error("共享标识被当成了账号标识")
	}
}

func TestNewIDsUnique(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		id, err := NewShareID()
		if err != nil {
			t.Fatalf("生成共享标识失败: %v", err)
		}
		if !ValidShareID(id) {
			t.Fatalf("生成的共享标识不合法: %q", id)
		}
		if seen[id] {
			t.Fatalf("共享标识重复: %q", id)
		}
		seen[id] = true
	}
	for i := 0; i < 64; i++ {
		id, err := NewAccountID()
		if err != nil {
			t.Fatalf("生成账号标识失败: %v", err)
		}
		if !ValidAccountID(id) {
			t.Fatalf("生成的账号标识不合法: %q", id)
		}
	}
}

// asDefinitionError 用类型断言代替 errors.As：DefinitionError 是值类型包一层指针，
// errors.As 对“目标类型与错误本身不是同一种”会漏，这里要的是精确类型。
func asDefinitionError(err error, target **DefinitionError) bool {
	de, ok := err.(*DefinitionError)
	if ok {
		*target = de
	}
	return ok
}
