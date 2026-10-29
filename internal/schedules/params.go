package schedules

// 动作参数（CONTRACTS.md 第 5 节的参数表）。
//
// 字段名与契约逐字对齐：schedule_id（新建为 null）、version、definition。
// 这里只做形状与基本形态的校验；"这个定义现在能不能落地"属于预检。

import (
	"fmt"
	"strings"
)

// ApplyParams 是 schedule.apply 的参数。
type ApplyParams struct {
	// ScheduleID 为 nil 表示新建；非 nil 表示改一条已有的计划。
	ScheduleID *string `json:"schedule_id"`
	// Version 是乐观并发令牌；新建时必须为 0。
	Version    int64      `json:"version"`
	Definition Definition `json:"definition"`
}

// Validate 校验参数自身的形态。
func (p ApplyParams) Validate() error {
	if p.Definition.Name == "" && p.Definition.Kind == "" {
		return fmt.Errorf("definition 不能为空")
	}
	if p.ScheduleID == nil {
		if p.Version != 0 {
			return fmt.Errorf("新建计划时 version 必须为 0（收到 %d）", p.Version)
		}
		return nil
	}
	id := strings.TrimSpace(*p.ScheduleID)
	if !ValidID(id) {
		return fmt.Errorf("schedule_id 必须是 sch_ 加 32 位小写十六进制")
	}
	if p.Version < 1 {
		return fmt.Errorf("修改计划时 version 必须为正整数（收到 %d）", p.Version)
	}
	return nil
}

// ID 返回目标计划 id（新建时为空串）。
func (p ApplyParams) ID() string {
	if p.ScheduleID == nil {
		return ""
	}
	return strings.TrimSpace(*p.ScheduleID)
}

// DeleteParams 是 schedule.delete 的参数。
type DeleteParams struct {
	ScheduleID string `json:"schedule_id"`
	Version    int64  `json:"version"`
}

// Validate 校验参数。
func (p DeleteParams) Validate() error {
	if !ValidID(strings.TrimSpace(p.ScheduleID)) {
		return fmt.Errorf("schedule_id 必须是 sch_ 加 32 位小写十六进制")
	}
	if p.Version < 1 {
		return fmt.Errorf("version 必须为正整数（收到 %d）", p.Version)
	}
	return nil
}

// TriggerParams 是 POST /internal/v1/schedules/:id/trigger 的请求体。
//
// 只有触发 ID：走哪条路由由计划自己决定，调用方给不了 action 或 params
// （CONTRACTS.md 第 15 节）。
type TriggerParams struct {
	TriggerID string `json:"trigger_id"`
}
