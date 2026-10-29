# P12.2 验收结果（2026-10-24）

## 结论

P12.2 **passed**：策略、运行、快照列表、文件一致性说明、停受管理应用备份、
面板 SQLite 在线备份，以及本阶段的 SFTP、断网、存储满、取消验收均已完成。
两台专用 Ubuntu VM 使用同一最终脚本和同一组二进制，最终均为：

```text
PASS=170 FAIL=0 NOT RUN=3
exit=0
```

宿主 `scripts/check.sh` 在最终产品代码上为 **PASS 13 / FAIL 0 / NOT RUN 0**。
三条 NOT RUN 全部属于后续阶段，不是 P12.2 的功能缺口，不折算成通过。

## 范围与判据

1. 快照不能凭命令退出 0 判成功：任务必须给出完整 `snapshot_id`，并以
   `restic stats` 回读，`verified_by = "restic stats"`。独立工具另将快照恢复到临时目录，
   逐文件 sha256 与源比对；这不是产品恢复接口的验收。
2. 备份定义只有代理镜像一份真相，面板不建 `backup_*` 表；代理停止后 `/backup/*` 整组 503。
3. PATCH 缺 `version` 为 400，版本不符为 409；`backup.run` 的旧版本预检为
   `BACKUP_POLICY_VERSION_STALE`。
4. `apps` 由 Web 展开，代理双向核对。调用方自报、少给运行对象、多给对象都拒绝。
5. `stop_managed_apps` 真停 systemd 单元再拉起；任务记录逐应用 stop/start 的
   `done/verified`，真机核对单元 active 且 `ActiveEnterTimestamp` 变化。
6. 面板 SQLite 用 `VACUUM INTO` 在线副本，包含 WAL 中已提交事务。恢复出的副本
   `PRAGMA integrity_check = ok`，并能读到备份前最后写入的 users / audit_events。
7. 快照列表明确 `size_included:false`，不发送 `size_bytes`；未登记仓库、未找到快照、
   短 id 和缺参数的响应按契约区分。
8. SFTP 真写快照并回读；运行中断网、独立 tmpfs 写满、运行中取消都必须让任务进入正确终态，
   源数据不变，已有快照仍可读。

## 环境、版本与指纹

- VM-A `upanel-2204`：Ubuntu 22.04，systemd 249，restic 0.12.1。
- VM-B `upanel-2404`：Ubuntu 24.04，systemd 255，restic 0.16.4。
- 两台不并发；VM 验收期间宿主不跑重型检查。
- 最终脚本 `tests/integration/p12-2-backup-acceptance.sh`：

  ```text
  d7028b17ea827ec51e8f943c842edb6d399f8659cc3084c14bd693b15ed40c78
  ```

- 常驻 JSON 助手 `tests/integration/json_helper.py`：

  ```text
  767b9844676a7f570c195b4de1a2e1768bb1f50b5aa7219963c016c1c9d6a837
  ```

- 两台最终部署的二进制 sha256 相同，构建时间 `2026-10-24T10:15:07Z`：

  ```text
  upanel-agent  90c83bcbadffe5e0487ff37902b18420b406951cb22a4e8b1add5c1fada06980
  upanel-web    a39259644194712fa866277c8e972451a84ec6bbd16b350fc2d0e7d793fd658c
  upanelctl     d0bb253a8e964eb42cbd3e739dd0e36294386d7560dd9f0cede447601b1aac3f
  ```

## 最终结果

| 机器 | 结果 | 退出码 |
|---|---|---|
| VM-A（22.04，restic 0.12.1） | **PASS 170 / FAIL 0 / NOT RUN 3** | 0 |
| VM-B（24.04，restic 0.16.4） | **PASS 170 / FAIL 0 / NOT RUN 3** | 0 |

宿主复核还逐字比较了两台 `results.txt`：170 个 PASS 名称、3 个 NOTRUN 名称及顺序完全一致，
FAIL 均为 0。每台最终证据目录为 logs 4 文件、payloads 89 文件。

### 新增四类真机证据

| 场景 | 22.04 | 24.04 | 独立事实 |
|---|---|---|---|
| SFTP 成功 | `job_73627d6273b0d8f25b4a180104e5a8c5` / succeeded | `job_8f26cf1ed3892ce017c55f332b473dc3` / succeeded | 完整快照 id、stats 回读、列表包含该快照、远端文件存在 |
| 中途断网 | `job_1f0c7d23ce273ade092ebcb13e92f9f0` / failed + COMMAND_FAILED | `job_2475d7a15a87190a1d91f9baddc0f23e` / failed + COMMAND_FAILED | 只杀本次 restic 的 SFTP ssh；源哈希不变，旧快照仍可读，成功后快照断线前后均可列出 |
| 存储满 | `job_9f1d3732a5289160d6d770b005968303` / failed + COMMAND_FAILED | `job_f65eded10df2e9f7994d6207ed608e11` / failed + COMMAND_FAILED | 独立 64 MiB tmpfs 真撞 ENOSPC；源不变，baseline 完整 id 回读一致，tmpfs 已卸载 |
| 运行中取消 | `job_ed8b9d6c25aa0b074b3b87b25a553ba3` / canceled | `job_2226015fc8791329a68501b8892158e5` / canceled | 取消请求 202、`cancellation_requested=true`；源哈希和旧快照不变 |

最终 SFTP 快照：

```text
22.04  765fdb419a3d7a0b6b3cc7b20ef3613c1e277c4fd4fda9b551fa15951c11124c
24.04  3894e17d9becc92a91b8edda6524d87baa6b5f6c4a97e7d23945c2f474195c41
```

断网与存储满不是只看“任务变红”：两种场景的源数据与旧快照都有独立核对。
存储满核对发生在任务终态、无并发写入之后，使用 `restic --no-cache --no-lock dump`，
避免核对工具自己向已满仓库写锁。

## NOT RUN（3 条，均属后续阶段）

1. `product-restore-api`：产品恢复接口是 P12.3；本脚本的 restore 是独立 restic 核对。
2. `retention-prune`：本阶段 retention 只登记不执行，显式清理是 P12.3 的 `backup.prune`。
3. `browser-ui`：VM 上没有浏览器；备份 UI 属 P12.5。

## 真机抓到的产品缺陷（4 个，均已修复并复验）

### 1. restic 0.12 的短快照 id 被误判为非法（Q343）

restic 0.12.1 的 `backup --json` summary 只给 8 位短 id，原实现只认 64 位，
于是快照已写入却整轮失败。修复为按前缀回仓库解析，恰好命中一条才换成完整 id，
再执行 `restic stats`；0 条或多条均失败，不把前缀当身份。

### 2. Web 写超时短于慢机器的一次 Argon2id 校验（Q344）

24.04 登录 handler 实测 30.406 秒后已决定返回 200，但 30 秒 `WriteTimeout` 已到期，
客户端只收到断连。修复为 120 秒，并用测试钉住下限；没有降低 Argon2id 参数。

### 3. 进度回调堵住 stdout 读协程，快照已建却报失败（Q345）

每条 restic status 都同步写 SQLite，拖住 `os/exec` 的读协程；进程按 0 退出后
`WaitDelay` 到期，旧代码又把 `exec.ErrWaitDelay` 误报成启动失败。修复包括：
独立有界进度队列、按百分比/时间节流、单独识别 `ErrOutputIncomplete`，以及输出截断保尾部。
反向测试在 24.04 Linux VM 上证明旧行为会红、新行为通过。

### 4. Web 的 10 秒代理客户端提前取消正常的 90 秒快照查询（Q347）

24.04 扩展轮中，两次 SFTP 快照列表均为 503 `AGENT_UNAVAILABLE`。Web 日志为
`Client.Timeout exceeded while awaiting headers`，代理随后才看到 `context canceled`；
代理实际查询约 10.146 秒和 10.263 秒。根因是代理给 restic/SFTP 查询 90 秒，
而 Web 通用 agentclient 只等 10 秒。

修复为备份管理单独使用 `agentclient.BackupQueryTimeout = 100s`，同时覆盖整体与响应头超时；
100 秒晚于代理 90 秒预算，且低于 Web 120 秒写超时。其他模块不全局放宽。
修复后同类 24.04 查询分别耗时 11.313 秒和 19.728 秒，均返回 200，证明旧上限真实可触发。

## 验收脚本与方法修正

- 120 秒预检窗口是契约值，不改。再认证移到预检前，常驻 JSON 助手消除慢 VM 上反复 Python
  启动成本；任务 id 读取失败立即报错，不再拿错误字符串拼 URL（Q346）。
- 多行 PEM 不能进常驻助手的一行协议，改为一次性 Python 从文件编码 JSON；请求发出后删除
  明文请求体，秘密只进秘密存储，响应只保留 `secret_ref`。
- 存储满 filler 改用 `oflag=append conv=notrunc`，避免第二次 dd 截断旧文件并释放空间；
  filler 必须真撞 ENOSPC，不能静默退化成写系统盘。
- 扩展用例后原再认证会越过 5 分钟窗口，在未初始化仓库创建前重新再认证。
- 快照列表按真实契约读取 `data.items[].snapshot_id`，不是 `snapshots[].id`。
- 旧版脚本判据修正保留记录：viewer 字段 `initial_password`、代理预检 201、
  `apps_restored` 是布尔、任务字段是 `job_id`；runuser 的 pid 不能用来判断 Web 实际 uid。
- 证据取回不再打包整个工作目录。`tools/pull-p122-evidence.sh` 只打包 logs/payloads，
  取回前扫描口令和 `BEGIN OPENSSH PRIVATE KEY`，发现秘密立即停止。

## 证据索引

- `logs/{2204,2404}-run.log`：两台最终完整运行输出。
- `logs/{2204,2404}/results.txt`：逐条 PASS/FAIL/NOTRUN。
- `logs/{2204,2404}/{agent.log,web.log}`：服务结构化日志。
- `payloads/{2204,2404}/`：原样请求与响应，秘密请求体已替换为 redacted 说明。
- `payloads/{2204,2404}/job-{sftp,network-cut,disk-full,cancel}.json`：四类新增任务结果。
- `payloads/{2204,2404}/snapshots-sftp*.json`：SFTP 与断线后的独立列表核对。
- `payloads/{2204,2404}/source-hashes.txt`、`restic-snapshots.json`：独立一致性证据。
- `payloads/{2204,2404}/operation-panel.json`：面板库副本完整性结果。
- `logs/host-check.txt`：最终宿主 13/0/0。
- `logs/host-check-before-client-timeout.txt`：客户端超时修复前检查，保留历史。
- `logs/2404-client-timeout*`：24.04 扩展轮 167/3/3 的真实失败现场，未覆盖或删除。
- `logs/2404-before-fix/`：更早“快照已建却报失败”的现场。
- `logs/{2204,2404}-baseline-121*`：早期 121/0/7 基线，仅作历史，不是最终结论。

## 秘密卫生与检查边界

仓库口令、面板口令和 SFTP 私钥不进入 argv、日志或面板数据库；restic 口令经环境变量，
Web 秘密经标准输入/秘密通道。最终证据在远程取回前和宿主取回后均扫描测试口令及
`BEGIN OPENSSH PRIVATE KEY`，均为零命中。

已验证的是上述两台冻结环境及列出的断言；未声称兼容所有 restic 版本，未执行产品恢复、
保留清理、浏览器 UI 或 `go test -race`。工作树改动未提交、未推送。
