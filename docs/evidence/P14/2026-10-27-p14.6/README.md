# P14.6 真机验收证据（TOTP / 一次性恢复码 / 启用确认 / CLI 本机恢复）

- 日期：2026-10-27（UTC 08:10 与 08:23 起两轮）
- 机器：Ubuntu 22.04.5 LTS，kernel 5.15.0-198-generic，Python 3.10.12（VM-A，`upanel-2204`）
- 脚本：`tests/integration/p14-6-acceptance.sh`，第二轮 sha256 `f3c6f442e9a07d9ff0ab00839b8a7715c20715ee03bbf6360132629f55fc8903`
- 二进制（交叉编译，`GOOS=linux GOARCH=amd64`，`-buildvcs=false -trimpath`，版本 `p14.6-accept`）：
  - `upanel-agent` sha256 `0040ed6be427f4af5099ca8704cd9f5726f655e1e2995a921d7c0791d5bbf5dc`
  - `upanel-web` sha256 `179ccfa02de58c1e57875b6438a4c14082e6cbb1c933209be7b402048aacc640`
  - `upanelctl` sha256 `8da5b1768c9f30fc837681ecd1e0335cb32b6f60c48c0eb9f229c25a226129f9`
- 前端产物：`npm run build` 后同步进 `internal/webui/dist`（`go:embed`），所以第 8 节能从二进制里 grep 到界面文案

## 结论

| 轮次 | 结果 | 说明 |
|---|---|---|
| 第一轮（`run-1.log`） | **PASS 149 / FAIL 11 / NOT RUN 6**，退出码 1 | 抓到 1 个**产品缺陷**（Q480）+ 4 类**脚本自身**的断言错误（Q481） |
| 第二轮（`run-2.log`） | **PASS 163 / FAIL 0 / NOT RUN 6**，退出码 0 | 同一台 VM、修完缺陷与脚本后重跑 |

第一轮脚本文件的 sha256 没有留存（同一路径被第二轮覆盖），只留了它的原始日志。

## STAGES §16 的两条硬要求，真机读数

**A. TOTP 未确认不启用**（`run-2.log` 第 2 节，28 条）

- 接口层：`setup` 之后 `GET /auth/session` 仍是 `totp_enabled=false`；
- 库层：面板库 `user_totp` **0 行**（不是 `enabled=0`）；
- 代理层：那条种子是 `state='temporary'`（十分钟自毁），没有 `persistent`；
- 登录规则未变：无码 200、带码也 200（码被忽略，不会"顺手启用"）；
- 确认时码错 → 401 `UNAUTHENTICATED`，库里仍 0 行、没有恢复码、种子仍是 `temporary`；
- 带外读数：`upanelctl totp-reset` 退出码 1 且 stderr 明说"没有启用二次验证"；
- 明文种子与 `otpauth://` 在面板库、代理库及其 `-wal`/`-shm` 五处文件里命中 **0**。

**B. 恢复码只能使用一次**（第 5 节）

判据不是"关掉之后旧码不认了"（那时整张表都没了，分不清"用过"还是"不存在"），而是那个**中间态**：

1. 先带码再认证，把再认证窗口推进 5 分钟（停代理之后就做不了带码再认证）；
2. 停代理 → 用 RC1 关闭 → `503 AGENT_UNAVAILABLE`；
3. 带外读库：RC1 那一行 `used_at` **已非空**（码被烧掉，失败也不回滚），状态行与 10 枚码都还在；
4. 起回代理 → **同一枚** RC1 再关 → `401`「每枚只能用一次」，没有复活；
5. 换 RC2 → 200，状态行与十枚哈希一起消失，种子 `destroyed`，另一台设备的会话 401、本条会话保留。

## 这一轮另外读到的（都在 `run-2.log`）

- 码由脚本自己按 RFC 6238 用 python 标准库算（扮演 authenticator），面板认的是真码；
- **重放**：同一个码第二次 401「刚刚用过」、不签发 Cookie，下一个时间步立刻可用（±1 步窗口）；
- **确认也算用掉一步**（Q480 的回归断言）：确认用的那个六位数立刻拿去登录取到 401；
- **CLI 本机恢复**：root 跑 `agent_status=forbidden`（`SO_PEERCRED` 只认 `upanel-web`，设计如此）+ 孤儿密文留在代理库；`runuser -u upanel-web` 跑则 `destroyed`；两种情况下面板库都清干净、会话全废、审计 actor 是 `local-cli:<who>` 且 `actor_id` 为 NULL；输出里没有口令也没有恢复码；
- **前端产物**：从 `/assets/*.js` 出发把动态 import 的 chunk 一代代抓下来（55 个分片），`二次验证`、`/auth/totp`、`只显示这一次`、`同一个码不能用第二次` 四处都在。

## NOT RUN（6 条，按设计）

1. 真浏览器里的登记/关闭点击流程 —— 这台 VM 上没有浏览器；交互由前端单测（21 例）+ 本轮接口读数共同覆盖
2. 二维码图片 —— P00 冻结的依赖集里没有 QR 编码器，界面按文本 + 复制按钮呈现 `otpauth_uri`
3. `GET /auth/totp`（还剩几枚恢复码）—— 本轮不开新接口
4. 手机与服务器时钟相差超过 ±1 步 —— 要改 VM 时钟，会同时搅乱窗口/过期/限速；边界由 `internal/totp` 的 RFC 6238 向量覆盖
5. 登录限速打满（15 分钟 5 次）—— 本轮刻意只制造 4 次失败，打满之后所有断言都会变成 429
6. root 跑 `totp-reset` 之后那条孤儿密文的彻底清理 —— 代理侧密文仍占一行（已无人引用），彻底清扫是 P16 的运维项

## 没跑的另一台

`upanel-2404`（24.04.5）本轮**没有**跑。P14.6 的对象全在账号/代理层（`user_totp` 表、`/internal/v1/totp/*`、`/auth/totp/*`），与发行版无关；P14.2—P14.5 的惯例也是 22.04 单台。24.04 的全套回归留到 P16.5 的整机验收。

## 目录

- `run-1.log` / `run-2.log` —— 两轮的原始输出（逐条 PASS/FAIL/NOT RUN）
- `facts/` —— 本轮带外读数与关键字迹：
  - `ctl-reset-root.json`（root：`agent_status=forbidden`）、`ctl-reset-webuser.json`（`destroyed`）
  - `ctl-reset-root.err` / `ctl-reset-webuser.err` / `ctl-reset-again.err`（二次调用失败的原因）
  - `databases-final.txt`（收尾时两个库及其 `-wal`/`-shm` 的属主与大小）
  - `final-totp-seeds.txt`、`final-user_totp.txt`（收尾库里的事实）
  - `settings-index.html`（设置页返回的入口页）、`bundle-greps.txt`（四处界面文案的命中原样；55 个分片拼起来的那份 1.6MB 文件本身不入库）
  - `payloads/totp-setup.json`、`payloads/totp-confirm.json` —— 请求/响应原件，**已抹去** base32 种子与十枚恢复码明文（这两样是本轮唯一能复用的秘密材料；形状与字段名保留，值换成占位符）
