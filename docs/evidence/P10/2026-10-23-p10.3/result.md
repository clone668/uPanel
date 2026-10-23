# P10.3 验收记录 — 证书签发、续期、手动导入与私钥保护

日期：2026-10-23
阶段条款：`docs/STAGES.md` §12 第 3 条
「Certbot staging 签发流程、正式签发明确选择、手动证书导入、到期信息、私钥保护；建立续期动作，timer 在 P12 接入。」
P10 验收附带条款：「私钥不能通过任何文件/API/日志读取；证书测试使用 staging，不无上限调用正式 ACME。」

---

## 1. 范围

| 条款 | 本轮做了什么 |
|---|---|
| staging 签发流程 | `POST /api/v1/certificates`（`source=certbot`）→ 预检 → `certificate.issue` 任务 → agent 以 root 调 `certbot certonly --webroot`；HTTP-01 校验目录 `/var/www/upanel-acme` |
| 正式签发明确选择 | 参数 `staging *bool` **必填**；`--server` 只由 `staging` 推导（`ServerURL`），不接受调用方给地址；正式地址只能由 root 拥有的 agent 配置覆盖（`agent.certificates.staging_directory_url`，且必须是 `https://`） |
| 手动证书导入 | 公钥走 `POST /certificates/uploads`（multipart 字段 `certificate`，1 MiB，10 分钟一次性、绑会话、正文带私钥直接 400）；私钥走 `POST /secrets`（purpose `tls_private_key`，16 KiB）→ `secret_ref`，落地时先 `Take` 再校验 `CoversDomain` |
| 到期信息 | `certificates` 账本 + `tlscert.Inspected` 现场读盘；`renewal_state` = `valid`/`expiring`（21 天窗）/`expired`；`cert_expiring`、`cert_expired` 进 `proxy.apply` 的 observed 与告警位 |
| 私钥保护 | 手动目录 `/etc/ssl/upanel` 0700、`<domain>.key` 0600 root；certbot 侧 `live` 0700、`live/<domain>` 只放符号链接 + README；面板（uid 999）读不到；所有 API/日志/DB 出口逐条搜索断言 |
| 续期动作 | `POST /api/v1/certificates/{id}/renew` + `certificate.renew` 任务；同域名 1 小时冷却（读 jobs 表，`RecentAttempts`）→ 429 `RESOURCE_BUSY`；续期只换盘上文件，生效靠下一次 `proxy.apply` 的轮换检测（见 §5） |
| timer | **不在本轮**：STAGES 写明 P12 接入 |

---

## 2. 产品代码清单

新增：

| 文件 | 行数 | 作用 |
|---|---|---|
| `internal/certificates/certificates.go` | 353 | 领域模型、状态推导、错误码 |
| `internal/certificates/ops.go` | 760 | 签发/续期/导入的 Decode→Plan→Execute |
| `internal/certificates/ledger.go` | 383 | 账本写入（唯一写点在 `OnJobFinished`） |
| `internal/certificates/store.go` | 563 | 持久化、按域名唯一 |
| `internal/certificates/materialize.go` | 286 | 手动证书落地（目录/权限/原子写） |
| `internal/certificates/certbot.go` | 204 | certbot 调用、`--server` 推导、参数拼装 |
| `internal/certificates/staged.go` | 195 | 上传暂存（一次性、绑会话、超时清） |
| `internal/certificates/inspect.go` | 93 | 现场读盘（权限、指纹、有效期） |
| `internal/certificates/check.go` | 52 | 签发前 HTTP-01 可达性 |
| `internal/certificates/errors.go` | 83 | 稳定错误码 |
| `internal/api/certificates.go` | 378 | 列表/详情/签发/续期/上传 5 个端点 |
| `internal/api/certificateexpand.go` | 272 | 预检参数展开、id 在展开期定死 |
| `internal/api/certlink.go` | 71 | 站点 ↔ 证书绑定 |
| `internal/agent/certificateaction.go` | 272 | agent 侧三个固定动作 |
| `internal/tlscert/` | — | 证书解析、指纹、`ExpiringWindow` |
| `internal/proxies/tlsmaterial.go` | 156 | 站点 TLS 材料解析 |
| `internal/proxies/tlscheck.go` | 147 | `PROXY_TLS_*` 十个码 |
| `internal/proxies/servedcert.go` | 128 | **443 实际在用的证书指纹（SNI 握手实测）** |
| `internal/proxies/certificatelink.go` | 70 | 证书绑定进入渲染 |
| `internal/operations/attempts.go` | — | 签发尝试的冷却查询 |
| `migrations/0010_certificates.sql` | — | 第 10 版迁移 |

修改（只列有语义变化的）：`internal/proxies/applyrun.go`（轮换检测与 settle 等待）、`internal/proxies/apply.go`（`ReloadReasonCertRotated`、`ServedCert` 注入缝）、`internal/proxies/render.go`（TLS server 块）、`internal/api/router.go`、`internal/api/server.go`、`internal/operations/jobs.go`。

---

## 3. 检查结果（宿主侧，全部真跑）

| 检查 | 结果 |
|---|---|
| `gofmt -l` | 无输出（clean） |
| `go vet ./...` | 通过 |
| `go test ./...` | 36 个包 ok，3 个包无测试文件 |
| Linux 交叉编译 | `upanel-agent` / `upanel-web` / `upanelctl` 三个二进制成功，sha256 见 `env.txt` |
| `scripts/check.sh` | PASS 12 / FAIL 1 / NOT RUN 0 — FAIL 是 `node-version`：宿主 v24.15.0，`.nvmrc` 钉 22.23.3。环境问题，未切 nvm，也没把它记成通过 |
| `api/openapi.yaml` | redocly lint valid，6 warning（`no-unused-components` 4→3）；66 path / 79 implemented path / 6 个 `Certificate*` schema |

明细：`check.txt`、`native-tests.txt`。

---

## 4. VM 验收（一次性 Ubuntu 24.04.5，第 4 遍）

`tests/integration/p10-3-cert-acceptance.sh`，sha256 `535f0a1c…`（与仓库内文件一致），
**PASS=176 / FAIL=0 / NOT RUN=7**，退出码 0。

| 段 | PASS | 内容 |
|---|---|---|
| 0 | 3 | 干净起点、`nginx -t` 通过、校验根目录存在 |
| 1 | 7 | Pebble 在跑；系统信任库接受它的 API 证书；目录里既没有 Let's Encrypt 正式也没有测试环境 |
| 2 | 3 | `/etc/hosts` + nat 把 80 → 5002（HTTP-01 落点） |
| 3 | 10 | 代理 root、Web uid 999 **不是 root**、库迁移到第 10 版、登录与 CSRF |
| 4 | 9 | 上游起来、站点 A 登记、apply 到 80 能取到页面 |
| 5 | 36 | 缺 `staging` 被拒（错误码 + 文案指到 staging）；签发 verified；`live/` 0700、`live/<域名>` 只有符号链接 + README；`privkey.pem` root:root；**uid 999 读不到私钥、也进不去目录**；库里指纹/到期时间与盘上一致 |
| 6 | 12 | 绑定证书 → `nginx -t` → reload → 带系统信任链在 443 取到上游页面 |
| 7 | 24 | 冷却 429；续期 verified；记录标识不变、指纹变、绑定不变、到期刷新；**续期后重新应用即使配置字节没变也 reload**（理由 `cert_rotated`）；报告里同时记下 reload 前（旧）与 reload 后（新）的指纹；第三轮又回到 `skipped_unchanged` |
| 8 | 32 | 上传接口取公钥、秘密通道取私钥、`Take` 一次性；站点 B 在 443 上服务的就是导入的证书 |
| 9 | 11 | `valid` / `expiring`（21 天窗）/ 过期证书绑定被挡 |
| 10 | 22 | 私钥正文逐出口搜索：三份日志、五个表、预检参数、任务结果、所有响应体；接口不吐 `key_path`/`cert_path`/`key_secret_ref` |
| 11 | 3 | 上传与证书任务留下审计；审计里没有私钥内容 |
| 12 | 4 | 收尾：托管配置清理、`nginx -t` 通过、`/etc/hosts` 还原、nat 规则删除 |

第 7 段的实测值（`payloads/job-apply-a3.json`）：
`file_state=unchanged`、`reload=done`、`reload_reason=cert_rotated`、
`served_cert_fingerprint=a185e5be…`（旧）、`served_cert_after_reload=2b8e7891…`（新，与盘上 `cert_fingerprint` 相等）、
`served_cert_probe_attempts=2`。

---

## 5. 本轮的四个失败根因（前几遍，已修，计数保留）

| 遍 | PASS/FAIL/NOT RUN | 失败 | 归到哪 |
|---|---|---|---|
| 1 | 114 / 37 / 8 | 37 | 7 个根因，其中脚本自身 4 处（断言取错、curl 缺 `--resolve`、路径未引号、清理顺序） |
| 2 | 165 / 2 / 7 | 2 | ① `live/<域名>` 里 certbot 会写一个 692 字节的 `README`，断言「没有真文件」错；② **续期后的证书永远不可能生效** —— 见下 |
| 3 | 172 / 4 / 7 | 4 | 2 个产品 + 2 个脚本，都是 `nginx -s reload` 的**异步性** |
| 4 | 176 / 0 / 7 | 0 | — |

**② 是本轮唯一真正补上的产品缺口。** 第 2 遍把续期走通了，但续期只换盘上的文件；`proxy.apply` 判断「要不要 reload」看的是配置字节，字节没变就是 `skipped_unchanged`，于是 443 上永远是旧证书 —— 也就是说**续期在面板上没有任何一条路径能让它真正生效**。

修法不是加参数、也不是记状态，而是**去测**：站点绑了证书、配置字节又没变时，用 SNI 对 `127.0.0.1:443` 做一次握手，取叶证书 DER 的 sha256，与磁盘上的证书指纹比。不一致就 reload，理由写 `cert_rotated`；reload 之后再测，给 12 × 250 ms 的 settle 预算，仍然不一致才报 `PROXY_TLS_SERVED_STALE`（告警，不阻断）。探测失败不改变结论（不 reload），但把 `served_cert_probe=failed: …` 记进 observed —— 不知道不能当作没变化。

第 3 遍的 4 个失败正是这个预算的必要性：`nginx -s reload` 只把 HUP 发出去就返回，紧跟着的握手还是旧 worker 的旧证书（实测拿到 `c202…` = 旧 `cert1.pem`），新 worker 起来才换成 `f1d4…`。产品侧因此有 settle 等待，脚本侧站点 B 也要等（SNI 落到默认块就会拿到站点 A 的证书）。

决策记录：`docs/DECISIONS.md` Q293–Q298（Q297 = 轮换检测这条）。

---

## 6. NOT RUN（七条，脚本自己打印，未跑就是未跑）

1. 真实 Let's Encrypt 签发：消耗速率限额且需要公网域名与 80 端口；本脚本只连本地 Pebble。
2. 正式签发（`staging=false`）：同上；这一条由「配置默认值 + 必须显式选择」共同保证，不是实测。
3. 真实公网 DNS / 外部可达性：用 `/etc/hosts` 与 nat 重定向代替。
4. 浏览器信任表现：VM 没有浏览器；staging 证书在浏览器里的告警未验证。
5. timer 自动续期：P12 才接入；本轮只验证续期动作本身。
6. DNS-01 / 通配符证书：产品只做 HTTP-01。
7. 证书吊销与 OCSP：不在 P10.3 范围。

---

## 7. 限制（与 `docs/PROGRESS.md` §8 同一份）

1. 续期不 reload，让新证书生效靠下一次 `proxy.apply`；**P12 的 timer 必须做两步**：续期 → 对绑定了这张证书的站点各应用一次。
2. 只做 HTTP-01，不支持通配符/DNS-01。
3. 「443 用哪张证书」是实测出来的；如果 443 前面还有一层代理，测到的是那层代理的证书，会按设计报 `PROXY_TLS_SERVED_STALE`。
4. 真实 Let's Encrypt 的速率限额与账号注册未验证。
5. 本轮不做续期调度。
6. **没有代理/证书前端页面**：`docs/STAGES.md` 没有列过这个页面（P12.5 只列「证书续期 timer、备份与定时任务 UI」），加页面属于范围变更，要走 §6 登记。P10.1 的旧记录里曾把「前端页面」写成 P10.4 的事，与 STAGES 原文不符，以 STAGES 为准。

---

## 8. 收尾状态

VM 上：托管配置已清理、`nginx -t` 通过、`/etc/hosts` 已还原、nat 规则已删（第 12 段四条断言）。
`/etc/letsencrypt`、`/etc/ssl/upanel` 的测试证书保留在 VM 上用于复查，VM 是一次性机。
Pebble 仍在跑（`/opt/p103/pebble`），下一个阶段若要重跑本脚本需要它。
