package shares

// 预检里的配置状态判决（P08.2）。
//
// 这一层回答的不是“定义对不对”，而是“这份定义此刻能不能真的被写进这台机器的
// Samba”。两者必须分开：定义合法但与既有配置撞名、全局项会被覆盖、上一份配置已经
// 与登记表漂移——这些都不是用户填错了参数，而是机器现在的样子不允许。
//
// 一条纪律贯穿全文件：**能说清就逐项说清，说不清就说说不清**。解析不了的 include
// 会让“有没有同名段”这个问题没有确定答案，那就给一条告警而不是默认“没有冲突”。

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// ConfigLockName 是托管配置的全局资源锁。
//
// 托管配置文件是**整份重写**的：任何一次共享动作都会读一遍现状、再把全集写下去。
// 两个人同时应用两份不同的共享时，后一个读到的“现状”里还没有前一个的段，写下去就把它
// 抹掉了。所以除了 share:<名字> 与 volume:<id>，还要一把覆盖整个托管配置的锁。
const ConfigLockName = "samba:config"

// overrideProbeTimeout 是读一项全局设置有效值的预算。
//
// 比整份配置校验短得多：`testparm --parameter-name` 不做完整校验，只是查表，
// 慢下来通常意味着机器整体不健康，继续等不会给出更好的答案。
const overrideProbeTimeout = 5 * time.Second

// configAssessment 判决一次“写配置”的前置条件。
func (e *Engine) configAssessment(ctx context.Context, def ShareDefinition) (blockers, warnings []Blocker) {
	capv := e.Capability(ctx)

	// 1. 工具与可读性。这三件事缺任何一件都不能写配置（见 capability.go 的说明）。
	if !capv.ConfigReadable {
		return append(blockers, Blocker{
			Code:    NoticeConfigNotReadable,
			Message: "读不到 " + capv.ConfigPath + "：不知道这台机器现有的共享长什么样，不能动它",
		}), nil
	}
	if capv.Tools.Testparm == "" {
		blockers = append(blockers, Blocker{
			Code:    NoticeTestparmMissing,
			Message: "没有 testparm：无法在替换文件之前校验候选配置，因此不写 Samba 配置",
		})
	}
	if capv.Tools.Smbcontrol == "" {
		blockers = append(blockers, Blocker{
			Code:    NoticeConfigNotWritable,
			Message: "没有 smbcontrol：配置写得进去，但改完没法让它生效（重载是唯一不需要重启的做法）",
		})
	}

	// 2. 现有配置的现场。读不到就到此为止：下面的每一条判断都建立在它之上。
	p := e.paths()
	fs := e.fs()
	main, err := fs.ReadFile(p.Main)
	if err != nil {
		if !os.IsNotExist(err) {
			blockers = append(blockers, Blocker{
				Code:    NoticeConfigNotReadable,
				Message: "读 " + p.Main + " 失败：" + err.Error(),
			})
			return blockers, warnings
		}
		// 主配置不存在：这台机器上 Samba 还没配过。这不算冲突，但也不是“查过了没问题”。
		warnings = append(warnings, Blocker{
			Code:    NoticeConfigNotReadable,
			Message: p.Main + " 不存在：应用时会新建 include 接入点，但现有 Samba 配置无从对照",
		})
	}
	sections, unresolved := ConfiguredSections(main, fs.ReadFile)

	// 3. 同名段。**大小写不敏感**：Samba 内部按小写比较段名，所以 [Data] 与 [data]
	//    是同一份共享。只做大小写敏感的比较会放行一次“看着不同、实际撞上”的写入。
	if def.Enabled {
		lower := strings.ToLower(def.Name)
		if src, taken := sections[lower]; taken {
			message := "现有 Samba 配置里已经有 [" + def.Name + "] 这一段（来自 " + src +
				"）：不允许靠重名覆盖不在面板登记表里的共享"
			if src == ManagedConfigPath {
				message = "托管配置里已经有 [" + def.Name + "] 这一段：这份共享已经被别处写入过"
			}
			blockers = append(blockers, Blocker{Code: NoticeShareNameTakenBySystem, Message: message})
		}
	}

	// 4. 解析不了的 include：这不是“没冲突”，是“查不全”。
	if len(unresolved) > 0 {
		warnings = append(warnings, Blocker{
			Code: NoticeConfigIncludeUnresolved,
			Message: "现有配置里有 " + strings.Join(unresolved, "、") +
				" 这类解析不了的 include（含 % 变量或通配符）：有没有同名段无法完全确认",
		})
	}

	// 5. 漂移：托管文件与登记表对不上（有人手工改过，或者上次写入没走到最后）。
	if drift := e.configDriftNames(ctx); len(drift) > 0 {
		warnings = append(warnings, Blocker{
			Code: NoticeShareConfigDrift,
			Message: "托管配置与登记表不一致：" + strings.Join(drift, "、") +
				"；应用任意一份共享都会把整个托管文件按登记表重写回去",
		})
	}

	// 6. 全局项会被改写。这条是告警不是阻塞：它确实是用户想做的事（最低 SMB2、
	//    禁 guest），但“现在是什么 → 会变成什么”必须写在确认页上。
	if overrides := e.globalOverrides(ctx); len(overrides) > 0 {
		parts := make([]string, 0, len(overrides))
		for _, o := range overrides {
			parts = append(parts, o.Key+"："+o.Current+" → "+o.Desired)
		}
		warnings = append(warnings, Blocker{
			Code:    NoticeConfigGlobalOverride,
			Message: "托管配置会覆盖这些全局设置（对全部共享生效）：" + strings.Join(parts, "；"),
		})
	}
	return blockers, warnings
}

// shareCurrentlyApplied 报告同名共享此刻是否已经写进了 Samba 配置。
func (e *Engine) shareCurrentlyApplied(ctx context.Context, name string) bool {
	rec, found, err := e.opts.Store.GetShareByName(ctx, name)
	if err != nil || !found {
		return false
	}
	return rec.ConfigApplied
}

// configDriftNames 报告托管文件与登记表不一致的共享名。
func (e *Engine) configDriftNames(ctx context.Context) []string {
	records, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil
	}
	managed, err := e.fs().ReadFile(e.paths().Managed)
	if err != nil {
		// 文件不存在：登记表里“已应用”的那些共享就是漂移了的（配置里没有它们）。
		// 这里不把 err 往上抛：漂移判断失败不该阻止用户做事，只是少一条告警。
		managed = nil
	}
	return configDrift(records, SectionNames(managed))
}

// globalOverrides 读出托管全局项**此刻**的有效值，与我们要写的值不同的列出来。
//
// 用 testparm 读有效值而不是解析 smb.conf 的文本：Samba 的有效值包含默认值，而
// “把默认的 Bad User 改成 Never”正是一次需要被看见的行为变化——文件里根本没有那
// 一行，只有 loadparm 算出来的结果能把它说出来。
//
// 预检与执行都用它（执行侧在写配置的流水线里调用，结果进 ManagedConfigReport）。
func (e *Engine) globalOverrides(ctx context.Context) []ConfigOverride {
	tool := e.tools(ctx).Testparm
	if tool == "" {
		return nil
	}
	p := e.paths()
	var out []ConfigOverride
	for _, kv := range managedGlobalSettings {
		res, err := e.run(ctx, syscmd.Spec{
			Program: tool,
			Args:    []string{"-s", "--parameter-name=" + kv.Key, p.Main},
		}, overrideProbeTimeout)
		if err != nil || res.ExitCode != 0 {
			// 读不出来就不列：把“没读出来”写成“会被覆盖”是编造。
			continue
		}
		current := strings.TrimSpace(res.Stdout)
		if current == "" || strings.EqualFold(current, kv.Value) {
			continue
		}
		out = append(out, ConfigOverride{Key: kv.Key, Current: current, Desired: kv.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
