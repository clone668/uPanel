package deploy

import (
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Normalize 校验**形状**并返回可入库、可渲染的规范形态。
//
// 它只碰定义自己能决定的事：字符集、范围、路径形状、必填项、默认值、排序。
// “这个地址是不是本机此刻的地址”“这个根在不在”“这个卷还在不在”属于宿主事实，
// 必须问 CheckHost——把二者混在一起，会让人以为一次校验就证明了部署能跑起来。
func Normalize(d Definition) (Definition, error) {
	var out Definition

	name := strings.TrimSpace(d.Name)
	if name == "" {
		return out, invalid(CodeMissing, "name", "部署需要一个名字")
	}
	if !namePattern.MatchString(name) {
		return out, invalid(CodeMalformed, "name",
			"部署名%s。只能用小写字母、数字、下划线、中划线，且必须以小写字母开头（收到 %q，%d 个字符）",
			nameShapeHint(name), name, utf8.RuneCountInString(name))
	}
	out.Name = name

	ref, err := parseImageReference(strings.TrimSpace(d.ImageReference))
	if err != nil {
		return out, err
	}
	out.ImageReference = ref.String()

	if err := checkPort("container_port", d.ContainerPort); err != nil {
		return out, err
	}
	out.ContainerPort = d.ContainerPort
	if err := checkPort("host_port", d.HostPort); err != nil {
		return out, err
	}
	out.HostPort = d.HostPort

	addr := strings.TrimSpace(d.HostBindAddress)
	if addr == "" {
		return out, invalid(CodeMissing, "host_bind_address",
			"必须指定宿主绑定地址；面板不接受“随便绑一个”——那等于把端口暴露到所有网卡上")
	}
	if !isIPLiteral(addr) {
		return out, invalid(CodeMalformed, "host_bind_address",
			"必须是字面 IP（例如 127.0.0.1 或 192.0.2.10），不接受主机名或带端口的写法（收到 %q）", addr)
	}
	out.HostBindAddress = addr

	env, err := normalizeEnvironment(d.Environment)
	if err != nil {
		return out, err
	}
	out.Environment = env

	mounts, err := normalizeMounts(d.Mounts)
	if err != nil {
		return out, err
	}
	out.Mounts = mounts

	limits, err := normalizeResourceLimits(d.ResourceLimits)
	if err != nil {
		return out, err
	}
	out.ResourceLimits = limits

	health, err := normalizeHealthCheck(d.HealthCheck, out.ContainerPort)
	if err != nil {
		return out, err
	}
	out.HealthCheck = health

	deps, err := normalizeVolumeDependencies(d.VolumeDependencies)
	if err != nil {
		return out, err
	}
	out.VolumeDependencies = deps

	return out, nil
}

// nameShapeHint 只说那条具体被违反的规则，不把三条规则一起念一遍。
func nameShapeHint(name string) string {
	switch {
	case len(name) > MaxNameLen:
		return fmt.Sprintf("最长 %d 个字符", MaxNameLen)
	case name[0] >= '0' && name[0] <= '9':
		return "不能以数字开头"
	case strings.ContainsAny(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"):
		return "不能含大写字母"
	default:
		return "含不允许的字符"
	}
}

func checkPort(field string, port int) error {
	if port < MinPort || port > MaxPort {
		return invalid(CodeOutOfRange, field,
			"端口必须在 %d 到 %d 之间（收到 %d）；0 表示随机端口，会让面板记下的入口地址失效",
			MinPort, MaxPort, port)
	}
	return nil
}

// normalizeEnvironment 校验并按名字排序。
//
// 三条硬规则：名字是 POSIX 标识符、名字不重复、值与秘密引用二选一。
func normalizeEnvironment(raw []Env) ([]Env, error) {
	if len(raw) > MaxEnvVars {
		return nil, invalid(CodeOutOfRange, "environment", "环境变量最多 %d 条（收到 %d）", MaxEnvVars, len(raw))
	}
	seen := make(map[string]bool, len(raw))
	out := make([]Env, 0, len(raw))
	for i, e := range raw {
		field := fmt.Sprintf("environment[%d]", i)
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return nil, invalid(CodeMissing, field+".name", "环境变量缺少名字")
		}
		if len(name) > MaxEnvNameLen {
			return nil, invalid(CodeOutOfRange, field+".name", "环境变量名最长 %d 个字符", MaxEnvNameLen)
		}
		if !envNamePattern.MatchString(name) {
			return nil, invalid(CodeMalformed, field+".name",
				"环境变量名必须是 POSIX 标识符（字母或下划线开头，只含字母、数字、下划线），收到 %q", name)
		}
		if seen[name] {
			return nil, invalid(CodeDuplicate, field+".name", "环境变量 %q 重复", name)
		}
		seen[name] = true

		norm := Env{Name: name}
		switch {
		case e.Value != nil && e.SecretRef != "":
			return nil, invalid(CodeNotAllowed, field,
				"环境变量 %q 同时给了 value 和 secret_ref；秘密只能走 secret_ref，公开值只能走 value", name)
		case e.Value == nil && e.SecretRef == "":
			return nil, invalid(CodeMissing, field,
				"环境变量 %q 既没有 value 也没有 secret_ref；要置空请显式给 value: \"\"", name)
		case e.SecretRef != "":
			ref := strings.TrimSpace(e.SecretRef)
			if !validSecretRefShape(ref) {
				return nil, invalid(CodeMalformed, field+".secret_ref",
					"秘密引用形状不对（期望 sec_ 加 32 位小写十六进制），收到 %q", ref)
			}
			norm.SecretRef = ref
		default:
			value := *e.Value
			if len(value) > MaxEnvValueBytes {
				return nil, invalid(CodeOutOfRange, field+".value",
					"环境变量 %q 的值超过 %d 字节", name, MaxEnvValueBytes)
			}
			if strings.IndexByte(value, 0) >= 0 {
				return nil, invalid(CodeMalformed, field+".value", "环境变量 %q 的值里不能有 NUL 字节", name)
			}
			norm.Value = &value
		}
		out = append(out, norm)
	}
	return sortedEnv(out), nil
}

// blockedContainerPaths 是容器内**不允许**被遮住的挂载目标。
//
// 判据不是“这个路径敏感”，而是“把这个路径交给镜像以外的人去决定，等于把内核接口或
// 宿主控制面塞给容器”：
//
//   - `/`：一条挂载就把整个镜像盖掉，之后跑起来的是什么不再由镜像决定；
//   - `/proc`、`/sys`、`/dev`：内核与设备接口，CONTRACTS.md §8.2 明确列出；
//   - `/boot`、`/lib/modules`：内核与模块，容器里碰它们只可能是越界；
//   - `/run` 下的 docker/podman socket：容器逃逸的经典路径（宿主 control plane）。
var blockedContainerPaths = []string{
	"/proc", "/sys", "/dev", "/boot", "/lib/modules",
	"/run/docker.sock", "/var/run/docker.sock",
	"/run/podman/podman.sock", "/run/containerd/containerd.sock",
}

// normalizeMounts 校验并按容器路径排序。
func normalizeMounts(raw []Mount) ([]Mount, error) {
	if len(raw) > MaxMounts {
		return nil, invalid(CodeOutOfRange, "mounts", "挂载最多 %d 条（收到 %d）", MaxMounts, len(raw))
	}
	seen := make(map[string]bool, len(raw))
	out := make([]Mount, 0, len(raw))
	for i, m := range raw {
		field := fmt.Sprintf("mounts[%d]", i)

		rootID := strings.TrimSpace(m.SourceRootID)
		if rootID == "" {
			return nil, invalid(CodeMissing, field+".source_root_id",
				"挂载缺少来源文件根；宿主路径不能直接写，只能用已登记的文件根")
		}
		if !rootIDPattern.MatchString(rootID) {
			return nil, invalid(CodeMalformed, field+".source_root_id",
				"文件根 id 不合法（小写字母开头，只含小写字母、数字、下划线、中划线，最多 32 字符），收到 %q", rootID)
		}

		rel, err := normalizeRelativePath(field+".relative_path", m.RelativePath)
		if err != nil {
			return nil, err
		}

		target := strings.TrimSpace(m.ContainerPath)
		if target == "" {
			return nil, invalid(CodeMissing, field+".container_path", "挂载缺少容器内路径")
		}
		if !strings.HasPrefix(target, "/") {
			return nil, invalid(CodeMalformed, field+".container_path",
				"容器内路径必须是绝对路径（收到 %q）", target)
		}
		if len(target) > MaxContainerPathLen {
			return nil, invalid(CodeOutOfRange, field+".container_path",
				"容器内路径最长 %d 个字符", MaxContainerPathLen)
		}
		if hasControl(target) {
			return nil, invalid(CodeMalformed, field+".container_path", "容器内路径不能含控制字符")
		}
		if path.Clean(target) != target {
			return nil, invalid(CodeMalformed, field+".container_path",
				"容器内路径不是规范形式（应为 %q）", path.Clean(target))
		}
		if target == "/" {
			return nil, invalid(CodeNotAllowed, field+".container_path",
				"不能把挂载盖在容器根目录上：那会让镜像内容整个失效，跑起来的是什么不再由镜像决定")
		}
		for _, blocked := range blockedContainerPaths {
			if target == blocked || strings.HasPrefix(target, blocked+"/") {
				return nil, invalid(CodeNotAllowed, field+".container_path",
					"不能挂到 %s（%s 及其子路径不允许作为容器内挂载目标）", target, blocked)
			}
		}
		if seen[target] {
			return nil, invalid(CodeDuplicate, field+".container_path", "容器内路径 %q 重复", target)
		}
		seen[target] = true

		out = append(out, Mount{
			SourceRootID:  rootID,
			RelativePath:  rel,
			ContainerPath: target,
			ReadOnly:      m.ReadOnly,
		})
	}
	return sortedMounts(out), nil
}

// normalizeRelativePath 校验根内相对路径。
//
// 空串与 "." 都归一化成 "."：它们指的都是根自身。除此之外必须是 path.Clean 的不动点——
// 于是 `..`、`a/../b`、`/abs`、`a//b` 全部在形状层面就写不出来。字符串层面只能到此为止：
// 软链接逃逸要在真正打开目录时用 openat2 挡住（P15.2 执行前核身份）。
func normalizeRelativePath(field, raw string) (string, error) {
	rel := strings.TrimSpace(raw)
	if rel == "" || rel == "." {
		return ".", nil
	}
	if len(rel) > MaxRelativePathLen {
		return "", invalid(CodeOutOfRange, field, "根内相对路径最长 %d 个字符", MaxRelativePathLen)
	}
	if hasControl(rel) {
		return "", invalid(CodeMalformed, field, "根内相对路径不能含控制字符")
	}
	if strings.HasPrefix(rel, "/") {
		return "", invalid(CodeNotAllowed, field,
			"根内相对路径不能是绝对路径（收到 %q）；宿主路径只能由“文件根 + 相对路径”拼出来", rel)
	}
	cleaned := path.Clean(rel)
	if cleaned != rel {
		return "", invalid(CodeNotAllowed, field,
			"根内相对路径必须是规范形式（收到 %q，规范形式是 %q）；`..`、重复斜杠、`.` 段都在这里被拒绝", rel, cleaned)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", invalid(CodeNotAllowed, field, "根内相对路径不能跳出文件根（收到 %q）", rel)
	}
	return cleaned, nil
}

// normalizeResourceLimits 校验两项必填的资源上限。
func normalizeResourceLimits(raw ResourceLimits) (ResourceLimits, error) {
	var out ResourceLimits
	if raw.MemoryLimitBytes == 0 {
		return out, invalid(CodeMissing, "resource_limits.memory_limit_bytes",
			"内存上限必填（字节）；不设上限的容器会把整台机器拖垮")
	}
	if raw.MemoryLimitBytes < MinMemoryLimitBytes || raw.MemoryLimitBytes > MaxMemoryLimitBytes {
		return out, invalid(CodeOutOfRange, "resource_limits.memory_limit_bytes",
			"内存上限必须在 %d 到 %d 字节之间（收到 %d）", MinMemoryLimitBytes, MaxMemoryLimitBytes, raw.MemoryLimitBytes)
	}
	out.MemoryLimitBytes = raw.MemoryLimitBytes

	cpu := strings.TrimSpace(raw.CPULimit)
	if cpu == "" {
		return out, invalid(CodeMissing, "resource_limits.cpu_limit", "CPU 上限必填（核数，例如 \"0.5\"）")
	}
	if !cpuLimitPattern.MatchString(cpu) {
		return out, invalid(CodeMalformed, "resource_limits.cpu_limit",
			"CPU 上限必须是十进制小数（例如 \"0.5\" 或 \"2\"），收到 %q", cpu)
	}
	v, err := strconv.ParseFloat(cpu, 64)
	if err != nil {
		return out, invalid(CodeMalformed, "resource_limits.cpu_limit", "CPU 上限无法解析（收到 %q）", cpu)
	}
	if v < MinCPULimit || v > MaxCPULimit {
		return out, invalid(CodeOutOfRange, "resource_limits.cpu_limit",
			"CPU 上限必须在 %g 到 %g 核之间（收到 %q）", MinCPULimit, MaxCPULimit, cpu)
	}
	out.CPULimit = formatFloatShorthand(v)
	return out, nil
}

// normalizeHealthCheck 校验面板侧探测描述并补默认值。
//
// 关闭时把其余字段清零：一份“关掉的健康检查”里留着 kind/port 只会让人以为它还在生效。
func normalizeHealthCheck(raw HealthCheck, containerPort int) (HealthCheck, error) {
	if !raw.Enabled {
		return HealthCheck{}, nil
	}
	out := HealthCheck{Enabled: true}

	switch raw.Kind {
	case "http", "tcp":
		out.Kind = raw.Kind
	case "":
		return out, invalid(CodeMissing, "health_check.kind", "启用健康检查时必须给 kind（http 或 tcp）")
	default:
		return out, invalid(CodeNotAllowed, "health_check.kind",
			"健康检查只支持 http 与 tcp（收到 %q）；udp 没有“通了”这回事", raw.Kind)
	}

	port := raw.Port
	if port == 0 {
		port = containerPort
	}
	if port != containerPort {
		return out, invalid(CodeNotAllowed, "health_check.port",
			"健康检查只能探容器端口 %d：面板只发布这一个端口，探别的端口等于探一个从外面连不上的东西（收到 %d）",
			containerPort, raw.Port)
	}
	out.Port = port

	if out.Kind == "http" {
		p := strings.TrimSpace(raw.Path)
		if p == "" {
			return out, invalid(CodeMissing, "health_check.path", "http 健康检查必须给 path（例如 /healthz）")
		}
		if !strings.HasPrefix(p, "/") {
			return out, invalid(CodeMalformed, "health_check.path",
				"健康检查路径必须以 / 开头，且只能是路径不能是完整 URL（收到 %q）", p)
		}
		if hasControl(p) {
			return out, invalid(CodeMalformed, "health_check.path", "健康检查路径不能含控制字符")
		}
		if len(p) > 256 {
			return out, invalid(CodeOutOfRange, "health_check.path", "健康检查路径最长 256 个字符")
		}
		out.Path = p
	} else if strings.TrimSpace(raw.Path) != "" {
		return out, invalid(CodeNotAllowed, "health_check.path", "tcp 健康检查不接受 path")
	}

	interval := raw.IntervalSeconds
	if interval == 0 {
		interval = DefaultHealthIntervalSeconds
	}
	if interval < MinHealthIntervalSeconds || interval > MaxHealthIntervalSeconds {
		return out, invalid(CodeOutOfRange, "health_check.interval_seconds",
			"探测间隔必须在 %d 到 %d 秒之间（收到 %d）", MinHealthIntervalSeconds, MaxHealthIntervalSeconds, interval)
	}
	out.IntervalSeconds = interval

	timeout := raw.TimeoutSeconds
	if timeout == 0 {
		timeout = DefaultHealthTimeoutSeconds
	}
	if timeout < MinHealthTimeoutSeconds || timeout > MaxHealthTimeoutSeconds {
		return out, invalid(CodeOutOfRange, "health_check.timeout_seconds",
			"单次超时必须在 %d 到 %d 秒之间（收到 %d）", MinHealthTimeoutSeconds, MaxHealthTimeoutSeconds, timeout)
	}
	if timeout >= interval {
		return out, invalid(CodeOutOfRange, "health_check.timeout_seconds",
			"单次超时（%d 秒）必须小于探测间隔（%d 秒），否则上一轮还没结束下一轮就开始了", timeout, interval)
	}
	out.TimeoutSeconds = timeout

	retries := raw.Retries
	if retries == 0 {
		retries = DefaultHealthRetries
	}
	if retries < 1 || retries > MaxHealthRetries {
		return out, invalid(CodeOutOfRange, "health_check.retries",
			"连续失败次数必须在 1 到 %d 之间（收到 %d）", MaxHealthRetries, retries)
	}
	out.Retries = retries

	if raw.StartPeriodSeconds < 0 || raw.StartPeriodSeconds > MaxHealthStartSeconds {
		return out, invalid(CodeOutOfRange, "health_check.start_period_seconds",
			"启动宽限期必须在 0 到 %d 秒之间（收到 %d）", MaxHealthStartSeconds, raw.StartPeriodSeconds)
	}
	out.StartPeriodSeconds = raw.StartPeriodSeconds
	return out, nil
}

// normalizeVolumeDependencies 校验依赖卷标识并去重（保持输入顺序）。
//
// 形状复用 storage 的判据；卷在不在是宿主事实，由 CheckHost 查。
func normalizeVolumeDependencies(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxVolumeDependencies {
		return nil, invalid(CodeOutOfRange, "volume_dependencies",
			"依赖卷最多 %d 个（收到 %d）", MaxVolumeDependencies, len(raw))
	}
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if !validVolumeID(id) {
			return nil, invalid(CodeMalformed, "volume_dependencies",
				"依赖卷标识必须是 vol_ 加 32 位小写十六进制，收到 %q", id)
		}
		out = append(out, id)
	}
	return sortedVolumeIDs(out), nil
}

// HostEnv 是一份定义**无法自己知道**的那部分事实。
//
// 它由调用方（P15.2 的存储层、代理侧的卷表与网卡表）填进来。所有字段缺失时的行为都是
// **更严**而不是更松：没有根表就挂不了东西，没有卷表就认不了依赖卷。
type HostEnv struct {
	// LocalAddresses 是本机此刻真的配着的地址（不含回环与通配，那两个单独判）。
	LocalAddresses []net.IP
	// AllowWildcard 是否允许 0.0.0.0 / ::。默认应关：面板安装完的部署只该绑本机。
	AllowWildcard bool
	// Roots 是已登记的文件根（id → 路径与可写性）。
	Roots map[string]RootFacts
	// DataBase 是允许作为容器挂载来源的基准目录（绝对路径，例如 /srv/upanel/volumes）。
	// 空表示“没有配置”，此时任何挂载都拒绝——没配基准就等于没有可挂的地方。
	DataBase string
	// Volumes 是已知受管理卷此刻的事实（volume_dependencies 与挂载来源的卷必须在这里面）。
	//
	// P15.1 时它只是个集合（在不在）。P15.2 起必须带挂载点与文件系统 UUID：
	// 包装单元的 RequiresMountsFor/BindsTo 要拼出挂载点对应的 mount unit 名，
	// 启动前的 UUID 核对要拿登记值与此刻挂着的值比。一个只有“在”的集合
	// 能让校验通过，却渲染不出一个真的会被卷挡住启动的单元。
	Volumes map[string]VolumeFacts
}

// VolumeFacts 是一个受管理卷此刻的事实。
type VolumeFacts struct {
	// MountPoint 是卷的挂载点（`/srv/upanel/volumes/<slug>`）。必须绝对、非 /。
	MountPoint string
	// FilesystemUUID 是登记的文件系统 UUID（fstab 那一行的依据）。
	FilesystemUUID string
	// Mounted 表示此刻卷是不是真的挂在这个挂载点上。
	Mounted bool
	// MountedUUID 是此刻挂在这个挂载点上的文件系统的 UUID（读不到时为空）。
	//
	// 它与 FilesystemUUID 相等才算“还是原来那块盘”。挂着一块 UUID 不同、
	// 或者根本没挂（目录空着）的情形，都会让容器起来往系统盘写数据。
	MountedUUID string
}

// RootFacts 是一个文件根此刻的事实。
type RootFacts struct {
	// Path 是根的绝对路径（配置里写的那个）。
	Path string
	// Writable 表示这个根允许写入。只读的根只能以只读方式挂进容器。
	Writable bool
	// VolumeID 是根所在受管理卷的标识。空表示这个根不在任何受管理卷里——
	// 那种根不能当挂载来源：依赖卷门禁查不到它，卷没挂上时容器照样会被拉起来，
	// 然后往系统盘上的空目录里写数据（STAGES.md APP-03 / DEPLOY-01）。
	VolumeID string
}

// CheckHost 用宿主事实复核一份**已规范化**的定义。
//
// 它回答三个“定义自己答不了”的问题：这个地址是不是本机的、这个根在不在且落不落在部署
// 数据根里、这些依赖卷还在不在。三个都不是格式问题——它们会随时间变化，所以每次部署前
// 都要重问一遍。
func CheckHost(d Definition, env HostEnv) error {
	ip := net.ParseIP(d.HostBindAddress)
	if ip == nil {
		return invalid(CodeMalformed, "host_bind_address", "绑定地址不是字面 IP（收到 %q）", d.HostBindAddress)
	}
	switch {
	case ip.IsLoopback():
		// 回环永远允许：它不对外暴露任何东西。
	case ip.IsUnspecified():
		if !env.AllowWildcard {
			return invalid(CodeNotAllowed, "host_bind_address",
				"不允许绑定 %s（监听所有网卡）。要么绑 127.0.0.1 由反向代理转发，要么明确绑定某张网卡的地址", ip)
		}
	case !localAddress(ip, env.LocalAddresses):
		return invalid(CodeNotAllowed, "host_bind_address",
			"%s 不是本机此刻的地址；不能把一个本机没有的地址当作绑定目标", ip)
	}

	// 依赖卷的登记表先查：一条“声明的依赖卷已经不在了”比“某个挂载的根所在的卷不在了”
	// 更接近根因，两条同时命中时应该看到前者。
	for _, id := range d.VolumeDependencies {
		vol, ok := env.Volumes[id]
		if !ok {
			return invalid(CodeVolumeMissing, "volume_dependencies",
				"依赖卷 %s 不在受管理卷登记表里（已删除、没导入，或这一轮没读到）；"+
					"卷不在就不部署——起来也会把数据写进系统盘上的空目录", id)
		}
		if _, err := volumeMountPoint(id, "volume_dependencies", vol); err != nil {
			return err
		}
	}

	if len(d.Mounts) > 0 {
		base, err := checkDataBase(env.DataBase)
		if err != nil {
			return err
		}
		for i, m := range d.Mounts {
			field := fmt.Sprintf("mounts[%d]", i)
			facts, ok := env.Roots[m.SourceRootID]
			if !ok {
				return invalid(CodeUnknownRoot, field+".source_root_id",
					"文件根 %q 没有登记过；未登记的文件根不能当挂载来源", m.SourceRootID)
			}
			rootPath := path.Clean(facts.Path)
			if !path.IsAbs(rootPath) || rootPath == "/" {
				return invalid(CodeNotAllowed, field+".source_root_id",
					"文件根 %q 的路径 %q 不是可用的绝对目录路径", m.SourceRootID, facts.Path)
			}
			if !withinBase(rootPath, base) {
				return invalid(CodeNotAllowed, field+".source_root_id",
					"文件根 %q 指向 %s，不在部署数据根 %s 之内；挂载来源只能是受管理卷下的目录",
					m.SourceRootID, rootPath, base)
			}
			if !m.ReadOnly && !facts.Writable {
				return invalid(CodeNotAllowed, field+".read_only",
					"文件根 %q 是只读的，不能以读写方式挂进容器；要么改用可写的根，要么把 read_only 设为 true",
					m.SourceRootID)
			}
			source := path.Join(rootPath, m.RelativePath)
			if !withinDir(source, rootPath) || !withinBase(source, base) {
				return invalid(CodeNotAllowed, field+".relative_path",
					"解析出来的宿主路径 %s 跳出了文件根 %s", source, rootPath)
			}
			if strings.ContainsAny(source, ":\r\n\x00") {
				return invalid(CodeNotAllowed, field,
					"解析出来的宿主路径含冒号或换行（%q），不能安全写进配置", source)
			}
			// 挂载必须落在**受管理卷**上，而且那个卷必须已经登记在 volume_dependencies 里。
			// 少了这一环，“卷缺失时重启也不能绕过保护”就断在半路：门禁守的是依赖卷，
			// 而容器吃的是这个根。三者（文件根 → 卷 → 依赖声明）必须对得上。
			if facts.VolumeID == "" {
				return invalid(CodeNotAllowed, field+".source_root_id",
					"文件根 %q 不在任何受管理卷里，挂载不受依赖卷门禁保护；数据目录只能落在受管理卷上",
					m.SourceRootID)
			}
			vol, ok := env.Volumes[facts.VolumeID]
			if !ok {
				return invalid(CodeVolumeMissing, field+".source_root_id",
					"文件根 %q 所在的卷 %s 不在受管理卷登记表里（已删除或没导入）；卷不在就不部署",
					m.SourceRootID, facts.VolumeID)
			}
			// 根的路径必须真的在那个卷的挂载点里。只写“卷在”是不够的：
			// 记着卷 id 而路径指向别处，等于门禁守 A 卷、容器吃 B 目录。
			mp, err := volumeMountPoint(facts.VolumeID, field+".source_root_id", vol)
			if err != nil {
				return err
			}
			if !withinDir(rootPath, mp) {
				return invalid(CodeNotAllowed, field+".source_root_id",
					"文件根 %q 的路径 %s 不在它登记的卷 %s 的挂载点 %s 之内；"+
						"卷依赖与挂载来源指向两个地方时，卷没挂上也不会有人发现",
					m.SourceRootID, rootPath, facts.VolumeID, mp)
			}
			if !containsID(d.VolumeDependencies, facts.VolumeID) {
				return invalid(CodeVolumeMissing, field+".source_root_id",
					"文件根 %q 挂在卷 %s 上，但 volume_dependencies 里没有这个卷；"+
						"依赖卷门禁查不到它，卷缺失时容器照样会被拉起，数据会写进系统盘上的空目录",
					m.SourceRootID, facts.VolumeID)
			}
		}
	}

	return nil
}

// volumeMountPoint 校验并返回卷的挂载点。
//
// 挂载点要拼进 systemd 的 RequiresMountsFor 与 BindsTo，所以它必须是一个能出现在
// 单元文件里的绝对路径：相对路径、`/`、含空白或换行的写法一律不要。没有挂载点的卷
// 依然可以“在登记表里”，但它挡不住任何东西——那种卷不能当依赖声明。
func volumeMountPoint(volumeID, field string, vol VolumeFacts) (string, error) {
	raw := strings.TrimSpace(vol.MountPoint)
	mp := path.Clean(raw)
	if raw == "" || !path.IsAbs(mp) || mp == "/" || mp == "." {
		return "", invalid(CodeNotAllowed, field,
			"受管理卷 %s 没有可用的挂载点（收到 %q）；没有挂载点就拼不出对应的 mount unit，"+
				"“卷不在就不启动”也就无从谈起", volumeID, vol.MountPoint)
	}
	if strings.ContainsAny(mp, " \t\r\n\x00") {
		return "", invalid(CodeNotAllowed, field,
			"受管理卷 %s 的挂载点含空白或控制字符（%q），不能写进 systemd 单元", volumeID, vol.MountPoint)
	}
	return mp, nil
}

// CheckVolumesReady 核对依赖卷此刻是不是真的挂着、而且挂的是**登记过的那块盘**。
//
// 这是 PLAN.md 第 11 节规则 2/4 里“启动前 UUID 校验”的那一步：单元文件里的
// BindsTo 只能保证“挂载点挂上了”，保证不了“挂上的是这个卷”。挂了一块别的盘、
// 或者挂载点空着（目录还在系统盘上），容器都会照常起来往空目录里写数据，
// 而面板上一片绿。这种情况必须在启动之前发现。
//
// 它返回的是**拒绝理由**（按卷挂载点排序，稳定），不是错误：一个卷没准备好不是
// 编程错误，是“现在还不能部署”。调用方把它放进预检的 blockers。
func CheckVolumesReady(d Definition, env HostEnv) []string {
	var problems []string
	seen := make(map[string]struct{}, len(d.VolumeDependencies))
	for _, id := range d.VolumeDependencies {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		vol, ok := env.Volumes[id]
		if !ok {
			// 形状层已经拦过一次；这里是“预检时在、执行时不在”的那一段。
			problems = append(problems, fmt.Sprintf(
				"依赖卷 %s 已经不在受管理卷登记表里了", id))
			continue
		}
		mp := path.Clean(strings.TrimSpace(vol.MountPoint))
		if !vol.Mounted {
			problems = append(problems, fmt.Sprintf(
				"依赖卷 %s 此刻没有挂载（挂载点 %s）；容器会在系统盘上的空目录里创建数据", id, mp))
			continue
		}
		want := strings.TrimSpace(vol.FilesystemUUID)
		got := strings.TrimSpace(vol.MountedUUID)
		if want != "" && got != want {
			problems = append(problems, fmt.Sprintf(
				"依赖卷 %s 的挂载点 %s 上此刻是另一块文件系统（登记 %s，实际 %s）；"+
					"UUID 对不上就不部署——数据可能写进别人的盘", id, mp, want, gotOrNone(got)))
		}
	}
	return problems
}

// gotOrNone 让“读不到 UUID”在消息里说成读不到，而不是一个空字符串。
func gotOrNone(uuid string) string {
	if uuid == "" {
		return "读不到"
	}
	return uuid
}

// checkDataBase 校验部署数据根，返回规范化后的路径。
func checkDataBase(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", invalid(CodeNotAllowed, "mounts",
			"面板没有配置部署数据根，无法确认挂载来源落在受管理范围内；此时不接受任何挂载")
	}
	cleaned := path.Clean(base)
	if !path.IsAbs(cleaned) || cleaned == "/" {
		return "", invalid(CodeNotAllowed, "mounts",
			"部署数据根必须是绝对路径且不能是 /（收到 %q）", base)
	}
	return cleaned, nil
}

// withinBase 判断 p 是否落在 base 之内（含 base 自身）。
func withinBase(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

// withinDir 判断 p 是否落在 dir 之内（含 dir 自身）。
func withinDir(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// VolumeOfPath 返回包含这个路径的受管理卷（最长挂载点优先），没有则返回空串。
//
// 代理侧用它把“文件根的路径”翻译成“它落在哪个卷上”，校验侧用它判断这条翻译对不对。
// 两边必须是同一份规则：调用点自己写一遍前缀比较，就会出现“登记说在 A 卷、门禁查的
// 是 B 卷”这种只能靠现场才发现的错。
//
// 最长优先而不是短优先：卷的挂载点可以嵌套（`/srv/upanel/volumes` 本身是一块盘、
// `/srv/upanel/volumes/media` 是另一块），目录落在里面那层时，真正兜住数据的是里面那块。
// 挂载点不是绝对路径、或者是 `/` 的卷直接跳过——那种卷挡不住任何东西（见 volumeMountPoint）。
func VolumeOfPath(p string, volumes map[string]VolumeFacts) string {
	clean := path.Clean(p)
	if !path.IsAbs(clean) {
		return ""
	}
	best, bestLen := "", 0
	for id, vol := range volumes {
		mp := path.Clean(strings.TrimSpace(vol.MountPoint))
		if !path.IsAbs(mp) || mp == "/" || mp == "." {
			continue
		}
		if !withinDir(clean, mp) {
			continue
		}
		if len(mp) > bestLen {
			best, bestLen = id, len(mp)
		}
	}
	return best
}

// containsID 判断标识是否在集合里（依赖卷的顺序无意义，只问在不在）。
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// localAddress 判断 ip 是否在本机地址表里（按字节比较，v4-mapped 也算同一台）。
func localAddress(ip net.IP, local []net.IP) bool {
	for _, candidate := range local {
		if candidate.Equal(ip) {
			return true
		}
	}
	return false
}

// hasControl 判断字符串里是否有控制字符（含换行）。
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// imageRef 是拆开的镜像引用。
type imageRef struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// String 拼回规范形态：`[registry/]repository[:tag][@digest]`。
func (r imageRef) String() string {
	var b strings.Builder
	if r.Registry != "" {
		b.WriteString(r.Registry)
		b.WriteByte('/')
	}
	b.WriteString(r.Repository)
	if r.Tag != "" {
		b.WriteByte(':')
		b.WriteString(r.Tag)
	}
	if r.Digest != "" {
		b.WriteByte('@')
		b.WriteString(r.Digest)
	}
	return b.String()
}

// ParseImageReference 校验并拆一份镜像引用，导出形态供 P15.3 读取预期 digest。
func ParseImageReference(raw string) (imageRef, error) { return parseImageReference(raw) }

// parseImageReference 校验镜像引用并补上隐式 tag。
//
// 允许 tag、允许 digest、允许两者都给（tag 只作线索，真正执行的是 digest）。没给任何
// 一个时补 `:latest`：Docker 的默认值就是这个，写出来之后摘要不会因为“写没写 latest”
// 而分叉。
func parseImageReference(raw string) (imageRef, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return imageRef{}, invalid(CodeMissing, "image_reference", "部署需要镜像引用")
	}
	if len(s) > MaxImageRefLen {
		return imageRef{}, invalid(CodeOutOfRange, "image_reference", "镜像引用最长 %d 个字符", MaxImageRefLen)
	}
	if hasControl(s) || strings.ContainsAny(s, " \t") {
		return imageRef{}, invalid(CodeMalformed, "image_reference",
			"镜像引用里不能有空白或控制字符（收到 %q）", s)
	}
	if strings.Contains(s, "://") {
		return imageRef{}, invalid(CodeMalformed, "image_reference",
			"镜像引用不带协议前缀（收到 %q）", s)
	}

	var out imageRef
	if at := strings.Index(s, "@"); at >= 0 {
		digest := s[at+1:]
		s = s[:at]
		if !validDigest(digest) {
			return imageRef{}, invalid(CodeMalformed, "image_reference",
				"镜像 digest 必须是 sha256: 加 64 位小写十六进制（或 sha512: 加 128 位），收到 %q", digest)
		}
		out.Digest = digest
	}

	// tag 在最后一个 '/' 之后的最后一个 ':'。registry 的端口号在最后一个 '/' 之前，
	// 因此不会被误当成 tag 分隔符。
	slash := strings.LastIndex(s, "/")
	colon := strings.LastIndex(s, ":")
	if colon > slash {
		out.Tag = s[colon+1:]
		s = s[:colon]
		if !validTag(out.Tag) {
			return imageRef{}, invalid(CodeMalformed, "image_reference",
				"镜像 tag 不合法（字母或数字开头，只含字母、数字、下划线、点、中划线，最多 128 字符），收到 %q", out.Tag)
		}
	}

	if s == "" {
		return imageRef{}, invalid(CodeMalformed, "image_reference", "镜像引用缺少仓库名")
	}

	name := s
	if first := strings.Index(s, "/"); first > 0 {
		head := s[:first]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			if !validRegistry(head) {
				return imageRef{}, invalid(CodeMalformed, "image_reference",
					"镜像仓库地址不合法（小写主机名，可带 1—65535 的端口），收到 %q", head)
			}
			out.Registry = head
			name = s[first+1:]
		}
	}
	if !validRepository(name) {
		return imageRef{}, invalid(CodeMalformed, "image_reference",
			"镜像仓库名不合法（小写字母、数字，允许 . _ - 分隔，不能有大写、空段或首尾分隔符），收到 %q", name)
	}
	out.Repository = name

	if out.Tag == "" && out.Digest == "" {
		out.Tag = "latest"
	}
	return out, nil
}

// validTag 与 Docker 的 tag 判据同形。
func validTag(tag string) bool {
	if tag == "" || len(tag) > 128 {
		return false
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// validDigest 只接受 sha256 / sha512（判据来自 OCI image spec 的 digest 形态）。
func validDigest(d string) bool {
	switch {
	case strings.HasPrefix(d, "sha256:") && len(d) == len("sha256:")+64:
		return isLowerHex(d[len("sha256:"):])
	case strings.HasPrefix(d, "sha512:") && len(d) == len("sha512:")+128:
		return isLowerHex(d[len("sha512:"):])
	}
	return false
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}

// validRegistry 校验 registry 主机与可选端口。
func validRegistry(host string) bool {
	h := host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		h = host[:i]
		portStr := host[i+1:]
		port, err := strconv.Atoi(portStr)
		if err != nil || port < MinPort || port > MaxPort {
			return false
		}
	}
	if h == "" || len(h) > 253 {
		return false
	}
	// IPv6 字面量在镜像引用里写作 [::1]:5000，方括号只在 registry 位置出现。
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		return net.ParseIP(h[1:len(h)-1]) != nil
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// validRepository 校验仓库名（registry 之后的部分）。
func validRepository(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" {
			return false
		}
		// 每段：小写字母或数字开头结尾，中间允许 . _ - 分隔。
		prevSep := true
		for i := 0; i < len(segment); i++ {
			c := segment[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
				prevSep = false
			case c == '.' || c == '_' || c == '-':
				if prevSep {
					return false
				}
				prevSep = true
			default:
				return false
			}
		}
		if prevSep {
			return false
		}
	}
	return true
}
