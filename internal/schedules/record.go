package schedules

// 计划与触发记录的形态（P12.4）。
//
// 这两个结构放在纯逻辑包里，代理库的仓储直接返回它们（agentstore 里是类型别名）：
// 同一份形状只写一次，就不会出现"库里存的是 A 字段、动作层读的是 A' 字段"这种
// 只能靠人眼对齐的错位。

import (
	"time"
)

// Record 是一条计划。
type Record struct {
	ID       string
	Name     string
	Kind     Kind
	TargetID string
	// Definition 是规范化后的完整定义；definition_json 存的就是它。
	Definition Definition
	// DefinitionKey 是定义指纹，用于判断"要不要重写单元"。
	DefinitionKey string
	// TimerRef 是已落地的定时器单元名；空串表示此刻这台机器上没有它的单元。
	TimerRef string
	// Enabled 是**意图**；是否真的在跑由 TimerRef 与 systemd 的事实共同决定。
	Enabled   bool
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TriggerRecord 是一次触发。
type TriggerRecord struct {
	TriggerID   string
	ScheduleID  string
	SlotAt      time.Time
	Action      string
	OperationID string
	// Outcome 是触发当时的判决：triggered / blocked / duplicate / failed。
	Outcome   string
	Reason    string
	CreatedAt time.Time
}

// 触发判决的闭集。
const (
	TriggerOutcomeTriggered = "triggered"
	TriggerOutcomeBlocked   = "blocked"
	TriggerOutcomeDuplicate = "duplicate"
	TriggerOutcomeFailed    = "failed"
)
