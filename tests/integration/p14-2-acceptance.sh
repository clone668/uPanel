#!/bin/sh
# P14.2 Linux 验收脚本：网络地址的受控改写、独立回滚与断连恢复。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。只需要 python3 与 util-linux，
# 不依赖 Go 工具链（用宿主交叉编译好的 Linux 二进制）。
#
# 用法（**必须后台起**，原因见下面"为什么 detached"）：
#   sudo BIN_DIR=/tmp/upanel-bin setsid nohup sh tests/integration/p14-2-acceptance.sh \
#        >/var/tmp/p142-run.log 2>&1 &
#   然后轮询 /var/tmp/upanel-accept-p14-2/RESULT
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（这台机器不在面板可改范围内，
# 或者缺 python3/netplan/runuser/openssl 这类前提）。NOT RUN 是**按设计**列出的
# 范围外项（每条都写了为什么、以及由什么覆盖），单独计数，不影响退出码——与 P14.1 一致。
#
# ---------------------------------------------------------------- 为什么 detached
#
# 这个脚本的核心用例会**故意把本机改成一个不可达地址**，然后停掉 Web 与代理，等 120 秒让
# systemd timer 自己把网络退回来。从那一步开始，任何靠 SSH 驱动的东西都会断线——包括
# 一个把输出写到 SSH 管道的脚本（写完就 SIGPIPE）。所以脚本必须脱离终端运行，结论写文件，
# 由宿主轮询。这本身就是被验的那件事的一部分：**恢复不能依赖有人连着**。
#
# ---------------------------------------------------------------- 两个看门狗（都不是产品）
#
# 如果产品没把网络带回来，这台 VM 就再也进不去了（QEMU 用 slirp + hostfwd，`-monitor none`）。
# 所以脚本自己装两条**互相独立**的抢救措施：一条 systemd timer（+200 秒）、一条 `sleep`
# 子壳（+215 秒）。它们只在"ens4 上已经没有 slirp 网段地址"时才动手，动手时留下
# `watchdog-fired-*` 标记。验收里明确断言**两个标记都不存在**——产品自己回来了才算过；
# 看门狗只要动过手，这一条就判 FAIL，哪怕机器最后是通的。
#
# ---------------------------------------------------------------- 覆盖的验收点
#
#   A. 环境与写入口的开关：不带 `-network-writes` 时 `network.config` 查询与三个写动作
#      都不存在（400），带开关后才出现——"默认关闭"在真机上是被证明过的，不是注释里的承诺（Q452）
#   B. 权限：`GET /network/config` 未登录 401、viewer 403、admin 200
#   C. 预检的判决：坏指纹（NETWORK_CONFIG_CHANGED）、禁用地址段（127.0.0.0/8）、
#      没有待确认记录时的 confirm（NETWORK_NO_PENDING_ROLLBACK）
#   D. 复杂 netplan：多一份把 ens4 当成员的桥接配置时，写入被判 `NETWORK_UNSUPPORTED`
#      并且理由点名文件；删掉之后又变回可改
#   E. 同址静态改写（连通性活着，所以能验确认语义）：
#      · 预检 allowed=true 且给出 RECONFIGURE 短语
#      · 执行结果 outcome=verified，回滚记录落盘（root 0600）、timer 已武装
#      · **调用方自己写 arrived_on=新地址 也没用**：从 127.0.0.1 发起的确认被拒
#      · 从新地址发起的确认通过，timer 停掉、记录变 confirmed
#      · 再确认一次是 NETWORK_NO_PENDING_ROLLBACK
#      · 改回 DHCP 并确认，机器回到 DHCP
#   F. **断连重连（核心）**：改成不可达地址 10.99.99.99/24 → 地址确实换了、内核事实核对过
#      → 从 127.0.0.1 发起确认被拒（此时的候选地址是那个不可达地址）
#      → **停掉 Web 与代理** → 60 秒时仍是 10.99.99.99（窗口没被提前咬掉）
#      → 到期后地址自己回到 10.0.2.x、面板文件回到 DHCP 版本、记录 state=rolled_back
#      → journal 里是 `upanel-netrollback@<token>.service` 干的
#      → 两个看门狗都没动手
#   G. 恢复之后的现场：没有残留的 pending 记录、没有残留的 timer 单元、Web/代理重新起得来
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * 浏览器里的真实渲染与"断线重连"的交互：VM 内没有 Playwright（Q21），由 SSR 渲染
#     测试与宿主 `web-smoke` 覆盖。
#   * 网卡被改名/拔掉（`NETWORK_TARGET_CHANGED` 的另一半）：VM 里改网卡名要重启，
#     重启会把这次的 timer 语义一起冲掉，属于另一组用例。
#   * 回滚自己失败（恢复也恢复不了）的 `failed` 分支：真机上造它等于故意把机器弄坏到
#     看门狗之外，风险不对等；单测 `TestApplyRollsBackWhenKernelDisagrees` 覆盖判决形状。

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

WORK=${WORK:-/var/tmp/upanel-accept-p14-2}
TMPD=$WORK/tmp
FACTS=$WORK/facts
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
STATE_DIR=$AGENT_DB_DIR/network/rollback
NETPLAN_DIR=/etc/netplan
UNIT_DIR=/etc/systemd/system
PANEL_PREFIX=90-upanel-
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19473}
WEB_ADDR=127.0.0.1
# 面板绑非回环地址时**必须**开 TLS：`config.CheckTransport` 会直接拒绝“明文管理网络”
# （会话 Cookie 带 `__Host-` 前缀，只在 HTTPS 下回传）。阶段 6.2 要把 Web 改绑到
# $CUR_ADDR 上去验“从新地址确认”，所以这一轮**全程 HTTPS + 自签证书**，不做双
# scheme 分支——两套 scheme 只会在“和验收对象无关的地方”多出一种失败。
SCHEME=https
BASE="$SCHEME://$WEB_ADDR:$PORT"
TLS_DIR=/etc/upanel/tls
TLS_CERT=$TLS_DIR/panel.crt
TLS_KEY=$TLS_DIR/panel.key
WEB_USER=upanel-web
PASSWORD='vm-p142-admin-47'
VIEWER_PASSWORD='vm-p142-viewer-63'

# 不可达候选地址：slirp 的虚拟网段是 10.0.2.0/24，这个地址不在里面，也没有默认路由，
# 所以改完之后宿主的 hostfwd 到 :22 立刻失效——"断开浏览器"这件事不需要模拟，它就是真的。
CANDIDATE=10.99.99.99
CANDIDATE_PREFIX=24

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
IFACE=""
CUR_ADDR=""
CUR_GW=""
CUR_PREFIX=""

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
# 这两个断言只认这一种写法——别处（curl 拿到的原始 JSON 文本）才是小写。
expect_true() { expect_eq "$1" "True" "$2"; }
expect_false() { expect_eq "$1" "False" "$2"; }

# ---- HTTP（全程 HTTPS，自签证书） ----
#
# 这一轮的面板绑过非回环地址，必须开 TLS，证书是脚本自己签的。所有调用统一放宽校验：
# 少写一个 -k 就是一个 000，而 000 在验收里长得和“产品挂了”一模一样。`-k` 对明文
# URL 无害，所以不按阶段切。
curl() { command curl -k "$@"; }

# ---- JSON 助手（常驻解释器：24.04 走 TCG 时一次冷启动 8—17 秒） ----
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
  # ${1:-} 是额外的参数（-network-writes）。
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

# start_web_on <地址>：Web 绑在这个地址上。
#
# 为什么要把地址换掉：`network.confirm` 的凭据是**服务端从连接里取的到达地址**。
# 要验"从新地址确认能过、从别处确认会被拒"，就得真的让 Web 分别绑在两个地址上，
# 让内核在这两种情形下给出不同的本地地址——这比在参数里塞 arrived_on 诚实得多
# （塞进去的那个本来就该被忽略）。
start_web_on() {
  WEB_ADDR=$1
  BASE="$SCHEME://$WEB_ADDR:$PORT"
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

# must_start_web_on <地址>：起不来就**中止**，不留一条 fail 继续跑。
#
# 上一轮（10-26 第二轮）这里只记了一条 fail 就往下走，于是后面十几条断言全是同一条
# "Web 不在" 的连锁反应：预检 allowed=false、没有 job_id、记录仍 pending、timer 仍
# 武装、第二次确认的理由是 CONFIRM_WRONG_ADDRESS 而不是 NO_PENDING_ROLLBACK……
# 那一段的最后一条结论（journal 里没有回滚单元的输出）甚至是在真的回滚发生**之前**
# 取的。连锁失败不是证据，是噪音——所以这种情况必须停下，把现场留下来。
must_start_web_on() {
  if start_web_on "$1"; then
    pass "Web 绑在 $1:$PORT"
    return 0
  fi
  fail "Web 绑在 $1:$PORT" "$(tail -5 "$WEB_LOG" 2>/dev/null)"
  {
    printf '== 监听端口的进程\n'; ss -ltnp 2>/dev/null | grep -F ":$PORT"
    printf '== web.log 尾部\n'; tail -40 "$WEB_LOG" 2>/dev/null
    printf '== 面板配置里的 tls\n'; grep -n 'tls\|allowed_origins' "$SYS_CFG" 2>/dev/null
    printf '== 证书\n'; ls -l "$TLS_CERT" "$TLS_KEY" 2>/dev/null
  } >"$FACTS/web-bind-failed-$1.txt" 2>&1
  write_result
  exit 1
}

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
  # runuser 是壳：它一般会把 TERM 转给子进程，但"一般"不是断言能接受的理由。
  # 真正的 Web 进程还活着就直接收掉——否则后面"Web 已停"那条会在端口上撞见它。
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

# web_child_pid：runuser 与真正的 Web 进程不是同一个号（同 p14-1）。
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

# write_result：把结论写进 RESULT（宿主轮一遍就能知道结果）。
write_result() {
  {
    printf 'PASS=%s\nFAIL=%s\nNOTRUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
    printf 'FAILED_NAMES=%s\n' "$FAILED_NAMES"
    printf 'NOTRUN_NAMES=%s\n' "$NOTRUN_NAMES"
    printf 'FINISHED=%s\n' "$(date -u +%FT%TZ)"
  } >"$RESULT"
}

cleanup() {
  # 抢救措施先撤：它们的到期时间还没到就撤掉，免得清理阶段自己被它们动一下。
  systemctl stop p142-watchdog.timer >/dev/null 2>&1 || true
  rm -f "$UNIT_DIR/p142-watchdog.timer" "$UNIT_DIR/p142-watchdog.service"
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

# api <jar> <csrf> <方法> <路径> [body] [留档名]
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
jbd_job() { jbd "$WORK/job.json" "$1"; }

# ---- 现场事实 ----

ipv4_of() { ip -4 -o addr show dev "$1" 2>/dev/null | awk '{print $4}' | head -n1 | cut -d/ -f1; }
prefix_of() { ip -4 -o addr show dev "$1" 2>/dev/null | awk '{print $4}' | head -n1 | cut -d/ -f2; }
gw_of() { ip -4 route show default 2>/dev/null | awk '{print $3}' | head -n1; }
panel_file() { printf '%s/%s%s.yaml' "$NETPLAN_DIR" "$PANEL_PREFIX" "$1"; }
# 记录文件按 token 取名（`<token>.json`），确认/回滚之后**不会删除**，只是状态变了。
# 所以取记录必须按 token 取：`ls *.json | head -1` 会在第二轮拿到上一轮那条 confirmed 记录，
# 于是"记录变成 rolled_back"这种断言会对着一个早就结束的文件做，永远看不出真相。
record_file() { printf '%s/%s.json' "$STATE_DIR" "$1"; }
record_field() {
  if [ ! -f "$1" ]; then printf 'MISSING'; return 0; fi
  jget "$1" "d.get(\"$2\")"
}
timer_unit() { printf '%s/upanel-netrollback@%s.timer' "$UNIT_DIR" "$1"; }
timer_active() { systemctl is-active --quiet "upanel-netrollback@$1.timer" 2>/dev/null && echo yes || echo no; }
# timer_next：下一次触发的单调时钟时刻。
#
# 只看 `is-active` 是不够的——timer 被触发过以后仍然是 `active`，而它的
# `NextElapseUSecMonotonic` 会变成 `infinity`。上一轮（10-26 第二轮）就是因为只看
# active、又在一个错误的时间点取 journal，把"timer 还活着"当成了"timer 会按时开火"。
timer_next() {
  systemctl show "upanel-netrollback@$1.timer" -p NextElapseUSecMonotonic 2>/dev/null |
    sed -n 's/^NextElapseUSecMonotonic=//p'
}
port_open() { (curl -s -o /dev/null -m 1 "$BASE/api/v1/health/live" && echo yes) || echo no; }

# ---- 看门狗 ----

write_watchdog() {
  cat >"$WORK/watchdog.sh" <<WATCH
#!/bin/sh
# 不是产品的一部分：VM 的救命绳。只在 ens4 上已经没有 slirp 网段地址时动手，
# 动手就留标记——验收会因为那个标记判 FAIL。
who=\${1:-unknown}
mkdir -p $WORK/logs
if ip -4 -o addr show dev $IFACE 2>/dev/null | grep -q ' 10\.0\.2\.'; then
  printf '%s %s not-needed: %s 上是 %s\n' "\$(date -u +%FT%TZ)" "\$who" $IFACE \
    "\$(ip -4 -o addr show dev $IFACE | awk '{print \$4}')" >>$WORK/logs/watchdog-\$who.log
  exit 0
fi
printf '%s %s FIRED: %s 上没有 slirp 网段地址，正在退回 DHCP\n' "\$(date -u +%FT%TZ)" "\$who" $IFACE \
  >>$WORK/logs/watchdog-\$who.log
: >$WORK/watchdog-fired-\$who
rm -f $NETPLAN_DIR/${PANEL_PREFIX}$IFACE.yaml
netplan generate >>$WORK/logs/watchdog-\$who.log 2>&1
netplan apply >>$WORK/logs/watchdog-\$who.log 2>&1
WATCH
  chmod 0700 "$WORK/watchdog.sh"
}

arm_watchdogs() {
  # 触发时刻用**绝对**墙上时间（OnCalendar），不用 OnActiveSec。
  #
  # 10-26 实测：产品的 `disarmRollback` 会 `systemctl daemon-reload`，而 daemon-reload
  # 会把 OnActiveSec 这种**相对**计时器按 reload 那一刻重新起算。第三轮就撞上了——
  # timer 14:34:57 武装（本该 14:38:17 开火），产品 14:37:25 回滚时 reload，下一次触发
  # 被推后 200 秒，到脚本收尾（14:39:06）它一次都没响，而"看门狗没留痕"被记成 FAIL。
  # 这个方向对验收是要命的：万一产品真的没把网络带回来，正需要这条绳子的时候它不响。
  # 绝对时刻不怕这个：reload 之后如果目标时刻已经过去，systemd 会立刻补一次。
  WD_FIRE=$(date -d "@$(( $(date +%s) + 200 ))" '+%Y-%m-%d %H:%M:%S')
  cat >"$UNIT_DIR/p142-watchdog.service" <<UNIT
[Unit]
Description=P14.2 acceptance fail-safe (not part of upanel)

[Service]
Type=oneshot
ExecStart=/bin/sh $WORK/watchdog.sh systemd-timer
UNIT
  cat >"$UNIT_DIR/p142-watchdog.timer" <<UNIT
[Unit]
Description=P14.2 acceptance fail-safe timer (not part of upanel)

[Timer]
OnCalendar=$WD_FIRE
AccuracySec=1s

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload >/dev/null 2>&1
  if ! systemctl start p142-watchdog.timer >/dev/null 2>&1; then
    fail "看门狗 timer 武装" "$(systemctl status p142-watchdog.timer --no-pager -l 2>&1 | tail -6)"
  elif [ "$(systemctl is-active p142-watchdog.timer 2>/dev/null)" != "active" ]; then
    fail "看门狗 timer 处于 active" "$(systemctl status p142-watchdog.timer --no-pager -l 2>&1 | tail -6)"
  else
    note "看门狗 systemd timer 定在 $WD_FIRE（绝对时刻），下一次触发：$(systemctl show p142-watchdog.timer -p NextElapseUSecRealtime 2>/dev/null | sed -n 's/^NextElapseUSecRealtime=//p')"
  fi
  # 第二条：一个 sleep 子壳，跟 systemd 无关，独立到连 systemd 都挂了也能跑。
  # 215 秒：比 systemd 那条晚一点，好让两条的日志分别落盘、各自可查；两条都远在
  # 120 秒的窗口之后，只可能在产品没把网络带回来时才动手。
  setsid nohup /bin/sh -c "sleep 215; /bin/sh $WORK/watchdog.sh sleep-shell" \
    >"$WORK/logs/watchdog-shell.out" 2>&1 &
  note "看门狗已武装：systemd timer 定在 $WD_FIRE，sleep 子壳 +215s"
}

watchdog_fired() { ls "$WORK"/watchdog-fired-* 2>/dev/null | head -n1; }

# begin_write <动作> <参数> <留档名> <短语>：再认证 → 预检 → 提交，结果放全局变量。
begin_write() {
  W_REAUTH=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
  W_PRE_CODE=$(preflight "$1" "$2" "preflight-$3")
  W_ALLOWED=$(pget 'd["data"]["allowed"]')
  W_BLOCKERS=$(pblockers)
  W_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
  _pref=$(pget 'd["data"]["preflight_id"]')
  # 短语默认用**预检给的那一条**：要求短语的动作（network.apply）必须原样回传，
  # 传空串会被判 PHRASE_MISMATCH。第 4 个参数只在故意要传错短语时用。
  _phrase=${4:-}
  if [ -z "$_phrase" ]; then _phrase=$W_PHRASE; fi
  W_SUBMIT_CODE=$(submit_job "$_pref" "p142-$3-$(date +%s%N)" "$_phrase" "$3")
  W_JOB=$(job_id_of "$WORK/submit.json")
  W_STATUS=$(wait_job "$W_JOB")
}

# why_not_verified <留档名>：任务没到 verified 时把 observed 打出来并留档。
#
# 没有这一行，失败现场只剩一个"rolled_back"，而真正要修的是**原因**（地址不对？
# 网关不对？网卡上有两个地址？）。10-26 那一轮就为了这一句话在旧日志里绕了一大圈：
# web.log 只记接口与状态码，job 结果体早被下一次 begin_write 覆盖掉了。
why_not_verified() {
  [ "$(jget_job 'd["data"]["result"]["outcome"]')" = "verified" ] && return 0
  cp "$WORK/job.json" "$FACTS/job-$1.json" 2>/dev/null
  note "$1 没到 verified：observed=$(jget_job 'json.dumps(d["data"]["result"].get("observed"), ensure_ascii=True, sort_keys=True)') hint=$(jget_job 'd["data"]["result"].get("recovery_hint") or ""')"
}

# ---------------------------------------------------------------- 起跑

install -d -m 0700 "$WORK" "$WORK/logs" "$TMPD" "$FACTS"
chmod 0711 "$WORK"
rm -f "$WORK"/watchdog-fired-* 2>/dev/null
rm -rf "$WEB_DB_DIR"
install -d -m 0750 "$WEB_DB_DIR"
chown "$WEB_USER" "$WEB_DB_DIR" 2>/dev/null || true
: >"$AGENT_LOG"
: >"$WEB_LOG"
mkdir -p "$WORK/payloads"

mkfifo "$JSON_IN" "$JSON_OUT"
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
# JSON 助手必须和本脚本一起上传（同一目录）：它晚了会让每一次 jget 卡在 fifo 上，
# 那不是失败，是永远不结束。
if [ -f "$SCRIPT_DIR/json_helper.py" ]; then pass "JSON 助手在同目录"; else
  fail "JSON 助手在同目录" "缺 $SCRIPT_DIR/json_helper.py，脚本无法继续"; write_result; exit 2
fi
if command -v runuser >/dev/null 2>&1; then pass "runuser 可用"; else fail "runuser 可用" "缺少 runuser"; fi
if command -v netplan >/dev/null 2>&1; then pass "netplan 可用"; else fail "netplan 可用" "缺少 netplan"; fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "服务账号 $WEB_USER 存在"; else
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" && pass "服务账号 $WEB_USER 已创建"
fi
# 私钥要给 Web 账号读：属主 root、属组 $WEB_USER、0640。组不存在就没有这条路。
if getent group "$WEB_USER" >/dev/null 2>&1; then pass "服务组 $WEB_USER 存在"; else
  fail "服务组 $WEB_USER 存在" "没有同名组，面板私钥无法按 root:$WEB_USER 0640 交给 Web"; write_result; exit 2
fi
if command -v openssl >/dev/null 2>&1; then pass "openssl 可用（自签面板证书）"; else
  fail "openssl 可用（自签面板证书）" "缺少 openssl：非回环监听必须有 TLS"; write_result; exit 2
fi

# 单物理网卡且可改：这是本阶段的前提，不满足就没法验（而不是"验失败"）。
IFACE=$(for d in /sys/class/net/*; do
  n=${d##*/}; [ "$n" = lo ] && continue
  [ -d "$d/device" ] || continue
  [ "$(cat "$d/type" 2>/dev/null)" = 1 ] || continue
  [ -d "$d/bridge" ] && continue
  [ -d "$d/bonding" ] && continue
  [ -d "$d/wireless" ] && continue
  [ -e "/proc/net/vlan/$n" ] && continue
  printf '%s\n' "$n"
done | head -n1)
if [ -n "$IFACE" ]; then pass "找到一张物理以太网卡（$IFACE）"; else
  fail "找到一张物理以太网卡" "没有可改的网卡"; write_result; exit 2
fi
CUR_ADDR=$(ipv4_of "$IFACE")
CUR_PREFIX=$(prefix_of "$IFACE")
CUR_GW=$(gw_of)
if [ -n "$CUR_ADDR" ]; then pass "当前地址 $CUR_ADDR/$CUR_PREFIX"; else
  fail "当前地址" "读不到 $IFACE 的 IPv4"; write_result; exit 2
fi
if [ -n "$CUR_GW" ]; then pass "当前默认网关 $CUR_GW"; else
  note "没有默认网关（DHCP 还没拿到？）：后面所有带网关的用例改用不带网关的写法"
fi
if [ "$CUR_ADDR" = "$CANDIDATE" ]; then
  fail "候选地址与现地址不同" "现地址已经是 $CANDIDATE"; write_result; exit 2
fi
# 起点必须是干净的：上一轮如果没跑完，面板自己那份 netplan 文件会留下来，而阶段 2 要
# 断言"写开关关闭时一份面板文件都没有"——那份残留会让这条断言红得说不清原因。
# 这里判环境不满足并把清理命令打出来，**不替操作者删**：那是上一轮的现场，删不删由人定。
STALE_PANEL=$(panel_file "$IFACE")
if [ -f "$STALE_PANEL" ]; then
  fail "起点没有上一轮留下的面板文件" \
    "确认可以清掉后执行：rm -f $STALE_PANEL && netplan generate && netplan apply"
  write_result; exit 2
fi

{
  printf '== identity\n'; uname -a; lsb_release -ds 2>/dev/null; systemd --version | head -n1
  printf '== interface\n'; ip -4 -o addr show; ip -4 route show
  printf '== netplan files\n'; ls -l "$NETPLAN_DIR"
  printf '== rendered\n'; ls /run/systemd/network/ 2>/dev/null
  printf '== binary sha256\n'; sha256sum "$AGENT" "$WEB" "$CTL" 2>/dev/null
  printf '== script sha256\n'; sha256sum "$0"
  printf '== kernel cmdline\n'; cat /proc/cmdline
} >"$FACTS/env.txt" 2>&1

# ---------------------------------------------------------------- 装配

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
    "listen_address": "127.0.0.1:$PORT",
    "shutdown_timeout_seconds": 10,
    "tls_cert_path": "$TLS_CERT",
    "tls_key_path": "$TLS_KEY",
    "allowed_origins": ["https://127.0.0.1:$PORT", "https://localhost:$PORT", "https://$CUR_ADDR:$PORT"],
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

# 自签面板证书：这一轮面板要绑到非回环地址上（阶段 6.2 验“从新地址确认”），
# 而 `config.CheckTransport` 不允许非回环监听走明文。私钥按文档里的规矩给
# root:$WEB_USER 0640——Web 要读得到，别的用户读不到。
install -d -m 0755 "$TLS_DIR"
rm -f "$TLS_KEY.new" "$TLS_CERT"
if openssl req -x509 -newkey rsa:2048 -nodes -days 3 -sha256 \
  -keyout "$TLS_KEY.new" -out "$TLS_CERT" \
  -subj "/CN=upanel-p142-test" \
  -addext "subjectAltName=IP:127.0.0.1,IP:$CUR_ADDR,IP:$CANDIDATE" \
  >"$WORK/openssl.log" 2>&1; then
  mv "$TLS_KEY.new" "$TLS_KEY"
  chown root:"$WEB_USER" "$TLS_KEY" && chmod 0640 "$TLS_KEY"
  chmod 0644 "$TLS_CERT"
  pass "生成自签面板证书（SAN 含 127.0.0.1、$CUR_ADDR、$CANDIDATE）"
  openssl x509 -in "$TLS_CERT" -noout -fingerprint -sha256 -subject -dates >>"$FACTS/env.txt" 2>&1
  ls -l "$TLS_CERT" "$TLS_KEY" >>"$FACTS/env.txt" 2>&1
else
  fail "生成自签面板证书" "$(tail -3 "$WORK/openssl.log" 2>/dev/null)"; write_result; exit 1
fi

if [ -f "$KEY_FILE" ]; then
  pass "代理密钥文件已存在（沿用上一轮）"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
  fi
fi

if start_agent; then pass "代理启动（无 -network-writes）"; else
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
echo "== 2. 写路径默认关闭（Q452 的真机证明）"
must_start_web_on 127.0.0.1

CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$CODE"
if [ "$CODE" != "200" ]; then
  fail "登录失败，后续无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"; write_result; exit 1
fi
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin.json")
if [ -n "$CSRF" ]; then pass "拿到 CSRF 令牌"; else fail "拿到 CSRF 令牌" "空"; write_result; exit 1; fi

# viewer
CODE=$(api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"initial_password\":\"$VIEWER_PASSWORD\",\"role\":\"viewer\"}" \
  user-viewer)
in_set "建 viewer 201/200" "$CODE" "200 201"
CODE=$(login viewer "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$CODE"

# 现在代理没有写动作：config 与三个动作都必须不存在。
CODE=$(api "$JAR" "$CSRF" GET /api/v1/network/config "" keep-config-off)
expect_eq "写开关关闭时 GET /network/config 是 400" "400" "$CODE"
expect_contains "关闭时的错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(cat "$WORK/resp.json" 2>/dev/null)"
expect_contains "关闭时代理说的是「未登记的查询」" "未登记的查询" "$(cat "$WORK/resp.json" 2>/dev/null)"
cp "$WORK/resp.json" "$FACTS/config-writes-off.json"

CODE=$(preflight network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"\",\"definition\":{\"mode\":\"dhcp\",\"ipv4\":null}}" \
  preflight-apply-writes-off)
expect_eq "写开关关闭时 network.apply 预检 400" "400" "$CODE"
expect_contains "关闭时命中的是 network.apply 这一条" "network.apply" "$(cat "$WORK/preflight.json" 2>/dev/null)"
# 形状合法的假回滚标识：`nrb_` + 32 位十六进制。用形状不合法的（比如 `nrb_x`）会让 Web 的
# 服务端展开先拒掉，于是"代理没登记这个动作"这条断言就变成了一句空话——400 来了，但原因不是它。
FAKE_TOKEN=nrb_00000000000000000000000000000000
CODE=$(preflight network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$FAKE_TOKEN\"}" preflight-confirm-writes-off)
expect_eq "写开关关闭时 network.confirm 预检 400" "400" "$CODE"
expect_contains "关闭时命中的是 network.confirm 这一条" "network.confirm" "$(cat "$WORK/preflight.json" 2>/dev/null)"
CODE=$(preflight network.rollback "{\"interface_id\":\"$IFACE\",\"token\":\"$FAKE_TOKEN\"}" preflight-rollback-writes-off)
expect_eq "写开关关闭时 network.rollback 预检 400" "400" "$CODE"
expect_contains "关闭时命中的是 network.rollback 这一条" "network.rollback" "$(cat "$WORK/preflight.json" 2>/dev/null)"
expect_contains "回滚这一条也报了「未登记的动作」" "未登记的动作" "$(cat "$WORK/preflight.json" 2>/dev/null)"
OFF_FILES=$(ls "$NETPLAN_DIR"/${PANEL_PREFIX}*.yaml 2>/dev/null | wc -l | tr -d ' ')
expect_eq "关闭期间没有写出任何面板文件" "0" "$OFF_FILES"
if [ -d "$STATE_DIR" ]; then fail "关闭期间不该有回滚记录目录" "$STATE_DIR"; else pass "关闭期间没有回滚记录目录"; fi

# ---------------------------------------------------------------- 3. 打开写路径

echo
echo "== 3. 打开写路径（-network-writes）"
stop_web
stop_agent
if start_agent -network-writes; then pass "代理以 -network-writes 启动"; else
  fail "代理以 -network-writes 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; write_result; exit 1
fi
must_start_web_on 127.0.0.1
CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin2)
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin2.json")

CODE=$(api "$JAR" "$CSRF" GET /api/v1/network/config "" config-on)
expect_eq "GET /network/config 200" "200" "$CODE"
cp "$WORK/resp.json" "$FACTS/config-on.json"
CFG_IFACE=$(rget 'd["data"].get("interface")')
CFG_EDITABLE=$(rget 'd["data"]["capability"]["editable"]')
CFG_BACKEND=$(rget 'd["data"]["capability"]["backend"]')
CFG_HASH=$(rget 'd["data"].get("config_hash")')
CFG_MODE=$(rget 'd["data"]["mode"]')
expect_eq "入口状态指向这张网卡" "$IFACE" "$CFG_IFACE"
expect_eq "后端是 netplan-networkd" "netplan-networkd" "$CFG_BACKEND"
expect_true "这台机器可改" "$CFG_EDITABLE"
if [ "$CFG_EDITABLE" != "True" ]; then
  # 前提不成立就不往下跑：后面两百条断言会一起变红，而真正的结论只有一句
  # "这台机器不在面板可改范围内"。这是"环境不满足"，不是"功能验失败"。
  printf '不可改原因：%s\n' "$(rget 'json.dumps(d["data"]["capability"]["reasons"], ensure_ascii=False)')" \
    >>"$FACTS/env.txt"
  note "这台机器不可改：$(rget 'json.dumps(d["data"]["capability"]["reasons"], ensure_ascii=False)')"
  fail "验收前提：这张机器必须可改" "capability.editable=false"
  write_result
  exit 2
fi
expect_ne "给了 config_hash" "" "$CFG_HASH"
expect_ne "读到了当前模式" "" "$CFG_MODE"
expect_eq "没有待确认记录" "None" "$(rget 'd["data"]["pending"]')"
# `unsupported` 可能是 null（没有判定层给的那一份）也可能是 []：两种都表示"没有不支持项"。
in_set "没有不支持项" "$(rget 'json.dumps(d["data"]["unsupported"])')" "null []"

# 权限：未登录 401 / viewer 403
_anon=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/network/config" -H "Origin: $BASE")
expect_eq "未登录 GET /network/config 是 401" "401" "$_anon"
VJAR=$WORK/viewer.jar
VCSRF=$(csrf_of "$WORK/login-viewer.json")
CODE=$(api "$VJAR" "$VCSRF" GET /api/v1/network/config "" config-viewer)
expect_eq "viewer GET /network/config 是 403" "403" "$CODE"
# viewer 连预检入口都进不去（admin-only 路由），这条同时说明"403 不是因为我们没带 CSRF"。
VCODE=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/operations/preflight" \
  -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $VCSRF" \
  -b "$VJAR" -d "{\"action\":\"network.apply\",\"params\":{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"$CFG_HASH\",\"definition\":{\"mode\":\"dhcp\",\"ipv4\":null}}}")
cp "$WORK/resp.json" "$FACTS/preflight-viewer.json"
in_set "viewer 预检 network.apply 是 403" "$VCODE" "403"

# ---------------------------------------------------------------- 4. 预检判决

echo
echo "== 4. 预检的判决（坏指纹 / 禁用地址段 / 没有待确认记录）"

CODE=$(preflight network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"sha256:0000000000000000\",\"definition\":{\"mode\":\"dhcp\",\"ipv4\":null}}" \
  preflight-badhash)
expect_eq "坏指纹预检 200" "200" "$CODE"
expect_false "坏指纹 allowed=false" "$(pget 'd["data"]["allowed"]')"
expect_contains "坏指纹的阻塞项是 CONFIG_CHANGED" "NETWORK_CONFIG_CHANGED" "$(pblockers)"

# 禁用地址段在**解码那一刻**就被拒（不是等到判决层）：定义不合法的参数根本进不了登记表，
# 所以这里是 400 而不是 200+blockers。理由是 `127.0.0.0/8` 这个名字，不是一句"参数错误"。
CODE=$(preflight network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"$CFG_HASH\",\"definition\":{\"mode\":\"static\",\"ipv4\":{\"address\":\"127.0.0.1\",\"prefix_length\":24,\"gateway\":\"\",\"dns\":null}}}" \
  preflight-loopback)
expect_eq "回环地址预检 400" "400" "$CODE"
expect_contains "理由点名 127.0.0.0/8" "127.0.0.0/8" "$(cat "$WORK/preflight.json" 2>/dev/null)"
# 同一段"禁令"的另一条：模式与地址对不上（dhcp 却给了 ipv4 对象）同样当场被拒。
CODE=$(preflight network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"$CFG_HASH\",\"definition\":{\"mode\":\"dhcp\",\"ipv4\":{\"address\":\"10.0.2.15\",\"prefix_length\":24,\"gateway\":\"\",\"dns\":null}}}" \
  preflight-dhcp-with-ipv4)
expect_eq "dhcp 却带 ipv4 对象被拒（400）" "400" "$CODE"
expect_contains "理由说清 ipv4 必须是 null" "null" "$(cat "$WORK/preflight.json" 2>/dev/null)"

CODE=$(preflight network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$FAKE_TOKEN\"}" \
  preflight-confirm-nopending)
expect_eq "没有待确认记录时 confirm 预检 200" "200" "$CODE"
expect_false "没有待确认记录时 allowed=false" "$(pget 'd["data"]["allowed"]')"
expect_contains "阻塞项是 NO_PENDING_ROLLBACK" "NETWORK_NO_PENDING_ROLLBACK" "$(pblockers)"

# ---------------------------------------------------------------- 5. 复杂 netplan

echo
echo "== 5. 复杂 netplan → unsupported（不 apply，只写文件让产品读）"
BR_FILE=$NETPLAN_DIR/99-p142-bridge.yaml
cat >"$BR_FILE" <<YAML
network:
  version: 2
  bridges:
    br-p142:
      interfaces: [$IFACE]
YAML
chmod 0600 "$BR_FILE"
sleep 0.3
CODE=$(api "$JAR" "$CSRF" GET /api/v1/network/config "" config-complex)
expect_eq "复杂 netplan 时 GET /network/config 仍 200" "200" "$CODE"
CO_UNSUP=$(rget 'json.dumps(d["data"]["unsupported"])')
expect_not_contains "unsupported 不是 null" "null" "$CO_UNSUP"
expect_contains "unsupported 点名了那个文件" "99-p142-bridge.yaml" "$CO_UNSUP"
CODE=$(preflight network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"\",\"definition\":{\"mode\":\"dhcp\",\"ipv4\":null}}" \
  preflight-complex)
expect_eq "复杂 netplan 预检 200" "200" "$CODE"
expect_false "复杂 netplan allowed=false" "$(pget 'd["data"]["allowed"]')"
expect_contains "复杂 netplan 的阻塞项是 UNSUPPORTED" "NETWORK_UNSUPPORTED" "$(pblockers)"
expect_contains "阻塞项点名桥接文件" "99-p142-bridge.yaml" "$(pblocktext)"
# 顺便证明"预检过不了就不许提交"：拿这个 allowed=false 的预检去提交必须被拒。
_pref=$(pget 'd["data"]["preflight_id"]')
CODE=$(submit_job "$_pref" "p142-blocked-$(date +%s%N)" "" blocked-submit)
expect_eq "allowed=false 的预检不许提交（409）" "409" "$CODE"
expect_contains "被拒的提交是 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" "$(cat "$WORK/submit.json" 2>/dev/null)"
expect_eq "被拒的提交没有落下任务" "" "$(job_id_of "$WORK/submit.json")"
rm -f "$BR_FILE"
sleep 0.3
CODE=$(api "$JAR" "$CSRF" GET /api/v1/network/config "" config-after-complex)
in_set "删掉桥接配置后 unsupported 清空" "$(rget 'json.dumps(d["data"]["unsupported"])')" "null []"
CFG_HASH=$(rget 'd["data"].get("config_hash")')

# ---------------------------------------------------------------- 6. 同址静态改写

echo
echo "== 6. 同址静态改写：确认语义（连通性活着）"
GW_PART=""
if [ -n "$CUR_GW" ]; then GW_PART=",\"gateway\":\"$CUR_GW\""; fi
DEF_SAME="{\"mode\":\"static\",\"ipv4\":{\"address\":\"$CUR_ADDR\",\"prefix_length\":${CUR_PREFIX:-24}$GW_PART,\"dns\":null}}"

begin_write network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"$CFG_HASH\",\"definition\":$DEF_SAME}" \
  apply-same ""
expect_eq "同址改写：再认证 200" "200" "$W_REAUTH"
expect_eq "同址改写：预检 200" "200" "$W_PRE_CODE"
expect_true "同址改写：allowed=true" "$W_ALLOWED"
expect_contains "同址改写：给了 RECONFIGURE 短语" "RECONFIGURE " "$W_PHRASE"
expect_ne "同址改写：拿到了 job_id" "" "$W_JOB"
expect_eq "同址改写：任务 succeeded" "succeeded" "$W_STATUS"
cp "$WORK/job.json" "$FACTS/job-apply-same.json"
expect_eq "同址改写：outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
why_not_verified apply-same-address
TOKEN=$(jget_job 'd["data"]["result"]["data"]["rollback_token"]')
expect_ne "同址改写：给了回滚标识" "" "$TOKEN"
expect_contains "回滚标识是 nrb_ 开头" "nrb_" "$TOKEN"
DEADLINE=$(jget_job 'd["data"]["result"]["data"]["deadline"]')
expect_ne "同址改写：给了到期时刻" "" "$DEADLINE"
expect_eq "窗口是 120 秒" "120" "$(jget_job 'd["data"]["result"]["data"]["window_seconds"]')"
OFFERED=$(jget_job 'd["data"]["result"]["observed_state"]["address"]')
expect_eq "现场读回的地址就是候选地址" "$CUR_ADDR" "$OFFERED"

REC=$(record_file "$TOKEN")
if [ -f "$REC" ]; then pass "回滚记录已落盘（$TOKEN.json）"; else fail "回滚记录已落盘" "$REC 不存在"; fi
if [ -f "$REC" ]; then
  expect_eq "记录的 state 是 pending" "pending" "$(record_field "$REC" state)"
  expect_eq "记录的候选地址" "$CUR_ADDR" "$(record_field "$REC" candidate_address)"
  expect_eq "记录的网卡" "$IFACE" "$(record_field "$REC" interface)"
  PERM=$(stat -c '%a %U' "$REC" 2>/dev/null)
  expect_eq "记录是 root 0600" "600 root" "$PERM"
  cp "$REC" "$FACTS/rollback-record-same.json"
fi
expect_eq "timer 已武装并处于 active" "yes" "$(timer_active "$TOKEN")"
if [ -f "$(timer_unit "$TOKEN")" ]; then pass "timer 单元文件已写出"; else
  fail "timer 单元文件已写出" "$(timer_unit "$TOKEN") 不存在"
fi
PF=$(panel_file "$IFACE")
if [ -f "$PF" ]; then pass "面板文件已写出（$PF）"; else fail "面板文件已写出" "$PF 不存在"; fi
if [ -f "$PF" ]; then
  expect_eq "面板文件是 0600" "600" "$(stat -c '%a' "$PF")"
  expect_contains "面板文件里是那个地址" "$CUR_ADDR" "$(cat "$PF")"
  cp "$PF" "$FACTS/panel-file-static.yaml"
fi

# 6.1a 调用方**不能自己说**它是从哪个地址来的：参数里出现 arrived_on 直接 400。
#
# 这是"旧地址不能绕过新地址验证"的第一道门：到达地址不是调用方交的证词，是服务端从
# socket 里读出来的事实。前端能改字段，改不了自己那条连接的本地地址。
CODE=$(preflight network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$TOKEN\",\"arrived_on\":\"$CUR_ADDR\"}" \
  preflight-confirm-selfclaim)
expect_eq "自称 arrived_on 的确认被拒（400）" "400" "$CODE"
expect_contains "拒绝理由点名只有两个字段" "两个字段" "$(cat "$WORK/preflight.json" 2>/dev/null)"
expect_eq "自称被拒之后 timer 还在" "yes" "$(timer_active "$TOKEN")"

# 6.1b 老老实实不带 arrived_on，但**从回环地址**发：服务端读到 127.0.0.1，与候选地址不符。
CODE=$(preflight network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$TOKEN\"}" preflight-confirm-oldaddr)
expect_eq "从旧地址确认：预检 200" "200" "$CODE"
expect_false "从旧地址确认被拒" "$(pget 'd["data"]["allowed"]')"
expect_contains "理由是 CONFIRM_WRONG_ADDRESS" "NETWORK_CONFIRM_WRONG_ADDRESS" "$(pblockers)"
expect_contains "理由里点名到达地址 127.0.0.1" "127.0.0.1" "$(pblocktext)"
expect_contains "理由里点名候选地址" "$CUR_ADDR" "$(pblocktext)"
expect_eq "被拒之后 timer 还在" "yes" "$(timer_active "$TOKEN")"
expect_eq "被拒之后记录仍是 pending" "pending" "$(record_field "$REC" state)"

# 6.2 换 Web 绑到候选地址，从新地址确认
stop_web
must_start_web_on "$CUR_ADDR"
CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin3)
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin3.json")
expect_eq "从新地址登录 200" "200" "$CODE"

begin_write network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$TOKEN\"}" confirm-newaddr ""
expect_eq "从新地址确认：再认证 200" "200" "$W_REAUTH"
expect_eq "从新地址确认：预检 200" "200" "$W_PRE_CODE"
expect_true "从新地址确认：allowed=true" "$W_ALLOWED"
expect_eq "从新地址确认：任务 succeeded" "succeeded" "$W_STATUS"
cp "$WORK/job.json" "$FACTS/job-confirm.json"
expect_eq "从新地址确认：outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
why_not_verified confirm-from-new-address
expect_eq "记录变 confirmed" "confirmed" "$(record_field "$REC" state)"
expect_eq "确认后 timer 已停" "no" "$(timer_active "$TOKEN")"
expect_eq "确认后 timer 单元文件已删" "no" "$([ -f "$(timer_unit "$TOKEN")" ] && echo yes || echo no)"
expect_eq "面板文件还在（确认=保留）" "yes" "$([ -f "$PF" ] && echo yes || echo no)"

# 6.3 再确认一次：没有待确认的改动
CODE=$(preflight network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$TOKEN\"}" preflight-confirm-twice)
expect_false "第二次确认 allowed=false" "$(pget 'd["data"]["allowed"]')"
expect_contains "第二次确认是 NO_PENDING_ROLLBACK" "NETWORK_NO_PENDING_ROLLBACK" "$(pblockers)"

# 6.4 改回 DHCP 并确认
CODE=$(api "$JAR" "$CSRF" GET /api/v1/network/config "" config-before-dhcp)
CFG_HASH=$(rget 'd["data"].get("config_hash")')
begin_write network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"$CFG_HASH\",\"definition\":{\"mode\":\"dhcp\",\"ipv4\":null}}" \
  apply-dhcp ""
expect_true "改回 DHCP：allowed=true" "$W_ALLOWED"
expect_eq "改回 DHCP：任务 succeeded" "succeeded" "$W_STATUS"
expect_eq "改回 DHCP：outcome=verified" "verified" "$(jget_job 'd["data"]["result"]["outcome"]')"
why_not_verified apply-dhcp
TOKEN2=$(jget_job 'd["data"]["result"]["data"]["rollback_token"]')
REC2=$(record_file "$TOKEN2")
if [ -f "$REC2" ]; then
  expect_eq "DHCP 记录里没有候选地址（DHCP 拿到的地址是随机的，不能做凭据）" "None" \
    "$(record_field "$REC2" candidate_address)"
  expect_eq "DHCP 记录的 mode 是 dhcp" "dhcp" "$(record_field "$REC2" mode)"
fi
sleep 3
begin_write network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$TOKEN2\"}" confirm-dhcp ""
expect_true "DHCP 确认：allowed=true（没有候选地址就不比地址）" "$W_ALLOWED"
expect_eq "DHCP 确认：任务 succeeded" "succeeded" "$W_STATUS"
expect_eq "DHCP 确认后记录 confirmed" "confirmed" "$(record_field "$REC2" state)"
expect_eq "DHCP 后拿到 DHCP 地址" "yes" "$(case $(ipv4_of "$IFACE") in 10.0.2.*) echo yes;; *) echo no;; esac)"
expect_eq "DHCP 后默认网关在" "yes" "$([ -n "$(gw_of)" ] && echo yes || echo no)"
cp "$(panel_file "$IFACE")" "$FACTS/panel-file-dhcp.yaml" 2>/dev/null

# ---------------------------------------------------------------- 7. 断连重连（核心）

echo
echo "== 7. 断连重连：不可达地址 → 停 Web 与代理 → 120 秒自动恢复"
stamp phase7-start

# Web 回到回环：这一段的 HTTP 调用都在同一台机器上，没必要绕过新地址。
stop_web
must_start_web_on 127.0.0.1
CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin4)
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin4.json")
expect_eq "阶段 7 登录 200" "200" "$CODE"

CODE=$(api "$JAR" "$CSRF" GET /api/v1/network/config "" config-phase7)
CFG_HASH=$(rget 'd["data"].get("config_hash")')
expect_eq "阶段 7 开始时没有待确认记录" "None" "$(rget 'd["data"]["pending"]')"

write_watchdog
arm_watchdogs

T0=$(date +%s)
DEF_FAR="{\"mode\":\"static\",\"ipv4\":{\"address\":\"$CANDIDATE\",\"prefix_length\":$CANDIDATE_PREFIX,\"dns\":null}}"
begin_write network.apply \
  "{\"interface_id\":\"$IFACE\",\"expected_config_hash\":\"$CFG_HASH\",\"definition\":$DEF_FAR}" \
  apply-unreachable ""

expect_true "不可达改写：预检 allowed=true" "$W_ALLOWED"
expect_contains "不可达改写：预检给了回滚影响说明" "rollback" \
  "$(pget 'json.dumps([i["kind"] for i in (d["data"].get("impacts") or [])])')"
expect_contains "不可达改写：影响说明里写了 120 秒与自动退回" "120" "$(pget 'json.dumps([i["description"] for i in (d["data"].get("impacts") or [])], ensure_ascii=True)')"
expect_eq "不可达改写：任务 succeeded" "succeeded" "$W_STATUS"
JOB_DONE=$(date +%s)
cp "$WORK/job.json" "$FACTS/job-apply-unreachable.json"
OUTCOME_FAR=$(jget_job 'd["data"]["result"]["outcome"]')
expect_eq "不可达改写：outcome=verified（内核里确实换成了那个地址）" "verified" "$OUTCOME_FAR"
TOKEN_FAR=$(jget_job 'd["data"]["result"]["data"]["rollback_token"]')
DEADLINE_FAR=$(jget_job 'd["data"]["result"]["data"]["deadline"]')

# 现场的地址现在必须已经是那个不可达地址。
NOW_ADDR=$(ipv4_of "$IFACE")
expect_eq "现场地址已换成不可达地址" "$CANDIDATE" "$NOW_ADDR"
expect_eq "原来的 DHCP 地址已经不在" "no" "$(case $(ip -4 -o addr show dev "$IFACE") in *" $CUR_ADDR/"*) echo yes;; *) echo no;; esac)"
sleep 2
expect_eq "默认路由跟着没了" "" "$(gw_of)"
expect_eq "timer 在跑" "yes" "$(timer_active "$TOKEN_FAR")"
NEXT_FAR=$(timer_next "$TOKEN_FAR")
printf 'NOTE  timer 的下一次触发：%s\n' "${NEXT_FAR:-<空>}"
expect_not_contains "timer 真的排上了下一次触发（不是 infinity）" "infinity" "${NEXT_FAR:-<空>}"
expect_ne "timer 的下一次触发不是空值" "" "${NEXT_FAR:-}"
REC_FAR=$(record_file "$TOKEN_FAR")
expect_eq "记录里是那个候选地址" "$CANDIDATE" "$(record_field "$REC_FAR" candidate_address)"
expect_eq "记录的 state 是 pending" "pending" "$(record_field "$REC_FAR" state)"
cp "$REC_FAR" "$FACTS/rollback-record-unreachable.json" 2>/dev/null
cp "$(panel_file "$IFACE")" "$FACTS/panel-file-unreachable.yaml" 2>/dev/null
DL_EPOCH=$(date -d "$DEADLINE_FAR" +%s 2>/dev/null || echo 0)
APPLIED_FAR=$(record_field "$REC_FAR" applied_at)
APPLIED_EPOCH=$(date -d "$APPLIED_FAR" +%s 2>/dev/null || echo 0)
if [ "$DL_EPOCH" -gt 0 ] && [ "$APPLIED_EPOCH" -gt 0 ]; then
  # 窗口要从**产品自己写的两个时刻**上量：`applied_at` 是它武装 timer 的那一刻，
  # `deadline` 是它答应退回来的那一刻。拿脚本里的 T0 去比是不对的——T0 在再认证
  # （bcrypt）、预检、提交、以及真正 apply 之前，那段耗时本来就不该计入窗口。
  # 10-26 第三轮这里红过一次：T0→applied_at 有 12 秒，而产品的窗口一秒没多。
  expect_eq "窗口是 120 秒（记录自己的 applied_at → deadline）" "120" \
    "$((DL_EPOCH - APPLIED_EPOCH))"
else
  fail "到期时刻与 applied_at 可解析" "deadline=$DEADLINE_FAR applied_at=$APPLIED_FAR"
fi
expect_eq "任务返回时窗口还开着（不然窗口在被用到之前就到期了）" "yes" \
  "$([ "$DL_EPOCH" -gt "$JOB_DONE" ] && echo yes || echo no)"

# 7.1 候选地址是那个不可达地址时，从这里（回环）确认必须被拒：到达地址由服务端从连接读。
CODE=$(preflight network.confirm "{\"interface_id\":\"$IFACE\",\"token\":\"$TOKEN_FAR\"}" \
  preflight-confirm-wrong-address)
expect_eq "不可达改动上从回环确认：预检 200" "200" "$CODE"
expect_false "不可达改动上从回环确认被拒" "$(pget 'd["data"]["allowed"]')"
expect_contains "理由仍是 CONFIRM_WRONG_ADDRESS" "NETWORK_CONFIRM_WRONG_ADDRESS" "$(pblockers)"
expect_contains "理由里点名的是那个不可达地址" "$CANDIDATE" "$(pblocktext)"
expect_eq "被拒之后 timer 仍在" "yes" "$(timer_active "$TOKEN_FAR")"

# 7.2 停 Web 与代理：剩下能救这台机器的只有 systemd timer。
stamp stop-services
stop_web
stop_agent
expect_eq "Web 进程已停（端口关闭）" "no" "$(port_open)"
expect_eq "代理 socket 已消失" "no" "$([ -S "$SOCK" ] && echo yes || echo no)"
expect_eq "代理进程已不在" "no" "$(pgrep -f "$AGENT --config $SYS_CFG" >/dev/null 2>&1 && echo yes || echo no)"
expect_eq "停掉之后 timer 仍在跑（这才是独立回滚）" "yes" "$(timer_active "$TOKEN_FAR")"
note "从这一刻起，除了 systemd timer 没有任何 upanel 进程在跑"

# 7.3 60 秒时仍然是不可达地址：窗口不许被提前咬掉。
WAIT=$((T0 + 60 - $(date +%s)))
[ "$WAIT" -gt 0 ] && sleep "$WAIT"
MID_ADDR=$(ipv4_of "$IFACE")
expect_eq "60 秒时仍停在不可达地址（窗口没被提前咬掉）" "$CANDIDATE" "$MID_ADDR"
expect_eq "60 秒时 timer 仍 active" "yes" "$(timer_active "$TOKEN_FAR")"
expect_eq "60 秒时记录仍是 pending" "pending" "$(record_field "$REC_FAR" state)"
stamp mid-window

# 7.4 到期之后：地址自己回来。
# 每 3 秒看一次，最多看到 DL+40：到期那一刻要跑 netplan generate + apply，这台 VM 是
# TCG 纯软件模拟，单次 apply 实测 8—11 秒，回滚路径上要跑两次。轮询而不是睡死，是为了
# 把"第几秒回来的"记进证据里——只看 DL+40 那一刻，没法区分"正好在窗口结束时回来"和
# "窗口被拉了 40 秒"。
RECOVER_AT=""
RECOVER_DEADLINE=$((DL_EPOCH + 45))
while :; do
  NOW=$(date +%s)
  if [ "$NOW" -lt "$DL_EPOCH" ]; then
    # 窗口还没到，睡觉等它；2 秒一跳，别在到期那一刻错过 3 秒采样点。
    sleep 2
    continue
  fi
  # 到期之后才看地址：`ipv4_of` 在 netplan apply 中途会短暂返回空串（地址被摘掉、
  # 新的还没上的那一瞬），空串也"不等于 CANDIDATE"，那是假信号，不算回来。
  ADDR_NOW=$(ipv4_of "$IFACE")
  if [ -n "$ADDR_NOW" ] && [ "$ADDR_NOW" != "$CANDIDATE" ]; then
    RECOVER_AT=$NOW
    break
  fi
  if [ "$NOW" -ge "$RECOVER_DEADLINE" ]; then break; fi
  sleep 2
done
if [ -n "$RECOVER_AT" ]; then
  note "地址在 deadline 之后 $((RECOVER_AT - DL_EPOCH))s 回来（T0+$((RECOVER_AT - T0))s）"
  expect_eq "回滚发生在 deadline 之后（没有提前咬掉窗口）" "yes" \
    "$([ "$RECOVER_AT" -ge "$DL_EPOCH" ] && echo yes || echo no)"
  expect_eq "回滚发生在 deadline+45s 之内（没有等到 120 秒窗口之外）" "yes" \
    "$([ "$RECOVER_AT" -le "$RECOVER_DEADLINE" ] && echo yes || echo no)"
else
  fail "到期后 45 秒内地址自己回来" \
    "仍停在 $(ipv4_of "$IFACE")（deadline 已过 $(( $(date +%s) - DL_EPOCH ))s）"
fi
BACK_ADDR=$(ipv4_of "$IFACE")
stamp after-deadline
expect_ne "到期后地址不再是那个不可达地址" "$CANDIDATE" "$BACK_ADDR"
expect_eq "到期后回到了 slirp 网段（DHCP 拿到了地址）" "yes" \
  "$(case $BACK_ADDR in 10.0.2.*) echo yes;; *) echo no;; esac)"
expect_eq "到期后默认网关回来了" "yes" "$([ -n "$(gw_of)" ] && echo yes || echo no)"
expect_eq "记录变成 rolled_back" "rolled_back" "$(record_field "$REC_FAR" state)"
expect_eq "timer 已经结束" "no" "$(timer_active "$TOKEN_FAR")"
PF_NOW=$(panel_file "$IFACE")
if [ -f "$PF_NOW" ]; then
  # 这一次的"改之前"是面板自己写的 DHCP 文件，所以退回之后它应该还是 DHCP。
  expect_contains "退回之后面板文件是 DHCP 版本" "dhcp4: true" "$(cat "$PF_NOW")"
  expect_not_contains "退回之后面板文件里没有那个不可达地址" "$CANDIDATE" "$(cat "$PF_NOW")"
  cp "$PF_NOW" "$FACTS/panel-file-after-rollback.yaml"
else
  fail "退回之后面板文件应被恢复" "$PF_NOW 不存在"
fi
journalctl -u "upanel-netrollback@$TOKEN_FAR.service" --no-pager >"$FACTS/journal-netrollback.txt" 2>&1
journalctl -u "upanel-netrollback@$TOKEN_FAR.timer" --no-pager >>"$FACTS/journal-netrollback.txt" 2>&1
systemctl status "upanel-netrollback@$TOKEN_FAR.service" --no-pager -l \
  >>"$FACTS/journal-netrollback.txt" 2>&1
JOURNAL=$(tr '\n' ' ' <"$FACTS/journal-netrollback.txt")
expect_contains "回滚单元是被 systemd 启动的（不是别的进程顺手做的）" \
  "Starting uPanel network rollback ($TOKEN_FAR)" "$JOURNAL"
expect_contains "journal 里有那个回滚单元自己的输出" "回滚 $TOKEN_FAR 已处理" "$JOURNAL"
# "这条单元跑的是代理自己的二进制、带 netrollback 子命令"这件事**不能**问 journal：
# systemd 249 只在 `status` 里给 `ExecStart=`，journal 里从头到尾没有那命令行（10-26 第四轮
# 就是在这里红的，红的是断言不是产品）。改问三个真能拿到的地方：
#   a) 单元里打日志的进程标识就是 upanel-agent（journal 已被 -u 过滤成这一条单元）；
#   b) 模板文件里写着 ExecStart="<代理二进制>" netrollback --token %i；
#   c) 进程退出是 Deactivated successfully / Finished（systemd 249 不写 status=0/SUCCESS）。
expect_contains "回滚进程就是代理二进制（journal 里的进程标识）" "upanel-agent[" "$JOURNAL"
expect_contains "模板里 ExecStart 跑的是代理二进制的 netrollback 子命令" \
  "netrollback --token %i" "$(cat "$UNIT_DIR/upanel-netrollback@.service" 2>/dev/null)"
expect_contains "模板里的二进制路径就是本次验收用的那一份" \
  "ExecStart=\"$AGENT\"" "$(cat "$UNIT_DIR/upanel-netrollback@.service" 2>/dev/null)"
expect_contains "回滚单元正常结束" "Finished uPanel network rollback ($TOKEN_FAR)" "$JOURNAL"
expect_contains "回滚单元没有失败" "Deactivated successfully" "$JOURNAL"
expect_not_contains "回滚单元没有报失败状态" "Failed with result" "$JOURNAL"

# 7.5 看门狗不许动手（要等到两条都到期之后：200 秒与 215 秒）。
WAIT=$((T0 + 232 - $(date +%s)))
[ "$WAIT" -gt 0 ] && sleep "$WAIT"
FIRED=$(watchdog_fired)
if [ -n "$FIRED" ]; then
  fail "看门狗没有动手" "动了：$FIRED（说明产品没把网络带回来）"
else
  pass "看门狗没有动手（产品自己回来的）"
fi
for who in sleep-shell systemd-timer; do
  if [ -f "$WORK/logs/watchdog-$who.log" ]; then
    cp "$WORK/logs/watchdog-$who.log" "$FACTS/" 2>/dev/null
    expect_contains "看门狗 $who 醒来时网络已经在（not-needed）" "not-needed" \
      "$(cat "$WORK/logs/watchdog-$who.log")"
  elif [ "$who" = sleep-shell ]; then
    # 这条是**权威**证据：一个跟 systemd 无关的 sleep 子壳，铁定在 +215 秒醒来。
    # 它连日志都没留下，就说明验收没跑全，不能拿"没有 fired 标记"当通过。
    fail "看门狗 $who 留下了记录" "没有日志：无法证明它跑过"
  else
    # systemd 那条用绝对时刻，正常一定会响；万一没响（比如被别的 reload 顶到收尾
    # 之后），只记一条 NOTE——"没有 fired 标记"这条硬证据已经由上一条覆盖了。
    note "看门狗 $who 没有留下日志（已过 $(( $(date +%s) - T0 ))s，收尾时它还没到点）"
  fi
done
cp /etc/netplan/*.yaml "$FACTS/" 2>/dev/null
stamp phase7-done

# ---------------------------------------------------------------- 8. 恢复之后

echo
echo "== 8. 恢复之后：重新起服务，现场干净"
if start_agent -network-writes; then pass "代理重新启动"; else
  fail "代理重新启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"
fi
must_start_web_on 127.0.0.1
CODE=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin5)
JAR=$WORK/admin.jar
CSRF=$(csrf_of "$WORK/login-admin5.json")
expect_eq "恢复后登录 200" "200" "$CODE"
CODE=$(api "$JAR" "$CSRF" GET /api/v1/network/config "" config-after-rollback)
expect_eq "恢复后 GET /network/config 200" "200" "$CODE"
expect_eq "恢复后没有待确认记录" "None" "$(rget 'd["data"]["pending"]')"
expect_eq "恢复后模式是 dhcp" "dhcp" "$(rget 'd["data"]["mode"]')"
expect_eq "恢复后读回的地址是本机地址" "$BACK_ADDR" "$(rget 'd["data"]["ipv4"]["address"]')"
LEFTOVER=$(ls "$UNIT_DIR"/upanel-netrollback@*.timer 2>/dev/null | wc -l | tr -d ' ')
expect_eq "没有残留的 timer 单元文件" "0" "$LEFTOVER"
LIVE_TIMERS=$(systemctl list-timers --all --no-pager 2>/dev/null | grep -c 'upanel-netrollback@' || true)
expect_eq "没有残留的 netrollback timer" "0" "$LIVE_TIMERS"
if grep -q 'panic' "$AGENT_LOG" "$WEB_LOG" 2>/dev/null; then
  fail "日志里没有 panic" "$(grep -h panic "$AGENT_LOG" "$WEB_LOG" | head -2)"
else
  pass "日志里没有 panic"
fi

# 审计：被拒的写尝试应留下 failure 记录，成功的提交应留下 success 记录。
#
# 审计的 action 列记的是**接口动作**（`job.submit`、`auth.reauth` 之类），不是被提交的
# 那个动作名；要看"这次拒绝是不是和网络有关"，只能落在 details 里的 http_path 上。
AUD=$(python3 - "$WEB_DB" <<'PY'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True, timeout=15)
    row = db.execute(
        "select"
        " (select count(*) from audit_events"
        "   where outcome='failure' and redacted_details like '%/api/v1/network%'),"
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
  pass "被拒的网络请求留下了 failure 审计（$AUD_DENIED 条）"
else
  fail "被拒的网络请求留下了 failure 审计" "原始输出：$AUD"
fi
if [ "${AUD_SUBMITTED:-0}" -ge 4 ] 2>/dev/null; then
  pass "成功提交的任务留下了 success 审计（$AUD_SUBMITTED 条）"
else
  fail "成功提交的任务留下了 success 审计" "原始输出：$AUD"
fi
cp /etc/netplan/*.yaml "$FACTS/" 2>/dev/null
systemctl list-timers --all --no-pager >"$FACTS/timers-after.txt" 2>&1
ip -4 -o addr show >"$FACTS/addr-after.txt" 2>&1

# 明确未验项
notrun "浏览器里的渲染与断线重连交互" "VM 内没有 Playwright（Q21），由 SSR 渲染测试与宿主 web-smoke 覆盖"
notrun "网卡被改名/拔掉（NETWORK_TARGET_CHANGED 的另一半）" "改网卡名要重启，会把 timer 语义一起冲掉"
notrun "回滚自己失败（failed 分支）" "真机上造它等于把机器弄坏到看门狗之外，风险不对等；单测覆盖判决形状"

echo
echo "== 结果"
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NTRUN"
[ -n "$FAILED_NAMES" ] && printf 'FAIL 明细:%s\n' "$FAILED_NAMES"
[ -n "$NOTRUN_NAMES" ] && printf 'NOT RUN 明细:%s\n' "$NOTRUN_NAMES"
stamp done
write_result
if [ "$FAILED" -ne 0 ]; then exit 1; fi
exit 0
