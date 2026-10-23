// 这些向量不是“想出来”的：左列绝大多数来自真机（P11.1 验收的 22.04 与 24.04 虚机上
// `dpkg-query` / `apt-cache policy` 的原文），以及 dpkg 自己的版本比较用例。
//
// 这个包存在的意义就在这些反例上：字符串比较会把 `2:12.3.5-3~ubuntu0.22.04.3` 与
// `2:12.3.5-3~ubuntu0.22.04.4` 的大小判反，会把 `1.0~rc1` 判成比 `1.0` 大，
// 会把 `1:0.1` 判成比 `9.9` 小。
package debversion

import (
	"errors"
	"testing"
)

func TestCompareStrings(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		why  string
	}{
		{"1.0", "1.0", 0, "完全相同"},
		{"1.0", "1.1", -1, "普通递增"},
		{"1.1", "1.0", 1, "反序"},
		{"1.0", "1.0-1", -1, "没有修订段 < 有修订段"},
		{"1.0-1", "1.0-2", -1, "修订段比较"},
		{"1.0-1", "1.0-1~", 1, "`~` 排在空串之前：1.0-1~ 比 1.0-1 小"},
		{"1.0~rc1", "1.0", -1, "预发布版本小于正式版本"},
		{"1.0~~", "1.0~", -1, "更多的 `~` 更小"},
		{"1.0", "1.0+b1", -1, "符号段排在后面"},
		{"1.0+", "1.0a", 1, "`+`(299) 排在字母 a(97) 之后"},
		{"1.0a", "1.0", 1, "字母结尾更大"},
		{"1:1.0", "2.0", 1, "epoch 优先于 upstream"},
		{"1:0.1", "9.9", 1, "epoch 1 的 0.1 大于 epoch 0 的 9.9"},
		{"1.0-1", "1:0.9-1", -1, "没有 epoch 等于 epoch 0"},
		{"1.01", "1.1", 0, "前导零不参与比较"},
		{"1.0", "1.00", 0, "同上"},
		{"007:1.0", "7:1.0", 0, "epoch 的前导零同样忽略"},
		{"1.0", "1.0-0", 0, "dpkg 认为空修订段与 0 相等"},
		{"1.2.3", "1.2.3-0ubuntu1", -1, "带发行版修订的更大"},
		// 真机原文（P11.1 验收 22.04）：
		{"2:12.3.5-3~ubuntu0.22.04.3", "2:12.3.5-3~ubuntu0.22.04.4", -1, "open-vm-tools 待升级"},
		{"2.1.5-1ubuntu1.1", "2.1.5-1ubuntu1.3", -1, "open-iscsi / libopeniscsiusr 待升级"},
		{"3.0.4-2ubuntu2.4", "3.0.4-2ubuntu2.5", -1, "apparmor：security 源里的是更旧的那个"},
		{"2.20.11-0ubuntu82.10", "2.20.11-0ubuntu82.10", 0, "apport：候选与已安装相同"},
		{"1.18.0-6ubuntu14.4", "1.18.0-6ubuntu14.5", -1, "nginx 在 22.04 上的升级"},
		// 真机原文（24.04）：候选比已安装旧的情形随 pin/回退出现，方向必须判对。
		{"1.24.0-2ubuntu7.1", "1.24.0-2ubuntu7", 1, "候选比已安装旧时返回正数"},
	}
	for _, c := range cases {
		got, err := CompareStrings(c.a, c.b)
		if err != nil {
			t.Errorf("CompareStrings(%q, %q) 报错: %v（%s）", c.a, c.b, err, c.why)
			continue
		}
		if got != c.want {
			t.Errorf("CompareStrings(%q, %q) = %d，期望 %d（%s）", c.a, c.b, got, c.want, c.why)
		}
		// 反对称性：反过来必须同号。
		rev, err := CompareStrings(c.b, c.a)
		if err != nil {
			t.Errorf("CompareStrings(%q, %q) 报错: %v", c.b, c.a, err)
			continue
		}
		if rev != -c.want {
			t.Errorf("CompareStrings(%q, %q) = %d，与正序 %d 不同号", c.b, c.a, rev, c.want)
		}
	}
}

// TestParseFields 锁住解析结果：epoch/upstream/revision 的切分是“为什么这么比”的依据。
func TestParseFields(t *testing.T) {
	cases := []struct {
		raw                string
		epoch              int64
		upstream, revision string
	}{
		{"1.0", 0, "1.0", ""},
		{"1.0-1", 0, "1.0", "1"},
		{"2:12.3.5-3~ubuntu0.22.04.3", 2, "12.3.5", "3~ubuntu0.22.04.3"},
		{"1:1.0", 1, "1.0", ""},
		{"1.0-1-2", 0, "1.0-1", "2"},
		{"0.1.5-1ubuntu1.1", 0, "0.1.5", "1ubuntu1.1"},
	}
	for _, c := range cases {
		v, err := Parse(c.raw)
		if err != nil {
			t.Errorf("Parse(%q) 报错: %v", c.raw, err)
			continue
		}
		if v.Epoch != c.epoch || v.Upstream != c.upstream || v.Revision != c.revision {
			t.Errorf("Parse(%q) = {%d %q %q}，期望 {%d %q %q}",
				c.raw, v.Epoch, v.Upstream, v.Revision, c.epoch, c.upstream, c.revision)
		}
		if v.String() != c.raw {
			t.Errorf("Parse(%q).String() = %q", c.raw, v.String())
		}
	}
}

// TestParseRejects 确认“读不懂就说读不懂”。把读不懂的版本当成相等，
// 会让一个真正可升级的包在页面上消失。
func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		":1.0",
		"+1:1.0",
		"a:1.0",
		"1.0-",
		"-1.0",
		"1.0-x_y",
		"v1.0",
		"1.0 ",
		"1.0\n",
	}
	for _, raw := range bad {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) 应当失败，却成功了", raw)
		} else if !errors.Is(err, ErrInvalidVersion) {
			t.Errorf("Parse(%q) 的错误不是 ErrInvalidVersion: %v", raw, err)
		}
	}
	// 任意一边不合法，比较就必须报错，而不是猜一个顺序。
	if _, err := CompareStrings("1.0", "v2.0"); err == nil {
		t.Error("CompareStrings 对不合法的 b 应当报错")
	}
	if _, err := CompareStrings("oops", "1.0"); err == nil {
		t.Error("CompareStrings 对不合法的 a 应当报错")
	}
}
