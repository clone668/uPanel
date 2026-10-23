package apt

import (
	"reflect"
	"strings"
	"testing"
)

// 夹具来源说明（这两点决定了这些测试值不值钱）：
//
//   - 下面每一段 apt-cache policy / dpkg-query 原文都是 2026-10 从验收虚拟机
//     （VM-A 22.04.5 / VM-B 24.04.5）上直接抓下来的，没有手写、没有改写。
//   - 唯一的例外明确标了「合成」：`Candidate: (none)`、`rc` 状态、flat 仓库这三段
//     那两台机器上抓不到，为了覆盖分支在真机原文旁边构造。

// realPolicy2204 是 VM-A 上 `apt-cache policy bash apparmor libssl3 nano snapd` 的
// 原始输出（未删行）。
const realPolicy2204 = `bash:
  Installed: 5.1-6ubuntu1.1
  Candidate: 5.1-6ubuntu1.1
  Version table:
 *** 5.1-6ubuntu1.1 500
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
        100 /var/lib/dpkg/status
     5.1-6ubuntu1 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
apparmor:
  Installed: 3.0.4-2ubuntu2.5
  Candidate: 3.0.4-2ubuntu2.5
  Version table:
 *** 3.0.4-2ubuntu2.5 500
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
        100 /var/lib/dpkg/status
     3.0.4-2ubuntu2.4 500
        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
     3.0.4-2ubuntu2 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
libssl3:
  Installed: 3.0.2-0ubuntu1.30
  Candidate: 3.0.2-0ubuntu1.30
  Version table:
 *** 3.0.2-0ubuntu1.30 500
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
        100 /var/lib/dpkg/status
     3.0.2-0ubuntu1 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
nano:
  Installed: 6.2-1ubuntu0.2
  Candidate: 6.2-1ubuntu0.3
  Version table:
     6.2-1ubuntu0.3 500 (phased 70%)
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
 *** 6.2-1ubuntu0.2 500
        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
        100 /var/lib/dpkg/status
     6.2-1 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
snapd:
  Installed: 2.76.3+ubuntu22.04.1
  Candidate: 2.77.1+ubuntu22.04.1
  Version table:
     2.77.1+ubuntu22.04.1 500 (phased 20%)
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
 *** 2.76.3+ubuntu22.04.1 500
        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
        100 /var/lib/dpkg/status
     2.55.3+22.04 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
`

// realPolicy2404 是 VM-B 上 `apt-cache policy nano snapd apparmor` 的原始输出。
const realPolicy2404 = `nano:
  Installed: 7.2-2ubuntu0.2
  Candidate: 7.2-2ubuntu0.2
  Version table:
 *** 7.2-2ubuntu0.2 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
        100 /var/lib/dpkg/status
     7.2-2build1 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
snapd:
  Installed: 2.77.1+ubuntu24.04
  Candidate: 2.77.1+ubuntu24.04
  Version table:
 *** 2.77.1+ubuntu24.04 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        100 /var/lib/dpkg/status
     2.76+ubuntu24.04.1 500
        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
     2.62+24.04build1 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
apparmor:
  Installed: 4.0.1really4.0.1-0ubuntu0.24.04.9
  Candidate: 4.0.1really4.0.1-0ubuntu0.24.04.9
  Version table:
 *** 4.0.1really4.0.1-0ubuntu0.24.04.9 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        100 /var/lib/dpkg/status
     4.0.0-beta3-0ubuntu3 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
`

// realDpkgQuery 是 VM-A 上 `dpkg-query -W -f=...` 输出里 grep 出来的那几行原文。
// 注意两处容易被"清洗"掉的东西：包名带 `:amd64` 限定，以及状态位是 `ii `（带尾空格，
// ${db:Status-Abbrev} 固定三字符）。
const realDpkgQuery = "apparmor\t3.0.4-2ubuntu2.5\tamd64\tii \n" +
	"bash\t5.1-6ubuntu1.1\tamd64\tii \n" +
	"bash-completion\t1:2.11-5ubuntu1\tall\tii \n" +
	"libc6:amd64\t2.35-0ubuntu3.15\tamd64\tii \n" +
	"libssl3:amd64\t3.0.2-0ubuntu1.30\tamd64\tii \n" +
	"linux-image-5.15.0-198-generic\t5.15.0-198.208\tamd64\tii \n" +
	"nano\t6.2-1ubuntu0.2\tamd64\tii \n" +
	"python3.10-minimal\t3.10.12-1~22.04.18\tamd64\tii \n"

func TestParseDpkgQueryReal(t *testing.T) {
	pkgs, err := ParseDpkgQuery(realDpkgQuery)
	if err != nil {
		t.Fatalf("解析真机输出失败: %v", err)
	}
	if len(pkgs) != 8 {
		t.Fatalf("包数 = %d，期望 8", len(pkgs))
	}
	byName := map[string]Package{}
	for _, p := range pkgs {
		byName[p.Name] = p
	}
	// 带架构限定的包名与架构列都要原样保留，不能被"规范化"成同一个键。
	if p := byName["libssl3:amd64"]; p.Architecture != "amd64" || p.InstalledVersion != "3.0.2-0ubuntu1.30" {
		t.Errorf("libssl3:amd64 解析错: %+v", p)
	}
	// epoch 与 `~` 都在版本号里活着。
	if p := byName["bash-completion"]; p.InstalledVersion != "1:2.11-5ubuntu1" {
		t.Errorf("bash-completion 版本 = %q", p.InstalledVersion)
	}
	if p := byName["python3.10-minimal"]; p.InstalledVersion != "3.10.12-1~22.04.18" {
		t.Errorf("python3.10-minimal 版本 = %q", p.InstalledVersion)
	}
	// 状态位原样（含尾空格），判断留给面板。
	if p := byName["bash"]; !strings.HasPrefix(p.Status, "ii") {
		t.Errorf("bash 状态 = %q", p.Status)
	}
}

func TestParseDpkgQueryErrors(t *testing.T) {
	cases := map[string]string{
		"字段不足": "bash\t5.1-6ubuntu1.1\tamd64\n",
		"字段过多": "bash\t5.1\tamd64\tii\textra\n",
		"包名为空": "\t5.1\tamd64\tii\n",
	}
	for name, in := range cases {
		if _, err := ParseDpkgQuery(in); err == nil {
			t.Errorf("%s：期望报错，实际通过（输入 %q）", name, in)
		}
	}
}

func TestParseAptCachePolicyReal2204(t *testing.T) {
	got, err := ParseAptCachePolicy(realPolicy2204)
	if err != nil {
		t.Fatalf("解析真机输出失败: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("包数 = %d，期望 5：%v", len(got), keysOf(got))
	}

	// bash：候选同时出现在 jammy-updates 与 jammy-security → 两个 suite 都要有。
	if e := got["bash"]; e.Version != "5.1-6ubuntu1.1" ||
		!reflect.DeepEqual(e.Suites, []string{"jammy-security", "jammy-updates"}) || e.OnlyLocal {
		t.Errorf("bash = %+v", e)
	}

	// nano：`***` 指着已安装的 6.2-1ubuntu0.2，候选是 6.2-1ubuntu0.3。
	// 候选的 suite 只能来自 6.2-1ubuntu0.3 那一块（jammy-updates）。
	// 若按 `***` 块取，会得到 jammy-security，把 nano 误判成安全更新
	// ——这条断言就是那个陷阱的回归护栏。
	if e := got["nano"]; e.Version != "6.2-1ubuntu0.3" ||
		!reflect.DeepEqual(e.Suites, []string{"jammy-updates"}) || e.OnlyLocal {
		t.Errorf("nano = %+v（期望候选 6.2-1ubuntu0.3、只有 jammy-updates）", e)
	}

	// snapd 同上，phased 标记不能把版本行解析坏。
	if e := got["snapd"]; e.Version != "2.77.1+ubuntu22.04.1" ||
		!reflect.DeepEqual(e.Suites, []string{"jammy-updates"}) {
		t.Errorf("snapd = %+v", e)
	}

	// apparmor：jammy-security 里放的是更旧的 3.0.4-2ubuntu2.4，
	// 候选 3.0.4-2ubuntu2.5 只来自 jammy-updates。不能算安全更新。
	if e := got["apparmor"]; e.Version != "3.0.4-2ubuntu2.5" ||
		!reflect.DeepEqual(e.Suites, []string{"jammy-updates"}) {
		t.Errorf("apparmor = %+v（jammy-security 里的更旧版本不该被算成候选来源）", e)
	}

	// libssl3：候选两边都有。
	if e := got["libssl3"]; e.Version != "3.0.2-0ubuntu1.30" ||
		!reflect.DeepEqual(e.Suites, []string{"jammy-security", "jammy-updates"}) {
		t.Errorf("libssl3 = %+v", e)
	}
}

func TestParseAptCachePolicyReal2404(t *testing.T) {
	got, err := ParseAptCachePolicy(realPolicy2404)
	if err != nil {
		t.Fatalf("解析真机输出失败: %v", err)
	}
	if e := got["nano"]; e.Version != "7.2-2ubuntu0.2" ||
		!reflect.DeepEqual(e.Suites, []string{"noble-security", "noble-updates"}) {
		t.Errorf("nano = %+v", e)
	}
	// 24.04 的 snapd：noble-security 里是更旧的 2.76+ubuntu24.04.1，候选只在 noble-updates。
	if e := got["snapd"]; e.Version != "2.77.1+ubuntu24.04" ||
		!reflect.DeepEqual(e.Suites, []string{"noble-updates"}) {
		t.Errorf("snapd = %+v", e)
	}
	if e := got["apparmor"]; e.Version != "4.0.1really4.0.1-0ubuntu0.24.04.9" ||
		!reflect.DeepEqual(e.Suites, []string{"noble-updates"}) {
		t.Errorf("apparmor = %+v", e)
	}
}

func TestParseAptCachePolicySynthetic(t *testing.T) {
	// 「合成」：以下三种形状在两台验收机上抓不到，为覆盖分支构造。
	const in = `localpkg:
  Installed: 1.0-1
  Candidate: (none)
  Version table:
 *** 1.0-1 100
        100 /var/lib/dpkg/status
removedpkg:
  Installed: (none)
  Candidate: 2:1.2.3-4
  Version table:
     2:1.2.3-4 500
        500 https://dl.example.com/stable ./ Packages
`
	got, err := ParseAptCachePolicy(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// (none)：候选为空串，但键必须在——"没有候选"与"没读到"要分得开。
	e, ok := got["localpkg"]
	if !ok {
		t.Fatal("localpkg 没有出现在结果里")
	}
	if e.Version != "" || e.Suites != nil {
		t.Errorf("localpkg = %+v", e)
	}
	// flat 仓库没有 component，取不出 suite，也不能瞎猜。
	if e := got["removedpkg"]; e.Version != "2:1.2.3-4" || e.Suites != nil {
		t.Errorf("removedpkg = %+v", e)
	}
}

func TestParseAptCachePolicyOnlyLocal(t *testing.T) {
	// 「合成」：候选只来自 dpkg status（本地 dpkg -i 装的包）。
	const in = `locally:
  Installed: 0.1.0
  Candidate: 0.1.0
  Version table:
 *** 0.1.0 100
        100 /var/lib/dpkg/status
`
	got, err := ParseAptCachePolicy(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if e := got["locally"]; e.Version != "0.1.0" || !e.OnlyLocal || e.Suites != nil {
		t.Errorf("locally = %+v", e)
	}
}

func TestParseAptCachePolicyRejectsGarbage(t *testing.T) {
	// 顶格但不是包名行：宁可报错，也不要把一半的块默默吞掉。
	if _, err := ParseAptCachePolicy("E: 无法定位软件包 nosuchpkg\n"); err == nil {
		t.Fatal("期望报错，实际通过")
	}
}

func TestParseRebootPkgs(t *testing.T) {
	// 「合成」：两台验收机上都还没有 /run/reboot-required.pkgs。
	in := []byte("linux-image-5.15.0-198-generic\n\nlibssl3:amd64\nlinux-image-5.15.0-198-generic\n  \n")
	got := ParseRebootPkgs(in)
	want := []string{"linux-image-5.15.0-198-generic", "libssl3:amd64"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRebootPkgs = %v，期望 %v", got, want)
	}
	if got := ParseRebootPkgs(nil); len(got) != 0 {
		t.Errorf("空输入应返回空切片，实际 %v", got)
	}
}

func TestParseBootID(t *testing.T) {
	// VM-A 的真实值。
	got, err := ParseBootID([]byte("ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc\n"))
	if err != nil {
		t.Fatalf("解析真机 boot_id 失败: %v", err)
	}
	if got != "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc" {
		t.Errorf("boot_id = %q", got)
	}
	if v, err := ParseBootID([]byte("AB2EB5E8-C1B4-4A00-A3D0-33CED7870ABC")); err != nil ||
		v != "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc" {
		t.Errorf("大写应统一成小写: %q, %v", v, err)
	}
	for _, bad := range []string{"", "   ", "ab2eb5e8", "ab2eb5e8-c1b4-4a00-a3d0", "zzzzzzzz-c1b4-4a00-a3d0-33ced7870abc"} {
		if _, err := ParseBootID([]byte(bad)); err == nil {
			t.Errorf("boot_id %q 应被拒绝", bad)
		}
	}
}

func keysOf(m map[string]policyEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
