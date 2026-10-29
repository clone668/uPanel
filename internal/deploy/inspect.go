package deploy

// 部署现场（P15.4）：一份部署此刻在磁盘与 systemd 上的样子，只读。
//
// 为什么需要这一层单独的读模型：docker 的项目清单（§8.3 `/deployments`）回答的是
// **这台机器上跑着什么**——里面混着别人手工 `docker compose up` 起来的、面板不知道
// 也管不了的项目。而部署页要回答的是另一个问题：**面板管着什么，它们现在是什么状态**。
// 两个问题用一份数据回答，就会有一边读错：拿项目清单当"我的部署"，手工起的项目会
// 变成一条点不动的部署；拿部署配置当"机器上跑着什么"，正在跑但配置不在盘上的项目
// 会凭空消失。
//
// 所以这里只读两类事实，两类都是可核对的：
//   - 盘上的三个文件：<name>.compose.yaml（这一版）、<name>.compose.yaml.prev（上一版）、
//     <name>.deployment.json（定义与镜像记录）；
//   - systemd 对包装单元的两个回答：is-enabled / is-active。
//
// 不拉镜像、不查 docker、不探测健康：这三件事都是"动作"该干的，一次列表刷新去做它们
// 会让页面在 docker 卡住时整页转圈。镜像 digest 来自记录（那是上一次执行时读回并写下的
// 事实），不是这一次现问的——这一点在 CONTRACTS §8.9 里写明，免得有人把它读成"现在跑的
// 就是这个镜像"。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 两条只读查询的名字（CONTRACTS §8.9）。名字放在这里而不是 agent 包：
// 查询名是这套能力的对外名字，和 deployment.apply 这些动作名一样属于契约。
const (
	// QueryListName 列出面板管理的部署现场。
	QueryListName = "deployments.list"
	// QueryGetName 读一份部署的现场。
	QueryGetName = "deployments.get"
)

// MaxInspections 是单次列表返回的部署条数上限。
//
// 与别处的分页上限同一个理由：一份没有上限的列表在异常情况下（有人在配置目录里放了
// 上千个文件）会把一次刷新变成上千次 systemctl 调用。超限时 Truncated 为 true，
// 页面必须把"没有全部列出"这件事说出来，而不是让人以为就这么几份。
const MaxInspections = 200

// UnitState 是 systemd 对包装单元的读数。
//
// Known=false 时 Reason 说明为什么没有读数。两种原因对管理员意味着完全不同的事：
// 单元文件不在磁盘上（部署没落地、或者被删了）与 systemctl 没答（这台机器的 systemd
// 出了问题）。把它们压成同一个"未知"，人就分不清该去翻文件还是去翻 journal。
type UnitState struct {
	Known   bool   `json:"known"`
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
	Reason  string `json:"reason,omitempty"`
}

// Inspection 是一份部署的现场。
//
// version 与 config_digest 是两个相关但不等价的字段：
//   - config_digest 是盘上那份渲染配置的 sha256，没有配置时是空串；
//   - version 是 **update / remove 要带回来的版本令牌**，取值同预检里的 StateVersion
//     （配置摘要，或者 unit-only / absent）。两者在正常情况下逐字相同，
//     不一样的时候以 version 为准——它是"提交时要对上的那个值"。
type Inspection struct {
	Name string `json:"name"`
	// DeploymentID 由名字派生（dep_ + 名字的 sha256 前缀），与 compose 项目标识同一个值：
	// 页面据此把部署与 §8.3 项目清单里的同名项目连起来。
	DeploymentID  string `json:"deployment_id"`
	ProjectName   string `json:"project"`
	UnitName      string `json:"unit"`
	UnitPath      string `json:"unit_path"`
	ConfigPath    string `json:"config_path"`
	ConfigPresent bool   `json:"config_present"`
	ConfigDigest  string `json:"config_digest"`
	Version       string `json:"version"`
	// PreviousConfigPresent 表示上一版渲染字节还在（回切的依据）。它只在 update
	// 成功过之后才为 true；为 false 时"回切"这件事没有落点。
	PreviousConfigPresent bool `json:"previous_config_present"`
	// RecordPresent 表示 <name>.deployment.json 在。没有记录的部署读不出定义，
	// 也因此**不能更新**（update 要求记录里那份规范化定义），页面必须据此
	// 把"更新"按钮关掉，而不是让它点了报 400。
	RecordPresent bool `json:"record_present"`
	// RecordError 是记录读不了的原因（不是合法 JSON、权限不对、格式版本不认）。
	// 它与 RecordPresent=false 是两件事：盘上有文件但读不懂，比没有文件更值得说。
	RecordError string `json:"record_error,omitempty"`
	// Record 是落盘那份记录的原样内容（规范化定义 + 镜像记录 + 上一版），
	// 没有记录时为 null。里面只有 sec_ 引用，不含明文秘密。
	Record *DeploymentRecord `json:"record,omitempty"`
	// ConfigDrift 表示盘上配置的摘要与记录里的摘要不一致：有人在面板之外动过
	// 那份文件。此时"现在跑的就是我批过的那一版"这句话已经不成立。
	ConfigDrift bool      `json:"config_drift"`
	Unit        UnitState `json:"unit_state"`
}

// SkippedEntry 是配置目录里存在、但没能读成一份部署的条目。
//
// 单独列出来而不是静默跳过：一个名字不合规的文件、一份读不了的记录，都是"这台机器上
// 有东西我不知道"的证据。列表把它吞掉，人就会以为配置目录里只有页面上那几行。
type SkippedEntry struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// InspectionList 是一次列表读取的结果。
type InspectionList struct {
	Deployments []Inspection   `json:"deployments"`
	Skipped     []SkippedEntry `json:"skipped,omitempty"`
	// Truncated 表示配置目录里的条目超过了 MaxInspections 条，后面的没有读。
	Truncated bool `json:"truncated"`
}

// Inspect 读一份部署的现场。
//
// 三样东西全都不在时返回 ErrNoDeployment（接口层映射成 404）：问一个没有部署的名字
// 与问一个没有部署的名字，答案必须一样，不能一次是空结构、一次是错误。
func (s *Service) Inspect(ctx context.Context, name string) (Inspection, error) {
	name = strings.TrimSpace(name)
	if !ValidDeploymentName(name) {
		return Inspection{}, invalid(CodeMalformed, "name", "部署名 %q 不能拼进文件名", name)
	}
	state, err := s.applier.Inspect(name)
	if err != nil {
		return Inspection{}, err
	}
	insp := Inspection{
		Name:          name,
		DeploymentID:  DeploymentIDForName(name),
		ProjectName:   name,
		UnitName:      mustUnitName(name),
		ConfigPresent: state.ConfigExists,
		ConfigDigest:  state.ConfigDigest,
		ConfigPath:    DeployConfigPath(name),
		Version:       deploymentStateVersion(state, ""),
	}
	if unitPath, err := UnitPath(name); err == nil {
		insp.UnitPath = filepath.Join(s.applier.Dir(), filepath.Base(unitPath))
	}
	if prevPath, err := s.applier.previousConfigPath(name); err == nil {
		if _, err := s.applier.stat(prevPath); err == nil {
			insp.PreviousConfigPresent = true
		}
	}
	rec, found, err := s.applier.ReadRecord(name)
	switch {
	case err != nil:
		// 盘上有文件但读不懂：照样把这份部署列出来，让"配置还在、记录坏了"
		// 这个现场可见；只是它不能更新。
		insp.RecordError = err.Error()
	case found:
		insp.RecordPresent = true
		copied := rec
		insp.Record = &copied
		if d := strings.TrimSpace(rec.ConfigDigest); d != "" && d != insp.ConfigDigest {
			insp.ConfigDrift = true
		}
	}
	if !state.ConfigExists && !state.UnitExists && !insp.RecordPresent && insp.RecordError == "" {
		return Inspection{}, ErrNoDeployment
	}
	insp.Unit = s.unitState(ctx, state.UnitExists, insp.UnitName)
	return insp, nil
}

// unitState 问 systemd 两个问题；单元文件不在磁盘上时不去问。
//
// 不问而不是问出一个 "not-found"：那两次调用只会得到"没有这条单元"，而这句话
// 我们本来就知道——多打两次 systemctl 换回一个已知答案，在 200 份部署的列表上
// 就是 400 次无用调用。
func (s *Service) unitState(ctx context.Context, unitExists bool, unitName string) UnitState {
	if !unitExists {
		return UnitState{Reason: "单元文件不在磁盘上（没有可读的单元状态）"}
	}
	enabled, active, err := s.applier.unitFacts(ctx, unitName)
	if err != nil {
		return UnitState{Reason: err.Error()}
	}
	return UnitState{Known: true, Enabled: enabled, Active: active}
}

// List 把配置目录里所有部署的现场读一遍，按名字排序。
//
// 名字从两个后缀收集：<name>.compose.yaml 与 <name>.deployment.json。后者的理由是
// "配置被人删了、记录还在"这种现场必须看得见——那正是一次说不清的漂移，
// 只按配置收集会让它凭空消失。
func (s *Service) List(ctx context.Context) (InspectionList, error) {
	dir := s.applier.ConfigDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 配置目录还不存在 = 这台机器上还没有任何部署。这不是错误。
			return InspectionList{Deployments: []Inspection{}}, nil
		}
		return InspectionList{}, fmt.Errorf("deploy: 读取部署配置目录 %s 失败: %w", dir, err)
	}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		file := entry.Name()
		switch {
		case strings.HasSuffix(file, ".compose.yaml"):
			names[strings.TrimSuffix(file, ".compose.yaml")] = struct{}{}
		case strings.HasSuffix(file, ".deployment.json"):
			names[strings.TrimSuffix(file, ".deployment.json")] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	out := InspectionList{Deployments: make([]Inspection, 0, len(ordered))}
	for _, name := range ordered {
		if err := ctx.Err(); err != nil {
			return InspectionList{}, err
		}
		if !ValidDeploymentName(name) {
			out.Skipped = append(out.Skipped, SkippedEntry{Name: name,
				Reason: "文件名不符合部署名规则（小写字母开头，只含小写字母、数字、下划线、中划线），" +
					"面板不会把它当成部署"})
			continue
		}
		if len(out.Deployments) >= MaxInspections {
			// 到上限就停：后面的不读，并且如实说"被截断了"。继续读只会让
			// 一次刷新变成几千次 systemctl。
			out.Truncated = true
			break
		}
		insp, err := s.Inspect(ctx, name)
		if err != nil {
			if errors.Is(err, ErrNoDeployment) {
				// 两个后缀都命不中（例如只剩一个 .prev）：不是部署，不列。
				continue
			}
			out.Skipped = append(out.Skipped, SkippedEntry{Name: name, Reason: err.Error()})
			continue
		}
		out.Deployments = append(out.Deployments, insp)
	}
	return out, nil
}
