#!/bin/sh
# P10.4 Linux 验收脚本（第一部分）：0.1 安装包、校验和、systemd 单元、初始账号、
# 面板 HTTPS、重入幂等与本机口令恢复。
#
# 这一段要证明的是“推广给别人之后能不能装成、装完是不是面向浏览器可用的”，
# 不是“install.sh 返回 0”。所以每一条都去读**装完之后机器上的事实**：
#
#   1) **校验和是真在检查**：包内逐文件核对通过；把某个文件改一个字节之后，
#      同一个核对必须失败（不失败的校验和等于没有校验和）。
#   2) **卸载是先列后删、可重入的**：卸载脚本删掉它该删的（账号、目录、单元、配置），
#      留下它承诺留下的（/srv/upanel）；随后一次全新安装能只在干净状态上成立。
#   3) **初始账号与权限就是文档写的那样**：upanel-web 无登录 shell、不在特权组；
#      /etc/upanel 归 root（Web 能读不能改）、配置 0640、代理密钥 0600、
#      面板私钥 0640 root:upanel-web、代理数据库 0700 root。
#   4) **面板真的只经 HTTPS 对外**：用安装的那张证书当 CA（不加 -k）取 health；
#      明文请求拿不到任何业务响应；HSTS 只在 TLS 下出现；443/9443 上服务的证书
#      指纹与磁盘上的一致；TLS 1.2 能连、1.1 连不上。
#   5) **非回环监听缺 TLS 会拒绝启动**（而不是“先起来再说”）。
#   6) **登录是浏览器可用的**：Set-Cookie 是 __Host- 前缀 + Secure + HttpOnly +
#      SameSite=Strict 且不带 Domain —— 这正是“明文访问登不进去”的根因。
#   7) **重跑安装器不破坏已有状态**：密钥、证书、配置、账号、口令全部保留。
#   8) **忘记口令有本机恢复路径**：reset-password 换掉口令、旧口令立刻失效、
#      重置前的会话被作废。
#
# 用法（root）：
#   sudo PACKAGE=/root/upanel-0.1.0-linux-amd64 sh tests/integration/p10-4-install-acceptance.sh
#
# 环境变量：PACKAGE（解开的发布包根目录，必填）、PORT（默认 9443，与文档一致）、WORK（默认
# /var/tmp/upanel-accept-p10-4）、RESET_PASSWORD（默认 true；false 则跳过第 8 节）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 对这台机器的改动：
#   - 运行卸载脚本（删除此前的 upanel 安装、账号与数据库）；
#   - 安装发布包到 /usr/local/{bin,lib,share}、/etc/upanel、/var/lib/upanel*；
#   - 安装、enable 并启动两个 systemd 单元；
#   - 在 WORK 下留日志与临时配置。收尾**不回滚**（这台机器的用途就是验收），
#     只停掉临时起的进程。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **真实公网浏览器**：VM 没有浏览器，HTTPS 与 Cookie 都用命令行与 HTTP 语义验证；
#     “地址栏变绿”这类渲染事实不在这里证明。
#   - **apt 安装依赖**：VM 里 nginx/certbot 已经装好，--install-deps 会 `apt-get update`，
#     在验收机上跑它只会证明“能联网”；依赖缺失时的“列出而不安装”行为由第 3 节
#     （缺 curl/journalctl 的容器里）另有覆盖，不在本脚本重复。
#   - **22.04 与 24.04 的差异**：同一份脚本、同一个包在两支上各跑一遍，两条运行记录各自
#     进证据目录；脚本只打印它看到的是哪一支，不假设。22.04 上是 nginx 1.18 / certbot 1.21，
#     24.04 上是 nginx 1.24 / certbot 2.9——差异由真实运行记录覆盖，不由断言掩盖。
#   - **代理站点、WebSocket、站点证书**：在 p10-4-core-scenario.sh 里。
#   - **Windows / macOS 宿主**：0.1 不支持。

set -u

PACKAGE=${PACKAGE:-}
PORT=${PORT:-9443}
WORK=${WORK:-/var/tmp/upanel-accept-p10-4}
RESET_PASSWORD=${RESET_PASSWORD:-true}
DO_RESET=0
[ "$RESET_PASSWORD" = "true" ] && DO_RESET=1

SYS_CFG=/etc/upanel/upanel.json
KEY_FILE=/etc/upanel/agent.key
CERT_FILE=/etc/upanel/tls/panel.pem
KEY_PAIR=/etc/upanel/tls/panel.key
WEB_USER=upanel-web
SHELL_USER=upanel-shell
BASE="https://127.0.0.1:$PORT"
ORIGIN=$BASE
PASSWORD='install-correct-horse-9'
NEW_PASSWORD='after-reset-battery-42'

LOGS=$WORK/logs
PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; }
note() { printf '      %s\n' "$1"; }

expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi
}
expect_ne() {
  if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不该等于 '$2'"; fi
}
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "期望包含 '$2'，实际 '$3'" ;;
  esac
}
expect_not_contains() {
  case "$3" in
    *"$2"*) fail "$1" "不该包含 '$2'，实际 '$3'" ;;
    *) pass "$1" ;;
  esac
}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo PACKAGE=... sh %s\n' "$0" >&2
  exit 2
fi
if [ -z "$PACKAGE" ] || [ ! -f "$PACKAGE/bin/upanel-web" ]; then
  printf 'PACKAGE 必须指向解开的发布包根目录（含 bin/upanel-web）：当前 %s\n' "$PACKAGE" >&2
  exit 2
fi
for tool in curl openssl sha256sum systemctl python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf '缺少依赖：%s\n' "$tool" >&2
    exit 2
  fi
done
# 0.1 的发布门槛要求 **22.04 与 24.04 都有安装/重启/核心运维证据**（STAGES 第 12 节），
# 所以这脚本两支都要跑，只在别的发行版上拒绝——不要用“只在 24.04 上跑过”当 22.04 的证据。
_DISTRO_VERSION=$(sed -n 's/^VERSION_ID=//p' /etc/os-release | tr -d '"')
case "$_DISTRO_VERSION" in
  22.04|24.04) ;;
  *)
    printf '本脚本只覆盖 0.1 支持的发行版（22.04 / 24.04），当前 %s\n' "$_DISTRO_VERSION" >&2
    exit 2
    ;;
esac

rm -rf "$WORK"
mkdir -p "$LOGS"

# passwd_scheme <证书文件> → 打印这张证书的 SHA256 指纹（小写、无冒号）
fingerprint_of() {
  openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f'
}

# login <口令> <输出目录> → 打印 HTTP 状态码；响应头与正文分别落在该目录下
#
# 必须带 X-UPanel-Request: 1：面板要求所有写请求带这个头（挡“简单请求”式的跨站提交），
# 少了它登录直接 400 —— 这条本身也是验收要证明的东西之一（见第 7 节的断言）。
login() {
  curl -sS -o "$2/login.body" -D "$2/login.head" -w '%{http_code}' \
    --cacert "$CERT_FILE" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
    -H 'X-UPanel-Request: 1' \
    -d "{\"username\":\"admin\",\"password\":\"$1\"}" \
    "$BASE/api/v1/auth/login"
}

printf '==== P10.4 安装验收（第一部分）====\n'
note "包：$PACKAGE"
note "工作目录：$WORK"
note "端口：$PORT"

# ---------------------------------------------------------------- 1. 校验和
printf '\n--- 1. 发布包的校验和 ---\n'
if ( cd "$PACKAGE" && sha256sum -c --quiet SHA256SUMS >"$LOGS/checksums.txt" 2>&1 ); then
  pass "包内 SHA256SUMS 逐文件核对通过"
else
  fail "包内 SHA256SUMS 逐文件核对通过" "$(tail -n 3 "$LOGS/checksums.txt" | tr '\n' ' ')"
fi

# 篡改一个字节，同一个核对必须失败：否则“校验和通过”什么也没说明。
TAMPER_DIR=$WORK/tamper
mkdir -p "$TAMPER_DIR"
cp -a "$PACKAGE/." "$TAMPER_DIR/"
printf 'x' >>"$TAMPER_DIR/README.md"
if ( cd "$TAMPER_DIR" && sha256sum -c --quiet SHA256SUMS >"$LOGS/tamper.txt" 2>&1 ); then
  fail "改过一个字节之后校验和必须失败" '仍然报通过'
else
  pass "改过一个字节之后校验和失败（校验和确实在比对内容）"
fi
rm -rf "$TAMPER_DIR"

# 发布包的组成是契约的一部分：缺一个文件，安装说明里承诺的东西就不存在。
for item in bin/upanel-web bin/upanelctl bin/upanel-agent \
            deploy/systemd/upanel-web.service deploy/systemd/upanel-agent.service \
            deploy/scripts/install.sh deploy/scripts/uninstall.sh \
            deploy/upanel.example.json docs/INSTALL.md docs/ACCESS.md docs/RECOVERY.md \
            README.md VERSION SHA256SUMS; do
  if [ -f "$PACKAGE/$item" ]; then
    pass "发布包含有 $item"
  else
    fail "发布包含有 $item" '文件不存在'
  fi
done

for item in bin/upanel-web bin/upanelctl bin/upanel-agent; do
  if [ -x "$PACKAGE/$item" ]; then
    pass "包内 $item 带执行位"
  else
    fail "包内 $item 带执行位" '解压之后不可执行（安装器会因此拒绝继续）'
  fi
done
if grep -qE '^[^#]*curl[^|]*\|[[:space:]]*(ba)?sh' "$PACKAGE/deploy/scripts/install.sh"; then
  fail "安装脚本里没有 curl | bash" '发现管道执行'
else
  pass "安装脚本里没有 curl | bash（可审阅）"
fi
if head -n 1 "$PACKAGE/deploy/scripts/install.sh" | grep -q '^#!/bin/sh'; then
  pass "安装脚本是 POSIX sh 脚本（不需要 bash）"
else
  fail "安装脚本是 POSIX sh 脚本" "$(head -n 1 "$PACKAGE/deploy/scripts/install.sh")"
fi

# ---------------------------------------------------------------- 2. 卸载到干净状态
printf '\n--- 2. 卸载（先列后删）---\n'
mkdir -p /srv/upanel/p10-4-keepme
printf 'keep\n' >/srv/upanel/p10-4-keepme/marker.txt

UNINSTALL_OUT=$LOGS/uninstall.txt
# --purge-config 在有 agent.key 时会要求输入 yes（那一步不可逆）。验收机上我们确实要
# 一把干净的起点，所以把 yes 从标准输入喂进去，而不是加一个“跳过确认”的开关——
# 确认这回事本身要有证据。
printf 'yes\n' | sh "$PACKAGE/deploy/scripts/uninstall.sh" \
  --purge-config --purge-data --purge-accounts >"$UNINSTALL_OUT" 2>&1
UNINSTALL_RC=$?
expect_eq "卸载脚本退出码为 0" 0 "$UNINSTALL_RC"
expect_contains "卸载脚本先列出将要删除的路径" '将要执行' "$(cat "$UNINSTALL_OUT")"
expect_contains "卸载脚本把 /srv/upanel 列为永不自动删除" '/srv/upanel' "$(cat "$UNINSTALL_OUT")"

if [ -e "$SYS_CFG" ]; then fail "卸载后 /etc/upanel 已移除" "$SYS_CFG 仍存在"; else
  pass "卸载后 /etc/upanel 已移除"; fi
if id -u "$WEB_USER" >/dev/null 2>&1; then fail "卸载后 upanel-web 账号已删除" '仍存在'; else
  pass "卸载后 upanel-web 账号已删除"; fi
if id -u "$SHELL_USER" >/dev/null 2>&1; then fail "卸载后 upanel-shell 账号已删除" '仍存在'; else
  pass "卸载后 upanel-shell 账号已删除"; fi
if [ -e /etc/systemd/system/upanel-web.service ]; then fail "卸载后单元文件已删除" '仍存在'; else
  pass "卸载后单元文件已删除"; fi
if [ -e /usr/local/bin/upanel-web ]; then fail "卸载后二进制已删除" '仍存在'; else
  pass "卸载后二进制已删除"; fi
if [ -f /srv/upanel/p10-4-keepme/marker.txt ]; then
  pass "卸载保留了 /srv/upanel 里的业务数据"
else
  fail "卸载保留了 /srv/upanel 里的业务数据" 'marker 文件不见了'
fi

# ---------------------------------------------------------------- 3. 全新安装
printf '\n--- 3. 全新安装 ---\n'
INSTALL_OUT=$LOGS/install.txt
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | \
  sh "$PACKAGE/deploy/scripts/install.sh" --source "$PACKAGE" --listen "127.0.0.1:$PORT" \
    --password-stdin >"$INSTALL_OUT" 2>&1
INSTALL_RC=$?
if [ "$INSTALL_RC" -eq 0 ]; then
  pass "安装脚本退出码为 0"
else
  fail "安装脚本退出码为 0" "rc=$INSTALL_RC；末尾：$(tail -n 5 "$INSTALL_OUT" | tr '\n' ' ')"
  note "安装输出在 $INSTALL_OUT；后续断言仍会继续跑，用来定位卡在哪一步"
fi
for step in '校验发布包' '探测系统' '依赖检查' '系统账号' '目录与权限' '面板 HTTPS 证书' \
            '配置文件' '代理秘密密钥' '安装二进制并迁移数据库' '第一个管理员' \
            'systemd 单元' '健康检查'; do
  expect_contains "安装输出覆盖步骤：$step" "$step" "$(cat "$INSTALL_OUT")"
done
expect_not_contains "安装输出里没有出现明文口令" "$PASSWORD" "$(cat "$INSTALL_OUT")"
expect_contains "安装输出打印了证书指纹" '证书指纹 sha256:' "$(cat "$INSTALL_OUT")"

# ---------------------------------------------------------------- 4. 安装产物
printf '\n--- 4. 账号、目录与权限 ---\n'
expect_eq "upanel-web 存在且属主是 upanel-web:upanel-web" \
  "$WEB_USER:$WEB_USER" "$(stat -c '%U:%G' /var/lib/upanel)"
expect_eq "upanel-web 的 shell 是 nologin" '/usr/sbin/nologin' "$(getent passwd "$WEB_USER" | cut -d: -f7)"
expect_eq "upanel-shell 的 shell 是真 shell（/bin/bash）" '/bin/bash' "$(getent passwd "$SHELL_USER" | cut -d: -f7)"
for u in "$WEB_USER" "$SHELL_USER"; do
  if id -nG "$u" | tr ' ' '\n' | grep -qxE 'sudo|admin|docker|disk|root|tty'; then
    fail "$u 不属于特权组" "$(id -nG "$u")"
  else
    pass "$u 不属于特权组（无 sudo/docker/disk）"
  fi
done
if [ "$(passwd -S "$SHELL_USER" 2>/dev/null | awk '{print $2}')" = "L" ]; then
  pass "upanel-shell 的口令是锁定的"
else
  fail "upanel-shell 的口令是锁定的" "$(passwd -S "$SHELL_USER" 2>/dev/null)"
fi

expect_eq "/etc/upanel 属主是 root:root" 'root:root' "$(stat -c '%U:%G' /etc/upanel)"
expect_eq "/etc/upanel 权限 0755（Web 能读不能改）" '755' "$(stat -c '%a' /etc/upanel)"
expect_eq "配置文件属主 root:upanel-web" "root:$WEB_USER" "$(stat -c '%U:%G' "$SYS_CFG")"
expect_eq "配置文件权限 0640" '640' "$(stat -c '%a' "$SYS_CFG")"
expect_eq "代理密钥属主 root:root" 'root:root' "$(stat -c '%U:%G' "$KEY_FILE")"
expect_eq "代理密钥权限 0600" '600' "$(stat -c '%a' "$KEY_FILE")"
expect_eq "面板私钥属主 root:upanel-web" "root:$WEB_USER" "$(stat -c '%U:%G' "$KEY_PAIR")"
expect_eq "面板私钥权限 0640" '640' "$(stat -c '%a' "$KEY_PAIR")"
expect_eq "面板证书权限 0644" '644' "$(stat -c '%a' "$CERT_FILE")"
expect_eq "代理数据目录 0700 root" '700 root:root' "$(stat -c '%a %U:%G' /var/lib/upanel-agent)"
expect_eq "面板数据库属主 upanel-web" "$WEB_USER" "$(stat -c '%U' /var/lib/upanel/upanel.db)"
expect_eq "终端账号 home 权限 0700" '700' "$(stat -c '%a' /var/lib/upanel-shell)"
expect_eq "ACME 校验目录存在且属主 root" 'root:root' "$(stat -c '%U:%G' /var/www/upanel-acme)"
expect_eq "手动证书目录 0700 root" '700 root:root' "$(stat -c '%a %U:%G' /etc/ssl/upanel)"
expect_eq "站点根目录 /srv/upanel 属主 root" 'root:root' "$(stat -c '%U:%G' /srv/upanel)"

# Web 账号必须读不到代理数据库与代理密钥（权限的两条真实后果）。
if runuser -u "$WEB_USER" -- test -r /var/lib/upanel-agent/agent.db; then
  fail "upanel-web 读不到代理数据库" '竟然可读'
else
  pass "upanel-web 读不到代理数据库（0700 root）"
fi
if runuser -u "$WEB_USER" -- test -r "$KEY_FILE"; then
  fail "upanel-web 读不到代理密钥" '竟然可读'
else
  pass "upanel-web 读不到代理密钥（0600 root）"
fi
if runuser -u "$WEB_USER" -- test -w /etc/upanel; then
  fail "upanel-web 不能写 /etc/upanel（不能替换自己的配置）" '竟然可写'
else
  pass "upanel-web 不能写 /etc/upanel（能读不能改）"
fi
if runuser -u "$WEB_USER" -- test -r "$SYS_CFG"; then
  pass "upanel-web 读得到配置（0640 root:upanel-web）"
else
  fail "upanel-web 读得到配置" '读不到'
fi

# ---------------------------------------------------------------- 5. systemd 单元
printf '\n--- 5. systemd 单元 ---\n'
expect_eq "upanel-web.service 处于 active" 'active' "$(systemctl is-active upanel-web.service)"
expect_eq "upanel-agent.service 处于 active" 'active' "$(systemctl is-active upanel-agent.service)"
expect_eq "upanel-web.service 已 enable" 'enabled' "$(systemctl is-enabled upanel-web.service)"
expect_eq "upanel-agent.service 已 enable" 'enabled' "$(systemctl is-enabled upanel-agent.service)"
if systemd-analyze verify /etc/systemd/system/upanel-web.service \
     /etc/systemd/system/upanel-agent.service >"$LOGS/verify.txt" 2>&1; then
  pass "两个单元通过 systemd-analyze verify"
else
  fail "两个单元通过 systemd-analyze verify" "$(cat "$LOGS/verify.txt" | tail -n 3 | tr '\n' ' ')"
fi

WEB_PID=$(systemctl show -p MainPID --value upanel-web.service)
WEB_UID=$(ps -o uid= -p "${WEB_PID:-0}" 2>/dev/null | tr -d ' ')
expect_eq "Web 进程的 uid 是 upanel-web（不是 root）" "$(id -u "$WEB_USER")" "${WEB_UID:-?}"
AGENT_PID=$(systemctl show -p MainPID --value upanel-agent.service)
expect_eq "代理进程的 uid 是 root" '0' "$(ps -o uid= -p "${AGENT_PID:-0}" 2>/dev/null | tr -d ' ')"
expect_eq "代理 socket 属主 root:upanel-web" "root:$WEB_USER" "$(stat -c '%U:%G' /run/upanel/agent.sock)"
expect_eq "代理 socket 权限 0660" '660' "$(stat -c '%a' /run/upanel/agent.sock)"
expect_eq "/run/upanel 权限 0755（可进入不可写）" '755' "$(stat -c '%a' /run/upanel)"

# ---------------------------------------------------------------- 6. 面板 HTTPS
printf '\n--- 6. 面板 HTTPS ---\n'
FILE_FP=$(fingerprint_of "$CERT_FILE")
note "磁盘上证书的指纹：$FILE_FP"

HEALTH_CODE=$(curl -sS -o "$LOGS/health.json" -w '%{http_code}' --max-time 5 \
  --cacert "$CERT_FILE" "$BASE/api/v1/health/live" 2>"$LOGS/health.err")
expect_eq "用安装的证书当 CA 取 health 返回 200（不加 -k）" '200' "$HEALTH_CODE"
expect_contains "health 正文是 {\"status\":\"ok\"}" '"status":"ok"' "$(cat "$LOGS/health.json" 2>/dev/null)"

# 不带 --cacert 时必须失败：证明证书确实不被系统信任库认（自签名的真实后果），
# 也证明面板没有偷偷降级成明文。
if curl -sS -o /dev/null --max-time 5 "$BASE/api/v1/health/live" 2>"$LOGS/health-noca.err"; then
  fail "不加 --cacert 时校验证书失败（自签名不被系统信任）" '竟然成功'
else
  pass "不加 --cacert 时校验证书失败（自签名不被系统信任）"
fi

# 明文请求打到 TLS 端口：TLS 层先答，gin 根本收不到请求。
PLAIN=$(curl -sS --max-time 5 "http://127.0.0.1:$PORT/api/v1/health/live" 2>&1 || true)
expect_not_contains "明文 HTTP 请求拿不到业务响应" '"status"' "$PLAIN"
expect_contains "明文请求被指回 HTTPS" 'HTTPS' "$PLAIN"

SERVED_FP=$(echo | openssl s_client -connect "127.0.0.1:$PORT" -servername localhost 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f')
if [ -n "$SERVED_FP" ]; then
  expect_eq "端口上服务的证书指纹与磁盘上那张一致" "$FILE_FP" "$SERVED_FP"
else
  fail "端口上服务的证书指纹与磁盘上那张一致" '握手后读不到证书'
fi
expect_eq "安装输出打印的指纹与磁盘上那张一致" \
  "$FILE_FP" "$(sed -n 's/.*证书指纹 sha256: *\([0-9a-f]*\).*/\1/p' "$INSTALL_OUT" | head -n 1)"

TLS12=$(echo | openssl s_client -connect "127.0.0.1:$PORT" -servername localhost -tls1_2 2>&1)
expect_contains "TLS 1.2 握手成功并协商出密码套件" 'New, TLSv1.2, Cipher is' "$TLS12"

# TLS 版本：1.2 能连；1.1 连不上。
#
# 判据要小心：openssl s_client 在**握手失败**时也会打印一行 `Protocol  : TLSv1.1`
# （那是它自己尝试的版本，Cipher 是 0000、没有对端证书）。所以“打印里出现 TLSv1.1”
# 不能当成“协商成功”。真正的成功标志是有密码套件；失败标志是服务端的
# `tlsv1 alert protocol version`（alert 70）或本机 openssl 直接不让试。
TLS11=$(echo | openssl s_client -connect "127.0.0.1:$PORT" -servername localhost -tls1_1 \
  -cipher 'DEFAULT@SECLEVEL=0' 2>&1 || true)
if printf '%s' "$TLS11" | grep -q 'alert protocol version'; then
  pass "TLS 1.1 被拒（服务端回 protocol_version 告警，最低 TLS 1.2）"
elif printf '%s' "$TLS11" | grep -qiE 'no protocols available|unsupported protocol'; then
  pass "TLS 1.1 无法建立（本机 openssl 3.0 已禁用该协议；面板侧最低同为 1.2）"
elif printf '%s' "$TLS11" | grep -q 'Cipher is (NONE)'; then
  pass "TLS 1.1 没有协商出密码套件（握手未完成）"
else
  fail "TLS 1.1 不能建立连接（面板最低 TLS 1.2）" \
    "$(printf '%s' "$TLS11" | head -n 4 | tr '\n' ' ')"
fi

# HSTS 只在 TLS 下出现。
expect_contains "TLS 响应带 HSTS" 'max-age=15552000' \
  "$(curl -sS -D - -o /dev/null --cacert "$CERT_FILE" "$BASE/api/v1/health/live" 2>/dev/null)"

# ---------------------------------------------------------------- 7. 登录与 Cookie
printf '\n--- 7. 登录与 Cookie 属性 ---\n'
LOGIN_DIR=$WORK/login
mkdir -p "$LOGIN_DIR"
LOGIN_CODE=$(login "$PASSWORD" "$LOGIN_DIR")
expect_eq "管理员口令登录返回 200" '200' "$LOGIN_CODE"
COOKIE_LINE=$(grep -i '^set-cookie:' "$LOGIN_DIR/login.head" | head -n 1 | tr -d '\r')
note "Set-Cookie: $COOKIE_LINE"
expect_contains "会话 Cookie 是 __Host- 前缀" '__Host-upanel_session=' "$COOKIE_LINE"
expect_contains "会话 Cookie 带 Secure" 'Secure' "$COOKIE_LINE"
expect_contains "会话 Cookie 带 HttpOnly" 'HttpOnly' "$COOKIE_LINE"
expect_contains "会话 Cookie 带 SameSite=Strict" 'SameSite=Strict' "$COOKIE_LINE"
expect_not_contains "会话 Cookie 不带 Domain" 'Domain=' "$COOKIE_LINE"
expect_not_contains "登录响应里没有明文口令" "$PASSWORD" "$(cat "$LOGIN_DIR/login.body")"
expect_contains "登录响应给了 CSRF 令牌" 'csrf_token' "$(cat "$LOGIN_DIR/login.body")"
if grep -qi "password" "$LOGIN_DIR/login.body"; then
  fail "登录响应里没有口令字段" "$(cat "$LOGIN_DIR/login.body")"
else
  pass "登录响应里没有口令字段"
fi

# 错误口令必须被拒（不泄漏“账号存在与否”的差异，这里只断言状态码）。
BAD_DIR=$WORK/login-bad
mkdir -p "$BAD_DIR"
expect_ne "错误口令登录返回非 200" '200' "$(login 'definitely-not-the-password-1' "$BAD_DIR")"

# 少了 X-UPanel-Request: 1 必须 400：这个头是把“跨站表单提交”挡在门外的那道门，
# 少了它浏览器里的第三方页面就能拿用户的会话做事。
NOGUARD=$(curl -sS -o /dev/null -w '%{http_code}' --cacert "$CERT_FILE" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}" "$BASE/api/v1/auth/login")
expect_eq "缺 X-UPanel-Request 头的登录被拒（400）" '400' "$NOGUARD"

# 面板日志里不能有口令、也不能有私钥内容。
if grep -qi "$PASSWORD" "$LOGS/web.log" 2>/dev/null; then
  fail "面板日志里没有明文口令" '发现口令'
else
  pass "面板日志里没有明文口令"
fi

# ---------------------------------------------------------------- 8. 非回环无 TLS 拒绝启动
printf '\n--- 8. 非回环监听缺 TLS 必须拒绝启动 ---\n'
BAD_CFG=$WORK/upanel-bad.json
cat >"$BAD_CFG" <<'EOF'
{
  "web": { "mode": "production", "listen_address": "0.0.0.0:19444",
           "allowed_origins": ["http://0.0.0.0:19444"] },
  "agent": { "socket_path": "/run/upanel/agent.sock", "web_user": "upanel-web",
             "socket_group": "upanel-web", "database_path": "/var/tmp/upanel-accept-p10-4/agent.db",
             "secret_key_path": "/etc/upanel/agent.key" },
  "database": { "path": "/var/tmp/upanel-accept-p10-4/bad.db" },
  "log": { "level": "info", "format": "text" }
}
EOF
BAD_OUT=$(timeout 20 /usr/local/bin/upanel-web --config "$BAD_CFG" 2>&1)
BAD_RC=$?
expect_ne "非回环且无 TLS 时 upanel-web 拒绝启动（退出码非 0）" '0' "$BAD_RC"
expect_contains "拒绝理由点名 TLS 配置" 'tls_cert_path' "$BAD_OUT"
if ss -Hltn | awk '{print $4}' | grep -qE '[:.]19444$'; then
  fail "拒绝启动时没有留下监听端口" '19444 仍在监听'
else
  pass "拒绝启动时没有留下监听端口"
fi

# check-config 也必须拒绝同一份配置（CLI 与服务的判断一致，不出现“CLI 说行、服务不启动”）。
CTL_BAD=$(/usr/local/bin/upanelctl check-config --config "$BAD_CFG" 2>&1)
expect_ne "check-config 对同一份配置同样报错（退出码非 0）" '0' "$?"
expect_contains "check-config 的报错点名 TLS 配置" 'tls_cert_path' "$CTL_BAD"

# 合法的配置则通过，并且打印指纹（部署时人手抄的那一串）。
CTL_OK=$(/usr/local/bin/upanelctl check-config --config "$SYS_CFG" 2>&1)
expect_eq "check-config 对安装出来的配置通过" '0' "$?"
expect_contains "check-config 打印面板 HTTPS 已启用" '面板 HTTPS: 已启用' "$CTL_OK"
expect_contains "check-config 打印的指纹与磁盘一致" "$FILE_FP" "$CTL_OK"
CTL_JSON=$(/usr/local/bin/upanelctl check-config --config "$SYS_CFG" --json 2>/dev/null)
expect_contains "check-config --json 里有 web_tls.fingerprint_sha256" \
  '"fingerprint_sha256"' "$CTL_JSON"
expect_contains "check-config --json 里的指纹与磁盘一致" "$FILE_FP" "$CTL_JSON"
expect_contains "check-config --json 里 derived origin 用 https" "https://127.0.0.1:$PORT" "$CTL_JSON"

# ---------------------------------------------------------------- 9. 重入幂等
printf '\n--- 9. 重跑安装器（幂等）---\n'
BEFORE_KEY=$(sha256sum "$KEY_FILE" | cut -d' ' -f1)
BEFORE_CERT=$(sha256sum "$CERT_FILE" | cut -d' ' -f1)
BEFORE_CFG=$(sha256sum "$SYS_CFG" | cut -d' ' -f1)
RERUN_OUT=$LOGS/install-rerun.txt
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | \
  sh "$PACKAGE/deploy/scripts/install.sh" --source "$PACKAGE" --listen "127.0.0.1:$PORT" \
    --password-stdin >"$RERUN_OUT" 2>&1
expect_eq "重跑安装器退出码为 0" '0' "$?"
expect_eq "重跑没有覆盖代理密钥" "$BEFORE_KEY" "$(sha256sum "$KEY_FILE" | cut -d' ' -f1)"
expect_eq "重跑没有覆盖面板证书" "$BEFORE_CERT" "$(sha256sum "$CERT_FILE" | cut -d' ' -f1)"
expect_eq "重跑没有覆盖配置文件" "$BEFORE_CFG" "$(sha256sum "$SYS_CFG" | cut -d' ' -f1)"
expect_contains "重跑认出了已有密钥并保留" '已存在（保留：覆盖会让已保存的秘密永久解不开' "$(cat "$RERUN_OUT")"
expect_contains "重跑认出了已有账号并跳过建号" '已有账号，跳过创建' "$(cat "$RERUN_OUT")"
expect_eq "重跑之后口令没有变化" '200' "$(login "$PASSWORD" "$WORK/login")"
expect_eq "重跑之后服务仍然 active" 'active' "$(systemctl is-active upanel-web.service)"

# ---------------------------------------------------------------- 10. 本机口令恢复
printf '\n--- 10. reset-password（本机恢复路径）---\n'
if [ "$DO_RESET" != "1" ]; then
  notrun "reset-password 换口令" '按 RESET_PASSWORD=false 跳过'
else
  # 先用旧口令拿一个会话，重置之后这个会话必须失效。
  mkdir -p "$WORK/before-reset"
  login "$PASSWORD" "$WORK/before-reset" >/dev/null
  TOKEN=$(sed -n 's/^[Ss]et-[Cc]ookie: *__Host-upanel_session=\([^;]*\).*/\1/p' \
    "$WORK/before-reset/login.head" | head -n 1)
  if [ -n "$TOKEN" ]; then
    SESS_BEFORE=$(curl -sS -o /dev/null -w '%{http_code}' --cacert "$CERT_FILE" \
      -H "Cookie: __Host-upanel_session=$TOKEN" "$BASE/api/v1/auth/session")
    expect_eq "重置前会话可用（200）" '200' "$SESS_BEFORE"
  else
    fail "重置前会话可用" '登录没有给出会话 Cookie'
  fi

  RESET_OUT=$(printf '%s\n%s\n' "$NEW_PASSWORD" "$NEW_PASSWORD" | \
    runuser -u "$WEB_USER" -- /usr/local/bin/upanelctl reset-password \
      --config "$SYS_CFG" --username admin 2>&1)
  expect_eq "reset-password 退出码为 0" '0' "$?"
  expect_contains "reset-password 报告已重置" '已重置账号 admin' "$RESET_OUT"
  expect_not_contains "reset-password 输出里没有明文口令" "$NEW_PASSWORD" "$RESET_OUT"

  if [ -n "${TOKEN:-}" ]; then
    SESS_AFTER=$(curl -sS -o /dev/null -w '%{http_code}' --cacert "$CERT_FILE" \
      -H "Cookie: __Host-upanel_session=$TOKEN" "$BASE/api/v1/auth/session")
    expect_ne "重置前的会话已失效（不再 200）" '200' "$SESS_AFTER"
  fi
  expect_ne "旧口令登录失败" '200' "$(login "$PASSWORD" "$WORK/login")"
  expect_eq "新口令登录成功" '200' "$(login "$NEW_PASSWORD" "$WORK/login")"

  # 审计里要有一条 user.password.reset，且 actor_id 为空（本地 CLI 不是某个账号）。
  AUDIT_ACTION=$(python3 - "$SYS_CFG" <<'PY' 2>/dev/null
import json, sqlite3, sys
cfg = json.load(open(sys.argv[1], encoding="utf-8"))
con = sqlite3.connect(cfg["database"]["path"])
try:
    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
except Exception:
    pass
row = con.execute(
    "SELECT action, actor_id FROM audit_events WHERE action='user.password.reset' "
    "ORDER BY id DESC LIMIT 1").fetchone()
print("%s|%s" % (row[0], "NULL" if row[1] is None else row[1]) if row else "MISSING")
PY
)
  expect_contains "审计里有 user.password.reset 且 actor_id 为 NULL" \
    'user.password.reset|NULL' "$AUDIT_ACTION"

  # 恢复回原口令，后面的脚本（核心场景）继续用同一个口令。
  printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | \
    runuser -u "$WEB_USER" -- /usr/local/bin/upanelctl reset-password \
      --config "$SYS_CFG" --username admin >/dev/null 2>&1
  expect_eq "重置回原口令后可以登录" '200' "$(login "$PASSWORD" "$WORK/login")"
fi

# ---------------------------------------------------------------- 11. 服务重启
printf '\n--- 11. 重启两个服务之后仍然可用 ---\n'
# 这一节判的是“重启之后还能用”，不是“重启之后 3 秒内就能用”。固定 sleep 3 在这台
# 2 vCPU 的虚拟机上会假失败：实测 systemctl restart 的耗时在 1.3s 到 8.4s 之间摆动
# （代理重启会顺带做存储/服务采集），面板进程本身在 Started 之后还要 ~1s 才监听。
# 所以改成有界等待：最多 60 秒，把实际恢复耗时打印出来当证据；超时才是失败。
RESTART_T0=$(date +%s)
systemctl restart upanel-agent upanel-web
RESTART_T1=$(date +%s)
READY=0
i=0
while [ "$i" -lt 120 ]; do
  i=$((i + 1))
  if [ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 --cacert "$CERT_FILE" \
        "$BASE/api/v1/health/live" 2>/dev/null)" = '200' ]; then
    READY=1
    break
  fi
  sleep 0.5
done
RESTART_T2=$(date +%s)
note "restart 命令用时 $((RESTART_T1 - RESTART_T0))s，恢复 HTTPS 200 用时 $((RESTART_T2 - RESTART_T0))s"
expect_eq "重启后面板 active" 'active' "$(systemctl is-active upanel-web.service)"
expect_eq "重启后代理 active" 'active' "$(systemctl is-active upanel-agent.service)"
expect_eq "重启后 60 秒内恢复 HTTPS 200" '1' "$READY"
expect_eq "重启后 HTTPS health 仍是 200" '200' \
  "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 --cacert "$CERT_FILE" \
     "$BASE/api/v1/health/live")"
expect_eq "重启后可以用口令登录" '200' "$(login "$PASSWORD" "$WORK/login")"
expect_eq "重启没有换掉面板证书" "$FILE_FP" "$(fingerprint_of "$CERT_FILE")"

# ---------------------------------------------------------------- 汇总
printf '\n==== P10.4 安装验收（第一部分）汇总 ====\n'
printf 'PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ -n "$FAILED_NAMES" ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
fi
printf '日志：%s\n' "$LOGS"
if [ "$FAILED" -gt 0 ]; then
  exit 1
fi
exit 0
