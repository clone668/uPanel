// Package audit 写入追加式审计事件（CONTRACTS.md 第 14.1 节 audit_events）。
//
// P02.2 起记录账号管理动作，P02.4 起与业务变更共享同一事务，并记录被拒绝的高风险请求。
// 两条硬约束在这里落地：
//   - 只写不读改：本包没有 Update/Delete，数据库触发器也会拒绝 UPDATE/DELETE；
//   - 秘密不入库：Details 按键名过滤，口令、令牌、哈希类字段一律替换为 "[redacted]"。
package audit

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

// 动作名。命名与接口路径对应，P02.4 扩展时沿用同一命名法（<域>.<对象>.<动作>）。
const (
	// ActionUserCreate 是管理员创建账号。
	ActionUserCreate = "user.create"
	// ActionUserUpdate 是修改显示名、角色或启用状态。
	ActionUserUpdate = "user.update"
	// ActionUserPasswordReset 是重置他人密码（会同时作废目标会话）。
	ActionUserPasswordReset = "user.password.reset"
	// ActionUserDelete 是删除账号。
	ActionUserDelete = "user.delete"
	// ActionUserManage 用于账号管理请求被中间件拒绝（admin/CSRF/再认证）时记录的事件：
	// 请求没有走到具体动作就结束了，用路由组的动作名比不记更诚实。
	ActionUserManage = "user.manage"
	// ActionAuthReauth 是密码再认证（成功与失败都记）。
	ActionAuthReauth = "auth.reauth"
	// ActionTotpSetup 是发起一次二次验证登记（生成种子，尚未启用）（P14.6）。
	//
	// 记它是因为"谁在什么时候试过往自己的账号上挂一个 authenticator"是安全上要紧的事：
	// 一次没能确认的登记同样说明有人的口令已经被别人拿到了。
	ActionTotpSetup = "auth.totp.setup"
	// ActionTotpEnable 是确认成功、二次验证真正启用（一次性恢复码在这一刻签发）。
	ActionTotpEnable = "auth.totp.enable"
	// ActionTotpDisable 是关闭二次验证（口令 + 验证码或恢复码）。
	ActionTotpDisable = "auth.totp.disable"
	// ActionTotpManage 用于这一组请求被中间件拒绝（admin/CSRF/再认证）时记录的事件。
	ActionTotpManage = "auth.totp.manage"
	// ActionTotpReset 是本机 CLI 清掉一个账号的二次验证（`upanelctl totp-reset`）。
	//
	// 与 ActionTotpDisable 分开记：那条是账号主人自己证明了两样东西之后关闭的，
	// 这条是"服务器上有人拿着数据库的钥匙把一个账号的第二因子摘掉了"。后者不区分是谁，
	// 所以 actor 是一个本地快照字符串，而不是某个账号。
	ActionTotpReset = "auth.totp.reset"
	// ActionOperationPreflight 是生成一份预检计划（P03.1）。
	//
	// 预检不改变系统状态，但它是“准备执行一个危险动作”的明确意图：谁、为哪个动作、
	// 代理是否允许、要满足哪些条件，都值得留痕。
	ActionOperationPreflight = "operation.preflight"
	// ActionJobSubmit 是提交一次系统写任务（P03.2）。
	//
	// 这是真正产生副作用的入口：记下 job_id、动作名、预检引用与提交后的状态，
	// 事后才能回答“这次格式化是谁在什么时候提交的”。
	ActionJobSubmit = "job.submit"
	// ActionJobCancel 是请求取消任务。
	ActionJobCancel = "job.cancel"
	// ActionJobManage 用于任务请求被中间件拒绝（管理员角色/CSRF）时记录的事件。
	ActionJobManage = "job.manage"
	// ActionTerminalAttach 是建立或尝试建立终端连接（P06.3）。
	//
	// 成功与被拒绝都记：后者（origin、protocol、grant、agent 四个阶段）是
	// “有人试图连一个不属于他的会话”的唯一痕迹。详情只记会话元数据与
	// 字节数量，不含终端内容。
	ActionTerminalAttach = "terminal.attach"
	// ActionTerminalClose 是主动关闭自己终端会话（CONTRACTS.md 第 10 节要求审计）。
	ActionTerminalClose = "terminal.close"
	// ActionSecretCreate 是一次秘密上传（P08.1，POST /api/v1/secrets）。
	//
	// 这条事件记的是“谁在什么时候把一份什么用途的密文交进了代理”，不记内容：
	// 明文从不进日志与审计，secret_ref 这个键也会被下面的秘密字段过滤替换。
	// 它值得留痕，因为这是面板上唯一一条“把密码交给系统”的入口。
	ActionSecretCreate = "secret.create"
	// ActionAppCreate 是登记一个应用入口/绑定（P09.1）。
	//
	// 只写元数据，不动主机；但“谁把这个容器登记成了应用”是需要能回答的问题，
	// 尤其是绑定了 docker/compose 的应用——它们是后续启停动作的前提。
	ActionAppCreate = "app.create"
	// ActionAppUpdate 是修改应用元数据（含解绑后重新绑定）。
	ActionAppUpdate = "app.update"
	// ActionAppDelete 是删除应用入口/绑定（不卸载项目本身）。
	ActionAppDelete = "app.delete"
	// ActionAppManage 用于应用请求被中间件拒绝（admin/CSRF）时记录的事件。
	ActionAppManage = "app.manage"
	// ActionAppGroupCreate / Update / Delete 是分组管理（P09.1）。
	ActionAppGroupCreate = "app_group.create"
	ActionAppGroupUpdate = "app_group.update"
	ActionAppGroupDelete = "app_group.delete"
	// ActionAppGroupManage 用于分组请求被中间件拒绝时记录的事件。
	ActionAppGroupManage = "app_group.manage"
	// ActionProxyCreate / Update / Delete 是代理站点记录的管理（P10.1）。
	//
	// 记这条事件的意义在于“谁把这个域名指向了哪个上游”。P10.1 只写记录、不写
	// Nginx 配置；配置真正落盘时（P10.2）会另有一条任务事件，两者不要混为一谈。
	ActionProxyCreate = "proxy.create"
	ActionProxyUpdate = "proxy.update"
	ActionProxyDelete = "proxy.delete"
	// ActionProxyManage 用于代理请求被中间件拒绝（admin/CSRF）时记录的事件。
	ActionProxyManage = "proxy.manage"
	// ActionCertificateUpload 是一次公钥证书上传（P10.3，POST /certificates/uploads）。
	//
	// 记的是"谁交进来一份 PEM、覆盖哪些域名、指纹是什么"，不记内容（公钥证书本身
	// 不是秘密，但整份 PEM 几百行，进审计只会让日志变得没人读）。这条事件重要的
	// 理由是：上传只是暂存，真正的动作是随后那次 certificate.import 任务——两者
	// 分开记，才能分清"传了没导"和"导了没成"。
	ActionCertificateUpload = "certificate.upload"
	// ActionCertificateManage 用于证书请求被中间件拒绝（admin/CSRF/再认证）时记录的事件。
	ActionCertificateManage = "certificate.manage"
	// ActionBackupRepositoryCreate / Update 是备份仓库定义的登记与修改（P12.2）。
	//
	// 这里记的不是"备份跑了"，而是"备份会往哪儿写"：backend、host、remote_path、
	// 以及有没有配 SFTP 凭据。定义改了而没人知道，一条备份就可能悄悄写到别的机器上。
	// 两份秘密引用不记内容——审计里只写"配没配"。
	ActionBackupRepositoryCreate = "backup.repository.create"
	ActionBackupRepositoryUpdate = "backup.repository.update"
	// ActionBackupRepositoryManage 用于仓库请求被中间件拒绝（admin/CSRF/再认证）时记录。
	ActionBackupRepositoryManage = "backup.repository.manage"
	// ActionBackupPolicyCreate / Update / Delete 是备份策略的管理（P12.2）。
	//
	// 策略决定"备份什么"与"备份前停哪些应用"——第二条是它值得留痕的原因：一次策略
	// 改动可以让下一次夜间备份把生产应用停掉一段时间。
	ActionBackupPolicyCreate = "backup.policy.create"
	ActionBackupPolicyUpdate = "backup.policy.update"
	ActionBackupPolicyDelete = "backup.policy.delete"
	// ActionBackupPolicyManage 用于策略请求被中间件拒绝时记录的事件。
	ActionBackupPolicyManage = "backup.policy.manage"
	// ActionScheduleManage / ActionScriptManage 是计划任务与受管理脚本的读取（P12.4）。
	//
	// 这两个接口只读，为什么还要一个动作名：被中间件拒绝的请求（viewer 越权、会话
	// 失效）发生在 handler 之前，只有路由组能带上正确的名字。计划里写着这台机器什么
	// 时间会做什么、脚本正文是什么——一次"想看却看不到"的尝试值得留痕。
	//
	// 写路径不在这里：计划的改动走 schedule.apply / schedule.delete 的预检与任务，
	// 审计随任务走（job.submit / operation.*），不在这两个只读动作名下。
	ActionScheduleManage = "schedule.manage"
	ActionScriptManage   = "script.manage"
	// ActionAlertRuleCreate / Update / Delete 是告警规则的管理（P13.2）。
	//
	// 规则本身不改变主机状态，但它决定"机器出事时会不会有人知道"。改阈值、改持续
	// 时间、把一条规则关掉，都可能让一次真实的磁盘写满没人报警——这类改动必须能
	// 回答"谁在什么时候把这条线挪了"。变更里记完整的旧值与新值（阈值、持续、
	// 恢复线、冷却、启停），只记"改了"等于没记。
	ActionAlertRuleCreate = "alert.rule.create"
	ActionAlertRuleUpdate = "alert.rule.update"
	ActionAlertRuleDelete = "alert.rule.delete"
	// ActionAlertRuleManage 用于规则请求被中间件拒绝（admin/CSRF/再认证）时记录的事件。
	ActionAlertRuleManage = "alert.rule.manage"
	// ActionAlertSilenceUpdate 是设置或清除维护静默窗口（P13.2）。
	//
	// 它值得单独留痕：静默期间告警仍然判定、事件仍然记录，但通知不发（P13.3）。
	// 一次不小心把窗口设成一周，等于一周内没人被打扰。
	ActionAlertSilenceUpdate = "alert.silence.update"
	// ActionAlertSilenceManage 用于静默请求被中间件拒绝时记录的事件。
	ActionAlertSilenceManage = "alert.silence.manage"
	// ActionAlertEventAck 是"确认某条告警事件我已经看见了"（P13.4）。
	//
	// 它不改主机、不改判定，但它把"有没有人在管这件事"从推断变成事实——一条
	// firing 的事件没人确认，与有人确认过但还没解决，是两种完全不同的处境。
	// 所以记是谁、什么时候、哪条事件（以及那条事件当时的规则与状态：确认之后
	// 事件还可能恢复，事后要能还原"他确认时它是什么样"）。
	ActionAlertEventAck = "alert.event.ack"

	// ActionNotificationChannelCreate / Update / Delete 是通知通道的管理（P13.3）。
	//
	// 通道不改变主机状态，但它决定“机器出事时有没有人真的收到消息”。把 webhook
	// 地址改到别处，等于把告警发给了别人；把通道停用，等于把告警调成静音。
	// 所以地址、端口、收件人、TLS 模式、secret_ref、启停与重试上限都记进变更。
	//
	// **口令与令牌本身永远不进审计**：只记 secret_ref（一个指向代理加密库的引用）。
	// 审计表是明文可读的，把口令写进去等于把加密库的意义作废。
	ActionNotificationChannelCreate = "notification.channel.create"
	ActionNotificationChannelUpdate = "notification.channel.update"
	ActionNotificationChannelDelete = "notification.channel.delete"
	// ActionNotificationChannelTest 是显式测试发送（P13.3）。它真的会往外发一封
	// 邮件或一个请求，值得单独留痕：出了问题第一个要回答的问题是“谁点的发送”。
	ActionNotificationChannelTest = "notification.channel.test"
	// ActionNotificationChannelManage 用于通道请求被中间件拒绝（非 admin/CSRF）时记录的事件。
	ActionNotificationChannelManage = "notification.channel.manage"

	// ActionNetworkManage 是网络现状的读取（P14.1）。
	//
	// 读的是网卡、地址、路由与监听端口——合起来就是"这台机器上开了什么入口、
	// 通向哪里"，侦察价值与容器清单同级，所以整组 admin。读取本身不改主机状态，
	// 但"谁在什么时候看了一遍这台机器的网络全貌"值得留痕：它是把一个账号从
	// 猜测变成事实的第一步。
	//
	// 写路径不在这里：改地址是 network.apply 的预检 → 任务（P14.2），审计随任务走。
	ActionNetworkManage = "network.manage"
	// ActionFirewallManage 是防火墙现状的读取（P14.1）。
	//
	// 规则表回答的是"哪些口子对外开着"，与端口监听是同一份侦察材料的两半，
	// 因此同样是 admin 且同样留痕。改规则是 firewall.apply（P14.3）。
	ActionFirewallManage = "firewall.manage"
)

// 结果枚举，与迁移里的 CHECK 约束一致。
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// redactedValue 是秘密字段的替代文本。
const redactedValue = "[redacted]"

// secretKeyFragments 命中即视为秘密字段。宁可按名字多拦，也不把秘密写进审计。
var secretKeyFragments = []string{
	"password", "passwd", "secret", "token", "hash", "cookie", "credential", "private_key",
}

// ErrInvalidEvent 表示事件本身不合法（编程错误，不是用户输入错误）。
var ErrInvalidEvent = errors.New("audit: 事件不合法")

// Event 是一条审计事件。
type Event struct {
	// ActorID 是操作者账号 id；0 表示没有账号主体（例如系统任务），写为 NULL。
	ActorID int64
	// ActorSnapshot 是操作者的文本快照（用户名与角色）：账号被删除后仍然可读。
	ActorSnapshot string
	// Action 是动作名，见上面的常量。
	Action string
	// Target 是作用对象，例如 "user:3"。
	Target string
	// JobID 是这次事件关联的任务 id（jobs.id）；不关联任务时为空，写库为 NULL。
	//
	// 单独一列而不是只放进 Details：迁移里就是这么建的（audit_events.job_id），
	// 事后要回答“这次失败的任务是谁提交的”，靠的是一列可索引的键，不是字符串匹配。
	JobID string
	// RequestID 是本次 HTTP 请求的 request_id，便于与日志对齐。
	RequestID string
	// Outcome 是 success 或 failure。
	Outcome string
	// Details 只放非秘密字段；写入时会再按字段名过滤一遍。
	Details map[string]any
}

// Writer 写入审计事件。
type Writer struct {
	db  *store.DB
	now func() time.Time
}

// New 构造 Writer。db 为空时返回错误：没有数据库就不能“假装记了审计”。
func New(db *store.DB, now func() time.Time) (*Writer, error) {
	if db == nil {
		return nil, errors.New("audit: 数据库句柄不能为空")
	}
	if now == nil {
		now = time.Now
	}
	return &Writer{db: db, now: now}, nil
}

// execer 是 *sql.DB 与 *sql.Tx 的公共写入子集。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Record 追加一条审计事件。
//
// **事务归属**：如果 ctx 由 store.WithTx 产生（业务事务），事件写在同一个事务里——
// 业务回滚则审计一起消失，业务提交则审计必然存在。没有事务时（例如记录被拒绝的请求）
// 直接用数据库连接写，独立提交。
func (w *Writer) Record(ctx context.Context, ev Event) error {
	if strings.TrimSpace(ev.Action) == "" {
		return fmt.Errorf("%w: 缺少 action", ErrInvalidEvent)
	}
	if ev.Outcome != OutcomeSuccess && ev.Outcome != OutcomeFailure {
		return fmt.Errorf("%w: 未知 outcome %q", ErrInvalidEvent, ev.Outcome)
	}
	actor := strings.TrimSpace(ev.ActorSnapshot)
	if actor == "" {
		actor = "system"
	}

	details, err := marshalDetails(ev.Details)
	if err != nil {
		return err
	}

	var actorID any
	if ev.ActorID > 0 {
		actorID = ev.ActorID
	}
	var jobID any
	if id := strings.TrimSpace(ev.JobID); id != "" {
		jobID = id
	}
	// 有业务事务就跟着走，否则独立写（被拒绝的请求没有业务事务可依附）。
	var target execer = w.db.SQL()
	if tx, ok := store.TxFromContext(ctx); ok {
		target = tx
	}

	_, err = target.ExecContext(ctx, `
		INSERT INTO audit_events (actor_id, actor_snapshot, action, target, request_id, job_id, outcome, redacted_details, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		actorID, actor, ev.Action, ev.Target, ev.RequestID, jobID, ev.Outcome, details,
		w.now().UTC().Truncate(time.Second).Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("audit: 写入审计事件失败: %w", err)
	}
	return nil
}

// JobIDFromTarget 从 "job:<id>" 这样的 target 里取出任务 id；不是任务对象时返回空串。
//
// 任务相关的事件都用这个 target 形态（job.submit、job.cancel、以及被拒绝的任务请求），
// 规则只有这一处，不必让每个调用点各传一遍。
func JobIDFromTarget(target string) string {
	const prefix = "job:"
	if !strings.HasPrefix(target, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(target, prefix))
}

// marshalDetails 序列化明细，并把秘密字段替换掉。
func marshalDetails(details map[string]any) (string, error) {
	if len(details) == 0 {
		return "{}", nil
	}
	safe := make(map[string]any, len(details))
	for k, v := range details {
		if isSecretKey(k) {
			safe[k] = redactedValue
			continue
		}
		safe[k] = v
	}
	buf, err := json.Marshal(safe)
	if err != nil {
		return "", fmt.Errorf("audit: 审计明细无法序列化: %w", err)
	}
	return string(buf), nil
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, frag := range secretKeyFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}
