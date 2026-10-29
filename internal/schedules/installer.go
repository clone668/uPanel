package schedules

// systemd 单元的落地（P12.4；PLAN.md 第 17 节）。
//
// 这一层回答四个问题，每个都对应一条会咬人的事实：
//
//	表达式能不能被 systemd 认     先跑 `systemd-analyze calendar`，不认就不许写单元。
//	                              自己写个正则"看着像就行"，最后换来的是一台永远不会触发的
//	                              timer——没有报警，只有一个安静的、什么都没发生过的凌晨三点。
//	单元写对了吗                  临时文件 → fsync → rename → 目录 fsync。掉电时要么是旧单元，
//	                              要么是新单元，不会是半份。
//	enable 之后真的生效了吗       回读 systemctl 的事实（UnitFileState / ActiveState），
//	                              不以 enable 的退出码作为"已启用"的证据。
//	删除时服务停了吗              先 disable --now 再删文件；停不下来就不删，否则留下一个
//	                              正在跑、却已经没有单元文件的定时任务。
//
// 只碰 /etc/systemd/system/upanel-<id>.{service,timer}。名字由 id 派生，调用方给不了路径。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// Runner 执行一条外部命令（生产实现是 syscmd.Run）。
type Runner func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)

// ErrBadScheduleID 表示计划 id 形态不合法（不能拼进单元名）。
var ErrBadScheduleID = errors.New("schedules: 计划 id 不合法")

// 单元文件权限：0644，systemd 以 root 读它，没有秘密在里面。
const unitFileMode = 0o644

// systemctl / systemd-analyze 的候选绝对路径。
//
// 不走 PATH：PLAN.md 第 4 节要求只按绝对路径调用。两个候选覆盖了 Ubuntu 的
// merge-usr 前后两种布局。
var (
	systemctlCandidates = []string{"/usr/bin/systemctl", "/bin/systemctl"}
	analyzerCandidates  = []string{"/usr/bin/systemd-analyze", "/bin/systemd-analyze"}
)

// InstallerOptions 是构造参数。
type InstallerOptions struct {
	// Dir 是单元目录，默认 /etc/systemd/system。
	Dir string
	// Runner 默认 syscmd.Run。
	Runner Runner
	// Systemctl / Analyzer 是可执行文件路径；为空时从候选绝对路径里挑第一个存在的。
	Systemctl string
	Analyzer  string
	// Stat 用于挑候选路径，默认 os.Stat（测试用来固定"哪个候选存在"）。
	Stat func(string) (os.FileInfo, error)
}

// Installer 负责写、启停、读单元事实。
type Installer struct {
	dir       string
	run       Runner
	systemctl string
	analyzer  string
}

// NewInstaller 构造 Installer。
func NewInstaller(opts InstallerOptions) (*Installer, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = UnitDir
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("schedules: 单元目录必须是绝对路径，当前为 %q", dir)
	}
	run := opts.Runner
	if run == nil {
		run = syscmd.Run
	}
	stat := opts.Stat
	if stat == nil {
		stat = os.Stat
	}
	systemctl := strings.TrimSpace(opts.Systemctl)
	if systemctl == "" {
		if systemctl = firstExisting(stat, systemctlCandidates); systemctl == "" {
			return nil, errors.New("schedules: 找不到 systemctl，无法管理定时单元")
		}
	}
	analyzer := strings.TrimSpace(opts.Analyzer)
	if analyzer == "" {
		if analyzer = firstExisting(stat, analyzerCandidates); analyzer == "" {
			return nil, errors.New("schedules: 找不到 systemd-analyze，无法校验日历表达式")
		}
	}
	return &Installer{dir: dir, run: run, systemctl: systemctl, analyzer: analyzer}, nil
}

// Dir 返回单元目录。
func (i *Installer) Dir() string { return i.dir }

// CalendarCheck 是一次日历表达式校验的结果。
type CalendarCheck struct {
	// Normalized 是 systemd 归一化之后的表达式（人写的写法不一定就是它理解的样子）。
	Normalized string
	// NextElapse 是下一次触发的绝对时刻（来自 systemd-analyze 的 "(in UTC)" 行）。
	NextElapse time.Time
}

// ValidateCalendar 让 systemd 自己回答"这个表达式什么时候触发"。
//
// 时区钉死成 UTC（Spec.Env 的 `TZ=UTC`）：`Next elapse` 那一行用的是**进程时区**，
// 而它在进程时区就是 UTC 时会省掉 `(in UTC)` 那一行。不钉死的话，同一份代码在
// UTC 服务器上读不到下次触发时刻（P12.4 真机现场），在别的时区上又会把一个
// 三字母缩写当成偏移 0——两种都错，且都错得很安静。
func (i *Installer) ValidateCalendar(ctx context.Context, d Definition) (CalendarCheck, error) {
	line := CalendarLine(d)
	res, err := i.run(ctx, syscmd.Spec{
		Program: i.analyzer,
		Args:    []string{"calendar", line},
		Env:     []string{"TZ=UTC"},
	}, syscmd.Limits{Timeout: 30 * time.Second, MaxStdout: 256 << 10, MaxStderr: 64 << 10})
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return CalendarCheck{}, fmt.Errorf("systemd 不认这个日历表达式 %q：%s", line, firstLine(res.Stderr))
		}
		return CalendarCheck{}, fmt.Errorf("校验日历表达式时无法执行 %s：%w", i.analyzer, err)
	}
	check, err := parseCalendarOutput(res.Stdout)
	if err != nil {
		return CalendarCheck{}, fmt.Errorf("解析 systemd-analyze 的输出失败（%v）：%s", err, firstLine(res.Stdout))
	}
	return check, nil
}

// parseCalendarOutput 从 systemd-analyze calendar 的输出里取归一化形式与下一次触发时刻。
//
// 输出形如（systemd 255，进程时区 Asia/Shanghai）：
//
//	  Original form: *-*-* 03:30:00 Asia/Shanghai
//	Normalized form: *-*-* 03:30:00 Asia/Shanghai
//	    Next elapse: Sun 2026-10-25 03:30:00 CST
//	       (in UTC): Sat 2026-10-24 19:30:00 UTC
//	       From now: 18h left
//
// 而进程时区就是 UTC 时（ValidateCalendar 会把它钉成 UTC），同一份输出少一行：
//
//	Normalized form: *-*-* 04:30:00
//	    Next elapse: Mon 2026-10-26 04:30:00 UTC
//	       From now: 22h left
//
// 所以两行都认：有 `(in UTC)` 就以它为准，没有就把 `Next elapse` 当成 UTC 读。
// 只认 `(in UTC)` 会让 UTC 机器上的每一条计划都被判成"不会触发"（真机现场）。
func parseCalendarOutput(out string) (CalendarCheck, error) {
	check, sawNever, sawNext := parseCalendarLines(out)
	if sawNever || check.NextElapse.IsZero() {
		// "不会触发"和"读不出来"在用户面前是同一件事：这个定时任务不会跑。
		if sawNext {
			return CalendarCheck{}, errors.New("读不出下一次触发时刻")
		}
		return CalendarCheck{}, errors.New("这个表达式不会再有触发时刻")
	}
	if check.Normalized == "" {
		return CalendarCheck{}, errors.New("没有找到归一化形式")
	}
	return check, nil
}

// parseCalendarLines 遍历输出行，返回归一化形式、是否明确写了 never、是否见到 Next elapse。
func parseCalendarLines(out string) (CalendarCheck, bool, bool) {
	var check CalendarCheck
	var sawNever, sawNext bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := cutPrefixFold(line, "Normalized form:"); ok {
			check.Normalized = strings.TrimSpace(rest)
			continue
		}
		// `(in UTC)` 优先：它是唯一不依赖进程时区的写法。
		if rest, ok := cutPrefixFold(line, "(in UTC):"); ok {
			if t, ok := parseElapseValue(strings.TrimSpace(rest), &sawNever); ok {
				check.NextElapse = t
			}
			continue
		}
		if rest, ok := cutPrefixFold(line, "Next elapse:"); ok {
			sawNext = true
			if check.NextElapse.IsZero() {
				if t, ok := parseElapseValue(strings.TrimSpace(rest), &sawNever); ok {
					check.NextElapse = t
				}
			}
		}
	}
	return check, sawNever, sawNext
}

// parseElapseValue 解析一个"绝对时刻"取值，如 `2026-10-24 19:30:00 UTC`、
// `Mon 2026-10-25 04:30:00 UTC`、`never`。
//
// 只接受 UTC/GMT 后缀：别的三字母缩写（CST、EDT…）不是偏移，Go 会把它们解析成
// 偏移 0 的假时区，于是下次触发时刻会静默地差好几个小时。读不出来就承认读不出来，
// 由调用方判成"这个表达式不会再有触发时刻"，而不是拿一个错的时刻去写单元。
func parseElapseValue(value string, sawNever *bool) (time.Time, bool) {
	if strings.HasPrefix(strings.ToLower(value), "never") {
		*sawNever = true
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "Mon 2006-01-02 15:04:05 MST"} {
		t, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		if name := t.Location().String(); name != "UTC" && name != "GMT" {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	return time.Time{}, false
}

// cutPrefixFold 按前缀切分，忽略大小写（systemd 各版本的标签大小写不统一）。
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// Status 是一份单元事实。
type Status struct {
	TimerPath   string
	ServicePath string
	// Present 表示两个单元文件都在磁盘上。
	Present bool
	// UnitFileState 是 systemctl 的 UnitFileState（enabled / disabled / static / not-found）。
	UnitFileState string
	// ActiveState 是 systemctl 的 ActiveState（active / inactive / failed / unknown）。
	ActiveState string
	// NextElapseRaw 是 systemd 给出的下次触发时间原文；"n/a" 表示它自己也不知道。
	NextElapseRaw string
}

// Enabled 报告定时单元是否处于启用状态。
func (s Status) Enabled() bool {
	return s.UnitFileState == "enabled" || s.UnitFileState == "enabled-runtime"
}

// Active 报告定时单元当前是否在计时。
func (s Status) Active() bool { return s.ActiveState == "active" }

// Ensure 写入并启用单元；返回回读到的单元事实。
//
// 顺序不可换：先校验日历（不认就什么都不写），再落盘，再 daemon-reload，最后 enable --now。
// 反过来做的话，一次失败的 enable 会留下一对"文件在、没人知道"的单元。
func (i *Installer) Ensure(ctx context.Context, id string, d Definition) (Status, error) {
	if !ValidID(id) {
		return Status{}, fmt.Errorf("%w：%q", ErrBadScheduleID, id)
	}
	if _, err := i.ValidateCalendar(ctx, d); err != nil {
		return Status{}, err
	}
	service, err := RenderService(id, d)
	if err != nil {
		return Status{}, err
	}
	timer, err := RenderTimer(id, d)
	if err != nil {
		return Status{}, err
	}
	if err := os.MkdirAll(i.dir, 0o755); err != nil {
		return Status{}, fmt.Errorf("创建单元目录 %s 失败: %w", i.dir, err)
	}
	// service 先写：timer 一旦 enable 就会立刻启 service，此时它必须已经存在。
	if err := writeAtomic(i.dir, filepath.Base(i.servicePath(id)), service); err != nil {
		return Status{}, err
	}
	if err := writeAtomic(i.dir, filepath.Base(i.timerPath(id)), timer); err != nil {
		return Status{}, err
	}
	if _, err := i.daemonReload(ctx); err != nil {
		return Status{}, err
	}
	if err := i.systemctlRun(ctx, "enable", "--now", TimerName(id)); err != nil {
		return Status{}, err
	}
	return i.Status(ctx, id)
}

// Remove 停止并删除单元。
//
// 单元文件已经不在时只做一次 daemon-reload：这时 disable 会报"没有这个单元"，
// 那不是错误，是把上一次没做完的清理收尾。
func (i *Installer) Remove(ctx context.Context, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%w：%q", ErrBadScheduleID, id)
	}
	_, statErr := os.Stat(i.timerPath(id))
	_, statErrSvc := os.Stat(i.servicePath(id))
	if errors.Is(statErr, os.ErrNotExist) && errors.Is(statErrSvc, os.ErrNotExist) {
		_, err := i.daemonReload(ctx)
		return err
	}
	if err := i.systemctlRun(ctx, "disable", "--now", TimerName(id)); err != nil {
		// 停不下来就不删文件：删了会留下一个还在计时、却没有单元可查的任务。
		return fmt.Errorf("停用定时单元失败，单元文件保持不变：%w", err)
	}
	// timer 先删：留一个指向不存在 service 的 timer 比反过来安全。
	for _, path := range []string{i.timerPath(id), i.servicePath(id)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("删除单元文件 %s 失败: %w", path, err)
		}
	}
	_, err := i.daemonReload(ctx)
	return err
}

// Status 读单元事实。读不到就报 "not-found"，不把"没装"说成"停用"。
func (i *Installer) Status(ctx context.Context, id string) (Status, error) {
	if !ValidID(id) {
		return Status{}, fmt.Errorf("%w：%q", ErrBadScheduleID, id)
	}
	out := Status{TimerPath: i.timerPath(id), ServicePath: i.servicePath(id)}
	if _, err := os.Stat(out.TimerPath); err == nil {
		if _, err := os.Stat(out.ServicePath); err == nil {
			out.Present = true
		}
	}
	res, err := i.run(ctx, syscmd.Spec{
		Program: i.systemctl,
		Args: []string{"show", "--no-pager",
			"-p", "UnitFileState", "-p", "ActiveState", "-p", "NextElapseUSecRealtime",
			TimerName(id)},
	}, syscmd.Limits{Timeout: 30 * time.Second, MaxStdout: 256 << 10, MaxStderr: 64 << 10})
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return out, fmt.Errorf("读取定时单元状态失败：%s", firstLine(res.Stderr))
		}
		return out, fmt.Errorf("无法执行 %s：%w", i.systemctl, err)
	}
	fields := parseShow(res.Stdout)
	out.UnitFileState = fields["UnitFileState"]
	out.ActiveState = fields["ActiveState"]
	out.NextElapseRaw = fields["NextElapseUSecRealtime"]
	if out.UnitFileState == "" {
		out.UnitFileState = "not-found"
	}
	if out.ActiveState == "" {
		out.ActiveState = "unknown"
	}
	return out, nil
}

func (i *Installer) timerPath(id string) string   { return filepath.Join(i.dir, TimerName(id)) }
func (i *Installer) servicePath(id string) string { return filepath.Join(i.dir, ServiceName(id)) }

// daemonReload 让 systemd 重新读单元目录。
func (i *Installer) daemonReload(ctx context.Context) (syscmd.Result, error) {
	res, err := i.run(ctx, syscmd.Spec{
		Program: i.systemctl,
		Args:    []string{"daemon-reload"},
	}, syscmd.Limits{Timeout: 60 * time.Second, MaxStdout: 64 << 10, MaxStderr: 64 << 10})
	if err != nil {
		return res, fmt.Errorf("systemctl daemon-reload 失败：%s", firstLine(res.Stderr))
	}
	return res, nil
}

// systemctlRun 跑一条 systemctl 子命令，失败时把 stderr 的第一行带出去。
func (i *Installer) systemctlRun(ctx context.Context, args ...string) error {
	res, err := i.run(ctx, syscmd.Spec{Program: i.systemctl, Args: args},
		syscmd.Limits{Timeout: 60 * time.Second, MaxStdout: 64 << 10, MaxStderr: 64 << 10})
	if err != nil {
		return fmt.Errorf("systemctl %s 失败：%s", strings.Join(args, " "), firstLine(res.Stderr))
	}
	return nil
}

// parseShow 解析 `systemctl show` 的 key=value 输出。
func parseShow(out string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" {
			continue
		}
		fields[key] = value
	}
	return fields
}

// writeAtomic 原子地写一个单元文件。
func writeAtomic(dir, name string, data []byte) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".upanel-unit-*")
	if err != nil {
		return fmt.Errorf("创建单元临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入单元 %s 失败: %w", name, err)
	}
	// 先落盘再改名：rename 只保证名字原子，不保证内容已经在盘上。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步单元 %s 失败: %w", name, err)
	}
	if err := tmp.Chmod(unitFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置单元 %s 权限失败: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭单元 %s 失败: %w", name, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("发布单元 %s 失败: %w", name, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// firstExisting 返回第一个存在的候选路径。
func firstExisting(stat func(string) (os.FileInfo, error), candidates []string) string {
	for _, c := range candidates {
		if _, err := stat(c); err == nil {
			return c
		}
	}
	return ""
}

// firstLine 取输出的第一行非空内容，用于把外部命令的抱怨塞进一条错误里。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "（没有输出）"
}

// parseEpochMicros 把 NextElapseUSecRealtime 的纯数字形式（微秒时间戳）转成时间。
//
// systemctl 在部分版本上把这个属性打成人类可读的本机时间（带时区缩写），
// 那不是可以安全解析成绝对时刻的东西，所以这里只接受纯数字形式，其余情况让
// 调用方去看 NextElapseRaw 原文。
func parseEpochMicros(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "n/a" {
		return time.Time{}, false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return time.Time{}, false
		}
	}
	micros, err := strconv.ParseInt(v, 10, 64)
	if err != nil || micros <= 0 {
		return time.Time{}, false
	}
	return time.UnixMicro(micros).UTC(), true
}
