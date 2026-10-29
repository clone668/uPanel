package agent

// 受管理 Linux 账号、SSH 公钥与受限 sudo 的动作（P14.5）。
//
// 六个动作与共享动作共用同一套骨架（参数解码 → 预检 → 执行 → 结果核验），
// 判决来自 accounts.Assess，执行来自 accounts.ExecuteWrite。适配器不是复制品：
// sharesPlanner / sharesExecutor 这两个函数吃的就是 shares.Assessment / WriteReport，
// 而 accounts 用的正是这两个形状（DECISIONS Q475：同一张动作表、同一张账号表，
// 不造第二套读回核验）。
//
// 策略（CONTRACTS.md 第 5 节 R 列）：
//   - account.apply        R=是：改的是一个人能不能登录、用什么壳登录
//   - account.delete       R=是：删掉账号之后，用它的公钥与 sudo 规则一起失效
//   - ssh_key.apply        R=是：加一把公钥等于发一把钥匙
//   - ssh_key.delete       R=是：撤钥匙必须与发钥匙同级
//   - sudo_rule.apply      R=是：发 sudo 权限
//   - sudo_rule.delete     R=是：撤 sudo 权限
//
// 都不需要确认短语（短语只给初始化、格式化、删分区这类不可逆操作），都不可取消：
// 账号与授权的写入都是“一动手就要走到结果”的动作，中途停下只会留下一个说不出
// 状态的账号。
//
// 共享账号不在这里：account.apply 收到 type=share 会被判决层拒绝并指向
// share.account.apply（那条路的凭据在 Samba 的 passdb 里）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"upanel/internal/accounts"
	"upanel/internal/shares"
)

// accountsSource 是登记账号动作与查询所需的能力。
//
// 与 sharesSource 分开声明：只接了共享判决的模块不该能登记账号动作。
type accountsSource interface {
	Assess(ctx context.Context, action string, params any) (shares.Assessment, error)
	ExecuteWrite(ctx context.Context, req shares.WriteRequest) (shares.WriteReport, error)
	Accounts(ctx context.Context) ([]shares.AccountRecord, error)
	Account(ctx context.Context, id string) (shares.AccountRecord, bool, error)
	SSHKeys(ctx context.Context, accountID string) ([]accounts.SSHKeyRecord, error)
	// Capability 是 accounts.capability 的来源（P14.5）。
	//
	// 它与 sharesSource.Capability 判的不是同一件事：这里问的是 useradd / getent /
	// chpasswd / 两档壳 / visudo 在不在，那边问的是 Samba 的工具在不在。两个模块
	// 的工具集不相交，共用一个 status 会把“没装 Samba”误判成“不能管账号”。
	Capability(ctx context.Context) accounts.Capability
}

// RegisterAccountActions 登记六个账号动作。
func RegisterAccountActions(reg *Registry, src accountsSource) error {
	for _, a := range []Action{
		newManagedAccountApplyAction(src),
		newManagedAccountDeleteAction(src),
		newSSHKeyApplyAction(src),
		newSSHKeyDeleteAction(src),
		newSudoRuleApplyAction(src),
		newSudoRuleDeleteAction(src),
	} {
		if err := reg.RegisterAction(a); err != nil {
			return fmt.Errorf("代理登记 %s 失败: %w", a.Name, err)
		}
	}
	return nil
}

// newManagedAccountApplyAction 登记 account.apply。
func newManagedAccountApplyAction(src accountsSource) Action {
	return Action{
		Name: accounts.ActionAccountApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p accounts.AccountApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if p.Definition == nil {
				return nil, fmt.Errorf("%w: definition 不能为空", ErrParamsInvalid)
			}
			if p.AccountID != nil {
				id := strings.TrimSpace(*p.AccountID)
				p.AccountID = &id
				if !shares.ValidAccountID(id) {
					return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
						ErrParamsInvalid, id)
				}
			}
			v, err := optionalVersion(p.Version)
			if err != nil {
				return nil, err
			}
			p.Version = v
			// 只做归一化之前的形状收拾：TrimSpace 与“不许提交系统身份”。
			p.Definition.Name = strings.TrimSpace(p.Definition.Name)
			p.Definition.DisplayName = strings.TrimSpace(p.Definition.DisplayName)
			p.Definition.PasswordSecretRef = strings.TrimSpace(p.Definition.PasswordSecretRef)
			if p.Definition.Type == "" {
				// 不猜：缺类型就是缺类型。
				return nil, fmt.Errorf("%w: definition.type 必须是 regular 或 share", ErrParamsInvalid)
			}
			if p.Definition.ShellProfile == "" {
				p.Definition.ShellProfile = accounts.ProfileDisabled
			}
			return p, nil
		},
		Plan: sharesPlanner(src, accounts.ActionAccountApply),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, accounts.ActionAccountApply),
	}
}

// newManagedAccountDeleteAction 登记 account.delete。
func newManagedAccountDeleteAction(src accountsSource) Action {
	return Action{
		Name: accounts.ActionAccountDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p accounts.AccountRefParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.AccountID = strings.TrimSpace(p.AccountID)
			if !shares.ValidAccountID(p.AccountID) {
				return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.AccountID)
			}
			v, err := requireVersion(p.Version)
			if err != nil {
				return nil, err
			}
			p.Version = v
			return p, nil
		},
		Plan: sharesPlanner(src, accounts.ActionAccountDelete),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, accounts.ActionAccountDelete),
	}
}

// newSSHKeyApplyAction 登记 ssh_key.apply。
func newSSHKeyApplyAction(src accountsSource) Action {
	return Action{
		Name: accounts.ActionSSHKeyApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p accounts.SSHKeyApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.AccountID = strings.TrimSpace(p.AccountID)
			if !shares.ValidAccountID(p.AccountID) {
				return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.AccountID)
			}
			v, err := optionalVersion(p.Version)
			if err != nil {
				return nil, err
			}
			p.Version = v
			if p.KeyID != nil {
				id := strings.TrimSpace(*p.KeyID)
				p.KeyID = &id
				if id == "" {
					p.KeyID = nil
				} else if !accounts.ValidSSHKeyID(id) {
					return nil, fmt.Errorf("%w: key_id 必须是 ssk_ 加 32 位小写十六进制，收到 %q",
						ErrParamsInvalid, id)
				}
			}
			// 公钥只在这里 trim 掉行尾空白；其余校验（算法闭集、base64、线格式）
			// 属于判决层，它同时被 sshd 的真实行为约束着。
			p.PublicKey = strings.TrimSpace(p.PublicKey)
			if p.PublicKey == "" {
				return nil, fmt.Errorf("%w: public_key 不能为空", ErrParamsInvalid)
			}
			return p, nil
		},
		Plan: sharesPlanner(src, accounts.ActionSSHKeyApply),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, accounts.ActionSSHKeyApply),
	}
}

// newSSHKeyDeleteAction 登记 ssh_key.delete。
func newSSHKeyDeleteAction(src accountsSource) Action {
	return Action{
		Name: accounts.ActionSSHKeyDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p accounts.SSHKeyRefParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.AccountID = strings.TrimSpace(p.AccountID)
			if !shares.ValidAccountID(p.AccountID) {
				return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.AccountID)
			}
			p.KeyID = strings.TrimSpace(p.KeyID)
			if !accounts.ValidSSHKeyID(p.KeyID) {
				return nil, fmt.Errorf("%w: key_id 必须是 ssk_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.KeyID)
			}
			v, err := requireVersion(p.Version)
			if err != nil {
				return nil, err
			}
			p.Version = v
			return p, nil
		},
		Plan: sharesPlanner(src, accounts.ActionSSHKeyDelete),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, accounts.ActionSSHKeyDelete),
	}
}

// newSudoRuleApplyAction 登记 sudo_rule.apply。
func newSudoRuleApplyAction(src accountsSource) Action {
	return Action{
		Name: accounts.ActionSudoRuleApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p accounts.SudoRuleApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.AccountID = strings.TrimSpace(p.AccountID)
			if !shares.ValidAccountID(p.AccountID) {
				return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.AccountID)
			}
			p.TemplateID = strings.TrimSpace(p.TemplateID)
			if p.TemplateID == "" {
				return nil, fmt.Errorf("%w: template_id 不能为空", ErrParamsInvalid)
			}
			// 闭集在这里先挡一次：模板不认识的字符串连预检都不该走到。
			if _, err := accounts.LookupSudoTemplate(p.TemplateID); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			v, err := optionalVersion(p.Version)
			if err != nil {
				return nil, err
			}
			p.Version = v
			return p, nil
		},
		Plan: sharesPlanner(src, accounts.ActionSudoRuleApply),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, accounts.ActionSudoRuleApply),
	}
}

// newSudoRuleDeleteAction 登记 sudo_rule.delete。
func newSudoRuleDeleteAction(src accountsSource) Action {
	return Action{
		Name: accounts.ActionSudoRuleDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p accounts.SudoRuleRefParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.AccountID = strings.TrimSpace(p.AccountID)
			if !shares.ValidAccountID(p.AccountID) {
				return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.AccountID)
			}
			v, err := requireVersion(p.Version)
			if err != nil {
				return nil, err
			}
			p.Version = v
			return p, nil
		},
		Plan: sharesPlanner(src, accounts.ActionSudoRuleDelete),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, accounts.ActionSudoRuleDelete),
	}
}

// requireVersion 取出必填的 version 并校验形状。
//
// 三个删除动作都要求它：删账号、撤公钥、撤 sudo 规则改的都是“已经存在的东西”，
// 没有版本号就没有并发校验——两次点击之间别人换了壳或换了口径，第二次点击照样
// 会把新状态一起抹掉。CONTRACTS.md 第 5 节把 version 列进了这三个动作的参数里。
func requireVersion(version *string) (*string, error) {
	if version == nil {
		return nil, fmt.Errorf("%w: version 不能为空（它来自当前账号记录，"+
			"用来防止覆盖别人刚做的变更）", ErrParamsInvalid)
	}
	v := strings.TrimSpace(*version)
	if err := validateVersionToken(v); err != nil {
		return nil, err
	}
	return &v, nil
}

// optionalVersion 取出可选的 version 并校验形状。
//
// apply 类动作的目标可以还不存在（新建账号、新增第一把公钥、第一条 sudo 规则），
// 那时没有版本可给；给了就必须是合法形状。目标存在却没有版本的情况不会静默通过：
// 判决层把当前版本放进 StateVersion，执行前复核会以版本冲突拒绝。
func optionalVersion(version *string) (*string, error) {
	if version == nil {
		return nil, nil
	}
	v := strings.TrimSpace(*version)
	if err := validateVersionToken(v); err != nil {
		return nil, err
	}
	return &v, nil
}
