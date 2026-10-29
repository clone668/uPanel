package backup

// 面板数据库的一致副本（P12.2；PLAN 第 17 节）。
//
// 要求原文是「面板数据库通过 SQLite 在线备份接口取得一致副本，不能仅复制正在运行的
// .db 而忽略 WAL」。所以这里**不是** os.Copy：直接拷贝主库文件会漏掉 -wal 里尚未
// 合并的事务，得到一个比"某个时刻"更旧、而且可能半截的库——而它看起来完全正常。
//
// 用的机制是 `VACUUM INTO`：SQLite 自己把它描述成"生成在线备份的另一种方式"，
// 它在一个读事务里读源库（连同 WAL），写出一份已整理、已自洽的新库。读连接以
// `mode=ro` 打开——源库一个字节都不会被改（这条在真机与单元测试里都验了：只读连接上
// 的 VACUUM INTO 能读到 WAL 里最新提交的行，而写回去会直接报 readonly）。
//
// 一个坑记在这里：DSN 上再加 `_pragma=query_only(1)` 会让 VACUUM INTO 报
// "attempt to write a readonly database"。测试里钉住了这一条，免得下次有人"顺手加固"
// 把备份弄坏在原地。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// 面板库副本相关的常量。
const (
	// PanelCopyBudget 是一次在线副本的时间上限。面板库是元数据库（几百 KB 到几十 MB），
	// 整份重写在秒级；两分钟是"慢到不该再等"的量级。
	PanelCopyBudget = 2 * time.Minute
	// panelCopyDirMode / panelCopyFileMode 是副本目录与文件的权限。
	// 副本里有口令哈希与会话记录，和源库一样只给 root。
	panelCopyDirMode  os.FileMode = 0o700
	panelCopyFileMode os.FileMode = 0o600
)

// ErrPanelDatabaseMissing 表示面板库文件不存在。
var ErrPanelDatabaseMissing = errors.New("backup: 面板数据库文件不存在")

// PanelCopy 是一次一致副本的结果。
type PanelCopy struct {
	// SourcePath 是源库路径（只读打开）。
	SourcePath string
	// CopyPath 是副本路径（已落盘、已校验）。
	CopyPath string
	// SizeBytes 是副本大小。
	SizeBytes int64
	// SchemaVersion 是副本的 PRAGMA user_version（恢复时的兼容性判断用它）。
	SchemaVersion int64
	// SourceSchemaVersion 是源库的 user_version；两者不等说明备份期间有人迁移了库
	// ——那时副本仍然自洽，但"它对应哪个版本的 schema"要说清楚。
	SourceSchemaVersion int64
	// PageCount 是副本的页数（读数，不是推算）。
	PageCount int64
	// Integrity 是 `PRAGMA integrity_check` 的结果（正常是 "ok"）。
	Integrity string
	// CopiedAt 是副本生成时间。
	CopiedAt time.Time
}

// CopyPanelDatabase 取一份面板数据库的一致副本到 destPath。
//
// 调用前 destPath 的父目录必须已存在（调用方负责建，并在失败时清理）；本函数会先删掉
// 同名的旧文件，避免"这次没写成、上次的副本被当成本次的"。
func CopyPanelDatabase(ctx context.Context, srcPath, destPath string) (PanelCopy, error) {
	out := PanelCopy{SourcePath: srcPath, CopyPath: destPath}
	if strings.TrimSpace(srcPath) == "" || strings.TrimSpace(destPath) == "" {
		return out, errors.New("backup: 面板库副本的源与目标都不能为空")
	}
	if !filepath.IsAbs(srcPath) || !filepath.IsAbs(destPath) {
		return out, errors.New("backup: 面板库副本的源与目标必须是绝对路径")
	}
	if same, err := sameFile(srcPath, destPath); err != nil {
		return out, err
	} else if same {
		return out, errors.New("backup: 面板库副本的目标不能是源文件本身")
	}
	if _, err := os.Stat(srcPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, fmt.Errorf("%w：%s", ErrPanelDatabaseMissing, srcPath)
		}
		return out, fmt.Errorf("backup: 读不到面板数据库 %s: %w", srcPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), panelCopyDirMode); err != nil {
		return out, fmt.Errorf("backup: 创建副本目录失败: %w", err)
	}
	// 先删旧文件与其 WAL/SHM：残留会让"这次没写成"变成"上次的副本顶替本次"。
	for _, p := range []string{destPath, destPath + "-wal", destPath + "-shm", destPath + "-journal"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return out, fmt.Errorf("backup: 清理旧副本 %s 失败: %w", p, err)
		}
	}

	if err := vacuumInto(ctx, srcPath, destPath); err != nil {
		return out, err
	}

	info, err := os.Stat(destPath)
	if err != nil {
		return out, fmt.Errorf("backup: 副本没有落盘: %w", err)
	}
	if info.Size() == 0 {
		return out, errors.New("backup: 副本是空文件")
	}
	if err := os.Chmod(destPath, panelCopyFileMode); err != nil {
		return out, fmt.Errorf("backup: 设置副本权限失败: %w", err)
	}
	out.SizeBytes = info.Size()
	out.CopiedAt = time.Now().UTC()

	srcSchema, err := schemaVersion(ctx, srcPath)
	if err != nil {
		return out, err
	}
	dstSchema, pages, integrity, err := verifyPanelCopy(ctx, destPath)
	if err != nil {
		return out, err
	}
	out.SourceSchemaVersion = srcSchema
	out.SchemaVersion = dstSchema
	out.PageCount = pages
	out.Integrity = integrity
	if integrity != "ok" {
		return out, fmt.Errorf("backup: 面板库副本没有通过完整性检查：%s", integrity)
	}
	if srcSchema != dstSchema {
		return out, fmt.Errorf("backup: 副本 schema 版本 %d 与源库 %d 不一致（备份期间有人迁移了库，请重跑）",
			dstSchema, srcSchema)
	}
	return out, nil
}

// vacuumInto 在只读连接上跑 `VACUUM INTO`。
func vacuumInto(ctx context.Context, srcPath, destPath string) error {
	dsn := "file:" + srcPath + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("backup: 打开面板数据库失败: %w", err)
	}
	defer func() { _ = db.Close() }()
	// 连接池只留一条：VACUUM INTO 必须在同一个连接上完成，多连接会让
	// "只读"与"读到同一时刻"这两件事都失去意义。
	db.SetMaxOpenConns(1)

	runCtx, cancel := context.WithTimeout(ctx, PanelCopyBudget)
	defer cancel()
	// 目标路径是我们自己拼出来的（父目录是代理的 StateDir），仍然按字面量转义引号：
	// VACUUM INTO 的语法不接受绑定参数（真机上验过：绑定参数会报 readonly）。
	stmt := `VACUUM INTO '` + strings.ReplaceAll(destPath, "'", "''") + `'`
	if _, err := db.ExecContext(runCtx, stmt); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("backup: 面板库在线副本超过 %s 没有完成", PanelCopyBudget)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w（面板库在线副本被中止）", ErrBudgetExhausted)
		}
		return fmt.Errorf("backup: 面板库在线副本失败: %w", err)
	}
	return nil
}

// schemaVersion 读 user_version。
func schemaVersion(ctx context.Context, path string) (int64, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, fmt.Errorf("backup: 打开数据库失败: %w", err)
	}
	defer func() { _ = db.Close() }()
	var v int64
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("backup: 读取 schema 版本失败: %w", err)
	}
	return v, nil
}

// verifyPanelCopy 校验副本：完整性、schema 版本、页数。
//
// 只校验不读内容：副本里是别的进程的数据，这一层不该把它解析成业务对象。
func verifyPanelCopy(ctx context.Context, path string) (schema int64, pages int64, integrity string, err error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, 0, "", fmt.Errorf("backup: 打开副本失败: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return 0, 0, "", fmt.Errorf("backup: 副本完整性检查失败: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schema); err != nil {
		return 0, 0, "", fmt.Errorf("backup: 读取副本 schema 版本失败: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, 0, "", fmt.Errorf("backup: 读取副本页数失败: %w", err)
	}
	return schema, pages, integrity, nil
}

// sameFile 判断两个路径是否指向同一个文件（不存在时按不同处理）。
func sameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(ai, bi), nil
}
