package processes

import (
	"math"
	"strconv"
	"strings"
)

// 这里的都是“只做一件事”的小工具：拆行、拆字段、解析无符号数、格式化字节。
// 单独放一处是为了让解析函数读起来像格式说明，而不是像一堆内联的 strings 调用。

// splitLines 按行拆开（忽略末尾空行）。
func splitLines(data []byte) []string {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// fieldsOf 按空白拆字段。
func fieldsOf(line string) []string { return strings.Fields(line) }

// isCPUName 判断 /proc/stat 里的行名是否形如 cpuN。
func isCPUName(name string) bool {
	if !strings.HasPrefix(name, "cpu") || len(name) == 3 {
		return false
	}
	for i := 3; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// parseUint 解析无符号整数；解析不了当 0（/proc 的字段只做累加，不做验证）。
func parseUint(text string) uint64 {
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// round1 保留一位小数。
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// formatBytes 把字节数格式化成十进制字符串（超过 2^53 的字节数不能走 JSON number）。
func formatBytes(bytes int64) string {
	return strconv.FormatInt(bytes, 10)
}
