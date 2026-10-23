package main

// P11.3 的装配：把 internal/power 接到真实的 systemctl 与 /proc 上。
//
// 单独一个文件，是因为这里有两件只在生产进程里成立的事：
//
//  1. **命令执行必须走 syscmd**（绝对路径、逐项参数、进程组、超时、输出上限）。
//     电源动作没有 shell：`systemctl reboot` 就两个 argv，没有“拼一条命令”的必要。
//  2. **在跑的任务来源晚一步才存在**（见 agent.PowerDependents）——这里把持有者塞进
//     执行器，把执行器塞进生产依赖；Server 起来之后再 Bind。

import (
	"context"
	"os"
	"time"

	"upanel/internal/agent"
	"upanel/internal/power"
	"upanel/internal/syscmd"
)

// powerIssueMaxOutput 是 systemctl 输出的上限。
//
// 成功时它什么也不打印；失败时是一句 "Failed to ..."，几百字节。
// 给 8 KiB 是为了“够看”，同时不让一条被污染的 stderr 进任务记录。
const powerIssueMaxOutput = 8 << 10

// newPowerExecutor 构造电源执行器。
func newPowerExecutor(inFlight power.InFlightSource) *power.Executor {
	paths := power.DefaultPaths()
	return power.New(power.Options{
		Paths: paths,
		Run: func(ctx context.Context, program string, args []string, timeout time.Duration) (power.CommandResult, error) {
			res, err := syscmd.Run(ctx,
				syscmd.Spec{Program: program, Args: args},
				syscmd.Limits{Timeout: timeout, MaxStdout: powerIssueMaxOutput, MaxStderr: powerIssueMaxOutput})
			return power.CommandResult{
				Program:  res.Program,
				ExitCode: res.ExitCode,
				Signal:   res.Signal,
				Stdout:   res.Stdout,
				Stderr:   res.Stderr,
				TimedOut: res.TimedOut,
			}, err
		},
		ReadFile: os.ReadFile,
		Stat: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		InFlight: inFlight,
	})
}

// powerAvailable 决定要不要把 system.power 登记进能力表。
//
// 条件只有一个：能找到 systemctl。`/run/systemd/system` 与 boot_id 的检查放到预检里——
// 它们是**运行时**事实，开机时成立、运行中也可能变（容器里挂载进来又卸载）。
// 装配时判断一次就永远相信，正是本轮要避免的那种“看起来检查过了”。
func powerAvailable(e *power.Executor) bool {
	return e != nil && fileExists(e.Paths().Systemctl)
}

// fileExists 是 os.Stat 的一层普通封装（这里只关心“在不在”）。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// bindPower 在 Server 建好之后把“谁在跑”接上。
func bindPower(deps *agent.PowerDependents, srv *agent.Server) {
	if deps != nil && srv != nil {
		deps.Bind(srv)
	}
}
