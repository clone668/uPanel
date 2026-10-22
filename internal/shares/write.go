package shares

// 共享与账号动作的执行器（P08.1）。
//
// 执行器只做一件事：把预检已经判过的事实**真的做出来**，然后读回来核对。三条纪律：
//
//  1. 执行前重新判决一次。排队这段时间里卷可能被卸载、账号可能被删、目录可能被
//     换成符号链接。guard 比对目标身份与状态版本，不一致就拒（409），不硬来。
//  2. 每一步之后都读回。useradd 退出码 0 不等于“账号按我们要的样子建出来了”；
//     setfacl 退出码 0 不等于“目录上现在就是这份授权”。读回用的是系统自己的
//     工具（getent、passwd -S、pdbedit、getfacl）。
//  3. 失败要说清在哪一步、差在哪。做成了一半就回滚（只回滚这次自己造出来的东西），
//     并把回滚结果写在 observed_state 里。
//
// P08.1 的边界写在返回值里：share.apply 的 observed_state 恒有
// samba_config=not_written 与 served=false，outcome 是 accepted 而不是 verified。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// ExecuteWrite 执行一次共享或账号动作。
func (e *Engine) ExecuteWrite(ctx context.Context, req WriteRequest) (WriteReport, error) {
	switch req.Action {
	case ActionShareApply:
		p, ok := req.Params.(ShareApplyParams)
		if !ok {
			return WriteReport{}, commandFailed("share.apply 的参数类型是 %T", req.Params)
		}
		return e.executeShareApply(ctx, p, req)
	case ActionShareDisable, ActionShareDelete:
		p, ok := req.Params.(ShareRefParams)
		if !ok {
			return WriteReport{}, commandFailed("%s 的参数类型是 %T", req.Action, req.Params)
		}
		return e.executeShareRef(ctx, req.Action, p, req)
	case ActionAccountApply:
		p, ok := req.Params.(AccountApplyParams)
		if !ok {
			return WriteReport{}, commandFailed("share.account.apply 的参数类型是 %T", req.Params)
		}
		return e.executeAccountApply(ctx, p, req)
	case ActionAccountDelete:
		p, ok := req.Params.(AccountRefParams)
		if !ok {
			return WriteReport{}, commandFailed("share.account.delete 的参数类型是 %T", req.Params)
		}
		return e.executeAccountDelete(ctx, p, req)
	default:
		return WriteReport{}, validationError("未登记的共享动作 %q", req.Action)
	}
}

// guard 把预检判决变成执行前的最后一道门。
func guard(a Assessment, req WriteRequest) error {
	if !a.Allowed {
		return blockerError(a)
	}
	// 目标身份：共享动作是文件系统 UUID，账号动作是账号名。
	// 预检记下来的那个必须还是现在这个。
	if req.ExpectedIdentity != "" && a.IdentityFingerprint != req.ExpectedIdentity {
		return identityChanged("目标身份已变化（%s），请重新预检", a.DisplayName)
	}
	if a.StateVersion != req.ExpectedStateVersion {
		return versionConflict("目标状态在预检之后发生变化（%s），请重新预检", a.DisplayName)
	}
	return nil
}

// blockerError 把阻塞项映射成一个带判决的错误。
//
// 依赖类阻塞（卷没挂、工具缺失、账号被引用）是 409 DEPENDENCY_BLOCKED：
// 调用方要做的是“先满足前置条件”。其余是 400 VALIDATION_ERROR：调用方要改参数。
func blockerError(a Assessment) error {
	codes := make([]string, 0, len(a.Blockers))
	messages := make([]string, 0, len(a.Blockers))
	dependency := false
	for _, b := range a.Blockers {
		codes = append(codes, b.Code)
		messages = append(messages, b.Message)
		switch b.Code {
		case NoticeVolumeNotMounted, NoticeVolumeNotFound, NoticeVolumeUUIDMismatch,
			NoticeSMBUnavailable, NoticeACLToolsMissing, NoticeSecretInvalid,
			NoticeAccountMissing, NoticeAccountNotManaged, NoticeAccountHasShares:
			dependency = true
		}
	}
	joined := strings.Join(messages, "；")
	if dependency {
		return dependencyBlocked("%s（%s）", joined, strings.Join(codes, ","))
	}
	return validationError("%s（%s）", joined, strings.Join(codes, ","))
}

// executeShareApply 执行 share.apply。
func (e *Engine) executeShareApply(ctx context.Context, p ShareApplyParams, req WriteRequest) (WriteReport, error) {
	a, dctx, err := e.assessShareApply(ctx, p)
	if err != nil {
		return WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return WriteReport{}, err
	}
	def := *p.Definition
	t := e.Capability(ctx).Tools

	// 预检到执行之间，路径上可能出现了符号链接。再查一次——只查、不改。
	if blockers, _ := e.checkSharePath(dctx.MountPath, dctx.Dir); len(blockers) > 0 {
		return WriteReport{}, dependencyBlocked("共享路径在执行前复核时不再安全：%s",
			blockers[0].Message)
	}

	// 1. 目录。权限先给 0700：先紧后松，任何中间状态下都不会对 world 开放。
	if err := e.ensureShareDir(dctx.MountPath, dctx.Dir); err != nil {
		return WriteReport{}, err
	}
	if req.Progress != nil {
		req.Progress("share_directory", map[string]any{"directory": dctx.Dir})
	}

	// 2. ACL。禁用的共享不授权任何人：定义里 enabled=false 时，目录上只留属主。
	access := resolvedAccesses(def, dctx.Names)
	desired := DesiredACL(access, def.ReadOnly)
	if !def.Enabled {
		desired = nil
	}
	spec := ACLSpec(desired)
	if _, err := e.run(ctx, specSetfacl(t, spec, dctx.Dir), aclCommandTimeout); err != nil {
		return WriteReport{}, commandFailed("设置 %s 的 POSIX ACL 失败：%v", dctx.Dir, err)
	}
	if req.Progress != nil {
		req.Progress("share_acl", map[string]any{"directory": dctx.Dir, "entries": len(desired)})
	}

	// 3. 读回核验。ACL 写成功不等于写对了：mask 自动计算、setfacl 版本差异、
	//    目录上有既有条目……都在这一步才会露出来。
	got, mask, err := e.readACL(ctx, t, dctx.Dir)
	if err != nil {
		return WriteReport{}, err
	}
	if !MaskCovers(desired, mask) {
		return WriteReport{}, commandFailed(
			"%s 的 ACL 条目写上了，但 mask 是 %q，没有覆盖定义要求的授权（条目存在、实际不生效）",
			dctx.Dir, mask)
	}
	diff := DiffACL(desired, got)
	if !diff.OK() {
		return WriteReport{}, commandFailed("%s 的 ACL 读回核验不通过：%s", dctx.Dir, diff.String())
	}

	// 4. 渲染与配置版本。
	rendered, err := def.RenderBlock(dctx.MountPath, dctx.Names)
	if err != nil {
		return WriteReport{}, validationError("%s", err.Error())
	}
	hash := ConfigHashOf(rendered)

	// 5. 落库。新建时生成 id；更新时沿用 id 并把版本自增。
	now := e.opts.Now().UTC()
	var rec ShareRecord
	if p.ShareID == nil {
		id, err := NewShareID()
		if err != nil {
			return WriteReport{}, err
		}
		rec = ShareRecord{ID: id, CreatedAt: now, Version: "1"}
	} else {
		existing, found, err := e.opts.Store.GetShare(ctx, *p.ShareID)
		if err != nil {
			return WriteReport{}, err
		}
		if !found {
			return WriteReport{}, notFound("共享 %s 在执行时已不存在", *p.ShareID)
		}
		rec = existing
		rec.Version = nextVersion(existing.Version)
	}
	rec.Definition = def
	rec.ConfigHash = hash
	rec.DirectoryPrepared = true
	// P08.1 没有写 Samba 配置的代码路径，这一项只能是 false。
	// 保留字段而不是删掉它：P08.2 要把它置真，页面也要按它显示“已生效/未生效”。
	rec.ConfigApplied = false
	rec.LastOperationID = req.OperationID
	rec.UpdatedAt = now
	if err := e.opts.Store.SaveShare(ctx, rec); err != nil {
		return WriteReport{}, err
	}

	aclLines := make([]string, 0, len(desired)+3)
	aclLines = append(aclLines, aclOwnerKey+permsOwner, aclGroupKey+permsNone, aclOtherKey+permsNone)
	for _, en := range desired {
		aclLines = append(aclLines, aclUserPrefix+en.Name+":"+en.Perms)
	}

	return WriteReport{
		Outcome: OutcomeAccepted,
		Observed: map[string]any{
			"share_id":           rec.ID,
			"share_name":         def.Name,
			"version":            rec.Version,
			"volume_id":          def.VolumeID,
			"mount_path":         dctx.MountPath,
			"directory":          dctx.Dir,
			"directory_created":  true,
			"directory_prepared": true,
			"acl":                aclLines,
			"acl_mask":           mask,
			"acl_verified":       true,
			"config_hash":        hash,
			// 下面两条是这一阶段最重要的事实：定义准备好了，但共享还没有被提供。
			"samba_config": "not_written",
			"served":       false,
		},
		Data: map[string]any{
			"share_id":    rec.ID,
			"version":     rec.Version,
			"config_hash": hash,
			// 渲染出来的共享段：这是“配置版本”背后的实际内容，界面上可以直接展示。
			"config_preview": rendered,
		},
		RecoveryHint: "本阶段（P08.1）不写 Samba 配置：定义、目录与 ACL 都已准备并核验，" +
			"但共享此刻不对外提供访问，P08.2 才会写入配置并重载",
	}, nil
}

// executeShareRef 执行 share.disable / share.delete。
func (e *Engine) executeShareRef(ctx context.Context, action string, p ShareRefParams, req WriteRequest) (WriteReport, error) {
	a, err := e.assessShareRef(ctx, action, p)
	if err != nil {
		return WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return WriteReport{}, err
	}
	rec, found, err := e.opts.Store.GetShare(ctx, p.ShareID)
	if err != nil {
		return WriteReport{}, err
	}
	if !found {
		return WriteReport{}, notFound("共享 %s 已不存在", p.ShareID)
	}

	cleared, note, err := e.revokeACL(ctx, rec)
	if err != nil {
		return WriteReport{}, err
	}

	now := e.opts.Now().UTC()
	if action == ActionShareDisable {
		rec.Definition.Enabled = false
		rec.DirectoryPrepared = false
		rec.Version = nextVersion(rec.Version)
		rec.LastOperationID = req.OperationID
		rec.UpdatedAt = now
		if err := e.opts.Store.SaveShare(ctx, rec); err != nil {
			return WriteReport{}, err
		}
	} else {
		if err := e.opts.Store.DeleteShare(ctx, rec.ID); err != nil {
			return WriteReport{}, err
		}
	}

	observed := map[string]any{
		"share_id":         rec.ID,
		"share_name":       rec.Definition.Name,
		"acl_revoked":      cleared,
		"directory_kept":   true,
		"samba_config":     "not_written",
		"served":           false,
		"record_retrained": action == ActionShareDisable,
	}
	if note != "" {
		observed["acl_note"] = note
	}
	if action == ActionShareDisable {
		observed["version"] = rec.Version
	}
	return WriteReport{
		Outcome:  OutcomeAccepted,
		Observed: observed,
		Data: map[string]any{
			"share_id":  rec.ID,
			"directory": "",
		},
		RecoveryHint: "目录与里面的文件没有被删除；ACL 已清空（除属主之外谁都进不去）",
	}, nil
}

// executeAccountApply 执行 share.account.apply。
func (e *Engine) executeAccountApply(ctx context.Context, p AccountApplyParams, req WriteRequest) (WriteReport, error) {
	// 执行前复核用的是任务自己的预算，不是预检那条 3 秒：这里没有 HTTP 连接在等，
	// 而且它才是“改不改系统”的最后一道门（DECISIONS Q223）。
	a, err := e.assessAccountApply(ctx, p, accountLookupExecTimeout)
	if err != nil {
		return WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return WriteReport{}, err
	}
	t := e.Capability(ctx).Tools

	if p.AccountID == nil {
		return e.createAccount(ctx, t, p, req)
	}
	return e.updateAccount(ctx, t, p, req)
}

// createAccount 建 Linux 账号 + Samba 账号，并逐项读回核验。
//
// 任何一步读回不符都回滚：只回滚这次自己造出来的东西（userdel / smbpasswd -x），
// 不回滚别人的账号，也不留下一个“密码可能没设上”的半成品。
func (e *Engine) createAccount(ctx context.Context, t ToolPaths, p AccountApplyParams, req WriteRequest) (WriteReport, error) {
	// 先拿密码：拿不到秘密就不该建出一个没有密码的账号。
	secret, err := e.opts.Secrets.Resolve(ctx, p.SecretRef)
	if err != nil {
		return WriteReport{}, validationError("秘密引用 %s 无法解析：%v（密码可能已经过期，请重新上传）",
			p.SecretRef, err)
	}
	defer wipe(secret)
	stdin := sambaPasswordStdin(secret)
	defer wipe(stdin)

	if req.Progress != nil {
		req.Progress("account_create", map[string]any{"name": p.Name})
	}
	if _, err := e.run(ctx, specUseradd(p.Name), commandTimeout); err != nil {
		// useradd 用退出码 9 表示“这个名字已经被占用”。到这一步说明预检没拦住它
		// （预检查询超时只发告警），或者在这几秒里别人建了同名账号。
		// 两种情况都不该接管：照实说名字被占，而不是把它报成一次笼统的命令失败——
		// 这是“绝不静默接管既有账号”这条不变量的可见落点。
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode == 9 {
			return WriteReport{}, dependencyBlocked(
				"系统里已经有账号 %s（useradd 退出码 9：名字已被占用），拒绝接管它（%s）",
				p.Name, NoticeAccountNotManaged)
		}
		return WriteReport{}, commandFailed("创建系统账号 %s 失败：%v", p.Name, err)
	}
	rollback := func(reason string) (WriteReport, error) {
		_, _ = e.run(ctx, specSambaDelete(t, p.Name), commandTimeout)
		_, _ = e.run(ctx, specUserdel(p.Name), commandTimeout)
		return WriteReport{}, commandFailed("%s；已回滚（删掉刚创建的系统账号 %s）", reason, p.Name)
	}

	// 读回一：getent passwd 里的样子必须是我们要的样子。
	entry, ok, err := e.readPasswd(ctx, p.Name)
	if err != nil {
		return rollback(fmt.Sprintf("读取系统账号失败：%v", err))
	}
	if !ok {
		return rollback("账号创建后 getent passwd 找不到它")
	}
	if entry.Shell != DefaultShell {
		return rollback(fmt.Sprintf("登录 shell 是 %q，期望 %q", entry.Shell, DefaultShell))
	}
	if entry.Home != DefaultHome {
		return rollback(fmt.Sprintf("home 是 %q，期望 %q", entry.Home, DefaultHome))
	}
	if entry.UID >= 1000 {
		return rollback(fmt.Sprintf("uid 是 %d，落在普通用户段（期望 < 1000 的系统段）", entry.UID))
	}

	// 读回二：Linux 密码必须是锁着的（SMB 密码只存在 Samba 的 passdb 里）。
	lock, err := e.passwordState(ctx, p.Name)
	if err != nil {
		return rollback(fmt.Sprintf("读取账号锁状态失败：%v", err))
	}
	if lock != LockLocked && lock != LockNone {
		return rollback(fmt.Sprintf("账号的 Linux 密码状态是 %q，期望锁定（L）", lock))
	}

	// 设置 Samba 密码：明文只经标准输入。
	if req.Progress != nil {
		req.Progress("account_password", map[string]any{"name": p.Name})
	}
	if _, err := e.run(ctx, specSambaSetPassword(t, p.Name, stdin), commandTimeout); err != nil {
		return rollback(fmt.Sprintf("设置 Samba 密码失败：%v", err))
	}

	// 读回三：passdb 里必须有这个账号，且密码已经设上。
	samba, ok, err := e.readSambaAccount(ctx, t, p.Name)
	if err != nil {
		return rollback(fmt.Sprintf("读取 Samba 账号失败：%v", err))
	}
	if !ok {
		return rollback("Samba passdb 里找不到刚创建的账号")
	}
	if !samba.PasswordSet {
		return rollback("passdb 里的 Password last set 没有被设上")
	}

	// 禁用：新建时就要求禁用的话，建完立刻打 D 标志，并读回确认。
	if p.Disabled {
		if _, err := e.run(ctx, specSambaDisable(t, p.Name), commandTimeout); err != nil {
			return rollback(fmt.Sprintf("禁用 Samba 账号失败：%v", err))
		}
		samba, ok, err = e.readSambaAccount(ctx, t, p.Name)
		if err != nil || !ok {
			return rollback("禁用之后读不到 Samba 账号")
		}
		if !samba.Disabled {
			return rollback("禁用之后 Account Flags 里没有 D 标志")
		}
	}

	now := e.opts.Now().UTC()
	id, err := NewAccountID()
	if err != nil {
		return rollback(err.Error())
	}
	rec := AccountRecord{
		ID:          id,
		Name:        p.Name,
		Type:        AccountTypeShare,
		Managed:     true,
		Disabled:    samba.Disabled,
		HasPassword: true,
		UID:         entry.UID,
		GID:         entry.GID,
		Shell:       entry.Shell,
		Home:        entry.Home,
		SecretRef:   p.SecretRef,
		Version:     "1",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	rec.LastOperationID = req.OperationID
	if err := e.opts.Store.SaveAccount(ctx, rec); err != nil {
		return rollback(fmt.Sprintf("保存账号记录失败：%v", err))
	}

	// 把临时秘密转成持久秘密。失败不影响系统状态（密码已经写进 passdb），
	// 但要在结果里说清楚，因为那条秘密 10 分钟后会自己消失。
	pinned := true
	pinNote := ""
	if err := e.opts.Secrets.Reference(ctx, p.SecretRef, "share_account:"+rec.ID); err != nil {
		pinned = false
		pinNote = err.Error()
	}

	observed := map[string]any{
		"account_id":        rec.ID,
		"name":              rec.Name,
		"uid":               rec.UID,
		"gid":               rec.GID,
		"shell":             rec.Shell,
		"home":              rec.Home,
		"linux_password":    string(lock),
		"samba_account":     true,
		"samba_disabled":    rec.Disabled,
		"password_set":      samba.PasswordSet,
		"password_last_set": formatObservedTime(samba.PasswordSetAt),
		"secret_persisted":  pinned,
		"account_flags":     samba.Flags,
	}
	if pinNote != "" {
		observed["secret_note"] = pinNote
	}
	return WriteReport{
		Outcome:  OutcomeVerified,
		Observed: observed,
		Data: map[string]any{
			"account_id":   rec.ID,
			"name":         rec.Name,
			"version":      rec.Version,
			"has_password": true,
			"disabled":     rec.Disabled,
		},
	}, nil
}

// updateAccount 改密码/改禁用状态。
func (e *Engine) updateAccount(ctx context.Context, t ToolPaths, p AccountApplyParams, req WriteRequest) (WriteReport, error) {
	rec, found, err := e.opts.Store.GetAccount(ctx, *p.AccountID)
	if err != nil {
		return WriteReport{}, err
	}
	if !found {
		return WriteReport{}, notFound("受管理账号 %s 已不存在", *p.AccountID)
	}

	// 系统读回一次：受管理记录说它存在，不等于系统上还在（有人手工删了）。
	entry, ok, err := e.readPasswd(ctx, rec.Name)
	if err != nil {
		return WriteReport{}, commandFailed("读取系统账号 %s 失败：%v", rec.Name, err)
	}
	if !ok {
		return WriteReport{}, dependencyBlocked(
			"受管理账号 %s 在系统上已经不存在（可能被手工删除）；请删掉这条记录后重建", rec.Name)
	}

	passwordSet := false
	if p.SecretRef != "" {
		secret, err := e.opts.Secrets.Resolve(ctx, p.SecretRef)
		if err != nil {
			return WriteReport{}, validationError("秘密引用 %s 无法解析：%v", p.SecretRef, err)
		}
		stdin := sambaPasswordStdin(secret)
		wipe(secret)
		if req.Progress != nil {
			req.Progress("account_password", map[string]any{"name": rec.Name})
		}
		_, runErr := e.run(ctx, specSambaSetPassword(t, rec.Name, stdin), commandTimeout)
		wipe(stdin)
		if runErr != nil {
			return WriteReport{}, commandFailed("重设 %s 的 Samba 密码失败：%v", rec.Name, runErr)
		}
		samba, ok, err := e.readSambaAccount(ctx, t, rec.Name)
		if err != nil || !ok {
			return WriteReport{}, commandFailed("重设密码后读不到 Samba 账号 %s", rec.Name)
		}
		if !samba.PasswordSet {
			return WriteReport{}, commandFailed("重设密码后 passdb 里的 Password last set 仍然是空")
		}
		passwordSet = true
		rec.SecretRef = p.SecretRef
		rec.HasPassword = true
	}

	if p.Disabled != rec.Disabled {
		spec := specSambaEnable(t, rec.Name)
		if p.Disabled {
			spec = specSambaDisable(t, rec.Name)
		}
		if _, err := e.run(ctx, spec, commandTimeout); err != nil {
			return WriteReport{}, commandFailed("切换 %s 的 Samba 禁用状态失败：%v", rec.Name, err)
		}
		samba, ok, err := e.readSambaAccount(ctx, t, rec.Name)
		if err != nil || !ok {
			return WriteReport{}, commandFailed("切换禁用状态后读不到 Samba 账号 %s", rec.Name)
		}
		if samba.Disabled != p.Disabled {
			return WriteReport{}, commandFailed("切换禁用状态后 Account Flags 仍是 %q，期望 disabled=%v",
				samba.Flags, p.Disabled)
		}
		rec.Disabled = samba.Disabled
	}

	now := e.opts.Now().UTC()
	rec.Version = nextVersion(rec.Version)
	rec.UID = entry.UID
	rec.GID = entry.GID
	rec.Shell = entry.Shell
	rec.Home = entry.Home
	rec.LastOperationID = req.OperationID
	rec.UpdatedAt = now
	if err := e.opts.Store.SaveAccount(ctx, rec); err != nil {
		return WriteReport{}, err
	}

	return WriteReport{
		Outcome: OutcomeVerified,
		Observed: map[string]any{
			"account_id":       rec.ID,
			"name":             rec.Name,
			"version":          rec.Version,
			"disabled":         rec.Disabled,
			"password_changed": passwordSet,
			"has_password":     rec.HasPassword,
			"uid":              rec.UID,
		},
		Data: map[string]any{
			"account_id": rec.ID,
			"version":    rec.Version,
		},
	}, nil
}

// executeAccountDelete 执行 share.account.delete。
func (e *Engine) executeAccountDelete(ctx context.Context, p AccountRefParams, req WriteRequest) (WriteReport, error) {
	a, err := e.assessAccountDelete(ctx, p)
	if err != nil {
		return WriteReport{}, err
	}
	if err := guard(a, req); err != nil {
		return WriteReport{}, err
	}
	t := e.Capability(ctx).Tools

	rec, found, err := e.opts.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return WriteReport{}, err
	}
	if !found {
		return WriteReport{}, notFound("受管理账号 %s 已不存在", p.AccountID)
	}

	if req.Progress != nil {
		req.Progress("account_delete", map[string]any{"name": rec.Name})
	}
	// 先删 Samba 账号再删 Linux 账号：反过来的话 smbpasswd 会因为找不到系统账号而失败，
	// 于是 passdb 里留下一个指向不存在 UID 的条目。
	if _, err := e.run(ctx, specSambaDelete(t, rec.Name), commandTimeout); err != nil {
		// 账号可能本来就不在 passdb 里（上一次删到一半）。继续往下走，
		// 真正的判据是最后的两条读回。
		_ = err
	}
	if _, ok, err := e.readSambaAccount(ctx, t, rec.Name); err != nil {
		return WriteReport{}, commandFailed("删除后读取 Samba 账号失败：%v", err)
	} else if ok {
		return WriteReport{}, commandFailed("删除后 passdb 里仍然有账号 %s", rec.Name)
	}

	if _, err := e.run(ctx, specUserdel(rec.Name), commandTimeout); err != nil {
		return WriteReport{}, commandFailed("删除系统账号 %s 失败：%v", rec.Name, err)
	}
	if _, still, err := e.readPasswd(ctx, rec.Name); err != nil {
		return WriteReport{}, commandFailed("删除后读取系统账号失败：%v", err)
	} else if still {
		return WriteReport{}, commandFailed("删除后系统里仍然有账号 %s", rec.Name)
	}

	if err := e.opts.Store.DeleteAccount(ctx, rec.ID); err != nil {
		return WriteReport{}, err
	}

	return WriteReport{
		Outcome: OutcomeVerified,
		Observed: map[string]any{
			"account_id":     rec.ID,
			"name":           rec.Name,
			"samba_removed":  true,
			"linux_removed":  true,
			"home_data_kept": true,
		},
		Data: map[string]any{"account_id": rec.ID},
	}, nil
}

// ensureShareDir 建共享目录。
//
// 权限先给 0700：先紧后松。任何中间状态下目录都不对 world 开放，
// 后面的 setfacl 再把具体账号的权限放进去。
func (e *Engine) ensureShareDir(mountPath, dir string) error {
	if !sameOrInside(path.Clean(mountPath), path.Clean(dir)) {
		return dependencyBlocked("共享目录 %s 不在卷挂载点 %s 之内", dir, mountPath)
	}
	if fi, err := e.opts.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return dependencyBlocked("共享目录 %s 是符号链接", dir)
		}
		if !fi.IsDir() {
			return dependencyBlocked("共享目录 %s 已存在且不是目录", dir)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return commandFailed("读取共享目录 %s 失败：%v", dir, err)
	}
	if err := e.opts.Mkdir(dir, 0o700); err != nil {
		return commandFailed("创建共享目录 %s 失败：%v", dir, err)
	}
	return nil
}

// revokeACL 清空共享目录上的授权。
//
// 返回 (是否真的清空, 没清空的原因, 错误)。卷没挂载时那条路径上的目录属于系统盘，
// 真正挂着的时候才会有的 ACL 在未挂载的文件系统里——这时既碰不到也不该碰，
// 所以如实报“没清”，而不是假装清过了。
func (e *Engine) revokeACL(ctx context.Context, rec ShareRecord) (bool, string, error) {
	t := e.Capability(ctx).Tools
	if t.Setfacl == "" || t.Getfacl == "" {
		return false, "缺少 setfacl/getfacl：目录上的 ACL 无法撤销", nil
	}
	vol, found, err := e.volumeFor(ctx, rec.Definition.VolumeID)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "卷 " + rec.Definition.VolumeID + " 已不在受管理卷里，无法定位共享目录", nil
	}
	if vol.ObservedState != storage.VolumeStateMounted {
		return false, "卷 " + vol.DisplayName + " 当前未挂载：目录上的 ACL 在未挂载的文件系统里，这次没有碰到它", nil
	}
	mountPath := vol.MountPath
	if mountPath == "" {
		mountPath = storage.MountPathOf(vol.Slug)
	}
	dir, err := ShareDir(mountPath, rec.Definition.RelativePath)
	if err != nil {
		return false, "", validationError("%s", err.Error())
	}
	if fi, err := e.opts.Lstat(dir); err != nil {
		if os.IsNotExist(err) {
			return true, "目录不存在，没有需要撤销的授权", nil
		}
		return false, "", commandFailed("读取共享目录 %s 失败：%v", dir, err)
	} else if !fi.IsDir() {
		return false, "共享目录 " + dir + " 不是目录，没有撤销任何东西", nil
	}

	empty := ACLSpec(nil)
	if _, err := e.run(ctx, specSetfacl(t, empty, dir), aclCommandTimeout); err != nil {
		return false, "", commandFailed("清空 %s 的 ACL 失败：%v", dir, err)
	}
	got, mask, err := e.readACL(ctx, t, dir)
	if err != nil {
		return false, "", err
	}
	if diff := DiffACL(nil, got); !diff.OK() {
		return false, "", commandFailed("%s 的 ACL 撤销后核验不通过：%s", dir, diff.String())
	}
	// 条目全清干净之后 mask 也必须是 ---。没有具名条目时 getfacl 根本不打 mask 行
	// （此时是平凡 ACL），所以空串也算对；但如果还留着一个 mask，说明这次 --set
	// 没有真的整份替换掉旧 ACL，下次读回就可能是假的。
	if mask != "" && mask != permsNone {
		return false, "", commandFailed("%s 的 ACL 条目已清空，但 mask 仍是 %q", dir, mask)
	}
	return true, "", nil
}

// readACL 跑 getfacl 并解析。
func (e *Engine) readACL(ctx context.Context, t ToolPaths, dir string) (map[string]string, string, error) {
	res, err := e.run(ctx, specGetfacl(t, dir), aclCommandTimeout)
	if err != nil {
		return nil, "", commandFailed("读取 %s 的 ACL 失败：%v", dir, err)
	}
	entries, mask, err := ParseGetfacl(res.Stdout)
	if err != nil {
		return nil, "", commandFailed("解析 %s 的 getfacl 输出失败：%v", dir, err)
	}
	return entries, mask, nil
}

// readPasswd 用 getent passwd 读系统账号。
func (e *Engine) readPasswd(ctx context.Context, name string) (PasswdEntry, bool, error) {
	res, err := e.run(ctx, syscmdSpecGetent("passwd", name), 10*time.Second)
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode == 2 {
			return PasswdEntry{}, false, nil
		}
		return PasswdEntry{}, false, err
	}
	line := strings.TrimSpace(res.Stdout)
	if line == "" {
		return PasswdEntry{}, false, nil
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	entry, err := parsePasswdLine(line)
	if err != nil {
		return PasswdEntry{}, false, err
	}
	return entry, true, nil
}

// passwordState 用 passwd -S 读 Linux 密码锁状态。
func (e *Engine) passwordState(ctx context.Context, name string) (LockState, error) {
	res, err := e.run(ctx, specPasswdStatus(name), 10*time.Second)
	if err != nil {
		return LockUnknown, err
	}
	return parseLockState(res.Stdout), nil
}

// readSambaAccount 用 pdbedit 读 passdb 里的账号。
//
// 账号不在 passdb 里时 pdbedit 以非零状态退出（不同版本退出码不同），这里把它
// 统一当成“不存在”，而不是错误——调用方关心的是“在不在、什么状态”。
func (e *Engine) readSambaAccount(ctx context.Context, t ToolPaths, name string) (SambaAccount, bool, error) {
	res, err := e.run(ctx, specPdbeditAccount(t, name), commandTimeout)
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return SambaAccount{}, false, nil
		}
		return SambaAccount{}, false, err
	}
	acc, parseErr := parsePdbedit(res.Stdout)
	if parseErr != nil {
		// 输出读不懂：当成“不在”会让执行器报一个假的失败原因；这里如实报错。
		return SambaAccount{}, false, parseErr
	}
	if acc.Name != "" && acc.Name != name {
		return SambaAccount{}, false, fmt.Errorf(
			"shares: pdbedit 返回的账号是 %q，期望 %q", acc.Name, name)
	}
	return acc, true, nil
}

// resolvedAccesses 把定义里的授权解析成“用户名 + 访问级别”。
func resolvedAccesses(def ShareDefinition, names map[string]string) []resolvedAccess {
	out := make([]resolvedAccess, 0, len(def.SMBUsers))
	for _, u := range def.SMBUsers {
		out = append(out, resolvedAccess{Name: names[u.AccountID], Access: u.Access})
	}
	return out
}

// nextVersion 把版本号自增。
func nextVersion(v string) string {
	n := 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			return "1"
		}
		n = n*10 + int(c-'0')
		if n > 1<<40 {
			return "1"
		}
	}
	return fmt.Sprintf("%d", n+1)
}

// formatObservedTime 把时间格式化成 RFC3339；零值返回空串。
func formatObservedTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
