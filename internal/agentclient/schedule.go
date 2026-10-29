package agentclient

// 计划任务与受管理脚本的读取端（P12.4）。
//
// 与备份定义同一条规矩：**代理库里那一份是唯一真相**，面板不存第二份。所以
// GET /schedules、GET /schedules/:id、GET /scripts、GET /scripts/:id 四个只读
// 接口只能从这里取。面板若另存一份，两边一定会有一刻不一致，而那一刻正好是
// "界面说 03:30 会跑、systemd 说不会"。
//
// 写路径不在这里：计划的建立/修改/删除走 schedule.apply 与 schedule.delete 的
// 预检 → 任务，脚本的建立/修改/删除走 script.apply 与 script.delete，脚本的执行
// 走 script.run。它们是系统写操作，要再认证、要落任务、要审计——没有"直接写"的
// 客户端方法，也就没有绕过那条链路的路。
//
// 时间字段一律用 time.Time 解码（代理发的是 RFC3339）：调用方不该自己再解析一遍。
// 只读接口把代理的形状原样带过来，包括 systemd 的**原话**（unit_file_state /
// active_state）——那是事实，不是面板的解读。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"upanel/internal/schedules"
)

// 代理镜像路由的前缀。
const (
	// SchedulesPath 是计划任务镜像的内部路径前缀。
	SchedulesPath = "/internal/v1/schedules"
	// ScriptsPath 是受管理脚本镜像的内部路径前缀。
	ScriptsPath = "/internal/v1/scripts"

	// ScheduleQueryTimeout 是读计划/脚本镜像时的调用上限，比通用 10 秒宽。
	//
	// 列表的代价**随计划条数增长**：每条启用计划都要 fork 一次
	// `systemd-analyze calendar` 算下次触发、一次 `systemctl show` 读单元事实。
	// 24.04 验收机上 calendar 单次 250—330 毫秒（`/bin/true` 本身 39 毫秒，是这台
	// 机器就慢），串行组装 12 条计划读出 10144 毫秒——通用 10 秒上限先到期，Web
	// 把一个**健康的**代理报成 AGENT_UNAVAILABLE 并回 503。这不是代理挂了，是预算
	// 选错了（P12.4 验收 §11 的原样失败）。
	//
	// 代理侧已改成 4 路并发组装（schedules.ViewsConcurrency），现场典型值降到 2 秒
	// 上下；30 秒是留给"计划很多"的余量（按 0.25 秒/条估算约 130 条），仍远低于 Web
	// 的 120 秒 WriteTimeout。
	ScheduleQueryTimeout = 30 * time.Second
)

// Notice 是一条阻塞项或提示。
//
// 不复用 agent.Notice：客户端不该 import 代理进程的包（那是实现端）。形状与
// CONTRACTS 里的 code/message 一致，就照这个形状解。
type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Schedule 是一条计划的现场：意图与事实并排放。
type Schedule struct {
	ID string `json:"id"`
	// Name / Kind / TargetID 来自定义，重复一份是为了让列表页不必再拆 definition。
	Name     string               `json:"name"`
	Kind     string               `json:"kind"`
	TargetID string               `json:"target_id"`
	Def      schedules.Definition `json:"definition"`
	// Enabled 是**意图**（下一次 apply 会不会写 enabled 的单元）。
	Enabled bool  `json:"enabled"`
	Version int64 `json:"version"`
	// TimerRef 是 systemd 定时单元的引用名；未安装时为空。
	TimerRef  string    `json:"timer_ref"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Installed / UnitFileState / ActiveState 是**事实**：这台机器上有没有这套单元、
	// systemd 说它是不是 enabled、现在是不是 active。三者都不是面板的推断。
	Installed     bool   `json:"installed"`
	UnitFileState string `json:"unit_file_state"`
	ActiveState   string `json:"active_state"`
	// NextElapse 是下次触发的绝对时刻；读不到时为零值（代理那边是 omitempty）。
	NextElapse time.Time `json:"next_elapse"`
	// Blockers 是"现在就算触发了也跑不起来"的原因，永不为 nil。
	Blockers []Notice `json:"blockers"`
}

// ScheduleTrigger 是一次触发记录。
type ScheduleTrigger struct {
	TriggerID   string    `json:"trigger_id"`
	SlotAt      time.Time `json:"slot_at"`
	Action      string    `json:"action"`
	OperationID string    `json:"operation_id"`
	// Outcome 是触发当时的判决闭集：triggered / blocked / duplicate / failed。
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// ScheduleDetail 是单条计划加上它的触发记录。
type ScheduleDetail struct {
	Schedule Schedule          `json:"schedule"`
	Triggers []ScheduleTrigger `json:"triggers"`
}

// Script 是一条受管理脚本。列表不带 Content，详情才带。
type Script struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentHash string    `json:"content_hash"`
	Interpreter string    `json:"interpreter"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Content 只在 GetScript 时有值；列表里为空。
	Content string `json:"content,omitempty"`
}

// ScheduledOperation 是代理自己产生的一条定时操作（P12.4）。
//
// 面板凭它**如实**落一条预检与一条任务：动作、规范化参数与哈希、目标、会话哈希、
// 阶段、结果。少一个字段面板就得猜，猜出来的任务记录比没有还坏。
type ScheduledOperation struct {
	OperationID string `json:"operation_id"`
	Action      string `json:"action"`
	// PreflightRef 是代理侧那份预检的引用：面板要凭它落一条本地预检记录，
	// 因为 jobs.preflight_id 是 NOT NULL 外键。
	PreflightRef string          `json:"preflight_ref"`
	Params       json.RawMessage `json:"params"`
	ParamsHash   string          `json:"params_hash"`
	StateVersion string          `json:"state_version"`
	Target       json.RawMessage `json:"target"`
	// SessionHash 是定时触发的会话指纹（代理侧派生的，不是任何人的会话）。
	SessionHash string          `json:"session_hash"`
	Stage       string          `json:"stage"`
	Step        string          `json:"step"`
	Result      json.RawMessage `json:"result"`
	Error       json.RawMessage `json:"error"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  time.Time       `json:"finished_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// OperationsPath 是操作集合的内部路径（P12.4 起 GET 用于定时操作对账）。
const OperationsPath = "/internal/v1/operations"

// scheduledOperationListEnvelope 是代理的响应体形状。
type scheduledOperationListEnvelope struct {
	Operations []ScheduledOperation `json:"operations"`
}

// ListScheduledOperations 问代理：从 since 起定时触发跑了哪些操作（按创建时间升序）。
//
// actor 固定为 scheduler：代理不接受别的值（它只回答这一个问题，不提供列全部操作的
// 通道）。since 为零值时从最早一条开始；limit 由代理封顶。
func (c *Client) ListScheduledOperations(ctx context.Context, since time.Time, limit int) ([]ScheduledOperation, error) {
	q := url.Values{}
	q.Set("actor", "scheduler")
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	data, err := c.do(ctx, http.MethodGet, OperationsPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out scheduledOperationListEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("代理返回的定时操作列表不是合法 JSON: %w", err)}
	}
	return out.Operations, nil
}

// scheduleEnvelope / scriptEnvelope 是代理的响应体形状。
type scheduleListEnvelope struct {
	Schedules []Schedule `json:"schedules"`
}

type scriptListEnvelope struct {
	Scripts []Script `json:"scripts"`
}

type scriptEnvelope struct {
	Script Script `json:"script"`
}

// schedulePath 返回单条计划的路径。
func schedulePath(id string) string { return SchedulesPath + "/" + url.PathEscape(id) }

// scriptPath 返回单条脚本的路径。
func scriptPath(id string) string { return ScriptsPath + "/" + url.PathEscape(id) }

// ListSchedules 列出全部计划。
func (c *Client) ListSchedules(ctx context.Context) ([]Schedule, error) {
	data, err := c.do(ctx, http.MethodGet, SchedulesPath, nil)
	if err != nil {
		return nil, err
	}
	var out scheduleListEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("代理返回的计划列表不是合法 JSON: %w", err)}
	}
	return out.Schedules, nil
}

// GetSchedule 读取一条计划及其最近的触发记录。
func (c *Client) GetSchedule(ctx context.Context, id string) (ScheduleDetail, error) {
	if id == "" {
		return ScheduleDetail{}, errors.New("agentclient: schedule_id 不能为空")
	}
	data, err := c.do(ctx, http.MethodGet, schedulePath(id), nil)
	if err != nil {
		return ScheduleDetail{}, err
	}
	var out ScheduleDetail
	if err := json.Unmarshal(data, &out); err != nil {
		return ScheduleDetail{}, &UnavailableError{Err: fmt.Errorf("代理返回的计划详情不是合法 JSON: %w", err)}
	}
	return out, nil
}

// ListScripts 列出全部受管理脚本（只有元数据，不含正文）。
func (c *Client) ListScripts(ctx context.Context) ([]Script, error) {
	data, err := c.do(ctx, http.MethodGet, ScriptsPath, nil)
	if err != nil {
		return nil, err
	}
	var out scriptListEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("代理返回的脚本列表不是合法 JSON: %w", err)}
	}
	return out.Scripts, nil
}

// GetScript 读取一条脚本，含正文。
func (c *Client) GetScript(ctx context.Context, id string) (Script, error) {
	if id == "" {
		return Script{}, errors.New("agentclient: script_id 不能为空")
	}
	data, err := c.do(ctx, http.MethodGet, scriptPath(id), nil)
	if err != nil {
		return Script{}, err
	}
	var out scriptEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return Script{}, &UnavailableError{Err: fmt.Errorf("代理返回的脚本详情不是合法 JSON: %w", err)}
	}
	return out.Script, nil
}
