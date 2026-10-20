//go:build linux

package config

// DefaultPathLinux 是 Linux 上的默认配置文件路径（PLAN.md 第 4 节：系统配置在 /etc/upanel/）。
const DefaultPathLinux = "/etc/upanel/upanel.json"

// defaultPath 返回当前平台的默认配置文件路径。文件不存在时按内置默认值运行。
func defaultPath() string { return DefaultPathLinux }

// DefaultDatabasePathLinux 是 Linux 上 Web 数据库的默认路径（PLAN.md 第 4 节）。
const DefaultDatabasePathLinux = "/var/lib/upanel/upanel.db"

// defaultDatabasePath 返回当前平台的默认数据库路径。
func defaultDatabasePath() string { return DefaultDatabasePathLinux }

// DefaultAgentDatabasePathLinux 是 Linux 上代理数据库的默认路径
// （PLAN.md 第 4 节：代理数据 /var/lib/upanel-agent/，root 独占）。
const DefaultAgentDatabasePathLinux = "/var/lib/upanel-agent/agent.db"

// defaultAgentDatabasePath 返回当前平台的默认代理数据库路径。
func defaultAgentDatabasePath() string { return DefaultAgentDatabasePathLinux }

// DefaultAgentSecretKeyPathLinux 是本机秘密密钥的默认路径（CONTRACTS.md 14.2：
// /etc/upanel/agent.key，root:root 0600）。
const DefaultAgentSecretKeyPathLinux = "/etc/upanel/agent.key"

// defaultAgentSecretKeyPath 返回当前平台的默认秘密密钥路径。
func defaultAgentSecretKeyPath() string { return DefaultAgentSecretKeyPathLinux }
