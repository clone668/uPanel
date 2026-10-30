package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// channelColumns 是通道表读取列的固定顺序，与 scanChannel 一一对应。
const channelColumns = `id, name, type, settings, secret_ref, enabled, max_attempts, version,
	created_at, updated_at`

// normalizeChannel 校验并标准化一份通道输入。
//
// 返回值里的 Settings 是**重新构造的**结构体（不是入参那份），序列化之后写库。
// 这样库里不会留下本进程不认识的键：解码时 DisallowUnknownFields 已经挡了一层，
// 这里再挡一层“字段存在但类型不对被静默丢掉”的情况。
func normalizeChannel(in ChannelInput) (ChannelInput, error) {
	out := ChannelInput{
		Name:        strings.TrimSpace(in.Name),
		Type:        strings.TrimSpace(in.Type),
		SecretRef:   strings.TrimSpace(in.SecretRef),
		Enabled:     in.Enabled,
		MaxAttempts: in.MaxAttempts,
	}
	if out.Name == "" {
		return ChannelInput{}, invalid("name", "不能为空")
	}
	if len([]rune(out.Name)) > MaxNameLen {
		return ChannelInput{}, invalid("name", "长度不能超过 %d 个字符", MaxNameLen)
	}
	if out.MaxAttempts == 0 {
		out.MaxAttempts = DefaultMaxAttempts
	}
	if out.MaxAttempts < 1 || out.MaxAttempts > MaxAttempts {
		return ChannelInput{}, invalid("max_attempts", "必须在 1—%d 之间", MaxAttempts)
	}

	switch out.Type {
	case TypeInApp:
		// 面板内通知没有参数、没有秘密、也没有“发不出去”这回事。
		if out.SecretRef != "" {
			return ChannelInput{}, invalid("secret_ref", "面板内通知不使用秘密")
		}
		out.Settings = Settings{}
		out.MaxAttempts = 1
		return out, nil

	case TypeSMTP:
		st := Settings{
			Host:     strings.TrimSpace(in.Settings.Host),
			Port:     in.Settings.Port,
			TLS:      strings.TrimSpace(in.Settings.TLS),
			From:     strings.TrimSpace(in.Settings.From),
			Username: strings.TrimSpace(in.Settings.Username),
		}
		if st.Host == "" {
			return ChannelInput{}, invalid("settings.host", "不能为空")
		}
		if len(st.Host) > 253 || strings.ContainsAny(st.Host, " \t\r\n/") {
			return ChannelInput{}, invalid("settings.host", "不是合法的主机名")
		}
		if st.Port < 1 || st.Port > 65535 {
			return ChannelInput{}, invalid("settings.port", "必须是 1—65535")
		}
		// 没有“明文”这个选项：端口写 25 而 tls 留空，等于让口令在网线上裸奔。
		if st.TLS != TLSStartTLS && st.TLS != TLSImplicit {
			return ChannelInput{}, invalid("settings.tls", "必须是 %s 或 %s（不接受不加密的 SMTP）",
				TLSStartTLS, TLSImplicit)
		}
		if !looksLikeAddress(st.From) {
			return ChannelInput{}, invalid("settings.from", "不是合法的邮件地址")
		}
		to := make([]string, 0, len(in.Settings.To))
		seen := make(map[string]struct{}, len(in.Settings.To))
		for _, raw := range in.Settings.To {
			addr := strings.TrimSpace(raw)
			if addr == "" {
				continue
			}
			if !looksLikeAddress(addr) {
				return ChannelInput{}, invalid("settings.to", "不是合法的邮件地址：%q", addr)
			}
			if _, dup := seen[addr]; dup {
				continue
			}
			seen[addr] = struct{}{}
			to = append(to, addr)
		}
		if len(to) == 0 {
			return ChannelInput{}, invalid("settings.to", "至少要有一个收件人")
		}
		if len(to) > MaxRecipients {
			return ChannelInput{}, invalid("settings.to", "收件人不能超过 %d 个", MaxRecipients)
		}
		st.To = to
		// 有口令就必须有用户名（代理不知道怎么用一个没有名字的口令），
		// 有用户名就必须有口令（否则那一栏是骗人的）。
		if st.Username == "" && out.SecretRef != "" {
			return ChannelInput{}, invalid("settings.username", "给了口令就必须给用户名")
		}
		if st.Username != "" && out.SecretRef == "" {
			return ChannelInput{}, invalid("secret_ref", "给了用户名就必须先保存口令（POST /secrets，用途 notification_password）")
		}
		out.Settings = st
		return out, nil

	case TypeWebhook:
		if _, err := validateURL(strings.TrimSpace(in.Settings.URL)); err != nil {
			return ChannelInput{}, err
		}
		out.Settings = Settings{
			URL:          strings.TrimSpace(in.Settings.URL),
			AllowPrivate: in.Settings.AllowPrivate,
		}
		// 令牌是可选的：有些 webhook 靠 URL 里的路径做鉴权（那不是秘密，只是不好猜）。
		// 但只要给了 secret_ref，投递时就一定带 Authorization: Bearer。
		return out, nil

	default:
		return ChannelInput{}, invalid("type", "必须是 %s/%s/%s 之一", TypeInApp, TypeSMTP, TypeWebhook)
	}
}

// encodeSettings 把已校验的设置序列化成写库的 JSON。
func encodeSettings(st Settings) (string, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("notifications: 序列化通道参数失败: %w", err)
	}
	return string(raw), nil
}

// decodeSettings 把库里的 JSON 解成设置。库里出现不认识的键时**报错而不是忽略**：
// 那说明有人绕过本进程写过这一行，静默忽略会让“配置看起来生效了”变成常态。
func decodeSettings(raw string) (Settings, error) {
	var st Settings
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return Settings{}, fmt.Errorf("notifications: 通道参数无法解析: %w", err)
	}
	return st, nil
}

// Create 新建一条通道。
func (s *Service) CreateChannel(ctx context.Context, in ChannelInput) (Channel, error) {
	norm, err := normalizeChannel(in)
	if err != nil {
		return Channel{}, err
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM notification_channels`).Scan(&count); err != nil {
		return Channel{}, fmt.Errorf("notifications: 统计通道数量失败: %w", err)
	}
	if count >= MaxChannels {
		return Channel{}, ErrLimitReached
	}
	if norm.Type == TypeInApp {
		// 面板内通知恒有一条（迁移里就建好了），不能再建第二条。
		var exists int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM notification_channels WHERE type = ?`,
			TypeInApp).Scan(&exists); err != nil {
			return Channel{}, fmt.Errorf("notifications: 统计面板内通道失败: %w", err)
		}
		if exists > 0 {
			return Channel{}, invalid("type", "面板内通知只有一条，不能重复创建")
		}
	}
	settings, err := encodeSettings(norm.Settings)
	if err != nil {
		return Channel{}, err
	}
	now := formatTime(s.nowUTC())
	res, err := s.exec(ctx, `
		INSERT INTO notification_channels
		    (name, type, settings, secret_ref, enabled, max_attempts, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		norm.Name, norm.Type, settings, nullStringValue(norm.SecretRef), boolInt(norm.Enabled),
		norm.MaxAttempts, now, now)
	if err != nil {
		return Channel{}, fmt.Errorf("notifications: 写入通道失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Channel{}, fmt.Errorf("notifications: 读取通道 id 失败: %w", err)
	}
	return s.GetChannel(ctx, id)
}

// GetChannel 读取一条通道。
func (s *Service) GetChannel(ctx context.Context, id int64) (Channel, error) {
	if id <= 0 {
		return Channel{}, ErrNotFound
	}
	row := s.queryRow(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = ?`, id)
	ch, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, fmt.Errorf("notifications: 读取通道失败: %w", err)
	}
	return ch, nil
}

// ListChannels 返回全部通道，按 id 升序。
func (s *Service) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.query(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("notifications: 读取通道列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]Channel, 0, 8)
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("notifications: 读取通道列表失败: %w", err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notifications: 读取通道列表失败: %w", err)
	}
	return out, nil
}

// enabledChannels 返回启用的通道（投递入队时用）。
func (s *Service) enabledChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.query(ctx, `SELECT `+channelColumns+` FROM notification_channels
		WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("notifications: 读取启用通道失败: %w", err)
	}
	defer rows.Close()
	out := make([]Channel, 0, 8)
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("notifications: 读取启用通道失败: %w", err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notifications: 读取启用通道失败: %w", err)
	}
	return out, nil
}

// UpdateChannel 用一份完整输入替换通道内容（version 必须匹配）。
//
// 改通道**不重发**已经发过的投递：投递账是事实，改配置只影响之后入队的事件。
func (s *Service) UpdateChannel(ctx context.Context, id, version int64, in ChannelInput) (Channel, error) {
	if id <= 0 {
		return Channel{}, ErrNotFound
	}
	norm, err := normalizeChannel(in)
	if err != nil {
		return Channel{}, err
	}
	settings, err := encodeSettings(norm.Settings)
	if err != nil {
		return Channel{}, err
	}
	res, err := s.exec(ctx, `
		UPDATE notification_channels
		   SET name = ?, settings = ?, secret_ref = ?, enabled = ?, max_attempts = ?,
		       version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		norm.Name, settings, nullStringValue(norm.SecretRef), boolInt(norm.Enabled),
		norm.MaxAttempts, formatTime(s.nowUTC()), id, version)
	if err != nil {
		return Channel{}, fmt.Errorf("notifications: 更新通道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Channel{}, fmt.Errorf("notifications: 更新通道失败: %w", err)
	}
	if affected == 0 {
		return Channel{}, s.conflictOrMissing(ctx, id)
	}
	return s.GetChannel(ctx, id)
}

// DeleteChannel 删除一条通道（version 必须匹配）。投递账里的行保留（channel_id 置空）。
//
// 面板内通知不能删：它是收件箱本身。停用它就够了（停用之后事件不再进收件箱）。
func (s *Service) DeleteChannel(ctx context.Context, id, version int64) error {
	ch, err := s.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if version > 0 && ch.Version != version {
		return ErrVersionConflict
	}
	if ch.Type == TypeInApp {
		return invalid("type", "面板内通知不能删除，改用 enabled=false 停用")
	}
	res, err := s.exec(ctx, `DELETE FROM notification_channels WHERE id = ? AND version = ?`,
		id, ch.Version)
	if err != nil {
		return fmt.Errorf("notifications: 删除通道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notifications: 删除通道失败: %w", err)
	}
	if affected == 0 {
		return ErrVersionConflict
	}
	return nil
}

func (s *Service) conflictOrMissing(ctx context.Context, id int64) error {
	var exists int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM notification_channels WHERE id = ?`,
		id).Scan(&exists); err != nil {
		return fmt.Errorf("notifications: 读取通道失败: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	return ErrVersionConflict
}

func scanChannel(sc scanner) (Channel, error) {
	var (
		ch                   Channel
		settings             string
		secretRef            sql.NullString
		enabled              int64
		maxAttempts          int64
		createdAt, updatedAt string
	)
	if err := sc.Scan(&ch.ID, &ch.Name, &ch.Type, &settings, &secretRef, &enabled,
		&maxAttempts, &ch.Version, &createdAt, &updatedAt); err != nil {
		return Channel{}, err
	}
	st, err := decodeSettings(settings)
	if err != nil {
		return Channel{}, err
	}
	ch.Settings = st
	ch.SecretRef = secretRef.String
	ch.Enabled = enabled != 0
	ch.MaxAttempts = int(maxAttempts)
	if ch.CreatedAt, err = parseTime(createdAt); err != nil {
		return Channel{}, err
	}
	if ch.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Channel{}, err
	}
	return ch, nil
}

func nullStringValue(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
