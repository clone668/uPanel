package agent

// 存储写动作（P07.1）。
//
// 这一轮登记四个动作的**参数解码与预检**，不登记执行器：Execute 为 nil，
// 提交时得到 501 NOT_IMPLEMENTED（ErrNoExecutor），而不是一个永远跑不起来的任务。
// 破坏性执行器在 P07.2（GPT/分区）与 P07.3（格式化）登记，届时会把这里的判决
// 在执行前重算一遍再动手（operations.go 的 revalidate）。
//
// 为什么先把“只会拒绝”的动作登记进来：PLAN.md 第 9.2 节的整盘锁、系统设备保护、
// 挂载/swap/holders 依赖判断、身份指纹与状态版本绑定，全都在预检阶段生效。把它们
// 先做出来并在真实设备上验证“该拒的都拒了”，才轮得到写磁盘的那一步。
//
// 四个动作都只允许 admin（由 Web 层保证），都需要 5 分钟内的再认证（CONTRACTS.md
// 第 5 节 R 列全为“是”），并且都不可取消：分区表一动手就没有“安全取消”可言。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"upanel/internal/storage"
)

// 存储动作的确认短语首词（CONTRACTS.md 第 4.1 节：初始化、格式化、删除分区必须要求短语）。
const (
	phraseVerbInit   = "INIT"
	phraseVerbFormat = "FORMAT"
	phraseVerbDelete = "DELETE"
)

// storageAssessor 是存储写动作的判决来源。
//
// 生产实现是 *storage.Collector：它持有最近一次拓扑快照与预检需要的探针
// （签名读取、占用检查），因此判决用的是与 storage.topology 同一份快照——
// 预检绑定的 StateVersion 与用户看到的页面是同一个版本号。
type storageAssessor interface {
	Assess(ctx context.Context, action string, params any) (storage.Assessment, error)
}

// RegisterStorageActions 登记四个存储写动作（只登记解码与预检）。
func RegisterStorageActions(reg *Registry, src storageAssessor) error {
	for _, a := range []Action{
		newPartitionTableInitializeAction(src),
		newPartitionCreateAction(src),
		newPartitionDeleteAction(src),
		newFilesystemFormatAction(src),
	} {
		if err := reg.RegisterAction(a); err != nil {
			return fmt.Errorf("代理登记 %s 失败: %w", a.Name, err)
		}
	}
	return nil
}

// newPartitionTableInitializeAction 登记 storage.partition_table.initialize。
func newPartitionTableInitializeAction(src storageAssessor) Action {
	return Action{
		Name: storage.ActionPartitionTableInitialize,
		Decode: func(raw json.RawMessage) (any, error) {
			var p storage.PartitionTableInitializeParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.DeviceID = strings.TrimSpace(p.DeviceID)
			p.TableType = strings.ToLower(strings.TrimSpace(p.TableType))
			if err := validateStorageDeviceID(p.DeviceID); err != nil {
				return nil, err
			}
			// 只接受 gpt：PLAN.md 第 9.1 节初期不修改 MBR 分区表，也没有别的类型可选。
			if !storage.ValidTableType(p.TableType) {
				return nil, fmt.Errorf("%w: table_type 只能是 gpt，收到 %q", ErrParamsInvalid, p.TableType)
			}
			return p, nil
		},
		Plan: storagePlanner(src, storage.ActionPartitionTableInitialize),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: true,
			PhraseVerb:     phraseVerbInit,
			Cancellable:    false,
		},
		// Execute 留空：P07.1 只做拒绝逻辑。见本文件开头的说明。
	}
}

// newPartitionCreateAction 登记 storage.partition.create。
func newPartitionCreateAction(src storageAssessor) Action {
	return Action{
		Name: storage.ActionPartitionCreate,
		Decode: func(raw json.RawMessage) (any, error) {
			var p storage.PartitionCreateParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.DeviceID = strings.TrimSpace(p.DeviceID)
			p.Name = strings.TrimSpace(p.Name)
			if err := validateStorageDeviceID(p.DeviceID); err != nil {
				return nil, err
			}
			// 区间合法性在预检里判（要看盘上的空闲区），这里只拦明显无意义的值。
			if p.SizeBytes == 0 {
				return nil, fmt.Errorf("%w: size_bytes 必须大于 0", ErrParamsInvalid)
			}
			if err := storage.ValidatePartitionName(p.Name); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: storagePlanner(src, storage.ActionPartitionCreate),
		Policy: ActionPolicy{
			RequiresReauth: true,
			// 新建分区不覆盖任何东西（只在空闲区里加一条条目），因此不需要短语。
			RequiresPhrase: false,
			Cancellable:    false,
		},
	}
}

// newPartitionDeleteAction 登记 storage.partition.delete。
func newPartitionDeleteAction(src storageAssessor) Action {
	return Action{
		Name: storage.ActionPartitionDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p storage.PartitionDeleteParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.DeviceID = strings.TrimSpace(p.DeviceID)
			if err := validateStorageDeviceID(p.DeviceID); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: storagePlanner(src, storage.ActionPartitionDelete),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: true,
			PhraseVerb:     phraseVerbDelete,
			Cancellable:    false,
		},
	}
}

// newFilesystemFormatAction 登记 storage.filesystem.format。
func newFilesystemFormatAction(src storageAssessor) Action {
	return Action{
		Name: storage.ActionFilesystemFormat,
		Decode: func(raw json.RawMessage) (any, error) {
			var p storage.FilesystemFormatParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.DeviceID = strings.TrimSpace(p.DeviceID)
			p.Filesystem = strings.ToLower(strings.TrimSpace(p.Filesystem))
			// 标签原样保留（首尾空格是合法的盘标），只拒绝控制字符与超长。
			if err := validateStorageDeviceID(p.DeviceID); err != nil {
				return nil, err
			}
			if !storage.ValidFilesystem(p.Filesystem) {
				return nil, fmt.Errorf("%w: filesystem 只能是 ext4 或 xfs，收到 %q",
					ErrParamsInvalid, p.Filesystem)
			}
			if err := storage.ValidateLabel(p.Filesystem, p.Label); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: storagePlanner(src, storage.ActionFilesystemFormat),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: true,
			PhraseVerb:     phraseVerbFormat,
			Cancellable:    false,
		},
	}
}

// storagePlanner 把判决映射成预检计划。
//
// 四个动作的映射一模一样，所以只有一份：Allowed/Blockers/Warnings/Impacts/Locks 直接
// 来自 storage.Assess，规范化参数就是 Decode 之后的那份（预检绑定它，执行时按它解码）。
func storagePlanner(src storageAssessor, action string) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		assessment, err := src.Assess(ctx, action, in.Params)
		if err != nil {
			// 判决本身出错（参数类型不对、动作名不认识）是编程错误，不是业务拒绝。
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

// validateStorageDeviceID 校验内核设备名。
//
// 设备名会成为 /sys/block/<id> 与 /dev/<id> 的路径成分，也可能进入审计与日志，
// 因此只接受内核会用的字符（字母数字与 . _ -），拒绝任何路径分隔符与 ".."。
// 这是“形状层”的拒绝：真正的身份校验在判决里按 sysfs 与指纹做。
func validateStorageDeviceID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: device_id 不能为空", ErrParamsInvalid)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("%w: device_id 不能是 %q", ErrParamsInvalid, id)
	}
	if len(id) > 64 {
		return fmt.Errorf("%w: device_id 过长（%d 字节）", ErrParamsInvalid, len(id))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%w: device_id 只能是内核设备名（字母数字与 . _ -），收到 %q",
				ErrParamsInvalid, id)
		}
	}
	return nil
}
