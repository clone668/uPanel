package schedules

// 受管理 timer 的落地形态（P12.4；PLAN.md 第 17 节，CONTRACTS.md 第 12 节）。
//
// uPanel 只写两种单元：`upanel-sch_<id>.service` 与 `upanel-sch_<id>.timer`。
// 单元名由计划 id 派生，调用方给不了名字——名字可写就能覆盖别人的单元。
//
// 四条规则在单元里必须**看得见**（PLAN §17：时区、下次执行时间、错过执行策略、
// 最大运行时长和不重叠规则必须明确）：
//
//	时区     OnCalendar 行尾直接带 IANA 时区名。不带时区的表达式按主机本地时区解释，
//	         于是“每天 03:00 备份”在两台机器上是两个时刻。
//	错过执行 Persistent=true 由 systemd 在开机后补跑一次；=false 就跳过。
//	最长运行 TimeoutStartSec 写死在单元的 [Service] 里，值取自 max_runtime_seconds
//	         再加一段余量（TriggerWaitSlackSeconds），与代理侧 script.run 的
//	         max_runtime_seconds 是同一个数。
//	不重叠   单元是 Type=oneshot，而 upanelctl 带 --wait：单元的主进程一直活到
//	         这次操作到达终态。unit 处于 active 时 systemd 不会起第二个实例；
//	         到期时撞上运行中的那次唤醒，可能被跳过，也可能被排到本次运行结束之后
//	         再起一次——两条都不并行，"不重叠"说的是不并行，不是一天只跑一次。
//	         这一点也写在单元注释里——读单元的人不用去翻文档。

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// UnitDir 是 uPanel 写自己单元文件的目录。
	//
	// 只有这里，不写 /lib/systemd/system（那是发行版的），也不写用户的 ~/.config。
	UnitDir = "/etc/systemd/system"

	// unitPrefix 是所有受管理单元的名字前缀。
	unitPrefix = "upanel-"

	// calendarSuffixComment 提示运维“这条 OnCalendar 到底在哪个时区”。
	triggerCommand = "/usr/local/bin/upanelctl schedule-trigger"
)

// TriggerWaitSlackSeconds 是单元 TimeoutStartSec 在计划最长运行时长之外多给的余量。
//
// upanelctl --wait 会一直守到操作到终态。代理杀进程、写结果、把阶段改成 failed
// 都要时间，所以单元的时限必须比计划的上限稍长：两个数相等就会出现"代理正好在
// 超时那一刻收尾、单元同一时刻被判超时"的错位，日志里留下一条与事实相反的 failed。
// 60 秒是这段收尾的上界（脚本收到 SIGTERM 后写结果通常不到 1 秒）。
const TriggerWaitSlackSeconds = 60

// UnitTimeoutSeconds 返回计划对应单元的 TimeoutStartSec 秒数。
func UnitTimeoutSeconds(d Definition) int {
	d = d.Normalize()
	return d.MaxRuntimeSeconds + TriggerWaitSlackSeconds
}

var scheduleIDPattern = regexp.MustCompile(`^sch_[0-9a-f]{32}$`)

// ValidID 判断计划 id 的形态。
//
// 由 store 生成，形态固定。执行前再核一次是因为单元名从它拼出来：一个带 `/` 或
// 空格的 id 会写到别的目录去，而这条路径上没有任何东西会报错。
func ValidID(id string) bool { return scheduleIDPattern.MatchString(id) }

// TimerName 返回定时器单元名（agent_timer_ref 存的就是它）。
func TimerName(scheduleID string) string { return unitPrefix + scheduleID + ".timer" }

// ServiceName 返回服务单元名。
func ServiceName(scheduleID string) string { return unitPrefix + scheduleID + ".service" }

// TimerPath 返回定时器单元文件路径。
func TimerPath(scheduleID string) string { return UnitDir + "/" + TimerName(scheduleID) }

// ServicePath 返回服务单元文件路径。
func ServicePath(scheduleID string) string { return UnitDir + "/" + ServiceName(scheduleID) }

// escapeUnitName 把可能含 % 的展示文本转义成单元文件里安全的写法。
//
// systemd 会展开 `%` 开头的说明符（`%n` 是单元名本身）。计划名由管理员填写，
// 不转义就等于允许一段文本改写单元内容。
func escapeUnitName(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// CalendarLine 返回 OnCalendar 的值：表达式 + 显式时区。
func CalendarLine(d Definition) string {
	d = d.Normalize()
	return d.CalendarExpression + " " + d.Timezone
}

// RenderService 渲染计划对应的服务单元。
//
// 触发路径只有一条：systemd 调 root 所有的固定 upanelctl，由它把计划 id 交给代理；
// 代理重新预检资源与锁、生成稳定触发 id 并审计。单元里没有动作名、没有参数、
// 没有命令替换——它只会说“这条计划到点了”。
func RenderService(scheduleID string, d Definition) ([]byte, error) {
	if !ValidID(scheduleID) {
		return nil, fmt.Errorf("schedules: 计划 id %q 不合法，不能拼进单元名", scheduleID)
	}
	d = d.Normalize()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	name := escapeUnitName(d.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "; uPanel 受管理计划任务：%s（%s）\n", name, scheduleID)
	b.WriteString(";\n")
	b.WriteString("; 这个文件由 uPanel 生成，每次 schedule.apply 整篇重写，不要手工修改——\n")
	b.WriteString("; 手工改动会在下一次保存时被覆盖，而覆盖后的行为以面板上的定义为准。\n")
	b.WriteString(";\n")
	b.WriteString("; 触发路径只有一条：本单元执行固定的 upanelctl schedule-trigger，由代理重新预检\n")
	b.WriteString("; 资源与锁、按稳定触发 id 幂等落库并审计。单元里没有动作名、没有参数、没有\n")
	b.WriteString("; 命令替换，改了单元也换不出另一个动作。\n")
	b.WriteString("\n[Unit]\n")
	fmt.Fprintf(&b, "Description=uPanel 计划任务：%s\n", name)
	b.WriteString("Documentation=file:/usr/local/share/doc/upanel/SCHEDULES.md\n")
	b.WriteString("After=network.target\n")
	b.WriteString("\n[Service]\n")
	// Type=oneshot：systemd 在单元 active 期间不会再启动第二个实例，这就是“不重叠”。
	b.WriteString("Type=oneshot\n")
	b.WriteString("User=root\n")
	fmt.Fprintf(&b, "ExecStart=%s --schedule-id %s --wait\n", triggerCommand, scheduleID)
	fmt.Fprintf(&b, "TimeoutStartSec=%ds\n", UnitTimeoutSeconds(d))
	b.WriteString("; 不重叠：oneshot + --wait。upanelctl 一直守到这次操作到达终态，单元在此期间一直处于 active；\n")
	b.WriteString("; systemd 不会为一个正在运行的 oneshot 单元再起一个实例；到期时撞上运行中的那次唤醒，\n")
	b.WriteString("; 可能被跳过，也可能被排到这次运行结束之后再起一次——两种都不并行。\n")
	b.WriteString("; 所以两份备份不会同时写同一个仓库，两次脚本也不会同时跑。\n")
	fmt.Fprintf(&b, "; TimeoutStartSec 比计划的最长运行时长多 %d 秒：那是代理杀进程并写结果的收尾。\n", TriggerWaitSlackSeconds)
	return []byte(b.String()), nil
}

// RenderTimer 渲染计划对应的定时器单元。
func RenderTimer(scheduleID string, d Definition) ([]byte, error) {
	if !ValidID(scheduleID) {
		return nil, fmt.Errorf("schedules: 计划 id %q 不合法，不能拼进单元名", scheduleID)
	}
	d = d.Normalize()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	name := escapeUnitName(d.Name)
	persistent := "false"
	misfire := "错过就跳过：停机期间到期的触发不会补跑"
	if d.Persistent {
		persistent = "true"
		misfire = "错过的触发在开机后补跑一次（Persistent=true）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "; uPanel 受管理定时器：%s（%s）\n", name, scheduleID)
	b.WriteString(";\n")
	fmt.Fprintf(&b, "; 时区    %s（写进 OnCalendar 行尾，不依赖主机本地时区）\n", d.Timezone)
	fmt.Fprintf(&b, "; 错过执行 %s\n", misfire)
	fmt.Fprintf(&b, "; 最长运行 %d 秒 + %d 秒收尾（由 %s 的 TimeoutStartSec 执行）\n", d.MaxRuntimeSeconds, TriggerWaitSlackSeconds, ServiceName(scheduleID))
	b.WriteString("; 不重叠  服务单元带 --wait，跑到操作终态才退出；active 期间不会起第二个实例（撞上的唤醒被跳过或排到跑完之后）\n")
	b.WriteString(";\n")
	b.WriteString("; 这个文件由 uPanel 生成，不要手工修改。\n")
	b.WriteString("\n[Unit]\n")
	fmt.Fprintf(&b, "Description=uPanel 计划任务定时器：%s\n", name)
	b.WriteString("Documentation=file:/usr/local/share/doc/upanel/SCHEDULES.md\n")
	b.WriteString("\n[Timer]\n")
	fmt.Fprintf(&b, "OnCalendar=%s\n", CalendarLine(d))
	fmt.Fprintf(&b, "Persistent=%s\n", persistent)
	// AccuracySec=1s：默认 1 分钟的系统级合并会把“03:00”推到 03:00:59，
	// 而面板上显示的下次执行时间来自 systemctl show，两边差一分钟会被当成 bug。
	b.WriteString("AccuracySec=1s\n")
	b.WriteString("RandomizedDelaySec=0\n")
	fmt.Fprintf(&b, "Unit=%s\n", ServiceName(scheduleID))
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=timers.target\n")
	return []byte(b.String()), nil
}

// ErrNoUnits 表示这条定义此刻不该有单元（未启用）。
//
// “定义存在但没启用”是正常状态，不是错误；把它做成错误会让停用一条计划
// 变成一次失败的处理。
var ErrNoUnits = errors.New("schedules: 定义未启用，不渲染单元")
