package accounts

// 受限 sudo 模板：闭集、渲染、读回（P14.5）。
//
// PLAN.md 第 19 节：sudo 权限变更使用受约束模板并以 visudo 校验，**不接受任意
// sudoers 文本**。CONTRACTS.md 第 5 节：sudo_rule.apply 的参数只有 account_id、
// version、template_id——没有自由度留给“命令”“参数”“单元”。
//
// 于是模板必须是“不带参数、不留通配符”的固定命令行。这不是保守，是唯一安全的选择：
// sudoers 里的 `*` 是按**单个参数**做通配的，`journalctl -u *` 看上去只读，实际
// 允许 `journalctl -u x --vacuum-size=1G`（真的会删日志）。既然合同不给单元参数，
// 模板就只到“整机状态”“最近若干行日志”这一档，更窄的粒度留给以后的模板。

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"
)

// SudoTemplate 是一个固定模板。
type SudoTemplate struct {
	// ID 是对外暴露的模板标识（闭集）。
	ID string
	// DisplayName 给人看。
	DisplayName string
	// Purpose 说明它能做什么，进页面与审计摘要。
	Purpose string
	// Commands 是允许的命令行，每一条都是绝对路径 + 固定参数，逐条对应 sudoers
	// 里的一行。**不允许出现通配符与 shell 元字符**，由 TestSudoTemplatesAreFixed
	// 守着。
	Commands []string
}

// sudoTemplates 是模板闭集。
//
// 两条都是只读：status 只是看，journalctl -n 只是读最近若干行。这一版**故意**不提供
// 启停服务的 sudo：那会绕过面板自己的服务动作（保护名单、依赖检查、审计摘要），
// 也会让人绕开面板去改系统状态。
var sudoTemplates = []SudoTemplate{
	{
		ID:          "system_status",
		DisplayName: "查看整机状态",
		Purpose:     "只读：systemctl status，不加 --lines 之外的任何参数",
		Commands:    []string{"/usr/bin/systemctl status --no-pager"},
	},
	{
		ID:          "journal_recent",
		DisplayName: "查看最近日志",
		Purpose:     "只读：journalctl 最近 200 行，不落盘、不过滤单元",
		Commands:    []string{"/usr/bin/journalctl -n 200 --no-pager"},
	},
}

// SudoTemplates 返回模板表的副本（顺序固定，供页面与文档使用）。
func SudoTemplates() []SudoTemplate {
	out := make([]SudoTemplate, len(sudoTemplates))
	copy(out, sudoTemplates)
	return out
}

// SudoTemplateIDs 返回模板标识的排序列表。
func SudoTemplateIDs() []string {
	out := make([]string, 0, len(sudoTemplates))
	for _, t := range sudoTemplates {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}

// LookupSudoTemplate 按标识取模板；不在闭集里就是错误，绝不回落到默认模板。
func LookupSudoTemplate(id string) (SudoTemplate, error) {
	for _, t := range sudoTemplates {
		if t.ID == id {
			return t, nil
		}
	}
	return SudoTemplate{}, newDefError(NoticeSudoTemplateInvalid,
		"sudo 模板 %q 不在受支持的清单里（可用：%s）；这一版不接受任意 sudoers 文本",
		id, strings.Join(SudoTemplateIDs(), "、"))
}

// SudoRule 是一条要落盘的规则。
type SudoRule struct {
	// ID 是规则标识 srule_<32 位小写十六进制>。
	ID string
	// AccountName 是受管理账号的系统名。
	AccountName string
	// Template 是选中的模板（已校验）。
	Template SudoTemplate
}

// sudoersFilePrefix 是 sudoers.d 里属于面板的文件前缀。
//
// sudoers.d 会**忽略带点或波浪号的文件名**，所以这里连点都不能有；账号名本身已经
// 被 ValidateName 限成 [a-z0-9_-]，拼上去天然安全。
const sudoersFilePrefix = "upanel-"

// SudoersPath 返回这条规则在 sudoers.d 里的文件路径。
func SudoersPath(dir, accountName string) string {
	if dir == "" {
		dir = "/etc/sudoers.d"
	}
	return strings.TrimSuffix(dir, "/") + "/" + sudoersFilePrefix + accountName
}

// claimMarker 是认领标记的前缀。
//
// 与共享、导出的认领是同一个思路：文件里没有这一行，就说明它不是面板写的，
// 面板既不覆盖也不删除它。
const claimMarker = "# upanel:"

// RenderSudoers 渲染 sudoers 文件内容，并返回它的 sha256（规则版本）。
//
// 三种违规在这里再拦一次（定义校验已经拦过，但规则是先落库再渲染的，中间隔着一次
// 写库）：账号名形状、模板不在闭集、模板里有通配符或 shell 元字符。
func RenderSudoers(rule SudoRule) ([]byte, string, error) {
	if err := ValidateName(rule.AccountName); err != nil {
		return nil, "", err
	}
	if !ValidSudoRuleID(rule.ID) {
		return nil, "", newDefError(NoticeSudoTemplateInvalid,
			"规则标识形状不对（期望 %s + 32 位小写十六进制），收到 %q", sudoRuleIDPrefix, rule.ID)
	}
	t, err := LookupSudoTemplate(rule.Template.ID)
	if err != nil {
		return nil, "", err
	}
	var b strings.Builder
	b.WriteString("# 由 uPanel 生成（P14.5），整份重写，手改会被覆盖。\n")
	b.WriteString(claimMarker + rule.ID + "\n")
	b.WriteString("# 模板：" + t.ID + "（" + t.DisplayName + "）——" + t.Purpose + "\n")
	for _, cmd := range t.Commands {
		if err := validateSudoCommand(cmd); err != nil {
			return nil, "", err
		}
		b.WriteString(rule.AccountName + " ALL = (root) NOPASSWD: " + cmd + "\n")
	}
	content := []byte(b.String())
	sum := sha256.Sum256(content)
	return content, hex.EncodeToString(sum[:]), nil
}

// validateSudoCommand 拒绝带通配符或 shell 元字符的命令行。
//
// 这一层是“模板写错了”的兜底，不是给人用的校验：模板是代码里的常量，改它要过
// TestSudoTemplatesAreFixed。但如果哪天有人在模板里写了 `*`，这一层会让所有 sudo
// 动作直接失败，而不是悄悄放行一条能删日志的规则。
func validateSudoCommand(cmd string) error {
	if !strings.HasPrefix(cmd, "/") {
		return newDefError(NoticeSudoTemplateInvalid,
			"模板里的命令必须以绝对路径开头，收到 %q", cmd)
	}
	for _, bad := range []string{"*", "?", "[", "]", "|", "&", ";", "`", "$", "\n", "\r", ">", "<"} {
		if strings.Contains(cmd, bad) {
			return newDefError(NoticeSudoTemplateInvalid,
				"模板里的命令含有通配符或 shell 元字符 %q（%s）；sudoers 的通配符可以跨参数，等于放开了别的子命令",
				bad, cmd)
		}
	}
	// 程序本身不能是 shell、解释器、提权包装器或任意文件写入工具。
	//
	// 这条不是防“模板写错”，而是防“以后有人觉得给个 sh 更方便”：`sudo /bin/sh`
	// 一行就等于 root shell，前面所有的模板约束都成了摆设。systemctl / journalctl
	// 不在名单里——它们被参数锁死了（sudoers 的精确参数匹配），这正是模板要用的东西。
	program := cmd
	if idx := strings.IndexByte(program, ' '); idx > 0 {
		program = program[:idx]
	}
	base := path.Base(program)
	if forbiddenPrograms[base] {
		return newDefError(NoticeSudoTemplateInvalid,
			"模板里不许直接授权 %s——它等于一条通用的 root 执行路径（%s）", program, cmd)
	}
	return nil
}

// forbiddenPrograms 是绝不能出现在 sudo 模板里的程序（按 basename 比较）。
var forbiddenPrograms = map[string]bool{
	// shell：一条就是 root shell。
	"sh": true, "bash": true, "dash": true, "rbash": true, "zsh": true,
	"ksh": true, "csh": true, "tcsh": true, "fish": true,
	// 解释器：同上，只是绕一下。
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"php": true, "lua": true, "awk": true, "sed": true,
	// 提权包装与命令转发。
	"sudo": true, "su": true, "env": true, "busybox": true, "xargs": true,
	"find": true, "nohup": true, "timeout": true, "watch": true,
	// 任意读写：给了就等于给了整个文件系统。
	"tee": true, "dd": true, "cp": true, "mv": true, "rm": true, "ln": true,
	"chmod": true, "chown": true, "chgrp": true, "install": true, "tar": true,
	"vi": true, "vim": true, "nano": true, "less": true, "more": true,
}

// ParseSudoers 从文件内容读回规则标识与模板标识。
//
// 认领标记缺失 → 这个文件不归面板管（NoticeSudoFileDamaged）。读回来的模板不在闭集
// 里 → 规则是旧版本留下的，同样报错，不猜。
func ParseSudoers(content []byte) (ruleID, templateID string, err error) {
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, claimMarker) {
			continue
		}
		ruleID = strings.TrimSpace(strings.TrimPrefix(line, claimMarker))
	}
	if ruleID == "" {
		return "", "", newDefError(NoticeSudoFileDamaged,
			"%s 里没有认领标记（%s<规则标识>），它不是面板写的，面板不会覆盖它", sudoersFilePrefix+"<账号>", claimMarker)
	}
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "# 模板：") {
			continue
		}
		rest := strings.TrimPrefix(line, "# 模板：")
		if idx := strings.Index(rest, "（"); idx > 0 {
			templateID = rest[:idx]
		}
	}
	if templateID == "" {
		return "", "", newDefError(NoticeSudoFileDamaged,
			"规则文件里读不出模板标识；面板不会覆盖它，请人工看一眼")
	}
	if _, err := LookupSudoTemplate(templateID); err != nil {
		return "", "", err
	}
	return ruleID, templateID, nil
}
