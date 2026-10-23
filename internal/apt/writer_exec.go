package apt

// 执行与核对（P11.2 的 Execute）。
//
// 一次写事务的执行分成四段，每一段都留下证据：
//
//  1. **执行前复核锁**。预检到执行之间可能过了几十秒，外部 apt 可能已经开跑。
//  2. **跑 apt-get**。参数与模拟时完全一致，只把 `-s` 去掉。
//  3. **再读一遍 dpkg 数据库**。这不是"确认命令没报错"，是"确认系统真的变成了
//     模拟说的样子"——包括版本号。
//  4. **逐条核对**。计划里的每一步都要在读回的事实里找到对应；对不上就是
//     partial，并点名是哪个包、期望什么、实际是什么。
//
// 这一层不做自动修复：apt 中断之后**不**跑 `dpkg --configure -a`。自动修复会把
// "apt 为什么中断"这条线索抹掉，而且用户可能正想看看现场。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// Execute 执行一次写事务。
//
// progress 是粗粒度进度（模拟 → 执行 → 核对）。这里**不编造百分比**：
// apt-get 不提供可靠的进度，硬造一个 0→100 的动画只是在骗人。
func (w *Writer) Execute(ctx context.Context, op Op, names []string, progress func(step string, fields map[string]any)) (ExecResult, error) {
	if !ValidOp(op) {
		return ExecResult{}, fmt.Errorf("%w: %q", ErrOpUnsupported, op)
	}
	names = normalizedNames(names)
	result := ExecResult{Op: op, Names: names}
	report := func(step string, fields map[string]any) {
		if progress != nil {
			progress(step, fields)
		}
	}

	// 1) 锁复核。
	busy := BusyLocks(w.locks(w.paths.LockPaths()))
	if len(busy) > 0 {
		paths := make([]string, 0, len(busy))
		for _, lock := range busy {
			paths = append(paths, lock.Path)
		}
		return result, fmt.Errorf("%w（%s）", ErrBusy, strings.Join(paths, "、"))
	}

	// 2) 先模拟一次，拿到"执行前"的计划：它既是审计材料，也是核对的基准。
	//    重复一次模拟的成本是秒级，换来的是"执行时用的清单与预检时同一套规则"。
	result.IndexBefore = w.indexUpdatedAt()
	report("simulate", map[string]any{"operation": string(op), "packages": names})
	plan, err := w.simulate(ctx, op, names)
	if err != nil {
		return result, err
	}
	result.Plan = plan
	if plan.Failed() {
		// 走到这一步说明预检与执行之间世界变了（索引刷新、依赖变化）。
		return result, fmt.Errorf("apt: 执行前的模拟被拒绝：%s", strings.Join(simFailureLines(plan), "；"))
	}

	// 3) 执行。
	report("execute", map[string]any{"operation": string(op), "packages": names})
	start := time.Now()
	res, runErr := w.run(ctx, command(w.paths.AptGet, aptGetArgs(op, names, false), true),
		limits(writeTimeout, writeMaxStdout, writeMaxStderr))
	result.Duration = time.Since(start)
	result.Stdout = res.Stdout
	result.Stderr = res.Stderr
	result.ExitCode = res.ExitCode

	// 4) 无论成功失败都读回事实。失败时的读回同样重要：它告诉我们"改到一半"是什么样。
	report("verify", map[string]any{"exit_code": res.ExitCode})
	after, stateErr := w.dpkgStateFresh(ctx)
	if stateErr != nil {
		return result, fmt.Errorf("apt: 执行后读不到 dpkg 状态，无法核对结果: %w", stateErr)
	}
	result.IndexUpdatedAt = w.indexUpdatedAt()
	result.Observed = observedFor(names, plan, after)
	result.Mismatches = mismatchesFor(plan, after)
	result.Verified = runErr == nil && len(result.Mismatches) == 0
	result.VerifiedAt = w.now().UTC()

	if runErr != nil {
		// 真实错误原样带出去，由任务层写进 error（而不是 result）。
		return result, fmt.Errorf("apt-get %s 失败（退出码 %d）：%s", op, res.ExitCode, firstMeaningfulLine(res.Stderr, res.Stdout, plan))
	}
	return result, nil
}

// Refresh 刷新软件源索引。
//
// 它的"验证"与安装不同：`apt-get update` 的产物是索引本身，所以核对的是
// 「退出码 0 + 输出里没有 Err:/E: + 索引时间确实往前走了（或者本来就新）」。
// 索引时间没变不是失败——源上本来就没有新东西。
func (w *Writer) Refresh(ctx context.Context, progress func(step string, fields map[string]any)) (ExecResult, error) {
	result := ExecResult{Op: OpRefresh}
	report := func(step string, fields map[string]any) {
		if progress != nil {
			progress(step, fields)
		}
	}
	busy := BusyLocks(w.locks(w.paths.LockPaths()))
	if len(busy) > 0 {
		paths := make([]string, 0, len(busy))
		for _, lock := range busy {
			paths = append(paths, lock.Path)
		}
		return result, fmt.Errorf("%w（%s）", ErrBusy, strings.Join(paths, "、"))
	}

	result.IndexBefore = w.indexUpdatedAt()
	report("execute", map[string]any{"operation": string(OpRefresh)})
	start := time.Now()
	res, runErr := w.run(ctx, command(w.paths.AptGet, []string{"-q", "update"}, true),
		limits(simulateTimeout, writeMaxStdout, writeMaxStderr))
	result.Duration = time.Since(start)
	result.Stdout = res.Stdout
	result.Stderr = res.Stderr
	result.ExitCode = res.ExitCode
	result.IndexUpdatedAt = w.indexUpdatedAt()
	result.VerifiedAt = w.now().UTC()

	result.RefreshErrors, result.RefreshWarnings = refreshMessages(res.Stdout + "\n" + res.Stderr)
	result.Verified = runErr == nil && len(result.RefreshErrors) == 0
	if runErr != nil {
		return result, fmt.Errorf("apt-get update 失败（退出码 %d）：%s",
			res.ExitCode, firstMeaningfulLine(res.Stderr, res.Stdout, Simulation{}))
	}
	if len(result.RefreshErrors) > 0 {
		return result, fmt.Errorf("apt-get update 有错误行：%s", joinCapped(result.RefreshErrors, 3))
	}
	return result, nil
}

// refreshMessages 从 `apt-get update` 的输出里分出错误与告警。
//
// 只认 apt 自己的 `E:` / `W:` / `Err:` 前缀：apt-get update 对单个源的失败
// 只打印一行 `Err:` 并且**整体退出码可能是 0**（其它源成功了），所以退出码不够。
func refreshMessages(out string) (errs, warnings []string) {
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "E: "), strings.HasPrefix(line, "Err:"):
			errs = append(errs, line)
		case strings.HasPrefix(line, "W: "), strings.HasPrefix(line, "WARNING: "):
			warnings = append(warnings, line)
		}
	}
	return dedupeKeepOrder(errs), dedupeKeepOrder(warnings)
}

// observedFor 收集"相关包"执行后的真实状态：计划里涉及的每一个包 + 调用方点名的包。
func observedFor(names []string, plan Simulation, state *dpkgState) map[string]ObservedPackage {
	touched := make(map[string]bool, len(names)+len(plan.Steps))
	for _, name := range names {
		touched[BaseName(name)] = true
	}
	for _, step := range plan.Steps {
		touched[BaseName(step.Package)] = true
	}
	for _, name := range plan.Remove {
		touched[BaseName(name)] = true
	}
	out := make(map[string]ObservedPackage, len(touched))
	for name := range touched {
		out[name] = ObservedPackage{Status: state.status[name], Version: state.version[name]}
	}
	return out
}

// mismatchesFor 逐条核对计划。"对不上"的三种形态都要点名。
func mismatchesFor(plan Simulation, state *dpkgState) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(pkg, message string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		out = append(out, message)
	}
	for _, step := range plan.Steps {
		base := BaseName(step.Package)
		status := state.status[base]
		version := state.version[base]
		switch step.Kind {
		case StepInstall, StepUpgrade, StepDowngrade:
			if !strings.HasPrefix(status, "ii") {
				add(base, fmt.Sprintf("%s 没有装上（状态 %q）", step.Package, status))
				continue
			}
			if step.ToVersion != "" && version != step.ToVersion {
				add(base, fmt.Sprintf("%s 装上的是 %s，apt 说要装 %s", step.Package, version, step.ToVersion))
			}
		case StepRemove:
			// `apt-get remove` 会留下配置文件，状态从 `ii` 变成 `rc`——这是**正常**的
			// 卸载结果，不是"没删掉"。所以判据是"不再是 ii"，不是"记录消失"。
			if strings.HasPrefix(status, "ii") {
				add(base, fmt.Sprintf("%s 仍然是安装状态（%s）", step.Package, version))
			}
		}
	}
	sort.Strings(out)
	return out
}

// firstMeaningfulLine 从 apt 的输出里挑一句最能说明问题的话。
//
// 顺序是：stderr 的 E: 行 → stdout 的 E: 行 → 模拟里记下的 E: 行 → stderr 最后一行
// → stdout 最后一行。都是为了"任务失败时页面上有一句人话"，而不是一整屏输出。
func firstMeaningfulLine(stderr, stdout string, plan Simulation) string {
	for _, text := range []string{stderr, stdout} {
		for _, raw := range strings.Split(text, "\n") {
			line := strings.TrimSpace(raw)
			if strings.HasPrefix(line, "E: ") {
				return line
			}
		}
	}
	if len(plan.Errors) > 0 {
		return plan.Errors[0]
	}
	for _, text := range []string{stderr, stdout} {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if line := strings.TrimSpace(lines[i]); line != "" {
				return line
			}
		}
	}
	return "apt 没有输出错误信息"
}

// dpkgState 读 dpkg 数据库的当前状态（带 5 秒缓存）。
func (w *Writer) dpkgState(ctx context.Context) (*dpkgState, error) {
	w.mu.Lock()
	cached := w.state
	w.mu.Unlock()
	if cached != nil && w.now().Sub(cached.at) < dpkgStateFresh {
		return cached, nil
	}
	return w.dpkgStateFresh(ctx)
}

// dpkgStateFresh 强制重读 dpkg 数据库。
//
// 一次 `dpkg-query` 同时拿到状态、版本与 Essential 位：三个字段来自同一份数据库快照，
// 分成三次查询会让"这一版是 Essential 吗"与"这一版是哪个版本"取自不同时刻。
func (w *Writer) dpkgStateFresh(ctx context.Context) (*dpkgState, error) {
	const format = `${binary:Package}\t${db:Status-Abbrev}\t${Version}\t${Essential}\n`
	res, err := w.run(ctx, commandWithFormat(w.paths.DpkgQuery, format), limits(dpkgQueryTimeout, dpkgQueryMaxOut, dpkgQueryMaxOut))
	if err != nil {
		return nil, fmt.Errorf("apt: 读 dpkg 状态失败: %w", err)
	}
	state := &dpkgState{
		at:        w.now(),
		status:    make(map[string]string, 1024),
		version:   make(map[string]string, 1024),
		essential: make(map[string]bool, 32),
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		name := BaseName(fields[0])
		state.status[name] = strings.TrimSpace(fields[1])
		state.version[name] = strings.TrimSpace(fields[2])
		if strings.TrimSpace(fields[3]) == "yes" {
			state.essential[name] = true
		}
	}
	w.mu.Lock()
	w.state = state
	w.mu.Unlock()
	return state, nil
}

// Essential 返回这台机器上标了 `Essential: yes` 的包名（供预检与测试使用）。
func (w *Writer) Essential(ctx context.Context) (map[string]bool, error) {
	state, err := w.dpkgState(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(state.essential))
	for name := range state.essential {
		out[name] = true
	}
	return out, nil
}

// indexUpdatedAt 读索引目录的 mtime（读不到就是 nil，不假装刚刷新过）。
func (w *Writer) indexUpdatedAt() *time.Time {
	if strings.TrimSpace(w.paths.ListsDir) == "" {
		return nil
	}
	info, err := w.stat(w.paths.ListsDir)
	if err != nil {
		return nil
	}
	at := info.ModTime().UTC()
	return &at
}

// command 构造一次非交互的 apt-get 调用。
func command(program string, args []string, nonInteractive bool) syscmd.Spec {
	spec := syscmd.Spec{Program: program, Args: args}
	if nonInteractive {
		spec.Env = aptNonInteractiveEnv()
	}
	return spec
}

// commandWithFormat 构造一次 dpkg-query 调用（-W -f 格式串）。
func commandWithFormat(program, format string) syscmd.Spec {
	return syscmd.Spec{Program: program, Args: []string{"-W", "-f", format}}
}

// limits 是 syscmd 的限额（零值会被 syscmd 换成默认值）。
func limits(timeout time.Duration, stdout, stderr int64) syscmd.Limits {
	return syscmd.Limits{Timeout: timeout, MaxStdout: stdout, MaxStderr: stderr}
}

// asExitError 判断 err 是不是"命令跑完但非零退出"。
//
// 这两种情况在 apt 上是完全不同的意思：非零退出是 apt 的**结论**（依赖不满足、
// 找不到包），而启动失败/超时是面板自己的问题。只有后者才该报内部错误。
func asExitError(err error) (*syscmd.ExitError, bool) {
	var exitErr *syscmd.ExitError
	if errors.As(err, &exitErr) {
		return exitErr, true
	}
	return nil, false
}
