package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/store"
)

// VolumeChecker 判断一个受管理卷此刻是否存在（生产实现是 storage.Fetcher）。
//
// 面板库里没有 volumes 表（卷是代理侧的记录，见迁移 0007 的说明），所以“依赖的卷存在”
// 不能用外键表达。写入时向代理要一次卷列表来确认，是唯一能让“依赖了不存在的卷”
// 当场失败、而不是存下一条永远对不上的引用的做法。
type VolumeChecker interface {
	VolumeExists(ctx context.Context, id string) (bool, error)
}

// VolumeMissingError 表示依赖列表里有代理不认识的卷。
type VolumeMissingError struct {
	// VolumeID 是那个不存在的卷标识。
	VolumeID string
}

func (e *VolumeMissingError) Error() string {
	return fmt.Sprintf("依赖卷 %s 不存在：代理的受管理卷列表里没有这个卷", e.VolumeID)
}

// ErrVolumeCheckUnavailable 表示无法向代理确认依赖卷是否存在。
//
// 这种情况下**不写库**：宁可让用户重试，也不能把一份可能永远对不上的依赖关系存下来。
var ErrVolumeCheckUnavailable = errors.New("apps: 无法确认依赖卷是否存在：代理不可用")

// Optional 区分“请求里没写这个字段”和“显式写了 null”。
//
// 大部分 PATCH 字段用指针就够了，只有可空字段需要这个区分：分组用 null 表示移出分组，
// 与“不改分组”是两件事，指针会把它们混成一个 nil。
type Optional[T any] struct {
	// Set 表示请求体里出现过这个字段。
	Set bool
	// Value 是解析后的值；显式 null 时是类型零值。
	Value T
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (o *Optional[T]) UnmarshalJSON(raw []byte) error {
	o.Set = true
	if string(raw) == "null" {
		var zero T
		o.Value = zero
		return nil
	}
	return json.Unmarshal(raw, &o.Value)
}

// Options 是 Service 的构造参数。
type Options struct {
	// Now 注入时钟（测试用）；nil 时用 time.Now。
	Now func() time.Time
	// Volumes 是受管理卷的存在性检查端。允许为 nil（不连代理的部署）：
	// 那时**带依赖卷**的写入会被拒绝并回 503，而不是存下未经验证的引用。
	Volumes VolumeChecker
	// Normalize 是校验环境（是否允许回环入口地址）。
	Normalize NormalizeOptions
}

// Service 是应用元数据的唯一入口。接口层不得直接写这几张表。
type Service struct {
	db        *store.DB
	now       func() time.Time
	volumes   VolumeChecker
	normalize NormalizeOptions
}

// New 构造 Service。db 为必需依赖。
func New(db *store.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("apps: 数据库句柄不能为空")
	}
	s := &Service{db: db, now: opts.Now, volumes: opts.Volumes, normalize: opts.Normalize}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// InTransaction 在一个事务里执行 fn，供接口层把“元数据变更 + 审计”合成一笔事务。
//
// 与 auth.Service.InTransaction 同一套语义：fn 内的写方法通过 store.ExecContext
// 加入同一事务，fn 返回错误则业务与审计一起回滚。
func (s *Service) InTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if fn == nil {
		return errors.New("apps: InTransaction 需要非空函数")
	}
	return s.db.WithTx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return fn(ctx)
	})
}

func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Second) }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("apps: 时间格式无效: %w", err)
	}
	return t.UTC(), nil
}

// queryRow 读取一行：ctx 携带事务时走事务，否则走连接池。
//
// 读也必须跟着事务走，否则“同一笔事务里先写后读”会读到另一个连接的旧快照。
func (s *Service) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.QueryRowContext(ctx, query, args...)
	}
	return s.db.SQL().QueryRowContext(ctx, query, args...)
}

// query 读多行：ctx 携带事务时走事务，否则走连接池。
func (s *Service) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.QueryContext(ctx, query, args...)
	}
	return s.db.SQL().QueryContext(ctx, query, args...)
}

// Create 新建一个应用元数据记录。
//
// 只写元数据：不碰容器、不碰单元、不启动任何东西。
func (s *Service) Create(ctx context.Context, def Definition) (App, error) {
	norm, err := def.Normalize(s.normalize)
	if err != nil {
		return App{}, err
	}
	if err := s.checkGroup(ctx, norm.GroupID); err != nil {
		return App{}, err
	}
	if err := s.checkVolumes(ctx, norm.VolumeDependencies); err != nil {
		return App{}, err
	}
	id, err := NewAppID()
	if err != nil {
		return App{}, err
	}
	return s.insert(ctx, id, norm)
}

// insert 把一份已经规范化的元数据写入库中（创建与测试共用）。
func (s *Service) insert(ctx context.Context, id string, norm Definition) (App, error) {
	binding, err := bindingJSON(norm.Type, norm.Binding)
	if err != nil {
		return App{}, err
	}
	health, err := healthJSON(norm.Health)
	if err != nil {
		return App{}, err
	}

	now := s.nowUTC()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO apps (id, name, description, icon_key, group_id, type, binding_json,
		                  public_url, health_json, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, norm.Name, norm.Description, norm.IconKey, nullableGroup(norm.GroupID),
		string(norm.Type), binding, norm.PublicURL, health, formatTime(now), formatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			// 唯一索引只有一条：同一个单元/容器/部署不能被登记成两个应用。
			return App{}, ErrBindingTaken
		}
		return App{}, fmt.Errorf("apps: 创建应用失败: %w", err)
	}
	if err := s.replaceVolumeDependencies(ctx, id, norm.VolumeDependencies); err != nil {
		return App{}, err
	}
	return App{
		ID:         id,
		Definition: norm,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// UpdateInput 是 PATCH /apps/:id 的输入。
type UpdateInput struct {
	// ID 是应用标识。
	ID string
	// Version 是客户端看到的版本号；与库中不一致返回 ErrVersionConflict。
	Version int
	// Patch 是要改的字段，nil 表示不改这一项。
	Patch Patch
}

// Patch 是应用的可改字段集合。
//
// Health 与 VolumeDependencies 是**整体替换**语义：数组里的顺序与内容就是新状态，
// 不做逐项合并。混合语义（部分字段替换、部分字段合并）最容易让人猜错。
type Patch struct {
	Name               *string
	Description        *string
	IconKey            *string
	GroupID            Optional[string]
	Type               *Type
	Binding            *Binding
	PublicURL          *string
	Health             *HealthCheck
	VolumeDependencies *[]string
}

// Update 修改一条应用元数据。
func (s *Service) Update(ctx context.Context, in UpdateInput) (App, error) {
	current, err := s.Get(ctx, 0, in.ID)
	if err != nil {
		return App{}, err
	}
	next := current.Definition

	if in.Patch.Name != nil {
		next.Name = *in.Patch.Name
	}
	if in.Patch.Description != nil {
		next.Description = *in.Patch.Description
	}
	if in.Patch.IconKey != nil {
		next.IconKey = *in.Patch.IconKey
	}
	if in.Patch.GroupID.Set {
		next.GroupID = in.Patch.GroupID.Value
	}
	if in.Patch.Type != nil {
		// 类型决定绑定的判别方式，两者必须一起改：只改类型会留下一个“docker 应用带着
		// systemd 单元名”的错位记录，页面上看起来正常、执行时全错。
		if in.Patch.Binding == nil {
			return App{}, invalid("binding", "修改 type 必须同时提供 binding")
		}
		next.Type = *in.Patch.Type
	}
	if in.Patch.Binding != nil {
		next.Binding = *in.Patch.Binding
	}
	if in.Patch.PublicURL != nil {
		next.PublicURL = *in.Patch.PublicURL
	}
	if in.Patch.Health != nil {
		next.Health = *in.Patch.Health
	}
	replaceDeps := in.Patch.VolumeDependencies != nil
	if replaceDeps {
		next.VolumeDependencies = *in.Patch.VolumeDependencies
	}

	norm, err := next.Normalize(s.normalize)
	if err != nil {
		return App{}, err
	}
	if err := s.checkGroup(ctx, norm.GroupID); err != nil {
		return App{}, err
	}
	if replaceDeps {
		if err := s.checkVolumes(ctx, norm.VolumeDependencies); err != nil {
			return App{}, err
		}
	}

	binding, err := bindingJSON(norm.Type, norm.Binding)
	if err != nil {
		return App{}, err
	}
	health, err := healthJSON(norm.Health)
	if err != nil {
		return App{}, err
	}
	now := s.nowUTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE apps SET name = ?, description = ?, icon_key = ?, group_id = ?, type = ?,
		                binding_json = ?, public_url = ?, health_json = ?,
		                version = version + 1, updated_at = ?
		WHERE id = ? AND version = ?`,
		norm.Name, norm.Description, norm.IconKey, nullableGroup(norm.GroupID), string(norm.Type),
		binding, norm.PublicURL, health, formatTime(now), in.ID, in.Version)
	if err != nil {
		if isUniqueViolation(err) {
			return App{}, ErrBindingTaken
		}
		return App{}, fmt.Errorf("apps: 更新应用失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return App{}, fmt.Errorf("apps: 更新应用失败: %w", err)
	}
	if affected == 0 {
		// 零行有两种原因：记录没了，或版本被别人改过。两者必须分开报——
		// “刷新后重试”和“这个应用已经不存在了”是两种完全不同的处理。
		return App{}, s.classifyAppMiss(ctx, in.ID)
	}
	if replaceDeps {
		if err := s.replaceVolumeDependencies(ctx, in.ID, norm.VolumeDependencies); err != nil {
			return App{}, err
		}
	}

	updated := current
	updated.Definition = norm
	updated.Version = current.Version + 1
	updated.UpdatedAt = now
	return updated, nil
}

// Delete 删除一条应用元数据。
//
// 只删除入口与绑定记录：不卸载项目、不停止服务、不删容器、不碰依赖卷里的任何数据
// （CONTRACTS.md 第 8 节：DELETE /apps/:id 只删除入口/绑定）。
func (s *Service) Delete(ctx context.Context, id string, version int) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM apps WHERE id = ? AND version = ?`, id, version)
	if err != nil {
		return fmt.Errorf("apps: 删除应用失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("apps: 删除应用失败: %w", err)
	}
	if affected == 0 {
		return s.classifyAppMiss(ctx, id)
	}
	return nil
}

// Get 按 id 读取一条应用元数据。userID 用于填充 favorite；传 0 表示不关心。
func (s *Service) Get(ctx context.Context, userID int64, id string) (App, error) {
	if !validAppID(id) {
		return App{}, ErrNotFound
	}
	row := s.queryRow(ctx, `
		SELECT a.id, a.name, a.description, a.icon_key, a.group_id, a.type, a.binding_json,
		       a.public_url, a.health_json, a.version, a.created_at, a.updated_at,
		       EXISTS (SELECT 1 FROM app_favorites f WHERE f.app_id = a.id AND f.user_id = ?)
		FROM apps a WHERE a.id = ?`, userID, id)
	app, err := scanApp(row)
	if err != nil {
		return App{}, err
	}
	deps, err := s.volumeDependencies(ctx, []string{app.ID})
	if err != nil {
		return App{}, err
	}
	app.VolumeDependencies = deps[app.ID]
	return app, nil
}

// ListOptions 是列表参数。
type ListOptions struct {
	// UserID 用于填充 favorite。
	UserID int64
	// Limit 为 0 时取 DefaultListLimit。
	Limit int
	// Cursor 为空表示第一页。
	Cursor string
	// GroupID 非空时只返回该分组的应用。
	GroupID string
}

// List 返回一页应用元数据，按 (name, id) 稳定排序。第二个返回值是下一页游标，
// 空表示没有更多。
func (s *Service) List(ctx context.Context, opts ListOptions) ([]App, string, error) {
	limit, cursor, err := normalizeListArgs(opts)
	if err != nil {
		return nil, "", err
	}
	afterName, afterID, err := decodeAppCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	query := `
		SELECT a.id, a.name, a.description, a.icon_key, a.group_id, a.type, a.binding_json,
		       a.public_url, a.health_json, a.version, a.created_at, a.updated_at,
		       EXISTS (SELECT 1 FROM app_favorites f WHERE f.app_id = a.id AND f.user_id = ?)
		FROM apps a`
	args := []any{opts.UserID}
	var where []string
	if opts.GroupID != "" {
		if !groupIDPattern.MatchString(opts.GroupID) {
			return nil, "", invalid("group_id", "分组标识必须是 grp_ 加 32 位小写十六进制")
		}
		where = append(where, "a.group_id = ?")
		args = append(args, opts.GroupID)
	}
	if afterID != "" {
		where = append(where, "(a.name > ? OR (a.name = ? AND a.id > ?))")
		args = append(args, afterName, afterName, afterID)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY a.name, a.id LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("apps: 查询应用列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]App, 0, limit)
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, app)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("apps: 读取应用列表失败: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeAppCursor(out[len(out)-1].Name, out[len(out)-1].ID)
	}

	deps, err := s.volumeDependencies(ctx, appIDs(out))
	if err != nil {
		return nil, "", err
	}
	for i := range out {
		out[i].VolumeDependencies = deps[out[i].ID]
	}
	return out, next, nil
}

// SetFavorite 设置“当前用户”对某个应用的收藏。重复调用幂等。
func (s *Service) SetFavorite(ctx context.Context, userID int64, appID string, favorite bool) error {
	if !validAppID(appID) {
		return ErrNotFound
	}
	var exists int
	if err := s.queryRow(ctx, `SELECT 1 FROM apps WHERE id = ?`, appID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("apps: 查询应用失败: %w", err)
	}
	if !favorite {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM app_favorites WHERE user_id = ? AND app_id = ?`, userID, appID); err != nil {
			return fmt.Errorf("apps: 取消收藏失败: %w", err)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO app_favorites (user_id, app_id, created_at) VALUES (?, ?, ?)`,
		userID, appID, formatTime(s.nowUTC())); err != nil {
		return fmt.Errorf("apps: 收藏失败: %w", err)
	}
	return nil
}

// GroupInput 是创建分组的输入。
type GroupInput struct {
	Name string
	// SortOrder 为 nil 时排在现有分组之后。
	SortOrder *int
}

// CreateGroup 新建一个分组。
func (s *Service) CreateGroup(ctx context.Context, in GroupInput) (Group, error) {
	name, err := normalizeGroupName(in.Name)
	if err != nil {
		return Group{}, err
	}
	order := 0
	if in.SortOrder != nil {
		order = *in.SortOrder
		if order < 0 {
			return Group{}, invalid("sort_order", "排序值不能为负数")
		}
	} else {
		// 未指定时排到末尾：新分组出现在最后，符合“按创建顺序排列”的直觉。
		if err := s.queryRow(ctx,
			`SELECT COALESCE(MAX(sort_order), -1) + 1 FROM app_groups`).Scan(&order); err != nil {
			return Group{}, fmt.Errorf("apps: 读取分组排序失败: %w", err)
		}
	}
	id, err := NewGroupID()
	if err != nil {
		return Group{}, err
	}
	now := s.nowUTC()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO app_groups (id, name, sort_order, version, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)`, id, name, order, formatTime(now), formatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return Group{}, ErrGroupNameTaken
		}
		return Group{}, fmt.Errorf("apps: 创建分组失败: %w", err)
	}
	return Group{ID: id, Name: name, SortOrder: order, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// UpdateGroup 修改分组名或排序。
func (s *Service) UpdateGroup(ctx context.Context, id string, version int, name *string, sortOrder *int) (Group, error) {
	if !groupIDPattern.MatchString(id) {
		return Group{}, ErrGroupNotFound
	}
	current, err := s.GetGroup(ctx, id)
	if err != nil {
		return Group{}, err
	}
	if current.Version != version {
		return Group{}, ErrVersionConflict
	}
	next := current
	if name != nil {
		next.Name, err = normalizeGroupName(*name)
		if err != nil {
			return Group{}, err
		}
	}
	if sortOrder != nil {
		if *sortOrder < 0 {
			return Group{}, invalid("sort_order", "排序值不能为负数")
		}
		next.SortOrder = *sortOrder
	}
	now := s.nowUTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE app_groups SET name = ?, sort_order = ?, version = version + 1, updated_at = ?
		WHERE id = ? AND version = ?`,
		next.Name, next.SortOrder, formatTime(now), id, version)
	if err != nil {
		if isUniqueViolation(err) {
			return Group{}, ErrGroupNameTaken
		}
		return Group{}, fmt.Errorf("apps: 更新分组失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Group{}, fmt.Errorf("apps: 更新分组失败: %w", err)
	}
	if affected == 0 {
		return Group{}, s.classifyGroupMiss(ctx, id)
	}
	next.Version = current.Version + 1
	next.UpdatedAt = now
	return next, nil
}

// DeleteGroup 删除分组。分组里还有应用时返回 ErrGroupInUse（外键 RESTRICT）。
func (s *Service) DeleteGroup(ctx context.Context, id string, version int) error {
	if !groupIDPattern.MatchString(id) {
		return ErrGroupNotFound
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM app_groups WHERE id = ? AND version = ?`, id, version)
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrGroupInUse
		}
		return fmt.Errorf("apps: 删除分组失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("apps: 删除分组失败: %w", err)
	}
	if affected == 0 {
		return s.classifyGroupMiss(ctx, id)
	}
	return nil
}

// GetGroup 按 id 读取分组。
func (s *Service) GetGroup(ctx context.Context, id string) (Group, error) {
	if !groupIDPattern.MatchString(id) {
		return Group{}, ErrGroupNotFound
	}
	row := s.queryRow(ctx,
		`SELECT id, name, sort_order, version, created_at, updated_at FROM app_groups WHERE id = ?`, id)
	return scanGroup(row)
}

// ListGroups 返回一页分组，按 (sort_order, id) 排序。
func (s *Service) ListGroups(ctx context.Context, limit int, cursor string) ([]Group, string, error) {
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, "", ErrInvalidLimit
	}
	afterOrder, afterID, err := decodeGroupCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	query := `SELECT id, name, sort_order, version, created_at, updated_at FROM app_groups`
	args := []any{}
	if afterID != "" {
		query += ` WHERE (sort_order > ? OR (sort_order = ? AND id > ?))`
		args = append(args, afterOrder, afterOrder, afterID)
	}
	query += ` ORDER BY sort_order, id LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("apps: 查询分组列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Group, 0, limit)
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("apps: 读取分组列表失败: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeGroupCursor(out[len(out)-1].SortOrder, out[len(out)-1].ID)
	}
	return out, next, nil
}

// normalizeGroupName 校验分组名。
func normalizeGroupName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", invalid("name", "分组名不能为空")
	}
	if len([]rune(name)) > MaxGroupNameLen {
		return "", invalid("name", "分组名最多 %d 个字符", MaxGroupNameLen)
	}
	if hasControl(name) {
		return "", invalid("name", "分组名不能包含控制字符或换行")
	}
	return name, nil
}

// checkGroup 确认分组存在（空表示未分组，直接通过）。
func (s *Service) checkGroup(ctx context.Context, groupID string) error {
	if groupID == "" {
		return nil
	}
	var exists int
	err := s.queryRow(ctx, `SELECT 1 FROM app_groups WHERE id = ?`, groupID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return &GroupMissingError{GroupID: groupID}
	}
	if err != nil {
		return fmt.Errorf("apps: 查询分组失败: %w", err)
	}
	return nil
}

// GroupMissingError 表示引用的分组不存在。
type GroupMissingError struct {
	GroupID string
}

func (e *GroupMissingError) Error() string {
	return fmt.Sprintf("分组 %s 不存在", e.GroupID)
}

// checkVolumes 确认每个依赖卷此刻都存在。空列表直接通过（不打扰代理）。
func (s *Service) checkVolumes(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if s.volumes == nil {
		return ErrVolumeCheckUnavailable
	}
	for _, id := range ids {
		ok, err := s.volumes.VolumeExists(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVolumeCheckUnavailable, err)
		}
		if !ok {
			return &VolumeMissingError{VolumeID: id}
		}
	}
	return nil
}

// replaceVolumeDependencies 整体替换一个应用的依赖卷列表（保序）。
func (s *Service) replaceVolumeDependencies(ctx context.Context, appID string, ids []string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM app_volume_dependencies WHERE app_id = ?`, appID); err != nil {
		return fmt.Errorf("apps: 清理依赖卷失败: %w", err)
	}
	for i, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO app_volume_dependencies (app_id, volume_id, position) VALUES (?, ?, ?)`,
			appID, id, i); err != nil {
			return fmt.Errorf("apps: 写入依赖卷失败: %w", err)
		}
	}
	return nil
}

// volumeDependencies 批量读取依赖卷，返回 app_id → 保序的卷标识列表。
func (s *Service) volumeDependencies(ctx context.Context, appIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(appIDs)), ",")
	args := make([]any, len(appIDs))
	for i, id := range appIDs {
		args[i] = id
	}
	rows, err := s.query(ctx, `
		SELECT app_id, volume_id FROM app_volume_dependencies
		WHERE app_id IN (`+placeholders+`) ORDER BY app_id, position`, args...)
	if err != nil {
		return nil, fmt.Errorf("apps: 查询依赖卷失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var appID, volumeID string
		if err := rows.Scan(&appID, &volumeID); err != nil {
			return nil, fmt.Errorf("apps: 读取依赖卷失败: %w", err)
		}
		out[appID] = append(out[appID], volumeID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apps: 读取依赖卷失败: %w", err)
	}
	return out, nil
}

// classifyAppMiss 判断应用的“零行”到底是记录不存在还是版本冲突。
func (s *Service) classifyAppMiss(ctx context.Context, id string) error {
	var v int
	err := s.queryRow(ctx, `SELECT version FROM apps WHERE id = ?`, id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("apps: 查询应用版本失败: %w", err)
	}
	return ErrVersionConflict
}

// classifyGroupMiss 判断分组的“零行”到底是记录不存在还是版本冲突。
func (s *Service) classifyGroupMiss(ctx context.Context, id string) error {
	var v int
	err := s.queryRow(ctx, `SELECT version FROM app_groups WHERE id = ?`, id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrGroupNotFound
	}
	if err != nil {
		return fmt.Errorf("apps: 查询分组版本失败: %w", err)
	}
	return ErrVersionConflict
}

// normalizeListArgs 校验分页参数。
func normalizeListArgs(opts ListOptions) (int, string, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return 0, "", ErrInvalidLimit
	}
	return limit, opts.Cursor, nil
}

// validAppID 判断标识形状。形状不对就是“不存在”，不必查库。
func validAppID(id string) bool {
	if len(id) != len(appIDPrefix)+idHexLen || !strings.HasPrefix(id, appIDPrefix) {
		return false
	}
	for i := len(appIDPrefix); i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// nullableGroup 把空字符串分组转成 NULL：未分组在库里是 NULL，不是空串。
func nullableGroup(groupID string) any {
	if groupID == "" {
		return nil
	}
	return groupID
}

// appIDs 取出标识列表。
func appIDs(list []App) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

// rowScanner 是 *sql.Row 与 *sql.Rows 的共同接口。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanApp 读出一条应用记录（含 favorite，不含依赖卷）。
func scanApp(row rowScanner) (App, error) {
	var (
		app      App
		groupID  sql.NullString
		typ      string
		binding  string
		health   string
		created  string
		updated  string
		favorite int
	)
	err := row.Scan(&app.ID, &app.Name, &app.Description, &app.IconKey, &groupID, &typ,
		&binding, &app.PublicURL, &health, &app.Version, &created, &updated, &favorite)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, fmt.Errorf("apps: 读取应用失败: %w", err)
	}
	app.Type = Type(typ)
	if !app.Type.Valid() {
		// 库里出现了判别集合之外的类型：这是数据损坏，不是用户输入问题。
		return App{}, fmt.Errorf("apps: 应用 %s 的类型 %q 不在固定集合内", app.ID, typ)
	}
	if groupID.Valid {
		app.GroupID = groupID.String
	}
	if app.Binding, err = parseBinding(app.Type, binding); err != nil {
		return App{}, fmt.Errorf("apps: 应用 %s 的绑定数据不合法: %w", app.ID, err)
	}
	if app.Health, err = parseHealth(health); err != nil {
		return App{}, fmt.Errorf("apps: 应用 %s 的健康检查数据不合法: %w", app.ID, err)
	}
	if app.CreatedAt, err = parseTime(created); err != nil {
		return App{}, err
	}
	if app.UpdatedAt, err = parseTime(updated); err != nil {
		return App{}, err
	}
	app.Favorite = favorite != 0
	return app, nil
}

// scanGroup 读出一条分组记录。
func scanGroup(row rowScanner) (Group, error) {
	var (
		g       Group
		created string
		updated string
	)
	err := row.Scan(&g.ID, &g.Name, &g.SortOrder, &g.Version, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrGroupNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("apps: 读取分组失败: %w", err)
	}
	if g.CreatedAt, err = parseTime(created); err != nil {
		return Group{}, err
	}
	if g.UpdatedAt, err = parseTime(updated); err != nil {
		return Group{}, err
	}
	return g, nil
}

// isUniqueViolation 判断是否为唯一约束冲突。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isForeignKeyViolation 判断是否为外键约束冲突。
func isForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
