package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/term"

	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/store"
	"upanel/migrations"
)

// stdin 是 init-admin 的输入来源。测试替换它；正常运行读标准输入。
var stdin io.Reader = os.Stdin

// initAdminReport 是 init-admin 的输出（不含任何秘密）。
type initAdminReport struct {
	DatabasePath  string `json:"database_path"`
	UserID        int64  `json:"user_id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	Role          string `json:"role"`
	SchemaVersion int    `json:"schema_version"`
}

// cmdInitAdmin 在本机创建第一个管理员（PLAN.md 第 5 节：首个管理员只能由本机 CLI 创建）。
//
// 安全约束：
//   - 系统没有任何默认密码：密码只来自本次交互，不写日志、不进命令行参数；
//   - 已有任何账号时直接拒绝，避免通过重复执行重置管理员；
//   - 密码输入在终端上不回显；从管道读取时要求两行一致，避免脚本里写错；
//   - 输出只包含用户名与角色，不包含密码或其哈希。
func cmdInitAdmin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
	dbFlag := fs.String("db", "", "数据库文件绝对路径（覆盖 database.path）")
	username := fs.String("username", "admin", "管理员用户名")
	displayName := fs.String("display-name", "", "显示名（默认与用户名相同）")
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

	// 先检查再问密码：已经初始化过就不该让人输一遍密码再被拒。
	count, err := svc.CountUsers(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 读取账号数量失败: %v\n", err)
		return 1
	}
	if count > 0 {
		fmt.Fprintf(stderr, "错误: 数据库中已有 %d 个账号；init-admin 只用于首次初始化。\n", count)
		fmt.Fprintf(stderr, "如需重置密码，请使用后续阶段提供的 reset-password（当前未实现）。\n")
		return 1
	}

	name := auth.NormalizeUsername(*username)
	if err := auth.ValidateUsername(name); err != nil {
		fmt.Fprintf(stderr, "错误: 用户名不合法: %v\n", err)
		return exitUsage
	}
	display := strings.TrimSpace(*displayName)
	if display == "" {
		display = name
	}
	if err := auth.ValidateDisplayName(display); err != nil {
		fmt.Fprintf(stderr, "错误: 显示名不合法: %v\n", err)
		return exitUsage
	}

	password, err := readNewPassword(stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "错误: %v\n", err)
		return 1
	}
	if err := auth.ValidatePassword(name, password); err != nil {
		fmt.Fprintf(stderr, "错误: 密码不符合要求: %v\n", err)
		return 1
	}

	user, err := svc.CreateFirstAdmin(ctx, name, display, password)
	if err != nil {
		if errors.Is(err, auth.ErrUsersExist) {
			fmt.Fprintf(stderr, "错误: 数据库中已有账号；init-admin 只用于首次初始化。\n")
			return 1
		}
		fmt.Fprintf(stderr, "错误: 创建管理员失败: %v\n", err)
		return 1
	}

	report := initAdminReport{
		DatabasePath:  dbPath,
		UserID:        user.ID,
		Username:      user.Username,
		DisplayName:   user.DisplayName,
		Role:          string(user.Role),
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

	fmt.Fprintf(stdout, "已创建管理员: %s（%s）\n", report.Username, report.Role)
	fmt.Fprintf(stdout, "数据库: %s\n", report.DatabasePath)
	fmt.Fprintf(stdout, "接下来: 启动 upanel-web，用该账号登录面板。\n")
	return 0
}

// readNewPassword 读取并确认新密码。
//
// 终端：两次输入都不回显，必须一致。
// 管道/脚本：两行密码，必须一致（不允许只读一行就生效，避免把误触的回车当成密码）。
func readNewPassword(stdout, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(stdout, "新密码（输入不回显，至少 %d 个字符）: ", auth.MinPasswordLength)
		first, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stdout)
		if err != nil {
			return "", fmt.Errorf("读取密码失败: %w", err)
		}
		fmt.Fprint(stdout, "再次输入新密码: ")
		second, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stdout)
		if err != nil {
			return "", fmt.Errorf("读取密码失败: %w", err)
		}
		if string(first) != string(second) {
			return "", errors.New("两次输入的密码不一致")
		}
		return string(first), nil
	}

	// 非终端：从管道读取两行。密码不会回显在任何地方。
	fmt.Fprintln(stderr, "提示: 从标准输入读取密码（两行，必须一致）")
	reader := bufio.NewReader(stdin)
	first, err := readLine(reader)
	if err != nil {
		return "", fmt.Errorf("读取密码失败: %w", err)
	}
	second, err := readLine(reader)
	if err != nil {
		return "", fmt.Errorf("需要两行密码用于确认: %w", err)
	}
	if first != second {
		return "", errors.New("两行密码不一致")
	}
	return first, nil
}

// readLine 读一行（去掉行尾 \n 与 \r），EOF 时返回错误而不是空串。
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
