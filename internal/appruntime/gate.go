package appruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"upanel/internal/apps"
	"upanel/internal/containers"
	"upanel/internal/operations"
	"upanel/internal/services"
	"upanel/internal/storage"
)

// 依赖卷阻塞项代码（预检里展示给管理员的固定枚举，CONTRACTS.md 第 8.4 节）。
const (
	// CodeVolumeOffline：依赖卷此刻没有挂载（或挂载失败）。
	CodeVolumeOffline = "VOLUME_OFFLINE"
	// CodeVolumeMissing：依赖卷不在代理的受管理卷列表里。
	CodeVolumeMissing = "VOLUME_MISSING"
	// CodeVolumeIdentityChanged：卷在，但 UUID 与登记的不一致（换了一块盘）。
	CodeVolumeIdentityChanged = "VOLUME_IDENTITY_CHANGED"
	// CodeVolumeCheckUnavailable：无法确认依赖卷状态（代理不可用）。
	CodeVolumeCheckUnavailable = "VOLUME_CHECK_UNAVAILABLE"
)

// Gate 是启动路径上的依赖门禁（P09.4）。
//
// 它只做一件事：当一个**被面板管理**的应用要启动/重启时，检查它的依赖卷是不是
// 都挂好了。命中就把这条预检钉成不允许——于是 POST /jobs 会得到 409
// DEPENDENCY_BLOCKED，而不是先启动一次、再靠人工发现容器起在了空目录上。
//
// 为什么门禁在 Web 而不是代理：依赖关系（哪个应用吃哪个卷）只存在 Web 库里，
// 代理那边只有共享的依赖（internal/agent/sharedeps.go），它不知道也不能知道
// 应用元数据。代理仍然是执行与状态版本复核的唯一位置，这里只补“代理看不到的那半边”。
type Gate struct {
	// Apps 用来按绑定反查应用。为 nil 时门禁不介入（没有元数据就没有依赖关系）。
	Apps *apps.Service
	// Volumes 读受管理卷的观测状态。为 nil 表示没法确认——那时启动会被拦下，
	// 因为“没检查”和“检查通过”必须说成两句话。
	Volumes VolumeSource
}

// controlParams 是 service.control 与 container.control 的参数交集。
//
// 只解这三个字段：门禁需要的是“操作什么、做什么”，多余字段留给代理的严格解码去拒。
type controlParams struct {
	Unit        string `json:"unit"`
	ContainerID string `json:"container_id"`
	Operation   string `json:"operation"`
}

// Blockers 实现 operations.Gate：返回额外的阻塞项（空表示不拦）。
//
// 返回 error 只用于“查不下去”的内部故障（数据库）。依赖卷读不到不算故障：
// 那是一种必须如实告诉用户的判决（VOLUME_CHECK_UNAVAILABLE），不是 500。
func (g *Gate) Blockers(ctx context.Context, action string, params json.RawMessage) ([]operations.Notice, error) {
	if g == nil || g.Apps == nil {
		return nil, nil
	}
	typ, binding, op, ok := controlTarget(action, params)
	if !ok {
		return nil, nil
	}
	// stop 不拦：停机在任何状态下都是安全的，拦它只会让一个起在空目录上的应用
	// 没法被停下来。
	if op != services.OpStart && op != services.OpRestart {
		return nil, nil
	}

	app, found, err := g.Apps.FindByBinding(ctx, typ, binding)
	if err != nil {
		return nil, err
	}
	if !found {
		// 面板不认识这个单元/容器：它没有依赖关系可守，也就没有理由拦。
		return nil, nil
	}
	return g.volumeBlockers(ctx, app)
}

// volumeBlockers 逐个核对依赖卷的观测状态。
func (g *Gate) volumeBlockers(ctx context.Context, app apps.App) ([]operations.Notice, error) {
	if len(app.VolumeDependencies) == 0 {
		return nil, nil
	}
	if g.Volumes == nil {
		return []operations.Notice{{
			Code:    CodeVolumeCheckUnavailable,
			Message: "无法确认依赖卷状态（未配置代理连接）：这次启动没有经过依赖卷检查，已阻止",
		}}, nil
	}
	volumes, err := g.Volumes.Volumes(ctx)
	if err != nil {
		return []operations.Notice{{
			Code:    CodeVolumeCheckUnavailable,
			Message: "无法确认依赖卷状态（" + err.Error() + "）：启动依赖没有经过检查，已阻止",
		}}, nil
	}
	byID := make(map[string]storage.Volume, len(volumes))
	for _, v := range volumes {
		byID[v.ID] = v
	}

	out := make([]operations.Notice, 0, len(app.VolumeDependencies))
	for _, id := range app.VolumeDependencies {
		v, ok := byID[id]
		if !ok {
			out = append(out, operations.Notice{
				Code:    CodeVolumeMissing,
				Message: "依赖卷 " + id + " 不在受管理卷列表里（可能已被删除或退出管理）",
			})
			continue
		}
		switch v.ObservedState {
		case storage.VolumeStateMounted:
			// 挂着的卷还要确认挂的是不是原来那块盘：UUID 不符时 state 是 mismatch，
			// 已经在下面的分支里处理；这里只剩“挂载路径可读”这一条不值得再拦。
		case storage.VolumeStateMismatch:
			out = append(out, operations.Notice{
				Code:    CodeVolumeIdentityChanged,
				Message: "依赖卷 " + volumeLabel(v) + " 的文件系统 UUID 与登记的不一致，可能换了一块盘",
			})
		default:
			out = append(out, operations.Notice{
				Code:    CodeVolumeOffline,
				Message: "依赖卷 " + volumeLabel(v) + " 当前未挂载（observed_state=" + v.ObservedState + "）" + observedReason(v),
			})
		}
	}
	return out, nil
}

// controlTarget 从动作名与参数里取出“要操作谁、做什么”。
//
// 参数用的是代理返回的**规范化**参数（Web 侧 canonical JSON），不是前端原样上传的那份：
// 代理可能已经去空格、统一大小写，门禁按它看到的同一份参数做判断才不会出现
// “代理执行 A、门禁检查 B”。
func controlTarget(action string, params json.RawMessage) (apps.Type, apps.Binding, string, bool) {
	var p controlParams
	dec := json.NewDecoder(bytes.NewReader(params))
	if err := dec.Decode(&p); err != nil {
		// 参数不是对象或字段类型不对：交给代理的严格解码去拒，门禁不在这里补一次校验。
		return "", apps.Binding{}, "", false
	}
	op := strings.ToLower(strings.TrimSpace(p.Operation))
	switch action {
	case services.ActionControl:
		unit := strings.TrimSpace(p.Unit)
		if unit == "" {
			return "", apps.Binding{}, "", false
		}
		return apps.TypeSystemd, apps.Binding{Unit: unit}, op, true
	case containers.ActionControl:
		id := strings.TrimSpace(p.ContainerID)
		if !containers.ValidContainerID(id) {
			return "", apps.Binding{}, "", false
		}
		return apps.TypeDocker, apps.Binding{ContainerID: id}, op, true
	}
	return "", apps.Binding{}, "", false
}

// volumeLabel 是给人看的卷名：有显示名用显示名，否则用标识。
func volumeLabel(v storage.Volume) string {
	if v.DisplayName != "" {
		return fmt.Sprintf("%s（%s）", v.DisplayName, v.ID)
	}
	if v.MountPath != "" {
		return fmt.Sprintf("%s（%s）", v.MountPath, v.ID)
	}
	return v.ID
}

// observedReason 把卷自己的观测原因附在后面（“盘没插好”和“文件系统坏了”是两回事）。
func observedReason(v storage.Volume) string {
	if v.ObservedError == "" {
		return ""
	}
	return "：" + v.ObservedError
}
