# P09.5 真机载荷夹具

这里的每一个文件都是**真的从验收 VM 上抓回来的**，不是手写的。抓取者是
`tests/integration/p09-5-apps-ui-acceptance.sh`（各节的 `$FIXDIR/app-*.json` 与
预检/任务载荷）；消费者是 `web/src/components/realdata-p09-5.test.ts`。

## 为什么这里没有一条“停下来的 systemd 应用”

`apps.json` 是收尾前那一次 `GET /api/v1/apps` 的快照，那时候夹具单元已经被重新
启动了，所以列表里每一条 systemd 应用都是 `running`。

停机那一刻的载荷是单独的一份：`app-sysd-stopped.json`，由第 8 节 `wait_app`
轮询成功的那一次写入。`app-sysd-started.json` 是同一条应用再启动之后的对应一拍。
两份载荷的 `id`、`binding`、`version` 完全相同，变的只有 `runtime_state` 和
`capabilities`——这正是卡片规则要证明的事，而且是从同一个真机上、同一轮验收里
前后两拍取到的，不是两台机器各取一份拼起来的。

如果为了“列表里有一条 stopped”而去手写一份 `apps.json`，那 P08.4 的 Q246 就又回来了。

## 一句提醒：能力是会晚一拍的

应用摘要里的 `runtime_state` 来自 services 采集器的快照，默认 10 秒一轮。
刚发完 `service.control`，摘要还会说旧状态——这不是缓存坏了，是“摘要如实描述它读到
的那一轮事实”。验收脚本第 8 节因此用 `wait_app` 轮询，不是立刻断言；页面同理，
它是照着请求回来的那一份载荷画，不会自己猜。

## 别手改

改这里的任何一个字节，等于把“手写夹具天然自洽”那类假象请回来。要更新夹具，
重跑验收脚本，再从 VM 上原样取回。

## 清单

抓取时刻 `2026-10-22T22:46:58Z` 起（以载荷自带的 `collected_at` / `created_at` 为准），
VM `upanel-2404`，Ubuntu 24.04.5，docker 29.1.3，util-linux 2.39.3，容器镜像 `alpine:3.20`。
那次运行：**PASS=211 / FAIL=0 / NOT RUN=0**（`docs/evidence/P09/2026-10-22-p09.5/run.log`）。

| 文件 | 接口 | 内容 |
| --- | --- | --- |
| `apps.json` | `GET /api/v1/apps` | 12 条应用：3 条 link、7 条 docker、3 条 systemd（含受保护的 `ssh.service`、不存在单元的幽灵绑定、重建后 `unknown` 的旧绑定） |
| `app-groups.json` | `GET /api/v1/app-groups` | 两个分组：「影音」`sort_order=5`、「基础设施」`sort_order=10`（都是真改过名/排过序的） |
| `discovery.json` | `GET /api/v1/apps/discovery` | `sources` 三路都 `ok`（systemd 7 / local_listen 5 / docker 8），20 个候选，3 条 `missing_bindings` |
| `discovery-rebuild.json` | `GET /api/v1/apps/discovery` | 重建之后那一次：旧 ID 出现在 `missing_bindings` 里（页面据此点名要求重新确认） |
| `health-ok.json` | `GET /api/v1/apps/{id}/health` | 探测成功：`http_state=ok`、`status_code=200` |
| `health-error.json` | `GET /api/v1/apps/{id}/health` | 容器在跑但回 500：`status_code=500`、`http_state=error`——与 `runtime_state` 是两件事 |
| `app-logs-docker.json` | `GET /api/v1/apps/{id}/logs` | 绑 docker 的应用：`binding.kind=docker`，`docker.page.entries` 有内容、`journal` 为空 |
| `app-logs-journal.json` | `GET /api/v1/apps/{id}/logs` | 绑 systemd 的应用：`binding.kind=systemd`，`journal.page.entries` 有内容、`docker` 为空 |
| `app-sysd-stopped.json` | `GET /api/v1/apps/{id}` | 停机瞬间：`runtime_state=stopped`、`capabilities.start=true`、`stop=false` |
| `app-sysd-started.json` | `GET /api/v1/apps/{id}` | 再启动之后：同一条应用，`running`、`stop=true`、`start=false` |
| `preflight-stop-running.json` | `POST /api/v1/operations/preflight` | `service.control` 停一个普通单元的判决：`allowed=true`、无 blocker |
| `preflight-sshstop.json` | `POST /api/v1/operations/preflight` | 停 `ssh.service`：`allowed=false`，blocker `SERVICE_PROTECTED` |
| `preflight-start-gated.json` | `POST /api/v1/operations/preflight` | 卷离线时启动受管理应用：`allowed=false`，blocker `VOLUME_OFFLINE`（点名 `vol_` 与 `observed_state=unmounted`） |
| `preflight-start-mounted.json` | `POST /api/v1/operations/preflight` | 同一个应用、卷挂回来之后：`allowed=true`（门禁读的是这次实测，不是记下来的结论） |
| `job-appstop.json` | `GET /api/v1/jobs/{id}` | `service.control` stop 的终态：`status=succeeded`、`outcome=verified`、`observed_state.active_state=inactive`、`previous_state=active` |
| `job-appstart.json` | `GET /api/v1/jobs/{id}` | 同一个单元的 start：`observed_state.active_state=active`、`main_pid` 有值 |

## 这些文件里没有秘密

管理员密码在脚本里只经 stdin 走一次，载荷里没有出现；`public_url` 用的是 VM 的
非回环地址 `10.0.2.15`（不是任何真实域名），`target_id` 是容器/socket 标识。
`app-logs-*.json` 的内容是夹具自己 echo 的心跳行。

## 回放断言在哪

`web/src/components/realdata-p09-5.test.ts`：字段集合必须是 openapi `App` 的**子集**、
三个状态分开写、能力来自底层对象（link 没有启停、受保护单元只有日志、看不见的绑定
一个按钮都没有）、入口是服务器地址且 `open` 是锚点不是请求、分组/搜索/收藏的桶规则、
日志按绑定判别、启停请求打的是 `service.control` / `container.control` 而不是一条
不存在的 `app.control`、任务结果里的 `observed_state` 是执行后的真实读数。
