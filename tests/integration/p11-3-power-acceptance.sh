#!/bin/sh
# P11.3 Linux 验收脚本：重启 / 关机（system.power）在真实 Ubuntu 上的行为。
#
# ⚠️ 这个脚本会**真的重启**、**真的关机**这台机器。**只在一台一次性的验收虚拟机上跑**
#    （PLAN 第 14 节、docs/STAGES.md 的硬规定）。关机之后机器不会自己回来，需要宿主
#    重新上电——所以它分成三个阶段，由宿主（见 docs/evidence 里的记录）分三次调用：
#
#      sudo sh <本脚本> --phase pre     # 预检、拒收面、在跑任务拒绝、取消、下发重启
#      （等机器重启回来）
#      sudo sh <本脚本> --phase post    # 用 boot_id 确认重启、下发关机
#      （等机器断电，宿主重新上电）
#      sudo sh <本脚本> --phase final   # 关机后的核对，汇总
#
#    状态与载荷都落在 /var/tmp（重启不清），所以三个阶段共享同一份现场。
#
# 要证明的不是"接口返回 202"，而是下面这些**很容易被糊过去**的事实：
#
#   1) **受理 ≠ 发生**：任务结果是 accepted，不是 verified。没人看见那次重启，
#      代理在 `systemctl reboot` 返回之后就死了。界面能说的只有"请求已受理"。
#   2) **重启真的发生过**：唯一站得住的证据是 boot_id。脚本在阶段一记下重启前的
#      boot_id，阶段二从 /proc 重新读一遍，两个值必须不同；任务的
#      observed_state.boot_id_before 必须等于阶段一记下的那个，power_check 必须是
#      confirmed + changed=true。
#   3) **关键任务运行时拒绝**：先起一个 300 秒倒计时的电源任务（它自己就是一条正在
#      执行的写操作），此时对同一个动作做预检必须被 POWER_JOBS_RUNNING 挡住，而且
#      提交这份预检也必须被拒（409 DEPENDENCY_BLOCKED）——预检的结论是固定的。
#   4) **倒计时可以取消，而且取消是真的**：300 秒的倒计时里取消，任务必须变成
#      canceled、机器必须还在原来那次开机上（boot_id 不变）。这不是"发出去之后再喊
#      停"——命令压根没发出去。
#   5) **关机不伪称已验证**：下发关机之后代理就不在了，没有任何本地证据能证明电源被
#      切断。脚本检查三件事：任务结果是 accepted（不是 verified）；那一瞬间的
#      power_check 不能是 confirmed；机器重新上电之后 power_check 可以说"回来过"，
#      但措辞必须明确拒绝把这件事当成关机成功的证据。
#
# 用法（root，在验收虚拟机上）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p11-3-power-acceptance.sh --phase pre
#
# 环境变量：BIN_DIR（默认 /var/tmp/upanel-bin）、WORK（默认 /var/tmp/upanel-accept-p11-3）、
# PORT（默认 19506）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制缺失 / 不是 systemd 系统 /
# 读不到 boot_id）。
#
# 明确不覆盖（NOT RUN，原因写在这里，不假装通过）：
#   - **电源真的被切断**：面板与代理都只能看到"自己还在不在"。PLAN §16 要求的就是
#     不许把推断说成证据，所以这一条本来就不该由软件验收通过。脚本能证明的是"机器在
#     关机之后重新启动过"（boot_id 又变了一次）。
#   - **平台侧断电/带外管理**：不在 0.1 范围内。
#   - **浏览器渲染**：VM 上没有浏览器；界面措辞由 web/src 的 SSR 测试锁住，载荷原样
#     抓在 $WORK/payloads 里。
#   - **倒计时只剩几秒时取消**：取消是协作式的（代理在 5 秒一格里检查一次），
#     "最后一秒取消"的边界属于 internal/power 的单元测试，不在真机上抢那个窗口。
#   - **POWER_BOOT_ID_UNREADABLE / POWER_UNSUPPORTED 的真机复现**：要去改 /proc 的权限
#     或停掉 systemd，代价是把一台验收机弄坏且不可复原。这两条由
#     internal/power/exec_test.go 覆盖。

set -u

PHASE=pre
for arg in "$@"; do
  case $arg in
    --phase) ;;
    pre|post|final|report) PHASE=$arg ;;
    --phase=*) PHASE=${arg#*=} ;;
    *) printf '未知参数：%s\n' "$arg" >&2; exit 2 ;;
  esac
done

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
WORK=${WORK:-/var/tmp/upanel-accept-p11-3}
PORT=${PORT:-19506}
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
WEB_USER=upanel-web
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'
PAYLOADS=$WORK/payloads
STATE=$WORK/state
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
RESULTS=$WORK/logs/results.txt
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
JAR=$WORK/cookies-admin.txt
VJAR=$WORK/cookies-viewer.txt
BOOTID_FILE=/proc/sys/kernel/random/boot_id
POWER_ACTION=system.power

PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS_PATH=/api/v1/jobs
POWER_STATE_PATH=/api/v1/system/power-state

AGENT_PID=""
WEB_PID=""
PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""

mkdir -p "$WORK/logs" "$PAYLOADS" "$STATE" "$WEB_DB_DIR" 2>/dev/null

phase_pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; printf 'PASS\t%s\n' "$1" >>"$RESULTS"; }
phase_fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
  printf 'FAIL\t%s\t%s\n' "$1" "${2:-}" >>"$RESULTS"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; printf 'NOTRUN\t%s\t%s\n' "$1" "$2" >>"$RESULTS"; }
note() { printf '      %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then phase_pass "$1"; else phase_fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ne() { if [ "$2" != "$3" ]; then phase_pass "$1"; else phase_fail "$1" "不该等于 '$2'"; fi; }

# jget <文件> <python 表达式>：d 是解析后的 JSON。
jget() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
ns={"d": d, "json": json}
try:
    print(eval(sys.argv[2], ns))
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))' "$1" "$2" 2>/dev/null
}

pf() { jget "$PAYLOADS/preflight-$1.json" "$2"; }
jbf() { jget "$PAYLOADS/job-$1.json" "$2"; }
st() { jget "$STATE/$1.json" "$2"; }

# require_value <值> <说明>：阶段二/三靠阶段一留下的状态文件接续。读不到就当场退出，
# 而不是拿空串继续跑——空串会让"boot_id 变了没有"这类断言变成一场假的通过。
require_value() {
  case $1 in
    ""|ERR:*)
      printf '状态文件里读不到%s：%s（阶段一的现场没落盘？）\n' "$2" "$1" >&2
      exit 2 ;;
  esac
}

# dbq <数据库文件> <SQL>：只读查询，返回第一行第一列。
dbq() {
  python3 - "$1" "$2" <<'PY'
import sqlite3, sys
con = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
try:
    row = con.execute(sys.argv[2]).fetchone()
    print("" if row is None else ("" if row[0] is None else row[0]))
finally:
    con.close()
PY
}

boot_id() { tr -d ' \n' <"$BOOTID_FILE" 2>/dev/null; }
uptime_seconds() { cut -d' ' -f1 /proc/uptime | cut -d. -f1; }

cleanup() {
  # KEEP_SERVICES=1 时不动服务。阶段一收尾必须这样：那一刻有一个重启任务还在倒计时里，
  # 它要杀的正是这个代理进程所在的机器。第一次跑时没设这个开关，trap 在退出时把代理
  # 优雅停掉了，任务于是变成 interrupted，机器压根没重启——脚本踩掉了自己要验的东西。
  if [ "${KEEP_SERVICES:-0}" = 1 ]; then
    return 0
  fi
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

ensure_run_dir() {
  # 重启之后 /run 是空的（tmpfs）：代理要求 socket 目录启动前就存在、root 拥有、
  # 且 Web 账号能进去。正式安装由 systemd-tmpfiles 负责，这里手工等效地建一次。
  if [ ! -d "$RUN_DIR" ]; then
    install -d -o root -g root -m 0755 "$RUN_DIR"
  fi
}

chown_web_db() {
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

guard_foreign_units() {
  # 这台机器上除了本脚本手启的两个进程，不该还有别的 upanel 服务在跑。
  #
  # P11.3 第一次跑就栽在这上面：前几轮留下的 upanel-agent.service /
  # upanel-web.service 还留在系统里，ExecStart 指向已经被删掉的二进制，于是 systemd
  # 每 3 秒重启一次（status=203/EXEC，计数器已经到 40 多次）。它们的
  # RuntimeDirectory=upanel 每停一次就清掉 /run/upanel——里面正是脚本手启的代理的
  # socket。现场表现是代理进程活着、日志正常、预检还能应答，Web 却报
  # "connect: no such file or directory"。
  #
  # 所以环境里先停掉、禁掉、mask 掉，再断言一个都不剩。停不掉就是环境不满足，
  # 直接退出码 2，不能带着一个每 3 秒抢一次 socket 的东西跑验收。
  _foreign=$(systemctl list-unit-files --no-legend 'upanel*.service' 2>/dev/null | awk '{print $1}')
  for _u in $_foreign; do
    systemctl disable --now "$_u" >/dev/null 2>&1
    # mask 只对"单元文件在别处"的单元有效。装到 /etc/systemd/system 里的真实文件
    # 会让 mask 直接失败（File ... already exists），那种情况把单元文件改名挪走，
    # 效果同样是"起不来"，而且能看出来是谁挪的。
    if ! systemctl mask "$_u" >/dev/null 2>&1; then
      _frag=$(systemctl show -p FragmentPath --value "$_u" 2>/dev/null)
      case $_frag in
        /etc/systemd/system/*)
          mv -f "$_frag" "$_frag.disabled-by-p11-3-acceptance" 2>/dev/null ;;
      esac
    fi
    systemctl daemon-reload >/dev/null 2>&1
    _state=$(systemctl is-active "$_u" 2>/dev/null)
    case $_state in
      inactive|failed|unknown|"") ;;
      *)
        printf '这台机器上还有外来服务 %s 处于 %s：它会在别处清 /run，验收结果不可信\n' \
          "$_u" "$_state" >&2
        printf '%s\n' "$_foreign" >&2
        exit 2 ;;
    esac
  done
  # 手启之前再确认一次：不许有别的代理进程占着这个 socket。
  # 只在本阶段还没启过代理时查（AGENT_PID 为空）——否则查到的是我们自己。
  if [ -z "${AGENT_PID:-}" ] && pgrep -x upanel-agent >/dev/null 2>&1; then
    printf '这台机器上已经有 upanel-agent 进程在跑，先停掉再验收（否则 socket 归属不明）\n' >&2
    exit 2
  fi
  return 0
}

start_agent() {
  ensure_run_dir
  # 这里**故意不 rm -f "$SOCK"**。陈旧 socket 由代理自己判断并清理；如果那个路径上
  # 其实有人在服务，代理会拒绝启动并把理由写进日志——那正是我们想看到的东西。
  # 脚本先删掉就等于替代理把"有人在服务"这件事抹掉了。
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 100); do
    if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null \
         --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  note "代理就绪探针失败：socket=$([ -S "$SOCK" ] && echo 在 || echo 不在)"
  note "代理日志最后一行：$(tail -1 "$AGENT_LOG" 2>/dev/null | cut -c1-200)"
  return 1
}

start_web() {
  chown "$WEB_USER" "$WEB_DB_DIR" 2>/dev/null
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  runuser -u "$WEB_USER" -- "$WEB" --mode production --db "$WEB_DB" --listen "127.0.0.1:$PORT" \
    >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 100); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <方法> <路径> [body] [idempotency-key]
api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}; _key=${6:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    if [ -n "$_key" ]; then
      curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
        -H "Idempotency-Key: $_key" -d @"$WORK/req.json"
    else
      curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
        -d @"$WORK/req.json"
    fi
  else
    curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  fi
}

# preflight <名> <动作> <参数 JSON>：响应存成 preflight-<名>.json，回显状态码。
preflight() {
  _code=$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" \
    "$(printf '{"action":"%s","params":%s}' "$2" "$3")")
  cp "$WORK/resp.json" "$PAYLOADS/preflight-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# submit <名> <preflight_id> <幂等键>：响应存成 job-submit-<名>.json。
submit() {
  _code=$(api "$JAR" "$CSRF" POST "$JOBS_PATH" \
    "$(printf '{"preflight_id":"%s"}' "$2")" "$3")
  cp "$WORK/resp.json" "$PAYLOADS/job-submit-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# wait_job <名> <job_id> <超时秒>：轮询到终态（或连接彻底断掉），响应存成 job-<名>.json。
#
# 重启/关机这两条路径上，"拿不到终态"是**预期之内**的：代理把命令交出去之后机器就
# 开始关了。所以这里除了终态，还把"连续 3 次连不上"当成一种结果返回 offline，
# 由调用方按场景分辨它是失败还是正常。
wait_job() {
  _deadline=$(( $(date +%s) + ${3:-120} ))
  _st=unknown
  _offline=0
  while [ "$(date +%s)" -lt "$_deadline" ]; do
    api "$JAR" "$CSRF" GET "$JOBS_PATH/$2" >/dev/null
    _st=$(jget "$WORK/resp.json" 'd["data"]["status"]')
    if [ -z "$_st" ]; then
      _offline=$((_offline + 1))
      if [ "$_offline" -ge 3 ]; then
        _st=offline
        break
      fi
      sleep 0.5
      continue
    fi
    _offline=0
    case $_st in
      succeeded|failed|canceled|interrupted) break ;;
    esac
    sleep 0.5
  done
  cp "$WORK/resp.json" "$PAYLOADS/job-$1.json" 2>/dev/null
  printf '%s' "${_st:-unknown}"
}

reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

# ---------------------------------------------------------------- 环境检查
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s --phase %s\n' "$BIN_DIR" "$0" "$PHASE" >&2
  exit 2
fi
if [ "$PHASE" = report ]; then
  :
else
  for f in "$AGENT" "$WEB" "$CTL"; do
    if [ ! -x "$f" ]; then
      printf '缺少可执行文件：%s\n' "$f" >&2
      exit 2
    fi
    # 必须是这台机器的架构。拿 Windows 编出来的 PE 拷上来时，症状是
    # `Exec format error`：代理"没起来"，然后是十几个连锁失败，全都在别处。
    # 一行魔术数字就把这种错按在现场。
    if [ "$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')" != "7f454c46" ]; then
      printf '%s 不是本机的 ELF 可执行文件（拿到的是别的平台的构建产物）\n' "$f" >&2
      exit 2
    fi
  done
  for tool in python3 curl runuser; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      printf '本机缺少 %s，P11.3 的验收无法进行\n' "$tool" >&2
      exit 2
    fi
  done
  if [ ! -x /usr/bin/systemctl ] || [ ! -d /run/systemd/system ]; then
    printf '这台机器没有 systemd，电源动作本来就会以 POWER_UNSUPPORTED 拒绝——不是可验收的环境\n' >&2
    exit 2
  fi
  if [ -z "$(boot_id)" ]; then
    printf '读不到 %s，无法验收重启确认\n' "$BOOTID_FILE" >&2
    exit 2
  fi
  if ! id -u "$WEB_USER" >/dev/null 2>&1; then
    printf '本机没有 %s 账号（先装一遍 0.1 包）\n' "$WEB_USER" >&2
    exit 2
  fi
fi

# ================================================================ 阶段一
# fresh_setup 只在阶段一做：它清掉 Web 库、重建账号。阶段二/三只把同一份现场
# 重新拉起来——那正是要验证的东西（任务记录跨过一次重启还在）。
fresh_setup() {
  install -d -m 0755 /etc/upanel
  install -d -m 0700 "$AGENT_DB_DIR"
  install -d -m 0755 "$RUN_DIR"
  cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "127.0.0.1:$PORT", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "$SOCK",
    "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER",
    "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DB",
    "secret_key_path": "$KEY_FILE",
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {},
    "logs": {}
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
  chmod 0644 "$SYS_CFG"

  if [ -f "$KEY_FILE" ]; then
    phase_pass "agent-key-present"
  elif "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
    phase_pass "agent-key-created"
  else
    phase_fail "agent-key-created" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
  fi
  if start_agent; then phase_pass "agent-started"; else phase_fail "agent-started" "代理没起来（见 $AGENT_LOG）"; fi

  # 只清 Web 库：代理库里的秘密登记与操作记录是真机状态，脚本不销毁。
  rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
  printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
    --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
  expect_eq "init-admin" "0" "$?"
  chown_web_db
  if start_web; then phase_pass "web-started"; else phase_fail "web-started" "Web 没起来（见 $WEB_LOG）"; fi
}

bring_up() {
  guard_foreign_units
  if start_agent; then phase_pass "agent-restarted"; else phase_fail "agent-restarted" "代理没起来（见 $AGENT_LOG）"; fi
  if start_web; then phase_pass "web-restarted"; else phase_fail "web-restarted" "Web 没起来（见 $WEB_LOG）"; fi
}

phase_pre() {
  echo "== 0. 这台机器的现场 =="
  note "发行版：$(sed -n 's/^PRETTY_NAME="\(.*\)"$/\1/p' /etc/os-release)"
  note "内核：$(uname -r)"
  note "boot_id（重启前）：$(boot_id)"
  note "uptime：$(uptime_seconds) 秒"
  note "systemd：$(systemctl is-system-running 2>&1 | head -1)"
  phase_pass "env-systemd-and-boot-id"
  guard_foreign_units
  phase_pass "env-no-foreign-units"

  echo
  echo "== 0.1 装配：配置、代理密钥、管理员与 viewer =="
  fresh_setup
  expect_eq "login-admin" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
  CSRF=$(csrf_of "$WORK/login-admin.json")
  if [ -z "$CSRF" ]; then printf '拿不到 CSRF\n' >&2; exit 1; fi
  # viewer 由管理员建：电源动作是 admin 的事，viewer 连预检都不该进去。
  CODE=$(api "$JAR" "$CSRF" POST /api/v1/users \
    "{\"username\":\"pkg-watcher\",\"display_name\":\"pkg-watcher\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
  case $CODE in
    200|201) phase_pass "create-viewer" ;;
    *) phase_fail "create-viewer" "状态码 $CODE：$(head -c 200 "$WORK/resp.json")" ;;
  esac
  expect_eq "login-viewer" "200" "$(login pkg-watcher "$VIEWER_PASSWORD" "$VJAR" login-viewer)"

  echo
  echo "== 1. 拒收面：形状不对的电源请求一步都不许走 =="
  CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"action\":\"$POWER_ACTION\",\"params\":{\"operation\":\"reboot\",\"delay_seconds\":60}}")
  expect_eq "anon-preflight-401" "401" "$CODE"

  # viewer 不能预检写动作（CONTRACTS 第 5 节：电源动作是 admin 的事）。
  VS=$(jget "$WORK/login-viewer.json" 'd["data"]["csrf_token"]')
  CODE=$(curl -s -b "$VJAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $VS" \
    -d "{\"action\":\"$POWER_ACTION\",\"params\":{\"operation\":\"reboot\",\"delay_seconds\":60}}")
  cp "$WORK/resp.json" "$PAYLOADS/preflight-viewer.json"
  expect_eq "viewer-preflight-403" "403" "$CODE"

  JOBS_BEFORE=$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')

  for case in \
    'no-delay|{"operation":"reboot"}' \
    'delay-too-short|{"operation":"reboot","delay_seconds":5}' \
    'delay-too-long|{"operation":"reboot","delay_seconds":9999}' \
    'delay-zero|{"operation":"reboot","delay_seconds":0}' \
    'bad-operation|{"operation":"halt","delay_seconds":60}' \
    'unknown-field|{"operation":"reboot","delay_seconds":60,"force":true}' \
    'extra-flag|{"operation":"poweroff","delay_seconds":60,"immediate":true}'
  do
    name=${case%%|*}
    body=${case#*|}
    CODE=$(preflight "$name" "$POWER_ACTION" "$body")
    expect_eq "reject-$name-400" "400" "$CODE"
  done

  # 一条都不能留下任务：被拒的请求不该在任务表里留下半个记录。
  expect_eq "reject-leaves-no-job" "$JOBS_BEFORE" "$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')"
  # 但审计要留下痕迹（被拒也要有记录）。
  expect_eq "reject-is-audited" "yes" \
    "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='operation.preflight' AND outcome='failure'")" -gt 0 ] && echo yes || echo no)"

  echo
  echo "== 2. 预检说的是真话：要再认证、不要短语、可取消、锁 power-global =="
  BOOT_BEFORE=$(boot_id)
  CODE=$(preflight reboot-ok "$POWER_ACTION" '{"operation":"reboot","delay_seconds":60}')
  expect_eq "reboot-preflight-200" "200" "$CODE"
  expect_eq "reboot-allowed" "True" "$(pf reboot-ok 'd["data"]["allowed"]')"
  expect_eq "reboot-requires-reauth" "True" "$(pf reboot-ok 'd["data"]["requires_reauth"]')"
  expect_eq "reboot-no-phrase" "None" "$(pf reboot-ok 'd["data"]["confirmation_phrase"]')"
  expect_eq "reboot-cancellable" "True" "$(pf reboot-ok 'd["data"]["cancellable"]')"
  expect_eq "reboot-locks-power-global" "True" \
    "$(pf reboot-ok '"power-global" in d["data"]["locks"]')"
  expect_eq "reboot-impact-connection" "True" \
    "$(pf reboot-ok 'any(i["kind"]=="connection_loss" for i in d["data"]["impacts"])')"
  expect_eq "reboot-impact-power" "True" \
    "$(pf reboot-ok 'any(i["kind"]=="power_state" for i in d["data"]["impacts"])')"
  expect_eq "reboot-says-boot-id-confirmation" "True" \
    "$(pf reboot-ok 'any(i["kind"]=="reboot_confirmation" for i in d["data"]["impacts"])')"
  # 目标绑定的两项是前端能看到的全部；state_version（= boot_id）属于内部绑定信息，
  # 面板刻意不往前端暴露（internal/api/preflights.go 的 preflightTargetDTO）。
  # 这里断言身份指纹绑的是"这一次开机"，而不是空字符串。
  PF_FP=$(pf reboot-ok 'd["data"]["target"]["identity_fingerprint"]')
  expect_eq "reboot-target-is-this-boot" "boot:$BOOT_BEFORE" "$PF_FP"
  expect_eq "reboot-target-names-boot-id" "True" \
    "$(pf reboot-ok '"boot_id " in d["data"]["target"]["display_name"]')"
  note "预检提示：$(pf reboot-ok 'json.dumps([w["code"] for w in d["data"]["warnings"]], ensure_ascii=False)')"
  PF_REBOOT_OK=$(pf reboot-ok 'd["data"]["preflight_id"]')

  echo
  echo "== 3. 关键任务运行时拒绝（STAGES §13）=="
  # 3.0 先验再认证闸门——**必须在任何一次再认证之前**做。再认证窗口是 5 分钟，
  # 一旦打开，后面所有"没有再认证"的断言都会 202，测出来的只是自己的时序错误。
  CODE=$(submit reboot-noreauth "$PF_REBOOT_OK" "p11-3-noreauth-$$")
  expect_eq "noreauth-submit-403" "403" "$CODE"
  expect_eq "noreauth-submit-code" "REAUTH_REQUIRED" \
    "$(jget "$PAYLOADS/job-submit-reboot-noreauth.json" 'd["error"]["code"]')"
  expect_eq "noreauth-did-not-execute" "$BOOT_BEFORE" "$(boot_id)"

  # 用一个 300 秒倒计时的重启任务充当"正在执行的写操作"：它确定在跑，而且不会
  # 在断言途中悄悄结束。它与随后那次预检是两个独立的任务。
  # 被 403 拒掉的提交不会消耗预检，所以同一份 PF_REBOOT_OK 可以接着用。
  expect_eq "reauth-1" "200" "$(reauth)"
  CODE=$(submit reboot-running "$PF_REBOOT_OK" "p11-3-running-$$")
  expect_eq "running-submit-202" "202" "$CODE"
  JOB_RUNNING=$(jget "$PAYLOADS/job-submit-reboot-running.json" 'd["data"]["job_id"]')
  note "倒计时中的任务：$JOB_RUNNING"
  sleep 1

  CODE=$(preflight poweroff-busy "$POWER_ACTION" '{"operation":"poweroff","delay_seconds":60}')
  expect_eq "busy-preflight-200" "200" "$CODE"
  expect_eq "busy-not-allowed" "False" "$(pf poweroff-busy 'd["data"]["allowed"]')"
  expect_eq "busy-blocker-jobs-running" "True" \
    "$(pf poweroff-busy 'any(b["code"]=="POWER_JOBS_RUNNING" for b in d["data"]["blockers"])')"
  expect_eq "busy-blocker-names-the-job" "True" \
    "$(pf poweroff-busy 'any("system.power" in b["message"] for b in d["data"]["blockers"])')"
  note "阻塞项：$(pf poweroff-busy 'json.dumps([b["code"] for b in d["data"]["blockers"]], ensure_ascii=False)')"
  note "消息：$(pf poweroff-busy 'd["data"]["blockers"][0]["message"] if d["data"]["blockers"] else ""' | cut -c1-200)"

  # 预检被拒之后，提交这份预检也必须被拒（409 DEPENDENCY_BLOCKED）：预检的结论是固定的。
  PF_BUSY=$(pf poweroff-busy 'd["data"]["preflight_id"]')
  CODE=$(submit poweroff-busy-submit "$PF_BUSY" "p11-3-busy-$$")
  expect_eq "busy-submit-409" "409" "$CODE"
  expect_eq "busy-submit-code" "DEPENDENCY_BLOCKED" \
    "$(jget "$PAYLOADS/job-submit-poweroff-busy-submit.json" 'd["error"]["code"]')"

  echo
  echo "== 4. 倒计时取消：命令压根没发出去 =="
  CODE=$(api "$JAR" "$CSRF" POST "$JOBS_PATH/$JOB_RUNNING/cancel")
  cp "$WORK/resp.json" "$PAYLOADS/job-cancel-running.json"
  expect_eq "cancel-202" "202" "$CODE"
  expect_eq "cancel-requested" "True" \
    "$(jget "$PAYLOADS/job-cancel-running.json" 'd["data"]["cancellation_requested"]')"
  expect_eq "cancel-job-is-power" "system.power" \
    "$(jget "$PAYLOADS/job-cancel-running.json" 'd["data"]["action"]')"
  ST=$(wait_job cancel-running "$JOB_RUNNING" 60)
  expect_eq "cancel-terminal" "canceled" "$ST"
  expect_eq "cancel-outcome-empty" "True" \
    "$(jget "$PAYLOADS/job-cancel-running.json" 'd["data"]["result"] in (None, {})')"
  expect_eq "cancel-no-power-check" "None" \
    "$(jget "$PAYLOADS/job-cancel-running.json" 'd["data"]["power_check"]')"
  # 取消是真的：机器还在原来那次开机上。
  expect_eq "cancel-boot-id-unchanged" "$BOOT_BEFORE" "$(boot_id)"
  phase_pass "cancel-did-not-execute"
  note "uptime：$(uptime_seconds) 秒（没有重启）"

  echo
  echo "== 5. 下发真实重启（本阶段到此为止）=="
  # 倒计时的秒数是**脚本能不能活着看到结果**的问题，不是产品参数：代理在倒计时结束时
  # 下发 `systemctl reboot`，机器立刻开始关机——这个脚本就住在那台机器上，它会在同一秒
  # 被杀掉。所以这里取 60 秒，在 25 秒的观察窗里确认"任务在跑、机器没动"，把"终态是
  # accepted"留给重启之后的阶段二去断言（记录跨重启还在，那正是 §7 要证明的事）。
  #
  # 第一次跑用的是 30 秒 + 等终态，脚本死在 wait_job 里：日志停在"等待终态"，
  # 连 state/reboot.json 都没写出来，阶段二无从开始。错在脚本的时序，不在代理。
  CODE=$(preflight reboot-run "$POWER_ACTION" '{"operation":"reboot","delay_seconds":60}')
  expect_eq "run-preflight-200" "200" "$CODE"
  expect_eq "run-allowed" "True" "$(pf reboot-run 'd["data"]["allowed"]')"
  PF_RUN=$(pf reboot-run 'd["data"]["preflight_id"]')
  # 再认证闸门在 §3.0 验过（403 REAUTH_REQUIRED）。这里不再重复：本阶段已经
  # 再认证过，窗口还没关，再断言一次"没有再认证必须被拒"只会断言出一场假的通过。
  # 仍然要确认一件事：这次提交没有绕过预检的放行结论。
  expect_eq "run-preflight-allows" "True" "$(pf reboot-run 'd["data"]["allowed"]')"

  CODE=$(submit reboot-run "$PF_RUN" "p11-3-reboot-$$")
  expect_eq "run-submit-202" "202" "$CODE"
  JOB_REBOOT=$(jget "$PAYLOADS/job-submit-reboot-run.json" 'd["data"]["job_id"]')
  note "重启任务：$JOB_REBOOT（60 秒倒计时 + 下发）"

  # 先记账再观察：这几行必须落盘，否则阶段二什么也接不上。
  printf '{"job_id":"%s","boot_before":"%s","uptime_before":"%s","job_running":"%s","submitted_at":"%s"}\n' \
    "$JOB_REBOOT" "$BOOT_BEFORE" "$(uptime_seconds)" "$JOB_RUNNING" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    >"$STATE/reboot.json"

  # 倒计时期间：任务在跑，机器还在原来那次开机上，命令还没发出去。
  ST=$(wait_job reboot-countdown "$JOB_REBOOT" 25)
  note "25 秒观察窗内的状态：$ST"
  case $ST in
    running|queued|offline) phase_pass "reboot-countdown-still-running" ;;
    *) phase_fail "reboot-countdown-still-running" "倒计时期间不该是这个状态：$ST" ;;
  esac
  expect_eq "reboot-countdown-did-not-restart-yet" "$BOOT_BEFORE" "$(boot_id)"
  # 还在倒计时 → 没有终态结果，也就不可能有 power_check。这是"受理 ≠ 发生"的
  # 正面证据：这一刻连结果都还没产生。
  expect_eq "reboot-countdown-has-no-result" "True" \
    "$(jbf reboot-countdown 'd["data"]["result"] in (None, {})')"

  # 终态（accepted / command / exit_code / 无 verified_at / power_check 不能是 confirmed）
  # 由阶段二在重启回来之后断言——那才是它真正的证据位置。这里如实记为未执行，
  # 不让它悄悄消失。
  notrun "reboot-terminal-state-in-phase-pre" "脚本与那台机器是同一台：下发 reboot 的同时它就被关掉了，终态只能等重启之后读（见阶段二 §7）"

  cp "$AGENT_LOG" "$WORK/logs/agent-pre.log" 2>/dev/null

  # 到此为止服务必须活着：重启任务还在倒计时，由它来关掉这台机器。
  KEEP_SERVICES=1
  echo
  echo "本阶段结束。机器将在几十秒内重启；重启之后由宿主调用 --phase post。"
  summary_pre
}

summary_pre() {
  printf '%s\n' "$FAILED" >"$STATE/fails-pre.txt"
  echo
  echo "== 阶段一汇总 =="
  printf 'PASS %d / FAIL %d / NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
  if [ "$FAILED" -gt 0 ]; then
    printf '失败项：%s\n' "$FAILED_NAMES"
    return 1
  fi
  return 0
}

# ================================================================ 阶段二
phase_post() {
  echo "== 6. 重启之后：boot_id 说了什么 =="
  if [ ! -f "$STATE/reboot.json" ]; then
    printf '缺少 %s：先跑 --phase pre\n' "$STATE/reboot.json" >&2
    exit 2
  fi
  BOOT_BEFORE=$(st reboot 'd["boot_before"]')
  JOB_REBOOT=$(st reboot 'd["job_id"]')
  require_value "$BOOT_BEFORE" "重启前的 boot_id"
  require_value "$JOB_REBOOT" "重启任务 id"
  BOOT_NOW=$(boot_id)
  note "重启前的 boot_id：$BOOT_BEFORE"
  note "现在的 boot_id：  $BOOT_NOW"
  note "现在的 uptime：   $(uptime_seconds) 秒"
  expect_ne "boot-id-changed" "$BOOT_BEFORE" "$BOOT_NOW"
  # 阶段一的失败不能因为重启就"消失"：它写在 /var/tmp 里，跨过一次重启还在。
  expect_eq "phase-pre-had-no-failures" "0" "$(cat "$STATE/fails-pre.txt" 2>/dev/null || echo missing)"
  if [ "$(uptime_seconds)" -lt 900 ]; then
    phase_pass "uptime-says-fresh-boot"
  else
    phase_fail "uptime-says-fresh-boot" "uptime $(uptime_seconds) 秒，不像刚启动"
  fi

  bring_up
  expect_eq "post-login-admin" "200" "$(login admin "$PASSWORD" "$JAR" login-admin-post)"
  CSRF=$(csrf_of "$WORK/login-admin-post.json")

  # 面板自己的现场读数要与内核一致。
  CODE=$(api "$JAR" "$CSRF" GET "$POWER_STATE_PATH")
  cp "$WORK/resp.json" "$PAYLOADS/power-state-post.json"
  expect_eq "power-state-200" "200" "$CODE"
  expect_eq "power-state-boot-id" "$BOOT_NOW" \
    "$(jget "$PAYLOADS/power-state-post.json" 'd["data"]["boot_id"]')"

  echo
  echo "== 7. 任务记录跨过一次重启还在，而且说的是老实话 =="
  ST=$(wait_job reboot-after "$JOB_REBOOT" 30)
  expect_eq "reboot-record-survives" "succeeded" "$ST"
  expect_eq "reboot-after-outcome" "accepted" "$(jbf reboot-after 'd["data"]["result"]["outcome"]')"
  expect_eq "reboot-after-boot-id-before" "$BOOT_BEFORE" \
    "$(jbf reboot-after 'd["data"]["result"]["observed_state"]["boot_id_before"]')"
  # 终态记录是重启之后读的（阶段一里脚本与那台机器同时被关掉）：这些事实偏偏只有
  # 活下来的一方才能证明——受理、命令、退出码，以及**没有** verified_at。
  expect_eq "reboot-no-verified-at" "None" \
    "$(jbf reboot-after 'd["data"]["result"]["verified_at"]')"
  expect_eq "reboot-command" "/usr/bin/systemctl reboot" \
    "$(jbf reboot-after 'd["data"]["result"]["observed_state"]["command"]')"
  expect_eq "reboot-exit-code" "0" \
    "$(jbf reboot-after 'd["data"]["result"]["observed_state"]["exit_code"]')"
  expect_eq "reboot-power-check-state" "confirmed" \
    "$(jbf reboot-after 'd["data"]["power_check"]["state"]')"
  expect_eq "reboot-power-check-changed" "True" \
    "$(jbf reboot-after 'd["data"]["power_check"]["changed"]')"
  expect_eq "reboot-power-check-boot-id-now" "$BOOT_NOW" \
    "$(jbf reboot-after 'd["data"]["power_check"]["boot_id_now"]')"
  expect_eq "reboot-power-check-says-confirmed" "True" \
    "$(jbf reboot-after '"重启已确认" in d["data"]["power_check"]["message"]')"
  note "核对消息：$(jbf reboot-after 'd["data"]["power_check"]["message"]')"

  # 列表不带核对节（一次几十条，每条去问一次代理既慢又没意义）。
  CODE=$(api "$JAR" "$CSRF" GET "$JOBS_PATH?limit=5")
  cp "$WORK/resp.json" "$PAYLOADS/jobs-list.json"
  expect_eq "jobs-list-200" "200" "$CODE"
  expect_eq "jobs-list-no-power-check" "True" \
    "$(jget "$PAYLOADS/jobs-list.json" 'all((j.get("power_check") is None) for j in d["data"]["jobs"])')"

  echo
  echo "== 8. 下发关机（本阶段到此为止，机器不再回来）=="
  BOOT_PRE_POWEROFF=$(boot_id)
  # 同 §5 的道理：脚本住在这台机器上，关机把它自己一起关掉。所以 60 秒倒计时 +
  # 25 秒观察窗，终态（accepted / command / 无 verified_at / 人工开机的提示）留到
  # 阶段三读——那时候机器已经由宿主重新上电，记录还在不在才是要证明的事。
  CODE=$(preflight poweroff-run "$POWER_ACTION" '{"operation":"poweroff","delay_seconds":60}')
  expect_eq "poweroff-preflight-200" "200" "$CODE"
  expect_eq "poweroff-allowed" "True" "$(pf poweroff-run 'd["data"]["allowed"]')"
  expect_eq "poweroff-warns-manual-start" "True" \
    "$(pf poweroff-run 'any(w["code"]=="poweroff_needs_manual_start" for w in d["data"]["warnings"])')"
  expect_eq "poweroff-requires-reauth" "True" "$(pf poweroff-run 'd["data"]["requires_reauth"]')"
  expect_eq "poweroff-no-phrase" "None" "$(pf poweroff-run 'd["data"]["confirmation_phrase"]')"
  expect_eq "poweroff-impact-connection" "True" \
    "$(pf poweroff-run 'any(i["kind"]=="connection_loss" for i in d["data"]["impacts"])')"
  expect_eq "poweroff-no-boot-id-confirmation-claim" "False" \
    "$(pf poweroff-run 'any(i["kind"]=="reboot_confirmation" for i in d["data"]["impacts"])')"

  PF_OFF=$(pf poweroff-run 'd["data"]["preflight_id"]')
  expect_eq "reauth-3" "200" "$(reauth)"
  CODE=$(submit poweroff-run "$PF_OFF" "p11-3-poweroff-$$")
  expect_eq "poweroff-submit-202" "202" "$CODE"
  JOB_OFF=$(jget "$PAYLOADS/job-submit-poweroff-run.json" 'd["data"]["job_id"]')
  note "关机任务：$JOB_OFF（60 秒倒计时 + 下发）"

  # 先记账：阶段三靠这两行接上。
  printf '{"job_id":"%s","boot_before":"%s","submitted_at":"%s"}\n' \
    "$JOB_OFF" "$BOOT_PRE_POWEROFF" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$STATE/poweroff.json"

  ST=$(wait_job poweroff-countdown "$JOB_OFF" 25)
  note "25 秒观察窗内的状态：$ST"
  case $ST in
    running|queued|offline) phase_pass "poweroff-countdown-still-running" ;;
    *) phase_fail "poweroff-countdown-still-running" "倒计时期间不该是这个状态：$ST" ;;
  esac
  expect_eq "poweroff-countdown-machine-still-up" "$BOOT_PRE_POWEROFF" "$(boot_id)"
  expect_eq "poweroff-countdown-has-no-result" "True" \
    "$(jbf poweroff-countdown 'd["data"]["result"] in (None, {})')"

  notrun "poweroff-terminal-state-in-phase-post" "脚本与那台机器是同一台：下发 poweroff 的同时它就被关掉了，终态只能等宿主重新上电之后读（见阶段三 §9）"

  cp "$AGENT_LOG" "$WORK/logs/agent-post.log" 2>/dev/null

  # 同阶段一：服务必须活到倒计时结束，由那个任务把机器关掉。
  KEEP_SERVICES=1
  echo
  echo "本阶段结束。机器将断电；宿主重新上电之后调用 --phase final。"
  summary_post
}

summary_post() {
  echo
  echo "== 阶段二汇总 =="
  printf 'PASS %d / FAIL %d / NOT RUN %d（本阶段）\n' "$PASSED" "$FAILED" "$NOTRUN"
  if [ "$FAILED" -gt 0 ]; then
    printf '失败项：%s\n' "$FAILED_NAMES"
    return 1
  fi
  return 0
}

# ================================================================ 阶段三
phase_final() {
  echo "== 9. 关机之后：能说什么、不能说什么 =="
  if [ ! -f "$STATE/poweroff.json" ]; then
    printf '缺少 %s：先跑 --phase post\n' "$STATE/poweroff.json" >&2
    exit 2
  fi
  BOOT_PRE_OFF=$(st poweroff 'd["boot_before"]')
  JOB_OFF=$(st poweroff 'd["job_id"]')
  require_value "$BOOT_PRE_OFF" "关机前的 boot_id"
  require_value "$JOB_OFF" "关机任务 id"
  BOOT_NOW=$(boot_id)
  note "关机前的 boot_id：$BOOT_PRE_OFF"
  note "现在的 boot_id：  $BOOT_NOW"
  note "现在的 uptime：   $(uptime_seconds) 秒"
  # 这里证明的是"机器在关机之后重新启动过"，**不是**"电源真的被切断"——
  # 后者没有任何本地证据，脚本不假装能证明。
  expect_ne "poweroff-machine-restarted-afterwards" "$BOOT_PRE_OFF" "$BOOT_NOW"

  bring_up
  expect_eq "final-login-admin" "200" "$(login admin "$PASSWORD" "$JAR" login-admin-final)"
  CSRF=$(csrf_of "$WORK/login-admin-final.json")

  ST=$(wait_job poweroff-after "$JOB_OFF" 30)
  expect_eq "poweroff-record-survives" "succeeded" "$ST"
  expect_eq "poweroff-after-outcome" "accepted" \
    "$(jbf poweroff-after 'd["data"]["result"]["outcome"]')"
  expect_eq "poweroff-after-boot-id-before" "$BOOT_PRE_OFF" \
    "$(jbf poweroff-after 'd["data"]["result"]["observed_state"]["boot_id_before"]')"
  # 终态事实在阶段二没能断言（脚本随机器一起被关掉），在这里补齐。
  expect_eq "poweroff-no-verified-at" "None" \
    "$(jbf poweroff-after 'd["data"]["result"]["verified_at"]')"
  expect_eq "poweroff-command" "/usr/bin/systemctl poweroff" \
    "$(jbf poweroff-after 'd["data"]["result"]["observed_state"]["command"]')"
  expect_eq "poweroff-exit-code" "0" \
    "$(jbf poweroff-after 'd["data"]["result"]["observed_state"]["exit_code"]')"
  expect_eq "poweroff-hint-says-manual-start" "True" \
    "$(jbf poweroff-after '"人工开机" in d["data"]["result"]["recovery_hint"]')"
  expect_eq "poweroff-after-power-check" "confirmed" \
    "$(jbf poweroff-after 'd["data"]["power_check"]["state"]')"
  # 这一条是本轮的核心措辞：可以说"回来过"，不可以把"回来过"当成"电源被切断"的证据。
  expect_eq "poweroff-message-refuses-power-claim" "True" \
    "$(jbf poweroff-after '"现场" in d["data"]["power_check"]["message"]')"
  expect_eq "poweroff-message-no-success-claim" "False" \
    "$(jbf poweroff-after '"关机成功" in d["data"]["power_check"]["message"]')"
  expect_ne "poweroff-outcome-is-not-verified" "verified" \
    "$(jbf poweroff-after 'd["data"]["result"]["outcome"]')"
  note "关机核对消息：$(jbf poweroff-after 'd["data"]["power_check"]["message"]')"

  echo
  echo "== 10. 审计：三次下发、一次取消、一次被拒都在账上 =="
  expect_eq "audit-has-power-preflights" "yes" \
    "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='operation.preflight' AND target='preflight:$POWER_ACTION'")" -gt 0 ] && echo yes || echo no)"
  # 任务类的审计记的是 job:<id>（一条意图一个 id），动作名在 details 里。
  expect_eq "audit-has-power-submits" "yes" \
    "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='job.submit' AND redacted_details LIKE '%$POWER_ACTION%'")" -ge 3 ] && echo yes || echo no)"
  expect_eq "audit-has-power-cancel" "yes" \
    "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='job.cancel' AND redacted_details LIKE '%$POWER_ACTION%'")" -ge 1 ] && echo yes || echo no)"
  # 这一条原本是假的通过。查询写成 SELECT group_concat(redacted_details) 却漏了
  # FROM audit_events：没有 FROM 时 SQLite 在空作用域里找列，直接报 no such column。
  # dbq 的 stderr 只进了日志，grep 从空串里当然找不到口令，于是"通过"。
  # 没查成不是清白，是不知道——所以查询本身失败也必须判失败。
  if ! AUDIT_DETAILS=$(dbq "$WEB_DB" 'SELECT group_concat(redacted_details) FROM audit_events'); then
    phase_fail "audit-without-password" "审计明文查询失败，无法断言（没查成 ≠ 清白）"
  elif printf '%s' "$AUDIT_DETAILS" | grep -qF "$PASSWORD"; then
    phase_fail "audit-without-password" "审计明文字段里出现管理员口令"
  else
    phase_pass "audit-without-password"
  fi
  note "审计行数：$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM audit_events')；任务数：$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')"

  echo
  echo "== 11. 三条电源任务各自说清了什么 =="
  for name in reboot-after poweroff-after; do
    note "$name: status=$(jbf $name 'd["data"]["status"]') outcome=$(jbf $name 'd["data"]["result"]["outcome"]') power_check=$(jbf $name 'd["data"]["power_check"]["state"]')"
  done
  expect_eq "three-power-tasks" "yes" \
    "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs WHERE action='$POWER_ACTION'")" -ge 3 ] && echo yes || echo no)"

  echo
  echo "== 阶段三汇总 =="
  printf 'PASS %d / FAIL %d / NOT RUN %d（本阶段）\n' "$PASSED" "$FAILED" "$NOTRUN"
  if [ "$FAILED" -gt 0 ]; then
    printf '失败项：%s\n' "$FAILED_NAMES"
    return 1
  fi
  return 0
}

# ================================================================ 汇总
phase_report() {
  if [ ! -f "$RESULTS" ]; then
    printf '没有 %s：三个阶段都还没跑\n' "$RESULTS"
    exit 2
  fi
  _p=$(grep -c '^PASS' "$RESULTS" || true)
  _f=$(grep -c '^FAIL' "$RESULTS" || true)
  _n=$(grep -c '^NOTRUN' "$RESULTS" || true)
  printf '阶段    PASS %s / FAIL %s / NOT RUN %s\n' "$_p" "$_f" "$_n"
  if [ "$_f" -gt 0 ]; then
    printf '失败项：\n'
    grep '^FAIL' "$RESULTS" | cut -f2,3 | sed 's/^/  /'
  fi
  [ "$_n" -gt 0 ] && grep '^NOTRUN' "$RESULTS" | cut -f2,3 | sed 's/^/  未执行：/'
  printf '\n明细：%s\n' "$RESULTS"
  [ "$_f" -gt 0 ] && exit 1
  exit 0
}

case $PHASE in
  pre) phase_pre ;;
  post) phase_post ;;
  final) phase_final ;;
  report) phase_report ;;
  *) printf '未知阶段：%s\n' "$PHASE" >&2; exit 2 ;;
esac
