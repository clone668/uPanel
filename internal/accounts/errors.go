package accounts

// 让判决层与执行层自己给出 HTTP 判决。
//
// 与 internal/shares/http.go 是同一套形状（连 httpx.Code 都一样）：代理的查询/执行层
// 不按错误文本猜状态码，只问“你这次拒绝对应哪个状态码”。把“账号不存在”说成 500
// 会让页面把一条没了的记录显示成“面板出故障了”，然后所有人都去查错地方。

import (
	"fmt"
	"net/http"

	"upanel/internal/httpx"
	"upanel/internal/shares"
)

// dependencyBlocked 表示前置条件不满足（账号被共享引用、visudo 没有……）。
func dependencyBlocked(format string, args ...any) *shares.HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeDependencyBlocked, format, args...)
}

// validationError 表示参数/状态不允许这次操作。
func validationError(format string, args ...any) *shares.HTTPError {
	return newHTTPError(http.StatusBadRequest, httpx.CodeValidationError, format, args...)
}

// versionConflict 表示乐观并发令牌不符。
func versionConflict(format string, args ...any) *shares.HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeVersionConflict, format, args...)
}

// identityChanged 表示目标身份变了（账号名被人改过/记录指向了别的账号）。
func identityChanged(format string, args ...any) *shares.HTTPError {
	return newHTTPError(http.StatusConflict, httpx.CodeDeviceIdentityChanged, format, args...)
}

// commandFailed 表示命令没有按预期完成，或读回核验不通过。
func commandFailed(format string, args ...any) *shares.HTTPError {
	return newHTTPError(http.StatusInternalServerError, httpx.CodeCommandFailed, format, args...)
}

// notFound 表示登记表里没有这个对象。
func notFound(format string, args ...any) *shares.HTTPError {
	return newHTTPError(http.StatusNotFound, httpx.CodeNotFound, format, args...)
}

func newHTTPError(status int, code httpx.Code, format string, args ...any) *shares.HTTPError {
	return &shares.HTTPError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// guard 在执行前复核一次：判决仍然允许、目标身份还是那个、状态版本还是那个。
//
// 与 shares 的同名函数做同一件事，但阻塞码的归类是本包自己的——shares 那份的
// switch 里不认识 SUDO_* / SSH_KEY_* / ACCOUNTS_UNSUPPORTED，会一律当 400，
// 而“这台机器上没有 visudo”是 409/503 的语义（前置条件/依赖缺失），不是参数错。
func guard(a shares.Assessment, req shares.WriteRequest) error {
	if !a.Allowed {
		return blockerError(a)
	}
	if req.ExpectedIdentity != "" && a.IdentityFingerprint != req.ExpectedIdentity {
		return identityChanged("目标身份已变化（%s），请重新预检", a.DisplayName)
	}
	if a.StateVersion != req.ExpectedStateVersion {
		return versionConflict("目标状态在预检之后发生变化（%s），请重新预检", a.DisplayName)
	}
	return nil
}

// blockerError 把阻塞项映射成一个带判决的错误。
//
// 依赖类阻塞是 409 DEPENDENCY_BLOCKED：调用方要做的是“先满足前置条件”。
// 其余是 400 VALIDATION_ERROR：调用方要改参数。工具缺失是 503：这台机器现在做不了
// 这件事，改参数也没用。
func blockerError(a shares.Assessment) error {
	codes := make([]string, 0, len(a.Blockers))
	messages := make([]string, 0, len(a.Blockers))
	dependency, missing := false, false
	for _, b := range a.Blockers {
		codes = append(codes, b.Code)
		messages = append(messages, b.Message)
		switch b.Code {
		case NoticeAccountNotManaged, NoticeAccountMissing, NoticeAccountHasShares,
			NoticeSudoAccountNotEligible, NoticeSudoFileDamaged, NoticeAccountNameTaken:
			dependency = true
		case NoticeAccountsUnsupported:
			missing = true
		}
	}
	msg := fmt.Sprintf("预检不通过（%v）：%v", codes, messages)
	switch {
	case missing:
		return newHTTPError(http.StatusServiceUnavailable, httpx.CodeDependencyMissing, "%s", msg)
	case dependency:
		return dependencyBlocked("%s", msg)
	default:
		return validationError("%s", msg)
	}
}

// withRollbackNote 把“回滚本身干不干净”接在错误消息后面。
//
// 判决（状态码与错误码）来自主因，回滚结果只是一句现场补充，所以这里**不改类型**：
// 把回滚失败升格成另一个错误码，会让“账号建失败了”变成“账号回滚失败了”，
// 而人最需要先知道的是前者。与 shares 的同名函数同一个理由。
func withRollbackNote(err, rollback error) error {
	if err == nil || rollback == nil {
		return err
	}
	note := "；回滚没干净：" + rollback.Error()
	if he, ok := err.(*shares.HTTPError); ok {
		clone := *he
		clone.Message += note
		return &clone
	}
	return fmt.Errorf("%s%s", err.Error(), note)
}
