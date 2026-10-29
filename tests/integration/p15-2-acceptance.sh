#!/bin/sh
# P15.2 Linux 验收脚本：systemd 包装、卷依赖、容器 restart=no、部署/停止/删除保留数据。
#
# 运行位置：Ubuntu 22.04 验收 VM 内，**必须以 root 运行**。这一轮会碰系统盘上的
# /etc/systemd/system、/etc/upanel/deploy，会碰一块**一次性**空盘（GPT→分区→ext4→卷），
# 会起一个真实的 docker 容器，并且会在最后**重启这台 VM**。
#
# 用法（两段，中间有一次重启）：
#   第一段（main）：
#     sudo BIN_DIR=/var/tmp/upanel-bin DISK=sdc REBOOT=0 \
#          setsid nohup sh tests/integration/p15-2-acceptance.sh >/var/tmp/p152-run.log 2>&1 &
#     轮询 /var/tmp/upanel-accept-p15-2/RESULT
#   重启（REBOOT=1 时第一段自己就叫重启；先看着 RESULT 写出来再等它回来）
#   第二段（boot，VM 回来之后跑）：
#     sudo BIN_DIR=/var/tmp/upanel-bin PHASE=boot \
#          setsid nohup sh tests/integration/p15-2-acceptance.sh >/var/tmp/p152-boot.log 2>&1 &
#     轮询 /var/tmp/upanel-accept-p15-2/RESULT-boot
#
# 两段都**必须后台起**：第一段会启动/停止代理与 Web、会调 systemctl 起停单元，SSH 的
# pipe 一抖就 SIGPIPE 掉整轮；第二段要重启 VM，前台跑等于把自己挂在半路。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。NOT RUN 是按设计列出的范围外项，单独
# 计数、不影响退出码——与 P14.x 一致。
#
# ---------------------------------------------------------------- 这一轮验什么
#
# STAGES §17 对 P15.2 的要求是“systemd 包装、挂载依赖、容器 restart=no、部署/停止/删除
# 保留数据”。四条都要有**真机读数**，不能拿单测折算：
#
#   A. **包装单元就是生命周期**（main §5）：
#      ① `/etc/systemd/system/upanel-deploy-webapp.service` 存在、0644、root:root，
#         里面有 Type=oneshot、RemainAfterExit=yes、Requires=docker.service、
#         ExecStart 只有一条 `/usr/bin/docker compose -p … -f … up -d --remove-orphans --no-build`；
#      ② `systemctl is-enabled` = enabled、`is-active` = active（**回读**，不是 compose 的退出码）；
#      ③ 渲染配置 `/etc/upanel/deploy/webapp.compose.yaml` 是目录 0700 / 文件 0600 root；
#      ④ 容器真的在跑：`compose ps` 有 1 个 running、宿主端口能 HTTP 200；
#      ⑤ 容器的绑定源就是卷上那个目录（`docker inspect` 的 Source 与 `findmnt -T` 交叉核对）。
#   B. **容器 restart=no**（main §5）：渲染配置里是字符串 `"no"`（写成裸 no 会被 YAML 1.1
#      读成布尔 false），`docker inspect` 的 RestartPolicy.Name 也是 `no`。
#   C. **卷依赖真的挡启动**（main §8）：
#      ① 单元里逐卷 BindsTo + After 对应的 mount unit，另有 RequiresMountsFor；
#      ② 卷卸下之后 `systemctl start` 必须失败（这是 BindsTo 的核心价值）；
#      ③ 卷挂回去之后同一个 `systemctl start` 必须成功，容器与 HTTP 都回来。
#   D. **重启之后不自启**（boot 段）：把 mount unit mask 掉（= 让依赖在这台机器上无法
#      成为 active），重启 VM。开机后单元仍是 enabled，但没有起来、没有容器、系统盘上那个
#      同名空目录里没有多出任何文件。这不是“关掉保护”，是把故障放到 systemd 自己的手里：
#      判据全在 journal 与 is-active 里，产品一句话都不参与。
#   E. **删除保留数据**（boot §B3/B4）：`deployment.remove` 之后单元、配置、容器都没了，
#      卷与业务目录一个字节不动（marker 的 sha256 前后逐字相同）。
#   F. **幂等重放不重写**（main §6）：同一份定义再 apply 一次 → written=false，
#      两份文件的 mtime、配置摘要、容器 ID 与启动时刻都不变。
#   G. **apply 只创建**（main §7）：带 deployment_id 再 apply → 判决层 blocker
#      `update_not_implemented`；同名但内容不同 → blocker `deployment_exists`。
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * **运行中卷失联 → systemd 自动停单元**：容器把那块卷 bind 进了自己的 mount namespace，
#     卷在内核里是 busy 的。产品会拒绝在这次 umount 上；强行 `umount -l` 造出来的现场只有
#     内核能解释，不能当产品读数。用“先停单元再卸卷、再 start 必须失败”替代（§8）。
#   * **镜像 digest 记录、健康检查、deployment.update、回切**：P15.3 的范围。
#   * **secret_ref 解出来的明文永不落日志**：P15.1 的范围（渲染层），这一轮不重验。
#   * **浏览器里的部署界面**：P15.4 的范围，这台 VM 上也没有浏览器。
#   * **24.04（VM-B）**：这一轮只跑 22.04；跨发行版矩阵在 P16 的整机矩阵里做。

set -u

umask 077

PHASE=${PHASE:-main}

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p15-2}
TMPD=$WORK/tmp
FACTS=$WORK/facts
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19491}
WEB_ADDR=127.0.0.1
BASE="http://$WEB_ADDR:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-p152-admin-43'

# 部署本体。名字同时是 compose 项目名、容器名前缀与单元名的一部分。
DEPLOY_NAME=webapp
HOST_PORT=${HOST_PORT:-18087}
IMAGE=${IMAGE:-traefik/whoami:v1.10.3}
UNIT=upanel-deploy-$DEPLOY_NAME.service
CONFIG_DIR=/etc/upanel/deploy
CONFIG_PATH=$CONFIG_DIR/$DEPLOY_NAME.compose.yaml
UNIT_PATH=/etc/systemd/system/$UNIT

# 卷夹具：一块一次性空盘（默认 sdc）。挂载点由产品按 slug 派生。
DISK=${DISK:-sdc}
SLUG=p15data
VOLUME_ROOT=/srv/upanel/volumes
MOUNT_POINT=$VOLUME_ROOT/$SLUG
MNT_UNIT=srv-upanel-volumes-$SLUG.mount
ROOT_PATH=$MOUNT_POINT/appdata

STORE=/api/v1/storage
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

STATE=$WORK/state.env
JAR=""
CSRF=""

if [ "$PHASE" = "boot" ]; then
  RESULT=$WORK/RESULT-boot
else
  RESULT=$WORK/RESULT
fi

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
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}
expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不该出现 '$2'（实际: $3）" ;;
    *) pass "$1" ;;
  esac
}
expect_true() { if [ "$2" = "True" ]; then pass "$1"; else fail "$1" "期望 True，实际 '$2'"; fi; }
expect_false() { if [ "$2" = "False" ]; then pass "$1"; else fail "$1" "期望 False，实际 '$2'"; fi; }

write_result() {
  {
    printf 'PHASE=%s\n' "$PHASE"
    printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
    printf 'FAILED_NAMES=%s\n' "${FAILED_NAMES:- (none)}"
    printf 'NOTRUN_NAMES=%s\n' "${NOTRUN_NAMES:- (none)}"
  } >"$RESULT"
}

# ---- JSON 助手 ----
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

# ---- 进程 ----

start_agent() {
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 200); do
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
  for _ in $(seq 1 250); do
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
}

chown_web_db() {
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

cleanup() {
  stop_web
  stop_agent
  return 0
}
trap cleanup EXIT
trap 'write_result; cleanup; exit 130' INT
trap 'write_result; cleanup; exit 143' TERM

# ---- 面板读的总线 ----

api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$TMPD/req.json"
    curl -s -b "$_jar" -o "$TMPD/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
      -d @"$TMPD/req.json"
  else
    curl -s -b "$_jar" -o "$TMPD/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  fi
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$TMPD/pre.json"
  _code=$(curl -s -b "$JAR" -o "$TMPD/resp.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$TMPD/pre.json")
  cp "$TMPD/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}
pget() { jget "$WORK/preflight.json" "$1"; }
pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pwarn() { pget '",".join(w["code"] for w in d["data"]["warnings"])'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
pstate() { pget 'd["data"]["target"]["display_name"]'; }
pidentity() { pget 'd["data"]["target"]["identity_fingerprint"]'; }
preauth() { pget 'bool(d["data"].get("requires_reauth"))'; }

submit_job() {
  python3 -c 'import json,sys
d={"preflight_id":sys.argv[1]}
if sys.argv[2]:
    d["confirmation_phrase"]=sys.argv[2]
print(json.dumps(d))' "$1" "${2:-}" >"$TMPD/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$TMPD/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $3" -d @"$TMPD/jobreq.json")
  cp "$TMPD/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}

wait_job() {
  _i=0
  while [ "$_i" -lt 600 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$1" >/dev/null
    cp "$TMPD/resp.json" "$WORK/job.json"
    _st=$(jobget 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}
jobget() { jget "$WORK/job.json" "$1"; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jverified() { jobget 'bool(d["data"]["result"].get("verified_at"))'; }
jerrmsg() { jobget 'd["data"].get("error",{}).get("message") or ""'; }
jerrcode() { jobget 'd["data"].get("error",{}).get("code") or ""'; }

# run_action <动作> <参数 JSON> <幂等键>
# 预检 →（需要再认证就先再认证）→ 提交 → 等终态。结果留在全局变量里。
run_action() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pallowed)
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  PRE_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
  PRE_REAUTH=$(preauth)
  if [ "$PRE_REAUTH" = "True" ]; then
    REAUTH_CODE=$(reauth)
    [ "$REAUTH_CODE" = "200" ] || fail "再认证（$3）" "状态码 $REAUTH_CODE"
  fi
  if [ -z "$PRE_ID" ] || { [ "$PRE_CODE" != "200" ] && [ "$PRE_CODE" != "201" ]; }; then
    fail "预检（$3）给出可提交的计划" "状态码 $PRE_CODE，响应 $(head -c 300 "$WORK/preflight.json")"
    JOB_ID=""
    JOB_STATE="preflight:$PRE_CODE"
    : >"$WORK/job.json"
    return 0
  fi
  JOB_CODE=$(submit_job "$PRE_ID" "$PRE_PHRASE" "$3$RUN_KEY")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$3.json"
  if [ "$JOB_CODE" = "202" ]; then
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-$3.json"
  else
    JOB_ID=""
    JOB_STATE="submit:$JOB_CODE"
    : >"$WORK/job.json"
    fail "提交（$3）" "状态码 $JOB_CODE，响应 $(head -c 300 "$WORK/submit.json")"
  fi
}
preflight_only() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pallowed)
  PRE_BLOCKERS=$(pall)
}
expect_verified() {
  expect_eq "$1任务终态" "succeeded" "$JOB_STATE"
  expect_eq "$1结果是 verified" "verified" "$(joutcome)"
  expect_eq "$1带 verified_at" "True" "$(jverified)"
}

RUN_KEY="-$(date -u +%Y%m%d%H%M%S)-$$"

# ---- 独立证据（不经过产品的解析器） ----

file_mode() { stat -c '%a' "$1" 2>/dev/null; }
file_owner() { stat -c '%U:%G' "$1" 2>/dev/null; }
file_mtime() { stat -c '%Y' "$1" 2>/dev/null; }
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }
unit_text() { cat "$1" 2>/dev/null; }
is_active() { systemctl is-active "$1" 2>/dev/null | head -1; }
is_enabled() { systemctl is-enabled "$1" 2>/dev/null | head -1; }
unit_prop() { systemctl show -p "$2" --value "$1" 2>/dev/null; }
mounted_at() { findmnt -rno TARGET --mountpoint "$1" 2>/dev/null | head -1; }
# mount_source 问的是“这个路径**落在哪块设备上**”，不是“它自己是不是一个挂载点”：
# --mountpoint 只在参数正好等于挂载点时回答，对卷里的子目录一律返回空，会把
# “数据确实落在卷上”读成假红。--target 找的是包含这个路径的那个挂载。
mount_source() { findmnt -no SOURCE --target "$1" 2>/dev/null | head -1; }

# compose 调用一律带项目名与配置路径：与单元里的那一条逐字同一份 argv。
cps() { docker compose -p "$DEPLOY_NAME" -f "$CONFIG_PATH" ps "$@" 2>&1; }
cps_running() { cps --format '{{.State}}' 2>/dev/null | grep -c '^running$' || true; }
# 删除之后不能再拿 `docker compose -f <配置>` 量容器：那个配置文件正是这一步要删掉的东西，
# compose 会先报一句"文件不存在"，而 cps 把 stderr 并进 stdout——错误行被 grep 数成 1 个容器，
# "删干净了"就这么被判成红（真机 boot 段 §B3）。项目里还剩什么，只能问 docker 自己的标签。
project_running() { docker ps --filter "label=com.docker.compose.project=$DEPLOY_NAME" --format '{{.ID}}' 2>/dev/null | grep -c . || true; }
project_all() { docker ps -a --filter "label=com.docker.compose.project=$DEPLOY_NAME" --format '{{.ID}}' 2>/dev/null | grep -c . || true; }
container_id() { docker compose -p "$DEPLOY_NAME" -f "$CONFIG_PATH" ps -q 2>/dev/null | head -1; }
inspect_field() { docker inspect -f "$2" "$1" 2>/dev/null; }

# diag_deploy 把"这一刻的部署现场"整份落盘。
#
# 为什么需要它：上一轮真机跑里，产品侧读到 0 个容器、脚本侧 compose ps -q 与 docker
# inspect 全空、HTTP 000，而 daemon 的同一个窗口里明明有容器的 exec 记录。只有一行
# FAIL 定不了性——是容器没起来、起来了没在跑、还是查询走的路径坏了，三种情况要查的
# 东西完全不同。四个视角（docker 自己数、compose 自己数、单元状态、事件时间线）
# 排在一起，才能把"哪一刻谁看见了什么"对上。
diag_deploy() {
  local dir="$FACTS/diag"
  mkdir -p "$dir" 2>/dev/null || true
  docker ps -a --no-trunc \
    --format '{{.ID}} {{.Names}} {{.Status}} {{.Label "com.docker.compose.project"}}' \
    >"$dir/docker-ps-a.txt" 2>&1 || true
  cps -a >"$dir/compose-ps-a.txt" 2>&1 || true
  docker compose -p "$DEPLOY_NAME" -f "$CONFIG_PATH" ps -aq >"$dir/compose-ps-aq.txt" 2>&1 || true
  systemctl status "$UNIT" --no-pager -l >"$dir/unit-status.txt" 2>&1 || true
  journalctl -u "$UNIT" --no-pager -n 40 >"$dir/unit-journal.txt" 2>&1 || true
  docker events --since "$(date -u -d '-10 minutes' +%Y-%m-%dT%H:%M:%S)" \
    --until "$(date -u +%Y-%m-%dT%H:%M:%S)" \
    --filter "label=com.docker.compose.project=$DEPLOY_NAME" \
    --format '{{.Time}} {{.Action}} {{.Actor.Attributes.name}}' \
    >"$dir/docker-events.txt" 2>&1 || true
  note "部署现场已落盘：facts/diag/（docker ps -a、compose ps、单元状态、journal、events）"
}

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$1" 2>/dev/null || true; }
wait_http() { # $1 URL $2 期望状态码
  for _ in $(seq 1 60); do
    [ "$(http_code "$1")" = "$2" ] && return 0
    sleep 0.5
  done
  return 1
}

# 系统盘视角：卷没挂上时这个路径上长出了什么（目录不存在也算 0 条）。
#
# 判据的意义在这里：create_host_path 关掉之后，卷缺失时 compose 必须失败，而**不是**
# 替我们建一个空目录再把容器的数据写进系统盘。目录不存在与“目录在但是空的”都算没写，
# 一旦有文件就是 APP-03 那个事故。
appdata_entries() {
  if [ -d "$ROOT_PATH" ]; then
    ls -A "$ROOT_PATH" 2>/dev/null | wc -l | tr -d ' '
  else
    echo 0
  fi
}
appdata_shape() {
  if [ -e "$ROOT_PATH" ]; then echo "存在"; else echo "不存在"; fi
}

# 卷的现场状态（产品自己报的 observed_state）。
volume_observed() { # $1 卷标识
  api "$JAR" "$CSRF" GET "$STORE/volumes/$1" >/dev/null
  jget "$TMPD/resp.json" 'd["data"]["observed_state"]'
}
# 等采集器看到卷的新状态：预检绑定的是采集器的 5 秒快照，刚挂完就问会判“没挂上”。
wait_volume_state() { # $1 卷标识 $2 期望状态
  for _ in $(seq 1 60); do
    [ "$(volume_observed "$1")" = "$2" ] && return 0
    sleep 0.5
  done
  return 1
}

# ---------------------------------------------------------------------------
#                                    M A I N
# ---------------------------------------------------------------------------
run_main() {
  echo "== 0. 环境与起点 =="

  for p in "$AGENT" "$WEB" "$CTL"; do
    if [ -x "$p" ]; then pass "二进制存在：$p"; else echo "缺少 $p（先交叉编译到 $BIN_DIR）" >&2; exit 2; fi
  done
  for tool in python3 curl ss runuser systemctl docker findmnt blkid sgdisk; do
    if command -v "$tool" >/dev/null 2>&1; then pass "夹具：有 $tool"; else
      echo "缺少 $tool —— 环境不满足，不是产品缺陷" >&2; exit 2
    fi
  done
  if docker compose version >/dev/null 2>&1; then
    pass "夹具：docker compose 可用（$(docker compose version --short 2>/dev/null)）"
  else
    echo "docker compose 不可用（需要 compose v2 插件）—— 环境不满足" >&2; exit 2
  fi
  if [ "$(systemctl is-active docker 2>/dev/null)" = "active" ]; then
    pass "夹具：docker 守护进程 active"
  else
    echo "docker 守护进程没有起来 —— 环境不满足" >&2; exit 2
  fi
  if docker image inspect "$IMAGE" >/dev/null 2>&1; then
    pass "夹具：镜像 $IMAGE 已在本地"
  else
    note "本地没有 $IMAGE，尝试拉一次"
    if docker pull "$IMAGE" >>"$WORK/logs/docker-pull.log" 2>&1; then
      pass "夹具：镜像 $IMAGE 拉取成功"
    else
      echo "拉不到 $IMAGE（无外网）—— 环境不满足；这一条不能用别的镜像替代，见 result.md" >&2
      exit 2
    fi
  fi

  # 空盘：整盘、不是系统盘、>= 3 GiB、没有分区表与文件系统（有就按“复用上一轮夹具”处理）。
  if [ ! -b "/dev/$DISK" ]; then echo "/dev/$DISK 不存在 —— 环境不满足" >&2; exit 2; fi
  case "$DISK" in
    sda|vda|nvme0n1) echo "/dev/$DISK 看着是系统盘，拒绝拿它做夹具" >&2; exit 2 ;;
  esac
  # -d：只看盘本身。lsblk 默认连子分区一起列，盘上已经分过区时这里会读到两行，
  # 拼进 [ ] 与 $(( )) 就是 "4294967296\n2147483648: integer expression expected"。
  DISK_SIZE=$(lsblk -bdno SIZE "/dev/$DISK" 2>/dev/null | head -1 | tr -d ' ')
  if [ "${DISK_SIZE:-0}" -lt 3221225472 ]; then
    echo "/dev/$DISK 只有 ${DISK_SIZE:-0} 字节，不足 3 GiB —— 环境不满足" >&2; exit 2
  fi
  pass "夹具盘 /dev/$DISK（$((DISK_SIZE / 1024 / 1024 / 1024)) GiB，不是系统盘）"

  rm -rf "$WORK"
  mkdir -p "$WORK" "$TMPD" "$FACTS" "$PAYLOADS" "$WORK/logs" "$WEB_DB_DIR"
  chmod 0755 "$WORK" "$WORK/logs" "$PAYLOADS" "$FACTS"
  chmod 0700 "$TMPD"
  chown -R "$WEB_USER" "$WEB_DB_DIR"
  rm -f "$RESULT"

  stamp "起点"

  # -------------------------------------------------------------------------
  echo
  echo "== 1. 配置与启动（代理 root、Web 非 root） =="

  install -d -m 0755 /etc/upanel
  install -d -m 0700 "$AGENT_DB_DIR"
  install -d -m 0755 "$RUN_DIR"
  # 起点必须是**没有部署**的机器：上一轮留下的单元会让“新建”变成“重放”。
  # 上一轮如果死在故障注入那一段，mount unit 可能还 mask 着——先解掉，
  # 否则这一轮的每一次挂载都会被自己上一轮的现场挡住。
  systemctl unmask "$MNT_UNIT" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
  # 上一轮的部署单元：**先 stop 再删文件**。只 rm 文件的话，systemd 内存里那个单元仍然
  # 是 active（Type=oneshot + RemainAfterExit），于是“部署前线上没有这个单元在跑”
  # 会读成 active 假红。stop 也顺带跑 ExecStop 的 compose down，把容器收掉。
  for stale_unit in $(systemctl list-units --all --plain --no-legend 'upanel-deploy-*.service' 2>/dev/null | awk '{print $1}'); do
    systemctl stop "$stale_unit" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
    systemctl disable "$stale_unit" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
  done
  rm -f /etc/systemd/system/upanel-deploy-*.service
  rm -rf "$CONFIG_DIR"
  systemctl daemon-reload
  systemctl reset-failed 2>/dev/null || true
  # 代理库清空：卷登记表必须是这张新表（否则“导入卷”会撞上一轮的 slug）。
  rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
  rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"

  # 夹具在盘外的痕迹也要清：卷记录在上面的库里，但**挂载与 fstab 条目是产品写的**，
  # 它们留在库里看不见的地方。不清掉，这一轮的“导入”会撞上一轮的同名 fstab 条目，
  # 而脚本自己已经看不到那条记录（记录就在刚刚清掉的库里）——那会变成一个假失败。
  # 只动本阶段的夹具挂载点：别的卷、别的 fstab 条目一概不碰。
  systemctl stop "$MNT_UNIT" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
  if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
    umount "$MOUNT_POINT" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
  fi
  rm -f "/etc/systemd/system/$MNT_UNIT"
  systemctl daemon-reload >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
  if [ -f /etc/fstab ]; then
    cp /etc/fstab "$WORK/logs/fstab-before-main.txt"
    python3 - "$MOUNT_POINT" <<'PY'
import sys
mp = sys.argv[1]
kept = []
for line in open("/etc/fstab", encoding="utf-8"):
    parts = line.split()
    if len(parts) >= 2 and not line.lstrip().startswith("#") and parts[1] == mp:
        continue
    kept.append(line)
open("/etc/fstab", "w", encoding="utf-8").writelines(kept)
PY
  fi
  # 上一轮如果死在部署成功之后，容器可能还在（单元已经删了，compose 不会自己收）。
  STALE_CIDS=$(docker ps -aq --filter "label=com.docker.compose.project=$DEPLOY_NAME" 2>/dev/null || true)
  if [ -n "$STALE_CIDS" ]; then
    echo "$STALE_CIDS" | xargs -r docker rm -f >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
    note "清掉上一轮剩下的容器：$(echo "$STALE_CIDS" | tr '\n' ' ')"
  fi

  # 第一阶段的配置里**没有** files.roots：卷还没导入，根还不存在。
  # 根只能在卷挂上之后写进配置（agent 启动时核对根的 dev/inode 与挂载点）。
  cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "$WEB_ADDR:$PORT", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "/var/lib/upanel-agent/agent.db",
    "secret_key_path": "/etc/upanel/agent.key",
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {},
    "logs": {},
    "files": { "roots": [] }
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
  chmod 0644 "$SYS_CFG"

  if [ -f "$KEY_FILE" ]; then
    pass "代理密钥文件已存在（沿用）"
  else
    if "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
      pass "-init-key 生成代理密钥"
    else
      fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
    fi
  fi
  if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
  expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

  printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
    --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
  expect_eq "init-admin 退出码" "0" "$?"
  chown_web_db
  if start_web; then pass "Web 在 $BASE 启动"; else
    fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"
    write_result; exit 1
  fi
  LISTEN_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
  [ -z "$LISTEN_PID" ] && LISTEN_PID=$WEB_PID
  WEB_UID=$(ps -o uid= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')
  if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
    pass "监听 $PORT 的 Web 进程不是 root（pid=$LISTEN_PID uid=$WEB_UID）"
  else
    fail "监听 $PORT 的 Web 进程不是 root" "pid=${LISTEN_PID:-未知} uid=${WEB_UID:-未知}"
  fi

  JAR=$WORK/admin.jar
  expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
  CSRF=$(jget "$WORK/login-admin.json" 'd["data"]["csrf_token"]')
  expect_ne "拿到 CSRF 令牌" "" "$CSRF"

  # -------------------------------------------------------------------------
  echo
  echo "== 2. 能力登记：两条部署动作真的在代理的登记表里 =="
  #
  # 读的是**代理**的 /internal/v1/capabilities（unix socket，以 upanel-web 身份）：
  # 那是登记表的原样输出。production 里 docker 不在时部署动作根本不登记——所以这一条
  # 同时是“这台机器上部署功能确实可用”的证据，不是一句自我声明。

  # 响应由 root 的 shell 重定向落盘：这个 socket 只认 upanel-web，但 $FACTS 归 root，
  # 那个用户写不进去——让 curl 自己 -o 到那里会一声不响地什么都不写。
  if runuser -u "$WEB_USER" -- curl -sS --fail --unix-socket "$SOCK" \
      "http://agent/internal/v1/capabilities" >"$FACTS/capabilities.json" \
      2>"$WORK/logs/capabilities.err"; then
    pass "capabilities 可读"
  else
    fail "capabilities 可读" "$(head -c 400 "$WORK/logs/capabilities.err" 2>/dev/null)"
  fi
  CAPS=$(jget "$FACTS/capabilities.json" '",".join(d["data"]["supported_actions"])')
  expect_contains "登记表里有 deployment.apply" "deployment.apply" "$CAPS"
  expect_contains "登记表里有 deployment.remove" "deployment.remove" "$CAPS"
  EXPECTED_QUERIES=$(jget "$FACTS/capabilities.json" '",".join(d["data"]["supported_queries"])')
  expect_not_contains "没有 deployment.* 的查询冒充动作" "deployment.apply" "$EXPECTED_QUERIES"

  # -------------------------------------------------------------------------
  echo
  echo "== 3. 卷夹具：一次性空盘 → GPT → 分区 → ext4 → 导入 → 挂载 =="
  #
  # 用产品动作造夹具而不是 sgdisk/mkfs：卷登记的 UUID、挂载点与 fstab 条目必须由产品
  # 自己写，否则“单元的 mount unit 名”与“卷说自己在哪”会来自两个地方。

  PTTYPE=$(blkid -p -o value -s PTTYPE "/dev/$DISK" 2>/dev/null | head -1)
  PART="/dev/${DISK}1"
  if [ -z "$PTTYPE" ]; then
    run_action storage.partition_table.initialize "{\"device_id\":\"$DISK\",\"table_type\":\"gpt\"}" init-gpt
    expect_verified "初始化 GPT（$DISK）"
    expect_eq "盘上确实有 GPT（blkid 独立读取）" "gpt" \
      "$(blkid -p -o value -s PTTYPE "/dev/$DISK" 2>/dev/null | head -1)"
    sleep 6   # 等拓扑采集器重扫一次（预检绑定的是采集器的快照）
    run_action storage.partition.create \
      "{\"device_id\":\"$DISK\",\"start_bytes\":\"1048576\",\"size_bytes\":\"2147483648\"}" part-main
    expect_verified "建 2 GiB 分区（$DISK）"
    sleep 6
    run_action storage.filesystem.format \
      "{\"device_id\":\"${DISK}1\",\"filesystem\":\"ext4\",\"label\":\"p15vol\"}" fmt-main
    expect_verified "格式化为 ext4（${DISK}1）"
    sleep 6   # 导入的预检要看到这个分区的文件系统
  else
    notrun "初始化 GPT / 建分区 / 格式化" "/dev/$DISK 上已有 $PTTYPE，按上一轮夹具复用（要重跑请换一块空盘：DISK=sdd）"
  fi
  if [ -b "$PART" ]; then pass "夹具分区 $PART 存在"; else fail "夹具分区 $PART 存在" "不存在"; fi
  UUID_FS=$(blkid -p -o value -s UUID "$PART" 2>/dev/null | head -1)
  expect_ne "独立工具读到文件系统 UUID" "" "$UUID_FS"
  expect_eq "独立工具读到 ext4" "ext4" "$(blkid -p -o value -s TYPE "$PART" 2>/dev/null | head -1)"

  # 上一轮留下的同名卷：按 slug 找，找到就复用（fstab 里已经有它的条目）。
  api "$JAR" "$CSRF" GET "$STORE/volumes" >/dev/null
  cp "$TMPD/resp.json" "$WORK/volumes.json"
  VOL_ID=$(jget "$WORK/volumes.json" \
    "[v[\"id\"] for v in d[\"data\"][\"volumes\"] if v[\"slug\"]==\"$SLUG\"][0] if any(v[\"slug\"]==\"$SLUG\" for v in d[\"data\"][\"volumes\"]) else \"\"")
  if [ -n "$VOL_ID" ]; then
    note "复用上一轮登记的卷 $VOL_ID（slug=$SLUG）"
    pass "卷登记表里有 $SLUG"
  else
    run_action storage.volume.import \
      "{\"device_id\":\"${DISK}1\",\"slug\":\"$SLUG\",\"display_name\":\"P15 部署数据卷\"}" import-vol
    expect_verified "导入卷（${DISK}1 → slug=$SLUG）"
    VOL_ID=$(jobdata volume_id)
    expect_contains "卷标识是 vol_ 形状" "vol_" "$VOL_ID"
    expect_eq "导入写进 fstab 的 UUID 与盘上一致（blkid 读回）" "$UUID_FS" "$(jobobserved uuid)"
    expect_eq "导入没有把卷挂上" "" "$(mounted_at "$MOUNT_POINT")"
  fi
  # 挂载点形状要在**导入之后**读：上面那份列表是导入之前取的，新建的卷不在里面，
  # 直接拿它断言会读到空串（首跑就是这么假红的）。
  api "$JAR" "$CSRF" GET "$STORE/volumes/$VOL_ID" >/dev/null
  cp "$TMPD/resp.json" "$WORK/volume.json"
  expect_eq "卷的挂载点由 slug 派生" "$MOUNT_POINT" \
    "$(jget "$WORK/volume.json" 'd["data"]["mount_path"]')"

  # 挂载（用当前 version 做乐观并发令牌）
  VOL_VER=$(jget "$WORK/volume.json" 'd["data"]["version"]')
  VOL_STATE=$(jget "$WORK/volume.json" 'd["data"]["observed_state"]')
  if [ "$VOL_STATE" = "mounted" ]; then
    pass "卷已经在挂载状态（复用）"
  else
    run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" mount-vol
    expect_verified "挂载卷"
    expect_eq "挂载结果里的挂载点" "$MOUNT_POINT" "$(jobobserved mount_path)"
    expect_eq "挂载结果里的 UUID 与盘上一致" "$UUID_FS" "$(jobobserved uuid)"
  fi
  expect_eq "挂载点真的挂着" "$MOUNT_POINT" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "挂的是这块盘（findmnt 的 UUID）" "$UUID_FS" \
    "$(findmnt -no UUID --mountpoint "$MOUNT_POINT" 2>/dev/null | head -1)"
  MNT_UNIT_LOADED=$(systemctl show -p LoadState --value "$MNT_UNIT" 2>/dev/null)
  expect_eq "systemd 认得这个 mount unit" "loaded" "$MNT_UNIT_LOADED"
  if wait_volume_state "$VOL_ID" mounted; then
    pass "采集器看到卷挂上了（部署预检读的是它的快照）"
  else
    fail "采集器看到卷挂上了" "observed_state=$(volume_observed "$VOL_ID")"
  fi

  # 业务目录与 marker：数据要能在卷上落地，删除后要逐字节还在。
  mkdir -p "$ROOT_PATH"
  chmod 0755 "$ROOT_PATH"
  printf 'p15-2 marker %s\n' "$(date -u +%FT%TZ)" >"$ROOT_PATH/marker.txt"
  MARKER_SHA=$(sha_of "$ROOT_PATH/marker.txt")
  expect_eq "marker 落在卷上（不是系统盘）" "/dev/${DISK}1" "$(mount_source "$ROOT_PATH")"

  # -------------------------------------------------------------------------
  echo
  echo "== 4. 文件根登记 + 重启代理（根必须来自卷里的目录） =="
  #
  # 部署的挂载来源只能是“受管理卷里的已登记根”。根写进配置后必须重启代理：
  # agent 启动时会核对根的 dev/inode 与它所在的挂载点，换挂载就是换身份。

  python3 - "$SYS_CFG" "$ROOT_PATH" <<'PY'
import json, sys
path, root = sys.argv[1], sys.argv[2]
cfg = json.load(open(path, encoding="utf-8"))
cfg["agent"]["files"]["roots"] = [
    {"id": "deploydata", "name": "部署数据", "path": root, "writable": True}
]
with open(path, "w", encoding="utf-8") as fh:
    json.dump(cfg, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
PY
  expect_eq "配置权限仍是 0644" "644" "$(file_mode "$SYS_CFG")"

  stop_agent
  if start_agent; then pass "代理带着文件根重启"; else fail "代理带着文件根重启" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
  # 查询总线只有 uid=upanel-web 能连（SO_PEERCRED），所以这里必须降权去问。
  # 请求体直接写在命令行上、响应由 root 的 shell 重定向落盘：$TMPD 是 0700 的
  # root 目录，那个用户读不到里面的文件；$FACTS 归 root，它也写不进去。
  # （上一轮就是这两条一起把 facts/roots.json 变成“文件不存在”的。）
  if runuser -u "$WEB_USER" -- curl -sS --fail --unix-socket "$SOCK" \
      -H 'Content-Type: application/json' --data-raw '{"query":"files.roots","params":{}}' \
      "http://agent/internal/v1/queries" >"$FACTS/roots.json" 2>"$WORK/logs/roots-query.err"; then
    pass "文件根查询返回 200"
  else
    fail "文件根查询返回 200" "$(head -c 400 "$WORK/logs/roots-query.err" 2>/dev/null)"
  fi
  ROOT_AVAIL=$(jget "$FACTS/roots.json" \
    "[str(r.get(\"available\")) for r in d[\"data\"][\"roots\"] if r[\"root_id\"]==\"deploydata\"][0] if any(r[\"root_id\"]==\"deploydata\" for r in d[\"data\"][\"roots\"]) else \"\"")
  expect_eq "文件根 deploydata 可用" "True" "$ROOT_AVAIL"
  expect_eq "文件根指向卷里的目录" "$ROOT_PATH" \
    "$(jget "$FACTS/roots.json" "[r[\"path\"] for r in d[\"data\"][\"roots\"] if r[\"root_id\"]==\"deploydata\"][0] if any(r[\"root_id\"]==\"deploydata\" for r in d[\"data\"][\"roots\"]) else \"\"")"
  expect_eq "文件根所在挂载点就是卷" "$MOUNT_POINT" \
    "$(jget "$FACTS/roots.json" "[r.get(\"mount_point\") or \"\" for r in d[\"data\"][\"roots\"] if r[\"root_id\"]==\"deploydata\"][0] if any(r[\"root_id\"]==\"deploydata\" for r in d[\"data\"][\"roots\"]) else \"\"")"

  # -------------------------------------------------------------------------
  echo
  echo "== 5. 部署：定义 → 渲染 → 单元 → 容器（四条硬读数） =="
  stamp "§5 部署开始"

  build_definition "$WORK/definition.json"
  cp "$WORK/definition.json" "$PAYLOADS/definition.json"
  apply_params "$TMPD/apply-new.json" ""

  PRE_CODE=$(preflight deployment.apply "$(cat "$TMPD/apply-new.json")")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-apply-new.json"
  expect_eq "预检 deployment.apply 状态码" "200" "$PRE_CODE"
  expect_true "预检允许" "$(pallowed)"
  expect_eq "预检没有阻塞项" "" "$(pall)"
  expect_eq "目标显示名" "部署 $DEPLOY_NAME" "$(pstate)"
  expect_eq "身份指纹由部署标识派生" "deployment:$(deployment_id_of "$DEPLOY_NAME")" "$(pidentity)"
  expect_contains "影响里有写单元" "unit_file" "$(pimpact)"
  expect_contains "影响里有写配置" "compose_config" "$(pimpact)"
  expect_contains "影响里有拉容器" "container_start" "$(pimpact)"
  expect_contains "锁住 compose 项目" "compose:$DEPLOY_NAME" "$(plocks)"
  # 锁名形状以产品为准：port:<proto>/<addr>/<port>（同一个端口在 tcp 与 udp、在回环与
  # 网卡地址上是四件不同的事，只按端口号上锁会让能共存的部署互相排队）。
  expect_contains "锁住宿主端口" "port:tcp/127.0.0.1/$HOST_PORT" "$(plocks)"
  expect_contains "锁住依赖卷" "volume:$VOL_ID" "$(plocks)"
  expect_true "部署需要再认证" "$(preauth)"
  expect_eq "部署没有确认短语（短语是关机类动作的事）" "" \
    "$(pget 'd["data"].get("confirmation_phrase") or ""')"

  expect_ne "部署前线上没有这个单元在跑" "active" "$(is_active "$UNIT")"
  expect_eq "部署前配置目录不存在" "" "$(file_mode "$CONFIG_DIR")"

  expect_eq "再认证" "200" "$(reauth)"
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  JOB_CODE=$(submit_job "$PRE_ID" "" "apply-new$RUN_KEY")
  cp "$WORK/submit.json" "$PAYLOADS/submit-apply-new.json"
  expect_eq "提交被受理" "202" "$JOB_CODE"
  JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  JOB_STATE=$(wait_job "$JOB_ID")
  cp "$WORK/job.json" "$PAYLOADS/job-apply-new.json"
  expect_verified "部署"
  DEPLOY_ID=$(jobdata deployment_id)
  CONFIG_DIGEST=$(jobdata config_digest)
  expect_eq "部署标识由名字派生" "$(deployment_id_of "$DEPLOY_NAME")" "$DEPLOY_ID"
  expect_contains "配置摘要是 sha256: 形状" "sha256:" "$CONFIG_DIGEST"
  expect_eq "这条任务写出了两份文件" "True" "$(jobobserved written)"
  expect_eq "第一次不是幂等重放" "False" "$(jobobserved idempotent)"
  # 盘上本来没有这个单元，走的是 start；restart 只在"单元已在盘上、这次又写了新内容"
  # 时才出现（那时 start 对 active 的 oneshot 是空操作，新配置永远读不到）。
  expect_eq "首次部署走 start 而不是 restart" "False" "$(jobobserved unit_restarted)"
  expect_eq "回读：单元 enabled" "True" "$(jobobserved unit_enabled)"
  expect_eq "回读：单元 active" "True" "$(jobobserved unit_active)"
  expect_eq "容器核对执行过" "True" "$(jobobserved containers_checked)"
  expect_eq "项目下有 1 个容器在跑" "1" "$(jobobserved containers_running)"
  # 容器读空的时候，光有一行 FAIL 定不了性（是没起来、起来了没在跑，还是查询路径坏了）。
  # 把这一时刻的四个视角一起落进 facts，下一次复现就能对得上时间线。
  diag_deploy

  # ---- 独立读数：单元文件 ----
  if [ -f "$UNIT_PATH" ]; then pass "单元文件存在：$UNIT_PATH"; else fail "单元文件存在" "不存在"; fi
  expect_eq "单元文件权限" "644" "$(file_mode "$UNIT_PATH")"
  expect_eq "单元文件属主" "root:root" "$(file_owner "$UNIT_PATH")"
  UNIT_BODY=$(unit_text "$UNIT_PATH")
  cp "$UNIT_PATH" "$FACTS/unit.service"
  expect_contains "单元里有 Type=oneshot" "Type=oneshot" "$UNIT_BODY"
  expect_contains "单元里有 RemainAfterExit=yes" "RemainAfterExit=yes" "$UNIT_BODY"
  expect_contains "单元要求 docker.service" "Requires=docker.service" "$UNIT_BODY"
  expect_contains "单元要开机自启" "WantedBy=multi-user.target" "$UNIT_BODY"
  expect_contains "单元逐卷 BindsTo 挂载单元" "BindsTo=$MNT_UNIT" "$UNIT_BODY"
  expect_contains "单元在挂载单元之后启动" "After=$MNT_UNIT" "$UNIT_BODY"
  expect_contains "单元声明 RequiresMountsFor" "RequiresMountsFor=$MOUNT_POINT" "$UNIT_BODY"
  expect_contains "ExecStart 只有一条 compose up" \
    "ExecStart=/usr/bin/docker compose -p $DEPLOY_NAME -f $CONFIG_PATH up -d --remove-orphans --no-build" "$UNIT_BODY"
  expect_not_contains "单元里没有 shell（没有 sh -c）" "/bin/sh" "$UNIT_BODY"

  # ---- 独立读数：渲染配置 ----
  if [ -f "$CONFIG_PATH" ]; then pass "渲染配置存在：$CONFIG_PATH"; else fail "渲染配置存在" "不存在"; fi
  expect_eq "配置目录权限（root 独占）" "700" "$(file_mode "$CONFIG_DIR")"
  expect_eq "配置文件权限（里面有秘密）" "600" "$(file_mode "$CONFIG_PATH")"
  expect_eq "配置文件属主" "root:root" "$(file_owner "$CONFIG_PATH")"
  cp "$CONFIG_PATH" "$FACTS/compose.yaml"
  CFG_BODY=$(cat "$CONFIG_PATH")
  expect_contains "配置里 restart 是字符串 \"no\"" 'restart: "no"' "$CFG_BODY"
  expect_contains "配置里宿主端口" "published: \"$HOST_PORT\"" "$CFG_BODY"
  expect_contains "配置里绑定地址" "host_ip: \"127.0.0.1\"" "$CFG_BODY"
  expect_contains "配置里镜像" "image: \"$IMAGE\"" "$CFG_BODY"
  expect_contains "配置里挂载源是卷里的目录" "source: \"$ROOT_PATH\"" "$CFG_BODY"
  expect_contains "配置里关掉自动建目录" "create_host_path: false" "$CFG_BODY"
  expect_contains "配置里环境变量" '"APP_MODE": "p15-2"' "$CFG_BODY"
  CFG_SHA=$(sha_of "$CONFIG_PATH")
  note "配置文件 sha256=$CFG_SHA（产品报的摘要另算一份，见 job 的 config_digest）"

  # ---- 独立读数：systemd 与容器 ----
  expect_eq "is-enabled 是 enabled" "enabled" "$(is_enabled "$UNIT")"
  expect_eq "is-active 是 active" "active" "$(is_active "$UNIT")"
  expect_eq "is-active 是 active（mount unit）" "active" "$(is_active "$MNT_UNIT")"

  CID=$(container_id)
  expect_ne "compose 项目里有容器" "" "$CID"
  expect_eq "容器在跑" "running" "$(inspect_field "$CID" '{{.State.Status}}')"
  expect_eq "容器没在重启循环里" "0" "$(inspect_field "$CID" '{{.RestartCount}}')"
  expect_eq "容器 restart 策略是 no（运行时读回）" "no" "$(inspect_field "$CID" '{{.HostConfig.RestartPolicy.Name}}')"
  expect_eq "容器内存上限" "134217728" "$(inspect_field "$CID" '{{.HostConfig.Memory}}')"
  expect_eq "容器 CPU 上限（NanoCpus）" "500000000" "$(inspect_field "$CID" '{{.HostConfig.NanoCpus}}')"
  expect_eq "容器的绑定源就是卷里的目录" "$ROOT_PATH:/data:true" \
    "$(inspect_field "$CID" '{{range .Mounts}}{{.Source}}:{{.Destination}}:{{.RW}}{{end}}')"
  expect_eq "compose 项目名就是部署名" "$DEPLOY_NAME" "$(inspect_field "$CID" '{{index .Config.Labels "com.docker.compose.project"}}')"

  if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
    pass "宿主端口 $HOST_PORT 上的容器真的在应答 HTTP 200"
  else
    fail "宿主端口 $HOST_PORT 上的容器应答 HTTP 200" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
  fi
  curl -s --max-time 3 "http://127.0.0.1:$HOST_PORT/" >"$FACTS/http-body.txt" 2>/dev/null
  expect_contains "应答来自 whoami（看见 Host 头）" "Host: 127.0.0.1:$HOST_PORT" \
    "$(cat "$FACTS/http-body.txt" 2>/dev/null)"

  # 容器确实看得见卷上的数据（挂载不是“写了配置但没生效”）。whoami 镜像是 scratch
  # 构建的，里面没有 cat——那就如实说验不了，不拿“宿主机读得到”冒充“容器读得到”。
  if docker exec "$CID" cat /data/marker.txt >"$FACTS/container-marker.txt" 2>/dev/null; then
    expect_eq "容器里读到的 marker 与卷上逐字相同" "$MARKER_SHA" \
      "$(sha_of "$FACTS/container-marker.txt")"
  else
    notrun "容器里读到的 marker" "whoami 是 scratch 镜像，容器内没有 cat/sh；挂载由 docker inspect 的 Source 与 findmnt 交叉核对"
  fi

  UNIT_MTIME=$(file_mtime "$UNIT_PATH")
  CFG_MTIME=$(file_mtime "$CONFIG_PATH")
  CONTAINER_STARTED=$(inspect_field "$CID" '{{.State.StartedAt}}')
  {
    printf 'UNIT_MTIME=%s\n' "$UNIT_MTIME"
    printf 'CFG_MTIME=%s\n' "$CFG_MTIME"
    printf 'CID=%s\n' "$CID"
    printf 'CONTAINER_STARTED=%s\n' "$CONTAINER_STARTED"
  } >>"$WORK/mtimes.env"

  # -------------------------------------------------------------------------
  echo
  echo "== 6. 幂等重放：同一份定义再 apply 一次，不重写、不重启 =="
  stamp "§6 重放开始"

  apply_params "$TMPD/apply-again.json" ""
  PRE_CODE=$(preflight deployment.apply "$(cat "$TMPD/apply-again.json")")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-apply-again.json"
  expect_eq "重放预检状态码" "200" "$PRE_CODE"
  expect_true "重放被允许（同内容不是“更新”）" "$(pallowed)"
  expect_eq "重放的锁与新建一致" "compose:$DEPLOY_NAME" \
    "$(plocks | cut -d, -f1)"
  expect_eq "重放需要再认证" "True" "$(preauth)"
  expect_eq "再认证" "200" "$(reauth)"
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  JOB_CODE=$(submit_job "$PRE_ID" "" "apply-again$RUN_KEY")
  expect_eq "重放提交被受理" "202" "$JOB_CODE"
  JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  JOB_STATE=$(wait_job "$JOB_ID")
  cp "$WORK/job.json" "$PAYLOADS/job-apply-again.json"
  expect_verified "重放"
  expect_eq "重放没有重写文件" "False" "$(jobobserved written)"
  expect_eq "重放被标成幂等" "True" "$(jobobserved idempotent)"
  expect_eq "重放没有重启单元" "False" "$(jobobserved unit_restarted)"
  expect_eq "重放后单元仍是 active" "True" "$(jobobserved unit_active)"
  expect_eq "重放后容器数还是 1" "1" "$(jobobserved containers_running)"

  expect_eq "单元文件 mtime 没变" "$UNIT_MTIME" "$(file_mtime "$UNIT_PATH")"
  expect_eq "配置 mtime 没变" "$CFG_MTIME" "$(file_mtime "$CONFIG_PATH")"
  expect_eq "配置字节没变" "$CFG_SHA" "$(sha_of "$CONFIG_PATH")"
  # "没变"类断言必须先有一条非空前提：两侧都读到空的时候，expect_eq 会以空对空判成
  # PASS——上一轮就是靠这个假绿把"容器根本读不到"演成了"容器没被重启"。
  RE_CID=$(container_id)
  if [ -z "$CID" ] || [ -z "$RE_CID" ]; then
    fail "重放前后都读得到那个容器" "重放前 CID='$CID'，重放后 CID='$RE_CID'（ID 与 StartedAt 的“没变”无从比）"
  else
    expect_eq "容器 ID 没变" "$CID" "$RE_CID"
    expect_eq "容器没有被重启（StartedAt 没变）" "$CONTAINER_STARTED" \
      "$(inspect_field "$RE_CID" '{{.State.StartedAt}}')"
    expect_eq "容器还在跑" "running" "$(inspect_field "$RE_CID" '{{.State.Status}}')"
  fi

  # -------------------------------------------------------------------------
  echo
  echo "== 7. apply 只创建：换定义与带 deployment_id 都被判决层拦住 =="

  # 7a. 带 deployment_id = 要求“更新”→ update_not_implemented（P15.3 的动作）
  apply_params "$TMPD/apply-update.json" "$DEPLOY_ID"
  preflight_only deployment.apply "$(cat "$TMPD/apply-update.json")" apply-update
  expect_eq "带 deployment_id 的预检状态码" "200" "$PRE_CODE"
  expect_false "带 deployment_id 的预检不允许" "$PRE_ALLOWED"
  expect_contains "阻塞码是 update_not_implemented" "update_not_implemented" "$PRE_BLOCKERS"
  expect_contains "阻塞理由指向 P15.3" "deployment.update" "$(pwhy)"

  # 7b. 同名、换端口 → deployment_exists
  build_definition "$TMPD/definition-changed.json" "$((HOST_PORT + 1))"
  python3 - "$TMPD/definition-changed.json" "$TMPD/apply-changed.json" <<'PY'
import json, sys
defn = json.load(open(sys.argv[1], encoding="utf-8"))
json.dump({"deployment_id": None, "version": "", "definition": defn},
          open(sys.argv[2], "w", encoding="utf-8"), ensure_ascii=False)
PY
  preflight_only deployment.apply "$(cat "$TMPD/apply-changed.json")" apply-changed
  expect_eq "换定义的预检状态码" "200" "$PRE_CODE"
  expect_false "换定义的预检不允许" "$PRE_ALLOWED"
  expect_contains "阻塞码是 deployment_exists" "deployment_exists" "$PRE_BLOCKERS"
  expect_eq "被拒的预检没有改动磁盘上的配置" "$CFG_SHA" "$(sha_of "$CONFIG_PATH")"
  expect_eq "被拒的预检没有改动单元" "$UNIT_MTIME" "$(file_mtime "$UNIT_PATH")"
  expect_eq "容器还在跑（没有被“更新”拆掉）" "running" "$(inspect_field "$(container_id)" '{{.State.Status}}')"

  # -------------------------------------------------------------------------
  echo
  echo "== 8. 停止与卷依赖：卷卸下之后这个单元起不来 =="
  stamp "§8 停止与卷依赖开始"

  # 8a. 停止单元：容器必须真的下来了，但数据一个字节都不动。
  systemctl stop "$UNIT" >>"$WORK/logs/systemctl-stop.log" 2>&1
  expect_eq "stop 之后单元不再 active" "inactive" "$(is_active "$UNIT")"
  expect_eq "stop 之后 enabled 仍是 enabled" "enabled" "$(is_enabled "$UNIT")"
  sleep 1
  expect_eq "stop 之后项目里没有运行中的容器" "0" "$(cps_running)"
  expect_eq "stop 之后 marker 还在" "$MARKER_SHA" "$(sha_of "$ROOT_PATH/marker.txt")"
  expect_eq "stop 之后卷还挂着" "$MOUNT_POINT" "$(mounted_at "$MOUNT_POINT")"
  # compose down 之后项目容器应该被清掉（单元用的是 down，不是 stop）
  expect_eq "stop 之后 compose ps 是空的" "0" "$(cps --all --format '{{.ID}}' 2>/dev/null | grep -c . || true)"

  # 8b. 卸卷（产品动作；容器已经停了，卷不 busy）
  api "$JAR" "$CSRF" GET "$STORE/volumes/$VOL_ID" >/dev/null
  cp "$TMPD/resp.json" "$WORK/volume.json"
  VOL_VER=$(jget "$WORK/volume.json" 'd["data"]["version"]')
  run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" unmount-vol
  expect_verified "卸载卷"
  expect_eq "卸载后挂载点为空" "" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "mount unit 不再 active" "inactive" "$(is_active "$MNT_UNIT")"
  if wait_volume_state "$VOL_ID" unmounted; then
    pass "采集器也看到卷卸下了（预检读的是它的快照）"
  else
    fail "采集器也看到卷卸下了" "observed_state=$(volume_observed "$VOL_ID")"
  fi

  # 8c. 让这个挂载在这台机器上无法成为 active（mask），再 start 单元。
  #
  # 为什么要 mask：单元里有 RequiresMountsFor，所以 `systemctl start` 本身会**顺带挂上**
  # 那块卷（这是设计：正常开机时卷跟着单元一起起来）。要验的恰恰是“挂不上的时候会怎样”，
  # 于是必须让那个 mount unit 起不来。mask 不动盘上一个字节、不动 fstab、可原样恢复，
  # 是这里唯一干净的故障注入。
  systemctl mask "$MNT_UNIT" >>"$WORK/logs/mask-runtime.log" 2>&1
  expect_eq "mount unit 被 mask（故障注入）" "masked" \
    "$(systemctl is-enabled "$MNT_UNIT" 2>/dev/null | head -1)"
  if systemctl start "$UNIT" >>"$WORK/logs/systemctl-start-blocked.log" 2>&1; then
    fail "依赖起不来时 systemctl start 必须失败" "start 成功了（BindsTo 没有生效）"
  else
    pass "依赖起不来时 systemctl start 失败（BindsTo 生效）"
  fi
  cp "$WORK/logs/systemctl-start-blocked.log" "$FACTS/start-blocked.log"
  expect_ne "被挡住的单元不是 active" "active" "$(is_active "$UNIT")"
  expect_eq "被挡住时项目里没有容器" "0" "$(cps_running)"
  expect_eq "被挡住时卷仍然没挂上（没有替我们建目录）" "" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "被挡住时 marker 不在系统盘上" "0" "$(appdata_entries)"
  {
    printf 'is-active=%s is-enabled=%s\n' "$(is_active "$UNIT")" "$(is_enabled "$UNIT")"
    printf 'mount-unit=%s\n' "$(systemctl is-enabled "$MNT_UNIT" 2>/dev/null | head -1)"
  } >>"$FACTS/start-blocked.log"

  # 8d. 判决层也看得见：卷没挂上时预检直接给 volume_not_ready
  preflight_only deployment.apply "$(cat "$TMPD/apply-again.json")" apply-volume-missing
  expect_false "卷没挂上时预检不允许" "$PRE_ALLOWED"
  expect_contains "阻塞码是 volume_not_ready" "volume_not_ready" "$PRE_BLOCKERS"
  expect_contains "阻塞理由说清了“没有挂载”" "没有挂载" "$(pwhy)"

  # 8e. 恢复：unmask → 挂回去 → 同一个 start 必须成功，容器与 HTTP 都回来
  systemctl unmask "$MNT_UNIT" >>"$WORK/logs/unmask-runtime.log" 2>&1
  expect_eq "mount unit 解除 mask" "loaded" \
    "$(systemctl show -p LoadState --value "$MNT_UNIT" 2>/dev/null)"
  api "$JAR" "$CSRF" GET "$STORE/volumes/$VOL_ID" >/dev/null
  cp "$TMPD/resp.json" "$WORK/volume.json"
  VOL_VER=$(jget "$WORK/volume.json" 'd["data"]["version"]')
  run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" remount-vol
  expect_verified "重新挂载卷"
  if wait_volume_state "$VOL_ID" mounted; then
    pass "采集器看到卷挂回来了"
  else
    fail "采集器看到卷挂回来了" "observed_state=$(volume_observed "$VOL_ID")"
  fi
  if systemctl start "$UNIT" >>"$WORK/logs/systemctl-start-ok.log" 2>&1; then
    pass "卷挂回来之后 systemctl start 成功"
  else
    fail "卷挂回来之后 systemctl start 成功" "$(cat "$WORK/logs/systemctl-start-ok.log" 2>/dev/null | tail -3)"
  fi
  expect_eq "单元回到 active" "active" "$(is_active "$UNIT")"
  expect_eq "容器回来了" "1" "$(cps_running)"
  if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
    pass "HTTP 200 回来了"
  else
    fail "HTTP 200 回来了" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
  fi
  expect_eq "数据还是那一份" "$MARKER_SHA" "$(sha_of "$ROOT_PATH/marker.txt")"

  # 记录“运行中卸卷”这一轮**不去验**的理由（见文件头 NOT RUN 第 1 条）。
  notrun "运行中卷失联 → systemd 自动停单元" \
    "容器把那块卷 bind 进了自己的 mount namespace，内核里的是 busy；产品拒绝、强行 umount -l 造出的现场不能当读数"

  # -------------------------------------------------------------------------
  echo
  echo "== 9. 准备重启验证：单元停掉、卷卸下、留一张 state.env =="
  #
  # 重启段的判据全在 systemd 手里：单元 enabled、卷不可用 → 开机不该起。
  # 为了把“卷不可用”做成一次**可回滚的故障注入**，这里把 mount unit mask 掉，
  # 而不是删掉 fstab 条目：mask 不碰盘上一个字节，重启段末尾 unmask 就恢复。

  systemctl stop "$UNIT" >>"$WORK/logs/systemctl-stop2.log" 2>&1
  expect_eq "重启前单元已停" "inactive" "$(is_active "$UNIT")"
  api "$JAR" "$CSRF" GET "$STORE/volumes/$VOL_ID" >/dev/null
  cp "$TMPD/resp.json" "$WORK/volume.json"
  VOL_VER=$(jget "$WORK/volume.json" 'd["data"]["version"]')
  run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" unmount-for-reboot
  expect_verified "重启前卸下卷"
  expect_eq "重启前挂载点为空" "" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "重启前系统盘上的同名目录里没有文件" "0" "$(appdata_entries)"
  note "系统盘视角：$ROOT_PATH $(appdata_shape)"
  systemctl mask "$MNT_UNIT" >>"$WORK/logs/mask.log" 2>&1
  expect_eq "mount unit 被 mask（故障注入，重启段末尾恢复）" "masked" \
    "$(systemctl is-enabled "$MNT_UNIT" 2>/dev/null | head -1)"
  expect_eq "mask 之后单元仍然 enabled（挡它的只能是依赖）" "enabled" "$(is_enabled "$UNIT")"

  {
    printf 'VOL_ID=%s\n' "$VOL_ID"
    printf 'VOL_VER=%s\n' "$VOL_VER"
    printf 'DEPLOY_NAME=%s\n' "$DEPLOY_NAME"
    printf 'DEPLOY_ID=%s\n' "$DEPLOY_ID"
    printf 'CONFIG_DIGEST=%s\n' "$CONFIG_DIGEST"
    printf 'CONFIG_PATH=%s\n' "$CONFIG_PATH"
    printf 'UNIT=%s\n' "$UNIT"
    printf 'MNT_UNIT=%s\n' "$MNT_UNIT"
    printf 'MOUNT_POINT=%s\n' "$MOUNT_POINT"
    printf 'ROOT_PATH=%s\n' "$ROOT_PATH"
    printf 'MARKER_SHA=%s\n' "$MARKER_SHA"
    printf 'HOST_PORT=%s\n' "$HOST_PORT"
    printf 'DISK=%s\n' "$DISK"
  } >"$STATE"
  chmod 0600 "$STATE"
  cp "$STATE" "$FACTS/state.env"
  pass "state.env 写好（重启段读它）"

  write_result
  stamp "main 段结束"

  if [ "${REBOOT:-0}" = "1" ]; then
    echo
    echo "== 10. 重启这台 VM（REBOOT=1） =="
    note "重启后跑：sudo BIN_DIR=$BIN_DIR PHASE=boot sh $0"
    sync
    systemctl reboot
    exit 0
  fi

  notrun "重启之后不自启（boot 段）" "REBOOT=0，这一轮没有重启；用 PHASE=boot 单独跑"
  notrun "删除保留数据（boot 段）" "REBOOT=0，这一轮没有重启；用 PHASE=boot 单独跑"
  write_result
}

# 定义 JSON：名字、镜像、端口、环境、挂载、资源与依赖卷。
build_definition() { # $1 输出文件 [$2 宿主端口]
  _port=${2:-$HOST_PORT}
  python3 - "$1" "$DEPLOY_NAME" "$IMAGE" "$_port" "$VOL_ID" <<'PY'
import json, sys
out, name, image, port, vol = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), sys.argv[5]
defn = {
    "name": name,
    "image_reference": image,
    "container_port": 80,
    "host_bind_address": "127.0.0.1",
    "host_port": port,
    "environment": [{"name": "APP_MODE", "value": "p15-2"}],
    "mounts": [{
        "source_root_id": "deploydata",
        "relative_path": "",
        "container_path": "/data",
        "read_only": False,
    }],
    "resource_limits": {"memory_limit_bytes": 134217728, "cpu_limit": "0.5"},
    "volume_dependencies": [vol],
}
with open(out, "w", encoding="utf-8") as fh:
    json.dump(defn, fh, ensure_ascii=False)
PY
}

apply_params() { # $1 输出文件 $2 deployment_id（空 = 新建）
  python3 - "$WORK/definition.json" "$1" "${2:-}" <<'PY'
import json, sys
defn = json.load(open(sys.argv[1], encoding="utf-8"))
dep = sys.argv[3]
params = {"deployment_id": (dep or None), "version": "", "definition": defn}
with open(sys.argv[2], "w", encoding="utf-8") as fh:
    json.dump(params, fh, ensure_ascii=False)
PY
}

# 部署标识 = "dep_" + sha256("compose:"+name) 的前 16 字节（32 位十六进制），
# 与产品（deploy.DeploymentIDForName）同一个派生规则。
deployment_id_of() {
  python3 - "$1" <<'PY'
import hashlib, sys
name = sys.argv[1]
print("dep_" + hashlib.sha256(("compose:" + name).encode()).hexdigest()[:32])
PY
}

# ---------------------------------------------------------------------------
#                                    B O O T
# ---------------------------------------------------------------------------
run_boot() {
  echo "== B0. 重启后的起点（还没有任何产品进程） =="

  if [ ! -f "$STATE" ]; then echo "缺少 $STATE —— 先跑 main 段" >&2; exit 2; fi
  . "$STATE"

  for tool in python3 curl systemctl docker findmnt; do
    if command -v "$tool" >/dev/null 2>&1; then pass "夹具：有 $tool"; else
      echo "缺少 $tool —— 环境不满足" >&2; exit 2
    fi
  done
  UPTIME_S=$(cut -d' ' -f1 /proc/uptime | cut -d. -f1)
  if [ "${UPTIME_S:-0}" -lt 1800 ]; then
    pass "这台机器刚起来（uptime ${UPTIME_S}s）"
  else
    fail "这台机器刚起来" "uptime ${UPTIME_S}s —— 不是重启后的读数，boot 段没有意义"
  fi
  BOOT_ID=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)
  printf 'boot_id=%s uptime=%s\n' "$BOOT_ID" "$UPTIME_S" >"$FACTS/boot.txt"
  expect_eq "代理没有在跑（这一段的判据不来自产品）" "0" \
    "$(pgrep -f "$AGENT" 2>/dev/null | wc -l | tr -d ' ')"
  expect_eq "marker 不在系统盘上（卷没挂）" "" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "系统盘上的同名目录里没有文件" "0" "$(appdata_entries)"

  # -------------------------------------------------------------------------
  echo
  echo "== B1. 开机：单元 enabled 但没有自己起来（STAGES APP-03） =="

  expect_eq "单元仍然 enabled" "enabled" "$(is_enabled "$UNIT")"
  expect_ne "单元没有自己起来" "active" "$(is_active "$UNIT")"
  printf 'is-active=%s is-enabled=%s\n' "$(is_active "$UNIT")" "$(is_enabled "$UNIT")" \
    >>"$FACTS/boot.txt"
  systemctl status "$UNIT" --no-pager -l >"$FACTS/status-after-boot.txt" 2>&1 || true
  journalctl -u "$UNIT" -b --no-pager >"$FACTS/journal-after-boot.txt" 2>&1 || true
  systemctl show -p LoadState,Result --value "$MNT_UNIT" >"$FACTS/mount-after-boot.txt" 2>&1 || true
  JOURNAL=$(cat "$FACTS/journal-after-boot.txt" 2>/dev/null)
  MNT_LOAD=$(systemctl show -p LoadState --value "$MNT_UNIT" 2>/dev/null)
  case "$JOURNAL$(
    cat "$FACTS/status-after-boot.txt" 2>/dev/null)" in
    *"$MNT_UNIT"*|*Dependency*|*dependency*|*masked*) pass "journal/status 指向那个 mount unit 或依赖失败" ;;
    *)
      # 依赖是 masked 的 mount unit 时，systemd 在**事务阶段**就把这次开机排除掉了：
      # 服务单元根本不会被调度，于是它自己的 journal 里一条记录都没有（只有
      # "-- No entries --"）。这不是"没记下来"，而是"没尝试过"，判据要换一副形状——
      # 三条同时成立才作数：这一轮的 journal 为空、单元不是 active、那个依赖仍是 masked。
      # 少任何一条，都不能说它是被依赖挡住的（手动 `systemctl start` 的报错形状另有一段
      # 在 main §8 里钉着：`Unit srv-…mount is masked`）。
      case "$JOURNAL" in
        ""|*"No entries"*)
          if [ "$(is_active "$UNIT")" != "active" ] && [ "$MNT_LOAD" = "masked" ]; then
            pass "journal/status 指向那个 mount unit 或依赖失败（masked 依赖让这次开机在事务阶段就被拒，journal 无条目）"
          else
            fail "journal/status 指向那个 mount unit 或依赖失败" \
              "journal 无条目，但 is-active='$(is_active "$UNIT")'、mount unit LoadState='$MNT_LOAD'，挡不住不等于挡住"
          fi ;;
        *)
          fail "journal/status 指向那个 mount unit 或依赖失败" \
            "$(printf '%s' "$JOURNAL" | head -c 300 | tr '\n' ' ')" ;;
      esac ;;
  esac
  expect_eq "开机后没有这个项目的运行中容器" "" \
    "$(docker ps --filter "label=com.docker.compose.project=$DEPLOY_NAME" --format '{{.ID}}' 2>/dev/null | head -1)"
  expect_eq "开机后系统盘上没有多出数据文件" "0" "$(appdata_entries)"
  printf 'appdata_shape=%s appdata_entries=%s\n' "$(appdata_shape)" "$(appdata_entries)" \
    >>"$FACTS/boot.txt"

  # -------------------------------------------------------------------------
  echo
  echo "== B2. 解除故障注入：卷挂回来，单元就能起来 =="

  systemctl unmask "$MNT_UNIT" >>"$WORK/logs/unmask.log" 2>&1
  expect_eq "mount unit 解除 mask" "loaded" "$(systemctl show -p LoadState --value "$MNT_UNIT" 2>/dev/null)"
  if systemctl start "$MNT_UNIT" >>"$WORK/logs/start-mount.log" 2>&1; then
    pass "挂载单元起来"
  else
    fail "挂载单元起来" "$(tail -3 "$WORK/logs/start-mount.log" 2>/dev/null)"
  fi
  expect_eq "卷挂回挂载点" "$MOUNT_POINT" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "卷上还是那份数据" "$MARKER_SHA" "$(sha_of "$ROOT_PATH/marker.txt")"
  if systemctl start "$UNIT" >>"$WORK/logs/start-unit.log" 2>&1; then
    pass "依赖恢复之后单元起来了"
  else
    fail "依赖恢复之后单元起来了" "$(tail -3 "$WORK/logs/start-unit.log" 2>/dev/null)"
  fi
  expect_eq "单元 active" "active" "$(is_active "$UNIT")"
  CID=$(container_id)
  expect_ne "容器被拉起来了" "" "$CID"
  if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
    pass "HTTP 200"
  else
    fail "HTTP 200" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
  fi
  expect_eq "容器 restart 策略仍是 no" "no" "$(inspect_field "$CID" '{{.HostConfig.RestartPolicy.Name}}')"
  diag_deploy

  # -------------------------------------------------------------------------
  echo
  echo "== B3. 删除部署：单元、配置、容器都没了，数据一个字节不动 =="

  install -d -m 0755 "$RUN_DIR"
  if start_agent; then pass "代理起来了"; else fail "代理起来了" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
  chown_web_db
  if start_web; then pass "Web 起来了"; else
    fail "Web 起来了" "$(tail -5 "$WEB_LOG" 2>/dev/null)"
    write_result; exit 1
  fi
  JAR=$WORK/admin.jar
  rm -f "$JAR"
  expect_eq "管理员登录（重启后重新登录）" "200" "$(login admin "$PASSWORD" "$JAR" login-boot)"
  CSRF=$(jget "$WORK/login-boot.json" 'd["data"]["csrf_token"]')
  expect_ne "拿到 CSRF 令牌" "" "$CSRF"

  remove_params "$TMPD/remove.json"
  preflight_only deployment.remove "$(cat "$TMPD/remove.json")" remove
  expect_eq "删除预检状态码" "200" "$PRE_CODE"
  expect_true "删除预检允许" "$PRE_ALLOWED"
  expect_contains "删除的告警里写明数据保留" "data_kept" "$(pwarn)"
  expect_contains "删除只锁 compose 项目" "compose:$DEPLOY_NAME" "$(plocks)"
  # CONTRACTS §5 的 R 列对 deployment.remove 也是“是”：删除会停掉一整套部署，
  # 与部署一样要重新输一次密码。
  expect_eq "删除也需要再认证（R=是）" "True" "$(preauth)"
  expect_eq "再认证" "200" "$(reauth)"

  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  JOB_CODE=$(submit_job "$PRE_ID" "" "remove$RUN_KEY")
  expect_eq "删除提交被受理" "202" "$JOB_CODE"
  JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  JOB_STATE=$(wait_job "$JOB_ID")
  cp "$WORK/job.json" "$PAYLOADS/job-remove.json"
  expect_verified "删除"
  expect_eq "报告里单元被移除" "True" "$(jobobserved unit_removed)"
  expect_eq "报告里配置被移除" "True" "$(jobobserved config_removed)"

  expect_eq "单元文件不存在了" "" "$(file_mode "$UNIT_PATH")"
  expect_eq "渲染配置不存在了" "" "$(file_mode "$CONFIG_PATH")"
  expect_eq "systemd 不再认得这个单元" "not-found" "$(systemctl show -p LoadState --value "$UNIT" 2>/dev/null)"
  expect_ne "单元不是 active" "active" "$(is_active "$UNIT")"
  sleep 1
  expect_eq "项目里没有运行中的容器" "0" "$(project_running)"
  expect_eq "项目里连停止的容器都没有" "0" "$(project_all)"

  expect_eq "数据目录还在" "True" "$([ -d "$ROOT_PATH" ] && echo True || echo False)"
  expect_eq "marker 逐字节还在（删除只动单元与配置）" "$MARKER_SHA" "$(sha_of "$ROOT_PATH/marker.txt")"
  expect_eq "卷还挂在原地" "$MOUNT_POINT" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "卷上的文件数没变" "1" "$(ls -A "$ROOT_PATH" | wc -l | tr -d ' ')"
  ls -la "$ROOT_PATH" >"$FACTS/appdata-after-remove.txt" 2>&1

  # 删除之后再删一次：判决层给 no_such_deployment，而不是 500。
  preflight_only deployment.remove "$(cat "$TMPD/remove.json")" remove-again
  expect_false "重复删除的预检不允许" "$PRE_ALLOWED"
  expect_contains "重复删除的阻塞码" "no_such_deployment" "$PRE_BLOCKERS"

  stamp "boot 段结束"
  write_result
}

remove_params() { # $1 输出文件
  python3 - "$1" "$DEPLOY_ID" "$CONFIG_DIGEST" <<'PY'
import json, sys
json.dump({"deployment_id": sys.argv[2], "version": sys.argv[3]},
          open(sys.argv[1], "w", encoding="utf-8"), ensure_ascii=False)
PY
}

# ---------------------------------------------------------------------------
case "$PHASE" in
  main) run_main ;;
  boot) run_boot ;;
  *) echo "PHASE 只能是 main 或 boot（收到 $PHASE）" >&2; exit 2 ;;
esac

write_result
if [ "$FAILED" -gt 0 ]; then exit 1; fi
exit 0
