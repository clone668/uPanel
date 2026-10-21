package terminal

// 代理侧的会话层（P06.3）。
//
// 这一层只做四件事：把会话表管好、把限额与超时执行到位、把 PTY 的生命周期对齐、
// 把“谁连着谁”说清楚。它不解析用户的输入，也不知道 shell 里发生了什么——输入只是
// 写进 tty 的字节，输出只是从 tty 读出的字节。终端全文不进日志、不进审计
// （PLAN.md 第 14 节）。
//
// 并发模型：一把锁护住整张会话表。会话是低频对象（全局最多 2 个），用一把锁换来的
// 是“计数、创建、关闭、扫描超时”全都在同一份真相上判定——如果分开加锁，
// 两个并发的 create 可能各自看到“还有空位”而同时创建，限额就形同虚设。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ExitStatus 是子进程的退出状态。
type ExitStatus struct {
	// Code 是退出码；HasCode 为 false 表示进程被信号终止（此时 Code 无意义）。
	Code    int
	HasCode bool
	// Signal 是终止进程的信号编号；0 表示不是被信号终止的。
	Signal int
	// Err 是等待过程本身的错误（fork 之后极罕见），不是进程的退出码。
	Err error
}

// Account 是运行终端的系统账号。
//
// 它不是“面板账号”，而是 PTY 里那个 shell 的 uid/gid。PLAN.md 第 4 节把它固定为
// upanel-shell：独立 home、无 sudo、不加入 docker/disk 等特权组。
type Account struct {
	User string
	UID  uint32
	GID  uint32
	// Home 是 shell 的工作目录（PLAN.md 第 4 节：/var/lib/upanel-shell）。
	Home string
	// Shell 是 shell 可执行文件的绝对路径。
	Shell string
}

// Options 是构造会话层的参数。
type Options struct {
	// Account 必须是已校验过的降权账号（UID/GID 非 0）。
	Account Account
	// Limits 为实际生效的限额；零值表示使用契约默认值。
	Limits Limits
	// Logger 允许为 nil（测试）。
	Logger *slog.Logger
	// Now 允许注入时钟；nil 时用 time.Now。测试用它把 15 分钟压缩成几毫秒的判定。
	Now func() time.Time
	// NewID 允许注入 id 生成；nil 时用 crypto/rand。
	NewID func() (string, error)
}

// Manager 是终端会话表。
type Manager struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// session 是一个活的 PTY 会话。
type session struct {
	id         string
	owner      int64
	ownerName  string
	account    Account
	createdAt  time.Time
	lastActive time.Time
	attachedAt *time.Time
	cols, rows int
	attached   bool
	detachedAt time.Time

	// sink 是当前连着的那条连接（没有连接时为 nil）。输出读泵每个会话只有**一个**，
	// 它把字节交给 sink；断线宽限期内没有 sink，这段输出按“实时通道”的语义丢弃
	// （PLAN.md 第 14 节：0.1 不做 tmux 式持久终端，没有回滚缓冲可交）。
	sink *Attachment

	proc ptyProcess

	// done 在会话结束时关闭（子进程退出、超时、用户关闭、代理停机）。
	done   chan struct{}
	closed bool
	reason string
	exit   ExitStatus
	hasEx  bool
	// handedOff 表示接下来由调用方负责关闭 pty：正常退出时保留 master 直到
	// 读端自然结束，避免丢掉最后一段输出。
	handedOff bool
}

// NewManager 构造会话层。
func NewManager(opts Options) (*Manager, error) {
	if opts.Account.User == "" {
		return nil, fmt.Errorf("%w: 缺少终端账号", ErrAccountInvalid)
	}
	if opts.Account.UID == 0 {
		return nil, fmt.Errorf("%w: 终端账号 %q 的 uid 不能是 0", ErrAccountInvalid, opts.Account.User)
	}
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = randomSessionID
	}
	return &Manager{opts: opts, sessions: make(map[string]*session)}, nil
}

// Account 返回运行终端的账号（只读快照）。
func (m *Manager) Account() Account { return m.opts.Account }

// Limits 返回实际生效的限额。
func (m *Manager) Limits() Limits { return m.opts.Limits }

// randomSessionID 生成 128 位随机 id（32 位小写十六进制）。
func randomSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("terminal: 生成会话 id 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Create 创建一个会话。
//
// 顺序是刻意的：先在锁内核对限额，再 fork/exec。反过来（先建后数）会让并发的
// 两次创建都成功，然后靠“事后关掉一个”来收场——那对一个交互终端来说毫无意义。
func (m *Manager) Create(ctx context.Context, in CreateInput) (SessionInfo, error) {
	if err := in.Params.Validate(); err != nil {
		return SessionInfo{}, err
	}
	if in.Owner <= 0 {
		return SessionInfo{}, fmt.Errorf("%w: owner 必须为正整数", ErrInvalidParams)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return SessionInfo{}, ErrClosed
	}
	now := m.opts.Now()
	m.sweepLocked(now)
	perUser, total := 0, 0
	for _, s := range m.sessions {
		if s.owner == in.Owner {
			perUser++
		}
		total++
	}
	limits := m.opts.Limits
	if perUser >= limits.MaxPerUser {
		m.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("%w: 每个账号最多 %d 个终端会话", ErrLimitReached, limits.MaxPerUser)
	}
	if total >= limits.MaxSessions {
		m.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("%w: 全局最多 %d 个终端会话", ErrLimitReached, limits.MaxSessions)
	}
	id, err := m.opts.NewID()
	if err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	if !IsValidSessionID(id) {
		m.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("terminal: 生成的会话 id 形态不合法")
	}

	// fork/exec 在锁内完成：它只有几毫秒，而放到锁外就要面对“同一毫秒内两次创建
	// 各自通过了限额检查”的问题。
	proc, err := startPTY(spawnSpec{
		Account: m.opts.Account,
		Cols:    in.Params.Cols,
		Rows:    in.Params.Rows,
	})
	if err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}

	s := &session{
		id:         id,
		owner:      in.Owner,
		ownerName:  in.OwnerName,
		account:    m.opts.Account,
		createdAt:  now,
		lastActive: now,
		cols:       in.Params.Cols,
		rows:       in.Params.Rows,
		proc:       proc,
		done:       make(chan struct{}),
	}
	m.sessions[id] = s
	info := s.info(now, limits)
	m.mu.Unlock()

	// 两个泵都在锁外起：读泵每会话一个（见 readLoop 的注释），等泵负责回收。
	go m.readLoop(s)
	go m.waitLoop(s)

	m.opts.Logger.Info("终端会话已创建",
		"session_id", id,
		"owner_user_id", in.Owner,
		"owner", in.OwnerName,
		"account", m.opts.Account.User,
		"account_uid", m.opts.Account.UID,
		"pid", proc.Pid(),
		"cols", in.Params.Cols,
		"rows", in.Params.Rows)
	return info, nil
}

// outputChunkSize 是一次从 PTY 读出的字节数。
const outputChunkSize = 32 << 10

// attachmentChunks 是每条连接允许积压的输出块数。
//
// 满了就丢新块，而不是阻塞读泵：终端是实时通道，卡住的连接不该把整台机器的 PTY
// 读端一起按住（那会让 shell 的 write 阻塞在一个已经没人看的东西上）。128 块
// × 32 KiB = 4 MiB 的余量足够吸收一次滚屏，再多的积压说明对端已经不读了。
const attachmentChunks = 128

// readLoop 是**每个会话一个**的输出读泵。
//
// 为什么不是“每条连接一个”：断线宽限期内没有连接，但 PTY 还活着（不然重连就是
// 接到一具尸体上）。如果读泵属于连接，断开时就必须关掉 master 才能让它退出——
// 而关 master 会给前台进程组发 SIGHUP，shell 立刻死，宽限期形同虚设。所以读泵
// 属于会话，连接只是它的 sink。
//
// 读泵退出意味着“tty 那头没了”（子进程退出、master 被会话结束关掉），此时由它
// 关掉 master：master 的关闭权归读泵，别人只在结束会话时催一下（handOffClose 幂等）。
func (m *Manager) readLoop(s *session) {
	defer s.handOffClose()
	buf := make([]byte, outputChunkSize)
	for {
		n, err := s.proc.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			m.deliver(s, chunk)
		}
		if err != nil {
			return
		}
	}
}

// deliver 把一块输出交给当前连接；没有连接时丢弃。
func (m *Manager) deliver(s *session, chunk []byte) {
	m.mu.Lock()
	sink := s.sink
	m.mu.Unlock()
	if sink == nil {
		return
	}
	sink.push(chunk)
}

// waitLoop 等子进程退出，并把会话结束的结论落到表上。
func (m *Manager) waitLoop(s *session) {
	exit := s.proc.Wait()

	m.mu.Lock()
	s.exit = exit
	s.hasEx = true
	// 正常退出**不**立刻关 master：读端要先把 tty 缓冲区里剩下的输出读完，
	// 客户端才能看到 shell 最后打印的那几行（登录提示、exit 的回应）。
	// master 由连接结束时统一关闭，所以这里只标记 handedOff。
	s.handedOff = true
	first := m.finishLocked(s, ReasonExit)
	owner, name, account := s.owner, s.ownerName, s.account.User
	m.mu.Unlock()

	if first {
		m.opts.Logger.Info("终端会话已结束",
			"session_id", s.id,
			"owner_user_id", owner,
			"owner", name,
			"account", account,
			"reason", ReasonExit,
			"exit_code", exit.Code,
			"has_exit_code", exit.HasCode,
			"signal", exit.Signal)
	}
}

// finishLocked 把会话标记为结束并唤醒等待者。返回是否是第一次结束。
// 调用方必须持有 m.mu。
func (m *Manager) finishLocked(s *session, reason string) bool {
	if s.closed {
		return false
	}
	s.closed = true
	s.reason = reason
	s.attached = false
	delete(m.sessions, s.id)
	close(s.done)
	return true
}

// sweepLocked 清理超时的会话，返回**已经移出表**的会话列表：真正的关闭动作
// （关 pty、记日志）由调用方在锁外完成——在锁里关文件描述符会把一次慢系统调用
// 变成整张表的等待。
func (m *Manager) sweepLocked(now time.Time) []*session {
	limits := m.opts.Limits
	var expired []*session
	for _, s := range m.sessions {
		reason := ""
		switch {
		case !now.Before(s.createdAt.Add(limits.MaxDuration)):
			reason = ReasonExpired
		case !now.Before(s.lastActive.Add(limits.IdleTimeout)):
			reason = ReasonIdle
		case !s.attached && !s.detachedAt.IsZero() && !now.Before(s.detachedAt.Add(limits.DisconnectGrace)):
			reason = ReasonDisconnect
		}
		if reason == "" {
			continue
		}
		if m.finishLocked(s, reason) {
			expired = append(expired, s)
		}
	}
	return expired
}

// closeExpired 关闭被 sweepLocked 摘出来的会话。
func (m *Manager) closeExpired(list []*session) {
	for _, s := range list {
		s.handOffClose()
		m.opts.Logger.Info("终端会话超时结束",
			"session_id", s.id,
			"owner_user_id", s.owner,
			"account", s.account.User,
			"reason", s.reason)
	}
}

// Run 是后台清理循环，ctx 结束（代理停机）时退出。
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			expired := m.sweepLocked(m.opts.Now())
			m.mu.Unlock()
			m.closeExpired(expired)
		}
	}
}

// Shutdown 关闭全部会话（代理停机时调用）。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	list := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if m.finishLocked(s, ReasonShutdown) {
			list = append(list, s)
		}
	}
	m.mu.Unlock()
	for _, s := range list {
		s.handOffClose()
	}
}

// Close 关闭一个会话（用户主动关闭）。
//
// 幂等：会话已经不在了（子进程自己退出、到期、被回收、刚刚被同一个人关掉）时返回
// 成功，因为 DELETE 的目标状态“它现在不跑了”已经成立。返回 404 会让调用方以为关错
// 了对象，继续拿着一个已经不存在的 id 重试。别人的会话对外仍然是“不存在”——这条
// 与 Get 一致：区分“没有这个会话”和“不是你的会话”只会告诉对方别人的 id 是否有效。
func (m *Manager) Close(in CloseInput) error {
	sessionID := in.SessionID
	if !IsValidSessionID(sessionID) {
		return fmt.Errorf("%w: session_id 形态不合法", ErrInvalidParams)
	}
	if in.Owner <= 0 {
		return fmt.Errorf("%w: owner 必须为正整数", ErrInvalidParams)
	}

	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	if s.owner != in.Owner {
		m.mu.Unlock()
		return ErrNotFound
	}
	reason := normaliseReason(in.Reason)
	first := m.finishLocked(s, reason)
	owner, account := s.owner, s.account.User
	m.mu.Unlock()
	if !first {
		return nil
	}
	s.handOffClose()
	m.opts.Logger.Info("终端会话被关闭",
		"session_id", sessionID,
		"owner_user_id", owner,
		"account", account,
		"reason", reason)
	return nil
}

// Get 返回单个会话的视图（仅当它属于 filter.UserID）。
func (m *Manager) Get(filter ListFilter) (SessionInfo, error) {
	if err := filter.Validate(); err != nil {
		return SessionInfo{}, err
	}
	if filter.SessionID == "" {
		return SessionInfo{}, fmt.Errorf("%w: 需要 session_id", ErrInvalidParams)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[filter.SessionID]
	// 不属于该用户的会话对外一律“不存在”：区分“没有这个会话”和“不是你的会话”
	// 只会告诉调用方别人的会话 id 是否有效。
	if !ok || s.owner != filter.UserID {
		return SessionInfo{}, ErrNotFound
	}
	return s.info(m.opts.Now(), m.opts.Limits), nil
}

// List 返回某个用户的会话，按创建时间升序。
func (m *Manager) List(filter ListFilter) ([]SessionInfo, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opts.Now()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.owner != filter.UserID {
			continue
		}
		if filter.SessionID != "" && s.id != filter.SessionID {
			continue
		}
		out = append(out, s.info(now, m.opts.Limits))
	}
	// 稳定顺序：会话很少，但“列表顺序随 map 迭代变化”会让页面无端重排。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.Before(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Attach 把一条连接接到会话上。
//
// 同一时刻只允许一条连接：两条连接同时写同一个 tty，输入会交错成谁都读不懂的东西，
// 而且“谁在敲”这件事再也说不清。断线宽限期内重连是允许的——那时旧连接已经 detach。
func (m *Manager) Attach(sessionID string, owner int64) (*Attachment, error) {
	if !IsValidSessionID(sessionID) {
		return nil, fmt.Errorf("%w: session_id 形态不合法", ErrInvalidParams)
	}
	if owner <= 0 {
		return nil, fmt.Errorf("%w: owner 必须为正整数", ErrInvalidParams)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrNotFound
	}
	if s.owner != owner {
		return nil, ErrNotOwner
	}
	if s.attached {
		return nil, ErrAlreadyAttached
	}
	now := m.opts.Now()
	s.attached = true
	s.detachedAt = time.Time{}
	s.lastActive = now
	attached := now
	s.attachedAt = &attached
	att := &Attachment{
		m:    m,
		s:    s,
		out:  make(chan []byte, attachmentChunks),
		done: make(chan struct{}),
	}
	s.sink = att
	return att, nil
}

// Attachment 是一条到会话的连接。
//
// 它不拥有 PTY：断开只是把会话切回“未连接”，PTY 由会话的读泵继续读到宽限期结束。
type Attachment struct {
	m *Manager
	s *session

	// out 是会话读泵交给这条连接的输出块。
	out chan []byte
	// done 在这条连接断开时关闭（Close 只关一次）。
	done chan struct{}
	// pending 是上一块里还没被上层取走的部分。
	pending []byte

	mu     sync.Mutex
	closed bool
}

// push 把一块输出放进这条连接的缓冲；缓冲满就丢掉这一块。
//
// 丢掉而不是阻塞：阻塞会让会话的读泵停摆，而读泵停摆会让 shell 的 write 卡住——
// 一个已经断开却还没来得及 Close 的连接，不该有能力影响终端本身。
func (a *Attachment) push(chunk []byte) {
	select {
	case a.out <- chunk:
	case <-a.done:
	default:
	}
}

// Read 读取终端输出。会话结束或连接断开后返回 io.EOF。
func (a *Attachment) Read(p []byte) (int, error) {
	for {
		if len(a.pending) > 0 {
			n := copy(p, a.pending)
			a.pending = a.pending[n:]
			return n, nil
		}
		// 先把已经积压的块交完，再等下一块或断开信号：先非阻塞取一次，
		// 避免 select 在“有数据”和“已断开”同时就绪时随机丢掉最后一段输出。
		select {
		case chunk, ok := <-a.out:
			if !ok {
				return 0, io.EOF
			}
			a.pending = chunk
			continue
		default:
		}
		select {
		case chunk, ok := <-a.out:
			if !ok {
				return 0, io.EOF
			}
			a.pending = chunk
		case <-a.done:
			return 0, io.EOF
		}
	}
}

// Write 把输入写进 tty。
func (a *Attachment) Write(p []byte) (int, error) {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	n, err := a.s.proc.Write(p)
	if n > 0 {
		a.m.mu.Lock()
		a.s.lastActive = a.m.opts.Now()
		a.m.mu.Unlock()
	}
	return n, err
}

// Resize 调整窗口大小。
func (a *Attachment) Resize(cols, rows int) error {
	p := Params{Cols: cols, Rows: rows}
	if err := p.Validate(); err != nil {
		return err
	}
	if err := a.s.proc.Resize(cols, rows); err != nil {
		return err
	}
	a.m.mu.Lock()
	a.s.cols, a.s.rows = cols, rows
	a.s.lastActive = a.m.opts.Now()
	a.m.mu.Unlock()
	return nil
}

// Info 返回会话视图。
func (a *Attachment) Info() SessionInfo {
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	return a.s.info(a.m.opts.Now(), a.m.opts.Limits)
}

// Done 在会话结束时关闭。
func (a *Attachment) Done() <-chan struct{} { return a.s.done }

// Exit 返回退出状态；ok 为 false 表示会话还没结束（或不是子进程退出导致的结束）。
func (a *Attachment) Exit() (ExitStatus, string, bool) {
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	return a.s.exit, a.s.reason, a.s.hasEx
}

// Close 断开连接（不是结束会话）。
//
// **不关 PTY**：断开之后会话进入宽限期，这段时间里 shell 还活着，重连接回的是
// 同一个 PTY（CONTRACTS.md 第 10 节：允许宽限期内重连）。PTY 只由会话结束关掉
// （子进程退出、空闲、到期、宽限期满、用户关闭、代理停机），见 finishLocked 的
// 各个调用点与 readLoop。
func (a *Attachment) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	close(a.done)
	a.mu.Unlock()

	m := a.m
	m.mu.Lock()
	s := a.s
	if s.attached {
		s.attached = false
		s.detachedAt = m.opts.Now()
	}
	if s.sink == a {
		s.sink = nil
	}
	live := !s.closed
	m.mu.Unlock()

	if live {
		m.opts.Logger.Info("终端连接断开", "session_id", s.id, "owner_user_id", s.owner,
			"grace_seconds", int(m.opts.Limits.DisconnectGrace.Seconds()))
	}
}

// handOffClose 关闭 pty。多次调用安全（ptyProcess.Close 幂等）。
func (s *session) handOffClose() { _ = s.proc.Close() }

// info 生成会话视图。调用方必须持有 m.mu（或持有 s 的生命周期）。
func (s *session) info(now time.Time, limits Limits) SessionInfo {
	info := SessionInfo{
		SessionID:      s.id,
		UserID:         s.owner,
		Account:        s.account.User,
		AccountUID:     s.account.UID,
		State:          StateDetached,
		Cols:           s.cols,
		Rows:           s.rows,
		CreatedAt:      s.createdAt,
		LastActivityAt: s.lastActive,
		IdleDeadline:   s.lastActive.Add(limits.IdleTimeout),
		ExpiresAt:      s.createdAt.Add(limits.MaxDuration),

		IdleTimeoutSeconds: int(limits.IdleTimeout.Seconds()),
		MaxDurationSeconds: int(limits.MaxDuration.Seconds()),
	}
	if s.attached {
		info.State = StateAttached
	}
	if s.attachedAt != nil {
		at := *s.attachedAt
		info.AttachedAt = &at
	}
	return info
}
