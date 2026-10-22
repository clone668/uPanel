package api

// 共享相关的只读路由与秘密写入通道（P08.1）。
//
// 三条只读路由对应代理登记的三条查询：GET /shares（share.list）、GET /shares/:id
// （同一份列表里挑一条）、GET /share-accounts（share.accounts）。按 CONTRACTS.md
// 第 7 节整组 admin：共享定义里有路径、账号名单与来源网段，那是“谁能访问哪个目录”
// 的完整答案，viewer 只在首页看数量与健康摘要。
//
// **这里没有“应用共享”的路由。** 应用一条共享（写目录、配 ACL、改配置）是系统写
// 操作，必须走统一的 POST /operations/preflight + POST /jobs（action=share.apply
// 等五个动作），不在这张表里开一个“直接执行”的入口。
//
// POST /secrets 是唯一的例外，也是唯一的写：它不改系统状态，只是把一段明文交给
// 代理加密保存，拿回一个引用。它同样要求 admin + CSRF + 5 分钟内再认证，并且
// **只进不出**——协议里没有读取明文的路由，这里也不提供。

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/audit"
	"upanel/internal/httpx"
	"upanel/internal/shares"
)

// SharesReader 是共享只读视图的读取端（P08.1）。生产实现是 *shares.Fetcher。
//
// 与 Storage 一样，接口定义在消费方，测试可以给假实现；生产里没有“固定成功”的
// 替身：没配置代理时 Shares 为 nil，接口回 503 AGENT_UNAVAILABLE，而不是给一份
// 空共享列表——那会被读成“这台机器上没有共享”。
//
// Shares / Share / Accounts 都把能力报告一起带回来：页面必须同时看到“这条共享的
// 定义是什么”与“它现在有没有被 Samba 提供”。definition.enabled 是意图，
// config_applied 才是事实。
type SharesReader interface {
	Shares(ctx context.Context) (shares.Capability, []shares.ShareRecord, error)
	Share(ctx context.Context, id string) (shares.Capability, shares.ShareRecord, bool, error)
	Accounts(ctx context.Context) (shares.Capability, []shares.AccountRecord, error)
	PutSecret(ctx context.Context, in agentclient.PutSecretInput) (agentclient.SecretRef, error)
}

// shareListResponse 是 GET /shares 的响应体。
type shareListResponse struct {
	Capability shares.Capability    `json:"capability"`
	Shares     []shares.ShareRecord `json:"shares"`
}

// shareDetailResponse 是 GET /shares/:id 的响应体。
//
// 包一层而不是直接返回记录：能力与记录在同一份响应里，页面不必发两次请求再自己
// 拼出一个“已启用”的结论。
type shareDetailResponse struct {
	Capability shares.Capability  `json:"capability"`
	Share      shares.ShareRecord `json:"share"`
}

// shareAccountListResponse 是 GET /share-accounts 的响应体。
type shareAccountListResponse struct {
	Capability shares.Capability      `json:"capability"`
	Accounts   []shares.AccountRecord `json:"accounts"`
}

// handleListShares 返回共享列表与能力报告（admin）。
func (s *Server) handleListShares(c *gin.Context) {
	reader := s.opts.Shares
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"共享列表不可用：未配置代理连接")
		return
	}
	capability, records, err := reader.Shares(c.Request.Context())
	if err != nil {
		s.writeSharesError(c, err, "读取共享列表失败")
		return
	}
	if records == nil {
		records = []shares.ShareRecord{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK,
		shareListResponse{Capability: capability, Shares: records})
}

// handleGetShare 返回单条共享（admin）。不存在是 404，不是空对象。
func (s *Server) handleGetShare(c *gin.Context) {
	reader := s.opts.Shares
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"共享详情不可用：未配置代理连接")
		return
	}
	id := c.Param("id")
	// 形状先在本层挡住：共享标识会进日志与审计，不能把任意字符串透传给代理。
	if !shares.ValidShareID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"共享标识必须是 shr_ 加 32 位小写十六进制")
		return
	}
	capability, record, found, err := reader.Share(c.Request.Context(), id)
	if err != nil {
		s.writeSharesError(c, err, "读取共享详情失败")
		return
	}
	if !found {
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "共享不存在："+id)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK,
		shareDetailResponse{Capability: capability, Share: record})
}

// handleListShareAccounts 返回受管理账号列表（admin）。
//
// 返回的每一条都没有密码、没有哈希，也没有 secret_ref（那个字段在 DTO 上就是
// json:"-"，代理序列化时也不会带出来）。
func (s *Server) handleListShareAccounts(c *gin.Context) {
	reader := s.opts.Shares
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"共享账号列表不可用：未配置代理连接")
		return
	}
	capability, records, err := reader.Accounts(c.Request.Context())
	if err != nil {
		s.writeSharesError(c, err, "读取共享账号列表失败")
		return
	}
	if records == nil {
		records = []shares.AccountRecord{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK,
		shareAccountListResponse{Capability: capability, Accounts: records})
}

// secretPurposePattern 是用途名的形状。
//
// **枚举不在这里。** 用途闭集只有一份，在代理侧（agentstore 的 purposes 表）：
// 不在表里的用途、以及 totp_seed 这类只能由代理内部流程写入的用途，都由代理以
// 400 拒绝。Web 层复制一份枚举只会得到两个会各自漂移的事实来源，而漂移的方向
// 通常是 Web 放行一个代理不接受的用途——那时用户看到的是莫名其妙的失败。
// 这里只挡形状，让明显不合法的输入连代理都不用到。
var secretPurposePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// createSecretRequest 是 POST /secrets 的请求体。
//
// 只有用途、明文与可选引用：没有类型标签，也没有任意键值对。多一个字段都会被
// decodeJSONBody 的 DisallowUnknownFields 拒绝。
type createSecretRequest struct {
	Purpose string `json:"purpose"`
	// Value 是明文。处理完立刻置空，不写日志、不回显。
	Value string `json:"value"`
	// Reference 是引用这份秘密的配置标识；为空时是 10 分钟后销毁的临时秘密。
	Reference string `json:"reference"`
}

// createSecretResponse 是 POST /secrets 的响应体：只有引用与生命周期，没有明文。
type createSecretResponse struct {
	SecretRef string `json:"secret_ref"`
	Purpose   string `json:"purpose"`
	State     string `json:"state"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// handleCreateSecret 把一段明文交给代理加密保存（admin + 再认证）。
//
// 这是唯一一条“写”但不进任务链路的接口：它不改变主机状态，只是把密码放进代理的
// 加密秘密表。真正的系统变更（设 Samba 密码、写配置）仍要预检 + 任务，参数里只有
// secret_ref。
func (s *Server) handleCreateSecret(c *gin.Context) {
	reader := s.opts.Shares
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"秘密通道不可用：未配置代理连接")
		return
	}
	sess := sessionFrom(c)

	var req createSecretRequest
	if !decodeJSONBody(c, &req) {
		return
	}
	// 明文用完即弃：这个 defer 只是让 GC 之前的那段窗口里少留一份。
	defer func() { req.Value = "" }()

	if !secretPurposePattern.MatchString(req.Purpose) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"用途名只能是小写字母、数字与下划线，且以字母开头")
		return
	}
	if req.Value == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "秘密内容不能为空")
		return
	}

	ref, err := reader.PutSecret(c.Request.Context(), agentclient.PutSecretInput{
		Purpose:   req.Purpose,
		Value:     req.Value,
		Reference: strings.TrimSpace(req.Reference),
	})
	if err != nil {
		s.writeSharesError(c, err, "写入秘密失败")
		// 审计：写入失败要留痕（谁在什么时候试图存一个什么用途的秘密），
		// 但**不记内容**。
		s.recordDenied(c, &sess, audit.ActionSecretCreate, "secret:"+req.Purpose,
			"agent", httpx.CodeValidationError)
		return
	}

	// 审计成功事件。secret_ref 这个键会被 audit 的秘密字段过滤替换成 [redacted]：
	// 引用本身不是密码，但它是“去解哪一份密文”的线索，按秘密处理更稳妥。
	if err := s.recordAuditInTx(c.Request.Context(), c, sess, audit.ActionSecretCreate,
		"secret:"+ref.Purpose, map[string]any{
			"purpose":    ref.Purpose,
			"secret_ref": ref.Ref,
			"state":      ref.State,
		}); err != nil {
		// 秘密已经存进代理了，撤销不了；审计没写成是部署问题，必须打 ERROR 并告诉
		// 调用方“这次没有留痕”。不能静默返回成功。
		s.opts.Logger.Error("审计写入失败（秘密已保存）", "error", err,
			"purpose", ref.Purpose, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError,
			"秘密已保存，但审计未写入：请联系管理员检查审计存储")
		return
	}

	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, createSecretResponse{
		SecretRef: ref.Ref,
		Purpose:   ref.Purpose,
		State:     ref.State,
		ExpiresAt: ref.ExpiresAt,
	})
}

// writeSharesError 把共享读取/秘密写入的错误映射成 HTTP 判决。
//
// 与 writeStorageError 同一套规则：代理明确拒绝的状态码与错误码原样转出，
// 代理不可用是 503，其余是 500。压成 500 会把“这条共享没了”显示成“面板坏了”。
func (s *Server) writeSharesError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, shares.ErrAgentUnavailable) {
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，暂时读不到共享信息")
		return
	}
	var rejected *shares.RemoteError
	if errors.As(err, &rejected) {
		status := rejected.Status
		if status < 400 || status >= 500 {
			status = http.StatusBadGateway
		}
		code := rejected.Code
		if code == "" {
			code = httpx.CodeCommandFailed
		}
		writeError(c, status, code, rejected.Message)
		return
	}
	s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	writeError(c, http.StatusInternalServerError, httpx.CodeCommandFailed, "共享信息读取失败")
}
