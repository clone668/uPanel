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
//
// P08.2 起配置写入是真的：apply 会写托管配置、接 include、校验、重载、读回；
// disable/delete 会把段从配置里摘掉再收回 ACL（先停止暴露，再收权限）。
// served 仍然只有在 smbd 正在运行且重载被接受时才是 true，“写好了”与“在提供服务”
// 是两件不同的事，这一点没有因为多了写配置的能力而放松。

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
			NoticeAccountMissing, NoticeAccountNotManaged, NoticeAccountHasShares,
			NoticeTestparmMissing, NoticeConfigNotWritable, NoticeConfigNotReadable:
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

	// 1. 目录。目标目录 0700（先紧后松），上级只要 x（能穿过去）；见 ensureShareDir。
	repaired, err := e.ensureShareDir(dctx.MountPath, dctx.Dir)
	if err != nil {
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

	// 5. 写 Samba 配置。
	//
	// 托管文件由面板独占，因此“加一份共享”从来不是往文件末尾追加一段，而是把
	// **当前应有的全集**整份渲染出来写下去。顺序也是刻意的：目录与 ACL 在前
	// （目录还不存在时把共享暴露出去没有意义），配置在后；配置写不成功时目录与 ACL
	// 保持已准备状态，登记表不动——下一步重试会从同一个地方继续。
	wantShares, err := e.desiredShares(ctx, def, p.ShareID)
	if err != nil {
		return WriteReport{}, err
	}
	cfg, err := e.applyManagedConfig(ctx, wantShares, req.Progress)
	if err != nil {
		return WriteReport{}, err
	}
	// 能力探针的缓存里还存着“include 还没接入”“托管文件不存在”，必须立刻作废，
	// 否则紧随其后的读取会看到一份已经过时的结论。
	e.opts.Prober.Invalidate()

	// 6. 落库。新建时生成 id；更新时沿用 id 并把版本自增。
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
	// ConfigApplied 说的是“托管配置里现在有这一段”，不是“此刻在对客户端提供服务”。
	// 禁用的共享不在配置里，所以它是 false；smbd 没运行属于 served=false，不影响这一项。
	rec.ConfigApplied = def.Enabled
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

	outcome := OutcomeAccepted
	if cfg.Report.Served {
		outcome = OutcomeVerified
	}
	observed := baseConfigObserved(cfg)
	for k, v := range map[string]any{
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
	} {
		observed[k] = v
	}
	// 修过哪些上级目录的权限，一律说出来。空列表不进 observed（结论里不留没意义的键）。
	if len(repaired) > 0 {
		observed["path_traversal_repaired"] = repaired
	}
	return WriteReport{
		Outcome:  outcome,
		Observed: observed,
		Data: map[string]any{
			"share_id":    rec.ID,
			"version":     rec.Version,
			"config_hash": hash,
			// 渲染出来的共享段：这是“配置版本”背后的实际内容，界面上可以直接展示。
			"config_preview":   rendered,
			"managed_config":   cfg.Report.Path,
			"managed_sections": cfg.Report.Sections,
			"served":           cfg.Report.Served,
			"include":          cfg.Report.Include,
			"reload":           cfg.Report.Reload,
			"service":          cfg.Report.Service,
		},
		RecoveryHint: recoveryHint(cfg.Report, def),
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

	// 1. 先把这份共享从配置里摘掉。
	//
	// 顺序与 apply 相反，理由是同一条：**先停止暴露，再收回权限**。反过来的话，
	// 在“ACL 已收回、配置还没重载”的那几毫秒里，共享还在，只是谁都进不去——
	// 客户端会看到“权限被拒”而不是“共享不存在”，排障时完全是两条线索。
	remaining, err := e.sharesExcept(ctx, rec.ID)
	if err != nil {
		return WriteReport{}, err
	}
	cfg, err := e.applyManagedConfig(ctx, remaining, req.Progress)
	if err != nil {
		return WriteReport{}, err
	}
	e.opts.Prober.Invalidate()

	// 2. 收回目录 ACL。
	cleared, note, err := e.revokeACL(ctx, rec)
	if err != nil {
		return WriteReport{}, err
	}

	now := e.opts.Now().UTC()
	if action == ActionShareDisable {
		rec.Definition.Enabled = false
		rec.DirectoryPrepared = false
		rec.ConfigApplied = false
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

	observed := baseConfigObserved(cfg)
	for k, v := range map[string]any{
		"share_id":         rec.ID,
		"share_name":       rec.Definition.Name,
		"acl_revoked":      cleared,
		"directory_kept":   true,
		"record_retrained": action == ActionShareDisable,
	} {
		observed[k] = v
	}
	if note != "" {
		observed["acl_note"] = note
	}
	if action == ActionShareDisable {
		observed["version"] = rec.Version
	}
	outcome := OutcomeAccepted
	// 摘掉配置之后 “served” 必须是 false：它不是“服务在不在”，而是“这份共享还在不在
	// 提供服务”。把 report 里那个值直接抄过来会把停用的共享显示成仍在提供服务。
	observed["served"] = false
	return WriteReport{
		Outcome:  outcome,
		Observed: observed,
		Data: map[string]any{
			"share_id":  rec.ID,
			"directory": "",
			"service":   cfg.Report.Service,
			"reload":    cfg.Report.Reload,
			"served":    false,
		},
		RecoveryHint: "目录与里面的文件没有被删除；ACL 已清空（除属主之外谁都进不去）；" +
			"配置里的这一段已经被移除" + revokeHint(cfg.Report),
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

// ancestorTraverseMode 是共享目录**上级**目录的权限：0711。
//
// 共享账号不是这些目录的属主、也不在属组里，能不能穿过去只看 other 的 x 位；
// 没有 x 时 SMB 客户端连目录都进不去（真机上表现为 put 时
// `NT_STATUS_ACCESS_DENIED opening remote file`，而目录自身的 ACL 一条不差）。
// 只给 x 不给 r/w：能穿过去，看不到里面有什么。
const ancestorTraverseMode = 0o711

// shareDirMode 是共享目录**自己**的初始权限。
const shareDirMode = 0o700

// ensureShareDir 建共享目录，并保证上级目录可穿越。
//
// 两件事分开做，因为它们的权限语义不同：目标是 0700（先紧后松——任何中间状态下
// 目录都不对 world 开放，后面的 setfacl 再把具体账号的权限放进去），而上级只要 x。
// 返回被修正过权限的上级目录（空表示没有需要修的），由调用方写进 observed：
// “我改了这台机器上哪些目录的权限”是必须能回答的问题。
func (e *Engine) ensureShareDir(mountPath, dir string) ([]string, error) {
	cleanMount, cleanDir := path.Clean(mountPath), path.Clean(dir)
	if !sameOrInside(cleanMount, cleanDir) {
		return nil, dependencyBlocked("共享目录 %s 不在卷挂载点 %s 之内", dir, mountPath)
	}

	// 1. 目标目录：已存在就只做形状检查。
	if fi, err := e.opts.Lstat(cleanDir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, dependencyBlocked("共享目录 %s 是符号链接", dir)
		}
		if !fi.IsDir() {
			return nil, dependencyBlocked("共享目录 %s 已存在且不是目录", dir)
		}
		return e.ensureAncestorsTraversable(cleanMount, cleanDir)
	} else if !os.IsNotExist(err) {
		return nil, commandFailed("读取共享目录 %s 失败：%v", dir, err)
	}

	// 2. 上级先建（0711），再建目标（0700）。顺序反过来会让 MkdirAll 用 0700 把
	//    缺的上级一起造出来——那正是“目录 ACK 全对、客户端却进不去”的成因。
	parent := path.Dir(cleanDir)
	if parent != cleanMount {
		if err := e.opts.Mkdir(parent, ancestorTraverseMode); err != nil {
			return nil, commandFailed("创建共享目录的上级 %s 失败：%v", parent, err)
		}
	}
	if err := e.opts.Mkdir(cleanDir, shareDirMode); err != nil {
		return nil, commandFailed("创建共享目录 %s 失败：%v", dir, err)
	}
	return e.ensureAncestorsTraversable(cleanMount, cleanDir)
}

// ensureAncestorsTraversable 让挂载点与共享目录之间的每一级都可穿越。
//
// 只加 other 的 x 位，其余位原样保留。只处理**挂载点之内**的目录：挂载点本身归存储
// 模块（导入卷时按 0755 建），不在这里改；挂载点之外的路径不是面板的地盘。
//
// 为什么不干脆报错让人去改：这些目录是面板自己建的，一个由面板造出来、又挡住面板
// 自己创建的共享账号的目录，是面板的产品缺陷，不是运维配置。修好它，并且说出来。
func (e *Engine) ensureAncestorsTraversable(mountPath, dir string) ([]string, error) {
	rel := strings.TrimPrefix(dir, mountPath+"/")
	if rel == dir || rel == "" {
		return nil, nil
	}
	segs := strings.Split(rel, "/")
	var repaired []string
	cur := mountPath
	for i := 0; i < len(segs)-1; i++ {
		cur = path.Join(cur, segs[i])
		fi, err := e.opts.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// 上级不存在意味着目标也建不出来（上面已经 Mkdir 过），走到这里说明
				// 有人在两步之间动过目录树：如实报错，不在看不见的地方猜。
				return repaired, commandFailed("共享目录的上级 %s 不存在", cur)
			}
			return repaired, commandFailed("读取共享目录的上级 %s 失败：%v", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			// 形状问题由 checkSharePath 判，这里只负责权限。
			return repaired, dependencyBlocked("共享目录的上级 %s 不再是目录", cur)
		}
		perm := fi.Mode().Perm()
		if perm&0o001 != 0 {
			continue
		}
		// 补的是 group 与 other 的 x：other 的 x 才是共享账号真正需要的（它们不在这个
		// 目录的属组里），group 的 x 一起补是为了让“修好的目录”与“新建的目录”长得一样
		// （0711），否则同一台机器上会出现两种形状，下次排查又要重新想一遍。
		if err := e.opts.Chmod(cur, perm|0o011); err != nil {
			return repaired, commandFailed("给 %s 补上穿越权限失败：%v", cur, err)
		}
		repaired = append(repaired, cur)
	}
	return repaired, nil
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

// desiredShares 算出托管配置应有的**全集**：登记表里所有记录，其中目标共享
// 用 newDef 替换（新建时追加）。
//
// 为什么是全集而不是“这一段”：托管文件由面板独占且整份重写，所以“这份定义该长什么样”
// 与“这个文件该长什么样”是同一个问题。分两次写（先追加这一段、以后再整理）会让文件里
// 出现半份状态，而半份状态正是“有些人能用、有些人不能用”这种故障的来源。
func (e *Engine) desiredShares(ctx context.Context, newDef ShareDefinition, shareID *string) ([]ManagedShare, error) {
	records, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ShareRecord, 0, len(records)+1)
	replaced := false
	for _, rec := range records {
		if shareID != nil && rec.ID == *shareID {
			rec.Definition = newDef
			replaced = true
		}
		out = append(out, rec)
	}
	if !replaced {
		out = append(out, ShareRecord{Definition: newDef})
	}
	return e.renderManagedShares(ctx, out)
}

// sharesExcept 算出“摘掉某条共享之后”的全集。
func (e *Engine) sharesExcept(ctx context.Context, shareID string) ([]ManagedShare, error) {
	records, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ShareRecord, 0, len(records))
	for _, rec := range records {
		if rec.ID == shareID {
			continue
		}
		out = append(out, rec)
	}
	return e.renderManagedShares(ctx, out)
}

// baseConfigObserved 把配置写入报告摊平成 observed_state 里的键。
//
// 键名与 P08.1 保持同名（samba_config / served），值从“恒为 not_written/false”
// 变成真实结果：改名会让已经写进审计日志的历史记录失去可比性。
func baseConfigObserved(cfg configApplyResult) map[string]any {
	r := cfg.Report
	out := map[string]any{
		"managed_config":  r.Path,
		"managed_hash":    r.Hash,
		"samba_config":    r.File,
		"config_testparm": r.Testparm,
		"include":         r.Include,
		"reload":          r.Reload,
		"service":         r.Service,
		"served":          r.Served,
	}
	if len(r.Backups) > 0 {
		out["config_backups"] = r.Backups
	}
	if len(r.Overrides) > 0 {
		out["config_overrides"] = r.Overrides
	}
	if len(r.Notes) > 0 {
		codes := make([]string, 0, len(r.Notes))
		for _, n := range r.Notes {
			codes = append(codes, n.Code)
		}
		out["config_notices"] = codes
	}
	return out
}

// recoveryHint 生成“现状是什么、下一步该干什么”的一句话。
//
// 它替代了 P08.1 那句“本阶段不写配置”：现在每一条分支都要说清到底还差什么，
// 因为差的东西各不相同，用户能做的事也各不相同。
func recoveryHint(r ManagedConfigReport, def ShareDefinition) string {
	if !def.Enabled {
		return "共享已按禁用状态写入配置（配置里不会有这一段），目录与 ACL 已按禁用处理"
	}
	switch {
	case r.Served:
		return "配置已写入并通过 Samba 自身校验与重载，smbd 正在提供这份共享"
	case r.Service == ServiceStateInactive:
		return "配置已写好并通过校验，但 smbd 不在运行，这份共享此刻不对外提供访问；" +
			"启动 Samba 服务与开机依赖属于 P08.3"
	case r.Reload == ReloadFailed:
		return "配置已写好并通过校验，但重载没有成功：Samba 仍在使用旧配置，" +
			"需要重启或由人工重新执行重载"
	case r.Service == ServiceStateUnknown:
		return "配置已写好并通过校验，但问不出 smbd 的运行状态，无法确认它是否已经在提供访问"
	default:
		return "配置已写入，但尚未确认是否在提供服务（见 config_notices）"
	}
}

// revokeHint 补充说明摘除配置之后服务侧的状态。
func revokeHint(r ManagedConfigReport) string {
	switch {
	case r.Service == ServiceStateInactive:
		return "；smbd 不在运行，重载无从谈起，配置改动会在下次启动时生效"
	case r.Reload == ReloadFailed:
		return "；重载失败，Samba 仍在使用旧配置，这段共享可能仍然可达，需要重新重载或重启 smbd"
	case r.Service == ServiceStateUnknown:
		return "；问不出 smbd 的运行状态，无法确认这段共享是否已经不可达"
	default:
		return "；重载已被接受，Samba 已按新配置服务"
	}
}
