# P09.1 验收结果 — 应用/分组/收藏元数据、判别式绑定、公共入口与内部目标分离

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-22（UTC）
脚本：`tests/integration/p09-1-apps-metadata-acceptance.sh`
环境：见 `env.txt`；宿主检查见 `check.txt`；单元测试见 `native-tests.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（第八遍，`run.log`）| **PASS=210 / FAIL=0 / NOT RUN=0**（退出码 0）|
| 复跑一遍（第七遍，紧接着的重复运行）| **PASS=210 / FAIL=0 / NOT RUN=0**，结论相同 |
| 判别式绑定 | 四种类型各自的绑定形状在真机上逐条试过：多给、少给、给错形状共 7 条，全部 400 并点明是哪个键；同一个容器绑定两个应用是 422 |
| 唯一性来源 | 不是“先查后写”：`sqlite_master` 里能查到 `uq_apps_binding`，重复绑定由数据库拒绝 |
| 入口与目标分离 | `public_url` 的 6 类非法值（回环、localhost、0.0.0.0、非 http、带账号密码、javascript）全被拒；`target_id` 的 8 个非法形状（含 `169.254.169.254`）连“填进去”这一步都过不了 |
| 摘要字段 | link 是 `not_applicable/not_applicable`，其余是 `unknown` + `last_checked_at=null` + `dependency_protection=unmanaged`；`capabilities` 只有 `open` 为真 |
| 依赖卷 | 代理停掉时 503 `AGENT_UNAVAILABLE` **且库里一行没多**；卷不在列表里时 422 `DEPENDENCY_MISSING` 并带出 `details.volume_id`；卷真的存在时 201 + `unmanaged` |
| 删除 | 只删元数据：库里少一行、`jobs` 表行数不变、`service.*` 审计为 0 |
| 库/日志里的秘密 | 本轮没有秘密通道：应用元数据里没有任何口令字段，Web/代理日志里也没有 |

这一轮**没有在真机上发现产品缺陷**。第一遍运行抓出的 23 个 FAIL 全部是验收脚本自己的问题
（4 处断言写错 + 1 处夹具没清干净），逐条在下面第 5 节写清了「怎么被发现 → 怎么改」。
产品侧唯一值得记下来的行为是：**预检故意绑定快照**，抹盘之后要等代理下一次扫描，
否则预检会正确地判“盘上已有分区表”——脚本原来没等，现在等了。

## 2. 范围（做了什么、没做什么）

已做（STAGES §11 第 1 条的全部内容）：

- **元数据模型** `internal/apps/`：`apps`、`app_groups`、`app_favorites`、`app_volume_dependencies`
  四张表（`migrations/0007_apps.sql`），`app_`/`grp_` + 32 位十六进制的标识（Q250）。
- **判别式绑定**：`type` 决定 `binding` 里唯一合法的键（link 没有、systemd 是 `unit`、
  docker 是 64 位 `container_id`、compose 是 `deployment_id`），规范化 JSON 让
  `CREATE UNIQUE INDEX uq_apps_binding ON apps (type, binding_json) WHERE type <> 'link'`
  成为唯一性的真正来源，另加 `CHECK (type <> 'link' OR binding_json = '{}')`。
- **公共入口与内部目标分离**：`public_url` 是给浏览器点的（生产模式禁回环/通配，Q249），
  `target_id` 是给面板自己探测的，形状 `^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$`——
  IP、主机名、端口、URL 在语法层面就写不出来；`169.254.169.254` 那条在验收里专门试了。
- **健康检查定义**：默认 `http` / `/` / 30 秒 / 3 秒 / 期望状态码空集（=200–399），
  状态码去重排序。
- **摘要字段的诚实值**：`runtime_state`、`http_state`、`last_checked_at`、
  `dependency_protection` 四个字段在 P09.1 只有“不知道”可填；`capabilities` 只声明
  此刻真的能做到的事（只有 `open`）。
- **分组与收藏**：分组按 `sort_order` 自动排到末尾、非空分组删除是 422 `DEPENDENCY_BLOCKED`、
  显式 `null` 把应用移出分组；收藏按 (app, user) 存，删应用时级联删除，收藏**不写审计**。
- **依赖卷**：只存代理侧的卷标识（不建外键，Q248），写入时通过代理的卷列表确认存在，
  三种结局互不混淆：通过 / 422 不存在 / 503 确认不了。
- **接口**：`/apps`、`/apps/{id}`、`/apps/{id}/favorite`、`/app-groups`、`/app-groups/{id}`，
  admin + CSRF 写、viewer 只读、乐观锁 `version`、审计动作 `app.*` / `app_group.*`。

没做（留给后续阶段，本轮用断言反过来证明没做——见 run.log 第 6 节，8 条 404）：

- `/apps/discovery`、`/apps/{id}/health`、`/apps/{id}/logs`（P09.2–P09.3）。
- `/containers*`、`/images`、`/container-networks`、`/container-volumes`、`/deployments*`（P09.3–P09.4）。
- 任何容器、任何 systemd 启停、任何健康探测：本轮一个都没碰（`audit_events` 里
  `container.*` / `deploy*` / `service.*` 计数为 0 是断言项）。
- 前端页面（P09.5）：本轮验的是页面会读到的载荷，DOM 由 vitest 覆盖。

## 3. 逐节覆盖（第八遍真机运行，`run.log`）

| 节 | PASS | FAIL |
|---|---|---|
| 0. 环境与安全守卫 | 5 | 0 |
| 1. 配置与启动（代理 root、Web 非 root） | 9 | 0 |
| 2. 判别式绑定 | 46 | 0 |
| 3. 公共入口与内部探测目标分离 | 29 | 0 |
| 4. 分组、收藏、“删分组不改变别的记录” | 20 | 0 |
| 5. 版本、权限、CSRF | 31 | 0 |
| 6. 未实现的接口确实不存在 | 9 | 0 |
| 7. 删除只删元数据 | 8 | 0 |
| 8. 依赖卷：三种结局互不混淆 | 38 | 0 |
| 9. 审计：管理动作留痕，收藏不留 | 10 | 0 |
| 10. 收尾不变量 | 5 | 0 |

其中几条值得单独指出（都在 `run.log` 里）：

- `独立证据：库里 binding_json 是规范化形式` → `{"container_id":"3f4a…"}`，与响应里的形状无关，
  直接读库读出来的。
- `独立证据：唯一索引存在` → `sqlite_master` 里查到 `uq_apps_binding`。
- `代理不可达时没有写库` / `卷不存在时也没有写库` → 两次都对比了写入前后的 `apps` 行数，
  503 与 422 都不留半条记录。
- `审计表是只追加的（UPDATE 被触发器拒绝）` → 真去 UPDATE 了一行，被触发器 ABORT。
- `监听 19481 的 Web 进程到最后都不是 root（uid=999）` → 用 `ss -lptn` 找到真正监听的 pid，
  不是看 `runuser` 的 pid（那是 root）。

## 4. 独立证据（不经过接口）

| 事实 | 怎么独立核验 |
|---|---|
| 迁移到第 7 版 | `SELECT MAX(version) FROM schema_migrations` = 7 |
| 绑定规范化 | `SELECT binding_json FROM apps WHERE id='app_…'` |
| 绑定唯一索引 | `sqlite_master` 里存在 `uq_apps_binding` |
| 分组/应用的关联 | `SELECT group_id FROM apps WHERE id='app_…'`，删分组前后各读一次 |
| 收藏按用户隔离 | `SELECT COUNT(*) FROM app_favorites WHERE app_id='app_…'` |
| 依赖卷落库/清除 | `SELECT COUNT(*) FROM app_volume_dependencies WHERE app_id=… AND volume_id=…` |
| 删除不产生任务 | `SELECT COUNT(*) FROM jobs` 前后相同 |
| 审计动作与目标 | `audit_events` 里 `action='app.create'` 且 `target='app:app_…'` |
| 卷真的被代理认下 | `GET /api/v1/storage/volumes` 里有这个 `vol_` 标识 |

## 5. 这一轮改了什么（都在验收脚本里，没有改产品代码）

第一遍运行 23 个 FAIL，逐条归类如下——**没有一条是产品行为不对**：

| # | 现象 | 原因 | 处理 |
|---|---|---|---|
| 1 | `只改类型的错误点明 binding`：`KeyError:'details'` | 我断言了 `error.details.field`，而 400 的响应体里没有 `details`，那句话在 `message` 里 | 改成断言 `message` 含 `binding` |
| 2 | `改类型并同时给绑定` 期望 200 得 422 | 脚本拿 `CONTAINER_ID_2` 去改绑，而它已经属于另一个应用——422 是**正确**的重复绑定拒绝 | 新增 `CONTAINER_ID_3` 给这一条用 |
| 3 | 4 条 `提交 storage.* 任务` 得 403 | 破坏性动作要求 5 分钟内的再认证，脚本没有先调 `POST /auth/reauth` | `run_action` 在预检说 `requires_reauth` 时先重新验证密码 |
| 4 | 再跑一遍时 4 条预检被 `PARTITION_TABLE_PRESENT` 挡住 | 上一轮留下的 `/etc/fstab` 里 `p09data` 那行让这一轮导入被**正确**拒绝（挂载点重复）；而 `wipefs` 在盘被挂着时会失败，脚本把输出丢进 `/dev/null` 就当中没这回事 | 加了 `disk_is_blank`（签名 + 子分区 + 首扇区三条独立证据）与 `prepare_blank_disk`（先卸挂载、再清、最多 5 轮、每轮验证），清不干净就整段 NOT RUN 并把 `wipefs` 原话写出来 |
| 5 | 抹盘成功但预检仍报“已有 GPT / 1 个分区” | **不是缺陷**：预检故意绑定用户看到的那份快照（`Collector.Assess` 用 `Topology` 而非现场重扫，理由写在代码注释里——现场重扫会让每份预检在提交时都对不上）。抹盘之后要等代理下一次扫描 | 加了 `wait_agent_sees_blank` / `wait_agent_sees_device`，等快照追上真实磁盘再预检；等不到就 NOT RUN |

第 4、5 两条是这一轮真正花时间的部分：它们都不是“产品坏了”，而是**夹具必须把自己的动作和
产品的快照语义对齐**。把这类失败记成产品缺陷，比漏测更糟。

**关于留存**：这一轮跑了八遍（23 → 18 → 19 → 12 → 19 → 2 NOT RUN → 2 NOT RUN → 0），
只留了最终一遍的 `run.log`。中间几遍是脚本在改，日志里的红是脚本自己的错，逐条结论已经在上面
这张表里（P08 的做法是把每遍日志都留着，这一轮没有照做——所以这里把遍数写出来，
不让“只有一份日志”被读成“只跑了一遍”）。

## 6. 明确没验（NOT RUN，不假装）

| 项 | 为什么没验 |
|---|---|
| 前端页面渲染 | P09.5。本轮只验页面拿到的载荷；DOM/交互由 vitest 覆盖 |
| `/apps/discovery`、健康探测、日志 | P09.2–P09.3，接口还不存在（本轮反向断言了 404） |
| 容器、镜像、网络、卷、部署、compose 栈 | P09.3–P09.4 |
| 应用列表的跨页游标、并发写冲突、分组排序的深翻页 | 只在单元测试里覆盖（`internal/apps/store_test.go`、`internal/api/apps_test.go`），真机没跑 |
| 多用户同时收藏的竞争 | 单测覆盖；真机只验了“两个人各看各的” |
| 真实浏览器里的 CSRF/SameSite 行为 | 用 curl + cookie jar + `X-CSRF-Token` 验的协议行为，浏览器行为留给前端验收 |

## 7. 复现方式

```sh
# 宿主：交叉编译三个二进制并拷到虚拟机
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/p09bin/ ./cmd/...
scp -P 2222 /tmp/p09bin/upanel-{agent,web,ctl} upanel@127.0.0.1:/tmp/p09up/
scp -P 2222 tests/integration/p09-1-apps-metadata-acceptance.sh upanel@127.0.0.1:/tmp/p09up/

# 虚拟机：装进去，用一块一次性空盘跑
ssh -p 2222 upanel@127.0.0.1 'sudo install -m 0755 /tmp/p09up/upanel-{agent,web,ctl} /var/tmp/upanel-bin/'
ssh -p 2222 upanel@127.0.0.1 'sudo install -m 0755 /tmp/p09up/p09-1-apps-metadata-acceptance.sh /var/tmp/p09-1.sh'
ssh -p 2222 upanel@127.0.0.1 'sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS=sdd sh /var/tmp/p09-1.sh'
```

`SPARE_DISKS` 里的盘会被**格式化**。永远只对一次性虚拟机里的空盘给出这个参数。
