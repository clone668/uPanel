package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// SecretPurposeAppEnv 是环境变量秘密的用途名。
//
// 传进 Resolver 是要它**再拒一次**：代理侧的秘密存储按用途登记（agentstore.purposes），
// `app_env` 之外的东西（TLS 私钥、备份口令、TOTP 种子）不能顺着这条路被塞进别人的容器里。
const SecretPurposeAppEnv = "app_env"

// ErrSecretUnavailable 表示秘密读不到（不存在、已销毁、代理不可用、用途不符）。
//
// 这里**没有**“读不到就当空串”的退路：一份带着空口令起来的容器，比一份没起来的容器
// 难查得多。
var ErrSecretUnavailable = errors.New("deploy: 环境变量引用的秘密读不到")

// Resolver 解析 secret_ref 指向的明文。
//
// 实现方负责解密与用途核对；本包只在内存里用一次，随后立刻丢弃（明文只出现在渲染出的
// 配置字节里，而那是 0600 落盘的东西）。
type Resolver interface {
	Resolve(ctx context.Context, ref string, purpose string) ([]byte, error)
}

// RenderEnv 是渲染时需要的宿主事实与秘密来源。
type RenderEnv struct {
	// Host 是宿主事实（地址、文件根、部署数据根、卷表）。
	Host HostEnv
	// Resolver 为 nil 时，任何 secret_ref 都会失败——宁可渲染不出来，也不渲染一份空口令。
	Resolver Resolver
}

// Rendered 是一次渲染的结果。
//
// **Config 里有明文秘密。** 调用方必须 root 独占 0600 落盘，且不得把它写进日志、审计、
// 任务参数或接口响应。接口对秘密只说“已设置”（CONTRACTS.md §8.2）。
type Rendered struct {
	// Config 是渲染出的 Compose 配置字节（确定性：同一份定义永远得到同一份字节）。
	Config []byte
	// ConfigDigest 是 Config 的 sha256，形如 sha256:<64 位小写十六进制>。
	//
	// 它是**内部**完整性值：拿它跟上次部署时的比，就知道配置有没有被动过。不要把它当
	// 公开标识发布——Config 里含秘密，一个公开的摘要等于给离线爆破提供了靶子。
	ConfigDigest string
	// ProjectName 是 Compose 项目名。渲染结果里不写顶层 name 字段（老版本 Compose 不认），
	// 执行时用 `-p <ProjectName>` 传进去，两者必须一致。
	ProjectName string
	// MountedSources 是 容器内路径 → 宿主路径。审计与“这份部署到底读了哪块盘”靠它。
	MountedSources map[string]string
	// SecretRefs 是这次真正读到的秘密引用（不含明文）。
	SecretRefs []string
	// ExpectedImageDigest 是定义里显式给的镜像 digest（没给就是空串）。
	// P15.3 用它比对执行后 docker 报回来的实际 digest。
	ExpectedImageDigest string
}

// Render 校验一份定义并渲染成 Compose 配置。
//
// 顺序是固定的：形状（Normalize）→ 宿主事实（CheckHost）→ 秘密 → 字节。任何一步失败
// 都不产出半份配置：没有“先写下去再说”的中间态。
//
// **健康检查不渲染成 Compose 的 healthcheck 块。** 容器内的 healthcheck 需要一条能在
// 镜像里跑起来的命令，面板不知道镜像里有什么；塞 `CMD-SHELL wget -q -O- ...` 会在没有
// wget 的镜像上永远 unhealthy，还会往容器里引入一条 shell 命令（又是一条注入面）。
// 定义里的 health_check 是给面板自己用的探测描述，由 P15.3 在启动后执行。
func Render(ctx context.Context, d Definition, env RenderEnv) (Rendered, error) {
	def, err := Normalize(d)
	if err != nil {
		return Rendered{}, err
	}
	if err := CheckHost(def, env.Host); err != nil {
		return Rendered{}, err
	}
	ref, err := parseImageReference(def.ImageReference)
	if err != nil {
		return Rendered{}, err
	}
	values, refs, err := resolveEnvironment(ctx, def, env.Resolver)
	if err != nil {
		return Rendered{}, err
	}

	sources := make(map[string]string, len(def.Mounts))
	for _, m := range def.Mounts {
		sources[m.ContainerPath] = path.Join(env.Host.Roots[m.SourceRootID].Path, m.RelativePath)
	}

	var buf bytes.Buffer
	writeConfig(&buf, def, ref, values, sources)

	return Rendered{
		Config:              buf.Bytes(),
		ConfigDigest:        RenderedDigest(buf.Bytes()),
		ProjectName:         def.Name,
		MountedSources:      sources,
		SecretRefs:          refs,
		ExpectedImageDigest: ref.Digest,
	}, nil
}

// RenderedDigest 是一份渲染配置的摘要（部署版本、记录里的 config_digest 都是它）。
//
// 单独导出一个函数而不是让调用方各自 sha256：摘要的形态（前缀、大小写、十六进制）
// 是逐字比对的东西，两处各写一遍就会在某个地方悄悄分叉。
func RenderedDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// resolveEnvironment 把公开值与秘密解析成 名字 → 值，并记下用到的引用。
func resolveEnvironment(ctx context.Context, def Definition, resolver Resolver) (map[string]string, []string, error) {
	values := make(map[string]string, len(def.Environment))
	refs := make([]string, 0, len(def.Environment))
	for _, e := range def.Environment {
		if e.Value != nil {
			values[e.Name] = *e.Value
			continue
		}
		if resolver == nil {
			return nil, nil, fmt.Errorf("%w：%s 引用了 %s，但这一轮没有可用的秘密读取器",
				ErrSecretUnavailable, e.Name, e.SecretRef)
		}
		raw, err := resolver.Resolve(ctx, e.SecretRef, SecretPurposeAppEnv)
		if err != nil {
			// 原始错误可能带出存储细节，这里只保留“哪个变量、哪个引用”。
			return nil, nil, fmt.Errorf("%w：%s 引用的 %s 读不出来（%v）",
				ErrSecretUnavailable, e.Name, e.SecretRef, err)
		}
		if len(raw) > MaxEnvValueBytes {
			return nil, nil, fmt.Errorf("%w：%s 引用的秘密超过 %d 字节",
				ErrSecretUnavailable, e.Name, MaxEnvValueBytes)
		}
		value := string(raw)
		if strings.IndexByte(value, 0) >= 0 {
			return nil, nil, fmt.Errorf("%w：%s 引用的秘密里有 NUL 字节，环境变量放不下",
				ErrSecretUnavailable, e.Name)
		}
		values[e.Name] = value
		refs = append(refs, e.SecretRef)
	}
	return values, refs, nil
}

// writeConfig 按固定顺序写出配置字节。
//
// 顺序固定是为了摘要稳定：同一份定义换个提交顺序，渲染结果必须一模一样。顺带的好处是
// 人读 diff 时不会看到一堆“只换了顺序”的噪音。
//
// 关于 **Compose 变量插值**：`docker compose` 会把配置里的 `$` 当插值起点（`$VAR`、
// `${VAR:-x}`），于是环境变量里的一个字面 `$` 会被 compose 悄悄改写，甚至从**宿主**
// 环境里取值拼进容器——一条配置注入。所有引号标量都经 yamlString 把 `$` 转义成 `$$`，
// compose 在解析后再还原成一个 `$`。
func writeConfig(buf *bytes.Buffer, def Definition, ref imageRef, env map[string]string, sources map[string]string) {
	buf.WriteString("# 由 uPanel 渲染，请勿手工修改：改这个文件不会回到面板定义，下一次部署会被覆盖。\n")
	buf.WriteString("# 项目 " + def.Name + "；服务 " + def.Name +
		"；容器端口 " + strconv.Itoa(def.ContainerPort) +
		"；宿主 " + def.HostBindAddress + ":" + strconv.Itoa(def.HostPort) + "/" + ProtocolTCP + "\n")
	buf.WriteString("services:\n")
	buf.WriteString("  " + yamlString(def.Name) + ":\n")
	buf.WriteString("    image: " + yamlString(ref.String()) + "\n")

	// restart 必须是字符串 "no"。写成裸的 no，YAML 1.1 会读成布尔 false，Compose 要么报错
	// 要么按默认策略处理——而“会自动重启”意味着开机或崩溃后容器绕过面板的依赖卷门禁自己
	// 起来（STAGES.md §17 的验收条目）。这个引号是功能，不是风格。
	buf.WriteString("    restart: " + yamlString("no") + "\n")

	buf.WriteString("    ports:\n")
	buf.WriteString("      - target: " + strconv.Itoa(def.ContainerPort) + "\n")
	buf.WriteString("        published: " + yamlString(strconv.Itoa(def.HostPort)) + "\n")
	buf.WriteString("        host_ip: " + yamlString(def.HostBindAddress) + "\n")
	buf.WriteString("        protocol: " + yamlString(ProtocolTCP) + "\n")

	if len(def.Environment) > 0 {
		buf.WriteString("    environment:\n")
		for _, e := range def.Environment {
			// def.Environment 已按名字排序（Normalize 做的），因此这里不依赖 map 迭代顺序。
			buf.WriteString("      " + yamlString(e.Name) + ": " + yamlString(env[e.Name]) + "\n")
		}
	}

	if len(def.Mounts) > 0 {
		buf.WriteString("    volumes:\n")
		for _, m := range def.Mounts {
			// 用长语法而不是 "宿主:容器:ro"：短语法要靠冒号分隔，路径里一个冒号就全乱，
			// 而长语法还能把 create_host_path 明确关掉（见下）。
			buf.WriteString("      - type: " + yamlString("bind") + "\n")
			buf.WriteString("        source: " + yamlString(sources[m.ContainerPath]) + "\n")
			buf.WriteString("        target: " + yamlString(m.ContainerPath) + "\n")
			buf.WriteString("        read_only: " + strconv.FormatBool(m.ReadOnly) + "\n")
			buf.WriteString("        bind:\n")
			// create_host_path: false 是 STAGES.md §17 与 APP-03 那条验收在渲染层的落点：
			// 卷没挂上时宿主路径不存在，Compose 默认会**替我们建一个空目录**然后照常启动，
			// 于是数据写进系统盘、而界面显示“运行中”。关掉它，这种情况会直接失败。
			buf.WriteString("          create_host_path: false\n")
		}
	}

	buf.WriteString("    mem_limit: " + strconv.FormatInt(def.ResourceLimits.MemoryLimitBytes, 10) + "\n")
	buf.WriteString("    cpus: " + def.ResourceLimits.CPULimit + "\n")
}

// yamlString 把一个字符串写成 YAML 双引号标量，并转义 Compose 的 `$` 插值。
//
// 双引号形态对每个字节都有确定写法，因此不需要在“要不要加引号”上做判断（判断错一次
// 就会把 `"no"` 读成 false、把 `2024-01-01` 读成日期、把 `0755` 读成八进制）。
// `\xNN` 与 `\n`、`\t` 都是 YAML 双引号里的合法转义；UTF-8 多字节字节序列原样透传。
func yamlString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '$':
			// Compose 插值转义：两个 $$ 在 compose 解析后还原成一个 $。
			b.WriteString("$$")
		default:
			if c < 0x20 || c == 0x7f {
				b.WriteString(`\x`)
				const hexDigits = "0123456789abcdef"
				b.WriteByte(hexDigits[c>>4])
				b.WriteByte(hexDigits[c&0x0f])
				continue
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
