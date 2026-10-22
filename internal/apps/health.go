package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HealthResult 是某个应用最近一次健康探测的结论（表 app_health_results，迁移 0008）。
//
// 与 HealthCheck（定义）分开：定义是用户写的，结论是代理看过之后说的。两者的字段看起来
// 相似，含义不同——定义里的 port 是“要探哪个端口”，结论里的 port 是“上次探的是哪个
// 端口”；如果有人在两次探测之间把定义改成 8080，结论仍然如实写着上次探的是 8000。
type HealthResult struct {
	AppID      string
	CheckedAt  time.Time
	HTTPState  string
	StatusCode *int
	DurationMS int64
	TargetID   string
	Scheme     string
	Port       int
	Path       string
	Address    string
	URL        string
	Error      string
}

// ErrHealthTargetCheckUnavailable 表示无法向代理确认健康检查目标是否存在。
var ErrHealthTargetCheckUnavailable = errors.New("apps: 无法确认健康检查目标：代理不可用")

// HealthTargetChecker 判断一个 target_id 此刻是不是代理发现出来的目标。
//
// 形状校验（^local_|^cntr_）在 Normalize 里已经做过，这里问的是“这个标识现在真的
// 指向本机上的一个东西吗”。两步缺一不可：只有形状校验时，一个手写的 local_foo
// 会被存下去，然后每次探测都报“目标不存在”；只问存在性而不校验形状，则没法解释
// 为什么 169.254.169.254 从来不被接受。
type HealthTargetChecker interface {
	TargetExists(ctx context.Context, targetID string) (bool, error)
}

// HealthTargetMissingError 表示健康检查目标不在代理的发现结果里。
type HealthTargetMissingError struct {
	TargetID string
}

func (e *HealthTargetMissingError) Error() string {
	return fmt.Sprintf("健康检查目标 %s 不在代理当前的发现结果里", e.TargetID)
}

// checkHealthTarget 在写入前确认目标存在。
//
// 只在“这次写入真的涉及健康检查”时调用（创建时总是；修改时仅当 patch 里带了
// health_check）。否则一次改名会因为这个目标恰好在这一刻重启而失败——那是把一个
// 与本次修改无关的瞬时状态，变成了保存失败的理由。
func (s *Service) checkHealthTarget(ctx context.Context, id string, health HealthCheck) error {
	if !health.Enabled {
		return nil
	}
	if s.targets == nil {
		return ErrHealthTargetCheckUnavailable
	}
	ok, err := s.targets.TargetExists(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHealthTargetCheckUnavailable, err)
	}
	if !ok {
		return &HealthTargetMissingError{TargetID: id}
	}
	return nil
}

// SaveHealthResult 写入（或覆盖）一个应用的探测结论。
//
// 用 UPSERT 而不是“先删后插”：一行一个应用，replace 语义在并发下会出现“删掉了、
// 插入失败”的空窗，而空窗里读到的是一句“没有结果”。
func (s *Service) SaveHealthResult(ctx context.Context, r HealthResult) error {
	if !validAppID(r.AppID) {
		return ErrNotFound
	}
	if r.HTTPState != HTTPOK && r.HTTPState != HTTPError && r.HTTPState != HTTPUnknown {
		return fmt.Errorf("apps: 未知的 http_state %q", r.HTTPState)
	}
	if r.CheckedAt.IsZero() {
		r.CheckedAt = s.nowUTC()
	}
	var statusCode any
	if r.StatusCode != nil {
		statusCode = *r.StatusCode
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_health_results (app_id, checked_at, http_state, status_code, duration_ms,
		                                target_id, scheme, port, path, address, url, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id) DO UPDATE SET
			checked_at = excluded.checked_at,
			http_state = excluded.http_state,
			status_code = excluded.status_code,
			duration_ms = excluded.duration_ms,
			target_id = excluded.target_id,
			scheme = excluded.scheme,
			port = excluded.port,
			path = excluded.path,
			address = excluded.address,
			url = excluded.url,
			error = excluded.error`,
		r.AppID, formatTime(r.CheckedAt.UTC()), r.HTTPState, statusCode, r.DurationMS,
		r.TargetID, r.Scheme, r.Port, r.Path, r.Address, r.URL, r.Error)
	if err != nil {
		// 外键失败只有一种原因：应用在探测期间被删了。那不是错误，是竞态的正常结果。
		if isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("apps: 写入健康探测结果失败: %w", err)
	}
	return nil
}

// HealthResult 读取一个应用最近一次的探测结论。没有结果时 ok=false。
func (s *Service) HealthResult(ctx context.Context, appID string) (HealthResult, bool, error) {
	if !validAppID(appID) {
		return HealthResult{}, false, ErrNotFound
	}
	results, err := s.healthResults(ctx, []string{appID})
	if err != nil {
		return HealthResult{}, false, err
	}
	r, ok := results[appID]
	return r, ok, nil
}

// healthResults 批量读取探测结论（列表与详情共用）。
func (s *Service) healthResults(ctx context.Context, ids []string) (map[string]HealthResult, error) {
	out := make(map[string]HealthResult, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	query := `SELECT app_id, checked_at, http_state, status_code, duration_ms,
	                 target_id, scheme, port, path, address, url, error
	          FROM app_health_results WHERE app_id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("apps: 查询健康探测结果失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			r          HealthResult
			checkedAt  string
			statusCode sql.NullInt64
			durationMS int64
		)
		if err := rows.Scan(&r.AppID, &checkedAt, &r.HTTPState, &statusCode, &durationMS,
			&r.TargetID, &r.Scheme, &r.Port, &r.Path, &r.Address, &r.URL, &r.Error); err != nil {
			return nil, fmt.Errorf("apps: 读取健康探测结果失败: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, checkedAt); err == nil {
			r.CheckedAt = t
		}
		if statusCode.Valid {
			code := int(statusCode.Int64)
			r.StatusCode = &code
		}
		r.DurationMS = durationMS
		out[r.AppID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apps: 读取健康探测结果失败: %w", err)
	}
	return out, nil
}

// HealthTarget 是调度器要探的一个应用：定义 + 上次结论。
type HealthTarget struct {
	App  App
	Last *HealthResult
}

// HealthTargets 返回所有启用健康检查的应用及其最近结论。
//
// 一页 500 条足够：带健康检查的应用是“这台服务器上我有几个 Web 服务”，
// 不是“所有应用”。超过时按 (name, id) 取前 MaxHealthTargets 条——宁可少探，
// 也不要在一次轮询里把代理打满。
// 最后一列是 scanApp 需要的 favorite：探测目标与“谁收藏了它”无关，
// 给个常量 0，不要为调度再按用户查一遍收藏表。
func (s *Service) HealthTargets(ctx context.Context) ([]HealthTarget, error) {
	rows, err := s.query(ctx, `
		SELECT id, name, description, icon_key, group_id, type, binding_json,
		       public_url, health_json, version, created_at, updated_at,
		       0
		FROM apps ORDER BY name, id LIMIT ?`, MaxHealthTargets)
	if err != nil {
		return nil, fmt.Errorf("apps: 查询健康检查目标失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	appsWithHealth := make([]App, 0, 16)
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		if app.Health.Enabled {
			appsWithHealth = append(appsWithHealth, app)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apps: 读取健康检查目标失败: %w", err)
	}

	results, err := s.healthResults(ctx, appIDs(appsWithHealth))
	if err != nil {
		return nil, err
	}
	out := make([]HealthTarget, 0, len(appsWithHealth))
	for _, app := range appsWithHealth {
		target := HealthTarget{App: app}
		if r, ok := results[app.ID]; ok {
			res := r
			target.Last = &res
		}
		out = append(out, target)
	}
	return out, nil
}

// MaxHealthTargets 是单次轮询最多考虑的带健康检查的应用数。
const MaxHealthTargets = 500

// placeholders 生成 SQL 里的 ?, ?, ? 序列。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, 3*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		out = append(out, '?')
	}
	return string(out)
}
