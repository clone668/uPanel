package storage

// 从真实设备读取签名（P07.1）。
//
// 与分区表探测同一套纪律：只读、带超时、不唤醒休眠设备。读失败返回错误，
// 由判决层记成阻塞项（SIGNATURE_UNREADABLE）——读不出来不等于盘是空的。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// DeviceSignatureProber 从设备节点读取头部与尾部字节并识别签名。
//
// Dev 与 Sys 可以指向夹具目录（单元测试就是这么做的：把一个临时文件当作设备），
// 因此这里不使用任何需要 root 的接口，只做 os.Open + ReadAt。
type DeviceSignatureProber struct {
	// Dev 是设备目录（真实值 /dev）。
	Dev string
	// Sys 是 sysfs 根（真实值 /sys），只用来判断设备是否在休眠。空串表示不判断。
	Sys string
	// Timeout 是单次读取的超时。0 时用 DefaultDiskReadTimeout。
	Timeout time.Duration
}

// SystemSignatureProber 返回指向真实设备的签名读取器。
func SystemSignatureProber() SignatureProber {
	return DeviceSignatureProber{Dev: "/dev", Sys: "/sys", Timeout: DefaultDiskReadTimeout}
}

// Probe 读取设备的头尾字节并识别签名。
func (p DeviceSignatureProber) Probe(ctx context.Context, devID string, sizeBytes uint64) (Signature, error) {
	if devID == "" {
		return Signature{}, errors.New("storage: 设备名为空")
	}
	if p.Dev == "" {
		return Signature{}, errors.New("storage: 没有配置设备目录")
	}
	// 读 /dev 会唤醒休眠的盘（PLAN.md 第 8 节不允许）。判断放在打开之前。
	if p.Sys != "" {
		if dir := deviceSysDir(p.Sys, devID); dir != "" && sleeping(dir) {
			return Signature{}, fmt.Errorf("storage: %s 处于休眠，未读取签名（不强制唤醒磁盘）", devID)
		}
	}

	path := filepath.Join(p.Dev, devID)
	f, err := os.Open(path)
	if err != nil {
		return Signature{}, fmt.Errorf("storage: 打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	size := sizeBytes
	if size == 0 {
		fi, err := f.Stat()
		if err != nil {
			return Signature{}, fmt.Errorf("storage: 取 %s 的大小失败: %w", path, err)
		}
		if fi.Mode().IsRegular() {
			size = uint64(fi.Size())
		}
	}
	if size < 512 {
		return Signature{}, fmt.Errorf("storage: %s 只有 %d 字节，不足一个扇区", devID, size)
	}

	headLen := uint64(headProbeBytes)
	if size < headLen {
		headLen = size
	}
	head := make([]byte, headLen)
	if _, err := readAtTimeout(ctx, f, head, 0, p.timeout()); err != nil {
		return Signature{}, fmt.Errorf("storage: 读 %s 头部失败: %w", devID, err)
	}

	tailOff := uint64(0)
	if size > tailProbeBytes {
		tailOff = size - tailProbeBytes
	}
	// 尾部窗口完全落在头部窗口里就没有必要再读一次。
	var tail []byte
	if tailOff >= headLen {
		tail = make([]byte, size-tailOff)
		if _, err := readAtTimeout(ctx, f, tail, int64(tailOff), p.timeout()); err != nil {
			// 尾部读不到不是“没有签名”：交换区与 md 1.0 就在那里。
			return Signature{}, fmt.Errorf("storage: 读 %s 尾部失败: %w", devID, err)
		}
	} else {
		tail = head[tailOff:]
	}

	return classifySignature(head, tail, size), nil
}

func (p DeviceSignatureProber) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultDiskReadTimeout
}

// readAtTimeout 在超时或取消时返回错误。
//
// 超时后关闭句柄：正常读取是毫秒级，超时说明盘在睡眠或链路卡住。如果内核把线程挂在
// 不可中断的 I/O 上，关闭句柄也解不开——那种情况会留下一个 goroutine，但调用方已经
// 拿到错误并继续，判决会按“读不出来”拒绝（Q169 的教训：不要指望 close 回收阻塞中的 fd，
// 要指望自己不再等它）。
func readAtTimeout(ctx context.Context, f *os.File, buf []byte, off int64, timeout time.Duration) (int, error) {
	// 已经取消的 context 先判一次，不进 goroutine。
	//
	// 不判的话这里是一场抛硬币：`f.ReadAt` 对一个普通文件是微秒级，调用方在
	// `cancel()` 之后立刻进来，读很可能在 select 之前就完成了——那时 `ctx.Done()`
	// 与 `done` **同时就绪**，Go 的 select 均匀随机挑一个，于是同一份输入有时给出
	// 数据、有时给出 `context.Canceled`。宿主机满载时（`go test ./...` 并行编译）
	// 这个窗口会真的被撞上，`TestDeviceSuperblockProberCancelledContext` 就有过一次
	// 红。已经取消的调用不该再去读盘，也不该把"读到了"当成成功。
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := f.ReadAt(buf, off)
		done <- result{n: n, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		_ = f.Close()
		return 0, ctx.Err()
	case <-timer.C:
		_ = f.Close()
		return 0, ErrDiskReadTimeout
	case res := <-done:
		// 读到文件尾是正常情况：设备可能比探测窗口短（小分区），
		// 这时 buf 里已经填好实际读到的字节，其余保持零。
		if res.err != nil && !errors.Is(res.err, io.EOF) {
			return res.n, res.err
		}
		return res.n, nil
	}
}

// deviceSysDir 找一台设备在 sysfs 里的目录。
//
// 顶层设备是 /sys/block/<name>；分区的目录嵌在整盘下面（/sys/block/sda/sda1），
// 不能由名字拼出来，所以要扫一层。找不到返回空串（调用方按“不知道是否休眠”处理）。
func deviceSysDir(sysRoot, devID string) string {
	if sysRoot == "" || devID == "" {
		return ""
	}
	direct := joinPath(sysRoot, "block", devID)
	if st, err := os.Stat(direct); err == nil && st.IsDir() {
		return direct
	}
	for _, disk := range dirNames(joinPath(sysRoot, "block")) {
		candidate := joinPath(sysRoot, "block", disk, devID)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	return ""
}
