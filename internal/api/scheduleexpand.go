package api

// 计划任务参数的**服务端展开**（P12.5）。
//
// 目前只有一件事要展开：证书续期计划的种类参数。理由与 proxy.apply、证书三个动作
// 完全一样——**能进系统的参数必须是面板算出来的**，不能是调用方说它是什么。
//
// 具体到定时续期，危险在于"指向哪一张证书"这件事有两个来源：定义里的 target_id
// （`crt_…`）与种类参数里的域名（/etc/letsencrypt/live/<domain> 才是真正被续的东西）。
// 两者一旦不一致，界面上一行写着"续 A 站的证书"，凌晨续掉的却是 B 站那张——
// 而 systemd 那边没有任何人能拦。代理侧判不了这件事：证书账本在面板库，代理手上
// 只有域名。所以由面板在预检之前按记录把域名与 ACME 环境**填**进去，调用方给的
// 那一份只用于核对。
//
// 填而不是信，还有一个副作用是好的：定义里现在存的是"这张证书自己的属性"，
// 触发那一刻（systemd，没有面板）代理照着它就能把 certificate.renew 提交出去。

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"upanel/internal/certificates"
	"upanel/internal/httpx"
	"upanel/internal/schedules"
)

// expandScheduleApply 把 schedule.apply 的定义展开成可提交的形状。
func (s *Server) expandScheduleApply(c *gin.Context, params json.RawMessage) (json.RawMessage, bool) {
	var p schedules.ApplyParams
	if !decodeExpandParams(c, params, &p) {
		return nil, false
	}
	if p.Definition.Kind != schedules.KindCertificateRenewal {
		// 备份与脚本计划不需要展开：它们的参数只有 target_id，版本由代理在保存与
		// 触发时自己取（计划定义里不存版本，见 schedules.TargetLookup 的注释）。
		return params, true
	}

	if s.opts.Certificates == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"证书服务不可用，无法展开证书续期计划的参数")
		return nil, false
	}
	id := p.Definition.TargetID
	if !certificates.ValidID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"证书续期计划的 target_id 必须是证书标识（crt_ 加 32 位小写十六进制）")
		return nil, false
	}
	rec, err := s.opts.Certificates.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, certificates.ErrNotFound) {
			writeError(c, http.StatusNotFound, httpx.CodeNotFound,
				"证书续期计划指向的证书不存在："+id)
			return nil, false
		}
		s.internalError(c, "预检展开失败", err)
		return nil, false
	}
	if rec.Source != certificates.SourceCertbot {
		writeError(c, http.StatusConflict, httpx.CodeValidationError,
			"这条记录是手动导入的证书：certbot 续不了它，给它建定时续期只会每晚失败一次。"+
				"手动证书到期请重新导入")
		return nil, false
	}

	// 调用方给了种类参数就核对：不一致不做"以服务端为准"的静默覆盖——那是把一个
	// 界面 bug 变成一条看起来正常的计划，而它续的是另一张证书。
	if got := p.Definition.Certificate; got != nil {
		if got.Domain != rec.Domain {
			writeError(c, http.StatusConflict, httpx.CodeValidationError,
				"计划里的域名（"+got.Domain+"）与证书记录 "+id+" 的域名（"+rec.Domain+"）不一致："+
					"续期按域名决定续哪张证书，这两个不能不一样")
			return nil, false
		}
		if got.Staging != rec.Staging {
			// staging 决定 certbot 的 --server。用正式入口续一张 staging 证书会把
			// 一个本不该上生产的对象换掉；反过来更糟——用测试入口续正式证书，
			// 盘上那张就变成浏览器不认的了。
			writeError(c, http.StatusConflict, httpx.CodeValidationError,
				"计划里的 ACME 环境与证书记录不一致（记录是 "+
					stagingWord(rec.Staging)+"）：续期必须用同一个 ACME 环境")
			return nil, false
		}
	}
	p.Definition.Certificate = &schedules.CertificateParams{
		Domain:  rec.Domain,
		Staging: rec.Staging,
	}
	return marshalExpanded(c, p)
}

// stagingWord 给用户看的环境说法。
func stagingWord(staging bool) string {
	if staging {
		return "测试环境"
	}
	return "正式环境"
}
