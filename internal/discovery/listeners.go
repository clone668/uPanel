package discovery

import (
	"encoding/hex"
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"

	"upanel/internal/processes"
)

// socket 是一个监听中的 TCP 套接字（来自 /proc/net/tcp{,6}）。
type socket struct {
	address string
	port    int
	inode   uint64
	// loopback 表示能从 127.0.0.1 连到它：回环地址本身，或通配地址（0.0.0.0 / ::）。
	//
	// :: 只有在双栈监听时才能接受 IPv4 连接。这里按“能”处理，因为 Linux 默认
	// bindv6only=0；万一是单栈，探测会如实报连接失败，而不是被这里悄悄跳过。
	loopback bool
}

// listenTable 是 TCP 监听表的解析结果。
type listenTable struct {
	sockets []socket
	// errors 是按文件记录的读取失败（读不到的协议栈不算“没有监听”）。
	errors []string
}

// netFiles 是监听表所在的文件，按顺序读取。
//
// 只读 TCP：这个功能的目标是 HTTP 服务，UDP 套接字既不能被 HTTP 探测，也不该占满
// 候选列表。要列 UDP 是另一个需求，不是这里的省略。
var netFiles = []string{"net/tcp", "net/tcp6"}

// ListenerProtocol 是监听项里唯一的协议值（见上）。
const ListenerProtocol = "tcp"

// readListenTable 读取并解析 TCP 监听表。
func readListenTable(fs processes.FileSystem, procRoot string) listenTable {
	out := listenTable{}
	for _, name := range netFiles {
		data, err := fs.ReadFile(path.Join(procRoot, name))
		if err != nil {
			out.errors = append(out.errors, fmt.Sprintf("读 %s 失败：%v", name, err))
			continue
		}
		socks, err := parseNetFile(data, strings.HasSuffix(name, "6"))
		if err != nil {
			out.errors = append(out.errors, fmt.Sprintf("解析 %s 失败：%v", name, err))
			continue
		}
		out.sockets = append(out.sockets, socks...)
	}
	return out
}

// parseNetFile 解析 /proc/net/tcp 或 /proc/net/tcp6 的正文。
//
// 行格式（内核 Documentation/networking/proc_net_tcp.rst）：
//
//	sl  local_address rem_address  st  tx_queue:rx_queue  tr:tm->when  retrnsmt  uid  timeout  inode
//
// 只保留 st=0A（LISTEN）的行。地址是十六进制，每个 32 位字按主机字节序（小端）
// 排列：IPv4 是 1 个字，IPv6 是 4 个字。
func parseNetFile(data []byte, v6 bool) ([]socket, error) {
	lines := strings.Split(string(data), "\n")
	out := make([]socket, 0, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 && strings.HasPrefix(line, "sl") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			// 字段不够就不是我们认识的格式：跳过这一行，不影响其它行。
			continue
		}
		if fields[3] != "0A" {
			continue
		}
		ip, port, err := parseHexAddr(fields[1], v6)
		if err != nil {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			// inode 为 0 的监听（极少数内核/容器命名空间组合）无法反查持有进程，
			// 但监听本身是真的：仍然保留，只是后面“持有进程”为空。
			inode = 0
		}
		out = append(out, socket{
			address:  ip,
			port:     port,
			inode:    inode,
			loopback: isLoopback(ip),
		})
	}
	return out, nil
}

// parseHexAddr 解析 "0100007F:1F90" 形式的地址。
func parseHexAddr(field string, v6 bool) (string, int, error) {
	parts := strings.SplitN(field, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("地址字段 %q 缺少端口", field)
	}
	port, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return "", 0, fmt.Errorf("端口 %q 不是十六进制", parts[1])
	}
	raw, err := hex.DecodeString(parts[0])
	if err != nil {
		return "", 0, fmt.Errorf("地址 %q 不是十六进制", parts[0])
	}
	want := 4
	if v6 {
		want = 16
	}
	if len(raw) != want {
		return "", 0, fmt.Errorf("地址 %q 长度是 %d 字节，期望 %d", parts[0], len(raw), want)
	}
	// 每个 32 位字内部是主机字节序：按字翻转。
	for off := 0; off < len(raw); off += 4 {
		raw[off], raw[off+1], raw[off+2], raw[off+3] = raw[off+3], raw[off+2], raw[off+1], raw[off]
	}
	ip := net.IP(raw)
	if v6 {
		// 内核把 IPv4 映射地址写成 ::ffff:1.2.3.4；统一成可读形状。
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), int(port), nil
		}
	}
	return ip.String(), int(port), nil
}

// isLoopback 判断监听地址是否包含回环（回环地址本身或通配地址）。
func isLoopback(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsUnspecified()
}

// socketOwner 是一个套接字的持有进程。
type socketOwner struct {
	pid  int64
	comm string
	unit string
}

// socketOwners 反查 inode → 持有它的进程。
//
// 做法是扫 /proc/<pid>/fd 里的 socket:[inode] 链接。这一步需要 root（代理就是 root），
// 否则只能看到自己进程的 fd；读不到的进程直接跳过——那意味着“持有者未知”，
// 不是“它不属于任何进程”。
func socketOwners(fs processes.FileSystem, procRoot string, inodes map[uint64]struct{}) map[uint64][]socketOwner {
	out := make(map[uint64][]socketOwner, len(inodes))
	if len(inodes) == 0 {
		return out
	}
	names, err := fs.ListDir(procRoot)
	if err != nil {
		return out
	}
	scanned := 0
	for _, name := range names {
		pid, err := strconv.ParseInt(name, 10, 64)
		if err != nil || pid <= 0 {
			continue
		}
		if scanned >= maxSocketScan {
			break
		}
		scanned++
		fdDir := path.Join(procRoot, name, "fd")
		fds, err := fs.ListDir(fdDir)
		if err != nil {
			continue
		}
		var mine []uint64
		for _, fd := range fds {
			target, err := fs.Readlink(path.Join(fdDir, fd))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(target, "socket:[") {
				continue
			}
			num := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			inode, err := strconv.ParseUint(num, 10, 64)
			if err != nil {
				continue
			}
			if _, ok := inodes[inode]; !ok {
				continue
			}
			mine = append(mine, inode)
		}
		if len(mine) == 0 {
			continue
		}
		owner := ownerOf(fs, procRoot, pid)
		for _, inode := range mine {
			out[inode] = append(out[inode], owner)
		}
	}
	return out
}

// ownerOf 读取一个进程的命令名与所属单元。
//
// comm 用 /proc/<pid>/comm 的第一行（内核限制 15 字符）；单元用 cgroup v2 路径里最后
// 一个 .service 片段。两者都读不到时留空——空的意思是“不知道”，不是“没有”。
func ownerOf(fs processes.FileSystem, procRoot string, pid int64) socketOwner {
	owner := socketOwner{pid: pid}
	if data, err := fs.ReadFile(path.Join(procRoot, strconv.FormatInt(pid, 10), "comm")); err == nil {
		owner.comm = strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	}
	if owner.comm == "" {
		// comm 读不到时退到 cmdline 的 argv[0] 基名：有些加固内核/受限命名空间
		// 只给 cmdline。仍然读不到就空着。
		if data, err := fs.ReadFile(path.Join(procRoot, strconv.FormatInt(pid, 10), "cmdline")); err == nil {
			fields := strings.SplitN(string(data), "\x00", 2)
			if len(fields) > 0 && fields[0] != "" {
				owner.comm = path.Base(fields[0])
			}
		}
	}
	if data, err := fs.ReadFile(path.Join(procRoot, strconv.FormatInt(pid, 10), "cgroup")); err == nil {
		owner.unit = cgroupService(string(data))
	}
	return owner
}

// cgroupService 从 /proc/<pid>/cgroup 里取 systemd 单元名（只要 .service）。
//
// 只认 cgroup v2 的 `0::<path>` 行：v1 的每一行属于不同的控制器，没有统一的单元语义。
func cgroupService(data string) string {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			continue
		}
		segments := strings.Split(strings.Trim(parts[2], "/"), "/")
		for i := len(segments) - 1; i >= 0; i-- {
			segment := segments[i]
			if !strings.HasSuffix(segment, ".service") {
				continue
			}
			if strings.ContainsAny(segment, " \t\x00\n") {
				return ""
			}
			return segment
		}
		return ""
	}
	return ""
}

// listenerGroup 把同一持有者的多个监听合在一起。
type listenerGroup struct {
	key       string
	pid       int64
	comm      string
	unit      string
	listeners []Listener
}

// groupListeners 把监听套接字按“持有者”分组：有单元的按单元，没有单元的按进程。
//
// 一个单元可能监听多个端口（主进程 + 工作进程），一个也没有的裸监听按进程分组。
// inode 反查不到持有者的，按端口各自成组——宁可多列一条，也不要把它藏起来。
func groupListeners(table listenTable, owners map[uint64][]socketOwner) []listenerGroup {
	groups := make(map[string]*listenerGroup)
	add := func(key string, owner socketOwner, l Listener) {
		g, ok := groups[key]
		if !ok {
			g = &listenerGroup{key: key, pid: owner.pid, comm: owner.comm, unit: owner.unit}
			groups[key] = g
		}
		for _, existing := range g.listeners {
			if existing.Port == l.Port && existing.Address == l.Address {
				return
			}
		}
		if len(g.listeners) >= MaxListenersPerCandidate {
			return
		}
		g.listeners = append(g.listeners, l)
	}

	for _, sock := range table.sockets {
		l := Listener{Address: sock.address, Port: sock.port, Protocol: ListenerProtocol, Loopback: sock.loopback}
		holders := owners[sock.inode]
		if len(holders) == 0 {
			add(fmt.Sprintf("port:%d/%s", sock.port, sock.address), socketOwner{}, l)
			continue
		}
		for _, owner := range holders {
			key := fmt.Sprintf("pid:%d", owner.pid)
			if owner.unit != "" && !userManagerUnit(owner.unit) {
				key = "unit:" + owner.unit
			}
			add(key, owner, l)
		}
	}

	out := make([]listenerGroup, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.listeners, func(i, j int) bool {
			if g.listeners[i].Port != g.listeners[j].Port {
				return g.listeners[i].Port < g.listeners[j].Port
			}
			return g.listeners[i].Address < g.listeners[j].Address
		})
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// userManagerUnit 判断是不是每个登录用户的 systemd 管理器（user@1000.service）。
//
// 这类单元不是“一个服务”，它下面挂着某个用户的所有进程。把整片进程归到它名下会让
// 候选列表出现一条 “user@1000.service 监听 12 个端口”的假服务。它们的进程按裸监听
// 处理（进程名 + 端口），说明里也不需要解释。
func userManagerUnit(unit string) bool { return strings.HasPrefix(unit, "user@") }
