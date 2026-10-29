# 面板自身出问题时的恢复

这份文档只讲**面板这一层**（Web 服务、代理、数据库、账号、面板证书）出事怎么办。
磁盘、分区、挂载、fstab 的现场恢复在 [STORAGE_RECOVERY.md](STORAGE_RECOVERY.md)，那份更详细，
不要混着看。访问方式与证书核对在 [ACCESS.md](ACCESS.md)。

一条前提先说清楚：**面板不会替你备份**。0.1 没有备份与恢复功能（那是 1.0 的 P12），
所以下面凡是要“恢复备份”的地方，前提都是你自己按第 7 节做过备份。

---

## 0. 先看三样东西，再动手

```sh
# 谁在跑、退出码是多少、重启过几次
systemctl status upanel-web upanel-agent --no-pager

# 最近的日志（两个服务一起看）
journalctl -u upanel-web -u upanel-agent -n 100 --no-pager

# 配置本身是否自洽（它读配置、读证书、算允许源，不改任何东西）
sudo -u upanel-web upanelctl check-config --config /etc/upanel/upanel.json
```

`check-config` 是**只读**的，任何故障排查都可以先跑它：它会把“证书不存在”“证书和私钥不是一对”
“监听地址与 TLS 的组合不合法”“允许源为空”这类问题直接点名。

---

## 1. 忘记管理员密码

0.1 的唯一合法恢复入口是本机 CLI：

```sh
sudo -i                       # 进入 root shell（Web 账号读不到你的终端，必须由 root 读口令）
read -rsp '给 admin 设置的新密码: ' PW; echo
printf '%s\n%s\n' "$PW" "$PW" | runuser -u upanel-web -- \
  /usr/local/bin/upanelctl reset-password --config /etc/upanel/upanel.json --username admin
unset PW
```

为什么要绕这一下：`reset-password` **以 `upanel-web` 身份**执行（这样数据库文件属主才对），
而 `upanel-web` 读不到你的终端设备（`/dev/pts/*` 归登录用户与 `tty` 组，它都不是），
所以口令由 root 读、经标准输入交过去。别写成 `sudo -u upanel-web upanelctl reset-password …`
不加管道——它会卡在等你输入，而它读不到你敲的东西。

这条命令做四件事：校验新口令符合策略 → 换成新的 Argon2id 哈希 → **作废该账号的所有既有会话**
→ 往 `audit_events` 写一条 `user.password.reset`（actor 是本地 CLI，不是某个账号）。
它不会改角色、不会建号、不会启用被禁用的账号。

输出可以机器读：`--json` 打一份不含秘密的报告。

### 如果连这条命令都不行

| 情况 | 处理 |
|---|---|
| 报 `没有账号 "xxx"` | 用户名拼错；先确认库里的账号名（见下） |
| 报账号已被禁用 | 用另一个管理员在面板上启用它；**已无可用管理员**时只能恢复数据库备份（第 4 节） |
| 报数据库打不开 | 走第 4 节（数据库损坏） |
| 报口令不符合要求 | 至少 12 个字符、不能与用户名相同 |

想确认库里有谁（只读操作，不影响服务）：

```sh
sudo sqlite3 -readonly /var/lib/upanel/upanel.db \
  "SELECT id, username, role, disabled FROM users ORDER BY id"
```

（没有 `sqlite3` 就 `sudo apt install sqlite3`。这条查询只读，可以在服务运行时执行。）

---

## 2. 面板证书出问题

面板**只经 HTTPS** 提供服务，所以证书问题一律表现为“打不开”，而不是“打开了但提示不安全”。

### 2.1 启动日志说 `web.tls_cert_path` / `web.tls_key_path`

| 日志里的说法 | 含义 | 处理 |
|---|---|---|
| 指向的文件不存在 | 文件被删了、路径写错 | 放回去或改路径（第 2.3 节） |
| 证书和私钥不是一对 | 两个文件不匹配 | 换成配对的两份 |
| 已过期 / 尚未生效 | 日期不在有效期内 | 换证书（第 2.3 节） |
| 权限过宽（告警，不阻止启动） | 私钥不是 `0640 root:upanel-web` | `sudo chown root:upanel-web … && chmod 0640 …` |
| 只配了一半 | 只填了 cert 没填 key（或反过来） | 两个字段必须成对 |

面板**不会**在证书有问题时退回明文，也不会自己生成一张新证书顶上——静默降级会让
“HTTPS 到底有没有生效”变成一件没人能回答的事。

### 2.2 浏览器提示证书不受信任

自签名证书本来就会这样。核对指纹后再继续，方法见 [ACCESS.md](ACCESS.md) 第 1 节。
指纹对不上就不是“提示烦人”，是连错了机器或有人中间人——停下来。

### 2.3 证书彻底坏了：先救回能登录

```sh
sudo -i
H=$(hostname -s)

# 1) 保留旧的（别删，说不定还能追责/续期）
mv /etc/upanel/tls/panel.pem /etc/upanel/tls/panel.pem.broken-$(date +%F)
mv /etc/upanel/tls/panel.key /etc/upanel/tls/panel.key.broken-$(date +%F)

# 2) 生成临时自签名证书（与安装脚本同一套参数）
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout /etc/upanel/tls/panel.key -out /etc/upanel/tls/panel.pem -days 90 -sha256 \
  -subj "/CN=$H" -addext "subjectAltName=DNS:$H,DNS:localhost,IP:127.0.0.1"
chown root:upanel-web /etc/upanel/tls/panel.pem /etc/upanel/tls/panel.key
chmod 0644 /etc/upanel/tls/panel.pem
chmod 0640 /etc/upanel/tls/panel.key

# 3) 校验 → 重启 → 核对
runuser -u upanel-web -- upanelctl check-config --config /etc/upanel/upanel.json
systemctl restart upanel-web
openssl x509 -in /etc/upanel/tls/panel.pem -noout -fingerprint -sha256
```

然后把新指纹记下来，按第 2.1 节换成正式证书。

### 2.4 站点证书（nginx 那边）

站点证书和面板证书是两套材料（见 [ACCESS.md](ACCESS.md) 第 5 节）。站点错误不会让面板登不进去
——这本身就是 P10 的验收条件之一（代理出错，面板仍可登录）。站点证书的处理在
[STORAGE_RECOVERY.md](STORAGE_RECOVERY.md) 之外的面板页面里：`代理 → 站点 → 证书`，
看到 `PROXY_TLS_SERVED_STALE` 表示 nginx 实际服务的还是旧证书，需要 reload
（应用站点时会自动尝试一次，仍不一致就说明有别的进程或配置在抢 443）。

如果任务报告里出现「**问不到 nginx.service 的状态**（连问 3 次都没有得到一个可用的答案）」，
意思是面板**没有**替你 reload：配置已经写对，但它手上没有证据说明这份配置被加载了，所以没往
正在跑的服务上发信号。原因通常是这台机器上的 `systemctl` 太慢（2 vCPU 虚机上实测过单次
11.4 秒、16.5 秒），三次都在 10 秒预算内没答上来。手工收尾即可：

```sh
systemctl is-active nginx && systemctl reload nginx   # 只 reload，不 restart
```

报告里的 `observed.unit_state_error` 带着每一次探测的耗时与原因，`observed.unit_state_attempts`
是实际问了几次。同一份报告里 `served=false`：
这只说明「此刻没有证据」，不等于配置错了。

---

## 3. 代理不工作（`AGENT_UNAVAILABLE`）

症状：能登录、能看页面，但所有系统类功能都报代理不可用，或长时间转圈后失败。

按这个顺序查：

```sh
# 1) 代理单元活着吗
systemctl status upanel-agent --no-pager
journalctl -u upanel-agent -n 50 --no-pager

# 2) socket 在不在、属主对不对（应为 root:upanel-web 0660）
ls -l /run/upanel/agent.sock
ls -ld /run/upanel

# 3) Web 账号能不能走到它（这一步不做任何特权动作）
sudo -u upanel-web test -r /run/upanel/agent.sock && echo 'socket 可读'
sudo -u upanel-web test -w /run/upanel/agent.sock && echo 'socket 可写'

# 4) 两个服务是不是同一个配置
systemctl cat upanel-agent | grep ExecStart=
systemctl cat upanel-web   | grep ExecStart=
```

| 现象 | 原因 | 处理 |
|---|---|---|
| 代理单元 `failed`，日志 `拒绝启动: 必须以 root 运行` | 单元被改过 | 检查 `User=root` |
| 日志 `拒绝启动: 配置错误` | 代理的 `ExecStartPre` 跑了 `check-config` 并失败 | 先修 `check-config` |
| 日志 `密钥文件不存在` | `agent.key` 丢了 | 见下（**不可随手重建**） |
| socket 不存在 | 代理没起来 | 先让代理起来，它自己会建 socket |
| socket 属组不是 `upanel-web` | `agent.socket_group` 与账号不匹配 | 改配置后重启代理 |
| Web 日志 403 `peer_uid` | 调用方不是 `upanel-web` | 检查 Web 单元的 `User=` |

### 关于 `agent.key`

它是保存秘密的加密密钥。**丢了就找回不了**已经存进去的秘密（共享口令、证书私钥引用），
重新生成一个只能让面板从“所有秘密都读不出”变成“能写新的秘密”，旧的永远打不开。

所以：密钥丢了就用备份恢复它。确实没有备份时，删掉/移走旧密钥再执行
`sudo upanel-agent -init-key --config /etc/upanel/upanel.json`，然后**把所有共享与应用凭据重新录一遍**。

### 代理数据库（`agent.db`）坏了

```sh
sudo systemctl stop upanel-agent
sudo cp -a /var/lib/upanel-agent/agent.db /root/agent.db.broken-$(date +%F)
sudo sqlite3 /var/lib/upanel-agent/agent.db "PRAGMA integrity_check"
```

`integrity_check` 不返回 `ok` 就恢复备份（第 4 节）。没有备份时的最小代价：删掉
`agent.db` 让代理重建一个空库——运维记录（操作历史）与秘密全部丢失，页面上的历史任务会查不到。

---

## 4. 数据库损坏或迁移失败

### 4.1 判断

```sh
systemctl stop upanel-web
sudo sqlite3 /var/lib/upanel/upanel.db "PRAGMA integrity_check;"
sudo -u upanel-web upanelctl migrate --config /etc/upanel/upanel.json
```

| 输出 | 含义 | 处理 |
|---|---|---|
| `ok` | 库是好的 | 问题不在这里，回去看日志 |
| `database disk image is malformed` | 文件被截断/损坏 | 4.3 恢复备份 |
| `database is locked` | 还有进程占着 | 确认两个服务都停了；`fuser -v /var/lib/upanel/upanel.db` |
| `store: 数据库 schema 版本为 N，高于程序内置的最高版本 M；请升级程序而不是降级数据库` | 用旧二进制开了新库 | 换回配套版本的二进制，或恢复 4.3 的备份 |
| `store: 迁移 00XX_xxx 的内容与已应用版本不一致（校验和不同）` | 迁移文件被改过 | 用发布包里的原始 `migrations`（不要手改迁移文件） |

### 4.2 手工重建（只在库还读得出来时）

万一迁移卡住，可以先把数据导出成 SQL 再导入一个空库：

```sh
sudo systemctl stop upanel-web
sudo sqlite3 /var/lib/upanel/upanel.db ".backup '/root/web-db-backup.db'"
sudo sqlite3 -readonly /var/lib/upanel/upanel.db ".dump" > /root/upanel-dump.sql
```

拿到 `upanel-dump.sql` 之后就能保住账号与审计，重建流程是：移走旧库 → `upanelctl migrate`
建新库 → 把 dump 里的 `INSERT` 按表导回去。**这一步请在恢复演练里做过再用**，
不要在唯一的故障现场第一次尝试。

### 4.3 从备份恢复

```sh
sudo systemctl stop upanel-web upanel-agent
B=/root/upanel-backup-<那天的日期>          # 见 INSTALL.md 第 7 节；backup-state.sh 的布局是
                                            # $B/etc-upanel、$B/panel-db、$B/agent-db

# 面板库：主库 + WAL + shm 三个文件必须一起回来，否则一致性没有保证
sudo rm -f /var/lib/upanel/upanel.db /var/lib/upanel/upanel.db-wal /var/lib/upanel/upanel.db-shm
sudo cp -a "$B/panel-db/." /var/lib/upanel/
sudo chown -R upanel-web:upanel-web /var/lib/upanel
sudo chmod 0640 /var/lib/upanel/upanel.db

# 代理库（如果它也坏了）
sudo rm -f /var/lib/upanel-agent/agent.db*
sudo cp -a "$B/agent-db/." /var/lib/upanel-agent/
sudo chown root:root /var/lib/upanel-agent/agent.db
sudo chmod 0600 /var/lib/upanel-agent/agent.db

# 配置与密钥（换了机器、或 /etc/upanel 也丢了才需要）——注意里面是 root 独占的密钥
sudo cp -a "$B/etc-upanel/." /etc/upanel/
sudo chown -R root:root /etc/upanel/agent.key && sudo chmod 0600 /etc/upanel/agent.key

sudo systemctl start upanel-agent upanel-web
```

恢复之后必须做的两件事：核对账号能登录（可能需要第 1 节重置口令，因为备份里的口令是当时的），
以及接受“备份时间点之后的操作记录没有了”。

### 4.4 面板库没了会失去什么

| 丢了 | 还在 |
|---|---|
| 账号与会话、审计历史、任务记录 | 系统上的 nginx 配置文件 |
| 代理站点/证书登记、应用登记与分组 | `/etc/ssl/upanel` 里的证书与私钥 |
| 面板的口令（需要重新 `init-admin`） | `/srv/upanel` 里的站点与应用数据 |
| | 已挂载的卷与 `/etc/fstab`（见 STORAGE_RECOVERY.md） |

也就是说：库丢了不等于机器坏了，但**面板会变成一台“什么都不知道”的面板**——
它不会去猜现有 nginx 配置属于谁，需要你重新登记。

---

## 5. fstab 写错、开机起不来、磁盘操作中断

这些属于存储写操作，有单独的一份现场手册，别在这里找答案：

- fstab 托管块结构、回滚、开机起不来 → [STORAGE_RECOVERY.md](STORAGE_RECOVERY.md) 第 3、5 节
- 卸载不掉、谁占着挂载点 → 同上第 2 节
- 磁盘操作中途断电/重启后的核对顺序 → 同上第 2 节「出事时的排查顺序」
- 格式化**没有**自动恢复保证（PLAN 明确写了），只能靠你自己的备份

面板在写 fstab 之前会算哈希、写候选文件、`findmnt --verify` 校验、再原子 rename，
写前发现文件被外部改过会直接拒绝（`storage: /etc/fstab 已被外部修改`）。
这些保护都在，但它们保护的是“写坏”，不是“写对之后你又手工改坏了”。

---

## 6. 0.1 **没有**恢复路径的几件事（别指望）

如实列出，避免在故障现场浪费时间：

| 场景 | 0.1 的状态 | 现在怎么办 |
|---|---|---|
| 网络配置改错后的自动回滚 | 未实现（PLAN 第 19 节，1.0 的 P14） | 0.1 不提供任何网络写操作，所以面板不会把你的网络改坏；手工改坏就按正常方式进单用户/救援模式修 |
| APT 中断（`dpkg` 半配置） | 未实现（1.0 的 P11 才管软件更新） | 面板不执行 apt，安装脚本只在 `--install-deps` 时装点名的包；中断后用标准做法 `sudo dpkg --configure -a` 收拾 |
| 在线更新与回退 | 未实现（1.0 的 P11） | 见 INSTALL.md 第 7 节的手工升级步骤 |
| 备份与快照 | 未实现（1.0 的 P12） | 自己做第 7 节的备份 |
| 定时任务与告警 | 未实现（1.0 的 P12 / P13） | 自己用 cron / 系统监控 |

把这几条写在文档里，比让用户在故障时去界面上找一个不存在的按钮要好。

---

## 7. 该备份什么（怎么动手）

从 P16.1 起，安装器与卸载器都会在动数据库之前自动备份（见 INSTALL.md 第 7 节），
单独做一次备份用同一个脚本：

```sh
sudo sh /path/to/deploy/scripts/backup-state.sh --to /var/backups/upanel/$(date +%F)
#   → etc-upanel/（配置 + 面板证书 + agent.key）、panel-db/、agent-db/、MANIFEST.sha256
```

它会停服务、把两个库连同 `-wal`/`-shm` 一起拷、再起回服务，并写一份 sha256 清单；
机器上有 `sqlite3` 时还会对每份副本跑 `PRAGMA integrity_check`（没有就如实说“未校验”）。
放进 cron 时建议在它前面加 `systemctl is-active` 的判断，避免与别的手工操作撞车。

要自己做而不用脚本，最小的一套是这个样子：

```sh
B=/var/backups/upanel/$(date +%F)
sudo mkdir -p "$B"
sudo cp -a /etc/upanel "$B/etc-upanel"                      # 配置 + 面板证书 + 代理密钥
sudo cp -a /var/lib/upanel/upanel.db* "$B/"                  # 面板库（含 WAL/shm）
sudo cp -a /var/lib/upanel-agent/agent.db* "$B/"             # 代理库
sudo cp -a /etc/ssl/upanel "$B/ssl-upanel"                   # 站点证书
sudo cp -a /etc/fstab "$B/fstab"                             # 存储写操作之前的对照
```

服务运行中直接拷 SQLite 文件**可能拿到不一致的快照**，所以两条路选一条：
停服务再拷（简单、会中断几秒），或者装 `sqlite3` 用 `.backup`（在线一致性备份）：

```sh
sudo sqlite3 /var/lib/upanel/upanel.db ".backup '$B/upanel.db'"
```

`/etc/upanel/agent.key` 是这套备份里最要紧的一份：它丢了，两个数据库里的密文就再也不可解。
把它放到与数据库**不同**的地方，并且只给 root 读。
