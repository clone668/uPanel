package monitoring

// 审计回归测试。

import (
	"strings"
	"testing"
)

// ParseStat 的字段数检查曾是 len(fields) < 8：cpu 行恰好 7 个值（连名字 8 段）
// 时检查放行，随后 fields[i+1] 越界，解析器靠 panic 拒绝坏输入。
// 现在必须返回错误。
func TestParseStatShortLineReturnsErrorNotPanic(t *testing.T) {
	// 7 个值：恰好穿过旧的 < 8 检查、又不够下面取 8 个值。
	_, _, err := ParseStat([]byte("cpu 1 2 3 4 5 6 7\n"))
	if err == nil {
		t.Fatal("只有 7 个值的 cpu 行必须报错")
	}
	if !strings.Contains(err.Error(), "至少 8 个") {
		t.Fatalf("错误信息应当说明值的个数不够，收到：%v", err)
	}
	// 按核行同样要挡。
	if _, _, err := ParseStat([]byte("cpu 1 2 3 4 5 6 7 8\ncpu0 1 2 3 4 5 6 7\n")); err == nil {
		t.Fatal("只有 7 个值的 cpu0 行必须报错")
	}
}
