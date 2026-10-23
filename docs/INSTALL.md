# 安装（0.1 核心单机版）

这份文档讲怎么把一个 uPanel 发布包装到一台 Ubuntu 服务器上、装完怎么核对、出问题怎么重入。
面板装好之后**怎么访问**见 [ACCESS.md](ACCESS.md)，坏了怎么救见 [RECOVERY.md](RECOVERY.md)。

---

## 1. 适用范围

| 项 | 支持的范围 | 超出范围时的表现 |
|---|---|---|
| 发行版 | Ubuntu 22.04 LTS、24.04 LTS | 安装脚本直接拒绝（`0.1 只支持 Ubuntu 22.04 与 24.04`） |
| 架构 | `x86_64`（amd64）、`aarch64`（arm64） | 架构不符时脚本在执行二进制探测那一步拒绝 |
| init | systemd | 没有 `/run/systemd/system` 就拒绝 |
| 磁盘 | `/usr` 与 `/var` 各留 300 MiB | 不足则拒绝 |
| 端口 | 默认 9443/TCP | 被占用则拒绝，并打印占用者 |
| 权限 | 需要 root（`sudo`） | 非 root 直接拒绝 |
| 反向代理 | 可选 nginx（P10 的站点管理需要） | 缺 nginx 不影响面板启动，只是代理功能显示“依赖未安装” |

0.1 面向**一台**服务器：面板与代理同机，代理只监听 Unix socket。多机管理不在 0.1 范围内。

---

## 2. 你会拿到什么

一次发布产出两个包（每个架构一个）和一份发布级校验和：

```
dist/
├── upanel-<版本>-linux-amd64.tar.gz
├── upanel-<版本>-linux-arm64.tar.gz
└── SHA256SUMS                 ← 上面两个包的校验和
```

解开之后的目录结构（`upanel-<版本>-linux-<架构>/`）：

```
bin/upanel-web                  面板（以 upanel-web 身份运行，不是 root）
bin/upanel-agent                特权代理（以 root 运行，只监听 Unix socket）
bin/upanelctl                   本机 CLI：check-config / migrate / init-admin / reset-password
deploy/systemd/*.service        两个 systemd 单元
deploy/scripts/install.sh       安装脚本
deploy/scripts/uninstall.sh     卸载脚本
deploy/upanel.example.json      配置样例（回环 + 无 TLS 的开发用样例）
docs/                           安装、访问、恢复、本机启动说明
SHA256SUMS                      包内每个文件的校验和
VERSION                         本次发布的版本号
```

二进制是 `CGO_ENABLED=0` 交叉编译的静态可执行文件，所以同一个 amd64 包在 22.04 与 24.04 上通用。

---

## 3. 先校验，再解包

```sh
# 1) 校验发布包本身（SHA256SUMS 与 tar.gz 必须放在同一个目录）
sha256sum -c SHA256SUMS

# 2) 解包
tar xzf upanel-0.1.0-linux-amd64.tar.gz
cd upanel-0.1.0-linux-amd64

# 3) 校验包内每个文件（二进制、单元、脚本、文档）
sha256sum -c SHA256SUMS
```

第 1 步确认“传输与存储没把包弄坏”，第 3 步确认“包里每个字节就是我们发布的那份”。
两步都不通过就别往下走——安装脚本自己也会在开头跑第 3 步，并且**不提供跳过**之外的默认放宽。

我们不发 `curl | bash` 这种一步安装命令：安装器必须在你的机器上被读过一遍再执行。

---

## 4. 读一遍安装脚本（它做什么、不做什么）

`deploy/scripts/install.sh` 是 POSIX `sh`，没有隐藏的下载步骤、没有 `eval`、没有从网络拉任何东西。
它按固定顺序做 12 件事，每件事都会打印 `[n/12]` 与结论：

| 步骤 | 做什么 |
|---|---|
| 1 | 校验发布包（`sha256sum -c SHA256SUMS`） |
| 2 | 探测系统：发行版、架构、systemd、端口占用、磁盘空间，并试跑一次 `upanelctl version` 确认二进制能执行 |
| 3 | 按功能列出缺失依赖（nginx / certbot / samba / docker / smartmontools），**默认不装** |
| 4 | 建两个系统账号：`upanel-web`（无登录 shell、无 sudo）、`upanel-shell`（有真 shell、口令锁定、无 sudo） |
| 5 | 建目录与权限（见第 6 节表格） |
| 6 | 生成面板自签名证书（ECDSA P-256），并打印 SHA-256 指纹 |
| 7 | 写 `/etc/upanel/upanel.json`，然后跑 `upanelctl check-config` 当裁判 |
| 8 | 生成代理秘密密钥 `/etc/upanel/agent.key`（已存在则跳过，**绝不覆盖**） |
| 9 | 装二进制与文档，以 `upanel-web` 身份执行数据库迁移 |
| 10 | 创建第一个管理员（密码只从标准输入读） |
| 11 | 装 systemd 单元，`systemd-analyze verify` → `enable` → `restart` |
| 12 | 健康检查：HTTPS（用安装的证书做 CA 校验）、服务端证书指纹比对、`upanel-web` 不是 root |

它**不做**的事：不 `apt upgrade`、不删任何文件、不改 `sshd`、不动防火墙、不装 `sudoers` 条目、
不覆盖已存在的配置或密钥、不创建任何带默认口令的账号。

任何一步失败，脚本会打印：已经完成的步骤（这些不需要重做，脚本是幂等的）、一条重跑命令、
以及该看哪几行日志。

---

## 5. 依赖：按功能装，不一次装满

安装器只报告，不擅自安装。缺依赖不影响面板启动，面板会在对应页面显示“依赖未安装”及需要的软件包。

| 功能 | 需要 | apt 包 |
|---|---|---|
| 反向代理 / HTTPS 站点 / ACME | nginx、certbot | `nginx`、`certbot` |
| 文件共享 | Samba | `samba` |
| 容器应用 | Docker | `docker.io` |
| 磁盘健康（SMART） | smartctl | `smartmontools` |

要装就显式点名：

```sh
sudo sh deploy/scripts/install.sh --source . --install-deps nginx,certbot
```

带 `--install-deps` 时脚本只做 `apt-get update` + `apt-get install -y --no-install-recommends <点名的包>`，
不升级系统、不删除任何软件包。

---

## 6. 执行安装

```sh
sudo sh deploy/scripts/install.sh --source .
```

交互式安装会在第 10 步提示输入管理员的密码（不回显，要求至少 12 个字符，且不能与用户名相同）。
自动化/无人值守时用 `--password-stdin` 从标准输入给两行：

```sh
printf '%s\n%s\n' "$PW" "$PW" | sudo sh deploy/scripts/install.sh --source . --password-stdin
```

密码不会出现在命令行参数（`ps` 看不到）、不会进日志、不会进 shell 历史——它只经标准输入
进入 `upanelctl init-admin`，最终以 Argon2id 哈希落库。

常用参数：

| 参数 | 含义 |
|---|---|
| `--source DIR` | 发布包根目录（含 `bin/` 与 `SHA256SUMS`），默认 `.` |
| `--listen ADDR` | 面板监听地址，默认 `127.0.0.1:9443` |
| `--admin-user NAME` | 第一个管理员用户名，默认 `admin` |
| `--admin-display NAME` | 显示名，默认与用户名相同 |
| `--password-stdin` | 从标准输入读两行密码（自动化用） |
| `--install-deps LIST` | 逗号分隔，见第 5 节 |
| `--write-config` | 覆盖已存在的配置文件（先备份到 `upanel.json.bak.<时间戳>`） |
| `--skip-checksums` | 跳过校验和，只用于反复调试安装器 |

### 安装产出的账号、目录与权限

| 路径 | 属主 | 权限 | 内容 |
|---|---|---|---|
| `/usr/local/bin/upanel-web`、`/usr/local/bin/upanelctl` | root:root | 0755 | 面板与 CLI |
| `/usr/local/lib/upanel/upanel-agent` | root:root | 0755 | 特权代理 |
| `/usr/local/share/doc/upanel/` | root:root | 0755 | 本机文档（安装、访问、恢复） |
| `/etc/upanel/` | root:root | 0755 | 配置目录本身 |
| `/etc/upanel/upanel.json` | root:upanel-web | 0640 | 配置：Web 能读、不能改 |
| `/etc/upanel/agent.key` | root:root | 0600 | 代理秘密密钥，**必须离线备份** |
| `/etc/upanel/tls/` | root:upanel-web | 0750 | 面板证书目录 |
| `/etc/upanel/tls/panel.pem` | root:upanel-web | 0644 | 面板证书 |
| `/etc/upanel/tls/panel.key` | root:upanel-web | 0640 | 面板私钥 |
| `/var/lib/upanel/` | upanel-web:upanel-web | 0750 | 面板数据库 `upanel.db` |
| `/var/lib/upanel-agent/` | root:root | 0700 | 代理数据库 `agent.db`（Web 账号进不去） |
| `/var/lib/upanel-shell/` | upanel-shell:upanel-shell | 0700 | 终端账号的 home |
| `/run/upanel/agent.sock` | root:upanel-web | 0660 | 代理 socket（由代理单元建立） |
| `/srv/upanel/` | root:root | 0755 | 站点与应用数据根（文件管理器的唯一根） |
| `/etc/ssl/upanel/` | root:root | 0700 | 签发/导入的证书（面板证书不放这里） |
| `/var/www/upanel-acme/` | root:root | 0755 | ACME HTTP-01 校验目录 |

`/etc/upanel` **刻意**不交给 systemd 的 `ConfigurationDirectory=` 管理：一旦那样声明，
systemd 会把目录属主改成 `upanel-web`，Web 账号就能替换自己的配置与证书私钥。
现在它归 root，配置 0640 `root:upanel-web`——能读、不能改。

### 装完自检

```sh
# 配置是否自洽（含证书加载、允许源、监听地址与 TLS 的组合）
sudo -u upanel-web upanelctl check-config --config /etc/upanel/upanel.json

# 两个服务是否在跑
systemctl status upanel-web upanel-agent --no-pager

# 面板是否真的在 HTTPS 上应答（用安装的证书做 CA，不加 -k）
curl -sS --cacert /etc/upanel/tls/panel.pem https://127.0.0.1:9443/api/v1/health/live

# 服务端证书指纹是否就是安装输出的那一串
echo | openssl s_client -connect 127.0.0.1:9443 -servername "$(hostname -s)" 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256

# Web 进程不能是 root
ps -o user=,pid=,cmd= -p "$(systemctl show -p MainPID --value upanel-web)"

# 代理 socket 的属主与权限
ls -l /run/upanel/agent.sock
```

---

## 7. 重入、升级与卸载

### 重入

安装器是幂等的，可以直接重跑：

- 已存在的账号：保留（不重置、不改 shell）
- 已存在的目录：保留并**重新校正权限与属主**
- 已存在的证书：保留
- 已存在的 `/etc/upanel/agent.key`：保留（覆盖会让已保存的秘密永久解不开）
- 已存在的配置文件：保留（要覆盖请加 `--write-config`）
- 数据库里已有账号：跳过创建管理员

所以“装到一半失败”的正确做法是：看脚本打印的已完成步骤 → 修掉那一处 → 重跑同一条命令。

### 升级

**0.1 不提供在线更新**（更新与回滚在 1.0 的 P11 才实现，见 README「1.0 尚未实现的功能」）。
手动升级请按下面的顺序，任何一步失败都停下来：

```sh
# 1) 备份：配置、面板库、代理库（WAL 与 shm 必须跟主库一起拷，所以先停服务）
sudo systemctl stop upanel-web upanel-agent
B=/root/upanel-backup-$(date +%F)
sudo mkdir -p "$B/web-db" "$B/agent-db"
sudo cp -a /etc/upanel "$B/etc-upanel"
sudo cp -a /var/lib/upanel/upanel.db* "$B/web-db/"
sudo cp -a /var/lib/upanel-agent/agent.db* "$B/agent-db/"

# 2) 校验新包 → 解包 → 重跑安装脚本（会替换二进制并执行迁移）
sudo sh upanel-<新版本>-linux-amd64/deploy/scripts/install.sh --source upanel-<新版本>-linux-amd64

# 3) 确认
sudo -u upanel-web upanelctl check-config --config /etc/upanel/upanel.json
curl -sS --cacert /etc/upanel/tls/panel.pem https://127.0.0.1:9443/api/v1/health/live
```

数据库迁移**只向前**：`upanelctl migrate` 在发现“库里的 schema 比二进制新”时会拒绝运行，
所以用旧二进制回退既要换回程序、也要恢复上面第 1 步的数据库备份。0.1 不声称支持自动降级。

### 卸载

```sh
sudo sh deploy/scripts/uninstall.sh                 # 停服务、删程序与单元，保留配置与数据
sudo sh deploy/scripts/uninstall.sh --purge-config  # 另删 /etc/upanel（含代理密钥，会二次确认）
sudo sh deploy/scripts/uninstall.sh --purge-data    # 另删两个数据库与 /etc/ssl/upanel
```

卸载脚本先列出“将要删除的路径”再动手，`/srv/upanel`（站点与应用数据）**永远不会自动删除**；
要清空它必须再加 `--yes-delete-srv` 并再次输入 `yes`。系统账号默认保留，`--purge-accounts` 才删。

---

## 8. 下一步

- 怎么打开面板、怎么核对证书、怎么换成受信任证书、怎么让它对管理网可达：见 [ACCESS.md](ACCESS.md)。
- 密码忘了、证书过期、代理连不上、数据库损坏、fstab 写坏、开机起不来：见 [RECOVERY.md](RECOVERY.md)。
- 只想在本机（Windows/macOS 或一台不打算改的机器）先看看界面：见 [LOCAL_START.md](LOCAL_START.md)。
