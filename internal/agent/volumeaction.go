package agent

// 卷管理动作（P07.4）：导入、挂载、卸载、改配置、退出管理。
//
// 五个动作与四个存储写动作共用同一套骨架（参数解码 → 预检 → 执行 → 结果核验），
// 但判决来源不同：设备动作看“这台设备能不能写”，卷动作看“这条登记记录 + fstab 里那一行
// + 现场挂载状态”三者是否一致。因此判决走 storage.AssessVolume，执行仍走同一个
// ExecuteWrite 入口（storage 侧按动作名分流到卷执行器）。
//
// 策略（CONTRACTS.md 第 5 节 R 列，逐条对照）：
//   - storage.volume.import    R=否：只登记，不格式化，不动 fstab 之外的东西
//   - storage.volume.mount     R=否：可逆，且要核对 UUID
//   - storage.volume.unmount   R=是：会让正在用这个卷的服务失败
//   - storage.volume.configure R=是：改写开机行为（fstab 条目）
//   - storage.volume.forget    R=是：撤销管理记录并删除 fstab 条目
//
// 五个动作都不需要确认短语（短语只用于初始化、格式化、删除分区这类不可逆操作），
// 都不可取消：fstab 的替换与 mount 的发起都是“一动手就要走到结果”的动作，
// 中途取消只会留下说不出状态的东西。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"upanel/internal/storage"
)

// volumeSource 是登记卷动作所需的能力：判决 + 执行。
//
// 与 storageSource 分开声明，是为了让“只实现了设备判决”的接线在编译期就失败，
// 而不是在第一次点击“导入卷”时返回 501。
type volumeSource interface {
	AssessVolume(ctx context.Context, action string, params any) (storage.Assessment, error)
	ExecuteWrite(ctx context.Context, req storage.WriteRequest) (storage.WriteReport, error)
}

// RegisterVolumeActions 登记五个卷管理动作。
func RegisterVolumeActions(reg *Registry, src volumeSource) error {
	for _, a := range []Action{
		newVolumeImportAction(src),
		newVolumeMountAction(src),
		newVolumeUnmountAction(src),
		newVolumeConfigureAction(src),
		newVolumeForgetAction(src),
	} {
		if err := reg.RegisterAction(a); err != nil {
			return fmt.Errorf("代理登记 %s 失败: %w", a.Name, err)
		}
	}
	return nil
}

// volumePlanner 把卷判决映射成预检计划（与 storagePlanner 同样的映射规则）。
func volumePlanner(src interface {
	AssessVolume(ctx context.Context, action string, params any) (storage.Assessment, error)
}, action string) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		assessment, err := src.AssessVolume(ctx, action, in.Params)
		if err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrPlanFailed, err)
		}
		blockers := make([]Notice, 0, len(assessment.Blockers))
		for _, b := range assessment.Blockers {
			blockers = append(blockers, Notice{Code: b.Code, Message: b.Message})
		}
		warnings := make([]Notice, 0, len(assessment.Warnings))
		for _, w := range assessment.Warnings {
			warnings = append(warnings, Notice{Code: w.Code, Message: w.Message})
		}
		impacts := make([]Impact, 0, len(assessment.Impacts))
		for _, i := range assessment.Impacts {
			impacts = append(impacts, Impact{Kind: i.Kind, Description: i.Description})
		}
		return Plan{
			NormalizedParams: in.Params,
			Target: Target{
				DisplayName:         assessment.DisplayName,
				IdentityFingerprint: assessment.IdentityFingerprint,
				StateVersion:        assessment.StateVersion,
			},
			Allowed:  assessment.Allowed,
			Blockers: blockers,
			Warnings: warnings,
			Impacts:  impacts,
			Locks:    assessment.Locks,
		}, nil
	}
}

// newVolumeImportAction 登记 storage.volume.import。
func newVolumeImportAction(src volumeSource) Action {
	return Action{
		Name: storage.ActionVolumeImport,
		Decode: func(raw json.RawMessage) (any, error) {
			var p storage.VolumeImportParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.DeviceID = strings.TrimSpace(p.DeviceID)
			p.Slug = strings.TrimSpace(p.Slug)
			p.DisplayName = strings.TrimSpace(p.DisplayName)
			if err := validateStorageDeviceID(p.DeviceID); err != nil {
				return nil, err
			}
			// 卷名派生挂载点，因此它的字符集就是“能出现在 /srv/upanel/volumes 下”的集合。
			// 在这一层拒绝，比在写 fstab 的路径上再拒绝早得多。
			if err := storage.ValidateSlug(p.Slug); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			if err := storage.ValidateDisplayName(p.DisplayName); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: volumePlanner(src, storage.ActionVolumeImport),
		Policy: ActionPolicy{
			RequiresReauth: false,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: storageExecutor(src, storage.ActionVolumeImport),
	}
}

// newVolumeMountAction 登记 storage.volume.mount。
func newVolumeMountAction(src volumeSource) Action {
	return Action{
		Name: storage.ActionVolumeMount,
		Decode: func(raw json.RawMessage) (any, error) {
			p, err := decodeVolumeParams(raw)
			if err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: volumePlanner(src, storage.ActionVolumeMount),
		Policy: ActionPolicy{
			RequiresReauth: false,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: storageExecutor(src, storage.ActionVolumeMount),
	}
}

// newVolumeUnmountAction 登记 storage.volume.unmount。
func newVolumeUnmountAction(src volumeSource) Action {
	return Action{
		Name: storage.ActionVolumeUnmount,
		Decode: func(raw json.RawMessage) (any, error) {
			return decodeVolumeParams(raw)
		},
		Plan: volumePlanner(src, storage.ActionVolumeUnmount),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: storageExecutor(src, storage.ActionVolumeUnmount),
	}
}

// newVolumeForgetAction 登记 storage.volume.forget。
func newVolumeForgetAction(src volumeSource) Action {
	return Action{
		Name: storage.ActionVolumeForget,
		Decode: func(raw json.RawMessage) (any, error) {
			return decodeVolumeParams(raw)
		},
		Plan: volumePlanner(src, storage.ActionVolumeForget),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: storageExecutor(src, storage.ActionVolumeForget),
	}
}

// newVolumeConfigureAction 登记 storage.volume.configure。
func newVolumeConfigureAction(src volumeSource) Action {
	return Action{
		Name: storage.ActionVolumeConfigure,
		Decode: func(raw json.RawMessage) (any, error) {
			var p storage.VolumeConfigureParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.VolumeID = strings.TrimSpace(p.VolumeID)
			p.Version = strings.TrimSpace(p.Version)
			if err := validateVolumeParams(p.VolumeID, p.Version); err != nil {
				return nil, err
			}
			// 四个选项都是可选的：全空表示“按当前配置重写一遍受管块”，
			// 这是修好“有人手工改了块内条目”的手段，不是空操作。
			return p, nil
		},
		Plan: volumePlanner(src, storage.ActionVolumeConfigure),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: storageExecutor(src, storage.ActionVolumeConfigure),
	}
}

// decodeVolumeParams 解 mount/unmount/forget 的公共参数。
func decodeVolumeParams(raw json.RawMessage) (storage.VolumeParams, error) {
	var p storage.VolumeParams
	if err := DecodeParams(raw, &p); err != nil {
		return storage.VolumeParams{}, err
	}
	p.VolumeID = strings.TrimSpace(p.VolumeID)
	p.Version = strings.TrimSpace(p.Version)
	if err := validateVolumeParams(p.VolumeID, p.Version); err != nil {
		return storage.VolumeParams{}, err
	}
	return p, nil
}

// validateVolumeParams 校验卷标识与版本号的形状。
//
// 卷标识会进 URL（/storage/volumes/:id）、日志与审计，因此只接受 vol_ + 32 位小写十六进制。
// 版本是单调计数器，只接受十进制数字：它是乐观并发的令牌，不该是一个可以随手编的字符串。
func validateVolumeParams(volumeID, version string) error {
	if !storage.ValidVolumeID(volumeID) {
		return fmt.Errorf("%w: volume_id 必须是 vol_ 加 32 位小写十六进制，收到 %q",
			ErrParamsInvalid, volumeID)
	}
	if err := validateVersionToken(version); err != nil {
		return err
	}
	return nil
}

// validateVersionToken 校验版本号形状。
func validateVersionToken(version string) error {
	if version == "" {
		return fmt.Errorf("%w: version 不能为空（它来自当前卷记录，用来防止覆盖别人刚做的变更）",
			ErrParamsInvalid)
	}
	if len(version) > 20 {
		return fmt.Errorf("%w: version 过长（%d 字节）", ErrParamsInvalid, len(version))
	}
	for i := 0; i < len(version); i++ {
		if version[i] < '0' || version[i] > '9' {
			return fmt.Errorf("%w: version 只能是十进制数字，收到 %q", ErrParamsInvalid, version)
		}
	}
	return nil
}
