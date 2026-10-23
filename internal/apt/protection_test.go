package apt

// 保护名单与连带删除的判定（P11.2）。
//
// 断言用的是真机抓下来的输出：`remove openssh-server` 在 24.04 上退出码 0、
// 只删它自己；`remove ca-certificates` 退出码 0、删 26 个。这两份输出正是
// "命令成功但机器回不来了"的样本，保护名单是唯一的拦网。

import (
	"sort"
	"strings"
	"testing"
)

// essentialLike24_04 是 24.04 上 `Essential: yes` 的一部分（真机 23 个里取常用的）。
// 它只用于测试：生产判断永远读 dpkg 数据库。
func essentialLike24_04() map[string]bool {
	return map[string]bool{
		"bash": true, "coreutils": true, "dpkg": true, "apt": true, "libc6": true,
		"util-linux": true, "python3-minimal": true, "debconf": true, "sysvinit-utils": true,
	}
}

func TestProtectCategories(t *testing.T) {
	essential := essentialLike24_04()
	tests := []struct {
		name     string
		category string
	}{
		{"bash", ProtectEssential},
		{"libc6", ProtectEssential},
		{"openssh-server", ProtectSSH},
		{"openssh-client", ProtectSSH},
		{"certbot", ProtectPanel},
		{"nginx-core", ProtectPanel},
		{"ca-certificates", ProtectPanel},
		{"netplan.io", ProtectNetwork},
		{"iproute2", ProtectNetwork},
		{"shim-signed", ProtectBoot},
		{"grub-efi-amd64-signed", ProtectBoot},
		{"linux-image-6.8.0-90-generic", ProtectBoot},
		{"systemd", ProtectCore},
		{"sudo", ProtectCore},
		{"python3", ProtectCore},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Protect(tc.name, essential)
			if !got.Protected {
				t.Fatalf("%s 应当受保护", tc.name)
			}
			if got.Category != tc.category {
				t.Fatalf("%s 类别应为 %s，实际 %s", tc.name, tc.category, got.Category)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Fatalf("%s 必须有理由（界面上要显示给管理员看）", tc.name)
			}
		})
	}
}

// TestProtectArchQualified：多架构限定不改变结论。
//
// P11.1 的缺陷就是名字里带 `:amd64` 没剥掉，导致 `bind9-libs` 之类的判断落空。
// 这里明确钉住：保护判定按基础名做。
func TestProtectArchQualified(t *testing.T) {
	essential := essentialLike24_04()
	for _, name := range []string{"bash:amd64", "openssh-server:amd64", "ca-certificates:all", "libc6:amd64"} {
		if !Protect(BaseName(name), essential).Protected {
			t.Fatalf("%s 剥掉架构后应受保护", name)
		}
		if !Protect(name, essential).Protected {
			t.Fatalf("%s 直接传进 Protect 也应受保护", name)
		}
	}
	// 反过来：普通包不能被误判。
	for _, name := range []string{"bind9-libs:amd64", "cowsay", "zlib1g:amd64", "vim-tiny"} {
		if Protect(name, essential).Protected {
			t.Fatalf("%s 不该受保护", name)
		}
	}
}

// TestProtectNoAccidentalPrefixMatch：`linux-modules-extra` 受保护（前缀），
// 但 `linux-libc-dev` 这种"看起来像内核其实不是启动链"的包不能因为前缀被拦。
func TestProtectEtymologyNotGuessed(t *testing.T) {
	essential := essentialLike24_04()
	if Protect("linux-libc-dev", essential).Protected {
		t.Fatal("linux-libc-dev 不在启动链前缀里，不该被拦")
	}
	if Protect("grub-customizer", essential).Protected {
		t.Fatal("grub-customizer 没在名单里也没命中前缀，不该被拦（不用前缀猜）")
	}
}

// TestProtectedRemovalsSSH：apt 同意删 SSH 服务端时，保护名单必须拦住。
func TestProtectedRemovalsSSH(t *testing.T) {
	sim := ParseSimulation(readCapture(t, "2404-sim-remove-ssh.txt"), []string{"openssh-server"})
	if sim.Failed() {
		t.Fatal("这份输出 apt 是同意删的，前提变了")
	}
	hits := ProtectedRemovals(sim, essentialLike24_04())
	if len(hits) != 1 || hits[0] != "openssh-server" {
		t.Fatalf("应拦下 openssh-server，实际 %v", hits)
	}
	// 点名要删的包不算连带。
	if collateral := CollateralRemovals(sim, []string{"openssh-server"}); len(collateral) != 0 {
		t.Fatalf("点名的包不该算连带: %v", collateral)
	}
}

// TestProtectedRemovalsCascade：删 ca-certificates 会带走 26 个，其中两个在名单里。
func TestProtectedRemovalsCascade(t *testing.T) {
	requested := []string{"ca-certificates"}
	sim := ParseSimulation(readCapture(t, "2404-sim-remove-ca.txt"), requested)
	hits := ProtectedRemovals(sim, essentialLike24_04())
	want := []string{"ca-certificates", "certbot"}
	if strings.Join(hits, ",") != strings.Join(want, ",") {
		t.Fatalf("应拦下 %v，实际 %v", want, hits)
	}
	collateral := CollateralRemovals(sim, requested)
	if len(collateral) != 25 {
		t.Fatalf("连带删除应为 25 个（26 减去点名的 1 个），实际 %d: %v", len(collateral), collateral)
	}
	if !sort.StringsAreSorted(collateral) {
		t.Fatal("连带列表必须升序，界面上才稳定")
	}
	for _, name := range collateral {
		if name == "ca-certificates" {
			t.Fatal("点名的包不该出现在连带里")
		}
	}
	// 这份连带里最该被看见的几个。
	for _, must := range []string{"ubuntu-server", "snapd", "cloud-init"} {
		found := false
		for _, name := range collateral {
			if name == must {
				found = true
			}
		}
		if !found {
			t.Fatalf("连带里应包含 %s", must)
		}
	}
}

// TestCollateralBaseNameMatch：点名的名字带架构限定，删除列表里不带（或反之），
// 不能因此把点名的包算成连带。
func TestCollateralArchNames(t *testing.T) {
	sim := Simulation{Remove: []string{"bind9-libs"}}
	if collateral := CollateralRemovals(sim, []string{"bind9-libs:amd64"}); len(collateral) != 0 {
		t.Fatalf("同一软件的两种写法不该算连带: %v", collateral)
	}
	if collateral := CollateralRemovals(sim, []string{"bind9-utils"}); len(collateral) != 1 {
		t.Fatalf("没点名的删除必须算连带: %v", collateral)
	}
}

// TestProtectedRemovalsOnRefusal：apt 自己拒绝（essential）时没有 Remv 行，
// 但标题段落里有名字——保护名单照样命中，两个阻塞项一起报。
func TestProtectedRemovalsOnRefusal(t *testing.T) {
	sim := ParseSimulation(readCapture(t, "2404-sim-remove.txt"), []string{"bash"})
	hits := ProtectedRemovals(sim, essentialLike24_04())
	if len(hits) != 1 || hits[0] != "bash" {
		t.Fatalf("apt 拒绝时也要从标题段落里认出 bash，实际 %v", hits)
	}
}
