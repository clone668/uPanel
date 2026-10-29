#!/bin/sh
# P13.2 Linux 验收脚本：告警规则、持续时间、恢复阈值、空间双阈值、去重、冷却、维护静默。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。不依赖 Go 工具链：
# 用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p13-2-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P13.2、PLAN §18、CONTRACTS §13.2）：
#   A. 规则校验与权限：viewer 只能读、写要 admin + CSRF、未登录 401；
#      duration_seconds 越界 / 未知指标 / 恢复线越到不安全侧 / 双口径三个字段只给一半
#      都回 400；PATCH 缺 version 400、旧 version 409、未知 id 404
#   B. 生命周期（**受控种入**的一条序列，不是靠压满 CPU）：先到 pending 不触发 →
#      持续够才 firing，并且只开**一条**事件 → 再评估两轮仍是同一条（未变化不刷屏）→
#      数据断掉时状态不动且 last_reason 说清原因（丢失数据不当成 0）→ 回落后要恢复
#      持续够才 resolved → 恢复只发生一次 → 事件带出实际 duration 与整个区间的 peak →
#      进冷却，冷却期内不再触发
#   C. 空间双阈值：同一份真实容量数据上，any 触发、all 不触发（组合方式真的是对偶的）
#   D. 维护静默：窗口内新开的事件 silenced=true 且**照样在列表里**、规则状态照样 firing
#      （只压通知不压判定）；解除后新开的事件 silenced=false
#   E. 规则删除：删掉正在报警的规则 → 事件被收口成 resolved、rule_id 变 0（历史保留，
#      不留一条永远 firing 的孤儿行）
#   F. 审计：alert.rule.create/update/delete 与 alert.silence.update 都落在 audit_events
#   G. 日志卫生：web 日志里没有口令、没有 panic
#
# 时间尺度说明：评估周期是 15 秒（alerts.DefaultInterval），duration_seconds 的下限是
# 30 秒（alerts.MinDurationSeconds；容量序列的真实下限是 60 秒，见 CONTRACTS §13.2），
# 而“持续”要求窗口里**每一个**样本都在线外——所以一次 pending→firing 要 30—60 秒，
# 一次 firing→ok 同样要 30—60 秒。这不是脚本慢，是契约本身的尺度。
#
# 为什么不靠压满 CPU 制造超限：早先版本起满 `while :` 循环压 CPU，结果 R1 一直 firing
# 不恢复、"冷却期内不再触发"那条断言跟着连锁失败——因为它们断言的是**负载调度**，而不是
# 告警逻辑。这里改成受控种入（seed.py）：给一条不存在的网卡 accept0 每 5 秒写一个
# rx_bytes_per_sec 样本，写什么值由 $WORK/probe.phase 决定（high=5000 / low=10 / gap=不写），
# 于是“哪一秒超限、哪一秒回落、哪一秒断数”都由脚本自己说了算，失败只会指向产品。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p13-2}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19453}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

# 保留窗口给足：验收期间不希望清理线程把正在被判定的样本裁掉。
RAW_RETENTION=3600
MINUTE_RETENTION=7200
HOUR_RETENTION=14400

# R1 的冷却给到 240 秒：断言“冷却期内不再触发”要在恢复后几十秒内取样，
# 冷却太短会跟断言窗口打架，那种失败什么也证明不了。
COOLDOWN=240

# 受控种入的序列（见文件头说明）：一条不存在的网卡，只有本脚本会写它。
SEED_SCOPE=interface
SEED_TARGET=accept0
SEED_METRIC=rx_bytes_per_sec
PHASE_FILE=$WORK/probe.phase

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log

PASSED=0
FAILED=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""
SEED_PID=""
WAITED=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }

expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}

# jget <文件> <python 表达式>：把表达式的结果印出来。
# 布尔统一印成 true/false：Python 的 True/False 与 JSON 的 true/false 在同一份脚本里
# 混用，会让断言写成 "false" 而实际拿到 "False"，那种失败看起来像产品不生效。
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
v=eval(sys.argv[2])
print(str(v).lower() if isinstance(v, bool) else v)' "$1" "$2" 2>/dev/null
}

jgetx() {
  f=$1; shift
  python3 -c 'import json,datetime,sys
f=sys.argv[1]; a=sys.argv[2:-1]; e=sys.argv[-1]
d=json.load(open(f, encoding="utf-8"))
def u(x):
    return int(datetime.datetime.fromisoformat(x.replace("Z","+00:00")).timestamp())
print(eval(e))' "$f" "$@" 2>/dev/null
}

start_agent() {
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 100); do
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
    --listen "127.0.0.1:$PORT" >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 150); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
  for _ in $(seq 1 150); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
    sleep 0.2
  done
  return 1
}

# write_seeder：把受控种入用的 python 脚本写到 $WORK。
#
# 为什么不在 shell 里直接写库：拼 INSERT 的引号在两种 VM 的 locale 上容易咬人，而且
# “对齐到 5 秒桶、按 phase 决定写不写”本来就不是 shell 该干的活。
#
# 为什么由 upanel-web 跑它（而不是 root）：这个库归 upanel-web 所有，root 写一次会留下
# root 属主的 -wal/-shm，之后 Web 自己就写不进去了——那种失败要查半天。
write_seeder() {
  cat >"$WORK/seed.py" <<'PY'
import os
import sqlite3
import sys
import time

SCOPE, TARGET, METRIC = "interface", "accept0", "rx_bytes_per_sec"
BUCKET = 5
# phase 文件里的一个词决定这一刻写什么。没有这一项（gap、或文件被删）= 这一刻不写，
# 于是窗口里出现真正的空洞——这就是“采集停了”的样子。
VALUES = {"high": 5000.0, "low": 10.0}


def main():
    db_path, phase_path = sys.argv[1], sys.argv[2]
    backfill = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    db = sqlite3.connect(db_path, timeout=15)
    db.execute("PRAGMA busy_timeout = 15000")

    def value_now():
        try:
            with open(phase_path, encoding="utf-8") as f:
                return VALUES.get(f.read().strip())
        except OSError:
            return None

    def write(ts, value):
        db.execute(
            "INSERT INTO metric_samples"
            " (scope, target_id, metric, resolution, ts, value, min_value, max_value, sample_count)"
            " VALUES (?, ?, ?, 'raw', ?, ?, ?, ?, 1)"
            " ON CONFLICT (scope, target_id, metric, resolution, ts)"
            " DO UPDATE SET value=excluded.value, min_value=excluded.min_value,"
            " max_value=excluded.max_value",
            (SCOPE, TARGET, METRIC, ts, value, value, value))

    # 先把窗口补满：规则建好后的第一轮评估就该看到一段完整的超限区间，
    # 否则“为什么它还在 pending”会被正确地回答成“窗口前段还没数据”而浪费一轮。
    if backfill > 0:
        value = value_now()
        if value is not None:
            now = int(time.time())
            start = ((now - backfill) // BUCKET) * BUCKET
            end = (now // BUCKET) * BUCKET
            ts = start
            while ts < end:
                write(ts, value)
                ts += BUCKET
            db.commit()
            print("seed: 回填 %d 秒（%s）" % (end - start, value), flush=True)

    while True:
        t = time.time()
        wake = (int(t) // BUCKET + 1) * BUCKET
        time.sleep(max(0.0, wake - t))
        value = value_now()
        if value is None:
            continue
        try:
            write(wake, value)
            db.commit()
        except sqlite3.Error as exc:
            # 写库偶尔撞上 Web 自己的写入（busy）不该让种入停掉：下一跳再来一次。
            print("seed: 写入失败（继续）: %s" % exc, file=sys.stderr, flush=True)


main()
PY
  chmod 0644 "$WORK/seed.py"
}

# start_seed <回填秒数>：以 upanel-web 身份起种入循环，日志进 $WORK/logs/seed.log。
start_seed() {
  runuser -u "$WEB_USER" -- python3 "$WORK/seed.py" "$WEB_DB" "$PHASE_FILE" "${1:-0}" \
    >>"$WORK/logs/seed.log" 2>&1 &
  SEED_PID=$!
}

stop_seed() {
  if [ -n "$SEED_PID" ] && kill -0 "$SEED_PID" 2>/dev/null; then
    kill -TERM "$SEED_PID" 2>/dev/null
    wait "$SEED_PID" 2>/dev/null
  fi
  SEED_PID=""
}

# phase <high|low|gap>：切换种入的值。写文件是原子的（printf 到临时文件再 mv），
# 避免种入循环读到半行。
phase() {
  printf '%s\n' "$1" >"$PHASE_FILE.tmp" && mv -f "$PHASE_FILE.tmp" "$PHASE_FILE"
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <method> <path> [body]
#
# CSRF 令牌**始终**带上（DELETE 没有 body，早先版本只在有 body 时才加这个头，
# 于是所有 DELETE 都拿到 403 CSRF_FAILED——那不是产品的边界，是脚本的 bug）。
api() {
  jar=$1; csrf=$2; method=$3; path=$4; body=${5:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
      -d @"$WORK/req.json"
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf"
  fi
}

# apinocsrf：故意不带 CSRF 令牌，用来证明写接口真的要求它。
apinocsrf() {
  jar=$1; method=$2; path=$3; body=${4:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -d @"$WORK/req.json"
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN"
  fi
}

rulesapi() {
  curl -s -b "${1:-$WORK/admin.jar}" -o "$WORK/rules.json" -w '%{http_code}' -X GET \
    "$BASE/api/v1/alerts/rules" -H "Origin: $ORIGIN"
}

# eventsapi <jar> [附加查询串]：结果落在 $WORK/events.json。
eventsapi() {
  curl -s -b "$1" -o "$WORK/events.json" -w '%{http_code}' -X GET \
    "$BASE/api/v1/alerts/events?limit=200${2:-}" -H "Origin: $ORIGIN"
}

silenceapi() {
  curl -s -b "$WORK/admin.jar" -o "$WORK/silence.json" -w '%{http_code}' -X GET \
    "$BASE/api/v1/alerts/silence" -H "Origin: $ORIGIN"
}

# rule_field <id> <字段>：从 $WORK/rules.json 里取。布尔印成 true/false，空字符串照印，
# 找不到规则印 <missing>——三种情况必须能一眼分开，否则“规则没了”会被读成“字段是空的”。
rule_field() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
rid=int(sys.argv[2]); f=sys.argv[3]
for r in d["data"]:
    if r["id"]==rid:
        v=r[f]
        print("" if v is None else (("true" if v else "false") if isinstance(v,bool) else v))
        break
else:
    print("<missing>")' "$WORK/rules.json" "$1" "$2" 2>/dev/null
}

# rule_unix <id> <字段>：把规则里的 RFC3339 字段换成 Unix 秒。空串印 0，规则不存在印 -1。
# 比在 jgetx 的表达式里塞引号可靠：那层嵌套的引号一定会在某个 VM 的 locale 上咬人。
rule_unix() {
  python3 -c 'import json,sys,datetime
d=json.load(open(sys.argv[1], encoding="utf-8"))
rid=int(sys.argv[2]); f=sys.argv[3]
for r in d["data"]:
    if r["id"]==rid:
        v=r[f]
        print(0 if not v else int(datetime.datetime.fromisoformat(v.replace("Z","+00:00")).timestamp()))
        break
else:
    print(-1)' "$WORK/rules.json" "$1" "$2" 2>/dev/null
}

ev_count() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(sum(1 for e in d["data"] if e["rule_id"]==int(sys.argv[2])))' "$WORK/events.json" "$1" 2>/dev/null
}

# ev_field <rule_id> <序号，0 为最新> <字段>
ev_field() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
evs=[e for e in d["data"] if e["rule_id"]==int(sys.argv[2])]
i=int(sys.argv[3]); f=sys.argv[4]
if i >= len(evs):
    print("<missing>")
else:
    v=evs[i][f]
    print("" if v is None else (("true" if v else "false") if isinstance(v,bool) else v))' \
    "$WORK/events.json" "$1" "$2" "$3" 2>/dev/null
}

# ev_null <rule_id> <序号，0 为最新> <字段>：这个字段在 JSON 里是不是 null。
# 印 null / value:<v> / missing——三者必须能分开。"是 null""字段不存在""有值"在契约上
# 是三件事，而 ev_field 把 null 印成空串，正好分不出前两者。
ev_null() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
evs=[e for e in d["data"] if e["rule_id"]==int(sys.argv[2])]
i=int(sys.argv[3]); f=sys.argv[4]
if i >= len(evs) or f not in evs[i]:
    print("missing")
elif evs[i][f] is None:
    print("null")
else:
    print("value:%s" % (evs[i][f],))' "$WORK/events.json" "$1" "$2" "$3" 2>/dev/null
}

# pending_probe <rule_id>：在**同一个读事务**里取规则状态、计持续起点、last_reason 与事件数。
#
# 为什么要合成一次读：分两次读会造出一个假失败。2026-10-26 那一轮就是——状态在 :14:16
# 读到 pending，事件列表到 :14:34 才读，中间那 18 秒里规则已经 firing，于是"pending 期间
# 还没有任何事件"拿到 1。那不是产品没守住"持续没够不报"，是判据在问一个已经过去的时刻。
#
# 注意 `state_since` 在库里是 **Unix 秒整数**（`alerts.applyOutcome` 用 `unixOrZero` 写），
# 不是 RFC3339 文本。别拿 iso_unix 去解它：那一刻 Python 抛异常、`$()` 吞掉输出，
# 剩下的算术表达式会以 "expecting primary" 崩掉整个脚本（第 5 轮就是这么死的）。
pending_probe() {
  python3 - "$WEB_DB" "$1" <<'PY'
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
db.execute("BEGIN")
rid = int(sys.argv[2])
row = db.execute(
    "SELECT state, state_since, last_reason FROM alert_rules WHERE id=?", (rid,)).fetchone()
n = db.execute("SELECT COUNT(*) FROM alert_events WHERE rule_id=?", (rid,)).fetchone()[0]
print("%s|%d|%s|%d" % (row[0], row[1] or 0, row[2] or "", n) if row else "missing|||0")
PY
}

# iso_unix <RFC3339>：把接口里的时间换成 Unix 秒。空串或解析失败印 0——
# 印空会让调用处的 `$(( ))` 直接崩掉，一次手误赔掉一整轮验收。
iso_unix() {
  python3 -c 'import datetime,sys
s=sys.argv[1].strip()
if not s:
    print(0)
else:
    try:
        print(int(datetime.datetime.fromisoformat(s.replace("Z","+00:00")).timestamp()))
    except ValueError:
        print(0)' "$1" 2>/dev/null || echo 0
}

refresh_rules() { rulesapi >/dev/null 2>&1; }
refresh_events() { eventsapi "$WORK/admin.jar" >/dev/null 2>&1; }

# wait_rule <id> <字段> <期望值> <超时秒>：等到规则的某个字段等于期望值。
# 结果留在 $WAITED，所以失败时 expect_eq 会把“实际等到了什么”印出来。
wait_rule() {
  wait_id=$1; wait_field=$2; wait_exp=$3
  wait_deadline=$(( $(now_unix) + $4 ))
  WAITED=""
  while :; do
    refresh_rules
    WAITED=$(rule_field "$wait_id" "$wait_field")
    [ "$WAITED" = "$wait_exp" ] && return 0
    if [ "$(now_unix)" -ge "$wait_deadline" ]; then return 1; fi
    sleep 2
  done
}

# sqlq <SQL>：只读查一个标量。
#
# 出错时印 <sql-error: ...> 而不是一声不吭地印空串。这条纪律是被 #5 打出来的：审计断言
# 把列名写成 details（真名 redacted_details），SQLite 报错被 2>/dev/null 吞掉，断言拿到
# 空串，印出来是“期望 1 实际 ''”——三行红看起来像产品没记审计，其实是脚本问错了列。
sqlq() {
  python3 - "$WEB_DB" "$1" <<'PY'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
    print(db.execute(sys.argv[2]).fetchone()[0])
except Exception as exc:
    print("<sql-error: %s>" % exc)
PY
}

sqllist() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
for row in db.execute(sys.argv[2]):
    print("      " + " | ".join(str(c) for c in row))
PY
}

chown_web_db() {
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
}

now_unix() { date -u +%s; }
iso_of() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

cleanup() {
  stop_seed
  stop_web
  stop_agent
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（解析响应、受控种入）"; else fail "python3 可用" "缺少 python3"; fi
if command -v runuser >/dev/null 2>&1; then pass "runuser 可用（以 Web 身份写库）"; else fail "runuser 可用" "缺少 runuser"; fi

# 环境体检：整机打开的文件描述符总数。
#
# 代理每 5 秒扫一次存储拓扑，其中“设备是否被占用”那一步要把**每个进程的每个 fd**
# 取一遍设备号。fd 总数一旦涨到几千，这一次扫描就要几十秒，于是代理忙在一个核上、
# storage.topology 查询超时、Web 侧把它报成“代理不可用”，容量序列一个点也落不下来——
# 后面所有跟文件系统有关的断言都会连锁失败，看起来像产品坏了。
#
# 24.04 验收 VM 上真撞到过：systemd-logind 泄漏了 7477 个 fd（大量验收 session 反复
# 进出留下的），拓扑查询从 0.03 秒变成 25—41 秒。这种机器上跑出来的结果什么也证明
# 不了，所以在这里直接停下（退出码 2 = 环境不满足），而不是产出一堆假失败。
FD_TOTAL=$(python3 - <<'PY' 2>/dev/null
import os
total = 0
top, top_n = "", 0
for pid in os.listdir("/proc"):
    if not pid.isdigit():
        continue
    try:
        n = len(os.listdir("/proc/" + pid + "/fd"))
    except OSError:
        continue
    total += n
    if n > top_n:
        try:
            with open("/proc/" + pid + "/comm", encoding="utf-8") as f:
                top, top_n = f.read().strip(), n
        except OSError:
            pass
print(total, top, top_n)
PY
)
FD_MAX=${FD_TOTAL%% *}
note "整机 fd 总数 ${FD_MAX:-?}（最多的进程：$(printf '%s' "$FD_TOTAL" | cut -d' ' -f2-3)）"
if [ -n "${FD_MAX:-}" ] && [ "$FD_MAX" -gt 4000 ] 2>/dev/null; then
  printf '%s\n' \
    "环境不满足：整机打开了 $FD_MAX 个 fd（空载 VM 只有几百个，最多的是 $(printf '%s' "$FD_TOTAL" | cut -d' ' -f2-3)）。" \
    "代理的占用检查要遍历每个进程的每个 fd，这种机器上存储拓扑查询会慢到超时，" \
    "跟文件系统有关的断言必然连锁失败——那不是产品的结论。" >&2
  printf '%s\n' "先 sudo systemctl restart systemd-logind（或重启这台 VM）再跑本脚本。" >&2
  exit 2
fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER（验收环境未预置）"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/out-web" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/out-web" "$WORK/logs"
chown "$WEB_USER" "$WORK/out-web"
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo
echo "== 1. 装配：配置（采集打开）+ 代理 + 管理员 + Web =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
chmod 0755 "$RUN_DIR"
chown root:root "$RUN_DIR"

cat >"$SYS_CFG" <<JSON
{
  "web": {
    "mode": "production",
    "listen_address": "127.0.0.1:$PORT",
    "shutdown_timeout_seconds": 10,
    "metrics": {
      "enabled": true,
      "interval_seconds": 2,
      "raw_retention_seconds": $RAW_RETENTION,
      "minute_retention_seconds": $MINUTE_RETENTION,
      "hour_retention_seconds": $HOUR_RETENTION,
      "max_rows": 500000
    }
  },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "/var/lib/upanel-agent/agent.db",
    "secret_key_path": "/etc/upanel/agent.key"
  },
  "database": {
    "path": "$WEB_DB",
    "busy_timeout_ms": 5000
  },
  "log": {
    "level": "info",
    "format": "json"
  }
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

if start_agent; then pass "代理启动并创建 socket"; else fail "代理启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
chown_web_db

if start_web; then pass "Web 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
  "{\"username\":\"alice\",\"display_name\":\"alice\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 201" "201" "$code"
code=$(login alice "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")

# 等容量序列落第一个点：容量 30 秒才刷新一次（storage.DefaultCapacityTTL），采集还要先
# 拿到代理的拓扑快照。单次取样在这个时间尺度上就是碰运气——这里最多等 90 秒，
# 超时就带着采集器的 last_error 一起报出来，而不是只说“没有 used_percent 条目”。
code=""
FS_TARGET=""
fs_deadline=$(( $(now_unix) + 90 ))
while :; do
  code=$(curl -s -b "$WORK/admin.jar" -o "$WORK/status.json" -w '%{http_code}' -X GET \
    "$BASE/api/v1/system/metrics/status" -H "Origin: $ORIGIN")
  FS_TARGET=$(jget "$WORK/status.json" \
    'sorted({e["target_id"] for e in d["data"]["series"].get("filesystem", []) if e["metric"]=="used_percent"})[0]')
  if [ -n "$FS_TARGET" ] && [ "$FS_TARGET" != "None" ]; then break; fi
  if [ "$(now_unix)" -ge "$fs_deadline" ]; then break; fi
  sleep 3
done
expect_eq "采集状态 200" "200" "$code"
if [ -n "$FS_TARGET" ] && [ "$FS_TARGET" != "None" ]; then
  pass "取到可用的文件系统目标 $FS_TARGET（空间规则要按挂载点建）"
else
  fail "取到可用的文件系统目标" \
    "等了 90 秒，status 的 series.filesystem 里仍没有 used_percent；采集器 last_error=$(jget "$WORK/status.json" 'd["data"]["last_error"]')"
fi

echo
echo "== 2. A：规则校验、权限与乐观并发 =="

code=$(rulesapi "$WORK/viewer.jar")
expect_eq "viewer 读规则列表 200（权限 V）" "200" "$code"
expect_eq "规则列表初始为空" "0" "$(jget "$WORK/rules.json" 'len(d["data"])')"

code=$(apinocsrf "$WORK/viewer.jar" POST "/api/v1/alerts/rules" \
  "{\"name\":\"x\",\"metric\":\"cpu_usage_percent\",\"comparison\":\"gte\",\"threshold\":1,\"recovery_threshold\":0,\"duration_seconds\":30}")
expect_eq "viewer 建规则 403（权限 A）" "403" "$code"
expect_eq "viewer 建规则的错误码" "FORBIDDEN" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X GET "$BASE/api/v1/alerts/rules" -H "Origin: $ORIGIN")
expect_eq "未登录读规则 401" "401" "$code"
expect_eq "未登录的错误码" "UNAUTHENTICATED" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(apinocsrf "$WORK/admin.jar" POST "/api/v1/alerts/rules" \
  "{\"name\":\"x\",\"metric\":\"cpu_usage_percent\",\"comparison\":\"gte\",\"threshold\":1,\"recovery_threshold\":0,\"duration_seconds\":30}")
expect_eq "管理员不带 CSRF 建规则 403" "403" "$code"

BAD_BODY="{\"name\":\"缺持续\",\"metric\":\"cpu_usage_percent\",\"comparison\":\"gte\",\"threshold\":85,\"recovery_threshold\":70}"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" "$BAD_BODY")
expect_eq "不给 duration_seconds → 400" "400" "$code"
expect_eq "不给 duration_seconds 的错误码" "VALIDATION_ERROR" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_contains "报错点出缺的是 duration_seconds" "duration_seconds" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  '{"name":"未知指标","metric":"nope_percent","comparison":"gte","threshold":85,"recovery_threshold":70,"duration_seconds":30}')
expect_eq "未知指标 → 400" "400" "$code"
expect_contains "未知指标的报错点出名字" "nope_percent" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  '{"name":"持续太短","metric":"cpu_usage_percent","comparison":"gte","threshold":85,"recovery_threshold":70,"duration_seconds":10}')
expect_eq "duration_seconds 低于下限 → 400" "400" "$code"
expect_contains "报错说明下限是 30" "30" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  '{"name":"恢复线在错的一侧","metric":"cpu_usage_percent","comparison":"gte","threshold":85,"recovery_threshold":90,"duration_seconds":30}')
expect_eq "恢复线不低于告警线 → 400（死区必须存在）" "400" "$code"
expect_contains "报错点出 recovery_threshold" "recovery_threshold" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"双口径跨 scope\",\"metric\":\"used_percent\",\"target_id\":\"$FS_TARGET\",\"comparison\":\"gte\",\"threshold\":85,\"recovery_threshold\":70,\"secondary_metric\":\"cpu_usage_percent\",\"secondary_comparison\":\"lte\",\"secondary_threshold\":10,\"duration_seconds\":30}")
expect_eq "次级指标跨 scope → 400" "400" "$code"
expect_contains "报错说明两个 scope 不在一处" "scope" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"双口径缺两项\",\"metric\":\"used_percent\",\"target_id\":\"$FS_TARGET\",\"comparison\":\"gte\",\"threshold\":85,\"recovery_threshold\":70,\"secondary_metric\":\"available_bytes\",\"duration_seconds\":30}")
expect_eq "次级口径只给一个字段 → 400" "400" "$code"
expect_contains "报错说明三个字段要一起给" "secondary_comparison" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

# 临时规则：用来验 combination 归一化、PATCH 的 version 语义、DELETE 的 version 语义。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  '{"name":"临时规则","metric":"load1","comparison":"gte","threshold":500,"recovery_threshold":400,"duration_seconds":30,"cooldown_seconds":0,"combination":"all","enabled":false}')
expect_eq "建临时规则 201" "201" "$code"
TMP_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
TMP_VER=$(jget "$WORK/resp.json" 'd["data"]["version"]')
expect_eq "没有次级口径时 combination 被归一成 any（留个 all 会骗人）" "any" \
  "$(jget "$WORK/resp.json" 'd["data"]["combination"]')"
expect_eq "enabled=false 生效（临时规则不参与判定）" "false" "$(jget "$WORK/resp.json" 'd["data"]["enabled"]')"
expect_eq "新建规则的初始状态是 ok" "ok" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/alerts/rules/$TMP_ID" '{"name":"改个名"}')
expect_eq "PATCH 不给 version → 400" "400" "$code"
expect_contains "报错说明 version 的用途" "version" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/alerts/rules/$TMP_ID" \
  "{\"name\":\"改个名\",\"version\":$TMP_VER}")
expect_eq "PATCH 带上正确 version 200" "200" "$code"
expect_eq "改名生效" "改个名" "$(jget "$WORK/resp.json" 'd["data"]["name"]')"
expect_eq "version 递增" "$((TMP_VER + 1))" "$(jget "$WORK/resp.json" 'd["data"]["version"]')"
expect_eq "没提的字段沿用原值（PATCH 是叠加不是覆盖）" "500" \
  "$(jget "$WORK/resp.json" 'd["data"]["threshold"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/alerts/rules/$TMP_ID" \
  "{\"name\":\"用旧 version 改\",\"version\":$TMP_VER}")
expect_eq "旧 version PATCH → 409" "409" "$code"
expect_eq "冲突的错误码" "VERSION_CONFLICT" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/alerts/rules/999999" '{"name":"x","version":1}')
expect_eq "改不存在的规则 → 404" "404" "$code"
expect_eq "不存在的错误码" "NOT_FOUND" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/alerts/rules/$TMP_ID?version=abc")
expect_eq "DELETE 的 version 不是数字 → 400" "400" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/alerts/rules/$TMP_ID?version=$TMP_VER")
expect_eq "DELETE 用旧 version → 409" "409" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/alerts/rules/$TMP_ID")
expect_eq "DELETE 不带 version 按当前版本删 200" "200" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/alerts/rules/999999")
expect_eq "删不存在的规则 → 404" "404" "$code"

code=$(eventsapi "$WORK/admin.jar" "&state=bogus")
expect_eq "事件列表 state 取闭集外的值 → 400" "400" "$code"
code=$(eventsapi "$WORK/admin.jar" "&rule_id=abc")
expect_eq "事件列表 rule_id 不是数字 → 400" "400" "$code"
code=$(eventsapi "$WORK/admin.jar" "")
expect_eq "事件列表 200" "200" "$code"
expect_eq "此刻还没有任何事件" "0" "$(jget "$WORK/events.json" 'len(d["data"])')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PUT "/api/v1/alerts/silence" \
  "{\"starts_at\":\"$(iso_of $(( $(now_unix) - 60 )))\"}")
expect_eq "静默只给起点 → 400（起止要同进同出）" "400" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PUT "/api/v1/alerts/silence" \
  "{\"starts_at\":\"$(iso_of $(( $(now_unix) + 600 )))\",\"ends_at\":\"$(iso_of $(( $(now_unix) - 60 )))\"}")
expect_eq "静默窗口终点早于起点 → 400" "400" "$code"
code=$(apinocsrf "$WORK/viewer.jar" PUT "/api/v1/alerts/silence" '{}')
expect_eq "viewer 改静默 403（权限 A）" "403" "$code"

# 契约说"没有窗口时起止为 null"。这里判的是 **JSON null**，不是空串——空串在客户端是
# 另一件事（`new Date("")` 是非法时间、`x == null` 判成有值）。产品曾经把零值时间格式化成
# 空串，openapi 的 lint 关把这类漂移挡在宿主检查里（见 Q418）。
code=$(silenceapi)
expect_eq "没设过窗口时读静默 200" "200" "$code"
expect_eq "没有窗口时 starts_at 是 null（不是空串）" "true" \
  "$(jget "$WORK/silence.json" 'd["data"]["starts_at"] is None')"
expect_eq "没有窗口时 ends_at 是 null（不是空串）" "true" \
  "$(jget "$WORK/silence.json" 'd["data"]["ends_at"] is None')"

echo
echo "== 3. B/C/D：生命周期（受控种入）、空间双阈值、维护静默 =="

# 受控序列先就位：一条不存在的网卡，由 seed.py 每 5 秒写一个 rx_bytes_per_sec 样本。
# 回填 40 秒是为了让规则建好后的**第一轮**评估就有完整窗口，否则“还在 pending”会被
# 正确地回答成“窗口前段还没数据”，白等一轮。
write_seeder
phase high
start_seed 40
seed_deadline=$(( $(now_unix) + 25 ))
SEEDED=0
while :; do
  SEEDED=$(sqlq "SELECT COUNT(*) FROM metric_samples
                  WHERE scope = '$SEED_SCOPE' AND target_id = '$SEED_TARGET'
                    AND metric = '$SEED_METRIC' AND resolution = 'raw'")
  [ "${SEEDED:-0}" -ge 5 ] 2>/dev/null && break
  if [ "$(now_unix)" -ge "$seed_deadline" ]; then break; fi
  sleep 1
done
if [ "${SEEDED:-0}" -ge 5 ] 2>/dev/null; then
  pass "受控序列就位（$SEED_TARGET/$SEED_METRIC 已写入 $SEEDED 个样本）"
else
  fail "受控序列就位" "只写入 ${SEEDED:-0} 个样本，见 $WORK/logs/seed.log"
fi

# 维护静默先开：窗口内新开的事件要被标成 silenced，且**照样记录、照样判定**。
SIL_START=$(iso_of $(( $(now_unix) - 60 )))
SIL_END=$(iso_of $(( $(now_unix) + 900 )))
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PUT "/api/v1/alerts/silence" \
  "{\"starts_at\":\"$SIL_START\",\"ends_at\":\"$SIL_END\",\"reason\":\"验收：维护窗口\"}")
expect_eq "设置维护静默 200" "200" "$code"
code=$(silenceapi)
expect_eq "静默窗口当前生效（active=true）" "true" "$(jget "$WORK/silence.json" 'd["data"]["active"]')"
expect_eq "静默理由被记住" "验收：维护窗口" "$(jget "$WORK/silence.json" 'd["data"]["reason"]')"

# 容量指标的写入节奏是 30 秒（见 metrics.CapacityCadence）：30 秒的窗口装不下两个点，
# 那种规则的症状是“配好了永远不响”。这里用真机把这个地板钉住。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"空间持续太短\",\"metric\":\"used_percent\",\"target_id\":\"$FS_TARGET\",\"comparison\":\"gte\",\"threshold\":85,\"recovery_threshold\":70,\"duration_seconds\":30}")
expect_eq "容量指标 duration_seconds 低于两个采集周期 → 400" "400" "$code"
expect_contains "报错点出 duration_seconds" "duration_seconds" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"
expect_contains "报错说清是“装不下两个点”" "两个点" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

# R1：受控序列上的全链路。阈值 1000、恢复线 500（中间是死区），持续 30 秒（raw 序列
# 的地板就是 30 秒），冷却 $COOLDOWN 秒。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"网卡入向流量持续偏高\",\"metric\":\"$SEED_METRIC\",\"target_id\":\"$SEED_TARGET\",\"comparison\":\"gte\",\"threshold\":1000,\"recovery_threshold\":500,\"duration_seconds\":30,\"cooldown_seconds\":$COOLDOWN}")
expect_eq "建 R1（网卡入向 gte 1000/500，持续 30 秒，冷却 $COOLDOWN 秒）201" "201" "$code"
R1=$(jget "$WORK/resp.json" 'd["data"]["id"]')
expect_eq "R1 的 scope 由指标反查成 interface" "interface" "$(jget "$WORK/resp.json" 'd["data"]["scope"]')"
# 计时基准取在 R1 建好**这一刻**，不是四条规则都建完那一刻。这台 VM 上每个 POST 要几秒，
# 四条建完已经过去十几秒，而评估循环在你建 R2 的时候就可能把 R1 判成 pending——
# 按"建完四条"起算会算出一个比 duration_seconds 还短的耗时，看起来像产品提前触发。
R1_T0=$(now_unix)

# R2/R3：同一份**真实**容量数据上的一对，只有 combination 不同。
# 阈值 0 让主口径恒超限、次级 available_bytes<=0 恒不超限——不用去填满磁盘，
# 也能把“any 只要一个超限就触发 / all 必须全部超限才触发”这件事分开证。
#
# 这两条同时也是 Q414 的回归位：容量序列 30 秒才落一行，如果覆盖率容差按 5 秒写死，
# 它们的 last_reason 永远会是“窗口内没有任何样本”，规则配好了却一声不响。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"空间（任一）\",\"metric\":\"used_percent\",\"target_id\":\"$FS_TARGET\",\"comparison\":\"gte\",\"threshold\":0,\"recovery_threshold\":-0.5,\"secondary_metric\":\"available_bytes\",\"secondary_comparison\":\"lte\",\"secondary_threshold\":0,\"combination\":\"any\",\"duration_seconds\":60,\"cooldown_seconds\":0}")
expect_eq "建 R2（空间双口径 any，持续 60 秒）201" "201" "$code"
R2=$(jget "$WORK/resp.json" 'd["data"]["id"]')
expect_eq "R2 的 scope 由指标反查成 filesystem" "filesystem" "$(jget "$WORK/resp.json" 'd["data"]["scope"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"空间（全部）\",\"metric\":\"used_percent\",\"target_id\":\"$FS_TARGET\",\"comparison\":\"gte\",\"threshold\":0,\"recovery_threshold\":-0.5,\"secondary_metric\":\"available_bytes\",\"secondary_comparison\":\"lte\",\"secondary_threshold\":0,\"combination\":\"all\",\"duration_seconds\":60,\"cooldown_seconds\":0}")
expect_eq "建 R3（空间双口径 all，持续 60 秒）201" "201" "$code"
R3=$(jget "$WORK/resp.json" 'd["data"]["id"]')

# R4：控制组。阈值高得永远到不了，用来证明“没有数据时规则不会被当成超限”。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  '{"name":"负载控制组","metric":"load1","comparison":"gte","threshold":1000,"recovery_threshold":999,"duration_seconds":30,"cooldown_seconds":0}')
expect_eq "建 R4（永不超限的控制组）201" "201" "$code"
R4=$(jget "$WORK/resp.json" 'd["data"]["id"]')

note "R1=$R1 R2=$R2 R3=$R3 R4=$R4，受控序列 $SEED_TARGET/$SEED_METRIC，目标文件系统 $FS_TARGET"

# ---- 3.1 超限 → pending（不触发）→ 持续够 → firing（只开一条） ----

# 轮询 pending_probe，命中的那个快照同时用来断言"状态是 pending"和"事件是 0 条"。
probe_deadline=$(( $(now_unix) + 60 ))
PROBE=""
while :; do
  PROBE=$(pending_probe "$R1")
  [ "${PROBE%%|*}" = "pending" ] && break
  if [ "$(now_unix)" -ge "$probe_deadline" ]; then break; fi
  sleep 1
done
PROBE_STATE=${PROBE%%|*}
PROBE_REST=${PROBE#*|}
PROBE_SINCE=${PROBE_REST%%|*}
PROBE_REST=${PROBE_REST#*|}
PROBE_REASON=${PROBE_REST%|*}
PROBE_COUNT=${PROBE##*|}
# state_since 必须是纯数字（Unix 秒）。不是的话当成 0：宁可让下面那条判据如实失败，
# 也不要让 `$(( ))` 崩掉整个脚本——一次崩溃赔掉的是一整轮验收的时间。
case $PROBE_SINCE in '' | *[!0-9]*) PROBE_SINCE=0 ;; esac
expect_eq "超限但还没持续够 → pending（不是立刻触发）" "pending" "$PROBE_STATE"
expect_contains "pending 的 last_reason 说明在计持续" "持续" "$PROBE_REASON"
# 同一个快照里的两个事实：还在 pending，且一条事件都没有。
expect_eq "pending 期间还没有任何事件（持续没够就不报）" "0" "$PROBE_COUNT"

wait_rule "$R1" state firing 120
expect_eq "持续够 30 秒后进入 firing" "firing" "$WAITED"
note "R1 从建规则到 firing 用了 $(( $(now_unix) - R1_T0 )) 秒（pending 至少 30 秒）"

refresh_events
expect_eq "触发时只开一条事件（不是每轮开一条）" "1" "$(ev_count "$R1")"
# 不受取样时刻影响的强判据：事件的 started_at 必须比"开始计持续"那一刻至少晚 30 秒。
# 上面那条"从建规则到 firing 用了几秒"只是给人看的旁注（POST 慢、断言顺序都会影响它）；
# 这条才是"持续时间真的被尊重了"的证明。
EV_DELAY=$(( $(iso_unix "$(ev_field "$R1" 0 started_at)") - PROBE_SINCE ))
if [ "$EV_DELAY" -ge 30 ]; then
  pass "事件从“开始计持续”起等了 $EV_DELAY 秒才开（≥ duration_seconds=30）"
else
  fail "事件从“开始计持续”起等了 $EV_DELAY 秒才开（≥ duration_seconds=30）" \
    "pending_since=$PROBE_SINCE（Unix 秒），event_started=$(ev_field "$R1" 0 started_at)"
fi
expect_eq "事件当时就是 firing" "firing" "$(ev_field "$R1" 0 state)"
expect_eq "事件落着规则的阈值快照" "1000" "$(ev_field "$R1" 0 threshold)"
expect_eq "事件落在受控序列上（scope/target 可追溯）" "$SEED_TARGET" "$(ev_field "$R1" 0 target_id)"
expect_eq "静默窗口内触发的事件被标记 silenced=true" "true" "$(ev_field "$R1" 0 silenced)"
# 还在报的事件不该有恢复时间。契约 §13.2 说这里是 null；产品曾经给的是空串，
# 于是"恢复了没有"只能靠 state 判，resolved_at 上什么也看不出来（Q418）。
expect_eq "还在 firing 的事件 resolved_at 是 null（不是空串）" "null" "$(ev_null "$R1" 0 resolved_at)"
expect_eq "没确认过的事件 acked_at 是 null" "null" "$(ev_null "$R1" 0 acked_at)"

# ---- 3.2 空间双口径：同一份真实数据上，any 触发、all 不触发 ----

wait_rule "$R2" state firing 200
expect_eq "R2（any）在真实容量数据上触发（Q414 回归位）" "firing" "$WAITED"
refresh_rules
expect_eq "R3（all）在**同一份数据**上没有触发" "ok" "$(rule_field "$R3" state)"
refresh_events
expect_eq "R2 开了一条事件" "1" "$(ev_count "$R2")"
expect_eq "R2 的事件也被标成 silenced（静默不拦判定，只标记通知）" "true" "$(ev_field "$R2" 0 silenced)"
expect_eq "R3 一条事件都没有" "0" "$(ev_count "$R3")"
expect_eq "R4（永不超限）仍是 ok" "ok" "$(rule_field "$R4" state)"

# ---- 3.3 未变化不刷屏 ----

R3_EVAL_BEFORE=$(rule_unix "$R3" last_eval_at)
note "等两轮评估（每轮 15 秒），确认事件数不涨"
sleep 33
refresh_rules
refresh_events
expect_eq "两轮之后 R1 仍只有一条事件（未变化不刷屏）" "1" "$(ev_count "$R1")"
expect_eq "两轮之后 R1 还是 firing" "firing" "$(rule_field "$R1" state)"
R3_EVAL_AFTER=$(rule_unix "$R3" last_eval_at)
if [ "$R3_EVAL_AFTER" -gt "$R3_EVAL_BEFORE" ] 2>/dev/null; then
  pass "R3 在这段时间里确实被评估过（last_eval_at $R3_EVAL_BEFORE → $R3_EVAL_AFTER），仍判不触发"
else
  fail "R3 在这段时间里确实被评估过" "last_eval_at 没有前进（$R3_EVAL_BEFORE），“没触发”可能只是“没跑”"
fi

# ---- 3.4 静默解除：只影响之后新开的事件 ----

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PUT "/api/v1/alerts/silence" '{}')
expect_eq "清除静默 200" "200" "$code"
expect_eq "清除后 active=false" "false" "$(jget "$WORK/resp.json" 'd["data"]["active"]')"
# 这条早先断言的是"起止为空串"——那正是产品把零值时间格式化成 "" 的那个漂移（Q418）。
# 契约里是 null，脚本跟着错的实现写，就成了"两个错的东西互相印证"。
expect_eq "清除后 starts_at 是 null（不是空串）" "true" \
  "$(jget "$WORK/resp.json" 'd["data"]["starts_at"] is None')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"网卡（静默解除后）\",\"metric\":\"$SEED_METRIC\",\"target_id\":\"$SEED_TARGET\",\"comparison\":\"gte\",\"threshold\":1000,\"recovery_threshold\":500,\"duration_seconds\":30,\"cooldown_seconds\":0}")
expect_eq "静默解除后建 R5 201" "201" "$code"
R5=$(jget "$WORK/resp.json" 'd["data"]["id"]')
wait_rule "$R5" state firing 120
expect_eq "R5 在解除静默后正常触发（静默只影响标记，不影响判定）" "firing" "$WAITED"
refresh_events
expect_eq "R5 的事件 silenced=false（silenced 是触发那一刻的快照）" "false" "$(ev_field "$R5" 0 silenced)"
expect_eq "R1 那条早先的事件 silenced 仍是 true（历史不被后来的设置改写）" "true" \
  "$(ev_field "$R1" 0 silenced)"

# ---- 3.5 丢失数据不是 0：受控序列断数 ----

phase gap
GAP_AT=$(now_unix)
note "受控序列停写（phase=gap），等 40 秒确认状态原地不动"
sleep 40
refresh_rules
refresh_events
expect_eq "断数期间正在报警的 R1 **不收口**（没有数据不等于恢复正常）" "firing" "$(rule_field "$R1" state)"
expect_eq "断数期间 R1 仍只有那一条 firing 事件" "1" "$(ev_count "$R1")"
expect_eq "断数期间事件也还是 firing" "firing" "$(ev_field "$R1" 0 state)"
R1_GAP_EVAL=$(rule_unix "$R1" last_eval_at)
if [ "$R1_GAP_EVAL" -gt "$GAP_AT" ] 2>/dev/null; then
  pass "断数期间评估循环真的跑过（last_eval_at=$R1_GAP_EVAL 晚于断数时刻 $GAP_AT），“状态没动”是判定结果"
else
  fail "断数期间评估循环真的跑过" "last_eval_at=$R1_GAP_EVAL 没有晚于断数时刻 $GAP_AT"
fi
expect_contains "断数期间 R1 的 last_reason 说清是数据不够" "样本" "$(rule_field "$R1" last_reason)"
expect_eq "断数期间永不超限的控制组仍是 ok（没有数据不等于超限）" "ok" "$(rule_field "$R4" state)"
phase high

# ---- 3.6 真实采集断掉也一样：停代理 ----

DEAD_AGENT_PID=$AGENT_PID
stop_agent
expect_eq "代理进程已经退出（kill -0 失败，不是只删了 socket 文件）" "gone" \
  "$(kill -0 "$DEAD_AGENT_PID" 2>/dev/null && echo alive || echo gone)"
# 停机 40 秒：比 staleAfter(主机 20 秒) 长出一个评估周期(15 秒)，才保证至少有一轮
# 是在“真实采集的样本已经过期”之后跑的——否则“状态没动”可能只是“这轮还没跑”。
note "代理停掉 40 秒（真实序列断数），受控序列继续写"
sleep 40
refresh_rules
refresh_events
expect_eq "真实采集断掉时控制组 R4 仍是 ok（0 个样本不被当成“全部超限”）" "ok" "$(rule_field "$R4" state)"
expect_contains "R4 的 last_reason 说清是真实数据不够" "样本" "$(rule_field "$R4" last_reason)"
expect_eq "真实采集断掉时 R2 没有被判成恢复（容量的容差是 90 秒，这一轮还不敢下结论）" "firing" \
  "$(rule_field "$R2" state)"
expect_eq "受控序列没断，R1 依然是 firing" "firing" "$(rule_field "$R1" state)"
if start_agent; then pass "代理恢复启动"; else fail "代理恢复启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
sleep 12

# ---- 3.7 恢复：回落到恢复线内还要持续够 ----

phase low
RESOLVE_AT=$(now_unix)
note "受控序列回落到 10（恢复线 500 以下），开始等 R1 收口"
wait_rule "$R1" state ok 240
expect_eq "回落到恢复线内并持续够 → 状态回到 ok" "ok" "$WAITED"
note "R1 从回落到收口用了 $(( $(now_unix) - RESOLVE_AT )) 秒（恢复同样要持续 30 秒）"

refresh_events
expect_eq "恢复之后仍然只有那一条事件（恢复只发一次，不是新开一条）" "1" "$(ev_count "$R1")"
expect_eq "那条事件被原地收口成 resolved" "resolved" "$(ev_field "$R1" 0 state)"
expect_eq "收敛事件带出实际持续时长（≥30 秒）" "true" \
  "$(jget "$WORK/events.json" 'all(int(e["duration_seconds"]) >= 30 for e in d["data"] if e["rule_id"]=='"$R1"')')"
expect_eq "收敛事件的 peak_value 覆盖整个区间（顶到 1000 以上）" "true" \
  "$(jget "$WORK/events.json" 'all(e["peak_value"] is not None and e["peak_value"] >= 1000 for e in d["data"] if e["rule_id"]=='"$R1"')')"
expect_eq "收敛事件有 resolved_at" "true" \
  "$(jget "$WORK/events.json" 'all(bool(e["resolved_at"]) for e in d["data"] if e["rule_id"]=='"$R1"')')"
refresh_rules
expect_eq "恢复后规则进入冷却（cooldown_until 非空）" "true" \
  "$(jget "$WORK/rules.json" 'all(bool(r["cooldown_until"]) for r in d["data"] if r["id"]=='"$R1"')')"

# ---- 3.8 冷却期内再超限：不能重新起算 ----

phase high
note "冷却期内再次超限，等 45 秒确认不会重新触发"
sleep 45
refresh_rules
refresh_events
expect_eq "冷却期内再超限不重新起算（状态仍是 ok）" "ok" "$(rule_field "$R1" state)"
expect_eq "冷却期内仍然只有一条事件" "1" "$(ev_count "$R1")"
expect_contains "冷却期的 last_reason 说清是在等冷却" "冷却" "$(rule_field "$R1" last_reason)"
# 后面的段落不再需要超限：让受控序列回落，免得冷却到期后 R1 又自己开一条，
# 把“事件数”类断言搅浑。
phase low

echo
echo "== 4. E：删除正在报警的规则 → 事件收口、历史保留 =="
refresh_events
R2_EVENT=$(ev_field "$R2" 0 id)
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/alerts/rules/$R2")
expect_eq "删除正在 firing 的 R2 200" "200" "$code"
expect_eq "删掉规则不会删掉它的事件（历史保留）" "$R2_EVENT" \
  "$(sqlq "SELECT id FROM alert_events WHERE id = $R2_EVENT")"
expect_eq "删规则时把打开的事件收口成 resolved" "resolved" \
  "$(sqlq "SELECT state FROM alert_events WHERE id = $R2_EVENT")"
expect_eq "被删规则的事件仍留着快照（rule_name 还在）" "空间（任一）" \
  "$(sqlq "SELECT rule_name FROM alert_events WHERE id = $R2_EVENT")"
expect_eq "被删规则的事件 rule_id 置空（外键 SET NULL，不是级联删除）" "0" \
  "$(sqlq "SELECT COALESCE(rule_id, 0) FROM alert_events WHERE id = $R2_EVENT")"
expect_eq "被删规则的快照阈值还在" "0.0" \
  "$(sqlq "SELECT threshold FROM alert_events WHERE id = $R2_EVENT")"

# 收尾：把剩下的规则都删掉，库里不该留任何一条 firing 的孤儿事件。
for rid in "$R1" "$R3" "$R4" "$R5"; do
  api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/alerts/rules/$rid" >/dev/null
done
code=$(rulesapi "$WORK/admin.jar")
expect_eq "清空后规则列表为空" "0" "$(jget "$WORK/rules.json" 'len(d["data"])')"
expect_eq "库里没有留下任何 firing 的事件（停用的规则不会留着永远在报的孤儿行）" "0" \
  "$(sqlq "SELECT COUNT(*) FROM alert_events WHERE state = 'firing'")"
expect_eq "事件历史仍在库里" "1" \
  "$(sqlq "SELECT COUNT(*) >= 3 FROM alert_events")"

echo
echo "== 5. F：审计 =="
expect_eq "审计里有 alert.rule.create" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.rule.create'")"
expect_eq "审计里有 alert.rule.update" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.rule.update'")"
expect_eq "审计里有 alert.rule.delete" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.rule.delete'")"
expect_eq "审计里有 alert.silence.update" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.silence.update'")"
# 审计表里那一列叫 redacted_details（口令这类秘密在这一层就被脱掉了），不是 details。
# 早先写成 details，SQLite 报“没有这一列”而 sqlq 把 stderr 吞掉了，断言拿到空串、
# 印出来是“期望 1 实际 ''”——看着像产品没记，实际是脚本问错了列名。
expect_eq "审计里记下了阈值（能回答“把线挪到了多少”）" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.rule.create' AND redacted_details LIKE '%\"threshold\"%'")"
expect_eq "审计里记下了恢复线" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.rule.create' AND redacted_details LIKE '%\"recovery_threshold\"%'")"
expect_eq "PATCH 的审计带改动前后的值" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.rule.update' AND redacted_details LIKE '%\"before\"%'")"
expect_eq "被拒的请求也留了审计（alert.rule.manage）" "1" \
  "$(sqlq "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'alert.rule.manage'")"

echo
echo "== 6. G：日志卫生 =="
expect_eq "Web 日志里没有出现管理员口令" "0" "$(grep -c -F -- "$PASSWORD" "$WEB_LOG" 2>/dev/null | head -n 1)"
expect_eq "Web 日志里没有出现 viewer 口令" "0" "$(grep -c -F -- "$VIEWER_PASSWORD" "$WEB_LOG" 2>/dev/null | head -n 1)"
expect_eq "Web 日志里没有 panic" "0" "$(grep -c 'panic' "$WEB_LOG" 2>/dev/null | head -n 1)"
if [ -s "$WEB_LOG" ]; then
  pass "Web 日志非空（失败会写进日志，不静默）"
else
  fail "Web 日志非空" "$WEB_LOG 是空的"
fi

echo
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
