package network

import (
	"encoding/hex"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxListeners 是返回条数上限。一台跑容器的机器上监听表可以有几千行，页面不该为了
// 把它们全列出来而卡住；超了就置 Truncated，让调用方知道"这不是全部"。
const maxListeners = 2000

// Listeners 采集监听中的套接字。
//
// 来源是 /proc/net/{tcp,tcp6,udp,udp6} 加 /proc/<pid>/fd 的 inode 反查，**不调用 ss**：
// ss 的输出是给人看的表格，列宽会随版本和 locale 变；/proc 的格式由内核 ABI 保证。
//
// 这里与 discovery 包里那份解析长得像，但不是同一件事：discovery 要回答"哪些监听属于
// 一个还没被面板管理的应用"（只关心 TCP、关心能不能连上）。这一份要回答"这台机器对
// 外开了什么"，所以带 UDP、带通配/回环标记、带进程与单元。两处合并会把两种语义揉成
// 一个参数化的函数，那比重复一百行更难读。
func Listeners(deps Deps) ListenerList {
	out := ListenerList{
		CollectedAt: deps.now().Format(time.RFC3339),
		Listeners:   []Listener{},
		Issues:      []Issue{},
	}
	fsys := deps.fs()
	procRoot := deps.procRoot()

	type protoFile struct {
		path     string
		protocol string
		family   string
	}
	files := []protoFile{
		{"net/tcp", "tcp", FamilyIPv4},
		{"net/tcp6", "tcp", FamilyIPv6},
		{"net/udp", "udp", FamilyIPv4},
		{"net/udp6", "udp", FamilyIPv6},
	}

	var entries []Listener
	inodes := map[uint64]struct{}{}
	for _, pf := range files {
		data, err := fsys.ReadFile(path.Join(procRoot, pf.path))
		if err != nil {
			// 单个协议栈读不到就记一条，继续读别的：读不到 tcp6 不代表没有 tcp。
			out.Issues = append(out.Issues, Issue{
				Source: "proc-net", Code: issueCode(err),
				Detail: "读 /proc/" + pf.path + " 失败：" + err.Error(),
			})
			continue
		}
		parsed, err := parseProcNet(data, pf.protocol, pf.family == FamilyIPv6)
		if err != nil {
			out.Issues = append(out.Issues, Issue{
				Source: "proc-net", Code: IssueUnparsable,
				Detail: "/proc/" + pf.path + " 里有解析不了的行：" + err.Error(),
			})
			continue
		}
		for _, e := range parsed {
			e.Family = pf.family
			entries = append(entries, e)
			inodes[e.inode] = struct{}{}
		}
	}

	owners := socketOwners(deps, procRoot, inodes)
	for i := range entries {
		if owner, ok := owners[entries[i].inode]; ok {
			entries[i].PID = owner.pid
			entries[i].Process = owner.name
			entries[i].Unit = owner.unit
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		return a.Address < b.Address
	})

	if len(entries) > maxListeners {
		entries = entries[:maxListeners]
		out.Truncated = true
	}
	for i := range entries {
		entries[i].inode = 0 // 内部字段不出现在响应里
	}
	out.Listeners = entries
	return out
}

// parseProcNet 解析 /proc/net/tcp、udp 及其 v6 版本。
//
// 表格前有一行表头，每行用空白分隔；取 local_address（第 2 列）、状态（第 4 列）与
// inode（第 10 列）。TCP 取状态 0A（LISTEN），UDP 取 07（CLOSE）——UDP 没有真正的
// 监听状态，内核把已绑定未连通的套接字记成 07。
func parseProcNet(data []byte, protocol string, v6 bool) ([]Listener, error) {
	var out []Listener
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			return out, errLine("字段不足", line)
		}
		state := fields[3]
		if protocol == "tcp" && state != "0A" {
			continue
		}
		if protocol == "udp" && state != "07" {
			continue
		}
		addrHex, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			return out, errLine("本地地址格式不对", line)
		}
		addr, err := parseHexAddr(addrHex, v6)
		if err != nil {
			return out, errLine("地址解析失败", line)
		}
		port64, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil {
			return out, errLine("端口解析失败", line)
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			return out, errLine("inode 解析失败", line)
		}
		wildcard := addr == "0.0.0.0" || addr == "::"
		out = append(out, Listener{
			Protocol: protocol,
			Address:  addr,
			Port:     int(port64),
			Wildcard: wildcard,
			Loopback: !wildcard && isLoopbackAddr(addr),
			inode:    inode,
		})
	}
	return out, nil
}

// parseHexAddr 把 /proc/net 里的十六进制地址还原成可读形式。
//
// IPv4 是主机字节序的一个 32 位整数（小端），所以要按字节倒过来；IPv6 是四组 32 位，
// 每组内部同样是小端，组与组之间不倒——这是内核的 netlink/proc 表示法，容易写反的地方。
func parseHexAddr(field string, v6 bool) (string, error) {
	raw, err := hex.DecodeString(field)
	if err != nil {
		return "", err
	}
	if !v6 {
		if len(raw) != 4 {
			return "", errHexLen
		}
		return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String(), nil
	}
	if len(raw) != 16 {
		return "", errHexLen
	}
	out := make([]byte, 16)
	for word := 0; word < 4; word++ {
		for b := 0; b < 4; b++ {
			out[word*4+b] = raw[word*4+(3-b)]
		}
	}
	return net.IP(out).String(), nil
}

func isLoopbackAddr(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

// ---- 进程归属 ----

type socketOwner struct {
	pid  int64
	name string
	unit string
}

// socketOwners 把 inode 反查成进程。读不动的进程直接跳过：一个进程在我们扫描的瞬间
// 退出是常态，那不是错误。
func socketOwners(deps Deps, procRoot string, inodes map[uint64]struct{}) map[uint64]socketOwner {
	out := map[uint64]socketOwner{}
	if len(inodes) == 0 {
		return out
	}
	fsys := deps.fs()
	pids, err := fsys.ListDir(procRoot)
	if err != nil {
		return out
	}
	for _, dir := range pids {
		pid, convErr := strconv.ParseInt(dir, 10, 64)
		if convErr != nil {
			continue
		}
		fdDir := path.Join(procRoot, dir, "fd")
		fds, err := fsys.ListDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := fsys.Readlink(path.Join(fdDir, fd))
			if err != nil {
				continue
			}
			raw, ok := strings.CutPrefix(target, "socket:[")
			if !ok {
				continue
			}
			raw = strings.TrimSuffix(raw, "]")
			inode, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				continue
			}
			if _, want := inodes[inode]; !want {
				continue
			}
			if _, seen := out[inode]; seen {
				continue
			}
			out[inode] = socketOwner{
				pid:  pid,
				name: readComm(fsys, path.Join(procRoot, dir, "comm")),
				unit: cgroupUnit(fsys, path.Join(procRoot, dir, "cgroup")),
			}
		}
	}
	return out
}

func readComm(fsys interface{ ReadFile(string) ([]byte, error) }, name string) string {
	data, err := fsys.ReadFile(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// cgroupUnit 从 cgroup 行里取出 systemd 单元名（`.../system.slice/sshd.service`）。
func cgroupUnit(fsys interface{ ReadFile(string) ([]byte, error) }, name string) string {
	data, err := fsys.ReadFile(name)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		_, unitPath, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		_, unitPath, ok = strings.Cut(unitPath, ":")
		if !ok {
			continue
		}
		idx := strings.LastIndex(unitPath, "/")
		if idx < 0 {
			continue
		}
		candidate := strings.TrimSpace(unitPath[idx+1:])
		if strings.HasSuffix(candidate, ".service") || strings.HasSuffix(candidate, ".scope") {
			return candidate
		}
	}
	return ""
}
