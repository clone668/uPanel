package agentstore

// 受管理脚本的持久化（CONTRACTS.md 第 5、12、14.2 节；P12.4）。
//
// 这张表存在的理由是**"批准的是哪一份"**：脚本内容与它的哈希在同一次事务里写进来，
// 落盘文件的字节永远由这里的正文派生。运行前再核一次磁盘上的哈希——对不上就不跑，
// 而不是"文件里是什么就跑什么"。没有这一条，任何一个能写那个目录的人（或任何一次
// 手工编辑）都能改掉一条已经获批的定时任务。
//
// 这一层不认识 systemd，也不做降权：单元与进程身份分别属于代理动作层。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"upanel/internal/scripts"
)

const scriptPrefix = "scr_"

// ScriptRecord 是代理库里的一条受管理脚本。
//
// 别名：形状由 internal/scripts 定义（正文、指纹、落盘路径三者的关系在那里说清楚）。
type ScriptRecord = scripts.Record

var (
	// ErrScriptNotFound 表示脚本 id 不在库里。
	ErrScriptNotFound = errors.New("agentstore: 脚本不存在")
	// ErrScriptVersion 表示期望版本与当前版本不一致（乐观并发冲突）。
	ErrScriptVersion = errors.New("agentstore: 脚本版本冲突")
	// ErrScriptName 表示名字已被占用。
	ErrScriptName = errors.New("agentstore: 脚本名字已被占用")
)

// ScriptStore 是受管理脚本的仓储。
type ScriptStore struct {
	db  *DB
	dir string
}

// NewScriptStore 构造仓储。dir 为空时用 scripts.DefaultDir。
func NewScriptStore(db *DB, dir string) (*ScriptStore, error) {
	if db == nil {
		return nil, errors.New("agentstore: 脚本仓储需要数据库句柄")
	}
	if dir == "" {
		dir = scripts.DefaultDir
	}
	return &ScriptStore{db: db, dir: dir}, nil
}

// Dir 返回脚本目录（供动作层落盘时使用，保证与 Ref 同源）。
func (s *ScriptStore) Dir() string { return s.dir }

// List 返回全部脚本（按名字排序）。它返回**完整记录**，含正文：这是代理库，
// 调用方是代理动作层；公开接口只把元数据交给 Web 的 GET /scripts。
func (s *ScriptStore) List(ctx context.Context) ([]ScriptRecord, error) {
	rows, err := s.db.store().SQL().QueryContext(ctx, `
		SELECT id, name, content, content_hash, agent_script_ref, interpreter, version,
		       created_at, updated_at
		  FROM scripts ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取脚本列表失败: %w", err)
	}
	defer rows.Close()
	var out []ScriptRecord
	for rows.Next() {
		rec, err := scanScript(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取脚本列表失败: %w", err)
	}
	return out, nil
}

// Get 取一条脚本；不存在时返回 (零值, false, nil)。
func (s *ScriptStore) Get(ctx context.Context, id string) (ScriptRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, name, content, content_hash, agent_script_ref, interpreter, version,
		       created_at, updated_at
		  FROM scripts WHERE id = ?`, id)
	rec, err := scanScript(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScriptRecord{}, false, nil
	}
	if err != nil {
		return ScriptRecord{}, false, err
	}
	return rec, true, nil
}

// Create 新建一条脚本并生成 id。输入必须已经 Normalize + Validate 过。
func (s *ScriptStore) Create(ctx context.Context, in scripts.Input, now time.Time) (ScriptRecord, error) {
	now = now.UTC()
	for attempt := 0; attempt < 2; attempt++ {
		id, err := newScriptID()
		if err != nil {
			return ScriptRecord{}, err
		}
		path, err := scripts.Path(s.dir, id)
		if err != nil {
			return ScriptRecord{}, err
		}
		// 解释器存绝对路径 scripts.Interpreter（`/bin/sh`），不是基名 `sh`。
		// 这一列回答的是“批准用哪个解释器跑这份脚本”，答案必须是一个绝对
		// 路径：同一个基名在不同 PATH 下指向不同二进制，而它又会原样回到
		// API（CONTRACTS §12 的枚举就是 `/bin/sh`）。列名与取值都对得上，
		// 读库的人不需要再做一次翻译。
		rec := ScriptRecord{
			ID: id, Name: in.Name, Content: in.Content, ContentHash: scripts.Hash(in.Content),
			Ref: path, Interpreter: scripts.Interpreter, Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		_, err = s.db.store().ExecContext(ctx, `
			INSERT INTO scripts (id, name, content, content_hash, agent_script_ref, interpreter,
			                     version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.Name, rec.Content, rec.ContentHash, rec.Ref, rec.Interpreter,
			rec.Version, millis(now), millis(now))
		if err == nil {
			return rec, nil
		}
		if isUniqueViolation(err) {
			if _, found, lookupErr := s.getByName(ctx, in.Name); lookupErr == nil && found {
				return ScriptRecord{}, fmt.Errorf("%w: %s", ErrScriptName, in.Name)
			}
			if attempt == 0 {
				continue
			}
		}
		return ScriptRecord{}, fmt.Errorf("agentstore: 写入脚本失败: %w", err)
	}
	return ScriptRecord{}, errors.New("agentstore: 生成脚本 id 连续冲突")
}

// Update 按期望版本更新脚本，成功后 version+1。
func (s *ScriptStore) Update(ctx context.Context, id string, in scripts.Input,
	expectedVersion int64, now time.Time) (ScriptRecord, error) {
	now = now.UTC()
	res, err := s.db.store().ExecContext(ctx, `
		UPDATE scripts
		   SET name = ?, content = ?, content_hash = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		in.Name, in.Content, scripts.Hash(in.Content), millis(now), id, expectedVersion)
	if err != nil {
		if isUniqueViolation(err) {
			return ScriptRecord{}, fmt.Errorf("%w: %s", ErrScriptName, in.Name)
		}
		return ScriptRecord{}, fmt.Errorf("agentstore: 更新脚本 %s 失败: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ScriptRecord{}, fmt.Errorf("agentstore: 更新脚本 %s 失败: %w", id, err)
	}
	if affected == 0 {
		if _, found, err := s.Get(ctx, id); err != nil {
			return ScriptRecord{}, err
		} else if !found {
			return ScriptRecord{}, ErrScriptNotFound
		}
		return ScriptRecord{}, fmt.Errorf("%w: %s 期望版本 %d", ErrScriptVersion, id, expectedVersion)
	}
	rec, found, err := s.Get(ctx, id)
	if err != nil {
		return ScriptRecord{}, err
	}
	if !found {
		return ScriptRecord{}, ErrScriptNotFound
	}
	return rec, nil
}

// Delete 按期望版本删除脚本（幂等：不存在时返回 nil）。
//
// 它不删磁盘上的文件：文件的清理与"还有没有计划引用它"的判断在动作层，
// 那里才有事务边界之外的现场可看。
func (s *ScriptStore) Delete(ctx context.Context, id string, expectedVersion int64) error {
	res, err := s.db.store().ExecContext(ctx,
		`DELETE FROM scripts WHERE id = ? AND version = ?`, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("agentstore: 删除脚本 %s 失败: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("agentstore: 删除脚本 %s 失败: %w", id, err)
	}
	if affected > 0 {
		return nil
	}
	if _, found, err := s.Get(ctx, id); err != nil {
		return err
	} else if !found {
		return nil
	}
	return fmt.Errorf("%w: %s 期望版本 %d", ErrScriptVersion, id, expectedVersion)
}

func (s *ScriptStore) getByName(ctx context.Context, name string) (ScriptRecord, bool, error) {
	row := s.db.store().SQL().QueryRowContext(ctx, `
		SELECT id, name, content, content_hash, agent_script_ref, interpreter, version,
		       created_at, updated_at
		  FROM scripts WHERE name = ?`, name)
	rec, err := scanScript(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScriptRecord{}, false, nil
	}
	if err != nil {
		return ScriptRecord{}, false, err
	}
	return rec, true, nil
}

func scanScript(row interface{ Scan(...any) error }) (ScriptRecord, error) {
	var (
		rec       ScriptRecord
		createdAt int64
		updatedAt int64
	)
	if err := row.Scan(&rec.ID, &rec.Name, &rec.Content, &rec.ContentHash, &rec.Ref,
		&rec.Interpreter, &rec.Version, &createdAt, &updatedAt); err != nil {
		return ScriptRecord{}, err
	}
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

func newScriptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agentstore: 生成脚本 id 失败: %w", err)
	}
	return scriptPrefix + hex.EncodeToString(b[:]), nil
}
