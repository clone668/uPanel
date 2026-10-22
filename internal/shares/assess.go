package shares

// 共享与账号动作的预检判决（P08.1）。
//
// 这一层**不写任何东西**（CONTRACTS.md 4.1：预检不做变更）。它只读：Samba 能力、
// 受管理卷的现场视图、登记表、目录的 Lstat 结果、系统的 passwd/passdb 读回。
// 所有“不允许”的理由都必须是可展示的事实，因为拒绝的人看到的第一句话就是这里的 message。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// Assess 对一次动作做预检判决。
//
// params 必须是该动作已解码的 DTO（agent 层解码之后传进来），这里不做类型猜测：
// 类型不对是编程错误，返回 error 而不是“拒绝”。
func (e *Engine) Assess(ctx context.Context, action string, params any) (Assessment, error) {
	switch action {
	case ActionShareApply:
		p, ok := params.(ShareApplyParams)
		if !ok {
			return Assessment{}, fmt.Errorf("shares: share.apply 的参数类型是 %T", params)
		}
		a, _, err := e.assessShareApply(ctx, p)
		return a, err
	case ActionShareDisable, ActionShareDelete:
		p, ok := params.(ShareRefParams)
		if !ok {
			return Assessment{}, fmt.Errorf("shares: %s 的参数类型是 %T", action, params)
		}
		return e.assessShareRef(ctx, action, p)
	case ActionAccountApply:
		p, ok := params.(AccountApplyParams)
		if !ok {
			return Assessment{}, fmt.Errorf("shares: share.account.apply 的参数类型是 %T", params)
		}
		return e.assessAccountApply(ctx, p, accountLookupTimeout)
	case ActionAccountDelete:
		p, ok := params.(AccountRefParams)
		if !ok {
			return Assessment{}, fmt.Errorf("shares: share.account.delete 的参数类型是 %T", params)
		}
		return e.assessAccountDelete(ctx, p)
	default:
		return Assessment{}, fmt.Errorf("shares: 未登记的动作 %q", action)
	}
}

// capabilityBlockers 把“Samba 不可用”变成一条阻塞项。
func (e *Engine) capabilityBlockers(ctx context.Context) []Blocker {
	capv := e.Capability(ctx)
	if capv.Available() {
		return nil
	}
	msg := "本机的 Samba 能力不可用"
	if capv.Reason != "" {
		msg += "：" + capv.Reason
	}
	return []Blocker{{Code: NoticeSMBUnavailable, Message: msg}}
}

// definitionContext 是共享定义判决需要的现场事实。
type definitionContext struct {
	MountPath string
	// VolumeUUID 是这次操作的目标身份（执行前复核“还是这个文件系统”）。
	VolumeUUID string
	VolumeID   string
	// Dir 是共享目录的绝对路径（词法拼接，可能还不存在）。
	Dir string
	// Names 是 account_id → 系统用户名。
	Names map[string]string
}

// resolveDefinition 取共享定义需要的现场事实，并把所有阻塞/告警收集齐。
//
// 它不提前返回：一次预检要能看到**全部**问题。只报第一条的话，用户修完一条再点一次，
// 又得到下一条，来回十次才知道这个共享到底哪里不对。
func (e *Engine) resolveDefinition(ctx context.Context, def ShareDefinition) (definitionContext, []Blocker, []Blocker, error) {
	var (
		blockers []Blocker
		warnings []Blocker
		out      definitionContext
	)

	if err := ValidateDefinition(def); err != nil {
		code, msg := defErrorParts(err)
		blockers = append(blockers, Blocker{Code: code, Message: msg})
		return out, blockers, warnings, nil
	}

	vol, found, err := e.volumeFor(ctx, def.VolumeID)
	if err != nil {
		return out, nil, nil, err
	}
	if !found {
		blockers = append(blockers, Blocker{
			Code:    NoticeVolumeNotFound,
			Message: "volume_id " + def.VolumeID + " 不在受管理卷里",
		})
		return out, blockers, warnings, nil
	}
	out.VolumeUUID = vol.FilesystemUUID
	out.VolumeID = vol.ID
	if vol.MountPath != "" {
		out.MountPath = vol.MountPath
	}

	// 卷必须**现在**挂着。没挂的时候挂载点只是系统盘上的一个空目录，
	// 在那里共享等于把根分区的一部分暴露出去（PLAN.md 第 10 节明令禁止）。
	if vol.ObservedState != storage.VolumeStateMounted {
		msg := "卷 " + vol.DisplayName + " 当前没有挂载（observed_state=" + vol.ObservedState + "）"
		if vol.ObservedError != "" {
			msg += "：" + vol.ObservedError
		}
		msg += "；此时共享目录只会落在一个系统盘上的空目录里，必须先把卷挂载起来"
		blockers = append(blockers, Blocker{Code: NoticeVolumeNotMounted, Message: msg})
	}
	if out.MountPath == "" {
		out.MountPath = storage.MountPathOf(vol.Slug)
	}

	dir, err := ShareDir(out.MountPath, def.RelativePath)
	if err != nil {
		code, msg := defErrorParts(err)
		blockers = append(blockers, Blocker{Code: code, Message: msg})
		return out, blockers, warnings, nil
	}
	out.Dir = dir

	// 路径安全检查（符号链接、非目录）。只在卷挂着的时候做：卷没挂时那条路径上的
	// 东西属于系统盘，拿它来判断共享路径的对错没有意义。
	if vol.ObservedState == storage.VolumeStateMounted {
		pathBlockers, pathWarnings := e.checkSharePath(out.MountPath, dir)
		blockers = append(blockers, pathBlockers...)
		warnings = append(warnings, pathWarnings...)
	}

	// 账号：定义引用的每一个账号都必须是受管理账号。
	out.Names = make(map[string]string, len(def.SMBUsers))
	recs, err := e.opts.Store.ListAccounts(ctx)
	if err != nil {
		return out, nil, nil, err
	}
	byID := make(map[string]AccountRecord, len(recs))
	for _, r := range recs {
		byID[r.ID] = r
	}
	for _, u := range def.SMBUsers {
		rec, ok := byID[u.AccountID]
		if !ok {
			blockers = append(blockers, Blocker{
				Code:    NoticeAccountMissing,
				Message: "账号 " + u.AccountID + " 不是受管理账号（先在受管理账号里创建它）",
			})
			continue
		}
		out.Names[u.AccountID] = rec.Name
		if rec.Disabled {
			warnings = append(warnings, Blocker{
				Code:    NoticeAccountDisabled,
				Message: "账号 " + rec.Name + " 当前是禁用状态：在它被启用之前，这条授权谁也用不上",
			})
		}
		if !rec.HasPassword {
			warnings = append(warnings, Blocker{
				Code:    NoticeSecretInvalid,
				Message: "账号 " + rec.Name + " 还没有设置 Samba 密码：没设密码的账号无法通过认证",
			})
		}
	}

	capv := e.Capability(ctx)
	if !capv.Available() {
		msg := "本机的 Samba 能力不可用"
		if capv.Reason != "" {
			msg += "：" + capv.Reason
		}
		blockers = append(blockers, Blocker{Code: NoticeSMBUnavailable, Message: msg})
	}

	// 这一条告警在 P08.1 永远出现，而且是**刻意**的：定义、目录与 ACL 都会准备好，
	// 但配置不会写进 Samba，共享因此不对外提供访问。把它做成告警而不是阻塞，
	// 是因为这一阶段的目标正是把定义与权限准备好（P08.2 才接入配置与重载）。
	warnings = append(warnings, Blocker{
		Code: NoticeShareConfigNotApplied,
		Message: "本阶段（P08.1）只保存共享定义、准备目录并应用 POSIX ACL；" +
			"不会写入 Samba 配置、也不会重载服务，这条共享此刻还不对外提供访问",
	})

	return out, blockers, warnings, nil
}

// checkSharePath 检查共享目录路径上的符号链接与非目录成分。
func (e *Engine) checkSharePath(mountPath, dir string) ([]Blocker, []Blocker) {
	var blockers, warnings []Blocker

	fi, err := e.opts.Lstat(mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			blockers = append(blockers, Blocker{
				Code:    NoticeVolumeNotMounted,
				Message: "卷挂载点 " + mountPath + " 不存在",
			})
			return blockers, warnings
		}
		blockers = append(blockers, Blocker{
			Code:    NoticeSharePathInvalid,
			Message: "读不到卷挂载点 " + mountPath + "：" + err.Error(),
		})
		return blockers, warnings
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		blockers = append(blockers, Blocker{
			Code:    NoticeSharePathSymlink,
			Message: "卷挂载点 " + mountPath + " 是一个符号链接；共享路径上的符号链接一律拒绝",
		})
		return blockers, warnings
	}
	if !fi.IsDir() {
		blockers = append(blockers, Blocker{
			Code:    NoticeSharePathNotDirectory,
			Message: "卷挂载点 " + mountPath + " 不是目录",
		})
		return blockers, warnings
	}

	// 相对部分由路径本身算出来，不调用 path/filepath 的相对路径函数：
	// dir 一定是 ShareDir 按“挂载点 + 相对路径”拼出来的，前缀就是判据本身。
	cleanMount := path.Clean(mountPath)
	if dir == cleanMount {
		// 共享整个卷根：挂载点本身就是目录，不需要再往下走。
		return blockers, warnings
	}
	rel := strings.TrimPrefix(dir, cleanMount+"/")
	if rel == dir || rel == "" {
		blockers = append(blockers, Blocker{
			Code:    NoticeSharePathEscapes,
			Message: "共享目录 " + dir + " 不在卷挂载点 " + cleanMount + " 之内",
		})
		return blockers, warnings
	}
	segs := strings.Split(rel, "/")
	cur := mountPath
	for i, seg := range segs {
		cur = path.Join(cur, seg)
		info, err := e.opts.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// 后面的成分都会被创建，不可能是符号链接。
				break
			}
			blockers = append(blockers, Blocker{
				Code:    NoticeSharePathInvalid,
				Message: "读不到 " + cur + "：" + err.Error(),
			})
			return blockers, warnings
		}
		if info.Mode()&os.ModeSymlink != 0 {
			blockers = append(blockers, Blocker{
				Code: NoticeSharePathSymlink,
				Message: "共享路径上的 " + cur + " 是符号链接；" +
					"初期一律不跟随符号链接（避免共享目录被链到卷外）",
			})
			return blockers, warnings
		}
		if !info.IsDir() {
			if i == len(segs)-1 {
				blockers = append(blockers, Blocker{
					Code:    NoticeSharePathNotDirectory,
					Message: "共享路径 " + cur + " 已存在且不是目录",
				})
			} else {
				blockers = append(blockers, Blocker{
					Code:    NoticeSharePathNotDirectory,
					Message: "共享路径上的 " + cur + " 不是目录",
				})
			}
			return blockers, warnings
		}
		if i == len(segs)-1 {
			// 目标目录已存在：看看里面有没有东西（有东西意味着 ACL 会覆盖既有权限）。
			if entries, err := e.opts.ReadDir(cur); err == nil && len(entries) > 0 {
				warnings = append(warnings, Blocker{
					Code: NoticeShareDirectoryNotEmpty,
					Message: "目录 " + cur + " 里已经有 " + fmt.Sprint(len(entries)) +
						" 个条目：这次只会改目录自身的 ACL，不会递归改动里面已有内容的权限",
				})
			}
		}
	}
	return blockers, warnings
}

// assessShareApply 判决 share.apply。
//
// 返回判决与解析出来的现场事实（挂载点、目录、账号名映射）：执行器要用同一份事实，
// 不重新解析一遍。两处各解析一次的话，“预检看到的目录”与“执行时改的目录”
// 就有了分头的机会。
func (e *Engine) assessShareApply(ctx context.Context, p ShareApplyParams) (Assessment, definitionContext, error) {
	out := Assessment{Action: ActionShareApply, NormalizedParams: p}
	if p.Definition == nil {
		out.Blockers = append(out.Blockers, Blocker{Code: NoticeSharePathInvalid, Message: "definition 不能为空"})
		return out, definitionContext{}, nil
	}
	def := *p.Definition

	// 新建与更新的判据只看 share_id 有没有：version 的存在性由它自己校验。
	if p.ShareID == nil {
		if p.Version != nil {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeShareVersionMismatch,
				Message: "新建共享时 version 必须是 null",
			})
		}
	} else {
		if !ValidShareID(*p.ShareID) {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeShareNotFound,
				Message: "share_id 必须是 shr_ 加 32 位小写十六进制，收到 " + *p.ShareID,
			})
			return out, definitionContext{}, nil
		}
		rec, found, err := e.opts.Store.GetShare(ctx, *p.ShareID)
		if err != nil {
			return out, definitionContext{}, err
		}
		if !found {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeShareNotFound,
				Message: "共享 " + *p.ShareID + " 不存在",
			})
			return out, definitionContext{}, nil
		}
		out.TargetID = rec.ID
		out.StateVersion = rec.Version
		if p.Version == nil || strings.TrimSpace(*p.Version) != rec.Version {
			got := "null"
			if p.Version != nil {
				got = *p.Version
			}
			out.Blockers = append(out.Blockers, Blocker{
				Code: NoticeShareVersionMismatch,
				Message: "共享 " + rec.Definition.Name + " 的版本是 " + rec.Version +
					"，调用方手上是 " + got + "（别人刚改过这条共享？）",
			})
		}
	}

	// 同名冲突：名字在登记表里唯一，新建与改名都要检查。
	if existing, found, err := e.opts.Store.GetShareByName(ctx, def.Name); err != nil {
		return out, definitionContext{}, err
	} else if found && (p.ShareID == nil || existing.ID != *p.ShareID) {
		out.Blockers = append(out.Blockers, Blocker{
			Code:    NoticeShareNameTaken,
			Message: "已有同名共享 " + def.Name + "（" + existing.ID + "）：不允许靠重名覆盖它",
		})
	} else if p.ShareID == nil {
		// 新建也要绑定状态版本。可绑定的事实就是“此刻登记表里还没有这条共享”。
		// 名字被别人占走时这里不设：那份判决是 allowed=false，没有版本可绑也不需要
		// （agent 的预检校验只对 allowed 的计划要求版本），而重新判决给出的
		// SHARE_NAME_TAKEN 才是执行端要拦的东西。
		out.StateVersion = absentStateVersion(def.Name)
	}

	dctx, blockers, warnings, err := e.resolveDefinition(ctx, def)
	if err != nil {
		return out, definitionContext{}, err
	}
	out.Blockers = append(out.Blockers, blockers...)
	out.Warnings = append(out.Warnings, warnings...)
	out.TargetID = def.Name
	out.DisplayName = def.Name
	out.IdentityFingerprint = dctx.VolumeUUID

	out.Impacts = append(out.Impacts,
		Impact{Kind: ImpactConfigChange,
			Description: "共享定义会写入代理登记表（配置版本 = 渲染结果的 sha256）"},
		Impact{Kind: ImpactAccessChange,
			Description: "目录 " + dctx.Dir + " 的 POSIX ACL 会被整份替换成这份定义的授权"},
	)
	// 锁按“共享名 + 卷”取：两个人同时改同一个名字或同一个卷上的共享时必须串行。
	// 用名字而不是 id：新建时还没有 id。
	out.Locks = []string{"share:" + def.Name, "volume:" + def.VolumeID}
	sort.Strings(out.Locks)

	out.Allowed = len(out.Blockers) == 0
	return out, dctx, nil
}

// assessShareRef 判决 share.disable / share.delete。
func (e *Engine) assessShareRef(ctx context.Context, action string, p ShareRefParams) (Assessment, error) {
	out := Assessment{Action: action, NormalizedParams: p}
	if !ValidShareID(p.ShareID) {
		out.Blockers = append(out.Blockers, Blocker{
			Code:    NoticeShareNotFound,
			Message: "share_id 必须是 shr_ 加 32 位小写十六进制，收到 " + p.ShareID,
		})
		return out, nil
	}
	rec, found, err := e.opts.Store.GetShare(ctx, p.ShareID)
	if err != nil {
		return out, err
	}
	if !found {
		out.Blockers = append(out.Blockers, Blocker{
			Code:    NoticeShareNotFound,
			Message: "共享 " + p.ShareID + " 不存在",
		})
		return out, nil
	}
	out.TargetID = rec.ID
	out.DisplayName = rec.Definition.Name
	out.StateVersion = rec.Version
	out.IdentityFingerprint = rec.ConfigHash
	if p.Version != rec.Version {
		out.Blockers = append(out.Blockers, Blocker{
			Code: NoticeShareVersionMismatch,
			Message: "共享 " + rec.Definition.Name + " 的版本是 " + rec.Version +
				"，调用方手上是 " + p.Version,
		})
	}

	if action == ActionShareDisable {
		if !rec.Definition.Enabled {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeShareVersionMismatch,
				Message: "共享 " + rec.Definition.Name + " 已经是禁用状态，没有可撤销的授权",
			})
		}
		out.Impacts = append(out.Impacts, Impact{Kind: ImpactAccessChange,
			Description: "共享目录上的 POSIX ACL 会被清空（除 root 之外谁都进不去）"})
	} else {
		out.Impacts = append(out.Impacts, Impact{Kind: ImpactAccessChange,
			Description: "共享登记会被删除，目录上的 ACL 会被清空；" +
				"目录与里面的文件**不会**被删除"})
	}

	capv := e.Capability(ctx)
	if !capv.Available() {
		// 撤销授权只需要 setfacl/getfacl 与目录，不需要 Samba 本身。
		// 缺工具才是阻塞，缺 smbd 不是。
		if capv.Tools.Setfacl == "" || capv.Tools.Getfacl == "" {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeACLToolsMissing,
				Message: "缺少 setfacl/getfacl，无法撤销目录上的访问授权",
			})
		}
	}
	out.Locks = []string{"share:" + rec.Definition.Name, "volume:" + rec.Definition.VolumeID}
	sort.Strings(out.Locks)

	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// assessAccountApply 判决 share.account.apply。
//
// budget 是这次判决里那次系统账号查询的总预算：预检按 HTTP 链路的预算给，
// 执行前复核按任务自己的预算给（见 commands.go 的两条常量）。
func (e *Engine) assessAccountApply(ctx context.Context, p AccountApplyParams, budget time.Duration) (Assessment, error) {
	out := Assessment{Action: ActionAccountApply, NormalizedParams: p}
	out.Blockers = append(out.Blockers, e.capabilityBlockers(ctx)...)
	// 目标身份就是账号名：它在预检与执行之间不会变（改名在这里是被拒绝的），
	// 因此可以用作“还是那个对象”的指纹。
	out.DisplayName = p.Name
	out.TargetID = p.Name
	out.IdentityFingerprint = p.Name

	// nameValid 记下“名字本身已经过不了校验”。它只用于一件事：跳过下面那次
	// getent——名字不合法时系统里有没有同名账号都不会改变判决，而这次观察在
	// 2 vCPU 的真机上实测可以拖到 7 秒（见 Q222），把一条本来必拒的预检变成 500。
	nameValid := true
	if err := ValidateAccountName(p.Name); err != nil {
		nameValid = false
		code, msg := defErrorParts(err)
		out.Blockers = append(out.Blockers, Blocker{Code: code, Message: msg})
	}
	if p.SecretRef != "" && !validSecretRefShape(p.SecretRef) {
		out.Blockers = append(out.Blockers, Blocker{
			Code:    NoticeSecretInvalid,
			Message: "secret_ref 必须是 sec_ 加 32 位小写十六进制（它来自密码秘密通道）",
		})
	}

	if p.AccountID == nil {
		if p.SecretRef == "" {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeSecretInvalid,
				Message: "新建账号必须带 secret_ref：没有密码的 Samba 账号无法认证",
			})
		}
		// 已经有一条受管理记录用这个名字 → 应该更新它，而不是再造一条。
		if rec, found, err := e.opts.Store.GetAccountByName(ctx, p.Name); err != nil {
			return out, err
		} else if found {
			out.Blockers = append(out.Blockers, Blocker{
				Code: NoticeAccountNameTaken,
				Message: "已有受管理账号 " + p.Name + "（" + rec.ID + "）：" +
					"要改它请用 account_id 更新，不要新建同名账号",
			})
		} else {
			// 新建也要绑定状态版本。可绑定的事实就是“此刻登记表里还没有这个名字”。
			// 名字被占时这里不设：那份判决是 allowed=false，不需要版本。
			out.StateVersion = absentStateVersion(p.Name)
		}
		// 系统上已有同名账号但不是受管理的 → 拒绝静默接管。
		// 名字本身已经不合法时跳过：判决已经是“不许”，问系统有没有它既不改判决，
		// 又要把一次可能几秒的 NSS 查询压在预检链路上（真机 P08.1 验收里
		// “Bad_Name”这一条就因为这次多余的查询以 500 收场）。
		//
		// 查询超时是**告警**，不是阻塞，也不是 500：
		//   - 预检只决定“能不能开始”，改不改系统由执行阶段决定；
		//   - 执行阶段会用更长的预算（accountLookupExecTimeout）再问一次；
		//   - 就算那一次也没问出来，useradd 对已存在的名字会失败（退出码 9），
		//     createAccount 直接报错回滚，绝不接管别人的账号。
		// 换句话说，安全性不靠这一条预检；它给的是一条好文案。机器忙到问不出来时，
		// 照实说“没核验”，而不是把一次正常的建账号判成服务内部错误。
		if nameValid {
			exists, err := e.systemUserExists(ctx, p.Name, budget)
			switch {
			case err == nil && exists:
				out.Blockers = append(out.Blockers, Blocker{
					Code: NoticeAccountNotManaged,
					Message: "系统里已经有一个不是本面板创建的账号 " + p.Name +
						"：不会接管它（改名或先由管理员确认后手工处理）",
				})
			case err == nil:
				// 系统上没有这个名字，可以建。
			case errors.Is(err, syscmd.ErrTimeout):
				out.Warnings = append(out.Warnings, Blocker{
					Code: NoticeAccountLookupTimeout,
					Message: "没能在 " + budget.String() + " 内确认系统里是否已有同名账号 " +
						p.Name + "（机器繁忙）：执行时会再核验一次，已存在的账号不会被接管",
				})
			default:
				return out, err
			}
		}
	} else {
		if !ValidAccountID(*p.AccountID) {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeAccountMissing,
				Message: "account_id 必须是 sac_ 加 32 位小写十六进制，收到 " + *p.AccountID,
			})
			return out, nil
		}
		rec, found, err := e.opts.Store.GetAccount(ctx, *p.AccountID)
		if err != nil {
			return out, err
		}
		if !found {
			out.Blockers = append(out.Blockers, Blocker{
				Code:    NoticeAccountMissing,
				Message: "受管理账号 " + *p.AccountID + " 不存在",
			})
			return out, nil
		}
		out.StateVersion = rec.Version
		out.TargetID = rec.ID
		out.DisplayName = rec.Name
		if p.Name != rec.Name {
			out.Blockers = append(out.Blockers, Blocker{
				Code: NoticeAccountNameInvalid,
				Message: "不支持修改账号名（" + rec.Name + " → " + p.Name + "）：" +
					"改名要连带改系统账号、Samba 账号与目录上的 ACL，请删除后重建",
			})
		}
		if p.SecretRef == "" {
			out.Warnings = append(out.Warnings, Blocker{
				Code:    NoticeShareAccountPasswordKept,
				Message: "这次没有换密码，沿用账号 " + rec.Name + " 现有的 Samba 密码",
			})
		}
	}

	if p.AccountID == nil || p.SecretRef != "" {
		out.Impacts = append(out.Impacts, Impact{Kind: ImpactAccessChange,
			Description: "Samba 账号的密码会被设置成秘密通道里那一份（明文只经标准输入传递）"})
	}
	if p.Disabled {
		out.Impacts = append(out.Impacts, Impact{Kind: ImpactAccessChange,
			Description: "账号会在 Samba passdb 里被禁用（D 标志），但不会被删除"})
	}
	out.Locks = []string{"account:" + p.Name}
	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// assessAccountDelete 判决 share.account.delete。
func (e *Engine) assessAccountDelete(ctx context.Context, p AccountRefParams) (Assessment, error) {
	out := Assessment{Action: ActionAccountDelete, NormalizedParams: p}
	out.Blockers = append(out.Blockers, e.capabilityBlockers(ctx)...)
	if !ValidAccountID(p.AccountID) {
		out.Blockers = append(out.Blockers, Blocker{
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
		out.Blockers = append(out.Blockers, Blocker{
			Code:    NoticeAccountMissing,
			Message: "受管理账号 " + p.AccountID + " 不存在",
		})
		return out, nil
	}
	out.TargetID = rec.ID
	out.DisplayName = rec.Name
	out.StateVersion = rec.Version
	// 身份指纹与授权路径同一个取法：账号名在预检与执行之间不会变（改名被拒），
	// 它就是“还是那个对象”的可复核凭据。少了这一行，删除的预检是 allowed=true
	// 却绑定不上任何目标，validatePlanResult 会把它判成 500（真机 P08.1 验收 §9
	// 两次删除都撞在这上面：接口回“服务内部错误”，账号自然没被删掉）。
	out.IdentityFingerprint = rec.Name

	// 仍然被共享定义引用就拒绝：删掉账号之后目录 ACL 里会留下一个指向不存在 UID 的
	// 条目，而那从界面上完全看不见。
	shares, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return out, err
	}
	var referenced []string
	for _, s := range shares {
		for _, u := range s.Definition.SMBUsers {
			if u.AccountID == p.AccountID {
				referenced = append(referenced, s.Definition.Name)
				break
			}
		}
	}
	if len(referenced) > 0 {
		sort.Strings(referenced)
		out.Blockers = append(out.Blockers, Blocker{
			Code: NoticeAccountHasShares,
			Message: "账号 " + rec.Name + " 仍被这些共享引用：" + strings.Join(referenced, "、") +
				"；先把它们从授权名单里去掉（并重新应用共享），再删账号",
		})
	}

	out.Impacts = append(out.Impacts, Impact{Kind: ImpactAccessChange,
		Description: "Samba 账号与同名 Linux 账号会被删除（home 是 " + DefaultHome +
			"，没有数据会跟着消失）"})
	out.Locks = []string{"account:" + rec.Name}
	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// absentStateVersion 是“目标此刻还不存在”这件事的状态快照。
//
// 新建共享与新建账号都必须给出一个非空的状态版本（agent 的预检校验要求
// allowed=true 的计划绑定状态快照：执行端要靠它确认“我要动的还是预检时那个东西”）。
// 对一个还没被创建的对象，可绑定的事实只有一个——此刻还没有它。执行端会重新判决
// 一次，这中间要是有人建了同名对象，判决层就会给出阻塞项（SHARE_NAME_TAKEN /
// ACCOUNT_NAME_TAKEN / ACCOUNT_NOT_MANAGED），版本核对在这一层是第二道门。
//
// 把名字放进字符串而不是只写 "absent"：日志与审计里能直接看出这是哪个对象的快照。
func absentStateVersion(name string) string {
	return "absent:" + name
}

// systemUserExists 用 getent passwd 问系统“有没有这个账号”。
//
// 用 getent 而不是读 /etc/passwd：getent 走的是 nsswitch，与 smbd 解析用户名时
// 看的是同一份事实。读文件在接了 LDAP 的机器上会给出“不存在”，而 smbd 却能找到它。
//
// budget 是这次查询的总预算，由调用方按阶段给：预检按 HTTP 链路的预算（3 秒），
// 执行前复核按任务自己的预算（20 秒）。同一个问题、同一份事实，但两处的等待能力
// 根本不同——用同一个数只会让其中一处要么太紧、要么白等。
//
// 超时重试一次：这是幂等的读，重试没有副作用，而一次超时更可能只是机器忙。
// 真机 P08.1 验收里撞到过一次：同一个名字在预检里是十几毫秒，4 秒后的执行前复核
// 却把 3 秒预算跑满（宿主机同时在跑 Go 构建，虚拟机只有 2 个 vCPU），于是一条本来
// 正确的建账号操作以 500 收场，后面整片断言跟着倒下。
//
// 预算用尽时返回的错误**包裹 syscmd.ErrTimeout**：调用方据此区分“问了，系统说没有”
// 与“没问出结果”。后者不是“存在”，也不是“不存在”。
func (e *Engine) systemUserExists(ctx context.Context, name string, budget time.Duration) (bool, error) {
	var err error
	for attempt := 0; attempt < accountLookupAttempts; attempt++ {
		var res syscmd.Result
		res, err = e.run(ctx, syscmdSpecGetent("passwd", name), lookupAttemptTimeout(budget))
		if err == nil {
			return strings.TrimSpace(res.Stdout) != "", nil
		}
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			// getent 用退出码 2 表示“没找到”；1 表示数据库不支持查询，
			// 那是这台机器的 NSS 配置问题，不能当成“账号不存在”。
			if exitErr.ExitCode == 2 {
				return false, nil
			}
			return false, fmt.Errorf("shares: 查询系统账号失败（getent 退出码 %d）: %w",
				exitErr.ExitCode, err)
		}
		// 只有超时才值得再问一次：退出码与权限这类失败重试也是同一个结果。
		if !errors.Is(err, syscmd.ErrTimeout) {
			break
		}
	}
	return false, fmt.Errorf("shares: 查询系统账号失败: %w", err)
}

// defErrorParts 把 DefinitionError 拆成码与文案；其它错误统一归到形状错误。
func defErrorParts(err error) (string, string) {
	var de *DefinitionError
	if errors.As(err, &de) {
		return de.Code, de.Message
	}
	return NoticeSharePathInvalid, err.Error()
}

// validSecretRefShape 判断秘密引用的形状（sec_ + 32 位小写十六进制）。
//
// 只查形状，不查存在性：存在性属于执行器——预检阶段去解析秘密明文会让一次只读的
// 预检碰到密码，那是没必要的暴露面。
func validSecretRefShape(ref string) bool {
	const prefix = "sec_"
	if len(ref) != len(prefix)+32 || !strings.HasPrefix(ref, prefix) {
		return false
	}
	for i := len(prefix); i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
