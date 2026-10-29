package main

// totp-reset 是本机管理员恢复二次验证的唯一入口（PLAN §5「1.0 添加 TOTP，CLI 提供本机
// 管理员恢复流程」）。
//
// 为什么必须有这条命令：管理员同时丢了 authenticator 与十枚恢复码时，Web 上没有任何
// 合法入口能关掉二次验证——`DELETE /auth/totp` 要口令**加**一种第二因子，登录本身也要
// 第二因子。没有这条命令，"丢了手机"的唯一出路就是恢复数据库备份，而备份可能是几小时前的。
//
// 边界（与 Web 接口同样严格，不因为"在本机"就放宽）：
//   - 只能清掉**已存在**账号的二次验证状态，不能建号、不能改口令、不能改角色、不能启用账号；
//   - 真正的授权是"能写 upanel.db"这把钥匙（与 `reset-password` 同一条理由）：谁本来就能
//     改这个库，谁本来就能清掉这张表。它不新增任何特权通道；
//   - 清掉之后**作废该账号的全部会话**：认证强度变了，旧会话不能继续生效；
//   - 写一条 `auth.totp.reset` 审计，actor 记为本地 CLI，不是某个账号（没有人证明过自己是谁）；
//   - 代理侧那条密文尽力销毁（需要以 upanel-web 身份运行才能通过 SO_PEERCRED 校验）。
//     销毁不掉也不会让面板多出一条可用凭据——面板库里已经没有任何人引用它，登录不会再
//     校验它——但要在输出里说清楚，不能假装清干净了。

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"upanel/internal/agentclient"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/store"
	"upanel/migrations"
)

// totpResetReport 是 totp-reset 的输出（不含任何秘密）。
type totpResetReport struct {
	DatabasePath  string `json:"database_path"`
	UserID        int64  `json:"user_id"`
	Username      string `json:"username"`
	Role          string `json:"role"`
	StateCleared  bool   `json:"state_cleared"`
	RecoveryCodes int    `json:"recovery_codes_cleared"`
	// AgentStatus 是代理侧种子的处理结果，闭集：
	// destroyed（已销毁）/ already_gone（本来就不在）/ unreachable（连不上，已放弃）/
	// forbidden（对端身份不被接受，通常在非 upanel-web 身份下运行）/ skipped（--no-agent）。
	AgentStatus     string `json:"agent_status"`
	AgentDetail     string `json:"agent_detail,omitempty"`
	RevokedSessions int64  `json:"revoked_sessions"`
	SchemaVersion   int    `json:"schema_version"`
}

func cmdTotpReset(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("totp-reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
	dbFlag := fs.String("db", "", "数据库文件绝对路径（覆盖 database.path）")
	socketFlag := fs.String("socket", "", "代理 socket 路径（覆盖 agent.socket_path）")
	username := fs.String("username", "", "要清掉二次验证的账号（必填）")
	noAgent := fs.Bool("no-agent", false, "不去碰代理侧种子（代理已卸载或 socket 不存在时用）")
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
	socketPath := strings.TrimSpace(*socketFlag)
	if socketPath == "" {
		socketPath = res.Config.Agent.SocketPath
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
	state, exists, err := svc.TotpState(ctx, target.ID)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 读取二次验证状态失败: %v\n", err)
		return 1
	}
	if !exists || !state.Enabled {
		fmt.Fprintf(stderr, "错误: 账号 %q 没有启用二次验证，没有可恢复的东西。\n", target.Username)
		fmt.Fprintln(stderr, "      如果你是想绕开验证码登录，那不是恢复：口令加恢复码仍然是唯一入口。")
		return 1
	}

	// 代理侧先尽力销毁：这一步失败不阻断恢复，但必须如实报告。
	status, detail := "skipped", "按 --no-agent 跳过"
	if !*noAgent {
		status, detail = destroyTotpSeed(ctx, socketPath, target.ID, state.SecretRef, stderr)
	}

	recovery, err := svc.RecoveryCodesRemaining(ctx, target.ID)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 统计恢复码失败: %v\n", err)
		return 1
	}
	if err := svc.DisableTotp(ctx, target.ID); err != nil {
		fmt.Fprintf(stderr, "错误: 清除二次验证状态失败: %v\n", err)
		return 1
	}
	revoked, err := svc.RevokeAllForUser(ctx, target.ID)
	if err != nil {
		// 状态已经清掉了，会话没作废不能反过来"恢复原状"；说清楚，让操作者自己拔网线。
		fmt.Fprintf(stderr, "警告: 二次验证已清除，但作废会话失败: %v\n", err)
		fmt.Fprintln(stderr, "      请手动重启 upanel-web，或让该账号的所有设备重新登录。")
	}

	writer, err := audit.New(db, nil)
	if err != nil {
		fmt.Fprintf(stderr, "警告: 审计写入不可用: %v\n", err)
	} else if err := writer.Record(ctx, audit.Event{
		ActorSnapshot: "local-cli:" + localActor(),
		Action:        audit.ActionTotpReset,
		Target:        fmt.Sprintf("user:%d", target.ID),
		Outcome:       audit.OutcomeSuccess,
		Details: map[string]any{
			"username":         target.Username,
			"source":           "upanelctl totp-reset",
			"agent_status":     status,
			"recovery_codes":   recovery,
			"revoked_sessions": revoked,
		},
	}); err != nil {
		fmt.Fprintf(stderr, "警告: 审计写入失败: %v\n", err)
	}

	report := totpResetReport{
		DatabasePath:    dbPath,
		UserID:          target.ID,
		Username:        target.Username,
		Role:            string(target.Role),
		StateCleared:    true,
		RecoveryCodes:   recovery,
		AgentStatus:     status,
		AgentDetail:     detail,
		RevokedSessions: revoked,
		SchemaVersion:   migrated.To,
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

	fmt.Fprintf(stdout, "已清除账号 %s（%s）的二次验证\n", report.Username, report.Role)
	fmt.Fprintf(stdout, "作废会话 %d 条，剩余 %d 枚恢复码一并删除。\n", report.RevokedSessions, report.RecoveryCodes)
	switch report.AgentStatus {
	case "destroyed":
		fmt.Fprintf(stdout, "代理侧种子已销毁。\n")
	case "already_gone":
		fmt.Fprintf(stdout, "代理侧种子本来就不在（可能已过期）。\n")
	default:
		fmt.Fprintf(stdout, "注意: 代理侧种子未销毁（%s）。面板已经不再引用它，任何登录都不会再校验它；\n", report.AgentStatus)
		fmt.Fprintln(stdout, "      密文仍留在代理的秘密库里（引用已无人指向）。要让它当场销毁，请以 upanel-web 身份运行")
		fmt.Fprintln(stdout, "      本命令（root 会被 SO_PEERCRED 规则拒绝）；已经清完之后只能按手册清理代理秘密库。")
	}
	fmt.Fprintf(stdout, "下一步: 用口令登录后在「设置 → 二次验证」重新登记一台 authenticator。\n")
	fmt.Fprintf(stdout, "数据库: %s\n", report.DatabasePath)
	return 0
}

// destroyTotpSeed 尽力让代理销毁那条种子，返回闭集状态与一句人话说明。
//
// 只用代理自己的接口（不直接改代理库）：代理库的密钥与文件布局是代理的内部事务，
// CLI 去动它就是绕过它自己的校验。
func destroyTotpSeed(ctx context.Context, socketPath string, userID int64, secretRef string, stderr io.Writer) (string, string) {
	client, err := agentclient.New(socketPath)
	if err != nil {
		return "unreachable", err.Error()
	}
	destroyed, err := client.TotpDisable(ctx, userID, secretRef)
	if err != nil {
		var bizErr *agentclient.Error
		if errors.As(err, &bizErr) && bizErr.Status == 403 {
			return "forbidden", "对端身份不被代理接受：请以 upanel-web 身份运行本命令"
		}
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			fmt.Fprintf(stderr, "警告: 代理不可用，跳过种子销毁: %v\n", err)
			return "unreachable", err.Error()
		}
		fmt.Fprintf(stderr, "警告: 代理拒绝销毁种子: %v\n", err)
		return "error", err.Error()
	}
	if destroyed {
		return "destroyed", ""
	}
	return "already_gone", ""
}
