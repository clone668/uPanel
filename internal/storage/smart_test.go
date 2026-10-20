package storage

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
	"upanel/internal/testsupport"
)

// SMART 的用例分成两种输入来源，混用会让结论失真，所以在每个用例上写明：
//
//   - 真实采集：tests/fixtures/smart/ 下的两份 smartctl -j 输出，来自 VM-E
//     （QEMU NVMe 与 QEMU SCSI 各一块），采集命令与日期见 PROVENANCE.md；
//   - **手工构造**：smartctl 不输出坏 JSON，也不在真机上给我们一块正在失效的盘。
//     失效、预失效、待机、损坏输出这些分支只能用合成输入覆盖，代码里标了 syntheticXXX。
//
// 合成用例证明的是“解析逻辑正确”，不是“真机上见过这个结果”。真机覆盖在
// docs/PROGRESS.md 的 P04.3 校验表里单独列，两者不能互相顶替。

// smartFixture 读一份真实采集的 smartctl JSON。
func smartFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := testsupport.Fixture(filepath.Join("smart", name))
	if err != nil {
		t.Fatalf("读 SMART 夹具 %s 失败: %v", name, err)
	}
	return string(raw)
}

// TestEvaluateSmartNVMeRealCapture 用 VM-E 的真实 NVMe 输出核对健康结论：
// 一块正常的 QEMU NVMe，smart_status.passed=true、critical_warning=0、温度 50 ℃。
func TestEvaluateSmartNVMeRealCapture(t *testing.T) {
	out := smartFixture(t, "upanel-smart-nvme0n1.json")
	h := evaluateSmart(out, 0, "", fixedNow)

	if h.Status != HealthHealthy {
		t.Errorf("Status = %q，想要 %q（details: %v）", h.Status, HealthHealthy, deref(h.Details))
	}
	if h.Source != smartHealthSource {
		t.Errorf("Source = %q，想要 %q", h.Source, smartHealthSource)
	}
	if h.CheckedAt == nil || !h.CheckedAt.Equal(fixedNow) {
		t.Errorf("CheckedAt = %v，想要 %v", h.CheckedAt, fixedNow)
	}
	if h.TemperatureCelsius == nil {
		t.Fatalf("TemperatureCelsius 为 null，但夹具里有 50 ℃")
	}
	if *h.TemperatureCelsius != 50 {
		t.Errorf("TemperatureCelsius = %v，想要 50", *h.TemperatureCelsius)
	}
}

// TestEvaluateSmartSCSIUnsupportedRealCapture：QEMU 的 SCSI 盘没有 SMART。
// 结论必须是 unsupported，而不是 healthy、也不是 unknown：
// “这块盘没有这个功能”和“我们没问出来”在现场处置上是两件不同的事。
func TestEvaluateSmartSCSIUnsupportedRealCapture(t *testing.T) {
	out := smartFixture(t, "upanel-smart-sda-scsi.json")
	h := evaluateSmart(out, 0, "", fixedNow)

	if h.Status != HealthUnsupported {
		t.Errorf("Status = %q，想要 %q", h.Status, HealthUnsupported)
	}
	if h.Details == nil || !strings.Contains(*h.Details, "不支持 SMART") {
		t.Errorf("Details = %v，要说明不支持 SMART", deref(h.Details))
	}
	// 夹具里 temperature.current = 0：这是“没有读数”，不是 0 ℃。
	if h.TemperatureCelsius != nil {
		t.Errorf("TemperatureCelsius = %v，current=0 必须转成 null", *h.TemperatureCelsius)
	}
}

// syntheticSmartJSON 是手工构造的 smartctl 输出骨架。
// 只放本阶段解析的字段，不冒充真实采集。占位符用 __NAME__ 形式，
// 避免 %s 的顺序依赖（模板里有两个 %s，替换错位会让用例假绿）。
const syntheticSmartJSON = `{
  "smartctl": {"exit_status": __EXIT__, "version": [7, 4], "messages": [__MESSAGES__]},
  "device": {"name": "/dev/sda", "type": "ata", "protocol": "ATA"},
  "model_name": "SYNTHETIC DISK",
  "serial_number": "SYNTH-0001",
  "smart_support": {"available": true, "enabled": true},
  __BODY__,
  "temperature": {"current": 35}
}`

// synthetic 用退出状态与正文填充骨架。
func synthetic(exit int, body string) string {
	out := strings.Replace(syntheticSmartJSON, "__EXIT__", strconv.Itoa(exit), 1)
	out = strings.Replace(out, "__MESSAGES__", "", 1)
	return strings.Replace(out, "__BODY__", body, 1)
}

// TestEvaluateSmartBitmask 逐位校验 smartctl 的退出状态位。
// 位是契约：把“磁盘失效”当成“命令失败”会让最该报警的那块盘显示成 unknown。
func TestEvaluateSmartBitmask(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		exit    int
		want    string
		reasons string // Details 里必须出现的关键词
	}{
		{
			name:    "磁盘失效",
			status:  `"smart_status": {"passed": false}`,
			exit:    statusBitDiskFailing,
			want:    HealthCritical,
			reasons: "DISK FAILING",
		},
		{
			name:    "预失效属性当前低于阈值",
			status:  `"smart_status": {"passed": true}`,
			exit:    statusBitPrefailNow,
			want:    HealthCritical,
			reasons: "当前低于阈值",
		},
		{
			name:    "属性过去低于阈值",
			status:  `"smart_status": {"passed": true}`,
			exit:    statusBitPrefailPast,
			want:    HealthWarning,
			reasons: "过去低于阈值",
		},
		{
			name:    "错误日志有记录",
			status:  `"smart_status": {"passed": true}`,
			exit:    statusBitErrorLog,
			want:    HealthWarning,
			reasons: "错误日志",
		},
		{
			name:    "自检日志有失败记录",
			status:  `"smart_status": {"passed": true}`,
			exit:    statusBitSelfTestLog,
			want:    HealthWarning,
			reasons: "自检日志",
		},
		{
			name:    "SMART 命令失败",
			status:  `"smart_status": {"passed": true}`,
			exit:    statusBitSmartCmdFail,
			want:    HealthUnknown,
			reasons: "SMART 命令执行失败",
		},
		{
			name:    "打不开设备",
			status:  `"smart_status": {"passed": true}`,
			exit:    statusBitOpenFailed,
			want:    HealthUnknown,
			reasons: "无法打开设备",
		},
		{
			name:    "命令行被拒绝",
			status:  `"smart_status": {"passed": true}`,
			exit:    statusBitCmdLine,
			want:    HealthUnknown,
			reasons: "命令行参数被拒绝",
		},
		{
			name:    "失效与错误日志同时置位时以失效为准",
			status:  `"smart_status": {"passed": false}`,
			exit:    statusBitDiskFailing | statusBitErrorLog,
			want:    HealthCritical,
			reasons: "DISK FAILING",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := synthetic(tc.exit, tc.status)
			h := evaluateSmart(out, tc.exit, "", fixedNow)
			if h.Status != tc.want {
				t.Fatalf("Status = %q，想要 %q（details: %v）",
					h.Status, tc.want, deref(h.Details))
			}
			if h.Details == nil || !strings.Contains(*h.Details, tc.reasons) {
				t.Errorf("Details = %v，应包含 %q", deref(h.Details), tc.reasons)
			}
		})
	}
}

// TestEvaluateSmartOverallFailureIsCritical：退出状态干净但 JSON 说总体未通过，
// 依然要报 critical —— 结论在 JSON 里，不在退出码里。
func TestEvaluateSmartOverallFailureIsCritical(t *testing.T) {
	out := synthetic(0, `"smart_status": {"passed": false}`)
	h := evaluateSmart(out, 0, "", fixedNow)
	if h.Status != HealthCritical {
		t.Errorf("Status = %q，JSON 说总体未通过就必须是 critical", h.Status)
	}
}

// TestEvaluateSmartNVMECriticalWarning 校验 NVMe 健康日志的告警位分级。
func TestEvaluateSmartNVMECriticalWarning(t *testing.T) {
	cases := []struct {
		name   string
		bits   int
		used   int
		media  int64
		want   string
		reason string
	}{
		{"备用块不足是告警", 1 << 0, 0, 0, HealthWarning, "可用备用块低于阈值"},
		{"温度越界是告警", 1 << 1, 0, 0, HealthWarning, "温度超出阈值"},
		{"可靠性下降是严重", 1 << 2, 0, 0, HealthCritical, "可靠性下降"},
		{"只读模式是严重", 1 << 3, 0, 0, HealthCritical, "只读模式"},
		{"介质错误计数是告警", 0, 0, 7, HealthWarning, "介质错误 7 条"},
		{"寿命用尽是告警", 0, 100, 0, HealthWarning, "寿命已用 100%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := `"nvme_smart_health_information_log": {"critical_warning": ` +
				strconv.Itoa(tc.bits) + `, "temperature": 40, "percentage_used": ` + strconv.Itoa(tc.used) +
				`, "media_errors": ` + strconv.FormatInt(tc.media, 10) + `}`
			out := synthetic(0, log)

			h := evaluateSmart(out, 0, "", fixedNow)
			if h.Status != tc.want {
				t.Fatalf("Status = %q，想要 %q（details: %v）",
					h.Status, tc.want, deref(h.Details))
			}
			if h.Details == nil || !strings.Contains(*h.Details, tc.reason) {
				t.Errorf("Details = %v，应包含 %q", deref(h.Details), tc.reason)
			}
		})
	}
}

// TestEvaluateSmartNVMECleanLogIsHealthy：没有 smart_status、只有健康日志且没有告警位时，
// NVMe 的健康日志本身就是可信的“通过”。
func TestEvaluateSmartNVMECleanLogIsHealthy(t *testing.T) {
	log := `"nvme_smart_health_information_log": {"critical_warning": 0, "temperature": 40, "percentage_used": 12}`
	out := synthetic(0, log)

	if h := evaluateSmart(out, 0, "", fixedNow); h.Status != HealthHealthy {
		t.Errorf("Status = %q，想要 %q（details: %v）", h.Status, HealthHealthy, deref(h.Details))
	}
}

// TestEvaluateSmartATAWhenFailed：ATA 属性表里 when_failed 非空说明盘自己记过掉阈值。
func TestEvaluateSmartATAWhenFailed(t *testing.T) {
	ata := `"ata_smart_attributes": {"table": [
      {"id": 5, "name": "Reallocated_Sector_Ct", "value": 100, "worst": 100, "thresh": 10,
       "when_failed": "now", "raw": {"value": 8, "string": "8"}}
    ]}, "smart_status": {"passed": true}`
	out := synthetic(0, ata)
	h := evaluateSmart(out, 0, "", fixedNow)
	if h.Status != HealthWarning {
		t.Errorf("Status = %q，想要 %q（details: %v）", h.Status, HealthWarning, deref(h.Details))
	}
	if h.Details == nil || !strings.Contains(*h.Details, "Reallocated_Sector_Ct") {
		t.Errorf("Details = %v，应指明是哪个属性", deref(h.Details))
	}
}

// TestEvaluateSmartStandby 校验带 -n standby 时的“没唤醒”结论。
//
// 只看退出状态位 1 不够：位 1 同时表示“打不开设备”。合成输入里给出
// 退出状态 2、没有 device/model、message 提到 standby，这三点齐了才是待机。
func TestEvaluateSmartStandby(t *testing.T) {
	msg := `{"string": "Device is in STANDBY mode, exit(2)", "severity": "information"}`
	out := `{
      "smartctl": {"exit_status": 2, "version": [7, 4], "messages": [` + msg + `]},
      "local_time": {"time_t": 0}
    }`

	h := evaluateSmart(out, statusBitOpenFailed, "", fixedNow)
	if h.Status != HealthSleeping {
		t.Fatalf("Status = %q，想要 %q（details: %v）", h.Status, HealthSleeping, deref(h.Details))
	}
	if h.Details == nil || !strings.Contains(*h.Details, "待机") {
		t.Errorf("Details = %v，要写清是待机跳过", deref(h.Details))
	}
	if h.TemperatureCelsius != nil {
		t.Errorf("待机时不可能有温度读数")
	}

	// 同样的退出状态但没有待机线索：这是“打不开设备”，必须是 unknown。
	broken := `{"smartctl": {"exit_status": 2}, "device": {"name": "/dev/sda", "type": "ata"}}`
	h = evaluateSmart(broken, statusBitOpenFailed, "Smartctl open device: /dev/sda failed: No such device", fixedNow)
	if h.Status != HealthUnknown {
		t.Errorf("没有待机线索时 Status = %q，想要 %q", h.Status, HealthUnknown)
	}
}

// TestEvaluateSmartUnparsableOutput 校验坏输出：不能当成健康，也不能只剩一个空的 unknown。
func TestEvaluateSmartUnparsableOutput(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		exit   int
		stderr string
		reason string
	}{
		{"空输出成功退出", "", 0, "", "没有可解析的 JSON"},
		{"截断的 JSON", `{"smartctl": {"exit_stat`, 0, "", "没有可解析的 JSON"},
		{"完全不是 JSON", "smartctl 7.4 2023-08-01", 0, "", "没有可解析的 JSON"},
		{"打不开设备", "", statusBitOpenFailed, "Smartctl open device: /dev/sdb failed: Permission denied", "无法打开设备"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := evaluateSmart(tc.stdout, tc.exit, tc.stderr, fixedNow)
			if h.Status != HealthUnknown {
				t.Fatalf("Status = %q，想要 %q", h.Status, HealthUnknown)
			}
			if h.Details == nil || !strings.Contains(*h.Details, tc.reason) {
				t.Errorf("Details = %v，应包含 %q", deref(h.Details), tc.reason)
			}
			if h.TemperatureCelsius != nil {
				t.Errorf("解析失败时温度必须是 null")
			}
		})
	}
}

// TestEvaluateSmartEmptyArrays 校验空数组：ATA 属性表为空、NVMe 命名空间为空、
// messages 为空，都是合法的 smartctl 输出，不能因为“列表是空的”就解析失败。
func TestEvaluateSmartEmptyArrays(t *testing.T) {
	out := `{
      "smartctl": {"exit_status": 0, "messages": []},
      "device": {"name": "/dev/sda", "type": "sat", "protocol": "ATA"},
      "smart_support": {"available": true, "enabled": true},
      "smart_status": {"passed": true},
      "ata_smart_attributes": {"table": []},
      "ata_device_statistics": {"pages": []},
      "nvme_namespaces": [],
      "temperature": {"current": 33}
    }`
	h := evaluateSmart(out, 0, "", fixedNow)
	if h.Status != HealthHealthy {
		t.Fatalf("Status = %q，想要 %q（details: %v）", h.Status, HealthHealthy, deref(h.Details))
	}
	if h.TemperatureCelsius == nil || *h.TemperatureCelsius != 33 {
		t.Errorf("温度 = %v，想要 33", h.TemperatureCelsius)
	}
}

// TestEvaluateSmartEmptyJSONObject 空对象是合法 JSON 但没有信息：
// 结论必须是 unknown，不能因为“解析成功”就当成 healthy。
func TestEvaluateSmartEmptyJSONObject(t *testing.T) {
	h := evaluateSmart(`{}`, 0, "", fixedNow)
	if h.Status != HealthUnknown {
		t.Fatalf("Status = %q，想要 %q", h.Status, HealthUnknown)
	}
	if h.Details == nil || !strings.Contains(*h.Details, "没有给出总体健康结论") {
		t.Errorf("Details = %v，要说明为什么不知道", deref(h.Details))
	}
}

// TestEvaluateSmartExitStatusMismatch 记录进程退出码与 JSON 不一致的情况，
// 并以进程退出码为准：JSON 可能被截断，退出码是内核直接观察到的。
func TestEvaluateSmartExitStatusMismatch(t *testing.T) {
	out := synthetic(0, `"smart_status": {"passed": true}`)
	h := evaluateSmart(out, 0, "", fixedNow)
	if h.Details != nil && strings.Contains(*h.Details, "不一致") {
		t.Fatalf("一致时不该提不一致: %v", *h.Details)
	}

	mismatch := synthetic(0, `"smart_status": {"passed": true}`)
	h = evaluateSmart(mismatch, statusBitPrefailNow, "", fixedNow)
	if h.Status != HealthCritical {
		t.Errorf("Status = %q，进程退出状态说预失效就必须 critical", h.Status)
	}
	if h.Details == nil || !strings.Contains(*h.Details, "不一致") {
		t.Errorf("Details = %v，应以进程退出状态为准并说明", deref(h.Details))
	}
}

// TestTemperatureNeverZero 是 CONTRACTS §6 的一条硬规则的正面用例：
// 读不到温度就是 null。0 ℃ 会被当成“盘很凉”，实际上是“没有读数”。
func TestTemperatureNeverZero(t *testing.T) {
	cases := []struct {
		name string
		json string
		want *float64
	}{
		{"current 为 0", `"temperature": {"current": 0}`, nil},
		{"current 为负", `"temperature": {"current": -5}`, nil},
		{"没有 temperature 字段", `"model_name": "X"`, nil},
		{"正常温度", `"temperature": {"current": 38}`, f64(38)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := `{"smartctl": {"exit_status": 0}, ` + tc.json + `}`
			h := evaluateSmart(out, 0, "", fixedNow)
			switch {
			case tc.want == nil && h.TemperatureCelsius != nil:
				t.Errorf("温度 = %v，想要 null", *h.TemperatureCelsius)
			case tc.want != nil && h.TemperatureCelsius == nil:
				t.Errorf("温度 = null，想要 %v", *tc.want)
			case tc.want != nil && *h.TemperatureCelsius != *tc.want:
				t.Errorf("温度 = %v，想要 %v", *h.TemperatureCelsius, *tc.want)
			}
		})
	}
}

// fakeRunner 记录被调用时收到的参数，并按预设返回。
type fakeRunner struct {
	calls   int
	specs   []syscmd.Spec
	limits  []syscmd.Limits
	result  syscmd.Result
	err     error
	onCall  func()
	sleepCh chan struct{}
}

func (f *fakeRunner) run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	f.calls++
	f.specs = append(f.specs, spec)
	f.limits = append(f.limits, limits)
	if f.onCall != nil {
		f.onCall()
	}
	return f.result, f.err
}

// TestSmartProberSleepingDeviceNoSpawn 是“睡眠盘不唤醒”的核心用例：
// sysfs 说设备在 suspended 时，**根本不启动 smartctl**。
//
// 这是唯一能保证不唤醒的位置：设备文件一旦被打开，盘就转起来了。
func TestSmartProberSleepingDeviceNoSpawn(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "device", "power"), 0o755); err != nil {
		t.Fatalf("准备 sysfs 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device", "power", "runtime_status"),
		[]byte("suspended\n"), 0o644); err != nil {
		t.Fatalf("写 runtime_status 失败: %v", err)
	}

	runner := &fakeRunner{result: syscmd.Result{ExitCode: 0, Stdout: smartFixture(t, "upanel-smart-sda-scsi.json")}}
	prober := &SmartProber{Path: "/usr/sbin/smartctl", Run: runner.run}

	h := prober.Probe(context.Background(), "sdb", dir, "scsi")
	if h.Status != HealthSleeping {
		t.Fatalf("Status = %q，想要 %q（details: %v）", h.Status, HealthSleeping, deref(h.Details))
	}
	if h.Source != "sysfs" {
		t.Errorf("Source = %q，这个结论来自 sysfs 而不是 smartctl", h.Source)
	}
	if runner.calls != 0 {
		t.Errorf("休眠设备上仍然启动了 smartctl（%d 次）：盘点被唤醒了", runner.calls)
	}
}

// TestSmartProberArgs 校验调用参数：固定 JSON + 身份 + 健康 + 属性，
// 非 NVMe 额外带 -n standby，设备路径是 /dev/<内核名>。
func TestSmartProberArgs(t *testing.T) {
	runner := &fakeRunner{result: syscmd.Result{ExitCode: 0, Stdout: smartFixture(t, "upanel-smart-sda-scsi.json")}}
	prober := &SmartProber{Path: "/usr/sbin/smartctl", Run: runner.run}

	prober.Probe(context.Background(), "sda", t.TempDir(), "scsi")
	if runner.calls != 1 {
		t.Fatalf("调用次数 = %d，想要 1", runner.calls)
	}
	args := runner.specs[0].Args
	if runner.specs[0].Program != "/usr/sbin/smartctl" {
		t.Errorf("Program = %q，想要绝对路径", runner.specs[0].Program)
	}
	want := []string{"-j", "-i", "-H", "-A", "-n", "standby", "/dev/sda"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v，想要 %v", args, want)
	}

	// NVMe 不支持 -n standby，带上会让 smartctl 报参数错误。
	runner2 := &fakeRunner{result: syscmd.Result{ExitCode: 0, Stdout: smartFixture(t, "upanel-smart-nvme0n1.json")}}
	prober2 := &SmartProber{Path: "/usr/sbin/smartctl", Run: runner2.run}
	if h := prober2.Probe(context.Background(), "nvme0n1", t.TempDir(), "nvme"); h.Status != HealthHealthy {
		t.Fatalf("NVMe 真实采集应判 healthy，得到 %q", h.Status)
	}
	for _, a := range runner2.specs[0].Args {
		if a == "-n" || a == "standby" {
			t.Errorf("NVMe 的调用里不该有 -n standby：%v", runner2.specs[0].Args)
		}
	}
	if got := runner2.limits[0].Timeout; got != DefaultSmartTimeout {
		t.Errorf("Timeout = %v，想要默认 %v", got, DefaultSmartTimeout)
	}
}

// TestSmartProberTimeout 校验超时结论：unknown 且说明卡住，不是 healthy。
func TestSmartProberTimeout(t *testing.T) {
	runner := &fakeRunner{err: syscmd.ErrTimeout, result: syscmd.Result{ExitCode: -1, TimedOut: true}}
	prober := &SmartProber{Path: "/usr/sbin/smartctl", Timeout: 2 * time.Second, Run: runner.run}

	h := prober.Probe(context.Background(), "sda", t.TempDir(), "scsi")
	if h.Status != HealthUnknown {
		t.Fatalf("Status = %q，想要 %q", h.Status, HealthUnknown)
	}
	if h.Details == nil || !strings.Contains(*h.Details, "未返回") {
		t.Errorf("Details = %v，要说明超时", deref(h.Details))
	}
	if h.TemperatureCelsius != nil {
		t.Errorf("超时时温度必须是 null")
	}
}

// TestSmartProberLaunchFailure：连 smartctl 都没启动（路径不对、权限不足），
// 这是采集失败，要如实说明原因。
func TestSmartProberLaunchFailure(t *testing.T) {
	runner := &fakeRunner{err: os.ErrNotExist}
	prober := &SmartProber{Path: "/usr/sbin/smartctl", Run: runner.run}

	h := prober.Probe(context.Background(), "sda", t.TempDir(), "scsi")
	if h.Status != HealthUnknown {
		t.Fatalf("Status = %q，想要 %q", h.Status, HealthUnknown)
	}
	if h.Details == nil || !strings.Contains(*h.Details, "启动失败") {
		t.Errorf("Details = %v，要说明启动失败", deref(h.Details))
	}
}

// TestSmartProberNoPath：没有 smartctl 时不许假装健康。
func TestSmartProberNoPath(t *testing.T) {
	runner := &fakeRunner{}
	prober := &SmartProber{Run: runner.run}
	h := prober.Probe(context.Background(), "sda", t.TempDir(), "scsi")
	if h.Status != HealthUnknown {
		t.Errorf("Status = %q，想要 %q", h.Status, HealthUnknown)
	}
	if runner.calls != 0 {
		t.Errorf("没有路径却调用了执行入口")
	}

	var nilProber *SmartProber
	if h := nilProber.Probe(context.Background(), "sda", t.TempDir(), "scsi"); h.Status != HealthUnknown {
		t.Errorf("nil prober 的 Status = %q，想要 %q", h.Status, HealthUnknown)
	}
}

// TestFindSmartctlConfiguredPath 校验“显式配置就按配置来”：
// 配错了要说找不到，不能悄悄退回别的路径。
func TestFindSmartctlConfiguredPath(t *testing.T) {
	// isExecutableFile 靠 POSIX 权限位判断，Windows 上所有文件都是 0666，
	// 这条用例在开发宿主上必然失败。Windows 不是部署目标，标为未执行而不是放宽断言。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX 执行位，本条在 Linux 验收机上执行（PROGRESS 记为 NOT RUN on host）")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "smartctl")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写假 smartctl 失败: %v", err)
	}

	if got, ok := FindSmartctl(exe); !ok || got != exe {
		t.Errorf("FindSmartctl(%q) = %q,%v，想要原样返回", exe, got, ok)
	}

	missing := filepath.Join(dir, "nope")
	if got, ok := FindSmartctl(missing); ok {
		t.Errorf("配置了不存在的路径却找到了 %q", got)
	}

	// 没有执行权限的文件不算找到：smartctl 必须以可执行文件存在。
	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, ok := FindSmartctl(noexec); ok {
		t.Errorf("没有执行权限的文件被当成了 smartctl")
	}

	// 目录也不算。
	if _, ok := FindSmartctl(dir); ok {
		t.Errorf("目录被当成了 smartctl")
	}
}

// TestSleepingSysfsStates 校验 sysfs 里几种“别碰这块盘”的表示。
func TestSleepingSysfsStates(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		value string
		want  bool
	}{
		{"power-status suspended", "device/power-status", "suspended\n", true},
		{"runtime_status suspended", "device/power/runtime_status", "suspended\n", true},
		{"device state standby", "device/state", "standby\n", true},
		{"device state running", "device/state", "running\n", false},
		{"runtime_status active", "device/power/runtime_status", "active\n", false},
		{"什么都不存在", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				full := filepath.Join(dir, filepath.FromSlash(tc.file))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("准备目录失败: %v", err)
				}
				if err := os.WriteFile(full, []byte(tc.value), 0o644); err != nil {
					t.Fatalf("写文件失败: %v", err)
				}
			}
			if got := sleeping(dir); got != tc.want {
				t.Errorf("sleeping() = %v，想要 %v", got, tc.want)
			}
		})
	}
}

// TestFirstLine 让 Details 保持一行：smartctl 的 stderr 可能很长。
func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":                              "无输出",
		"one line":                      "one line",
		"first\nsecond":                 "first",
		"   padded   ":                  "padded",
		strings.Repeat("x", 250) + "\n": strings.Repeat("x", 200) + "…",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// TestIsNetworkFS 校验网络文件系统的判定：按 fstype，不按路径写法。
func TestIsNetworkFS(t *testing.T) {
	network := []string{"nfs", "nfs4", "cifs", "smb3", "smbfs", "fuse.sshfs", "sshfs", "9p", "ceph", "glusterfs", "afs", "davfs", "NFS4"}
	for _, fs := range network {
		if !IsNetworkFS(fs) {
			t.Errorf("IsNetworkFS(%q) = false，想要 true", fs)
		}
	}
	local := []string{"ext4", "xfs", "btrfs", "vfat", "f2fs", "overlay", "tmpfs", "", "zfs"}
	for _, fs := range local {
		if IsNetworkFS(fs) {
			t.Errorf("IsNetworkFS(%q) = true，本地文件系统不该走隔离通道", fs)
		}
	}
}

// ---- 小工具 ----

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func f64(v float64) *float64 { return &v }
