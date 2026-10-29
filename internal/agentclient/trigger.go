package agentclient

// 定时触发的客户端（P12.4）。
//
// 这条调用与别的不同：它的对端不是 Web，而是 root 跑的 upanelctl。代理按路径形状
// 把 /internal/v1/schedules/<id>/trigger 划成 root-only，Web 账号调它会被 gate
// 直接拒掉——这是 SO_PEERCRED 给出的 UID 决定的，不靠调用方自律。
//
// 触发 ID 由调用方算，代理再算一次并核对（见 internal/schedules 的 TriggerID）：
// 幂等的锚是那个哈希，不是"我保证只跑一次"。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// TriggerInput 是一次定时触发的输入。
type TriggerInput struct {
	ScheduleID string
	// TriggerID 由 schedules.TriggerID(scheduleID, slot) 算出。
	TriggerID string
}

// TriggerResult 是代理的答复。
type TriggerResult struct {
	ScheduleID  string `json:"schedule_id"`
	TriggerID   string `json:"trigger_id"`
	OperationID string `json:"operation_id"`
	SlotAt      string `json:"slot_at"`
	Action      string `json:"action"`
	Outcome     string `json:"outcome"`
	// MaxRuntimeSeconds 是这次触发允许跑多久（计划定义里的数，代理随答复给出）。
	// --wait 的等待上限由它算，不再另读一次计划——root 读计划是 403。
	MaxRuntimeSeconds int `json:"max_runtime_seconds"`
	// Duplicate 表示这个时间槽已经触发过；OperationID 是当时那一次。
	Duplicate bool `json:"duplicate"`
}

// TriggerSchedule 请求代理按计划执行一次定时触发。
func (c *Client) TriggerSchedule(ctx context.Context, in TriggerInput) (TriggerResult, error) {
	if in.ScheduleID == "" {
		return TriggerResult{}, errors.New("agentclient: schedule_id 不能为空")
	}
	if in.TriggerID == "" {
		return TriggerResult{}, errors.New("agentclient: trigger_id 不能为空")
	}
	body := struct {
		TriggerID string `json:"trigger_id"`
	}{TriggerID: in.TriggerID}

	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/schedules/"+in.ScheduleID+"/trigger", body)
	if err != nil {
		return TriggerResult{}, err
	}
	var out TriggerResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return TriggerResult{}, &UnavailableError{Err: fmt.Errorf("触发响应无法解析: %w", err)}
	}
	if out.OperationID == "" {
		return TriggerResult{}, &UnavailableError{Err: errors.New("触发响应缺少 operation_id")}
	}
	return out, nil
}

// GetTriggerRun 读回一条计划自己触发的那次操作（--wait 的状态来源）。
//
// 为什么不是 GetOperation：`/internal/v1/operations/<id>` 是 Web 账号的路由，
// root 调它是 403——而调用这里的就是 root 跑的定时单元。代理把读收窄成
// `/internal/v1/schedules/<id>/trigger-runs/<op>`，并在处理端核对那条操作确实是
// 这条计划触发的；不匹配时回 404（与"操作不存在"同形）。
func (c *Client) GetTriggerRun(ctx context.Context, scheduleID, operationID string) (Operation, error) {
	if scheduleID == "" {
		return Operation{}, errors.New("agentclient: schedule_id 不能为空")
	}
	if operationID == "" {
		return Operation{}, errors.New("agentclient: operation_id 不能为空")
	}
	raw, err := c.do(ctx, http.MethodGet,
		"/internal/v1/schedules/"+scheduleID+"/trigger-runs/"+operationID, nil)
	if err != nil {
		return Operation{}, err
	}
	return decodeOperation(raw)
}
