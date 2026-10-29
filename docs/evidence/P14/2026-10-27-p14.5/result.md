# P14.5 验收结果：受管理 Linux 账号、authorized_keys 托管区、受限 sudo 模板

**日期**：2026-10-27（VM 内时间 04:30:0xZ — 04:38:40Z，UTC）
**机器**：Ubuntu 22.04.5 LTS / kernel 5.15.0-198-generic，QEMU，宿主 SSH 转发 `127.0.0.1:2223`
**结果**：**PASS 168 / FAIL 0 / NOT RUN 6**，退出码 0（第三轮）
**脚本**：`tests/integration/p14-5-acceptance.sh`，sha256 `c2a5236a32c7b6672b140768685fcec65105a2bb3ab374f1be26798a6a45e6ab`
**二进制**：`upanel-agent 402564380d…7bbc5f9b9a`、`upanel-web 5edd2bbb04…227a7f24ff`、`upanelctl ef69fa7362…258245df3f44`（宿上交叉编译，VM 内 `sha256sum` 逐个核对一致，读数见 `facts/binaries-sha256.txt`）

---

## 1. 结论

**P14.5 的验收点在真机上成立。** STAGES §16 对这一项的要求是「受管理 Linux 账号、SSH
公钥、受限 sudo 模板；禁止修改 root/系统账号和面板身份提权」，逐条对应：

1. **口令不进 argv、不进日志**：口令先交给 `POST /secrets`（`purpose=account_password`）
   拿回 `sec_` 引用，任务参数里只有引用；执行器把明文从标准输入交给 `chpasswd`。
   验收断言的是结果而不是面板自述——`passwd -S` 读回 `P`。
2. **uid 由系统分配**：`getent passwd` 读回 `uid=1002`（落在 1000–59999），home 在
   `/home` 之下且真的建出来了，登录壳是 `/bin/rbash`（`restricted` 档）。
3. **托管区只碰自己那几行**：`authorized_keys` 里先放一条人手写的外来钥匙，加一把、加到
   两把、删一把三次操作之后那一条**逐字节仍在**；文件 `0600`、`.ssh` `0700`；
   `ssh-keygen -lf` 读回的指纹就是登记表里那把。
4. **受限 sudo 四道门禁都真的走了一遍**：规则文件在 `/etc/sudoers.d/upanel-alice`、
   权限 `0440`、带认领标记、内容里只有模板那一条命令且无通配符；整份 `visudo -c` 通过；
   `sudo -l -U` 里**恰好只有那一条**；然后**真的跑**——`runuser` 换到 alice，`sudo -n`
   执行模板命令退出码 0，模板外的命令被 sudo 自己拒。
5. **真机制造一次 visudo 失败，旧规则逐字节保住**：放一个坏文件让整份 `visudo -c` 失败，
   此时给 alice 换模板——候选文件自己合格、rename 已经发生，整份校验失败 → 旧文件哈希与
   失败前一致、内容逐字节没变、没有残留候选临时文件、任务失败原因带 `SUDO_VISUDO_FAILED`；
   移走坏文件再应用 → 成功。移走前后各断言一次，才证明失败是那个坏文件造成的。
6. **面板不覆盖别人的文件**：手工把 `upanel-alice` 改成合法但没有认领标记的内容（文件本身
   能过 `visudo -c`，所以拒绝只能来自标记），预检与删除都被拒（`SUDO_FILE_DAMAGED`），
   文件逐字节保留；复原之后 `sudo -l -U` 又读到面板那条规则。
7. **禁用是换壳不是 `-L`**：禁用之后登录壳是 `/usr/sbin/nologin`，而口令状态**仍然是 `P`**
   （没有 `usermod -L`），已配的公钥文件没被带走；启用之后壳回到 `/bin/rbash`。
8. **删账号不删数据**：`userdel` 不带 `-r`，home 与里面的文件一个不少，sudoers 文件跟着撤掉。
9. **去掉了文档里一条反了的断言**（Q479）：首轮 §11 断言"没有口令的账号配了公钥也登不上"，
   真机登得上去。现在这一节分四面，把"能登/登不上"分别归因到**钥匙**和**壳**：
   key-only 账号能登 → 面板禁用（换壳）后**同一把钥匙被拒**，sshd 原文
   `This account is currently not available.` → 再启用又能登（把拒绝归因到壳，而不是
   sshd 的配置）→ 撤掉公钥后登不上（托管区真在生效）。

**这一轮跑了三次才绿，三次里真正的产品缺陷只有两个**（Q477 的 json 标签、Q478 的能力判据
挂错模块），一次是环境（`/etc/sudoers.d/upanel` 的权限），剩下的红全部来自验收脚本与
**文档里一句说反了的断言**。贵的不是写代码，是把每一条红查到根——详见第 3、4 节。

---

## 2. 这一轮验的范围

脚本 13 节、168 条断言（`logs/run.log`，`RESULT`）：

| 节 | 断言 | 验的是什么 |
|---|---|---|
| 0 环境与起点 | 21 | root、三个二进制、夹具工具、`visudo -c` 起点干净、上一轮的 `upanel-*` 与账号先清掉 |
| 1 配置与启动 | 10 | 代理以 root 起、Web **不以 root** 起、socket 就位 |
| 2 能力报告 | 8 | 这台机器上账号能力 `available`；**不含任何 Samba 工具**（Q478 的回归）；列表读的是同一张 `managed_accounts` |
| 3 建普通账号 | 26 | 口令经秘密通道、`passwd -S=P`、uid 区间、home、壳档、登记版本 |
| 4 authorized_keys 托管区 | 31 | 外来钥匙逐字节不动、托管区指纹 = 登记表、0600/0700、加/换/删三次 |
| 5 受限 sudo | 14 | 文件 0440 + 认领标记 + 无通配符、`visudo -c`、`sudo -l -U` 恰好一条、**真跑** + 模板外被拒 |
| 6 制造 visudo 失败 | 13 | 旧规则哈希/内容/无残留 + `SUDO_VISUDO_FAILED`，移走坏文件后成功 |
| 7 手改过的文件 | 7 | `SUDO_FILE_DAMAGED`，文件逐字节保留，复原后规则仍在 |
| 8 禁用 / 启用 | 9 | 换壳不是 `-L`（`passwd -S` 仍 `P`）、钥匙没被带走、`shell_profile` 仍记着 `restricted` |
| 9 第二个账号 | 6 | 没有口令的账号：`has_password=false`、登记与磁盘一致 |
| 10 删账号 | 9 | `userdel` 不带 `-r`、home 内容一个不少、sudoers 跟着撤 |
| 11 公钥登录（有条件） | 10 | key-only 能登 / 禁用后被拒 / 启用后又行 / 撤钥匙被拒 |
| 12 收尾 | 4 | `sudoers.d` 里没有残留、整份 `visudo -c` 通过、账号列表回到干净 |

宿主侧检查（`logs/check-run.txt` 是 `scripts/check.sh` 的完整输出，`logs/host-check.txt`
是逐包细读）：`scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**、退出码 0——line-endings
134 个文件全 LF、go **48 包 ok + 3 个 no test files**、gofmt 干净、Linux 交叉构建与 vet 过、
web **57 文件 1116 用例**、`web-smoke` **10/0/0**、openapi valid + 6 warnings（与基线一致）。
另外单独跑过 `go test -count=1` 的五个包（accounts / agent / agentstore / api / shares）全 `ok`。

---

## 3. 三轮才绿：红在哪

| 轮次 | 结果 | 红的根因 |
|---|---|---|
| 第一轮 | 63 PASS / 111 FAIL | ① 环境：`/etc/sudoers.d/upanel` 是 `0644`，`visudo -c` 整份 rc=1；② 产品 Q477：`accounts.Definition` 一个 json 标签都没有，`display_name` 直接 400；③ 产品 Q478：`GET /accounts` 拿 Samba 能力当账号判据，这台机器没装 Samba → `blocked` → 页面禁用新建 |
| 第二轮 | 162 PASS / 3 FAIL | 四条都是脚本/口径：`password_changed` 只在改账号时出现（建号读的是 `password_state`）；`ssh-keygen -lf` 一行一把，被当单值比；§11 断言"没口令就登不上"与真机相反（Q479） |
| 第三轮 | **168 PASS / 0 FAIL / 6 NOT RUN** | — |

---

## 4. 逐条

### 4.1 环境缺陷（不是产品问题）

`/etc/sudoers.d/` 下任何文件权限不是 `0440`，`visudo -c` 就整份 rc=1。首轮手写的
`upanel` 文件是 `0644`，于是第 5 节起全红。归档到 `/var/tmp/p145-envfix/upanel.badmode`
后删除，`visudo -c` 回到 rc=0，`sudo -n true` 仍通（cloud-init 的
`/etc/sudoers.d/90-cloud-init-users` 保证这一点）。写进 `docs/TEST_ENVIRONMENT.md` §8.12：
**验收环境里不许留非 0440 的 sudoers 文件**，以及"这台 VM 没装 Samba，账号能力就应该是
`available`，不要用装 Samba 绕开"。

### 4.2 产品缺陷（见 DECISIONS）

- **Q477**：`accounts.Definition` 六个字段全无 json 标签。代理用 `DisallowUnknownFields`，
  encoding/json 的大小写不敏感只对整词有效——`name` 能匹配 `Name`，`display_name` /
  `password_secret_ref` / `shell_profile` 一个都匹配不上。修完补了从**线格式**出发的
  `internal/agent/accountaction_test.go`，并对标签做过变异核验。
- **Q478**：账号与 Samba 是两个不相交的工具集，`GET /accounts` 却返回 Samba 的能力。
  修法是照 P14.4 给 NFS 单开一份判定的成例，拆出 `accounts.Capability` 与只读查询
  `accounts.capability`，Web 侧 `AccountCapability` / `ShareCapability` 两个类型。

### 4.3 脚本缺陷

- `expect_eq "执行器读回 password_changed"` → 建号的结果里根本没有这个键（它只在改账号时
  出现），改成 `password_state=P` 与 `has_password=true`。
- `ssh-keygen -lf` 文件里有多把钥匙时一行一把，原来当单值比；新增 `fp_listed()` 做包含判断。

### 4.4 口径错误（Q479）

`ACCOUNT_NO_PASSWORD` 的四处文案（`api/accounts.ts`、`AccountsView.vue` 三处、
`utils/jobresult.ts`、`CONTRACTS.md` §13）都写着"即使配了公钥也登不上"，并且自认"未验证"。
真机读数：publickey 认证不看 shadow 的口令字段，key-only 账号能登。全部改成"只能用已配的
公钥登录 / 要拦人就禁用"，并在 `accounts.test.ts`、`jobresult.test.ts` 加了钉住的用例。

---

## 5. NOT RUN（六条，都在设计范围内）

| 项 | 原因 |
|---|---|
| 共享账号不能拿 sudo | 没有 `pdbedit`/`smbpasswd`（`samba-common-bin` 未安装）；不为一条验收去装包（PLAN §10） |
| 共享账号 + Samba passdb 那一半 | 同上；共享账号的 SMB 认证属于 P14.3 的验收面 |
| 两条公钥动作并发写同一份 `authorized_keys` | 锁行为由 `internal/accounts/sshkey_test.go` 与 shares 的锁单测覆盖；真机并发会把冲突变成时序问题 |
| 第 11 把公钥被拒（上限 10） | 上限判定在判决层，单测覆盖；真机验到 2 把即可 |
| home 在别处 / home 是符号链接的账号 | 面板对这类账号是拒绝写（`SSH_KEY_HOME_INVALID`），只在单测里造过；真机没有造这种磁盘布局 |
| 面板自己的 TOTP 与登录策略 | 属于 P14.6 |

---

## 6. 证据文件

```
docs/evidence/P14/2026-10-27-p14.5/
├── RESULT                      # PASS=168 FAIL=0 NOT RUN=6
├── logs/
│   ├── run.log                 # 全量 13 节输出（204 行）
│   ├── agent.log               # 代理自己记的每一步
│   ├── web.log                 # Web 侧
│   ├── sudoers-alice-good      # 门禁二通过后的规则文件原件
│   └── sudoers.d-initial/      # 起点留档
├── facts/
│   ├── ssh-bob-keyonly.out     # 面一：能登（ssh 自己什么都没说）
│   ├── ssh-bob-disabled.out    # 面二：This account is currently not available.
│   ├── ssh-bob-enabled.out     # 面三：又能登
│   ├── ssh-bob-nokey.out       # 面四：撤钥匙后被拒
│   ├── sudo-status.out         # 真跑的 sudo status 输出
│   ├── sudo-journal.out        # 真跑的 sudo journalctl 输出
│   ├── sudo-restart.out        # 模板外命令被 sudo 拒的原文
│   ├── sudoers.d-final.txt     # 收尾时 sudoers.d 的 ls
│   ├── binaries-sha256.txt     # VM 内对三个二进制与脚本的 sha256（与本文件首页一致）
│   └── human-uids-final.txt    # 收尾时 1000–59999 的账号（只剩验收机原有的 upanel、p12sftp）
├── logs/check-run.txt          # 宿主侧 scripts/check.sh 全量输出（PASS 14 / FAIL 0 / NOT RUN 0）
└── logs/host-check.txt         # 宿主侧逐包细读（gofmt/vet/build/go test/typecheck/lint/vitest）
```

---

## 7. 还没做的事

- 面板自己的 TOTP、恢复码、启用确认 → P14.6。
- 共享账号那半边的真机验收需要一个装了 `samba-common-bin` 的环境；本轮按 PLAN §10 不装包。
- 三条 NOT RUN 里能被单测覆盖的已经覆盖，剩下的（并发、上限 10、home 异位）留作已知的
  真机空白，不折算成"已验证"。
