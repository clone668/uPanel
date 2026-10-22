package shares

// Samba 服务状态的探测（P08.2）。
//
// 为什么单独一个文件：这个判断被两处用到——能力探针要说“配置写好了但服务没跑”，
// 写配置的流水线要说“重载有没有意义、重载之后还在不在”。两处如果各自实现一遍，
// 迟早会出现“能力页说 active、执行结果说 inactive”这种自相矛盾。

import (
	"context"
	"errors"
	"strings"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

const (
	// validateTimeout 是一次 testparm 的预算。真机上满载配置的校验是毫秒级，
	// 20 秒足够覆盖磁盘很慢的情形，又不至于让一次点击卡住页面半分钟。
	validateTimeout = 20 * time.Second
	// reloadTimeout 是一次 smbcontrol 的预算。
	reloadTimeout = 20 * time.Second
	// serviceProbeTimeout 是一次 systemctl 的预算：只读一个 unit 属性，
	// 超过 10 秒说明 systemd 那边已经不健康了。
	serviceProbeTimeout = 10 * time.Second
)

// serviceUnits 是我们关心的两个 unit。
//
// 两个都要问：socket 激活的部署里 smbd 进程是按连接起来的，只看 smbd.service 会把
// “能提供服务”误判成“没在服务”，于是明明生效了的配置被判成 served=false。
var serviceUnits = []string{"smbd.service", "smbd.socket"}

// serviceStateFrom 把一次 `systemctl is-active` 的结果翻译成闭集里的一个值。
//
// `is-active` 对未激活的 unit 以退出码 3 结束，所以**退出码不能用来判断**——
// 看的是每一行打印出来的取值。只有 systemctl 本身跑不起来（非 ExitError 的错误、
// 或者一行都没认出来）才是 unknown：那种情况下“服务在不在”是真的不知道，
// 而不是“没在跑”。
func serviceStateFrom(res syscmd.Result, err error) string {
	// 先看错误：超时或没跑起来的时候，可能已经打印了半行 "active"——那不是证据。
	// 非零退出是正常业务结果（未激活的 unit 就是这样），不算错误。
	var exitErr *syscmd.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return ServiceStateUnknown
	}
	combined := res.Stdout
	anyKnown, active := false, false
	for _, line := range strings.Split(combined, "\n") {
		switch strings.TrimSpace(line) {
		case "active", "reloading":
			active = true
			anyKnown = true
		case "inactive", "failed", "activating", "deactivating", "unknown", "maintenance":
			anyKnown = true
		}
	}
	if active {
		return ServiceStateActive
	}
	if !anyKnown {
		return ServiceStateUnknown
	}
	return ServiceStateInactive
}

// probeServiceState 问一次 smbd 的运行状态。
func probeServiceState(ctx context.Context, runner CommandRunner) string {
	if runner == nil {
		return ServiceStateUnknown
	}
	args := append([]string{"is-active"}, serviceUnits...)
	res, err := runner.Run(ctx, syscmd.Spec{
		Program: storage.DefaultSystemctlPath,
		Args:    args,
	}, syscmd.Limits{Timeout: serviceProbeTimeout})
	return serviceStateFrom(res, err)
}
