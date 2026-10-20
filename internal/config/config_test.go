package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"upanel/internal/platform"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

func TestDefaultsValid(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("内置默认值必须合法: %v", err)
	}
	if cfg.Web.Mode != platform.ModeProduction.String() {
		t.Fatalf("默认模式应为 production，实际 %q", cfg.Web.Mode)
	}
	if cfg.Web.ListenAddress != ListenProduction {
		t.Fatalf("默认监听应为 %s，实际 %q", ListenProduction, cfg.Web.ListenAddress)
	}
	if cfg.Agent.SocketPath != AgentSocketPath {
		t.Fatalf("默认 socket 应为 %s，实际 %q", AgentSocketPath, cfg.Agent.SocketPath)
	}
	if cfg.Agent.WebUser != AgentWebUser {
		t.Fatalf("默认对端账号应为 %s，实际 %q", AgentWebUser, cfg.Agent.WebUser)
	}
	if cfg.Agent.SocketGroup != "" {
		t.Fatalf("默认 socket 属组应留空（跟随 web_user），实际 %q", cfg.Agent.SocketGroup)
	}
}

func TestLoadExplicitMissingFileIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	if _, err := Load(missing); err == nil {
		t.Fatal("显式指定的配置文件不存在时必须报错，不能悄悄退回默认值")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeTemp(t, "c.json", `{"web":{"mode":"production","unknown_field":1}}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("未知字段必须导致解析失败")
	}
	if !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("错误信息应指出未知字段，实际: %v", err)
	}
}

func TestLoadRejectsTrailingContent(t *testing.T) {
	path := writeTemp(t, "c.json", `{"web":{"mode":"production"}}{"web":{"mode":"mock"}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("首个 JSON 对象之后的多余内容必须报错")
	}
}

func TestLoadTracksListenExplicit(t *testing.T) {
	without := writeTemp(t, "a.json", `{"web":{"mode":"mock"}}`)
	res, err := Load(without)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if res.ListenExplicit {
		t.Fatal("配置未写 listen_address 时 ListenExplicit 应为 false")
	}

	with := writeTemp(t, "b.json", `{"web":{"listen_address":"127.0.0.1:19443"}}`)
	res, err = Load(with)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !res.ListenExplicit {
		t.Fatal("配置写了 listen_address 时 ListenExplicit 应为 true")
	}
	if res.Config.Web.ListenAddress != "127.0.0.1:19443" {
		t.Fatalf("读取到 %q", res.Config.Web.ListenAddress)
	}
}

func TestLoadPartialConfigKeepsDefaults(t *testing.T) {
	path := writeTemp(t, "c.json", `{"log":{"level":"debug"}}`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if res.Config.Log.Level != "debug" {
		t.Fatalf("log.level = %q", res.Config.Log.Level)
	}
	if res.Config.Log.Format != Defaults().Log.Format {
		t.Fatalf("未指定字段应保留默认值，实际 %q", res.Config.Log.Format)
	}
	if res.Config.Web.Mode != platform.ModeProduction.String() {
		t.Fatalf("web.mode 应保留默认值，实际 %q", res.Config.Web.Mode)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"未知模式", func(c *Config) { c.Web.Mode = "demo" }, "web.mode"},
		{"端口越界", func(c *Config) { c.Web.ListenAddress = "127.0.0.1:70000" }, "web.listen_address"},
		{"省略主机", func(c *Config) { c.Web.ListenAddress = ":9443" }, "web.listen_address"},
		{"停止超时过小", func(c *Config) { c.Web.ShutdownTimeoutSeconds = 0 }, "shutdown_timeout_seconds"},
		{"socket 相对路径", func(c *Config) { c.Agent.SocketPath = "run/agent.sock" }, "agent.socket_path"},
		{"代理停止超时过大", func(c *Config) { c.Agent.ShutdownTimeoutSeconds = 999 }, "shutdown_timeout_seconds"},
		{"对端账号为空", func(c *Config) { c.Agent.WebUser = "  " }, "agent.web_user"},
		{"对端账号是数字 UID", func(c *Config) { c.Agent.WebUser = "1000" }, "agent.web_user"},
		{"对端账号含路径", func(c *Config) { c.Agent.WebUser = "../etc/passwd" }, "agent.web_user"},
		{"socket 属组含冒号", func(c *Config) { c.Agent.SocketGroup = "root:root" }, "agent.socket_group"},
		{"代理数据库相对路径", func(c *Config) { c.Agent.DatabasePath = "data/agent.db" }, "agent.database_path"},
		{"代理数据库含问号", func(c *Config) { c.Agent.DatabasePath = "/var/lib/agent.db?x=1" }, "agent.database_path"},
		{"密钥相对路径", func(c *Config) { c.Agent.SecretKeyPath = "agent.key" }, "agent.secret_key_path"},
		{"未知日志级别", func(c *Config) { c.Log.Level = "trace" }, "log.level"},
		{"未知日志格式", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s: 应当报错", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: 错误应提到 %q，实际: %v", tc.name, tc.want, err)
			}
		})
	}
}

func TestResolveListen(t *testing.T) {
	cases := []struct {
		name     string
		mode     platform.Mode
		address  string
		explicit bool
		want     string
		wantErr  bool
	}{
		{"mock 未显式指定", platform.ModeMock, ListenProduction, false, ListenMock, false},
		{"mock 显式正确", platform.ModeMock, ListenMock, true, ListenMock, false},
		{"mock 显式错误", platform.ModeMock, "127.0.0.1:9443", true, "", true},
		{"mock 显式 0.0.0.0", platform.ModeMock, "0.0.0.0:18881", true, "", true},
		{"production 默认", platform.ModeProduction, "", false, ListenProduction, false},
		{"production 自定义", platform.ModeProduction, "10.0.0.5:9443", true, "10.0.0.5:9443", false},
		{"production 非法", platform.ModeProduction, "nope", true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveListen(tc.mode, tc.address, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("得到 %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestIsLoopbackAddress(t *testing.T) {
	loopback := []string{"127.0.0.1:9443", "[::1]:9443", "localhost:9443"}
	for _, a := range loopback {
		if !IsLoopbackAddress(a) {
			t.Fatalf("%q 应判定为回环", a)
		}
	}
	nonLoopback := []string{"0.0.0.0:9443", "10.0.0.5:9443", "garbage"}
	for _, a := range nonLoopback {
		if IsLoopbackAddress(a) {
			t.Fatalf("%q 不应判定为回环", a)
		}
	}
}

func TestShutdownTimeoutConversions(t *testing.T) {
	cfg := Defaults()
	if got := cfg.Web.ShutdownTimeout().Seconds(); got != defaultShutdownSeconds {
		t.Fatalf("web 停止超时 = %v 秒", got)
	}
	if got := cfg.Agent.ShutdownTimeout().Seconds(); got != defaultShutdownSeconds {
		t.Fatalf("agent 停止超时 = %v 秒", got)
	}
}

func TestDatabaseDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Database.BusyTimeoutMilliseconds != defaultBusyTimeoutMilliseconds {
		t.Fatalf("默认 busy_timeout_ms = %d", cfg.Database.BusyTimeoutMilliseconds)
	}
	if got := cfg.Database.BusyTimeout(); got != 5*time.Second {
		t.Fatalf("BusyTimeout() = %v，期望 5s", got)
	}
	if cfg.Database.Path != defaultDatabasePath() {
		t.Fatalf("默认数据库路径 = %q，期望 %q", cfg.Database.Path, defaultDatabasePath())
	}
	if runtime.GOOS != "linux" && cfg.Database.Path != "" {
		t.Fatalf("非 Linux 平台不应给出默认数据库路径，实际 %q", cfg.Database.Path)
	}
}

func TestDatabaseValidate(t *testing.T) {
	cases := []struct {
		name    string
		db      Database
		wantErr bool
	}{
		{name: "默认值合法", db: Database{Path: "/var/lib/upanel/upanel.db", BusyTimeoutMilliseconds: 5000}},
		{name: "路径可留空由 --db 提供", db: Database{Path: "", BusyTimeoutMilliseconds: 5000}},
		{name: "相对路径拒绝", db: Database{Path: "data/upanel.db", BusyTimeoutMilliseconds: 5000}, wantErr: true},
		{name: "问号拒绝", db: Database{Path: "/var/lib/upanel/a?b.db", BusyTimeoutMilliseconds: 5000}, wantErr: true},
		{name: "井号拒绝", db: Database{Path: "/var/lib/upanel/a#b.db", BusyTimeoutMilliseconds: 5000}, wantErr: true},
		{name: "busy_timeout 过小拒绝", db: Database{Path: "/tmp/x.db", BusyTimeoutMilliseconds: 99}, wantErr: true},
		{name: "busy_timeout 过大拒绝", db: Database{Path: "/tmp/x.db", BusyTimeoutMilliseconds: 60001}, wantErr: true},
		{name: "busy_timeout 边界值接受", db: Database{Path: "/tmp/x.db", BusyTimeoutMilliseconds: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.db.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("应当报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("意外错误: %v", err)
			}
		})
	}
}

func TestLoadDatabaseSection(t *testing.T) {
	path := writeTemp(t, "db.json", `{"web":{"mode":"mock"},
		"database":{"path":"/srv/upanel/test.db","busy_timeout_ms":2500}}`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := res.Config.Validate(); err != nil {
		t.Fatalf("配置应合法: %v", err)
	}
	if res.Config.Database.Path != "/srv/upanel/test.db" {
		t.Fatalf("database.path = %q", res.Config.Database.Path)
	}
	if got := res.Config.Database.BusyTimeout(); got != 2500*time.Millisecond {
		t.Fatalf("BusyTimeout() = %v", got)
	}

	// 严格解析：database 段拼错的字段不得被静默忽略。
	bad := writeTemp(t, "db-bad.json", `{"database":{"path":"/srv/x.db","busy_timeout":5000}}`)
	if _, err := Load(bad); err == nil {
		t.Fatal("未知字段 busy_timeout 必须被拒绝")
	}
}

func TestResolveAllowedOriginsDerivesLoopback(t *testing.T) {
	cases := []struct {
		listen string
		want   []string
	}{
		{ListenProduction, []string{"http://127.0.0.1:9443", "http://localhost:9443"}},
		{ListenMock, []string{"http://127.0.0.1:18881", "http://localhost:18881"}},
		{"[::1]:9443", []string{"http://127.0.0.1:9443", "http://localhost:9443", "http://[::1]:9443"}},
	}
	for _, c := range cases {
		got, err := ResolveAllowedOrigins(nil, c.listen)
		if err != nil {
			t.Fatalf("%s: 推导允许源失败: %v", c.listen, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: 允许源 = %v，期望 %v", c.listen, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("%s: 允许源 = %v，期望 %v", c.listen, got, c.want)
			}
		}
	}
}

func TestResolveAllowedOriginsExplicit(t *testing.T) {
	got, err := ResolveAllowedOrigins([]string{"HTTPS://Panel.Example.com/", "https://panel.example.com"}, "0.0.0.0:9443")
	if err != nil {
		t.Fatalf("显式配置应可用: %v", err)
	}
	if len(got) != 1 || got[0] != "https://panel.example.com" {
		t.Fatalf("允许源 = %v，期望去重规范化后的单一项", got)
	}

	if _, err := ResolveAllowedOrigins([]string{"not-a-url"}, ListenProduction); err == nil {
		t.Fatal("非法配置项必须报错")
	}
}

func TestResolveAllowedOriginsRejectsNonLoopbackWithoutConfig(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:9443", "192.0.2.10:9443", "[::]:9443"} {
		if _, err := ResolveAllowedOrigins(nil, listen); err == nil {
			t.Fatalf("%s: 非回环监听且未配置允许源时必须报错", listen)
		}
	}
}

func TestWebValidateRejectsBadAllowedOrigins(t *testing.T) {
	cfg := Defaults()
	cfg.Web.AllowedOrigins = []string{"http://127.0.0.1:9443/evil"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("带路径的允许源必须被拒绝")
	}
	cfg.Web.AllowedOrigins = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("留空允许源应合法（回环监听时由启动流程推导）: %v", err)
	}
}
