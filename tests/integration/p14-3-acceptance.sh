#!/bin/sh
# P14.3 Linux 验收脚本：UFW 规则集的受控改写、独立回滚与"只碰带标记的规则"。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。需要 python3、ufw 与
# systemd，不依赖 Go 工具链（用宿主交叉编译好的 Linux 二进制）。
#
# 用法（**必须后台起**，理由与 P14.2 相同）：
#   sudo BIN_DIR=/tmp/upanel-bin setsid nohup sh tests/integration/p14-3-acceptance.sh \
#        >/var/tmp/p143-run.log 2>&1 &
#   然后轮询 /var/tmp/upanel-accept-p14-3/RESULT
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（没有 ufw、起点不干净、这台机器不能从面板
# 改防火墙）。NOT RUN 是**按设计**列出的范围外项（每条都写了为什么、由什么覆盖），
# 单独计数、不影响退出码——与 P14.1/P14.2 一致。
#
# ---------------------------------------------------------------- 为什么 detached
#
# 核心用例会在**停掉 Web 与代理之后**等 120 秒，让 systemd timer 自己把规则退回去。
# 那一段里没有任何进程在替我们记录，结论只能写文件、由宿主轮询。这本身就是被验的那件事：
# 恢复不能依赖有人连着。防火墙这一轮不会掐断 SSH（每条定义里都留着 22/tcp），但脚本仍然
# 必须脱离终端——`ssh` 管道在 Web 重启的窗口里抖动一次就会 SIGPIPE 掉整轮验收。
#
# ---------------------------------------------------------------- 两条看门狗（都不是产品）
#
# 这一轮的风险和 P14.2 不同：改坏了不是"地址没了"，而是"规则把 22 挡了"。所以看门狗判的是
# **UFW 处于启用状态、且没有任何 `from any` 的规则放行 22/tcp**。一旦成立就 `ufw --force
# disable` 并把 22 放回来（停用比补齐安全：停用之后没有任何规则会挡人）。
# 两条互相独立：一条 systemd timer（+200 秒）、一条 sleep 子壳（+215 秒）。它们动手就留
# `watchdog-fired-*` 标记，验收会断言**两个标记都不存在**——产品自己回来了才算过。
#
# ---------------------------------------------------------------- 覆盖的验收点
#
#   A. 写开关：不带 `-firewall-writes` 时 `firewall.config` 查询与三个写动作都不存在（400），
#      带开关后才出现；开关是与 `-network-writes` **分开**的（这一轮全程不带网络写开关）
#   B. 权限：`GET /firewall/config` 未登录 401、viewer 403、admin 200
#   C. 预检判决（**都不改机器**）：坏指纹 → FIREWALL_CONFIG_CHANGED；越界定义 →
#      FIREWALL_DEFINITION_INVALID；没有待确认记录时的 confirm → FIREWALL_NO_PENDING_ROLLBACK
#   D. 管理通路（**核心**）：要启用、但定义里只放行 8443 不放行 22/tcp → FIREWALL_MANAGEMENT_PATH
#      并拦下；把 22/tcp 加回定义后 allowed=true。全程 `ufw status` 仍是 inactive
#   E. 第一次真改：启用 + 22/tcp + 8443/tcp → outcome=verified、`ufw status` active、
#      `ufw show added` 里有 `comment upanel:ssh` 与 `comment upanel:web`、记录落盘 0600、
#      timer 已武装、模板 ExecStart 指向代理二进制的 fwrollback 子命令、默认策略一动没动；
#      手工确认 → 记录 confirmed、timer 没了；再确认一次 → FIREWALL_NO_PENDING_ROLLBACK
#   F. 加缺删多 + 不动手写规则：手工 `ufw allow 9000/tcp`（无 comment）之后再 apply 一份
#      去掉 web、加上 db 的定义 → 9000 还在、upanel:web 没了、upanel:db 在；
#      token 不符的确认被拒（FIREWALL_TOKEN_MISMATCH）
#   G. **独立回滚（核心）**：apply 一份新规则集 → 60 秒时规则仍是新的（窗口没被提前咬掉）
#      → **停掉 Web 与代理** → 到期后规则自己回到旧的 → 记录 rolled_back → journal 里是
#      `upanel-fwrollback@<token>.service` 干的 → 两个看门狗都没动手
#   H. 恢复之后的现场：没有残留 pending 记录、没有残留 timer 单元、Web/代理重新起得来
#   I. 用产品把自己撤干净：apply `{enabled:false, rules:[]}` → UFW 回到 inactive、
#      一条 `upanel:` 规则都不剩，机器回到验收前的状态
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * 浏览器里的真实渲染与交互：VM 内没有 Playwright（Q21），由 SSR 渲染测试
#     （`web/src/components/firewall-write.test.ts`）与宿主 `web-smoke` 覆盖。
#   * 回滚自己失败（记录 `state=failed`）：真机上造它等于把机器弄坏到看门狗之外，
#     风险不对等；单测 `TestFirewallVerifyMismatchRollsBack` 覆盖判决形状。
#   * `ufw` 根本没装的机器（`editable=false` + FIREWALL_UNSUPPORTED）：卸载 ufw 会连容器/
#     VM 自己的转发规则一起带走，那不是一条能安全回头的用例；单测覆盖判决形状。
#   * IPv6 规则、应用配置（`ufw allow 'Nginx Full'`）、端口区间、按接口限定：这一版**明确
#     不支持**，返回 FIREWALL_UNSUPPORTED / FIREWALL_DEFINITION_INVALID；形状由单测覆盖，
#     真机上只验证"手写的 v4 规则不进面板列表、也不被改"（阶段 F 的 9000）。

set -u

umask 077

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p14-3}
TMPD=$WORK/tmp
FACTS=$WORK/facts
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
STATE_DIR=$AGENT_DB_DIR/firewall/rollback
UNIT_DIR=/etc/systemd/system
UFW=/usr/sbin/ufw
USER_RULES=/etc/ufw/user.rules
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19474}
WEB_ADDR=127.0.0.1
# 面板绑回环、走明文 HTTP：这一轮**不需要**从别的地址确认（防火墙的确认不看来源地址，
# 见 CONTRACTS §13.7 / DECISIONS Q462），所以没有理由再把自签证书那一套搬进来。
SCHEME=http
BASE="$SCHEME://$WEB_ADDR:$PORT"
WEB_USER=upanel-web
PASSWORD='vm-p143-admin-52'
VIEWER_PASSWORD='vm-p143-viewer-68'

# 面板管理的那两条规则的端口。8443 而不是 443：真机上 443 可能已经有东西（nginx），
# 而这一轮要验的是"这两条规则是面板加的、改的、删的"，不是"某个服务通不通"。
WEB_PORT=8443
DB_PORT=5432
# 手写规则的端口（没有任何 comment）。与面板的任何一个都不重合。
FOREIGN_PORT=9000

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
JSON_IN=$TMPD/json.in
JSON_OUT=$TMPD/json.out
RESULT=$WORK/RESULT

PASSED=0
FAILED=0
NTRUN=0
FAILED_NAMES=""
NOTRUN_NAMES=""
AGENT_PID=""
WEB_PID=""
WEB_APP_PID=""
JSON_PID=""
JAR=""
CSRF=""
HASH=""

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
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}
expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不该出现 '$2'（实际: $3）" ;;
    *) pass "$1" ;;
  esac
}
in_set() {
  for _v in $3; do
    if [ "$2" = "$_v" ]; then pass "$1"; return 0; fi
  done
  fail "$1" "'$2' 不在闭集 {$3}"
  return 1
}
# json_helper 用 `%s` 打印 Python 值：bool 出来是 `True`/`False`，不是 JSON 的 `true`。
expect_true() { expect_eq "$1" "True" "$2"; }
expect_false() { expect_eq "$1" "False" "$2"; }

# ---- JSON 助手（常驻解释器：TCG 下一次冷启动 8—17 秒） ----
JSON_TAB=$(printf '\t')
JSON_SEQ=0
jget() {
  JSON_SEQ=$((JSON_SEQ + 1)); _seq=$JSON_SEQ
  printf '%s\tget\t%s\t%s\n' "$_seq" "$1" "$2" >&7
  IFS=$JSON_TAB read -r _got _val <&8
  if [ "${_got:-}" != "$_seq" ]; then printf 'ERR:helper-mismatch'; return 0; fi
  printf '%s' "$_val"
}
jbd() { jget "$1" "json.dumps($2)"; }

# ---- 进程 ----

start_agent() {
  "$AGENT" --config "$SYS_CFG" ${1:-} >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 150); do
    [ -S "$SOCK" ] && return 0
    sleep 0.1
  done
  return 1
}

stop_agent() {
  if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  AGENT_PID=""
  rm -f "$SOCK"
}

start_web() {
  runuser -u "$WEB_USER" -- "$WEB" --config "$SYS_CFG" --mode production --db "$WEB_DB" \
    --listen "$WEB_ADDR:$PORT" >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  WEB_APP_PID=""
  for _ in $(seq 1 250); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
      WEB_APP_PID=$(web_child_pid)
      return 0
    fi
    sleep 0.2
  done
  return 1
}

# must_start_web：起不来就**中止**。连锁失败不是证据，是噪音（P14.2 上一轮的教训）。
must_start_web() {
  if start_web; then
    pass "Web 启动在 $WEB_ADDR:$PORT"
    return 0
  fi
  fail "Web 启动在 $WEB_ADDR:$PORT" "$(tail -5 "$WEB_LOG" 2>/dev/null)"
  {
    printf '== 监听端口的进程\n'; ss -ltnp 2>/dev/null | grep -F ":$PORT"
    printf '== web.log 尾部\n'; tail -40 "$WEB_LOG" 2>/dev/null
  } >"$FACTS/web-bind-failed.txt" 2>&1
  write_result
  exit 1
}

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
  if [ -n "${WEB_APP_PID:-}" ] && kill -0 "$WEB_APP_PID" 2>/dev/null; then
    kill -TERM "$WEB_APP_PID" 2>/dev/null
    sleep 0.5
    kill -KILL "$WEB_APP_PID" 2>/dev/null
  fi
  WEB_APP_PID=""
  for _ in $(seq 1 150); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
    sleep 0.2
  done
  return 1
}

web_child_pid() {
  for _ in $(seq 1 50); do
    _c=$(tr ' ' '\n' <"/proc/$WEB_PID/task/$WEB_PID/children" 2>/dev/null | grep -m1 -E '^[0-9]+$')
    if [ -n "$_c" ]; then printf '%s' "$_c"; return 0; fi
    sleep 0.1
  done
  printf '%s' "$WEB_PID"
}

stop_json() {
  if [ -n "$JSON_PID" ] && kill -0 "$JSON_PID" 2>/dev/null; then
    kill -TERM "$JSON_PID" 2>/dev/null
    wait "$JSON_PID" 2>/dev/null
  fi
}

write_result() {
  {
    printf 'PASS=%s\nFAIL=%s\nNOTRUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
    printf 'FAILED_NAMES=%s\n' "$FAILED_NAMES"
    printf 'NOTRUN_NAMES=%s\n' "$NOTRUN_NAMES"
    printf 'FINISHED=%s\n' "$(date -u +%FT%TZ)"
  } >"$RESULT"
}

cleanup() {
  systemctl stop p143-watchdog.timer >/dev/null 2>&1 || true
  rm -f "$UNIT_DIR/p143-watchdog.timer" "$UNIT_DIR/p143-watchdog.service"
  systemctl daemon-reload >/dev/null 2>&1 || true
  stop_json
  stop_web
  stop_agent
  write_result
}

# ---- HTTP ----

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $BASE" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}; _keep=${6:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    _code=$(curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $_csrf" \
      -d @"$WORK/req.json")
  else
    _code=$(curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $BASE" -H "X-CSRF-Token: $_csrf")
  fi
  if [ -n "$_keep" ]; then cp "$WORK/resp.json" "$FACTS/$_keep.json" 2>/dev/null; fi
  printf '%s' "$_code"
}
rget() { jget "$WORK/resp.json" "$1"; }

preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST \
    "$BASE/api/v1/operations/preflight" \
    -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  if [ -n "${3:-}" ]; then cp "$WORK/resp.json" "$FACTS/preflight-$3.json" 2>/dev/null; fi
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}
pget() { jget "$WORK/preflight.json" "$1"; }
pblockers() { pget 'json.dumps([b["code"] for b in (d["data"].get("blockers") or [])])'; }
pblocktext() { pget '" ".join((b["message"] or "") for b in (d["data"].get("blockers") or []))'; }

submit_job() {
  printf '{"preflight_id": "%s", "confirmation_phrase": "%s"}' "$1" "${3:-}" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $2" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  if [ -n "${4:-}" ]; then cp "$WORK/resp.json" "$FACTS/submit-$4.json" 2>/dev/null; fi
  printf '%s' "$_code"
}
job_id_of() {
  _v=$(jget "$1" 'd["data"]["job_id"]')
  case "$_v" in ERR:*) printf '' ;; *) printf '%s' "$_v" ;; esac
}
wait_job() {
  if [ -z "${1:-}" ]; then printf 'no-job-id'; return 0; fi
  _i=0
  while [ "$_i" -lt 900 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$1" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jget "$WORK/job.json" 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}
jget_job() { jget "$WORK/job.json" "$1"; }

# ---- ufw 的现场事实 ----

# show_added：`ufw show added` 的原话。**它和启用状态无关**（Q460），所以判"面板管着哪些
# 规则"只能用这一份，不能用 `ufw status`。
show_added() { "$UFW" show added 2>/dev/null; }
# ufw_active：判据是**整行** `Status: active`，不能只 grep `active`——
# `ufw status` 的第一行在未启用时是 `Status: inactive`，而 `inactive` 里含 `active`
# 这个词。写成 `grep -qi active` 会让"未启用"永远读成"已启用"（本轮实测踩到的坑）。
ufw_active() { "$UFW" status 2>/dev/null | head -n1 | grep -qiE '^Status: active' && echo yes || echo no; }
# managed_ids：现场里带面板标记的规则标识（排序后一行一个）。
managed_ids() {
  show_added | sed -n "s/.*comment 'upanel:\([^']*\)'.*/\1/p" | sort | tr '\n' ' '
}
# foreign_allowed：手写规则（没有 comment）里放行的端口，用来证明"面板没碰它们"。
foreign_rule_present() { show_added | grep -F "ufw allow $1/tcp" | grep -vq 'upanel:' && echo yes || echo no; }
record_file() { printf '%s/%s.json' "$STATE_DIR" "$1"; }
record_field() {
  if [ ! -f "$1" ]; then printf 'MISSING'; return 0; fi
  jget "$1" "d.get(\"$2\")"
}
timer_unit() { printf '%s/upanel-fwrollback@%s.timer' "$UNIT_DIR" "$1"; }
# default_policy：默认策略的判据是 /etc/default/ufw 里那三行，**不是** `ufw status verbose`。
# inactive 时 status verbose 只打印 `Status: inactive`（VM 实测），拿它比"改前/改后"两头都是
# 空串，断言永真；而 /etc/default/ufw 启用与否都读得到，且它才是 ufw 重启后的生效来源。
default_policy() {
  grep -E '^DEFAULT_(INPUT|OUTPUT|FORWARD)_POLICY' /etc/default/ufw 2>/dev/null | tr '\n' ' '
}
timer_active() { systemctl is-active --quiet "upanel-fwrollback@$1.timer" 2>/dev/null && echo yes || echo no; }
timer_next() {
  systemctl show "upanel-fwrollback@$1.timer" -p NextElapseUSecMonotonic 2>/dev/null |
    sed -n 's/^NextElapseUSecMonotonic=//p'
}

# ---- 防火墙写路径的入口状态 ----

fw_config() { api "$JAR" "$CSRF" GET /api/v1/firewall/config "" "$1"; }
# refresh_hash：把当前指纹取进 $HASH。每次 apply 之前都必须重新取——上一轮的改动已经
# 让指纹变了，拿旧值提交只会得到 FIREWALL_CONFIG_CHANGED，而那条断言是给"故意用错指纹"用的。
refresh_hash() { fw_config "$1" >/dev/null; HASH=$(rget 'd["data"].get("config_hash") or ""'); }

# ---- 看门狗 ----

write_watchdog() {
  cat >"$WORK/watchdog.sh" <<WATCH
#!/bin/sh
# 不是产品的一部分：VM 的救命绳。只在"UFW 启用、且没有任何 from any 的规则放行 22/tcp"
# 时动手，动手就留标记——验收会因为那个标记判 FAIL。
who=\${1:-unknown}
mkdir -p $WORK/logs
if [ "\$("$UFW" status 2>/dev/null | head -n1 | grep -ciE '^Status: active')" = "0" ]; then
  printf '%s %s not-needed: ufw 不处于启用状态\n' "\$(date -u +%FT%TZ)" "\$who" >>$WORK/logs/watchdog-\$who.log
  exit 0
fi
if "$UFW" show added 2>/dev/null | grep -qE '^ufw (allow|limit) (22/tcp|from any to any port 22 proto tcp)( |\$)'; then
  printf '%s %s not-needed: 22/tcp 有放行\n' "\$(date -u +%FT%TZ)" "\$who" >>$WORK/logs/watchdog-\$who.log
  exit 0
fi
printf '%s %s FIRED: ufw 启用而 22/tcp 没有放行，正在停用 ufw\n' "\$(date -u +%FT%TZ)" "\$who" \
  >>$WORK/logs/watchdog-\$who.log
: >$WORK/watchdog-fired-\$who
"$UFW" --force disable >>$WORK/logs/watchdog-\$who.log 2>&1
"$UFW" allow 22/tcp >>$WORK/logs/watchdog-\$who.log 2>&1
WATCH
  chmod 0700 "$WORK/watchdog.sh"
}

arm_watchdogs() {
  # 先把救命绳本身写出来。少了这一行，`$WORK/watchdog.sh` 不存在，两条看门狗在到期
  # 那一刻只会各自打一行 "cannot open"，而"看门狗没有动手"就变成一条**永远不会红**的
  # 断言——失败的方向恰恰是最坏的那个（真把 22 挡了也没人救）。
  write_watchdog
  # 绝对时刻（OnCalendar），不用 OnActiveSec：产品的 disarm 会 `daemon-reload`，
  # 而 reload 会把相对计时器按 reload 那一刻重新起算（Q457 的同一个坑）。
  WD_ARMED_AT=$(date +%s)
  WD_FIRE=$(date -d "@$(( WD_ARMED_AT + 200 ))" '+%Y-%m-%d %H:%M:%S')
  cat >"$UNIT_DIR/p143-watchdog.service" <<UNIT
[Unit]
Description=P14.3 acceptance fail-safe (not part of upanel)

[Service]
Type=oneshot
ExecStart=/bin/sh $WORK/watchdog.sh systemd-timer
UNIT
  cat >"$UNIT_DIR/p143-watchdog.timer" <<UNIT
[Unit]
Description=P14.3 acceptance fail-safe timer (not part of upanel)

[Timer]
OnCalendar=$WD_FIRE
AccuracySec=1s

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload >/dev/null 2>&1
  if ! systemctl start p143-watchdog.timer >/dev/null 2>&1; then
    fail "看门狗 timer 武装" "$(systemctl status p143-watchdog.timer --no-pager -l 2>&1 | tail -6)"
  elif [ "$(systemctl is-active p143-watchdog.timer 2>/dev/null)" != "active" ]; then
    fail "看门狗 timer 处于 active" "$(systemctl status p143-watchdog.timer --no-pager -l 2>&1 | tail -6)"
  else
    note "看门狗 systemd timer 定在 $WD_FIRE"
  fi
  setsid nohup /bin/sh -c "sleep 215; /bin/sh $WORK/watchdog.sh sleep-shell" \
    >"$WORK/logs/watchdog-shell.out" 2>&1 &
  note "看门狗已武装：systemd timer 定在 $WD_FIRE，sleep 子壳 +215s"
}

watchdog_fired() { ls "$WORK"/watchdog-fired-* 2>/dev/null | head -n1; }

# begin_write <动作> <参数> <留档名> [短语]：再认证 → 预检 → 提交，结果放全局变量。
begin_write() {
  W_REAUTH=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
  W_PRE_CODE=$(preflight "$1" "$2" "preflight-$3")
  W_ALLOWED=$(pget 'd["data"]["allowed"]')
  W_BLOCKERS=$(pblockers)
  W_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
  _pref=$(pget 'd["data"]["preflight_id"]')
  _phrase=${4:-}
  if [ -z "$_phrase" ]; then _phrase=$W_PHRASE; fi
  W_SUBMIT_CODE=$(submit_job "$_pref" "p143-$3-$(date +%s%N)" "$_phrase" "$3")
  W_JOB=$(job_id_of "$WORK/submit.json")
  W_STATUS=$(wait_job "$W_JOB")
}

# why_not_verified <留档名>：没到 verified 时把 observed 与 hint 打出来并留档。
why_not_verified() {
  [ "$(jget_job 'd["data"]["result"]["outcome"]')" = "verified" ] && return 0
  cp "$WORK/job.json" "$FACTS/job-$1.json" 2>/dev/null
  note "$1 没到 verified：status=$(jget_job 'd["data"]["status"]') outcome=$(jget_job 'd["data"]["result"].get("outcome")') hint=$(jget_job 'd["data"]["result"].get("recovery_hint") or ""')"
}

# 三个定义：都用同一个形状，改的只有规则集与启用状态。
def_enable_ssh_web() {
  printf '{"enabled":true,"rules":[{"id":"ssh","action":"allow","port":22,"proto":"tcp","from":""},{"id":"web","action":"allow","port":%s,"proto":"tcp","from":""}]}' "$WEB_PORT"
}
def_no_ssh() {
  printf '{"enabled":true,"rules":[{"id":"web","action":"allow","port":%s,"proto":"tcp","from":""}]}' "$WEB_PORT"
}
def_enable_ssh_db() {
  printf '{"enabled":true,"rules":[{"id":"ssh","action":"allow","port":22,"proto":"tcp","from":""},{"id":"db","action":"allow","port":%s,"proto":"tcp","from":""}]}' "$DB_PORT"
}
def_off() { printf '{"enabled":false,"rules":[]}'; }

# apply_def <定义> <留档名> [短语]：走完一次完整的 apply 并把结果留在 W_* 里。
apply_def() {
  refresh_hash "config-before-$2"
  begin_write firewall.apply \
    "{\"expected_config_hash\":\"$HASH\",\"definition\":$1}" "$2" "${3:-}"
}

# ---------------------------------------------------------------- 起跑

install -d -m 0700 "$WORK" "$WORK/logs" "$TMPD" "$FACTS"
# 0711 而不是 0700：Web 以非 root 的 $WEB_USER 跑，它得能**穿过** WORK 走到自己的
# webdata 目录，但不需要列这个目录里有什么（验收的留档、日志全在这里）。0700 会让
# upanel-web 在 `stat webdata` 上就 permission denied——P14.2 踩过，这里照抄那一行。
chmod 0711 "$WORK"
rm -f "$WORK"/watchdog-fired-* 2>/dev/null
rm -rf "$WEB_DB_DIR"
install -d -m 0750 "$WEB_DB_DIR"
chown "$WEB_USER" "$WEB_DB_DIR" 2>/dev/null || true
: >"$AGENT_LOG"
: >"$WEB_LOG"

mkfifo "$JSON_IN" "$JSON_OUT" 2>/dev/null || true
python3 "$SCRIPT_DIR/json_helper.py" <"$JSON_IN" >"$JSON_OUT" 2>"$TMPD/json_helper.log" &
JSON_PID=$!
exec 7>"$JSON_IN" 8<"$JSON_OUT"

trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

echo "== 0. 环境"
stamp start
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用"; else fail "python3 可用" "缺少 python3"; fi
if [ -f "$SCRIPT_DIR/json_helper.py" ]; then pass "JSON 助手在同目录"; else
  fail "JSON 助手在同目录" "缺 $SCRIPT_DIR/json_helper.py，脚本无法继续"; write_result; exit 2
fi
if command -v runuser >/dev/null 2>&1; then pass "runuser 可用"; else fail "runuser 可用" "缺少 runuser"; fi

# ufw 是这一轮的前提：没有它就没有可验的东西（那是"环境不满足"，不是"验失败"）。
if [ -x "$UFW" ]; then pass "ufw 存在（$UFW）"; else
  fail "ufw 存在（$UFW）" "这台 VM 没装 ufw，P14.3 无法验收"; write_result; exit 2
fi
UFW_VERSION=$("$UFW" --version 2>/dev/null | head -n1)
note "ufw 版本：$UFW_VERSION"
if systemctl list-unit-files ufw.service >/dev/null 2>&1; then pass "ufw.service 在 systemd 里"; else
  note "没有 ufw.service（容器里常见）：不影响本脚本，回滚用的是代理自己的模板单元"
fi

# 起点必须干净：上一轮如果没跑完，`upanel:` 规则与 enabled 会留下来，而阶段 C/D 要断言
# "预检不改机器"、"起点一条面板规则都没有"——残留会让那些断言红得说不清原因。
# 这里判环境不满足并把清理命令打出来，**不替操作者动**：那是上一轮的现场。
STALE_IDS=$(managed_ids)
if [ -n "$STALE_IDS" ]; then
  fail "起点没有上一轮留下的面板规则" "残留：$STALE_IDS"
  note "清干净再跑：$UFW --force disable && $UFW --force reset"
  write_result; exit 2
fi
if [ "$(ufw_active)" = "yes" ]; then
  fail "起点 ufw 不处于启用状态" "现在是 active：先 $UFW --force disable 再跑"
  write_result; exit 2
fi
pass "起点 ufw 不处于启用状态，且没有面板规则"
DEFAULT_BEFORE=$(default_policy)
note "起点的默认策略：${DEFAULT_BEFORE:-读不到}"
expect_ne "起点读得到默认策略（否则下面的比对是空的）" "" "$DEFAULT_BEFORE"

if id -u "$WEB_USER" >/dev/null 2>&1; then pass "服务账号 $WEB_USER 存在"; else
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" && pass "服务账号 $WEB_USER 已创建"
fi

{
  printf '== identity\n'; uname -a; lsb_release -ds 2>/dev/null; systemd --version | head -n1
  printf '== ufw\n'; "$UFW" --version 2>&1; "$UFW" status verbose 2>&1; "$UFW" show added 2>&1
  printf '== /etc/default/ufw 策略行\n'; grep -E '^DEFAULT_' /etc/default/ufw 2>&1
  printf '== user.rules 尾部\n'; tail -n 20 "$USER_RULES" 2>&1
  printf '== binary sha256\n'; sha256sum "$AGENT" "$WEB" "$CTL" 2>/dev/null
  printf '== script sha256\n'; sha256sum "$0"
} >"$FACTS/env.txt" 2>&1

arm_watchdogs

# ---------------------------------------------------------------- 1. 装配

echo
echo "== 1. 装配（先不带写开关）"
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
chmod 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -rf "$STATE_DIR"

cat >"$SYS_CFG" <<JSON
{
  "web": {
    "mode": "production",
    "listen_address": "$WEB_ADDR:$PORT",
    "shutdown_timeout_seconds": 10,
    "allowed_origins": ["http://127.0.0.1:$PORT", "http://localhost:$PORT"],
    "metrics": {
      "enabled": false,
      "interval_seconds": 5,
      "raw_retention_seconds": 3600,
      "minute_retention_seconds": 7200,
      "hour_retention_seconds": 14400,
      "max_rows": 100000
    }
  },
  "agent": {
    "socket_path": "$SOCK",
    "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER",
    "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DB",
    "secret_key_path": "$KEY_FILE"
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"

if [ -f "$KEY_FILE" ]; then
  pass "代理密钥文件已存在（沿用上一轮）"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
  fi
fi

if start_agent; then pass "代理启动（无 -firewall-writes）"; else
  fail "代理启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; write_result; exit 1
fi

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
for suffix in "" "-wal" "-shm"; do
  [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
done

# ---------------------------------------------------------------- 2. 默认关闭

echo
echo "== 2. 写路径默认关闭（两个开关是分开的）"
must_start_web

CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$CODE"
if [ "$CODE" != "200" ]; then
  fail "登录失败，后续无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"; write_result; exit 1
fi
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin.json")
if [ -n "$CSRF" ]; then pass "拿到 CSRF 令牌"; else fail "拿到 CSRF 令牌" "空"; write_result; exit 1; fi

CODE=$(api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"initial_password\":\"$VIEWER_PASSWORD\",\"role\":\"viewer\"}" \
  user-viewer)
in_set "建 viewer 201/200" "$CODE" "200 201"
CODE=$(login viewer "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$CODE"

CODE=$(api "$JAR" "$CSRF" GET /api/v1/firewall/config "" config-writes-off)
expect_eq "写开关关闭时 GET /firewall/config 是 400" "400" "$CODE"
expect_contains "关闭时的错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(cat "$WORK/resp.json" 2>/dev/null)"
expect_contains "关闭时代理说的是「未登记的查询」" "未登记的查询" "$(cat "$WORK/resp.json" 2>/dev/null)"

FAKE_TOKEN=frb_00000000000000000000000000000000
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"\",\"definition\":$(def_enable_ssh_web)}" preflight-apply-writes-off)
expect_eq "写开关关闭时 firewall.apply 预检 400" "400" "$CODE"
expect_contains "关闭时命中的是 firewall.apply 这一条" "firewall.apply" "$(cat "$WORK/preflight.json" 2>/dev/null)"
expect_contains "apply 报的是「未登记的动作」" "未登记的动作" "$(cat "$WORK/preflight.json" 2>/dev/null)"
CODE=$(preflight firewall.confirm "{\"token\":\"$FAKE_TOKEN\"}" preflight-confirm-writes-off)
expect_eq "写开关关闭时 firewall.confirm 预检 400" "400" "$CODE"
CODE=$(preflight firewall.rollback "{\"token\":\"$FAKE_TOKEN\"}" preflight-rollback-writes-off)
expect_eq "写开关关闭时 firewall.rollback 预检 400" "400" "$CODE"
expect_eq "关闭期间 ufw 仍不启用" "no" "$(ufw_active)"
expect_eq "关闭期间没有写出任何面板规则" "" "$(managed_ids)"
if [ -d "$STATE_DIR" ]; then fail "关闭期间不该有回滚记录目录" "$STATE_DIR"; else pass "关闭期间没有回滚记录目录"; fi

# ---------------------------------------------------------------- 3. 打开写路径

echo
echo "== 3. 打开写路径（-firewall-writes，**不带** -network-writes）"
stop_web
stop_agent
if start_agent -firewall-writes; then pass "代理以 -firewall-writes 启动"; else
  fail "代理以 -firewall-writes 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; write_result; exit 1
fi
must_start_web
CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin2)
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin2.json")

CODE=$(fw_config config-on)
expect_eq "GET /firewall/config 200" "200" "$CODE"
CFG_MANAGER=$(rget 'd["data"]["manager"]')
CFG_EDITABLE=$(rget 'd["data"]["editable"]')
CFG_INSTALLED=$(rget 'd["data"]["installed"]')
CFG_ENABLED=$(rget 'd["data"]["enabled"]')
HASH=$(rget 'd["data"].get("config_hash") or ""')
expect_eq "管理器判成 ufw" "ufw" "$CFG_MANAGER"
expect_true "ufw 装了" "$CFG_INSTALLED"
expect_false "ufw 现在没启用" "$CFG_ENABLED"
expect_true "这台机器可改防火墙" "$CFG_EDITABLE"
if [ "$CFG_EDITABLE" != "True" ]; then
  note "不可改原因：$(rget 'json.dumps(d["data"]["reasons"], ensure_ascii=False)')"
  fail "验收前提：这台机器必须能改防火墙" "editable=false"
  write_result; exit 2
fi
expect_eq "面板还没管任何规则" "0" "$(rget 'len(d["data"]["rules"] or [])')"
expect_eq "没有待确认记录" "None" "$(rget 'd["data"]["pending"]')"
expect_ne "给了 config_hash" "" "$HASH"

_anon=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/firewall/config" -H "Origin: $BASE")
expect_eq "未登录 GET /firewall/config 是 401" "401" "$_anon"
VJAR=$WORK/viewer.jar
VCSRF=$(csrf_of "$WORK/login-viewer.json")
CODE=$(api "$VJAR" "$VCSRF" GET /api/v1/firewall/config "" config-viewer)
expect_eq "viewer GET /firewall/config 是 403" "403" "$CODE"

# ---------------------------------------------------------------- 4. 预检判决

echo
echo "== 4. 预检判决（都不许改动机器）"
BEFORE_ADDED=$(show_added)

CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"sha256:deadbeef\",\"definition\":$(def_enable_ssh_web)}" preflight-bad-hash)
expect_eq "坏指纹的预检 200" "200" "$CODE"
expect_false "坏指纹 allowed=false" "$(pget 'd["data"]["allowed"]')"
expect_contains "坏指纹命中 FIREWALL_CONFIG_CHANGED" "FIREWALL_CONFIG_CHANGED" "$(pblockers)"

# 定义形状不合法：**在参数解码这一步就被拒**（代理的 Decode 先跑 ValidateFirewallDefinition），
# 于是拿到的是 400 VALIDATION_ERROR，而不是"预检 200 + FIREWALL_DEFINITION_INVALID"。
# 这比阻塞项更好：更早、更便宜，且不需要人再去读一份预检报告。所以这里断言的是 400。
# 代理侧 Assess 里那个 FIREWALL_DEFINITION_INVALID 是纵深防御（Decode 放行才走得到），
# 真机上不可达——由单测覆盖它的形状。
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"$HASH\",\"definition\":{\"enabled\":true,\"rules\":[{\"id\":\"web\",\"action\":\"allow\",\"port\":0,\"proto\":\"tcp\",\"from\":\"\"}]}}" \
  preflight-bad-port)
expect_eq "端口越界在解码就被拒（400）" "400" "$CODE"
expect_contains "端口越界报 VALIDATION_ERROR" "VALIDATION_ERROR" "$(cat "$WORK/preflight.json")"
expect_contains "端口越界点名了范围" "port 必须在 1—65535 之间" "$(cat "$WORK/preflight.json")"
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"$HASH\",\"definition\":{\"enabled\":true,\"rules\":[{\"id\":\"Bad Id\",\"action\":\"allow\",\"port\":80,\"proto\":\"tcp\",\"from\":\"\"}]}}" \
  preflight-bad-id)
expect_eq "标识形状越界在解码就被拒（400）" "400" "$CODE"
expect_contains "标识形状越界报 VALIDATION_ERROR" "VALIDATION_ERROR" "$(cat "$WORK/preflight.json")"
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"$HASH\",\"definition\":{\"enabled\":true,\"rules\":[{\"id\":\"web\",\"action\":\"log\",\"port\":80,\"proto\":\"tcp\",\"from\":\"\"}]}}" \
  preflight-bad-action)
expect_eq "动作越界在解码就被拒（400）" "400" "$CODE"
expect_contains "动作越界报 VALIDATION_ERROR" "VALIDATION_ERROR" "$(cat "$WORK/preflight.json")"
expect_contains "动作越界点名了闭集" "allow/deny/reject/limit" "$(cat "$WORK/preflight.json")"
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"$HASH\",\"definition\":{\"enabled\":true,\"rules\":[{\"id\":\"web\",\"action\":\"allow\",\"port\":80,\"proto\":\"tcp\",\"from\":\"::1\"}]}}" \
  preflight-bad-from)
# IPv6 来源在**解码**这一层被拒（这一版明确不支持 v6），同样是 400。
expect_ne "IPv6 来源被拒" "200" "$CODE"
expect_contains "IPv6 来源报 VALIDATION_ERROR" "VALIDATION_ERROR" "$(cat "$WORK/preflight.json")"

CODE=$(preflight firewall.confirm "{\"token\":\"$FAKE_TOKEN\"}" preflight-confirm-nothing)
expect_eq "没有待确认记录时 confirm 预检 200" "200" "$CODE"
expect_false "没有待确认记录时 confirm 被拦" "$(pget 'd["data"]["allowed"]')"
expect_contains "命中 FIREWALL_NO_PENDING_ROLLBACK" "FIREWALL_NO_PENDING_ROLLBACK" "$(pblockers)"
CODE=$(preflight firewall.rollback "{\"token\":\"$FAKE_TOKEN\"}" preflight-rollback-nothing)
expect_contains "rollback 也命中 FIREWALL_NO_PENDING_ROLLBACK" "FIREWALL_NO_PENDING_ROLLBACK" "$(pblockers)"

# 管理通路：要启用、但定义里只有 8443，没有 22/tcp。
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"$HASH\",\"definition\":$(def_no_ssh)}" preflight-no-ssh)
expect_eq "缺 SSH 放行的预检 200" "200" "$CODE"
expect_false "缺 SSH 放行时 allowed=false" "$(pget 'd["data"]["allowed"]')"
expect_contains "命中 FIREWALL_MANAGEMENT_PATH" "FIREWALL_MANAGEMENT_PATH" "$(pblockers)"
expect_contains "阻塞项说清了是 SSH" "SSH" "$(pblocktext)"
# 停用状态下的同一份定义不该被拦：停用的时候没有任何规则会挡人。
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"$HASH\",\"definition\":{\"enabled\":false,\"rules\":[{\"id\":\"web\",\"action\":\"allow\",\"port\":$WEB_PORT,\"proto\":\"tcp\",\"from\":\"\"}]}}" \
  preflight-no-ssh-off)
expect_true "停用状态下缺 SSH 放行不拦" "$(pget 'd["data"]["allowed"]')"

expect_eq "预检全程没有改动 ufw 的规则" "$BEFORE_ADDED" "$(show_added)"
expect_eq "预检全程 ufw 仍不启用" "no" "$(ufw_active)"

# ---------------------------------------------------------------- 5. 第一次真改

echo
echo "== 5. 第一次真改：启用 + 22/tcp + $WEB_PORT/tcp"
apply_def "$(def_enable_ssh_web)" apply-enable
expect_eq "apply 预检 allowed" "True" "$W_ALLOWED"
expect_contains "apply 预检给了 RECONFIGURE 短语" "RECONFIGURE " "$W_PHRASE"
expect_eq "apply 任务 succeeded" "succeeded" "$W_STATUS"
why_not_verified "apply-enable"
expect_eq "apply 结果 outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
expect_true "读回来 ufw 是启用状态" "$(jget_job 'd["data"]["result"]["observed_state"]["enabled"]')"
expect_eq "内核事实：ufw 现在 active" "yes" "$(ufw_active)"
IDS=$(managed_ids)
expect_contains "现场有面板的 ssh 规则" "ssh" "$IDS"
expect_contains "现场有面板的 web 规则" "web" "$IDS"
show_added >"$FACTS/show-added-after-first-apply.txt"
expect_contains "ssh 规则的写法带 comment" "ufw allow 22/tcp comment 'upanel:ssh'" "$(cat "$FACTS/show-added-after-first-apply.txt")"
expect_eq "默认策略一动没动" "$DEFAULT_BEFORE" "$(default_policy)"

TOKEN_WEB=$(jget_job 'd["data"]["result"]["data"]["rollback_token"]')
DEADLINE_WEB=$(jget_job 'd["data"]["result"]["data"]["deadline"]')
WINDOW_WEB=$(jget_job 'd["data"]["result"]["data"]["window_seconds"]')
expect_eq "回滚窗口是 120 秒" "120" "$WINDOW_WEB"
expect_ne "结果里给了回滚标识" "" "$TOKEN_WEB"
case $TOKEN_WEB in
  frb_*) pass "回滚标识的形状是 frb_ + 32 位十六进制" ;;
  *) fail "回滚标识的形状是 frb_ + 32 位十六进制" "$TOKEN_WEB" ;;
esac
REC_WEB=$(record_file "$TOKEN_WEB")
if [ -f "$REC_WEB" ]; then
  pass "回滚记录落盘"
  expect_eq "记录状态是 pending" "pending" "$(record_field "$REC_WEB" state)"
  expect_eq "记录里的到期时刻与结果一致" "$DEADLINE_WEB" "$(record_field "$REC_WEB" deadline)"
  _mode=$(stat -c '%a %U' "$REC_WEB" 2>/dev/null)
  expect_eq "记录是 root 0600" "600 root" "$_mode"
else
  fail "回滚记录落盘" "$REC_WEB 不存在"
fi
expect_eq "timer 已武装" "yes" "$(timer_active "$TOKEN_WEB")"
_TNEXT=$(timer_next "$TOKEN_WEB")
expect_ne "timer 真的会开火（NextElapse 不是 infinity）" "infinity" "$_TNEXT"
expect_ne "timer 有下一次触发时刻" "" "$_TNEXT"
expect_contains "模板里的二进制就是本次验收用的那一份" \
  "ExecStart=\"$AGENT\"" "$(cat "$UNIT_DIR/upanel-fwrollback@.service" 2>/dev/null)"
expect_contains "模板跑的是代理二进制的 fwrollback 子命令" \
  "fwrollback --token %i" "$(cat "$UNIT_DIR/upanel-fwrollback@.service" 2>/dev/null)"
# 计时的单元是**每个 token 一个**（`upanel-fwrollback@<token>.timer`，`OnActiveSec` 写的是
# 这一轮的窗口）。没有 `@.timer` 模板——模板只有 service 那一个，timer 是逐 token 生成的。
expect_contains "这一轮的 timer 写的是 120 秒窗口" "OnActiveSec=120s" \
  "$(cat "$(timer_unit "$TOKEN_WEB")" 2>/dev/null)"
expect_contains "这一轮的 timer 指向自己的 service" "Unit=upanel-fwrollback@$TOKEN_WEB.service" \
  "$(cat "$(timer_unit "$TOKEN_WEB")" 2>/dev/null)"
# comment 在 ufw 自己的数据文件里是十六进制（Q460 的第四条）：这是"我们说的是实话"的证据，
# 不是产品行为，所以记 NOTE 而不是断言。
if grep -q 'upanel:ssh' "$USER_RULES" 2>/dev/null; then
  note "user.rules 里出现明文 comment（与 Q460 记录的十六进制不符，值得再看一眼）"
else
  note "user.rules 里没有明文 comment：与 Q460 的实测一致（ufw 存成十六进制）"
fi
cp "$USER_RULES" "$FACTS/user.rules-after-first-apply" 2>/dev/null

# 手工确认。
W_REAUTH=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
begin_write firewall.confirm "{\"token\":\"$TOKEN_WEB\"}" confirm-web
expect_eq "confirm 任务 succeeded" "succeeded" "$W_STATUS"
expect_eq "confirm 结果 outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
expect_eq "记录变成 confirmed" "confirmed" "$(record_field "$REC_WEB" state)"
expect_eq "timer 已经停掉" "no" "$(timer_active "$TOKEN_WEB")"
expect_eq "timer 单元文件被删掉" "no" "$([ -f "$(timer_unit "$TOKEN_WEB")" ] && echo yes || echo no)"
expect_eq "确认之后没有待确认记录" "None" "$(fw_config config-after-confirm >/dev/null; rget 'd["data"]["pending"]')"

CODE=$(preflight firewall.confirm "{\"token\":\"$TOKEN_WEB\"}" preflight-confirm-again)
expect_contains "再确认一次命中 FIREWALL_NO_PENDING_ROLLBACK" "FIREWALL_NO_PENDING_ROLLBACK" "$(pblockers)"

# ---------------------------------------------------------------- 6. 加缺删多 + 不动手写规则

echo
echo "== 6. 加缺删多；管理员手写的规则一条都不动"
"$UFW" allow "$FOREIGN_PORT/tcp" >/dev/null 2>&1
expect_eq "手工加了一条没有 comment 的放行" "yes" "$(foreign_rule_present "$FOREIGN_PORT")"
FOREIGN_BEFORE=$(show_added | grep -F "$FOREIGN_PORT/tcp")
apply_def "$(def_enable_ssh_db)" apply-swap
expect_eq "swap 任务 succeeded" "succeeded" "$W_STATUS"
why_not_verified "apply-swap"
expect_eq "swap 结果 outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
IDS=$(managed_ids)
expect_contains "upanel:db 加上了" "db" "$IDS"
expect_not_contains "upanel:web 被删掉了" "web" "$IDS"
expect_contains "upanel:ssh 还在" "ssh" "$IDS"
expect_eq "手写的那条规则原样还在" "$FOREIGN_BEFORE" "$(show_added | grep -F "$FOREIGN_PORT/tcp")"
TOKEN_DB=$(jget_job 'd["data"]["result"]["data"]["rollback_token"]')
REC_DB=$(record_file "$TOKEN_DB")

# token 不符的确认必须被拒（此刻有一条真的 pending 记录）。
CODE=$(preflight firewall.confirm "{\"token\":\"$FAKE_TOKEN\"}" preflight-token-mismatch)
expect_false "token 不符时 confirm 被拦" "$(pget 'd["data"]["allowed"]')"
expect_contains "命中 FIREWALL_TOKEN_MISMATCH" "FIREWALL_TOKEN_MISMATCH" "$(pblockers)"

# 两条回滚记录不许叠加。
refresh_hash config-pending
CODE=$(preflight firewall.apply \
  "{\"expected_config_hash\":\"$HASH\",\"definition\":$(def_enable_ssh_web)}" preflight-while-pending)
expect_false "已有 pending 时 apply 被拦" "$(pget 'd["data"]["allowed"]')"
expect_contains "命中 FIREWALL_ROLLBACK_PENDING" "FIREWALL_ROLLBACK_PENDING" "$(pblockers)"

W_REAUTH=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
begin_write firewall.confirm "{\"token\":\"$TOKEN_DB\"}" confirm-db
expect_eq "确认 swap 之后记录是 confirmed" "confirmed" "$(record_field "$REC_DB" state)"
"$UFW" delete allow "$FOREIGN_PORT/tcp" >/dev/null 2>&1
expect_eq "手写规则被验收自己清掉（不是产品删的）" "no" "$(foreign_rule_present "$FOREIGN_PORT")"

# ---------------------------------------------------------------- 7. 独立回滚（核心）

echo
echo "== 7. 独立回滚：不确认 → 停掉 Web 与代理 → 到期自己退回去"
apply_def "$(def_enable_ssh_web)" apply-for-rollback
expect_eq "回滚用例的 apply succeeded" "succeeded" "$W_STATUS"
why_not_verified "apply-for-rollback"
expect_eq "回滚用例的结果 outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
TOKEN_FAR=$(jget_job 'd["data"]["result"]["data"]["rollback_token"]')
DEADLINE_FAR=$(jget_job 'd["data"]["result"]["data"]["deadline"]')
REC_FAR=$(record_file "$TOKEN_FAR")
DL_EPOCH=$(date -d "$DEADLINE_FAR" +%s)
T0=$(date +%s)
note "回滚用例：token=$TOKEN_FAR deadline=$DEADLINE_FAR（T0+$((DL_EPOCH - T0))s）"
expect_eq "改完之后 db 那条不见了" "no" "$(managed_ids | grep -qw db && echo yes || echo no)"
expect_eq "改完之后 web 那条在了" "yes" "$(managed_ids | grep -qw web && echo yes || echo no)"
expect_eq "timer 已武装" "yes" "$(timer_active "$TOKEN_FAR")"

# 停服务：从这一刻起，把规则退回去的只能是 systemd timer 自己。
stop_web
stop_agent
stamp services-stopped
expect_eq "Web 与代理都已停掉（端口不通）" "000" "$(curl -s -o /dev/null -m 2 -w '%{http_code}' "$BASE/api/v1/health/live")"

# 窗口不许被提前咬掉：过 60 秒时规则必须还是新的。
WAIT60=$((T0 + 60 - $(date +%s)))
[ "$WAIT60" -gt 0 ] && sleep "$WAIT60"
stamp t-plus-60
expect_eq "60 秒时 web 那条还在（窗口没被提前咬掉）" "yes" "$(managed_ids | grep -qw web && echo yes || echo no)"
expect_eq "60 秒时 timer 仍武装" "yes" "$(timer_active "$TOKEN_FAR")"

# 到期之后轮询，把"第几秒回来的"记进证据。
RECOVER_AT=""
RECOVER_DEADLINE=$((DL_EPOCH + 45))
while :; do
  NOW=$(date +%s)
  if [ "$NOW" -lt "$DL_EPOCH" ]; then sleep 2; continue; fi
  if ! managed_ids | grep -qw web; then
    if managed_ids | grep -qw db; then RECOVER_AT=$NOW; break; fi
  fi
  if [ "$NOW" -ge "$RECOVER_DEADLINE" ]; then break; fi
  sleep 2
done
stamp after-deadline
if [ -n "$RECOVER_AT" ]; then
  note "规则在 deadline 之后 $((RECOVER_AT - DL_EPOCH))s 退回来（T0+$((RECOVER_AT - T0))s）"
  expect_eq "回滚发生在 deadline 之后（没有提前咬掉窗口）" "yes" \
    "$([ "$RECOVER_AT" -ge "$DL_EPOCH" ] && echo yes || echo no)"
  expect_eq "回滚发生在 deadline+45s 之内" "yes" \
    "$([ "$RECOVER_AT" -le "$RECOVER_DEADLINE" ] && echo yes || echo no)"
else
  fail "到期后 45 秒内规则自己退回去" "现在管着：$(managed_ids)（deadline 已过 $(( $(date +%s) - DL_EPOCH ))s）"
fi
IDS_AFTER=$(managed_ids)
expect_eq "退回之后 web 那条不在了" "no" "$(printf '%s' "$IDS_AFTER" | grep -qw web && echo yes || echo no)"
expect_eq "退回之后 db 那条回来了" "yes" "$(printf '%s' "$IDS_AFTER" | grep -qw db && echo yes || echo no)"
expect_eq "退回之后 ufw 仍是启用状态（退回的是规则集，不是把防火墙关掉）" "yes" "$(ufw_active)"
expect_eq "记录变成 rolled_back" "rolled_back" "$(record_field "$REC_FAR" state)"
expect_eq "timer 已经结束" "no" "$(timer_active "$TOKEN_FAR")"
cp "$USER_RULES" "$FACTS/user.rules-after-rollback" 2>/dev/null
show_added >"$FACTS/show-added-after-rollback.txt"

journalctl -u "upanel-fwrollback@$TOKEN_FAR.service" --no-pager >"$FACTS/journal-fwrollback.txt" 2>&1
journalctl -u "upanel-fwrollback@$TOKEN_FAR.timer" --no-pager >>"$FACTS/journal-fwrollback.txt" 2>&1
systemctl status "upanel-fwrollback@$TOKEN_FAR.service" --no-pager -l \
  >>"$FACTS/journal-fwrollback.txt" 2>&1
JOURNAL=$(tr '\n' ' ' <"$FACTS/journal-fwrollback.txt")
expect_contains "回滚单元是被 systemd 启动的" "Starting uPanel firewall rollback ($TOKEN_FAR)" "$JOURNAL"
expect_contains "journal 里有那个回滚单元自己的输出" "回滚 $TOKEN_FAR 已处理" "$JOURNAL"
expect_contains "回滚进程就是代理二进制（journal 里的进程标识）" "upanel-agent[" "$JOURNAL"
expect_contains "回滚单元正常结束" "Finished uPanel firewall rollback ($TOKEN_FAR)" "$JOURNAL"
expect_contains "回滚单元没有失败" "Deactivated successfully" "$JOURNAL"
expect_not_contains "回滚单元没有报失败状态" "Failed with result" "$JOURNAL"

# 看门狗不许动手。等的是**武装时刻** +232 秒，不是 T0 +232：两条看门狗都是从脚本
# 开头就起算的，按 T0 等会白等一两分钟。
WAIT=$((WD_ARMED_AT + 232 - $(date +%s)))
[ "$WAIT" -gt 0 ] && sleep "$WAIT"
FIRED=$(watchdog_fired)
if [ -n "$FIRED" ]; then
  fail "看门狗没有动手" "动了：$FIRED（说明规则把 22 挡了）"
else
  pass "看门狗没有动手（产品自己退回来的）"
fi
for who in sleep-shell systemd-timer; do
  # 两条都**必须**留下日志。没有日志不是"它还没到点"，而是"它压根没跑"——
  # 而"看门狗没有动手"这条断言只有在它确实跑过之后才有意义。缺日志一律 FAIL，
  # 不给"收尾时还没到点"留后门：到这一行时武装时刻已过 232 秒，两条都该醒过了。
  if [ -f "$WORK/logs/watchdog-$who.log" ]; then
    cp "$WORK/logs/watchdog-$who.log" "$FACTS/" 2>/dev/null
    expect_contains "看门狗 $who 醒来时 22/tcp 有放行（not-needed）" "not-needed" \
      "$(cat "$WORK/logs/watchdog-$who.log")"
  else
    fail "看门狗 $who 留下了记录" "没有日志：无法证明它跑过（那条 not-fired 的断言因此是空的）"
  fi
done
stamp phase7-done

# ---------------------------------------------------------------- 8. 恢复之后

echo
echo "== 8. 恢复之后：重新起服务，现场干净"
if start_agent -firewall-writes; then pass "代理重新启动"; else
  fail "代理重新启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"
fi
must_start_web
CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin5)
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin5.json")
expect_eq "恢复后登录 200" "200" "$CODE"
CODE=$(fw_config config-after-rollback)
expect_eq "恢复后 GET /firewall/config 200" "200" "$CODE"
expect_eq "恢复后没有待确认记录" "None" "$(rget 'd["data"]["pending"]')"
expect_eq "恢复后读回的规则集是退回后的那一份" "db ssh " "$(managed_ids)"
LEFTOVER=$(ls "$UNIT_DIR"/upanel-fwrollback@*.timer 2>/dev/null | wc -l | tr -d ' ')
expect_eq "没有残留的 timer 单元文件" "0" "$LEFTOVER"
LIVE_TIMERS=$(systemctl list-timers --all --no-pager 2>/dev/null | grep -c 'upanel-fwrollback@' || true)
expect_eq "没有残留的 fwrollback timer" "0" "$LIVE_TIMERS"
if grep -q 'panic' "$AGENT_LOG" "$WEB_LOG" 2>/dev/null; then
  fail "日志里没有 panic" "$(grep -h panic "$AGENT_LOG" "$WEB_LOG" | head -2)"
else
  pass "日志里没有 panic"
fi

# 审计：被拒的写尝试留 failure，成功提交留 success。action 列记的是接口动作，
# 要看"和防火墙有关"，落在 details 里的 http_path 上。
AUD=$(python3 - "$WEB_DB" <<'PY'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True, timeout=15)
    row = db.execute(
        "select"
        " (select count(*) from audit_events"
        "   where outcome='failure' and redacted_details like '%/api/v1/firewall%'),"
        " (select count(*) from audit_events"
        "   where outcome='success' and action='job.submit')"
    ).fetchone()
    print("%d %d" % (row[0], row[1]))
except Exception as exc:
    print("ERR %s" % exc)
PY
)
AUD_DENIED=$(printf '%s' "$AUD" | awk '{print $1}')
AUD_SUBMITTED=$(printf '%s' "$AUD" | awk '{print $2}')
if [ "${AUD_DENIED:-0}" -gt 0 ] 2>/dev/null; then
  pass "被拒的防火墙请求留下了 failure 审计（$AUD_DENIED 条）"
else
  fail "被拒的防火墙请求留下了 failure 审计" "原始输出：$AUD"
fi
if [ "${AUD_SUBMITTED:-0}" -ge 3 ] 2>/dev/null; then
  pass "成功提交的任务留下了 success 审计（$AUD_SUBMITTED 条）"
else
  fail "成功提交的任务留下了 success 审计" "原始输出：$AUD"
fi

# ---------------------------------------------------------------- 9. 用产品把自己撤干净

echo
echo "== 9. 用产品撤干净：{enabled:false, rules:[]}"
apply_def "$(def_off)" apply-off
expect_eq "撤干净的任务 succeeded" "succeeded" "$W_STATUS"
why_not_verified "apply-off"
expect_eq "撤干净的结果 outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
expect_eq "ufw 回到不启用" "no" "$(ufw_active)"
expect_eq "一条面板规则都不剩" "" "$(managed_ids)"
TOKEN_OFF=$(jget_job 'd["data"]["result"]["data"]["rollback_token"]')
REC_OFF=$(record_file "$TOKEN_OFF")
W_REAUTH=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
begin_write firewall.confirm "{\"token\":\"$TOKEN_OFF\"}" confirm-off
expect_eq "撤干净的记录确认掉" "confirmed" "$(record_field "$REC_OFF" state)"
expect_eq "最后没有残留的 pending 记录" "None" "$(fw_config config-final >/dev/null; rget 'd["data"]["pending"]')"
expect_eq "默认策略到最后也没被改过" "$DEFAULT_BEFORE" "$(default_policy)"
show_added >"$FACTS/show-added-final.txt"

# 明确未验项
notrun "浏览器里的渲染与交互" "VM 内没有 Playwright（Q21），由 SSR 渲染测试 web/src/components/firewall-write.test.ts 覆盖"
notrun "回滚自己失败（记录 state=failed）" "真机上造它等于把机器弄坏在两条看门狗之外，风险不对等；单测覆盖"
notrun "ufw 根本没装的机器（FIREWALL_UNSUPPORTED）" "卸载 ufw 会连 VM 自己的转发规则一起带走；单测覆盖判决形状"
notrun "IPv6 规则、应用配置、端口区间、按接口限定" "这一版明确不支持，返回 FIREWALL_DEFINITION_INVALID/FIREWALL_UNSUPPORTED；形状由单测覆盖"

echo
echo "== 结果"
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NTRUN"
[ -n "$FAILED_NAMES" ] && printf 'FAIL 明细:%s\n' "$FAILED_NAMES"
[ -n "$NOTRUN_NAMES" ] && printf 'NOT RUN 明细:%s\n' "$NOTRUN_NAMES"
stamp done
write_result
if [ "$FAILED" -ne 0 ]; then exit 1; fi
exit 0
