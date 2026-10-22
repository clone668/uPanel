package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// Docker 来源的失败原因分类。它们对管理员意味着完全不同的下一步，所以分开说。
const (
	// dockerUnavailableNoBinary 表示本机没有 docker 命令。
	dockerUnavailableNoBinary = "本机没有 docker 命令，容器来源不可用"
	// dockerUnavailableNoSocket 表示命令在、守护进程没在跑。
	dockerUnavailableNoSocket = "Docker 守护进程没有运行，容器来源不可用"
)

// dockerCandidates 是 docker 命令的常规安装位置。
//
// 与 storage/shares 同一套做法：固定候选路径 + 可注入覆盖，绝不从 PATH 里找，
// 也绝不把调用方给的字符串当程序名（PLAN.md 第 4 节）。
var dockerCandidates = []string{"/usr/bin/docker", "/snap/bin/docker"}

// dockerSocket 是守护进程的 Unix 套接字。它不存在就没有容器可谈。
var dockerSocket = "/var/run/docker.sock"

// Container 是一个容器（docker ps 的一行）。
type Container struct {
	// ID 是完整容器 ID（--no-trunc）。容器的身份是 ID，不是名字：
	// 名字会随重建变化，ID 不会（P09.1 的 docker 绑定就是这个形状）。
	ID string
	// Name 是容器名（去掉前导 / 与可能存在的手工前缀）。
	Name string
	// Image 是镜像引用（原样）。
	Image string
	// State 是 docker 的原始状态（running / exited / created / paused / restarting / dead）。
	State string
	// Ports 是已发布的端口映射（只有带 -> 的才算）。
	Ports []PortMapping
}

// PortMapping 是一条发布映射：宿主 hostIP:hostPort → 容器 containerPort/proto。
type PortMapping struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
}

// DockerSource 是容器来源的能力接口。*DockerClient 是生产实现；测试用假对象。
type DockerSource interface {
	// Containers 返回本机容器。返回 error 表示这次读不出来（原因要能直接给人看）。
	// 返回 Available=false 表示这台机器没有容器来源（没装命令/没跑守护进程）。
	Containers(ctx context.Context) (containers []Container, available bool, notice string, err error)
}

// DockerClient 通过固定的 docker 命令行读取容器清单。
//
// 只用 `docker ps`：P09.2 的发现不需要 inspect，也不做任何写操作。命令与参数都是
// 常量，容器名只出现在输出里，不会被拼回命令。
type DockerClient struct {
	// Program 是 docker 的绝对路径；为空时按 dockerCandidates 探测。
	Program string
	// Socket 是守护进程套接字路径（测试可替换）。
	Socket string
	// Timeout 是单次调用的时限。
	Timeout time.Duration
	// Stat 用来判断套接字是否存在（测试可替换）。
	Stat func(string) (os.FileInfo, error)

	// resolved 记录上一次探测到的路径，避免每次都重新统计。
	resolved string
	// resolvedOK 表示 resolved 里的路径已确认存在。
	resolvedOK bool
}

// Containers 实现 DockerSource。
func (c *DockerClient) Containers(ctx context.Context) ([]Container, bool, string, error) {
	program := c.program()
	if program == "" {
		return nil, false, dockerUnavailableNoBinary, nil
	}
	socket := c.Socket
	if socket == "" {
		socket = dockerSocket
	}
	stat := c.Stat
	if stat == nil {
		stat = os.Stat
	}
	if _, err := stat(socket); err != nil {
		return nil, false, dockerUnavailableNoSocket, nil
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	res, err := syscmd.Run(ctx, syscmd.Spec{
		Program: program,
		Args: []string{
			"ps", "-a", "--no-trunc",
			"--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.State}}|{{.Ports}}",
		},
	}, syscmd.Limits{Timeout: timeout, MaxStdout: 1 << 20, MaxStderr: 64 << 10})
	if err != nil {
		return nil, true, "", fmt.Errorf("docker ps 失败：%s", firstLine(firstNonEmpty(res.Stderr, err.Error())))
	}
	containers, perr := parseDockerPS(res.Stdout)
	if perr != nil {
		return nil, true, "", perr
	}
	return containers, true, "", nil
}

// program 返回 docker 的绝对路径（探测一次后记住）。
func (c *DockerClient) program() string {
	if c.Program != "" {
		return c.Program
	}
	if c.resolvedOK {
		return c.resolved
	}
	for _, candidate := range dockerCandidates {
		if _, err := os.Stat(candidate); err == nil {
			c.resolved = candidate
			c.resolvedOK = true
			return candidate
		}
	}
	return ""
}

// ErrDockerOutput 表示 docker ps 的输出不是预期的行格式。
var ErrDockerOutput = errors.New("discovery: docker ps 输出格式不认识")

// parseDockerPS 解析 `{{.ID}}|{{.Names}}|{{.Image}}|{{.State}}|{{.Ports}}` 的输出。
//
// 一行一个容器。名字理论上可能含 | （docker 允许的字符集里没有 |，但仍然按
// “第一列是 ID，最后一列是端口”来切，中间列用 SplitN 保守处理）。
func parseDockerPS(out string) ([]Container, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	containers := make([]Container, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 5)
		if len(fields) < 5 {
			return nil, fmt.Errorf("%w：%q", ErrDockerOutput, firstLine(line))
		}
		c := Container{
			ID:    strings.TrimSpace(fields[0]),
			Name:  strings.TrimPrefix(strings.TrimSpace(fields[1]), "/"),
			Image: strings.TrimSpace(fields[2]),
			State: strings.TrimSpace(fields[3]),
		}
		// 多名字用逗号分隔（同一容器可以有多个别名）；取第一个作为显示名。
		if idx := strings.Index(c.Name, ","); idx >= 0 {
			c.Name = strings.TrimSpace(c.Name[:idx])
		}
		c.Ports = parseDockerPorts(fields[4])
		containers = append(containers, c)
	}
	return containers, nil
}

// parseDockerPorts 解析 docker 的端口列，例如：
//
//	0.0.0.0:8080->80/tcp, :::8080->80/tcp
//	127.0.0.1:9000->9000/tcp
//	80/tcp                       （只 expose 没 publish，不算映射）
func parseDockerPorts(field string) []PortMapping {
	out := make([]PortMapping, 0, 2)
	for _, piece := range strings.Split(field, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		arrow := strings.Index(piece, "->")
		if arrow < 0 {
			// 没有 -> 就是没发布到宿主机，从回环上连不到它。
			continue
		}
		left := piece[:arrow]
		right := piece[arrow+2:]
		proto := "tcp"
		if idx := strings.Index(right, "/"); idx >= 0 {
			proto = right[idx+1:]
			right = right[:idx]
		}
		// left 可能是 "127.0.0.1:8080"、"[::]:8080" 或 ":::8080"。
		hostIP := ""
		hostPortStr := left
		if idx := strings.LastIndex(left, ":"); idx >= 0 {
			hostIP = strings.Trim(left[:idx], "[]")
			hostPortStr = left[idx+1:]
		}
		hostPort, err := strconv.Atoi(strings.TrimSpace(hostPortStr))
		if err != nil || hostPort <= 0 || hostPort > 65535 {
			continue
		}
		containerPort, err := strconv.Atoi(strings.TrimSpace(right))
		if err != nil || containerPort <= 0 || containerPort > 65535 {
			continue
		}
		out = append(out, PortMapping{
			HostIP:        hostIP,
			HostPort:      hostPort,
			ContainerPort: containerPort,
			Protocol:      strings.ToLower(proto),
		})
	}
	return out
}

// containerState 把 docker 的 State 映射到面板的观测枚举。
//
// paused/restarting 都算 running 吗？不。restarting 是“在重启循环里”，那正是最需要
// 人看一眼的状态，映射成 running 会把一个坏掉的容器显示成正常。它是 unknown：
// 既不是稳定运行，也不是稳定停止。
func containerState(state string) string {
	switch strings.ToLower(state) {
	case "running":
		return ObservedRunning
	case "exited", "dead", "created", "removing":
		return ObservedStopped
	case "paused", "restarting":
		return ObservedUnknown
	}
	return ObservedUnknown
}

// loopbackReachable 判断一条发布映射能不能从 127.0.0.1 连到。
//
// 通配（0.0.0.0 / :: / 空）与 127.0.0.1 都可以；只在某个具体外网地址上发布的
// （例如 10.0.0.5:8080），回环上没有它。面板不从非回环地址探测。
func loopbackReachable(hostIP string) bool {
	if hostIP == "" {
		return true
	}
	return isLoopback(hostIP) || hostIP == "0.0.0.0" || hostIP == "::"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.SplitN(s, "\n", 2)[0]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
