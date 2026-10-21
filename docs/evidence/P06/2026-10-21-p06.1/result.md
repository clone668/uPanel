# P06.1 验收结果 — 受管理文件根、句柄级路径约束、列表/文本/下载

- 阶段：P06（文件管理与受限终端）/ 子任务 P06.1
- 日期：2026-10-21
- 结论：**P06.1 passed**。真机断言 **PASS=115 / FAIL=0 / NOT RUN=0**，载荷校验 **PASS=6379 / FAIL=0**，
  宿主 `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**，前端 **406** 用例全过。
- 只读轮：本轮**没有**任何写动作（上传、提交、编辑、删除、改名、权限、所有者）进入生产登记表。

---

## 1. 本轮做了什么

`STAGES.md` §8 P06.1：“受管理文件根、句柄级路径约束、列表/文本/下载；建立路径穿越和符号链接测试。”

交付了四条只读接口（全部 admin）：

| 接口 | 作用 | 代理侧 |
|---|---|---|
| `GET /api/v1/files/roots` | 列出**配置里登记**的文件根及其容量 | 查询 `files.roots` |
| `GET /api/v1/files/list?root_id&path&cursor` | 列目录（keyset 分页，页 200） | 查询 `files.list` |
| `GET /api/v1/files/text?root_id&path&expected_version` | 读文本（≤ 1 MiB、必须合法 UTF-8） | 查询 `files.text` |
| `GET /api/v1/files/download?root_id&path&expected_version` | 原始字节流 | 流 `files.download`（单次许可，首行 JSON 头 + 原始字节） |

“边界”不是靠字符串比较做出来的：

1. **根身份**在登记时记下 `dev/ino + 挂载点 + fstype`，**每次请求重新核对**；根被替换 → `ROOT_UNAVAILABLE`（409）。
2. **路径解析只用一次 `openat2`**（`RESOLVE_BENEATH|RESOLVE_NO_MAGICLINKS|RESOLVE_NO_XDEV|RESOLVE_NO_SYMLINKS` + `O_NOFOLLOW`），
   不存在“先检查再打开”的时间窗。
3. **判据取自已打开的句柄**：对同一个 fd `fstat`，核对普通文件、`nlink==1`、`dev` 与根相同、大小上限；
   版本号也取自这次 stat（不是另一次 `stat()`）。
4. **配置层禁止 `/`**：`agent.files.roots[i].path` 为 `/` 时 `upanel-agent` 以退出码 2 拒绝启动，
   所以“能管理什么”在数据上就只有一份根列表——界面不可能变成“可管理整个服务器”。

逐文件改动见 `docs/PROGRESS.md` §5「P06.1」，此处不重复。

---

## 2. 与 STAGES §8 P06.1 验收条目逐条对应

| 判据 | 实现位置 | 真机证据 | 结论 |
|---|---|---|---|
| 受管理文件根：根只来自配置 | `internal/files/manager_linux.go`（登记 + 每次核对）、`internal/api/files.go`（`root_id` 必填） | 验收 §3：`files.roots` 只回登记的根；未登记 `root_id` → 404 `NOT_FOUND`；响应里没有“任意绝对路径”这样的入参 | 通过 |
| 配置里不能把 `/` 登记成根 | `internal/config/config.go` | 验收 §2 / §13 载荷：`配置错误: agent.files.roots[0].path 不能是根目录 '/': 那等于把整个服务器文件系统登记成一个文件根；请登记具体目录`，代理退出码 2 | 通过 |
| 句柄级路径约束（逃逸靠内核判，不靠字符串前缀） | `internal/files/manager_linux.go`（单次 `openat2` + 同句柄 `fstat`） | 验收 §4/§5/§8/§9：见下四行 | 通过 |
| `../../etc/shadow`、`/etc/shadow` 等**形状**路径被拒 | `internal/files/types.go`(`ValidateRelPath`) | 验收 §4 的九条形状（`../../etc/shadow`、绝对路径、中段 `..`、`a//b`、`.` 段、超长路径、文本接口空路径）+ 宿主 `files.test` 的 `TestValidateRelPathRejects` 十个形状（含 NUL、结尾斜杠、单段 256 字节、超过 PATH_MAX、非法 UTF-8）：越界类 422 `PATH_OUTSIDE_ROOT`、形状类 400 `VALIDATION_ERROR`，**没有一条 200** | 通过 |
| 符号链接不跟随（含“符号链接替换”） | `openat2` 的 `RESOLVE_NO_SYMLINKS`/`O_NOFOLLOW` + `classifyNotDir` | 验收 §5：根里的三种链接——指向根外文件（`link-secret`）、指向根外目录（`link-dir`）、指向 `/proc/self/fd` 的跳板（`link-magic`）——列举/读文本/下载一律 422；列表里只把条目标成 `symlink`，不报告目标（载荷校验断言“目录项不带符号链接目标字段”） | 通过 |
| magic link 无法逃逸 | `RESOLVE_NO_MAGICLINKS` | 验收 §5：`/proc/self/root/etc/shadow`、根内指向 `/proc/self/fd` 的链接 → 422 `PATH_OUTSIDE_ROOT`，响应里不含根外内容 | 通过 |
| 根内嵌套挂载不跨出去 | `RESOLVE_NO_XDEV` | 验收 §8：根里的 tmpfs 子挂载 → 422 `PATH_OUTSIDE_ROOT` | 通过 |
| 硬链接不读（`nlink>1`） | 同句柄 `fstat` 的 `nlink==1` | 验收 §5：`nlink=2` 的文件读/下载 → 422 `PATH_OUTSIDE_ROOT`（Q143：借用 `PATH_OUTSIDE_ROOT`，新增错误码只有 `ROOT_UNAVAILABLE`、`FILE_NOT_TEXT` 两个） | 通过 |
| 特殊文件不被当普通文件打开，且**不会把代理挂住** | `openRead = O_RDONLY\|O_NONBLOCK` + `fstat` | 验收 §5：FIFO 与目录 → 400 `VALIDATION_ERROR`，**立即返回**（这是 Q137 的真机证据） | 通过 |
| 根被删掉重建（dev/ino 变了） | 每次请求核对根身份 | 验收 §9：`mv` 替换根 → 409 `ROOT_UNAVAILABLE`，且“重建的根没有被当成原来的根（响应里没有任何目录项）”，`roots` 里也标出该根不可用 | 通过 |
| 列表：页大小 / 游标 / 尾页 / 截断 | `internal/files/manager_linux.go`(`List`) | 验收 §7：首页 200 项、`next_cursor` 是末项名字；250 个文件翻两页无重复无跳过；尾页 `next_cursor is None`（**不是空串**）；50 010 项目录 `truncated=true`（如实标记，不静默截断） | 通过 |
| 文本：内容、大小、版本自洽 | `internal/files/manager_linux.go`(`Text`) | 验收 §6：`size_bytes` 是字符串 `"27"` 且 content 恰好 27 字节、写完版本后带**同一 version** 重读仍然成功、文件变了之后旧版本 → 409 `VERSION_CONFLICT` | 通过 |
| 文本上限与非文本 | `MaxTextBytes=1<<20`、UTF-8/NUL 检查 | 验收 §6：超过 1 MiB → 413；非 UTF-8 → 422 `FILE_NOT_TEXT`；含 NUL → 422 `FILE_NOT_TEXT`；空路径 → 400 | 通过 |
| 下载：响应头与字节一致 | `internal/api/files.go`(`contentDisposition`/`percentEncodeRFC5987`) + 代理流 | 验收 §10：`Content-Type: application/octet-stream`、`X-Content-Type-Options: nosniff`、`Cache-Control: no-store`、`Content-Length` 与 `stat` 一致、`Content-Disposition: attachment`（ASCII 退化名 + RFC 5987 的 `filename*`）、`X-UPanel-File-Version` 合法、下载字节 sha256 与磁盘一致 | 通过 |
| 越界下载回的是**错误信封**而不是文件流 | `writeFilesError` + `HTTPStatusOf` | 验收 §10：符号链接下载 422；`application/json`，**没有** `octet-stream`，错误码 `PATH_OUTSIDE_ROOT`，body 不含根外内容 | 通过 |
| 权限：未登录 / viewer / admin | `internal/api/router.go`（四条全 admin） | 验收 §11：未登录 401（roots/list/download 各一条）、viewer 403（roots/list/text/download 各一条，且下载拿不到内容）；admin 的四条 200 由 §3—§10 的成功断言给出 | 通过 |
| 能力表只报真实存在的东西 | `internal/agent/registry.go` + 能力表 | 验收 §13：`supported_queries` 只含 `files.roots|list|text`，`supported_actions` 里**没有任何 files 写动作** | 通过 |
| 文件 UI 不显示“可管理整个服务器所有目录” | `web/src/views/FilesView.vue`、`components/FileEntryRow.vue` | 页面级证据在宿主（VM 无浏览器，见 §7）：SSR 回放 27 例 + 手写形态 32 例，断言根列表里每个 `path != "/"`、文案只说“服务端登记的文件根”、特殊文件行内写明“不跟随/不读取”、>1 MiB 只给“只能下载” | 通过（宿主） |
| 文件模块没有把已有功能打坏 | — | 验收 §12：健康检查、系统概要、日志查询（P05.3）、存储设备列表（P04）、服务列表（P05.1）全部仍 200 | 通过 |

**本阶段交付验收行里属于 P06.1 的部分**：`../../etc/shadow`、绝对路径、符号链接替换、magic link 无法逃逸 —— 已通过并留证。
同一行里的 2 GiB 上传、断流/磁盘满清理暂存、写路径的版本冲突属于 **P06.2**；终端（`id` 显示 `upanel-shell`、无法 sudo/读代理密钥/写系统目录）属于 **P06.3**。**本轮不声称它们已通过。**

---

## 3. 实际执行的检查

| 检查 | 命令/位置 | 结果 | 证据文件 |
|---|---|---|---|
| 宿主 Go 门禁 | `go build ./...`、`go vet ./...`、`GOOS=linux GOARCH=amd64 go build/vet ./...`、`gofmt -l ./cmd ./internal` | 全部退出码 0，`gofmt` 无输出 | `go-gates.txt` |
| 宿主单测 | `go test ./... -count=1` | **25 个包 ok**（`internal/files` 0.610s 首次真跑） | `go-test.txt` |
| 综合检查脚本 | `sh scripts/check.sh`（Node **v22.23.3**） | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| 前端门禁 | `typecheck` / `lint` / `test` / `build` | 0 error / 0 problem / **406 passed（28 文件）** / 构建成功 | `web-gates.txt` |
| 契约 lint | `redocly lint api/openapi.yaml` | valid，**6 warning**（4 个未使用组件 + 2 条操作缺 4xx，均为历史遗留；本轮新增组件全部被引用） | `openapi-lint.txt` |
| **VM 真机验收**（Ubuntu 24.04.5，13 节） | `sudo BIN_DIR=/tmp/upanel-bin sh …/p06-1-files-acceptance.sh` | **PASS=115 / FAIL=0 / NOT RUN=0** | `p06-1-acceptance-2404.txt` |
| **VM 载荷校验**（字段闭集、游标语义、错误码与文案） | 脚本内 §13 调用 `p06-1-payload-validate.py` | **PASS=6379 / FAIL=0** | 同文件 §13 |
| VM 原生测试二进制（`//go:build linux`） | `sudo /tmp/upanel-bin/tests/files.test -test.v`、`api.test` | `files.test` **54 个 PASS / 0 FAIL**、`api.test` PASS | `native-tests.txt` |
| 真实载荷留档 | 44 份 VM 响应逐字节存入 `payloads/` | 见 §10 | `payloads/` |

验收脚本完整跑了 **3 次**：第 1 次被脚本自身缺陷拦下（§6-1），第 2 次暴露出三个产品缺陷（§5-1—3），修完后第 3 次在最终产物（含内嵌前端 dist）上运行，留档结果 115/0/0 与 6379/0。

---

## 4. 关键断言原文（逐行摘自真机日志，未改写）

```
== 3. 根只来自配置 ==
PASS  roots 里恰好三个登记根
PASS  roots 没有 path 为 / 的项
PASS  未登记的 root_id 是 404 NOT_FOUND

== 4. 路径穿越与形状校验 ==
PASS  '../../etc/shadow' 是 422
PASS  '../../etc/shadow' 错误码 PATH_OUTSIDE_ROOT
PASS  绝对路径是 422
PASS  绝对路径错误码 PATH_OUTSIDE_ROOT
PASS  中段 '..' 是 422
PASS  空段（a//b）是 400
PASS  '.' 段是 400
PASS  超长路径是 400

== 5. 符号链接 / magic link / 硬链接 / 特殊文件 ==
PASS  符号链接在列表里标为 symlink
PASS  符号链接指向根外的文件：读取是 422
PASS  符号链接指向根外的目录：列举是 422
PASS  magic link 跳板（/proc/self/fd）：列举是 422
PASS  硬链接（nlink=2）：读取是 422
PASS  FIFO 读取是 400（不是普通文件，且不阻塞）

== 6. 文本：内容、版本与上限 ==
PASS  带同一版本重读仍然成功
PASS  文件变了之后旧版本是 409
PASS  超过 1 MiB 的文本是 413
PASS  非 UTF-8 是 422 FILE_NOT_TEXT
PASS  含 NUL 的文本也是 FILE_NOT_TEXT

== 7. 分页与超大目录 ==
PASS  第一页条数是一页 200
PASS  尾页 next_cursor 为空（不是空串）
PASS  两页无重复
PASS  超大目录如实标记 truncated

== 8. 根内子挂载（RESOLVE_NO_XDEV） ==
PASS  根内子挂载上的路径是 422

== 9. 根身份变化 ==
PASS  根被删掉重建后列举是 409
PASS  重建的根没有被当成原来的根（响应里没有任何目录项）

== 10. 下载 ==
PASS  Content-Length 与文件一致
PASS  Content-Disposition 是 attachment
PASS  下载字节与原文件一致
PASS  越界下载的 body 是 JSON 错误信封
PASS  越界下载没有 octet-stream

== 11. 权限：未登录 / viewer / admin ==
PASS  未登录 roots 是 401
PASS  viewer roots 是 403
PASS  viewer download 是 403
PASS  viewer 下载拿不到文件内容

通过 115 项，失败 0 项，未执行 0 项
```

§13 载荷校验（同一份日志末尾）：

```
PASS  能力表里没有 files 的写操作（['files.list', 'files.roots', 'files.text']）
PASS  拒绝消息指向 path="/"（配置错误: agent.files.roots[0].path 不能是根目录 '/': 那等于把整个服务器文件系统登记成一个文件根；请登记具体目录）
通过 6379 项，失败 0 项
```

## 5. 本轮发现并修掉的缺陷（全部改产品，不改断言）

1. **`O_PATH` 的目录 fd 上 `getdents64` 是 `EBADF`**（Q136）。
   列目录用 `O_PATH|O_DIRECTORY` 打开再 `getdents64`，每个列举都 500。`go:build linux` 的代码在 Windows 宿主上
   一次都没真跑过，只有 VM 能抓到。改：列目录用 `openRead|O_DIRECTORY`（即 `O_RDONLY|O_NONBLOCK|O_DIRECTORY`，另加 `O_CLOEXEC|O_NOFOLLOW`）；根句柄继续用 `O_PATH`，只当 dirfd、不再在它上面 `getdents64`。
2. **`O_RDONLY` 打开 FIFO 会永久阻塞**（Q137）。一个 `?path=fifo` 就能钉死一个协程。
   改：读句柄用 `O_RDONLY|O_NONBLOCK`，再由 `fstat` 判普通文件。
3. **根内“指向目录的符号链接”被内核报 `ENOTDIR`，被误归成 400**（Q138）。契约要求 422。
   改：`classifyNotDir` 用 `O_PATH|O_NOFOLLOW` 重解一次 + `fstat`，`S_IFLNK` → `ErrPathRejected`，`ELOOP/EXDEV` → 拒。
4. **尾页 `next_cursor` 返回 `""`**（Q140），与 `/jobs`、`/logs`、`/users` 的 `null` 不一致，界面会出现点不动的“加载更多”。
   改：`ListPage.NextCursor` 改 `*string`，前端 DTO 同步。
5. **前端 `ApiErrorPanel` 的 `title` 从来没有声明成 prop**——`FilesView` 传的标题落成了 attr，静默失效。
   改：加可选 `title` + `heading` computed（同一错误码在“列目录”和“读文件”里说法不同）。

---

## 6. 流程事故（记录，不掩盖）

1. **验收脚本漏拼端点（Q139）**：第一轮 `list`/`text`/`roots`/`download` 的请求路径漏了子端点，
   全部打到 `/api/v1/files`，59 条失败。因为很多断言只检查“非 200”，失败看起来像产品问题。
   改成“端点必填”的取数函数后重跑，才看见真实的产品缺陷（即 §5 的 1—3）。
2. **`TestRootIdentityChangeIsUnavailable` 曾经假通过（Q146）**：用例先 `RemoveAll` 临时目录、再建诱饵，
   内核把刚释放的 inode 又分给了诱饵，`dev/ino` 与登记时相同，于是“根变了”根本没被造出来。
   改成“先造诱饵 → 删原根 → `mv` 到位”。

另：`tests/integration/p06-1-files-acceptance.sh` 顶部注释第 11 条措辞由“为空”改为“为 null（不是空串）”，
**纯注释**，断言在验收运行时已经是 `next_cursor is None`；因此不为这一行重跑验收。

---

## 7. 未验证项（NOT RUN）与原因

| 项 | 状态 | 原因 / 后续 |
|---|---|---|
| Ubuntu 22.04.5（内核 5.15）上跑本轮验收 | **NOT RUN** | 本轮只在 24.04.5 上取证。`openat2` 与四个 `RESOLVE_*` 在 5.15 有，但**未在真机验证**。复核：把 `BIN_DIR` 换到 2204 VM 上跑同一脚本 |
| 内核 < 5.6（无 `openat2`）时的 `NOT_IMPLEMENTED` 回退 | **NOT RUN** | 验收内核 6.8；代码分支存在但没被触发过 |
| VM 里用浏览器操作文件页 | **NOT RUN** | VM 无浏览器。页面级证据是宿主 SSR 真实载荷回放（27 例）+ 手写形态（32 例），不是浏览器端证据 |
| 网络文件系统（NFS/CIFS）作为文件根 | **NOT RUN** | 真机只造了 ext4 与 tmpfs 根；`mountinfo` 的 fstype 解析对网络盘有处理，但没造出真实 NFS 根 |
| `go test -race ./...` | **NOT RUN** | 本轮未跑（P04.3 曾在 22.04 VM 上跑过全量 race，不含 P06 代码） |
| `manager_other.go`（非 Linux 平台回退） | **NOT RUN** | 没有第二平台可跑；Windows 宿主上的 `go build ./...` 会编译这个分支（编译通过），但它的 `NOT_IMPLEMENTED` 行为没被断言 |
| `scripts/run-native-tests.sh` 的包列表包含 `files` | **NOT DONE（有意）** | 本轮是**单独**跑 `files.test`，证据 `native-tests.txt` 与旧列表一致。P06.2 开始前把 `files` 并入列表并重跑，避免同一份证据对应两个不同脚本 |
| 2 GiB 上传不涨内存、断流/磁盘满清理暂存、写路径的版本冲突、配额 | **NOT RUN** | 属于 P06.2，本轮不做写入 |
| 终端（`upanel-shell`、PTY、WebSocket、再认证、Origin） | **NOT RUN** | 属于 P06.3 |

**不要把本节的 mock/宿主测试当成真机功能验收**：宿主上 `internal/files` 的 `//go:build linux` 用例并不执行，
真机证据只有 `p06-1-acceptance-2404.txt` 与 `native-tests.txt` 两份。

---

## 8. 环境事故（与产品无关，但影响证据可信度，故记录）

本轮开始时 VM 处于 **boot 0**（2026-10-21 05:34:25 起）。上一轮 boot −1 的最后一条记录是 SSH 会话关闭（05:16:11），
`journalctl --list-boots` 里**没有任何** `Reached target Shutdown|Power-Off|Reboot` 或 `systemd-shutdown` 记录，
即上一次运行以**非正常终止**结束（第 10 次；panic 文本没有落盘，所以不引用原文）。
本轮（boot 0）期间 3 次完整验收运行、2 次原生测试二进制、若干次 SSH 命令，**未再发生非正常终止**。

---

## 9. 复核命令

```sh
# 宿主门禁
export PATH="/c/Program Files/Go/bin:$PATH"
go build ./... && go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./... && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...
gofmt -l ./cmd ./internal
go test ./... -count=1

# 前端（必须 Node v22.23.3，宿主默认 v24.15.0 会让 check.sh 的 node-version 步失败）
export PATH="/c/Users/huxi/AppData/Local/nvm/v22.23.3:$PATH"
npm --prefix web run typecheck && npm --prefix web run lint && npm --prefix web run test && npm --prefix web run build
sh scripts/check.sh

# 交叉编译 + 上传
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
mkdir -p .build/linux-amd64/tests && cd .build/linux-amd64
go build -o upanel-agent ../../cmd/upanel-agent
go build -o upanel-web   ../../cmd/upanel-web
go build -o upanelctl    ../../cmd/upanelctl
go test -c -o tests/files.test ../../internal/files
go test -c -o tests/api.test   ../../internal/api
scp upanel-agent upanel-web upanelctl upanel@127.0.0.1:/tmp/upanel-bin/
scp tests/files.test tests/api.test upanel@127.0.0.1:/tmp/upanel-bin/tests/
scp -r ../../tests/integration upanel@127.0.0.1:/tmp/upanel-bin/tests/

# VM 内
ssh -i H:/Code/vms/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1 \
  'chmod 0755 /tmp/upanel-bin/upanel-agent /tmp/upanel-bin/upanel-web /tmp/upanel-bin/upanelctl /tmp/upanel-bin/tests/*.test /tmp/upanel-bin/tests/integration/*.sh'
ssh -i H:/Code/vms/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1 \
  'sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-bin/tests/integration/p06-1-files-acceptance.sh'
ssh -i H:/Code/vms/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1 \
  'sudo /tmp/upanel-bin/tests/files.test -test.v; sudo /tmp/upanel-bin/tests/api.test'

# 取回载荷
scp -r upanel@127.0.0.1:/var/tmp/upanel-accept-p06-1/payloads docs/evidence/P06/2026-10-21-p06.1/
```

---

## 10. 证据文件清单

| 文件 | 内容 |
|---|---|
| `p06-1-acceptance-2404.txt`（6543 行 / 634 KB） | 完整真机验收日志：§0—§13 共 14 节（13 个验收节 + 载荷校验节），115 条脚本断言 + 6379 条载荷校验 |
| `payloads/`（44 份） | 真机原始响应：根列表与前后对比、目录页 1/2、截断页、文本与版本、11 个错误信封、下载头与二进制、能力表、`path="/"` 拒绝输出 |
| `go-gates.txt` / `go-test.txt` | 宿主 Go 门禁与 25 个包的单测 |
| `native-tests.txt` | VM 内核原生 `files.test`（54 PASS）与 `api.test` |
| `check.txt` / `web-gates.txt` / `openapi-lint.txt` | 综合检查、前端四步、契约 lint |
| `env.txt` | 验收机事实（系统、内核、CPU/内存、账号、文件系统、`openat2` 可用性）与内核异常启动记录 |
| `result.md`（本文件）/ `commands.log` | 验收结论与按时间顺序的实际命令 |
