// SMART / NVMe 健康采集（P04.3）。
//
// 三条硬规则直接来自 PLAN.md 第 8 节与 CONTRACTS.md 第 6 节：
//
//   - **SMART 不可用 ≠ 健康良好。** smartctl 说“这块盘没有 SMART”时状态是
//     unsupported，不是 healthy；读失败是 unknown。只有拿到明确的“通过”结论才是 healthy。
//   - **退出状态是位掩码。** smartctl 用位表示不同的问题（磁盘失效、预失效属性、
//     错误日志……），把所有非零退出都当成“命令失败”会丢掉最重要的告警信息。
//   - **不为读健康而唤醒磁盘。** 先看 sysfs 的电源状态，休眠就跳过；
//     非休眠时也带 `-n standby`，让 smartctl 在磁盘恰好转入待机时直接放弃而不是启动它。
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// smartctl 退出状态位（smartctl(8) 的 EXIT STATUS 一节）。
// 位的位置是契约的一部分：数值变了会把“磁盘失效”误读成“命令失败”。
const (
	statusBitCmdLine      = 1 << 0 // 1：命令行没解析成功（我们的调用参数有问题）
	statusBitOpenFailed   = 1 << 1 // 2：打开设备失败，或设备处于低功耗模式（-n 时）
	statusBitSmartCmdFail = 1 << 2 // 4：某条 SMART/ATA 命令失败，或数据结构校验和错误
	statusBitDiskFailing  = 1 << 3 // 8：SMART 自检结论是 DISK FAILING
	statusBitPrefailNow   = 1 << 4 // 16：有预失效属性当前低于阈值
	statusBitPrefailPast  = 1 << 5 // 32：有属性曾在某个时刻低于阈值
	statusBitErrorLog     = 1 << 6 // 64：设备错误日志里有记录
	statusBitSelfTestLog  = 1 << 7 // 128：自检日志里有失败记录
)

// smartHealthSource 是 health.source 的取值之一：数据来自 smartctl。
const smartHealthSource = "smartctl"

// smartctlCandidates 是 smartctl 的常规安装位置（按发行版惯例排序）。
//
// 只查固定目录，不用 PATH：代理是 root 服务，PATH 在这里不可控，
// 而“从环境里找一个 root 要执行的程序”本身就是一条提权路径。
var smartctlCandidates = []string{
	"/usr/sbin/smartctl",
	"/usr/bin/smartctl",
	"/sbin/smartctl",
	"/bin/smartctl",
	"/usr/local/sbin/smartctl",
	"/usr/local/bin/smartctl",
}

// FindSmartctl 找到可执行的 smartctl。
//
// configured 非空时只认这一个路径（运维显式指定了就按他说的来，找不到就是找不到，
// 不静默退回别的路径——那会让人以为配置生效了）。
func FindSmartctl(configured string) (string, bool) {
	if configured != "" {
		return configured, isExecutableFile(configured)
	}
	for _, path := range smartctlCandidates {
		if isExecutableFile(path) {
			return path, true
		}
	}
	return "", false
}

// isExecutableFile 判断路径是一个普通文件且有执行权限。
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// SmartProber 用 smartctl 读取一块设备的 SMART/NVMe 健康。
//
// 只按绝对路径调用 smartctl，逐项传参、不经过 shell（syscmd 的边界）。
// 设备名来自 sysfs 枚举，不是 HTTP 参数；路径固定为 /dev/<内核设备名>。
type SmartProber struct {
	// Path 是 smartctl 的绝对路径。
	Path string
	// Timeout 是单块设备的读取时限。SMART 查询会唤醒盘并等它回答，
	// 超过这个时间说明盘有问题或链路卡住，宁可报 unknown 也不挂着。
	Timeout time.Duration
	// Run 是执行入口，测试可以注入；nil 时用 syscmd.Run。
	Run func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// DefaultSmartTimeout 是单块设备 SMART 查询的默认时限。
const DefaultSmartTimeout = 15 * time.Second

// Probe 读取一台设备的健康。永远返回一个 Health：失败的原因写在 Details 里，
// 状态是 unknown 而不是 healthy —— 采集失败不能变成“一切正常”。
//
// name 是内核设备名（sda、nvme0n1），sysfsDir 是它在 sysfs 里的目录，
// transport 来自 sysfs 的真实路径（nvme/scsi/…）。
func (p *SmartProber) Probe(ctx context.Context, name, sysfsDir, transport string) Health {
	now := time.Now().UTC()

	if p == nil || p.Path == "" {
		return Health{
			Status:    HealthUnknown,
			Source:    "none",
			CheckedAt: nil,
			Details:   strPtr("未找到 smartctl（未安装 smartmontools），无法读取 SMART"),
		}
	}
	// 第一道闸：sysfs 说盘在休眠就根本不启动 smartctl。
	// 这是唯一能保证“不唤醒”的位置——一旦设备文件被打开，盘就已经转起来了。
	if sleeping(sysfsDir) {
		return Health{
			Status:    HealthSleeping,
			Source:    "sysfs",
			CheckedAt: &now,
			Details:   strPtr("设备处于低功耗状态，已跳过 SMART 查询（不唤醒磁盘）"),
		}
	}

	args := []string{"-j", "-i", "-H", "-A"}
	// 第二道闸：smartctl 自己检查电源状态。设备在两次检查之间转入待机时，
	// 它直接以退出状态 2 返回，不会把盘叫醒。NVMe 不支持这个开关。
	if transport != "nvme" {
		args = append(args, "-n", "standby")
	}
	args = append(args, "/dev/"+name)

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultSmartTimeout
	}
	run := p.Run
	if run == nil {
		run = syscmd.Run
	}
	res, err := run(ctx, syscmd.Spec{Program: p.Path, Args: args}, syscmd.Limits{Timeout: timeout})
	if errors.Is(err, syscmd.ErrTimeout) {
		return Health{
			Status:    HealthUnknown,
			Source:    smartHealthSource,
			CheckedAt: &now,
			Details:   strPtr(fmt.Sprintf("smartctl 超过 %s 未返回，已终止（设备可能卡在 I/O 上）", timeout)),
		}
	}
	if err != nil {
		var exitErr *syscmd.ExitError
		if !errors.As(err, &exitErr) {
			// 连启动都没成功（找不到文件、权限被拒）：这是采集失败，不是设备状态。
			return Health{
				Status:    HealthUnknown,
				Source:    smartHealthSource,
				CheckedAt: &now,
				Details:   strPtr("smartctl 启动失败：" + firstLine(err.Error())),
			}
		}
		// 非零退出是**结果**，交给位掩码解释，不在这里当成错误。
	}

	health := evaluateSmart(res.Stdout, res.ExitCode, res.Stderr, now)
	return health
}

// smartReport 是 smartctl `-j` 输出里本阶段用到的字段。
// 未列出的字段一律忽略：smartctl 的小版本会加字段，解析不能因为多一个键就失败。
type smartReport struct {
	Smartctl struct {
		ExitStatus int   `json:"exit_status"`
		Version    []int `json:"version"`
		Messages   []struct {
			String   string `json:"string"`
			Severity string `json:"severity"`
		} `json:"messages"`
	} `json:"smartctl"`

	Device struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Protocol string `json:"protocol"`
	} `json:"device"`

	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`

	SmartSupport *struct {
		Available bool `json:"available"`
		Enabled   bool `json:"enabled"`
	} `json:"smart_support"`

	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`

	// Temperature 是 smartctl 归一化后的摄氏温度（ATA/NVMe/SCSI 都走这里）。
	Temperature *struct {
		Current   int `json:"current"`
		DriveTrip int `json:"drive_trip"`
	} `json:"temperature"`

	ATASmartAttributes *struct {
		Table []struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			Value      int    `json:"value"`
			Worst      int    `json:"worst"`
			Thresh     int    `json:"thresh"`
			WhenFailed string `json:"when_failed"`
			Raw        struct {
				Value  int64  `json:"value"`
				String string `json:"string"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`

	NVMeLog *struct {
		CriticalWarning         int   `json:"critical_warning"`
		Temperature             int   `json:"temperature"`
		AvailableSpare          int   `json:"available_spare"`
		AvailableSpareThreshold int   `json:"available_spare_threshold"`
		PercentageUsed          int   `json:"percentage_used"`
		MediaErrors             int64 `json:"media_errors"`
		NumErrLogEntries        int64 `json:"num_err_log_entries"`
	} `json:"nvme_smart_health_information_log"`
}

// NVMe 健康日志 Critical Warning 位的严重程度（NVMe 规范 1.4 第 5.14.1.2 节）。
// 温度越界、备用块不足是可恢复告警；可靠性下降与只读模式已经不能再当作“正常”。
var nvmeCriticalWarnings = []struct {
	bit    int
	label  string
	severe bool
}{
	{0, "可用备用块低于阈值", false},
	{1, "温度超出阈值", false},
	{2, "可靠性下降", true},
	{3, "已进入只读模式", true},
	{4, "易失存储备份失败", false},
}

// evaluateSmart 把一次 smartctl 调用的输出翻译成 Health。
//
// stdout 应为 JSON；解析失败时退回“看退出状态”，并把 stderr 的第一行写进 Details，
// 这样界面能显示“为什么不知道”，而不是一个没有解释的 unknown。
func evaluateSmart(stdout string, exitCode int, stderr string, now time.Time) Health {
	h := Health{
		Status:    HealthUnknown,
		Source:    smartHealthSource,
		CheckedAt: &now,
	}

	var report smartReport
	parseErr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &report)
	if parseErr != nil || strings.TrimSpace(stdout) == "" {
		// 没有可解析的输出：只能依据退出状态说话。
		switch {
		case exitCode&statusBitOpenFailed != 0:
			h.Details = strPtr("smartctl 无法打开设备（退出状态 " + fmt.Sprint(exitCode) + "）：" + firstLine(stderr))
		case exitCode == 0:
			h.Details = strPtr("smartctl 返回成功但没有可解析的 JSON，健康状态未知")
		default:
			h.Details = strPtr("smartctl 输出无法解析（退出状态 " + fmt.Sprint(exitCode) + "）：" + firstLine(stderr))
		}
		return h
	}

	// 进程退出码与 JSON 里的 exit_status 应当一致；不一致时以进程退出码为准，
	// 因为它是内核直接观察到的结果，而 JSON 可能被截断。这条说明必须保留到最终
	// Details 里，不能被后面的分类理由覆盖掉。
	mismatch := ""
	if report.Smartctl.ExitStatus != exitCode {
		mismatch = "smartctl 进程退出状态 " + fmt.Sprint(exitCode) +
			" 与 JSON 报告的 " + fmt.Sprint(report.Smartctl.ExitStatus) + " 不一致，以进程退出状态为准"
	}

	// 1) 待机：带 -n standby 时 smartctl 以退出状态 2 返回，且没有设备信息。
	if isStandby(report, exitCode) {
		h.Status = HealthSleeping
		h.Details = strPtr("smartctl 报告设备处于待机/休眠，未唤醒（-n standby）")
		return h
	}

	// 2) 设备不支持 SMART：这是“没有这个功能”，不是“功能坏了”，也不是“健康”。
	if report.SmartSupport != nil && !report.SmartSupport.Available {
		h.Status = HealthUnsupported
		h.Details = strPtr("设备不支持 SMART（smartctl: smart_support.available=false）")
		return h
	}

	temp := temperatureOf(report)
	h.TemperatureCelsius = temp

	status, reasons := classifySmart(report, exitCode)
	if mismatch != "" {
		reasons = append([]string{mismatch}, reasons...)
	}
	h.Status = status
	if len(reasons) > 0 {
		h.Details = strPtr(strings.Join(reasons, "；"))
	}
	return h
}

// isStandby 判断这次调用是不是“因为设备在待机而没有读”。
//
// 只看退出状态不够：位 1 同时表示“打不开设备”。因此要求同时满足
// 退出状态位 1 置位、没有设备类型/型号信息、且 smartctl 的 message 里提到待机或低功耗。
func isStandby(report smartReport, exitCode int) bool {
	if exitCode&statusBitOpenFailed == 0 {
		return false
	}
	if report.Device.Type != "" || report.ModelName != "" || report.SmartStatus != nil {
		return false
	}
	for _, m := range report.Smartctl.Messages {
		s := strings.ToLower(m.String)
		if strings.Contains(s, "standby") || strings.Contains(s, "sleep") ||
			strings.Contains(s, "low-power") || strings.Contains(s, "low power") {
			return true
		}
	}
	return false
}

// temperatureOf 取摄氏温度。
//
// smartctl 对不支持温度读数的设备会给出 current=0。0 °C 对一块正在工作的盘是不可能的，
// 把它当读数写进界面比写 null 更糟——因此 <=0 一律视为“没有读数”。
func temperatureOf(report smartReport) *float64 {
	if report.Temperature == nil || report.Temperature.Current <= 0 {
		return nil
	}
	v := float64(report.Temperature.Current)
	return &v
}

// classifySmart 给出状态与理由。顺序体现优先级：
// 磁盘失效 > 预失效属性 > 错误日志 > 明确的通过 > 无法判断。
func classifySmart(report smartReport, exitCode int) (string, []string) {
	var reasons []string
	critical := false
	warning := false

	// 命令行被拒绝时，后面的一切都不可信：smartctl 根本没按我们的意图去查。
	// 这种情况下哪怕 JSON 里带着一个 passed=true（可能是上一次的输出被残留），
	// 也不能报 healthy。
	if exitCode&statusBitCmdLine != 0 {
		return HealthUnknown, []string{"smartctl 命令行参数被拒绝（退出状态位 0），本次结果不可信"}
	}
	if exitCode&statusBitDiskFailing != 0 {
		critical = true
		reasons = append(reasons, "SMART 自检结论为 DISK FAILING")
	}
	if exitCode&statusBitPrefailNow != 0 {
		critical = true
		reasons = append(reasons, "有预失效属性当前低于阈值")
	}
	if report.SmartStatus != nil && !report.SmartStatus.Passed {
		critical = true
		reasons = append(reasons, "SMART 总体健康结论为未通过")
	}
	if exitCode&statusBitPrefailPast != 0 {
		warning = true
		reasons = append(reasons, "有属性曾在过去低于阈值")
	}
	if exitCode&statusBitErrorLog != 0 {
		warning = true
		reasons = append(reasons, "设备错误日志中有记录")
	}
	if exitCode&statusBitSelfTestLog != 0 {
		warning = true
		reasons = append(reasons, "自检日志中有失败记录")
	}

	// ATA 属性表：when_failed 非空说明内核/盘自身已经判定它掉出阈值。
	if report.ATASmartAttributes != nil {
		for _, a := range report.ATASmartAttributes.Table {
			if strings.TrimSpace(a.WhenFailed) != "" {
				warning = true
				reasons = append(reasons, fmt.Sprintf("属性 %s 曾在 %s 低于阈值", a.Name, a.WhenFailed))
			}
		}
	}

	// NVMe 健康日志：Critical Warning 的每一位置对应一类问题。
	if report.NVMeLog != nil {
		for _, w := range nvmeCriticalWarnings {
			if report.NVMeLog.CriticalWarning&(1<<uint(w.bit)) == 0 {
				continue
			}
			if w.severe {
				critical = true
			} else {
				warning = true
			}
			reasons = append(reasons, "NVMe 告警："+w.label)
		}
		if report.NVMeLog.MediaErrors > 0 {
			warning = true
			reasons = append(reasons, fmt.Sprintf("NVMe 介质错误 %d 条", report.NVMeLog.MediaErrors))
		}
		if report.NVMeLog.PercentageUsed >= 100 {
			warning = true
			reasons = append(reasons, fmt.Sprintf("NVMe 寿命已用 %d%%", report.NVMeLog.PercentageUsed))
		}
	}

	switch {
	case critical:
		return HealthCritical, reasons
	case warning:
		return HealthWarning, reasons
	}

	// 能读到 SMART 命令本身（位 1）、但某条命令失败了：这台设备的状态不可判断。
	// 注意不把这种情况算成 warning——warning 是“盘说它有问题”，这里是“我们没问出来”。
	if exitCode&statusBitOpenFailed != 0 {
		return HealthUnknown, append(reasons, "smartctl 无法打开设备或设备处于低功耗模式")
	}
	if exitCode&statusBitSmartCmdFail != 0 {
		return HealthUnknown, append(reasons, "SMART 命令执行失败或数据结构校验失败")
	}

	if report.SmartStatus != nil && report.SmartStatus.Passed {
		return HealthHealthy, append(reasons, "SMART 总体健康结论为通过")
	}
	if report.NVMeLog != nil {
		// NVMe 有健康日志且没有任何告警位：这本身就是可信的“通过”。
		return HealthHealthy, append(reasons, "NVMe 健康日志无告警位")
	}
	if len(reasons) > 0 {
		return HealthUnknown, reasons
	}
	return HealthUnknown, append(reasons, "该设备没有给出总体健康结论（SCSI 设备通常没有）")
}

// firstLine 取一段错误文本的第一行，并限制长度。
// smartctl 的 stderr 可能带设备路径与英文诊断，摘一行足够解释问题。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "无输出"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
