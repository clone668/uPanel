package network

import (
	"errors"
	"fmt"
)

// errHexLen 表示 /proc/net 里的地址长度不对（IPv4 应 8 个十六进制字符，IPv6 应 32）。
var errHexLen = errors.New("地址长度不对")

// errLine 给解析失败加一句定位：说清是哪种错，并附上行的前 32 个字符。
//
// 只附行首是因为这一行里可能带着别人连接的地址；定位问题前 32 个字符足够
// （序号 + 本地地址），不需要把整行搬进错误里再搬进日志。
func errLine(what, line string) error {
	const limit = 32
	if len(line) > limit {
		line = line[:limit] + "…"
	}
	return fmt.Errorf("%s：%s", what, line)
}
