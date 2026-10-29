package main

// `upanel-agent fwrollback --config <cfg> --token <token>`：防火墙回滚 timer 到期时
// 执行的那一条命令。
//
// 与 netrollback 同一条理由，逐条对应：
//
//   - 不出现在任何路由里，也不走 Unix socket，systemd 在到期那一刻直接执行它；
//   - **不读代理配置、不开数据库、不碰 socket**。要恢复成什么样，全在
//     `/var/lib/upanel-agent/firewall/rollback/<token>.json` 里；
//   - 两个参数都校验：`--token` 必须是 `frb_` + 32 位小写十六进制——它会被拼进文件名，
//     形状不固定就等于允许路径穿越。
//
// 退出码：0 = 走到了终态（已回滚，或记录已被确认/已回滚过，无需动作）；
// 1 = 回滚尝试失败（记录已标 failed 并写明原因，按设计不重试）；2 = 参数不对。

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"upanel/internal/agent"
	"upanel/internal/network"
)

// fwrollbackTimeout 比单元里的 TimeoutStartSec=180 略短：被 systemd 掐死会留下
// "单元超时"这种没有结论的记录，而我们更希望看到自己写下的原因。
const fwrollbackTimeout = 150 * time.Second

func runFwrollback(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fwrollback", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "代理配置文件路径（记入回滚记录，不在此处读取）")
	token := fs.String("token", "", "回滚记录标识（frb_ 开头的固定形状）")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "upanel-agent fwrollback — 到期未确认时把防火墙规则退回改之前的样子\n\n"+
			"用法: upanel-agent fwrollback --token <token> [--config <cfg>]\n\n参数:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数\n")
		return 2
	}
	if !network.ValidFirewallToken(*token) {
		fmt.Fprintf(stderr, "错误: --token 必须是 %s 加 32 位小写十六进制，收到 %q\n",
			"frb_", *token)
		return 2
	}

	engine, err := agent.NewFirewallApplyEngine(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 组装防火墙写路径失败: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), fwrollbackTimeout)
	defer cancel()

	if err := engine.ExecutePendingRollback(ctx, *token); err != nil {
		fmt.Fprintf(stderr, "防火墙回滚 %s 未完成: %v\n", *token, err)
		return 1
	}
	fmt.Fprintf(stdout, "防火墙回滚 %s 已处理\n", *token)
	return 0
}
