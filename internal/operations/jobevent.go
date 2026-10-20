package operations

// P03.3 Web 侧：任务事件（SSE 推送、重连补发、日志分页）与代理事件镜像。
//
// 事件从哪来：**代理是唯一的产地**。执行器真正干活时才写 progress/finished/error，
// Web 不能自己编一条“进度 50%”来让页面好看（CONTRACTS.md 4.3：不知道就是 null）。
// 因此这里的职责只有两件：
//
//  1. 把代理侧 operation_events 镜像进 job_events（带水位线 agent_event_seq，避免重复抄写）；
//  2. 按 seq 升序把 job_events 读出来，供 SSE 补发与日志分页。
//
// 两条 seq 别搞混：
//   - job_events.seq      Web 侧推送序号，就是 SSE 的 id，客户端重连时带回来；
//   - jobs.agent_event_seq 代理侧水位，只用于“哪些事件还没抄过”。

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxJobEventPage 是一次最多读回的事件条数（SSE 单轮补发与日志分页的上限）。
	MaxJobEventPage = 200
	// MaxJobLogEvents 是每个任务保留的日志行数上限（CONTRACTS.md 4.3：限制日志容量）。
	//
	// 环形保留：写新的、删最旧的。状态与进度事件永不删除——删掉它们，重连的客户端就
	// 拼不出“这个任务走到哪一步”了；日志可以截断，页面会显示“更早的日志已清理”。
	// 这个删除权限由迁移 0006 的触发器**只**开给 kind='log'（DECISIONS Q68）。
	MaxJobLogEvents = 1000
	// jobEventLogCursorPrefix 是日志游标的前缀，防止游标被当成别的资源的位置。
	jobEventLogCursorPrefix = "log:"
)

// JobEvent 是 job_events 的一行。
type JobEvent struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"-"`
}

// JobEventBounds 返回一个任务事件的最小与最大 seq（没有事件时为 0）。
//
// 用途只有一个：判断客户端带来的 Last-Event-ID 是否已经落在被清理的历史里。
// 那种情况下补发是不可能的，唯一诚实的做法是发一帧 gap 让客户端重新拉状态，
// 而不是假装“没有新事件”让页面永远停在旧进度上。
type JobEventBounds struct {
	MinSeq int64
	MaxSeq int64
}

// Bounds 读回事件区间。
func (s *Service) Bounds(ctx context.Context, userID int64, jobID string) (JobEventBounds, error) {
	if _, err := s.getJobByID(ctx, jobID, userID); err != nil {
		return JobEventBounds{}, err
	}
	var out JobEventBounds
	err := s.db.SQL().QueryRowContext(ctx,
		`SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0) FROM job_events WHERE job_id = ?`, jobID).
		Scan(&out.MinSeq, &out.MaxSeq)
	if err != nil {
		return JobEventBounds{}, fmt.Errorf("operations: 读取事件区间失败: %w", err)
	}
	return out, nil
}

// ListJobEvents 读回 seq > after 的事件，按 seq 升序，最多 limit 条。
//
// 归属校验放在这里而不是交给调用方：事件是任务内容的旁支，漏一次校验就等于
// 让别人的任务进度可读。
func (s *Service) ListJobEvents(ctx context.Context, userID int64, jobID string, after int64, limit int) ([]JobEvent, error) {
	if after < 0 {
		after = 0
	}
	if limit <= 0 || limit > MaxJobEventPage {
		limit = MaxJobEventPage
	}
	if _, err := s.getJobByID(ctx, jobID, userID); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT seq, kind, redacted_payload, created_at FROM job_events
		  WHERE job_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`, jobID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("operations: 读取任务事件失败: %w", err)
	}
	defer rows.Close()
	out := make([]JobEvent, 0, limit)
	for rows.Next() {
		ev, err := scanJobEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operations: 读取任务事件失败: %w", err)
	}
	return out, nil
}

// ListJobLogs 分页读取任务的脱敏日志（kind = 'log'）。
//
// 游标是 base64url("log:<seq>")：客户端只要把 next_cursor 原样传回来就行，
// 不解读它。游标为空表示从头开始；格式不合法返回 400 而不是静默重来——
// 静默重来会让翻页悄悄重复数据。
func (s *Service) ListJobLogs(ctx context.Context, userID int64, jobID, cursor string, limit int) ([]JobEvent, string, error) {
	after := int64(0)
	if cursor != "" {
		v, err := decodeLogCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: 游标无效", ErrInvalidLogCursor)
		}
		after = v
	}
	if limit <= 0 || limit > MaxJobEventPage {
		limit = MaxJobEventPage
	}
	if _, err := s.getJobByID(ctx, jobID, userID); err != nil {
		return nil, "", err
	}
	// 多读一条用来判断“还有没有下一页”，避免下一页返回空列表让前端多翻一次。
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT seq, kind, redacted_payload, created_at FROM job_events
		  WHERE job_id = ? AND kind = 'log' AND seq > ? ORDER BY seq ASC LIMIT ?`,
		jobID, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("operations: 读取任务日志失败: %w", err)
	}
	defer rows.Close()
	out := make([]JobEvent, 0, limit)
	for rows.Next() {
		ev, err := scanJobEvent(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("operations: 读取任务日志失败: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeLogCursor(out[len(out)-1].Seq)
	}
	return out, next, nil
}

// ErrInvalidLogCursor 表示日志游标无法解析。
var ErrInvalidLogCursor = errors.New("operations: 日志游标无效")

func encodeLogCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(jobEventLogCursorPrefix + strconv.FormatInt(seq, 10)))
}

func decodeLogCursor(cursor string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	text := string(raw)
	if !strings.HasPrefix(text, jobEventLogCursorPrefix) {
		return 0, errors.New("游标前缀不匹配")
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(text, jobEventLogCursorPrefix), 10, 64)
	if err != nil || seq < 0 {
		return 0, errors.New("游标内容不合法")
	}
	return seq, nil
}

// MirrorAgentEvents 把代理侧尚未镜像的事件抄进 job_events。
//
// 幂等靠水位线：只抄 seq > jobs.agent_event_seq 的事件，并在同一个事务里把水位推到
// 本次抄写的最大 seq。重复调用只会抄到 0 条，不会把同一条 progress 写两遍。
//
// 失败**不影响**状态同步：事件是旁支，状态是主干。调用方只记日志。
func (s *Service) MirrorAgentEvents(ctx context.Context, jobID, operationID string) error {
	if s.agent == nil || operationID == "" {
		return nil
	}
	events, err := s.agent.OperationEvents(ctx, operationID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	now := s.now().UTC()
	return s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var watermark int64
		if err := tx.QueryRowContext(ctx,
			`SELECT agent_event_seq FROM jobs WHERE id = ?`, jobID).Scan(&watermark); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrJobNotFound
			}
			return fmt.Errorf("operations: 读取事件水位失败: %w", err)
		}
		var maxSeq int64
		for _, ev := range events {
			if ev.Seq <= watermark {
				continue
			}
			// 未知类型不丢：代理将来加新事件类型时，Web 侧至少能把它当日志留下来，
			// 而不是因为枚举对不上就吞掉事实。
			payload := map[string]any{
				"agent_seq":  ev.Seq,
				"agent_kind": ev.Kind,
				"payload":    ev.Payload,
			}
			if err := appendJobEventTx(ctx, tx, jobID, jobEventKind(ev.Kind), payload, now); err != nil {
				return err
			}
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
		}
		if maxSeq == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET agent_event_seq = ? WHERE id = ? AND agent_event_seq < ?`,
			maxSeq, jobID, maxSeq); err != nil {
			return fmt.Errorf("operations: 更新事件水位失败: %w", err)
		}
		return pruneJobLogsTx(ctx, tx, jobID)
	})
}

// jobEventKind 把代理事件类型映射成契约里的四类（status/progress/log/gap）。
//
// gap 只由 Web 自己产生（历史被清理），代理不会发，所以这里不返回 gap。
func jobEventKind(agentKind string) string {
	switch agentKind {
	case "progress":
		return "progress"
	case "status", "finished", "canceled", "error", "accepted", "running":
		return "status"
	default:
		return "log"
	}
}

// pruneJobLogsTx 把超出上限的最旧日志删掉（状态与进度事件不动）。
func pruneJobLogsTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM job_events
		 WHERE job_id = ? AND kind = 'log'
		   AND seq NOT IN (
		       SELECT seq FROM job_events WHERE job_id = ? AND kind = 'log'
		        ORDER BY seq DESC LIMIT ?)`,
		jobID, jobID, MaxJobLogEvents)
	if err != nil {
		return fmt.Errorf("operations: 清理任务日志失败: %w", err)
	}
	return nil
}

// scanJobEvent 读一行事件。
func scanJobEvent(row interface{ Scan(...any) error }) (JobEvent, error) {
	var (
		ev        JobEvent
		payload   string
		createdAt string
	)
	if err := row.Scan(&ev.Seq, &ev.Kind, &payload, &createdAt); err != nil {
		return JobEvent{}, fmt.Errorf("operations: 读取任务事件失败: %w", err)
	}
	ev.Payload = json.RawMessage(payload)
	at, err := parseStoredTime(createdAt)
	if err != nil {
		return JobEvent{}, err
	}
	ev.CreatedAt = at
	return ev, nil
}
