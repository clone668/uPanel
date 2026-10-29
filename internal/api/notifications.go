package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/notifications"
)

// 通知通道接口（P13.3）。这一层与告警接口同构：解请求、把领域错误翻成契约错误码、
// 拼审计。判定全在 internal/notifications。
//
// **这一层永远看不到口令。** SMTP 口令与 webhook 令牌由调用方上传成秘密，接口只收
// 一个 secret_ref；发送时由代理去解。所以这里所有 DTO 都不含明文，审计也不含。

// notificationChannelDTO 是一条通道的对外形状。
//
// Target 是“这条通道往哪发”的人读形式（收件人列表或 URL）：界面上要能一眼看出
// 告警会发到哪个邮箱，而不是让人自己去 settings 里拼。
type notificationChannelDTO struct {
	ID          int64                  `json:"id"`
	Name        string                 `json:"name"`
	Type        string                 `json:"type"`
	Settings    notifications.Settings `json:"settings"`
	SecretRef   *string                `json:"secret_ref"`
	Enabled     bool                   `json:"enabled"`
	MaxAttempts int                    `json:"max_attempts"`
	Version     int64                  `json:"version"`
	Target      string                 `json:"target"`
	CreatedAt   string                 `json:"created_at"`
	UpdatedAt   string                 `json:"updated_at"`
}

// notificationDeliveryDTO 是一次投递的对外形状（测试发送的即时回执，以及 P13.4 的
// 投递账/面板收件箱）。
//
// LastError 与 Detail 里放的是**对端原话**（"550 5.1.1 user unknown"），不是我们
// 猜的原因。运维看到这句话就知道该去改什么；看到"发送失败"只能去翻日志。
//
// 同一个形状两处用（测试回执与列表行）是刻意的：投递行就是投递行，给它两套字段
// 只会让人对着两个几乎一样的类型猜"哪个才是真的"。列表比回执多的信息不过是
// 事件引用与正文——那两样在测试行里本来就是空（kind=test，没有事件）。
type notificationDeliveryDTO struct {
	ID          int64  `json:"id"`
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	ChannelType string `json:"channel_type"`
	Target      string `json:"target"`
	// EventID 为 0 表示这是一次测试发送（或事件行已被删掉）。
	EventID   int64  `json:"event_id"`
	EventRule string `json:"event_rule"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	// MaxAttempts 是入队时从通道上抄下来的重试上限快照。
	MaxAttempts int     `json:"max_attempts"`
	LastStatus  int     `json:"last_status"`
	LastError   *string `json:"last_error"`
	// NextAttemptAt 为 null 表示"不会再排下一次"：已经发出去了、已经判死、
	// 或者正在发（抢占时不清空它）。它不是"下次重试倒计时"，界面别拿它当进度条。
	NextAttemptAt *string `json:"next_attempt_at"`
	SentAt        *string `json:"sent_at"`
	CreatedAt     string  `json:"created_at"`
}

// notificationChannelRequest 是创建与修改共用的请求体。
//
// 用指针是为了让 PATCH 分得清“没提这个字段”和“把它设成零值”：enabled=false、
// max_attempts=0（非法）、secret_ref=""（清除）都是有意义的操作。
//
// Settings 是整体替换而不是逐字段合并：smtp 与 webhook 的设置字段集不同，逐字段
// 合并会在“把 smtp 通道改成 webhook”时留下一堆无主的 host/port。整体替换的语义
// 只有一种，不需要在文档里解释“改了 type 之后旧字段怎么办”。
type notificationChannelRequest struct {
	Name        *string                 `json:"name"`
	Type        *string                 `json:"type"`
	Settings    *notifications.Settings `json:"settings"`
	SecretRef   *string                 `json:"secret_ref"`
	Enabled     *bool                   `json:"enabled"`
	MaxAttempts *int                    `json:"max_attempts"`
	Version     *int64                  `json:"version"`
}

// requireNotifications 取出通知服务。未装配时回 503 并说明原因，而不是回一份空列表。
func (s *Server) requireNotifications(c *gin.Context) (*notifications.Service, bool) {
	if s.opts.Notifications == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeNotImplemented,
			"这个进程没有装配通知功能（未连接面板数据库）")
		return nil, false
	}
	return s.opts.Notifications, true
}

// handleListNotificationChannels 处理 GET /api/v1/notification-channels（admin）。
//
// 读也给 admin 而不是 viewer：通道列表里带着 webhook 地址与收件邮箱，那是“告警
// 会发给谁”的答案。viewer 能看告警规则已经够了，不必知道外部投递的落点。
func (s *Server) handleListNotificationChannels(c *gin.Context) {
	svc, ok := s.requireNotifications(c)
	if !ok {
		return
	}
	channels, err := svc.ListChannels(c.Request.Context())
	if err != nil {
		s.internalError(c, "读取通知通道失败", err)
		return
	}
	out := make([]notificationChannelDTO, 0, len(channels))
	for _, ch := range channels {
		out = append(out, notificationChannelDTOOf(ch))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleListNotificationDeliveries 处理 GET /api/v1/notification-deliveries（admin）。
//
// 这是**面板收件箱**的读路径，也是"那次告警到底发没发出去"的唯一答案。它读的是
// 面板库里的投递账，不经过代理：代理此刻是否连得上，与"三天前那封邮件发出去了没有"
// 是两件不相干的事——把历史查询绑在代理的可用性上，等于代理一断就看不见历史。
//
// 权限跟通道列表一致（admin），理由也一致：行里带着收件邮箱与 webhook 地址。
//
// 过滤只有 state 与 kind 两个（都是闭集）加 limit，没有时间范围与全文搜索：
// 投递账的用处是"最近发生了什么"，翻三个月前的投递该去查审计，不该在这里加索引。
func (s *Server) handleListNotificationDeliveries(c *gin.Context) {
	svc, ok := s.requireNotifications(c)
	if !ok {
		return
	}
	q := notifications.DeliveryQuery{State: c.Query("state"), Kind: c.Query("kind")}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "limit 必须是正整数")
			return
		}
		q.Limit = n
	}
	items, err := svc.ListDeliveries(c.Request.Context(), q)
	if err != nil {
		if notifications.IsInvalidChannel(err) {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
		s.internalError(c, "读取投递账失败", err)
		return
	}
	out := make([]notificationDeliveryDTO, 0, len(items))
	for _, d := range items {
		out = append(out, notificationDeliveryDTOOf(d))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleCreateNotificationChannel 处理 POST /api/v1/notification-channels（admin + CSRF）。
func (s *Server) handleCreateNotificationChannel(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionNotificationChannelCreate

	var req notificationChannelRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, "notification_channel", "validation", httpx.CodeValidationError)
		return
	}
	if req.Type == nil || *req.Type == "" {
		s.recordDenied(c, &sess, action, "notification_channel", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"type 必须给出：in_app / smtp / webhook")
		return
	}
	if req.Name == nil || *req.Name == "" {
		s.recordDenied(c, &sess, action, "notification_channel", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "name 必须给出")
		return
	}
	in := notifications.ChannelInput{
		Name:        *req.Name,
		Type:        *req.Type,
		Enabled:     true, // 新建的通道默认启用：建一条不启用的通道等于什么也没做。
		MaxAttempts: notifications.DefaultMaxAttempts,
	}
	applyChannelRequest(&in, req)

	var created notifications.Channel
	err := s.opts.Notifications.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		ch, err := s.opts.Notifications.CreateChannel(ctx, in)
		if err != nil {
			return &errDenied{err}
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, notificationChannelTarget(ch.ID),
			channelAuditDetails(ch)); err != nil {
			return err
		}
		created = ch
		return nil
	})
	if err != nil {
		s.failNotificationAction(c, &sess, action, "notification_channel", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, notificationChannelDTOOf(created))
}

// handleUpdateNotificationChannel 处理 PATCH /api/v1/notification-channels/:id（admin + CSRF）。
func (s *Server) handleUpdateNotificationChannel(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionNotificationChannelUpdate

	id, ok := s.parseNotificationID(c)
	if !ok {
		return
	}
	var req notificationChannelRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, notificationChannelTargetRaw(c.Param("id")),
			"validation", httpx.CodeValidationError)
		return
	}
	if req.Version == nil || *req.Version <= 0 {
		s.recordDenied(c, &sess, action, notificationChannelTarget(id), "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"version 必须给出：它是防止两次改同一条通道互相覆盖的依据")
		return
	}

	var updated notifications.Channel
	var before notifications.Channel
	err := s.opts.Notifications.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		current, err := s.opts.Notifications.GetChannel(ctx, id)
		if err != nil {
			return &errDenied{err}
		}
		before = current
		in := notifications.ChannelInput{
			Name:        current.Name,
			Type:        current.Type,
			Settings:    current.Settings,
			SecretRef:   current.SecretRef,
			Enabled:     current.Enabled,
			MaxAttempts: current.MaxAttempts,
		}
		applyChannelRequest(&in, req)
		ch, err := s.opts.Notifications.UpdateChannel(ctx, id, *req.Version, in)
		if err != nil {
			return &errDenied{err}
		}
		details := channelAuditDetails(ch)
		// 改动前后的落点一起记：审计要能回答“告警被改发到哪去了”。
		details["before"] = map[string]any{
			"name":         before.Name,
			"type":         before.Type,
			"target":       before.Target(),
			"secret_ref":   nullString(before.SecretRef),
			"enabled":      before.Enabled,
			"max_attempts": before.MaxAttempts,
			"version":      before.Version,
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, notificationChannelTarget(id), details); err != nil {
			return err
		}
		updated = ch
		return nil
	})
	if err != nil {
		s.failNotificationAction(c, &sess, action, notificationChannelTarget(id), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, notificationChannelDTOOf(updated))
}

// handleDeleteNotificationChannel 处理 DELETE /api/v1/notification-channels/:id（admin + CSRF）。
//
// in_app 通道删不掉（服务层拒绝）：它是面板自己的收件箱，删掉之后“告警去哪看”
// 就没有答案了。停用它才是想要的效果。
//
// 投递账不跟着删：外键是 SET NULL，历史留下来，仍然看得到“这条通道当时发过什么”。
func (s *Server) handleDeleteNotificationChannel(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionNotificationChannelDelete

	id, ok := s.parseNotificationID(c)
	if !ok {
		return
	}
	version := int64(0)
	if raw := c.Query("version"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			s.recordDenied(c, &sess, action, notificationChannelTarget(id), "validation", httpx.CodeValidationError)
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "version 必须是正整数")
			return
		}
		version = v
	}

	var deleted notifications.Channel
	err := s.opts.Notifications.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		ch, err := s.opts.Notifications.GetChannel(ctx, id)
		if err != nil {
			return &errDenied{err}
		}
		deleted = ch
		if err := s.opts.Notifications.DeleteChannel(ctx, id, version); err != nil {
			return &errDenied{err}
		}
		return s.recordAuditInTx(ctx, c, sess, action, notificationChannelTarget(id),
			channelAuditDetails(ch))
	})
	if err != nil {
		s.failNotificationAction(c, &sess, action, notificationChannelTarget(id), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, map[string]any{"id": deleted.ID})
}

// handleTestNotificationChannel 处理 POST /api/v1/notification-channels/:id/test。
//
// **测试发送是显式动作，保存时绝不自动发**（CONTRACTS §13 第 2515 行）：保存一次
// 配置就往外面喷一封邮件，是那种“没人要求、但取消不掉”的行为。
//
// 它只发一次，不重试：测试的目的是立刻知道通不通，不是把重试队列跑一遍。
// 结果回的是那一行投递账——包括对端原话，因为“到底为什么不通”才是这次点击的价值。
func (s *Server) handleTestNotificationChannel(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionNotificationChannelTest

	id, ok := s.parseNotificationID(c)
	if !ok {
		return
	}
	svc := s.opts.Notifications
	if svc == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeNotImplemented,
			"这个进程没有装配通知功能（未连接面板数据库）")
		return
	}

	var delivery notifications.Delivery
	err := svc.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		ch, err := svc.GetChannel(ctx, id)
		if err != nil {
			return &errDenied{err}
		}
		// 审计先写：这次点击真的会让一个请求离开这台机器，必须在它发生之前留痕。
		// 投递结果写进同一笔事务的 details 里没有意义（那时还不知道），所以这里记
		// “谁点了对哪条通道的测试”，结果留在投递账里。
		if err := s.recordAuditInTx(ctx, c, sess, action, notificationChannelTarget(id),
			map[string]any{
				"channel_name": ch.Name,
				"channel_type": ch.Type,
				"target":       ch.Target(),
			}); err != nil {
			return err
		}
		d, err := svc.SendTest(ctx, id)
		if err != nil {
			return &errDenied{err}
		}
		delivery = d
		return nil
	})
	if err != nil {
		s.failNotificationAction(c, &sess, action, notificationChannelTarget(id), err)
		return
	}
	// 投递本身失败（对端拒绝）不是接口错误：200 + 投递行，让界面把对端原话显示出来。
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, notificationDeliveryDTOOf(delivery))
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 转换与错误映射
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// applyChannelRequest 把请求体里给出的字段叠到输入上。
//
// 没给的字段一律不动：PATCH 的语义是“改我说的这些”，不是“把没说的清空”。
// 想清除 secret_ref 就显式给空串——那是唯一一个“空”有意义的地方。
func applyChannelRequest(in *notifications.ChannelInput, req notificationChannelRequest) {
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.Type != nil {
		in.Type = *req.Type
	}
	if req.Settings != nil {
		in.Settings = *req.Settings
	}
	if req.SecretRef != nil {
		in.SecretRef = *req.SecretRef
	}
	if req.Enabled != nil {
		in.Enabled = *req.Enabled
	}
	if req.MaxAttempts != nil {
		in.MaxAttempts = *req.MaxAttempts
	}
}

func notificationChannelDTOOf(ch notifications.Channel) notificationChannelDTO {
	return notificationChannelDTO{
		ID:          ch.ID,
		Name:        ch.Name,
		Type:        ch.Type,
		Settings:    ch.Settings,
		SecretRef:   nullString(ch.SecretRef),
		Enabled:     ch.Enabled,
		MaxAttempts: ch.MaxAttempts,
		Version:     ch.Version,
		Target:      ch.Target(),
		CreatedAt:   formatAlertTime(ch.CreatedAt),
		UpdatedAt:   formatAlertTime(ch.UpdatedAt),
	}
}

func notificationDeliveryDTOOf(d notifications.Delivery) notificationDeliveryDTO {
	return notificationDeliveryDTO{
		ID:            d.ID,
		ChannelID:     d.ChannelID,
		ChannelName:   d.ChannelName,
		ChannelType:   d.ChannelType,
		Target:        d.Target,
		EventID:       d.EventID,
		EventRule:     d.EventRule,
		Kind:          d.Kind,
		Title:         d.Title,
		Body:          d.Body,
		State:         d.State,
		Attempts:      d.Attempts,
		MaxAttempts:   d.MaxAttempts,
		LastStatus:    d.LastStatus,
		LastError:     nullString(d.LastError),
		NextAttemptAt: formatAlertTimeOrNil(d.NextAttemptAt),
		SentAt:        formatAlertTimeOrNil(d.SentAt),
		CreatedAt:     formatAlertTime(d.CreatedAt),
	}
}

// channelAuditDetails 是通道进审计时记的字段。
//
// settings 里没有任何口令（口令只有 secret_ref），所以可以整体记进来：host、port、
// tls、from、to、url、username 全是“这条告警会发到哪”的事实，少记一个事后就说不清。
func channelAuditDetails(ch notifications.Channel) map[string]any {
	return map[string]any{
		"channel_id":   ch.ID,
		"name":         ch.Name,
		"type":         ch.Type,
		"target":       ch.Target(),
		"settings":     ch.Settings,
		"secret_ref":   nullString(ch.SecretRef),
		"enabled":      ch.Enabled,
		"max_attempts": ch.MaxAttempts,
		"version":      ch.Version,
	}
}

// nullString 把空串变成 JSON null（Q418：可空字段回 null，不回空串）。
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseNotificationID 解析路径里的通道 id。
func (s *Server) parseNotificationID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "通道 id 必须是正整数")
		return 0, false
	}
	return id, true
}

// notificationChannelTarget / Raw 是审计的目标名。
//
// 目标里带 id 而不是只带名字：名字可以改，改完就再也对不上历史审计。
func notificationChannelTarget(id int64) string {
	return "notification_channel:" + strconv.FormatInt(id, 10)
}

func notificationChannelTargetRaw(raw string) string {
	if raw == "" {
		return "notification_channel"
	}
	return "notification_channel:" + raw
}

// failNotificationAction 与 failAlertAction 同构：审计写失败是一种，业务拒绝是另一种。
func (s *Server) failNotificationAction(c *gin.Context, sess *auth.Session, action, target string, err error) {
	if errors.Is(err, errAuditWrite) {
		s.opts.Logger.Error("审计写入失败，业务变更已回滚", "error", err, "action", action,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误")
		return
	}
	businessErr := err
	var denied *errDenied
	if errors.As(err, &denied) {
		businessErr = denied.err
	}
	status, code, msg, internal := mapNotificationError(businessErr)
	if internal {
		s.recordDenied(c, sess, action, target, "internal", code)
		s.internalError(c, "通知通道操作失败", businessErr)
		return
	}
	s.recordDenied(c, sess, action, target, "business", code)
	writeError(c, status, code, msg)
}

// mapNotificationError 把通知领域的错误翻成状态码与契约错误码。
//
// ErrSenderUnavailable 是 503 而不是 500：它不是“面板坏了”，是“代理没连上”，
// 而且这个状态会自己好起来——界面上该说的是“稍后重试”，不是“联系管理员”。
func mapNotificationError(err error) (status int, code httpx.Code, message string, internal bool) {
	var invalid *notifications.InvalidChannelError
	switch {
	case errors.Is(err, notifications.ErrSenderUnavailable):
		return http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法发送测试消息", false
	case errors.Is(err, notifications.ErrNotSendable):
		return http.StatusBadRequest, httpx.CodeValidationError,
			"in_app 通道不需要发送测试：它的投递就是面板收件箱里的一行", false
	case errors.Is(err, notifications.ErrNotFound):
		return http.StatusNotFound, httpx.CodeNotFound, "通知通道不存在", false
	case errors.Is(err, notifications.ErrVersionConflict):
		return http.StatusConflict, httpx.CodeVersionConflict, "通道已被其他人修改，请刷新后重试", false
	case errors.Is(err, notifications.ErrLimitReached):
		return http.StatusConflict, httpx.CodeDependencyBlocked,
			"通知通道数量已达上限（" + strconv.Itoa(notifications.MaxChannels) + "）；先删掉不再需要的通道", false
	case errors.As(err, &invalid):
		return http.StatusBadRequest, httpx.CodeValidationError, invalid.Error(), false
	default:
		// 领域包里还有两个“配置不合法”的错误（ErrNotSendable 之外的类型不匹配等）
		// 都以 InvalidChannelError 的形式给出；走到这里的只可能是真的内部错误。
		return http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误", true
	}
}
