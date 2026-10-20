package agentstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"upanel/internal/store"
)

// 代理数据库的固定路径与权限（PLAN.md 第 4 节：代理数据 root 独占）。
const (
	// DefaultPath 是代理数据库的默认路径。
	DefaultPath = "/var/lib/upanel-agent/agent.db"
	// DataDirMode 是代理数据目录的权限：只有 root 能进入与写入。
	DataDirMode os.FileMode = 0o700
	// DatabaseFileMode 是数据库文件（含 WAL/SHM）的权限。
	DatabaseFileMode os.FileMode = 0o600
)

// ErrDataDir 表示代理数据目录不存在、不归 root 独占，或不可写。
var ErrDataDir = errors.New("agentstore: 代理数据目录不可用")

// Options 是打开代理数据库的参数。
type Options struct {
	// Path 是数据库文件的绝对路径（生产为 /var/lib/upanel-agent/agent.db）。
	Path string
	// BusyTimeout 为 SQLITE_BUSY 等待上限；0 表示使用 store 的默认值。
	BusyTimeout time.Duration
	// Logger 必须非空：打开与迁移都要留下可查记录。
	Logger *slog.Logger
}

// DB 是代理数据库句柄。零值不可用，必须用 Open 构造。
type DB struct {
	inner *store.DB
	path  string
}

// Open 打开代理数据库，并在打开前后各校验一次目录与文件归属。
//
// 拒绝打开的情形（全部返回错误，调用方必须拒绝启动）：
//   - 路径不是绝对路径；
//   - 数据目录不存在、属主不是 root、或对属组/其他用户可写（否则别人能替换数据库文件）；
//   - 底层 SQLite 打开失败或 PRAGMA 未生效（由 internal/store 判定）；
//   - 打开后数据库文件不是 root:root 或对属组/其他用户可读。
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Logger == nil {
		return nil, errors.New("agentstore: Logger 不能为空")
	}
	if opts.Path == "" {
		return nil, errors.New("agentstore: 数据库路径不能为空")
	}
	if !filepath.IsAbs(opts.Path) {
		return nil, fmt.Errorf("agentstore: 数据库路径必须是绝对路径，当前为 %q", opts.Path)
	}

	dir := filepath.Dir(opts.Path)
	if err := requireRootOnlyDir(dir); err != nil {
		return nil, fmt.Errorf("%w: %v（应执行 `install -d -o root -g root -m %#o %s`）",
			ErrDataDir, err, DataDirMode, dir)
	}

	inner, err := store.Open(ctx, store.Options{
		Path:        opts.Path,
		BusyTimeout: opts.BusyTimeout,
		Logger:      opts.Logger,
	})
	if err != nil {
		return nil, err
	}

	// store.Open 已经把文件权限收到 0600，但归属取决于创建者：如果有人在代理首次启动前
	// 预置了一个 upanel-web 拥有的文件，这里必须拦住，而不是带着别人的文件继续跑。
	if err := requireRootOnlyFile(opts.Path, DatabaseFileMode); err != nil {
		_ = inner.Close()
		return nil, fmt.Errorf("%w: %v", ErrDataDir, err)
	}
	return &DB{inner: inner, path: opts.Path}, nil
}

// Path 返回数据库文件路径。
func (db *DB) Path() string { return db.path }

// Migrate 应用代理数据库的迁移（文件集合由调用方给定，生产为 migrations/agentdb.FS）。
func (db *DB) Migrate(ctx context.Context, fsys fs.FS) (store.MigrateResult, error) {
	return db.inner.Migrate(ctx, fsys)
}

// Close 关闭数据库。
func (db *DB) Close() error { return db.inner.Close() }

// store 返回底层句柄，供同包内的仓储使用。
func (db *DB) store() *store.DB { return db.inner }
