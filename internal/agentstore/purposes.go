package agentstore

import "fmt"

// Purpose 是一个固定的秘密用途（CONTRACTS.md 第 8、14.2 节）。
//
// 用途是**闭集**：不在表里的用途一律拒绝，避免出现“随便起个名字存点东西”的通用
// 键值存储。用途在创建时固定，之后不随引用改变——解密用的 AAD 也把它绑进去。
type Purpose struct {
	// Name 是写入数据库的用途名。
	Name string
	// Uploadable 表示该用途可以通过 POST /internal/v1/secrets 上传。
	// totp_seed 为 false：它由代理内部的 enrollment 流程生成，
	// 不接受通用秘密上传替代（CONTRACTS.md 第 8 节）。
	Uploadable bool
	// MaxBytes 是该用途允许的明文长度上限。
	MaxBytes int
}

// purposes 是用途闭集。顺序仅用于稳定输出，不参与匹配。
var purposes = []Purpose{
	{Name: "smb_password", Uploadable: true, MaxBytes: 4 << 10},
	{Name: "account_password", Uploadable: true, MaxBytes: 4 << 10},
	{Name: "proxy_basic_password", Uploadable: true, MaxBytes: 4 << 10},
	{Name: "backup_repository_password", Uploadable: true, MaxBytes: 4 << 10},
	{Name: "notification_password", Uploadable: true, MaxBytes: 4 << 10},
	{Name: "tls_private_key", Uploadable: true, MaxBytes: 16 << 10},
	{Name: "sftp_private_key", Uploadable: true, MaxBytes: 16 << 10},
	{Name: "app_env", Uploadable: true, MaxBytes: MaxSecretValueBytes},
	// 代理内部用途：只能由代理自己的流程写入，不接受上传。
	{Name: "totp_seed", Uploadable: false, MaxBytes: 1 << 10},
}

// LookupPurpose 按名字查找用途。
func LookupPurpose(name string) (Purpose, bool) {
	for _, p := range purposes {
		if p.Name == name {
			return p, true
		}
	}
	return Purpose{}, false
}

// UploadablePurposes 返回允许上传的用途名（保持固定顺序，便于契约文档与测试比对）。
func UploadablePurposes() []string {
	out := make([]string, 0, len(purposes))
	for _, p := range purposes {
		if p.Uploadable {
			out = append(out, p.Name)
		}
	}
	return out
}

// validateValue 校验明文与该用途的长度约束。
func (p Purpose) validateValue(value []byte) error {
	if len(value) == 0 {
		return fmt.Errorf("%w: 秘密内容不能为空", ErrValueSize)
	}
	if len(value) > p.MaxBytes {
		return fmt.Errorf("%w: 用途 %s 的明文上限为 %d 字节，实际 %d 字节", ErrValueSize, p.Name, p.MaxBytes, len(value))
	}
	return nil
}

// validateReference 校验引用标识：可打印 ASCII、不含控制字符、长度受限。
func validateReference(ref string) error {
	if ref == "" {
		return nil
	}
	if len(ref) > MaxReferenceLength {
		return fmt.Errorf("%w: 长度不能超过 %d 字节", ErrReference, MaxReferenceLength)
	}
	for _, r := range ref {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("%w: 只能包含可打印 ASCII 字符，发现 %q", ErrReference, r)
		}
	}
	return nil
}
