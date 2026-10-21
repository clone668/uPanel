//go:build linux

package terminal

// Linux 侧的 PTY 启动与账号校验（P06.3）。
//
// 这里是整个终端功能唯一的提权点，因此每一步都写成“可以被读懂的显式代码”：
// 找账号 → 核对它不该有特权 → 清空环境 → fork/exec → **读回子进程的内核身份**。
// 最后一步是关键：`setuid` 有没有真的生效，不能靠“我传了 Credential 参数”来推断，
// 要去看 /proc/<pid>/status 里内核报告的 Uid/Gid。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// nologinShells 是“不能登录”的 shell。终端功能要求一个真 shell：拿到 nologin 就
// 没有可交互的解释器，与其在 PTY 里立刻退出、让人以为是面板的 bug，不如在启动阶段说清楚。
var nologinShells = map[string]bool{
	"":                   true,
	"/usr/sbin/nologin":  true,
	"/sbin/nologin":      true,
	"/bin/false":         true,
	"/usr/bin/false":     true,
	"/bin/sync":          true,
	"/usr/sbin/shutdown": true,
	"/bin/shutdown":      true,
	"/usr/sbin/halt":     true,
	"/usr/sbin/reboot":   true,
}

// privilegedGroups 是终端账号**不能**加入的组（PLAN.md 第 4 节：不加入 docker/disk
// 等特权组）。加入其中任何一个，降权就只是名义上的：
//   - docker / lxd：一条命令就能挂载宿主根目录，等价于 root；
//   - disk：直接读写块设备；
//   - sudo / admin / wheel：等于给了 root；
//   - adm：读系统日志；
//   - kvm / libvirt：接管虚拟化。
var privilegedGroups = map[string]bool{
	"root": true, "sudo": true, "admin": true, "wheel": true, "docker": true,
	"lxd": true, "disk": true, "adm": true, "kvm": true, "libvirt": true,
}

// ResolveAccount 按账号名解析终端账号，并校验它确实是一个降权账号。
//
// homeOverride / shellOverride 来自配置（root:root 0600），留空则用账号自身属性。
// 任何一项不满足都返回 ErrAccountInvalid 并说明怎么修——终端功能的失败必须在
// 启动日志里就能看懂，而不是等到用户点开页面才发现“shell 起不来”。
func ResolveAccount(name, homeOverride, shellOverride string) (Account, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Account{}, fmt.Errorf("%w: 终端账号名不能为空", ErrAccountInvalid)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, fmt.Errorf("%w: 账号 %q 不存在（安装步骤会创建 upanel-shell）: %v",
			ErrAccountInvalid, name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return Account{}, fmt.Errorf("%w: 账号 %q 的 UID %q 不是数字", ErrAccountInvalid, name, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return Account{}, fmt.Errorf("%w: 账号 %q 的 GID %q 不是数字", ErrAccountInvalid, name, u.Gid)
	}
	if uid == 0 || gid == 0 {
		return Account{}, fmt.Errorf("%w: 账号 %q 的 UID/GID 不能是 0：终端只允许专用非 root 账号",
			ErrAccountInvalid, name)
	}

	home := strings.TrimSpace(homeOverride)
	if home == "" {
		home = u.HomeDir
	}
	if err := checkHome(home, uint32(uid)); err != nil {
		return Account{}, err
	}

	shell := strings.TrimSpace(shellOverride)
	if shell == "" {
		// os/user 的 User 结构里没有 shell 字段，它只在 /etc/passwd 里。
		// 这里用 root 进程读本地账号库，取第 7 个字段，不解析其他内容。
		shell, err = lookupShell(name)
		if err != nil {
			return Account{}, err
		}
	}
	if !filepath.IsAbs(shell) {
		return Account{}, fmt.Errorf("%w: shell 必须是绝对路径，当前为 %q", ErrAccountInvalid, shell)
	}
	if nologinShells[shell] {
		return Account{}, fmt.Errorf("%w: 账号 %q 的 shell 是 %q，无法交互；"+
			"请在安装时指定 /bin/bash（usermod -s /bin/bash %s）",
			ErrAccountInvalid, name, shell, name)
	}
	info, err := os.Stat(shell)
	if err != nil {
		return Account{}, fmt.Errorf("%w: shell %q 不可用: %v", ErrAccountInvalid, shell, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return Account{}, fmt.Errorf("%w: shell %q 不是可执行文件", ErrAccountInvalid, shell)
	}

	if err := checkGroups(name, uint32(gid)); err != nil {
		return Account{}, err
	}

	return Account{User: name, UID: uint32(uid), GID: uint32(gid), Home: home, Shell: shell}, nil
}

// checkHome 校验 shell 的工作目录：必须存在、是目录、属于该账号，且其他人不能写。
//
// “其他人不能写”不是洁癖：home 里的 .bashrc 会被 shell 读取执行，如果别的账号
// 能往里写，就等于能把代码塞进终端账号里跑——降权到 upanel-shell 之后，
// 那里又会成为新的落脚点。
func checkHome(home string, uid uint32) error {
	if home == "" || !filepath.IsAbs(home) {
		return fmt.Errorf("%w: 终端 home 必须是绝对路径，当前为 %q", ErrAccountInvalid, home)
	}
	info, err := os.Stat(home)
	if err != nil {
		return fmt.Errorf("%w: 终端 home %q 不可用: %v（安装步骤会创建它并 chown 给终端账号）",
			ErrAccountInvalid, home, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: 终端 home %q 不是目录", ErrAccountInvalid, home)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: 无法读取 %q 的属主", ErrAccountInvalid, home)
	}
	if st.Uid != uid {
		return fmt.Errorf("%w: 终端 home %q 的属主是 uid %d，应为 %d",
			ErrAccountInvalid, home, st.Uid, uid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: 终端 home %q 允许组或其他账号写入（权限 %04o），"+
			"shell 会执行里面 rc 文件，必须收紧到 0750 或更严",
			ErrAccountInvalid, home, info.Mode().Perm())
	}
	return nil
}

// lookupShell 从 /etc/passwd 里取账号的登录 shell。
//
// 用 os/user 的 User 结构拿不到 shell 字段，而这里需要的正是它。只读本地文件、
// 只取第 7 个字段，不解析其他内容。
func lookupShell(name string) (string, error) {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "", fmt.Errorf("%w: 读取 /etc/passwd 失败: %v", ErrAccountInvalid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[0] != name {
			continue
		}
		return fields[6], nil
	}
	return "", fmt.Errorf("%w: /etc/passwd 里没有账号 %q", ErrAccountInvalid, name)
}

// checkGroups 核对终端账号的附加组。
//
// 只读 /etc/group：这是**本地**成员关系，SSSD/LDAP 提供的组不在这里。所以它是
// 一道防手滑的检查（把 upanel-shell 加进 docker 是最常见的错法），不是边界本身；
// 边界是下面 fork/exec 时真正生效的 credential。
func checkGroups(name string, primaryGID uint32) error {
	data, err := os.ReadFile("/etc/group")
	if err != nil {
		return fmt.Errorf("%w: 读取 /etc/group 失败: %v", ErrAccountInvalid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		groupName := fields[0]
		gid, convErr := strconv.ParseUint(fields[2], 10, 32)
		if convErr != nil {
			continue
		}
		member := false
		if len(fields[3]) > 0 {
			for _, m := range strings.Split(fields[3], ",") {
				if m == name {
					member = true
					break
				}
			}
		}
		if (member || uint32(gid) == primaryGID) && privilegedGroups[groupName] {
			return fmt.Errorf("%w: 终端账号 %q 属于特权组 %q（PLAN.md 第 4 节要求终端账号"+
				"不加入 sudo/docker/disk 等特权组）；请把它从该组移除后重启代理",
				ErrAccountInvalid, name, groupName)
		}
	}
	return nil
}

// ptyProcess 是一个已启动的 PTY 子进程。
type ptyProcess interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Close() error
	Pid() int
	Wait() ExitStatus
}

// spawnSpec 是一次 PTY 启动的输入。
type spawnSpec struct {
	Account    Account
	Cols, Rows int
}

// ptyProc 是 ptyProcess 的实现。
type ptyProc struct {
	ptmx *os.File
	cmd  *exec.Cmd

	// startTick 是子进程的启动时刻（/proc/<pid>/stat 第 22 字段）。
	//
	// 结束会话时要给子进程所在的进程组发信号，而 pid 一旦被内核回收就可能被复用：
	// 先用 (ppid, starttime) 确认“这个 pid 还是我启动的那个子进程”，否则宁可只关
	// master 也不能往一个陌生进程组里打信号。
	startTick uint64

	waitOnce sync.Once
	waitRes  ExitStatus

	closeOnce sync.Once
	closeErr  error
}

// startPTY 以终端账号的身份启动一个交互 shell。
func startPTY(spec spawnSpec) (ptyProcess, error) {
	cmd := exec.Command(spec.Account.Shell, "-i")
	// 环境是白名单，不是继承：nil 之外的任何继承都会把调用者的
	// LD_PRELOAD / BASH_ENV / GIT_* 之类带进一个“受控”终端里。
	cmd.Env = buildEnv(spec.Account)
	cmd.Dir = spec.Account.Home

	// Setsid + Setctty：让 shell 成为新会话的首进程，并把 tty 作为控制终端。
	// 没有这两项，bash 的作业控制（Ctrl-C、前后台）不工作——它需要一个真正的终端。
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	dropped := uint32(os.Getuid()) != spec.Account.UID
	if dropped {
		attrs.Credential = &syscall.Credential{
			Uid: spec.Account.UID,
			Gid: spec.Account.GID,
			// Groups 留空 + NoSetGroups=false：内核会调用 setgroups(0, NULL)，
			// 也就是**清空附加组**。只 setuid 不清附加组，子进程仍然带着 root 的
			// 补充组（disk、docker 常常就是这种形式），降权等于没做。
			NoSetGroups: false,
		}
	}

	ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{
		Cols: uint16(spec.Cols), //nolint:gosec // 已由 Params.Validate 限定在 1—500
		Rows: uint16(spec.Rows), //nolint:gosec
	}, attrs)
	if err != nil {
		// 失败原因往往是权限：uid 不匹配、home 不可进入、shell 不可执行。
		// 把它原样带上，别让运维去猜。
		return nil, fmt.Errorf("terminal: 启动 PTY 失败（账号 %s uid=%d home=%s shell=%s）: %w",
			spec.Account.User, spec.Account.UID, spec.Account.Home, spec.Account.Shell, err)
	}

	proc := &ptyProc{ptmx: ptmx, cmd: cmd}
	// 记下启动时刻：Close 里发信号之前要用它确认 pid 没有易主。
	if _, _, start, ok := procStateOf(cmd.Process.Pid); ok {
		proc.startTick = start
	}
	// 读回内核身份：这是“降权真的生效了”的唯一证据。
	if err := verifyChildAccount(cmd.Process.Pid, spec.Account, dropped); err != nil {
		_ = proc.Close()
		return nil, err
	}
	return proc, nil
}

// buildEnv 构造最小环境（PLAN.md 第 4 节：最小白名单、LANG=C.UTF-8、固定 PATH）。
//
// 白名单就是这一份，**不往里加“标记变量”**：任何面板自己的变量进了终端环境，都会
// 被终端里的程序当成可依赖的输入（脚本按它分支、被打印进日志），而它同时也把
// “这个 shell 是面板开的”这件事泄露给了任意被执行的程序。终端里的进程与一次
// SSH 登录之间的差别不需要靠环境变量表达。
func buildEnv(acc Account) []string {
	return []string{
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + acc.Home,
		"USER=" + acc.User,
		"LOGNAME=" + acc.User,
		"SHELL=" + acc.Shell,
		"TERM=xterm-256color",
	}
}

// verifyChildAccount 从 /proc/<pid>/status 读回子进程真实的 uid/gid 与附加组。
func verifyChildAccount(pid int, acc Account, dropped bool) error {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return fmt.Errorf("terminal: 读不到子进程 %d 的身份: %w", pid, err)
	}
	var uidLine, gidLine, groupsLine string
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "Uid:"):
			uidLine = strings.TrimSpace(strings.TrimPrefix(line, "Uid:"))
		case strings.HasPrefix(line, "Gid:"):
			gidLine = strings.TrimSpace(strings.TrimPrefix(line, "Gid:"))
		case strings.HasPrefix(line, "Groups:"):
			groupsLine = strings.TrimSpace(strings.TrimPrefix(line, "Groups:"))
		}
	}
	uidFields := strings.Fields(uidLine)
	gidFields := strings.Fields(gidLine)
	if len(uidFields) < 2 || len(gidFields) < 2 {
		return fmt.Errorf("terminal: 子进程 %d 的身份行无法解析", pid)
	}
	// 核对真实与有效 uid/gid：只要有效身份不是终端账号，就直接杀掉子进程。
	for _, v := range uidFields[:2] {
		if v != strconv.FormatUint(uint64(acc.UID), 10) {
			return fmt.Errorf("terminal: 子进程 %d 的 uid 是 %s，应为 %d：降权没有生效，已终止",
				pid, v, acc.UID)
		}
	}
	for _, v := range gidFields[:2] {
		if v != strconv.FormatUint(uint64(acc.GID), 10) {
			return fmt.Errorf("terminal: 子进程 %d 的 gid 是 %s，应为 %d：降权没有生效，已终止",
				pid, v, acc.GID)
		}
	}
	// 附加组必须被清空：残留 root 组意味着 setgroups 没跑（或者有人改了 credential）。
	if dropped && groupsLine != "" {
		for _, v := range strings.Fields(groupsLine) {
			if v == "0" {
				return fmt.Errorf("terminal: 子进程 %d 仍带着 root 附加组：降权没有清空附加组，已终止", pid)
			}
		}
	}
	return nil
}

// Read 实现 ptyProcess。
func (p *ptyProc) Read(buf []byte) (int, error) { return p.ptmx.Read(buf) }

// Write 实现 ptyProcess。
func (p *ptyProc) Write(buf []byte) (int, error) { return p.ptmx.Write(buf) }

// Resize 实现 ptyProcess。
func (p *ptyProc) Resize(cols, rows int) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{
		Cols: uint16(cols), //nolint:gosec // 已由 Params.Validate 限定在 1—500
		Rows: uint16(rows), //nolint:gosec
	})
}

// Pid 实现 ptyProcess。
func (p *ptyProc) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Close 结束这个 PTY：关 master，并**显式**挂断跑在上面的进程组。
//
// 幂等：会话结束与读泵退出都会调它。
func (p *ptyProc) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.ptmx.Close()
		p.hangup()
	})
	if errors.Is(p.closeErr, os.ErrClosed) {
		return nil
	}
	return p.closeErr
}

const (
	// childHangupGrace 是发出 SIGHUP 之后等 shell 自己退出的时间。
	// bash/sh 收到 SIGHUP 立刻就走，这段时间只用来兜住“tty 上还有前台作业”的情况。
	childHangupGrace = 2 * time.Second
	// childExitPoll 是等待期间查看 /proc 的间隔。
	childExitPoll = 10 * time.Millisecond
)

// hangup 结束 PTY 上跑着的进程组。
//
// **为什么只关 master 不够**：Linux 的挂断信号（SIGHUP 到前台进程组）是在 tty 被
// 释放时发出的，而本进程的读泵正阻塞在这个 master 的 read 上——Go 的 os.File.Close
// 对阻塞 fd 只做“标记 + 减引用”，真正的 close(2) 要等那次 read 返回。于是：
// fd 还挂在 /proc/<pid>/fd 里、tty 没被释放、shell 收不到 SIGHUP，会话在表里已经
// 结束，upanel-shell 却一直活着（P06.3 验收第 13 节实测到的残留）。
//
// 所以挂断必须显式做：先 SIGHUP 整个进程组（子进程 setsid 过，pgid == pid，打负号
// 覆盖组内所有进程），短时间内不退再补 SIGKILL。信号只在确认 pid 仍是本进程的子
// 进程时才发——见 startTick。
func (p *ptyProc) hangup() {
	pid := p.Pid()
	if pid <= 0 {
		return
	}
	state, ours := p.childState()
	if !ours {
		return // 已经被 Wait 回收（或根本不是我们的子进程），pid 可能已经易主
	}
	if state == 'Z' || state == 'X' {
		return // 已经退出，只是还没被回收，不需要挂断
	}
	_ = syscall.Kill(-pid, syscall.SIGHUP)

	deadline := time.Now().Add(childHangupGrace)
	for {
		state, ours = p.childState()
		if !ours || state == 'Z' || state == 'X' {
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(childExitPoll)
	}
	// 不理会 SIGHUP 的作业（例如自己 trap 过的脚本）在这里被连组清掉：
	// 会话结束意味着这个 tty 上不该再留下任何东西。
	if _, still := p.childState(); still {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// childState 返回子进程的内核状态，并说明这个 pid 是否仍是本进程启动的那个子进程。
func (p *ptyProc) childState() (byte, bool) {
	pid := p.Pid()
	if pid <= 0 {
		return 0, false
	}
	state, ppid, start, ok := procStateOf(pid)
	if !ok || ppid != os.Getpid() {
		return 0, false
	}
	if p.startTick != 0 && start != p.startTick {
		return 0, false
	}
	return state, true
}

// procStateOf 读 /proc/<pid>/stat 的状态、父进程与启动时刻。
//
// comm 字段（第 2 字段）里可以有空格和括号，所以从**最后一个** ')' 之后开始切字段：
// 之后依次是状态（第 3 字段）、ppid（第 4 字段）……启动时刻是第 22 字段。
// 不用 kill(pid, 0) 判断存活：僵尸进程在它眼里“还活着”，而这里需要区分
// “还在跑”和“已经退出、等着回收”。
func procStateOf(pid int) (state byte, ppid int, start uint64, ok bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0, false
	}
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	if end < 0 || end+2 >= len(text) {
		return 0, 0, 0, false
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 20 || fields[0] == "" {
		return 0, 0, 0, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, 0, false
	}
	start, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	return fields[0][0], ppid, start, true
}

// Wait 等待子进程退出。只允许调用一次（多个 goroutine 调 Wait 会互相抢同一份状态）。
func (p *ptyProc) Wait() ExitStatus {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		state := p.cmd.ProcessState
		if state == nil {
			p.waitRes = ExitStatus{Err: err}
			return
		}
		if ws, ok := state.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				p.waitRes = ExitStatus{Signal: int(ws.Signal())}
				return
			}
			p.waitRes = ExitStatus{Code: ws.ExitStatus(), HasCode: true}
			return
		}
		p.waitRes = ExitStatus{Err: err}
	})
	return p.waitRes
}

// isPTYClosed 判断读错误是不是“tty 对端已经没了”。
//
// Linux 上子进程退出后 master 的 read 返回 EIO；这既不是 EOF 也不是真正的故障，
// 对读循环来说它等价于“输出结束”。
func isPTYClosed(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EBADF) || errors.Is(err, os.ErrClosed)
}
