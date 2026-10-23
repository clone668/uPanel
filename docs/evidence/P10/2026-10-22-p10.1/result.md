# P10.1 验收结果 — Nginx 发现、配置冲突、单域名单 upstream、WebSocket、请求限制、访问策略

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-22（UTC，虚拟机时钟）
脚本：`tests/integration/p10-1-proxy-acceptance.sh`（630 行，sha256 `58526e67…121ca12e`）
环境：见 `env.txt`；原生单元测试与 `go vet` 见 `native-tests.txt`；宿主门禁见 `check.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（`run.log`） | **PASS=100 / FAIL=0 / NOT RUN=0**（退出码 0） |
| 真机发现的产品缺陷 | **1 条**：`nginx -v` 的时限写成了 `5`（无单位），`syscmd.Limits.Timeout` 是 `time.Duration`，于是上限变成 5 纳秒 —— 命令还没启动就超时，版本号永远是空。单元测试在 Windows 上被跳过，只有真机跑得出来。见 §5 |
| Nginx 现状是真读的 | 版本 `1.24.0`、`unit_state=active`、`config_root=/etc/nginx`；4 份配置（`nginx.conf`、`conf.d/p101-taken.conf`、`conf.d/p101-wild.conf`、`sites-enabled/default`）；`sites-enabled/default` 是符号链接，按解析后的真实路径去重，**只登记一次** |
| 冲突结论带证据 | 命中时给出**文件 + `server_name` 所在行（第 3 行，不是 `server` 块第 1 行）+ 原文**；`*.p101wild.example.com` 占住子域但**不占住上级域**；正则 `server_name` 与解析失败的文件都把结论压成 `unknown` |
| `unknown` 不是 `clear` | 正则在场时**连无关域名**也判 `unknown` 并拒绝写入（409）；坏文件在场时同样 409；两种情况库里都**没有留下记录**。删掉夹具后同一域名立刻恢复 `clear`（正对照），证明拦住写入的确实是那份文件 |
| 读不到 ≠ 没有 | 代理停掉后列表仍 200，`nginx.read_error=agent_unavailable`、`config_missing=false`；写入与冲突判断 503 `AGENT_UNAVAILABLE`，库不变；代理重启后恢复 201 |
| 面板端口是禁区 | `upstream_port` 等于 Web **真正监听的端口 19495** → 400，错误说明指向“管理面板”。这条用的是真实端口，不是写死的 9443 |
| 归一化真的生效 | `P101-OK.Example.COM.` → `p101-ok.example.com`；同一域名换个大小写重复登记 409，库里仍是 1 条 |
| 默认值对齐 nginx | `max_body_bytes=1048576`（1m）、`read_timeout_seconds=60`（60s）、`access_mode=public`、`upstream_scheme=http`；`enabled=true`、`version=1` |
| 秘密只进不出 | 建 `basic_auth` 站点后响应里只有 `name`，`secret_ref` 不在响应、也不在审计（`redacted_details` 里 0 次），审计两条 `proxy.create` 都带 `"applied":false` |
| **一个字节都没写 Nginx** | 全部写操作前后对 `/etc/nginx` 做内容摘要（路径+内容 sha256），两次相等；收尾 `nginx -t` 仍通过；`conf.d` 无残留夹具；`managed_file_count=0` |
| 权限边界 | viewer 读列表 / 读现状 / 建站点全 403；未登录 401 |

这一轮**没有**放宽任何断言，也**没有**为了让脚本过而改产品代码去迁就测试；产品代码只改了一处真实缺陷（`5` → `5 * time.Second`）。

## 2. 范围（做了什么、没做什么）

已做（STAGES §12 第 1 条的全整内容）：

- **Nginx 发现**：`nginx.state` 查询（代理侧只给事实）+ `internal/proxies` 判断。读 `nginx.conf`、`conf.d/*.conf`、`sites-enabled/*`（含符号链接去重），`nginx -v` 版本、`nginx.service` 状态。
- **配置冲突**：`GET /proxies/conflict`，三态（`clear` / `taken` / `unknown`），带 `matches` / `undecidable` / `broken_files` / `existing_site_id` 证据。
- **单域名单 upstream**：`domain` 归一（IDNA A-label、小写、去尾点、禁通配符/协议/端口/路径）、`upstream_target_id` 闭集形状、`upstream_scheme` 只 http/https、`upstream_port` ≠ 面板端口。
- **WebSocket**：`websocket_enabled` 作为定义字段入库（生成配置是 P10.2）。
- **请求限制**：`max_body_bytes`（1 KiB—1 GiB，默认 1m）、`read_timeout_seconds`（1—3600，默认 60）。
- **访问策略**：`public` / `cidr`（非空、掩码收敛、排序去重、拒绝 `/0`）/ `basic_auth`（账号名形状、密码只收 `sec_` 引用、重名拒绝），模式与附加字段必须匹配，多给也报错。
- 站点定义的增删改查、乐观锁（`version`）、审计（`proxy.create` / `proxy.update` / `proxy.delete`，`applied:false`）。

没做（本阶段明确不做，脚本也不验）：

- **配置生成、原子写入、`nginx -t` 门禁、reload、失败恢复**：P10.2。本轮 `/etc/nginx` 内容摘要前后一致，正是“没做”的证据。
- **证书 / HTTPS 上游的临期与续期**：P10.3。
- **基本认证口令的实际校验**：要等 P10.2 生成 htpasswd 才有意义。
- **`enabled` 的语义**：P10.1 里它是“意图”，不是“已生效”；没有任何 `applied_*` 字段。

## 3. 证据清单

| 文件 | 内容 |
|---|---|
| `run.log` | 最终一遍，100 PASS / 0 FAIL |
| `run-attempt-1.log` | 第 1 遍全 78 项失败：Web 没起来（sqlite `attempt to write a readonly database`）。原因是**脚本**把库所在目录留给了 root，Web 用户无法建 `-wal`。修脚本，不改产品 |
| `run-attempt-2.log` | 第 2 遍 93 PASS / 6 FAIL：1 条**产品缺陷**（版本号为空）+ 4 条脚本自身期望写错（审计应为 2 条、`taken` 夹具在 §4 之前已被删、以及由此连锁的删除版本号）+ 1 条连锁 |
| `payloads/nginx-facts.json` | `GET /proxies/nginx` 的原始响应（4 份配置、`names_line=3`/`46`、`listen`、`includes`） |
| `logs/agent.log` | 代理启动、Nginx 现状读取开关、停/起（503 段） |
| `logs/web.log` | Web 启动与 `readonly database` 那次失败的原话 |
| `logs/nginx-t.log` | 收尾 `nginx -t` |
| `native-tests.txt` | 宿主 `go vet ./...` + `go test -count=1 ./...`：全部包 ok，无 FAIL |
| `check.txt` | 宿主门禁 `scripts/check.sh`：**PASS 13 / FAIL 0 / NOT RUN 0**（go 与 frontend 全套 + openapi lint） |
| `env.txt` | 虚拟机、nginx、身份、二进制 md5 |

## 4. 关键断言是怎么证明的（不是“接口返回 200”）

1. **符号链接去重**：`sites-enabled/default` → `sites-available/default`，断言“路径以 `/default` 结尾的条目恰好 1 条”且“所有路径互不重复”。若按显示路径不去重，`server_name _` 会被数两遍，冲突证据会指两次。
2. **证据行号**：夹具第 1 行是 `server {`，第 3 行是 `server_name`。断言 `line == 3`，证明取的是 `names_line` 而不是块起始行 —— 给人看的证据必须能直接翻到那一行。
3. **夹具隔离**：正则 `server_name` 会让**所有**域名的结论变成 `unknown`。脚本因此分三段放夹具（普通/通配 → 正则 → 坏文件），每段测完立刻删并做正对照，避免“上一段的夹具把下一段判成 unknown”被误读成产品崩了。
4. **`applied:false`**：直接查库 `SELECT COUNT(*) … redacted_details LIKE '%"applied":false%'`，期望 2（两次成功创建），不是靠接口字段自说。
5. **没动 Nginx**：`find /etc/nginx -type f | sort | xargs sha256sum | sha256sum` 前后相等 + `nginx -t` 通过 + `conf.d` 无 `p101-*` 残留，三重。
6. **读不到 ≠ 没有**：停代理后 `read_error=agent_unavailable` 而 `config_missing=false`；写 503、库条数不变；重启后补做同一条创建拿到 201（同一进程、同一库）。

## 5. 过程中改了什么（含一条真缺陷）

1. **产品缺陷（真机才有）**：`internal/agent/nginxquery.go` 里 `const maxNginxVersionTimeout = 5` → `5 * time.Second`。证据：`run-attempt-2.log` 的 `读到 nginx 版本: 不该等于 ''` 与代理日志 `读 nginx 版本失败：syscmd: 命令超出运行时限（/usr/sbin/nginx，上限 5ns）`。修后 `run.log` 显示 `版本：1.24.0`；代理 `warnings` 清空（`payloads/nginx-facts.json`）。
   教训：`nginx -v` 这条路径的单元测试在 Windows 上 `t.Skip`，宿主的“全绿”证明不了它。
2. **脚本缺陷**：库目录归 root 导致 `readonly database`（第 1 遍 78 项失败）；审计期望应为 2 而非 1；`taken` 夹具在 §4 前已删导致 PATCH 用例失去对照 —— 改为“改成另一条面板记录已用的域名”，反而更贴 P10.2 的真实风险。
3. **写文档时才发现的两处不一致**（不是验收抓出来的，是自己对账对出来的）：迁移 `0009_proxies.sql` 的注释说站点标识是 `prx_`、说 `enabled` 只由 P10.2 置 1，与实现/契约都不符；表名写成 `proxy_sites` 而 CONTRACTS §14.1 写的是 `proxies`。两处都在提交前改正（迁移文件本轮新增、尚未在任何长期库上应用过，改的是注释与表名，`run.log` 是在改完之后重跑出来的——不是先跑后改）。
4. **没有**为了提高通过数删断言、关保护或改契约。

## 6. 未执行（NOT RUN，明确列出）

- 配置生成 / 原子写 / `nginx -t` 门禁 / reload / 失败回滚：**未执行**（P10.2 的范围，本轮未实现）。
- 真实域名的 HTTPS 访问与证书签发：**未执行**（P10.3）。
- `basic_auth` 口令校验（htpasswd 生效态）：**未执行**（依赖 P10.2）。
- 浏览器渲染与前端交互：**未执行**（虚拟机上没有浏览器；P10.1 也没有前端页面）。
- 多机 / 远程 Nginx：**未执行**（PLAN 限定单机）。

以上全部写在脚本头注释里，脚本不会假装验过。
