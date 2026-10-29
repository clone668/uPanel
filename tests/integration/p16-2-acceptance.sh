#!/bin/sh
# P16.2 Linux 验收脚本：整机端到端、真实资源与延迟、日志/磁盘/连接容量限制。
#
# 运行位置：**全新安装的** Ubuntu 验收 VM 内，**必须以 root 运行**。这一轮会真的执行
# 发布包里的 install.sh（13 步）与 uninstall.sh，会停/起 systemd 单元，会在
# /var/lib/upanel 与 /etc/upanel 上写东西，会重启一次机器，会造一个坏包让升级失败，
# 再从备份路径把它救回来。**不要在有数据的机器上跑。**
#
# 分相位跑（中间要重启，一条命令跑不完）：
#
#   sudo PHASE=install   PKG_DIR=/root/upanel-pkg sh tests/integration/p16-2-acceptance.sh
#   sudo PHASE=measure   ...                                          # 装好之后立刻测
#   sudo reboot                                                       # 真重启
#   sudo PHASE=boot      ...                                          # 起来之后验自启与数据
#   sudo PHASE=upgrade   ...                                          # 再跑一次安装器 = 升级
#   sudo PHASE=failup    ...                                          # 坏包升级失败 → 用备份救回
#   sudo PHASE=uninstall ...                                          # 默认卸载（保留数据）→ 再装回
#   sudo PHASE=purge     ...                                          # --purge-* 卸载（删前备份）
#
# 每个相位写 $WORK/RESULT-<phase>，并往 $WORK/RESULT-ALL 追加一行。退出码：
# 0 全部通过；1 有失败项；2 环境不满足（不是产品的问题）。NOT RUN 单独计数、不影响
# 退出码——与 P14/P15 各轮一致。
#
# ---------------------------------------------------------------- 这一轮验什么
#
# STAGES §18 对 P16 的验收是「全新安装、重启、升级、失败升级恢复、默认保留数据的卸载
# 通过；从浏览器完成主要用户流程；没有伪成功状态」，P16.2 这一条再加「真实资源使用和
# 延迟测量，日志/磁盘/连接容量限制」。
#
#   A. **全新安装**：干净机器上 `install.sh --source <包>` 一次跑完 13 步，退出码 0。
#      断电点不是"打印了安装完成"，而是下面这些**独立读数**：二进制的位置与权限、
#      配置文件 0640 root:upanel-web、agent.key 0600 root:root、面板库属主是 upanel-web、
#      代理库 root 0700、两个单元 enabled+active、Web 进程 uid 不是 0、HTTPS 健康检查
#      200、端口上服务的证书指纹与装进去的那张**逐字相同**、代理 socket 的属主与权限。
#   B. **依赖缺失不阻断安装**（这台干净 VM 上 nginx/certbot/samba/docker/smartctl 全没有）：
#      第 3 步必须**警告并继续**，不许擅自 apt 装东西，也不许因为缺依赖就拒绝安装。
#      证据是安装仍然成功 + 日志里有"缺 nginx"这类话 + 面板的 capabilit… 页能读出 absent。
#   C. **全新安装不备份**：第 9 步必须说"这台机器上还没有数据库（全新安装），跳过备份"。
#      反过来，升级时它**必须**备份——见 D。
#   D. **升级**（再跑一次安装器）：第 9 步这次不再跳过，备份目录里有两库（含 -wal/-shm）、
#      /etc/upanel（含 agent.key）、MANIFEST.sha256；升级后配置与 agent.key 的 sha256
#      **逐字不变**（不 --write-config 就不覆盖），服务回来了，数据还在（登录仍可用、
#      标记账号还在）。
#   E. **失败升级恢复**：拿一个"校验和自洽但代理二进制是坏的"包升级——第 10 步会把坏
#      二进制拷进去，第 12 步起不来，安装器 die。这时要能证明两件事：①**备份在动二进制
#      之前就已经做完**（目录 + MANIFEST 在，库在里面且 integrity_check ok）；②**再跑一次
#      好包就能救回来**（退出码 0、服务回来、登录可用、数据没丢）。不做自动回滚二进制，
#      这一版明确不做——恢复路径是"备份 + 重跑"，所以这条路径必须真的走得通。
#   F. **默认卸载保留数据**：不带 --purge-* 时删程序与单元，**留**配置、两个库、/srv/upanel；
#      而且**不许**在这一步偷偷备份（输出里不该出现"本次删除前的备份"）。装回去之后
#      标记账号与审计还在——"保留数据"是可读回的事实，不是一句承诺。
#   G. **带清除的卸载先备份**：`--purge-data --purge-config` 默认先备份再删；备份里有
#      agent.key 与两库，库副本 integrity_check ok；删完之后三个目录都不在了，
#      而 /srv/upanel **永远不自动删**。
#   H. **资源与延迟**（measure）：两个进程的空闲 RSS、接口延迟的 p50/p95/max、
#      60 秒空闲里库与日志的增长。全部是现场读数，不是估算。
#   I. **容量限制**（measure）：请求体 1 MiB 上限、日志单页 1000 行上限（超界 400）、
#      单用户 SSE 连接 4 条上限（第 5 条 429）。
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * **上传 2 GiB 上限**：要造一块真的 2 GiB 上传与受管文件根，VM 系统盘只剩 37 GiB，
#     能造但会把这轮拖成半小时；P06 的验收里已有该限额的读数。记为 NOT RUN。
#   * **文本编辑 1 MiB 上限**：同上，属 P06 范围。
#   * **从浏览器完成主要用户流程**：这台 VM 没有浏览器；宿主侧的 Playwright 能打隧道，
#     但那是另一份工件，本轮没做。记为 NOT RUN。
#   * **24.04 的差异**：本轮只跑 22.04（VM-A）。VM-B 走同一份脚本，见 RUNBOOK。
#   * **systemd 之外的自启路径**（容器化安装、非 systemd 发行版）：不在产品范围。

set -u

umask 077

PHASE=${PHASE:-}
PKG_DIR=${PKG_DIR:-/root/upanel-pkg}
WORK=${WORK:-/var/tmp/upanel-accept-p16-2}
TMPD=$WORK/tmp
FACTS=$WORK/facts
LOGS=$WORK/logs

ADMIN=admin
PW='vm-p162-admin-88'
MARK_USER=p16mark
MARK_PW='vm-p162-mark-19'
PORT=9443
BASE="https://127.0.0.1:$PORT"
ORIGIN="$BASE"
CERT=/etc/upanel/tls/panel.pem
CFG=/etc/upanel/upanel.json
KEYFILE=/etc/upanel/agent.key
WEB_DATA=/var/lib/upanel
AGENT_DATA=/var/lib/upanel-agent
PANEL_DB=$WEB_DATA/upanel.db
AGENT_DB=$AGENT_DATA/agent.db
SOCK=/run/upanel/agent.sock
WEB_USER=upanel-web

PASSED=0
FAILED=0
NTRUN=0
FAILED_NAMES=""
NOTRUN_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
notrun() { NTRUN=$((NTRUN + 1)); NOTRUN_NAMES="$NOTRUN_NAMES | $1"; printf 'NOT RUN  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }
stamp() { printf 'STAMP %s %s\n' "$(date -u +%FT%TZ)" "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ne() { if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不该等于 '$3'"; fi; }
expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $(printf '%s' "$3" | tail -c 400)）" ;;
  esac
}
expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不该出现 '$2'" ;;
    *) pass "$1" ;;
  esac
}
expect_nonempty() { if [ -n "$2" ]; then pass "$1"; else fail "$1" "期望非空，实际是空的"; fi; }
expect_empty() { if [ -z "$2" ]; then pass "$1"; else fail "$1" "期望空，实际 '$2'"; fi; }
expect_file_mode() { # $1 名字 $2 路径 $3 期望 "owner:group mode"
  if [ ! -e "$2" ]; then fail "$1" "$2 不存在"; return 0; fi
  _got=$(stat -c '%U:%G %a' "$2" 2>/dev/null)
  expect_eq "$1" "$3" "$_got"
}

if [ "$(id -u)" != "0" ]; then
  printf '本脚本必须以 root 运行：sudo PHASE=... sh %s\n' "$0" >&2
  exit 2
fi
if [ -z "$PHASE" ]; then
  printf '必须给 PHASE（install|measure|boot|upgrade|failup|uninstall|purge）\n' >&2
  exit 2
fi
mkdir -p "$TMPD" "$FACTS" "$LOGS" || exit 2

RESULT=$WORK/RESULT-$PHASE

write_result() {
  {
    printf 'PHASE=%s\n' "$PHASE"
    printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
    printf 'FAILED_NAMES=%s\n' "${FAILED_NAMES:- (none)}"
    printf 'NOTRUN_NAMES=%s\n' "${NOTRUN_NAMES:- (none)}"
  } >"$RESULT"
  printf '%s %s PASS=%s FAIL=%s NOTRUN=%s\n' \
    "$(date -u +%FT%TZ)" "$PHASE" "$PASSED" "$FAILED" "$NTRUN" >>"$WORK/RESULT-ALL"
}
finish() {
  write_result
  printf '\n==== %s: PASS=%s FAIL=%s NOT RUN=%s ====\n' "$PHASE" "$PASSED" "$FAILED" "$NTRUN"
  [ "$FAILED" -eq 0 ] || exit 1
  exit 0
}

# ---- JSON 助手（VM 上有 python3，没有 jq）----
jget() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
src=sys.argv[2]
ns={"d": d, "json": json}
try:
    try:
        print(eval(src, ns))
    except SyntaxError:
        ns["_"]=None
        exec(compile(src, "<jget>", "exec"), ns)
        print("" if ns["_"] is None else ns["_"])
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))' "$1" "$2" 2>/dev/null
}

# sqlite_ok 是**独立于产品**的完整性判据：产品自己说"库是好的"不算数，
# 这里用 python3 的 sqlite3 直接打开副本跑 PRAGMA integrity_check。
sqlite_ok() { # $1 库文件 → 打印 ok / 其他
  python3 -c 'import sqlite3,sys
try:
    con=sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
    print(con.execute("PRAGMA integrity_check").fetchone()[0])
except Exception as exc:
    print("ERR:%s" % exc)' "$1" 2>/dev/null
}

# ---- 面板 HTTP ----
CSRF=""
JAR=$WORK/cookies.txt

api() { # $1 方法  $2 路径  [$3 body] [$4 输出文件名]
  _m=$1; _p=$2; _b=${3:-}; _o=${4:-resp.json}
  if [ -n "$_b" ]; then
    printf '%s' "$_b" >"$TMPD/req.json"
    _code=$(curl -sS --cacert "$CERT" -b "$JAR" -o "$TMPD/$_o" -w '%{http_code}' -X "$_m" "$BASE$_p" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
      --data-binary "@$TMPD/req.json")
  else
    _code=$(curl -sS --cacert "$CERT" -b "$JAR" -o "$TMPD/$_o" -w '%{http_code}' -X "$_m" "$BASE$_p" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF")
  fi
  printf '%s' "$_code"
}

# do_login 把结果写在 LOGIN_CODE / CSRF 两个全局变量里，**不要用 $(...) 接返回值**：
# 命令替换会开子 shell，在子 shell 里赋的 CSRF 出了括号就没了（本轮第一版就是这么错的，
# 表现为"登录 200 但 CSRF 是空的"，然后所有写动作被 403 拒绝——那是产品在正常工作）。
do_login() {
  LOGIN_CODE=$(curl -sS --cacert "$CERT" -c "$JAR" -o "$TMPD/login.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}")
  if [ "$LOGIN_CODE" = "200" ]; then
    CSRF=$(jget "$TMPD/login.json" 'd["data"]["csrf_token"]')
  else
    CSRF=""
  fi
}

health_code() {
  curl -sS --cacert "$CERT" -o "$TMPD/health.json" -w '%{http_code}' --max-time 5 \
    "$BASE/api/v1/health/live"
}

unit_active() { systemctl is-active "$1" 2>/dev/null; }
unit_enabled() { systemctl is-enabled "$1" 2>/dev/null; }
boot_ts() { date -u -d "@$(awk '/^btime/ {print $2}' /proc/stat)" +%FT%TZ; }

# 证书指纹：一个从文件算，一个从端口握手读。两者必须逐字相同。
fp_file() {
  openssl x509 -in "$CERT" -noout -fingerprint -sha256 2>/dev/null \
    | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f'
}
fp_served() {
  echo | openssl s_client -connect "127.0.0.1:$PORT" -servername "$(hostname -s)" "$@" 2>/dev/null \
    | openssl x509 -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f'
}

sha_of() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }

marker_present() { # 标记账号还在吗（0 = 在）
  _c=$(api GET /api/v1/users "" users.json)
  if [ "$_c" != "200" ]; then printf 'HTTP:%s' "$_c"; return 1; fi
  _hit=$(jget "$TMPD/users.json" "any(u['username']=='$MARK_USER' for u in d['data']['items'])")
  printf '%s' "$_hit"
  [ "$_hit" = "True" ]
}

# ================================================================ PHASE=install
phase_install() {
  stamp "install 开始：$PKG_DIR"
  [ -d "$PKG_DIR" ] || { note "找不到发布包目录 $PKG_DIR"; exit 2; }

  # 前置：真的干净吗。不干净就别假装"全新安装"——那会让后面每一条断言都失去意义。
  _dirty=""
  [ -e /etc/upanel ] && _dirty="$_dirty /etc/upanel"
  [ -e "$WEB_DATA" ] && _dirty="$_dirty $WEB_DATA"
  [ -e "$AGENT_DATA" ] && _dirty="$_dirty $AGENT_DATA"
  for u in upanel-web.service upanel-agent.service; do
    [ -e "/etc/systemd/system/$u" ] && _dirty="$_dirty /etc/systemd/system/$u"
  done
  if [ -n "$_dirty" ]; then
    note "机器不干净，无法验'全新安装'：$_dirty"
    notrun "全新安装前置：干净机器" "发现残留：$_dirty"
    finish
  fi
  pass "全新安装前置：干净机器（无 /etc/upanel、无两个数据目录、无单元文件）"

  # ---------- 跑安装器 ----------
  T0=$(date +%s%N)
  printf '%s\n%s\n' "$PW" "$PW" \
    | sh "$PKG_DIR/deploy/scripts/install.sh" --source "$PKG_DIR" --password-stdin \
      >"$LOGS/install.log" 2>&1
  RC=$?
  T1=$(date +%s%N)
  INSTALL_MS=$(( (T1 - T0) / 1000000 ))
  printf 'install_ms=%s\n' "$INSTALL_MS" >"$FACTS/install-time.txt"
  head -c 200000 "$LOGS/install.log" >"$FACTS/install-log-head.txt"

  expect_eq "安装器退出码" "0" "$RC"
  expect_contains "安装器打出完成横幅" "==== 安装完成 ====" "$(cat "$LOGS/install.log")"
  expect_contains "13 步全走到" "[13/13]" "$(cat "$LOGS/install.log")"
  expect_contains "第 1 步校验了发布包" "[1/13] 校验发布包" "$(cat "$LOGS/install.log")"

  # ---------- B. 依赖缺失不阻断 ----------
  _log=$(cat "$LOGS/install.log")
  expect_contains "第 3 步说明依赖事实来自 host-caps" "host-caps（与面板里那一页是同一份事实）" "$_log"
  expect_contains "缺 nginx 时只说缺、不擅自安装" "缺 nginx" "$_log"
  expect_not_contains "没有擅自 apt 安装依赖" "已安装：nginx" "$_log"
  expect_contains "第 3 步给出装依赖的重跑方式" "--install-deps" "$_log"

  # ---------- C. 全新安装不备份 ----------
  expect_contains "第 9 步对全新安装跳过备份" "全新安装），跳过备份" "$_log"

  # ---------- D. 文档随包进本机：面板起不来的时候，人在机器上得有东西可查 ----------
  # P16.4 的交付物之一是四份新文档（用户手册、支持矩阵、已知限制、发布说明）。README 里
  # "看这两份文档"如果指向一个没装到机器上的文件，那句话就是死的——所以在这里数一遍。
  _docs=$(ls -1 /usr/local/share/doc/upanel 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
  for _d in README.md VERSION USER_GUIDE.md SUPPORT_MATRIX.md KNOWN_LIMITATIONS.md RELEASE_NOTES.md; do
    expect_contains "本机文档里有 $_d" "$_d" "$_docs"
  done
  expect_eq "本机文档份数（README + VERSION + 配置样例 + 十份 docs）" "13" \
    "$(ls -1 /usr/local/share/doc/upanel 2>/dev/null | wc -l | tr -d ' ')"

  # ---------- A. 独立读数 ----------
  expect_file_mode "upanel-web 二进制 0755 root:root" /usr/local/bin/upanel-web "root:root 755"
  expect_file_mode "upanelctl 二进制 0755 root:root" /usr/local/bin/upanelctl "root:root 755"
  expect_file_mode "upanel-agent 在 /usr/local/lib/upanel 下 0755" /usr/local/lib/upanel/upanel-agent "root:root 755"
  expect_file_mode "配置文件 0640 root:upanel-web" "$CFG" "root:$WEB_USER 640"
  expect_file_mode "代理密钥 0600 root:root" "$KEYFILE" "root:root 600"
  # 库的属主只能断言"属于谁"，不能断言 mode：0600 还是 0644 取决于进程 umask，
  # 那不是产品契约的一部分，把它写死会让一条无关的 umask 变化变成"产品缺陷"。
  expect_eq "面板库属主是 upanel-web（root 建的库 Web 写不进去）" "$WEB_USER" \
    "$(stat -c %U "$PANEL_DB" 2>/dev/null)"
  expect_file_mode "代理数据目录 0700 root:root" "$AGENT_DATA" "root:root 700"
  expect_eq "两个单元都 enabled" "enabled enabled" "$(unit_enabled upanel-web.service) $(unit_enabled upanel-agent.service)"
  expect_eq "两个单元都 active" "active active" "$(unit_active upanel-web.service) $(unit_active upanel-agent.service)"

  WEB_PID=$(systemctl show -p MainPID --value upanel-web.service)
  AGENT_PID=$(systemctl show -p MainPID --value upanel-agent.service)
  WEB_UID=$(ps -o uid= -p "${WEB_PID:-0}" 2>/dev/null | tr -d ' ')
  AGENT_UID=$(ps -o uid= -p "${AGENT_PID:-0}" 2>/dev/null | tr -d ' ')
  WANT_UID=$(id -u "$WEB_USER" 2>/dev/null)
  expect_ne "Web 进程不是 root" "0" "${WEB_UID:-0}"
  expect_eq "Web 进程就是 upanel-web" "$WANT_UID" "$WEB_UID"
  expect_eq "代理进程是 root（它要碰 root 才能碰的东西）" "0" "$AGENT_UID"

  expect_eq "HTTPS 健康检查 200" "200" "$(health_code)"
  expect_contains "健康响应里有 status" '"status"' "$(cat "$TMPD/health.json")"

  FP_F=$(fp_file)
  FP_S=$(fp_served)
  expect_nonempty "能从文件算出证书指纹" "$FP_F"
  expect_eq "端口上服务的正是装进去的那张证书" "$FP_F" "$FP_S"

  if [ -S "$SOCK" ]; then pass "代理 socket 存在"; else fail "代理 socket 存在" "$SOCK 不存在"; fi
  # SocketMode 是 0660（PLAN 第 4 节）：Web 用户能连、别人连不上。
  expect_file_mode "代理 socket 0660 root:upanel-web" "$SOCK" "root:$WEB_USER 660"

  # ---------- 登录 + 能力页 + 标记数据 ----------
  do_login "$ADMIN" "$PW"
  expect_eq "管理员能登录" "200" "$LOGIN_CODE"
  expect_nonempty "登录拿到了 CSRF 令牌" "$CSRF"

  _code=$(api GET /api/v1/system/capabilities "" caps.json)
  expect_eq "能力探测接口 200（admin）" "200" "$_code"
  # ready 在外层 data 上，host_caps 本身就是报告（generated_at/items/partial/issues）。
  expect_eq "host_caps 已探完（ready）" "True" "$(jget "$TMPD/caps.json" 'd["data"]["ready"]')"
  expect_eq "探完了就有报告，不是 null" "True" "$(jget "$TMPD/caps.json" 'd["data"]["host_caps"] is not None')"
  expect_eq "能力清单 21 条（缺的那几条是 absent，不是消失）" "21" \
    "$(jget "$TMPD/caps.json" 'len(d["data"]["host_caps"]["items"])')"
  expect_eq "systemd 在这台机器上是 present" "present" \
    "$(jget "$TMPD/caps.json" "[i['state'] for i in d['data']['host_caps']['items'] if i['id']=='systemd'][0]")"
  expect_eq "nginx 在这台机器上是 absent（干净 VM 没装）" "absent" \
    "$(jget "$TMPD/caps.json" "[i['state'] for i in d['data']['host_caps']['items'] if i['id']=='nginx'][0]")"
  expect_eq "说不清的每一条都带原因" "True" \
    "$(jget "$TMPD/caps.json" 'all(bool(i.get("reason")) for i in d["data"]["host_caps"]["items"] if i["state"]=="unknown")')"

  _code=$(api POST /api/v1/users \
    "{\"username\":\"$MARK_USER\",\"display_name\":\"P16 标记账号\",\"role\":\"viewer\",\"initial_password\":\"$MARK_PW\"}" \
    users-create.json)
  expect_eq "建了标记账号（卸载/重装之后靠它证明数据还在）" "201" "$_code"
  MARK_ID=$(jget "$TMPD/users-create.json" 'd["data"]["id"]')
  printf 'mark_id=%s\n' "$MARK_ID" >"$FACTS/marker.txt"

  # 用户清单是"库里有东西"的第二个读数：标记账号能列出来，就说明写进库了。
  _code=$(api GET "/api/v1/users" "" users.json)
  expect_eq "用户清单可读（admin）" "200" "$_code"
  USER_COUNT=$(jget "$TMPD/users.json" 'len(d["data"]["items"])')
  expect_eq "库里至少有两个账号（安装器建的 admin + 这次建的标记账号）" "True" \
    "$(python3 -c "print(${USER_COUNT:-0} >= 2)")"

  # 给 boot 相位留下对比基线
  {
    printf 'mark_id=%s\n' "$MARK_ID"
    printf 'install_ms=%s\n' "$INSTALL_MS"
    printf 'fp=%s\n' "$FP_F"
    printf 'user_count=%s\n' "$USER_COUNT"
    printf 'panel_db_sha=%s\n' "$(sha_of "$PANEL_DB")"
    printf 'key_sha=%s\n' "$(sha_of "$KEYFILE")"
    printf 'cfg_sha=%s\n' "$(sha_of "$CFG")"
  } >"$FACTS/install-baseline.env"

  note "安装耗时 ${INSTALL_MS}ms（/usr/bin/time 不在默认镜像里，用 date 前后取差）"
  finish
}

# ================================================================ PHASE=measure
phase_measure() {
  stamp "measure 开始"
  [ -f "$CFG" ] || { note "还没安装：先跑 PHASE=install"; exit 2; }

  do_login "$ADMIN" "$PW"
  if [ "$LOGIN_CODE" != "200" ]; then note "measure 相位登录失败：HTTP $LOGIN_CODE"; fi

  # ---------- H1. 空闲 RSS ----------
  WEB_PID=$(systemctl show -p MainPID --value upanel-web.service)
  AGENT_PID=$(systemctl show -p MainPID --value upanel-agent.service)
  WEB_RSS=$(ps -o rss= -p "${WEB_PID:-0}" 2>/dev/null | tr -d ' ')
  AGENT_RSS=$(ps -o rss= -p "${AGENT_PID:-0}" 2>/dev/null | tr -d ' ')
  expect_nonempty "读到 Web 进程的 RSS（KiB）" "$WEB_RSS"
  expect_nonempty "读到代理进程的 RSS（KiB）" "$AGENT_RSS"
  note "RSS: web=${WEB_RSS}KiB agent=${AGENT_RSS}KiB"

  # ---------- H2. 延迟 ----------
  # health 不需要会话；system/summary 需要（走一次会话校验 + 代理采集）。
  lat() { # $1 路径 $2 次数 → 打印每个样本的秒数
    _i=0
    while [ "$_i" -lt "$2" ]; do
      curl -sS --cacert "$CERT" -b "$JAR" -o /dev/null -w '%{time_total}\n' --max-time 10 \
        "$BASE$1" 2>/dev/null
      _i=$((_i + 1))
    done
  }
  pct() { # $1 文件 $2 分位（0-100）
    sort -n "$1" | awk -v p="$2" '{a[NR]=$1} END { if (NR==0) {print ""; exit} i=int((p/100)*NR); if (i<1) i=1; if (i>NR) i=NR; printf "%.4f", a[i] }'
  }
  lat /api/v1/health/live 20 >"$FACTS/lat-health.txt"
  lat /api/v1/system/summary 20 >"$FACTS/lat-summary.txt"
  for f in health summary; do
    _file=$FACTS/lat-$f.txt
    _n=$(wc -l <"$_file" | tr -d ' ')
    expect_eq "延迟样本数（$f）" "20" "$_n"
    _p50=$(pct "$_file" 50); _p95=$(pct "$_file" 95)
    _max=$(sort -n "$_file" | tail -n 1)
    note "延迟 $f: p50=${_p50}s p95=${_p95}s max=${_max}s"
    printf '%s p50=%s p95=%s max=%s\n' "$f" "$_p50" "$_p95" "$_max" >>"$FACTS/latency.txt"
    # 上限只做"没有离谱"的粗判：回环 + 本机采集，>2 秒就是哪里在阻塞。
    awk -v v="$_p95" 'BEGIN { exit !(v+0 <= 2.0) }' \
      && pass "延迟 p95（$f）不超过 2s" \
      || fail "延迟 p95（$f）不超过 2s" "p95=${_p95}s"
  done

  # ---------- I1. 请求体 1 MiB 上限 ----------
  python3 -c 'import json,sys
sys.stdout.write(json.dumps({"username":"big","display_name":"x"*1100000,"role":"viewer","initial_password":"A1!aaaaaaaaaa"}))' \
    >"$TMPD/big.json"
  _code=$(curl -sS --cacert "$CERT" -b "$JAR" -o "$TMPD/big-resp.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/users" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
    -H "X-CSRF-Token: $CSRF" --data-binary "@$TMPD/big.json")
  expect_eq "超过 1 MiB 的请求体被拒（413）" "413" "$_code"

  # ---------- I2. 日志单页 1000 行上限 ----------
  _code=$(api GET "/api/v1/logs?unit=upanel-web.service&limit=5000" "" logs-big.json)
  expect_eq "日志 limit=5000 越界被拒（400）" "400" "$_code"
  _code=$(api GET "/api/v1/logs?unit=upanel-web.service&limit=1000" "" logs-1000.json)
  expect_eq "日志 limit=1000 接受" "200" "$_code"
  if [ "$_code" != "200" ]; then
    # 400 的响应体必须留档：只有状态码看不出来是哪一条规则在拒。
    note "limit=1000 的响应体：$(head -c 400 "$TMPD/logs-1000.json" 2>/dev/null)"
  fi
  # 直接用 python 读原始响应，不经过 jget：jget 出错时会打印 ERR:…，
  # 那个字符串再被塞进 python 表达式就变成 SyntaxError，看起来像断言写错了。
  _page=$(python3 - "$TMPD/logs-1000.json" <<'PY' 2>/dev/null
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception as exc:
    print("ERR 0 n n %s" % exc)
    sys.exit(0)
es = d["data"]["entries"]
n = len(es)
b = sum(len(e["message"].encode()) for e in es)
print("%d %d %s %s" % (n, b, n <= 1000, b <= 1048576))
PY
)
  _n=$(printf '%s' "$_page" | awk '{print $1}')
  _bytes=$(printf '%s' "$_page" | awk '{print $2}')
  expect_eq "单页条数不超过 1000" "True" "$(printf '%s' "$_page" | awk '{print $3}')"
  expect_eq "单页消息总量不超过 1 MiB" "True" "$(printf '%s' "$_page" | awk '{print $4}')"
  note "日志单页：${_n} 条 / ${_bytes} 字节"

  # ---------- I3. 单用户 SSE 连接上限（4 条，第 5 条 429）----------
  SSE_PIDS=""
  _i=0
  while [ "$_i" -lt 4 ]; do
    curl -sS -N --max-time 25 --cacert "$CERT" -b "$JAR" \
      "$BASE/api/v1/logs/stream?unit=upanel-web.service" >/dev/null 2>&1 &
    SSE_PIDS="$SSE_PIDS $!"
    _i=$((_i + 1))
  done
  sleep 3
  _code=$(curl -sS -o "$TMPD/sse5.json" -w '%{http_code}' --max-time 5 --cacert "$CERT" -b "$JAR" \
    "$BASE/api/v1/logs/stream?unit=upanel-web.service")
  expect_eq "第 5 条并发事件流被限流（429）" "429" "$_code"
  for p in $SSE_PIDS; do kill "$p" 2>/dev/null; done
  wait 2>/dev/null
  # 释放之后还能再开：限流不能变成"一旦撞上就永远打不开"。
  sleep 1
  _code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 --cacert "$CERT" -b "$JAR" \
    "$BASE/api/v1/logs/stream?unit=upanel-web.service")
  case "$_code" in
    200) pass "流断开后名额被释放（又能开一条）" ;;
    000) pass "流已建立且被 --max-time 截断（名额未被永久占住）" ;;
    *) fail "流断开后名额被释放（又能开一条）" "状态码 $_code" ;;
  esac

  # ---------- H3. 磁盘与日志增长（60 秒空闲）----------
  snap() { # $1 标签
    {
      printf 'panel_db_bytes=%s\n' "$(stat -c %s "$PANEL_DB" 2>/dev/null || echo 0)"
      printf 'agent_db_bytes=%s\n' "$(stat -c %s "$AGENT_DB" 2>/dev/null || echo 0)"
      printf 'web_data_bytes=%s\n' "$(du -sb "$WEB_DATA" 2>/dev/null | awk '{print $1}')"
      printf 'agent_data_bytes=%s\n' "$(du -sb "$AGENT_DATA" 2>/dev/null | awk '{print $1}')"
      printf 'journal_lines=%s\n' "$(journalctl -u upanel-web -u upanel-agent --no-pager -n 100000 2>/dev/null | wc -l)"
    } >>"$FACTS/growth-$1.txt"
    # 目录清单也留下来。只看 `du -sb` 一个总数，事后无法回答"到底是哪个文件在长"——
    # 22.04 那一轮就留下一个 313 KB 的增长，只能写成"未查明"（证据目录 README 里有记录）。
    # 这份清单不改任何断言，只是让"长的是什么"有据可查。
    {
      printf '## %s\n' "$1"
      find "$WEB_DATA" "$AGENT_DATA" -maxdepth 1 -type f \
        -printf '%s\t%p\n' 2>/dev/null | sort -k2
    } >>"$FACTS/growth-files.txt"
  }
  : >"$FACTS/growth-before.txt"
  : >"$FACTS/growth-after.txt"
  : >"$FACTS/growth-files.txt"
  snap before
  sleep 60
  snap after
  _b=$(du -sb "$WEB_DATA" "$AGENT_DATA" 2>/dev/null | awk '{s+=$1} END {print s+0}')
  note "两库目录合计 ${_b} 字节；60 秒空闲后见 $FACTS/growth-after.txt"
  _jb=$(awk -F= '/journal_lines/ {print $2}' "$FACTS/growth-before.txt" | tail -n 1)
  _ja=$(awk -F= '/journal_lines/ {print $2}' "$FACTS/growth-after.txt" | tail -n 1)
  expect_eq "60 秒空闲里日志没有失控增长（< 500 行）" "True" \
    "$(python3 -c "print((${_ja:-0} - ${_jb:-0}) < 500)")"
  note "journal 行数：${_jb} → ${_ja}"

  # ---------- 不验的容量项 ----------
  notrun "上传 2 GiB 上限" "P06 已有该限额读数；本轮不造 2 GiB 文件"
  notrun "文本编辑 1 MiB 上限" "属 P06 范围"
  notrun "浏览器完成主要用户流程" "VM 内没有浏览器；Playwright 隧道属另一份工件"
  finish
}

# ================================================================ PHASE=boot
phase_boot() {
  stamp "boot 开始（重启之后）"
  [ -f "$FACTS/install-baseline.env" ] || { note "没有安装基线：先跑 PHASE=install"; exit 2; }
  . "$FACTS/install-baseline.env"

  expect_eq "upanel-web 开机自启并 active" "active" "$(unit_active upanel-web.service)"
  expect_eq "upanel-agent 开机自启并 active" "active" "$(unit_active upanel-agent.service)"
  expect_eq "两个单元都还是 enabled" "enabled enabled" \
    "$(unit_enabled upanel-web.service) $(unit_enabled upanel-agent.service)"
  if [ -S "$SOCK" ]; then pass "重启后代理 socket 又在了"; else fail "重启后代理 socket 又在了" "不存在"; fi
  expect_eq "重启后健康检查 200" "200" "$(health_code)"
  expect_eq "重启后还是那张证书" "$fp" "$(fp_file)"

  # 从 btime 到 Web 单元 active 的时间：这是"开机到能用"的真实延迟。
  BT=$(awk '/^btime/ {print $2}' /proc/stat)
  WEB_SINCE=$(date -u -d "$(systemctl show -p ActiveEnterTimestamp --value upanel-web.service)" +%s 2>/dev/null)
  if [ -n "${WEB_SINCE:-}" ] && [ "${WEB_SINCE:-0}" -gt 0 ]; then
    _secs=$((WEB_SINCE - BT))
    note "开机到 Web active：${_secs}s（btime→ActiveEnterTimestamp）"
    printf 'boot_to_web_seconds=%s\n' "$_secs" >>"$FACTS/boot.txt"
    # 把"这个秒数里有多少是面板自己的"也记下来。2026-10-27 的 24.04 那一轮在这里红了
    # （152s > 120s），查下去发现单线程 TCG 下客户机自己就要 1—2 分钟（snapd 播种 43s、
    # cloud-init 16s），而 upanel-web 自己的 blame 只有 6.3s——没有这条读数，那条红只能
    # 靠事后手工 ssh 进去补测，下一次还得再查一遍。
    _own=$(systemd-analyze blame 2>/dev/null | awk '$2 ~ /^upanel-(web|agent)\.service$/ {printf "%s=%s ", $2, $1}')
    _boot=$(systemd-analyze 2>/dev/null | tr '\n' ' ')
    note "其中面板自身：${_own:-读不到 blame}；整机：${_boot:-读不到 systemd-analyze}"
    { printf 'upanel_own_seconds=%s\n' "${_own:-}"
      printf 'systemd_analyze=%s\n' "${_boot:-}"; } >>"$FACTS/boot.txt"
    awk -v v="$_secs" 'BEGIN { exit !(v >= 0 && v <= 120) }' \
      && pass "开机到 Web 可用不超过 120s" \
      || fail "开机到 Web 可用不超过 120s" "${_secs}s"
  else
    notrun "开机到 Web 可用的秒数" "读不到 ActiveEnterTimestamp"
  fi

  do_login "$ADMIN" "$PW"
  expect_eq "重启后管理员仍能登录（库没坏）" "200" "$LOGIN_CODE"
  expect_eq "重启后标记账号还在" "True" "$(marker_present)"
  expect_eq "重启后配置没有被改写" "$cfg_sha" "$(sha_of "$CFG")"
  expect_eq "重启后代理密钥没有被改写" "$key_sha" "$(sha_of "$KEYFILE")"

  # 采集器与代理之间的链路：能力接口要读得出来（代理活着且认得出这台机器）
  _code=$(api GET /api/v1/system/capabilities "" caps.json)
  expect_eq "重启后能力探测仍可用" "200" "$_code"
  expect_eq "重启后 host_caps 仍是探完的" "True" \
    "$(jget "$TMPD/caps.json" 'd["data"]["ready"]')"

  # 服务没起来时最容易出现的伪成功：页面 200 但代理挂了。这里从代理侧再读一次。
  _code=$(api GET /api/v1/system/summary "" summary.json)
  expect_eq "重启后系统概览 200（代理链路通）" "200" "$_code"
  finish
}

# ================================================================ PHASE=upgrade
phase_upgrade() {
  stamp "upgrade 开始（同一份包再装一次 = 升级）"
  . "$FACTS/install-baseline.env"
  BAK=$WORK/backup-upgrade
  rm -rf "$BAK"

  # 升级前把两者的指纹记下来：升级不许动配置与密钥。
  CFG_BEFORE=$(sha_of "$CFG")
  KEY_BEFORE=$(sha_of "$KEYFILE")

  T0=$(date +%s%N)
  printf '%s\n%s\n' "$PW" "$PW" \
    | sh "$PKG_DIR/deploy/scripts/install.sh" --source "$PKG_DIR" --password-stdin \
      --backup-dir "$BAK" >"$LOGS/upgrade.log" 2>&1
  RC=$?
  T1=$(date +%s%N)
  printf 'upgrade_seconds=%s\n' "$(( (T1 - T0) / 1000000000 ))" >>"$FACTS/timings.txt"

  _log=$(cat "$LOGS/upgrade.log")
  expect_eq "升级退出码 0" "0" "$RC"
  expect_contains "升级走到第 13 步" "[13/13]" "$_log"
  expect_contains "第 9 步这次**没有**跳过备份" "备份在 $BAK" "$_log"
  expect_not_contains "不再是'全新安装跳过备份'" "全新安装），跳过备份" "$_log"

  # ---------- 备份内容是真读数 ----------
  if [ -d "$BAK" ]; then pass "备份目录存在"; else fail "备份目录存在" "$BAK 不存在"; fi
  expect_file_mode "备份清单只有 root 能读（0600）" "$BAK/MANIFEST.sha256" "root:root 600"
  if [ -f "$BAK/MANIFEST.sha256" ]; then pass "备份写了 MANIFEST.sha256"; else fail "备份写了 MANIFEST.sha256" "缺失"; fi
  if [ -e "$BAK/.incomplete" ]; then fail "成功备份不留 .incomplete" "标记还在"; else pass "成功备份不留 .incomplete"; fi
  if [ -f "$BAK/etc-upanel/agent.key" ]; then pass "备份里有 agent.key（秘密与库必须同一份）"; else fail "备份里有 agent.key" "缺失"; fi
  if [ -f "$BAK/panel-db/upanel.db" ]; then pass "备份里有面板库"; else fail "备份里有面板库" "缺失"; fi
  if [ -f "$BAK/agent-db/agent.db" ]; then pass "备份里有代理库"; else fail "备份里有代理库" "缺失"; fi
  # 副本本身是不是好的：用 python3 直接开，不听产品自己的说法。
  _chk=$(sqlite_ok "$BAK/panel-db/upanel.db")
  expect_eq "面板库副本 integrity_check = ok" "ok" "$_chk"
  _chk=$(sqlite_ok "$BAK/agent-db/agent.db")
  expect_eq "代理库副本 integrity_check = ok" "ok" "$_chk"
  # 备份时服务被停过又起回了：备份脚本的 trap 与"起回原本在跑的"这条必须真的成立。
  expect_eq "备份之后两个单元还是 active" "active active" \
    "$(unit_active upanel-web.service) $(unit_active upanel-agent.service)"
  # MANIFEST 能校验：这是"这份备份可自证完整"的唯一硬判据。
  if ( cd "$BAK" && sha256sum -c MANIFEST.sha256 >/dev/null 2>&1 ); then
    pass "MANIFEST.sha256 全部校验通过"
  else
    fail "MANIFEST.sha256 全部校验通过" "$(cd "$BAK" && sha256sum -c MANIFEST.sha256 2>&1 | grep -v ': OK$' | head -3)"
  fi

  # ---------- 升级没有动配置与密钥 ----------
  expect_eq "升级没有改写配置文件" "$CFG_BEFORE" "$(sha_of "$CFG")"
  expect_eq "升级没有改写 agent.key" "$KEY_BEFORE" "$(sha_of "$KEYFILE")"

  # ---------- 服务与数据 ----------
  expect_eq "升级后两个单元 active" "active active" \
    "$(unit_active upanel-web.service) $(unit_active upanel-agent.service)"
  expect_eq "升级后健康检查 200" "200" "$(health_code)"
  do_login "$ADMIN" "$PW"
  expect_eq "升级后管理员能登录" "200" "$LOGIN_CODE"
  expect_eq "升级后标记账号还在" "True" "$(marker_present)"

  # VERSION 是包里的文件，也是安装器拷进 /usr/local/share/doc 的东西：
  # 它变了才说明"这次真的换了包里的东西"，不是空跑一遍。
  if [ -f "$PKG_DIR/VERSION" ]; then
    expect_eq "装进去的 VERSION 与包里的逐字相同" "$(cat "$PKG_DIR/VERSION")" \
      "$(cat /usr/local/share/doc/upanel/VERSION 2>/dev/null)"
  else
    notrun "VERSION 对照" "发布包根目录没有 VERSION"
  fi
  finish
}

# ================================================================ PHASE=failup
phase_failup() {
  stamp "failup 开始（造一次会失败的升级，再救回来）"
  . "$FACTS/install-baseline.env"

  # 坏包：校验和**自洽**（改完重算 SHA256SUMS），但代理二进制是个一跑就退的脚本。
  # 为什么不直接改字节：那样第 1 步就挂了，验不到"动了二进制之后失败"。
  BAD=$WORK/pkgbad
  rm -rf "$BAD"
  cp -a "$PKG_DIR" "$BAD" || { note "复制包失败"; exit 2; }
  printf '#!/bin/sh\nexit 1\n' >"$BAD/bin/upanel-agent"
  chmod 0755 "$BAD/bin/upanel-agent"
  if [ -f "$BAD/SHA256SUMS" ]; then
    # 按**原清单里列出的路径**逐个重算（包里的清单带子目录，如 bin/upanel-agent），
    # 只把被换掉的那一行的哈希更新成新值：坏包在"校验和自洽"这一层必须无懈可击，
    # 否则第 1 步就把它拦下来了，验不到"动了二进制之后才失败"。
    python3 - "$BAD" <<'PY' || note "重算 SHA256SUMS 失败"
import hashlib, os, sys
root = sys.argv[1]
man = os.path.join(root, 'SHA256SUMS')
out = []
for line in open(man):
    line = line.rstrip('\n')
    if not line.strip():
        continue
    _, name = line.split('  ', 1)
    path = os.path.join(root, name)
    if not os.path.isfile(path):
        print('缺少清单里的文件: %s' % name, file=sys.stderr)
        sys.exit(1)
    h = hashlib.sha256(open(path, 'rb').read()).hexdigest()
    out.append("%s  %s\n" % (h, name))
open(man, 'w').write(''.join(out))
PY
  else
    note "包里没有 SHA256SUMS，坏包将按'无校验和'处理"
  fi

  BAK2=$WORK/backup-failup
  rm -rf "$BAK2"
  printf '%s\n%s\n' "$PW" "$PW" \
    | sh "$BAD/deploy/scripts/install.sh" --source "$BAD" --password-stdin \
      --backup-dir "$BAK2" >"$LOGS/failup.log" 2>&1
  RC=$?
  _log=$(cat "$LOGS/failup.log")

  expect_ne "坏包升级没有成功（退出码非 0）" "0" "$RC"
  expect_contains "坏包升级打出了失败横幅" "安装没有完成" "$_log"
  # 失败点在"动了二进制之后"：代理起不来，而不是第 1 步就挂了。
  #
  # 这里钉的不是某一行文案，而是 P16.2 抓到的那个伪成功：代理单元是
  # Type=simple，`systemctl restart` 在 fork 之后立刻返回 0，所以一个马上 exit 1
  # 的二进制能让第 12 步打印"已 enable 并启动"。修法是把"代理真的起来了"
  # （socket 在 + 单元 active）挪到第 13 步做硬校验，装不上就 die。
  # 因此这里要断言的是**第 13 步**喊停，而不是第 12 步。
  expect_contains "失败发生在第 13 步（代理没起来）" "代理 30 秒内没有起来" "$_log"
  expect_contains "喊停时点名了 panel 会只剩 AGENT_UNAVAILABLE" "AGENT_UNAVAILABLE" "$_log"
  expect_contains "喊停时贴出了代理的 journal" "journalctl -u upanel-agent" "$_log"
  expect_not_contains "没有打出安装完成横幅" "==== 安装完成 ====" "$_log"
  expect_not_contains "不是第 1 步校验失败（坏包是自洽的）" "校验和不匹配" "$_log"

  # ① 备份在动二进制之前就做完了 —— 这是"失败升级"能救回来的唯一理由。
  if [ -d "$BAK2" ] && [ -f "$BAK2/MANIFEST.sha256" ]; then
    pass "失败之前备份已经完成（目录 + MANIFEST 在）"
  else
    fail "失败之前备份已经完成" "找不到 $BAK2/MANIFEST.sha256"
  fi
  expect_eq "失败升级的备份里面板库 integrity_check = ok" "ok" "$(sqlite_ok "$BAK2/panel-db/upanel.db")"
  expect_eq "失败升级的备份里代理库 integrity_check = ok" "ok" "$(sqlite_ok "$BAK2/agent-db/agent.db")"
  if ( cd "$BAK2" 2>/dev/null && sha256sum -c MANIFEST.sha256 >/dev/null 2>&1 ); then
    pass "失败升级的备份自身可校验"
  else
    fail "失败升级的备份自身可校验" "MANIFEST 校验不过"
  fi

  # 失败之后数据库本身没被写坏（坏包在第 10 步就跑过迁移了）
  expect_eq "失败之后面板库仍能通过 integrity_check" "ok" "$(sqlite_ok "$PANEL_DB")"
  expect_eq "失败之后代理库仍能通过 integrity_check" "ok" "$(sqlite_ok "$AGENT_DB")"
  # 代理服务确实没起来（这一步是"失败"的证据，不是缺陷）。
  #
  # 这里不能只采一次 `systemctl is-active`：P16.2 在 24.04 上就是这样假红的（Q515）。
  # 单元是 Type=simple，"Started" 一打就进 active，而"主进程 exit 1"要 0.23~0.40 秒
  # 之后才被 systemd 记账；Restart=on-failure + RestartSec=3s 又让这个单元每约 3.4 秒
  # 重来一轮，于是 active 的占空比约一成（11 轮实测 0.234~0.400 秒，
  # 见 evidence 的 active-window-analysis.txt）。单次采样有约 10% 的概率正好落在里面，
  # 坏包明明一次都没起来也会被判红。判据改成两件都不吃这个竞态的事：
  #   ① socket 不存在——"面板用不了代理"的直接判据，坏包永远造不出它；
  #   ② 隔 1.2 秒采两次 active，只有连续两次都是 active 才算真的活着（跨过一轮重启）。
  _st1=$(unit_active upanel-agent.service)
  sleep 1.2
  _st2=$(unit_active upanel-agent.service)
  if [ -S /run/upanel/agent.sock ]; then
    fail "坏包让代理服务起不来（这正是失败现场）" "socket 还在：/run/upanel/agent.sock"
  elif [ "$_st1" = "active" ] && [ "$_st2" = "active" ]; then
    fail "坏包让代理服务起不来（这正是失败现场）" "1.2 秒内两次采样都是 active（$_st1/$_st2）"
  else
    pass "坏包让代理服务起不来（这正是失败现场）: socket 不在，is-active 采样 '$_st1'/'$_st2'"
  fi

  # ② 用好包重跑一次就能救回来
  printf '%s\n%s\n' "$PW" "$PW" \
    | sh "$PKG_DIR/deploy/scripts/install.sh" --source "$PKG_DIR" --password-stdin \
      --backup-dir "$WORK/backup-recover" >"$LOGS/recover.log" 2>&1
  RC=$?
  expect_eq "重跑好包后退出码 0（失败可重入）" "0" "$RC"
  expect_eq "恢复后代理服务 active" "active" "$(unit_active upanel-agent.service)"
  expect_eq "恢复后 Web 服务 active" "active" "$(unit_active upanel-web.service)"
  expect_eq "恢复后健康检查 200" "200" "$(health_code)"
  do_login "$ADMIN" "$PW"
  expect_eq "恢复后管理员能登录" "200" "$LOGIN_CODE"
  expect_eq "恢复后标记账号还在（数据没丢）" "True" "$(marker_present)"
  expect_eq "恢复后代理密钥没被改写" "$key_sha" "$(sha_of "$KEYFILE")"
  finish
}

# ================================================================ PHASE=uninstall
phase_uninstall() {
  stamp "uninstall 开始（默认卸载：保留数据）"
  . "$FACTS/install-baseline.env"

  sh "$PKG_DIR/deploy/scripts/uninstall.sh" >"$LOGS/uninstall.log" 2>&1
  RC=$?
  _log=$(cat "$LOGS/uninstall.log")
  expect_eq "默认卸载退出码 0" "0" "$RC"
  expect_contains "打出卸载完成横幅" "==== 卸载完成 ====" "$_log"

  # 删掉的
  for u in upanel-web.service upanel-agent.service; do
    if [ -e "/etc/systemd/system/$u" ]; then fail "单元文件已删除（$u）" "还在"; else pass "单元文件已删除（$u）"; fi
    expect_not_contains "单元已不再登记（$u）" "$u" "$(systemctl list-unit-files 'upanel-*' --no-pager 2>/dev/null)"
  done
  for p in /usr/local/bin/upanel-web /usr/local/bin/upanelctl /usr/local/lib/upanel; do
    if [ -e "$p" ]; then fail "已删除 $p" "还在"; else pass "已删除 $p"; fi
  done

  # 留着的（默认保留数据）
  if [ -d /etc/upanel ]; then pass "/etc/upanel 保留"; else fail "/etc/upanel 保留" "被删了"; fi
  expect_file_mode "agent.key 还在且仍是 0600" "$KEYFILE" "root:root 600"
  if [ -f "$PANEL_DB" ]; then pass "面板库保留"; else fail "面板库保留" "被删了"; fi
  if [ -f "$AGENT_DB" ]; then pass "代理库保留"; else fail "代理库保留" "被删了"; fi
  if [ -d /srv/upanel ]; then pass "/srv/upanel 保留（永远不会自动删）"; else fail "/srv/upanel 保留" "被删了"; fi

  # 没带 --purge-* 就不该偷偷备份
  expect_not_contains "默认卸载不打印备份行" "本次删除前的备份" "$_log"
  expect_contains "保留清单里点名了配置与两库" "$WEB_DATA" "$_log"

  # 服务真的停了
  expect_ne "服务已停" "active" "$(unit_active upanel-web.service)"
  if curl -sS --cacert "$CERT" -o /dev/null --max-time 3 "$BASE/api/v1/health/live" 2>/dev/null; then
    fail "端口不再响应" "仍有响应"
  else
    pass "端口不再响应（进程真的没了）"
  fi

  # 装回去：数据必须还在。这是"默认保留数据"唯一有意义的证明。
  printf '%s\n%s\n' "$PW" "$PW" \
    | sh "$PKG_DIR/deploy/scripts/install.sh" --source "$PKG_DIR" --password-stdin \
      --backup-dir "$WORK/backup-reinstall" >"$LOGS/reinstall.log" 2>&1
  RC=$?
  expect_eq "重装退出码 0" "0" "$RC"
  expect_eq "重装后健康检查 200" "200" "$(health_code)"
  do_login "$ADMIN" "$PW"
  expect_eq "重装后管理员能登录（账号在库里，不是重装的）" "200" "$LOGIN_CODE"
  expect_eq "重装后标记账号还在" "True" "$(marker_present)"
  expect_eq "重装后 agent.key 没变（秘密仍然解得开）" "$key_sha" "$(sha_of "$KEYFILE")"
  finish
}

# ================================================================ PHASE=purge
phase_purge() {
  stamp "purge 开始（--purge-data --purge-config：删前必须备份）"
  . "$FACTS/install-baseline.env"
  BEFORE_BAK=$(ls -1d /var/backups/upanel/uninstall-* 2>/dev/null | wc -l | tr -d ' ')

  # 两个 yes：--purge-config 要确认删 agent.key；这里没有 --yes-delete-srv，
  # 所以 /srv/upanel 的确认不会被问到。
  printf 'yes\n' | sh "$PKG_DIR/deploy/scripts/uninstall.sh" --purge-data --purge-config \
    >"$LOGS/purge.log" 2>&1
  RC=$?
  _log=$(cat "$LOGS/purge.log")
  expect_eq "带清除的卸载退出码 0" "0" "$RC"

  AFTER_BAK=$(ls -1d /var/backups/upanel/uninstall-* 2>/dev/null | wc -l | tr -d ' ')
  expect_eq "多出一份删除前的备份" "$((BEFORE_BAK + 1))" "$AFTER_BAK"
  NEW_BAK=$(ls -1dt /var/backups/upanel/uninstall-* 2>/dev/null | head -n 1)
  expect_nonempty "找到那份备份的路径" "$NEW_BAK"
  expect_contains "输出里点名了备份路径" "本次删除前的备份: $NEW_BAK" "$_log"
  if [ -f "$NEW_BAK/MANIFEST.sha256" ]; then pass "备份有 MANIFEST"; else fail "备份有 MANIFEST" "缺失"; fi
  if [ -f "$NEW_BAK/etc-upanel/agent.key" ]; then
    pass "备份里有 agent.key（删了也能解秘密）"
    expect_eq "备份里 agent.key 与原来逐字相同" "$key_sha" "$(sha_of "$NEW_BAK/etc-upanel/agent.key")"
  else
    fail "备份里有 agent.key" "缺失"
  fi
  expect_eq "备份里的面板库 integrity_check = ok" "ok" "$(sqlite_ok "$NEW_BAK/panel-db/upanel.db")"
  expect_eq "备份里的代理库 integrity_check = ok" "ok" "$(sqlite_ok "$NEW_BAK/agent-db/agent.db")"
  # 备份里能不能读出标记账号：这是"备份可用"最直接的一次读回。
  _hit=$(python3 -c 'import sqlite3,sys
try:
    con=sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
    rows=con.execute("select count(*) from users where username=?", (sys.argv[2],)).fetchone()
    print(rows[0])
except Exception as exc:
    print("ERR:%s" % exc)' "$NEW_BAK/panel-db/upanel.db" "$MARK_USER" 2>/dev/null)
  expect_eq "备份里能查到标记账号（库是可用的，不只是能打开）" "1" "$_hit"

  # 删干净
  for p in /etc/upanel "$WEB_DATA" "$AGENT_DATA"; do
    if [ -e "$p" ]; then fail "--purge 之后 $p 已删除" "还在"; else pass "--purge 之后 $p 已删除"; fi
  done
  if [ -d /srv/upanel ]; then pass "/srv/upanel 仍然保留（--yes-delete-srv 才动它）"; else fail "/srv/upanel 仍然保留" "被删了"; fi
  finish
}

case "$PHASE" in
  install) phase_install ;;
  measure) phase_measure ;;
  boot) phase_boot ;;
  upgrade) phase_upgrade ;;
  failup) phase_failup ;;
  uninstall) phase_uninstall ;;
  purge) phase_purge ;;
  *) printf '未知 PHASE: %s\n' "$PHASE" >&2; exit 2 ;;
esac
