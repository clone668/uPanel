package apt

// 预检（P11.2 的 Assess）。
//
// 预检本身**不写系统**：探测锁、读 dpkg 状态、跑一次 `apt-get -s`（模拟不落盘），
// 然后把"能不能做、为什么不能、会发生什么"整理成一份结论。所有判断都来自现场读数，
// 没有一条来自缓存或配置。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Assess 计算一次写事务的预检结论。
func (w *Writer) Assess(ctx context.Context, op Op, names []string) (Assessment, error) {
	if !ValidOp(op) {
		return Assessment{}, fmt.Errorf("%w: %q", ErrOpUnsupported, op)
	}
	names = normalizedNames(names)
	state, err := w.dpkgState(ctx)
	if err != nil {
		return Assessment{}, err
	}
	indexAt := w.indexUpdatedAt()

	assessment := Assessment{
		Op:             op,
		DisplayName:    displayName(op, names),
		Identity:       identityFor(op, names),
		StateVersion:   stateVersion(indexAt, state, names),
		Locks:          []string{"apt-global"},
		IndexUpdatedAt: indexAt,
	}

	// 1) 锁。外部 apt 正在跑时不做任何事——这不是"排队等一会儿"，是明确拒绝。
	lockStates := w.locks(w.paths.LockPaths())
	assessment.BusyLocks = BusyLocks(lockStates)
	for _, lock := range lockStates {
		if lock.Error != "" {
			assessment.Blockers = append(assessment.Blockers, Reason{
				Code:    CodeLockUnreadable,
				Message: fmt.Sprintf("%s 探测失败：%s（读不到不等于没人占用，所以不放行）", lock.Path, lock.Error),
			})
		}
	}
	if len(assessment.BusyLocks) > 0 {
		paths := make([]string, 0, len(assessment.BusyLocks))
		for _, lock := range assessment.BusyLocks {
			paths = append(paths, lock.Path)
		}
		assessment.Blockers = append(assessment.Blockers, Reason{
			Code: CodeAptBusy,
			Message: fmt.Sprintf("有别的 apt/dpkg 正在运行（%s 被持有）：等它结束再操作，面板不会删除锁文件",
				strings.Join(paths, "、")),
		})
		// 锁被占着就不再模拟：模拟本身也要读索引，而且结论会过期。
		assessment.Allowed = false
		return assessment, nil
	}

	if op == OpRefresh {
		assessment.Impacts = []Impact{{
			Kind:        ImpactSoftware,
			Description: "重新下载软件源索引；已安装的软件不会改变",
		}}
		if indexAt != nil {
			if age := w.now().Sub(*indexAt); age > 24*time.Hour {
				assessment.Warnings = append(assessment.Warnings, Reason{
					Code:    WarningIndexAge,
					Message: fmt.Sprintf("索引已经 %s 没刷新了", humanDuration(age)),
				})
			}
		} else {
			assessment.Warnings = append(assessment.Warnings, Reason{
				Code:    WarningIndexAge,
				Message: "读不到索引时间（/var/lib/apt/lists 的 mtime）",
			})
		}
		assessment.Allowed = len(assessment.Blockers) == 0
		return assessment, nil
	}

	// 2) 现场核对"这些包在不在"。升级一个没装的包、卸载一个没装的包都不是"成功"。
	//
	// 这一条一旦命中就直接停下：剩下的模拟只会让 apt 把没装的包跳过一遍，然后
	// 给出"什么都没变"的结论——那会让界面看起来像"可以执行，只是没效果"，
	// 而真实情况是**名字给错了**，需要人去改。
	if missing := missingForOp(op, names, state); len(missing) > 0 {
		assessment.Blockers = append(assessment.Blockers, Reason{
			Code:    CodeNotInstalled,
			Message: fmt.Sprintf("这些包没有安装，%s 无从谈起：%s", opVerb(op), strings.Join(missing, "、")),
		})
		assessment.Warnings = append(assessment.Warnings, Reason{
			Code:    WarningNoChange,
			Message: "因为上面的包没有安装，这次预检没有跑模拟：先确认包名",
		})
		assessment.Allowed = false
		return assessment, nil
	}

	// 3) 模拟。这是整份预检的核心：apt 自己算一遍。
	sim, simErr := w.simulate(ctx, op, names)
	assessment.Simulation = sim
	if simErr != nil {
		// 模拟跑不起来（超时、apt-get 不存在）属于内部失败，不是"业务拒绝"。
		return Assessment{}, simErr
	}
	if sim.Failed() {
		assessment.Blockers = append(assessment.Blockers, Reason{
			Code:    CodeUnmet,
			Message: "apt 拒绝了这次操作：" + strings.Join(simFailureLines(sim), "；"),
		})
	}

	// 4) 保护名单：模拟的删除列表里有没有不该删的。
	assessment.Protected = ProtectedRemovals(sim, state.essential)
	if len(assessment.Protected) > 0 {
		details := make([]string, 0, len(assessment.Protected))
		for _, name := range assessment.Protected {
			protection := Protect(name, state.essential)
			details = append(details, fmt.Sprintf("%s（%s：%s）", name, protection.Category, protection.Reason))
		}
		assessment.Blockers = append(assessment.Blockers, Reason{
			Code:    CodeProtected,
			Message: "这次操作会卸载受保护的包：" + strings.Join(details, "；"),
		})
	}

	// 5) 顺手删掉的东西。不是错误，但必须点名——"我选了一个包，它删了 26 个"。
	assessment.Collateral = CollateralRemovals(sim, names)
	if len(assessment.Collateral) > 0 {
		assessment.Warnings = append(assessment.Warnings, Reason{
			Code:    WarningCollateral,
			Message: fmt.Sprintf("这次操作还会删除 %d 个没有点名的包：%s", len(assessment.Collateral), joinCapped(assessment.Collateral, 12)),
		})
	}

	// 6) 被保留的包（升级时 apt 因为依赖不动它们）。
	if len(sim.KeptBack) > 0 {
		kept := make([]string, 0, len(sim.KeptBack))
		for _, item := range sim.KeptBack {
			if item.Reason == "" {
				kept = append(kept, item.Package)
				continue
			}
			kept = append(kept, fmt.Sprintf("%s（%s）", item.Package, item.Reason))
		}
		assessment.Warnings = append(assessment.Warnings, Reason{
			Code:    WarningKeptBack,
			Message: fmt.Sprintf("apt 会保留 %d 个包不升级：%s", len(sim.KeptBack), joinCapped(kept, 8)),
		})
	}

	// 7) 什么都不变。这不是失败，但界面必须说清楚，否则用户会以为面板没干活。
	if !sim.Changed() {
		assessment.Warnings = append(assessment.Warnings, Reason{
			Code:    WarningNoChange,
			Message: noChangeMessage(op, names, sim),
		})
	}

	assessment.Impacts = impactsFor(op, names, sim)
	assessment.Allowed = len(assessment.Blockers) == 0
	return assessment, nil
}

// simulate 跑一次 `apt-get -s`。
func (w *Writer) simulate(ctx context.Context, op Op, names []string) (Simulation, error) {
	args := aptGetArgs(op, names, true)

	res, err := w.run(ctx, command(w.paths.AptGet, args, true), limits(simulateTimeout, writeMaxStdout, writeMaxStderr))
	if err != nil {
		// 非零退出是 apt 的**结论**（依赖不满足、找不到包），不是执行失败：
		// 输出里那句 `E:` 才是要展示给管理员的东西。启动失败/超时才是内部错误。
		if _, isExit := asExitError(err); !isExit {
			return Simulation{}, fmt.Errorf("apt: 模拟失败: %w", err)
		}
	}
	sim := ParseSimulation(res.Stdout+"\n"+res.Stderr, names)
	return sim, nil
}

// simulationVerb 是模拟时用的动词。
//
// 升级用 `install --only-upgrade` 而不是 `upgrade`：`apt-get upgrade` 会自己挑包，
// 而面板要的是"用户明确选中的这些包"。`--only-upgrade` 让没装的包被跳过
// （apt 会说 `Skipping … only upgrades are requested`），不会顺手装上。
func simulationVerb(op Op) []string {
	switch op {
	case OpRemove:
		return []string{"remove"}
	case OpUpgrade:
		return []string{"install", "--only-upgrade"}
	default:
		return []string{"install"}
	}
}

// execVerb 是真正执行时用的动词（与模拟必须完全一致，否则验的不是同一件事）。
func execVerb(op Op) []string { return simulationVerb(op) }

// aptGetArgs 拼一次 apt-get 的完整参数。
//
// 模拟与执行**共用这一个函数**：唯一的差别是 `-s`。分开拼过一次，结果就是执行
// 漏掉了 `-y` 与非交互的 conffile 策略——那会让 apt 在没有终端的情况下要么挂住、
// 要么报 "Abort"。让两边从同一个函数长出来，这种缺陷就没有落点。
func aptGetArgs(op Op, names []string, simulate bool) []string {
	args := make([]string, 0, 12+len(names))
	if simulate {
		args = append(args, "-s")
	}
	args = append(args, aptGetNonInteractive()...)
	args = append(args, execVerb(op)...)
	args = append(args, names...)
	return args
}

// missingForOp 返回"这个操作要求装着、但实际没装"的包。
//
// 只对 upgrade/remove 有意义：install 的目标本来就可以是没装的包。
func missingForOp(op Op, names []string, state *dpkgState) []string {
	if op != OpUpgrade && op != OpRemove {
		return nil
	}
	out := make([]string, 0, 2)
	for _, name := range names {
		status, ok := state.status[BaseName(name)]
		if ok && strings.HasPrefix(status, "ii") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// simFailureLines 把 apt 的拒绝理由压成一行能读的话。
func simFailureLines(sim Simulation) []string {
	out := make([]string, 0, 4)
	out = append(out, sim.Errors...)
	if len(sim.Unmet) > 0 {
		// unmet 块的第一行通常是"包 : 依赖关系"，后面是缩进的细节；取前 3 行够说明问题了。
		out = append(out, joinCapped(sim.Unmet, 3))
	}
	for _, warning := range sim.Warnings {
		if strings.Contains(warning, "essential") {
			out = append(out, warning)
		}
	}
	if len(out) == 0 {
		out = append(out, "apt 没有给出原因（模拟输出里没有 E: 行）")
	}
	return out
}

// impactsFor 生成影响列表。
func impactsFor(op Op, names []string, sim Simulation) []Impact {
	switch op {
	case OpRemove:
		extra := len(sim.Remove) - len(names)
		if extra < 0 {
			extra = 0
		}
		return []Impact{{
			Kind:        ImpactRemoval,
			Description: fmt.Sprintf("将卸载 %d 个软件包（其中 %d 个是为此连带删除的）", len(sim.Remove), extra),
		}}
	case OpInstall, OpUpgrade:
		return []Impact{{
			Kind:        ImpactSoftware,
			Description: fmt.Sprintf("将安装 %d 个、升级 %d 个软件包", len(sim.New)+len(sim.Additional), len(sim.Upgrade)),
		}}
	}
	return nil
}

// noChangeMessage 解释"为什么这次操作不会改变任何东西"。
func noChangeMessage(op Op, names []string, sim Simulation) string {
	switch {
	case len(sim.AlreadyNewest) > 0:
		return "已经是最新版本：" + strings.Join(sim.AlreadyNewest, "、")
	case len(sim.Skipped) > 0:
		return "没有安装这些包，而升级只处理已安装的：" + strings.Join(sim.Skipped, "、")
	case op == OpRemove:
		return "这些包本来就没有安装：" + strings.Join(names, "、")
	}
	return "apt 的模拟结果显示不需要做任何改动"
}

// displayName 是目标描述（界面上的"你要动的是谁"）。
func displayName(op Op, names []string) string {
	if op == OpRefresh {
		return "软件源索引"
	}
	if len(names) == 1 {
		return fmt.Sprintf("%s %s", opVerb(op), names[0])
	}
	return fmt.Sprintf("%s %d 个软件包：%s", opVerb(op), len(names), joinCapped(names, 6))
}

// opVerb 是操作的中文动词。
func opVerb(op Op) string {
	switch op {
	case OpInstall:
		return "安装"
	case OpRemove:
		return "卸载"
	case OpUpgrade:
		return "升级"
	case OpRefresh:
		return "刷新索引"
	}
	return string(op)
}

// identityFor 是操作身份指纹：操作 + **排序后**的包名。
//
// 排序是必须的：`install a b` 与 `install b a` 是同一件事，指纹不一样会让同一意图
// 重试时被判成"换了参数"。包名以 `\n` 连接，不给"名字里带分隔符"留歧义。
func identityFor(op Op, names []string) string {
	sorted := normalizedNames(names)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte("apt/v1|" + string(op) + "|" + strings.Join(sorted, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// stateVersion 是这些包当前状态的版本：索引时间 + 每个包的已安装版本。
//
// 预检到执行之间，只要索引刷新了或某个包被装了/卸了，这个值就变，执行前复核会
// 判 PREFLIGHT_STALE。它不包含"候选版本"：候选属于索引，索引时间已经覆盖了。
func stateVersion(indexAt *time.Time, state *dpkgState, names []string) string {
	var b strings.Builder
	if indexAt != nil {
		b.WriteString(indexAt.UTC().Format(time.RFC3339Nano))
	} else {
		b.WriteString("index-unknown")
	}
	sorted := normalizedNames(names)
	sort.Strings(sorted)
	for _, name := range sorted {
		b.WriteString("|")
		b.WriteString(BaseName(name))
		b.WriteString("=")
		b.WriteString(state.version[BaseName(name)])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalizedNames 去空白、去重、保持顺序。
func normalizedNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// joinCapped 拼接并截断（预检里展示用，避免一行几万字）。
func joinCapped(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, "、")
	}
	return strings.Join(items[:max], "、") + fmt.Sprintf(" 等 %d 个", len(items))
}

// humanDuration 把时长说成人话（只用于提示，不参与判断）。
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
}
