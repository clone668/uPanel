package main

// reset-password 是本机管理员恢复口令的唯一入口（PLAN §20「忘记管理员密码」）。
//
// 为什么必须存在这条命令：0.1 的面板里，重置密码的 Web 接口
// （POST /users/:id/password）要求**另一个**管理员 + 再认证。唯一的
// 管理员忘了密码时，Web 上没有任何合法入口，而 init-admin 又拒绝在已有
// 账号的库上运行——没有这条命令，“忘记密码”的恢复说明就只能答“重装”。
//
// 边界（与 Web 接口同样严格，不因为“在本机”就放宽）：
//   - 只能改**已存在**账号的口令，不能建号、不能改角色、不能改启用状态；
//   - 口令只从标准输入读（两行一致），不进 argv、不进日志、不进审计；
//   - 复用 auth.Service.SetPassword：口令策略、哈希参数、会话作废都在产品代码里，
//     CLI 不自己拼 SQL；
//   - 执行后写一条 user.password.reset 审计（actor 为本地 CLI，不是某个账号）；
//   - 运行身份必须是能写数据库的那个人（安装后是 upanel-web）。它不新增任何
//     特权通道：谁本来就能写 upanel.db，谁本来就能改这个库里的口令哈希。

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/store"
	"upanel/migrations"
)

// resetPasswordReport 是 reset-password 的输出（不含任何秘密）。
type resetPasswordReport struct {
	DatabasePath  string `json:"database_path"`
	UserID        int64  `json:"user_id"`
	Username      string `json:"username"`
	Role          string `json:"role"`
	Disabled      bool   `json:"disabled"`
	SchemaVersion int    `json:"schema_version"`
}

func cmdResetPassword(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
	dbFlag := fs.String("db", "", "数据库文件绝对路径（覆盖 database.path）")
	username := fs.String("username", "", "要重置口令的账号（必填）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}
	name := auth.NormalizeUsername(*username)
	if err := auth.ValidateUsername(name); err != nil {
		fmt.Fprintf(stderr, "错误: --username 不合法: %v\n", err)
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

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{
		Path:        dbPath,
		BusyTimeout: res.Config.Database.BusyTimeout(),
		Logger:      logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "错误: 无法打开数据库 %s: %v\n", dbPath, err)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(stderr, "警告: 关闭数据库失败: %v\n", err)
		}
	}()

	migrated, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 数据库迁移失败: %v\n", err)
		return 1
	}

	svc, err := auth.NewService(db, auth.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "错误: 初始化认证服务失败: %v\n", err)
		return 1
	}
	target, err := svc.FindUserByUsername(ctx, name)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			fmt.Fprintf(stderr, "错误: 数据库里没有账号 %q（本命令不能建号；首次建号用 init-admin）\n", name)
			return 1
		}
		fmt.Fprintf(stderr, "错误: 查询账号失败: %v\n", err)
		return 1
	}
	if target.Disabled {
		fmt.Fprintf(stderr, "错误: 账号 %q 已被禁用，重置口令也不会生效。\n", target.Username)
		fmt.Fprintln(stderr, "      请先用另一个管理员在面板上启用它；若已无可用管理员，只能恢复数据库备份。")
		return 1
	}

	password, err := readNewPassword(stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "错误: %v\n", err)
		return 1
	}
	if err := auth.ValidatePassword(target.Username, password); err != nil {
		fmt.Fprintf(stderr, "错误: 密码不符合要求: %v\n", err)
		return 1
	}

	user, err := svc.SetPassword(ctx, target.ID, password)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 重置口令失败: %v\n", err)
		return 1
	}

	// 审计与业务分开也没关系：口令已经改了，这条记录只有“记上/记不上”两种结果，
	// 记不上就要说清楚（否则库被改却没有痕迹）。
	writer, err := audit.New(db, nil)
	if err != nil {
		fmt.Fprintf(stderr, "警告: 审计写入不可用: %v\n", err)
	} else if err := writer.Record(ctx, audit.Event{
		ActorSnapshot: "local-cli:" + localActor(),
		Action:        audit.ActionUserPasswordReset,
		Target:        fmt.Sprintf("user:%d", user.ID),
		Outcome:       audit.OutcomeSuccess,
		Details: map[string]any{
			"username": user.Username,
			"source":   "upanelctl reset-password",
		},
	}); err != nil {
		fmt.Fprintf(stderr, "警告: 审计写入失败: %v\n", err)
	}

	report := resetPasswordReport{
		DatabasePath:  dbPath,
		UserID:        user.ID,
		Username:      user.Username,
		Role:          string(user.Role),
		Disabled:      user.Disabled,
		SchemaVersion: migrated.To,
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
	fmt.Fprintf(stdout, "已重置账号 %s（%s）的口令\n", report.Username, report.Role)
	fmt.Fprintf(stdout, "该账号的所有既有会话已作废，需要用新口令重新登录。\n")
	fmt.Fprintf(stdout, "数据库: %s\n", report.DatabasePath)
	return 0
}

// localActor 返回“谁在本机执行了这条命令”，只用于审计快照。
func localActor() string {
	name := os.Getenv("SUDO_USER")
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" {
		if u, err := os.UserHomeDir(); err == nil && u != "" {
			name = u
		}
	}
	if name == "" {
		name = fmt.Sprintf("uid-%d", os.Getuid())
	}
	return name
}
