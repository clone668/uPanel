package proxies

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/store"
)

// AppChecker 判断关联的应用是否存在。
//
// 生产实现是 internal/apps 的 Service（Get 一次），这样 proxies 表的外键之外还有一道
// 明确的业务判断：app_id 写错时得到的是“应用不存在”，而不是数据库的 FOREIGN KEY。
type AppChecker interface {
	AppExists(ctx context.Context, id string) (bool, error)
}

// Options 是 Service 的构造参数。
type Options struct {
	// Now 注入时钟（测试用）；nil 时用 time.Now。
	Now func() time.Time
	// Facts 提供 Nginx 现状。允许为 nil（不连代理的部署）：那时**所有写入**都会被
	// 拒绝（无法判断域名冲突），读取仍可用但会标明“判不了”。
	Facts FactsSource
	// Apps 是关联应用的存在性检查端。允许为 nil：那时带 app_id 的写入会被拒绝并
	// 回 503，而不是存下一个对不上的引用。
	Apps AppChecker
	// Normalize 是校验环境（面板端口）。
	Normalize NormalizeOptions
	// Certs 是证书服务的投影（P10.3）。允许为 nil：那时绑定证书的写入会被拒绝
	// （无法确认那张证书存不存在、覆不覆盖这个域名），不绑证书的写入照常可用。
	Certs CertificateChecker
}

// Service 是代理站点的唯一入口。接口层不得直接写这张表。
type Service struct {
	db        *store.DB
	now       func() time.Time
	facts     FactsSource
	apps      AppChecker
	certs     CertificateChecker
	normalize NormalizeOptions
}

// New 构造 Service。db 为必需依赖。
func New(db *store.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("proxies: 数据库句柄不能为空")
	}
	s := &Service{db: db, now: opts.Now, facts: opts.Facts, apps: opts.Apps,
		certs: opts.Certs, normalize: opts.Normalize}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// InTransaction 在一个事务里执行 fn，供接口层把“站点变更 + 审计”合成一笔事务。
func (s *Service) InTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if fn == nil {
		return errors.New("proxies: InTransaction 需要非空函数")
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
		return time.Time{}, fmt.Errorf("proxies: 时间格式无效: %w", err)
	}
	return t.UTC(), nil
}

// queryRow 读取一行：ctx 携带事务时走事务，否则走连接池。
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

// Create 新建一个代理站点。
//
// 顺序是有讲究的：先校验形状（400），再确认应用存在（503/400），最后判断域名冲突
// （读主机上的 Nginx 现状）。冲突判断放在最后，是因为它最贵、也最可能因为“读不到”
// 而失败——把便宜的检查排在前面，用户不必为了一个打错的域名去读一遍配置。
func (s *Service) Create(ctx context.Context, def Definition) (Site, error) {
	norm, err := def.Normalize(s.normalize)
	if err != nil {
		return Site{}, err
	}
	if err := s.checkApp(ctx, norm.AppID); err != nil {
		return Site{}, err
	}
	if err := s.checkCertificate(ctx, norm.CertificateID, norm.Domain); err != nil {
		return Site{}, err
	}
	if err := s.requireDomainFree(ctx, norm.Domain, ""); err != nil {
		return Site{}, err
	}

	cidrs, err := cidrJSON(norm.AllowedCIDRs)
	if err != nil {
		return Site{}, err
	}
	users, err := usersJSON(norm.BasicAuthUsers)
	if err != nil {
		return Site{}, err
	}
	id, err := newID()
	if err != nil {
		return Site{}, err
	}
	now := s.nowUTC()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO proxies (id, app_id, domain, upstream_target_id, upstream_scheme,
		                        upstream_port, access_mode, allowed_cidrs_json, basic_auth_json,
		                        websocket_enabled, max_body_bytes, read_timeout_seconds,
		                        enabled, certificate_id, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, nullableString(norm.AppID), norm.Domain, norm.UpstreamTargetID, norm.UpstreamScheme,
		norm.UpstreamPort, string(norm.AccessMode), cidrs, users,
		boolInt(norm.WebSocketEnabled), norm.MaxBodyBytes, norm.ReadTimeoutSeconds,
		boolInt(norm.Enabled), nullableString(norm.CertificateID), formatTime(now), formatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return Site{}, ErrDomainTaken
		}
		if isForeignKeyViolation(err) {
			return Site{}, invalid("app_id", "关联的应用不存在")
		}
		return Site{}, fmt.Errorf("proxies: 创建站点失败: %w", err)
	}
	// 写完之后把 Nginx 快照作废：下一次展示要重新读，不能还说着写之前的话。
	s.invalidateFacts()

	return Site{ID: id, Definition: norm, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// Get 按 id 读取一个站点。
func (s *Service) Get(ctx context.Context, id string) (Site, error) {
	if !validSiteID(id) {
		return Site{}, ErrNotFound
	}
	row := s.queryRow(ctx, `
		SELECT id, app_id, domain, upstream_target_id, upstream_scheme, upstream_port,
		       access_mode, allowed_cidrs_json, basic_auth_json, websocket_enabled,
		       max_body_bytes, read_timeout_seconds, enabled, certificate_id,
		       version, created_at, updated_at
		FROM proxies WHERE id = ?`, id)
	return scanSite(row)
}

// GetByDomain 按域名读取一个站点（域名必须已归一化）。
func (s *Service) GetByDomain(ctx context.Context, domain string) (Site, error) {
	row := s.queryRow(ctx, `
		SELECT id, app_id, domain, upstream_target_id, upstream_scheme, upstream_port,
		       access_mode, allowed_cidrs_json, basic_auth_json, websocket_enabled,
		       max_body_bytes, read_timeout_seconds, enabled, certificate_id,
		       version, created_at, updated_at
		FROM proxies WHERE domain = ?`, domain)
	return scanSite(row)
}

// ListOptions 是列表参数。
type ListOptions struct {
	// Limit 为 0 时取 DefaultListLimit。
	Limit int
	// Cursor 为空表示第一页。
	Cursor string
	// AppID 非空时只返回关联该应用的站点。
	AppID string
	// Domain 非空时只返回该域名（精确匹配，已归一化）。
	Domain string
}

// List 返回一页站点，按 (domain, id) 稳定排序。第二个返回值是下一页游标。
func (s *Service) List(ctx context.Context, opts ListOptions) ([]Site, string, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, "", ErrInvalidLimit
	}
	afterDomain, afterID, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, "", err
	}

	query := `
		SELECT id, app_id, domain, upstream_target_id, upstream_scheme, upstream_port,
		       access_mode, allowed_cidrs_json, basic_auth_json, websocket_enabled,
		       max_body_bytes, read_timeout_seconds, enabled, certificate_id,
		       version, created_at, updated_at
		FROM proxies`
	var where []string
	var args []any
	if opts.AppID != "" {
		if !isAppIDShape(opts.AppID) {
			return nil, "", invalid("app_id", "应用标识必须是 app_ 加 32 位小写十六进制")
		}
		where = append(where, "app_id = ?")
		args = append(args, opts.AppID)
	}
	if opts.Domain != "" {
		where = append(where, "domain = ?")
		args = append(args, opts.Domain)
	}
	if afterID != "" {
		where = append(where, "(domain > ? OR (domain = ? AND id > ?))")
		args = append(args, afterDomain, afterDomain, afterID)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY domain, id LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("proxies: 查询站点列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Site, 0, limit)
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, site)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("proxies: 读取站点列表失败: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeCursor(out[len(out)-1].Domain, out[len(out)-1].ID)
	}
	return out, next, nil
}

// Patch 是站点的可改字段集合。nil 表示不改这一项。
type Patch struct {
	AppID              *string
	Domain             *string
	UpstreamTargetID   *string
	UpstreamScheme     *string
	UpstreamPort       *int
	AccessMode         *AccessMode
	AllowedCIDRs       *[]string
	BasicAuthUsers     *[]BasicAuthUser
	WebSocketEnabled   *bool
	MaxBodyBytes       *int64
	ReadTimeoutSeconds *int
	Enabled            *bool
	// CertificateID 指向一张证书记录；传空串表示解绑（站点退回只监听 80）。
	//
	// 语义只有两条：**字段不出现** = 不改；**出现且为空串** = 解绑。这三态不能用
	// *string 之外的形状表达，也不能用 null（JSON null 与“没写”在解码后长得一样）。
	CertificateID *string
}

// IsEmpty 表示这个 patch 一个字段都没写。
func (p Patch) IsEmpty() bool {
	return p.AppID == nil && p.Domain == nil && p.UpstreamTargetID == nil && p.UpstreamScheme == nil &&
		p.UpstreamPort == nil && p.AccessMode == nil && p.AllowedCIDRs == nil && p.BasicAuthUsers == nil &&
		p.WebSocketEnabled == nil && p.MaxBodyBytes == nil && p.ReadTimeoutSeconds == nil &&
		p.Enabled == nil && p.CertificateID == nil
}

// UpdateInput 是 PATCH /proxies/:id 的输入。
type UpdateInput struct {
	// ID 是站点标识。
	ID string
	// Version 是客户端看到的版本号；与库中不一致返回 ErrVersionConflict。
	Version int
	// Patch 是要改的字段。
	Patch Patch
}

// Update 修改一个站点。
func (s *Service) Update(ctx context.Context, in UpdateInput) (Site, error) {
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return Site{}, err
	}
	if in.Patch.IsEmpty() {
		return Site{}, errors.New("proxies: 至少要提供一个要修改的字段")
	}
	next := current.Definition

	if in.Patch.AppID != nil {
		next.AppID = *in.Patch.AppID
	}
	if in.Patch.Domain != nil {
		next.Domain = *in.Patch.Domain
	}
	if in.Patch.UpstreamTargetID != nil {
		next.UpstreamTargetID = *in.Patch.UpstreamTargetID
	}
	if in.Patch.UpstreamScheme != nil {
		next.UpstreamScheme = *in.Patch.UpstreamScheme
	}
	if in.Patch.UpstreamPort != nil {
		next.UpstreamPort = *in.Patch.UpstreamPort
	}
	if in.Patch.AccessMode != nil {
		next.AccessMode = *in.Patch.AccessMode
	}
	if in.Patch.AllowedCIDRs != nil {
		next.AllowedCIDRs = *in.Patch.AllowedCIDRs
	}
	if in.Patch.BasicAuthUsers != nil {
		next.BasicAuthUsers = *in.Patch.BasicAuthUsers
	}
	if in.Patch.WebSocketEnabled != nil {
		next.WebSocketEnabled = *in.Patch.WebSocketEnabled
	}
	if in.Patch.MaxBodyBytes != nil {
		next.MaxBodyBytes = *in.Patch.MaxBodyBytes
	}
	if in.Patch.ReadTimeoutSeconds != nil {
		next.ReadTimeoutSeconds = *in.Patch.ReadTimeoutSeconds
	}
	if in.Patch.Enabled != nil {
		next.Enabled = *in.Patch.Enabled
	}
	if in.Patch.CertificateID != nil {
		next.CertificateID = *in.Patch.CertificateID
	}

	norm, err := next.Normalize(s.normalize)
	if err != nil {
		return Site{}, err
	}
	// 只在真的动了 app_id / domain 时才花代价去确认：一次改超时不该因为代理临时
	// 不可用而失败。
	if in.Patch.AppID != nil {
		if err := s.checkApp(ctx, norm.AppID); err != nil {
			return Site{}, err
		}
	}
	if in.Patch.Domain != nil && norm.Domain != current.Domain {
		if err := s.requireDomainFree(ctx, norm.Domain, in.ID); err != nil {
			return Site{}, err
		}
	}
	// 证书与域名任一变化都要重新确认覆盖关系：绑着 A 域名的证书被改到 B 域名上，
	// 结果是一个证书名字对不上的 443 站点。只在 patch 里出现这两个字段时才问，
	// 理由与 checkApp 上面那一段相同。
	if in.Patch.CertificateID != nil || (in.Patch.Domain != nil && norm.Domain != current.Domain) {
		if err := s.checkCertificate(ctx, norm.CertificateID, norm.Domain); err != nil {
			return Site{}, err
		}
	}

	cidrs, err := cidrJSON(norm.AllowedCIDRs)
	if err != nil {
		return Site{}, err
	}
	users, err := usersJSON(norm.BasicAuthUsers)
	if err != nil {
		return Site{}, err
	}
	now := s.nowUTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE proxies SET app_id = ?, domain = ?, upstream_target_id = ?, upstream_scheme = ?,
		                       upstream_port = ?, access_mode = ?, allowed_cidrs_json = ?,
		                       basic_auth_json = ?, websocket_enabled = ?, max_body_bytes = ?,
		                       read_timeout_seconds = ?, enabled = ?, certificate_id = ?,
		                       version = version + 1, updated_at = ?
		WHERE id = ? AND version = ?`,
		nullableString(norm.AppID), norm.Domain, norm.UpstreamTargetID, norm.UpstreamScheme,
		norm.UpstreamPort, string(norm.AccessMode), cidrs, users,
		boolInt(norm.WebSocketEnabled), norm.MaxBodyBytes, norm.ReadTimeoutSeconds,
		boolInt(norm.Enabled), nullableString(norm.CertificateID), formatTime(now), in.ID, in.Version)
	if err != nil {
		if isUniqueViolation(err) {
			return Site{}, ErrDomainTaken
		}
		if isForeignKeyViolation(err) {
			return Site{}, invalid("app_id", "关联的应用不存在")
		}
		return Site{}, fmt.Errorf("proxies: 更新站点失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Site{}, fmt.Errorf("proxies: 更新站点失败: %w", err)
	}
	if affected == 0 {
		// 零行有两种原因：记录没了，或版本被别人改过。分开报。
		return Site{}, s.classifyMiss(ctx, in.ID)
	}
	s.invalidateFacts()

	updated := current
	updated.Definition = norm
	updated.Version = current.Version + 1
	updated.UpdatedAt = now
	return updated, nil
}

// Delete 删除一个站点。
//
// 只删面板的记录：P10.1 还没有生成过 Nginx 配置，所以这里没有文件要收尾。
// P10.2 接上配置文件之后，删除会变成“先删文件再删记录”的任务，而不是一行 SQL。
func (s *Service) Delete(ctx context.Context, id string, version int) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM proxies WHERE id = ? AND version = ?`, id, version)
	if err != nil {
		return fmt.Errorf("proxies: 删除站点失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("proxies: 删除站点失败: %w", err)
	}
	if affected == 0 {
		return s.classifyMiss(ctx, id)
	}
	s.invalidateFacts()
	return nil
}

// checkApp 确认关联应用存在。appID 为空表示不关联应用。
func (s *Service) checkApp(ctx context.Context, appID string) error {
	if appID == "" {
		return nil
	}
	if s.apps == nil {
		return fmt.Errorf("%w: 面板没有连上应用元数据服务，无法确认 app_id", ErrFactsUnavailable)
	}
	ok, err := s.apps.AppExists(ctx, appID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFactsUnavailable, err)
	}
	if !ok {
		return invalid("app_id", "关联的应用不存在")
	}
	return nil
}

// siteIDByDomain 按域名查站点标识，查不到返回空串。
func (s *Service) siteIDByDomain(ctx context.Context, domain string) (string, error) {
	var id string
	err := s.queryRow(ctx, `SELECT id FROM proxies WHERE domain = ?`, domain).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("proxies: 按域名查询站点失败: %w", err)
	}
	return id, nil
}

func (s *Service) invalidateFacts() {
	if s.facts != nil {
		s.facts.Invalidate()
	}
}

// classifyMiss 把“更新/删除影响零行”分成“不存在”和“版本冲突”。
func (s *Service) classifyMiss(ctx context.Context, id string) error {
	var exists int
	err := s.queryRow(ctx, `SELECT 1 FROM proxies WHERE id = ?`, id).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("proxies: 查询站点失败: %w", err)
	}
	return ErrVersionConflict
}

// rowScanner 是 *sql.Row 与 *sql.Rows 的公共部分。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSite(row rowScanner) (Site, error) {
	var (
		site       Site
		appID      sql.NullString
		certID     sql.NullString
		accessMode string
		cidrsJSON  string
		usersJSON  string
		wsInt      int
		enabled    int
		created    string
		updated    string
	)
	err := row.Scan(
		&site.ID, &appID, &site.Domain, &site.UpstreamTargetID, &site.UpstreamScheme,
		&site.UpstreamPort, &accessMode, &cidrsJSON, &usersJSON, &wsInt,
		&site.MaxBodyBytes, &site.ReadTimeoutSeconds, &enabled, &certID,
		&site.Version, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Site{}, ErrNotFound
		}
		return Site{}, fmt.Errorf("proxies: 读取站点失败: %w", err)
	}
	site.AppID = appID.String
	site.CertificateID = certID.String
	site.AccessMode = AccessMode(accessMode)
	site.WebSocketEnabled = wsInt != 0
	site.Enabled = enabled != 0
	if site.AllowedCIDRs, err = decodeCIDRs(cidrsJSON); err != nil {
		return Site{}, err
	}
	if site.BasicAuthUsers, err = decodeUsers(usersJSON); err != nil {
		return Site{}, err
	}
	if site.CreatedAt, err = parseTime(created); err != nil {
		return Site{}, err
	}
	if site.UpdatedAt, err = parseTime(updated); err != nil {
		return Site{}, err
	}
	return site, nil
}

func cidrJSON(in []string) (string, error) {
	if len(in) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("proxies: 序列化网段失败: %w", err)
	}
	return string(raw), nil
}

func decodeCIDRs(raw string) ([]string, error) {
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("proxies: 网段字段损坏: %w", err)
	}
	return out, nil
}

func usersJSON(in []BasicAuthUser) (string, error) {
	if len(in) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("proxies: 序列化访问账号失败: %w", err)
	}
	return string(raw), nil
}

func decodeUsers(raw string) ([]BasicAuthUser, error) {
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var out []BasicAuthUser
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("proxies: 访问账号字段损坏: %w", err)
	}
	return out, nil
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// isAppIDShape 只判形状，不查库。
func isAppIDShape(id string) bool {
	if len(id) != 4+idHexLen || !strings.HasPrefix(id, "app_") {
		return false
	}
	for i := 4; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// 站点列表游标。排序键是 (domain, id)：域名在库里唯一，但带上 id 让游标对未来的
// 排序键变化不敏感。
func encodeCursor(domain, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("site:" + domain + "\x00" + id))
}

func decodeCursor(cursor string) (domain, id string, err error) {
	if cursor == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", ErrInvalidCursor
	}
	rest, ok := strings.CutPrefix(string(raw), "site:")
	if !ok {
		return "", "", ErrInvalidCursor
	}
	domain, id, ok = strings.Cut(rest, "\x00")
	if !ok || domain == "" || id == "" {
		return "", "", ErrInvalidCursor
	}
	return domain, id, nil
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
