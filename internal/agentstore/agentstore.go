// Package agentstore 是代理侧的持久层：独立数据库与加密秘密存储（CONTRACTS.md 14.2）。
//
// 与 internal/store 的关系：复用它的打开参数（WAL、foreign_keys、busy_timeout）、
// 显式 SQL 迁移与事务工具，但**数据库是另一个文件**（/var/lib/upanel-agent/agent.db），
// 属主 root、目录 root 独占，Web 进程打不开也读不到。
//
// 本包不提供任何“读明文”的对外入口，也不把明文写进日志：明文只在内存里存在，
// 从 Put 的参数进入、经 AEAD 加密后落库，随即被清零。
package agentstore

import (
	"errors"
	"strings"
	"time"
)

// 生命周期与尺寸常量。
const (
	// TemporarySecretTTL 是未被引用的临时秘密的存活时间（CONTRACTS.md 第 8 节：10 分钟）。
	TemporarySecretTTL = 10 * time.Minute
	// MaxSecretValueBytes 是单条秘密明文的长度上限。RSA 8192 私钥 PEM 约 6.4 KiB，
	// app_env 这类多行内容也远小于此值；上限存在的意义是防止接口变成任意大对象上传。
	MaxSecretValueBytes = 32 << 10
	// MaxReferenceLength 是引用标识（reference）的长度上限。
	MaxReferenceLength = 200
)

// 秘密生命周期状态。
const (
	StateTemporary  = "temporary"
	StatePersistent = "persistent"
	StateDestroyed  = "destroyed"
)

var (
	// ErrSecretNotFound 表示引用不存在或已被销毁。
	ErrSecretNotFound = errors.New("agentstore: 秘密不存在或已销毁")
	// ErrSecretExpired 表示秘密已过销毁时间，调用方不得再解密使用。
	ErrSecretExpired = errors.New("agentstore: 秘密已过期")
	// ErrValueSize 表示明文长度不合法。
	ErrValueSize = errors.New("agentstore: 秘密长度不合法")
	// ErrPurpose 表示用途不在固定枚举内。
	ErrPurpose = errors.New("agentstore: 用途不合法")
	// ErrReference 表示引用标识不合法。
	ErrReference = errors.New("agentstore: 引用标识不合法")
)

// Secret 是一条秘密的元数据。它**不含明文**：本包没有返回明文的对外查询方法。
type Secret struct {
	// Ref 是 secret_ref，形如 sec_<32 位十六进制>。
	Ref string
	// Purpose 是创建时固定的用途枚举。
	Purpose string
	// State 是 temporary、persistent 或 destroyed。
	State string
	// Reference 是引用这份秘密的配置标识（temporary 时为空）。
	Reference string
	// CreatedAt 为创建时间（UTC）。
	CreatedAt time.Time
	// ExpiresAt 为 temporary 的销毁时间；persistent 时为零值。
	ExpiresAt time.Time
}

// wipe 把字节清零，用于尽快消除内存中的明文副本。
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// millis 把时间转成 Unix 毫秒；零值转成 0（数据库里表示“不适用”）。
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

// fromMillis 把 Unix 毫秒还原成 UTC 时间；0 还原成零值。
func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// isUniqueViolation 判断错误是否是主键/唯一约束冲突。
//
// modernc.org/sqlite 的驱动错误没有导出可判定的常量，这里按 SQLite 的固定错误文本判断；
// 只用于“引用撞车就重试一次”这一条路径，判断失误的后果是多返回一个错误，不会写坏数据。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
