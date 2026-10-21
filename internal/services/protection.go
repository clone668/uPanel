package services

import (
	"sort"
	"strings"
)

// protectionRule 是一条保护规则：匹配的单元不允许通过面板启动/停止/重启/改开机启动。
type protectionRule struct {
	category string
	reason   string
	// exact 是精确匹配的单元基名（不含 .service 与实例部分）。
	exact map[string]bool
	// prefix 是按前缀匹配的单元基名（例如 systemd-），用于核心组件的兜底。
	prefix string
}

// protectionRules 是固定的保护名单（PLAN.md 第 14 节）。
//
// 这张表**只能通过改代码扩充**：不接受配置、不接受 API 写入、不接受“临时豁免”。
// 理由写在每一条里，界面直接展示，让人知道为什么按钮是灰的。
//
// 刻意保留的边界：
//   - Nginx/Samba/Docker 不在这里实现“受约束动作”，而是**整体拒绝**：
//     它们的启停必须由所属模块按依赖顺序执行（PLAN.md 第 13、14 节），
//     通用服务接口顺手停一个共享服务会绕过卷依赖检查。
//   - 不做“按名字前缀猜保护”（systemd- 除外）：o 其他的守护进程由管理员自己负责。
var protectionRules = []protectionRule{
	{
		category: ProtectSelf,
		reason:   "面板或代理自身：停止后会失去管理通道，只能到主机上恢复",
		exact: map[string]bool{
			"upanel-agent": true,
			"upanel-web":   true,
		},
	},
	{
		category: ProtectSSH,
		reason:   "SSH 远程登录：停止后可能无法再连上这台服务器",
		exact: map[string]bool{
			"ssh":  true,
			"sshd": true,
		},
	},
	{
		category: ProtectCore,
		reason:   "systemd 核心组件：停止会破坏日志、会话或设备管理",
		exact: map[string]bool{
			"systemd-journald": true,
			"systemd-logind":   true,
			"systemd-udevd":    true,
			"dbus":             true,
			"polkit":           true,
		},
		prefix: "systemd-",
	},
	{
		category: ProtectNetwork,
		reason:   "网络基础服务：停止后面板自身也会失去连接",
		exact: map[string]bool{
			"systemd-networkd":  true,
			"systemd-resolved":  true,
			"NetworkManager":    true,
			"networking":        true,
			"wpa_supplicant":    true,
			"systemd-timesyncd": true,
		},
	},
	{
		category: ProtectOtherModule,
		reason:   "该服务由反代模块管理：请通过反向代理页面操作，以便检查域名与证书依赖",
		exact: map[string]bool{
			"nginx": true,
		},
	},
	{
		category: ProtectOtherModule,
		reason:   "该服务由共享模块管理：请通过共享页面操作，以便检查卷挂载依赖",
		exact: map[string]bool{
			"smbd":    true,
			"nmbd":    true,
			"samba":   true,
			"winbind": true,
		},
	},
	{
		category: ProtectOtherModule,
		reason:   "该服务由容器模块管理：请通过容器页面操作",
		exact: map[string]bool{
			"docker":     true,
			"containerd": true,
			"podman":     true,
		},
	},
}

// Protect 计算一个单元的保护判定。
//
// 输入是单元名；返回的理由去重并按字典序排序，界面与测试因此稳定。
// 多条规则同时命中时（例如 systemd-resolved 既是 systemd- 前缀又是网络名单），
// 保留第一条规则的类别，理由全部列出——理由比分类更重要。
func Protect(unit string) Protection {
	base := baseName(unit)
	reasons := make([]string, 0, 2)
	category := ""
	for _, rule := range protectionRules {
		hit := rule.exact[base]
		if !hit && rule.prefix != "" && strings.HasPrefix(base, rule.prefix) {
			hit = true
		}
		if !hit {
			continue
		}
		if category == "" {
			category = rule.category
		}
		reasons = append(reasons, rule.reason)
	}
	if len(reasons) == 0 {
		return Protection{Protected: false, Reasons: []string{}}
	}
	return Protection{
		Protected: true,
		Category:  category,
		Reasons:   dedupeSorted(reasons),
	}
}

// dedupeSorted 去重并排序（理由是给人看的文本，顺序稳定才便于测试与阅读）。
func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
