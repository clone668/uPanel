package accounts

// 标识的生成与形状（P14.5）。
//
// 三种标识都进 URL、日志与审计摘要，所以形状固定：4 字符前缀 + 32 位小写十六进制
// = 36 字节。这不是审美——`sac_` 与共享那边（internal/shares/system.go）是同一个
// 前缀，因为两者写的是同一张 managed_accounts 表；两套前缀会让“这个 id 是哪张表的”
// 变成要靠前缀猜的事。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const (
	// accountIDPrefix 与 internal/shares 的 accountIDPrefix 一致：同一张表，同一种形状。
	accountIDPrefix  = "sac_"
	sshKeyIDPrefix   = "ssk_"
	sudoRuleIDPrefix = "sru_"
)

// NewAccountID 生成一个受管理账号标识。
func NewAccountID() (string, error) { return newID(accountIDPrefix) }

// NewSSHKeyID 生成一个公钥标识。
func NewSSHKeyID() (string, error) { return newID(sshKeyIDPrefix) }

// NewSudoRuleID 生成一个 sudo 规则标识。
func NewSudoRuleID() (string, error) { return newID(sudoRuleIDPrefix) }

// ValidAccountID 判断账号标识的形状。
func ValidAccountID(id string) bool { return validID(id, accountIDPrefix) }

// ValidSSHKeyID 判断公钥标识的形状。
func ValidSSHKeyID(id string) bool { return validID(id, sshKeyIDPrefix) }

// ValidSudoRuleID 判断 sudo 规则标识的形状。
func ValidSudoRuleID(id string) bool { return validID(id, sudoRuleIDPrefix) }

func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 %s 标识失败: %w", prefix, err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func validID(id, prefix string) bool {
	if len(id) != len(prefix)+32 {
		return false
	}
	if id[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
