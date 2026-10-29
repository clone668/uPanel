package deploy

// 受管部署在宿主上的落地形态：一个 systemd 包装单元 + 一份渲染好的 Compose 配置
// （PLAN.md 第 11 节依赖规则 3—6，STAGES.md P15.2）。
//
// 为什么必须有一个包装单元，而不是让面板直接 `docker compose up`：
//
//	restart  容器本身的 restart 写死为 no（rendered 配置里那处引号是功能）。
//	         可如果生命周期没有别的宿主，no 就只是“没人再拉它起来”——包括开机。
//	         包装单元补上这一层：开机由 systemd 起，运行中由 systemd 守，停止由
//	         systemd 收。容器永远不会自己启动，也就永远不会绕过卷检查先跑起来。
//	挂载     单元带 RequiresMountsFor 与对每个卷 mount unit 的 BindsTo/After。
//	         卷没挂上，单元根本进不了 active；运行中卷失联，systemd 停掉单元，
//	         ExecStop 随之把 compose 项目收下来（规则 5 的落点）。
//	不建目录 单元不创建任何目录，配置里 create_host_path 为 false（规则 6）。
//	         绑定源不存在时 compose 直接失败——挂载出问题必须看起来像出了问题。
//
// 这个文件里的东西全部是**纯函数**：给定定义与宿主事实，输出确定的单元文本。
// 真正的写盘与 systemctl 调用在 P15.2 的执行端，不在这里。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// UnitDir 是 uPanel 写自己单元文件的目录（与受管计划任务同一个目录，同一个理由：
	// 不写发行版的 /lib/systemd/system，也不写某个用户的 ~/.config）。
	UnitDir = "/etc/systemd/system"

	// DeployConfigDir 是渲染出来的 Compose 配置目录。
	//
	// 配置里可能有明文秘密（app_env 用途解出来的值），因此目录 0700、文件 0600，
	// 属主 root。P15.2 的执行端负责这两条权限——渲染函数本身不管权限，
	// 它只负责把该写的东西写对。
	DeployConfigDir = "/etc/upanel/deploy"

	// DockerBinary 是唯一允许出现在 ExecStart 里的可执行文件。
	//
	// 写死绝对路径而不是让 PATH 决定：单元以 root 运行，ExecStart 里任何一个
	// 由环境决定的东西都是一条替换入口。
	DockerBinary = "/usr/bin/docker"

	// unitPrefix 是所有受管部署单元的名字前缀。
	unitPrefix = "upanel-deploy-"

	// ComposeUpTimeoutSeconds 是 ExecStart 的时限。
	//
	// compose up 可能要拉镜像，但它不该无限期挂着：600 秒之后 systemd 杀掉
	// 这一次启动，任务以失败收场，而不是让面板上永远显示“部署中”。
	ComposeUpTimeoutSeconds = 600

	// ComposeDownTimeoutSeconds 是 ExecStop 的时限。停容器要给应用写盘的时间，
	// 但不能给到“停机卡住”的程度。
	ComposeDownTimeoutSeconds = 300
)

// DeploymentIDForName 把部署名映射成面板里的稳定标识。
//
// 它与 containers.ProjectID 用的是同一个算法（`compose:` 前缀 + SHA-256 前 16 字节）：
// 观测侧从 compose 标签里的项目名算出来的 id，与管理侧按定义名算出来的 id 必须是
// 同一个字符串，否则“面板知道有这个部署”和“宿主机上有这个项目”会永远对不上。
// 有一条测试逐字比较两者。
func DeploymentIDForName(name string) string {
	sum := sha256.Sum256([]byte("compose:" + name))
	return "dep_" + hex.EncodeToString(sum[:16])
}

// ValidDeploymentName 判断部署名能不能拼进单元名与文件名。
//
// 与 Normalize 用的判据是同一条：小写字母开头，只含小写字母、数字、下划线、中划线。
// 单独再判一次是因为单元名由它拼出来——这条路径上没有任何东西会替我们报错。
func ValidDeploymentName(name string) bool { return namePattern.MatchString(name) }

// UnitName 返回部署对应的 systemd 服务单元名。
func UnitName(name string) (string, error) {
	if !ValidDeploymentName(name) {
		return "", invalid(CodeMalformed, "name", "部署名 %q 不能拼进 systemd 单元名", name)
	}
	return unitPrefix + name + ".service", nil
}

// UnitPath 返回服务单元文件路径。
func UnitPath(name string) (string, error) {
	unit, err := UnitName(name)
	if err != nil {
		return "", err
	}
	return UnitDir + "/" + unit, nil
}

// ConfigPath 返回渲染出来的 Compose 配置路径。
func ConfigPath(name string) (string, error) {
	if !ValidDeploymentName(name) {
		return "", invalid(CodeMalformed, "name", "部署名 %q 不能拼进配置文件路径", name)
	}
	return DeployConfigDir + "/" + name + ".compose.yaml", nil
}

// MountUnitName 把挂载点转成 systemd 的 mount unit 名。
//
// 规则与 `systemd-escape --path` 一致：去掉首尾的 `/`，分隔符 `/` 变成 `-`，
// 每个路径段里除 `[A-Za-z0-9:_.]` 之外的字符写成 `\xNN`，末尾加 `.mount`。
//
// 两个细节决定了这条链会不会断：
//   - **路径段里的 `-` 必须转义成 `\x2d`**。不转义的话 `/srv/upanel/volumes/my-app`
//     与 `/srv/upanel/volumes/my/app` 会拼出同一个单元名，卷依赖就指到别人身上了。
//   - 单元名必须是 systemd **真的会加载**的名字。写错一个字符，BindsTo 指向一个
//     不存在的单元，那条依赖不会变成“没检查”，而是让整个单元起不来——
//     安全的方向是对的，但部署会以一条看不懂的报错收场，所以这里逐字符照着来。
func MountUnitName(mountPoint string) (string, error) {
	raw := strings.TrimSpace(mountPoint)
	if raw == "" || !path.IsAbs(raw) {
		return "", invalid(CodeNotAllowed, "mounts", "挂载点 %q 不是绝对路径，拼不出 mount unit", mountPoint)
	}
	trimmed := strings.Trim(raw, "/")
	if trimmed == "" {
		return "", invalid(CodeNotAllowed, "mounts", "挂载点是 /，不能作为受管理卷的挂载点")
	}
	segments := strings.Split(trimmed, "/")
	for i, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", invalid(CodeNotAllowed, "mounts",
				"挂载点 %q 含空的或相对的路径段，拼不出 mount unit", mountPoint)
		}
		segments[i] = escapeUnitSegment(seg)
	}
	return strings.Join(segments, "-") + ".mount", nil
}

// escapeUnitSegment 转义一个路径段（systemd unit_name_escape 的等价实现）。
func escapeUnitSegment(seg string) string {
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == ':', c == '_', c == '.':
			b.WriteByte(c)
		default:
			// 含 `-`：它在拼好的单元名里是路径分隔符，不转义就有歧义。
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.String()
}

// UnitPlan 是渲染一个包装单元需要的全部输入。
//
// 它由 PlanUnit 从“已校验的定义 + 渲染结果 + 宿主事实”算出来，不再回头读任何东西：
// 渲染必须是确定的，任何一次“再查一下”都会让同一个定义在不同时刻产生不同的单元。
type UnitPlan struct {
	// Definition 是已归一化、已通过 CheckHost 的定义。
	Definition Definition
	// DeploymentID 是面板记录里的部署标识（由名字派生，与观测侧同一个算法）。
	DeploymentID string
	// ConfigDir 是配置目录（生产里恒为 DeployConfigDir）。
	ConfigDir string
	// ConfigPath 是渲染出来的 Compose 配置路径。
	ConfigPath string
	// Sources 是解析后的宿主路径（绑定来源），按容器路径排序。
	Sources []string
	// VolumeMountPoints 是这些来源与 volume_dependencies 所在的受管理卷挂载点，
	// 去重并按路径排序。单元对每一个写一条 RequiresMountsFor 与 BindsTo/After。
	VolumeMountPoints []string
}

// absPath 判断一个路径是否绝对。
//
// POSIX 语义为主（这些都是 Linux 上的路径），同时接受 Windows 盘符路径：
// 本包的单测在 Windows 上跑，只认 `/` 开头会让所有用例在那里直接失败，
// 而"在哪个系统上写代码"不该决定一条路径判据的形状。
func absPath(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return filepath.IsAbs(filepath.FromSlash(p))
}

// PlanUnit 把渲染结果与宿主事实折成一份单元计划（配置放默认目录）。
func PlanUnit(d Definition, rendered Rendered, env HostEnv) (UnitPlan, error) {
	return PlanUnitAt(d, rendered, env, DeployConfigDir)
}

// PlanUnitAt 与 PlanUnit 相同，但把配置目录作为参数。
//
// 存在的理由是执行端的单元目录/配置目录必须与单元里写的那条路径一致：
// 单元里写 /etc/upanel/deploy/x.compose.yaml，执行端却把配置写到别处，
// 单元起来时 compose 找不到 -f 指定的文件——而单元本身"启动成功"过。
// 生产里两者都是常量，测试里用参数把这条一致性变成可验证的东西。
func PlanUnitAt(d Definition, rendered Rendered, env HostEnv, configDir string) (UnitPlan, error) {
	// 目录名统一成 `/` 分隔再拼：单元文件里只能出现 `/`，而这条路径会被原样写进
	// ExecStart。Linux 上 ToSlash 是恒等变换，测试里（Windows 上跑 go test）才看得出作用。
	dir := filepath.ToSlash(strings.TrimSpace(configDir))
	if dir == "" || !absPath(dir) {
		return UnitPlan{}, fmt.Errorf("deploy: 配置目录必须是绝对路径，当前为 %q", configDir)
	}
	plan := UnitPlan{Definition: d, DeploymentID: DeploymentIDForName(d.Name), ConfigDir: dir}
	if !ValidDeploymentName(d.Name) {
		return plan, invalid(CodeMalformed, "name", "部署名 %q 不能拼进配置文件路径", d.Name)
	}
	plan.ConfigPath = path.Join(dir, d.Name+".compose.yaml")

	containers := make([]string, 0, len(rendered.MountedSources))
	for c := range rendered.MountedSources {
		containers = append(containers, c)
	}
	sort.Strings(containers)
	for _, c := range containers {
		plan.Sources = append(plan.Sources, rendered.MountedSources[c])
	}

	points := make(map[string]struct{})
	for i, m := range d.Mounts {
		field := fmt.Sprintf("mounts[%d].source_root_id", i)
		facts, ok := env.Roots[m.SourceRootID]
		if !ok {
			return plan, invalid(CodeUnknownRoot, field, "文件根 %q 没有登记过", m.SourceRootID)
		}
		vol, ok := env.Volumes[facts.VolumeID]
		if !ok {
			return plan, invalid(CodeVolumeMissing, field,
				"文件根 %q 所在的卷 %s 不在受管理卷登记表里", m.SourceRootID, facts.VolumeID)
		}
		mp, err := volumeMountPoint(facts.VolumeID, field, vol)
		if err != nil {
			return plan, err
		}
		if !withinDir(path.Clean(facts.Path), mp) {
			return plan, invalid(CodeNotAllowed, field,
				"文件根 %q 的路径 %s 不在它登记的卷 %s 的挂载点 %s 之内",
				m.SourceRootID, path.Clean(facts.Path), facts.VolumeID, mp)
		}
		points[mp] = struct{}{}
	}
	for _, id := range d.VolumeDependencies {
		vol, ok := env.Volumes[id]
		if !ok {
			return plan, invalid(CodeVolumeMissing, "volume_dependencies",
				"依赖卷 %s 不在受管理卷登记表里", id)
		}
		mp, err := volumeMountPoint(id, "volume_dependencies", vol)
		if err != nil {
			return plan, err
		}
		points[mp] = struct{}{}
	}
	for mp := range points {
		plan.VolumeMountPoints = append(plan.VolumeMountPoints, mp)
	}
	sort.Strings(plan.VolumeMountPoints)
	return plan, nil
}

// ComposeArgv 返回一次 compose 调用的完整 argv（不含 systemd）。
//
// 参数表是固定的：项目名与配置路径由单元计划决定，其余动词由调用方从闭集里选。
// 这里绝不能出现“把用户的某个字符串原样放进 argv”——argv 里多一个可写元素，
// docker 就离“任意命令执行器”近一步（container.control 只收 64 位容器 ID 是同一个理由）。
//
// 用到它的地方有两处：单元里的 ExecStart/ExecStop 文本，以及执行端在**停止单元之前**
// 做的读回核对（`compose ps`）。两者必须是同一份 argv，否则核对的对象就不是被启动的东西。
func ComposeArgv(name, configPath string, verbs ...string) ([]string, error) {
	if !ValidDeploymentName(name) {
		return nil, invalid(CodeMalformed, "name", "部署名 %q 不能进 compose 参数", name)
	}
	if !absPath(configPath) || strings.ContainsAny(configPath, " \t\r\n\"'\\$") {
		return nil, invalid(CodeNotAllowed, "name", "配置路径 %q 不能进 compose 参数", configPath)
	}
	if len(verbs) == 0 {
		return nil, fmt.Errorf("deploy: compose 调用没有动词")
	}
	argv := make([]string, 0, 6+len(verbs))
	argv = append(argv, DockerBinary, "compose", "-p", name, "-f", configPath)
	return append(argv, verbs...), nil
}

// RenderUnit 渲染包装单元。
//
// 单元里没有 shell、没有命令替换、没有环境变量展开：ExecStart 只有一条固定的
// docker compose 调用，项目名与配置路径都已经过形状校验。
func RenderUnit(p UnitPlan) ([]byte, error) {
	if !ValidDeploymentName(p.Definition.Name) {
		return nil, invalid(CodeMalformed, "name", "部署名 %q 不能拼进 systemd 单元名", p.Definition.Name)
	}
	if p.DeploymentID != DeploymentIDForName(p.Definition.Name) {
		return nil, fmt.Errorf("deploy: 部署标识 %q 与部署名 %q 不匹配",
			p.DeploymentID, p.Definition.Name)
	}
	if !absPath(p.ConfigPath) {
		return nil, fmt.Errorf("deploy: 配置路径 %q 不是绝对路径", p.ConfigPath)
	}
	if p.ConfigDir != "" && path.Dir(p.ConfigPath) != path.Clean(p.ConfigDir) {
		return nil, fmt.Errorf("deploy: 配置路径 %q 不在配置目录 %q 里", p.ConfigPath, p.ConfigDir)
	}
	up, err := ComposeArgv(p.Definition.Name, p.ConfigPath, "up", "-d", "--remove-orphans", "--no-build")
	if err != nil {
		return nil, err
	}
	down, err := ComposeArgv(p.Definition.Name, p.ConfigPath, "down", "--remove-orphans")
	if err != nil {
		return nil, err
	}

	mountUnits := make([]string, 0, len(p.VolumeMountPoints))
	for _, mp := range p.VolumeMountPoints {
		unit, err := MountUnitName(mp)
		if err != nil {
			return nil, err
		}
		mountUnits = append(mountUnits, unit)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "; uPanel 受管理部署：%s（%s）\n", p.Definition.Name, p.DeploymentID)
	b.WriteString(";\n")
	b.WriteString("; 这个文件由 uPanel 生成，每次 deployment.apply 整篇重写。不要手工修改：\n")
	b.WriteString("; 手工改动会在下一次部署时被覆盖，而覆盖后的行为以面板上的定义为准。\n")
	b.WriteString(";\n")
	b.WriteString("; 四条纪律，改单元的人一眼要能看见：\n")
	b.WriteString(";   生命周期  容器自己的 restart 是 no。开机、启动、停止都只有本单元一条路径。\n")
	b.WriteString(";             让 docker 在开机时拉容器会绕过下面的卷依赖（PLAN §11 规则 4）。\n")
	b.WriteString(";   卷依赖    BindsTo + After 逐卷绑定 mount unit：卷没挂上，本单元起不来；\n")
	b.WriteString(";             运行中卷失联，systemd 停掉本单元，ExecStop 把项目收下来（规则 3/5）。\n")
	b.WriteString(";   不建目录  本单元不创建任何目录，配置里 create_host_path 为 false。\n")
	b.WriteString(";             绑定源不存在时 compose 直接失败——挂载坏了必须看起来像坏了（规则 6）。\n")
	b.WriteString(";   无 shell   ExecStart/ExecStop 只有固定的 docker compose 调用，没有变量拼接、\n")
	b.WriteString(";             没有命令替换。单元里能改的东西越少，越不可能被改成别的动作。\n")
	b.WriteString("\n[Unit]\n")
	fmt.Fprintf(&b, "Description=uPanel 受管理部署：%s\n", p.Definition.Name)
	b.WriteString("Documentation=file:/usr/local/share/doc/upanel/DEPLOY.md\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("After=network-online.target docker.service\n")
	// Requires 而不是 Wants：docker 没起来时 compose 调用必然失败，那不该被当成
	// “这个单元启动成功、应用跑着”（Type=oneshot 的单元一旦 active 就是这个意思）。
	b.WriteString("Requires=docker.service\n")
	if len(p.VolumeMountPoints) > 0 {
		fmt.Fprintf(&b, "RequiresMountsFor=%s\n", strings.Join(p.VolumeMountPoints, " "))
		for _, unit := range mountUnits {
			fmt.Fprintf(&b, "BindsTo=%s\n", unit)
			fmt.Fprintf(&b, "After=%s\n", unit)
		}
	}
	b.WriteString("\n[Service]\n")
	// Type=oneshot + RemainAfterExit：`compose up -d` 是“拉起来就返回”的命令，
	// 它的返回值是这次启动的结果，不是容器的运行状态。容器活着由 docker 负责，
	// 单元只负责“我起过、我还算 active、停止时我来收”。
	b.WriteString("Type=oneshot\n")
	b.WriteString("RemainAfterExit=yes\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(up, " "))
	fmt.Fprintf(&b, "ExecStop=%s\n", strings.Join(down, " "))
	fmt.Fprintf(&b, "TimeoutStartSec=%d\n", ComposeUpTimeoutSeconds)
	fmt.Fprintf(&b, "TimeoutStopSec=%d\n", ComposeDownTimeoutSeconds)
	b.WriteString(";\n")
	b.WriteString("; up -d --remove-orphans：项目里多出来的容器（改了定义、名字对不上）随这次启动清掉，\n")
	b.WriteString("; 否则旧容器会继续占着端口，而面板上看到的是“新版本已部署”。\n")
	b.WriteString("; --no-build：镜像只来自 registry，永远不在本机构建——没有 Dockerfile，也没有构建上下文。\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return []byte(b.String()), nil
}
