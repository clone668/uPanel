package shares

// POSIX ACL 的单测（P08.1）。
//
// getfacl 的输出样本取自验收 VM（Ubuntu 24.04 的 acl 2.3.2）上的真实输出，
// 不是照着 man 手册编的：解析器必须对得上真实字符流，包括空行与结尾换行。

import (
	"strings"
	"testing"
)

// realGetfaclOutput 是 VM 上 `setfacl --set u::rwx,g::---,o::---,u:facl-a:r-x,u:facl-b:rwx <dir>`
// 之后 `getfacl -c -p <dir>` 的真实输出（-p 会多打一行路径，这里保留）。
const realGetfaclOutput = `user::rwx
user:facl-a:r-x
user:facl-b:rwx
group::---
mask::rwx
other::---
`

func names(entries []ACLEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Name] = e.Perms
	}
	return out
}

func TestDesiredACLReadOnlyWins(t *testing.T) {
	access := []resolvedAccess{
		{Name: "bob", Access: AccessWrite},
		{Name: "alice", Access: AccessRead},
	}
	got := DesiredACL(access, false)
	want := map[string]string{"alice": PermsRead, "bob": PermsWrite}
	for name, perms := range want {
		if names(got)[name] != perms {
			t.Errorf("可写共享里 %s 的权限是 %q，期望 %q", name, names(got)[name], perms)
		}
	}
	// 顺序必须是按名字排序：两次渲染结果要能逐字节比较。
	if got[0].Name != "alice" || got[1].Name != "bob" {
		t.Fatalf("条目没有按名字排序：%v", got)
	}

	got = DesiredACL(access, true)
	if p := names(got)["bob"]; p != PermsRead {
		t.Fatalf("只读共享里标了 write 的账号拿到 %q，期望 %q（更强的限制优先）", p, PermsRead)
	}
}

func TestACLSpecShape(t *testing.T) {
	entries := []ACLEntry{{Name: "alice", Perms: PermsRead}, {Name: "bob", Perms: PermsWrite}}
	got := ACLSpec(entries)
	want := "user::rwx,group::---,other::---,user:alice:r-x,user:bob:rwx"
	if got != want {
		t.Fatalf("setfacl 参数是\n%s\n期望\n%s", got, want)
	}
	// 空条目（撤销授权）也必须是一份**完整**的 ACL，而不是空串：
	// setfacl --set "" 会直接报错，什么都不会改。
	empty := ACLSpec(nil)
	if empty != "user::rwx,group::---,other::---" {
		t.Fatalf("撤销用的 ACL 是 %q", empty)
	}
}

func TestParseGetfaclRealOutput(t *testing.T) {
	entries, mask, err := ParseGetfacl(realGetfaclOutput)
	if err != nil {
		t.Fatalf("解析真实 getfacl 输出失败: %v", err)
	}
	if mask != "rwx" {
		t.Fatalf("mask 是 %q，期望 rwx", mask)
	}
	want := map[string]string{
		aclOwnerKey:   "rwx",
		aclGroupKey:   "---",
		aclOtherKey:   "---",
		"user:facl-a": "r-x",
		"user:facl-b": "rwx",
	}
	for k, v := range want {
		if entries[k] != v {
			t.Errorf("条目 %s 是 %q，期望 %q", k, entries[k], v)
		}
	}
	// 撤销后 VM 上的真实输出（setfacl --set u::rwx,g::---,o::---）：
	after := `user::rwx
group::---
other::---
`
	entries2, mask2, err := ParseGetfacl(after)
	if err != nil {
		t.Fatalf("解析撤销后的 getfacl 输出失败: %v", err)
	}
	if mask2 != "" {
		t.Fatalf("撤销后的输出里不该有 mask 行，却读到 %q", mask2)
	}
	if diff := DiffACL(nil, entries2); !diff.OK() {
		t.Fatalf("撤销后的 ACL 应该判定为一致，实际：%s", diff.String())
	}
}

func TestParseGetfaclRejectsGarbage(t *testing.T) {
	if _, _, err := ParseGetfacl(""); err == nil {
		t.Fatal("空输出应该报错，而不是返回空表")
	}
	if _, _, err := ParseGetfacl("group::---\nother::---\n"); err == nil {
		t.Fatal("缺少 user:: 条目的输出应该报错")
	}
	// 注释行与空行必须被跳过（getfacl 不带 -c 时会打印 # file: 之类）。
	out := "# file: /srv/upanel/volumes/data/x\n# owner: root\n\nuser::rwx\ngroup::---\nother::---\n"
	if _, _, err := ParseGetfacl(out); err != nil {
		t.Fatalf("带注释的输出应该能解析: %v", err)
	}
}

func TestDiffACLDetectsMissingWrongExtra(t *testing.T) {
	desired := []ACLEntry{
		{Name: "alice", Perms: PermsRead},
		{Name: "bob", Perms: PermsWrite},
	}
	got := map[string]string{
		aclOwnerKey:  "rwx",
		aclGroupKey:  "---",
		aclOtherKey:  "---",
		"user:alice": "rwx", // 权限不符
		"user:carol": "r-x", // 多余
	}
	diff := DiffACL(desired, got)
	if diff.OK() {
		t.Fatal("有差异却判定为一致")
	}
	if len(diff.Missing) != 1 || !strings.Contains(diff.Missing[0], "bob") {
		t.Fatalf("没报出缺少的 bob：%v", diff.Missing)
	}
	if len(diff.Wrong) != 1 || !strings.Contains(diff.Wrong[0], "alice") {
		t.Fatalf("没报出权限不符的 alice：%v", diff.Wrong)
	}
	if len(diff.Extra) != 1 || !strings.Contains(diff.Extra[0], "carol") {
		t.Fatalf("没报出多余的 carol：%v", diff.Extra)
	}
	if s := diff.String(); s == "" || s == "一致" {
		t.Fatalf("差异描述是 %q", s)
	}
}

func TestDiffACLRejectsWorldReadable(t *testing.T) {
	desired := []ACLEntry{{Name: "alice", Perms: PermsRead}}
	// other 有 r-x：这台机器上任何本地账号都能进目录看一眼。
	got := map[string]string{
		aclOwnerKey:  "rwx",
		aclGroupKey:  "---",
		aclOtherKey:  "r-x",
		"user:alice": "r-x",
	}
	diff := DiffACL(desired, got)
	if diff.OK() {
		t.Fatal("other::r-x 却判定为一致：目录对所有人可读")
	}
	if len(diff.Wrong) != 1 || !strings.Contains(diff.Wrong[0], "其他用户") {
		t.Fatalf("没报出其他用户权限问题：%v", diff.Wrong)
	}
}

func TestDiffACLFlagsNamedGroups(t *testing.T) {
	desired := []ACLEntry{{Name: "alice", Perms: PermsRead}}
	got := map[string]string{
		aclOwnerKey:    "rwx",
		aclGroupKey:    "---",
		aclOtherKey:    "---",
		"user:alice":   "r-x",
		"group:admins": "rwx",
	}
	diff := DiffACL(desired, got)
	if len(diff.Extra) != 1 || !strings.Contains(diff.Extra[0], "group:admins") {
		t.Fatalf("具名组条目没被当成多余授权：%v", diff.Extra)
	}
}

func TestMaskCovers(t *testing.T) {
	desired := []ACLEntry{
		{Name: "alice", Perms: PermsRead},
		{Name: "bob", Perms: PermsWrite},
	}
	if !MaskCovers(desired, "rwx") {
		t.Fatal("mask=rwx 应该覆盖 r-x 与 rwx")
	}
	if MaskCovers(desired, "r-x") {
		t.Fatal("mask=r-x 覆盖不了 rwx：写权限实际不生效，必须判失败")
	}
	if MaskCovers(desired, "---") {
		t.Fatal("mask=--- 时谁都没有权限，必须判失败")
	}
	if MaskCovers(desired, "rw-") {
		t.Fatal("mask=rw- 应该判为不覆盖（少了进入目录的权限）")
	}
}
