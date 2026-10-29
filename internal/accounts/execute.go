package accounts

// 六个账号动作的执行（P14.5）。
//
// 骨架与共享那半边一致：**执行前复核 → 动手 → 每一步之后读回 → 失败说清在哪一步、
// 并且只回滚这次自己造出来的东西**。
//
// 三条这条路径特有的纪律：
//
//  1. **面板身份与系统账号一个字节都不碰**。所有命令的目标名都来自登记表里的
//     rec.Name，它建出来时已经过 ValidateName；reservedAccountNames 在判决层与
//     执行层各挡一次。
//  2. **密码只经标准输入**。chpasswd 的 stdin 是它唯一到过的地方，用完立刻清零。
//  3. **sudoers 先校验候选，再 rename**。任何语法问题都发生在“正式文件还没被
//     碰过”的时候；visudo 失败时旧规则原样保留，这不是回滚，是根本没动。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"upanel/internal/shares"
	"upanel/internal/syscmd"
)

// ExecuteWrite 执行一个账号动作。
func (e *Engine) ExecuteWrite(ctx context.Context, req shares.WriteRequest) (shares.WriteReport, error) {
	switch req.Action {
	case ActionAccountApply:
		p, ok := req.Params.(AccountApplyParams)
		if !ok {
			return shares.WriteReport{}, validationError("account.apply 的参数类型是 %T", req.Params)
		}
		return e.executeAccountApply(ctx, p, req)
	case ActionAccountDelete:
		p, ok := req.Params.(AccountRefParams)
		if !ok {
			return shares.WriteReport{}, validationError("account.delete 的参数类型是 %T", req.Params)
		}
		return e.executeAccountDelete(ctx, p, req)
	case ActionSSHKeyApply:
		p, ok := req.Params.(SSHKeyApplyParams)
		if !ok {
			return shares.WriteReport{}, validationError("ssh_key.apply 的参数类型是 %T", req.Params)
		}
		return e.executeSSHKeyApply(ctx, p, req)
	case ActionSSHKeyDelete:
		p, ok := req.Params.(SSHKeyRefParams)
		if !ok {
			return shares.WriteReport{}, validationError("ssh_key.delete 的参数类型是 %T", req.Params)
		}
		return e.executeSSHKeyDelete(ctx, p, req)
	case ActionSudoRuleApply:
		p, ok := req.Params.(SudoRuleApplyParams)
		if !ok {
			return shares.WriteReport{}, validationError("sudo_rule.apply 的参数类型是 %T", req.Params)
		}
		return e.executeSudoRuleApply(ctx, p, req)
	case ActionSudoRuleDelete:
		p, ok := req.Params.(SudoRuleRefParams)
		if !ok {
			return shares.WriteReport{}, validationError("sudo_rule.delete 的参数类型是 %T", req.Params)
		}
		return e.executeSudoRuleDelete(ctx, p, req)
	default:
		return shares.WriteReport{}, validationError("未登记的账号动作 %q", req.Action)
	}
}

// executeAccountApply 执行 account.apply。
func (e *Engine) executeAccountApply(ctx context.Context, p AccountApplyParams, req shares.WriteRequest) (shares.WriteReport, error) {
	// 执行前复核用的是任务自己的预算，不是预检那条 3 秒：这里没有 HTTP 连接在等，
	// 而且它才是“改不改系统”的最后一道门（DECISIONS Q223）。
	a, err := e.assessAccountApply(ctx, p, lookupBudget{lookupExecTimeout})
	if err != nil {
		return shares.WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return shares.WriteReport{}, err
	}
	// 判决把归一化后的定义放进了 NormalizedParams——执行用的是那一份，不是原始的。
	np, _ := a.NormalizedParams.(AccountApplyParams)
	if np.Definition == nil {
		return shares.WriteReport{}, validationError("definition 不能为空")
	}
	def := *np.Definition
	if p.AccountID == nil {
		return e.createRegularAccount(ctx, def, req)
	}
	return e.updateRegularAccount(ctx, *p.AccountID, def, req)
}

// createRegularAccount 建普通账号并逐项读回核验。
//
// 任何一步读回不符都回滚：只删掉这次自己建出来的那个账号。useradd 对已存在的名字
// 会以退出码 9 失败，那说明预检没拦住（预检查询超时只发告警）——这种情况照样拒绝
// 接管，因为“绝不静默接管既有账号”这条不变量不取决于预检有多快。
func (e *Engine) createRegularAccount(ctx context.Context, def Definition, req shares.WriteRequest) (shares.WriteReport, error) {
	var secret []byte
	if def.PasswordSecretRef != "" {
		resolved, err := e.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
		if err != nil {
			return shares.WriteReport{}, validationError(
				"秘密引用 %s 无法解析：%v（密码可能已经过期，请重新上传）", def.PasswordSecretRef, err)
		}
		secret = resolved
		defer wipe(secret)
	}

	shell := def.ShellProfile.Shell()
	if def.Disabled {
		shell = ShellDisabled
	}
	if req.Progress != nil {
		req.Progress("account_create", map[string]any{"name": def.Name})
	}
	if _, err := e.run(ctx, specUseradd(def.Name, shell), commandTimeout); err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode == 9 {
			return shares.WriteReport{}, dependencyBlocked(
				"系统里已经有账号 %s（useradd 退出码 9：名字已被占用），拒绝接管它（%s）",
				def.Name, NoticeAccountNotManaged)
		}
		return shares.WriteReport{}, commandFailed("创建系统账号 %s 失败：%v", def.Name, err)
	}
	rollback := func(reason string) (shares.WriteReport, error) {
		_, rbErr := e.run(ctx, specUserdel(def.Name), commandTimeout)
		return shares.WriteReport{}, withRollbackNote(
			commandFailed("%s；已回滚（删掉刚创建的系统账号 %s）", reason, def.Name), rbErr)
	}

	// 读回一：getent passwd 里的样子必须是我们要的样子。
	entry, ok, timedOut, err := e.systemUser(ctx, def.Name, lookupBudget{lookupExecTimeout})
	if err != nil {
		return rollback(fmt.Sprintf("读取系统账号失败：%v", err))
	}
	if timedOut {
		return rollback("账号创建后没能在预算内读回它")
	}
	if !ok {
		return rollback("账号创建后 getent passwd 找不到它")
	}
	if !UIDInRange(entry.UID) {
		return rollback(fmt.Sprintf("uid 是 %d，不在普通账号区间（%d—%d）",
			entry.UID, MinManagedUID, MaxManagedUID))
	}
	if entry.Shell != shell {
		return rollback(fmt.Sprintf("登录 shell 是 %q，期望 %q", entry.Shell, shell))
	}
	if !strings.HasPrefix(path.Clean(entry.Home), e.opts.Paths.HomeBase+"/") {
		return rollback(fmt.Sprintf("home 是 %q，不在 %s 之下：面板不会在这个位置管公钥",
			entry.Home, e.opts.Paths.HomeBase))
	}

	// 读回二：没有口令时必须确认它是锁着的，有口令时必须确认它是可用的。
	lock, err := e.passwordState(ctx, def.Name)
	if err != nil {
		return rollback(err.Error())
	}
	hasPassword := false
	if def.PasswordSecretRef != "" {
		stdin := passwordStdin(def.Name, secret)
		if req.Progress != nil {
			req.Progress("account_password", map[string]any{"name": def.Name})
		}
		_, runErr := e.run(ctx, specChpasswd(stdin), commandTimeout)
		wipe(stdin)
		if runErr != nil {
			return rollback(fmt.Sprintf("设置口令失败：%v", runErr))
		}
		lock, err = e.passwordState(ctx, def.Name)
		if err != nil {
			return rollback(err.Error())
		}
		if lock != LockUsable {
			return rollback(fmt.Sprintf("设了口令之后 `passwd -S` 报 %q，期望 P（可用）", lock))
		}
		hasPassword = true
	} else if lock != LockLocked && lock != LockNone {
		return rollback(fmt.Sprintf("没设口令的账号其口令状态是 %q，期望锁定（L）", lock))
	}

	now := e.now()
	id, err := shares.NewAccountID()
	if err != nil {
		return rollback(err.Error())
	}
	rec := shares.AccountRecord{
		ID:           id,
		Name:         def.Name,
		DisplayName:  def.DisplayName,
		Type:         string(TypeRegular),
		Managed:      true,
		Disabled:     def.Disabled,
		HasPassword:  hasPassword,
		UID:          entry.UID,
		GID:          entry.GID,
		Shell:        entry.Shell,
		Home:         entry.Home,
		ShellProfile: string(def.ShellProfile),
		SecretRef:    def.PasswordSecretRef,
		Version:      "1",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	rec.LastOperationID = req.OperationID
	if err := e.opts.Store.SaveAccount(ctx, rec); err != nil {
		return rollback(fmt.Sprintf("保存账号记录失败：%v", err))
	}
	// 把临时秘密转成持久秘密。失败不影响系统状态，但要在结果里说清楚，
	// 因为那条秘密 10 分钟后会自己消失。
	pinned, pinNote := true, ""
	if def.PasswordSecretRef != "" && e.opts.Secrets != nil {
		if err := e.opts.Secrets.Reference(ctx, def.PasswordSecretRef, "account:"+rec.ID); err != nil {
			pinned, pinNote = false, err.Error()
		}
	}

	observed := map[string]any{
		"account_id":       rec.ID,
		"name":             rec.Name,
		"uid":              rec.UID,
		"gid":              rec.GID,
		"shell":            rec.Shell,
		"home":             rec.Home,
		"disabled":         rec.Disabled,
		"shell_profile":    rec.ShellProfile,
		"has_password":     rec.HasPassword,
		"password_state":   string(lock),
		"secret_persisted": pinned,
	}
	if pinNote != "" {
		observed["secret_note"] = pinNote
	}
	return shares.WriteReport{
		Outcome:  shares.OutcomeVerified,
		Observed: observed,
		Data: map[string]any{
			"account_id": rec.ID,
			"name":       rec.Name,
			"version":    rec.Version,
		},
	}, nil
}

// updateRegularAccount 改壳/改口令/改展示名。
//
// 禁用与启用都是“换一个壳”：禁用 → nologin，启用 → profile 那一档。不用
// `usermod -L/-U` 是因为 -U 对从未设过口令的账号会写一个**空口令**，而空口令
// 是“谁都能 su 进去”的同义词；换壳的结果还能用 getent 一眼读回来。
func (e *Engine) updateRegularAccount(ctx context.Context, accountID string, def Definition, req shares.WriteRequest) (shares.WriteReport, error) {
	rec, found, err := e.opts.Store.GetAccount(ctx, accountID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if !found {
		return shares.WriteReport{}, notFound("受管理账号 %s 已不存在", accountID)
	}
	entry, ok, timedOut, err := e.systemUser(ctx, rec.Name, lookupBudget{lookupExecTimeout})
	if err != nil {
		return shares.WriteReport{}, commandFailed("读取系统账号 %s 失败：%v", rec.Name, err)
	}
	if timedOut {
		return shares.WriteReport{}, commandFailed("没能在预算内读回系统账号 %s，未做任何修改", rec.Name)
	}
	if !ok {
		return shares.WriteReport{}, dependencyBlocked(
			"受管理账号 %s 在系统上已经不存在（可能被手工删除）；请删掉这条记录后重建", rec.Name)
	}

	shellChanged, passwordChanged := false, false
	desiredShell := def.ShellProfile.Shell()
	if def.Disabled {
		desiredShell = ShellDisabled
	}
	// 口令状态与壳分开处理：改壳不动口令，改口令不动壳。
	if entry.Shell != desiredShell {
		if req.Progress != nil {
			req.Progress("account_shell", map[string]any{"name": rec.Name, "shell": desiredShell})
		}
		if _, err := e.run(ctx, specUsermodShell(rec.Name, desiredShell), commandTimeout); err != nil {
			return shares.WriteReport{}, commandFailed("修改 %s 的登录壳失败：%v", rec.Name, err)
		}
		entry, ok, timedOut, err = e.systemUser(ctx, rec.Name, lookupBudget{lookupExecTimeout})
		if err != nil || timedOut || !ok {
			return shares.WriteReport{}, commandFailed("改壳之后读不回账号 %s", rec.Name)
		}
		if entry.Shell != desiredShell {
			return shares.WriteReport{}, commandFailed(
				"改壳之后读回来的是 %q，期望 %q", entry.Shell, desiredShell)
		}
		shellChanged = true
	}

	if def.PasswordSecretRef != "" {
		if e.opts.Secrets == nil {
			return shares.WriteReport{}, dependencyBlocked("秘密通道不可用（没有接秘密存储）")
		}
		secret, err := e.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
		if err != nil {
			return shares.WriteReport{}, validationError("秘密引用 %s 无法解析：%v", def.PasswordSecretRef, err)
		}
		stdin := passwordStdin(rec.Name, secret)
		wipe(secret)
		if req.Progress != nil {
			req.Progress("account_password", map[string]any{"name": rec.Name})
		}
		_, runErr := e.run(ctx, specChpasswd(stdin), commandTimeout)
		wipe(stdin)
		if runErr != nil {
			return shares.WriteReport{}, commandFailed("重设 %s 的口令失败：%v", rec.Name, runErr)
		}
		lock, err := e.passwordState(ctx, rec.Name)
		if err != nil {
			return shares.WriteReport{}, err
		}
		if lock != LockUsable {
			return shares.WriteReport{}, commandFailed(
				"重设口令之后 `passwd -S` 报 %q，期望 P（可用）", lock)
		}
		passwordChanged = true
		rec.HasPassword = true
		rec.SecretRef = def.PasswordSecretRef
	}

	rec.DisplayName = def.DisplayName
	rec.ShellProfile = string(def.ShellProfile)
	rec.Disabled = def.Disabled
	rec.Shell = entry.Shell
	rec.UID = entry.UID
	rec.GID = entry.GID
	rec.Home = entry.Home
	rec.Version = nextVersion(rec.Version)
	rec.LastOperationID = req.OperationID
	rec.UpdatedAt = e.now()
	if err := e.opts.Store.SaveAccount(ctx, rec); err != nil {
		return shares.WriteReport{}, err
	}
	if def.PasswordSecretRef != "" && e.opts.Secrets != nil {
		// 引用失败不影响系统状态：口令已经生效，只是那条临时秘密会过期。
		_ = e.opts.Secrets.Reference(ctx, def.PasswordSecretRef, "account:"+rec.ID)
	}

	return shares.WriteReport{
		Outcome: shares.OutcomeVerified,
		Observed: map[string]any{
			"account_id":       rec.ID,
			"name":             rec.Name,
			"version":          rec.Version,
			"shell":            rec.Shell,
			"shell_profile":    rec.ShellProfile,
			"disabled":         rec.Disabled,
			"shell_changed":    shellChanged,
			"password_changed": passwordChanged,
			"has_password":     rec.HasPassword,
		},
		Data: map[string]any{"account_id": rec.ID, "version": rec.Version},
	}, nil
}

// executeAccountDelete 执行 account.delete。
func (e *Engine) executeAccountDelete(ctx context.Context, p AccountRefParams, req shares.WriteRequest) (shares.WriteReport, error) {
	a, err := e.assessAccountDelete(ctx, p)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return shares.WriteReport{}, err
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if !found {
		return shares.WriteReport{}, notFound("受管理账号 %s 已不存在", p.AccountID)
	}

	// 顺序：先删 sudoers 里的规则文件，再删系统账号，最后删记录。
	// 反过来的话，中途失败会在系统上留下一个“账号没了但还能 sudo”的规则。
	ruleRemoved := false
	if _, hasRule, err := e.opts.Store.GetSudoRule(ctx, rec.ID); err != nil {
		return shares.WriteReport{}, err
	} else if hasRule {
		sudoersPath := SudoersPath(e.opts.Paths.SudoersDir, rec.Name)
		if current, err := e.opts.FS.ReadFile(sudoersPath); err == nil {
			if _, _, perr := ParseSudoers(current); perr != nil {
				return shares.WriteReport{}, dependencyBlocked("%v", perr)
			}
			if req.Progress != nil {
				req.Progress("sudo_rule_remove", map[string]any{"path": sudoersPath})
			}
			if err := e.opts.FS.Remove(sudoersPath); err != nil {
				return shares.WriteReport{}, commandFailed("删除 %s 失败：%v", sudoersPath, err)
			}
			if _, err := e.opts.FS.Stat(sudoersPath); err == nil {
				return shares.WriteReport{}, commandFailed("删除后 %s 仍然存在", sudoersPath)
			}
			ruleRemoved = true
		}
		if err := e.opts.Store.DeleteSudoRule(ctx, rec.ID); err != nil {
			return shares.WriteReport{}, err
		}
	}

	// 系统账号可能已经被手工删掉了：那种情况不算失败，记录照样要清掉。
	_, exists, timedOut, err := e.systemUser(ctx, rec.Name, lookupBudget{lookupExecTimeout})
	if err != nil {
		return shares.WriteReport{}, err
	}
	if timedOut {
		return shares.WriteReport{}, commandFailed("没能在预算内确认账号 %s 在系统上的现状，未做任何修改", rec.Name)
	}
	linuxRemoved, linuxWasAbsent := false, false
	if exists {
		if req.Progress != nil {
			req.Progress("account_delete", map[string]any{"name": rec.Name})
		}
		// 不加 -r：home 里的数据不归面板删。
		if _, err := e.run(ctx, specUserdel(rec.Name), commandTimeout); err != nil {
			return shares.WriteReport{}, commandFailed("删除系统账号 %s 失败：%v", rec.Name, err)
		}
		_, still, stillTimedOut, err := e.systemUser(ctx, rec.Name, lookupBudget{lookupExecTimeout})
		if err != nil {
			return shares.WriteReport{}, commandFailed("删除后读取系统账号失败：%v", err)
		}
		if stillTimedOut {
			return shares.WriteReport{}, commandFailed("删除后没能在预算内确认账号 %s 是否还在", rec.Name)
		}
		if still {
			return shares.WriteReport{}, commandFailed("删除后系统里仍然有账号 %s", rec.Name)
		}
		linuxRemoved = true
	} else {
		linuxWasAbsent = true
	}

	homeKept := false
	if _, err := e.opts.FS.Stat(rec.Home); err == nil {
		homeKept = true
	}
	if err := e.opts.Store.DeleteAccount(ctx, rec.ID); err != nil {
		return shares.WriteReport{}, err
	}

	return shares.WriteReport{
		Outcome: shares.OutcomeVerified,
		Observed: map[string]any{
			"account_id":         rec.ID,
			"name":               rec.Name,
			"linux_removed":      linuxRemoved,
			"linux_was_absent":   linuxWasAbsent,
			"sudo_rule_removed":  ruleRemoved,
			"home":               rec.Home,
			"home_data_kept":     true,
			"home_still_present": homeKept,
		},
		Data: map[string]any{"account_id": rec.ID},
	}, nil
}

// executeSSHKeyApply 执行 ssh_key.apply。
func (e *Engine) executeSSHKeyApply(ctx context.Context, p SSHKeyApplyParams, req shares.WriteRequest) (shares.WriteReport, error) {
	a, err := e.assessSSHKeyApply(ctx, p)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return shares.WriteReport{}, err
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if !found {
		return shares.WriteReport{}, notFound("受管理账号 %s 已不存在", p.AccountID)
	}
	entry, ok, timedOut, err := e.systemUser(ctx, rec.Name, lookupBudget{lookupExecTimeout})
	if err != nil || timedOut || !ok {
		return shares.WriteReport{}, commandFailed("执行前读不到系统账号 %s", rec.Name)
	}
	target, perr := e.homeKeyPath(entry)
	if perr != nil {
		return shares.WriteReport{}, dependencyBlocked("%v", perr)
	}
	key, err := ParsePublicKey(p.PublicKey)
	if err != nil {
		return shares.WriteReport{}, validationError("%v", err)
	}

	keys, err := e.opts.Store.ListSSHKeys(ctx, rec.ID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	sortKeys(keys)
	replacing := ""
	if p.KeyID != nil {
		replacing = strings.TrimSpace(*p.KeyID)
	}
	// 目标托管区：替换时保持原位，新增时排在最后。
	desired := make([]PublicKey, 0, len(keys)+1)
	placed := false
	for _, k := range keys {
		if k.ID == replacing {
			desired = append(desired, key)
			placed = true
			continue
		}
		pk, err := KeyFromRecord(k)
		if err != nil {
			return shares.WriteReport{}, commandFailed("%v", err)
		}
		desired = append(desired, pk)
	}
	if !placed {
		desired = append(desired, key)
	}

	report, err := e.writeManagedKeys(ctx, target, desired)
	if err != nil {
		return shares.WriteReport{}, err
	}

	now := e.now()
	recKey := SSHKeyRecord{
		AccountID:   rec.ID,
		Type:        key.Type,
		Blob:        key.BlobText(),
		Comment:     key.Comment,
		Fingerprint: key.Fingerprint,
		CreatedAt:   now,
	}
	if replacing != "" {
		existing, found, err := e.opts.Store.GetSSHKey(ctx, rec.ID, replacing)
		if err != nil {
			return shares.WriteReport{}, err
		}
		if !found {
			return shares.WriteReport{}, notFound("认证通过之后那条公钥记录却不见了（%s）", replacing)
		}
		recKey.ID = existing.ID
		recKey.CreatedAt = existing.CreatedAt
	} else {
		id, err := NewSSHKeyID()
		if err != nil {
			return shares.WriteReport{}, err
		}
		recKey.ID = id
	}
	if err := e.opts.Store.SaveSSHKey(ctx, recKey); err != nil {
		// 文件已经写好了。落库失败要回滚文件，否则“文件里有、登记表里没有”
		// 的公钥永远不会被任何一页看到，也删不掉。
		rbErr := e.restoreKeys(target, report.Before)
		return shares.WriteReport{}, withRollbackNote(
			commandFailed("保存公钥记录失败：%v", err), rbErr)
	}

	return shares.WriteReport{
		Outcome: shares.OutcomeVerified,
		Observed: map[string]any{
			"account_id":   rec.ID,
			"name":         rec.Name,
			"key_id":       recKey.ID,
			"fingerprint":  recKey.Fingerprint,
			"key_type":     recKey.Type,
			"path":         target.keyPath,
			"managed_keys": len(desired),
			"replaced":     replacing != "",
			"file_hash":    report.Hash,
		},
		Data: map[string]any{
			"account_id":  rec.ID,
			"key_id":      recKey.ID,
			"fingerprint": recKey.Fingerprint,
		},
	}, nil
}

// executeSSHKeyDelete 执行 ssh_key.delete。
func (e *Engine) executeSSHKeyDelete(ctx context.Context, p SSHKeyRefParams, req shares.WriteRequest) (shares.WriteReport, error) {
	a, err := e.assessSSHKeyDelete(ctx, p)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return shares.WriteReport{}, err
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if !found {
		return shares.WriteReport{}, notFound("受管理账号 %s 已不存在", p.AccountID)
	}
	entry, ok, timedOut, err := e.systemUser(ctx, rec.Name, lookupBudget{lookupExecTimeout})
	if err != nil || timedOut || !ok {
		return shares.WriteReport{}, commandFailed("执行前读不到系统账号 %s", rec.Name)
	}
	target, perr := e.homeKeyPath(entry)
	if perr != nil {
		return shares.WriteReport{}, dependencyBlocked("%v", perr)
	}
	keys, err := e.opts.Store.ListSSHKeys(ctx, rec.ID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	sortKeys(keys)
	desired := make([]PublicKey, 0, len(keys))
	removed := SSHKeyRecord{}
	for _, k := range keys {
		if k.ID == p.KeyID {
			removed = k
			continue
		}
		pk, err := KeyFromRecord(k)
		if err != nil {
			return shares.WriteReport{}, commandFailed("%v", err)
		}
		desired = append(desired, pk)
	}
	if removed.ID == "" {
		return shares.WriteReport{}, notFound("账号 %s 上没有这把公钥（%s）", rec.Name, p.KeyID)
	}
	report, err := e.writeManagedKeys(ctx, target, desired)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if err := e.opts.Store.DeleteSSHKey(ctx, rec.ID, p.KeyID); err != nil {
		rbErr := e.restoreKeys(target, report.Before)
		return shares.WriteReport{}, withRollbackNote(
			commandFailed("删除公钥记录失败：%v", err), rbErr)
	}
	return shares.WriteReport{
		Outcome: shares.OutcomeVerified,
		Observed: map[string]any{
			"account_id":   rec.ID,
			"name":         rec.Name,
			"key_id":       removed.ID,
			"fingerprint":  removed.Fingerprint,
			"path":         target.keyPath,
			"managed_keys": len(desired),
			"file_hash":    report.Hash,
		},
		Data: map[string]any{"account_id": rec.ID, "key_id": removed.ID},
	}, nil
}

// keyWriteReport 是一次 authorized_keys 写入的现场。
type keyWriteReport struct {
	// Before 是写之前的整份内容（文件不存在时是 nil）。回滚靠它。
	Before []byte
	// Hash 是写完之后文件的 sha256。
	Hash string
}

// writeManagedKeys 把托管区改成 keys，并逐项读回核验。
//
// 顺序：读现状 → 渲染 → 写候选（0600，属主是账号本人）→ rename → fsync 目录 →
// 再读一遍逐把比对。托管区外面的行一个字节都不动——这一点由 ApplyKeys 保证，
// 由写完之后的重读证明。
func (e *Engine) writeManagedKeys(ctx context.Context, target keyTarget, keys []PublicKey) (keyWriteReport, error) {
	before, err := e.opts.FS.ReadFile(target.keyPath)
	if err != nil && !os.IsNotExist(err) {
		return keyWriteReport{}, commandFailed("读取 %s 失败：%v", target.keyPath, err)
	}
	if os.IsNotExist(err) {
		before = nil
	}
	desired, err := ApplyKeys(before, keys)
	if err != nil {
		return keyWriteReport{}, validationError("%v", err)
	}

	entry, ok, timedOut, err := e.systemUser(ctx, target.name, lookupBudget{lookupExecTimeout})
	if err != nil || timedOut || !ok {
		return keyWriteReport{}, commandFailed("写公钥前读不到系统账号 %s", target.name)
	}
	sshDir := path.Dir(target.keyPath)
	if err := e.opts.FS.MkdirAll(sshDir, 0o700); err != nil {
		return keyWriteReport{}, commandFailed("创建 %s 失败：%v", sshDir, err)
	}
	// .ssh 必须是账号本人的，0700：root 拥有的 .ssh 会让 sshd 拒绝使用里面的钥匙
	// （StrictModes）。
	if err := e.opts.FS.Chown(sshDir, entry.UID, entry.GID); err != nil {
		return keyWriteReport{}, commandFailed("设置 %s 的属主失败：%v", sshDir, err)
	}
	tmp := target.keyPath + ".upanel.tmp"
	if err := e.opts.FS.WriteFileSync(tmp, desired, 0o600); err != nil {
		return keyWriteReport{}, commandFailed("写候选文件 %s 失败：%v", tmp, err)
	}
	if err := e.opts.FS.Chown(tmp, entry.UID, entry.GID); err != nil {
		_ = e.opts.FS.Remove(tmp)
		return keyWriteReport{}, commandFailed("设置候选文件的属主失败：%v", err)
	}
	if err := e.opts.FS.Rename(tmp, target.keyPath); err != nil {
		_ = e.opts.FS.Remove(tmp)
		return keyWriteReport{}, commandFailed("替换 %s 失败：%v", target.keyPath, err)
	}
	if err := e.opts.FS.FsyncDir(sshDir); err != nil {
		return keyWriteReport{}, commandFailed("落盘 %s 失败：%v", sshDir, err)
	}

	// 读回：整份重读，托管区里的指纹集合必须与我们写进去的一模一样。
	after, err := e.opts.FS.ReadFile(target.keyPath)
	if err != nil {
		return keyWriteReport{Before: before}, commandFailed("写完读回 %s 失败：%v", target.keyPath, err)
	}
	_, _, got, found, perr := SplitManagedBlock(after)
	if perr != nil {
		return keyWriteReport{Before: before}, commandFailed("写完读回时托管区解析失败：%v", perr)
	}
	if len(keys) > 0 && !found {
		return keyWriteReport{Before: before}, commandFailed("写完读回时托管区不见了")
	}
	if !sameKeySet(got, keys) {
		return keyWriteReport{Before: before}, commandFailed(
			"写完读回的公钥集合与期望不符（期望 %d 把，读回 %d 把）", len(keys), len(got))
	}
	// 托管区外面的内容必须一字不差。
	if before != nil && found {
		if !outerContentKept(before, after) {
			return keyWriteReport{Before: before}, commandFailed(
				"%s 里托管区之外的内容发生了变化：面板只会重写托管区", target.keyPath)
		}
	}
	return keyWriteReport{Before: before, Hash: hashBytes(after)}, nil
}

// restoreKeys 把 authorized_keys 恢复到写之前的样子。
//
// 文件原先不存在时删掉它——留一个空文件会让 sshd 与人都以为这个账号配过钥匙。
func (e *Engine) restoreKeys(target keyTarget, before []byte) error {
	if before == nil {
		if err := e.opts.FS.Remove(target.keyPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return e.opts.FS.FsyncDir(path.Dir(target.keyPath))
	}
	tmp := target.keyPath + ".upanel.rb"
	if err := e.opts.FS.WriteFileSync(tmp, before, 0o600); err != nil {
		return err
	}
	entry, ok, _, err := e.systemUser(context.Background(), target.name, lookupBudget{lookupExecTimeout})
	if err == nil && ok {
		_ = e.opts.FS.Chown(tmp, entry.UID, entry.GID)
	}
	if err := e.opts.FS.Rename(tmp, target.keyPath); err != nil {
		_ = e.opts.FS.Remove(tmp)
		return err
	}
	return e.opts.FS.FsyncDir(path.Dir(target.keyPath))
}

// sameKeySet 比较两组公钥（按指纹，顺序无关）。
func sameKeySet(a, b []PublicKey) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, k := range a {
		seen[k.Fingerprint]++
	}
	for _, k := range b {
		seen[k.Fingerprint]--
		if seen[k.Fingerprint] < 0 {
			return false
		}
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// outerContentKept 判断托管区之外的内容有没有变。
func outerContentKept(before, after []byte) bool {
	b1, a1, _, _, err1 := SplitManagedBlock(before)
	b2, a2, _, _, err2 := SplitManagedBlock(after)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(b1) == string(b2) && string(a1) == string(a2)
}

// executeSudoRuleApply 执行 sudo_rule.apply。
//
// 顺序是这一阶段最要紧的那一段：**候选先过 visudo，通过了才 rename**。
// 于是“visudo 失败”这件事发生时，正式文件一个字节都没被碰过——旧规则天然保留，
// 不需要回滚（STAGES 第 16 节的验收项就是这条）。
func (e *Engine) executeSudoRuleApply(ctx context.Context, p SudoRuleApplyParams, req shares.WriteRequest) (shares.WriteReport, error) {
	a, err := e.assessSudoRuleApply(ctx, p)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return shares.WriteReport{}, err
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if !found {
		return shares.WriteReport{}, notFound("受管理账号 %s 已不存在", p.AccountID)
	}
	tmpl, err := LookupSudoTemplate(p.TemplateID)
	if err != nil {
		return shares.WriteReport{}, validationError("%v", err)
	}

	existing, hasRule, err := e.opts.Store.GetSudoRule(ctx, rec.ID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	ruleID := existing.ID
	if !hasRule {
		if ruleID, err = NewSudoRuleID(); err != nil {
			return shares.WriteReport{}, err
		}
	}
	content, hash, err := RenderSudoers(SudoRule{ID: ruleID, AccountName: rec.Name, Template: tmpl})
	if err != nil {
		return shares.WriteReport{}, validationError("%v", err)
	}

	sudoersPath := SudoersPath(e.opts.Paths.SudoersDir, rec.Name)
	before, err := e.opts.FS.ReadFile(sudoersPath)
	if err != nil && !os.IsNotExist(err) {
		return shares.WriteReport{}, commandFailed("读取 %s 失败：%v", sudoersPath, err)
	}
	if os.IsNotExist(err) {
		before = nil
	}

	const sudoersMode = 0o440
	dir := path.Dir(sudoersPath)
	if err := e.opts.FS.MkdirAll(dir, 0o755); err != nil {
		return shares.WriteReport{}, commandFailed("创建 %s 失败：%v", dir, err)
	}
	candidate := path.Join(dir, "."+path.Base(sudoersPath)+".upanel.tmp")
	if err := e.opts.FS.WriteFileSync(candidate, content, sudoersMode); err != nil {
		return shares.WriteReport{}, commandFailed("写候选文件 %s 失败：%v", candidate, err)
	}
	cleanup := func() { _ = e.opts.FS.Remove(candidate) }

	// 门禁一：候选自己合格吗。
	if res, err := e.run(ctx, specVisudoFile(candidate), commandTimeout); err != nil {
		cleanup()
		return shares.WriteReport{}, dependencyBlocked(
			"候选规则没有通过 visudo（%s）：%s；正式文件未被修改，旧规则原样保留（%s）",
			syscmdSummary(res, err), strings.TrimSpace(res.Stderr+res.Stdout), NoticeSudoVisudoFailed)
	}

	// 内容没变就不 rename：不改变文件的 mtime，也不给“这次改了什么”制造噪音。
	if before != nil && string(before) == string(content) {
		cleanup()
		if !hasRule {
			// 文件内容一样但登记表里没有记录（上一次落库失败留下的）——补上。
			if err := e.opts.Store.SaveSudoRule(ctx, SudoRuleRecord{
				ID: ruleID, AccountID: rec.ID, Template: tmpl.ID, Version: hash,
				CreatedAt: e.now(), UpdatedAt: e.now(),
			}); err != nil {
				return shares.WriteReport{}, err
			}
		}
		return shares.WriteReport{
			Outcome: shares.OutcomeVerified,
			Observed: map[string]any{
				"account_id": rec.ID, "name": rec.Name, "rule_id": ruleID,
				"template_id": tmpl.ID, "path": sudoersPath, "unchanged": true,
				"version": hash,
			},
			Data: map[string]any{"account_id": rec.ID, "rule_id": ruleID, "version": hash},
		}, nil
	}

	if err := e.opts.FS.Rename(candidate, sudoersPath); err != nil {
		cleanup()
		return shares.WriteReport{}, commandFailed("替换 %s 失败：%v", sudoersPath, err)
	}
	if err := e.opts.FS.FsyncDir(dir); err != nil {
		return shares.WriteReport{}, commandFailed("落盘 %s 失败：%v", dir, err)
	}

	restore := func(reason string) (shares.WriteReport, error) {
		rbErr := e.restoreSudoers(sudoersPath, before)
		return shares.WriteReport{}, withRollbackNote(commandFailed("%s", reason), rbErr)
	}

	// 门禁二：整份配置还合格吗。候选自己合格，不代表它拼进主配置之后也合格
	// （主配置里别的地方可能本来就坏了，此时往里加一条等于往坏配置上再加一条）。
	if res, err := e.run(ctx, specVisudoAll(), commandTimeout); err != nil {
		return restore(fmt.Sprintf(
			"写入之后整份 sudo 配置没有通过 visudo（%s）：%s；已恢复旧规则（%s）",
			syscmdSummary(res, err), strings.TrimSpace(res.Stderr+res.Stdout), NoticeSudoVisudoFailed))
	}

	// 读回一：文件里读出来的规则标识与模板必须是刚写的那一份。
	written, err := e.opts.FS.ReadFile(sudoersPath)
	if err != nil {
		return restore(fmt.Sprintf("写完读回 %s 失败：%v", sudoersPath, err))
	}
	gotRule, gotTemplate, perr := ParseSudoers(written)
	if perr != nil {
		return restore(fmt.Sprintf("写完读回时规则文件解析失败：%v", perr))
	}
	if gotRule != ruleID || gotTemplate != tmpl.ID {
		return restore(fmt.Sprintf("写完读回的规则是 %s/%s，期望 %s/%s",
			gotRule, gotTemplate, ruleID, tmpl.ID))
	}

	// 读回二：sudo 自己解析出来的授权必须与模板完全一致。
	// 文件内容对不等于 sudo 读出来的结果对——这一条才回答“他现在能跑什么”。
	res, err := e.run(ctx, specSudoListAs(rec.Name), commandTimeout)
	rules := parseSudoList(res.Stdout + res.Stderr)
	if err != nil {
		return restore(fmt.Sprintf("读回 `sudo -l -U %s` 失败：%v", rec.Name, err))
	}
	if !sudoRulesMatchTemplate(rules, tmpl) {
		return restore(fmt.Sprintf("`sudo -l -U %s` 读回来的授权与模板不一致（读回 %d 条，期望 %d 条）",
			rec.Name, len(rules), len(tmpl.Commands)))
	}

	newRec := SudoRuleRecord{
		ID: ruleID, AccountID: rec.ID, Template: tmpl.ID, Version: hash,
		CreatedAt: e.now(), UpdatedAt: e.now(),
	}
	if hasRule {
		newRec.CreatedAt = existing.CreatedAt
	}
	if err := e.opts.Store.SaveSudoRule(ctx, newRec); err != nil {
		return restore(fmt.Sprintf("保存 sudo 规则记录失败：%v", err))
	}

	return shares.WriteReport{
		Outcome: shares.OutcomeVerified,
		Observed: map[string]any{
			"account_id": rec.ID, "name": rec.Name, "rule_id": ruleID,
			"template_id": tmpl.ID, "path": sudoersPath, "version": hash,
			"unchanged": false, "commands": len(tmpl.Commands),
			"sudo_rules": len(rules),
		},
		Data: map[string]any{"account_id": rec.ID, "rule_id": ruleID, "version": hash},
	}, nil
}

// executeSudoRuleDelete 执行 sudo_rule.delete。
func (e *Engine) executeSudoRuleDelete(ctx context.Context, p SudoRuleRefParams, req shares.WriteRequest) (shares.WriteReport, error) {
	a, err := e.assessSudoRuleDelete(ctx, p)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return shares.WriteReport{}, err
	}
	rec, found, err := e.opts.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if !found {
		return shares.WriteReport{}, notFound("受管理账号 %s 已不存在", p.AccountID)
	}
	existing, hasRule, err := e.opts.Store.GetSudoRule(ctx, rec.ID)
	if err != nil {
		return shares.WriteReport{}, err
	}
	if !hasRule {
		return shares.WriteReport{}, notFound("账号 %s 上没有 sudo 规则", rec.Name)
	}
	sudoersPath := SudoersPath(e.opts.Paths.SudoersDir, rec.Name)
	before, err := e.opts.FS.ReadFile(sudoersPath)
	if err != nil && !os.IsNotExist(err) {
		return shares.WriteReport{}, commandFailed("读取 %s 失败：%v", sudoersPath, err)
	}
	if os.IsNotExist(err) {
		before = nil
	}
	if before != nil {
		if _, _, perr := ParseSudoers(before); perr != nil {
			return shares.WriteReport{}, dependencyBlocked("%v", perr)
		}
		if err := e.opts.FS.Remove(sudoersPath); err != nil {
			return shares.WriteReport{}, commandFailed("删除 %s 失败：%v", sudoersPath, err)
		}
		if err := e.opts.FS.FsyncDir(path.Dir(sudoersPath)); err != nil {
			return shares.WriteReport{}, commandFailed("落盘 %s 失败：%v", path.Dir(sudoersPath), err)
		}
	}
	if _, err := e.opts.FS.Stat(sudoersPath); err == nil {
		return shares.WriteReport{}, commandFailed("删除后 %s 仍然存在", sudoersPath)
	}
	// 删掉一个文件不该让整份配置坏掉，但它确实可能（文件里原本有别人 include 的
	// 依赖）。检查一次，坏了就把文件放回去。
	if res, err := e.run(ctx, specVisudoAll(), commandTimeout); err != nil {
		rbErr := e.restoreSudoers(sudoersPath, before)
		return shares.WriteReport{}, withRollbackNote(commandFailed(
			"删除之后整份 sudo 配置没有通过 visudo（%s）：%s",
			syscmdSummary(res, err), strings.TrimSpace(res.Stderr+res.Stdout)), rbErr)
	}
	if err := e.opts.Store.DeleteSudoRule(ctx, rec.ID); err != nil {
		rbErr := e.restoreSudoers(sudoersPath, before)
		return shares.WriteReport{}, withRollbackNote(
			commandFailed("删除 sudo 规则记录失败：%v", err), rbErr)
	}
	remaining := 0
	if res, err := e.run(ctx, specSudoListAs(rec.Name), commandTimeout); err == nil {
		remaining = len(parseSudoList(res.Stdout + res.Stderr))
	}
	return shares.WriteReport{
		Outcome: shares.OutcomeVerified,
		Observed: map[string]any{
			"account_id": rec.ID, "name": rec.Name, "rule_id": existing.ID,
			"template_id": existing.Template, "path": sudoersPath,
			"removed": true, "sudo_rules_remaining": remaining,
		},
		Data: map[string]any{"account_id": rec.ID, "rule_id": existing.ID},
	}, nil
}

// restoreSudoers 把规则文件恢复到写之前的样子（原先不存在时删掉它）。
func (e *Engine) restoreSudoers(sudoersPath string, before []byte) error {
	dir := path.Dir(sudoersPath)
	if before == nil {
		if err := e.opts.FS.Remove(sudoersPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return e.opts.FS.FsyncDir(dir)
	}
	tmp := path.Join(dir, "."+path.Base(sudoersPath)+".upanel.rb")
	if err := e.opts.FS.WriteFileSync(tmp, before, 0o440); err != nil {
		return err
	}
	if err := e.opts.FS.Rename(tmp, sudoersPath); err != nil {
		_ = e.opts.FS.Remove(tmp)
		return err
	}
	return e.opts.FS.FsyncDir(dir)
}

// syscmdSummary 把一次失败的执行压成一句能读的话。
func syscmdSummary(res syscmd.Result, err error) string {
	var exitErr *syscmd.ExitError
	switch {
	case errors.As(err, &exitErr):
		return fmt.Sprintf("退出码 %d", exitErr.ExitCode)
	case errors.Is(err, syscmd.ErrTimeout):
		return "执行超时"
	default:
		return err.Error()
	}
}

// nextVersion 把 "1" 推到 "2"。版本是不可比较的自增串（不是时间戳）。
func nextVersion(v string) string {
	n := 0
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return v + "+1"
		}
		n = n*10 + int(v[i]-'0')
	}
	return itoa(n + 1)
}
