package main

// `upanel-agent netrollback --config <cfg> --token <token>`：回滚 timer 到期时执行的
// 那一条命令。
//
// 它存在的理由，就是它不是面板的一部分：
//
//   - 不出现在 HTTP 路由里，也不走 Unix socket。systemd 直接在到期那一刻执行它；
//   - 它**不读代理配置、不开数据库、不碰 socket**。要恢复什么、恢复成什么样，全在
//     `/var/lib/upanel-agent/network/rollback/<token>.json` 里写着。救生索上挂的东西
//     越少，它越可能在出事的时候还能用；
//   - 只有两个参数，且都被校验：`--config` 只是被记进日志与记录（下次武装 timer 用），
//     `--token` 必须符合 `nrb_` + 32 位十六进制的固定形状——它会被拼进文件名，
//     形状不固定就等于允许路径穿越。
//
// 退出码：0 = 回滚走到了终态（已回滚，或记录已被确认/已回滚过，无需动作）；
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

// netrollbackTimeout 比单元里的 TimeoutStartSec=180 略短：被 systemd 掐死会留下
// "单元超时"这种没有结论的记录，而我们更希望看到自己写下的原因。
const netrollbackTimeout = 150 * time.Second

func runNetrollback(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("netrollback", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "代理配置文件路径（记入回滚记录，不在此处读取）")
	token := fs.String("token", "", "回滚记录标识（nrb_ 开头的固定形状）")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "upanel-agent netrollback — 到期未确认时把网络配置退回改之前的样子\n\n"+
			"用法: upanel-agent netrollback --token <token> [--config <cfg>]\n\n参数:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数\n")
		return 2
	}
	if !network.ValidRollbackToken(*token) {
		fmt.Fprintf(stderr, "错误: --token 必须是 %s 加 32 位小写十六进制，收到 %q\n",
			"nrb_", *token)
		return 2
	}

	engine, err := agent.NewNetworkApplyEngine(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 组装网络写路径失败: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), netrollbackTimeout)
	defer cancel()

	if err := engine.ExecutePendingRollback(ctx, *token); err != nil {
		fmt.Fprintf(stderr, "回滚 %s 未完成: %v\n", *token, err)
		return 1
	}
	fmt.Fprintf(stdout, "回滚 %s 已处理\n", *token)
	return 0
}
