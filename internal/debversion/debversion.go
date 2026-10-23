// Package debversion 按 dpkg 的规则比较 Debian 版本号。
//
// 为什么不能直接比较字符串：真实主机上就有反例。P11.1 的验收机上，
// `open-vm-tools` 的候选版本是 `2:12.3.5-3~ubuntu0.22.04.3` → `2:12.3.5-3~ubuntu0.22.04.4`，
// 字符串比较认不出后者更大；`~` 在 Debian 里排在**空串之前**（`1.0~rc1` < `1.0`），
// 而 epoch（`2:`）比 upstream 版本更优先（`1:0.1` > `9.9`）。
//
// 规则来自 dpkg 的 `parseversion()` 与 `verrevcmp()`（lib/dpkg/version.c）：
//
//	version = [epoch ":"] upstream ["-" debian_revision]
//
// 比较顺序是 epoch（数值）→ upstream → debian_revision，后两段逐字符比较：
// 非数字段按 order() 定序（`~` < 空 < ASCII 字母 < 其它符号（+256）），数字段按数值比较
// （先跳过前导零，再比长度与字典序）。
//
// 本包只做比较，不做“这个版本号合不合法”的宽松解释：不合规的输入返回错误，
// 由调用方把它翻译成“读不懂这个版本”，而不是猜一个顺序。
package debversion

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidVersion 表示版本号不符合 Debian 的语法，无法比较。
var ErrInvalidVersion = errors.New("debversion: 版本号不合法")

// Version 是一个解析后的 Debian 版本号。
type Version struct {
	// Epoch 是冒号前的数字（没有冒号时为 0）。
	Epoch int64
	// Upstream 是 upstream 版本段。
	Upstream string
	// Revision 是最后一个 `-` 之后的 Debian 修订段；没有则为空串。
	Revision string
	// Raw 是原始输入，便于把“哪个版本比较失败了”原样报出来。
	Raw string
}

// String 返回原始输入。
func (v Version) String() string { return v.Raw }

// Parse 解析一个 Debian 版本号。
//
// 严格程度与 dpkg 的 parseversion() 对齐：
//   - 版本号不能为空；
//   - 冒号前必须是十进制数字（不允许前导 `+`/`-`/空）；
//   - upstream 段必须非空、且以数字开头（Debian Policy 5.6.12）；
//   - upstream 只允许字母、数字与 `. + - : ~`；revision 只允许字母、数字与 `. + ~`；
//   - 有 `-` 就必须有非空 revision。
func Parse(raw string) (Version, error) {
	if raw == "" {
		return Version{}, fmt.Errorf("%w: 空串", ErrInvalidVersion)
	}
	v := Version{Raw: raw}
	rest := raw
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		epoch := rest[:i]
		if epoch == "" {
			return Version{}, fmt.Errorf("%w: %q 的 epoch 为空", ErrInvalidVersion, raw)
		}
		// strconv 会接受 "+1"，dpkg 不会：epoch 必须是纯十进制数字。
		for k := 0; k < len(epoch); k++ {
			if epoch[k] < '0' || epoch[k] > '9' {
				return Version{}, fmt.Errorf("%w: %q 的 epoch %q 不是十进制数字",
					ErrInvalidVersion, raw, epoch)
			}
		}
		n, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("%w: %q 的 epoch %q 不是十进制数字",
				ErrInvalidVersion, raw, epoch)
		}
		if n < 0 {
			return Version{}, fmt.Errorf("%w: %q 的 epoch 为负", ErrInvalidVersion, raw)
		}
		v.Epoch = n
		rest = rest[i+1:]
	}
	if rest == "" {
		return Version{}, fmt.Errorf("%w: %q 只有 epoch", ErrInvalidVersion, raw)
	}
	if i := strings.LastIndexByte(rest, '-'); i >= 0 {
		v.Upstream = rest[:i]
		v.Revision = rest[i+1:]
		if v.Revision == "" {
			return Version{}, fmt.Errorf("%w: %q 的修订段为空", ErrInvalidVersion, raw)
		}
	} else {
		v.Upstream = rest
	}
	if v.Upstream == "" {
		return Version{}, fmt.Errorf("%w: %q 的 upstream 段为空", ErrInvalidVersion, raw)
	}
	if v.Upstream[0] < '0' || v.Upstream[0] > '9' {
		return Version{}, fmt.Errorf("%w: %q 的 upstream 段必须数字开头（Debian Policy 5.6.12）",
			ErrInvalidVersion, raw)
	}
	for i := 0; i < len(v.Upstream); i++ {
		if !upstreamChar(v.Upstream[i]) {
			return Version{}, fmt.Errorf("%w: %q 的 upstream 段含非法字符 %q",
				ErrInvalidVersion, raw, string(v.Upstream[i]))
		}
	}
	for i := 0; i < len(v.Revision); i++ {
		if !revisionChar(v.Revision[i]) {
			return Version{}, fmt.Errorf("%w: %q 的修订段含非法字符 %q",
				ErrInvalidVersion, raw, string(v.Revision[i]))
		}
	}
	return v, nil
}

func upstreamChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c == '.', c == '+', c == '-', c == ':', c == '~':
		return true
	}
	return false
}

func revisionChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c == '.', c == '+', c == '~':
		return true
	}
	return false
}

// Compare 比较两个**已解析**的版本号：a<b 返回 -1，a==b 返回 0，a>b 返回 1。
func Compare(a, b Version) int {
	if a.Epoch != b.Epoch {
		if a.Epoch < b.Epoch {
			return -1
		}
		return 1
	}
	if c := verrevcmp(a.Upstream, b.Upstream); c != 0 {
		return sign(c)
	}
	return sign(verrevcmp(a.Revision, b.Revision))
}

// CompareStrings 解析并比较两个版本号。
//
// 任意一边解析失败都返回错误（并带上出错的那个原文）。调用方必须把它当成
// “关系未知”（`candidate_relation=unknown`），不能当成“相等”——把读不懂的版本
// 说成相等，会让一个真正可升级的包在页面上消失。
func CompareStrings(a, b string) (int, error) {
	va, err := Parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	return Compare(va, vb), nil
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// order 是 dpkg 的字符定序：`~` 最小（-1），字符串结束其次（0），
// ASCII 字母按自身码值，其余符号整体排在字母之后（+256）。
//
// 这套定序解释了两件反直觉的事：`1.0~rc1` < `1.0`，而 `1.0+` > `1.0a`。
func order(c byte) int {
	switch {
	case c == 0:
		return 0
	case c >= '0' && c <= '9':
		// 数字段在 verrevcmp 里单独按数值比，这里不该被调用。
		return 0
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return int(c)
	case c == '~':
		return -1
	}
	return int(c) + 256
}

// verrevcmp 是 dpkg verrevcmp() 的移植：逐段比较非数字部分与数字部分。
func verrevcmp(a, b string) int {
	i, j := 0, 0
	at := func(s string, k int) byte {
		if k < len(s) {
			return s[k]
		}
		return 0
	}
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for i < len(a) || j < len(b) {
		firstDiff := 0
		// 非数字段：逐字符按 order() 比。
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			ac, bc := order(at(a, i)), order(at(b, j))
			if ac != bc {
				return ac - bc
			}
			if i < len(a) {
				i++
			}
			if j < len(b) {
				j++
			}
		}
		// 数字段：先跳前导零，再按“长度优先、其次字典序”比较。
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		for i < len(a) && j < len(b) && isDigit(a[i]) && isDigit(b[j]) {
			if firstDiff == 0 {
				firstDiff = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isDigit(a[i]) {
			return 1
		}
		if j < len(b) && isDigit(b[j]) {
			return -1
		}
		if firstDiff != 0 {
			return firstDiff
		}
	}
	return 0
}
