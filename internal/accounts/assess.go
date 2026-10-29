package accounts

// 六个账号动作的判决（P14.5）。
//
// 判决只读：它问系统、读登记表、读文件，一个字节都不改。改系统是 execute.go 的事。
// 判据全部来自 CONTRACTS.md 第 5 节与第 13 节，逐条对应：
//
//	account.apply     只管理面板创建的普通账号；不接受 uid/gid/groups/home
//	account.delete    检查共享引用；不删 home 数据
//	ssh_key.apply     只改 authorized_keys 里的托管区
//	ssh_key.delete    同上
//	sudo_rule.apply   固定模板 + visudo 通过 + 只碰带认领标记的文件
//	sudo_rule.delete  同上
//
// 一条纪律反复出现：**判决阶段不解析秘密明文**。预检是只读的，去 Resolve 一次密码
// 会让一次“能不能做”的询问碰到凭据；真实的明文只在执行器里出现一次。

import (
	"context"
	"errors"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"upanel/internal/shares"
	"upanel/internal/syscmd"
)

// Assess 判决一个账号动作。
func (e *Engine) Assess(ctx context.Context, action string, params any) (shares.Assessment, error) {
	switch action {
	case ActionAccountApply:
		p, ok := params.(AccountApplyParams)
		if !ok {
			return shares.Assessment{}, validationError("account.apply 的参数类型是 %T", params)
		}
		return e.assessAccountApply(ctx, p, lookupBudget{lookupTimeout})
	case ActionAccountDelete:
		p, ok := params.(AccountRefParams)
		if !ok {
			return shares.Assessment{}, validationError("account.delete 的参数类型是 %T", params)
		}
		return e.assessAccountDelete(ctx, p)
	case ActionSSHKeyApply:
		p, ok := params.(SSHKeyApplyParams)
		if !ok {
			return shares.Assessment{}, validationError("ssh_key.apply 的参数类型是 %T", params)
		}
		return e.assessSSHKeyApply(ctx, p)
	case ActionSSHKeyDelete:
		p, ok := params.(SSHKeyRefParams)
		if !ok {
			return shares.Assessment{}, validationError("ssh_key.delete 的参数类型是 %T", params)
		}
		return e.assessSSHKeyDelete(ctx, p)
	case ActionSudoRuleApply:
		p, ok := params.(SudoRuleApplyParams)
		if !ok {
			return shares.Assessment{}, validationError("sudo_rule.apply 的参数类型是 %T", params)
		}
		return e.assessSudoRuleApply(ctx, p)
	case ActionSudoRuleDelete:
		p, ok := params.(SudoRuleRefParams)
		if !ok {
			return shares.Assessment{}, validationError("sudo_rule.delete 的参数类型是 %T", params)
		}
		return e.assessSudoRuleDelete(ctx, p)
	default:
		return shares.Assessment{}, validationError("未登记的账号动作 %q", action)
	}
}

// assessAccountApply 判决 account.apply。
//
// budget 是这次判决里那次系统账号查询的总预算：预检按 HTTP 链路的预算给，
// 执行前复核按任务自己的预算给（见 commands.go 的两条常量）。
func (e *Engine) assessAccountApply(ctx context.Context, p AccountApplyParams, budget lookupBudget) (shares.Assessment, error) {
	out := shares.Assessment{Action: ActionAccountApply, NormalizedParams: p}
	if p.Definition == nil {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountTypeInvalid, Message: "definition 不能为空",
		})
		return out, nil
	}
	// 归一化：补默认值、拒绝闭集之外的值。所有“接受的形状”都在 Definition.Normalize 里，
	// 判决层与执行层读的是同一份判据。
	def, err := p.Definition.Normalize()
	if err != nil {
		out.Blockers = append(out.Blockers, defBlocker(err))
	}
	out.NormalizedParams = AccountApplyParams{AccountID: p.AccountID, Version: p.Version, Definition: &def}
	// 目标身份就是账号名：它在预检与执行之间不会变（改名在这里是被拒绝的），
	// 因此可以用作“还是那个对象”的指纹。
	out.DisplayName = def.Name
	out.TargetID = def.Name
	out.IdentityFingerprint = def.Name
	out.Locks = []string{"account:" + def.Name}

	// 共享账号不在这里执行（DECISIONS Q475）。
	if def.Type == TypeShare {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountTypeInvalid,
			Message: "account.apply 只处理 regular 账号；共享账号的凭据在 Samba 的 passdb 里，" +
				"请用 share.account.apply（两条写路径各有各的读回核验）",
		})
		return out, nil
	}

	nameValid := err == nil
	if p.AccountID == nil {
		if rec, found, err := e.opts.Store.GetAccountByName(ctx, def.Name); err != nil {
			return out, err
		} else if found {
			out.Blockers = append(out.Blockers, shares.Blocker{
				Code: NoticeAccountNameTaken,
				Message: "已有受管理账号 " + def.Name + "（" + rec.ID + "）：" +
					"要改它请用 account_id 更新，不要新建同名账号",
			})
		} else {
			// 新建也要绑定状态版本。可绑定的事实就是“此刻登记表里还没有这个名字”。
			out.StateVersion = absentStateVersion(def.Name)
		}
		// 系统上已有同名账号但不是受管理的 → 拒绝静默接管。名字本身不合法时跳过这次
		// 查询：判决已经是“不许”，而它可能把预检拖成一次超时（DECISIONS Q223）。
		if nameValid {
			found, timedOut, err := e.systemUserExists(ctx, def.Name, budget)
			if err != nil {
				return out, err
			}
			switch {
			case timedOut:
				out.Warnings = append(out.Warnings, shares.Blocker{
					Code: NoticeAccountLookupTimeout,
					Message: "没能在 " + budget.String() + " 内确认系统里是否已有同名账号 " +
						def.Name + "（机器繁忙）：执行时会再核验一次，已存在的账号不会被接管",
				})
			case found:
				out.Blockers = append(out.Blockers, shares.Blocker{
					Code: NoticeAccountNotManaged,
					Message: "系统里已经有一个不是本面板创建的账号 " + def.Name +
						"：不会接管它（改名或先由管理员确认后手工处理）",
				})
			}
		}
		out.Impacts = append(out.Impacts, shares.Impact{
			Kind: shares.ImpactAccessChange,
			Description: "会在系统上创建一个新账号（uid 由系统分配）" +
				shellImpact(def),
		})
	} else {
		rec, ok := e.applyAccountUpdateTarget(ctx, *p.AccountID, &out)
		if !ok {
			return out, nil
		}
		out.StateVersion = rec.Version
		out.TargetID = rec.ID
		out.DisplayName = rec.Name
		if def.Name != rec.Name {
			out.Blockers = append(out.Blockers, shares.Blocker{
				Code: NoticeAccountNameInvalid,
				Message: "不支持修改账号名（" + rec.Name + " → " + def.Name + "）：" +
					"改名要连带改 home、authorized_keys 与 sudoers 里的名字，请删除后重建",
			})
		}
		// 系统读回一次：受管理记录说它存在，不等于系统上还在（有人手工删了）。
		entry, found, timedOut, err := e.systemUser(ctx, rec.Name, budget)
		if err != nil {
			return out, err
		}
		switch {
		case timedOut:
			out.Warnings = append(out.Warnings, shares.Blocker{
				Code: NoticeAccountLookupTimeout,
				Message: "没能在 " + budget.String() + " 内确认账号 " + rec.Name +
					" 在系统上的现状（机器繁忙）：执行时会再核验一次",
			})
		case !found:
			out.Blockers = append(out.Blockers, shares.Blocker{
				Code:    NoticeAccountMissing,
				Message: "受管理账号 " + rec.Name + " 在系统上已经不存在（可能被手工删除）；请删掉这条记录后重建",
			})
		case !UIDInRange(entry.UID):
			out.Blockers = append(out.Blockers, shares.Blocker{
				Code: NoticeAccountUIDOutOfRange,
				Message: "系统上 " + rec.Name + " 的 uid 是 " + itoa(entry.UID) +
					"，不在普通账号区间（" + itoa(MinManagedUID) + "—" + itoa(MaxManagedUID) + "）",
			})
		}
		if def.PasswordSecretRef == "" && !rec.HasPassword {
			out.Warnings = append(out.Warnings, shares.Blocker{
				Code:    NoticeAccountNoPassword,
				Message: "这个账号还没有口令：Ubuntu 的 sshd 走 PAM，口令被锁定的账号即使配了公钥也登不上",
			})
		}
		out.Impacts = append(out.Impacts, shares.Impact{
			Kind: shares.ImpactAccessChange,
			Description: "账号 " + rec.Name + " 的登录方式会被改成" + shellImpact(def) +
				passwordImpact(def.PasswordSecretRef),
		})
	}

	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// applyAccountUpdateTarget 取更新的目标记录，并把这一类阻塞塞进 out。
//
// 返回 ok=false 表示判决已经结束（记录不存在、类型不对），调用方直接返回。
func (e *Engine) applyAccountUpdateTarget(ctx context.Context, id string, out *shares.Assessment) (shares.AccountRecord, bool) {
	if !shares.ValidAccountID(id) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeAccountMissing,
			Message: "account_id 必须是 sac_ 加 32 位小写十六进制，收到 " + id,
		})
		return shares.AccountRecord{}, false
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, id)
	if err != nil {
		out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeAccountMissing, Message: err.Error()})
		return shares.AccountRecord{}, false
	}
	if !found {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountMissing, Message: "受管理账号 " + id + " 不存在",
		})
		return shares.AccountRecord{}, false
	}
	if rec.Type != string(TypeRegular) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountTypeInvalid,
			Message: "账号 " + rec.Name + " 的类型是 " + rec.Type +
				"，account.apply 只处理 regular 账号（共享账号走 share.account.apply）",
		})
		return shares.AccountRecord{}, false
	}
	return rec, true
}

// assessAccountDelete 判决 account.delete。
func (e *Engine) assessAccountDelete(ctx context.Context, p AccountRefParams) (shares.Assessment, error) {
	out := shares.Assessment{Action: ActionAccountDelete, NormalizedParams: p}
	if !shares.ValidAccountID(p.AccountID) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeAccountMissing,
			Message: "account_id 必须是 sac_ 加 32 位小写十六进制，收到 " + p.AccountID,
		})
		return out, nil
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return out, err
	}
	if !found {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountMissing, Message: "受管理账号 " + p.AccountID + " 不存在",
		})
		return out, nil
	}
	out.TargetID = rec.ID
	out.DisplayName = rec.Name
	out.IdentityFingerprint = rec.Name
	out.StateVersion = rec.Version
	out.Locks = []string{"account:" + rec.Name}
	if rec.Type != string(TypeRegular) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountTypeInvalid,
			Message: "账号 " + rec.Name + " 的类型是 " + rec.Type +
				"；共享账号请用 share.account.delete（要先删 passdb 里那条）",
		})
		return out, nil
	}

	// 共享引用：删掉一个还在授权名单里的账号会让那份共享对所有人失效，而失效发生在
	// 客户端下一次连接时，不是在删除这一秒。
	sharesUsing, err := e.accountsInShares(ctx, rec.Name)
	if err != nil {
		return out, err
	}
	if len(sharesUsing) > 0 {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountHasShares,
			Message: "账号 " + rec.Name + " 还在这些共享的授权名单里：" +
				strings.Join(sharesUsing, "、") + "；请先把它从那些共享里移除",
		})
	}

	keys, err := e.opts.Store.ListSSHKeys(ctx, rec.ID)
	if err != nil {
		return out, err
	}
	rule, hasRule, err := e.opts.Store.GetSudoRule(ctx, rec.ID)
	if err != nil {
		return out, err
	}
	if len(keys) > 0 {
		out.Impacts = append(out.Impacts, shares.Impact{
			Kind:        shares.ImpactAccessChange,
			Description: "这个账号的 " + itoa(len(keys)) + " 把公钥记录会一起删掉",
		})
	}
	if hasRule {
		out.Impacts = append(out.Impacts, shares.Impact{
			Kind:        shares.ImpactConfigChange,
			Description: "会删掉 sudoers.d 里属于它的规则文件（" + SudoersPath(e.opts.Paths.SudoersDir, rec.Name) + "）",
		})
		_ = rule
	}
	out.Impacts = append(out.Impacts, shares.Impact{
		Kind:        ImpactDataKept,
		Description: "系统账号会被删除，但它的 home 目录与里面的数据不会被删（面板不删数据）",
	})
	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// assessSSHKeyApply 判决 ssh_key.apply。
func (e *Engine) assessSSHKeyApply(ctx context.Context, p SSHKeyApplyParams) (shares.Assessment, error) {
	out := shares.Assessment{Action: ActionSSHKeyApply, NormalizedParams: p}
	rec, target, ok, err := e.keyTarget(ctx, p.AccountID, &out)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, nil
	}
	out.Locks = []string{"account:" + rec.Name, "authorized_keys:" + rec.Name}

	key, err := ParsePublicKey(p.PublicKey)
	if err != nil {
		out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeSSHKeyInvalid, Message: err.Error()})
		return out, nil
	}
	keys, err := e.opts.Store.ListSSHKeys(ctx, rec.ID)
	if err != nil {
		return out, err
	}
	replacing := ""
	if p.KeyID != nil {
		replacing = strings.TrimSpace(*p.KeyID)
		if !ValidSSHKeyID(replacing) {
			out.Blockers = append(out.Blockers, shares.Blocker{
				Code:    NoticeSSHKeyNotFound,
				Message: "key_id 必须是 ssk_ 加 32 位小写十六进制，收到 " + replacing,
			})
			return out, nil
		}
		if _, found, err := e.opts.Store.GetSSHKey(ctx, rec.ID, replacing); err != nil {
			return out, err
		} else if !found {
			out.Blockers = append(out.Blockers, shares.Blocker{
				Code:    NoticeSSHKeyNotFound,
				Message: "账号 " + rec.Name + " 上没有这把公钥（" + replacing + "）",
			})
			return out, nil
		}
	}
	// 同一把钥匙：换注释再加一次还是同一把，指纹相同。
	for _, k := range keys {
		if k.Fingerprint != key.Fingerprint {
			continue
		}
		if k.ID == replacing {
			// 自己替换自己、内容没变——幂等，不算重复。
			continue
		}
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeSSHKeyDuplicate,
			Message: "这把公钥已经在 " + rec.Name + " 上了（" + DescribeKey(key) +
				"）；同一条记录请用 key_id 替换，不要在账号上叠两把一样的钥匙",
		})
	}
	if replacing == "" && len(keys) >= MaxKeysPerAccount {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeSSHKeyTooMany,
			Message: "账号 " + rec.Name + " 已经有 " + itoa(len(keys)) + " 把公钥（上限 " +
				itoa(MaxKeysPerAccount) + "）：请先删掉不用的",
		})
	}

	// authorized_keys 里的托管区必须先读得懂。读不懂（标记只有一半）就既不敢整份
	// 重写、也不敢在半截里插一把钥匙——那种“猜着修”会吃掉文件里别人的行。
	if _, err := e.readAuthorizedKeys(target); err != nil {
		out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeSSHKeyBlockDamaged, Message: err.Error()})
		return out, nil
	}

	action := "新增"
	if replacing != "" {
		action = "替换"
	}
	out.Impacts = append(out.Impacts, shares.Impact{
		Kind: shares.ImpactAccessChange,
		Description: action + "一把公钥（" + DescribeKey(key) + "）：会写 " +
			target.keyPath + " 里面板的托管区，托管区外面的行一个字节都不动",
	})
	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// assessSSHKeyDelete 判决 ssh_key.delete。
func (e *Engine) assessSSHKeyDelete(ctx context.Context, p SSHKeyRefParams) (shares.Assessment, error) {
	out := shares.Assessment{Action: ActionSSHKeyDelete, NormalizedParams: p}
	rec, target, ok, err := e.keyTarget(ctx, p.AccountID, &out)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, nil
	}
	out.Locks = []string{"account:" + rec.Name, "authorized_keys:" + rec.Name}
	if !ValidSSHKeyID(p.KeyID) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeSSHKeyNotFound,
			Message: "key_id 必须是 ssk_ 加 32 位小写十六进制，收到 " + p.KeyID,
		})
		return out, nil
	}
	key, found, err := e.opts.Store.GetSSHKey(ctx, rec.ID, p.KeyID)
	if err != nil {
		return out, err
	}
	if !found {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeSSHKeyNotFound,
			Message: "账号 " + rec.Name + " 上没有这把公钥（" + p.KeyID + "）",
		})
		return out, nil
	}
	if _, err := e.readAuthorizedKeys(target); err != nil {
		out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeSSHKeyBlockDamaged, Message: err.Error()})
		return out, nil
	}
	pub, kerr := KeyFromRecord(key)
	desc := key.Fingerprint
	if kerr == nil {
		desc = DescribeKey(pub)
	}
	out.Impacts = append(out.Impacts, shares.Impact{
		Kind:        shares.ImpactAccessChange,
		Description: "会删掉一把公钥（" + desc + "）：用这把钥匙的会话还能继续，但下一次登录会被拒绝",
	})
	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// assessSudoRuleApply 判决 sudo_rule.apply。
func (e *Engine) assessSudoRuleApply(ctx context.Context, p SudoRuleApplyParams) (shares.Assessment, error) {
	out := shares.Assessment{Action: ActionSudoRuleApply, NormalizedParams: p}
	rec, ok, err := e.sudoTarget(ctx, p.AccountID, &out)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, nil
	}
	tmpl, err := LookupSudoTemplate(p.TemplateID)
	if err != nil {
		out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeSudoTemplateInvalid, Message: err.Error()})
		return out, nil
	}
	if !e.visudoAvailable() {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeSudoVisudoUnavailable,
			Message: "这台机器上没有 visudo（" + ProgramVisudo + "）：没有校验器就不写 sudoers，" +
				"一条写坏的规则会让整台机器的 sudo 都不可用",
		})
		return out, nil
	}
	sudoersPath := SudoersPath(e.opts.Paths.SudoersDir, rec.Name)
	// 同名文件不是面板写的 → 不覆盖、不删除。
	if current, err := e.opts.FS.ReadFile(sudoersPath); err == nil {
		if _, _, perr := ParseSudoers(current); perr != nil {
			out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeSudoFileDamaged, Message: perr.Error()})
			return out, nil
		}
	} else if !os.IsNotExist(err) {
		return out, err
	}
	// 已经有规则且模板相同 → 幂等：执行器会重算内容，内容一样就不重写。
	existing, hasRule, err := e.opts.Store.GetSudoRule(ctx, rec.ID)
	if err != nil {
		return out, err
	}
	if hasRule && existing.Template == tmpl.ID {
		out.Warnings = append(out.Warnings, shares.Blocker{
			Code:    NoticeSudoTemplateInvalid,
			Message: "账号 " + rec.Name + " 已经用的是同一个模板（" + tmpl.ID + "）：内容不变就不重写",
		})
	}
	out.Impacts = append(out.Impacts, shares.Impact{
		Kind: shares.ImpactAccessChange,
		Description: "账号 " + rec.Name + " 会获得 " + itoa(len(tmpl.Commands)) + " 条免密 sudo 权限（" +
			tmpl.DisplayName + "），写进 " + sudoersPath + " 并用 visudo 校验",
	})
	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// assessSudoRuleDelete 判决 sudo_rule.delete。
func (e *Engine) assessSudoRuleDelete(ctx context.Context, p SudoRuleRefParams) (shares.Assessment, error) {
	out := shares.Assessment{Action: ActionSudoRuleDelete, NormalizedParams: p}
	rec, ok, err := e.sudoTarget(ctx, p.AccountID, &out)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, nil
	}
	existing, hasRule, err := e.opts.Store.GetSudoRule(ctx, rec.ID)
	if err != nil {
		return out, err
	}
	if !hasRule {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeSudoRuleMissing,
			Message: "账号 " + rec.Name + " 上没有 sudo 规则",
		})
		return out, nil
	}
	sudoersPath := SudoersPath(e.opts.Paths.SudoersDir, rec.Name)
	if current, err := e.opts.FS.ReadFile(sudoersPath); err == nil {
		if _, _, perr := ParseSudoers(current); perr != nil {
			out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeSudoFileDamaged, Message: perr.Error()})
			return out, nil
		}
	} else if !os.IsNotExist(err) {
		return out, err
	}
	_ = existing
	out.Impacts = append(out.Impacts, shares.Impact{
		Kind:        shares.ImpactAccessChange,
		Description: "会删掉 " + sudoersPath + " 并 revoke 这个账号的免密 sudo 权限",
	})
	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// keyTarget 是“给哪个账号写公钥”的公共判决：记录存在、类型对、home 归面板管。
type keyTarget struct {
	name string
	home string
	// keyPath 是 authorized_keys 的绝对路径。
	keyPath string
}

// keyTarget 取公钥动作的目标，并把这一类阻塞塞进 out。
func (e *Engine) keyTarget(ctx context.Context, accountID string, out *shares.Assessment) (shares.AccountRecord, keyTarget, bool, error) {
	if !shares.ValidAccountID(accountID) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountMissing, Message: "account_id 必须是 sac_ 加 32 位小写十六进制，收到 " + accountID,
		})
		return shares.AccountRecord{}, keyTarget{}, false, nil
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, accountID)
	if err != nil {
		return shares.AccountRecord{}, keyTarget{}, false, err
	}
	if !found {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountMissing, Message: "受管理账号 " + accountID + " 不存在",
		})
		return shares.AccountRecord{}, keyTarget{}, false, nil
	}
	out.TargetID = rec.ID
	out.DisplayName = rec.Name
	out.IdentityFingerprint = rec.Name
	out.StateVersion = rec.Version
	if rec.Type != string(TypeRegular) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountTypeInvalid,
			Message: "账号 " + rec.Name + " 的类型是 " + rec.Type +
				"：共享账号只做 SMB 认证，不配公钥",
		})
		return shares.AccountRecord{}, keyTarget{}, false, nil
	}
	entry, exists, timedOut, err := e.systemUser(ctx, rec.Name, lookupBudget{lookupTimeout})
	if err != nil {
		return shares.AccountRecord{}, keyTarget{}, false, err
	}
	switch {
	case timedOut:
		out.Warnings = append(out.Warnings, shares.Blocker{
			Code:    NoticeAccountLookupTimeout,
			Message: "没能在预算内确认账号 " + rec.Name + " 在系统上的现状：执行时会再核验一次",
		})
	case !exists:
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeAccountMissing,
			Message: "受管理账号 " + rec.Name + " 在系统上已经不存在（可能被手工删除）",
		})
		return shares.AccountRecord{}, keyTarget{}, false, nil
	}
	target, perr := e.homeKeyPath(entry)
	if perr != nil {
		out.Blockers = append(out.Blockers, shares.Blocker{Code: NoticeSSHKeyHomeInvalid, Message: perr.Error()})
		return shares.AccountRecord{}, keyTarget{}, false, nil
	}
	target.name = rec.Name
	return rec, target, true, nil
}

// homeKeyPath 从读回来的 home 推出 authorized_keys 的位置，并检查它归面板管。
//
// 两道门：home 必须在 Paths.HomeBase 之下（否则面板会把一把钥匙放进 /etc 或别人的
// 家目录），且 home 与 .ssh 都不能是符号链接（否则“写 /home/alice/.ssh/x”实际写的
// 可能是 /etc/shadow 的同目录）。
func (e *Engine) homeKeyPath(entry PasswdEntry) (keyTarget, error) {
	home := path.Clean(entry.Home)
	if home == "/" || !strings.HasPrefix(home, e.opts.Paths.HomeBase+"/") {
		return keyTarget{}, &DefinitionError{
			Code: NoticeSSHKeyHomeInvalid,
			Message: "账号 " + entry.Name + " 的 home 是 " + entry.Home + "，不在 " +
				e.opts.Paths.HomeBase + " 之下：面板不会往那里写 authorized_keys",
		}
	}
	if fi, err := e.opts.FS.Stat(home); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return keyTarget{}, &DefinitionError{
				Code:    NoticeSSHKeyHomeInvalid,
				Message: "账号 " + entry.Name + " 的 home " + home + " 是符号链接，拒绝往里写公钥",
			}
		}
	} else if !os.IsNotExist(err) {
		return keyTarget{}, err
	}
	sshDir := path.Join(home, ".ssh")
	if fi, err := e.opts.FS.Stat(sshDir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return keyTarget{}, &DefinitionError{
				Code:    NoticeSSHKeyHomeInvalid,
				Message: sshDir + " 是符号链接，拒绝往里写公钥",
			}
		}
	} else if !os.IsNotExist(err) {
		return keyTarget{}, err
	}
	return keyTarget{name: entry.Name, home: home, keyPath: path.Join(sshDir, "authorized_keys")}, nil
}

// readAuthorizedKeys 读 authorized_keys 并解析托管区（文件不存在等于没有托管区）。
func (e *Engine) readAuthorizedKeys(target keyTarget) ([]PublicKey, error) {
	content, err := e.opts.FS.ReadFile(target.keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	_, _, keys, _, err := SplitManagedBlock(content)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// sudoTarget 是“给哪个账号加 sudo 规则”的公共判决。
func (e *Engine) sudoTarget(ctx context.Context, accountID string, out *shares.Assessment) (shares.AccountRecord, bool, error) {
	if !shares.ValidAccountID(accountID) {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountMissing, Message: "account_id 必须是 sac_ 加 32 位小写十六进制，收到 " + accountID,
		})
		return shares.AccountRecord{}, false, nil
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, accountID)
	if err != nil {
		return shares.AccountRecord{}, false, err
	}
	if !found {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeAccountMissing, Message: "受管理账号 " + accountID + " 不存在",
		})
		return shares.AccountRecord{}, false, nil
	}
	out.TargetID = rec.ID
	out.DisplayName = rec.Name
	out.IdentityFingerprint = rec.Name
	out.StateVersion = rec.Version
	out.Locks = []string{"account:" + rec.Name, SudoersLockName}
	switch {
	case rec.Type != string(TypeRegular):
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code: NoticeSudoAccountNotEligible,
			Message: "账号 " + rec.Name + " 是共享账号（" + rec.Type + "）：它连登录壳都没有，" +
				"给 sudo 规则没有意义",
		})
		return shares.AccountRecord{}, false, nil
	case rec.Disabled:
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeSudoAccountNotEligible,
			Message: "账号 " + rec.Name + " 已被禁用：规则会先写下去但用不了，先启用再授权",
		})
		return shares.AccountRecord{}, false, nil
	}
	// 面板身份永远拿不到 sudo 规则（PLAN.md 第 19 节）。保留名在定义层已经被拦住，
	// 这里再挡一次是因为登记表里可能有手工写进去的行。
	if reservedAccountNames[rec.Name] {
		out.Blockers = append(out.Blockers, shares.Blocker{
			Code:    NoticeSudoAccountNotEligible,
			Message: "账号 " + rec.Name + " 是系统/面板的保留名：面板不给面板自己发 sudo 权限",
		})
		return shares.AccountRecord{}, false, nil
	}
	return rec, true, nil
}

// systemUserExists 只回答“有没有”，不关心 uid。
func (e *Engine) systemUserExists(ctx context.Context, name string, budget lookupBudget) (bool, bool, error) {
	entry, found, timedOut, err := e.systemUser(ctx, name, budget)
	if err != nil || timedOut || !found {
		_ = entry
		return found, timedOut, err
	}
	return true, false, nil
}

// systemUser 用 getent passwd 问系统“这个账号现在是什么样”。
//
// 用 getent 而不是读 /etc/passwd：getent 走的是 nsswitch，与 sshd / sudo 解析用户名
// 时看的是同一份事实。读文件在接了 LDAP 的机器上会给出“不存在”，而 sshd 却能找到它。
//
// budget 是这次查询的总预算，由调用方按阶段给：预检给 HTTP 链路的预算，
// 执行前复核给任务自己的预算。超时**不是**错误——它是一个说出来就能继续的事实。
func (e *Engine) systemUser(ctx context.Context, name string, budget lookupBudget) (PasswdEntry, bool, bool, error) {
	res, err := e.run(ctx, specGetentPasswd(name), budget.duration())
	if err != nil {
		if errors.Is(err, syscmd.ErrTimeout) {
			return PasswdEntry{}, false, true, nil
		}
		// getent 用退出码 2 表示“这个键不在这个库里”——那不是错误，是“没有”。
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode == 2 {
			return PasswdEntry{}, false, false, nil
		}
		return PasswdEntry{}, false, false, commandFailed("查询系统账号 %s 失败：%v", name, err)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return PasswdEntry{}, false, false, nil
	}
	entry, perr := parsePasswdLine(res.Stdout)
	if perr != nil {
		return PasswdEntry{}, false, false, commandFailed("%v", perr)
	}
	return entry, true, false, nil
}

// passwordState 读 `passwd -S <name>` 的状态列。
func (e *Engine) passwordState(ctx context.Context, name string) (LockState, error) {
	res, err := e.run(ctx, specPasswdStatus(name), commandTimeout)
	if err != nil {
		return LockUnknown, commandFailed("读取账号 %s 的口令状态失败：%v", name, err)
	}
	return parseLockState(res.Stdout), nil
}

// accountsInShares 找出授权名单里还引用着这个账号的共享。
func (e *Engine) accountsInShares(ctx context.Context, name string) ([]string, error) {
	recs, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil, err
	}
	var using []string
	for _, rec := range recs {
		for _, user := range rec.Definition.SMBUsers {
			if user.AccountID == "" {
				continue
			}
			// 授权名单里存的是 account_id；名字要经过登记表才能对上。
			if rec2, found, err := e.opts.Store.GetAccount(ctx, user.AccountID); err != nil {
				return nil, err
			} else if found && rec2.Name == name {
				using = append(using, rec.Definition.Name)
				break
			}
		}
	}
	sort.Strings(using)
	return using, nil
}

// visudoAvailable 判断这台机器上有没有校验器。
//
// 只查文件在不在：visudo 自己就是校验器，它不需要“探针”。没有它就不写 sudoers——
// 这是“不接受任意 sudoers 文本”的一半，另一半是模板闭集。
func (e *Engine) visudoAvailable() bool {
	_, err := e.opts.FS.Stat(ProgramVisudo)
	return err == nil
}

// lookupBudget 把“查询预算”写成一个有名字的类型，避免两个 duration 参数串味。
type lookupBudget struct{ d time.Duration }

func (b lookupBudget) String() string          { return b.d.String() }
func (b lookupBudget) duration() time.Duration { return b.d }

// defBlocker 把一条定义校验错误映射成阻塞项。
func defBlocker(err error) shares.Blocker {
	var de *DefinitionError
	if errors.As(err, &de) {
		return shares.Blocker{Code: de.Code, Message: de.Message}
	}
	return shares.Blocker{Code: NoticeAccountTypeInvalid, Message: err.Error()}
}

// absentStateVersion 是“这个对象还不存在”的可绑定版本。
func absentStateVersion(name string) string { return "absent:" + name }

// shellImpact 描述这份定义会把壳改成什么（给影响清单用的半句话）。
func shellImpact(def Definition) string {
	if def.Disabled {
		return "，登录壳改成 nologin（禁用）"
	}
	if def.ShellProfile == ProfileRestricted {
		return "，登录壳改成 /bin/rbash（受限）"
	}
	return "，登录壳改成 /usr/sbin/nologin（不可登录）"
}

// passwordImpact 描述这次会不会换口令。
func passwordImpact(secretRef string) string {
	if secretRef == "" {
		return "；这次不换口令"
	}
	return "；口令会被设置成秘密通道里那一份（明文只经标准输入传递）"
}

// itoa 是一个不引入 strconv 噪音的小工具（只用于拼人的错误消息）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
