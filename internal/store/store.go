// Package store 是 Web 侧的 SQLite 持久层：打开数据库、设置连接参数、事务工具和显式 SQL 迁移。
//
// 依据 PLAN.md 第 3 节：`database/sql` + `modernc.org/sqlite`（纯 Go，不用 cgo），
// SQLite 使用 WAL、开启 foreign_keys、设置 busy_timeout；数据库只放本机文件系统。
// 迁移是显式 SQL 文件（见 migrations/ 目录），不做 ORM 自动建表。
//
// 本包不实现任何业务查询。各业务包按阶段在 store 之上建立自己的仓储，并通过 WithTx
// 获取事务，保证“要么整笔生效、要么整笔回滚”。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	// 非空导入：除了注册 "sqlite" 驱动（纯 Go 实现，见 docs/DEPENDENCIES.md），
	// 还要用 *sqlite.Error 的 Code() 判断"写锁被别人占着"（见 isBusyError）。
	"modernc.org/sqlite"
)

const (
	// DefaultBusyTimeout 是数据库被其他连接写入时的等待上限。
	DefaultBusyTimeout = 5 * time.Second

	minBusyTimeout = 100 * time.Millisecond
	maxBusyTimeout = 60 * time.Second

	// driverName 是 modernc.org/sqlite 注册的驱动名。
	driverName = "sqlite"
)

// Options 是打开数据库的参数。
type Options struct {
	// Path 是数据库文件的绝对路径（生产为 /var/lib/upanel/upanel.db）。
	Path string
	// BusyTimeout 为 SQLITE_BUSY 等待上限；0 表示使用 DefaultBusyTimeout。
	BusyTimeout time.Duration
	// Logger 必须非空：打开与迁移过程都要留下可查记录。
	Logger *slog.Logger
}

// DB 是 Web 数据库句柄。零值不可用，必须用 Open 构造。
type DB struct {
	sql         *sql.DB
	path        string
	busyTimeout time.Duration
	logger      *slog.Logger
}

// Open 打开数据库并校验连接参数。
//
// 打开即失败的情形（全部返回错误，调用方必须拒绝启动）：
//   - 路径为空或不是绝对路径（避免跟随工作目录漂移）
//   - 数据目录不存在（不由本函数创建：目录归属由安装步骤决定）
//   - 驱动打开失败、Ping 失败
//   - WAL / foreign_keys / busy_timeout 没有按预期生效
func Open(ctx context.Context, opts Options) (*DB, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("store: 数据库路径不能为空")
	}
	if opts.Logger == nil {
		return nil, errors.New("store: Logger 不能为空")
	}
	if !filepath.IsAbs(opts.Path) {
		return nil, fmt.Errorf("store: 数据库路径必须是绝对路径，当前为 %q", opts.Path)
	}
	// DSN 用 ? 和 # 分隔参数，路径里出现这两个字符会造成歧义，直接拒绝。
	if strings.ContainsAny(opts.Path, "?#") {
		return nil, fmt.Errorf("store: 数据库路径不能包含 '?' 或 '#'，当前为 %q", opts.Path)
	}

	busy := opts.BusyTimeout
	if busy == 0 {
		busy = DefaultBusyTimeout
	}
	if busy < minBusyTimeout || busy > maxBusyTimeout {
		return nil, fmt.Errorf("store: busy_timeout 必须在 %s 与 %s 之间，当前为 %s",
			minBusyTimeout, maxBusyTimeout, busy)
	}

	dir := filepath.Dir(opts.Path)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("store: 数据目录不可用 %s: %w", dir, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("store: 数据目录 %s 不是目录", dir)
	}

	sqlDB, err := sql.Open(driverName, dsn(opts.Path, busy))
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库失败: %w", err)
	}
	// SQLite 单写者：限制连接数并允许并发读，配合 busy_timeout 处理写锁竞争。
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(0)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: 连接数据库失败 %s: %w", opts.Path, err)
	}

	db := &DB{sql: sqlDB, path: opts.Path, busyTimeout: busy, logger: opts.Logger}
	pragmas, err := db.verifyPragmas(ctx)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	// SQLite 按 umask 建文件（常见 0644），而库里有口令哈希与会话哈希：
	// 同机其他用户不该读得到，这里把文件权限收到 0600。
	if err := db.tightenFileModes(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	// 记录的是从数据库读回的实际值，而不是“我们期望的值”：日志要能当证据用。
	opts.Logger.Info("数据库已打开",
		"path", opts.Path,
		"journal_mode", pragmas.journalMode,
		"foreign_keys", pragmas.foreignKeys == 1,
		"busy_timeout_ms", pragmas.busyTimeoutMS,
	)
	return db, nil
}

// dsn 构造 modernc.org/sqlite 的连接串，把 WAL、foreign_keys、busy_timeout 写进连接参数，
// 使每条新连接都带上这些设置（PRAGMA 是连接级状态，不能只在一条连接上设置）。
//
// `_txlock=immediate` 不是可选项，是缺陷修复：默认的 DEFERRED 事务在 WAL 下先取读快照、
// 再尝试升级成写，**升级失败时 busy handler 不会被调用**，直接返回 SQLITE_BUSY。表现是
// “另一个写事务刚提交完 350 ms，这次请求还是 500”（P03.2 验收里真的撞到了：代理重启后
// 恢复未结束操作，紧接着的任务提交在 `UPDATE preflight_plans` 上拿到 database is locked）。
// IMMEDIATE 在 BEGIN 时就拿写锁，冲突交给 busy_timeout 排队等待，这才是“同时只有一个写者、
// 其余人排队”的语义。代价是只读事务也会拿写锁——本仓库的 WithTx 调用点全是写事务（业务变更
// + 审计同生共死），所以这个代价现在是零。
func dsn(path string, busy time.Duration) string {
	escaped := (&url.URL{Path: path}).EscapedPath()
	return "file:" + escaped +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		fmt.Sprintf("&_pragma=busy_timeout(%d)", busy.Milliseconds()) +
		"&_txlock=immediate"
}

// pragmaState 是从连接上读回的实际 PRAGMA 状态。
type pragmaState struct {
	journalMode   string
	foreignKeys   int
	busyTimeoutMS int64
}

// verifyPragmas 读回 PRAGMA 并逐项校验，返回值用于日志（记录实际值而非期望值）。
func (db *DB) verifyPragmas(ctx context.Context) (pragmaState, error) {
	var state pragmaState

	if err := db.sql.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&state.journalMode); err != nil {
		return state, fmt.Errorf("store: 读取 journal_mode 失败: %w", err)
	}
	if !strings.EqualFold(state.journalMode, "wal") {
		return state, fmt.Errorf("store: journal_mode 期望 wal，实际为 %q（数据库可能位于不支持 WAL 的文件系统）", state.journalMode)
	}

	if err := db.sql.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&state.foreignKeys); err != nil {
		return state, fmt.Errorf("store: 读取 foreign_keys 失败: %w", err)
	}
	if state.foreignKeys != 1 {
		return state, fmt.Errorf("store: foreign_keys 未开启（实际 %d），拒绝在不校验外键的情况下运行", state.foreignKeys)
	}

	if err := db.sql.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&state.busyTimeoutMS); err != nil {
		return state, fmt.Errorf("store: 读取 busy_timeout 失败: %w", err)
	}
	if time.Duration(state.busyTimeoutMS)*time.Millisecond != db.busyTimeout {
		return state, fmt.Errorf("store: busy_timeout 期望 %d ms，实际 %d ms", db.busyTimeout.Milliseconds(), state.busyTimeoutMS)
	}
	return state, nil
}

// SQL 返回底层 *sql.DB，供业务仓储使用。
func (db *DB) SQL() *sql.DB { return db.sql }

// tightenFileModes 把数据库文件及其 WAL/SHM 附属文件的权限收紧到 0600。
//
// SQLite 用 umask 决定新建文件的权限，默认常常是 0644，同机其他用户就能读到口令哈希和
// 会话哈希。目录权限属于部署（systemd 的 StateDirectory 与实例用户），这里只负责文件本身：
// 已有的其他可读写权限一律去掉，已经 0600 的不动，附属文件不存在则跳过。
// 非 POSIX 平台上 chmod 只影响只读位，函数不因此报错，也不假装权限已按 POSIX 语义生效。
func (db *DB) tightenFileModes() error {
	for _, name := range []string{db.path, db.path + "-wal", db.path + "-shm"} {
		info, err := os.Stat(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("store: 检查数据库文件权限失败 %s: %w", name, err)
		}
		if info.Mode().Perm()&0o077 == 0 {
			continue
		}
		if err := os.Chmod(name, info.Mode().Perm()&0o700|0o600); err != nil {
			return fmt.Errorf("store: 收紧数据库文件权限失败 %s: %w", name, err)
		}
	}
	return nil
}

// ExecContext 执行一条写语句：ctx 携带事务时用事务，否则用连接池。
//
// 需要“跟着当前事务走”的写操作用它，不要直接用 SQL()——后者总会独立提交，
// 破坏“业务与审计同生共死”的保证。
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx, ok := TxFromContext(ctx); ok {
		return tx.ExecContext(ctx, query, args...)
	}
	return db.sql.ExecContext(ctx, query, args...)
}

// Path 返回数据库文件路径。
func (db *DB) Path() string { return db.path }

// Logger 返回该数据库关联的日志器。
func (db *DB) Logger() *slog.Logger { return db.logger }

// Close 关闭数据库。可重复调用。
func (db *DB) Close() error {
	if db == nil || db.sql == nil {
		return nil
	}
	return db.sql.Close()
}

// WithTx 在一个事务中执行 fn。
//
// fn 返回错误或 panic 时回滚；只有 fn 正常返回且 Commit 成功才算提交。
// 业务代码必须通过它写库，不要手工拼 BEGIN/COMMIT。
//
// **事务传播**：如果 ctx 已经携带事务（由外层 WithTx 放入），本次调用直接使用外层事务，
// 不 begin、不 commit——提交或回滚都由最外层决定。这样“业务变更 + 审计”可以合成一笔事务
// （CONTRACTS.md 第 14.1 节 audit_events 只追加；P02.4 要求审计与业务同生共死），
// 又不需要把 *sql.Tx 一路穿过服务层签名。内层返回错误时必须**向上返回**，外层据此回滚。
func (db *DB) WithTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) (err error) {
	if tx, ok := TxFromContext(ctx); ok {
		if owner, ok := txOwnerFromContext(ctx); ok && owner != db {
			return errors.New("store: 事务属于另一个数据库句柄，不能跨库复用")
		}
		return fn(ctx, tx)
	}

	tx, err := db.beginImmediate(ctx)
	if err != nil {
		return fmt.Errorf("store: 开始事务失败: %w", err)
	}
	ctx = context.WithValue(context.WithValue(ctx, txContextKey{}, tx), txOwnerKey{}, db)

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = fn(context.WithValue(ctx, txContextKey{}, tx), tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return nil
}

// beginAttempts / beginBackoff 是"取写锁被挤掉"时的重试次数与首次退避。
//
// 三次、100ms 起步（100、300、900）是刻意的：等待总时长仍然是 busy_timeout 的量级，
// 不会让一个请求无限期挂着，但足以把"前面几个写者排队"这段时间等过去。
const (
	beginAttempts = 3
	beginBackoff  = 100 * time.Millisecond
)

// beginImmediate 取一个 IMMEDIATE 事务；写锁正被别人占着时重试有限次。
//
// 为什么要重试，而不是把错误交给调用方：BEGIN 失败时事务根本没开始，没有半截状态、
// 没有要回滚的东西——重试比任何业务层的重试都干净。而 busy_timeout 不够用的场景不是
// 死锁，是"排队时间被前面几个写者分完"：P12.4 真机现场（24.04，两块 vCPU）同时跑
// restic、两次 Argon2id 校验和 systemd-analyze 时，5 秒的等待被摊薄，一次正常的导入
// 或登录就拿到 `database is locked (5) (SQLITE_BUSY)`。对用户来说那是一次 500：
// 写操作明明可以排队，却被报成失败。
//
// 重试用尽仍然报错——不吞、不降级：真到了机器撑不住的程度，错误必须浮上来。
func (db *DB) beginImmediate(ctx context.Context) (*sql.Tx, error) {
	backoff := beginBackoff
	var lastErr error
	for attempt := 1; attempt <= beginAttempts; attempt++ {
		tx, err := db.sql.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		if !isBusyError(err) {
			return nil, err
		}
		lastErr = err
		if attempt == beginAttempts {
			break
		}
		// 留一行日志：排队排到要重试，说明这台机器此刻的写压力值得知道。
		db.logger.Warn("取写锁被占用，稍后重试", "attempt", attempt, "error", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%w（等待写锁时调用已取消：%v）", lastErr, ctx.Err())
		case <-timer.C:
		}
		backoff *= 3
	}
	return nil, lastErr
}

// isBusyError 判断错误是不是"写锁被别人占着"（可以重试）。
//
// 只认 SQLITE_BUSY(5) 与 SQLITE_LOCKED(6)：别的错误（约束冲突、磁盘满、库损坏）
// 重试多少次都是一样的结果，重试它们只会把真正的故障拖长。
func isBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() {
	case 5, 6:
		return true
	}
	return false
}

// txContextKey 是事务在 context 中的键（未导出类型，外部无法伪造或覆盖）。
type txContextKey struct{}

// txOwnerKey 记录事务属于哪个 *DB：Web 库与代理库都有事务时不能串用。
type txOwnerKey struct{}

func txOwnerFromContext(ctx context.Context) (*DB, bool) {
	if v := ctx.Value(txOwnerKey{}); v != nil {
		if db, ok := v.(*DB); ok && db != nil {
			return db, true
		}
	}
	return nil, false
}

// TxFromContext 取出 ctx 携带的事务。没有则返回 nil, false。
//
// 用途：需要“跟着当前事务走”的写入方（审计）据此判断该用事务还是数据库连接。
func TxFromContext(ctx context.Context) (*sql.Tx, bool) {
	if v := ctx.Value(txContextKey{}); v != nil {
		if tx, ok := v.(*sql.Tx); ok && tx != nil {
			return tx, true
		}
	}
	return nil, false
}
