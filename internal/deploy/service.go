package deploy

// 部署动作的参数与“预检—执行”两阶段的实现（CONTRACTS.md 第 5 节：
// deployment.apply / deployment.remove；PLAN.md 第 12 节）。
//
// 这一层把校验、渲染、单元计划与落地串成一个顺序，并且**每个阶段都重新做一遍**：
// 预检给的是“现在能不能部署”的判断，执行前再问一次是因为秘密可能刚被轮换、
// 卷可能刚被卸下、有人可能刚手工改过单元。两处用的是同一份代码，所以两处不会打架。

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// 动作名（CONTRACTS.md 第 5 节）。由本包定义：Web 侧要用同一批字符串去调用动作，
// 拼错一个字就会在代理那边得到 404，而 404 与“这个版本还不支持”长得一样。
const (
	ActionApply  = "deployment.apply"
	ActionUpdate = "deployment.update"
	ActionRemove = "deployment.remove"
)

// ApplyParams 是 deployment.apply 的参数。
//
// DeploymentID 为 null 表示新建。非 null 表示"我已经有一份这个部署了"——
// 换镜像、换定义都不走 apply：换镜像用 deployment.update（P15.3），
// 换端口/挂载/环境变量要删除重建。在 apply 里明确拒绝，而不是当成
// "再 apply 一次"悄悄覆盖。
type ApplyParams struct {
	DeploymentID *string    `json:"deployment_id"`
	Version      string     `json:"version"`
	Definition   Definition `json:"definition"`
}

// RemoveParams 是 deployment.remove 的参数。
type RemoveParams struct {
	DeploymentID string `json:"deployment_id"`
	Version      string `json:"version"`
}

// Notice 是一条给人看的阻塞原因或告警（code 稳定，message 可读）。
type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ApplyAssessment 是一次部署预检的结论。
type ApplyAssessment struct {
	// Definition 是规范化之后的定义（预检绑定的是这一份）。
	Definition Definition
	// DeploymentID 是这次部署的标识（由名字派生）。
	DeploymentID string
	// ProjectName 是 Compose 项目名（等于部署名）。
	ProjectName string
	// ConfigDigest 是这次将要落盘的配置摘要，也是这份部署的版本。
	ConfigDigest string
	// UnitName 是包装单元名。
	UnitName string
	// Existing 表示这个名字上已经有一份部署。
	Existing bool
	// ExistingDigest 是磁盘上那一份的摘要（不存在时为空）。
	ExistingDigest string
	// StateVersion 是执行前复核用的版本令牌：已有配置时是磁盘上那一份的摘要；
	// 新建时是这一次**将要落盘**的摘要（磁盘上还没有旧版本，但令牌不能为空，
	// 见 deploymentStateVersion）。
	StateVersion string
	// Allowed 为假时 Blockers 至少有一条。
	Allowed bool
	// Blockers / Warnings 是拒绝原因与提示。
	Blockers []Notice
	Warnings []Notice
	// VolumeMountPoints 是这次部署会绑定到的卷挂载点（展示用）。
	VolumeMountPoints []string
	// Claims 是这次部署占用的端口（锁与冲突检测用）。
	Claims []Claim
	// MountedSources 是容器路径 → 宿主路径（展示用：让人看见数据会落在哪）。
	MountedSources map[string]string
}

// RemoveAssessment 是一次删除预检的结论。
type RemoveAssessment struct {
	DeploymentID   string
	ProjectName    string
	UnitName       string
	ConfigDigest   string
	StateVersion   string
	Allowed        bool
	Blockers       []Notice
	Warnings       []Notice
	MountedSources map[string]string
}

// ApplyOutcome 是一次部署执行的结果。
type ApplyOutcome struct {
	Report      ApplyReport
	Definition  Definition
	ProjectName string
	Idempotent  bool
	Duration    int64
	// Image 是这次部署对应的镜像事实（P15.3 的记录用）。
	Image ImageRecord
	// ImageDigestKnown 为假表示 digest 没读到（部署本身是成功的）。
	ImageDigestKnown bool
	// Recorded 表示定义与镜像记录写进了 <name>.deployment.json：没有它，
	// 下一次 deployment.update 会被拒绝（记录是更新唯一的定义来源）。
	Recorded bool
	// Health 是部署之后的健康探测（P15.3；定义没开健康检查时 Checked=false）。
	//
	// 它不改变"部署已经落地"这个事实，但也绝不能被读成"部署成功"：单元 active 与
	// 应用能应答是两件事，探不通就是探不通。
	Health HealthProbe
	// Warnings 是执行期的告警：不改变结论，但必须说出来。
	Warnings []string
}

// RemoveOutcome 是一次删除执行的结果。
type RemoveOutcome struct {
	Report      RemoveReport
	ProjectName string
}

// LockName 是一份部署占用的 compose 项目锁。
//
// 锁名由本包给出，不由调用点拼：代理登记表、预检计划与并发测试都读同一个函数，
// 拼错一处就会得到两把互不相干的锁，而那正是“两个部署同时抢一个项目”的样子。
func LockName(projectName string) string { return "compose:" + projectName }

// PortLockName 是一个端口声明的锁名。
//
// 协议与地址都进名字：同一个端口在 tcp 与 udp 上是两件事，绑在 127.0.0.1 与绑在
// 网卡地址上也是两件事。只按端口号上锁会让两个本来可以共存的部署互相排队。
func PortLockName(c Claim) string {
	return fmt.Sprintf("port:%s/%s/%d", c.Protocol, c.Address, c.Port)
}

// VolumeLockName 是一个受管理卷的锁名（与 storage 卷动作同一个名字）。
//
// 部署动的不是卷本身，但容器写的是同一块盘：卷在被卸载/格式化时不该有一个容器
// 正在往上面写。同名锁让这两类操作排进同一条队，而不是各拿一把谁也拦不住谁的锁。
func VolumeLockName(volumeID string) string { return "volume:" + volumeID }

// Locks 返回一次部署要持有的资源锁（CONTRACTS.md 第 5 节：compose-project+ports+volumes）。
//
// 去重并排序：代理侧的锁管理器要求列表已排序（死锁避免靠固定顺序），而
// volume_dependencies 里重复写一个卷是合法的定义。
func (a ApplyAssessment) Locks() []string {
	set := make(map[string]struct{}, 2+len(a.Claims)+len(a.Definition.VolumeDependencies))
	add := func(name string) {
		if name != "" {
			set[name] = struct{}{}
		}
	}
	if a.ProjectName != "" {
		add(LockName(a.ProjectName))
	}
	for _, c := range a.Claims {
		add(PortLockName(c))
	}
	for _, id := range a.Definition.VolumeDependencies {
		add(VolumeLockName(id))
	}
	locks := make([]string, 0, len(set))
	for name := range set {
		locks = append(locks, name)
	}
	sort.Strings(locks)
	return locks
}

// Locks 返回一次删除要持有的资源锁：只锁 compose 项目。
//
// 删除不声明端口（它释放端口），也不动卷（CONTRACTS §5 明文保留数据），
// 所以它不该拿着一把 volume 锁把别的部署堵在那里。
func (a RemoveAssessment) Locks() []string {
	if a.ProjectName == "" {
		return nil
	}
	return []string{LockName(a.ProjectName)}
}

// ServiceError 带稳定 code 的错误（参数/宿主事实层面的拒绝）。
type ServiceError struct {
	Code    string
	Message string
}

func (e *ServiceError) Error() string { return e.Message }

// Service 把一次部署从定义走到落地。
type Service struct {
	applier  *Applier
	resolver Resolver
	// env 返回宿主事实（文件根、卷、本机地址、部署数据根）。
	//
	// 它是函数而不是快照：执行前复核要的是**此刻**的事实，一份缓存下来的
	// 卷表会让"卷刚被卸下"这件事在部署那一刻不可见。
	env func(ctx context.Context) (HostEnv, error)
	// backup 是"更新前备份"的实现端（P15.3）。nil 表示这台机器上没有备份能力：
	// 带 backup_policy_id 的更新会在预检里被挡住，而不是被当成"已经备份过"。
	backup BackupCause
	// prober 是面板侧健康探测（P15.3）。nil 时用真的网络去探。
	prober HealthProber
}

// ServiceOptions 是构造参数。
type ServiceOptions struct {
	Applier  *Applier
	Resolver Resolver
	Env      func(ctx context.Context) (HostEnv, error)
	// Backup 可空：没有它就不能选"更新前备份"。
	Backup BackupCause
	// Prober 可空：默认用 DefaultHealthProber。
	Prober HealthProber
}

// NewService 构造 Service。
func NewService(opts ServiceOptions) (*Service, error) {
	if opts.Applier == nil {
		return nil, errors.New("deploy: 缺少落地端（Applier）")
	}
	if opts.Resolver == nil {
		return nil, errors.New("deploy: 缺少秘密解析器；没有它就渲染不出带秘密的配置")
	}
	if opts.Env == nil {
		return nil, errors.New("deploy: 缺少宿主事实来源")
	}
	return &Service{applier: opts.Applier, resolver: opts.Resolver, env: opts.Env,
		backup: opts.Backup, prober: opts.Prober}, nil
}

// reportStep 回报一个执行步骤；progress 为 nil 时什么也不做。
//
// 进度回调是**每次执行**的参数，不是 Service 上的字段：一个 Service 会被多个任务
// 并发使用，挂在字段上的回调会让两次同时进行的部署互相收到对方的步骤——进度条
// 于是显示的是另一件事的进度，而人对着它等。
func reportStep(progress func(step string, fields map[string]any), step string, fields map[string]any) {
	if progress != nil {
		progress(step, fields)
	}
}

// AssessApply 预检一次部署。
func (s *Service) AssessApply(ctx context.Context, p ApplyParams) (ApplyAssessment, error) {
	assessment := ApplyAssessment{ProjectName: strings.TrimSpace(p.Definition.Name)}
	def, err := Normalize(p.Definition)
	if err != nil {
		return assessment, err
	}
	assessment.Definition = def
	assessment.ProjectName = def.Name
	assessment.DeploymentID = DeploymentIDForName(def.Name)
	assessment.Claims = def.Claims()

	// 新建与更新在这一层的边界：apply 只创建。
	if p.DeploymentID != nil && strings.TrimSpace(*p.DeploymentID) != "" {
		given := strings.TrimSpace(*p.DeploymentID)
		if given != assessment.DeploymentID {
			return assessment, &ValidationError{Field: "deployment_id", Code: CodeMalformed,
				Message: fmt.Sprintf("部署标识 %s 与部署名 %s 派生出的标识 %s 不一致",
					given, def.Name, assessment.DeploymentID)}
		}
		return assessment, &ServiceError{Code: "update_not_implemented",
			Message: "已经有一份部署时不能重新 apply：换镜像用 deployment.update，" +
				"换端口/挂载/环境变量要删除后重建；apply 只创建新部署"}
	}

	env, err := s.env(ctx)
	if err != nil {
		return assessment, err
	}
	if err := CheckHost(def, env); err != nil {
		return assessment, err
	}
	for _, problem := range CheckVolumesReady(def, env) {
		assessment.Blockers = append(assessment.Blockers, Notice{Code: "volume_not_ready", Message: problem})
	}

	rendered, err := Render(ctx, def, RenderEnv{Host: env, Resolver: s.resolver})
	if err != nil {
		if errors.Is(err, ErrSecretUnavailable) {
			assessment.Blockers = append(assessment.Blockers, Notice{Code: "secret_unavailable",
				Message: "环境变量引用的秘密读不到，整份配置不产出：" + err.Error()})
			assessment.Allowed = false
			return assessment, nil
		}
		return assessment, err
	}
	assessment.ConfigDigest = rendered.ConfigDigest
	assessment.MountedSources = rendered.MountedSources

	plan, err := PlanUnitAt(def, rendered, env, s.applier.ConfigDir())
	if err != nil {
		return assessment, err
	}
	assessment.UnitName = mustUnitName(def.Name)
	assessment.VolumeMountPoints = plan.VolumeMountPoints

	state, err := s.applier.Inspect(def.Name)
	if err != nil {
		return assessment, err
	}
	assessment.Existing = state.ConfigExists || state.UnitExists
	assessment.ExistingDigest = state.ConfigDigest
	// 版本令牌必须非空——代理层要求一条 allowed 的预检绑定一个可复核的状态快照
	// （agent/preflight.go）。新建时磁盘上没有旧版本，绑定的就是这一次渲染出来的
	// 摘要：它同样是可复核的，定义或秘密在预检与执行之间变了，摘要就变。
	assessment.StateVersion = deploymentStateVersion(state, rendered.ConfigDigest)
	if assessment.Existing {
		if state.ConfigExists && state.ConfigDigest != rendered.ConfigDigest {
			assessment.Blockers = append(assessment.Blockers, Notice{Code: "deployment_exists",
				Message: fmt.Sprintf("这个名字上已经有一份内容不同的部署（版本 %s）。"+
					"换镜像用 deployment.update；换端口、挂载或环境变量要删除后重建",
					state.ConfigDigest)})
		}
	}

	assessment.Allowed = len(assessment.Blockers) == 0
	return assessment, nil
}

// ExecuteApply 执行一次部署：重新取宿主事实、重新渲染、再落地并读回。
//
// progress 可空。它只回报**已经开始做**的步骤（写单元、启动单元），不回报"准备中"：
// 一条在动作被拒绝之后还留在时间线上的"准备中"会让人以为部署已经开始。
func (s *Service) ExecuteApply(ctx context.Context, p ApplyParams, expectState string,
	progress func(step string, fields map[string]any)) (ApplyOutcome, error) {
	out := ApplyOutcome{}
	// 执行前复核：把预检那一套全部重跑一遍。秘密轮换、卷被卸、单元被人改过，
	// 三种情况都会在这里被看见，而不是照着旧预检硬上。
	assessment, err := s.AssessApply(ctx, p)
	if err != nil {
		return out, err
	}
	if !assessment.Allowed {
		return out, &ServiceError{Code: "blocked", Message: firstBlockerMessage(assessment.Blockers)}
	}
	if strings.TrimSpace(expectState) != "" && expectState != assessment.StateVersion {
		return out, &ServiceError{Code: "stale",
			Message: fmt.Sprintf("预检时磁盘上是 %s，现在是 %s；期间有人改过这份部署",
				orNone(expectState), orNone(assessment.StateVersion))}
	}

	env, err := s.env(ctx)
	if err != nil {
		return out, err
	}
	rendered, err := Render(ctx, assessment.Definition, RenderEnv{Host: env, Resolver: s.resolver})
	if err != nil {
		return out, err
	}
	if rendered.ConfigDigest != assessment.ConfigDigest {
		return out, &ServiceError{Code: "stale", Message: "秘密在预检与执行之间变了，" +
			"渲染出来的配置摘要不同；请重新预检"}
	}
	plan, err := PlanUnitAt(assessment.Definition, rendered, env, s.applier.ConfigDir())
	if err != nil {
		return out, err
	}

	reportStep(progress, "writing", map[string]any{"unit": assessment.UnitName, "config_digest": rendered.ConfigDigest})
	report, err := s.applier.Apply(ctx, ApplyRequest{Plan: plan, Rendered: rendered})
	if err != nil {
		return out, err
	}
	out.Report = report
	out.Definition = assessment.Definition
	out.ProjectName = assessment.ProjectName
	out.Idempotent = !report.Written
	reportStep(progress, "started", map[string]any{"unit": report.UnitName, "written": report.Written})
	// 健康探测（P15.3）：定义开了就探一次**公开入口**。探的是宿主端口而不是容器端口
	// ——端口映射、反代、监听地址三样都可能让容器内 200 而外面打不开，而管理员用的
	// 是外面那一个。探不通不改结论（部署确实落地了），但要在结果与告警里说出来。
	if assessment.Definition.HealthCheck.Enabled {
		reportStep(progress, "probing", map[string]any{"port": assessment.Definition.HostPort,
			"kind": assessment.Definition.HealthCheck.Kind})
		out.Health = s.probeHealth(ctx, time.Now().Add(HealthProbeBudget), assessment.Definition)
		if !out.Health.Checked {
			out.Warnings = append(out.Warnings, "健康探测没有做成："+out.Health.LastError)
		} else if !out.Health.Healthy {
			out.Warnings = append(out.Warnings, "健康探测没探通（"+probeSummary(out.Health)+
				"）；部署已经落地，但应用现在不应答")
		}
	}
	// 记录（P15.3）：定义落一份，下一次更新才有据可依；镜像 digest 是"现在跑的是
	// 哪一份字节"的唯一证据。两者都不改变这次部署的结论，所以失败只记告警。
	s.recordAfterApply(ctx, assessment, rendered, &out)
	return out, nil
}

// recordAfterApply 在成功落地之后写部署记录。
//
// 幂等重放时**保留上一版**：重放不会换镜像，把 Previous 抹掉等于把回切的路标拆了。
// 同理，这次读不到 digest 时保留记录里原来的那个——"没读到"不等于"变了"。
func (s *Service) recordAfterApply(ctx context.Context, assessment ApplyAssessment,
	rendered Rendered, out *ApplyOutcome) {
	facts, factsErr := s.applier.ImageFacts(ctx, assessment.Definition.ImageReference)
	out.Image = s.imageRecordOf(assessment.Definition, rendered, facts, factsErr)
	out.ImageDigestKnown = factsErr == nil && facts.ID != ""
	if factsErr != nil {
		out.Warnings = append(out.Warnings, "读不到镜像 digest："+factsErr.Error()+
			"；部署本身是成功的，但记录里没有镜像指纹")
	}
	rec := DeploymentRecord{
		Schema:       RecordSchema,
		DeploymentID: assessment.DeploymentID,
		Name:         assessment.ProjectName,
		Definition:   assessment.Definition,
		ConfigDigest: rendered.ConfigDigest,
		Image:        out.Image,
		UpdatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	if existing, found, err := s.applier.ReadRecord(assessment.ProjectName); err == nil && found {
		rec.Previous = existing.Previous
		if !out.ImageDigestKnown && existing.Image.Digest != "" &&
			existing.Image.Requested == rec.Image.Requested {
			rec.Image.Digest = existing.Image.Digest
			rec.Image.RepoDigests = existing.Image.RepoDigests
		}
	}
	if err := s.applier.WriteRecord(rec); err != nil {
		out.Warnings = append(out.Warnings, "部署记录没写进去："+err.Error()+
			"；部署本身已经起来了，但下一次更新会因为没有记录被拒绝（重新 apply 可补上记录）")
		return
	}
	out.Recorded = true
}

// AssessRemove 预检一次删除。
func (s *Service) AssessRemove(ctx context.Context, p RemoveParams) (RemoveAssessment, error) {
	assessment := RemoveAssessment{DeploymentID: strings.TrimSpace(p.DeploymentID)}
	if assessment.DeploymentID == "" {
		return assessment, &ValidationError{Field: "deployment_id", Code: CodeMissing,
			Message: "删除部署必须给出部署标识"}
	}
	name, err := s.applier.Find(assessment.DeploymentID)
	if err != nil {
		assessment.Blockers = append(assessment.Blockers, Notice{Code: "no_such_deployment", Message: err.Error()})
		return assessment, nil
	}
	assessment.ProjectName = name
	assessment.UnitName = mustUnitName(name)
	state, err := s.applier.Inspect(name)
	if err != nil {
		return assessment, err
	}
	assessment.ConfigDigest = state.ConfigDigest
	assessment.StateVersion = deploymentStateVersion(state, "")
	if want := strings.TrimSpace(p.Version); want != "" && state.ConfigExists && want != state.ConfigDigest {
		assessment.Blockers = append(assessment.Blockers, Notice{Code: "stale",
			Message: fmt.Sprintf("磁盘上是 %s，请求的是 %s；期间有人改过这份部署", state.ConfigDigest, want)})
	}
	assessment.Warnings = append(assessment.Warnings, Notice{Code: "data_kept",
		Message: "删除只移除包装单元与渲染配置，数据卷与业务目录保留；要清数据请用文件管理与卷管理"})
	assessment.Allowed = len(assessment.Blockers) == 0
	return assessment, nil
}

// ExecuteRemove 执行一次删除。
//
// progress 可空；只在真的要停下单元时回报一步——预检被拒时不该留下“正在停止”的痕迹。
func (s *Service) ExecuteRemove(ctx context.Context, p RemoveParams, expectState string,
	progress func(step string, fields map[string]any)) (RemoveOutcome, error) {
	out := RemoveOutcome{}
	assessment, err := s.AssessRemove(ctx, p)
	if err != nil {
		return out, err
	}
	if !assessment.Allowed {
		return out, &ServiceError{Code: "blocked", Message: firstBlockerMessage(assessment.Blockers)}
	}
	if strings.TrimSpace(expectState) != "" && expectState != assessment.StateVersion {
		return out, &ServiceError{Code: "stale",
			Message: fmt.Sprintf("预检时版本是 %s，现在是 %s", orNone(expectState), orNone(assessment.StateVersion))}
	}
	reportStep(progress, "stopping", map[string]any{"unit": assessment.UnitName})
	report, err := s.applier.Remove(ctx, RemoveRequest{
		DeploymentID: assessment.DeploymentID,
		Version:      assessment.ConfigDigest,
	})
	if err != nil {
		return out, err
	}
	out.Report = report
	out.ProjectName = assessment.ProjectName
	return out, nil
}

// mustUnitName 只用在已经通过 Normalize 的名字上：名字合法时它不可能失败。
func mustUnitName(name string) string {
	unit, err := UnitName(name)
	if err != nil {
		return ""
	}
	return unit
}

func firstBlockerMessage(blockers []Notice) string {
	if len(blockers) == 0 {
		return "被拒绝"
	}
	return blockers[0].Message
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（没有部署）"
	}
	return s
}

// deploymentStateVersion 把“这个名字此刻在磁盘上的样子”压成一个可逐字比较的令牌。
//
// 令牌必须非空：一条 allowed 的预检要求绑定一个可复核的状态快照（agent/preflight.go
// 的 validatePlanResult），空字符串等于“没有绑定”，会让新建部署的预检直接变成
// 内部错误——真机 P15.2 验收 §5 撞到的就是它。
//
// 三种现场各有各的令牌，都是**可重算**的：
//   - 渲染配置在盘上：令牌就是它的摘要（配置是生成的字节，摘要是它唯一的内容指纹，
//     也是这份部署的版本）；
//   - 盘上什么都没有：令牌是这一次将要落盘的摘要（pending）。新建没有旧版本可比，
//     但“我要写下去的这一份”照样是事实——秘密轮换或定义变化都会让摘要变；
//   - 单元还在而配置被人删了：令牌是 unit-only。给一个非空的字面量而不是摘要，
//     是因为这时盘上确实没有摘要可绑，编一个假的会掩盖“配置已经不见了”这件事。
func deploymentStateVersion(state InspectState, pending string) string {
	if d := strings.TrimSpace(state.ConfigDigest); d != "" {
		return d
	}
	if d := strings.TrimSpace(pending); d != "" {
		return d
	}
	if state.UnitExists {
		return "unit-only"
	}
	return "absent"
}

// DeployConfigPath 返回部署的渲染配置路径（展示与审计用）。
func DeployConfigPath(name string) string {
	if !ValidDeploymentName(name) {
		return ""
	}
	return path.Join(DeployConfigDir, name+".compose.yaml")
}
