package storage

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// scanVolumeHolders 把“打开着这块设备”和“打开着这个挂载点上任何文件”的进程并起来。
//
// 为什么两个都要问：umount 撞上的 EBUSY 来自“这个文件系统上还有打开的文件”，
// 而 dd/mkfs/lvm 这类直接开着块设备的进程反过来不会被 umount 拦（它拦的是文件系统）。
// 两种占用都会让“现在卸载”变成一句空话，因此判决与占用列表必须同时看到它们。
//
// 任何一个扫描失败都返回错误而不是空列表：在确认不了的地方说“没人用”，
// 会让人按下卸载、然后在 umount 的半途看到一句 target is busy。
func scanVolumeHolders(ctx context.Context, devID string, mountPoints []string, scan BusyScanner) ([]BusyProcess, error) {
	hits := map[string]BusyProcess{}
	collect := func(procs []BusyProcess) {
		for _, p := range procs {
			if _, ok := hits[p.PID]; !ok {
				hits[p.PID] = p
			}
		}
	}

	if devID != "" {
		procs, err := scan.Scan(ctx, devID)
		if err != nil {
			return nil, fmt.Errorf("检查 %s 的进程占用失败（无法确认没人在用）：%w", devID, err)
		}
		collect(procs)
	}
	for _, mp := range mountPoints {
		procs, err := scan.ScanMount(ctx, mp)
		if err != nil {
			return nil, fmt.Errorf("检查 %s 上的打开文件失败（无法确认没人在用）：%w", mp, err)
		}
		collect(procs)
	}

	out := make([]BusyProcess, 0, len(hits))
	for _, p := range hits {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].PID)
		b, _ := strconv.Atoi(out[j].PID)
		if a != b {
			return a < b
		}
		return out[i].PID < out[j].PID
	})
	return out, nil
}

// holdersTarget 是提示文本里“谁在用”的那个名字：优先挂载点（人认路径），
// 没有挂载点时才说设备名。
func holdersTarget(devID string, mountPoints []string) string {
	if len(mountPoints) > 0 {
		return strings.Join(mountPoints, "、")
	}
	if devID != "" {
		return devID
	}
	return "这个卷"
}
