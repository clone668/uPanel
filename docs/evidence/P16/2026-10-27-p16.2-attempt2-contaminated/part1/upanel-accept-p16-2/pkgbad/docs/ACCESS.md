# 访问面板（HTTPS 与 SSH 隧道）

面板**只经 HTTPS** 对外提供服务。原因不是洁癖：会话 Cookie 是 `__Host-` 前缀且带 `Secure`
（`Path=/`、不带 `Domain`、`HttpOnly`、`SameSite=Strict`），浏览器在明文 HTTP 下根本不会回传它，
换句话说——明文访问不是“不安全”，是**登不进去**。

安装后的默认形态是：面板监听 `127.0.0.1:9443`，证书是安装脚本生成的自签名证书。
你从自己的电脑建一条 SSH 隧道进来，浏览器看到的是 `https://127.0.0.1:9443`。

---

## 1. 默认部署：SSH 隧道 + 自签名证书

在**你自己的电脑**上执行（`<你的用户>`、`<服务器>` 换成实际值）：

```sh
ssh -N -L 9443:127.0.0.1:9443 <你的用户>@<服务器>
```

`-N` 表示只转发不开 shell；这条命令会一直占着那个终端，关掉它就断开。
想让它在后台跑，去掉 `-N`，在远端 shell 里按 `~C` 或直接用：

```sh
ssh -f -N -L 9443:127.0.0.1:9443 <你的用户>@<服务器>
```

然后浏览器打开：

```
https://127.0.0.1:9443
```

### 第一次连接：核对指纹

浏览器会提示“证书不受信任”，因为这个证书是自签名的、没有公共 CA 给它背书。
**这不等于要你无脑点继续**——正确做法是对指纹：

```sh
# 方法一：安装脚本最后打印过这一串，直接对比
# 方法二：在服务器上读
sudo openssl x509 -in /etc/upanel/tls/panel.pem -noout -fingerprint -sha256

# 方法三：在服务器上问“443/9443 上现在服务的是哪张证书”
echo | openssl s_client -connect 127.0.0.1:9443 -servername "$(hostname -s)" 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

浏览器的“查看证书 → 指纹”里是对应的一串 SHA-256 十六进制。三个地方一致，才点继续；
不一致就停下来——那说明有人在中间，或者你连错了机器。

```sh
# 也可以用命令行直接证明证书确实是这台机器的、并且它在应答
curl -sS --cacert /etc/upanel/tls/panel.pem https://127.0.0.1:9443/api/v1/health/live
```

`--cacert` 用的是安装的那张证书本身，所以这条命令同时验证了“证书配得上”和“服务在应答”。
（`-k` 会把这两件事一起跳过去，别用它做验收。）

---

## 2. 换成受信任的证书

自签名证书适合“只从隧道进来”的场景。如果面板要对一台受管网络里的其他机器开放，
或者你受不了每次打开都点警告，就把面板证书换成你自己的那份：

```sh
# 1) 放好证书与私钥（路径可以任意，安装脚本的默认位置是 /etc/upanel/tls/）
sudo install -m 0644 -o root -g upanel-web 你的证书.pem /etc/upanel/tls/panel.pem
sudo install -m 0640 -o root -g upanel-web 你的私钥.key /etc/upanel/tls/panel.key

# 2) 如果换了路径，同步改配置里的两个字段（必须成对给出）
sudo vi /etc/upanel/upanel.json
#    "web": { "tls_cert_path": "/etc/upanel/tls/panel.pem",
#             "tls_key_path":  "/etc/upanel/tls/panel.key" }

# 3) 校验 + 重启
sudo -u upanel-web upanelctl check-config --config /etc/upanel/upanel.json
sudo systemctl restart upanel-web
```

`check-config` 会打印证书的主题、到期时间、覆盖的域名和 SHA-256 指纹；有问题会直接说清是
“文件不存在”“证书和私钥不是一对”“已过期”还是“还没生效”。

几条硬约束：

- 私钥权限不要宽于 `0640`，属组是 `upanel-web`（Web 进程要以这个身份启动）。
  权限过宽时 `check-config` 与启动日志都会给一条告警。
- 证书**过期**或**尚未生效**时面板拒绝启动，并在错误里写明日期——不静默降级、不回退到明文。
- 剩余有效期不足 21 天时启动会打一条告警。0.1 **不会**自动续期面板证书，
  到期前自己换（代理站点用的证书由面板管理，那是另一套，见第 5 节）。
- 最低协议版本是 TLS 1.2。

---

## 3. 对管理网直接开放（会改变暴露面）

默认只监听回环。要让它对一台管理网地址可达，三件事必须一起做，少一件都起不来：

```sh
sudo vi /etc/upanel/upanel.json
```

```jsonc
"web": {
  "mode": "production",
  "listen_address": "192.0.2.10:9443",        // 1) 改监听地址
  "tls_cert_path": "/etc/upanel/tls/panel.pem", // 2) 必须保留 TLS
  "tls_key_path":  "/etc/upanel/tls/panel.key",
  "allowed_origins": ["https://192.0.2.10:9443"] // 3) 显式给出 https 源
}
```

```sh
sudo -u upanel-web upanelctl check-config --config /etc/upanel/upanel.json
sudo systemctl restart upanel-web
```

为什么要这三条：

- **非回环监听必须配 TLS。** 启动器会检查；缺 TLS 时直接拒绝启动，不会“先起来再说”。
  这是有意的不便：面板的 Cookie 在明文下无法工作，而一个连不上又没明说原因的端口，
  比一个明确拒绝启动的服务更难排查。
- **`allowed_origins` 必须显式列 `https://` 源。** 生产模式不信任 `Host`/`X-Forwarded-*`，
  登录与写操作的 Origin 校验就靠这张表。派生值（`https://<listen>`）只在监听回环时自动加；
  一旦监听非回环地址，就必须自己写清楚，避免“谁把这个域名指过来就认谁”。
- **先确认 SSH 还在。** 面板不接管 `sshd`、不改防火墙，但如果你的环境里 9443 默认被挡，
  开监听不会让防火墙也开——这属于你的网络策略，面板不去替你放宽。

改完之后从管理网的那台机器上验证：

```sh
curl -sS --cacert /path/to/你的CA.pem https://192.0.2.10:9443/api/v1/health/live
```

---

## 4. HSTS 与响应头

TLS 生效时，面板对所有响应下发：

```
Strict-Transport-Security: max-age=15552000
```

只有 `max-age`（180 天），不带 `includeSubDomains`，也不带 `preload`。
这两项一旦下发就无法单方面撤回（`preload` 还要进浏览器内置列表），
不该由一台单机部署替主机上**所有**子域做决定。要更强就自己在反向代理层加。

---

## 5. 代理站点的证书（与面板证书无关）

P10 的站点管理里，每个域名可以有自己的证书：签发（Let's Encrypt HTTP-01）、续期、手动导入。
它们和面板自己的证书是两套完全独立的材料：

| | 面板证书 | 站点证书 |
|---|---|---|
| 放在哪 | `/etc/upanel/tls/` | `/etc/ssl/upanel/<域名>.pem` + `<域名>.key` |
| 谁用 | `upanel-web` | nginx（由代理加载） |
| 怎么来 | 安装脚本生成 / 你放进去 | 面板里签发或导入 |
| 续期 | 手动（0.1） | 面板管理（`renew` 任务） |

站点证书的私钥**不经过 Web 进程**：导入时私钥走 `/secrets` 通道直达代理的秘密存储，
`/etc/ssl/upanel` 是 `0700 root`，任何 API、日志、备份清单都读不到私钥内容。
换证书后 nginx 需要 reload；面板会在站点应用时探测 443 上实际服务的证书指纹，
发现还是旧证书就再 reload 一次，仍然不一致时记一条 `PROXY_TLS_SERVED_STALE` 告警。

证书签发默认走 **staging**（Let's Encrypt 的测试环境），避免反复消耗正式环境的签发配额；
确认流程走通后再切到正式。

---

## 6. 访问出问题时先看哪里

| 现象 | 先看 | 常见原因 |
|---|---|---|
| 浏览器转圈、`curl` 超时 | `systemctl status upanel-web` | 服务没起来 / 监听地址改错 |
| 连上了但证书警告里的指纹对不上 | 第 1 节的三个指纹 | 连错机器 / 有人中间人 |
| 登录提交后 401 且没有任何日志 | Cookie 是否被浏览器拦下 | 你用的是 `http://` 而不是 `https://` |
| 登录提示 429 | 登录失败限速 | 连续输错；等窗口过去 |
| 页面能开，功能报 `AGENT_UNAVAILABLE` | `systemctl status upanel-agent`、`ls -l /run/upanel/agent.sock` | 代理没起 / socket 权限不对 |
| 面板拒绝启动，日志说 `web.tls_cert_path` | 证书路径与权限 | 见 [RECOVERY.md](RECOVERY.md) 第 2 节 |

更完整的恢复流程见 [RECOVERY.md](RECOVERY.md)。
