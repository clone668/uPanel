# P08.2 验收结果 — 托管 include、testparm、原子更新、重载、共享连接状态

对应 STAGES §10 P08.2；契约见 CONTRACTS「共享配置写入」「共享连接状态」；决策见 DECISIONS Q232–Q234。

## 1. 结论

| 项 | 结果 |
| --- | --- |
| 真机验收（Linux VM integration） | **PASS=237 FAIL=0 NOT RUN=0**（15 节，`run.log`） |
| 主机侧全套检查（`scripts/check.sh`） | **PASS 13 / FAIL 0 / NOT RUN 0**（`check.txt`） |
| 主机侧 `go test ./... -count=1` | 28 个包 `ok`，0 失败（`native-tests.txt`） |
| 本轮新增/重写单元测试 | 4 条，全部 PASS（`native-tests.txt` 第二节） |
| 这轮之前的状态 | 实现在，但**真机跑不通**：见 §4 的两个真实缺陷 |

“PASS” 在这里的含义是：断言由**面板之外**的独立手段复核过，不是面板自己回一句 success。
每一节标题右边都写了它用什么外部证据核实（`testparm` 读回、`namei`/`getfacl`、真实 `smbclient`、
network namespace 里的真实源地址、`smb.conf` 逐字节前缀比对、明文扫描）。

## 2. 环境与被测物

一次性 Ubuntu 测试虚拟机（**不是开发宿主，也不是日常服务器**），完整采集见 `env.txt`：

- Ubuntu 24.04.5 LTS，kernel 6.8.0-147-generic x86_64，2 vCPU，≈4 GB
- Samba `2:4.19.5+dfsg-4ubuntu9.8`，`smbd --version` = `Version 4.19.5-Ubuntu`；`acl 2.3.2-1build1.1`
- 根文件系统在 `/dev/sda`；本轮夹具盘 `/dev/sdb`（8 G，验收前清成空白）
- 被测二进制 sha256（与宿主构建产物一致）：
  - `ee89f150b0813996dbd8e1621f8179ab37df6b0c97cdbfd82d4a87d254e1cb10  upanel-agent`
  - `792fba89757f0b08b290afaf8a63e9e83f9638ffc3ff2c921210b0126b77a235  upanel-web`
  - `6fa4404f53b4ffce57256cf5f12a8bfd893662e2a4a5516a5346f90beb061f43  upanelctl`
- 验收脚本 sha256 `6cbe92db76a8b369bc0674a46cbe3617d96e90ef551524c0c0a89306208e5504`，
  1346 行，与仓库中 `tests/integration/p08-2-share-config-acceptance.sh` **一致**（跑的就是提交的这份）

## 3. 逐节断言（`run.log`）

| 节 | 断言数 | 这一节证明什么 / 外部的眼睛是什么 |
| --- | --- | --- |
| 0. 环境与安全守卫 | 38 | 二进制、依赖命令、Samba 工具齐全；`/` 在 `sda`；`smb.conf` 起点哈希已记录且无残留 include；备份目录已清空；Web 身份是 `upanel-web`（非 root） |
| 1. 配置与启动 | 14 | 代理以 root 起、建 socket；Web 以 `upanel-web` 起 |
| 2. 能力检测 | 25 | 这一阶段起 `config_write` 必须真是 true，不是硬编码的成功 |
| 3. 夹具：空盘 → GPT → ext4 → 导入 → 挂载 | 14 | 共享目录真的落在一块卷上（走的是产品自己的分区/格式化/导入/挂载链路）；`blkid` 卷标独立复核 |
| 4. 受管理账号 + 秘密通道 | 10 | 密码只经标准输入进秘密通道；`secret.json` 里只有 `secret_ref`，没有明文 |
| 5. `share.apply`：配置真写进去、Samba 真在用 | 36 | 托管文件整份重写、`include` 只接一行；**`smb.conf` 原有内容逐字节保留（新旧前缀比对）**；追加只有“一行注释 + 一行 include”；`testparm --section-name --parameter-name` 逐参数读回 path / valid users / read only / guest ok / hosts allow；备份 0600；`sudo -u` 验账号能走进目录、能穿过每一级上级目录；上级目录对 other 只开 `x` |
| 6. 真实 SMB 客户端 | 21 | 真实 `smbclient` 会话：write 用户能上传、文件真的落在卷里、内容一致、能读回；名单外账号被拒；匿名被拒；`hosts allow` 用 **network namespace 里的 10.99.0.2** 做真实非环回源地址——不在网段被拒，改成 `10.99.0.0/24` 后同一客户端连得上（正对照） |
| 7. 共享连接状态 | 12 | 读的是 Samba 自己的会话表：账号、机器、协商方言、签名状态、建立时间；面板连接数与 `smbstatus` 自报一致；断开后回到 0；空闲时仍报 `active`（空列表 ≠ 服务停了） |
| 8. 幂等重放 | 14 | 内容没变 → 不写文件、不重载、不产生新备份、哈希不变、`served` 仍为 true；同时夹具把上级目录改回 `0700`，重放把它修成可穿越（`path_traversal_repaired` 点名），修完 write 用户又能上传 |
| 9. 名单变更 | 14 | 加只读账号：`valid users` 两个、`write list` 只有一个；`smbclient` 证明只读用户能列目录、上传被拒且卷里没留文件；`getfacl` 读回 r-x / rwx |
| 10. smbd 没在跑 | 13 | 配置照写、任务 success，但结果是 `accepted` **不是** `verified`、不带 `verified_at`、`served=false`、不重载、说清原因；连接状态报 inactive 并给 `SMB_SERVICE_INACTIVE` |
| 11. `share.disable` | 9 | 段消失（`testparm` 复核）、托管文件里也没了、客户端连不上、命名 ACL 已撤销、**目录和文件一个都没删** |
| 12. `share.delete` | 4 | 详情 404、段没了、目录与内容都还在 |
| 13. 第二条共享收尾 | 4 | 托管文件回到只剩 `[global]`，列表回到空 |
| 14. 收尾不变量 | 9 | 整轮下来 `include` 仍只有一行、原有段名一个没少、目录 0755 / 文件 0644；**两个日志 + 两个数据库（含 WAL）+ 所有进程 argv/environ 里都扫不到共享密码明文**；代理没退出、socket 还在 |

断言全部落在“外部能看见的状态”上，没有一条是“面板说成功所以成功”。

## 4. 四次运行与两个真实缺陷

同一份脚本在同一台一次性 VM 上跑了四遍，每一遍的日志都留着（未修饰）：

| 运行 | 结果 | 根因 |
| --- | --- | --- |
| #1 | 在 §4 中止，`EXIT=2` | **脚本缺陷**：`set -u` 下函数外引用局部变量，`_ACCT_RW: parameter not set` |
| #2 | PASS=211 **FAIL=7** | **产品缺陷（Q233）**：见下 |
| #3 | PASS=225 **FAIL=1** | **脚本缺陷（Q234）**：见下 |
| #4 | **PASS=237 FAIL=0 NOT RUN=0** | — |

### Q233 — 产品缺陷：多级上级目录 0700 导致真实客户端 `ACCESS_DENIED`

第一次真机跑挂在这里：

```
FAIL  write 用户能上传: 期望包含 'putting file'，实际 'NT_STATUS_ACCESS_DENIED opening remote file \probe-write.txt'
```

`namei -l` 直接给出答案：`/srv/upanel/volumes/data/team` 是 `drwx------ root root`。
原因是 `MkdirAll(dir, 0700)` 把缺失的父目录也一起建成了 `0700`；共享账号既不是 owner 也不是 group 成员，
连“走进去”的 `x` 都没有。7 条断言失败是**一个**根因的下游表现（记在 DECISIONS Q233 的记账规则里：
失败断言数与根因数分开记，不许含糊成“7 个 bug”）。

修法：叶子共享目录 `0700` + `setfacl`（不变）；**上级目录 `0711`**（other 只给 `x`，可穿越、不可列出）；
对历史上已经建成 `0700` 的上级目录做 `perm|0o011` 修复并上报 `observed.path_traversal_repaired`。
`internal/shares/system.go` 为此新增可注入的 `Chmod`，`engine_test.go` 的假文件系统改成忠实复刻
`MkdirAll` 的递归语义（对已存在路径 no-op）。新测试用**变异验证**过：把 `ancestorTraverseMode` 改回
`0o700`，四条测试里对应的会红——它们不是空断言。

### Q234 — 脚本缺陷：`hosts allow` 管不住本机，环回测不了这个参数

第二次跑只剩 1 条红：

```
FAIL  不在允许网段里的客户端被拒
```

`man 5 smb.conf` 写得很清楚：`hosts allow` 里 `127.0.0.1` 永远被放行。所以“从本机连、期望被拒”
这条断言本身就是错的，参数再好也过不了。手工在 VM 上临时加 `[nstest]` 段验证过：`10.99.0.2` 拿到
`NT_STATUS_ACCESS_DENIED`，`127.0.0.1` 正常列目录。`probe5.sh` / `probe5-output.txt` 把这段复现固定下来了
（跑完 `/etc/samba/smb.conf` 的 sha256 与跑之前完全一致：`8b519488…`）。

修法分两边：
- 脚本：加 `setup_netns`，用 veth 在 network namespace 里造一个真实非环回客户端 `10.99.0.2`，
  再加一条正对照（改成客户端所在网段后**同一个**客户端必须连得上）——只测“被拒”会被一个坏配置骗过。
- 产品：新增**非阻断**告警 `SHARE_HOSTS_ALLOW_LOOPBACK`——`allowed_cidrs` 不覆盖 `127.0.0.0/8` 时提示
  “这份名单管不住本机”，但**不**偷偷替用户加 `hosts deny`（那是改用户意图）。

## 5. 主机侧检查（未在 VM 上跑的部分）

- `scripts/check.sh`：PASS 13 / FAIL 0 / NOT RUN 0，明细 `go-build go-vet go-test go-build-linux
  go-vet-linux gofmt node-version web-typecheck web-lint web-test web-build web-smoke openapi-lint` 全 PASS；
  `openapi.yaml` 合法，7 条 warning（`no-unused-components: 4`、`operation-4xx-response: 2`、
  `operation-2xx-response: 1`），与 P08.1 一致。
- `go test ./... -count=1`：28 个包 `ok`（`internal/testsupport`、`migrations/*` 无测试文件）。
- 本轮四条测试：`TestExecuteShareApplyCreatesAncestorsTraversable`、
  `TestExecuteShareApplyRepairsTightAncestor`、`TestAssessShareApplyWarnsNotTraversableAncestor`、
  `TestAssessShareApplyWarnsHostsAllowLoopback`——全 PASS。
- `probe.sh` … `probe4-output.txt` 是这一轮定契约前在 VM 上求证 Samba 行为的记录：
  `testparm --parameter-name` 的输出细节、smbstatus/smbcontrol 在 smbd 停着时仍 rc=0、
  smbstatus 的 id 是字符串、`smbclient -A authfile` 不让密码进 argv。

## 6. 未验证 / NOT RUN

| 项 | 状态 | 原因 |
| --- | --- | --- |
| `P00-VM-D`（独立 SMB 客户端虚拟机） | **NOT RUN** | 未创建该独立虚拟机；SMB 客户端行为改用同机 VM 内真实 `smbclient` + network namespace 客户端验证，覆盖的是同一批系统调用与同一条 SMB 协议路径，但**不是**跨机器的真实网络路径 |
| 跨机器真实网段访问（经物理/虚拟交换机的远程客户端） | **NOT RUN** | 一次性 VM 只有本机 veth；`10.99.0.2` 是 netns 里的真实地址而非另一台主机 |
| Windows/macOS 客户端兼容性 | **NOT RUN** | 未测；`server min protocol = SMB2` 只做了 Linux `smbclient` 侧验证 |
| `P08.3`（volume 绑定、卸载/格式化依赖阻断、运行时脱盘） | 未开始 | 下一轮 |
| `P08.4`（共享 UI） | 未开始 | 排队 |
| `internal/storage` 的 `TestOSCapacityReaderNetworkTimeoutQuarantines` | 曾不稳定 | 早前一次 `go test ./...` 里失败过一次，随后多次重跑（含本轮全量）均 `ok`；未定位到确定原因，不在本轮范围内修，记录在案 |

## 7. 已知限制（写进契约的，不是“以后再说”）

1. **`hosts allow` 对环回永远放行**：`allowed_cidrs` 不覆盖 `127.0.0.0/8` 时面板给
   `SHARE_HOSTS_ALLOW_LOOPBACK` 告警，但本机访问仍然连得上——这是 Samba 的文档行为，面板不改写它。
2. **上级目录 `0711` 的语义**：共享账号能穿越但不能列出上级目录名。叶子目录仍是 `0700` + `setfacl`，
   所以权限的真相在 ACL 上，不在 mode 上。
3. **重载只在 `smbd` 是 active 时执行**：否则报 `reload=skipped_service_inactive`、
   结果 `accepted` 而非 `verified`、`served=false`——面板不会在服务停着时说“已生效”。
4. **`testparm` 读回是逐参数的**：`--section-name=X --parameter-name=Y` 一次一个参数，
   所以“配置真的生效”的判定粒度是参数级，不是整段文本比对。
5. **`smb.conf` 只在末尾追加两行**：一行说明注释 + 一行 `include`。原有内容逐字节保留（有前缀比对断言守着），
   后面的定义覆盖前面的定义。
