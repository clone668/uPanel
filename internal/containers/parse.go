package containers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// psRow 是 `docker ps` 的一行。
type psRow struct {
	ID string
	// Name 是容器名（去掉前导斜杠；同一容器多个别名时取第一个）。
	Name string
	// Image 是镜像引用（原样）。
	Image string
	// State 是 docker 的原始状态词（running/exited/…）。
	State string
	// StateText 是人看的描述（Up 2 hours (healthy)）。
	StateText string
	CreatedAt string
	Ports     []Port
	Compose   *ComposeRef
}

// ErrDockerOutput 表示 docker 的输出不是预期的行格式。
var ErrDockerOutput = fmt.Errorf("containers: docker 输出格式不认识")

// parsePS 解析容器列表的固定格式。
//
// 字段顺序与 psFormat 逐字对应；行内的分隔符不会出现在数据里（见 fs 的说明）。
func parsePS(out string) ([]psRow, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	rows := make([]psRow, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, fs)
		if len(fields) != 8 {
			return nil, fmt.Errorf("%w：期望 8 列，实际 %d 列：%q", ErrDockerOutput, len(fields), firstLine(line))
		}
		name := strings.TrimPrefix(strings.TrimSpace(fields[1]), "/")
		if idx := strings.Index(name, ","); idx >= 0 {
			name = strings.TrimSpace(name[:idx])
		}
		rows = append(rows, psRow{
			ID:        strings.TrimSpace(fields[0]),
			Name:      name,
			Image:     strings.TrimSpace(fields[2]),
			State:     strings.ToLower(strings.TrimSpace(fields[3])),
			StateText: strings.TrimSpace(fields[4]),
			CreatedAt: strings.TrimSpace(fields[5]),
			Ports:     parsePortField(fields[6]),
			Compose:   parseComposeLabels(fields[7]),
		})
	}
	return rows, nil
}

// parsePortField 解析端口列：
//
//	0.0.0.0:8080->80/tcp, :::8080->80/tcp
//	80/tcp                       （只 expose 没 publish，不算映射）
func parsePortField(field string) []Port {
	out := make([]Port, 0, 2)
	for _, piece := range strings.Split(field, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		arrow := strings.Index(piece, "->")
		if arrow < 0 {
			continue
		}
		left := piece[:arrow]
		right := piece[arrow+2:]
		proto := "tcp"
		if idx := strings.Index(right, "/"); idx >= 0 {
			proto = right[idx+1:]
			right = right[:idx]
		}
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
		out = append(out, Port{
			HostIP:        hostIP,
			HostPort:      hostPort,
			ContainerPort: containerPort,
			Protocol:      strings.ToLower(proto),
		})
	}
	return out
}

// composeLabelKeys 是唯一会从容器标签里被取走的六个键。
//
// 其余标签一律丢弃：第三方标签里可能写着数据库口令或访问令牌（traefik basic auth、
// watchtower 的 webhook 之类），而它们对“这台机器上有哪些项目”没有帮助。
var composeLabelKeys = map[string]bool{
	"com.docker.compose.project":              true,
	"com.docker.compose.service":              true,
	"com.docker.compose.project.working_dir":  true,
	"com.docker.compose.project.config_files": true,
	"com.docker.compose.oneoff":               true,
	"com.docker.compose.container-number":     true,
}

// parseComposeLabels 从 ps 的标签列里只取 compose 的六个键。
func parseComposeLabels(field string) *ComposeRef {
	field = strings.TrimSpace(field)
	if field == "" || field == "<no value>" {
		return nil
	}
	values := map[string]string{}
	for _, pair := range strings.Split(field, ",") {
		eq := strings.Index(pair, "=")
		if eq <= 0 {
			continue
		}
		key := pair[:eq]
		if !composeLabelKeys[key] {
			continue
		}
		values[key] = pair[eq+1:]
	}
	if len(values) == 0 {
		return nil
	}
	ref := &ComposeRef{
		Project:     values["com.docker.compose.project"],
		Service:     values["com.docker.compose.service"],
		WorkingDir:  values["com.docker.compose.project.working_dir"],
		ConfigFiles: values["com.docker.compose.project.config_files"],
		OneOff:      strings.EqualFold(values["com.docker.compose.oneoff"], "True"),
	}
	if n, err := strconv.Atoi(values["com.docker.compose.container-number"]); err == nil {
		ref.Number = n
	}
	if ref.Project == "" {
		return nil
	}
	return ref
}

// ProjectID 把 compose 项目名映射成面板里的稳定标识。
//
// 用名字的 SHA-256 前 32 位十六进制，而不是自增行号：项目名来自标签，
// 面板没有它的表，重启、重装之后同一个项目必须还是同一个 id。
func ProjectID(name string) string {
	sum := sha256.Sum256([]byte("compose:" + name))
	return "dep_" + hex.EncodeToString(sum[:16])
}

// parseInspect 解析批量 inspect 的输出。
func parseInspect(out string) (map[string]inspectFacts, error) {
	result := map[string]inspectFacts{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, fs)
		if len(fields) != 21 {
			return nil, fmt.Errorf("%w：inspect 期望 21 列，实际 %d 列", ErrDockerOutput, len(fields))
		}
		f := inspectFacts{
			ID:            strings.TrimSpace(fields[0]),
			Created:       strings.TrimSpace(fields[1]),
			State:         strings.ToLower(strings.TrimSpace(fields[2])),
			StartedAt:     strings.TrimSpace(fields[3]),
			RestartCount:  int(ParseInt(fields[4])),
			RestartPolicy: strings.TrimSpace(fields[5]),
			Privileged:    strings.TrimSpace(fields[6]) == "true",
			NetworkMode:   strings.TrimSpace(fields[7]),
			PidMode:       strings.TrimSpace(fields[8]),
			IpcMode:       strings.TrimSpace(fields[9]),
			NanoCPUs:      ParseInt(fields[10]),
			Memory:        ParseInt(fields[11]),
			ImageID:       strings.TrimSpace(fields[14]),
		}
		if f.ID == "" {
			continue
		}
		if err := json.Unmarshal([]byte(fields[12]), &f.Mounts); err != nil {
			return nil, fmt.Errorf("%w：inspect 的挂载列表不是 JSON", ErrDockerOutput)
		}
		if err := json.Unmarshal([]byte(fields[13]), &f.CapAdd); err != nil {
			return nil, fmt.Errorf("%w：inspect 的 capability 列表不是 JSON", ErrDockerOutput)
		}
		project := labelValue(fields[15])
		if project != "" {
			ref := &ComposeRef{
				Project:     project,
				Service:     labelValue(fields[16]),
				WorkingDir:  labelValue(fields[17]),
				ConfigFiles: labelValue(fields[18]),
				OneOff:      strings.EqualFold(labelValue(fields[19]), "True"),
			}
			if n, err := strconv.Atoi(labelValue(fields[20])); err == nil {
				ref.Number = n
			}
			f.Compose = ref
		}
		result[f.ID] = f
	}
	return result, nil
}

// labelValue 处理模板里缺失标签时 docker 打出的占位符。
func labelValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "<no value>" {
		return ""
	}
	return s
}

// parseImages 解析镜像列表。
func parseImages(out string) ([]Image, error) {
	rows := make([]Image, 0, 16)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, fs)
		if len(fields) != 6 {
			return nil, fmt.Errorf("%w：镜像列表期望 6 列，实际 %d 列", ErrDockerOutput, len(fields))
		}
		rows = append(rows, Image{
			ID:         strings.TrimSpace(fields[0]),
			Repository: strings.TrimSpace(fields[1]),
			Tag:        strings.TrimSpace(fields[2]),
			Digest:     strings.TrimSpace(fields[3]),
			Size:       strings.TrimSpace(fields[4]),
			CreatedAt:  strings.TrimSpace(fields[5]),
		})
	}
	return rows, nil
}

// parseNetworkList 解析网络列表（子网由 network inspect 补齐）。
func parseNetworkList(out string) ([]Network, error) {
	rows := make([]Network, 0, 8)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, fs)
		if len(fields) != 4 {
			return nil, fmt.Errorf("%w：网络列表期望 4 列，实际 %d 列", ErrDockerOutput, len(fields))
		}
		rows = append(rows, Network{
			ID:      strings.TrimSpace(fields[0]),
			Name:    strings.TrimSpace(fields[1]),
			Driver:  strings.TrimSpace(fields[2]),
			Scope:   strings.TrimSpace(fields[3]),
			Subnets: []string{},
		})
	}
	return rows, nil
}

// parseVolumes 解析数据卷列表。
func parseVolumes(out string) ([]Volume, error) {
	rows := make([]Volume, 0, 8)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, fs)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w：数据卷列表期望 3 列，实际 %d 列", ErrDockerOutput, len(fields))
		}
		rows = append(rows, Volume{
			Name:    strings.TrimSpace(fields[0]),
			Driver:  strings.TrimSpace(fields[1]),
			Scope:   strings.TrimSpace(fields[2]),
			InUseBy: []string{},
		})
	}
	return rows, nil
}

// firstLine 取第一行（错误消息只带一行，调用方不需要整份输出）。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
