# P01.4 结果

日期：2026-10-20
范围：前端构建产物嵌入 Go Web 二进制、开发/生产配置、systemd 单元模板与本地启动文档。

## 结论

P01.4 的代码与检查已完成，并在两台 Ubuntu 验收 VM 上通过真实 systemd 启动验收。

| 项目 | 状态 | 证据 |
| --- | --- | --- |
| 前端产物嵌入二进制（`//go:embed all:dist`） | 已验证 | `host-checks.txt`、`vm-*-acceptance.txt` |
| 服务端注入运行模式（mock / production） | 已验证 | `host-checks.txt`、`vm-*-acceptance.txt` |
| 安全头与 CSP nonce 每响应轮换 | 已验证 | `vm-*-acceptance.txt` |
| 同源 API 信封、未知 API 恒为 JSON 404 | 已验证 | `vm-*-acceptance.txt`、`go-test-webui-api.txt` |
| SPA 深链接回退 + 缺失资源 404 + 穿越不泄露 | 已验证 | `vm-*-acceptance.txt` |
| mock 只绑 127.0.0.1:18881，写错地址拒绝启动 | 已验证 | `vm-*-acceptance.txt` |
| production 只绑回环（TLS 前） | 已验证 | `vm-*-acceptance.txt` |
| 非 Linux production 明确拒绝 | 已验证（宿主 Windows） | `host-nonlinux-production-refusal.txt` |
| 无嵌入资源时不伪装正常（503 + 启动告警） | 已验证（宿主运行） | `host-missing-assets-503.txt` |
| systemd 单元语法与引用校验 | 已验证（两台 VM） | `vm-*-acceptance.txt` |
| systemd 真实安装启动、进程归属 `upanel-web` | 已验证（两台 VM） | `vm-*-acceptance.txt` |
| 真实浏览器冒烟（对二进制，非同源 mock） | 已验证（宿主） | `host-smoke-mock.txt` 19/19 |
| 仓库统一检查 | 已验证 | `check-run.txt` PASS 12 / FAIL 0 / NOT RUN 0 |
| 默认安装到真实生产服务器（非验收 VM） | 未验证 | 无生产环境，属部署动作 |

## 数字

- Go 测试：12 个包全部 `ok`；其中 `internal/webui` + `internal/api` 33 个用例 PASS / 0 FAIL。
- VM 验收脚本：114 项断言，两台 VM 均 114 PASS / 0 FAIL / 0 NOT RUN。
- 浏览器冒烟：19 项全通过（页面、模式、API、控制台错误）。
- 内嵌入口页 1287 字节；`bin/linux-amd64/upanel-web` 19.4 MB（未嵌入时 6.8 MB）。

## 未覆盖 / 限制

- TLS（P10）未接入，9443 目前是明文回环；production 因此只允许回环地址。
- 认证、会话与 `/meta` 未实现（P02），页面刷新后运行模式仍以服务端注入为准。
- `upanel-agent` 的 systemd 单元未提供：socket 属主与权限属 P02.3。
- 面板尚未实现 SIGHUP 重载配置；配置变更需重启服务。

## 复现

见 `commands.log`。VM 侧只需交叉编译好的二进制与 `deploy/` 目录，无需 Go/Node。
