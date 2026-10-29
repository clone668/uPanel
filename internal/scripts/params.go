package scripts

// 动作参数（CONTRACTS.md 第 5 节的参数表）。
//
//	script.apply   script_id（新建为 null）、version、name、content
//	script.delete  script_id、version
//	script.run     script_id、version、max_runtime_seconds
//
// 这里没有 path 字段，也没有 interpreter：路径由 id 拼、解释器是常量。
// 少一个字段，就少一条"调用方指定它不该指定的东西"的路。

import (
	"fmt"
	"strings"
)

// ApplyParams 是 script.apply 的参数。
type ApplyParams struct {
	// ScriptID 为 nil 表示新建。
	ScriptID *string `json:"script_id"`
	Version  int64   `json:"version"`
	Name     string  `json:"name"`
	Content  string  `json:"content"`
}

// Validate 校验参数形态。
//
// 名字与正文交给 Input.Validate：一份"能落盘的东西"长什么样只有一处定义，
// 解码这一步问的就是同一个问题。长度、控制字符、UTF-8、NUL 都是形状问题，
// 在这里拒掉比让它进预检表更好——预检表不该先收下一份 10 MiB 的正文。
func (p ApplyParams) Validate() error {
	if err := (Input{Name: p.Name, Content: p.Content}).Validate(); err != nil {
		return err
	}
	if p.ScriptID == nil {
		if p.Version != 0 {
			return fmt.Errorf("新建脚本时 version 必须为 0（收到 %d）", p.Version)
		}
		return nil
	}
	if !ValidID(strings.TrimSpace(*p.ScriptID)) {
		return fmt.Errorf("script_id 必须是 scr_ 加 32 位小写十六进制")
	}
	if p.Version < 1 {
		return fmt.Errorf("修改脚本时 version 必须为正整数（收到 %d）", p.Version)
	}
	return nil
}

// ID 返回目标脚本 id（新建时为空串）。
func (p ApplyParams) ID() string {
	if p.ScriptID == nil {
		return ""
	}
	return strings.TrimSpace(*p.ScriptID)
}

// DeleteParams 是 script.delete 的参数。
type DeleteParams struct {
	ScriptID string `json:"script_id"`
	Version  int64  `json:"version"`
}

// Validate 校验参数。
func (p DeleteParams) Validate() error {
	if !ValidID(strings.TrimSpace(p.ScriptID)) {
		return fmt.Errorf("script_id 必须是 scr_ 加 32 位小写十六进制")
	}
	if p.Version < 1 {
		return fmt.Errorf("version 必须为正整数（收到 %d）", p.Version)
	}
	return nil
}

// RunParams 是 script.run 的参数。
type RunParams struct {
	ScriptID string `json:"script_id"`
	Version  int64  `json:"version"`
	// MaxRuntimeSeconds 是这次运行的时间上限；上限由计划定义带入。
	MaxRuntimeSeconds int `json:"max_runtime_seconds"`
}

// Validate 校验参数。
func (p RunParams) Validate() error {
	if !ValidID(strings.TrimSpace(p.ScriptID)) {
		return fmt.Errorf("script_id 必须是 scr_ 加 32 位小写十六进制")
	}
	if p.Version < 1 {
		return fmt.Errorf("version 必须为正整数（收到 %d）", p.Version)
	}
	if p.MaxRuntimeSeconds < 1 || p.MaxRuntimeSeconds > MaxRuntimeSecondsLimit {
		return fmt.Errorf("max_runtime_seconds 必须在 1—%d 之间（收到 %d）", MaxRuntimeSecondsLimit, p.MaxRuntimeSeconds)
	}
	return nil
}
