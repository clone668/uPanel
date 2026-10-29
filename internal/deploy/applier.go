package deploy

// 包装单元与 Compose 配置的落地（P15.2；PLAN.md 第 11 节规则 3—6）。
//
// 这一层回答四个问题，每个都对应一条会咬人的事实：
//
//	是不是同一份部署    配置文件的摘要就是这份部署的版本。想改镜像、改端口、改挂载，
//	                  那是 deployment.update（P15.3）的事，不是"再 apply 一次"。
//	                  在这里悄悄覆盖，等于把一次更新伪装成一次部署，而更新需要的
//	                  备份、digest 记录与回切边界一件都还没做。
//	文件写对了吗        临时文件 → fsync → rename → 目录 fsync。掉电时要么是旧内容，
//	                  要么是新内容，不会是半份。配置 0600（里面有明文秘密），
//	                  单元 0644（systemd 以 root 读它，里面没有秘密）。
//	enable 真的生效了吗 回读 `systemctl is-active`，不以 start 的退出码当"跑起来了"。
//	                  Type=oneshot 的单元 start 成功只说明那次 compose up 返回 0。
//	删除时停了吗        `disable --now` 先停，确认不再是 active 才删文件。
//	                  停不下来就不删——留下一个在跑、却没有单元文件的部署，
//	                  下次 apply 会当成"可以从头创建"。
//
// 只碰 /etc/systemd/system/upanel-deploy-<name>.service 与
// /etc/upanel/deploy/<name>.compose.yaml。业务目录与数据卷一个字节都不动：
// 删除部署保留数据是 CONTRACTS §5 对 deployment.remove 的明文要求。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// Runner 执行一条外部命令（生产实现是 syscmd.Run）。
type Runner func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)

const (
	// unitFileMode / configFileMode：单元与配置的权限。
	//
	// 配置里可能有 app_env 解出来的明文口令，所以 0600 + 目录 0700，属主 root；
	// 单元文件里没有任何秘密，0644 便于运维直接读。
	unitFileMode   = 0o644
	configFileMode = 0o600
	configDirMode  = 0o700

	// systemctlTimeout 是单次 systemctl 调用的时限。
	//
	// 比 syscmd 的默认 120 秒长：`systemctl start` 会同步执行 ExecStart（compose up），
	// 那一步最坏要拉镜像。单元的 TimeoutStartSec 是 600 秒，这里给它一点余量，
	// 让 systemd 自己先判超时——我们要看到的是 systemd 的判断，不是被杀掉的客户端。
	systemctlTimeout = ComposeUpTimeoutSeconds*time.Second + 60*time.Second
)

// 错误哨兵。调用方按类型分派，不用看错误文本。
var (
	// ErrDeploymentExists 表示这个名字上已经有一份**内容不同**的部署。
	ErrDeploymentExists = errors.New("deploy: 这个名字上已经有一份内容不同的部署")
	// ErrNoDeployment 表示找不到对应的部署。
	ErrNoDeployment = errors.New("deploy: 没有这个部署")
	// ErrVersionMismatch 表示请求里的版本与磁盘上那份不一致（别人改过）。
	ErrVersionMismatch = errors.New("deploy: 部署版本与磁盘上的不一致")
	// ErrUnitStillActive 表示单元停不下来，因此不删文件。
	ErrUnitStillActive = errors.New("deploy: 单元仍在运行，不删除单元文件")
)

// systemctl 的候选绝对路径（Ubuntu 的 merge-usr 前后两种布局）。
var systemctlCandidates = []string{"/usr/bin/systemctl", "/bin/systemctl"}

// ApplierOptions 是构造参数。
type ApplierOptions struct {
	// Dir 是单元目录，默认 /etc/systemd/system。
	Dir string
	// ConfigDir 是渲染配置目录，默认 /etc/upanel/deploy。
	ConfigDir string
	// Runner 默认 syscmd.Run。
	Runner Runner
	// Systemctl 是可执行文件路径；为空时从候选绝对路径里挑第一个存在的。
	Systemctl string
	// Docker 是可执行文件路径，默认 DockerBinary（/usr/bin/docker）。
	//
	// 与单元里写死的路径同一个值：单元里的 ExecStart 与这里的 pull/inspect 必须是
	// 同一个二进制，否则"我拉的那份"和"它跑的那份"可以不是一回事。
	Docker string
	// Stat 用于挑候选路径与判断文件是否存在，默认 os.Stat（测试用来固定现场）。
	Stat func(string) (os.FileInfo, error)
}

// Applier 把单元与配置写到宿主上，并读回 systemd 的事实。
type Applier struct {
	dir       string
	configDir string
	run       Runner
	systemctl string
	docker    string
	stat      func(string) (os.FileInfo, error)
}

// NewApplier 构造 Applier。
func NewApplier(opts ApplierOptions) (*Applier, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = UnitDir
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("deploy: 单元目录必须是绝对路径，当前为 %q", dir)
	}
	configDir := strings.TrimSpace(opts.ConfigDir)
	if configDir == "" {
		configDir = DeployConfigDir
	}
	if !filepath.IsAbs(configDir) {
		return nil, fmt.Errorf("deploy: 配置目录必须是绝对路径，当前为 %q", configDir)
	}
	run := opts.Runner
	if run == nil {
		run = syscmd.Run
	}
	stat := opts.Stat
	if stat == nil {
		stat = os.Stat
	}
	systemctl := strings.TrimSpace(opts.Systemctl)
	if systemctl == "" {
		if systemctl = firstExisting(stat, systemctlCandidates); systemctl == "" {
			return nil, errors.New("deploy: 找不到 systemctl，无法管理部署单元")
		}
	}
	docker := strings.TrimSpace(opts.Docker)
	if docker == "" {
		docker = DockerBinary
	}
	return &Applier{dir: dir, configDir: configDir, run: run, systemctl: systemctl, docker: docker, stat: stat}, nil
}

// Dir / ConfigDir 把目录暴露给调用方（执行端要写进观测结果里）。
func (a *Applier) Dir() string       { return a.dir }
func (a *Applier) ConfigDir() string { return a.configDir }

// InspectState 是磁盘上这个名字此刻的现场。
type InspectState struct {
	// ConfigExists / ConfigDigest：渲染配置在不在、它的摘要（也是这份部署的版本）。
	ConfigExists bool
	ConfigDigest string
	// UnitExists：包装单元在不在。
	UnitExists bool
}

// Inspect 读磁盘上这个名字的现场（不写、不改、不读 systemd）。
func (a *Applier) Inspect(name string) (InspectState, error) {
	var state InspectState
	if !ValidDeploymentName(name) {
		return state, invalid(CodeMalformed, "name", "部署名 %q 不能拼进文件名", name)
	}
	cfg, err := ConfigPath(name)
	if err != nil {
		return state, err
	}
	digest, exists, err := a.configDigest(filepath.Join(a.configDir, filepath.Base(cfg)))
	if err != nil {
		return state, err
	}
	state.ConfigExists = exists
	state.ConfigDigest = digest
	unit, err := UnitPath(name)
	if err != nil {
		return state, err
	}
	state.UnitExists = a.exists(filepath.Join(a.dir, filepath.Base(unit)))
	return state, nil
}

// ApplyRequest 是一次部署请求。
type ApplyRequest struct {
	// Plan 是单元计划（已含部署标识与配置路径）。
	Plan UnitPlan
	// Rendered 是渲染好的 Compose 配置。
	Rendered Rendered
	// MustRun 表示"即使文件一个字都没变，也要让 ExecStart 再跑一次"。
	//
	// 更新用得到它：换镜像只改配置的字节，单元文件与上一次逐字节相同，于是
	// `written=false`、apply 那条"没写就不重启"的幂等判据会让新配置永远读不到
	// （`start` 对 RemainAfterExit 的 active oneshot 是空操作，见第 3 步）。
	MustRun bool
	// ReplaceExisting 表示"盘上那份不同内容的配置就是要被换掉"，即一次更新。
	//
	// 只有 deployment.update 该带它，而且必须自己承担后果：旧字节先落成 .prev、
	// 记录随后换掉、健康探测不过时还要能按 .prev 逐字节回切。apply 永远不带——
	// 悄悄覆盖别人写下去的内容，等于把一次更新伪装成一次部署。
	ReplaceExisting bool
	// OperationID 只用于日志与错误消息，不参与任何判断。
	OperationID string
}

// ApplyReport 是这次部署的读回事实。
type ApplyReport struct {
	DeploymentID string
	UnitName     string
	UnitPath     string
	ConfigPath   string
	// ConfigDigest 是**写到磁盘上那一份**配置的摘要，也是这份部署的版本。
	ConfigDigest string
	// Written 报告这次是否真的重写了两份文件（幂等重放时为 false）。
	Written bool
	// Restarted 报告这次是用 restart（而不是 start）把单元交给 systemd 的：
	// 单元本来就在盘上、而这次又写了新内容，只有 restart 能让 ExecStart 重跑。
	Restarted bool
	// Enabled / Active 来自回读 systemctl 的事实，不是命令的退出码。
	Enabled bool
	Active  bool
}

// Apply 落地一次部署：写配置、写单元、daemon-reload、enable、start，然后读回。
//
// 幂等：同一份定义再 apply 一次时，配置摘要相同、单元字节相同 → 不重写、不重启，
// 只做一次 enable/start 与读回。这是"重试"和"漂移修复"两种情形都需要的。
// 反过来，这次真的重写了单元（新建，或单元被人改过）而盘上早就有这个单元时，
// 启动动词是 restart 而不是 start：详见第 3 步的注释。
func (a *Applier) Apply(ctx context.Context, req ApplyRequest) (ApplyReport, error) {
	plan := req.Plan
	name := plan.Definition.Name
	report := ApplyReport{
		DeploymentID: plan.DeploymentID,
		ConfigDigest: req.Rendered.ConfigDigest,
	}
	unitName, err := UnitName(name)
	if err != nil {
		return report, err
	}
	report.UnitName = unitName
	report.UnitPath = filepath.Join(a.dir, unitName)
	// 配置路径必须就是单元里写的那一条。两侧算出来的不一样时，单元起来会拿着一个
	// 指向不存在文件的 -f，而单元本身"启动成功"过——这条一致性必须在写盘前断掉。
	if plan.ConfigPath == "" || filepath.Clean(filepath.FromSlash(plan.ConfigPath)) != filepath.Join(a.configDir, name+".compose.yaml") {
		return report, fmt.Errorf("deploy: 单元计划里的配置路径 %q 与配置目录 %q 对不上",
			plan.ConfigPath, a.configDir)
	}
	report.ConfigPath = filepath.Join(a.configDir, name+".compose.yaml")

	unit, err := RenderUnit(plan)
	if err != nil {
		return report, err
	}
	if req.Rendered.ConfigDigest == "" {
		return report, errors.New("deploy: 渲染结果没有配置摘要，无法作为部署版本")
	}

	// 1. 这个名字上是不是已经有一份**内容不同**的部署。
	//
	// 这条拦截只对 deployment.apply 成立：一次更新**就是要**换掉盘上那份字节，所以
	// update 会带 ReplaceExisting 来说明"我知道盘上是什么，上一版我已经存好了、
	// 记录我也换过了"。没有这个开关时，更新会在写盘前被自己上一次的部署挡住——
	// 单测一跑就撞上（P15.3）。
	existingDigest, exists, err := a.configDigest(report.ConfigPath)
	if err != nil {
		return report, err
	}
	if exists && existingDigest != req.Rendered.ConfigDigest && !req.ReplaceExisting {
		return report, fmt.Errorf("%w：%s 上已经有一份部署（版本 %s），这次请求的是 %s。"+
			"换镜像用 deployment.update；换端口、挂载或环境变量要删除后重建",
			ErrDeploymentExists, name, existingDigest, req.Rendered.ConfigDigest)
	}

	// 盘上是不是已经站着这个单元——决定第 3 步用 start 还是 restart，见那里的说明。
	unitPreexisted := a.exists(report.UnitPath)

	// 2. 写两份文件。目录先建（只有 uPanel 自己的两个目录，不碰业务目录）。
	if err := os.MkdirAll(a.configDir, configDirMode); err != nil {
		return report, fmt.Errorf("deploy: 创建配置目录 %s 失败: %w", a.configDir, err)
	}
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return report, fmt.Errorf("deploy: 创建单元目录 %s 失败: %w", a.dir, err)
	}
	// 配置按**内容**判断要不要写：盘上没有、或者字节不同（一次更新）都写。同字节
	// 重放不写——那正是"重试不改变任何东西"的意思。
	//
	// 早先这里只写了 `!exists` 一个条件，于是带 ReplaceExisting 的更新会"通过了防
	// 覆盖判据、然后什么都没写"，只把单元 restart 一次，磁盘上还是旧镜像；报告里
	// 却写着新摘要（P15.3 单测第一次跑就撞上）。
	if !exists || existingDigest != req.Rendered.ConfigDigest {
		if err := writeFileAtomic(report.ConfigPath, req.Rendered.Config, configFileMode); err != nil {
			return report, err
		}
		report.Written = true
	}
	unitSame, err := a.fileEquals(report.UnitPath, unit)
	if err != nil {
		return report, err
	}
	if !unitSame {
		// 单元内容与计划不一致（第一次写，或被人改过）：整篇重写。
		// 配置摘要相同意味着"还是同一份部署"，重写单元只是把我们生成的那份改回来。
		if err := writeFileAtomic(report.UnitPath, unit, unitFileMode); err != nil {
			return report, err
		}
		report.Written = true
	}

	// 3. 让 systemd 认这份单元，再 enable + start。
	if err := a.daemonReload(ctx); err != nil {
		return report, err
	}
	// enable 不只是"开机自启"：restart=no 的容器唯一的生命周期来源就是这个单元，
	// 不 enable 的话机器重启后应用永远回不来，而面板上看起来一切正常。
	if err := a.runSystemctl(ctx, "enable", unitName); err != nil {
		return report, err
	}
	// start 对"已经在 active 的 RemainAfterExit 单元"是**空操作**：ExecStart 不会重跑，
	// 刚写下去的配置永远不会被读到。真机 P15.2 §5 撞到的就是这个现场——上一轮崩溃留下
	// 一个 active 的单元、配置目录却没了，apply 写了新配置、`systemctl start` 立刻返回 0、
	// 单元读回 active，而容器一个都没起（任务因此诚实地报 partial）。
	//
	// 判据是"这次真的写了东西 + 单元本来就存在"，而不是去问一次 is-active：restart 对
	// 一个没在跑的单元等价于 start，没必要为它多一次 systemctl 往返；反过来，只重写了
	// 单元文件却没在跑的情形，restart 同样是把新内容交给 systemd 的唯一办法。
	//
	// 更新（req.MustRun）走同一条路，但理由是另一种：换镜像不改单元文件，只改配置的
	// 字节，`written` 因此是 false——必须由调用方明确说"这次要让 ExecStart 重跑"。
	verb := "start"
	if unitPreexisted && (report.Written || req.MustRun) {
		verb = "restart"
		report.Restarted = true
	}
	if err := a.runSystemctl(ctx, verb, unitName); err != nil {
		return report, err
	}

	// 4. 读回事实。
	report.Enabled, report.Active, err = a.unitFacts(ctx, unitName)
	if err != nil {
		return report, err
	}
	if !report.Active {
		return report, fmt.Errorf(
			"deploy: 单元 %s 启动后不是 active（enabled=%t）。compose up 的那次调用返回了 0，"+
				"也不代表应用在跑——请看 `journalctl -u %s` 与 `%s compose -p %s logs`",
			unitName, report.Enabled, unitName, DockerBinary, name)
	}
	return report, nil
}

// StopUnit 停掉一条部署的包装单元（ExecStop 跑 `compose down`），并确认它真的停了。
//
// 更新在"先停 → 备份 → 换镜像 → 起"这条路上用它。为什么要先停：容器还在往卷上写，
// 那份快照就是"读了一半的目录"，而它会被打上 consistency 标签，看起来像一份可信的备份。
// `systemctl stop` 的退出码不足以证明停了，所以照老规矩读回 `is-active`。
func (a *Applier) StopUnit(ctx context.Context, unitName string) error {
	if strings.TrimSpace(unitName) == "" {
		return errors.New("deploy: 要停的单元名是空的")
	}
	if err := a.runSystemctl(ctx, "stop", unitName); err != nil {
		return err
	}
	_, active, err := a.unitFacts(ctx, unitName)
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("deploy: %s 在 stop 之后仍是 active", unitName)
	}
	return nil
}

// RestartUnit 只让单元重跑一次 ExecStart，不碰任何文件。
//
// 更新在"什么都还没改"的失败路径上用它把单元起回来（备份失败、拉镜像失败）：
// 那一刻盘上的配置一个字节都没动，唯一被做过的事是 stop，所以也不需要 apply——
// 走 apply 反而会拿"这次要写的新字节"去比盘上的旧字节，然后被自己的防覆盖判据挡住。
func (a *Applier) RestartUnit(ctx context.Context, unitName string) error {
	if strings.TrimSpace(unitName) == "" {
		return errors.New("deploy: 要重新拉起的单元名是空的")
	}
	if err := a.runSystemctl(ctx, "restart", unitName); err != nil {
		return err
	}
	_, active, err := a.unitFacts(ctx, unitName)
	if err != nil {
		return err
	}
	if !active {
		return fmt.Errorf("deploy: %s 在 restart 之后不是 active", unitName)
	}
	return nil
}

// RemoveRequest 是一次删除请求。
type RemoveRequest struct {
	DeploymentID string
	// Version 是客户端看到的部署版本（配置摘要）；与磁盘上不一致就拒绝。
	Version string
}

// RemoveReport 是删除的读回事实。
type RemoveReport struct {
	DeploymentID string
	UnitName     string
	UnitPath     string
	ConfigPath   string
	// ConfigDigest 是被删掉的那份配置的摘要（删除前读的，供审计对账）。
	ConfigDigest  string
	Stopped       bool
	UnitRemoved   bool
	ConfigRemoved bool
}

// Find 按部署标识找到部署名。
//
// 部署标识是名字的 SHA-256 派生值，不可逆，所以只能扫一遍单元目录比对。
// 扫描只认 `upanel-deploy-<合法名字>.service`，别的东西一律跳过：
// 别人的单元里出现同前缀的巧合不值得为它冒"删错东西"的险。
func (a *Applier) Find(deploymentID string) (string, error) {
	id := strings.TrimSpace(deploymentID)
	if id == "" {
		return "", fmt.Errorf("%w：没有给部署标识", ErrNoDeployment)
	}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w：单元目录 %s 不存在", ErrNoDeployment, a.dir)
		}
		return "", fmt.Errorf("deploy: 读取单元目录 %s 失败: %w", a.dir, err)
	}
	matches := make([]string, 0, 1)
	for _, e := range entries {
		name, ok := unitNameToDeployment(e.Name())
		if !ok {
			continue
		}
		if DeploymentIDForName(name) == id {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w：%s", ErrNoDeployment, id)
	case 1:
		return matches[0], nil
	default:
		// 一个摘要撞出两个名字的概率可以忽略；真发生了说明目录里有人手工拼过单元，
		// 这时候删哪一个都是猜。
		return "", fmt.Errorf("deploy: 部署标识 %s 对应多个单元（%s），拒绝猜测",
			id, strings.Join(matches, "、"))
	}
}

// Remove 停掉并删除一份部署，**保留**数据卷与业务目录。
func (a *Applier) Remove(ctx context.Context, req RemoveRequest) (RemoveReport, error) {
	report := RemoveReport{DeploymentID: req.DeploymentID}
	name, err := a.Find(req.DeploymentID)
	if err != nil {
		return report, err
	}
	unitName, err := UnitName(name)
	if err != nil {
		return report, err
	}
	report.UnitName = unitName
	report.UnitPath = filepath.Join(a.dir, unitName)
	cfgPath, err := ConfigPath(name)
	if err != nil {
		return report, err
	}
	report.ConfigPath = filepath.Join(a.configDir, filepath.Base(cfgPath))

	digest, exists, err := a.configDigest(report.ConfigPath)
	if err != nil {
		return report, err
	}
	if !exists {
		// 没有配置却有单元：说明上一次删除中途断在这里，或者有人手工建过单元。
		// 继续删（单元里引用的配置已经不在，stop 时的 compose down 也不会做什么），
		// 但版本核对无从谈起——这条路径写进报告里，别让人以为核对过了。
		digest = ""
	}
	report.ConfigDigest = digest
	if want := strings.TrimSpace(req.Version); want != "" && exists && digest != want {
		return report, fmt.Errorf("%w：磁盘上是 %s，请求的是 %s", ErrVersionMismatch, digest, want)
	}

	// 先停。disable --now 会跑 ExecStop（compose down），容器在这里被收掉。
	// 单元文件不存在时 disable 会报"没有这个单元"，那不是错误——已经停了。
	unitExists := a.exists(report.UnitPath)
	if unitExists {
		if err := a.runSystemctl(ctx, "disable", "--now", unitName); err != nil {
			return report, err
		}
	}
	_, active, err := a.unitFacts(ctx, unitName)
	if err != nil {
		return report, err
	}
	if active {
		return report, fmt.Errorf("%w：%s", ErrUnitStillActive, unitName)
	}
	report.Stopped = true

	// 再删文件。只删这两个路径：业务目录与数据卷不在这一层的视野里，
	// 删除部署保留数据是契约要求，不是"这次先这样"。
	if unitExists {
		if err := os.Remove(report.UnitPath); err != nil && !os.IsNotExist(err) {
			return report, fmt.Errorf("deploy: 删除单元 %s 失败: %w", report.UnitPath, err)
		}
		report.UnitRemoved = true
		if err := a.daemonReload(ctx); err != nil {
			return report, err
		}
	}
	if exists {
		if err := os.Remove(report.ConfigPath); err != nil && !os.IsNotExist(err) {
			return report, fmt.Errorf("deploy: 删除配置 %s 失败: %w", report.ConfigPath, err)
		}
		report.ConfigRemoved = true
	}
	// 记录与上一版配置（P15.3）：它们属于这份部署，部署被删掉后留着只会让下一次
	// apply 看到一份来路不明的定义。删失败不改变结论——单元与配置已经删掉了。
	if err := a.RemoveRecord(name); err != nil {
		return report, err
	}
	return report, nil
}

// configDigest 读一份配置的 sha256（与 Rendered.ConfigDigest 同形）。
func (a *Applier) configDigest(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("deploy: 读取配置 %s 失败: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), true, nil
}

// fileEquals 判断文件内容是否与 data 逐字节相同。文件不存在时返回 false,nil。
func (a *Applier) fileEquals(path string, data []byte) (bool, error) {
	got, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("deploy: 读取 %s 失败: %w", path, err)
	}
	if len(got) != len(data) {
		return false, nil
	}
	for i := range got {
		if got[i] != data[i] {
			return false, nil
		}
	}
	return true, nil
}

func (a *Applier) exists(path string) bool {
	_, err := a.stat(path)
	return err == nil
}

// unitFacts 读回 systemd 对这条单元的判断。
//
// `is-enabled` / `is-active` 的**退出码本身就是答案**：disabled / masked 退出 1，
// inactive 退出 3（syscmd 把它们包成 *syscmd.ExitError，而 syscmd 的契约写得很清楚：
// 非零退出是结果，不是"命令执行失败"）。只有超时、起不来、输出超限才是真的读不到。
//
// 把退出码当错误，代价是真机 P15.2 boot 段 §B3 那个现场：`deployment.remove` 先
// `disable --now`，紧接着问 `is-enabled` 得到 "disabled" 与退出码 1，于是删除永远失败
// ——一个已经被停掉的部署反而删不掉了，唯一出路是手工动 systemctl。
func (a *Applier) unitFacts(ctx context.Context, unitName string) (enabled, active bool, err error) {
	state, err := a.systemctlAnswer(ctx, "is-enabled", unitName)
	if err != nil {
		return false, false, err
	}
	enabled = state == "enabled" || state == "enabled-runtime"

	state, err = a.systemctlAnswer(ctx, "is-active", unitName)
	if err != nil {
		return enabled, false, err
	}
	active = state == "active"
	return enabled, active, nil
}

// systemctlAnswer 问一句"现在是什么状态"，把状态词读回来。
//
// 非零退出被当成回答（见 unitFacts），其余错误原样上抛。
func (a *Applier) systemctlAnswer(ctx context.Context, verb, unitName string) (string, error) {
	res, err := a.run(ctx, syscmd.Spec{Program: a.systemctl, Args: []string{verb, unitName}},
		syscmd.Limits{Timeout: systemctlTimeout, MaxStdout: 4 << 10, MaxStderr: 4 << 10})
	var exitErr *syscmd.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", fmt.Errorf("deploy: systemctl %s %s 执行失败: %w", verb, unitName, err)
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (a *Applier) runSystemctl(ctx context.Context, args ...string) error {
	res, err := a.run(ctx, syscmd.Spec{Program: a.systemctl, Args: args},
		syscmd.Limits{Timeout: systemctlTimeout})
	// 同一条线：能跑起来但说了"不"，与控制不了这条命令，是两件事。
	var exitErr *syscmd.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return fmt.Errorf("deploy: systemctl %s 执行失败: %w", strings.Join(args, " "), err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("deploy: systemctl %s 退出码 %d：%s",
			strings.Join(args, " "), res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func (a *Applier) daemonReload(ctx context.Context) error {
	if err := a.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return nil
}

// unitNameToDeployment 从单元文件名里取回部署名。
func unitNameToDeployment(fileName string) (string, bool) {
	if !strings.HasPrefix(fileName, unitPrefix) || !strings.HasSuffix(fileName, ".service") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(fileName, unitPrefix), ".service")
	if !ValidDeploymentName(name) {
		return "", false
	}
	return name, true
}

// firstExisting 返回候选路径里第一个存在的。
func firstExisting(stat func(string) (os.FileInfo, error), candidates []string) string {
	for _, c := range candidates {
		if _, err := stat(c); err == nil {
			return c
		}
	}
	return ""
}

// writeFileAtomic 写一份文件：临时文件 → fsync → chmod → rename → 目录 fsync。
//
// 临时文件建在**目标目录**里：跨文件系统的 rename 会退化成"复制 + 删除"，
// 那就不是原子的了。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".upanel-deploy-*")
	if err != nil {
		return fmt.Errorf("deploy: 创建临时文件失败（目录 %s）: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("deploy: 写入 %s 失败: %w", path, err)
	}
	// 先落盘再改名：rename 只保证名字原子，不保证内容已经在盘上。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("deploy: 同步 %s 失败: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("deploy: 设置 %s 权限失败: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("deploy: 关闭 %s 失败: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("deploy: 发布 %s 失败: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// firstLine 取错误输出的第一行（systemctl 的报错第一行通常就是结论）。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
