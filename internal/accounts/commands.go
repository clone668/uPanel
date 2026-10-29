package accounts

// 命令模板（P14.5）。
//
// 所有外部命令都在这里构造：绝对路径、argv 逐项、参数来自代码或已经过形状校验的
// 内部标识（账号名过 ValidateName，路径来自 Paths 与读回来的 home）。
// 这里**没有**任何把 HTTP 原始字符串当参数塞进 argv 的路径。
//
// 密码只出现在一处：chpasswd 的标准输入（passwordStdin）。argv 会出现在
// /proc/<pid>/cmdline 与 ps 里，日志会出现在日志文件里，数据库会长期留存——
// 这三处都不该有密码。

import (
	"time"

	"upanel/internal/syscmd"
)

// 固定工具路径。全部是常量：不接受配置、不来自 HTTP 参数。
const (
	ProgramGetent   = "/usr/bin/getent"
	ProgramPasswd   = "/usr/bin/passwd"
	ProgramUseradd  = "/usr/sbin/useradd"
	ProgramUsermod  = "/usr/sbin/usermod"
	ProgramUserdel  = "/usr/sbin/userdel"
	ProgramChpasswd = "/usr/sbin/chpasswd"
	ProgramVisudo   = "/usr/sbin/visudo"
	ProgramSudo     = "/usr/bin/sudo"
)

// 命令超时。
const (
	// commandTimeout 是写操作的预算。useradd/usermod 在慢盘上也就是几十毫秒，
	// 给 30 秒是给 NSS 后端留余量。
	commandTimeout = 30 * time.Second
	// lookupTimeout 是**预检**里那一次 getent 的上限。
	//
	// 预检跑在 Web 等着的这条路径上，整条链路的预算比执行短得多
	// （agentclient.DefaultTimeout 是 10 秒）。用 30 秒等于把“NSS 卡住”翻译成
	// “代理不可用”。预算用尽时**不是**报错：预检把“没问出结果”写成
	// ACCOUNT_LOOKUP_TIMEOUT 告警，由执行阶段的长预算复核兜底（与 P08.1 同一条
	// 结论，DECISIONS Q223）。
	lookupTimeout = 3 * time.Second
	// lookupExecTimeout 是**执行阶段**（任务里的执行前复核）同一次查询的总预算。
	//
	// 执行不在 Web 的等待路径上，用的是任务自己的预算，而且它才是“改不改系统”的
	// 最后一道门——它不该因为机器忙就把一次正确的操作判死（真机实测过 4.7—7.5 秒）。
	lookupExecTimeout = 20 * time.Second
)

// specGetentPasswd 构造 `getent passwd <name>`（只读）。
func specGetentPasswd(name string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramGetent, Args: []string{"passwd", name}}
}

// specGetentGroup 构造 `getent group <gid>`（只读）。
func specGetentGroup(gid string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramGetent, Args: []string{"group", gid}}
}

// specPasswdStatus 构造 `passwd -S <name>`（只读：读 shadow 的状态列）。
func specPasswdStatus(name string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramPasswd, Args: []string{"-S", name}}
}

// specUseradd 构造普通账号的创建命令。
//
// 每一个参数都在守一件事：
//   - --create-home：普通账号要能放 authorized_keys，所以要有 home；
//   - --shell：只能是闭集里的两档（nologin 或 rbash），不接受任意路径；
//   - --user-group：建一个同名组，避免落进 users 组而继承别人的组权限。
//
// **不给 --uid / --gid / --groups**：这三个数由系统分配，面板只读回。
// 能提交 uid 就等于能提交 uid=0（CONTRACTS.md 第 13 节）。
func specUseradd(name, shell string) syscmd.Spec {
	return syscmd.Spec{
		Program: ProgramUseradd,
		Args:    []string{"--create-home", "--shell", shell, "--user-group", name},
	}
}

// specUsermodShell 构造改壳的命令。
//
// 禁用、启用、切换受限/禁用三件事在系统上都只是“换一个壳”，所以只有一条模板。
// 用 usermod -L 锁密码而不是换壳的话，启用时无法把账号恢复到“有密码但没改过密码”
// 的状态（-U 对从未设过密码的账号会写一个空密码），而空密码比 nologin 危险得多。
func specUsermodShell(name, shell string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramUsermod, Args: []string{"--shell", shell, name}}
}

// specUserdel 构造删除命令。不加 -r：home 里的数据不归面板删（CONTRACTS.md 第 5 节）。
func specUserdel(name string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramUserdel, Args: []string{name}}
}

// specChpasswd 构造 `chpasswd`，密码经标准输入。
//
// 用 chpasswd 而不是 `passwd --stdin`：后者是 RHEL 系的选项，Debian/Ubuntu 的
// passwd 不认（真机上会以 usage 错误退出）。chpasswd 从标准输入读 `<名字>:<明文>`
// 行，是这个平台上唯一不需要 argv 也不需要交互终端的路径。
//
// **不传 -c**：那会把 crypt 方法钉成 SHA512，而这个平台的默认是 yescrypt
// （VM 实测 chpasswd --help 里 -c 只认 NONE/DES/MD5/SHA256/SHA512，没有 yescrypt）。
// 面板设密码不该比系统自己的 passwd 更弱，所以这里跟系统默认走。
func specChpasswd(stdin []byte) syscmd.Spec {
	return syscmd.Spec{Program: ProgramChpasswd, Args: nil, Stdin: stdin}
}

// specVisudoFile 构造 `visudo -c -f <候选文件>`（只读校验）。
//
// -c 是检查模式、-f 指定文件。它是候选文件**在 rename 之前**唯一的一道门：
// 通过了才允许它变成正式文件，所以任何语法问题都发生在“正式文件还没被碰过”的时候。
func specVisudoFile(path string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramVisudo, Args: []string{"-c", "-f", path}}
}

// specVisudoAll 构造 `visudo -c`（检查整份配置，含 include 进去的每个文件）。
//
// 它回答的是另一个问题：**整台机器**的 sudo 还能不能用。候选文件自己合格，
// 但主配置里别的地方已经坏了，此时把新规则放进去等于往一堆坏配置里再加一条。
func specVisudoAll() syscmd.Spec {
	return syscmd.Spec{Program: ProgramVisudo, Args: []string{"-c"}}
}

// specSudoListAs 构造 `sudo -l -U <name>`（只读：列出某个账号实际拥有的规则）。
//
// 这是“sudoers 里写了什么”之外的第二种证据：文件内容对不代表 sudo 解析出来的
// 结果对。它不需要 root 身份执行，也不需要目标账号的密码。
func specSudoListAs(name string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramSudo, Args: []string{"-l", "-U", name}}
}

// passwordStdin 构造 chpasswd 需要的标准输入。
//
// 这是密码离开秘密存储之后唯一到过的地方：不进 argv、不进日志、不进数据库。
// 调用方必须在用完之后把返回的切片清掉（本函数返回的就是它自己分配的那个）。
func passwordStdin(name string, secret []byte) []byte {
	buf := make([]byte, 0, len(name)+1+len(secret)+1)
	buf = append(buf, name...)
	buf = append(buf, ':')
	buf = append(buf, secret...)
	buf = append(buf, '\n')
	return buf
}

// wipe 把一段字节清零。
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
