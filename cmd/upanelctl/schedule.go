package main

// upanelctl schedule-trigger（P12.4）。
//
// 这是 systemd 定时单元唯一会调的东西：
//
//	ExecStart=/usr/local/bin/upanelctl schedule-trigger --schedule-id sch_<32hex> --wait
//
// 它自己不做任何决定：算出"这一次"的触发 ID，交给代理，代理再独立算一遍并核对。
// 触发 ID 与 operation_id 都是同一个 UTC 分钟槽的哈希，所以 systemd 重试、
// 手工 systemctl start、以及定时器在同一分钟内二次唤醒，都落在同一个 operation 上。
//
// --wait 是"不重叠"的实现位置。定时单元是 Type=oneshot：systemd 只有在单元的主进程
// 还在跑时才认为它 active。代理对触发请求的答复是立刻的（真活跑在后台），所以没有
// --wait 时单元几十毫秒就结束，oneshot 拦不住"上一次还没跑完、下一次又起来"——它只
// 拦得住同一次唤醒的重复执行。带上 --wait 之后，单元一直 active 到这次操作到达终态，
// 单元 active 期间 systemd 不会起第二个实例：到期时撞上运行中的那次唤醒会被跳过，或者
// 被排到这次运行结束之后再起一次——两种都不并行。"不重叠"说的是不并行，不是不补跑。
// 这不是注释里的声称：它由这里的轮询和单元的 TimeoutStartSec 共同保证。
//
// 退出码：0 表示代理已接受并且（--wait 时）操作成功；1 表示失败或被中止。
// 非零会让 systemd 记一次 failed unit，这是我们要的——静默失败比报警更糟。
//
// --wait 的两次读取都走**只给 root 的那条路**：状态来自
// GET /internal/v1/schedules/<id>/trigger-runs/<op>（代理会核对那条操作确实是这条
// 计划触发的），等待上限来自触发答复里的 max_runtime_seconds。别改成
// /internal/v1/operations/<id> 或读计划详情：那两条是 Web 账号的路由，root 调它们
// 是 403。P12.4 在 24.04 上就是这么撞出来的——单元一直卡在 403 重试里，看起来
// "一直 active"（于是不重叠成立），实际是根本没在等这次操作。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/config"
	"upanel/internal/schedules"
)

// scheduleTriggerTimeout 是等代理答复的上限。
//
// 代理对触发请求只做"预检 + 登记 operation + 起后台执行"，答复是立刻的；
// 真正的备份/脚本跑在后台，不会挂在这条连接上。给 60 秒是留给代理库慢的时候。
const scheduleTriggerTimeout = 60 * time.Second

func cmdScheduleTrigger(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schedule-trigger", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scheduleID := fs.String("schedule-id", "", "计划 ID（sch_ 开头；由定时单元的 ExecStart 传入）")
	socketPath := fs.String("socket", "", "代理 socket 路径；缺省取配置里的 agent.socket_path")
	configPath := fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	wait := fs.Bool("wait", false, "等这次触发产生的操作跑到终态再退出（定时单元靠它实现不重叠）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}

	id := strings.TrimSpace(*scheduleID)
	if id == "" {
		fmt.Fprintf(stderr, "错误: 必须指定 --schedule-id\n")
		return exitUsage
	}
	// 先在本机拦下形状不对的 id：这样 systemd 日志里留下的是"参数不对"，
	// 而不是一条绕到代理才被拒的请求。
	if !schedules.ValidID(id) {
		fmt.Fprintf(stderr, "错误: 计划 ID 形状不合法: %q\n", id)
		return exitUsage
	}

	socket := strings.TrimSpace(*socketPath)
	if socket == "" {
		socket = config.AgentSocketPath
		// 配置读不出来不阻止触发：定时器该跑还是得跑，代理多半就在默认 socket 上。
		// 但要把这件事说清楚，否则"为什么打到别处去了"会变成一次猜谜。
		res, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "警告: 读取配置失败，改用默认 socket %s: %v\n", socket, err)
		} else if res.Config.Agent.SocketPath != "" {
			socket = res.Config.Agent.SocketPath
		}
	}

	// 触发 ID 与代理算的是同一个式子；这里算只是为了让请求带上它。
	// 时间取本机时钟——定时器所在的机器，就是计划定义里的那台机器。
	slot := schedules.SlotAt(time.Now())
	triggerID := schedules.TriggerID(id, slot)

	client, err := agentclient.New(socket)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 构造代理客户端失败: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), scheduleTriggerTimeout)
	defer cancel()

	result, err := client.TriggerSchedule(ctx, agentclient.TriggerInput{
		ScheduleID: id,
		TriggerID:  triggerID,
	})
	if err != nil {
		fmt.Fprintf(stderr, "错误: 触发失败: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(stderr, "输出失败: %v\n", err)
			return 1
		}
		if *wait {
			return waitForTerminal(client, id, result.OperationID, result.MaxRuntimeSeconds, stdout, stderr)
		}
		return 0
	}

	if result.Duplicate {
		// 不是错误：同一分钟内 systemd 又进来一次，代理把上一次的 operation 还回来了。
		fmt.Fprintf(stdout, "计划 %s 的时间槽 %s 已触发过，沿用操作 %s\n",
			result.ScheduleID, result.SlotAt, result.OperationID)
	} else {
		fmt.Fprintf(stdout, "计划 %s 已在时间槽 %s 触发 %s，操作 %s\n",
			result.ScheduleID, result.SlotAt, result.Action, result.OperationID)
	}
	if *wait {
		return waitForTerminal(client, id, result.OperationID, result.MaxRuntimeSeconds, stdout, stderr)
	}
	return 0
}

// --wait 的轮询参数。
const (
	// scheduleWaitPoll 是两次状态查询之间的间隔。定时任务是分钟级的，
	// 2 秒的粒度对"单元什么时候结束"足够，也不会让代理忙起来。
	scheduleWaitPoll = 2 * time.Second
	// scheduleWaitSlack 是在计划的最长运行时长之外多等的余量：代理杀进程、
	// 写结果、把阶段改成 failed 都要时间，少给这一点余量会把"刚超时"误报成
	// "等不到结果"。
	//
	// 它必须**小于**单元的余量（schedules.TriggerWaitSlackSeconds）：单元的超时是
	// "代理杀进程"那个数再加 60 秒，这里是加 30 秒。两处一样大就会变成"systemd 先
	// 发 TERM"，journal 里只剩一句 start operation timed out——查的人看不出是操作
	// 没结束还是等的人没在等。留下这 30 秒的差，超时那一刻先由这里说话：
	// "等到 … 仍未见操作 … 到达终态；结果未知"。cmd/upanelctl 有测试钉住这个不等式。
	scheduleWaitSlack = 30 * time.Second
	// scheduleWaitFallback 是答复里没有最长运行时长时的兜底上限。拿不到不等于
	// 不用等（单元还开着，不等就等于没有 --wait），但也不能无限挂着。
	scheduleWaitFallback = 10 * time.Minute
	// scheduleWaitQueryTimeout 是单次状态查询的上限。
	scheduleWaitQueryTimeout = 30 * time.Second
)

// waitForTerminal 轮询到这次操作到达终态，返回给 systemd 的退出码。
//
// 成功（succeeded）返回 0；failed / canceled / interrupted、等不到结果、
// 连不上代理都返回 1——单元变红，运维在 journal 里看得见。
func waitForTerminal(client *agentclient.Client, scheduleID, operationID string, maxRuntimeSeconds int, stdout, stderr io.Writer) int {
	deadline := waitDeadline(maxRuntimeSeconds)
	fmt.Fprintf(stdout, "等待操作 %s 跑到终态（最多 %s）\n", operationID, time.Until(deadline).Round(time.Second))

	for {
		ctx, cancel := context.WithTimeout(context.Background(), scheduleWaitQueryTimeout)
		op, err := client.GetTriggerRun(ctx, scheduleID, operationID)
		cancel()
		if err != nil {
			// 连不上不等于操作失败：代理重启、socket 短暂不可用都可能发生。
			// 但等到期仍读不到，就得如实报"不知道结果"，而不是猜成功。
			fmt.Fprintf(stderr, "警告: 读取操作状态失败，稍后重试: %v\n", err)
		} else {
			switch op.Stage {
			case "succeeded":
				fmt.Fprintf(stdout, "操作 %s 成功\n", operationID)
				return 0
			case "failed", "canceled", "interrupted":
				fmt.Fprintf(stdout, "操作 %s 结束于 %s：%s\n", operationID, op.Stage, operationErrorText(op.Error))
				return 1
			}
		}
		if time.Now().Add(scheduleWaitPoll).After(deadline) {
			fmt.Fprintf(stderr, "错误: 等到 %s 仍未见操作 %s 到达终态；结果未知，请用 upanelctl 或面板查 %s\n",
				deadline.UTC().Format(time.RFC3339), operationID, operationID)
			return 1
		}
		time.Sleep(scheduleWaitPoll)
	}
}

// waitDeadline 返回这次等待的上限：计划的最长运行时长（代理随答复给出的那个数）+ 余量。
//
// 这个数与单元 TimeoutStartSec 同源，所以"单元还在跑、这边先放弃"的顺序是确定的。
// 值由触发答复带来，不再回头读计划：读计划详情是 Web 账号的路由，root 跑的这一条
// 调它只会拿到 403——那正是 P12.4 在 24.04 上抓到的第一个真缺陷。
func waitDeadline(maxRuntimeSeconds int) time.Time {
	if maxRuntimeSeconds <= 0 {
		return time.Now().Add(scheduleWaitFallback)
	}
	return time.Now().Add(time.Duration(maxRuntimeSeconds)*time.Second + scheduleWaitSlack)
}

// operationErrorText 把代理给的错误体折成一行。错误体是结构化 JSON，
// 直接打印原文会让 journal 里出现一串转义；取 message 字段，取不到就退回原文。
func operationErrorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "没有给出原因"
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Message != "" {
		if body.Code != "" {
			return body.Code + ": " + body.Message
		}
		return body.Message
	}
	return strings.TrimSpace(string(raw))
}
