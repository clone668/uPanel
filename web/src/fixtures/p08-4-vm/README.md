# P08.4 真机载荷夹具

这里的每一个文件都是**真的从验收 VM 上抓回来的**，不是手写的。抓取者是
`tests/integration/p08-4-share-ui-client-acceptance.sh` 的第 8、9、10 节；
消费者是 `web/src/components/realdata-p08-4.test.ts`。

## 为什么必须是真的

手写夹具天然自洽。P08.4 第一次真机验收就栽在这上面（DECISIONS **Q246**）：
前端 DTO 里写着 `volume_state`、`volume_name`、`startup_deps`、`startup_paths`，
而 `api/openapi.yaml` 的 `ShareRecord` **一个都没有**——代理从来不发送它们。
手写夹具带着这四个字段，所以渲染断言全绿；真机载荷里它们是空的/不存在的，
于是“卷没挂”这条状态在真机上**永远亮不起来**。

同一轮还抓出 **Q247**：`runtime_revoked` 从来没有被清过，人点了“重新应用”、
共享已经在服务，页面却永远显示“运行时已撤销”。

这两条都不是“测试写得不好”，而是**测试问的问题不对**：渲染断言问“画出来对不对”，
没人问“服务端到底发了什么形状”。这里的文件就是回答后一个问题的依据。

## 别手改

改这里的任何一个字节，等于把 Q246 那类自洽的假象请回来。
要更新夹具，重跑验收脚本，再从 VM 上原样取回；`sources` 见下表“来源”一列，
脚本里对应 `$WORK/*.json` 与 `$PAYLOADS/*`。

## 清单

抓取时刻以载荷自带的 `collected_at` 为准（这一份是 `2026-10-22T16:41:49Z` 起），
VM `upanel-2404`，Ubuntu 24.04.5，Samba 4.19.5。
那次运行：**PASS=190 / FAIL=0 / NOT RUN=4**（`docs/evidence/P08/2026-10-23-p08.4/run.log`）。

| 文件 | 接口 | 内容 |
| --- | --- | --- |
| `shares.json` | `GET /api/v1/shares` | 能力报告 + 那条共享（`data`/`smbp08rw` rw、`smbp08ro` ro，卷 `srv/upanel/volumes/data`） |
| `share.json` | `GET /api/v1/shares/{id}` | 同一条的详情（三状态 + 版本号） |
| `accounts.json` | `GET /api/v1/share-accounts` | 三个受管理账号：rw / ro / 名单外，各带 `has_password` |
| `connections.json` | `GET /api/v1/shares/{id}/connections` | `service: active` + **空**连接列表（空 ≠ 服务停了） |
| `volumes.json` | `GET /api/v1/storage/volumes` | 页面 join 用：`id` / `display_name` / `observed_state` |
| `preflight-apply.json` | `POST /api/v1/operations/preflight` | `share.apply` 的判决（含 `requires_reauth`、计划） |
| `job-apply.json` | `GET /api/v1/jobs/{id}` | `share.apply` 的结果：`config_hash`、`startup_deps`、`startup_paths` 在 `result.data` 里 |
| `job-account-apply.json` | `GET /api/v1/jobs/{id}` | `share.account.apply` 的结果（密码走 `sec_` 引用） |
| `shares-offline.json` | `GET /api/v1/shares` | 卷被**从外面** umount 之后：`runtime_revoked` 五个字段齐全、`config_applied=false` |
| `shares-restored.json` | `GET /api/v1/shares` | 卷挂回来之后：**仍是撤销状态**（只撤不恢复）。与上一份逐字节相同是有意的——差别在卷那一侧 |
| `client-mount-steps.txt` | 页面说明 | `mount.cifs` 那一串步骤的**原文**（here-doc 写凭据、0600、`credentials=`） |
| `client-commands-run.txt` | 执行记录 | 真机上**真的跑过**的那条 `smbclient` 命令（只把账号占位符换成真账号） |
| `smbclient-put-ro.txt` | 执行记录 | read 账号上传被拒的**真实** `NT_STATUS_*` 输出 |

## 这些文件里没有秘密

验收脚本第 7、8 节对它们做过比对面：密码文件（`$WORK/secrets/passwords.txt`，三个共享账号密码 + 管理员密码）
在这批文件里的命中数是 **0**。账号载荷里只有 `has_password: true`，没有密文、没有哈希、没有 `sec_` 引用；
`client-mount-steps.txt` 里的密码位置写的是 `<已填>`。

## 回放断言在哪

`web/src/components/realdata-p08-4.test.ts`：记录的键必须是 openapi `ShareRecord` 的**子集**、
三个状态分开、卷按 `volume_id` join、撤销块五字段、账号无秘密、`service` 与空列表分开、
说明里的命令与真机跑过的命令是同一条字符串。
