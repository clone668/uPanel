package api

// 受管理 Linux 账号的只读路由（P14.5）。
//
// 两条路由对应代理侧的两条读：
//   GET /accounts              → share.accounts（同一张 managed_accounts）
//   GET /accounts/:id/ssh-keys → accounts.ssh_keys
//
// 整组 admin（CONTRACTS.md 第 13 节）：账号列表里有登录名、uid、有没有口令、
// 是不是在哪个共享里；公钥列表里有“谁能拿这把钥匙登进来”。这两份数据合起来
// 就是这台机器上“谁有入口”的完整答案，viewer 看首页摘要就够。
//
// **这里没有写入口。** 建账号、改壳、改口令、加/撤公钥、发/撤 sudo 规则都是
// 系统写操作，一律走 POST /operations/preflight + POST /jobs
// （action=account.apply / account.delete / ssh_key.* / sudo_rule.*），
// 与本文件无关——六条动作的判决与执行都在内部代理侧（internal/accounts）。
//
// /share-accounts（P08.1）与 /accounts 读的是同一份记录：共享账号与普通账号的
// 区别是 type 那一列，不是两张表（DECISIONS Q475）。旧路由保留是因为共享页
// 已经在用它，语义上它现在只是“同一份列表的另一个名字”。

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"upanel/internal/accounts"
	"upanel/internal/httpx"
	"upanel/internal/shares"
)

// AccountsReader 是账号只读视图的读取端（P14.5）。生产实现是 *accounts.Reader。
//
// 允许为 nil（演示模式不连代理）：那时两条路由明确回 503 AGENT_UNAVAILABLE，
// 而不是给一份空账号列表——空列表会被读成“这台机器上一个账号都没有”。
type AccountsReader interface {
	Accounts(ctx context.Context) (accounts.Capability, []shares.AccountRecord, error)
	SSHKeys(ctx context.Context, accountID string) ([]accounts.SSHKeyRecord, error)
}

// accountListResponse 是 GET /accounts 的响应体。
//
// 账号列表的**记录**来自 share.accounts（同一份 managed_accounts），而这里带的
// 能力报告是账号模块自己的一份（accounts.capability）：它判的是 useradd / getent /
// chpasswd / 两档壳 / visudo 在不在。share.accounts 带的那一份判的是 Samba，
// 用它当账号页的判据会让一台没装 Samba 的机器连普通账号都建不了——虚拟机验收
// 抓到的就是这个。两份能力的形状不同，所以这里不能复用 shares.Capability。
type accountListResponse struct {
	Capability accounts.Capability    `json:"capability"`
	Accounts   []shares.AccountRecord `json:"accounts"`
}

// accountKeysResponse 是 GET /accounts/:id/ssh-keys 的响应体。
//
// 只带指纹与算法名，不带密钥体：Blob 在 SSHKeyRecord 上是 json:"-"，代理序列化
// 时就不出这个边界。页面要的是“哪把钥匙、什么时候加的”，不是那串 base64。
type accountKeysResponse struct {
	AccountID string                  `json:"account_id"`
	SSHKeys   []accounts.SSHKeyRecord `json:"ssh_keys"`
}

// handleListAccounts 返回受管理账号列表（admin）。
func (s *Server) handleListAccounts(c *gin.Context) {
	reader := s.opts.Accounts
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"账号列表不可用：未配置代理连接")
		return
	}
	capability, records, err := reader.Accounts(c.Request.Context())
	if err != nil {
		s.writeAccountsError(c, err, "读取账号列表失败")
		return
	}
	if records == nil {
		records = []shares.AccountRecord{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK,
		accountListResponse{Capability: capability, Accounts: records})
}

// handleListAccountSSHKeys 返回某个账号登记的公钥（admin）。
//
// 标识形状先在本层挡：account_id 会进日志与审计，不能把任意字符串透传给代理。
func (s *Server) handleListAccountSSHKeys(c *gin.Context) {
	reader := s.opts.Accounts
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"账号公钥不可用：未配置代理连接")
		return
	}
	id := c.Param("id")
	if !shares.ValidAccountID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"账号标识必须是 sac_ 加 32 位小写十六进制")
		return
	}
	keys, err := reader.SSHKeys(c.Request.Context(), id)
	if err != nil {
		s.writeAccountsError(c, err, "读取账号公钥失败")
		return
	}
	if keys == nil {
		// 没有公钥是“没配”，不是“不知道”。null 会被前端读成后者。
		keys = []accounts.SSHKeyRecord{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK,
		accountKeysResponse{AccountID: id, SSHKeys: keys})
}

// writeAccountsError 把账号读端的错误翻成响应。
//
// 与 writeSharesError 同一个分类（代理不可用 503、代理明确拒绝按它的状态码、
// 其余 500），但文案是自己的：一个人读账号页时看到的“暂时读不到共享信息”
// 只会让他以为点错了页面。错误类型本身是共用的（accounts 直接复用了 shares 的
// ErrAgentUnavailable / RemoteError），所以判定条件不变。
func (s *Server) writeAccountsError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, accounts.ErrAgentUnavailable) {
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，暂时读不到账号信息")
		return
	}
	var rejected *accounts.RemoteError
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
	writeError(c, http.StatusInternalServerError, httpx.CodeCommandFailed, "账号信息读取失败")
}
