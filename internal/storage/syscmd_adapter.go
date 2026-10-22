package storage

// 这一层把 syscmd 接到写原语上。
//
// 之所以单独放一个文件、而不是在写原语里直接调用 syscmd.Run：写路径要能被单元测试
// 驱动（Windows 开发机上既没有 /dev/sdb 也没有 sfdisk），而 syscmd 的契约是
// “绝对路径 + argv + 固定环境 + 有上限的输出”。中间隔一层接口，测试就能喂进
// 一段记录下来的命令序列，生产装配仍然只有这一条真实路径。

import (
	"context"

	"upanel/internal/syscmd"
)

// SyscmdRunner 返回生产用的命令执行器：直接转给 syscmd.Run。
//
// syscmd.Run 自身已经保证了：不经 shell（argv 逐项传递）、环境变量白名单、
// 标准输入只能是代码里的固定模板、输出有上限、超时连同子进程组一起终止。
func SyscmdRunner() CommandRunner {
	return CommandRunnerFunc(func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
		return syscmd.Run(ctx, spec, limits)
	})
}

// CheckWriteDependencies 检查本机是否具备磁盘写原语的固定依赖
// （sfdisk、udevadm、mkfs.ext4、mkfs.xfs）。
//
// 装配时调用一次：缺依赖不该阻止代理启动（拓扑与预检仍然有用），但启动日志里必须
// 说清楚哪几个缺，否则管理员只会在第一次点按钮时才发现。
func CheckWriteDependencies() error {
	return newWriter(WriteOptions{}).requireAllPrograms()
}
