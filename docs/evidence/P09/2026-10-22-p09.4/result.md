# P09.4 验收结果 — 应用关联 systemd/容器的状态、日志、启停与依赖卷保护

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-22（UTC，虚拟机时钟）
脚本：`tests/integration/p09-4-app-runtime-acceptance.sh`
环境：见 `env.txt`；宿主检查见 `check.txt`；单元测试见 `native-tests.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（`run.log`） | **PASS=165 / FAIL=0 / NOT RUN=0**（退出码 0） |
| 真机发现的产品缺陷 | **0 条**（本轮产品代码在真机上一次通过；10 处失败全是脚本自己的毛病，见 §5） |
| 摘要来自观测 | link 是 `not_applicable`；running 的单元/容器是 `running`；**绑在不存在的单元上是 `unknown` 而不是 `stopped`**；代理停掉后 docker 应用变 `unknown` + 「这一轮没读到」 |
| 能力来自底层对象 | 运行中的单元只有 stop/restart（没有 start）；受保护单元（`ssh.service`）三项全 false 但**仍能读日志**；容器重建后旧绑定连 `logs` 都没有 |
| 容器 running + HTTP 500 | `runtime_state=running` 且 `http_state=error` 且 `status_code=500` 三者同时出现——在跑、不健康、说得出是哪个码 |
| 启动门禁 | 未挂载的依赖卷 → 预检 `allowed=false` + `VOLUME_OFFLINE`，提交 409 `DEPENDENCY_BLOCKED`，**容器真的没被启动**（`docker inspect` 现场对照仍是 `exited`） |
| 门禁只收紧不扩大 | stop 不被拦（同一台机器、同一个未挂载的卷）；没登记成应用的容器不被拦；卷挂上之后同一份 start 预检 `allowed=true` |
| 依赖保护 | `managed` 只留给「docker + `restart` 是 no/空 + 有依赖卷」这一种；`restart=always` 与 systemd 一律 `unmanaged`，并在 note 里点名是什么绕开了面板 |
| 日志 | `/apps/:id/logs` 按绑定判别 journal / docker，两种 `page` 形状各自与 `/logs`、`/containers/:id/logs` 完全一致；链接 422、compose 501、viewer 403、匿名 401 |
| 机器还原 | 收尾用产品自己的 `storage.volume.forget` 退出管理：fstab 里不再有那个 UUID，卷列表里也不再有它 |

这一轮**没有**改松任何断言，也**没有**为了让脚本过而改产品：门禁、能力、保护判定都是按
CONTRACTS §8.4 写的，真机跑出来就是那份契约说的样子。

## 2. 范围（做了什么、没做什么）

已做（STAGES §11 第 4 条的全整内容）：

- **`internal/appruntime`（新）**：把一个应用的三件事合成一次判断。
  - 状态：systemd `active/reloading → running`、`failed → failed`、`activating/deactivating → unknown`、
    单元不在列表 → `unknown`；docker `running → running`、`stopped → stopped`、其余 → `unknown`。
  - 能力：**从对象自己的能力集合推导**（`services.Unit.Capabilities` / `containers.Container.Capabilities`），
    不按“它是个 systemd 服务”猜。运行中的单元没有 `start`，受保护单元没有 `stop/restart`。
  - 保护：`managed` 只给「docker + `restart_policy ∈ {"", "no"}` + 有依赖卷」；systemd 一律
    `unmanaged`（P09.4 不改写单元文件，没有写 `RequiresMountsFor`/`BindsTo`）；compose `unmanaged`；
    没有依赖卷或不是运行时类型 → `not_applicable`。读不到底层事实时降级成 `unmanaged`。
- **`internal/appruntime/gate.go`（新）**：启动门禁。只对 `service.control` / `container.control`
  的 `start`、`restart` 生效；`stop` 永不拦；没登记成应用的容器不拦；卷状态 `mounted` 放行、
  `mismatch` → `VOLUME_IDENTITY_CHANGED`、其余 → `VOLUME_OFFLINE`/`VOLUME_MISSING`、
  读不到卷清单 → `VOLUME_CHECK_UNAVAILABLE`（fail closed）。门禁**只能收紧**：它跑在
  `operations.CreatePreflight` 已有的判决之后，只会把 `allowed` 从 true 改成 false。
- **`internal/api/applogs.go`（新）**：`GET /api/v1/apps/{id}/logs`。对象来自绑定，不是来自请求：
  journal 参数与 docker 参数各认各的，`unit` / `source` 这类“想改对象”的参数一律 400。
- **`internal/apps/target.go`（新）**：`TargetKind`/`TargetID` 把绑定折成“该看谁”，
  appruntime、applogs、gate 三处共用一份判别，避免三份各说各话。
- **`internal/apps`**：`Capabilities` 一段删掉（能力不再由应用类型推导），改成由 appruntime 现算。
- **`internal/api/apps.go`**：DTO 增两个可空字段 `runtime_note` / `dependency_protection_note`；
  `runtime_state` / `dependency_protection` / `capabilities` 改成现算。
- **`internal/operations/service.go`**：预检里接门禁（`Gate` 是一个接口，nil 时行为不变）。
- 契约与文档：CONTRACTS §8.4（8 段）、openapi（`/apps/{id}/logs` + 两个日志 schema + 三处字段说明）、
  DECISIONS Q273–Q276。

没做（属后续子任务或明确推迟）：

- 应用页面前端（P09.5）。
- compose 部署生命周期（P10 起）。
- **运行时卷失联自动停止**：需要代理侧常驻协调者（不在本轮范围）。现在的真实行为写在
  CONTRACTS §8.4(7)：门禁拦的是「经面板发起的启动」，已经跑着的容器不会被卷掉线自动停。
  这一条**没有拿门禁冒充**（Q276）。
- docker 的 `failed` 状态：DTO 里没有 ExitCode，这一版不区分，容器非 running 一律 `stopped`
  （Q275）。`state_text` 仍可读。

## 3. 逐节断言（`run.log`）

| 节 | 内容 | PASS |
|---|---|---|
| 0 | 环境与安全守卫（root、Web 身份无 docker 组、python3、夹具镜像、两个端口空闲） | 7 |
| 1 | 配置与启动（代理 root 建 socket、Web 监听进程 uid=999、迁移第 8 版、登录 + CSRF） | 9 |
| 2 | 夹具：6 个容器（ok / 500 / gated / always / rebuild / compose）、1 个自有 systemd 单元、1 个走产品登记路径造出来的受管理卷 | 18 |
| 3 | 摘要是观测结果：link / systemd(running) / 受保护单元 / 不存在的单元 / docker(running) / docker(stopped) | 32 |
| 4 | 容器 running 但 HTTP 500：发现结果 → 探测 → `error` + `500` + `running` 三者并存 | 8 |
| 5 | 依赖保护：`not_applicable` / `managed` / `unmanaged`（restart=always）/ `unmanaged`（systemd） | 12 |
| 6 | 启动门禁：拦 start、放 stop、不碰未绑定容器、正对照（挂上就放行） | 24 |
| 7 | 容器重建：ID 变了 → `unknown` + 要求重新确认绑定 + 名字相同也不自动接手 | 8 |
| 8 | `/apps/:id/logs`：两种来源的判别、12 种参数拒绝、link 422、compose 501、权限 | 35 |
| 9 | 代理停掉：`/apps` 仍 200、docker 应用 `unknown` + 「没读到」、protection 降级、日志 503 | 7 |
| 10 | 收尾：夹具卷退出管理（fstab 与卷列表双向读回） | 5 |
| — | 合计 | **165** |

节 8 里 viewer/匿名那 5 条是同一次 `/apps/:id/logs` 的权限面：viewer 403、匿名 401，
但**摘要本身** viewer 能看（读状态不是管理动作）。

## 4. 这一轮真机验证到的几处“容易写错”的地方（都在 `run.log` 里能指到行）

1. **`unknown` 不是 `stopped`**（§3）。绑定一个不存在的单元，最省事的写法是回 `stopped`
   （“没在跑嘛”），但那会让页面给用户一个没有意义的「启动」按钮，点下去又一定失败。
   真机上断言的就是 `unknown` + note 要求重新确认绑定。
2. **受保护单元仍有日志能力**（§3、§8）。`ssh.service` 的三项生命周期能力全 false，
   `logs` 却是 true——读日志不是写操作。把「受保护」直接翻成「什么都不能做」是错的。
3. **门禁不放行 `stop`**（§6）。卷掉线的时候最想做的就是停机，这时候拦 stop 只会把人逼到
   命令行去（绕过面板也就绕过了审计）。三条断言（stop 不拦、未绑定不拦、挂着就放行）
   把门禁的边界钉死在「只拦经面板的启动」。
4. **容器重建后不给日志能力**（§7）。库里的绑定指向一个已经不存在的 ID，此时给出 `logs`
   只会得到一个 404；能力表里就不该有。
5. **代理停掉时 `/apps` 仍然 200**（§9）。列表页是首页，代理挂掉不该让首页整个 503；
   每个应用自己说 `unknown` + 「这一轮没读到」，同时 protection 降级成 `unmanaged`
   （判断不了就不能声称“接管了依赖”）。

## 5. 验收脚本自身的缺陷（10 类，与本轮产品代码无关）

| # | 现象 | 根因 | 修法 |
|---|---|---|---|
| 1 | 每个应用接口都 404「应用不存在」 | `create_app` 在 `$(...)` 里跑，子壳里设的变量出不来，`LAST_APP` 一直是空 | 标识写进 `$WORK/lastapp`，调用点用 `app_id` 读；文件是子壳唯一诚实的交接方式 |
| 2 | 「运行中的单元出现 stop 能力」失败 | 拿 `ssh.service` 当夹具，而它是受保护单元，能力按契约就是空的 | 脚本自己建 `upn-p094-svc.service`；受保护单元另开一组断言（三项全 false + 日志 true） |
| 3 | 卷发现拿不到任何卷 | `/storage/volumes` 是 `{"data":{"volumes":[…]}}`，脚本读的是 `data.items` | 改读 `data.volumes` |
| 4 | 断言 `pre_` 前缀失败 | 预检标识前缀是 `pf_` | 改断言 |
| 5 | 「登记一个受管理卷」整段 NOT RUN | 一台刚装好的机器上没有受管理卷；脚本原来指望机器上“已经有” | 改成走产品自己的 `storage.volume.import` 造夹具，收尾 `forget` 还原（顺带把 §6 的正对照也做成了真的） |
| 6 | 5 条 systemd 断言读到「绑定的单元不在本机服务列表里」 | 服务列表是采集成快照之后缓存的，脚本 `enable --now` 完 1 秒内就建应用，读的是旧快照 | 加 `wait_unit`：轮询 `/api/v1/services` 直到单元出现再建应用。这不是产品的 bug——脚本不能拿旧快照断言新事实 |
| 7 | 卷夹具整段 NOT RUN，`pick_volume_device` 返回空 | `lsblk -lnpr`：util-linux 2.39 里 `-r`（raw）与 `-l`（list）**互斥**，命令直接退出 1，拿到空列表 | 改成 `lsblk -l -n -p -o …`（不用 `-r`）；这条坑也写进了 `env.txt` |
| 8 | 两次 PATCH 应用都 409 | `version` 是**乐观锁的当前版本**，脚本给的是“改完之后的版本号”2，库里是 1 | 加 `app_version`/`apppatch`：先读当前 version 再提交 |
| 9 | 4 条断言报 `SyntaxError: invalid syntax (<jget>)` | `pfget` 是把 `"$2"` 拼在 `d["data"]` 后面，只能给 `["allowed"]` 这种后缀；表达式 `any(...)` 拼出来是语法错误 | 新增 `pfexpr`（在整份载荷上求值），表达式断言走它 |
| 10 | 「卷挂着时 protection 仍是 managed」失败 | 依赖 #8：PATCH 没成功，应用根本没带上依赖卷 | 随 #8 一起修好 |

## 6. NOT RUN（0 条）

最终一遍 **NOT RUN=0**：容器、systemd 单元、受管理卷三类夹具都在真机上造出来了。

脚本头部另有三条**声明式**不覆盖（不是执行后才发现）：

- 浏览器渲染（VM 无浏览器，页面级证据在 P09.5 之后由宿主回放给出）。
- systemd/docker 控制的**执行**本身（`service.control` / `container.control` 的执行通道在
  P05.2 / P09.3 已经验过；本轮验的是“门禁有没有拦住它”，以及拦住之后容器**真的没动**）。
- 运行时卷失联自动停止（不在 P09.4 范围，见 §2）。

## 7. 已实现 / 已验证 / 未验证 / 受阻

**已实现（本轮新增）**

- `internal/appruntime/appruntime.go`（355 行）：状态/能力/保护三合一解析。
- `internal/appruntime/gate.go`（183 行）：启动门禁，只收紧。
- `internal/api/applogs.go`（156 行）：`GET /apps/{id}/logs`。
- `internal/apps/target.go`（41 行）：绑定 → 目标的一处判别。
- 改：`internal/apps/apps.go`（删掉按类型推能力）、`internal/api/apps.go`（+80/−?）、
  `internal/operations/service.go`（+69 接门禁）、`internal/api/router.go`、`internal/api/server.go`、
  `cmd/upanel-web/main.go`。

**已验证（真机 + 单测）**

- 上面 11 节 165 条断言，`FAIL=0 / NOT RUN=0`。
- `go test -count=1 ./...` 全绿（`native-tests.txt`），含新增
  `internal/appruntime/appruntime_test.go`（481 行）与 `internal/api/applogs_test.go`（371 行）。
- 宿主 `scripts/check.sh`：**PASS 13 / FAIL 0 / NOT RUN 0**（`check.txt`；openapi lint 7 warning
  与 P09.3 相同，全部是既有的 no-unused-components / operation-4xx-response）。
- 机器还原：`env.txt` 是在**验收跑完之后**采集的，fstab 受管理块与卷列表里都没有夹具卷。

**未验证**

- 前端页面（P09.5）。
- 浏览器里“打开服务器地址”的那条入口（需要页面；接口侧 `public_url` 与 `health_check.target_id`
  分开这件事在 P09.2 已验）。
- 运行时卷失联自动停止（未实现，见 §2）。

**受阻**：无。

## 8. 已知限制

- **`managed` 不等于「依赖被接管了」**：它只说「经面板发起的启动会被门禁检查」。
  手工 `docker start`、开机自启、`systemctl enable` 过的单元、`restart: always` 这四条路都
  绕过面板，所以这几种情况一律标 `unmanaged` 并在 note 里点名是谁绕开了（Q274）。
- **docker 的 `failed` 这一版不区分**（Q275）：DTO 里没有 ExitCode，容器非 running 就报
  `stopped`，退出码写在 `state_text` 里给人看。
- **门禁只拦启动，不拦已经跑着的**：卷在运行期掉线不会自动停机（Q276）。这是本轮明确的
  范围边界，不是漏做。

## 9. 下一子任务

**P09.5**（STAGES §11 第 5 条）：应用卡片、搜索、分组、一键打开、状态区分和常用入口首页集成。
接口侧已经全部就位（摘要里的 `runtime_state`/`runtime_note`/`http_state`/`capabilities`/
`dependency_protection`、`/apps/{id}/logs`、分组与收藏），页面只做消费。
P09 的四条验收线里，「入口在远程浏览器打开服务器地址」与「卡片上没有启停按钮」要到 P09.5
才算验完——本轮验的是它们背后的接口判决（`public_url` 与 `target_id` 分开、能力为空就不出按钮）。
