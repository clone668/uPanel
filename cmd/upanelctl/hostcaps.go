package main

// upanelctl host-caps — 这台机器上到底装了什么（P16.1）。
//
// 为什么放进 CLI 而不只是接口：安装器与排障都在没有面板的时候问这一句话。
// 安装脚本在装二进制之前只有发布包里的 upanelctl 可用，装完之后也只有它是
// 不需要 socket、不需要数据库、不需要会话就能跑的那一个。探测器因此只有一份实现，
// 三种问法读同一份事实：`host-caps`（本命令）、代理的 /internal/v1/capabilities、
// 面板的 GET /api/v1/system/capabilities。
//
// 退出码只表示“这次探测本身有没有跑完”，**不表示缺不缺东西**：
// 一台没装 Samba 的机器是正常的，把它算成失败会让安装器拒绝安装。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"upanel/internal/hostcaps"
)

// defaultHostCapsTimeout 是整体时限。十几条版本命令并发跑，正常在 1 秒内结束；
// 30 秒是留给“某台机器上这些命令特别慢”的余量，不是给它们慢慢磨的。
const defaultHostCapsTimeout = 30 * time.Second

func cmdHostCaps(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("host-caps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "以 JSON 输出（字段与接口一致）")
	asLine := fs.Bool("line", false, "以制表符分隔的每行一条输出（给安装脚本用）")
	timeout := fs.Duration("timeout", defaultHostCapsTimeout, "整体探测时限")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}
	if *asJSON && *asLine {
		fmt.Fprintln(stderr, "错误: --json 与 --line 只能选一个")
		return exitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintf(stderr, "错误: --timeout 必须为正数，当前 %s\n", timeout.String())
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report := hostcaps.Probe(ctx, hostcaps.Deps{})

	if err := ctx.Err(); err != nil {
		// 时限到了：报出来的是一份跑了一半的探测，绝不能当成完整结论。
		fmt.Fprintf(stderr, "探测没有跑完（%s）：%v\n", timeout.String(), err)
		switch {
		case *asJSON:
			// 仍然把已跑完的部分打出来，但退出码非零——读的人不会被半份数据骗到。
			_ = json.NewEncoder(stdout).Encode(report)
		case *asLine:
			printHostCapsLines(stdout, report)
		}
		return 1
	}

	if *asLine {
		printHostCapsLines(stdout, report)
		return 0
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "输出失败: %v\n", err)
			return 1
		}
		return 0
	}

	printHostCaps(stdout, report)
	return 0
}

// printHostCapsLines 打一份给脚本读的最小格式：每行一条，制表符分隔，没有表头。
//
// 列的顺序是契约（改了就是破坏兼容）：
//
//	1 id      稳定标识，如 restic、docker.compose
//	2 state   present / absent / unknown（三态，不许并）
//	3 version 版本号；没有就是空字段——**空不等于 0，也不等于未知**
//	4 path    找到时的绝对路径；没有就是空字段
//	5 feature 依赖它的能力，中文短语
//	6 hint    补齐它的建议（一般是 apt 包名）；没有就是空字段
//
// 为什么还要一个非 JSON 的输出：安装脚本是 POSIX sh，没有 jq 可用，
// 用 sed 去切 JSON 一定会切错（字段里就有中文括号和斜杠）。
// 制表符列在这里是唯一一个不会和内容打架的分隔符。
func printHostCapsLines(w io.Writer, report hostcaps.Report) {
	for _, item := range report.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			item.ID, string(item.State), item.Version, item.Path, item.Feature, item.Hint)
	}
}

// printHostCaps 打一张人读的表。
//
// 三列宽度写死：状态、标识、版本/路径。缺的东西**不省略**，而是带着缺席理由打出来——
// 这张表的用途就是回答“为什么那个功能不出现”，把缺席的行藏掉等于把答案藏掉。
func printHostCaps(w io.Writer, report hostcaps.Report) {
	fmt.Fprintf(w, "探测时间 %s\n\n", report.GeneratedAt)
	fmt.Fprintf(w, "%-8s %-18s %-14s %s\n", "状态", "标识", "版本", "路径 / 说明")
	for _, item := range report.Items {
		version := item.Version
		if version == "" {
			version = "—"
		}
		detail := item.Path
		switch {
		case item.State == hostcaps.StatePresent && item.Reason != "":
			detail = item.Path + "（" + item.Reason + "）"
		case item.State != hostcaps.StatePresent:
			detail = item.Reason
			if item.Hint != "" {
				detail += "；" + item.Hint
			}
		}
		fmt.Fprintf(w, "%-8s %-18s %-14s %s\n", string(item.State), item.ID, version, detail)
	}

	if report.Partial {
		fmt.Fprintf(w, "\n注意：这一轮有目录读不出来，标着 absent 的条目不能当成“没有”。\n")
	}
	if len(report.Issues) > 0 {
		fmt.Fprintf(w, "\n这一轮遇到的问题：\n")
		for _, issue := range report.Issues {
			fmt.Fprintf(w, "  - %s\n", issue)
		}
	}
	if len(report.Issues) == 0 && !report.Partial {
		fmt.Fprintf(w, "\n（缺席不是错误：不装 Samba 的机器上 samba 就是 absent。）\n")
	}
}
