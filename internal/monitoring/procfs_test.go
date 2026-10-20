package monitoring

import (
	"strings"
	"testing"

	"upanel/internal/testsupport"
)

// fixture 读取 tests/fixtures 下的样本。真实样本来自验收 VM（见同目录 PROVENANCE.md），
// *.synthetic.* 是手写反例。查找逻辑（含验收 VM 上的 UPANEL_FIXTURES）在 testsupport 里。
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := testsupport.Fixture(name)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return raw
}

func TestParseStatRealFixture(t *testing.T) {
	agg, per, err := ParseStat(fixture(t, "stat-cpu-2404.txt"))
	if err != nil {
		t.Fatalf("解析真实 /proc/stat 失败: %v", err)
	}
	if agg.user != 20427 || agg.idle != 378980 || agg.iowait != 5878 || agg.softirq != 1022 {
		t.Fatalf("合计行解析错误: %+v", agg)
	}
	if len(per) != 2 {
		t.Fatalf("逻辑核数量 = %d，期望 2（VM 是 2 vCPU）", len(per))
	}
	if per[0].user != 10308 || per[1].user != 10119 {
		t.Fatalf("按核解析错误: %+v", per)
	}
	// intr/ctxt/btime 等行必须被跳过而不是被当成 cpu 行。
	if agg.total() == 0 {
		t.Fatal("合计时间不应为 0")
	}
}

func TestParseStatRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"分量不足", string(fixture(t, "stat.synthetic.truncated.txt")), "至少 8 个"},
		{"没有合计行", string(fixture(t, "stat.synthetic.no-agg.txt")), "没有合计的 cpu 行"},
		{"非数字分量", "cpu  1 2 x 4 5 6 7 8\n", "不是无符号整数"},
	}
	for _, c := range cases {
		_, _, err := ParseStat([]byte(c.data))
		if err == nil {
			t.Fatalf("%s: 期望报错，实际成功", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误信息 %q 不含 %q", c.name, err.Error(), c.want)
		}
	}
	// "cpuu" 不是 cpu 行也不是按核行：必须停止按核收集，而不是被当成合计行。
	if _, _, err := ParseStat([]byte("cpuu 1 2 3 4 5 6 7 8\n")); err == nil {
		t.Fatal("cpuu 行不应被当成合计 cpu 行")
	}
}

func TestUsagePercent(t *testing.T) {
	prev := cpuTimes{user: 100, system: 100, idle: 800}
	cur := cpuTimes{user: 150, system: 150, idle: 900}

	got := usagePercent(prev, cur)
	if got == nil {
		t.Fatal("有前后两次样本时应能算出使用率")
	}
	// total delta 200，idle delta 100 → 使用率 50%。
	if *got < 49.9 || *got > 50.1 {
		t.Fatalf("使用率 = %v，期望 50", *got)
	}

	if usagePercent(prev, prev) != nil {
		t.Fatal("时间窗没有推进时必须返回 nil，不能报 0%")
	}
	reset := cpuTimes{user: 1, system: 1, idle: 2}
	if usagePercent(prev, reset) != nil {
		t.Fatal("计数器变小（重置/重启）时必须返回 nil")
	}
	inconsistent := cpuTimes{user: 200, system: 100, idle: 200}
	if usagePercent(prev, inconsistent) != nil {
		t.Fatal("idle 增量大于总量（内核数据不自洽）时必须返回 nil")
	}
}

func TestParseMeminfoRealFixture(t *testing.T) {
	m, err := ParseMeminfo(fixture(t, "meminfo-2404.txt"))
	if err != nil {
		t.Fatalf("解析真实 /proc/meminfo 失败: %v", err)
	}
	if m.total != 4009864*1024 {
		t.Fatalf("MemTotal = %d，期望 %d（4009864 kB）", m.total, 4009864*1024)
	}
	if m.available != 3637424*1024 {
		t.Fatalf("MemAvailable = %d", m.available)
	}
	if m.free != 3322760*1024 || m.buffers != 78204*1024 || m.cached != 436972*1024 {
		t.Fatalf("其余字段解析错误: %+v", m)
	}
	if m.swapTotal != 0 || m.swapFree != 0 {
		t.Fatalf("这台 VM 没有 swap，解析结果应为 0: %+v", m)
	}
}

func TestParseMeminfoRejectsMissingField(t *testing.T) {
	_, err := ParseMeminfo(fixture(t, "meminfo.synthetic.missing-available.txt"))
	if err == nil {
		t.Fatal("缺少 MemAvailable 必须报错")
	}
	if !strings.Contains(err.Error(), "MemAvailable") {
		t.Fatalf("错误信息应点名缺失字段，实际 %q", err.Error())
	}
	// 未知单位不猜：整行跳过，于是缺字段。
	if _, err := ParseMeminfo([]byte("MemTotal: 1 MiB\nMemFree: 1 kB\nMemAvailable: 1 kB\nBuffers: 1 kB\nCached: 1 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")); err == nil {
		t.Fatal("未知单位必须按缺失处理，而不是按字节数当成有效值")
	}
}

func TestParseLoadavgRealFixture(t *testing.T) {
	l, err := ParseLoadavg([]byte("0.10 0.11 0.15 1/127 37178\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if l.load1 != 0.10 || l.load5 != 0.11 || l.load15 != 0.15 {
		t.Fatalf("负载值解析错误: %+v", l)
	}
	if l.runnable != 1 || l.threads != 127 {
		t.Fatalf("可运行/总线程解析错误: %+v", l)
	}
	if _, err := ParseLoadavg(fixture(t, "loadavg.synthetic.bad.txt")); err == nil {
		t.Fatal("非数字负载值必须报错")
	}
	if _, err := ParseLoadavg([]byte("0.1 0.2 0.3\n")); err == nil {
		t.Fatal("缺少进程字段必须报错")
	}
}

func TestParseNetDevRealFixture(t *testing.T) {
	counters, err := ParseNetDev(fixture(t, "netdev-2404.txt"))
	if err != nil {
		t.Fatalf("解析真实 /proc/net/dev 失败: %v", err)
	}
	if len(counters) != 2 {
		t.Fatalf("网卡数量 = %d，期望 2（lo、ens4）", len(counters))
	}
	lo := counters["lo"]
	if lo.rxBytes != 3022016 || lo.txBytes != 3022016 || lo.rxPackets != 10310 {
		t.Fatalf("lo 计数器解析错误: %+v", lo)
	}
	ens := counters["ens4"]
	if ens.rxBytes != 402930897 || ens.txBytes != 3350163 || ens.txPackets != 21162 {
		t.Fatalf("ens4 计数器解析错误: %+v", ens)
	}
	names := netNames(counters)
	if strings.Join(names, ",") != "ens4,lo" {
		t.Fatalf("网卡名应按字典序: %v", names)
	}
}

func TestParseNetDevRejectsMalformed(t *testing.T) {
	if _, err := ParseNetDev(fixture(t, "netdev.synthetic.badcols.txt")); err == nil {
		t.Fatal("列数不是 16 必须报错")
	}
	if _, err := ParseNetDev(fixture(t, "netdev.synthetic.empty.txt")); err == nil {
		t.Fatal("没有任何网卡必须报错")
	}
	if _, err := ParseNetDev([]byte("ens4: not-a-number 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n")); err == nil {
		t.Fatal("非数字计数器必须报错")
	}
}

func TestParseOSReleaseRealFixture(t *testing.T) {
	rel, err := ParseOSRelease(fixture(t, "os-release-2404.txt"))
	if err != nil {
		t.Fatalf("解析真实 /etc/os-release 失败: %v", err)
	}
	if rel.id != "ubuntu" {
		t.Fatalf("ID = %q", rel.id)
	}
	if rel.version != "24.04" {
		t.Fatalf("VERSION_ID = %q", rel.version)
	}
	if rel.prettyName != "Ubuntu 24.04.5 LTS" {
		t.Fatalf("PRETTY_NAME = %q", rel.prettyName)
	}
	if rel.name != "Ubuntu" {
		t.Fatalf("NAME = %q", rel.name)
	}
}

func TestParseOSReleaseRejectsUnknownFile(t *testing.T) {
	if _, err := ParseOSRelease(fixture(t, "os-release.synthetic.no-id.txt")); err == nil {
		t.Fatal("既没有 ID 也没有 NAME 必须报错")
	}
}

func TestParseOSReleaseUnquoting(t *testing.T) {
	rel, err := ParseOSRelease([]byte("# 注释\nID=debian\nNAME=\"Debian GNU/Linux\"\nVERSION_ID='12'\nPRETTY_NAME=\"Debian \\\"bookworm\\\"\"\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if rel.name != "Debian GNU/Linux" {
		t.Fatalf("双引号未去掉: %q", rel.name)
	}
	if rel.version != "12" {
		t.Fatalf("单引号未去掉: %q", rel.version)
	}
	if rel.prettyName != `Debian "bookworm"` {
		t.Fatalf("转义未还原: %q", rel.prettyName)
	}
}

func TestParseUptime(t *testing.T) {
	up, err := ParseUptime(fixture(t, "uptime-2404.txt"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if up != 2076.61 {
		t.Fatalf("开机秒数 = %v，期望 2076.61", up)
	}
	for _, bad := range []string{"", "abc 1.0", "-1.0 2.0"} {
		if _, err := ParseUptime([]byte(bad)); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

func TestParseCPUModel(t *testing.T) {
	x86 := "processor\t: 0\nmodel name\t: Intel(R) Xeon(R) CPU E5-2670 0 @ 2.60GHz\ncpu MHz\t\t: 2600.000\n"
	if got := ParseCPUModel([]byte(x86)); got != "Intel(R) Xeon(R) CPU E5-2670 0 @ 2.60GHz" {
		t.Fatalf("x86 型号 = %q", got)
	}
	arm := "processor\t: 0\nBogoMIPS\t: 108.00\nHardware\t: BCM2835\n"
	if got := ParseCPUModel([]byte(arm)); got != "BCM2835" {
		t.Fatalf("ARM 型号 = %q", got)
	}
	if got := ParseCPUModel([]byte("processor\t: 0\n")); got != "" {
		t.Fatalf("没有型号字段时应返回空串，实际 %q", got)
	}
}
