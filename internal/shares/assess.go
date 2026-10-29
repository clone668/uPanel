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
func (e *Engine) resolveDefinition(ctx context.Context, def ShareDefinition, shareID string) (definitionContext, []Blocker, []Blocker, error) {
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

	// NFS 到这里就分头了：它没有 Samba 账号那一层，也没有 hosts allow 那条本机例外
	// （NFS 的客户端是按网段与 IP 判的，127.0.0.1 只有名单里写了才放行）。
	if def.Protocol == ProtocolNFS {
		nb, nw, err := e.exportsAssessment(ctx, def, dir, shareID)
		if err != nil {
			return out, blockers, warnings, err
		}
		blockers = append(blockers, nb...)
		warnings = append(warnings, nw...)
		return out, blockers, warnings, nil
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

	// 允许网段挡不住本机：Samba 的 hosts allow **永远放行** 127.0.0.1，除非在
	// hosts deny 里点名（man 5 smb.conf, hosts allow 一节原文：
	// "the localhost address 127.0.0.1 will always be allowed access unless
	// specifically denied by a hosts deny option"）。真机验证过：一份
	// allowed_cidrs=["10.0.0.0/24"] 的共享，本机 smbclient 照样列得出目录。
	//
	// 不阻塞、也不偷偷加 hosts deny：加 deny 会把面板自己依赖的本地通道（smbpasswd、
	// smbclient 自检）一起关掉，那是一次没人要求的语义改变。说清楚就够了。
	if len(def.AllowedCIDRs) > 0 && !cidrsCoverLoopback(def.AllowedCIDRs) {
		warnings = append(warnings, Blocker{
			Code: NoticeHostsAllowLoopback,
			Message: "allowed_cidrs 里没有 127.0.0.0/8：Samba 仍然会放行本机（127.0.0.1）——" +
				"这是 Samba 的文档行为，这条名单管不住本机连进来的访问",
		})
	}

	// 现状说明：这条共享**此刻**在不在 Samba 配置里。
	//
	// P08.1 这里是无条件告警（那时配置永远写不进去）；P08.2 起它变成对真实状态的
	// 描述——新建或改名的共享此刻确实还不在配置里，改完之后才在。它不阻塞，
	// 因为 apply 这个动作本身就是为了改变这个状态；说清楚是为了让人在确认页上
	// 分得清“现在”与“做完之后”。
	if !e.shareCurrentlyApplied(ctx, def.Name) {
		warnings = append(warnings, Blocker{
			Code: NoticeShareConfigNotApplied,
			Message: "这条共享此刻不在 Samba 配置里：应用会写入托管配置并重载，" +
				"之后才对外提供访问",
		})
	}

	return out, blockers, warnings, nil
}

// exportsAssessment 判决 NFS 导出这一半（P14.4）。
//
// 与 Samba 那一半的区别不是“少做几件事”，而是判据换了：
//   - 阻塞项只有两条来源：能力不可用（缺 exportfs）与路径冲突（同一条路径被导出
//     两次，会让 exportfs 整份刷新失败，连别人的导出一起停摆）；
//   - “此刻在不在导出表里”“服务在不在跑”都是告警：应用这个动作本身就是来改变
//     前者的，而后者不影响配置正确性（服务起来之后导出就生效）。
func (e *Engine) exportsAssessment(ctx context.Context, def ShareDefinition, dir, shareID string) ([]Blocker, []Blocker, error) {
	var blockers, warnings []Blocker

	capv := e.Capability(ctx)
	if capv.NFS.Status != CapabilityAvailable {
		msg := "本机的 NFS 能力不可用"
		if capv.NFS.Reason != "" {
			msg += "：" + capv.NFS.Reason
		}
		blockers = append(blockers, Blocker{Code: NoticeNFSUnavailable, Message: msg})
	}

	// 冲突检查。读不到别人的导出文件时不能断言“没有冲突”，但那也不该阻塞：
	// 执行期会用同一份数据再查一次，而且那一步失败会自动回滚。
	cb, cw, err := e.exportPathConflicts(ctx, []ManagedExport{{Name: def.Name, Dir: dir}})
	if err != nil {
		warnings = append(warnings, Blocker{Code: NoticeExportsUnparsable, Message: err.Error()})
	} else {
		blockers = append(blockers, cb...)
		warnings = append(warnings, cw...)
	}

	// 同目录：面板自己的另一条共享是否已经占着这个目录。
	//
	// 这条与上面那条是同一个危害的两半，却各有各的来源：上面比的是“别人的导出文件”，
	// 这里比的是“登记表里的别的共享”。漏掉任何一半，用户都会在预检里看到 allowed，
	// 然后在执行时收到一句 exportfs 的 duplicated。
	if def.Enabled {
		names, err := e.sharesSharingDir(ctx, def, dir, shareID)
		if err != nil {
			return nil, nil, err
		}
		if len(names) > 0 {
			blockers = append(blockers, Blocker{
				Code:    NoticeExportPathConflict,
				Message: describeSameDirShares(map[string][]string{dir: names}),
			})
		}
	}

	// 此刻在不在导出表里。禁用的共享不需要这条：应用它正是为了把它摘掉。
	if def.Enabled && capv.NFS.Status == CapabilityAvailable {
		want := ManagedExport{Dir: dir, CIDRs: def.AllowedCIDRs, ReadOnly: def.ReadOnly}
		table, err := e.readExportTable(ctx)
		switch {
		case err != nil:
			warnings = append(warnings, Blocker{
				Code: NoticeExportsUnreadable,
				Message: "读不到当前的实际导出表（exportfs -v）：" + err.Error() +
					"；这条共享此刻有没有在被导出，无法确认",
			})
		default:
			if why := VerifyExport(table, want); why != "" {
				warnings = append(warnings, Blocker{
					Code: NoticeExportsNotApplied,
					Message: "这条共享此刻不在导出表里（" + why + "）：应用会写进 " +
						e.paths().Exports + " 并让 exportfs 重新读一遍",
				})
			}
		}
	}

	if capv.NFS.ServiceState != ServiceStateActive {
		msg := "nfs-server 现在不是 active"
		if capv.NFS.ServiceState == ServiceStateUnknown {
			msg = "问不出 nfs-server 的运行状态"
		}
		warnings = append(warnings, Blocker{
			Code:    NoticeNFSServiceInactive,
			Message: msg + "：导出会照写并被 exportfs 接受，但客户端连不上，执行结果只能是 accepted",
		})
	}
	return blockers, warnings, nil
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
			continue
		}
		// 上级目录：共享账号不是它的属主、也不在它的属组里，能不能穿过去只看 other 的 x 位。
		// 缺 x 时客户端连目标目录都到不了（真机上是 put 的 ACCESS_DENIED，而目标目录的
		// ACL 一条不差——这种“每条证据都对、就是不通”最费时间）。
		if info.Mode().Perm()&0o001 == 0 {
			warnings = append(warnings, Blocker{
				Code: NoticeSharePathNotTraversable,
				Message: "上级目录 " + cur + " 没有 other 的 x 位，共享账号会被挡在这一级；" +
					"应用时会补上 x（只加穿越，不加读取与列出）",
			})
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

	// 新建时没有 id：同目录冲突的判定按名字排除自己（登记表里名字唯一）。
	var assessingID string
	if p.ShareID != nil {
		assessingID = *p.ShareID
	}
	dctx, blockers, warnings, err := e.resolveDefinition(ctx, def, assessingID)
	if err != nil {
		return out, definitionContext{}, err
	}
	out.Blockers = append(out.Blockers, blockers...)
	out.Warnings = append(out.Warnings, warnings...)

	// 配置侧的判决：现有 smb.conf 里有没有同名段、工具齐不齐、全局项会不会被改写。
	// 它只看、不写：预检做的每一个观察都必须是可重复的。
	//
	// NFS 不走这一段：它没有 smb.conf 可读，同名判断对导出也没有意义（导出的身份是
	// 目录，不是名字）。它的现场判决在 resolveDefinition 的 exportsAssessment 里做完。
	if def.Protocol != ProtocolNFS {
		configBlockers, configWarnings := e.configAssessment(ctx, def)
		out.Blockers = append(out.Blockers, configBlockers...)
		out.Warnings = append(out.Warnings, configWarnings...)
	}

	out.TargetID = def.Name
	out.DisplayName = def.Name
	out.IdentityFingerprint = dctx.VolumeUUID

	if def.Protocol == ProtocolNFS {
		out.Impacts = append(out.Impacts,
			Impact{Kind: ImpactConfigChange,
				Description: "共享定义会写入代理登记表（配置版本 = 导出行的 sha256）"},
			Impact{Kind: ImpactConfigChange,
				Description: "托管导出文件 " + DefaultExportsFile + " 会被整份重写（只含面板自己那几条），" +
					"随后 exportfs 重新读取导出表；nfs-server 不在运行时导出仍然写下去，但客户端连不上"},
		)
		// 锁按“共享名 + 卷 + 导出文件”取。导出文件是整份重写的，并发写会让后一个
		// 把前一个的导出行覆盖掉；它与 Samba 那把锁分开，因为两者写的是不同文件，
		// 互相排队只会让“改一份 NFS 共享”白等一次 SMB 应用。
		out.Locks = []string{"share:" + def.Name, "volume:" + def.VolumeID, ExportsLockName}
		sort.Strings(out.Locks)
		out.Allowed = len(out.Blockers) == 0
		return out, dctx, nil
	}

	out.Impacts = append(out.Impacts,
		Impact{Kind: ImpactConfigChange,
			Description: "共享定义会写入代理登记表（配置版本 = 渲染结果的 sha256）"},
		Impact{Kind: ImpactConfigChange,
			Description: "托管配置 " + ManagedConfigPath + " 会被整份重写，" +
				"主配置里会多出一行 include（已接入时不重复添加），随后重载 Samba"},
		Impact{Kind: ImpactAccessChange,
			Description: "目录 " + dctx.Dir + " 的 POSIX ACL 会被整份替换成这份定义的授权"},
	)
	// 锁按“共享名 + 卷 + 全局配置”取：两个人同时改同一个名字或同一个卷上的共享、
	// 或者同时改任何两份共享，都必须串行——托管配置文件是整份重写的，
	// 并发写入会让后一个把前一个的段覆盖掉（它读到的“现状”里还没有前一个）。
	// 用名字而不是 id：新建时还没有 id。
	out.Locks = []string{"share:" + def.Name, "volume:" + def.VolumeID, ConfigLockName}
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

	// NFS 的撤销在这一步就分头：判据、影响面、锁全都不一样。
	if rec.Definition.Protocol == ProtocolNFS {
		return e.assessShareRefNFS(ctx, action, rec, p, out), nil
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
	// 摘除配置需要写文件与重载。工具缺失在这里**不阻塞**：停用一份共享是安全方向的
	// 操作，先收回 ACL 也能立刻止住访问（目录权限收回到属主）。但必须说清代价：
	// 配置可能还留在文件里，Samba 也可能还在按旧配置服务。
	var missingTools []string
	if capv.Tools.Testparm == "" {
		missingTools = append(missingTools, "testparm")
	}
	if capv.Tools.Smbcontrol == "" {
		missingTools = append(missingTools, "smbcontrol")
	}
	if len(missingTools) > 0 {
		out.Warnings = append(out.Warnings, Blocker{
			Code: NoticeConfigNotWritable,
			Message: "缺少 " + strings.Join(missingTools, "/") + "：摘除仍会执行（先收回目录权限），" +
				"但配置改动可能无法被 Samba 校验或重载",
		})
	}
	out.Locks = []string{"share:" + rec.Definition.Name, "volume:" + rec.Definition.VolumeID, ConfigLockName}
	sort.Strings(out.Locks)

	out.Allowed = len(out.Blockers) == 0
	return out, nil
}

// assessShareRefNFS 判决 NFS 共享的 share.disable / share.delete（P14.4）。
//
// 单独一段而不是在 SMB 那条里塞条件，因为三件东西换掉了：
//   - **影响面**：没有 POSIX ACL 可收。拿掉导出行之后网段内就再也连不上，
//     目录权限原样留着——说成“ACL 会被清空”是一句会让人以为目录已经锁死的假话；
//   - **阻塞判据**：缺 exportfs 才是阻塞（摘不掉那一行、也刷不了表，客户端还连得上）；
//     缺 setfacl 与它无关；
//   - **锁**：托管导出文件是整份重写的，所以要取 ExportsLockName。取 ConfigLockName
//     看起来“也算拿了把锁”，但它管的是 smb.conf——两个 NFS 操作的锁一旦错开，
//     同一份导出文件就可能被并发重写，后一个把前一个的导出行抹掉。
func (e *Engine) assessShareRefNFS(ctx context.Context, action string, rec ShareRecord, p ShareRefParams, out Assessment) Assessment {
	if action == ActionShareDisable && !rec.Definition.Enabled {
		out.Blockers = append(out.Blockers, Blocker{
			Code:    NoticeShareVersionMismatch,
			Message: "共享 " + rec.Definition.Name + " 已经是禁用状态，导出表里本来就没有它",
		})
	}
	if action == ActionShareDisable {
		out.Impacts = append(out.Impacts, Impact{Kind: ImpactAccessChange,
			Description: "导出行会从 " + DefaultExportsFile + " 摘掉并让 exportfs 重新读一遍：" +
				"网段内的客户端将连不上（可能还挂着旧句柄，卸载重挂才断）；目录与里面的文件不会被动"})
	} else {
		out.Impacts = append(out.Impacts, Impact{Kind: ImpactAccessChange,
			Description: "共享登记会被删除，导出行会从 " + DefaultExportsFile + " 摘掉；" +
				"目录与里面的文件**不会**被删除"})
	}

	capv := e.Capability(ctx)
	if capv.NFS.Status != CapabilityAvailable {
		msg := "本机的 NFS 能力不可用"
		if capv.NFS.Reason != "" {
			msg += "：" + capv.NFS.Reason
		}
		out.Blockers = append(out.Blockers, Blocker{
			Code:    NoticeNFSUnavailable,
			Message: msg + "；摘不掉导出行也刷不了表，客户端会继续按旧导出连进来",
		})
	}
	// 服务没在跑不阻塞：摘一行配置是安全方向的动作，而且没有 exportfs 才是硬阻塞。
	if capv.NFS.ServiceState != ServiceStateActive {
		out.Warnings = append(out.Warnings, Blocker{
			Code: NoticeNFSServiceInactive,
			Message: "nfs-server 现在不是 active（" + capv.NFS.ServiceState +
				"）：导出表仍会被刷新，但客户端此刻本来就连不上",
		})
	}

	out.Locks = []string{"share:" + rec.Definition.Name, "volume:" + rec.Definition.VolumeID, ExportsLockName}
	sort.Strings(out.Locks)
	out.Allowed = len(out.Blockers) == 0
	return out
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
