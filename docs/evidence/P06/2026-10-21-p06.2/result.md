# P06.2 验收结果 — 流式暂存上传、提交、编辑备份、目录创建、复制/移动/删除、权限/所有者受限修改、版本冲突与配额

- 阶段：P06（文件管理与受限终端）/ 子任务 P06.2
- 日期：2026-10-21
- 结论：**P06.2 passed**。Ubuntu 24.04.5 真机断言 **PASS=228 / FAIL=0 / NOT RUN=0**，
  VM 内核原生套件 **PASS=909 / FAIL=0**，宿主 `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**，
  前端 **428** 用例全过，`api/openapi.yaml` valid（6 warning）。
- 写入第一次真正进入生产登记表：`file.mkdir`、`file.write_text`、`file.copy`、`file.move`、
  `file.delete`、`file.chmod`、`file.chown` 七个动作，加上单次许可的上传三件套
  （`files.upload.declare|write|commit`，`commit_upload` 才是第八个动作）。
  全部走“预检 → 任务 → 幂等 → 资源锁 → 结果读回 → 审计”同一条链路，**没有任何一条旁路**。

---

## 1. 本轮做了什么

`STAGES.md` §8 P06.2：“流式暂存上传、提交、编辑备份、目录创建、复制/移动/删除、权限/所有者受限修改、版本冲突与配额。”

### 1.1 接口

| 接口 | 作用 | 权限 |
|---|---|---|
| `POST /api/v1/files/uploads` | 声明一次上传（root_id、目标路径、期望大小、期望目标版本），返回 `upload_id` 与窗口截止时间 | admin |
| `PUT /api/v1/files/uploads/{id}/content` | 流式写入字节（`Content-Length` 必须等于声明大小，可续传偏移） | admin |
| `DELETE /api/v1/files/uploads/{id}` | 放弃上传，立刻归还预留 | admin |
| `GET /api/v1/files/uploads/{id}` | 查已收字节与状态 | admin |
| `GET /api/v1/files/roots` 等 P06.1 五条 | 不变 | admin |

写动作本身**没有新 HTTP 路由**——它们从 `POST /operations/preflight` → `POST /jobs` 走，
和 P05.1/P05.2 的服务启停、进程信号完全同一条链路。这是刻意的：一条写路径只有一套权限检查、
一套幂等、一套审计。

### 1.2 暂存语义（`internal/files/staging.go`）

- 暂存区是每个可写根下的 `.upanel/staging/<sha256(rel)[:8]>/<UnixNano>.part`，
  `.upanel` 在列举里不可见（Q147 + 本轮 Q157 修复），路径层也拒绝进入。
- **声明即预留**：声明时按 `SizeBytes` 记进 `reserved[rootID]`，与容量读取器给出的
  `available` 比较，放不下就 `INSUFFICIENT_SPACE` 阻塞（预检阶段，不是写的时候才 500）。
- **字节只落在暂存区**：`PUT` 期间目标路径下的文件一个字节都不动，`GET /files/text` 读到的
  仍是旧内容、旧版本号（验收 §3 专门断言这一点）。
- **收满即释放**：`received` 那一刻把预留还回，之后它不再占额度。
- **拿不到容量读取器不等于不记账**（Q158）：`reserved` 照样累计，只是跳过比较。
- **任何删掉暂存记录的地方都归还**（Q158）：`dropUpload` 是唯一出口——
  放弃（`DELETE`）、过期清理（`SweepStaging`）、写入失败三处都走它。
- 期限 `UploadTTL` 24 小时；后台每 10 分钟扫一次，过期即删并归还。

### 1.3 提交与版本冲突

`commit_upload` 是第八个动作，预检阶段做四件事：

1. 目标版本必须仍等于声明时的 `expected_version`（`null` 表示“当时不存在”）——
   外部改过就 `VERSION_CONFLICT`。
2. 目标若是目录或非常规文件 → `TARGET_NOT_REGULAR` / `TARGET_IS_DIRECTORY`。
3. 目标字节必须仍是声明时算出的那份（大小 + 修改时间快照）——验收 §6 落的就是这条。
4. 父目录必须存在且可写（`PARENT_MISSING`）。

提交时用 `renameat2` 从暂存区换进目标，**同一文件系统内一次原子替换**；
目标原有内容在覆盖前先备份（见 1.4）。落位后用**同句柄** `fstat` 读回：
inode、大小、`nlink==1`、`dev` 与根相同、内容摘要，进 `observed_state`。

### 1.4 编辑备份（`internal/files/write.go`）

- 覆盖写（`file.write_text` 与 `commit_upload` 覆盖已存在文件）之前，把旧内容复制进
  `<root>/.upanel/backups/<sha256(rel)[:8]>/<UnixNano>`，文件名单调递增。
- 每个文件保留最近 **10** 份（`MaxEditBackups`），总字节上限 **500 MiB**（`MaxEditBackupBytes`），
  超出按时间从旧往新删。
- 备份**失败不阻止写入**，但会在结果里记一条 warning——用户能看到“这次改了但没留底”。
  反过来，备份成功而写入失败时备份保留（不删，宁可多一份）。
- `backup_path` 只在真的产生了备份时才出现在结果里。

### 1.5 目录、复制、移动、删除

- `file.mkdir`：单层（不带 `-p` 语义），父目录缺失 → `PARENT_MISSING`；
  已存在 → `TARGET_EXISTS`；权限 0755 或显式指定。
- `file.copy`：`StatTree` 先数条目（`TreeEntries`/`TreeFiles`/`TreeDirs`/`TreeSymlinks`/`TreeBytes`），
  超 50 000 条 → `TREE_TOO_LARGE`；树里**有符号链接**（任何位置）→ `TREE_UNSUPPORTED`，
  不复制一半。复制先在暂存区拼完整棵树再换位，中途失败不留半截目录。
- `file.move`：同根内 rename；跨根走 copy + delete 并在结果里标明两步。
- `file.delete`：单文件直接删；目录必须显式递归（预检给 `RECURSIVE_DELETE` 警告，
  确认短语是**带目标路径**的那一句）。读回确认路径确实消失。
- 条目数：`file.delete` 的 `Entries` 是 `TreeEntries + 1`（含目录自身），
  验收 §9 的现场是 `docs/a.txt` + `docs/nested/` + `docs/nested/b.txt` → 4。

### 1.6 权限与所有者（`file.chmod` / `file.chown`）

- 两个动作都**必须再认证**（5 分钟窗口）+ 一次性确认短语。
- `chmod` 只接受三位或四位八进制，拒绝 setuid/setgid/sticky 的“顺带设置”写法
  （要设就显式写，但仍然只对非系统文件开放）。
- `chown` 的名字必须在受管理账号表里（`MANAGED_ACCOUNTS_UNAVAILABLE` 时直接阻塞，
  不去猜 `/etc/passwd`）；uid/gid 由代理按名字解析，Web 从不传数字。
- 两者都拒绝作用在 `.upanel` 与根自身。

### 1.7 前端（`web/src/views/FilesView.vue` 等）

文件页从只读扩成可写，全部走同一套 DTO（`web/src/api/files.ts` 与 `api/openapi.yaml` 同源）：

- 上传：选择文件 → 声明 → 分片 `PUT`（带进度、可取消）→ 走预检/提交；
  目标已变时给出“文件已被别人改过”的 VERSION_CONFLICT 提示而不是“上传失败”。
- 新建目录、重命名/移动、复制、删除（递归删除要点确认框）、编辑文本（带备份提示）、改权限/改所有者。
- 状态齐全：加载中、空目录、无权限（viewer 看不到任何写按钮，而不是点了才 403）、
  错误（阻塞项逐条翻译成下一步该做什么）、只读根（`ROOT_READ_ONLY` 时写按钮禁用并说明原因）。
- 确认短语弹窗（`FileActionConfirm.vue`）显示服务端给的原文，逐字匹配才放行，
  过期就整条重来。

### 1.8 代理侧通道

上传的字节不经过 Web 的 job 表：Web 用 `files.upload.*` 三个查询/动作开单次许可，
把上传交给**代理**的 `/internal/v1/uploads`（与 P05.3 日志流同样的“单次许可”模式，
连接建立即作废）。这样 Web 的 `POST /jobs` 表里不会有 2 GiB 的字节流，
Web 进程的内存也不随上传大小增长。

---

## 2. 与 STAGES §8 P06.2 验收条目逐条对应

| 判据 | 实现位置 | 真机证据 | 结论 |
|---|---|---|---|
| 流式暂存上传：字节不直接落到目标 | `internal/files/staging.go`、`internal/api/files_upload.go`、`internal/agentclient/uploads.go` | 验收 §3：声明 → 传 14 字节 → `GET /files/text` 仍是旧内容旧版本 → 提交后才变；`staging` 目录在传输中只有一份 `.part`，提交后清空 | 通过 |
| 声明大小必须被兑现，断流不留垃圾 | 同上 + `dropUpload` | 验收 §4：`Content-Length` 与声明不符 → 400；少传即断 → 上传过期后可清理且 `reserved` 归零；`staging` 计数回到 0 | 通过 |
| 超大上传/配额：放不下在预检就拒 | `reserveSpace` + `capacityFor` | 验收 §4：声明超过剩余空间 → `INSUFFICIENT_SPACE` 阻塞（预检 200 带阻塞项，不是提交时 500）；`reserved` 在多个并发声明间正确累计 | 通过 |
| 暂存期限与清理 | `SweepStaging`（10 分钟一轮，TTL 24 h） | 验收 §4：把上传的 `expires_at` 做旧后一轮扫描即删除、预留归还；见 §5 的“24 小时自然过期未验证” | 通过（清理路径）/ 未验证（自然到期） |
| 提交前的版本冲突检测 | `filesaction.go` 的 `planCheck`/`commit_upload` | 验收 §6：`echo … >> app.conf` 之后提交 → 409 + `error.code=PREFLIGHT_STALE`，且文件内容**没有**被覆盖；作业表里多一条 `failed`/`step=rejected`（Q159） | 通过 |
| 覆盖已存在文件时留编辑备份 | `write.go` 的 `backupFile` | 验收 §5：三次覆盖后该文件自己的 key 目录里 3 份，内容分别是上一版；`backup_count` 如实返回；12 次覆盖只留 10 份（VM 原生 `TestBackupPruneKeepsTen`） | 通过 |
| 目录创建 | `file.mkdir` | 验收 §7：正常创建 → `observed_state.exists=true`、`mode=0755`；已存在 → `TARGET_EXISTS`；父目录缺失 → `PARENT_MISSING`（不是 `TARGET_MISSING`）；嵌套只建一层 | 通过 |
| 复制/移动 | `file.copy` / `file.move` | 验收 §8：文件复制、目录复制（含子目录）、目标已存在 → `TARGET_EXISTS`、树里有符号链接 → `TREE_UNSUPPORTED` 且目标未创建、移动后源消失目标存在 | 通过 |
| 删除（含递归） | `file.delete` | 验收 §9：单文件删除、非空目录不递归 → `DIRECTORY_NOT_EMPTY`、递归删除需要**再认证 + 带路径的短语**、条目数 4、删除后读回 `exists=false` | 通过 |
| 权限受限修改 | `file.chmod` | 验收 §10：`0640` 生效且 `observed_state.mode` 读回；`4755`/非八进制/超四位 → 400；未再认证 → `REAUTH_REQUIRED`；短语不符 → 400 `CONFIRMATION_MISMATCH` | 通过 |
| 所有者受限修改 | `file.chown` | 验收 §10：改为受管理账号生效（`uid`/`gid` 读回一致）；不受管理的名字 → `VALIDATION_ERROR`；账号表不可用 → `MANAGED_ACCOUNTS_UNAVAILABLE` | 通过 |
| 只读根上所有写动作被挡 | `ROOT_READ_ONLY` 阻塞 | 验收 §12：七个动作在只读根上一律 200 预检 + `ROOT_READ_ONLY` 阻塞，没有一条进任务 | 通过 |
| 写动作的路径边界与 P06.1 一致 | `ValidateRelPath` + `openat2` | 验收 §11：`../../etc/shadow`、绝对路径、`.upanel` 段、超长路径在**声明/预检**阶段就被拒（422/400），没有字节被写进暂存区 | 通过 |
| 权限边界（未登录/viewer/admin） | `internal/api` 权限中间件 | 验收 §14：未登录 401、viewer 读得到列表但每个写动作 403 `FORBIDDEN`、admin 全部可达；viewer 的 403 里不含任何写按钮所需的许可 | 通过 |
| 任务中心可见 | P03.3 的 `/jobs` | 验收 §13：写入任务带 `step`、`observed_state`、`result.data`（如 `backup_path`、`entries`）；失败的写入带契约错误码 | 通过 |
| P06.1 只读能力没被破坏 | 回归 | 验收 §15：roots/list/text/download 与 P06.1 行为一致（含符号链接 422、`.upanel` 不可见） | 通过 |

---

## 3. 真机抓出并修掉的缺陷

这一轮的第一次真机跑几乎每一条都失败，而**失败的原因不是脚本**——是产品。全文记录在
`docs/DECISIONS.md` Q155—Q158。

### 3.1 `O_PATH` 的 fd 上做 `getdents64`（EBADF）——第二次犯（Q155）

`StatTree`、`walkTree`、以及备份目录的 `pruneBackups` 都用 `O_PATH|O_DIRECTORY` 打开目录，
然后在同一个 fd 上调 `readNames`。`O_PATH` 的 fd 没有目录流，`getdents64` 一律 `EBADF`。
后果不是一个接口坏了，是**三个功能同时坏**：

- 目录删除的预检永远 `PATH_REJECTED`（统计不出条目数）；
- 目录复制预检直接 500；
- `pruneBackups` 每次都失败，而失败只写一条 warning —— 于是 11 次覆盖留下 **12 份备份**、
  `backup_count` 恒为 **0**。这条最危险：功能看起来正常，只是磁盘在慢慢涨。

修复：`StatTree` 用 `openRead|O_DIRECTORY`，`walkTree` 的子目录用
`O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC`，`backupFile`/`openBackupKey` 一律 `O_RDONLY`，
三处都在常量旁写明原因。配了会真的走进子目录的用例：
`TestStatTreeCountsDirectories`（1 文件 + 1 子目录 + 1 嵌套文件 + 1 链接 → entry 4 / 文件 2 /
子目录 1 / 链接 1 / 字节 3）与 `TestBackupPruneKeepsTen`（12 次覆盖 → 10 份，最新是 v11）。

### 3.2 `PARENT_MISSING` 在真实 files 层不可达（Q156）

契约里有这个阻塞码，预检也会在目标不存在时核对父目录。但目标路径里有**一级**父目录缺失时，
`openat2` 在 `openParent` 就撞 ENOENT —— 返回的是**错误**，不是 `Exists=false` 的路径状态；
`blockerFor(ErrNotFound)` 把它翻成 `TARGET_MISSING`，`parentNotices` 根本没机会跑。
真机上 `ghost/child` 报的是 `TARGET_MISSING`，用户看到的是“目标不存在”，
而他真正需要知道的是“父目录不存在，先建目录”。

修复：`planCheck`/`commit_upload` 的 Stat 错误分支里 `errors.Is(err, files.ErrNotFound)` 时
改走 `missingNotice`（内部再核对父目录）；`parentNotices` 自己遇到父目录 `ErrNotFound`
（两级以上都不存在）也归到 `PARENT_MISSING`。加了三条会走到真实分支的用例
（`ghost/child`、`ghost`、`deep/ghost/child`），并给 `fakeWriter` 加了按路径注入的 Stat 错误槽。

### 3.3 列举里看得见 `.upanel`（Q157）

Q147 定了保留目录不出现，但 `List` 只做了路径层拒绝，名字表照原样返回。
界面于是多出一个**既列不出内容、又删不掉**的条目，而它恰恰是面板自己的内部目录。
修复：`List` 排序后 `dropReservedNames`。不动读写的拒绝（那条由形状层负责）。

### 3.4 预留计数在两种配置下不成立（Q158）

- `reserveSpace` 在 `m.caps == nil` 时**直接 return**，于是没配容量读取器的机器上
  `ReservedBytes` 恒为 0，并发上传看到的可用空间比实际大。
- 释放在 `DeleteUpload` 与 `WriteUploadContent`（过期那条）各写一遍，而 `SweepStaging`
  走 `dropUpload` **不释放** —— 过期上传的预留永不归还，盘上没数据、账上一直占着。

修复：① `reserveSpace` 无论有没有容量读取器都记账，`caps == nil`/读不到容量只跳过**比较**；
② 释放在 `dropUpload` 里统一做（`received` 那一档跳过），删掉两个调用方的重复释放。
`TestUploadLifecycleHappyPath` 与 `TestUploadExpirySweep` 就是这两条的照妖镜。

### 3.5 验收方法上的三条教训（已写进 commands.log）

1. `chmod +x /tmp/upanel-bin/upanel-*` **不匹配** `upanelctl` → `init-admin` 退出码 126，
   整份验收全是 401。必须逐个点名。
2. python3 的 `-c` 脚本里取不到 `"$1"`（`jget` 之前那样写），改成 `eval` + `_` 约定，
   异常统一打印成 `ERR:<Type>:<msg>`。
3. `jobs` 的 `created_at` 是**秒级**，同一秒内的顺序由 `id DESC` 决定，
   「按位置取 `jobs[0]`」在同一次跑里不稳定 —— 断言必须按内容匹配（`any(...)`）。

---

## 4. 实际执行的检查与证据

| 检查 | 命令/位置 | 结果 |
|---|---|---|
| Go 门禁（宿主） | `go build/vet ./...`、`gofmt -l` | exit 0 / 无输出 — `go-gates.txt` |
| Go 门禁（linux/amd64） | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build/vet ./...` | exit 0 — `go-gates.txt` |
| Go 单测（宿主） | `go test ./... -count=1` | 25 个包 ok — `go-test.txt` |
| Go 单测（VM 内核原生） | `sh tests/run-native-tests.sh` | **TOTAL PASS=909 FAIL=0** — `native-tests.txt` |
| 前端 | `npm run typecheck / lint / test / build` | 28 文件 / **428** 用例 — `web-test.txt`、`web-gates.txt` |
| 契约 | `npx @redocly/cli lint api/openapi.yaml` | valid，6 warning — `openapi-lint.txt` |
| 全量门禁 | `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** — `check.txt` |
| P06.2 真机验收 | `p06-2-files-write-acceptance.sh`（Ubuntu 24.04.5） | **通过 228 / 失败 0 / 未执行 0** — `p06-2-acceptance-2404.txt` |
| 载荷归档 | 验收 §16 | 42 份真实响应体 + 1 份被上传的二进制（`upload-payload.bin`），共 43 个文件 — `payloads/` |
| 环境事实 | `uname -a`、`os-release`、`nproc`、`free -m`、`id` | — `env.txt` |

---

## 5. 已实现 / 已验证 / 未验证

**已实现**（代码在树上，接口进 `api/openapi.yaml` 与 `docs/CONTRACTS.md`）：
七个写动作 + 上传三件套 + 四条上传接口、暂存区与预留记账、编辑备份与两级上限、
复制/移动/删除的树统计与失败原子性、`chmod`/`chown` 的再认证与短语、
前端全部交互与四态。

**已验证**（真机或 VM 原生测试有断言）：§2 表中的每一条；§3 的四个缺陷修复都有对应用例；
`reserved` 在声明/收满/放弃/过期四种路径上的值；409 语义与“作业留下一条 failed”；
批量覆盖的备份份数与内容。

**未验证（NOT RUN，本环境造不出来或代价过大）**：

1. **浏览器里真实渲染**这套新界面——本轮只做了 `npm run test`（组件级 428 例）与
   `check.sh` 的 web-smoke，没有用 Playwright 对着真机后端点一遍上传/递归删除/改所有者。
   P06.1 的浏览器冒烟不能替代本轮新增的交互。
2. **ENOSPC**（写到一半磁盘真的满）——VM 的 root 是 overlay，塞满会把宿主一起挂住；
   代码路径有 `INSUFFICIENT_SPACE` 的预检与写时 ENOSPC 的错误映射，但**没有真机证据**。
3. **24 小时自然过期**——只把 `expires_at` 做旧后扫一轮，没有真等 24 小时。
4. **500 MiB 备份总量上限**——只验证了 10 份的份数上限；字节上限需要造 500 MiB 备份，
   VM 磁盘不够且会污染其他用例。
5. **50 000 条目树**——`TREE_TOO_LARGE` 的判据来自 `StatTree` 的计数上限，
   没有真造一个 5 万文件的目录（造一次要十几分钟且拖慢后续用例）。
   条目计数正确性由 `TestStatTreeCountsDirectories` 覆盖。
6. **多客户端并发真竞态**——两个进程同时提交同一个文件只靠 `VERSION_CONFLICT` 的
   顺序断言（先提交的成功、后提交的 409），没有做“两边同时 `renameat2`”的赛跑。
   资源锁按锁名排序在所有动作上生效，但“同时”这件事在单机验收里只能近似。
7. **代理侧 `runner` 的再验证步骤**——受理时复核预检的那段代码路径，
   本轮是通过 Web 侧观察到的 409 间接证明的，没有单独断言 `step=revalidate`。
8. **跨文件系统移动**——`/tmp` 与根不同 fs 时的 copy+delete 分支没有真机造跨 fs 现场。

未验证项一律**不计入通过项**；它们不改变 §2 的结论，因为 §2 的每一条都有独立断言。

---

## 6. 已知限制

- 暂存区与备份区都放在**被管理根内部**（`.upanel`），因此它们占用该根的配额，
  且对“根被整个 `rm -rf`”没有保护——那是 P07/P12 的范围。
- 单次上传上限默认 2 GiB（`DefaultMaxUploadBytes`），是配置项；超过要改配置，
  界面上没有分片合并。
- 备份是**整文件复制**，不是写时复制；改一个 400 MiB 的文件就要再写 400 MiB。
- `file.chown` 只接受受管理账号表里的名字，不接受数字 uid，也不接受 `root`
  （那等于提权）。这是刻意的，写进 `docs/CONTRACTS.md`。
- 递归删除的短语绑定了目标路径，路径改了短语就失效——这是特性不是缺陷，
  但界面上必须把路径原样显示，否则用户会莫名其妙地点不过。
- VM 本轮又在收尾前后各崩一次（`console.panic-10`/`console.panic-11`，
  guest 内 `Attempted to kill init! exitcode=0x8b`）。重启后原生套件与验收**重跑**，
  结论与崩前一致；这是验收环境的问题，不是产品的问题，记录在 `env.txt`。
