package shares

// POSIX ACL：按共享定义准备目录权限，并把结果读回来核对（P08.1）。
//
// 为什么共享权限必须落在 POSIX ACL 上而不是 chmod：PLAN.md 第 10 节要求“共享的读写
// 权限必须与 POSIX ACL 结合”，因为一个目录要同时对若干账号给出**不同的**访问级别
// （只读用户 r-x、读写用户 rwx），而模式位只有 owner/group/other 三档。
//
// 这一层只用两个外部工具：setfacl 写、getfacl 读。写用 `--set`（整份替换）而不是
// `-m`（增量修改）：增量修改会让“上一次共享给过、这一次没给”的账号把权限留在目录上，
// 那种残留从界面上完全看不见。
//
// 明确不做：递归。共享目录里已有内容的权限由管理员自己负责——递归改权限会碰到
// 用户自己的文件，那不是“准备共享”的一部分。

import (
	"fmt"
	"sort"
	"strings"
)

// ACLPerms 是一组 POSIX ACL 权限位。
const (
	// PermsRead 是只读用户拿到的权限：能进目录、能列、能读内容。
	PermsRead = "r-x"
	// PermsWrite 是读写用户拿到的权限。
	PermsWrite = "rwx"
	// permsOwner 是共享目录属主（root）的权限。
	permsOwner = "rwx"
	// permsNone 是组与其他人的权限。
	//
	// 都是 ---：共享目录不允许 world 访问，也不靠“同名组”放行任何人。
	// 谁能访问只看 ACL 里逐个账号的条目。
	permsNone = "---"
)

// ACLModeName 是 getfacl 输出里的键名前缀与集合名。
const (
	aclUserPrefix  = "user:"
	aclGroupPrefix = "group:"
	aclOwnerKey    = "user::"
	aclGroupKey    = "group::"
	aclOtherKey    = "other::"
	aclMaskKey     = "mask::"
)

// ACLEntry 是一条访问授权（已解析成系统用户名）。
type ACLEntry struct {
	Name  string
	Perms string
}

// DesiredACL 由共享定义算出目录应当有的 ACL 条目。
//
// read_only 的共享里，即使某账号标了 write，也只给 r-x——定义里写着只读却给写权限，
// 是两份事实互相矛盾，而目录上的权限必须服从“更强的那条限制”。
func DesiredACL(access []resolvedAccess, readOnly bool) []ACLEntry {
	out := make([]ACLEntry, 0, len(access))
	for _, a := range access {
		perms := PermsRead
		if a.Access == AccessWrite && !readOnly {
			perms = PermsWrite
		}
		out = append(out, ACLEntry{Name: a.Name, Perms: perms})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ACLSpec 渲染 setfacl 的 `--set` 参数值。
//
// 顺序固定：属主、组、其他人，然后按名字排序的账号条目。setfacl 不关心顺序，
// 但**测试**关心——同一份定义两次渲染出不同的字符串会让“这份目录已按定义准备好”
// 变成一个无法比较的问题。
func ACLSpec(entries []ACLEntry) string {
	parts := make([]string, 0, len(entries)+3)
	parts = append(parts,
		aclOwnerKey+permsOwner,
		aclGroupKey+permsNone,
		aclOtherKey+permsNone,
	)
	for _, e := range entries {
		parts = append(parts, aclUserPrefix+e.Name+":"+e.Perms)
	}
	return strings.Join(parts, ",")
}

// ParseGetfacl 把 `getfacl -cp <dir>` 的输出解析成 键 → 权限 的表。
//
// 真实输出（-c 省略注释、-p 保留路径，这里两者都用）：
//
//	user::rwx
//	user:facl-a:r-x
//	group::---
//	mask::r-x
//	other::---
//
// 只保留 user::/group::/other::/user:<name>: 四类；mask:: 单独记在 mask 里
// （它由 setfacl 自动算出来，不参与“授权对不对”的判定）。
func ParseGetfacl(out string) (entries map[string]string, mask string, err error) {
	entries = make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		// 只有形如 "<集合>:<可选限定名>:<权限>" 的行才有意义。
		last := strings.LastIndexByte(l, ':')
		if last <= 0 {
			continue
		}
		key := l[:last+1]
		perms := l[last+1:]
		switch {
		case key == aclMaskKey:
			mask = perms
		case key == aclOwnerKey, key == aclGroupKey, key == aclOtherKey:
			entries[key] = perms
		case strings.HasPrefix(key, aclUserPrefix):
			// user:<name>: —— 限定名为空时上面已经处理过。
			name := strings.TrimSuffix(strings.TrimPrefix(key, aclUserPrefix), ":")
			if name == "" {
				entries[aclOwnerKey] = perms
				continue
			}
			entries[aclUserPrefix+name] = perms
		case strings.HasPrefix(key, aclGroupPrefix):
			// 具名组条目不是这一阶段写出来的东西，但读到了要能被发现（见 DiffACL）。
			name := strings.TrimSuffix(strings.TrimPrefix(key, aclGroupPrefix), ":")
			if name == "" {
				entries[aclGroupKey] = perms
				continue
			}
			entries[aclGroupPrefix+name] = perms
		}
	}
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("shares: getfacl 输出里没有可识别的 ACL 条目")
	}
	if _, ok := entries[aclOwnerKey]; !ok {
		return nil, "", fmt.Errorf("shares: getfacl 输出里缺少 user:: 条目")
	}
	return entries, mask, nil
}

// ACLDiff 是一次 ACL 核对的差异。
type ACLDiff struct {
	// Missing 是定义要求、但目录上没有的账号条目。
	Missing []string
	// Wrong 是权限位不对的账号条目（形如 `user:alice 期望 rwx 实际 r-x`）。
	Wrong []string
	// Extra 是目录上有、定义里没有的条目。撤销访问权靠它说话：
	// 多出来的条目就是“这个人还能访问”。
	Extra []string
}

// OK 报告差异是否为空。
func (d ACLDiff) OK() bool {
	return len(d.Missing) == 0 && len(d.Wrong) == 0 && len(d.Extra) == 0
}

// String 把差异拼成一句能直接给人看的话。
func (d ACLDiff) String() string {
	var parts []string
	if len(d.Missing) > 0 {
		parts = append(parts, "缺少 "+strings.Join(d.Missing, ", "))
	}
	if len(d.Wrong) > 0 {
		parts = append(parts, "权限不符 "+strings.Join(d.Wrong, ", "))
	}
	if len(d.Extra) > 0 {
		parts = append(parts, "多余 "+strings.Join(d.Extra, ", "))
	}
	if len(parts) == 0 {
		return "一致"
	}
	return strings.Join(parts, "；")
}

// DiffACL 把期望的条目与读回来的表对上。
//
// 除账号条目之外还核对三件固定的事：属主 rwx、组 ---、其他人 ---。任何一条不符都
// 算失败：`other::r-x` 意味着这台机器上任何本地账号都能进这个共享目录看一眼，
// 而界面上它会显示成“只有这两个账号可访问”。
func DiffACL(desired []ACLEntry, got map[string]string) ACLDiff {
	var diff ACLDiff
	check := func(key, want, label string) {
		if have, ok := got[key]; !ok {
			diff.Missing = append(diff.Missing, label)
		} else if have != want {
			diff.Wrong = append(diff.Wrong, fmt.Sprintf("%s 期望 %s 实际 %s", label, want, have))
		}
	}
	check(aclOwnerKey, permsOwner, "属主权限")
	check(aclGroupKey, permsNone, "组权限")
	check(aclOtherKey, permsNone, "其他用户权限")

	want := make(map[string]string, len(desired))
	for _, e := range desired {
		want[aclUserPrefix+e.Name] = e.Perms
		check(aclUserPrefix+e.Name, e.Perms, "账号 "+e.Name)
	}
	for key, have := range got {
		switch {
		case key == aclOwnerKey, key == aclGroupKey, key == aclOtherKey, key == aclMaskKey:
			continue
		case strings.HasPrefix(key, aclUserPrefix):
			name := strings.TrimPrefix(key, aclUserPrefix)
			if _, ok := want[key]; !ok {
				diff.Extra = append(diff.Extra, fmt.Sprintf("账号 %s(%s)", name, have))
			}
		default:
			// 具名组条目同样是“还能访问”的来源。
			diff.Extra = append(diff.Extra, fmt.Sprintf("%s(%s)", key, have))
		}
	}
	sort.Strings(diff.Missing)
	sort.Strings(diff.Wrong)
	sort.Strings(diff.Extra)
	return diff
}

// MaskCovers 检查 ACL 的 mask 是否覆盖了每条期望授权。
//
// mask 是 ACL 里唯一一个“看得见权限、却说了不算”的东西：目录上的条目写着 rwx，
// 而 mask 是 r-- 时，实际生效的只有读。`setfacl --set` 会按条目自动算出 mask，
// 正常情况下一定覆盖；但 mask 可能被别处改小（有人手工 getfacl/setfacl，或者
// 目录从别的地方整个搬过来）。这种状态最危险的地方是它从 getfacl 的条目行上看
// 不出来——所以必须单独核。
func MaskCovers(desired []ACLEntry, mask string) bool {
	if len(desired) == 0 {
		return true
	}
	mr, mw, mx, ok := permBits(mask)
	if !ok {
		return false
	}
	for _, e := range desired {
		r, w, x, ok := permBits(e.Perms)
		if !ok {
			return false
		}
		if (r && !mr) || (w && !mw) || (x && !mx) {
			return false
		}
	}
	return true
}

// permBits 把 "rwx"/"r-x"/"---" 这类三字符权限串拆成三个布尔。
func permBits(p string) (r, w, x, ok bool) {
	if len(p) != 3 {
		return false, false, false, false
	}
	flag := func(c byte) bool { return c != '-' }
	// 只接受 r/w/x/- 这些字符：getfacl 还会打印 s/t（setuid/sticky），
	// 那不属于访问授权，出现了就当成读不懂。
	for i := 0; i < 3; i++ {
		switch p[i] {
		case 'r', 'w', 'x', '-':
		default:
			return false, false, false, false
		}
	}
	return flag(p[0]), flag(p[1]), flag(p[2]), true
}
