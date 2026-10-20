//go:build linux

package config

import "testing"

// TestLinuxDefaults 固定 PLAN.md 第 4 节写死的系统路径，避免被无意改动。
func TestLinuxDefaults(t *testing.T) {
	if DefaultPathLinux != "/etc/upanel/upanel.json" {
		t.Fatalf("默认配置路径 = %q", DefaultPathLinux)
	}
	if DefaultDatabasePathLinux != "/var/lib/upanel/upanel.db" {
		t.Fatalf("默认数据库路径 = %q", DefaultDatabasePathLinux)
	}
	if DefaultAgentDatabasePathLinux != "/var/lib/upanel-agent/agent.db" {
		t.Fatalf("默认代理数据库路径 = %q", DefaultAgentDatabasePathLinux)
	}
	if DefaultAgentSecretKeyPathLinux != "/etc/upanel/agent.key" {
		t.Fatalf("默认秘密密钥路径 = %q", DefaultAgentSecretKeyPathLinux)
	}
	cfg := Defaults()
	if cfg.Database.Path != DefaultDatabasePathLinux {
		t.Fatalf("Defaults().Database.Path = %q", cfg.Database.Path)
	}
	if cfg.Agent.DatabasePath != DefaultAgentDatabasePathLinux {
		t.Fatalf("Defaults().Agent.DatabasePath = %q", cfg.Agent.DatabasePath)
	}
	if cfg.Agent.SecretKeyPath != DefaultAgentSecretKeyPathLinux {
		t.Fatalf("Defaults().Agent.SecretKeyPath = %q", cfg.Agent.SecretKeyPath)
	}
}
