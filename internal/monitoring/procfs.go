package monitoring

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// cpuTimes 是 /proc/stat 里一行 cpu 的时间分量（单位：USER_HZ 的 jiffies）。
//
// guest/guest_nice 不单独存：内核已经把 guest 时间计在 user 里，再相加会重复。
type cpuTimes struct {
	user    uint64
	nice    uint64
	system  uint64
	idle    uint64
	iowait  uint64
	irq     uint64
	softirq uint64
	steal   uint64
}

// total 是这一行所有分量之和（wall time 的份额）。
func (t cpuTimes) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

// idleTotal 是空闲分量：idle 加上 iowait。iowait 期间 CPU 并没有干活，
// 把它算作忙会让一块等着磁盘的机器看起来 100% 满载。
func (t cpuTimes) idleTotal() uint64 { return t.idle + t.iowait }

// decreased 判断计数器是否变小（系统重启或计数器重置）。
func (t cpuTimes) decreased(prev cpuTimes) bool {
	return t.user < prev.user || t.nice < prev.nice || t.system < prev.system ||
		t.idle < prev.idle || t.iowait < prev.iowait || t.irq < prev.irq ||
		t.softirq < prev.softirq || t.steal < prev.steal
}

// ParseStat 解析 /proc/stat 里的 cpu 行。
//
// 返回合计行（"cpu"）与每个逻辑核（"cpuN"）。合计行必须存在；
// 其余行（intr、ctxt、btime……）在第一个非 cpu 行之后停止收集，不参与解析。
func ParseStat(data []byte) (cpuTimes, []cpuTimes, error) {
	var agg cpuTimes
	var per []cpuTimes
	sawAgg := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		name := fields[0]
		if name != "cpu" && !strings.HasPrefix(name, "cpu") {
			break
		}
		// "cpu" 必须整字段匹配：strings.HasPrefix 会把 "cpuu" 也算进来，
		// 那是畸形输入，不该被当成合计行。
		isAgg := name == "cpu"
		if !isAgg && !isCPUIndex(name) {
			break
		}
		// 名字之外还要 8 个值：len(fields) < 9。判 < 8 会在“恰好 7 个值”的畸形行上
		// 让下面 fields[i+1] 越界——一个解析器不该靠 panic 拒绝坏输入。
		if len(fields) < 9 {
			return cpuTimes{}, nil, fmt.Errorf("/proc/stat 的 %s 行只有 %d 个值（至少 8 个）", name, len(fields)-1)
		}
		var vals [8]uint64
		for i := 0; i < 8; i++ {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return cpuTimes{}, nil, fmt.Errorf("/proc/stat 的 %s 行第 %d 个值 %q 不是无符号整数", name, i+1, fields[i+1])
			}
			vals[i] = v
		}
		t := cpuTimes{
			user: vals[0], nice: vals[1], system: vals[2], idle: vals[3],
			iowait: vals[4], irq: vals[5], softirq: vals[6], steal: vals[7],
		}
		if isAgg {
			agg = t
			sawAgg = true
			continue
		}
		per = append(per, t)
	}
	if !sawAgg {
		return cpuTimes{}, nil, errors.New("/proc/stat 里没有合计的 cpu 行")
	}
	return agg, per, nil
}

// isCPUIndex 判断 "cpu12" 这种按核的名字。
func isCPUIndex(name string) bool {
	rest := strings.TrimPrefix(name, "cpu")
	if rest == "" || rest == name {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// usagePercent 用两次样本算使用率。时间窗没有推进（delta 为 0）时返回 nil：
// 那是“还没有可用样本”，不是 0% 使用。
func usagePercent(prev, cur cpuTimes) *float64 {
	if cur.decreased(prev) {
		return nil
	}
	totalDelta := cur.total() - prev.total()
	if totalDelta == 0 {
		return nil
	}
	idleDelta := cur.idleTotal() - prev.idleTotal()
	if idleDelta > totalDelta {
		// 内核自己都不自洽的数据，不猜。
		return nil
	}
	v := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	return &v
}

// memValues 是 /proc/meminfo 里本包关心的字段（单位：字节）。
type memValues struct {
	total     uint64
	free      uint64
	available uint64
	buffers   uint64
	cached    uint64
	swapTotal uint64
	swapFree  uint64
}

// ParseMeminfo 解析 /proc/meminfo。
//
// 需要的字段缺一个就报错：这种情况说明数据源不是我们认识的 /proc/meminfo，
// 与其给出 memset 为 0 的内存数据，不如让这一段标成失败。
func ParseMeminfo(data []byte) (memValues, error) {
	raw := make(map[string]uint64, 64)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			// 单位是 kB 的字段都是纯数字；解析不了就记 0，让下面的必需字段检查兜住。
			continue
		}
		unit := ""
		if len(fields) > 1 {
			unit = fields[1]
		}
		switch unit {
		case "", "B":
		case "kB", "KB":
			v *= 1024
		case "mB", "MB":
			v *= 1024 * 1024
		default:
			// 未知单位：不猜，按缺失处理。
			continue
		}
		raw[strings.TrimSpace(key)] = v
	}

	required := []string{
		"MemTotal", "MemFree", "MemAvailable", "Buffers", "Cached", "SwapTotal", "SwapFree",
	}
	var missing []string
	for _, k := range required {
		if _, ok := raw[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return memValues{}, fmt.Errorf("/proc/meminfo 缺少字段 %s", strings.Join(missing, ", "))
	}
	return memValues{
		total:     raw["MemTotal"],
		free:      raw["MemFree"],
		available: raw["MemAvailable"],
		buffers:   raw["Buffers"],
		cached:    raw["Cached"],
		swapTotal: raw["SwapTotal"],
		swapFree:  raw["SwapFree"],
	}, nil
}

// loadValues 是 /proc/loadavg 的内容。
type loadValues struct {
	load1, load5, load15 float64
	runnable             uint64
	threads              uint64
}

// ParseLoadavg 解析 /proc/loadavg：三个负载值加上 “可运行/总数” 与最后分配的 PID。
func ParseLoadavg(data []byte) (loadValues, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 4 {
		return loadValues{}, fmt.Errorf("/proc/loadavg 少于 4 个字段（实际 %d 个）", len(fields))
	}
	var out loadValues
	for i, dst := range []*float64{&out.load1, &out.load5, &out.load15} {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return loadValues{}, fmt.Errorf("/proc/loadavg 第 %d 个值 %q 不是数字", i+1, fields[i])
		}
		*dst = v
	}
	runnable, total, ok := strings.Cut(fields[3], "/")
	if !ok {
		return loadValues{}, fmt.Errorf("/proc/loadavg 的进程字段 %q 不是 a/b 形式", fields[3])
	}
	r, err := strconv.ParseUint(runnable, 10, 64)
	if err != nil {
		return loadValues{}, fmt.Errorf("/proc/loadavg 的可运行进程数 %q 不是无符号整数", runnable)
	}
	t, err := strconv.ParseUint(total, 10, 64)
	if err != nil {
		return loadValues{}, fmt.Errorf("/proc/loadavg 的进程总数 %q 不是无符号整数", total)
	}
	out.runnable, out.threads = r, t
	return out, nil
}

// netCounters 是一块网卡的累计计数器。
//
// 字段顺序就是 /proc/net/dev 冒号后的 16 列顺序，不能重排。
type netCounters struct {
	rxBytes   uint64
	rxPackets uint64
	rxErrors  uint64
	rxDropped uint64
	// rx fifo, frame, compressed, multicast 不用于本阶段展示，但必须被跳过。
	txBytes   uint64
	txPackets uint64
	txErrors  uint64
	txDropped uint64
}

// ParseNetDev 解析 /proc/net/dev，按网卡名返回计数器，按名字排序。
//
// 前两行是表头（含 '|'，不含 ':'），跳过；每行冒号后必须正好 16 列。
func ParseNetDev(data []byte) (map[string]netCounters, error) {
	out := make(map[string]netCounters, 8)
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, errors.New("/proc/net/dev 里有空的网卡名")
		}
		fields := strings.Fields(rest)
		if len(fields) != 16 {
			return nil, fmt.Errorf("/proc/net/dev 的 %s 行有 %d 列（应为 16 列）", name, len(fields))
		}
		var vals [8]uint64
		// 只取需要的列：0..3 是接收的 bytes/packets/errs/drop，8..11 是发送的同样四项。
		for i, idx := range []int{0, 1, 2, 3, 8, 9, 10, 11} {
			v, err := strconv.ParseUint(fields[idx], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("/proc/net/dev 的 %s 行第 %d 列 %q 不是无符号整数", name, idx+1, fields[idx])
			}
			vals[i] = v
		}
		out[name] = netCounters{
			rxBytes: vals[0], rxPackets: vals[1], rxErrors: vals[2], rxDropped: vals[3],
			txBytes: vals[4], txPackets: vals[5], txErrors: vals[6], txDropped: vals[7],
		}
	}
	if len(out) == 0 {
		return nil, errors.New("/proc/net/dev 里没有任何网卡")
	}
	return out, nil
}

// netNames 返回排序后的网卡名，保证输出稳定。
func netNames(m map[string]netCounters) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// osRelease 是 /etc/os-release 里本包用到的字段。
type osRelease struct {
	id         string
	name       string
	version    string
	prettyName string
}

// ParseOSRelease 解析 /etc/os-release（与 /usr/lib/os-release 同格式）。
//
// 只按 KEY=VALUE 解析，不猜发行版名字；值两侧的引号按 shell 变量的规则去掉。
func ParseOSRelease(data []byte) (osRelease, error) {
	var out osRelease
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))
		switch key {
		case "ID":
			out.id = value
		case "NAME":
			out.name = value
		case "VERSION_ID":
			out.version = value
		case "PRETTY_NAME":
			out.prettyName = value
		}
	}
	if out.id == "" && out.name == "" {
		return osRelease{}, errors.New("/etc/os-release 里既没有 ID 也没有 NAME")
	}
	return out, nil
}

// unquote 去掉一层成对的单/双引号，并处理双引号里的常见转义。
func unquote(v string) string {
	if len(v) < 2 {
		return v
	}
	if v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	return v
}

// ParseUptime 解析 /proc/uptime，返回开机秒数。
func ParseUptime(data []byte) (float64, error) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("/proc/uptime 是空的")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime 的第一个值 %q 不是数字", fields[0])
	}
	if v < 0 {
		return 0, fmt.Errorf("/proc/uptime 的值 %q 是负数", fields[0])
	}
	return v, nil
}

// cpuModelKeys 是各架构 /proc/cpuinfo 里表示型号的键，按优先级排列。
var cpuModelKeys = []string{"model name", "cpu model", "Hardware", "Processor"}

// ParseCPUModel 从 /proc/cpuinfo 取 CPU 型号。取不到返回空串（这是可选展示字段，
// 不是采集成功与否的依据）。
func ParseCPUModel(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, want := range cpuModelKeys {
			if key == want {
				return value
			}
		}
	}
	return ""
}
