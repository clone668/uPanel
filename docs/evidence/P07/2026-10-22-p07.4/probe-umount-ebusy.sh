#!/bin/sh
# P07.4 探针：能不能在真机上确定性到达“执行端 umount 撞上 EBUSY”那一格。
#
# 为什么要单独探：卷动作有两道门，受理时的复核会把“预检之后变忙”的提交直接拒掉
# （400 VALIDATION_ERROR），所以执行端那段“umount 失败 → 把占用者再列一遍”的代码
# 只有在**受理已经通过之后**才出现占用者时才跑得到。这个窗口是毫秒级，
# 只能冻住产品自己 fork 的 umount 来制造。这个探针就是先把“冻得住、冻住之后
# 内核真的返回 EBUSY”这件事量出来，再决定验收脚本怎么写。
#
# 只用已经在盘上的东西（/etc/upanel/upanel.json、上一轮验收留下的卷登记），
# 不擦盘、不改 fstab、不删登记。跑完把手里的 fd 关掉、把卷恢复成挂载状态。
set -u
BIN=/tmp/upanel-bin
WORK=/var/tmp/upanel-accept-p07-4
PORT=19474
BASE=http://127.0.0.1:$PORT
SOCK=/run/upanel/agent.sock
CFG=/etc/upanel/upanel.json
WEB_DB=$WORK/webdata/upanel.db
WEB_USER=upanel-web
PASSWD='vm-correct-horse-9'
JAR=$WORK/probe.jar
CSRF=""
AGENT_PID=""
WEB_PID=""

say() { printf '%s\n' "$*"; }
val() { printf 'VALUE %-30s %s\n' "$1" "$2"; }

jget() { python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(eval(sys.argv[2]))" "$1" "$2" 2>/dev/null; }

start_agent() {
  "$BIN/upanel-agent" --config "$CFG" >>"$WORK/logs/agent.log" 2>&1 &
  AGENT_PID=$!
  i=0
  while [ "$i" -lt 100 ]; do [ -S "$SOCK" ] && return 0; i=$((i+1)); sleep 0.1; done
  return 1
}
start_web() {
  runuser -u "$WEB_USER" -- "$BIN/upanel-web" --mode production --db "$WEB_DB" \
    --listen "127.0.0.1:$PORT" >>"$WORK/logs/web.log" 2>&1 &
  WEB_PID=$!
  i=0
  while [ "$i" -lt 200 ]; do curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 && return 0; i=$((i+1)); sleep 0.2; done
  return 1
}
api() {
  _m=$1; _p=$2; _b=${3:-}
  if [ -n "$_b" ]; then
    printf '%s' "$_b" >"$WORK/probe-req.json"
    curl -s -b "$JAR" -o "$WORK/probe-resp.json" -w '%{http_code}' -X "$_m" "$BASE$_p" \
      -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" \
      -d @"$WORK/probe-req.json"
  else
    curl -s -b "$JAR" -o "$WORK/probe-resp.json" -w '%{http_code}' -X "$_m" "$BASE$_p" \
      -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF"
  fi
}

if [ -S "$SOCK" ]; then echo "已经有代理在跑，先停掉" >&2; exit 2; fi
start_agent || { echo "代理起不来" >&2; exit 2; }
say "代理 pid=$AGENT_PID"
start_web || { echo "Web 起不来" >&2; kill "$AGENT_PID"; exit 2; }
say "Web pid=$WEB_PID"

code=$(curl -s -c "$JAR" -o "$WORK/probe-login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $BASE" \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWD\"}")
val LOGIN "$code"
CSRF=$(jget "$WORK/probe-login.json" 'd["data"]["csrf_token"]')

# 找到一个挂着的卷（本轮的 /srv/upanel/volumes/data）。
api GET /api/v1/storage/volumes >/dev/null
cp "$WORK/probe-resp.json" "$WORK/probe-volumes.json"
VOL_ID=$(jget "$WORK/probe-volumes.json" 'next((v["id"] for v in d["data"]["volumes"] if v["observed_state"]=="mounted"), "")')
vol_ver() { api GET "/api/v1/storage/volumes/$VOL_ID" >/dev/null; cp "$WORK/probe-resp.json" "$WORK/probe-vol.json"; jget "$WORK/probe-vol.json" 'd["data"]["version"]'; }
MP=$(jget "$WORK/probe-volumes.json" 'next((v["mount_path"] for v in d["data"]["volumes"] if v["observed_state"]=="mounted"), "")')
val VOLUME_ID "$VOL_ID"
val MOUNT_PATH "$MP"
[ -n "$VOL_ID" ] || { echo "找不到挂着的卷，先手动挂一个再跑探针" >&2; kill "$WEB_PID" "$AGENT_PID"; exit 2; }

preflight() {
  printf '{"action":"storage.volume.unmount","params":{"volume_id":"%s","version":"%s"}}' "$VOL_ID" "$1" >"$WORK/probe-pre.json"
  _c=$(curl -s -b "$JAR" -o "$WORK/probe-preflight.json" -w '%{http_code}' -X POST "$BASE/api/v1/operations/preflight" \
    -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" -d @"$WORK/probe-pre.json")
  printf '%s' "$_c"
}
submit() {
  printf '{"preflight_id":"%s"}' "$1" >"$WORK/probe-sub.json"
  _c=$(curl -s -b "$JAR" -o "$WORK/probe-submit.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $2" -d @"$WORK/probe-sub.json")
  printf '%s' "$_c"
}
wait_job() {
  i=0
  while [ "$i" -lt 400 ]; do
    curl -s -b "$JAR" -o "$WORK/probe-job.json" "$BASE/api/v1/jobs/$1" -H "X-CSRF-Token: $CSRF" >/dev/null
    st=$(jget "$WORK/probe-job.json" 'd["data"]["status"]')
    case "$st" in succeeded|failed|canceled|interrupted) printf '%s' "$st"; return 0;; esac
    i=$((i+1)); sleep 0.2
  done
  printf timeout
}

# 看门狗：盯代理的子进程，看到 umount 就 SIGSTOP（同验收脚本里的实现）。
freeze_loop() {
  python3 - "$AGENT_PID" "$1" <<'PY'
import glob, os, signal, sys, time
agent, out = int(sys.argv[1]), sys.argv[2]
with open(out + ".watch", "w", encoding="ascii") as fh:
    fh.write(str(os.getpid()))
traced, deadline = set(), time.time() + 120.0
while time.time() < deadline:
    kids = []
    for path in glob.glob("/proc/%d/task/*/children" % agent):
        try:
            with open(path, encoding="ascii") as fh:
                kids.extend(fh.read().split())
        except OSError:
            continue
    for kid in kids:
        try:
            with open("/proc/%s/comm" % kid, encoding="ascii") as fh:
                name = fh.read().strip()
        except OSError:
            continue
        if kid not in traced:
            traced.add(kid)
            try:
                with open(out + ".trace", "a", encoding="ascii") as fh:
                    fh.write("%s %s\n" % (kid, name))
            except OSError:
                pass
        if name != "umount":
            continue
        try:
            os.kill(int(kid), signal.SIGSTOP)
        except OSError:
            continue
        with open(out, "w", encoding="ascii") as fh:
            fh.write(kid)
        sys.exit(0)
    time.sleep(0.0005)
sys.exit(1)
PY
}

attempt=0
while [ "$attempt" -lt 3 ]; do
  attempt=$((attempt + 1))
  VER=$(vol_ver)
  val "PREFLIGHT_ATTEMPT$attempt" "$(preflight "$VER") ver=$VER"
  allowed=$(jget "$WORK/probe-preflight.json" 'd["data"]["allowed"]')
  val "ALLOWED_ATTEMPT$attempt" "$allowed"
  [ "$allowed" = "True" ] || break
  PRE_ID=$(jget "$WORK/probe-preflight.json" 'd["data"]["preflight_id"]')
  rcode=$(api POST /api/v1/auth/reauth "{\"password\":\"$PASSWD\"}")
  val "REAUTH_ATTEMPT$attempt" "$rcode"
  rm -f "$WORK/probe-frozen.pid" "$WORK/probe-frozen.pid.watch" "$WORK/probe-frozen.pid.trace"
  freeze_loop "$WORK/probe-frozen.pid" >/dev/null 2>&1 &
  FREEZE_JOB=$!
  i=0
  while [ "$i" -lt 300 ] && [ ! -s "$WORK/probe-frozen.pid.watch" ]; do i=$((i+1)); sleep 0.01; done
  val "WATCHDOG_READY$attempt" "$( [ -s "$WORK/probe-frozen.pid.watch" ] && echo yes || echo no )"
  SCODE=$(submit "$PRE_ID" "probe-ebusy-$attempt")
  val "SUBMIT_ATTEMPT$attempt" "$SCODE"
  JOB_ID=""
  if [ "$SCODE" = "202" ]; then JOB_ID=$(jget "$WORK/probe-submit.json" 'd["data"]["job_id"]'); fi
  FROZEN=""
  i=0
  while [ "$i" -lt 1200 ]; do
    if [ -s "$WORK/probe-frozen.pid" ]; then FROZEN=$(tr -d ' \n' <"$WORK/probe-frozen.pid"); break; fi
    i=$((i+1)); sleep 0.05
  done
  if [ -n "${FREEZE_JOB:-}" ] && kill -0 "$FREEZE_JOB" 2>/dev/null; then kill -KILL "$FREEZE_JOB" 2>/dev/null; fi
  wp=$(tr -d ' \n' <"$WORK/probe-frozen.pid.watch" 2>/dev/null)
  [ -n "$wp" ] && kill -KILL "$wp" 2>/dev/null
  val "FROZEN_ATTEMPT$attempt" "${FROZEN:-none}"
  if [ -z "$FROZEN" ]; then
    val "TRACE$attempt" "$(tr '\n' ';' <"$WORK/probe-frozen.pid.trace" 2>/dev/null)"
    val "JOB_STATE$attempt" "$(wait_job "$JOB_ID")"
    # 没冻住说明卷被卸掉了：挂回去再试。
    VER=$(vol_ver)
    api POST /api/v1/operations/preflight "{\"action\":\"storage.volume.mount\",\"params\":{\"volume_id\":\"$VOL_ID\",\"version\":\"$VER\"}}" >/dev/null
    MPRE=$(jget "$WORK/probe-resp.json" 'd["data"]["preflight_id"]')
    [ -n "$MPRE" ] && api POST /api/v1/jobs "{\"preflight_id\":\"$MPRE\"}" >/dev/null
    sleep 3
    continue
  fi
  # 冻住了：这一刻受理复核已经过了、内核还没被调用。开一个 fd，再放它走。
  val "FROZEN_COMM$attempt" "$(cat /proc/"$FROZEN"/comm 2>/dev/null)"
  sh -c "exec 3<>'$MP/probe-hold.txt'; exec sleep 600" >/dev/null 2>&1 &
  HOLDER=$!
  sleep 1
  kill -CONT "$FROZEN" 2>/dev/null
  val "JOB_STATE$attempt" "$(wait_job "$JOB_ID")"
  val "JOB_CODE$attempt" "$(jget "$WORK/probe-job.json" 'd["data"]["error"]["code"]')"
  val "JOB_MESSAGE$attempt" "$(jget "$WORK/probe-job.json" 'd["data"]["error"]["message"]')"
  val "HOLDER_PID$attempt" "$HOLDER"
  val "STILL_MOUNTED$attempt" "$(findmnt -rno TARGET "$MP" >/dev/null 2>&1 && echo yes || echo no)"
  kill -9 "$HOLDER" 2>/dev/null
  break
done

# 收尾：确保卷是挂着的，停掉自己起的进程。
sleep 1
VAL=$(findmnt -rno TARGET "$MP" 2>/dev/null)
if [ -z "$VAL" ]; then
  VER=$(vol_ver)
  api POST /api/v1/operations/preflight "{\"action\":\"storage.volume.mount\",\"params\":{\"volume_id\":\"$VOL_ID\",\"version\":\"$VER\"}}" >/dev/null
  MPRE=$(jget "$WORK/probe-resp.json" 'd["data"]["preflight_id"]')
  [ -n "$MPRE" ] && api POST /api/v1/jobs "{\"preflight_id\":\"$MPRE\"}" >/dev/null
  sleep 3
fi
val FINAL_MOUNTED "$(findmnt -rno TARGET "$MP" >/dev/null 2>&1 && echo yes || echo no)"
kill "$WEB_PID" 2>/dev/null
kill "$AGENT_PID" 2>/dev/null
wait "$WEB_PID" 2>/dev/null
wait "$AGENT_PID" 2>/dev/null
say "done"
