package agentstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Secrets 是加密秘密存储。所有写入都落密文，任何读取明文的路径都只在本包内部。
type Secrets struct {
	db     *DB
	key    *Keyring
	logger *slog.Logger
	ttl    time.Duration
	now    func() time.Time
}

// SecretsOptions 是构造 Secrets 的可选参数。生产一律使用默认值。
type SecretsOptions struct {
	// TTL 覆盖临时秘密的存活时间；0 表示使用 TemporarySecretTTL（10 分钟）。
	// 只有测试会改它，避免为了验证到期销毁而真的等十分钟。
	TTL time.Duration
	// Now 覆盖时钟；nil 表示使用 time.Now。
	Now func() time.Time
}

// NewSecrets 构造秘密存储。db 与 key 都必须非空——没有密钥就不该启动代理。
func NewSecrets(db *DB, key *Keyring, logger *slog.Logger, opts SecretsOptions) (*Secrets, error) {
	if db == nil {
		return nil, errors.New("agentstore: 秘密存储需要数据库句柄")
	}
	if key == nil {
		return nil, errors.New("agentstore: 秘密存储需要本机密钥")
	}
	if logger == nil {
		return nil, errors.New("agentstore: 秘密存储需要 Logger")
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = TemporarySecretTTL
	}
	if ttl < 0 {
		return nil, fmt.Errorf("agentstore: 临时秘密存活时间必须为正数，当前为 %s", ttl)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Secrets{db: db, key: key, logger: logger, ttl: ttl, now: now}, nil
}

// PutInput 是写入一条秘密的参数。
type PutInput struct {
	// Purpose 必须是 LookupPurpose 命中的可上传用途。
	Purpose string
	// Value 是明文；Put 返回后调用方应自行清零自己的副本（本包不会再引用它）。
	Value []byte
	// Reference 是引用这份秘密的配置标识。非空表示创建即被引用（persistent）；
	// 为空表示临时秘密，TemporarySecretTTL 后自动销毁。
	Reference string
}

// Put 加密并保存一条秘密，返回**元数据**（不含明文）。
//
// 明文只做一次加密，不复制、不落日志、不进错误信息；调用方传入的切片由调用方自己清零
// （HTTP 处理器在返回前会 wipe 掉请求体里的明文副本）。
func (s *Secrets) Put(ctx context.Context, in PutInput) (Secret, error) {
	purpose, ok := LookupPurpose(in.Purpose)
	if !ok {
		return Secret{}, fmt.Errorf("%w: 未登记的用途 %q", ErrPurpose, in.Purpose)
	}
	if !purpose.Uploadable {
		// totp_seed 这类内部用途不接受通用上传，必须走各自的专用流程。
		return Secret{}, fmt.Errorf("%w: 用途 %s 只能由代理内部流程写入，不接受上传", ErrPurpose, purpose.Name)
	}
	if err := purpose.validateValue(in.Value); err != nil {
		return Secret{}, err
	}
	if err := validateReference(in.Reference); err != nil {
		return Secret{}, err
	}

	now := s.now().UTC()
	secret := Secret{
		Purpose:   purpose.Name,
		Reference: in.Reference,
		CreatedAt: now,
		State:     StateTemporary,
	}
	if in.Reference != "" {
		secret.State = StatePersistent
	} else {
		secret.ExpiresAt = now.Add(s.ttl)
	}

	// 引用与密文绑定（AAD），所以必须先定下 ref 再加密。
	if err := s.insert(ctx, &secret, in.Value, now); err != nil {
		return Secret{}, err
	}
	s.logger.Info("已保存加密秘密",
		// 字段名用 ref 而不是 secret_ref：脱敏按字段名匹配，含 secret 的键会被整值打码，
		// 而引用本身不是秘密，必须在日志里可查。
		"ref", secret.Ref,
		"purpose", secret.Purpose,
		"state", secret.State,
		"bytes", len(in.Value),
		"key_version", s.key.Version(),
		"reference", secret.Reference,
		"expires_at", formatTimeValue(secret.ExpiresAt))
	return secret, nil
}

// PutInternal 写入**代理内部用途**的秘密（目前只有 totp_seed）。
//
// 与 Put 的差别只有一条：跳过 "Uploadable" 检查。其余校验（用途登记、值的长度区间、
// 引用形状）一条不少——内部流程同样是代码，代码也会写错。
//
// 为什么内部用途不能走通用上传：totp_seed 的明文是二次验证的全部，一旦允许 Web 层
// 直接 POST 一段种子上去，"种子由代理生成、Web 从未见过明文"这句话就不成立了
// （CONTRACTS.md 第 8 节：内部用途不接受通用秘密上传替代）。
func (s *Secrets) PutInternal(ctx context.Context, in PutInput) (Secret, error) {
	purpose, ok := LookupPurpose(in.Purpose)
	if !ok {
		return Secret{}, fmt.Errorf("%w: 未登记的用途 %q", ErrPurpose, in.Purpose)
	}
	if purpose.Uploadable {
		return Secret{}, fmt.Errorf("%w: 用途 %s 是可上传用途，应走 Put 而不是内部写入", ErrPurpose, purpose.Name)
	}
	if err := purpose.validateValue(in.Value); err != nil {
		return Secret{}, err
	}
	if err := validateReference(in.Reference); err != nil {
		return Secret{}, err
	}

	now := s.now().UTC()
	secret := Secret{
		Purpose:   purpose.Name,
		Reference: in.Reference,
		CreatedAt: now,
		State:     StateTemporary,
	}
	if in.Reference != "" {
		secret.State = StatePersistent
	} else {
		secret.ExpiresAt = now.Add(s.ttl)
	}
	if err := s.insert(ctx, &secret, in.Value, now); err != nil {
		return Secret{}, err
	}
	s.logger.Info("已保存加密秘密（内部用途）",
		"ref", secret.Ref,
		"purpose", secret.Purpose,
		"state", secret.State,
		"bytes", len(in.Value),
		"key_version", s.key.Version(),
		"reference", secret.Reference,
		"expires_at", formatTimeValue(secret.ExpiresAt))
	return secret, nil
}

// Binding 返回一条秘密的用途与引用标识，**不解密**。
//
// 二次验证要用它做归属核对：一个 seed_ref 是给 3 号用户的，就不能拿来验 5 号用户的码。
// 只核对"能解密"是不够的——任何一条 totp_seed 都解得开。
func (s *Secrets) Binding(ctx context.Context, ref string) (purpose, reference string, ok bool, err error) {
	if !ValidSecretRef(ref) {
		return "", "", false, nil
	}
	var state string
	err = s.db.store().SQL().QueryRowContext(ctx,
		`SELECT purpose, reference, state FROM secrets WHERE id = ?`, ref).
		Scan(&purpose, &reference, &state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("agentstore: 读取秘密归属失败: %w", err)
	}
	if state == StateDestroyed {
		return "", "", false, nil
	}
	return purpose, reference, true, nil
}

// Destroy 销毁一条指定的秘密：清空密文与 nonce，留墓碑行。
//
// 与 DestroyExpired 的区别只是范围：这里点名销毁一条，给"关闭二次验证"用。
// 已销毁的再销毁一次返回 nil——重复调用是幂等的，不制造第二种结果。
func (s *Secrets) Destroy(ctx context.Context, ref string) error {
	if !ValidSecretRef(ref) {
		return ErrSecretNotFound
	}
	now := s.now().UTC()
	res, err := s.db.store().ExecContext(ctx,
		`UPDATE secrets
		    SET state = ?, ciphertext = X'', nonce = X'', destroyed_at = ?
		  WHERE id = ? AND state <> ?`,
		StateDestroyed, millis(now), ref, StateDestroyed)
	if err != nil {
		return fmt.Errorf("agentstore: 销毁秘密失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("agentstore: 读取销毁结果失败: %w", err)
	}
	if affected == 0 {
		// 零行有两种可能：从来没有这条，或者已经是墓碑。后者按成功处理（幂等），
		// 前者必须报"不存在"，否则调用方会以为自己关掉了什么。
		var state string
		err := s.db.store().SQL().QueryRowContext(ctx,
			`SELECT state FROM secrets WHERE id = ?`, ref).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSecretNotFound
		}
		if err != nil {
			return fmt.Errorf("agentstore: 读取秘密状态失败: %w", err)
		}
		return nil
	}
	s.logger.Info("秘密已销毁", "ref", ref, "destroyed_at", formatTimeValue(now))
	return nil
}

// insert 生成引用、加密并写入一行。引用冲突时重试一次（36 字符随机撞车概率可忽略）。
func (s *Secrets) insert(ctx context.Context, secret *Secret, plaintext []byte, now time.Time) error {
	for attempt := 0; attempt < 2; attempt++ {
		ref, err := newSecretRef()
		if err != nil {
			return err
		}
		nonce, ciphertext, err := s.key.Seal(plaintext, ref, secret.Purpose)
		if err != nil {
			return err
		}
		_, err = s.db.store().ExecContext(ctx,
			`INSERT INTO secrets
			   (id, purpose, ciphertext, nonce, key_version, state, reference,
			    created_at, expires_at, referenced_at, destroyed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			ref, secret.Purpose, ciphertext, nonce, s.key.Version(), secret.State,
			secret.Reference, millis(now), millis(secret.ExpiresAt), millis(referencedAt(secret)))
		if err != nil {
			if isUniqueViolation(err) && attempt == 0 {
				continue
			}
			return fmt.Errorf("agentstore: 写入秘密失败: %w", err)
		}
		secret.Ref = ref
		return nil
	}
	return errors.New("agentstore: 生成不重复的秘密引用失败")
}

// referencedAt 返回“转为 persistent”的时间：只有创建即被引用的秘密才有。
func referencedAt(secret *Secret) time.Time {
	if secret.State == StatePersistent {
		return secret.CreatedAt
	}
	return time.Time{}
}

// Resolve 解密一条秘密，供代理内部流程使用（例如把仓库口令写进备份配置）。
//
// 这个方法是**代理内部接口**：不暴露任何 HTTP 路由，不返回给 Web，也不进日志。
// 调用方用完必须清零返回的字节。
func (s *Secrets) Resolve(ctx context.Context, ref string) ([]byte, error) {
	if !ValidSecretRef(ref) {
		return nil, ErrSecretNotFound
	}
	var (
		purpose     string
		ciphertext  []byte
		nonce       []byte
		keyVersion  int
		state       string
		expiresAtMS int64
	)
	err := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT purpose, ciphertext, nonce, key_version, state, expires_at
		   FROM secrets WHERE id = ?`, ref).
		Scan(&purpose, &ciphertext, &nonce, &keyVersion, &state, &expiresAtMS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSecretNotFound
		}
		return nil, fmt.Errorf("agentstore: 读取秘密失败: %w", err)
	}

	switch state {
	case StateDestroyed:
		return nil, ErrSecretNotFound
	case StateTemporary:
		if !s.now().UTC().Before(fromMillis(expiresAtMS)) {
			return nil, ErrSecretExpired
		}
	case StatePersistent:
	default:
		return nil, fmt.Errorf("agentstore: 秘密 %s 的状态 %q 不合法", ref, state)
	}
	if keyVersion != s.key.Version() {
		return nil, fmt.Errorf("agentstore: 秘密 %s 使用密钥版本 %d，当前密钥版本为 %d",
			ref, keyVersion, s.key.Version())
	}

	plaintext, err := s.key.Open(nonce, ciphertext, ref, purpose)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

// Reference 把一条临时秘密转为持久秘密：绑定配置标识，取消到期时间。
//
// 这是“未被配置引用的临时秘密 10 分钟后销毁；一旦被引用转为持久秘密”的后半句
// （CONTRACTS.md 第 8 节）。已被引用的秘密不能被再次引用到另一个位置。
func (s *Secrets) Reference(ctx context.Context, ref, reference string) error {
	if !ValidSecretRef(ref) {
		return ErrSecretNotFound
	}
	if reference == "" {
		return fmt.Errorf("%w: 引用标识不能为空", ErrReference)
	}
	if err := validateReference(reference); err != nil {
		return err
	}

	var (
		state     string
		existing  string
		expiresAt int64
	)
	err := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT state, reference, expires_at FROM secrets WHERE id = ?`, ref).
		Scan(&state, &existing, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSecretNotFound
		}
		return fmt.Errorf("agentstore: 读取秘密失败: %w", err)
	}

	switch state {
	case StateDestroyed:
		return ErrSecretNotFound
	case StatePersistent:
		if existing == reference {
			return nil
		}
		return fmt.Errorf("%w: 秘密 %s 已被 %q 引用，不能改挂到 %q", ErrReference, ref, existing, reference)
	case StateTemporary:
		if !s.now().UTC().Before(fromMillis(expiresAt)) {
			return ErrSecretExpired
		}
	default:
		return fmt.Errorf("agentstore: 秘密 %s 的状态 %q 不合法", ref, state)
	}

	_, err = s.db.store().ExecContext(ctx,
		`UPDATE secrets
		    SET state = ?, reference = ?, referenced_at = ?, expires_at = 0
		  WHERE id = ? AND state = ?`,
		StatePersistent, reference, millis(s.now().UTC()), ref, StateTemporary)
	if err != nil {
		return fmt.Errorf("agentstore: 转为持久秘密失败: %w", err)
	}
	s.logger.Info("临时秘密已转为持久秘密",
		"ref", ref, "reference", reference, "key_version", s.key.Version())
	return nil
}

// Purpose 返回一条秘密的用途与存在性，**不解密**（P12.1）。
//
// 为什么需要它：把一份秘密挂到某个配置上时，光“能解密”不够。拿一张 TLS 私钥当
// 备份仓库口令，解密是成功的，直到 restic 说“口令不对”才暴露——而那时失败的
// 原因已经离真正的原因很远了。用途在写入时就固定，正是为了在这里能被核对。
func (s *Secrets) Purpose(ctx context.Context, ref string) (string, bool, error) {
	if !ValidSecretRef(ref) {
		return "", false, nil
	}
	var purpose, state string
	err := s.db.store().SQL().QueryRowContext(ctx,
		`SELECT purpose, state FROM secrets WHERE id = ?`, ref).Scan(&purpose, &state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("agentstore: 读取秘密用途失败: %w", err)
	}
	// destroyed 是墓碑：行还在，但材料已清零，按“不存在”处理。这样“已销毁”
	// 与“从未存在”在调用方看来是同一种回答——它们对配置的含义确实相同。
	if state == StateDestroyed {
		return "", false, nil
	}
	return purpose, true, nil
}

// DestroyExpired 销毁所有已到期的临时秘密：清空密文与 nonce，留下墓碑行。
//
// 返回本次销毁的条数。只碰 state='temporary' 且 expires_at 已到的行——被引用的
// 持久秘密永远不在范围内。
func (s *Secrets) DestroyExpired(ctx context.Context, now time.Time) (int, error) {
	nowMS := millis(now.UTC())
	db := s.db.store()

	type pending struct {
		ref     string
		purpose string
	}
	var due []pending
	rows, err := db.SQL().QueryContext(ctx,
		`SELECT id, purpose FROM secrets
		  WHERE state = ? AND expires_at > 0 AND expires_at <= ?
		  ORDER BY expires_at`, StateTemporary, nowMS)
	if err != nil {
		return 0, fmt.Errorf("agentstore: 查询到期临时秘密失败: %w", err)
	}
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.ref, &p.purpose); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("agentstore: 解析到期临时秘密失败: %w", err)
		}
		due = append(due, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("agentstore: 遍历到期临时秘密失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("agentstore: 关闭结果集失败: %w", err)
	}
	if len(due) == 0 {
		return 0, nil
	}

	// 先更新再逐条记录：更新是幂等的（WHERE 带 state），重复扫描不会重复计数。
	res, err := db.ExecContext(ctx,
		`UPDATE secrets
		    SET state = ?, ciphertext = X'', nonce = X'', destroyed_at = ?
		  WHERE state = ? AND expires_at > 0 AND expires_at <= ?`,
		StateDestroyed, nowMS, StateTemporary, nowMS)
	if err != nil {
		return 0, fmt.Errorf("agentstore: 销毁到期临时秘密失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("agentstore: 读取销毁条数失败: %w", err)
	}
	destroyed := int(affected)
	for _, p := range due {
		if destroyed == 0 {
			break
		}
		// 墓碑信息不含任何秘密材料：只有引用、用途与时间。
		s.logger.Info("临时秘密已到期销毁",
			"ref", p.ref, "purpose", p.purpose, "destroyed_at", formatTimeValue(now))
	}
	return destroyed, nil
}

// formatTimeValue 把时间格式化成日志里可读的形式，零值显示 "-"。
func formatTimeValue(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
