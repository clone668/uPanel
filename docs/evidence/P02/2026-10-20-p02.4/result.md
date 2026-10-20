# P02.4 验收结果 — 审计事务化、日志脱敏、代理独立数据库与秘密加密存储

- 日期：2026-10-20
- 范围：STAGES.md P02.4（追加式审计；日志秘密脱敏；独立代理数据库和秘密加密存储），
  契约依据 CONTRACTS.md §14.2（代理侧数据与秘密）与 §15（代理内部协议），
  权限依据 PLAN.md §4（`upanel-web` 非 root / `upanel-agent` root 仅 socket）。
- 结论：**通过**。宿主检查全绿；两台 Ubuntu 验收 VM 上 P02.1/P02.2/P02.3/P02.4 四套验收全部
  `FAIL=0`，并在 VM 内核上原生执行了 AEAD、到期销毁、权限校验的测试二进制。

## 1. 本子任务交付了什么

| 交付项 | 实现位置 | 说明 |
|---|---|---|
| 审计跟随业务事务 | `internal/audit/audit.go`、`internal/api/users.go` | `ctx` 里有 `store.TxFromContext` 的事务就写同一事务；没有事务（被拒绝的请求）独立提交并记 `outcome=failure` |
| 失败/拒绝也留痕 | `internal/api/users.go`（`recordDenied`/`failUserAction`/`reject`） | PLAN.md 第 332 行要求；details 只有 `stage`/`error_code`/方法/路径，从不带口令 |
| 日志秘密脱敏 | `internal/logging/redact.go` | 记录器自身属性：键名片段 + 值形状（含 `password=…`、`token: …` 内嵌文本） |
| 独立代理数据库 | `internal/agentstore/{store,perms_linux,perms_other}.go`、`migrations/agentdb` | `/var/lib/upanel-agent/agent.db`，目录 0700、文件 0600，**打开前后各校验一次归属与权限** |
| 秘密加密存储 | `internal/agentstore/{keyring,secrets,purposes}.go` | AES-256-GCM、每条独立随机 nonce、AAD 绑定 `ref`+`purpose`、`key_version` 列 |
| 密钥文件与首次生成 | `cmd/upanel-agent/main.go`（`-init-key`）、`/etc/upanel/agent.key` | 两行文本格式、`root:root 0600`、`O_CREATE\|O_EXCL` 永不覆盖、输出指纹 |
| 秘密 HTTP 接口 | `internal/agent/secrets.go`、`internal/agentclient/client.go` | 只有 `POST /internal/v1/secrets`；无任何读取明文的路由 |
| 到期销毁 | `internal/agent/secrets.go`（`runSecretSweeper`）、`agentstore.DestroyExpired` | 启动即扫一次 + 每 30 s；密文与 nonce 清零、保留墓碑行、幂等 |
| 配置与部署 | `internal/config`、`deploy/upanel.example.json`、`deploy/systemd/upanel-agent.service` | 新增 `agent.database_path`、`agent.secret_key_path`；`StateDirectory=upanel-agent`（0700） |

## 2. 宿主（开发机，Windows + Git Bash）执行过的检查

| 检查 | 命令 | 结果 | 证据 |
|---|---|---|---|
| 编译 | `go build ./...` | 通过 | `host-build.txt` |
| 静态检查 | `go vet ./...` | 无输出 | `go-vet.txt` |
| 格式 | `gofmt -l .` | 无输出（0 个文件需要格式化） | `gofmt.txt` |
| 单元/集成测试 | `go test ./...` | 全部包 ok | `go-test.txt` |
| 项目闸门 | `sh scripts/check.sh` | PASS 12 / FAIL 0 / NOT RUN 0 | `check.txt` |
| 交叉编译产物哈希 | `sha256sum .tmp-linux-bin/...` | 见文件；两台 VM 上 `sha256sum` 一致 | `cross-build-sha256.txt`、`vm-*/` 中的执行记录 |

## 3. Ubuntu 验收 VM 结果

两台 VM：`upanel-2404`（Ubuntu 24.04.5，内核 6.8.0-147）、`upanel-2204`（Ubuntu 22.04.5，内核 5.15.0-198）。
脚本在 VM 内以 root 运行，二进制与测试二进制由宿主交叉编译后拷入（VM 内无 Go/Node 工具链，DECISIONS Q9）。

| 脚本 | 2404 | 2204 | 日志 |
|---|---|---|---|
| `p02-1-acceptance.sh`（回归） | PASS 58 / FAIL 0 | PASS 58 / FAIL 0 | `vm-2404-p021-acceptance.txt`、`vm-2204-p021-acceptance.txt` |
| `p02-2-acceptance.sh`（回归） | PASS 103 / FAIL 0 | PASS 103 / FAIL 0 | `vm-2404-p022-acceptance.txt`、`vm-2204-p022-acceptance.txt` |
| `p02-3-agent-acceptance.sh`（回归） | PASS 84 / FAIL 0 | PASS 84 / FAIL 0 | `vm-2404-p023-acceptance.txt`、`vm-2204-p023-acceptance.txt` |
| `p02-4-agent-secrets-acceptance.sh`（本次） | PASS 87 / FAIL 0 | PASS 87 / FAIL 0 | `vm-2404-p024-acceptance.txt`、`vm-2204-p024-acceptance.txt` |

P02.4 的 87 项断言覆盖（逐条见日志）：

1. **密钥生命周期**：无密钥时启动被拒绝并给出 `-init-key` 提示、启动路径**不会**隐式生成密钥、
   `-init-key` 输出 16 位十六进制指纹、文件两行且头为 `upanel/agent-secret-key/v1`、内容解码为 32 字节、
   `root:root 0600`、再次生成被拒绝且密钥未被改动。
2. **代理数据库**：目录 `0700 root`、`agent.db` 与 `-wal`/`-shm` 均 `0600 root`、
   `schema_version=1`、迁移名为 `0001_secrets`、`secrets` 表存在；`upanel-web` 读不到库、读不到密钥、
   也列不出数据目录；journal 里有 `key_fingerprint`/`agent_schema_version`/库路径。
3. **接口行为**：201 + `sec_` 引用 + `state=temporary` + RFC3339 到期时间（540—660 s 窗口内）、
   响应里没有明文；带 `reference` 创建直接持久且无到期；`GET /secrets` → 405 + `Allow`；
   `/secrets/<ref>` 与 `/secrets/<ref>/value` → 404；未知用途、`totp_seed`（内部用途）、空值、
   超限值、未知字段 → 400；`root` 与同组其他 UID → 403，`nobody` 连接失败（000）。
4. **落盘形态**：临时 + 持久两行、状态与引用正确、密文 ≥16 字节（含 GCM tag）、nonce 长度 12 且互不相同、
   `key_version=1`；在 `agent.db`/`-wal`/`-shm` 里搜两段明文标记均为 **0 次**。
5. **重启持久性**：重启服务后 socket 回来、两行仍在、持久那行仍可读。
6. **真实实现执行**：`agentstore.test`、`agent.test` 在 VM 内核上原生运行，`FAIL=0`
   （明细见 `vm-2404-unit-tests.txt` / `vm-2204-unit-tests.txt`：agentstore 32 个用例 / 19 个顶层 PASS，
   agent 45 个用例 / 37 个 PASS，含 `TestCiphertextNeverLeaksPlaintextToDisk`、
   `TestDestroyExpiredTemporary`、`TestOpenRejectsNonRootOwnedDir`、`TestPostSecretDeniedForOtherUIDs` 等）。
7. **日志无明文**：本次运行的 journal 里搜不到提交的两个明文标记、也没有密钥本体；
   同时能读到“已保存加密秘密”这类不含秘密的运维信息（`vm-<name>-journal.txt`）。
8. **停止清理**：停服务后单元 inactive、socket 被移除（脚本退出即还原环境）。

## 4. 未执行 / 未验证（NOT RUN）

| 项 | 原因 | 补做方法 |
|---|---|---|
| 服务进程上 **10 分钟挂钟到期销毁** | 需要真实等待 10 分钟；到期逻辑已用短 TTL 在真实 SQLite + 真实 AEAD 上执行（`TestDestroyExpiredTemporary`、`TestResolveExpiredTemporaryBeforeSweep`） | 在 VM 上 `POST` 一个秘密后 `sleep 600`，再查 `state`/`ciphertext` 是否被清空 |
| 密钥轮换（`key_version` 从 1 升到 2） | 本阶段只落 `key_version` 列与校验，轮换流程未实现（契约未要求） | 实现轮换时补：旧版本解密、重加密、切换指纹、审计 |
| Web 侧真实写入秘密的调用方 | 还没有需要秘密的业务功能（SMB/备份/证书都在后续阶段） | 随 P08/P12/P10 落地时以真实业务路径验收 |
| `totp_seed` 的写入路径 | 属 P14 注册流程（内部用途，本阶段明确拒绝上传） | P14 实施时补 |
| 审计/日志在长时间运行下的脱敏压力 | 只做了用例级与单次验收级验证 | 后续阶段加入日志量大的场景时复核 |

## 5. 本轮发现并修复的问题

| 问题 | 性质 | 处理 |
|---|---|---|
| `secret_key_fingerprint` 等元数据字段被脱敏成 `[redacted]`，VM 上 `journal-reports-key-fingerprint` 失败 | 设计冲突（脱敏按字段名片段命中） | 改字段命名为 `key_fingerprint`/`key_path`/`key_version`/`ref`，并加两条回归测试固定这条约定（不放宽脱敏、不加放行名单）。DECISIONS Q46 |
| `p02-2` 的审计断言 `审计结果均为 success` / `TARGETS=user` 在 P02.4 后失败 | **验收脚本断言错误**（P02.4 按要求给被拒绝请求记了 failure 行） | 换成三条更精确且更严的断言（业务动作各至少一条 success、failure 行必须带结构化 stage/error_code、target 只允许 user/session）。DECISIONS Q47 |
| `p02-3`/`p02-4` 的 journal 断言受历史日志污染（`journalctl` 读到之前运行的记录） | 验收脚本缺陷（未限定时间窗） | 两个脚本都改为 `--since "$JOURNAL_SINCE"`，只针对本次运行。DECISIONS Q48 |
| `p02-4`/`p02-3` 脚本在非 root 下运行时产生 50 条连锁失败，掩盖真正错因 | 可用性缺陷 | 脚本开头即早退并打印正确的 `sudo …` 用法（退出码 2） |
| 脚本 CRLF 导致 `set: Illegal option -` | 仓库缺换行策略 | 新增 `.gitattributes`（`*.sh` / `*.service` / `*.sql` / `*.go` 强制 LF），并把两个脚本转成 LF |
| `gofmt` 闸门从 PASS 变 FAIL：`cmd/upanel-agent/main.go` 与 `internal/agentstore/secrets.go` 被编辑工具写成了 CRLF | 仓库卫生缺陷（不是代码问题） | 全部 `.go` 转成 LF 后重新 `gofmt -l`（0 行）、重建全部交叉编译产物；`upanel-agent` 的哈希因此变化（43ca7e78… → 32c5e305…），两台 VM 换新二进制重跑 P02.3/P02.4。见 `.gitattributes` |
| 24.04 VM 在回归途中内核 panic（`Attempted to kill init!` exitcode=0x8b） | **测试环境问题**，非产品行为 | 重启 VM、重新上传二进制与脚本（sha256 复核一致）后整套重跑，最终结果取自重启后的干净运行 |

## 6. 复现方式

```sh
# 宿主：交叉编译 + 测试二进制
GOOS=linux GOARCH=amd64 go build -o .tmp-linux-bin/upanel-agent ./cmd/upanel-agent
GOOS=linux GOARCH=amd64 go test -c -o .tmp-linux-bin/tests/agentstore.test ./internal/agentstore
GOOS=linux GOARCH=amd64 go test -c -o .tmp-linux-bin/tests/agent.test ./internal/agent

# VM：把二进制放到 /tmp/upanel-bin，脚本树放到 /tmp/upanel-src，然后
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-4-agent-secrets-acceptance.sh
```

完整命令序列见同目录 `commands.log`。

## 7. 脚本一致性

验收用的脚本就是仓库里的脚本，没有“VM 上另一个版本”。最终一轮运行后核对过 sha256（宿主 vs 两台 VM 逐字相同）：

```
host    a4d9afdc34e6ec6957bd7792f1d69a5f729b35dc382965418a5bf3edad235a04  tests/integration/p02-2-acceptance.sh
host    888a41f7c86cab33be09c2ec6fcfb83f639aa910de1b36b269722acdb214a40b  tests/integration/p02-3-agent-acceptance.sh
host    4e63ef6b5228ea3bbfc052c710de9c263bfe0c6b5e109ff544f3a065dcbb9e36  tests/integration/p02-4-agent-secrets-acceptance.sh
vm-2404 / vm-2204：三者的 sha256 与上面一致
```

`upanel-agent` 的二进制哈希在两台 VM 上核到 `32c5e305e4bf54b8b0622e2952652f33b7ff15c2bded0da37c13a67019d80a10`
（与 `cross-build-sha256.txt` 一致）。P02.3/P02.4 的最终结果取自**换上这个二进制之后**的运行。

二进制同理：`cross-build-sha256.txt` 记录宿主交叉编译产物，两台 VM 上 `sha256sum` 复核一致后
才执行验收。

