// Package config 负责配置读取、默认值、覆盖优先级与校验。
//
// 配置文件为 JSON（严格解码：未知字段直接报错，不接受拼写错误的静默忽略）。
// 优先级：内置默认值 < 配置文件 < 命令行参数。
//
// 固定约定来自 PLAN.md 第 4、5 节：
//   - 面板默认绑定 127.0.0.1:9443；
//   - mock 模式只能绑定 127.0.0.1:18881，与真实代理完全隔离；
//   - 代理只监听 Unix socket，默认 /run/upanel/agent.sock。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/logging"
	"upanel/internal/platform"
)

// 固定端口与路径（PLAN.md 第 4、5 节，CONTRACTS.md 第 3 节）。
const (
	// ListenProduction 是 production 模式的默认监听地址。
	ListenProduction = "127.0.0.1:9443"
	// ListenMock 是 mock 模式的唯一允许监听地址。
	ListenMock = "127.0.0.1:18881"
	// AgentSocketPath 是特权代理的默认 Unix socket 路径。
	AgentSocketPath = "/run/upanel/agent.sock"
	// AgentWebUser 是普通代理接口唯一允许的对端账号（PLAN.md 第 4 节）。
	AgentWebUser = "upanel-web"

	// 数据库 busy_timeout 的允许范围（毫秒）。
	minBusyTimeoutMilliseconds     = 100
	maxBusyTimeoutMilliseconds     = 60000
	defaultBusyTimeoutMilliseconds = 5000

	minShutdownTimeoutSeconds = 1
	maxShutdownTimeoutSeconds = 120
	defaultShutdownSeconds    = 10
)

// Config 是配置文件根结构。Web 与代理共用同一份文件，各自只读取自己的段。
type Config struct {
	Web      Web      `json:"web"`
	Agent    Agent    `json:"agent"`
	Database Database `json:"database"`
	Log      Log      `json:"log"`
}

// Web 是 Web 服务配置。
type Web struct {
	// Mode 为 production 或 mock；默认 production。
	Mode string `json:"mode"`
	// ListenAddress 为 host:port；留空时按模式取默认值。
	ListenAddress string `json:"listen_address"`
	// AllowedOrigins 是允许发起状态变更请求的源（scheme://host[:port]）。
	// 留空时按最终监听地址推导（仅对回环地址成立）；非回环地址必须显式配置。
	AllowedOrigins []string `json:"allowed_origins"`
	// ShutdownTimeoutSeconds 为优雅停止等待时间。
	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`
}

// Agent 是特权代理配置。代理没有 mock 模式。
type Agent struct {
	// SocketPath 必须是绝对路径。
	SocketPath string `json:"socket_path"`
	// ShutdownTimeoutSeconds 为优雅停止等待时间。
	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`
	// WebUser 是普通接口唯一允许的对端账号（PLAN.md 第 4 节：upanel-web）。
	// 启动时按账号名解析成数字 UID，用 SO_PEERCRED 与内核报告的身份比对。
	WebUser string `json:"web_user"`
	// SocketGroup 是 socket 文件的属组；留空时与 WebUser 相同。
	SocketGroup string `json:"socket_group"`
	// DatabasePath 是代理数据库的绝对路径（PLAN.md 第 4 节：root 独占）。
	// Linux 默认 /var/lib/upanel-agent/agent.db；非 Linux 没有默认值。
	DatabasePath string `json:"database_path"`
	// SecretKeyPath 是本机秘密密钥文件的绝对路径（CONTRACTS.md 14.2，root:root 0600）。
	// Linux 默认 /etc/upanel/agent.key；非 Linux 没有默认值。
	SecretKeyPath string `json:"secret_key_path"`
}

// Database 是 Web 侧 SQLite 配置（PLAN.md 第 3、4 节）。
type Database struct {
	// Path 必须是绝对路径；Linux 默认 /var/lib/upanel/upanel.db。
	// 非 Linux 平台没有默认值：开发宿主必须显式指定，避免误写到系统目录。
	Path string `json:"path"`
	// BusyTimeoutMilliseconds 是写锁等待上限，默认 5000。
	BusyTimeoutMilliseconds int `json:"busy_timeout_ms"`
}

// Log 是日志配置。
type Log struct {
	Level     string `json:"level"`
	Format    string `json:"format"`
	AddSource bool   `json:"add_source"`
}

// Defaults 返回内置默认配置。
func Defaults() Config {
	return Config{
		Web: Web{
			Mode:                   platform.ModeProduction.String(),
			ListenAddress:          ListenProduction,
			ShutdownTimeoutSeconds: defaultShutdownSeconds,
		},
		Agent: Agent{
			SocketPath:             AgentSocketPath,
			ShutdownTimeoutSeconds: defaultShutdownSeconds,
			WebUser:                AgentWebUser,
			DatabasePath:           defaultAgentDatabasePath(),
			SecretKeyPath:          defaultAgentSecretKeyPath(),
		},
		Database: Database{
			Path:                    defaultDatabasePath(),
			BusyTimeoutMilliseconds: defaultBusyTimeoutMilliseconds,
		},
		Log: Log{Level: "info", Format: string(logging.FormatJSON)},
	}
}

// BusyTimeout 返回数据库 busy_timeout。
func (d Database) BusyTimeout() time.Duration {
	return time.Duration(d.BusyTimeoutMilliseconds) * time.Millisecond
}

// Result 是加载结果。
type Result struct {
	Config Config
	// Source 为实际读取的配置文件路径；使用内置默认值时为 ""。
	Source string
	// Explicit 表示路径由调用方显式指定。
	Explicit bool
	// ListenExplicit 表示 web.listen_address 由配置文件显式给出（而不是内建默认值）。
	// mock 模式的安全判断依赖它：只有真正显式写错地址时才报错。
	ListenExplicit bool
}

// Load 读取配置。
//
//   - 显式传入 path：文件必须存在且合法，否则返回错误（不允许悄悄改用默认值）。
//   - 未传入 path：尝试各平台默认路径；不存在时使用内置默认值，Source 为空。
//
// 返回的错误信息包含路径与具体字段问题，供 CLI 直接展示。
func Load(path string) (Result, error) {
	res := Result{Config: Defaults()}

	explicit := strings.TrimSpace(path) != ""
	if !explicit {
		path = defaultPath()
		if path == "" {
			return res, nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("读取配置文件失败 %s: %w", path, err)
	}

	cfg := Defaults()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return res, fmt.Errorf("解析配置文件失败 %s: %w", path, err)
	}
	// 拒绝文件尾部的多余内容，避免“改了两个对象只生效一个”的静默错误。
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return res, fmt.Errorf("解析配置文件失败 %s: 文件在首个 JSON 对象之后仍有内容", path)
	}

	res.Config = cfg
	res.Source = path
	res.Explicit = explicit

	// 记录 web.listen_address 是否在文件中显式出现（严格解码已通过，这里的宽松解析只用于
	// 判断“键是否存在”，不参与取值）。
	var presence struct {
		Web map[string]json.RawMessage `json:"web"`
	}
	if err := json.Unmarshal(data, &presence); err == nil {
		_, res.ListenExplicit = presence.Web["listen_address"]
	}
	return res, nil
}

// Validate 校验整份配置。返回的错误面向运维人员，指出具体字段。
func (c Config) Validate() error {
	if err := c.Web.Validate(); err != nil {
		return err
	}
	if err := c.Agent.Validate(); err != nil {
		return err
	}
	if err := c.Database.Validate(); err != nil {
		return err
	}
	return c.Log.Validate()
}

// Validate 校验数据库段。
//
// 路径为空表示“本平台没有默认数据库位置”，此时必须由命令行显式给出；
// 校验本身不强制非空，是为了让 check-config 能如实报告“未配置”。
func (d Database) Validate() error {
	if d.Path != "" {
		if !path.IsAbs(d.Path) {
			return fmt.Errorf("database.path 必须是 POSIX 绝对路径，当前为 %q", d.Path)
		}
		if strings.ContainsAny(d.Path, "?#") {
			return fmt.Errorf("database.path 不能包含 '?' 或 '#'，当前为 %q", d.Path)
		}
	}
	if d.BusyTimeoutMilliseconds < minBusyTimeoutMilliseconds || d.BusyTimeoutMilliseconds > maxBusyTimeoutMilliseconds {
		return fmt.Errorf("database.busy_timeout_ms 必须在 %d—%d 之间，当前为 %d",
			minBusyTimeoutMilliseconds, maxBusyTimeoutMilliseconds, d.BusyTimeoutMilliseconds)
	}
	return nil
}

// Validate 校验 Web 段。
func (w Web) Validate() error {
	if _, err := platform.ParseMode(w.Mode); err != nil {
		return fmt.Errorf("web.mode: %w", err)
	}
	if _, err := validateListen(w.ListenAddress); err != nil {
		return fmt.Errorf("web.listen_address: %w", err)
	}
	if err := validateAllowedOrigins(w.AllowedOrigins); err != nil {
		return fmt.Errorf("web.allowed_origins: %w", err)
	}
	// “非回环地址必须显式配置允许源”这条跨字段规则放在 ResolveAllowedOrigins 里判断：
	// 它需要最终监听地址，而这里是配置段的单段校验。check-config 与启动路径都会调用它。
	if w.ShutdownTimeoutSeconds < minShutdownTimeoutSeconds || w.ShutdownTimeoutSeconds > maxShutdownTimeoutSeconds {
		return fmt.Errorf("web.shutdown_timeout_seconds 必须在 %d—%d 之间，当前为 %d",
			minShutdownTimeoutSeconds, maxShutdownTimeoutSeconds, w.ShutdownTimeoutSeconds)
	}
	return nil
}

// Validate 校验代理段。
func (a Agent) Validate() error {
	if a.SocketPath == "" {
		return errors.New("agent.socket_path 不能为空")
	}
	// 代理只在 Linux 上运行，socket 路径按 POSIX 语义判断（不受开发宿主路径分隔符影响）。
	if !path.IsAbs(a.SocketPath) {
		return fmt.Errorf("agent.socket_path 必须是 POSIX 绝对路径，当前为 %q", a.SocketPath)
	}
	if a.ShutdownTimeoutSeconds < minShutdownTimeoutSeconds || a.ShutdownTimeoutSeconds > maxShutdownTimeoutSeconds {
		return fmt.Errorf("agent.shutdown_timeout_seconds 必须在 %d—%d 之间，当前为 %d",
			minShutdownTimeoutSeconds, maxShutdownTimeoutSeconds, a.ShutdownTimeoutSeconds)
	}
	if err := validateAccountName("agent.web_user", a.WebUser); err != nil {
		return err
	}
	if a.SocketGroup != "" {
		if err := validateAccountName("agent.socket_group", a.SocketGroup); err != nil {
			return err
		}
	}
	// 两个代理数据路径都按 POSIX 语义校验。空值表示“本平台没有默认路径”，
	// 必须在配置文件或命令行显式给出——由启动路径判断，而不是在这里悄悄填一个。
	for _, item := range []struct{ field, value string }{
		{"agent.database_path", a.DatabasePath},
		{"agent.secret_key_path", a.SecretKeyPath},
	} {
		if item.value == "" {
			continue
		}
		if !path.IsAbs(item.value) {
			return fmt.Errorf("%s 必须是 POSIX 绝对路径，当前为 %q", item.field, item.value)
		}
		if strings.ContainsAny(item.value, "?#") {
			return fmt.Errorf("%s 不能包含 '?' 或 '#'，当前为 %q", item.field, item.value)
		}
	}
	return nil
}

// validateAccountName 校验账号/组名：只允许常规名字，不接受数字 UID、路径或空值。
// 使用数字 UID 会绕过“专用账号”的要求，因此这里直接拒绝纯数字。
func validateAccountName(field, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%s 不能为空（普通接口只允许专用 Web 账号访问）", field)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s 含有非法字符 %q，只接受账号名而不是数字 UID 或路径", field, r)
		}
	}
	if _, err := strconv.Atoi(name); err == nil {
		return fmt.Errorf("%s 不能是纯数字的 UID %q：必须使用专用账号名", field, name)
	}
	return nil
}

// ShutdownTimeout 返回代理优雅停止等待时间。
func (a Agent) ShutdownTimeout() time.Duration {
	return time.Duration(a.ShutdownTimeoutSeconds) * time.Second
}

// Validate 校验日志段。
func (l Log) Validate() error {
	if _, err := logging.ParseLevel(l.Level); err != nil {
		return fmt.Errorf("log.level: %w", err)
	}
	if _, err := logging.ParseFormat(l.Format); err != nil {
		return fmt.Errorf("log.format: %w", err)
	}
	return nil
}

// ModeValue 返回解析后的运行模式（Mode 字段名留给配置字段本身）。
func (w Web) ModeValue() (platform.Mode, error) {
	return platform.ParseMode(w.Mode)
}

// ResolveListen 计算最终监听地址。
//
// listenExplicit 表示地址由命令行或配置文件显式给出：
//   - mock 模式且未显式指定 → 固定 127.0.0.1:18881；
//   - mock 模式且显式指定为其他地址 → 报错，不允许 mock 暴露到别处；
//   - production 模式 → 使用给定地址。
func ResolveListen(mode platform.Mode, address string, listenExplicit bool) (string, error) {
	address = strings.TrimSpace(address)
	if mode == platform.ModeMock {
		if !listenExplicit {
			return ListenMock, nil
		}
		if address != ListenMock {
			return "", fmt.Errorf("mock 模式只能绑定 %s，当前配置为 %q（演示数据不得暴露到其他地址）", ListenMock, address)
		}
		return ListenMock, nil
	}
	if address == "" {
		return ListenProduction, nil
	}
	if _, err := validateListen(address); err != nil {
		return "", err
	}
	return address, nil
}

// ShutdownTimeout 返回优雅停止等待时间。
func (w Web) ShutdownTimeout() time.Duration {
	return time.Duration(w.ShutdownTimeoutSeconds) * time.Second
}

// validateListen 校验 host:port 形式。
//
// 要求显式主机：":9443" 这类“所有接口”的写法必须写成 "0.0.0.0:9443" 或 "::"，
// 避免误配置把面板暴露到公网。
func validateListen(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return ListenProduction, nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("不是合法的 host:port（%v）；例如 127.0.0.1:9443", err)
	}
	if strings.TrimSpace(host) == "" {
		return "", errors.New("必须显式给出监听主机，例如 127.0.0.1 或 0.0.0.0，不能用 :port 省略")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("端口必须是 1—65535 之间的整数，当前为 %q", port)
	}
	return address, nil
}

// validateAllowedOrigins 校验 web.allowed_origins 的每一项都能规范化。
//
// 允许为空（回环部署时推导即可）；但不允许写入无法识别的值——那只会让登录
// 在运行时诡异失败，而不是在启动时明确报错。
func validateAllowedOrigins(origins []string) error {
	for i, origin := range origins {
		if _, err := httpx.NormalizeOrigin(origin); err != nil {
			return fmt.Errorf("第 %d 项不合法: %w", i+1, err)
		}
	}
	return nil
}

// ResolveAllowedOrigins 计算最终允许源列表。
//
//   - 配置里显式给出了源 → 规范化、去重后使用（无论监听地址是什么）。
//   - 配置为空且监听地址是回环 → 用 http://127.0.0.1:<port>、http://localhost:<port>
//     以及监听主机自身（当它既不是 127.0.0.1 也不是 localhost 时，例如 ::1）。
//   - 配置为空且监听地址非回环 → 返回错误。
//
// 期望值只来自本地配置，**不**从 Host 头或 X-Forwarded-* 推导（CONTRACTS.md 第 2 节）。
func ResolveAllowedOrigins(origins []string, listenAddress string) ([]string, error) {
	if len(origins) > 0 {
		set, err := httpx.NewOriginSet(origins)
		if err != nil {
			return nil, fmt.Errorf("web.allowed_origins: %w", err)
		}
		return set.List(), nil
	}

	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddress))
	if err != nil {
		return nil, fmt.Errorf("web.allowed_origins: 无法从监听地址 %q 推导允许源: %w", listenAddress, err)
	}
	if !IsLoopbackAddress(listenAddress) {
		return nil, fmt.Errorf("web.allowed_origins 不能为空：监听地址 %s 不是回环地址", listenAddress)
	}

	hosts := []string{"127.0.0.1", "localhost"}
	if h := strings.Trim(host, "[]"); h != "" && h != "127.0.0.1" && !strings.EqualFold(h, "localhost") {
		hosts = append(hosts, h)
	}
	raw := make([]string, 0, len(hosts))
	for _, h := range hosts {
		raw = append(raw, "http://"+net.JoinHostPort(h, port))
	}
	set, err := httpx.NewOriginSet(raw)
	if err != nil {
		return nil, fmt.Errorf("web.allowed_origins: 推导出的源不合法: %w", err)
	}
	return set.List(), nil
}

// IsLoopbackAddress 判断监听地址是否只绑定回环接口。
func IsLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
