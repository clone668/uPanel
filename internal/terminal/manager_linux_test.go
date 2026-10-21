//go:build linux

package terminal

// 会话层的原生 PTY 用例（P06.3）。
//
// 这些用例只能在真的 Linux 上跑：它们 fork 一个真 shell、通过真 tty 收发字节。
// 开发宿主（Windows）上这一整个文件不参与编译，所以“宿主 go test 全绿”**不**等于
// 终端可用；结论只能由验收 VM 的原生套件给出。
//
// 覆盖的重点是“会话活得比连接长”这条最容易被写错的语义：
//   - 断开连接不杀 PTY，宽限期内重接连回同一个 shell（DetachKeepsPTY）；
//   - 宽限期满才回收（GraceExpiry）；
//   - 空闲与绝对寿命两条终止线各自生效（IdleExpiry / MaxDuration）；
//   - 数量上限、单连接占用、结束原因与退出码如实上报。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testAccount 返回一个可以用来起 PTY 的账号。
//
// 以 root 跑测试时（验收 VM 的原生套件就是 root）必须换一个真实的非 root 账号：
// NewManager 拒绝 uid 0 正是它的职责，测试不能为了跑通而绕过它。
func testAccount(t *testing.T) Account {
	t.Helper()
	uid, gid := os.Getuid(), os.Getgid()
	if uid != 0 {
		home := t.TempDir()
		return Account{User: currentUserName(t), UID: uint32(uid), GID: uint32(gid), //nolint:gosec // 取自内核
			Home: home, Shell: testShell()}
	}
	target, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("没有 nobody 账号，无法在 root 下模拟降权终端")
	}
	uid64, err := strconv.ParseUint(target.Uid, 10, 32)
	if err != nil || uid64 == 0 {
		t.Skipf("nobody 的 uid %q 不可用", target.Uid)
	}
	gid64, err := strconv.ParseUint(target.Gid, 10, 32)
	if err != nil || gid64 == 0 {
		t.Skipf("nobody 的 gid %q 不可用", target.Gid)
	}
	// home 必须让目标账号进得去：/tmp/TestXxx 是 0700 root，穿过它到不了里面。
	home, err := os.MkdirTemp("/var/tmp", "upanel-term-home-")
	if err != nil {
		t.Fatalf("创建测试 home 失败: %v", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatalf("设置测试 home 权限失败: %v", err)
	}
	if err := os.Chown(home, int(uid64), int(gid64)); err != nil { //nolint:gosec // 上面已限定范围
		t.Fatalf("设置测试 home 属主失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return Account{User: target.Username, UID: uint32(uid64), GID: uint32(gid64), //nolint:gosec
		Home: home, Shell: testShell()}
}

func currentUserName(t *testing.T) string {
	t.Helper()
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid" + strconv.Itoa(os.Getuid())
}

func testShell() string {
	for _, cand := range []string{"/bin/sh", "/usr/bin/sh", "/bin/bash"} {
		if st, err := os.Stat(cand); err == nil && st.Mode()&0o111 != 0 {
			return cand
		}
	}
	return "/bin/sh"
}

// testLimits 是测试用的收紧限额：契约值在 VerifyLimits 里另有断言，
// 这里只需要“几秒钟内就能看到结果”。
func testLimits() Limits {
	return Limits{
		IdleTimeout:     30 * time.Second,
		MaxDuration:     60 * time.Second,
		DisconnectGrace: time.Second,
		MaxPerUser:      1,
		MaxSessions:     2,
	}
}

func newTestManager(t *testing.T, limits Limits) *Manager {
	t.Helper()
	if limits == (Limits{}) {
		limits = testLimits()
	}
	m, err := NewManager(Options{Account: testAccount(t), Limits: limits})
	if err != nil {
		t.Fatalf("构造会话层失败: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

// sweep 立刻跑一次超时清理，等价于后台 Run 的一跳。
//
// 让用例不必等待一秒一跳的调度器；同时它走的正是生产代码那条路径（sweepLocked +
// closeExpired），不是为测试另开的分支。
func sweep(m *Manager) {
	m.mu.Lock()
	expired := m.sweepLocked(m.opts.Now())
	m.mu.Unlock()
	m.closeExpired(expired)
}

func createSession(t *testing.T, m *Manager, owner int64) SessionInfo {
	t.Helper()
	info, err := m.Create(context.Background(), CreateInput{
		Owner: owner, Params: Params{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if !IsValidSessionID(info.SessionID) {
		t.Fatalf("会话 id 形态不合法: %q", info.SessionID)
	}
	return info
}

// readUntil 把输出读到 buf，直到出现 want（或超时）。
func readUntil(t *testing.T, att *Attachment, want string, timeout time.Duration) string {
	t.Helper()
	var collected bytes.Buffer
	buf := make([]byte, 4096)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := att.Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
			if strings.Contains(collected.String(), want) {
				return collected.String()
			}
		}
		if err != nil {
			break
		}
	}
	t.Fatalf("等 %q 超时；已收到 %q", want, collected.String())
	return collected.String()
}

func echoMarker(t *testing.T, att *Attachment, marker string) string {
	t.Helper()
	if _, err := att.Write([]byte("echo " + marker + "\n")); err != nil {
		t.Fatalf("写入 tty 失败: %v", err)
	}
	return readUntil(t, att, marker, 10*time.Second)
}

func TestManagerSessionLifecycle(t *testing.T) {
	m := newTestManager(t, Limits{})
	const owner int64 = 7

	info := createSession(t, m, owner)
	if info.Account != m.Account().User || info.AccountUID != m.Account().UID {
		t.Fatalf("会话账号是 %s(uid %d)，应为 %s(uid %d)",
			info.Account, info.AccountUID, m.Account().User, m.Account().UID)
	}
	if info.State != StateDetached {
		t.Fatalf("刚创建的会话状态是 %q，应为 %q", info.State, StateDetached)
	}
	if got, err := m.List(ListFilter{UserID: owner}); err != nil || len(got) != 1 {
		t.Fatalf("列表应有 1 个会话，得到 %d 个（err=%v）", len(got), err)
	}
	// 别人的查询看不见它，也拿不到它。
	if _, err := m.Get(ListFilter{UserID: owner + 1, SessionID: info.SessionID}); err == nil {
		t.Fatal("别人的 user_id 竟然读到了这个会话")
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatalf("读取别人的会话应返回 ErrNotFound，得到 %v", err)
	}

	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	if att.Info().State != StateAttached {
		t.Fatalf("附加后状态是 %q", att.Info().State)
	}
	echoMarker(t, att, "upanel-marker-1")

	if _, err := m.Attach(info.SessionID, owner); err == nil {
		t.Fatal("同一会话被附加了两次")
	} else if !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("第二次附加应返回 ErrAlreadyAttached，得到 %v", err)
	}

	// 别人的会话对外一律“不存在”：Close 与 Get 同一套规则，避免用关闭接口探测
	// 某个会话 id 是否存在。
	if err := m.Close(CloseInput{SessionID: info.SessionID, Owner: owner + 1, Reason: ReasonUser}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("非属主关闭应返回 ErrNotFound，得到 %v", err)
	}

	att.Close()
	if err := m.Close(CloseInput{SessionID: info.SessionID, Owner: owner, Reason: ReasonUser}); err != nil {
		t.Fatalf("属主关闭失败: %v", err)
	}
	if _, err := m.Get(ListFilter{UserID: owner, SessionID: info.SessionID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("关闭后会话仍在表里（err=%v）", err)
	}
	if _, err := m.Attach(info.SessionID, owner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("关闭后仍能附加（err=%v）", err)
	}
}

// TestManagerDetachKeepsPTY 是断线宽限的核心用例：断开连接之后 PTY 还活着，
// 重连接回同一个 shell。
//
// 这条语义很容易被写成“断开即关 master”（关 master 会让读泵立刻退出，看着很干净），
// 但那样 shell 会收到 SIGHUP 当场死掉，宽限期就成了一句空话。回归点就在这里：
// 重连之后 `echo` 还能打出新标记。
func TestManagerDetachKeepsPTY(t *testing.T) {
	m := newTestManager(t, Limits{
		IdleTimeout: 30 * time.Second, MaxDuration: 60 * time.Second,
		DisconnectGrace: 5 * time.Second, MaxPerUser: 1, MaxSessions: 2,
	})
	const owner int64 = 11
	info := createSession(t, m, owner)

	first, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("第一次附加失败: %v", err)
	}
	echoMarker(t, first, "before-detach")
	first.Close()

	// 断开之后会话还在表里，状态是 detached。
	view, err := m.Get(ListFilter{UserID: owner, SessionID: info.SessionID})
	if err != nil {
		t.Fatalf("断开后会话消失了: %v", err)
	}
	if view.State != StateDetached {
		t.Fatalf("断开后状态是 %q，应为 %q", view.State, StateDetached)
	}

	second, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("宽限期内重连失败: %v", err)
	}
	defer second.Close()
	// 同一个 shell：新标记能被解释并回显。若 PTY 已被关掉，这里的写入会失败、
	// 或者读到 EOF。
	echoMarker(t, second, "after-reattach")

	sweep(m) // 没有超时，会话必须还在
	if _, err := m.Get(ListFilter{UserID: owner, SessionID: info.SessionID}); err != nil {
		t.Fatalf("刚重连的会话被误回收: %v", err)
	}
}

// TestManagerGraceExpiry 证明宽限期真的会到期回收。
func TestManagerGraceExpiry(t *testing.T) {
	m := newTestManager(t, Limits{
		IdleTimeout: 30 * time.Second, MaxDuration: 60 * time.Second,
		DisconnectGrace: time.Second, MaxPerUser: 1, MaxSessions: 2,
	})
	const owner int64 = 12
	info := createSession(t, m, owner)
	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	echoMarker(t, att, "grace-start")
	att.Close()

	// 宽限期内还在。
	sweep(m)
	if _, err := m.Get(ListFilter{UserID: owner, SessionID: info.SessionID}); err != nil {
		t.Fatalf("宽限期还没到就回收了: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	sweep(m)
	if _, err := m.Get(ListFilter{UserID: owner, SessionID: info.SessionID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("宽限期过后会话仍在（err=%v）", err)
	}
	if _, err := m.Attach(info.SessionID, owner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("宽限期过后仍能附加（err=%v）", err)
	}
}

// shellPid 取会话里那个 shell 的 pid（同包内可见，只给原生用例用）。
func shellPid(t *testing.T, m *Manager, sessionID string) int {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		t.Fatalf("会话 %s 不在表里", sessionID)
	}
	return s.proc.Pid()
}

// waitPidGone 等 pid 从 /proc 里消失（说明子进程真的退出了、也被回收了）。
func waitPidGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestManagerGraceExpiryReapsShell 是 P06.3 验收第 13 节那个残留问题的回归用例。
//
// 会话被回收之后，tty 上的 shell 必须真的结束，不能只从会话表里消失。只关 master
// 做不到这件事：读泵阻塞在 master 的 read 上握着 file 引用，close 只是减引用，
// tty 不会被释放，shell 也就收不到 SIGHUP——实测会留下一个永生的 upanel-shell。
func TestManagerGraceExpiryReapsShell(t *testing.T) {
	m := newTestManager(t, Limits{
		IdleTimeout: 30 * time.Second, MaxDuration: 60 * time.Second,
		DisconnectGrace: 300 * time.Millisecond, MaxPerUser: 1, MaxSessions: 2,
	})
	const owner int64 = 17
	info := createSession(t, m, owner)
	pid := shellPid(t, m, info.SessionID)

	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	echoMarker(t, att, "reap-start")
	att.Close()

	time.Sleep(400 * time.Millisecond)
	sweep(m)
	if _, err := m.Get(ListFilter{UserID: owner, SessionID: info.SessionID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("宽限期过后会话仍在（err=%v）", err)
	}
	if !waitPidGone(pid, 5*time.Second) {
		t.Fatalf("会话已回收，shell 进程 %d 却还活着", pid)
	}
}

// TestManagerCloseReapsShell：用户主动关闭同样要把 shell 收干净。
func TestManagerCloseReapsShell(t *testing.T) {
	m := newTestManager(t, Limits{})
	const owner int64 = 18
	info := createSession(t, m, owner)
	pid := shellPid(t, m, info.SessionID)

	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	defer att.Close()
	echoMarker(t, att, "close-reap")

	if err := m.Close(CloseInput{SessionID: info.SessionID, Owner: owner, Reason: ReasonUser}); err != nil {
		t.Fatalf("关闭会话失败: %v", err)
	}
	if !waitPidGone(pid, 5*time.Second) {
		t.Fatalf("会话已关闭，shell 进程 %d 却还活着", pid)
	}
}

// TestManagerIdleExpiry 用真正的后台循环（Run）验证空闲回收，而不是手工 sweep。
func TestManagerIdleExpiry(t *testing.T) {
	m := newTestManager(t, Limits{
		IdleTimeout: 300 * time.Millisecond, MaxDuration: time.Minute,
		DisconnectGrace: 5 * time.Second, MaxPerUser: 1, MaxSessions: 2,
	})
	const owner int64 = 13
	info := createSession(t, m, owner)
	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	defer att.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	select {
	case <-att.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("空闲会话没有被回收")
	}
	exit, reason, hasExit := att.Exit()
	if reason != ReasonIdle {
		t.Fatalf("结束原因应是被空闲回收（%q），得到 %q", ReasonIdle, reason)
	}
	if hasExit && exit.Code != 0 {
		t.Fatalf("空闲回收时 shell 的退出码是 %d", exit.Code)
	}
	if _, err := m.Get(ListFilter{UserID: owner, SessionID: info.SessionID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空闲回收后会话仍在（err=%v）", err)
	}
}

// TestManagerMaxDuration 验证绝对寿命：一直在敲键盘也到点结束。
func TestManagerMaxDuration(t *testing.T) {
	m := newTestManager(t, Limits{
		IdleTimeout: 30 * time.Second, MaxDuration: 700 * time.Millisecond,
		DisconnectGrace: 5 * time.Second, MaxPerUser: 1, MaxSessions: 2,
	})
	const owner int64 = 14
	info := createSession(t, m, owner)
	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	defer att.Close()
	echoMarker(t, att, "max-duration-start")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	select {
	case <-att.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("会话超过绝对寿命后没有被结束")
	}
	if _, reason, _ := att.Exit(); reason != ReasonExpired {
		t.Fatalf("结束原因应是 %q，得到 %q", ReasonExpired, reason)
	}
}

// TestManagerExitCodeAndReason：shell 自己退出时要如实报退出码与原因。
func TestManagerExitCodeAndReason(t *testing.T) {
	m := newTestManager(t, Limits{})
	const owner int64 = 15
	info := createSession(t, m, owner)
	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	defer att.Close()
	if _, err := att.Write([]byte("exit 7\n")); err != nil {
		t.Fatalf("写入 exit 失败: %v", err)
	}
	select {
	case <-att.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("shell 退出后会话没有结束")
	}
	exit, reason, hasExit := att.Exit()
	if reason != ReasonExit {
		t.Fatalf("结束原因应是 %q，得到 %q", ReasonExit, reason)
	}
	if !hasExit || !exit.HasCode || exit.Code != 7 {
		t.Fatalf("退出码读回不对: hasExit=%v hasCode=%v code=%d", hasExit, exit.HasCode, exit.Code)
	}
	// 已经结束的会话再关一次是**成功**（幂等）：DELETE 的目标状态已经成立。
	// 只有“别人的会话”才继续报“不存在”。
	if err := m.Close(CloseInput{SessionID: info.SessionID, Owner: owner, Reason: ReasonUser}); err != nil {
		t.Fatalf("结束后的会话重复关闭应成功（幂等），得到 %v", err)
	}
}

// TestManagerResize：窗口调整要真的落到 PTY 上，而不是只改会话表里的两个数字。
func TestManagerResize(t *testing.T) {
	m := newTestManager(t, Limits{})
	const owner int64 = 16
	info := createSession(t, m, owner)
	att, err := m.Attach(info.SessionID, owner)
	if err != nil {
		t.Fatalf("附加失败: %v", err)
	}
	defer att.Close()
	echoMarker(t, att, "resize-ready")

	if err := att.Resize(132, 43); err != nil {
		t.Fatalf("调整窗口失败: %v", err)
	}
	if view := att.Info(); view.Cols != 132 || view.Rows != 43 {
		t.Fatalf("会话表里的尺寸是 %dx%d，应为 132x43", view.Cols, view.Rows)
	}
	if _, err := att.Write([]byte("stty size\n")); err != nil {
		t.Fatalf("写入 tty 失败: %v", err)
	}
	// stty size 打印 “行 列”。
	out := readUntil(t, att, "43 132", 10*time.Second)
	if !strings.Contains(out, "43 132") {
		t.Fatalf("PTY 里的窗口尺寸不是 43x132：%q", out)
	}
}

// TestManagerLimits：单用户与全局上限都在 Create 里原子判定。
func TestManagerLimits(t *testing.T) {
	m := newTestManager(t, Limits{
		IdleTimeout: 30 * time.Second, MaxDuration: time.Minute,
		DisconnectGrace: 5 * time.Second, MaxPerUser: 1, MaxSessions: 2,
	})
	first := createSession(t, m, 21)
	defer m.Shutdown()

	if _, err := m.Create(context.Background(), CreateInput{Owner: 21, Params: Params{Cols: 80, Rows: 24}}); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("同一用户第二个会话应被拒绝（ErrLimitReached），得到 %v", err)
	}
	second := createSession(t, m, 22)
	if _, err := m.Create(context.Background(), CreateInput{Owner: 23, Params: Params{Cols: 80, Rows: 24}}); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("全局第三个会话应被拒绝（ErrLimitReached），得到 %v", err)
	}

	// Shutdown 把还活着的会话都结束掉，并给出 shutdown 这个原因。
	m.Shutdown()
	if _, err := m.Get(ListFilter{UserID: 21, SessionID: first.SessionID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("停机后会话仍在（err=%v）", err)
	}
	if _, err := m.Get(ListFilter{UserID: 22, SessionID: second.SessionID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("停机后第二个会话仍在（err=%v）", err)
	}
	if _, err := m.Create(context.Background(), CreateInput{Owner: 24, Params: Params{Cols: 80, Rows: 24}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("停机后仍能创建会话（err=%v）", err)
	}
}

// TestManagerParamsRejected：越界尺寸在进入 PTY 之前就被拒。
func TestManagerParamsRejected(t *testing.T) {
	m := newTestManager(t, Limits{})
	for _, p := range []Params{{Cols: 0, Rows: 24}, {Cols: 80, Rows: 501}, {Cols: 501, Rows: 24}} {
		if _, err := m.Create(context.Background(), CreateInput{Owner: 31, Params: p}); !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%+v 应被拒绝（ErrInvalidParams），得到 %v", p, err)
		}
	}
	if got, err := m.List(ListFilter{UserID: 31}); err != nil || len(got) != 0 {
		t.Fatalf("被拒的创建留下了会话: %d 个（err=%v）", len(got), err)
	}
}

// TestManagerConcurrentCreateRespectsLimits：并发创建不会一起越过上限。
func TestManagerConcurrentCreateRespectsLimits(t *testing.T) {
	m := newTestManager(t, Limits{
		IdleTimeout: 30 * time.Second, MaxDuration: time.Minute,
		DisconnectGrace: 5 * time.Second, MaxPerUser: 1, MaxSessions: 2,
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(owner int64) {
			defer wg.Done()
			if _, err := m.Create(context.Background(), CreateInput{
				Owner: owner, Params: Params{Cols: 80, Rows: 24},
			}); err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}(int64(40 + i))
	}
	wg.Wait()
	if created > 2 {
		t.Fatalf("并发创建出了 %d 个会话，越过全局上限 2", created)
	}
	total := 0
	for _, owner := range []int64{40, 41, 42, 43, 44, 45, 46, 47} {
		got, err := m.List(ListFilter{UserID: owner})
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(got) > 1 {
			t.Fatalf("用户 %d 出现了 %d 个会话（单用户上限 1）", owner, len(got))
		}
		total += len(got)
	}
	if total != created {
		t.Fatalf("表里有 %d 个会话，创建了 %d 个", total, created)
	}
}

// TestManagerFileBackedHomeMissing 证明 home 不可用时启动失败会带着原因返回，
// 而不是留下一个“看起来建好了、其实 shell 立刻死了”的会话。
func TestManagerFileBackedHomeMissing(t *testing.T) {
	acc := testAccount(t)
	acc.Home = filepath.Join(os.TempDir(), "upanel-term-missing-home-does-not-exist")
	m, err := NewManager(Options{Account: acc, Limits: testLimits()})
	if err != nil {
		t.Fatalf("构造会话层失败: %v", err)
	}
	defer m.Shutdown()
	if _, err := m.Create(context.Background(), CreateInput{
		Owner: 51, Params: Params{Cols: 80, Rows: 24},
	}); err == nil {
		t.Fatal("home 不存在时仍然创建了会话")
	}
	if got, err := m.List(ListFilter{UserID: 51}); err != nil || len(got) != 0 {
		t.Fatalf("失败的创建在表里留下了 %d 个会话（err=%v）", len(got), err)
	}
}
