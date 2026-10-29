package shares

// 命令模板（P08.1）。
//
// 所有外部命令都在这里构造：绝对路径、argv 逐项、参数来自代码或已经过形状校验的
// 内部标识。这里**没有**任何把 HTTP 原始字符串直接当参数塞进 argv 的路径。
//
// 密码只出现在一处：smbpasswd 的标准输入（sambaPasswordStdin）。argv 会出现在
// /proc/<pid>/cmdline 与 ps 里，日志会出现在日志文件里，数据库会长期留存——
// 这三处都不该有密码，所以它只走标准输入。
//
// 工具路径显式传参（ToolPaths），不放在包级变量里：包级可变状态会让两个并发操作
// 互相改写对方的工具路径，也会让单元测试之间串味。

import (
	"time"

	"upanel/internal/syscmd"
)

// 命令超时。账号操作在慢盘上也就是几十毫秒，给 30 秒是给 NSS 后端留余量。
const (
	commandTimeout    = 30 * time.Second
	aclCommandTimeout = 30 * time.Second
	// accountLookupTimeout 是**预检**里那一次 getent 的上限。
	//
	// 预检不是执行：它跑在 Web 等着的这条路径上，整条链路的预算比执行短得多
	// （agentclient.DefaultTimeout 是 10 秒）。用 30 秒等于把“NSS 卡住”翻译成
	// “代理不可用”——真机 P08.1 验收撞到过：getent 卡住，Web 在 10 秒时先超时，
	// 用户看到 503，而真正的原因（账号查询卡住）连日志都来不及写清楚。
	// 3 秒对一次 /etc/passwd 查询是奢侈的余量。预算用尽时**不是**报错：预检把
	// “没问出结果”写成 ACCOUNT_LOOKUP_TIMEOUT 告警，由执行阶段的长预算复核兜底
	// （见下一条常量与 DECISIONS Q223）。
	accountLookupTimeout = 3 * time.Second
	// accountLookupAttempts 是一次查询最多试几次（只在超时时重试）。
	//
	// 观察类命令是幂等的读，重试没有副作用；而真机上的超时实测多数是机器忙，
	// 不是 NSS 坏了。第二次拿到了答案，就不必把它写成一条“没核验”的告警。
	accountLookupAttempts = 2
	// accountLookupExecTimeout 是**执行阶段**（任务里的执行前复核）同一次查询的总预算。
	//
	// 执行不在 Web 的等待路径上：没有人拿着 HTTP 连接等它，用的是任务自己的预算。
	// 真机 P08.1 验收里 2 个 vCPU 的机器被服务采集器占住时，getent 实测卡到过
	// 4.7 / 7.3 / 7.5 秒（诱因见 DECISIONS Q222）。预检那条 3 秒的预算撞上它只能认输；
	// 执行这条 20 秒的不会——而执行阶段才是真正决定“改不改系统”的那一道门，
	// 它不该因为机器忙就把一次正确的操作判死。
	accountLookupExecTimeout = 20 * time.Second
)

// lookupAttemptTimeout 是单次尝试的上限：总预算除以尝试次数。
//
// 重试不能让调用方多等一秒——预检那条链路的总预算没变。执行阶段给的是更大的总预算，
// 单次上限同样只是它的一个分片。
func lookupAttemptTimeout(total time.Duration) time.Duration {
	return total / accountLookupAttempts
}

// syscmdSpecGetent 构造 `getent <db> <key>`。
func syscmdSpecGetent(db, key string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramGetent, Args: []string{db, key}}
}

// specUseradd 构造受管理共享账号的创建命令。
//
// 六个参数每一个都在守一件事：
//   - --system：用系统 UID 段（<1000），不会与登录用户抢号段；
//   - --no-create-home + --home-dir /nonexistent：不建 home，
//     删账号时也就没有“会不会删掉数据”的问题；
//   - --shell /usr/sbin/nologin：登录 shell 不可用（第一道屏障）；
//   - --user-group：建一个同名组，避免落进 users 组而获得别人的组权限。
//
// 这样建出来的账号没有 Linux 密码（shadow 里是 "!"），这是第二道屏障：SMB 密码
// 存在 Samba 的 passdb 里，Linux 密码从未被设置过。
func specUseradd(name string) syscmd.Spec {
	return syscmd.Spec{
		Program: ProgramUseradd,
		Args: []string{
			"--system",
			"--no-create-home",
			"--home-dir", DefaultHome,
			"--shell", DefaultShell,
			"--user-group",
			name,
		},
	}
}

// specUserdel 构造删除命令。不加 -r：home 是 /nonexistent，本来就没有可删的。
func specUserdel(name string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramUserdel, Args: []string{name}}
}

// specPasswdStatus 构造 `passwd -S <name>`（只读）。
func specPasswdStatus(name string) syscmd.Spec {
	return syscmd.Spec{Program: ProgramPasswd, Args: []string{"-S", name}}
}

// specSambaSetPassword 构造 `smbpasswd -s -a <name>`，密码经标准输入。
//
// -s 是 silent：从标准输入读两遍密码，不交互、不打印。缺了它 smbpasswd 会去开终端，
// 在代理这种没有终端的环境里直接失败。
//
// -a 对已存在的账号就是重设密码，所以新建与改密码用同一条模板——模板只有一份，
// 密码通道也只有一条。
func specSambaSetPassword(t ToolPaths, name string, stdin []byte) syscmd.Spec {
	return syscmd.Spec{Program: t.Smbpasswd, Args: []string{"-s", "-a", name}, Stdin: stdin}
}

// specSambaDisable 在 passdb 里给账号打上 D 标志。
func specSambaDisable(t ToolPaths, name string) syscmd.Spec {
	return syscmd.Spec{Program: t.Smbpasswd, Args: []string{"-d", name}}
}

// specSambaEnable 清掉 passdb 里的 D 标志。
func specSambaEnable(t ToolPaths, name string) syscmd.Spec {
	return syscmd.Spec{Program: t.Smbpasswd, Args: []string{"-e", name}}
}

// specSambaDelete 删除 passdb 里的账号（不动 Linux 账号）。
func specSambaDelete(t ToolPaths, name string) syscmd.Spec {
	return syscmd.Spec{Program: t.Smbpasswd, Args: []string{"-x", name}}
}

// specPdbeditAccount 构造 `pdbedit -L -v -u <name>`（只读）。
//
// 注意这里**没有** -w：`pdbedit -L -w` 会把 passdb 导出成 smbpasswd 格式，里面含
// NT/LM 哈希。这条命令在本代码里不存在，所以也没有“不小心把哈希打进日志”的路。
func specPdbeditAccount(t ToolPaths, name string) syscmd.Spec {
	return syscmd.Spec{Program: t.Pdbedit, Args: []string{"-L", "-v", "-u", name}}
}

// specSetfacl 构造 `setfacl --set <spec> <dir>`。
//
// 用 --set 而不是 -m：--set 整份替换目录上的 ACL，上一次授权过、这一次没授权的账号
// 会真的失去权限。增量修改做不到这一点，而“残留的授权”从界面上看不见。
func specSetfacl(t ToolPaths, spec, dir string) syscmd.Spec {
	return syscmd.Spec{Program: t.Setfacl, Args: []string{"--set", spec, dir}}
}

// specGetfacl 构造 `getfacl -c -p <dir>`（只读）。
//
// -c 省略注释行、-p 保留绝对路径：输出里只剩条目，解析不必猜。
func specGetfacl(t ToolPaths, dir string) syscmd.Spec {
	return syscmd.Spec{Program: t.Getfacl, Args: []string{"-c", "-p", dir}}
}

// specExportfsReload 构造 `exportfs -ra`（NFS，P14.4）。
//
// -r 重读全部导出文件、-a 对所有条目生效。没有别的参数：面板不按路径单独刷，
// 因为单独刷的结果取决于“现在内核里有什么”，而整表刷新才是一份确定的输入对应的
// 一份确定的输出。
//
// 退出码不是证据：VM 实测，目录不存在时它打印 `Failed to stat …` 并以 1 退出，
// **但导出仍然被注册了**。所以判决一律来自随后的 `exportfs -v`。
func specExportfsReload(t ToolPaths) syscmd.Spec {
	return syscmd.Spec{Program: t.Exportfs, Args: []string{"-ra"}}
}

// specExportfsV 构造 `exportfs -v`（只读：列出实际生效的导出与选项）。
//
// 不带路径：要的就是整张表。逐条读回时才知道“该消失的消失了没有”，
// 而只查“我在的那条”会漏掉删不掉的旧条目。
func specExportfsV(t ToolPaths) syscmd.Spec {
	return syscmd.Spec{Program: t.Exportfs, Args: []string{"-v"}}
}
