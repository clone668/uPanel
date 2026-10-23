package proxies

// 判决与执行的具体实现（P10.2）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"upanel/internal/nginx"
	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// AssessApply 计算一次 proxy.apply 的判决（预检调用，不写任何东西）。
func (a *Applier) AssessApply(ctx context.Context, in ApplyParams) (Assessment, error) {
	p, err := in.Normalize()
	if err != nil {
		return Assessment{}, paramsRejected(err)
	}
	def := *p.Definition
	paths := a.paths()

	assess := Assessment{
		Action:           ActionProxyApply,
		NormalizedParams: p,
		TargetID:         p.SiteID,
		DisplayName:      def.Domain,
		// 身份指纹是站点标识：它在预检与执行之间不会变（参数是固化下来的），
		// 因此这个比对不会产生“重试一次就失败”的假阳性。
		IdentityFingerprint: p.SiteID,
		StateVersion:        StateVersionOf(p),
		Locks:               []string{LockNginxConfig, "proxy:" + p.SiteID},
	}

	fileName, ferr := nginx.ManagedFileName(def.Domain)
	if ferr != nil {
		assess.Blockers = append(assess.Blockers, Blocker{
			Code: CodeDomainUnknown, Message: ferr.Error(),
		})
		return assess, nil
	}
	confPath := path.Join(paths.ConfDir(), fileName)

	facts, err := a.opts.Facts.Collect(ctx)
	if err != nil {
		assess.Blockers = append(assess.Blockers, Blocker{
			Code:    CodeNginxUnavailable,
			Message: "读不到这台机器上的 Nginx 现状，无法判断域名冲突：" + err.Error(),
		})
		return assess, nil
	}
	if !facts.Available {
		code := CodeNginxUnavailable
		if facts.ConfigMissing {
			code = CodeNginxNotInstalled
		}
		assess.Blockers = append(assess.Blockers, Blocker{
			Code:    code,
			Message: "这台机器上的 Nginx 不可用：" + facts.Reason,
		})
		return assess, nil
	}

	// 域名冲突：忽略**自己那份**托管文件（它是上一次写下去的我们自己的成果）。
	// 别人的文件、别的面板文件、正则、坏文件，一律按 P10.1 的规则处理。
	conflict := JudgeDomainIgnoring(facts, def.Domain, confPath)
	switch conflict.State {
	case ConflictTaken:
		assess.Blockers = append(assess.Blockers, Blocker{
			Code:    CodeDomainTaken,
			Message: "域名 " + def.Domain + " 已被现有配置占用：" + describeHits(conflict.Matches),
		})
	case ConflictUnknown:
		assess.Blockers = append(assess.Blockers, Blocker{
			Code:    CodeDomainUnknown,
			Message: "无法判断域名 " + def.Domain + " 是否可用：" + conflict.Reason,
		})
	}

	// 账号密码：只在 basic_auth 时检查，且只检查“解不解得开”，不留明文。
	if def.AccessMode == AccessBasicAuth {
		if a.opts.Secrets == nil {
			assess.Blockers = append(assess.Blockers, Blocker{
				Code:    CodeSecretInvalid,
				Message: "这台代理没有接上秘密存储，无法生成 basic auth 账号文件",
			})
		} else {
			for _, u := range def.BasicAuthUsers {
				if _, err := a.opts.Secrets.Resolve(ctx, u.SecretRef); err != nil {
					assess.Blockers = append(assess.Blockers, Blocker{
						Code:    CodeSecretInvalid,
						Message: fmt.Sprintf("账号 %s 的密码引用解不开：%v", u.Name, err),
					})
				}
			}
		}
	}

	// 证书：绑定了才查，且只在站点启用时查——停用一个站点不该被一张过期证书挡住。
	// 这里查的是磁盘上的事实（文件在不在、覆盖不覆盖这个域名、有没有过期），
	// 因为这三条中的后两条能通过 nginx -t 却一定打不开站点。
	if def.Enabled && p.Certificate != nil {
		inspection := a.inspectTLS(a.fs(), *p.Certificate, def.Domain, a.now())
		assess.Blockers = append(assess.Blockers, inspection.Blockers...)
		assess.Warnings = append(assess.Warnings, inspection.Warnings...)
	}

	// 告警：服务没在跑、上游目标没被发现、托管文件与记录不一致。
	if facts.UnitState != "" && facts.UnitState != "active" && facts.UnitState != "reloading" {
		assess.Warnings = append(assess.Warnings, Blocker{
			Code: CodeUnitInactive,
			Message: "nginx.service 的状态是 " + facts.UnitState +
				"：配置会写下去并校验，但这次不会 reload，站点此刻不提供服务",
		})
	}
	if a.opts.TargetExists != nil {
		ok, err := a.opts.TargetExists(ctx, def.UpstreamTargetID)
		switch {
		case err != nil:
			assess.Warnings = append(assess.Warnings, Blocker{
				Code:    CodeTargetMissing,
				Message: "无法确认上游目标当前是否在运行：" + err.Error(),
			})
		case !ok:
			assess.Warnings = append(assess.Warnings, Blocker{
				Code: CodeTargetMissing,
				Message: "上游目标 " + def.UpstreamTargetID +
					" 不在代理当前的发现结果里：配置仍会生效，但访问会得到 502，直到那个应用起来",
			})
		}
	}

	// 影响：说清楚这次会动哪些文件、会不会 reload。
	assess.Impacts = append(assess.Impacts, Impact{
		Kind:        "nginx_config",
		Description: managedActionText(def.Enabled, confPath),
	})
	if def.Enabled && def.AccessMode == AccessBasicAuth {
		assess.Impacts = append(assess.Impacts, Impact{
			Kind: "nginx_auth_file",
			Description: "写入 basic auth 账号文件 " +
				path.Join(paths.AuthDir(), AuthFileNameOf(p.SiteID)) + "（明文密码不进配置文件）",
		})
	}
	assess.Impacts = append(assess.Impacts, Impact{
		Kind:        "nginx_reload",
		Description: "用 nginx -t 校验整棵配置树，通过后执行 nginx -s reload（不重启服务）",
	})

	assess.Allowed = len(assess.Blockers) == 0
	return assess, nil
}

// now 是注入的时钟（单元测试要能固定“证书是否过期”这一刻）。
func (a *Applier) now() time.Time {
	if a.opts.Now != nil {
		return a.opts.Now()
	}
	return time.Now()
}

// managedActionText 描述这次对托管文件做什么。
func managedActionText(enabled bool, confPath string) string {
	if !enabled {
		return "站点的 enabled=false：移除托管配置 " + confPath + "（如果它存在），域名将不再被代理"
	}
	return "写入或替换托管配置 " + confPath + "（只动这一个文件，不碰发行版默认站点与用户文件）"
}

// ExecuteApply 执行一次 proxy.apply。
func (a *Applier) ExecuteApply(ctx context.Context, req WriteRequest) (WriteReport, error) {
	params, ok := req.Params.(ApplyParams)
	if !ok {
		return WriteReport{}, commandFailed("proxy.apply 的参数类型是 %T", req.Params)
	}
	p, err := params.Normalize()
	if err != nil {
		return WriteReport{}, paramsRejected(err)
	}
	def := *p.Definition

	// 执行前复核一次：域名可能在这段时间里被别人占了、Nginx 可能被卸了。
	assess, err := a.AssessApply(ctx, p)
	if err != nil {
		return WriteReport{}, err
	}
	if !assess.Allowed {
		return WriteReport{}, blockerError(assess)
	}
	if req.ExpectedIdentity != "" && assess.IdentityFingerprint != req.ExpectedIdentity {
		return WriteReport{}, versionConflict("站点标识在预检之后变了，请重新预检")
	}
	if req.ExpectedStateVersion != "" && assess.StateVersion != req.ExpectedStateVersion {
		return WriteReport{}, versionConflict("目标状态在预检之后变了，请重新预检")
	}

	paths := a.paths()
	fs := a.fs()
	reporter := func(step string, fields map[string]any) {
		if req.Progress != nil {
			req.Progress(step, fields)
		}
	}

	fileName, ferr := nginx.ManagedFileName(def.Domain)
	if ferr != nil {
		return WriteReport{}, validationError("%s", ferr.Error())
	}
	confPath := path.Join(paths.ConfDir(), fileName)
	authPath := path.Join(paths.AuthDir(), AuthFileNameOf(p.SiteID))

	// 渲染。enabled=false 时渲染结果就是“这个文件不该存在”。
	var desired string
	var desiredHash string
	if def.Enabled {
		rendered, err := RenderSite(p.SiteID, def, RenderOptions{
			HtpasswdPath: authPath,
			TLS:          p.Certificate,
		})
		if err != nil {
			return WriteReport{}, validationError("渲染配置失败：%v", err)
		}
		desired = rendered
		desiredHash = hashOf(desired)
	}

	observed := map[string]any{
		"config_file": confPath,
		"enabled":     def.Enabled,
	}
	if p.Certificate != nil {
		observed["certificate_id"] = p.Certificate.CertificateID
		observed["listen_ports"] = []int{ManagedListenPort, ManagedTLSPort}
	} else {
		observed["listen_ports"] = []int{ManagedListenPort}
	}
	var notes []Blocker

	// ---- 0. 证书现场（绑定证书时）----
	// 放在写任何文件之前：证书不在、不覆盖这个域名、已经过期，都是“配置能过
	// nginx -t 但一定打不开站点”的故障，没有理由先写文件再回滚。
	// 复核（AssessApply）刚查过一遍，这里查的是“那一刻之后有没有变”——两次之间
	// 有人换掉证书文件是可能的，而这份报告要说清生效的是哪一张。
	if def.Enabled && p.Certificate != nil {
		inspection := a.inspectTLS(fs, *p.Certificate, def.Domain, a.now())
		for k, v := range inspection.Observed {
			observed[k] = v
		}
		notes = append(notes, inspection.Warnings...)
		if len(inspection.Blockers) > 0 {
			return WriteReport{}, blockerError(Assessment{Blockers: inspection.Blockers})
		}
	}

	// ---- 1. 目录 ----
	if err := ensureDir(fs, paths.ConfDir()); err != nil {
		return WriteReport{}, dependencyBlocked("创建 %s 失败：%v（%s）",
			paths.ConfDir(), err, CodeNotWritable)
	}

	// ---- 2. 账号文件（basic_auth）----
	// 顺序在配置之前：配置一旦进了 include 范围并 reload，请求就会来找这个文件。
	// 反过来做，中间有一个窗口期是所有请求都 500。
	rollback := newRollback(fs)
	authMode := ""
	if def.Enabled && def.AccessMode == AccessBasicAuth {
		oldAuth, readErr := readMaybe(fs, authPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			return WriteReport{}, dependencyBlocked("读不到现有的账号文件 %s：%v", authPath, readErr)
		}
		authExisted := readErr == nil
		plain, err := a.resolvePasswords(ctx, def)
		if err != nil {
			return WriteReport{}, err
		}
		content, err := RenderAuthFile(p.SiteID, def.BasicAuthUsers, plain)
		zeroPasswords(plain)
		if err != nil {
			return WriteReport{}, validationError("生成账号文件失败：%v", err)
		}
		if err := ensureDir(fs, paths.AuthDir()); err != nil {
			return WriteReport{}, dependencyBlocked("创建 %s 失败：%v（%s）",
				paths.AuthDir(), err, CodeNotWritable)
		}
		mode, uid, gid, modeNote := a.authFileOwner(fs, paths)
		if modeNote != "" {
			notes = append(notes, Blocker{Code: CodeAuthFileWorldReadable, Message: modeNote})
		}
		if string(oldAuth) != content {
			if err := fs.WriteFileSync(authPath, []byte(content), mode); err != nil {
				return WriteReport{}, dependencyBlocked("写账号文件 %s 失败：%v", authPath, err)
			}
			if uid >= 0 && fs.Chown != nil {
				// 需要 nginx 的 worker 用户读得到；失败不算致命（权限位已经放宽到 group 可读），
				// 但要报告出来——否则请求回来 403 时没人知道为什么。
				if err := fs.Chown(authPath, uid, gid); err != nil {
					notes = append(notes, Blocker{
						Code:    CodeAuthFileWorldReadable,
						Message: "账号文件的属主没有改成 nginx 的 worker 用户：" + err.Error(),
					})
				}
			}
			rollback.record(authPath, oldAuth, authExisted)
			authMode = "written"
		} else {
			authMode = "unchanged"
		}
		observed["auth_file"] = authPath
		observed["auth_file_state"] = authMode
		observed["basic_auth_users"] = len(def.BasicAuthUsers)
		reporter("proxy_auth_file", map[string]any{"path": authPath, "state": authMode})
	} else {
		// 不是 basic_auth（或已停用）：账号文件不该留着。
		if old, err := readMaybe(fs, authPath); err == nil {
			rollback.record(authPath, old, true)
			if err := fs.Remove(authPath); err != nil && !os.IsNotExist(err) {
				return WriteReport{}, commandFailed("删除遗留的账号文件 %s 失败：%v", authPath, err)
			}
			authMode = "removed"
		}
		if authMode != "" {
			observed["auth_file"] = authPath
			observed["auth_file_state"] = authMode
		}
	}

	// ---- 3. 清理同一站点改名后留下的旧文件 ----
	stale, err := a.staleManagedFiles(fs, paths, p.SiteID, confPath)
	if err != nil {
		return WriteReport{}, err
	}
	for _, stalePath := range stale {
		old, err := readMaybe(fs, stalePath)
		if err != nil {
			return WriteReport{}, commandFailed("读不到旧托管文件 %s：%v", stalePath, err)
		}
		// 先备份再删：改名之后这份文件就没有第二份了，而它曾经是这个域名的全部配置。
		backupPath, err := a.backup(paths, stalePath, old)
		if err != nil {
			return WriteReport{}, err
		}
		rollback.record(stalePath, old, true)
		if err := fs.Remove(stalePath); err != nil && !os.IsNotExist(err) {
			return WriteReport{}, commandFailed("删除旧托管文件 %s 失败：%v", stalePath, err)
		}
		notes = append(notes, Blocker{
			Code: CodeStaleFileRemoved,
			Message: "已移除这个站点改名前的旧配置 " + stalePath +
				"（不再被 include，备份在 " + backupPath + "）",
		})
	}

	// ---- 4. 托管配置：候选 → 备份 → rename → fsync ----
	fileState := FileStateUnchanged
	current, currentErr := readMaybe(fs, confPath)
	if currentErr != nil && !os.IsNotExist(currentErr) {
		return WriteReport{}, dependencyBlocked("读不到托管配置 %s：%v", confPath, currentErr)
	}
	// 三种情形：内容变了要写；停用时文件还在（等于当初写下去过）要删；其余不动。
	currentExists := currentErr == nil
	changed := (desired != "" && string(current) != desired) || (desired == "" && currentExists)

	if changed {
		if currentExists {
			backup, err := a.backup(paths, confPath, current)
			if err != nil {
				return WriteReport{}, err
			}
			observed["backup"] = backup
			rollback.record(confPath, current, true)
		} else {
			rollback.record(confPath, nil, false)
		}
		if desired == "" {
			if err := fs.Remove(confPath); err != nil && !os.IsNotExist(err) {
				return WriteReport{}, commandFailed("移除托管配置 %s 失败：%v", confPath, err)
			}
			fileState = FileStateRemoved
		} else {
			candidate := confPath + ".upanel-new"
			_ = fs.Remove(candidate)
			if err := fs.WriteFileSync(candidate, []byte(desired), managedFileMode); err != nil {
				return WriteReport{}, dependencyBlocked("写候选配置 %s 失败：%v（%s）",
					candidate, err, CodeNotWritable)
			}
			if err := fs.Rename(candidate, confPath); err != nil {
				return WriteReport{}, commandFailed("替换托管配置 %s 失败：%v", confPath, err)
			}
			fileState = FileStateWritten
		}
		if err := fs.FsyncDir(paths.ConfDir()); err != nil {
			return WriteReport{}, commandFailed("%s 已变更，但目录 fsync 失败（掉电后可能回退）：%v",
				paths.ConfDir(), err)
		}
		reporter("proxy_config_file", map[string]any{"path": confPath, "state": fileState})
	} else if desired == "" {
		fileState = FileStateAbsent
	}
	observed["file_state"] = fileState
	observed["config_hash"] = desiredHash
	changedSomething := changed || len(stale) > 0 || authMode == "written" || authMode == "removed"

	// ---- 5. nginx -t 门禁 ----
	testResult, err := a.nginxTest(ctx)
	if err != nil || testResult != "" {
		// 校验不过：回到原样，并且再校验一次，把“回滚之后是好的”也变成证据。
		rollbackErr := rollback.apply()
		observed["nginx_test"] = "failed"
		observed["nginx_test_error"] = testResult
		observed["rollback"] = rollback.describe(rollbackErr)
		if rollbackErr != nil {
			observed["rollback_failed"] = true
			return WriteReport{}, commandFailed(
				"新配置没有通过 nginx -t，且回滚失败：%s（回滚错误：%v）。"+
					"这台机器上的 Nginx 配置现在需要人工检查：%s",
				testResult, rollbackErr, rollback.hint())
		}
		// 回滚后的树也校验一次。它应当是好的；如果不是，说明这台机器本来就有别的问题。
		afterTest, afterErr := a.nginxTest(ctx)
		if afterErr != nil || afterTest != "" {
			return WriteReport{}, commandFailed(
				"新配置没有通过 nginx -t，已回滚到旧配置，但回滚后的整棵树仍然校验失败：%s。"+
					"这台机器上还有别的配置问题，请人工检查：%s", afterTest, rollback.hint())
		}
		return WriteReport{}, dependencyBlocked(
			"新配置没有通过 nginx -t，已回滚、旧配置仍可用：%s", testResult)
	}
	observed["nginx_test"] = "ok"

	// ---- 5b. 证书换过没有（配置字节不变也可能换了服务内容）----
	//
	// 续期只改证书文件，渲染结果逐字节相同。如果这里只按 changedSomething 判断，
	// 续期之后应用一次会得到 skipped_unchanged——新证书在盘上，443 上还是旧的，
	// 而报告说"什么都没变"。所以问 443 一句：你现在拿的是哪一张？
	certRotated := false
	diskFP, _ := observed["cert_fingerprint"].(string)
	if !changedSomething && def.Enabled && p.Certificate != nil {
		served, probeErr := a.servedCert(ctx, def.Domain, ManagedTLSPort)
		switch {
		case probeErr != nil:
			// 问不到不改结论：配置已经过了 nginx -t，reload 也修不好一个握不上手的 443。
			// 但必须说出来——"没测到"和"没变化"不是一回事。
			observed["served_cert_probe"] = "failed: " + probeErr.Error()
		default:
			observed["served_cert_fingerprint"] = served
			observed["served_cert_probe"] = "ok"
			certRotated = diskFP != "" && !sameFingerprint(diskFP, served)
		}
	}

	// ---- 6. reload ----
	unit := a.unitState(ctx)
	observed["unit_state"] = unit
	reloadState := ReloadSkippedInactive
	if unit == "active" || unit == "reloading" {
		if !changedSomething && !certRotated {
			reloadState = ReloadSkippedNoChange
		} else {
			if certRotated && !changedSomething {
				// 只有这一个原因是"配置没动但必须 reload"，写出来，别让人以为
				// 一次什么都没改的应用凭空重载了一次。
				observed["reload_reason"] = ReloadReasonCertRotated
			}
			reloadState, err = a.nginxReload(ctx)
			if err != nil {
				// 配置是好的，reload 没被接受：不回滚（回滚会把一份正确的配置丢掉），
				// 如实说“没生效”，并在告警里点名。
				notes = append(notes, Blocker{
					Code:    CodeUnitInactive,
					Message: "reload 没有被接受：" + err.Error() + "；配置已经写对，下次 reload 会生效",
				})
			} else if certRotated {
				// reload 是动作，"服务上换成了新证书"是结果。等新一代 worker 接上之后再问，
				// 把结果写进报告：续期之后真正要紧的就是这一句。
				after, tries, afterErr := a.waitServedCertMatch(ctx, def.Domain, ManagedTLSPort, diskFP)
				observed["served_cert_probe_attempts"] = tries
				if afterErr != nil {
					observed["served_cert_after_reload"] = "probe_failed: " + afterErr.Error()
				} else {
					observed["served_cert_after_reload"] = after
					if !sameFingerprint(diskFP, after) {
						notes = append(notes, Blocker{
							Code: CodeTLSServedStale,
							Message: "reload 已经做过、也等过新一代 worker，但 443 上仍然不是磁盘上这张证书（" +
								shortFingerprint(after) + " ≠ " + shortFingerprint(diskFP) + "，问了 " +
								strconv.Itoa(tries) + " 次）：中间可能还有一层代理，或者这个域名没有命中本面板的 server 块",
						})
					}
				}
			}
		}
	} else if unit == "" {
		notes = append(notes, Blocker{
			Code:    CodeUnitInactive,
			Message: "读不到 nginx.service 的状态：配置已写入，但无法确认服务是否重载",
		})
	} else {
		notes = append(notes, Blocker{
			Code: CodeUnitInactive,
			Message: "nginx.service 的状态是 " + unit + "：配置已写入，" +
				"但这次没有 reload（服务起来之后它会读到这份配置）",
		})
	}
	observed["reload"] = reloadState

	// ---- 7. 读回核对 ----
	final, err := a.opts.Facts.Collect(ctx)
	if err != nil {
		return WriteReport{}, commandFailed("配置已写入，但读回核对失败：%v", err)
	}
	a.opts.Facts.Invalidate()
	readback := readbackState(final, confPath, desiredHash, def.Enabled, def.Domain)
	observed["readback"] = readback
	if def.Enabled && !readback.OK {
		return WriteReport{}, commandFailed("配置写进去了，但读回核对不通过：%s（文件 %s）",
			readback.Reason, confPath)
	}

	served := def.Enabled && readback.OK &&
		(unit == "active" || unit == "reloading") &&
		(reloadState == ReloadDone || reloadState == ReloadSkippedNoChange)
	observed["served"] = served
	observed["managed_file_count"] = managedCount(final)

	report := WriteReport{
		Outcome:  OutcomeAccepted,
		Observed: observed,
		Data: map[string]any{
			"site_id":     p.SiteID,
			"domain":      def.Domain,
			"config_file": confPath,
			"config_hash": desiredHash,
			"file_state":  fileState,
			"reload":      reloadState,
			"served":      served,
			"tested":      true,
			"applied":     def.Enabled && readback.OK,
		},
	}
	if served || (!def.Enabled && readback.OK && reloadState != ReloadFailed) {
		report.Outcome = OutcomeVerified
	}
	if len(notes) > 0 {
		msgs := make([]string, 0, len(notes))
		for _, n := range notes {
			msgs = append(msgs, n.Message)
		}
		report.RecoveryHint = strings.Join(msgs, "；")
	}
	return report, nil
}

// blockerError 把判决映射成带状态码的错误。
//
// 依赖类阻塞是 409 DEPENDENCY_BLOCKED（调用方要做的是先满足前置条件），
// 其余是 400：那是参数本身不被接受。
func blockerError(a Assessment) error {
	codes := make([]string, 0, len(a.Blockers))
	messages := make([]string, 0, len(a.Blockers))
	dependency := false
	for _, b := range a.Blockers {
		codes = append(codes, b.Code)
		messages = append(messages, b.Message)
		switch b.Code {
		case CodeNginxUnavailable, CodeNginxNotInstalled, CodeSecretInvalid,
			CodeDomainTaken, CodeDomainUnknown, CodeNotWritable:
			dependency = true
		}
	}
	joined := strings.Join(messages, "；")
	if dependency {
		return dependencyBlocked("%s（%s）", joined, strings.Join(codes, ","))
	}
	return validationError("%s（%s）", joined, strings.Join(codes, ","))
}

// resolvePasswords 按 secret_ref 解出每个账号的明文（只在内存里停留一次）。
func (a *Applier) resolvePasswords(ctx context.Context, def Definition) (map[string]string, error) {
	if a.opts.Secrets == nil {
		return nil, dependencyBlocked("没有接上秘密存储，无法读取 basic auth 的密码（%s）", CodeSecretInvalid)
	}
	out := make(map[string]string, len(def.BasicAuthUsers))
	for _, u := range def.BasicAuthUsers {
		plain, err := a.opts.Secrets.Resolve(ctx, u.SecretRef)
		if err != nil {
			zeroPasswords(out)
			return nil, dependencyBlocked("账号 %s 的密码引用解不开：%v（%s）",
				u.Name, err, CodeSecretInvalid)
		}
		s := string(plain)
		zeroBytes(plain)
		if s == "" {
			zeroPasswords(out)
			return nil, dependencyBlocked("账号 %s 的密码是空的（%s）", u.Name, CodeSecretInvalid)
		}
		out[u.Name] = s
	}
	return out, nil
}

// zeroPasswords 清掉已经用过的明文。
func zeroPasswords(m map[string]string) {
	for k := range m {
		delete(m, k)
	}
}

// zeroBytes 把一段字节清成 0。
//
// Go 的 string 无法原地清零，所以这里只清 []byte 这一份；能做的做到，说清楚做不到的。
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// authFileOwner 决定 htpasswd 的权限与属主。
//
// nginx 以 worker 用户读 auth_basic_user_file，所以文件必须对该用户可读。首选
// 0640 + 属组改成 worker 的组；定位不到 worker 用户时退回 0644 并**明说**——
// 一份读不到的账号文件会让所有请求 403，而一份世界可读的哈希文件只是把爆破材料
// 放在了本机用户手里。两者都不好，但前者更难查，所以先保证能用，同时把风险写进
// 报告，而不是让它在某个下午变成一次“为什么一直 401”。
func (a *Applier) authFileOwner(fs ConfigFS, paths ConfigPaths) (os.FileMode, int, int, string) {
	worker, source := nginxWorkerUser(fs.ReadFile, path.Join(paths.Root, "nginx.conf"))
	if worker == "" {
		return 0o644, -1, -1, "定位不到 nginx 的 worker 用户：" +
			"账号文件写成 0644（任何本机用户都能读到这些 apr1 哈希）"
	}
	u, err := user.Lookup(worker)
	if err != nil {
		return 0o644, -1, -1, fmt.Sprintf("nginx 的 worker 用户 %q 在本机不存在：账号文件写成 0644", worker)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0o644, -1, -1, "worker 用户的组 id 解析失败：账号文件写成 0644"
	}
	_ = source
	// 属主仍留 root（只有 root 能改），属组给 worker。
	return 0o640, 0, gid, ""
}

// workerUserPattern 匹配主配置里的 user 指令。
var workerUserPattern = regexp.MustCompile(`(?m)^\s*user\s+([A-Za-z0-9_.-]+)`)

// nginxWorkerUser 从主配置里读 `user <name>;`。
//
// 读不到就返回空串（调用方据此退回 0644 并报告），**不猜**：猜成 www-data 在
// 一台用 nobody 的机器上只会让文件多一个读不到的用户。
func nginxWorkerUser(readFile func(string) ([]byte, error), confPath string) (string, string) {
	data, err := readFile(confPath)
	if err != nil {
		return "", ""
	}
	m := workerUserPattern.FindSubmatch(data)
	if m == nil {
		return "", ""
	}
	return string(m[1]), confPath
}

// readMaybe 读一个文件，不存在时返回 (nil, os.ErrNotExist)。
func readMaybe(fs ConfigFS, path string) ([]byte, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}

// ensureDir 建目录（已存在不算错）。
func ensureDir(fs ConfigFS, dir string) error {
	if _, err := fs.Stat(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return fs.MkdirAll(dir, managedDirMode)
}

// staleManagedFiles 找出属于这个站点、但文件名已经不对的托管文件。
//
// 判据是文件内容里的标记行（`# 由 uPanel 管理：站点 site_…`），不是文件名：
// 域名改过之后，旧文件名与新域名无关，只有标记能说明它曾经属于谁。只认
// `upanel-*.conf` 这个前缀——那是面板自己的文件；用户把文件改名成别的，
// 面板就当它变成了用户的文件（P10.1 的“松手”规则，同一条）。
func (a *Applier) staleManagedFiles(fs ConfigFS, paths ConfigPaths, siteID, keepPath string) ([]string, error) {
	entries, err := fs.ReadDir(paths.ConfDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, commandFailed("读不到 %s：%v", paths.ConfDir(), err)
	}
	marker := ManagedMarker + siteID
	var stale []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !nginx.IsManagedName(name) {
			continue
		}
		full := path.Join(paths.ConfDir(), name)
		if full == keepPath {
			continue
		}
		data, err := readMaybe(fs, full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, commandFailed("读不到 %s：%v", full, err)
		}
		if strings.Contains(string(data), marker) {
			stale = append(stale, full)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// backup 把一份文件复制到备份目录，文件名带时间戳与内容哈希前缀。
func (a *Applier) backup(paths ConfigPaths, original string, data []byte) (string, error) {
	fs := a.fs()
	if err := fs.MkdirAll(paths.BackupDir, 0o700); err != nil {
		return "", commandFailed("创建备份目录 %s 失败：%v", paths.BackupDir, err)
	}
	name := fmt.Sprintf("%s-%s.%s",
		a.opts.Now().UTC().Format("20060102T150405Z"), hashOf(string(data))[:12], path.Base(original))
	dest := path.Join(paths.BackupDir, name)
	if err := fs.WriteFileSync(dest, data, 0o600); err != nil {
		return "", commandFailed("备份 %s 失败：%v", original, err)
	}
	return dest, nil
}

// nginxTest 跑 `nginx -t`。返回空串表示通过，否则返回一段可读的失败原因。
//
// **-t 校验的是整棵树**，包括发行版默认站点和用户自己的文件。这是刻意的：面板
// 写入之后必须能说“这台机器现在的配置是好的”，而不只是“我这一份是对的”。
// 代价是：机器上本来就有一份坏配置时，面板什么都写不进去——这同样是对的，
// 那种时候最不该做的事就是再往上叠一份。
func (a *Applier) nginxTest(ctx context.Context) (string, error) {
	res, err := a.opts.Runner.Run(ctx, syscmd.Spec{
		Program: a.paths().NginxPath,
		Args:    []string{"-t"},
	}, syscmd.Limits{Timeout: validateTimeout})
	if err != nil {
		// “nginx -t 退出码非 0”不是执行失败，那是**校验结论**：nginx 说了什么，
		// 必须原样带出去。syscmd 对非零退出返回 *ExitError，如果把它当成传输层错误，
		// 上层就只能报“校验失败：”，把 nginx 的原话丢掉——P10.4 的验收正是被这条
		// 卡住过：作业说明里只有“回滚后的整棵树仍然校验失败：。”，没有人知道为什么。
		var exitErr *syscmd.ExitError
		if !errors.As(err, &exitErr) {
			return "", err
		}
	}
	if res.ExitCode == 0 {
		return "", nil
	}
	return trimOutput(res.Stderr + res.Stdout), nil
}

// nginxReload 发 HUP（nginx -s reload）。
//
// 不用 systemctl reload：那是“按发行版的方式重载”，而我们已经知道 nginx 二进制的
// 绝对路径（-t 用的同一个）。用同一个程序做校验和重载，两件事作用于同一棵配置树。
// 更不会用 restart：PLAN.md 第 13 节明确禁止把重启当作常规重载。
func (a *Applier) nginxReload(ctx context.Context) (string, error) {
	res, err := a.opts.Runner.Run(ctx, syscmd.Spec{
		Program: a.paths().NginxPath,
		Args:    []string{"-s", "reload"},
	}, syscmd.Limits{Timeout: reloadTimeout})
	if err != nil {
		return ReloadFailed, err
	}
	if res.ExitCode != 0 {
		return ReloadFailed, fmt.Errorf("nginx -s reload 退出码 %d：%s",
			res.ExitCode, trimOutput(res.Stderr+res.Stdout))
	}
	return ReloadDone, nil
}

// unitState 问一次 nginx.service 的 ActiveState。
//
// 返回空串表示读不到（不是“没在跑”）。判据与 internal/shares 一致：看打印出来的
// 值，不看退出码——`is-active` 对未激活的 unit 以退出码 3 结束，那是一个正常答案。
func (a *Applier) unitState(ctx context.Context) string {
	res, err := a.opts.Runner.Run(ctx, syscmd.Spec{
		Program: storage.DefaultSystemctlPath,
		Args:    []string{"is-active", nginx.DefaultUnitName},
	}, syscmd.Limits{Timeout: unitProbeTimeout})
	var exitErr *syscmd.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return ""
	}
	value := strings.TrimSpace(strings.Split(res.Stdout, "\n")[0])
	switch value {
	case "active", "reloading", "inactive", "failed", "activating", "deactivating", "unknown":
		return value
	}
	return ""
}

// readbackResult 是写完之后从主机读回来的核对结论。
type readbackResult struct {
	OK     bool
	Reason string
	// FileHash / ExpectedHash 两边都留着：不一致时报告要能说出差在哪。
	FileHash     string
	ExpectedHash string
	// Present 报告文件此刻在不在。
	Present bool
	// Names 是这份文件里解析出的 server_name。
	Names []string
}

// readbackState 用一次**新的采集**核对结果。
//
// 核对三件事：文件在不在该在的状态、内容字节是不是刚写下去的那一份、server_name
// 里有没有这个域名。第三件是关键：文件内容对但没被 include 时，前两件都成立而站点
// 不生效——那种“写对了却没生效”正是最该被抓住的一种。
func readbackState(facts nginx.Facts, confPath, expectedHash string, enabled bool, domain string) readbackResult {
	out := readbackResult{ExpectedHash: expectedHash}
	var found *nginx.ConfigFile
	for i := range facts.Files {
		if facts.Files[i].Path == confPath {
			found = &facts.Files[i]
			break
		}
	}
	out.Present = found != nil
	if !enabled {
		if found != nil {
			out.Reason = "enabled=false，但配置树里仍然有 " + confPath
			return out
		}
		out.OK = true
		return out
	}
	if found == nil {
		out.Reason = "配置树里没有 " + confPath + "（写下去了，但 include 没读到它？）"
		return out
	}
	out.FileHash = found.SHA256
	if found.Error != "" {
		out.Reason = "文件解析失败：" + found.Error
		return out
	}
	if found.SHA256 != expectedHash {
		out.Reason = "文件内容与刚写下去的那一份不一致（可能是别人改了）"
		return out
	}
	for _, s := range found.Servers {
		for _, n := range s.Names {
			out.Names = append(out.Names, n.Raw)
			if n.Kind == nginx.NameExact && n.Host == domain {
				out.OK = true
			}
		}
	}
	if !out.OK {
		out.Reason = "文件里没有 server_name " + domain
	}
	return out
}

// managedCount 数一下这台机器上有几份面板自己的托管文件。
func managedCount(facts nginx.Facts) int {
	n := 0
	for _, f := range facts.Files {
		if f.Managed {
			n++
		}
	}
	return n
}

// hashOf 返回内容的 sha256（小写十六进制）。
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// trimOutput 把命令输出收成一行可读的摘要。
func trimOutput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "（命令没有输出）"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 6 {
		lines = append(lines[:6], "…")
	}
	return strings.Join(lines, " / ")
}

// rollbackList 记录这次动过的文件，失败时逐个还原。
type rollbackList struct {
	fs      ConfigFS
	entries []rollbackEntry
}

type rollbackEntry struct {
	path    string
	content []byte
	existed bool
}

func newRollback(fs ConfigFS) *rollbackList { return &rollbackList{fs: fs} }

// record 记下一个文件的旧内容。existed=false 表示这次是“新建”，回滚就是删掉它。
func (r *rollbackList) record(path string, content []byte, existed bool) {
	r.entries = append(r.entries, rollbackEntry{path: path, content: content, existed: existed})
}

// apply 逐个还原。返回第一个错误，但**继续**还原其余的：能救一个是一个。
func (r *rollbackList) apply() error {
	var first error
	for i := len(r.entries) - 1; i >= 0; i-- {
		e := r.entries[i]
		var err error
		if e.existed {
			err = r.fs.WriteFileSync(e.path, e.content, managedFileMode)
		} else {
			err = r.fs.Remove(e.path)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err == nil {
			if fsyncErr := r.fs.FsyncDir(path.Dir(e.path)); fsyncErr != nil && first == nil {
				first = fsyncErr
			}
			continue
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// describe 给报告用的回滚说明。
func (r *rollbackList) describe(err error) string {
	if err != nil {
		return "回滚失败：" + err.Error()
	}
	if len(r.entries) == 0 {
		return "没有需要回滚的文件"
	}
	return fmt.Sprintf("已回滚 %d 个文件", len(r.entries))
}

// hint 给出人工恢复路径。
func (r *rollbackList) hint() string {
	if len(r.entries) == 0 {
		return "（这次没有改动任何文件）"
	}
	paths := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		paths = append(paths, e.path)
	}
	sort.Strings(paths)
	return "检查 " + strings.Join(paths, "、") + "，备份在 " + DefaultBackupDir + "（或配置里指定的备份目录）"
}
