package certificates

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
	"upanel/internal/tlscert"
)

// Usage 报告哪些站点绑着某张证书。
//
// 删除证书时要先问它：库层的 ON DELETE RESTRICT 会挡住删除，但那条错误信息对用户
// 没有意义（“FOREIGN KEY constraint failed”），而“还有两个站点在用这张证书”能直接
// 指向要先去改哪儿。
type Usage interface {
	SitesUsingCertificate(ctx context.Context, certID string) ([]string, error)
}

// Options 是 Service 的构造参数。
type Options struct {
	// Now 注入时钟（测试用）。
	Now func() time.Time
	// Paths 是证书落地路径（零值走默认）。
	Paths Paths
	// Usage 报告证书占用情况；为 nil 时删除前不做占用检查（仍然有库层外键兜底）。
	Usage Usage
}

// Service 是证书记录的唯一入口。
type Service struct {
	db    *store.DB
	now   func() time.Time
	paths Paths
	usage Usage
}

// New 构造 Service。db 为必需依赖。
func New(db *store.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("certificates: 数据库句柄不能为空")
	}
	s := &Service{db: db, now: opts.Now, paths: opts.Paths.WithDefaults(), usage: opts.Usage}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// InTransaction 在一个事务里执行 fn，供接口层把“记录变更 + 审计”合成一笔事务。
func (s *Service) InTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if fn == nil {
		return errors.New("certificates: InTransaction 需要非空函数")
	}
	return s.db.WithTx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return fn(ctx)
	})
}

func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Second) }

// Paths 返回生效的路径约定（只读给接口层拼展示用）。
func (s *Service) Paths() Paths { return s.paths }

// CreateInput 是新建一条证书记录所需的全部输入。
//
// Info 必须来自**解析过的证书**（tlscert.Info）：本服务不接受“调用方说有这些域名”，
// 而是把解析结果写进库。签发与导入两条来路都走这里。
type CreateInput struct {
	// ID 是可选的记录标识。空表示由这里生成。
	//
	// 为什么允许调用方指定：证书任务的参数里带着 certificate_id（站点绑定的是这个
	// 标识），任务报告回来的也是它。让账本按报告里的 id 落库，才能保证"站点绑的那条
	// 记录"与"任务实际装上去的那张证书"是同一行；否则绑定会指向一个空标识。
	ID string
	// Domain 是绑定的域名（调用方已归一化）。
	Domain string
	// Info 是证书解析结果。
	Info tlscert.Info
	// Source 是来路。
	Source Source
	// KeySecretRef 是手动导入的私钥引用（certbot 来路必须为空）。
	KeySecretRef string
	// ACMEEmail 是 Certbot 账号邮箱（certbot 来路必须有效，手动导入必须为空）。
	ACMEEmail string
	// AttemptAt 是这次尝试的时间（签发/导入发生的那一刻）。
	AttemptAt time.Time
}

// Create 新建一条证书记录。
func (s *Service) Create(ctx context.Context, in CreateInput) (Certificate, error) {
	prepared, err := s.prepare(in)
	if err != nil {
		return Certificate{}, err
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		generated, gerr := NewID()
		if gerr != nil {
			return Certificate{}, gerr
		}
		id = generated
	} else if !ValidID(id) {
		return Certificate{}, invalid("id", "证书标识必须是 crt_ 加 32 位小写十六进制")
	}
	now := s.nowUTC()
	attempt := in.AttemptAt.UTC().Truncate(time.Second)
	if attempt.IsZero() {
		attempt = now
	}
	domainsJSON, err := encodeDomains(prepared.Info.Domains)
	if err != nil {
		return Certificate{}, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO certificates (id, domain, domains_json, source, staging, issuer, subject,
		                          not_before, not_after, fingerprint, cert_path, key_path,
		                          key_secret_ref, acme_email, last_attempt_at, last_error,
		                          version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 1, ?, ?)`,
		id, prepared.Domain, domainsJSON, string(prepared.Source), boolInt(prepared.Info.Staging),
		prepared.Info.Issuer, prepared.Info.Subject,
		formatTime(prepared.Info.NotBefore), formatTime(prepared.Info.NotAfter),
		prepared.Info.Fingerprint,
		s.paths.CertPathOf(prepared.Domain, prepared.Source),
		s.paths.KeyPathOf(prepared.Domain, prepared.Source),
		prepared.KeySecretRef, prepared.ACMEEmail, formatTime(attempt),
		formatTime(now), formatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return Certificate{}, ErrDomainTaken
		}
		return Certificate{}, fmt.Errorf("certificates: 创建证书记录失败: %w", err)
	}
	return s.Get(ctx, id)
}

// RenewInput 是“同一个域名换了一张新证书”的更新输入。
//
// 续期（certbot）与重新导入（manual）都是这件事：证书换了、绑定不变。记录 id 不变
// 才能让站点绑定、审计历史、告警一路跟着走；如果做成“删旧建新”，站点会先掉到
// 无证书状态，再绑定回去——中间那段时间是真的一直在提供服务。
type RenewInput struct {
	ID     string
	Info   tlscert.Info
	Source Source
	// KeySecretRef 为空表示“保留原来的引用”（certbot 续期时本来就没有引用）。
	KeySecretRef string
	// ACMEEmail 为空表示“保留原来的邮箱”。
	ACMEEmail string
	// Staging 用证书自身的事实覆盖（staging 证书续期还是 staging）。
	AttemptAt time.Time
}

// Renew 用新证书的事实更新一条既有记录。
func (s *Service) Renew(ctx context.Context, in RenewInput) (Certificate, error) {
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return Certificate{}, err
	}
	if in.Source != "" && in.Source != current.Source {
		return Certificate{}, invalid("source", "证书来路不能通过续期改变（当前是 %s）", current.Source)
	}
	if in.Info.IsCA {
		return Certificate{}, invalid("certificate", "这是 CA 证书，不能给站点使用")
	}
	if !tlscert.CoversDomain(in.Info, current.Domain) {
		return Certificate{}, fmt.Errorf("%w: 新证书不覆盖 %s", ErrNotCovered, current.Domain)
	}
	if in.Info.Expired(s.nowUTC()) {
		return Certificate{}, invalid("certificate", "新证书已经过期（%s）", formatTime(in.Info.NotAfter))
	}
	keyRef := in.KeySecretRef
	if keyRef == "" {
		keyRef = current.KeySecretRef
	}
	email := in.ACMEEmail
	if email == "" {
		email = current.ACMEEmail
	}
	if current.Source == SourceManual && keyRef == "" {
		return Certificate{}, invalid("private_key_secret_ref", "手动导入的证书必须带私钥引用")
	}
	if current.Source == SourceCertbot && !ValidEmail(email) {
		return Certificate{}, invalid("acme_email", "Certbot 证书必须保留一个有效邮箱")
	}

	domainsJSON, err := encodeDomains(in.Info.Domains)
	if err != nil {
		return Certificate{}, err
	}
	now := s.nowUTC()
	attempt := in.AttemptAt.UTC().Truncate(time.Second)
	if attempt.IsZero() {
		attempt = now
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE certificates SET domains_json = ?, staging = ?, issuer = ?, subject = ?,
		                        not_before = ?, not_after = ?, fingerprint = ?,
		                        key_secret_ref = ?, acme_email = ?, last_attempt_at = ?,
		                        last_error = '', version = version + 1, updated_at = ?
		WHERE id = ?`,
		domainsJSON, boolInt(in.Info.Staging), in.Info.Issuer, in.Info.Subject,
		formatTime(in.Info.NotBefore), formatTime(in.Info.NotAfter), in.Info.Fingerprint,
		keyRef, email, formatTime(attempt), formatTime(now), in.ID)
	if err != nil {
		return Certificate{}, fmt.Errorf("certificates: 更新证书失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Certificate{}, fmt.Errorf("certificates: 读取证书更新结果失败: %w", err)
	}
	if affected == 0 {
		return Certificate{}, ErrNotFound
	}
	return s.Get(ctx, in.ID)
}

// RecordAttempt 记下一次尝试的时间与（可选的）失败原因。
//
// 它在**预检通过之后、真正开始签发之前**被调用一次，也在失败之后被调用一次。
// 之所以要提前记：如果只在失败后记，一个被反复重启的任务永远不会写下“我试过了”，
// 而限流正是靠这个字段。失败原因写成空串表示“这次成功了”。
func (s *Service) RecordAttempt(ctx context.Context, id string, at time.Time, failure string) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	if at.IsZero() {
		at = s.nowUTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE certificates SET last_attempt_at = ?, last_error = ?,
		                        version = version + 1, updated_at = ?
		WHERE id = ?`,
		formatTime(at.UTC().Truncate(time.Second)), truncateRunes(failure, 2000),
		formatTime(s.nowUTC()), id)
	if err != nil {
		return fmt.Errorf("certificates: 记录尝试失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("certificates: 读取尝试记录结果失败: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Get 按 id 读取一条记录。
func (s *Service) Get(ctx context.Context, id string) (Certificate, error) {
	if !ValidID(id) {
		return Certificate{}, ErrNotFound
	}
	row := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, domain, domains_json, source, staging, issuer, subject, not_before,
		       not_after, fingerprint, cert_path, key_path, key_secret_ref, acme_email,
		       last_attempt_at, last_error, version, created_at, updated_at
		FROM certificates WHERE id = ?`, id)
	return scanCertificate(row)
}

// GetByDomain 按域名读取（域名必须已归一化）。
func (s *Service) GetByDomain(ctx context.Context, domain string) (Certificate, error) {
	row := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, domain, domains_json, source, staging, issuer, subject, not_before,
		       not_after, fingerprint, cert_path, key_path, key_secret_ref, acme_email,
		       last_attempt_at, last_error, version, created_at, updated_at
		FROM certificates WHERE domain = ?`, domain)
	return scanCertificate(row)
}

// ListOptions 是列表参数。
type ListOptions struct {
	Limit  int
	Cursor string
	Domain string
}

// List 返回一页证书，按 (domain, id) 稳定排序；第二个返回值是下一页游标。
func (s *Service) List(ctx context.Context, opts ListOptions) ([]Certificate, string, error) {
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
		SELECT id, domain, domains_json, source, staging, issuer, subject, not_before,
		       not_after, fingerprint, cert_path, key_path, key_secret_ref, acme_email,
		       last_attempt_at, last_error, version, created_at, updated_at
		FROM certificates`
	var where []string
	var args []any
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

	rows, err := s.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("certificates: 查询证书列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Certificate, 0, limit)
	for rows.Next() {
		cert, err := scanCertificate(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, cert)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("certificates: 读取证书列表失败: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeCursor(out[len(out)-1].Domain, out[len(out)-1].ID)
	}
	return out, next, nil
}

// Delete 删除一条证书记录。
//
// 先问占用（有站点绑着就 409），再删；库层的外键是最后一道防线。注意这里**不删
// 主机上的证书文件**：手动导入的文件在 /etc/ssl/upanel，certbot 的文件由 certbot
// 自己管（它还有 renewal 配置与账号）。删记录只是“面板不再管它”，把别人的目录
// 顺手清掉是另一件需要单独证明的事。
func (s *Service) Delete(ctx context.Context, id string, version int) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	if s.usage != nil {
		sites, err := s.usage.SitesUsingCertificate(ctx, id)
		if err != nil {
			return fmt.Errorf("certificates: 查询证书占用失败: %w", err)
		}
		if len(sites) > 0 {
			return fmt.Errorf("%w: %s", ErrInUse, strings.Join(sites, "、"))
		}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM certificates WHERE id = ? AND version = ?`, id, version)
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrInUse
		}
		return fmt.Errorf("certificates: 删除证书失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("certificates: 删除证书失败: %w", err)
	}
	if affected == 0 {
		return s.classifyMiss(ctx, id)
	}
	return nil
}

// prepare 校验并补齐一条新建记录的输入。
func (s *Service) prepare(in CreateInput) (CreateInput, error) {
	out := in
	out.Domain = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(in.Domain)), ".")
	if out.Domain == "" || len(out.Domain) > 253 {
		return CreateInput{}, invalid("domain", "域名为空或过长")
	}
	if !in.Source.Valid() {
		return CreateInput{}, invalid("source", "来路只能是 certbot 或 manual")
	}
	if in.Info.Fingerprint == "" || len(in.Info.Domains) == 0 {
		return CreateInput{}, invalid("certificate", "证书事实不完整（缺少指纹或域名）")
	}
	if in.Info.IsCA {
		return CreateInput{}, invalid("certificate", "这是 CA 证书，不能给站点使用")
	}
	if !tlscert.CoversDomain(in.Info, out.Domain) {
		return CreateInput{}, fmt.Errorf("%w: 证书不覆盖 %s（覆盖 %s）",
			ErrNotCovered, out.Domain, strings.Join(in.Info.Domains, "、"))
	}
	if in.Info.Expired(s.nowUTC()) {
		return CreateInput{}, invalid("certificate", "证书已经过期（%s）", formatTime(in.Info.NotAfter))
	}
	switch out.Source {
	case SourceManual:
		if strings.TrimSpace(in.KeySecretRef) == "" {
			return CreateInput{}, invalid("private_key_secret_ref", "手动导入必须带私钥引用")
		}
		if in.ACMEEmail != "" {
			return CreateInput{}, invalid("acme_email", "手动导入的证书不该带 ACME 邮箱")
		}
	case SourceCertbot:
		if !ValidEmail(in.ACMEEmail) {
			return CreateInput{}, invalid("acme_email", "Certbot 签发需要一个有效邮箱（ACME 账号用）")
		}
		if in.KeySecretRef != "" {
			return CreateInput{}, invalid("private_key_secret_ref", "Certbot 证书的私钥由 certbot 保管，不接受引用")
		}
	}
	return out, nil
}

// classifyMiss 把“更新/删除影响零行”分成“不存在”和“版本冲突”。
func (s *Service) classifyMiss(ctx context.Context, id string) error {
	var exists int
	err := s.db.SQL().QueryRowContext(ctx, `SELECT 1 FROM certificates WHERE id = ?`, id).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("certificates: 查询证书失败: %w", err)
	}
	return ErrVersionConflict
}

// rowScanner 是 *sql.Row 与 *sql.Rows 的公共部分。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCertificate(row rowScanner) (Certificate, error) {
	var (
		cert        Certificate
		domainsJSON string
		source      string
		staging     int
		notBefore   string
		notAfter    string
		attempt     string
		createdAt   string
		updatedAt   string
	)
	err := row.Scan(&cert.ID, &cert.Domain, &domainsJSON, &source, &staging, &cert.Issuer,
		&cert.Subject, &notBefore, &notAfter, &cert.Fingerprint, &cert.CertPath, &cert.KeyPath,
		&cert.KeySecretRef, &cert.ACMEEmail, &attempt, &cert.LastError, &cert.Version,
		&createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Certificate{}, ErrNotFound
		}
		return Certificate{}, fmt.Errorf("certificates: 读取证书记录失败: %w", err)
	}
	cert.Source = Source(source)
	cert.Staging = staging == 1
	if cert.Domains, err = decodeDomains(domainsJSON); err != nil {
		return Certificate{}, err
	}
	if cert.NotBefore, err = parseTime(notBefore); err != nil {
		return Certificate{}, err
	}
	if cert.NotAfter, err = parseTime(notAfter); err != nil {
		return Certificate{}, err
	}
	if cert.LastAttemptAt, err = parseTime(attempt); err != nil {
		return Certificate{}, err
	}
	if cert.CreatedAt, err = parseTime(createdAt); err != nil {
		return Certificate{}, err
	}
	if cert.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Certificate{}, err
	}
	return cert, nil
}

func encodeDomains(domains []string) (string, error) {
	if len(domains) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(domains)
	if err != nil {
		return "", fmt.Errorf("certificates: 编码域名列表失败: %w", err)
	}
	return string(raw), nil
}

func decodeDomains(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("certificates: 解析域名列表失败: %w", err)
	}
	return out, nil
}

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
		return time.Time{}, fmt.Errorf("certificates: 时间格式无效: %w", err)
	}
	return t.UTC(), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// 游标：base64url(domain \x00 id)，与 proxies 同一套形状。
func encodeCursor(domain, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(domain + "\x00" + id))
}

func decodeCursor(cursor string) (string, string, error) {
	if cursor == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", ErrInvalidCursor
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 || parts[0] == "" || !ValidID(parts[1]) {
		return "", "", ErrInvalidCursor
	}
	return parts[0], parts[1], nil
}

// isUniqueViolation / isForeignKeyViolation 与 proxies 的同名函数同源。
//
// 不共用一份实现：那两个函数是 internal/proxies 的私有细节，跨包导出它们会让
// “两个模块共用错误判断”变成一个需要维护的公共 API，而这条判断只有两行。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY")
}
