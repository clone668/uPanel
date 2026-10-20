// upanelctl 是 uPanel 的本机初始化与恢复 CLI（PLAN.md 第 22 节）。
//
// 已实现的子命令：
//
//	upanelctl version          打印 Web/代理共用的版本信息
//	upanelctl check-config     校验配置文件并打印生效值
//	upanelctl migrate          执行显式 SQL 迁移（幂等，只前滚）
//	upanelctl init-admin       创建第一个管理员（仅本机、无默认密码）
//
// 恢复流程等子命令在对应阶段加入；未实现的子命令不会以“成功”返回，而是明确报告未实现。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/store"
	"upanel/internal/version"
	"upanel/migrations"
)

const program = "upanelctl"

// exitUsage 表示命令行用法错误。
const exitUsage = 2

// exitNotImplemented 表示子命令尚未实现（对应 CONTRACTS.md 的 501 语义）。
const exitNotImplemented = 3

// 已规划但尚未实现的子命令：显式列出以便给出明确提示，而不是“未知命令”。
var plannedCommands = map[string]string{
	"reset-password": "P02.2（本机管理员恢复）",
	"scheduler":      "P12（定时任务触发，仅 root 使用专用入口）",
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "version":
		return cmdVersion(args[1:], stdout, stderr)
	case "check-config":
		return cmdCheckConfig(args[1:], stdout, stderr)
	case "migrate":
		return cmdMigrate(args[1:], stdout, stderr)
	case "init-admin":
		return cmdInitAdmin(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		if stage, ok := plannedCommands[args[0]]; ok {
			fmt.Fprintf(stderr, "%s: 子命令 %q 尚未实现（计划阶段 %s）\n", program, args[0], stage)
			return exitNotImplemented
		}
		fmt.Fprintf(stderr, "%s: 未知子命令 %q\n\n", program, args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `%s — uPanel 本机初始化与恢复 CLI

用法: %s <子命令> [参数]

子命令:
  version                 打印版本信息（--json 输出 JSON）
  check-config            校验配置文件并打印生效值（--config PATH、--json）
  migrate                 执行数据库迁移（--config PATH、--db PATH、--json）
  init-admin              创建第一个管理员（--config PATH、--db PATH、--username、--display-name、--json）
  help                    显示本帮助

已规划但未实现: reset-password、scheduler
（未实现的子命令返回退出码 %d，不会伪装成功）
`, program, program, exitNotImplemented)
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}

	info := version.Get()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			fmt.Fprintf(stderr, "输出失败: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s %s\n", program, info.String())
	return 0
}

func cmdCheckConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}

	res, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 1
	}
	if err := res.Config.Validate(); err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 1
	}

	mode, err := res.Config.Web.ModeValue()
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: web.mode: %v\n", err)
		return 1
	}
	listen, err := config.ResolveListen(mode, res.Config.Web.ListenAddress, res.ListenExplicit)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: web.listen_address: %v\n", err)
		return 1
	}

	// 允许源跨字段规则（非回环监听必须显式配置）也在这里判定，让 check-config
	// 与真正启动的结论一致。
	allowedOrigins, err := config.ResolveAllowedOrigins(res.Config.Web.AllowedOrigins, listen)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 1
	}

	platformErr := platform.CheckMode(mode)
	report := checkReport{
		ConfigSource:          res.Source,
		Mode:                  mode.String(),
		WebListen:             listen,
		WebLoopbackOnly:       config.IsLoopbackAddress(listen),
		AllowedOrigins:        allowedOrigins,
		AgentSocket:           res.Config.Agent.SocketPath,
		LogLevel:              res.Config.Log.Level,
		LogFormat:             res.Config.Log.Format,
		DatabasePath:          res.Config.Database.Path,
		DatabaseBusyTimeoutMS: res.Config.Database.BusyTimeoutMilliseconds,
		Platform:              platform.Current(),
	}
	if platformErr != nil {
		report.PlatformError = platformErr.Error()
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "输出失败: %v\n", err)
			return 1
		}
	} else {
		source := report.ConfigSource
		if source == "" {
			source = "内置默认值（未找到配置文件）"
		}
		fmt.Fprintf(stdout, "配置来源: %s\n", source)
		fmt.Fprintf(stdout, "运行模式: %s\n", report.Mode)
		fmt.Fprintf(stdout, "Web 监听: %s（仅回环: %t）\n", report.WebListen, report.WebLoopbackOnly)
		fmt.Fprintf(stdout, "代理 socket: %s\n", report.AgentSocket)
		fmt.Fprintf(stdout, "数据库: %s（busy_timeout=%dms）\n",
			orUnset(report.DatabasePath, "未配置（需 --db 或 database.path）"), report.DatabaseBusyTimeoutMS)
		fmt.Fprintf(stdout, "日志: level=%s format=%s\n", report.LogLevel, report.LogFormat)
		fmt.Fprintf(stdout, "平台: %s/%s（production 可用: %t）\n",
			report.Platform.OS, report.Platform.Arch, platformErr == nil)
		if platformErr != nil {
			fmt.Fprintf(stdout, "注意: %v\n", platformErr)
		}
	}

	// check-config 只判定配置文件是否合法；平台准入由各服务在启动时强制。
	return 0
}

// checkReport 是 check-config 的输出结构。
type checkReport struct {
	ConfigSource          string        `json:"config_source"`
	Mode                  string        `json:"mode"`
	WebListen             string        `json:"web_listen"`
	WebLoopbackOnly       bool          `json:"web_loopback_only"`
	AllowedOrigins        []string      `json:"allowed_origins"`
	AgentSocket           string        `json:"agent_socket"`
	LogLevel              string        `json:"log_level"`
	LogFormat             string        `json:"log_format"`
	DatabasePath          string        `json:"database_path"`
	DatabaseBusyTimeoutMS int           `json:"database_busy_timeout_ms"`
	Platform              platform.Info `json:"platform"`
	PlatformError         string        `json:"platform_error,omitempty"`
}

func orUnset(v, placeholder string) string {
	if strings.TrimSpace(v) == "" {
		return placeholder
	}
	return v
}

// migrateReport 是 migrate 的 --json 输出结构。
type migrateReport struct {
	DatabasePath    string `json:"database_path"`
	FromVersion     int    `json:"from_version"`
	SchemaVersion   int    `json:"schema_version"`
	AppliedVersions []int  `json:"applied_versions"`
}

// cmdMigrate 执行显式 SQL 迁移。
//
// 语义：幂等（重复执行不再应用任何迁移）、只前滚（数据库版本高于程序内置版本时
// 拒绝执行）、迁移文件被改动或删除时拒绝执行（见 internal/store/migrate.go）。
// 失败一律以非 0 退出，不写“成功”字样。
func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
	dbFlag := fs.String("db", "", "数据库文件绝对路径（覆盖 database.path）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}

	res, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 1
	}
	if err := res.Config.Validate(); err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 1
	}

	dbPath := strings.TrimSpace(*dbFlag)
	if dbPath == "" {
		dbPath = res.Config.Database.Path
	}
	if strings.TrimSpace(dbPath) == "" {
		fmt.Fprintf(stderr, "错误: 未指定数据库路径（使用 --db PATH 或配置 database.path）\n")
		return exitUsage
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{
		Path:        dbPath,
		BusyTimeout: res.Config.Database.BusyTimeout(),
		Logger:      logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "迁移失败: 无法打开数据库: %v\n", err)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(stderr, "警告: 关闭数据库失败: %v\n", err)
		}
	}()

	result, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		fmt.Fprintf(stderr, "迁移失败: %v\n", err)
		return 1
	}

	report := migrateReport{
		DatabasePath:    dbPath,
		FromVersion:     result.From,
		SchemaVersion:   result.To,
		AppliedVersions: result.AppliedVersions(),
	}
	if report.AppliedVersions == nil {
		report.AppliedVersions = []int{}
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

	fmt.Fprintf(stdout, "数据库: %s\n", report.DatabasePath)
	fmt.Fprintf(stdout, "schema 版本: %d（执行前 %d）\n", report.SchemaVersion, report.FromVersion)
	if len(report.AppliedVersions) == 0 {
		fmt.Fprintf(stdout, "本次应用: 无（schema 已是最新）\n")
	} else {
		parts := make([]string, 0, len(report.AppliedVersions))
		for _, v := range report.AppliedVersions {
			parts = append(parts, fmt.Sprintf("%04d", v))
		}
		fmt.Fprintf(stdout, "本次应用: %s\n", strings.Join(parts, ", "))
	}
	return 0
}
