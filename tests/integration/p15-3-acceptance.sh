#!/bin/sh
# P15.3 Linux 验收脚本：deployment.update、镜像 digest 记录、按选择备份、健康探测与回切。
#
# 运行位置：Ubuntu 22.04 验收 VM 内，**必须以 root 运行**。这一轮会碰系统盘上的
# /etc/systemd/system、/etc/upanel/deploy，会碰一块**一次性**空盘（GPT→分区→ext4→卷），
# 会起真实容器、真的跑一次 restic 备份、真的把应用回切一次。
#
# 用法（单段，不需要重启）：
#   sudo BIN_DIR=/var/tmp/upanel-bin DISK=sdd \
#        setsid nohup sh tests/integration/p15-3-acceptance.sh >/var/tmp/p153-run.log 2>&1 &
#   轮询 /var/tmp/upanel-accept-p15-3/RESULT
#
# **必须后台起**：这一轮要启动/停止代理与 Web、要调 systemctl 起停单元、要跑 restic，
# SSH 的 pipe 一抖就 SIGPIPE 掉整轮。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。NOT RUN 是按设计列出的范围外项，单独
# 计数、不影响退出码——与 P14.x / P15.2 一致。
#
# ---------------------------------------------------------------- 这一轮验什么
#
# STAGES §17 对 P15.3 的要求是「deployment.update（拉镜像并记录实际 digest）、按选择备份、
# 启动后健康探测与失败可见、回切边界」。四条都要有**真机读数**，不能拿单测折算：
#
#   A. **部署记录落盘**（§6）：apply 之后 /etc/upanel/deploy/webapp.deployment.json 存在、
#      0640、root:root，schema=1，definition.name 是部署名，image.requested 是定义里写的
#      镜像，image.digest 是 sha256: 形状，config_digest 等于任务报的版本；`.prev` 这时
#      还不存在（还没有过一次更新）。记录里的 digest 要与 `docker image inspect` 的 .Id
#      **逐字相同**——这是"记录了实际 digest"的唯一硬判据。
#   B. **正常更新**（§7）：预检有 image_pull / unit_restart / health_probe_rollback /
#      deployment_record 四种影响，锁住 compose 项目、宿主端口与依赖卷，StateVersion 是
#      盘上配置的摘要；执行后 config_changed=true、unit_restarted=true、
#      previous_config_digest 是旧摘要、config_digest 是新摘要、image 是新的、
#      image_digest_known=true、health_checked=true 且 health_ok=true。独立读数：`.prev`
#      0600 且内容就是**旧的**字节（sha256 与更新前一致）、盘上配置是新的、容器真的换了
#      镜像、宿主端口 HTTP 200。
#   C. **同镜像重放**（§8）：config_changed=false、idempotent=true、预检告警里有
#      `config_unchanged`，但 unit_restarted 仍是 true（对 active 的 oneshot，`start` 是
#      空操作，Q485）。
#   D. **健康探测失败 → 回切**（§9）：把镜像换成一个起来就退出的（busybox），探不通 →
#      结果是 rolled_back（不是 failed）、error 为空、recovery_hint 说清新版本为什么没成；
#      observed 里 rolled_back=true、rollback_verified=true、rollback_health_ok=true。
#      独立读数：盘上配置回到**旧的**字节（sha256 与更新前一致）、容器回到旧镜像并在跑、
#      宿主端口 HTTP 200 又回来了、部署记录里的 image 仍是旧的那个（回切不改记录）。
#   E. **按选择备份**（§10）：带 backup_policy_id 的更新，预检多出
#      backup_before_update 影响与 backup-policy:/backup-repo: 两把锁、告警里有
#      backup_before_update；执行后 observed 有 backup_policy_id / backup_repository_id /
#      backup_snapshot_id / backup_verified=true，且 unit_stopped=true（先停再备）。
#      独立读数：用脚本自己的 restic 客户端列出仓库里的快照，那个 snapshot id 真的在。
#   F. **拒绝：只收 files_only**（§11）：stop_managed_apps 策略在预检被拒
#      （backup_policy_needs_apps），而且没有任何东西被改动。
#   G. **拒绝：版本与记录**（§12a/§12b）：version 与盘上摘要不符 → stale；手改盘上配置 →
#      record_mismatch（更新会覆盖别人的改动）；删掉记录 → no_definition_record。改回之后
#      一切照常——这三条不是"坏掉了"，是判据在工作。
#   H. **拒绝：拉不到的镜像**（§12c）：定义里钉一个注册表里不存在的 digest，执行在 pull
#      这一步失败；盘上配置与 `.prev` 一个字节都不变（pull 在写盘之前）。
#   I. **apply 仍然是创建语义**（§12d）：同名但换了端口的定义被拒（deployment_exists），
#      并指路到 deployment.update——update 没有把 apply 变成别的东西。
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * **image_digest_mismatch（定义里钉的 digest 与拉回来的不一致）**：造不出来。注册表
#     按 digest 寻址，钉一个不存在的 digest 会在 `docker pull` 就失败（manifest unknown），
#     走不到后面那一步核对；要造这条现场得自己假装一个注册表并在握手后换字节。只有单测
#     读数（`TestExecuteUpdateRejectsDigestMismatch`）。
#   * **回切之后的数据一致性**：这一版明确不做（CONTRACTS §8.8（4）），也不假装做。
#   * **备份超预算（360 秒）**：要造它得让一次 restic 备份真的跑过六分钟，而验收机上的
#     数据量不到一秒。只有单测读数。
#   * **`stop_managed_apps` 策略真的把应用停下来再备份**：`deployment.update` 根本不接受
#     这条口径（§10 验的是"被拒"），它属于 backup.run。
#   * **24.04（VM-B）**：这一轮只跑 22.04；跨发行版矩阵在 P16 的整机矩阵里做。
#   * **浏览器里的更新界面**：P15.4 的范围，这台 VM 上也没有浏览器。

set -u

umask 077

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p15-3}
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
PORT=${PORT:-19492}
WEB_ADDR=127.0.0.1
BASE="http://$WEB_ADDR:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-p153-admin-57'
RESULT=$WORK/RESULT

# 部署本体。名字同时是 compose 项目名、容器名前缀与单元名的一部分。
DEPLOY_NAME=webapp
HOST_PORT=${HOST_PORT:-18088}
# 三个镜像各管一段：
#   IMAGE_BASE  建站用（apply 之后跑的就是它）
#   IMAGE_NEXT  正常更新的目标：nginx，对 / 回 200
#   IMAGE_BAD   回切用：busybox 起来就退出，端口永远不开
IMAGE_BASE=${IMAGE_BASE:-traefik/whoami:v1.10.3}
IMAGE_NEXT=${IMAGE_NEXT:-nginx:1.27-alpine}
IMAGE_BAD=${IMAGE_BAD:-busybox:1.36.1}
UNIT=upanel-deploy-$DEPLOY_NAME.service
CONFIG_DIR=/etc/upanel/deploy
CONFIG_PATH=$CONFIG_DIR/$DEPLOY_NAME.compose.yaml
PREV_PATH=$CONFIG_PATH.prev
RECORD_PATH=$CONFIG_DIR/$DEPLOY_NAME.deployment.json
UNIT_PATH=/etc/systemd/system/$UNIT

# 卷夹具：一块一次性空盘（默认 sdd）。挂载点由产品按 slug 派生。
DISK=${DISK:-sdd}
SLUG=p15data
VOLUME_ROOT=/srv/upanel/volumes
MOUNT_POINT=$VOLUME_ROOT/$SLUG
MNT_UNIT=srv-upanel-volumes-$SLUG.mount
ROOT_PATH=$MOUNT_POINT/appdata
STORE_PATH=$MOUNT_POINT/backupstore
REPO_REL=repo1
REPO_PATH=$STORE_PATH/$REPO_REL
REPO_PASSWORD='p153-repo-pass-77'

STORE=/api/v1/storage
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

JAR=""
CSRF=""

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
expect_empty() { if [ -z "$2" ]; then pass "$1"; else fail "$1" "期望空，实际 '$2'"; fi; }
expect_nonempty() { if [ -n "$2" ]; then pass "$1"; else fail "$1" "期望非空，实际是空的"; fi; }
# expect_falsy：值不是 True 就算过。
#
# 存在的理由：探针没跑起来时 `health_ok` 这个键**根本不出现**（deployRecordHealth 只在
# checked=true 时写它），jget 于是返回 ERR:KeyError。"没探过"与"探了不通"在"新版本没
# 通过验收"这件事上是同一个结论，不该因为实现走的是哪条分支而红。
expect_falsy() {
  case "$2" in
    True) fail "$1" "不该是 True" ;;
    *) pass "$1" ;;
  esac
}

write_result() {
  {
    printf 'PHASE=%s\n' "main"
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
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}; _name=${6:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$TMPD/req.json"
    _code=$(curl -s -b "$_jar" -o "$TMPD/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
      -d @"$TMPD/req.json")
  else
    _code=$(curl -s -b "$_jar" -o "$TMPD/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf")
  fi
  if [ -n "$_name" ]; then cp "$TMPD/resp.json" "$PAYLOADS/$_name.json" 2>/dev/null; fi
  printf '%s' "$_code"
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

# web_curl：以 upanel-web 身份走代理的 unix socket（SO_PEERCRED 只认这个 uid）。
web_curl() {
  _m=$1; _p=$2; _o=$3; _b=${4:-}
  _tmp=$TMPD/out.json
  rm -f "$_tmp"
  if [ -n "$_b" ]; then
    _code=$(runuser -u "$WEB_USER" -- curl -s -o "$_tmp" -w '%{http_code}' -X "$_m" \
      --unix-socket "$SOCK" -H 'Content-Type: application/json' \
      --data-binary "@$_b" "http://agent$_p" 2>/dev/null)
  else
    _code=$(runuser -u "$WEB_USER" -- curl -s -o "$_tmp" -w '%{http_code}' -X "$_m" \
      --unix-socket "$SOCK" "http://agent$_p" 2>/dev/null)
  fi
  if [ -f "$_tmp" ]; then cp "$_tmp" "$_o" 2>/dev/null; fi
  [ -f "$_o" ] || : >"$_o"
  printf '%s' "$_code"
}

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
  # 3000 × 0.2s ≈ 10 分钟：更新的总预算就是 9 分钟，轮询窗口必须比它宽，
  # 否则一次"备份 + 拉镜像 + 回切"会被脚本自己判成 timeout。
  while [ "$_i" -lt 3000 ]; do
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
jobdata() { jobget "(d[\"data\"].get(\"result\") or {}).get(\"data\",{}).get(\"$1\")"; }
joutcome() { jobget "(d[\"data\"].get(\"result\") or {}).get(\"outcome\")"; }
jverified() { jobget "bool((d[\"data\"].get(\"result\") or {}).get(\"verified_at\"))"; }
jerrmsg() { jobget "(d[\"data\"].get(\"error\") or {}).get(\"message\") or \"\""; }
jerrcode() { jobget "(d[\"data\"].get(\"error\") or {}).get(\"code\") or \"\""; }
jhint() { jobget "(d[\"data\"].get(\"result\") or {}).get(\"recovery_hint\") or \"\""; }

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
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }
is_active() { systemctl is-active "$1" 2>/dev/null | head -1; }
is_enabled() { systemctl is-enabled "$1" 2>/dev/null | head -1; }
mounted_at() { findmnt -rno TARGET --mountpoint "$1" 2>/dev/null | head -1; }
mount_source() { findmnt -no SOURCE --target "$1" 2>/dev/null | head -1; }

cps_running() {
  docker compose -p "$DEPLOY_NAME" -f "$CONFIG_PATH" ps --format '{{.State}}' 2>/dev/null \
    | grep -c '^running$' || true
}
project_running() {
  docker ps --filter "label=com.docker.compose.project=$DEPLOY_NAME" --format '{{.ID}}' 2>/dev/null \
    | grep -c . || true
}
container_id() { docker compose -p "$DEPLOY_NAME" -f "$CONFIG_PATH" ps -q 2>/dev/null | head -1; }
inspect_field() { docker inspect -f "$2" "$1" 2>/dev/null; }
image_id_of() { docker image inspect -f '{{.Id}}' "$1" 2>/dev/null; }
http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$1" 2>/dev/null || true; }
wait_http() { # $1 URL $2 期望状态码
  for _ in $(seq 1 60); do
    [ "$(http_code "$1")" = "$2" ] && return 0
    sleep 0.5
  done
  return 1
}

# restic_q <参数…>：口令走环境变量，不进 argv。
restic_q() { RESTIC_PASSWORD="$REPO_PASSWORD" restic -r "$REPO_PATH" --no-cache "$@" 2>&1; }

# diag_deploy 把"这一刻的部署现场"整份落盘。
#
# 为什么需要它：一行 FAIL 定不了性——是容器没起来、起来了没在跑、还是查询走的路径坏了，
# 三种情况要查的东西完全不同。四个视角（docker 自己数、compose 自己数、单元状态、
# 事件时间线）排在一起，才能把"哪一刻谁看见了什么"对上。
diag_deploy() {
  local dir="$FACTS/diag-$1"
  mkdir -p "$dir" 2>/dev/null || true
  docker ps -a --no-trunc \
    --format '{{.ID}} {{.Names}} {{.Status}} {{.Image}} {{.Label "com.docker.compose.project"}}' \
    >"$dir/docker-ps-a.txt" 2>&1 || true
  docker compose -p "$DEPLOY_NAME" -f "$CONFIG_PATH" ps -a >"$dir/compose-ps-a.txt" 2>&1 || true
  systemctl status "$UNIT" --no-pager -l >"$dir/unit-status.txt" 2>&1 || true
  journalctl -u "$UNIT" --no-pager -n 40 >"$dir/unit-journal.txt" 2>&1 || true
  docker events --since "$(date -u -d '-10 minutes' +%Y-%m-%dT%H:%M:%S)" \
    --until "$(date -u +%Y-%m-%dT%H:%M:%S)" \
    --filter "label=com.docker.compose.project=$DEPLOY_NAME" \
    --format '{{.Time}} {{.Action}} {{.Actor.Attributes.name}} {{.Actor.Attributes.image}}' \
    >"$dir/docker-events.txt" 2>&1 || true
  note "部署现场已落盘：facts/diag-$1/"
}

# 记录文件的字段（读 JSON，不猜格式）。
record_field() { jget "$RECORD_PATH" "$1"; }

# ---------------------------------------------------------------------------
#                                    M A I N
# ---------------------------------------------------------------------------
run_main() {
  echo "== 0. 环境与起点 =="

  # 工作目录先建出来：下面每一个检查都会往 logs/ 里写东西，路径不存在时重定向失败
  # 会让"拉镜像失败"和"目录不存在"看起来是同一件事。
  rm -rf "$WORK"
  mkdir -p "$WORK" "$TMPD" "$FACTS" "$PAYLOADS" "$WORK/logs" "$WEB_DB_DIR"
  chmod 0755 "$WORK" "$WORK/logs" "$PAYLOADS" "$FACTS"
  # 1777 而不是 0700：web_curl 以 upanel-web 身份把响应写在这里（请求体也由它读），
  # 那个用户既进不来也写不进去的话，$SEC_CODE 会变成 000 而原因藏在 stderr 里。
  chmod 1777 "$TMPD"
  chown -R "$WEB_USER" "$WEB_DB_DIR"
  rm -f "$RESULT"

  for p in "$AGENT" "$WEB" "$CTL"; do
    if [ -x "$p" ]; then pass "二进制存在：$p"; else echo "缺少 $p（先交叉编译到 $BIN_DIR）" >&2; exit 2; fi
  done
  for tool in python3 curl ss runuser systemctl docker findmnt blkid sgdisk restic openssl; do
    if command -v "$tool" >/dev/null 2>&1; then pass "夹具：有 $tool"; else
      echo "缺少 $tool —— 环境不满足，不是产品缺陷" >&2; exit 2
    fi
  done
  note "restic $(restic version 2>&1 | head -1)"
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
  # 三个镜像都要在本地或者能拉到：这一轮不许用"拉不到"当借口改口径。
  for img in "$IMAGE_BASE" "$IMAGE_NEXT" "$IMAGE_BAD"; do
    if docker image inspect "$img" >/dev/null 2>&1; then
      pass "夹具：镜像 $img 已在本地"
    else
      note "本地没有 $img，拉一次"
      if docker pull "$img" >>"$WORK/logs/docker-pull.log" 2>&1; then
        pass "夹具：镜像 $img 拉取成功"
      else
        echo "拉不到 $img（无外网）—— 环境不满足，见 logs/docker-pull.log" >&2; exit 2
      fi
    fi
  done

  if [ ! -b "/dev/$DISK" ]; then echo "/dev/$DISK 不存在 —— 环境不满足" >&2; exit 2; fi
  case "$DISK" in
    sda|vda|nvme0n1) echo "/dev/$DISK 看着是系统盘，拒绝拿它做夹具" >&2; exit 2 ;;
  esac
  DISK_SIZE=$(lsblk -bdno SIZE "/dev/$DISK" 2>/dev/null | head -1 | tr -d ' ')
  if [ "${DISK_SIZE:-0}" -lt 3221225472 ]; then
    echo "/dev/$DISK 只有 ${DISK_SIZE:-0} 字节，不足 3 GiB —— 环境不满足" >&2; exit 2
  fi
  pass "夹具盘 /dev/$DISK（$((DISK_SIZE / 1024 / 1024 / 1024)) GiB，不是系统盘）"

  stamp "起点"

  # -------------------------------------------------------------------------
  echo
  echo "== 1. 配置与启动（代理 root、Web 非 root） =="

  install -d -m 0755 /etc/upanel
  install -d -m 0700 "$AGENT_DB_DIR"
  install -d -m 0755 "$RUN_DIR"
  # 起点必须是**没有部署**的机器：上一轮留下的单元会让"新建"变成"重放"。
  systemctl unmask "$MNT_UNIT" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
  for stale_unit in $(systemctl list-units --all --plain --no-legend 'upanel-deploy-*.service' 2>/dev/null | awk '{print $1}'); do
    systemctl stop "$stale_unit" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
    systemctl disable "$stale_unit" >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
  done
  rm -f /etc/systemd/system/upanel-deploy-*.service
  rm -rf "$CONFIG_DIR"
  systemctl daemon-reload
  systemctl reset-failed 2>/dev/null || true
  rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
  rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"

  # 夹具在盘外的痕迹：挂载与 fstab 条目是产品写的，清库之后它们还在。
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
  STALE_CIDS=$(docker ps -aq --filter "label=com.docker.compose.project=$DEPLOY_NAME" 2>/dev/null || true)
  if [ -n "$STALE_CIDS" ]; then
    echo "$STALE_CIDS" | xargs -r docker rm -f >>"$WORK/logs/stale-cleanup.log" 2>&1 || true
    note "清掉上一轮剩下的容器：$(echo "$STALE_CIDS" | tr '\n' ' ')"
  fi

  # 第一阶段的配置里**没有** files.roots：卷还没导入，根还不存在。
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
  echo "== 2. 能力登记：deployment.update 真的在代理的登记表里 =="

  if runuser -u "$WEB_USER" -- curl -sS --fail --unix-socket "$SOCK" \
      "http://agent/internal/v1/capabilities" >"$FACTS/capabilities.json" \
      2>"$WORK/logs/capabilities.err"; then
    pass "capabilities 可读"
  else
    fail "capabilities 可读" "$(head -c 400 "$WORK/logs/capabilities.err" 2>/dev/null)"
  fi
  CAPS=$(jget "$FACTS/capabilities.json" '",".join(d["data"]["supported_actions"])')
  expect_contains "登记表里有 deployment.apply" "deployment.apply" "$CAPS"
  expect_contains "登记表里有 deployment.update" "deployment.update" "$CAPS"
  expect_contains "登记表里有 deployment.remove" "deployment.remove" "$CAPS"

  # -------------------------------------------------------------------------
  echo
  echo "== 3. 卷夹具：一次性空盘 → GPT → 分区 → ext4 → 导入 → 挂载 =="
  #
  # 用产品动作造夹具而不是 sgdisk/mkfs：卷登记的 UUID、挂载点与 fstab 条目必须由产品
  # 自己写，否则"单元的 mount unit 名"与"卷说自己在哪"会来自两个地方。

  PTTYPE=$(blkid -p -o value -s PTTYPE "/dev/$DISK" 2>/dev/null | head -1)
  PART="/dev/${DISK}1"
  if [ -z "$PTTYPE" ]; then
    run_action storage.partition_table.initialize "{\"device_id\":\"$DISK\",\"table_type\":\"gpt\"}" init-gpt
    expect_verified "初始化 GPT（$DISK）"
    sleep 6
    run_action storage.partition.create \
      "{\"device_id\":\"$DISK\",\"start_bytes\":\"1048576\",\"size_bytes\":\"2147483648\"}" part-main
    expect_verified "建 2 GiB 分区（$DISK）"
    sleep 6
    run_action storage.filesystem.format \
      "{\"device_id\":\"${DISK}1\",\"filesystem\":\"ext4\",\"label\":\"p15vol\"}" fmt-main
    expect_verified "格式化为 ext4（${DISK}1）"
    sleep 6
  else
    notrun "初始化 GPT / 建分区 / 格式化" "/dev/$DISK 上已有 $PTTYPE，按上一轮夹具复用（要重跑请换一块空盘）"
  fi
  if [ -b "$PART" ]; then pass "夹具分区 $PART 存在"; else fail "夹具分区 $PART 存在" "不存在"; fi
  UUID_FS=$(blkid -p -o value -s UUID "$PART" 2>/dev/null | head -1)
  expect_ne "独立工具读到文件系统 UUID" "" "$UUID_FS"

  api "$JAR" "$CSRF" GET "$STORE/volumes" >/dev/null
  cp "$TMPD/resp.json" "$WORK/volumes.json"
  VOL_ID=$(jget "$WORK/volumes.json" \
    "[v[\"id\"] for v in d[\"data\"][\"volumes\"] if v[\"slug\"]==\"$SLUG\"][0] if any(v[\"slug\"]==\"$SLUG\" for v in d[\"data\"][\"volumes\"]) else \"\"")
  if [ -n "$VOL_ID" ]; then
    note "复用上一轮登记的卷 $VOL_ID（slug=$SLUG）"
  else
    run_action storage.volume.import \
      "{\"device_id\":\"${DISK}1\",\"slug\":\"$SLUG\",\"display_name\":\"P15 部署数据卷\"}" import-vol
    expect_verified "导入卷（${DISK}1 → slug=$SLUG）"
    VOL_ID=$(jobdata volume_id)
    expect_contains "卷标识是 vol_ 形状" "vol_" "$VOL_ID"
  fi
  api "$JAR" "$CSRF" GET "$STORE/volumes/$VOL_ID" >/dev/null
  cp "$TMPD/resp.json" "$WORK/volume.json"
  expect_eq "卷的挂载点由 slug 派生" "$MOUNT_POINT" "$(jget "$WORK/volume.json" 'd["data"]["mount_path"]')"
  VOL_VER=$(jget "$WORK/volume.json" 'd["data"]["version"]')
  VOL_STATE=$(jget "$WORK/volume.json" 'd["data"]["observed_state"]')
  if [ "$VOL_STATE" = "mounted" ]; then
    pass "卷已经在挂载状态（复用）"
  else
    run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" mount-vol
    expect_verified "挂载卷"
  fi
  expect_eq "挂载点真的挂着" "$MOUNT_POINT" "$(mounted_at "$MOUNT_POINT")"
  expect_eq "挂的是这块盘（findmnt 的 UUID）" "$UUID_FS" \
    "$(findmnt -no UUID --mountpoint "$MOUNT_POINT" 2>/dev/null | head -1)"

  # 两个根：部署数据（容器挂它）与仓库存储（restic 写它）。
  #
  # 为什么分成两个：仓库不能落在被备份的目录里面——restic 备份自己那份仓库，快照会
  # 一边写一边被读，读到的是跨版本的半成品。两个根都在同一块卷上，但互不包含。
  #
  # 先把卷根清空：这是一块一次性夹具盘，上一轮（P15.2）留下的数据混进快照会让
  # "备份里有什么"变成一件说不清的事。
  find "$MOUNT_POINT" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
  mkdir -p "$ROOT_PATH" "$STORE_PATH"
  chmod 0755 "$ROOT_PATH" "$STORE_PATH"
  printf 'p15-3 marker %s\n' "$(date -u +%FT%TZ)" >"$ROOT_PATH/marker.txt"
  MARKER_SHA=$(sha_of "$ROOT_PATH/marker.txt")
  expect_eq "marker 落在卷上（不是系统盘）" "/dev/${DISK}1" "$(mount_source "$ROOT_PATH")"

  # -------------------------------------------------------------------------
  echo
  echo "== 4. 文件根登记 + 重启代理 =="

  python3 - "$SYS_CFG" "$ROOT_PATH" "$STORE_PATH" <<'PY'
import json, sys
path, root, store = sys.argv[1], sys.argv[2], sys.argv[3]
cfg = json.load(open(path, encoding="utf-8"))
cfg["agent"]["files"]["roots"] = [
    {"id": "deploydata", "name": "部署数据", "path": root, "writable": True},
    {"id": "backupstore", "name": "仓库存储", "path": store, "writable": True},
]
with open(path, "w", encoding="utf-8") as fh:
    json.dump(cfg, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
PY
  expect_eq "配置权限仍是 0644" "644" "$(file_mode "$SYS_CFG")"

  stop_agent
  if start_agent; then pass "代理带着两个文件根重启"; else fail "代理带着文件根重启" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
  if runuser -u "$WEB_USER" -- curl -sS --fail --unix-socket "$SOCK" \
      -H 'Content-Type: application/json' --data-raw '{"query":"files.roots","params":{}}' \
      "http://agent/internal/v1/queries" >"$FACTS/roots.json" 2>"$WORK/logs/roots-query.err"; then
    pass "文件根查询返回 200"
  else
    fail "文件根查询返回 200" "$(head -c 400 "$WORK/logs/roots-query.err" 2>/dev/null)"
  fi
  for rid in deploydata backupstore; do
    expect_eq "文件根 $rid 可用" "True" \
      "$(jget "$FACTS/roots.json" "[str(r.get(\"available\")) for r in d[\"data\"][\"roots\"] if r[\"root_id\"]==\"$rid\"][0] if any(r[\"root_id\"]==\"$rid\" for r in d[\"data\"][\"roots\"]) else \"\"")"
    expect_eq "文件根 $rid 所在挂载点就是卷" "$MOUNT_POINT" \
      "$(jget "$FACTS/roots.json" "[r.get(\"mount_point\") or \"\" for r in d[\"data\"][\"roots\"] if r[\"root_id\"]==\"$rid\"][0] if any(r[\"root_id\"]==\"$rid\" for r in d[\"data\"][\"roots\"]) else \"\"")"
  done
  expect_eq "部署数据根指向卷里的 appdata" "$ROOT_PATH" \
    "$(jget "$FACTS/roots.json" "[r[\"path\"] for r in d[\"data\"][\"roots\"] if r[\"root_id\"]==\"deploydata\"][0] if any(r[\"root_id\"]==\"deploydata\" for r in d[\"data\"][\"roots\"]) else \"\"")"
  expect_eq "仓库根与部署数据根不重叠" "$STORE_PATH" \
    "$(jget "$FACTS/roots.json" "[r[\"path\"] for r in d[\"data\"][\"roots\"] if r[\"root_id\"]==\"backupstore\"][0] if any(r[\"root_id\"]==\"backupstore\" for r in d[\"data\"][\"roots\"]) else \"\"")"

  # -------------------------------------------------------------------------
  echo
  echo "== 5. 备份夹具：秘密 → 仓库 → restic init → files_only 策略 =="
  #
  # "按选择备份"这一条必须有**真的仓库**才能验：一个空的/假的仓库会让 backup_verified
  # 永远是 false，而那条路正是要验的东西。仓库口令只经标准输入进秘密库，不进 argv。

  printf '%s' "$REPO_PASSWORD" >"$TMPD/repo-pass.txt"
  python3 -c 'import json,sys
value=open(sys.argv[1],encoding="utf-8").read()
print(json.dumps({"purpose":"backup_repository_password","value":value,"reference":"p15.3"}))' \
    "$TMPD/repo-pass.txt" >"$TMPD/secret.req.json"
  rm -f "$TMPD/repo-pass.txt"
  chmod 0644 "$TMPD/secret.req.json"
  SEC_CODE=$(web_curl POST /internal/v1/secrets "$PAYLOADS/secret-repopass.json" "$TMPD/secret.req.json")
  rm -f "$TMPD/secret.req.json"
  expect_eq "秘密入库（仓库口令）" "201" "$SEC_CODE"
  SEC_REF=$(jget "$PAYLOADS/secret-repopass.json" 'd["data"]["secret_ref"]')
  expect_contains "秘密引用是 sec_ 形状" "sec_" "$SEC_REF"
  expect_not_contains "秘密响应里没有明文口令" "$REPO_PASSWORD" "$(cat "$PAYLOADS/secret-repopass.json" 2>/dev/null)"

  # 仓库写操作要再认证：先确认"没再认证就是 403"，再真的建。
  REPO_DEF="{\"name\":\"p15-3 本地仓库\",\"backend\":\"local\",\"location\":{\"root_id\":\"backupstore\",\"relative_path\":\"$REPO_REL\"},\"password_secret_ref\":\"$SEC_REF\"}"
  expect_eq "建仓库要再认证（没再认证是 403）" "403" \
    "$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories "$REPO_DEF" repo-no-reauth)"
  expect_eq "再认证" "200" "$(reauth)"
  expect_eq "建仓库" "201" "$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories "$REPO_DEF" repo-create)"
  REP_ID=$(jget "$PAYLOADS/repo-create.json" 'd["data"]["id"]')
  expect_contains "仓库标识是 rep_ 形状" "rep_" "$REP_ID"

  run_action backup.repository.initialize "{\"repository_id\":\"$REP_ID\",\"version\":1}" repo-init
  expect_eq "初始化仓库任务终态" "succeeded" "$JOB_STATE"
  REPO64=$(jobdata repo_id)
  expect_eq "restic 仓库标识是 64 位十六进制" "64" "$(printf '%s' "$REPO64" | wc -c | tr -d ' ')"
  # 独立核对：脚本自己用同一个口令读一次仓库 config，两边都是 restic，但读取路径不同。
  restic_q cat config >"$PAYLOADS/independent-cat-config.json" 2>&1 || true
  expect_eq "独立读到的仓库标识与任务报的一致" "$REPO64" \
    "$(jget "$PAYLOADS/independent-cat-config.json" 'd["id"]')"

  POL_BODY="{\"name\":\"p15-3 部署数据\",\"repository_id\":\"$REP_ID\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"deploydata\",\"relative_path\":\"\"}],\"consistency_mode\":\"files_only\",\"retention\":{\"keep_last\":7}}"
  expect_eq "建 files_only 策略" "201" "$(api "$JAR" "$CSRF" POST /api/v1/backup/policies "$POL_BODY" policy-create)"
  POL_ID=$(jget "$PAYLOADS/policy-create.json" 'd["data"]["id"]')
  expect_contains "策略标识是 pol_ 形状" "pol_" "$POL_ID"

  # stop_managed_apps 策略：只用来验"更新前备份拒收它"，不会被真的执行。
  POL_APPS_BODY="{\"name\":\"p15-3 停应用\",\"repository_id\":\"$REP_ID\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"deploydata\",\"relative_path\":\"\"}],\"consistency_mode\":\"stop_managed_apps\",\"app_ids\":[\"app_0123456789abcdef0123456789abcdef\"],\"retention\":{\"keep_last\":3}}"
  POL_APPS_CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies "$POL_APPS_BODY" policy-apps-create)
  if [ "$POL_APPS_CODE" = "201" ]; then
    POL_APPS=$(jget "$PAYLOADS/policy-apps-create.json" 'd["data"]["id"]')
    pass "建 stop_managed_apps 策略（只用于验拒绝）"
  else
    POL_APPS=""
    fail "建 stop_managed_apps 策略" "状态码 $POL_APPS_CODE，响应 $(head -c 300 "$PAYLOADS/policy-apps-create.json" 2>/dev/null)"
  fi

  # -------------------------------------------------------------------------
  echo
  echo "== 6. 建站：apply（带健康检查）→ 记录落盘 =="
  stamp "§6 建站开始"

  build_definition "$WORK/definition.json" "$IMAGE_BASE"
  cp "$WORK/definition.json" "$PAYLOADS/definition.json"
  apply_params "$TMPD/apply-new.json" ""

  expect_ne "建站前线上没有这个单元在跑" "active" "$(is_active "$UNIT")"
  expect_eq "建站前配置目录不存在" "" "$(file_mode "$CONFIG_DIR")"

  run_action deployment.apply "$(cat "$TMPD/apply-new.json")" apply-new
  expect_verified "建站"
  DEPLOY_ID=$(jobdata deployment_id)
  CONFIG_DIGEST_V1=$(jobdata config_digest)
  expect_eq "部署标识由名字派生" "$(deployment_id_of "$DEPLOY_NAME")" "$DEPLOY_ID"
  expect_contains "配置摘要是 sha256: 形状" "sha256:" "$CONFIG_DIGEST_V1"
  expect_eq "回读：单元 active" "True" "$(jobobserved unit_active)"

  # ---- apply 侧的健康探测（P15.3 新接的：以前只有一条 health_probe_pending 告警） ----
  expect_eq "apply 做了健康探测" "True" "$(jobobserved health_checked)"
  expect_eq "apply 的探测探通了" "True" "$(jobobserved health_ok)"
  expect_eq "apply 的探测打在宿主公开入口" "http://127.0.0.1:$HOST_PORT/" "$(jobobserved health_url)"
  expect_eq "apply 的探测读到 200" "200" "$(jobobserved health_status_code)"
  expect_not_contains "预检里不再有 health_probe_pending" "health_probe_pending" \
    "$(pget '",".join(w["code"] for w in d["data"]["warnings"])')"

  if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
    pass "宿主端口 $HOST_PORT 真的在应答 HTTP 200（独立读数）"
  else
    fail "宿主端口应答 HTTP 200" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
    diag_deploy apply
  fi

  # ---- 记录：这一轮更新的全部依据 ----
  if [ -f "$RECORD_PATH" ]; then pass "部署记录存在：$RECORD_PATH"; else fail "部署记录存在" "不存在"; fi
  expect_eq "记录权限（0640）" "640" "$(file_mode "$RECORD_PATH")"
  expect_eq "记录属主" "root:root" "$(file_owner "$RECORD_PATH")"
  cp "$RECORD_PATH" "$FACTS/deployment-record-v1.json"
  expect_eq "记录的格式版本" "1" "$(record_field 'd["schema"]')"
  expect_eq "记录里的部署名" "$DEPLOY_NAME" "$(record_field 'd["name"]')"
  expect_eq "记录里的部署标识" "$DEPLOY_ID" "$(record_field 'd["deployment_id"]')"
  expect_eq "记录里的定义名与部署名一致" "$DEPLOY_NAME" "$(record_field 'd["definition"]["name"]')"
  expect_eq "记录里的定义钉的是这次写的镜像" "$IMAGE_BASE" "$(record_field 'd["definition"]["image_reference"]')"
  expect_eq "记录里的镜像引用就是定义里写的那个" "$IMAGE_BASE" "$(record_field 'd["image"]["requested"]')"
  expect_eq "记录里的配置摘要就是任务报的版本" "$CONFIG_DIGEST_V1" "$(record_field 'd["image"]["config_digest"]')"
  expect_eq "记录顶层的配置摘要与镜像那一条一致" "$CONFIG_DIGEST_V1" "$(record_field 'd["config_digest"]')"
  REC_DIGEST=$(record_field 'd["image"]["digest"]')
  expect_contains "记录里有镜像 digest（sha256:）" "sha256:" "$REC_DIGEST"
  # **这一条是"记录了实际 digest"的硬判据**：记录的 digest 必须与 docker 自己的读数逐字相同。
  expect_eq "记录里的 digest 与 docker image inspect 的 .Id 逐字相同" \
    "$(image_id_of "$IMAGE_BASE")" "$REC_DIGEST"
  expect_not_contains "记录里没有明文口令" "$REPO_PASSWORD" "$(cat "$RECORD_PATH" 2>/dev/null)"
  # 定义里只该留 sec_ 引用：这一份定义没有引用秘密，所以连 sec_ 都不该出现。
  expect_not_contains "记录里没有 secret_ref 以外的秘密残留" "password" "$(cat "$RECORD_PATH" 2>/dev/null)"
  if [ -e "$PREV_PATH" ]; then
    fail "还没有更新过，所以不该有 .prev" "文件存在"
  else
    pass "还没有更新过，所以没有 .prev"
  fi

  CONFIG_V1_SHA=$(sha_of "$CONFIG_PATH")
  CONFIG_V1_BYTES=$(wc -c <"$CONFIG_PATH" | tr -d ' ')

  # -------------------------------------------------------------------------
  echo
  echo "== 7. 正常更新：$IMAGE_BASE → $IMAGE_NEXT =="
  stamp "§7 更新开始"

  update_params "$TMPD/update-1.json" "$IMAGE_NEXT" ""
  PRE_CODE=$(preflight deployment.update "$(cat "$TMPD/update-1.json")")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-update-1.json"
  expect_eq "预检 deployment.update 状态码" "200" "$PRE_CODE"
  expect_true "预检允许" "$(pallowed)"
  expect_eq "预检没有阻塞项" "" "$(pall)"
  expect_contains "目标显示名带上了换镜像" "→ $IMAGE_NEXT" "$(pstate)"
  expect_contains "影响里要拉镜像" "image_pull" "$(pimpact)"
  expect_contains "影响里要重启单元" "unit_restart" "$(pimpact)"
  expect_contains "影响里写了健康探测失败会回切" "health_probe_rollback" "$(pimpact)"
  expect_contains "影响里要写部署记录" "deployment_record" "$(pimpact)"
  expect_not_contains "这次没有选备份，所以没有 backup_before_update" "backup_before_update" "$(pimpact)"
  expect_contains "锁住 compose 项目" "compose:$DEPLOY_NAME" "$(plocks)"
  expect_contains "锁住宿主端口" "port:tcp/127.0.0.1/$HOST_PORT" "$(plocks)"
  expect_contains "锁住依赖卷" "volume:$VOL_ID" "$(plocks)"
  expect_true "更新需要再认证" "$(preauth)"
  # 影响面里必须明写"数据不回切"——这句话是这一版最重要的边界。
  expect_contains "回切的影响说明里写明数据不回切" "数据不会跟着回切" \
    "$(pget '" ".join(i["description"] for i in d["data"]["impacts"])')"

  expect_eq "再认证" "200" "$(reauth)"
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  JOB_CODE=$(submit_job "$PRE_ID" "" "update-1$RUN_KEY")
  expect_eq "提交被受理" "202" "$JOB_CODE"
  JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  JOB_STATE=$(wait_job "$JOB_ID")
  cp "$WORK/job.json" "$PAYLOADS/job-update-1.json"
  expect_verified "更新"

  CONFIG_DIGEST_V2=$(jobdata config_digest)
  expect_ne "换镜像之后版本变了" "$CONFIG_DIGEST_V1" "$CONFIG_DIGEST_V2"
  expect_eq "观测里的上一版摘要就是更新前的版本" "$CONFIG_DIGEST_V1" "$(jobobserved previous_config_digest)"
  expect_eq "观测里的新摘要就是任务报的版本" "$CONFIG_DIGEST_V2" "$(jobobserved config_digest)"
  expect_eq "这次真的改了配置" "True" "$(jobobserved config_changed)"
  expect_eq "这次不是幂等重放" "False" "$(jobobserved idempotent)"
  expect_eq "更新走的是 restart" "True" "$(jobobserved unit_restarted)"
  expect_eq "回读：单元 active" "True" "$(jobobserved unit_active)"
  expect_eq "观测里的镜像是新的" "$IMAGE_NEXT" "$(jobobserved image)"
  expect_eq "镜像 digest 读到了" "True" "$(jobobserved image_digest_known)"
  expect_contains "镜像 digest 是 sha256: 形状" "sha256:" "$(jobobserved image_digest)"
  expect_eq "更新之后做了健康探测" "True" "$(jobobserved health_checked)"
  expect_eq "更新之后探测通了" "True" "$(jobobserved health_ok)"
  expect_eq "没有回切" "False" "$(jobobserved rolled_back)"

  # ---- 独立读数：盘上两份文件、容器、HTTP ----
  if [ -f "$PREV_PATH" ]; then pass "上一版配置存在：$PREV_PATH"; else fail "上一版配置存在" "不存在"; fi
  expect_eq "上一版权限（0600，里面有秘密）" "600" "$(file_mode "$PREV_PATH")"
  expect_eq "上一版就是更新前那份字节（sha256 逐字相同）" "$CONFIG_V1_SHA" "$(sha_of "$PREV_PATH")"
  expect_eq "上一版的字节数也没变" "$CONFIG_V1_BYTES" "$(wc -c <"$PREV_PATH" | tr -d ' ')"
  cp "$PREV_PATH" "$FACTS/compose.yaml.prev"
  expect_contains "盘上的配置已经是新镜像" "image: \"$IMAGE_NEXT\"" "$(cat "$CONFIG_PATH")"
  expect_not_contains "盘上的配置里没有旧镜像了" "image: \"$IMAGE_BASE\"" "$(cat "$CONFIG_PATH")"
  cp "$CONFIG_PATH" "$FACTS/compose.yaml.v2"
  expect_eq "配置文件仍是 0600" "600" "$(file_mode "$CONFIG_PATH")"
  expect_eq "is-active 仍是 active" "active" "$(is_active "$UNIT")"

  CID=$(container_id)
  expect_ne "compose 项目里有容器" "" "$CID"
  expect_eq "容器在跑" "running" "$(inspect_field "$CID" '{{.State.Status}}')"
  expect_eq "容器真的换了镜像（运行时读回）" "$IMAGE_NEXT" "$(inspect_field "$CID" '{{.Config.Image}}')"
  expect_eq "compose 项目名就是部署名" "$DEPLOY_NAME" \
    "$(inspect_field "$CID" '{{index .Config.Labels "com.docker.compose.project"}}')"
  if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
    pass "新版本在宿主端口上应答 HTTP 200（独立读数）"
  else
    fail "新版本应答 HTTP 200" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
    diag_deploy update-1
  fi

  # ---- 记录跟着换新，且 Previous 指向上一个镜像 ----
  cp "$RECORD_PATH" "$FACTS/deployment-record-v2.json"
  expect_eq "记录里的镜像换成新的" "$IMAGE_NEXT" "$(record_field 'd["image"]["requested"]')"
  expect_eq "记录里的配置摘要是新版本" "$CONFIG_DIGEST_V2" "$(record_field 'd["image"]["config_digest"]')"
  expect_eq "记录里的 digest 与 docker 的读数一致" "$(image_id_of "$IMAGE_NEXT")" \
    "$(record_field 'd["image"]["digest"]')"
  expect_ne "记录里的 digest 与上一版不同" "$REC_DIGEST" "$(record_field 'd["image"]["digest"]')"
  expect_eq "记录记着上一版的镜像引用" "$IMAGE_BASE" \
    "$(record_field 'd.get("previous",{}).get("requested","")')"

  # -------------------------------------------------------------------------
  echo
  echo "== 8. 同镜像重放：配置没变，仍然要 restart =="
  #
  # 这一条验的是 Q485 那条教训在更新路上的形态：配置一个字节都没变，但容器要真的
  # 重起一次；如果实现退化成 `start`，对 active 的 oneshot 就是空操作，"更新成功、
  # 什么都没变"会安静地发生。

  update_params "$TMPD/update-2.json" "$IMAGE_NEXT" ""
  run_action deployment.update "$(cat "$TMPD/update-2.json")" update-2
  expect_verified "同镜像重放"
  expect_eq "配置没变" "False" "$(jobobserved config_changed)"
  expect_eq "这一次是幂等的" "True" "$(jobobserved idempotent)"
  expect_eq "版本没有变" "$CONFIG_DIGEST_V2" "$(jobdata config_digest)"
  expect_eq "上一次的摘要与这次相同" "$CONFIG_DIGEST_V2" "$(jobobserved previous_config_digest)"
  expect_eq "仍然 restart 了单元" "True" "$(jobobserved unit_restarted)"
  # config_unchanged 是**预检**的告警（执行期告警是另一组字符串），所以读预检那一份。
  expect_contains "预检里给了 config_unchanged 告警" "config_unchanged" "$(pwarn)"
  expect_eq "盘上配置没有被改写（sha256 与更新后一致）" \
    "$(sha_of "$FACTS/compose.yaml.v2")" "$(sha_of "$CONFIG_PATH")"

  # -------------------------------------------------------------------------
  echo
  echo "== 9. 健康探测失败 → 回切（换成一个起来就退出的镜像） =="
  stamp "§9 回切开始"

  CONFIG_V2_SHA=$(sha_of "$CONFIG_PATH")
  PREV_V2_SHA=$(sha_of "$PREV_PATH")
  update_params "$TMPD/update-3.json" "$IMAGE_BAD" ""
  run_action deployment.update "$(cat "$TMPD/update-3.json")" update-bad

  # 回切成功**不是失败**：现场已经回到上一版，剩下的问题只有一个"新版本为什么没成"。
  expect_eq "回切之后任务终态仍是 succeeded" "succeeded" "$JOB_STATE"
  expect_eq "结果是 rolled_back（不是 failed、不是 verified）" "rolled_back" "$(joutcome)"
  expect_eq "回切不是错误（error 为空）" "" "$(jerrcode)"
  expect_contains "恢复提示说清已经回切过" "回切" "$(jhint)"
  expect_eq "观测里 rolled_back=true" "True" "$(jobobserved rolled_back)"
  expect_eq "回切之后复核通过" "True" "$(jobobserved rollback_verified)"
  expect_eq "回切之后探测通了" "True" "$(jobobserved rollback_health_ok)"
  expect_falsy "新版本没通过（探不通，或压根起不来）" "$(jobobserved health_ok)"

  # 独立读数：盘上回到旧字节，容器回到旧镜像，HTTP 又通了。
  expect_eq "盘上配置回到更新前那份字节（sha256 逐字相同）" "$CONFIG_V2_SHA" "$(sha_of "$CONFIG_PATH")"
  expect_eq "上一版文件没有被回切改掉" "$PREV_V2_SHA" "$(sha_of "$PREV_PATH")"
  cp "$CONFIG_PATH" "$FACTS/compose.yaml.after-rollback"
  expect_contains "盘上配置是回到的那一版镜像" "image: \"$IMAGE_NEXT\"" "$(cat "$CONFIG_PATH")"
  expect_eq "is-active 仍是 active" "active" "$(is_active "$UNIT")"
  CID=$(container_id)
  expect_eq "容器回到旧镜像并在跑" "$IMAGE_NEXT" "$(inspect_field "$CID" '{{.Config.Image}}')"
  expect_eq "容器状态是 running" "running" "$(inspect_field "$CID" '{{.State.Status}}')"
  if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
    pass "回切之后宿主端口又能应答 HTTP 200（独立读数）"
  else
    fail "回切之后宿主端口应答 HTTP 200" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
    diag_deploy rollback
  fi
  # 回切不改记录：记录里仍然是**旧**镜像，因为新版本从来没有被认下来过。
  expect_eq "部署记录里的镜像仍是旧的那个（回切不改记录）" "$IMAGE_NEXT" \
    "$(record_field 'd["image"]["requested"]')"
  expect_eq "部署记录里的配置版本仍是旧版本" "$CONFIG_DIGEST_V2" \
    "$(record_field 'd["image"]["config_digest"]')"

  # -------------------------------------------------------------------------
  echo
  echo "== 10. 按选择备份：先停单元、按策略备份、读回核对，再换镜像 =="
  stamp "§10 备份更新开始"

  update_params "$TMPD/update-4.json" "$IMAGE_BASE" "$POL_ID"
  PRE_CODE=$(preflight deployment.update "$(cat "$TMPD/update-4.json")")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-update-4.json"
  expect_eq "预检（带备份策略）状态码" "200" "$PRE_CODE"
  expect_true "预检允许（带备份策略）" "$(pallowed)"
  expect_eq "预检没有阻塞项（带备份策略）" "" "$(pall)"
  expect_contains "影响里要停单元再备份" "backup_before_update" "$(pimpact)"
  expect_contains "锁住备份策略" "backup-policy:$POL_ID" "$(plocks)"
  expect_contains "锁住备份仓库" "backup-repo:$REP_ID" "$(plocks)"
  expect_contains "告警里说清会先停再备" "backup_before_update" "$(pwarn)"

  expect_eq "再认证" "200" "$(reauth)"
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  JOB_CODE=$(submit_job "$PRE_ID" "" "update-4$RUN_KEY")
  expect_eq "提交被受理（带备份）" "202" "$JOB_CODE"
  JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  JOB_STATE=$(wait_job "$JOB_ID")
  cp "$WORK/job.json" "$PAYLOADS/job-update-4.json"
  expect_verified "带备份的更新"

  SNAP_ID=$(jobobserved backup_snapshot_id)
  expect_nonempty "观测里有快照标识" "$SNAP_ID"
  expect_eq "快照被读回核对过" "True" "$(jobobserved backup_verified)"
  expect_eq "观测里记着用的是哪条策略" "$POL_ID" "$(jobobserved backup_policy_id)"
  expect_eq "观测里记着用的是哪个仓库" "$REP_ID" "$(jobobserved backup_repository_id)"
  expect_eq "备份之前停过单元" "True" "$(jobobserved unit_stopped)"
  expect_eq "换完镜像是新镜像" "$IMAGE_BASE" "$(jobobserved image)"
  expect_eq "探测通了" "True" "$(jobobserved health_ok)"

  # 独立读数：脚本自己拿 restic 列一遍仓库，那个快照真的在。
  #
  # 比前 8 位而不是比全串：restic 的 `snapshots --json` 在不同版本里 `id` 有时是全长、
  # 有时是短标识，前 8 位是两边都稳的那一段（短标识本身就是它的前缀）。
  restic_q snapshots --json >"$PAYLOADS/independent-snapshots.json" 2>&1 || true
  SNAP_SHORT=$(printf '%s' "$SNAP_ID" | cut -c1-8)
  expect_contains "独立 restic 也看得见这个快照" "$SNAP_SHORT" \
    "$(cat "$PAYLOADS/independent-snapshots.json" 2>/dev/null)"
  expect_eq "容器跑的是备份后换上的镜像" "$IMAGE_BASE" \
    "$(inspect_field "$(container_id)" '{{.Config.Image}}')"
  if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
    pass "带备份的更新之后宿主端口应答 HTTP 200"
  else
    fail "带备份的更新之后宿主端口应答 HTTP 200" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
    diag_deploy update-4
  fi

  # -------------------------------------------------------------------------
  echo
  echo "== 11. 拒绝：只收 files_only 备份策略 =="

  if [ -n "$POL_APPS" ]; then
    DISK_NOW_SHA=$(sha_of "$CONFIG_PATH")
    update_params "$TMPD/update-5.json" "$IMAGE_NEXT" "$POL_APPS"
    preflight_only deployment.update "$(cat "$TMPD/update-5.json")" update-stop-apps
    expect_eq "预检（stop_managed_apps 策略）状态码" "200" "$PRE_CODE"
    expect_false "预检不允许" "$PRE_ALLOWED"
    expect_contains "阻塞码是 backup_policy_needs_apps" "backup_policy_needs_apps" "$PRE_BLOCKERS"
    expect_contains "阻塞说明里指了出路（单独跑 backup.run）" "backup.run" "$(pwhy)"
    expect_eq "盘上配置没被动过" "$DISK_NOW_SHA" "$(sha_of "$CONFIG_PATH")"
    expect_eq "容器也没被动过（还在跑 §10 换上的那个镜像）" "$IMAGE_BASE" \
      "$(inspect_field "$(container_id)" '{{.Config.Image}}')"
  else
    notrun "stop_managed_apps 策略被拒" "这条策略没建起来（见 §5）"
  fi

  # -------------------------------------------------------------------------
  echo
  echo "== 12. 拒绝：版本不符、记录不符、拉不到的镜像、apply 仍是创建语义 =="

  UPDATE_NOW_SHA=$(sha_of "$CONFIG_PATH")

  # (a) version 与盘上摘要不符 → stale
  update_params "$TMPD/update-stale.json" "$IMAGE_NEXT" ""
  python3 - "$TMPD/update-stale.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p, encoding="utf-8"))
d["version"] = "sha256:" + "0" * 64
json.dump(d, open(p, "w", encoding="utf-8"))
PY
  preflight_only deployment.update "$(cat "$TMPD/update-stale.json")" update-stale
  expect_false "版本不符的预检不允许" "$PRE_ALLOWED"
  expect_contains "阻塞码是 stale" "stale" "$PRE_BLOCKERS"

  # (b) 手改盘上配置 → record_mismatch（更新会覆盖别人的改动，所以先停下来）
  cp "$CONFIG_PATH" "$WORK/config-backup.yaml"
  printf '\n# 手工改一行（P15.3 验收：记录与盘上不一致）\n' >>"$CONFIG_PATH"
  TAMPERED_SHA=$(sha_of "$CONFIG_PATH")
  expect_ne "盘上配置确实被改过（sha256 变了）" "$UPDATE_NOW_SHA" "$TAMPERED_SHA"

  update_params "$TMPD/update-tampered.json" "$IMAGE_NEXT" ""
  preflight_only deployment.update "$(cat "$TMPD/update-tampered.json")" update-tampered
  expect_false "记录不符的预检不允许" "$PRE_ALLOWED"
  expect_contains "阻塞码是 record_mismatch" "record_mismatch" "$PRE_BLOCKERS"
  expect_contains "阻塞说明里写明" "配置文件被改过" "$(pwhy)"

  # 删掉记录也算一种"不符"：没有记录就不许更新（不许当场补一份猜出来的）。
  cp "$RECORD_PATH" "$WORK/record-backup.json"
  rm -f "$RECORD_PATH"
  preflight_only deployment.update "$(cat "$TMPD/update-tampered.json")" update-no-record
  expect_false "没有记录的预检不允许" "$PRE_ALLOWED"
  expect_contains "阻塞码是 no_definition_record" "no_definition_record" "$PRE_BLOCKERS"
  expect_contains "阻塞说明里说清是缺记录" "没有定义记录" "$(pwhy)"
  cp "$WORK/record-backup.json" "$RECORD_PATH"
  chmod 0640 "$RECORD_PATH"
  cp "$WORK/config-backup.yaml" "$CONFIG_PATH"
  chmod 0600 "$CONFIG_PATH"
  expect_eq "改回之后盘上配置与原来逐字相同" "$UPDATE_NOW_SHA" "$(sha_of "$CONFIG_PATH")"

  # (c) 拉不到的镜像：pull 在写盘之前，所以盘上两份文件都不该变
  PREV_BEFORE_PULL_SHA=$(sha_of "$PREV_PATH")
  update_params "$TMPD/update-nopull.json" "traefik/whoami@sha256:$(printf '1%.0s' $(seq 1 64))" ""
  preflight_only deployment.update "$(cat "$TMPD/update-nopull.json")" update-nopull
  if [ "$PRE_ALLOWED" = "True" ]; then
    pass "钉了一个拉不到的 digest，预检仍然允许（拉镜像是执行阶段的事）"
    expect_eq "再认证" "200" "$(reauth)"
    PRE_ID=$(pget 'd["data"]["preflight_id"]')
    JOB_CODE=$(submit_job "$PRE_ID" "" "update-nopull$RUN_KEY")
    expect_eq "提交被受理（拉不到的镜像）" "202" "$JOB_CODE"
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-update-nopull.json"
    expect_eq "拉不到镜像的任务终态是 failed" "failed" "$JOB_STATE"
    expect_nonempty "失败原因说到了镜像" "$(jerrmsg)"
    expect_eq "盘上配置一个字节都没变（pull 在写盘之前）" "$UPDATE_NOW_SHA" "$(sha_of "$CONFIG_PATH")"
    expect_eq "上一版文件也没被动过" "$PREV_BEFORE_PULL_SHA" "$(sha_of "$PREV_PATH")"
    expect_eq "容器还在跑原来那个镜像" "$IMAGE_BASE" \
      "$(inspect_field "$(container_id)" '{{.Config.Image}}')"
    expect_eq "单元仍是 active" "active" "$(is_active "$UNIT")"
    if wait_http "http://127.0.0.1:$HOST_PORT/" "200"; then
      pass "失败之后服务仍在应答（没有把应用留在停掉的状态）"
    else
      fail "失败之后服务仍在应答" "读到 $(http_code "http://127.0.0.1:$HOST_PORT/")"
      diag_deploy nopull
    fi
  else
    notrun "拉不到的镜像在执行阶段失败" "预检就不允许：$PRE_BLOCKERS"
  fi

  # (d) apply 仍是创建语义。两条边界都要在：
  #     带上 deployment_id = 调用方要求"更新" → update_not_implemented（指路到 update）；
  #     同名但换了端口                    → deployment_exists（那是另一次部署）。
  #
  # 拿"内容相同"的定义去 apply 不算这一条：那份定义渲染出来的字节与盘上一模一样，
  # apply 会走幂等路径、允许通过——它验的是别的性质。
  apply_params "$TMPD/apply-update.json" "$DEPLOY_ID"
  preflight_only deployment.apply "$(cat "$TMPD/apply-update.json")" apply-update
  expect_false "带 deployment_id 的 apply 不允许（apply 只创建）" "$PRE_ALLOWED"
  expect_contains "阻塞码是 update_not_implemented" "update_not_implemented" "$PRE_BLOCKERS"
  expect_contains "阻塞说明里指路到 deployment.update" "deployment.update" "$(pwhy)"

  python3 - "$WORK/definition.json" "$TMPD/definition-other.json" <<'PY'
import json, sys
defn = json.load(open(sys.argv[1], encoding="utf-8"))
defn["host_port"] = defn["host_port"] + 1
json.dump(defn, open(sys.argv[2], "w", encoding="utf-8"), ensure_ascii=False)
PY
  python3 - "$TMPD/definition-other.json" "$TMPD/apply-other.json" <<'PY'
import json, sys
defn = json.load(open(sys.argv[1], encoding="utf-8"))
json.dump({"deployment_id": None, "version": "", "definition": defn},
          open(sys.argv[2], "w", encoding="utf-8"), ensure_ascii=False)
PY
  preflight_only deployment.apply "$(cat "$TMPD/apply-other.json")" apply-other
  expect_false "同名不同内容的 apply 不允许" "$PRE_ALLOWED"
  expect_contains "阻塞码是 deployment_exists" "deployment_exists" "$PRE_BLOCKERS"
  expect_eq "盘上配置仍然一个字节没变" "$UPDATE_NOW_SHA" "$(sha_of "$CONFIG_PATH")"

  # -------------------------------------------------------------------------
  echo
  echo "== 13. 留状态与证据 =="

  printf '%s\n' "VOL_ID=$VOL_ID" "VOL_VER=$VOL_VER" "DEPLOY_ID=$DEPLOY_ID" \
    "REP_ID=$REP_ID" "POL_ID=$POL_ID" "DEPLOY_NAME=$DEPLOY_NAME" \
    "HOST_PORT=$HOST_PORT" "IMAGE_BASE=$IMAGE_BASE" "IMAGE_NEXT=$IMAGE_NEXT" \
    "CONFIG_V1=$CONFIG_DIGEST_V1" "CONFIG_V2=$CONFIG_DIGEST_V2" \
    >"$WORK/state.env"

  {
    echo "# 单元"; cat "$UNIT_PATH" 2>/dev/null
    echo; echo "# 记录"; cat "$RECORD_PATH" 2>/dev/null
    echo; echo "# restic snapshots"; restic_q snapshots 2>&1
    echo; echo "# docker ps -a"; docker ps -a --format '{{.Names}} {{.Image}} {{.Status}}' 2>&1
    echo; echo "# 容器镜像"; inspect_field "$(container_id)" '{{.Config.Image}}' 2>&1
  } >"$FACTS/deploy-after.txt" 2>&1

  expect_eq "业务目录的 marker 逐字节还在（更新与回切都不动数据）" "$MARKER_SHA" "$(sha_of "$ROOT_PATH/marker.txt")"

  note "证据目录：$WORK（RESULT、facts/、payloads/、logs/）"
  stamp "收尾"
}

# ---------------------------------------------------------------------------
#                                 夹具与载荷
# ---------------------------------------------------------------------------

# build_definition <输出文件> <镜像引用>
#
# 定义是**受限**的十字段闭集（CONTRACTS §8.6）：这里只出现契约里有的字段，
# 多写一个会被 400 unknown_field。
build_definition() {
  python3 - "$1" "$DEPLOY_NAME" "$2" "$HOST_PORT" "$VOL_ID" <<'PY'
import json, sys
out, name, image, port, vol = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), sys.argv[5]
defn = {
    "name": name,
    "image_reference": image,
    "container_port": 80,
    "host_bind_address": "127.0.0.1",
    "host_port": port,
    "environment": [{"name": "APP_MODE", "value": "p15-3"}],
    "mounts": [{
        "source_root_id": "deploydata",
        "relative_path": "",
        "container_path": "/data",
        "read_only": False,
    }],
    "resource_limits": {"memory_limit_bytes": 134217728, "cpu_limit": "0.5"},
    "volume_dependencies": [vol],
    # 面板侧探测：**不**渲染进 Compose，由面板从宿主打公开入口。
    # 宽限期 0 是刻意的：宽限期会把"一起来就退出"的失败算成健康（§9 就靠它不成立）。
    "health_check": {
        "enabled": True,
        "kind": "http",
        "port": 80,
        "path": "/",
        "interval_seconds": 5,
        "timeout_seconds": 2,
        "retries": 3,
        "start_period_seconds": 0,
    },
}
with open(out, "w", encoding="utf-8") as fh:
    json.dump(defn, fh, ensure_ascii=False)
PY
}

# apply_params <输出文件> <deployment_id>（空 = 新建）
apply_params() {
  python3 - "$WORK/definition.json" "$1" "${2:-}" <<'PY'
import json, sys
defn = json.load(open(sys.argv[1], encoding="utf-8"))
dep = sys.argv[3]
params = {"deployment_id": (dep or None), "version": "", "definition": defn}
with open(sys.argv[2], "w", encoding="utf-8") as fh:
    json.dump(params, fh, ensure_ascii=False)
PY
}

# update_params <输出文件> <镜像引用> <备份策略 id 或空>
#
# version 现算：盘上那份配置的 sha256（与产品自己那个 configDigest 同一个口径）。
# 写死一份"我记得的版本"会让 §12 的 stale 用例自欺——预检拿到的版本必须是真的，
# 否则"版本不符被拒"这件事验的是脚本的错，不是产品的判据。
update_params() {
  _ver=""
  if [ -f "$CONFIG_PATH" ]; then
    _ver=$(sha_of "$CONFIG_PATH" | sed 's/^/sha256:/')
  fi
  python3 - "$1" "$DEPLOY_ID" "$_ver" "$2" "${3:-}" <<'PY'
import json, sys
out, dep, ver, image, policy = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
params = {"deployment_id": dep, "version": ver, "image_reference": image}
if policy:
    params["backup_policy_id"] = policy
with open(out, "w", encoding="utf-8") as fh:
    json.dump(params, fh, ensure_ascii=False)
PY
}

deployment_id_of() {
  python3 - "$1" <<'PY'
import hashlib, sys
name = sys.argv[1]
print("dep_" + hashlib.sha256(("compose:" + name).encode()).hexdigest()[:32])
PY
}

run_main
write_result
echo
echo "PASS=$PASSED FAIL=$FAILED NOT RUN=$NTRUN"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
