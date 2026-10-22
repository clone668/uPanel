#!/bin/sh
# P08.3 重启顺序验收（两阶段）：面板写下的 smbd 开机依赖，重启之后真的按顺序生效吗。
#
# 为什么单独一份脚本：真的 `reboot` 会把「主验收脚本」从中间切断，所以在主脚本里
# 那条只能记 NOT RUN。这里用两阶段把它补上——`pre` 布置现场并在重启前自检，
# 人工（或 `-r`）重启，`post` 在**同一个虚拟机的新一次开机**上核对顺序。
#
# 要证明的东西：
#   1) 重启之前：drop-in 在、systemd 认了 `RequiresMountsFor=`，而 fstab 里那一行
#      是 `noauto`（开机不由 local-fs.target 拉起）。这一点很关键——如果卷的
#      automount 本来是 true，那“开机时卷先挂上”就与面板的 drop-in 无关了。
#   2) 重启之后：卷挂着、smbd active、共享里的真文件能被真实客户端读到；
#      并且**挂载单元的 ActiveEnterTimestampMonotonic 早于 smbd 的**。
#   3) journal 里这一次开机两条单元的实际启动记录（原始时间戳，不经任何加工）。
#
# 用法（在一次性 Ubuntu 测试虚拟机里，root）：
#   sudo sh p08-3-reboot-order.sh pre        # 布置 + 重启前自检
#   sudo reboot
#   sudo sh p08-3-reboot-order.sh post       # 重启后核对
#   sudo sh p08-3-reboot-order.sh clean      # 收拾（删共享、卸卷）
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
WORK=${WORK:-/var/tmp/upanel-accept-p08-3}
PHASE=${1:-}
STATE=$WORK/reboot-state.txt
SOCK=/run/upanel/agent.sock
SYS_CFG=/etc/upanel/upanel.json
WEB_DB=$WORK/webdata/upanel.db
PORT=19478
BASE="http://127.0.0.1:$PORT"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
ACCT_RW=smbp08b3
ACCT_RW_PW='vm-bind-rw-horse-5'

MANAGED_CONF=/etc/samba/upanel.d/upanel.conf
STARTUP_DROPIN=/etc/systemd/system/smbd.service.d/upanel-shares.conf
SLUG=p08v3
SHARE_NAME=p08sh3
REL_PATH=team/docs
VOLUME_ROOT=/srv/upanel/volumes
FSTAB=/etc/fstab

AGENT_LOG=$WORK/logs/agent-reboot.log
WEB_LOG=$WORK/logs/web-reboot.log
AGENT_PID=""
WEB_PID=""

PASSED=0
FAILED=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
}
note() { printf '      %s\n' "$1"; }
expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi
}
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "期望包含 '$2'，实际 '$3'" ;;
  esac
}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo sh %s pre|post|clean\n' "$0" >&2
  exit 2
fi
case "$PHASE" in
  pre|post|clean) : ;;
  *) printf '用法：sh %s pre|post|clean\n' "$0" >&2; exit 2 ;;
esac

# 幂等键必须**每次运行都不同**：Web 侧的幂等记录是持久的，同一个键第二次提交会被
# 判成 IDEMPOTENCY_CONFLICT（"已经用于另一个请求"），而作业根本没被创建——第一遍跑完
# 再跑一遍 pre，作业会全部提交失败。键里带时间戳与 pid，run 内仍然稳定（同一键只提交一次）。
RUN_KEY="-$(date -u +%Y%m%d%H%M%S)-$$"

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

# ---------- 面板侧的最小操作（只用于布置现场与收拾） ----------
start_agent() {
  "$BIN_DIR/upanel-agent" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 100); do [ -S "$SOCK" ] && return 0; sleep 0.1; done
  return 1
}
stop_agent() {
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
  fi
  AGENT_PID=""
}
start_web() {
  runuser -u "$WEB_USER" -- "$BIN_DIR/upanel-web" --mode production --db "$WEB_DB" \
    --listen "127.0.0.1:$PORT" >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 100); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  return 1
}
stop_web() {
  if [ -n "${WEB_PID:-}" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null; wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
}
cleanup() { stop_web; stop_agent; return 0; }
trap cleanup EXIT INT TERM

# /run 是 tmpfs，重启之后是空的；安装形态下这个目录由 systemd 单元的 RuntimeDirectory=
# 建立，而这里直接跑二进制、没有单元文件（Q202）。所以由脚本扮演安装脚本的角色：
# 缺就建，权限照单元里的 0755。不这么做，重启后 `clean` 连代理都起不来。
ensure_run_dir() { install -d -m 0755 "$(dirname "$SOCK")"; }

api() {
  _m=$1; _p=$2; _b=${3:-}
  if [ -n "$_b" ]; then
    printf '%s' "$_b" >"$WORK/reboot-req.json"
    curl -s -b "$WORK/reboot.jar" -o "$WORK/reboot-resp.json" -w '%{http_code}' -X "$_m" "$BASE$_p" \
      -H 'Content-Type: application/json' -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" \
      -d @"$WORK/reboot-req.json"
  else
    curl -s -b "$WORK/reboot.jar" -o "$WORK/reboot-resp.json" -w '%{http_code}' -X "$_m" "$BASE$_p" \
      -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF"
  fi
}

# run_action <动作> <参数> <幂等键>：与主脚本同一套（预检 → 再认证 → 作业 → 等终态）。
run_action() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/reboot-pre.json"
  _code=$(curl -s -b "$WORK/reboot.jar" -o "$WORK/reboot-pf.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/operations/preflight" -H 'Content-Type: application/json' \
    -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" -d @"$WORK/reboot-pre.json")
  _allowed=$(jget "$WORK/reboot-pf.json" 'd["data"]["allowed"]')
  _id=$(jget "$WORK/reboot-pf.json" 'd["data"]["preflight_id"]')
  _phrase=$(jget "$WORK/reboot-pf.json" 'd["data"].get("confirmation_phrase") or ""')
  _reauth=$(jget "$WORK/reboot-pf.json" 'bool(d["data"].get("requires_reauth"))')
  if [ "$_reauth" = "True" ]; then api POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}" >/dev/null; fi
  if [ "$_allowed" != "True" ] || [ -z "$_id" ]; then
    fail "预检（$3）允许" "allowed=$_allowed id=$_id $(jget "$WORK/reboot-pf.json" '"; ".join(b["code"] for b in d["data"]["blockers"])')"
    JOB_STATE="preflight"
    return 0
  fi
  python3 -c 'import json,sys
d={"preflight_id":sys.argv[1]}
if sys.argv[2]:
    d["confirmation_phrase"]=sys.argv[2]
print(json.dumps(d))' "$_id" "$_phrase" >"$WORK/reboot-job.json"
  _code=$(curl -s -b "$WORK/reboot.jar" -o "$WORK/reboot-submit.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/jobs" -H 'Content-Type: application/json' -H "Origin: $BASE" \
    -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: $3" -d @"$WORK/reboot-job.json")
  _jid=$(jget "$WORK/reboot-submit.json" 'd["data"]["job_id"]')
  # 没有 job_id 就不要去轮询一个假 id：先前这里把 jget 的 `ERR:KeyError:'data'` 当成 id，
  # 于是脚本对着 /api/v1/jobs/ERR:... 打满 60 秒 404，最后报“timeout”——一个提交失败
  # 会被记成“作业超时”，指向完全错误的地方（Q244）。提交失败当场说清、当场返回。
  case "$_jid" in
    ""|ERR:*)
      fail "提交作业（$3）" "HTTP $_code $(jget "$WORK/reboot-submit.json" \
        'd["error"]["code"]+": "+d["error"]["message"] if "error" in d else str(d)[:200]')"
      JOB_STATE="submit_failed"
      return 0
      ;;
  esac
  JOB_STATE="timeout"
  _i=0
  while [ "$_i" -lt 300 ]; do
    api GET "/api/v1/jobs/$_jid" >/dev/null
    _st=$(jget "$WORK/reboot-resp.json" 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) JOB_STATE=$_st; break ;;
    esac
    _i=$((_i + 1)); sleep 0.2
  done
}

login() {
  curl -s -c "$WORK/reboot.jar" -o "$WORK/reboot-login.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
    -H 'X-UPanel-Request: 1' -H "Origin: $BASE" \
    -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}"
}

# ---------- 独立证据 ----------
mount_unit_of() { systemd-escape -p --suffix=mount "$1" 2>/dev/null; }
systemd_requires() { systemctl show smbd.service -p RequiresMountsFor 2>/dev/null | sed -n 's/^RequiresMountsFor=//p'; }
systemd_after() { systemctl show smbd.service -p After 2>/dev/null | sed -n 's/^After=//p'; }
mono_of() { systemctl show "$1" -p ActiveEnterTimestampMonotonic 2>/dev/null | sed -n 's/^ActiveEnterTimestampMonotonic=//p'; }
human_of() { systemctl show "$1" -p ActiveEnterTimestamp 2>/dev/null | sed -n 's/^ActiveEnterTimestamp=//p'; }
is_mounted() { if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi; }
smb_conf_sha() { sha256sum /etc/samba/smb.conf 2>/dev/null | cut -d' ' -f1; }
authfile() {
  printf 'username=%s\npassword=%s\ndomain=\n' "$ACCT_RW" "$ACCT_RW_PW" >"$WORK/reboot-auth"
  chmod 0600 "$WORK/reboot-auth"
  printf '%s' "$WORK/reboot-auth"
}
smb_ls() { smbclient "//127.0.0.1/$1" -A "$(authfile)" -m SMB3 -c 'ls' 2>&1; }

# =====================================================================
case "$PHASE" in
pre)
  echo "== P08.3 重启顺序验收：pre（布置现场） =="
  for b in "$BIN_DIR/upanel-agent" "$BIN_DIR/upanel-web"; do
    if [ -x "$b" ]; then pass "二进制存在 $b"; else fail "二进制存在 $b" "缺失"; fi
  done
  expect_eq "夹具：smbd 在运行" "active" \
    "$(systemctl is-active smbd.service 2>/dev/null | head -1)"
  rm -f "$STATE"
  ensure_run_dir
  if start_agent; then pass "代理启动"; else fail "代理启动" "$(tail -5 "$AGENT_LOG")"; fi
  if start_web; then pass "Web 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG")"; fi
  expect_eq "管理员登录" "200" "$(login)"
  CSRF=$(jget "$WORK/reboot-login.json" 'd["data"]["csrf_token"]')
  if [ -n "$CSRF" ]; then pass "拿到 CSRF"; else fail "拿到 CSRF" "登录响应里没有 csrf_token"; fi

  api GET /api/v1/storage/volumes >/dev/null
  # 列表视图里的标识字段是 id（不是详情里的 volume_id）——按猜测的形状断言过一次，
  # 结果三条断言对着一个不存在的键说话（Q241）。
  VOL_ID=$(jget "$WORK/reboot-resp.json" \
    "next((v[\"id\"] for v in d[\"data\"][\"volumes\"] if v.get(\"slug\")==\"$SLUG\"), \"\")")
  if [ -n "$VOL_ID" ]; then pass "找到主验收脚本留下的卷登记"; else fail "找到主验收脚本留下的卷登记" "列表里没有 slug=$SLUG"; fi
  api GET "/api/v1/storage/volumes/$VOL_ID" >/dev/null
  VOL_VER=$(jget "$WORK/reboot-resp.json" 'd["data"]["version"]')
  if [ "$(is_mounted "$VOLUME_ROOT/$SLUG")" != "True" ]; then
    run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" "reboot-mount$RUN_KEY"
    expect_eq "挂载卷任务终态" "succeeded" "$JOB_STATE"
  fi
  expect_eq "独立证据：卷挂着" "True" "$(is_mounted "$VOLUME_ROOT/$SLUG")"

  api GET /api/v1/share-accounts >/dev/null
  ACCT_ID=$(jget "$WORK/reboot-resp.json" \
    "next((a[\"id\"] for a in d[\"data\"][\"accounts\"] if a.get(\"name\")==\"$ACCT_RW\"), \"\")")
  if [ -n "$ACCT_ID" ]; then pass "找到受管理账号 $ACCT_RW"; else fail "找到受管理账号" "$ACCT_RW 不在账号列表里"; fi
  DEF=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":false,"allowed_cidrs":["127.0.0.0/8"],"smb_users":[{"account_id":"%s","access":"write"}]}' \
    "$SHARE_NAME" "$VOL_ID" "$REL_PATH" "$ACCT_ID")
  run_action share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF}" "reboot-apply$RUN_KEY"
  expect_eq "应用共享任务终态" "succeeded" "$JOB_STATE"
  api GET /api/v1/shares >/dev/null
  SHARE_ID=$(jget "$WORK/reboot-resp.json" \
    "next((s[\"id\"] for s in d[\"data\"][\"shares\"] if s[\"definition\"][\"name\"]==\"$SHARE_NAME\"), \"\")")
  if [ -n "$SHARE_ID" ]; then pass "共享已登记"; else fail "共享已登记" "列表里没有 $SHARE_NAME"; fi

  # —— 重启之前的现场
  MP=$VOLUME_ROOT/$SLUG
  expect_eq "drop-in 存在" "True" "$([ -f "$STARTUP_DROPIN" ] && echo True || echo False)"
  expect_eq "systemd 解析出的 RequiresMountsFor" "$MP" "$(systemd_requires)"
  FSTAB_LINE=$(awk -v mp="$MP" '$2==mp {print; exit}' "$FSTAB" 2>/dev/null)
  note "fstab 里这一行：$FSTAB_LINE"
  expect_contains "fstab 条目带 noauto（开机不由 local-fs 拉起）" "noauto" "$FSTAB_LINE"
  expect_contains "fstab 条目带 nofail" "nofail" "$FSTAB_LINE"
  LS_PRE=$(smb_ls "$SHARE_NAME")
  printf '%s\n' "$LS_PRE" >"$WORK/logs/reboot-pre-smbclient.txt"
  expect_contains "重启之前客户端能列出共享里的文件" "probe.txt" "$LS_PRE"
  SMB_SHA_PRE=$(smb_conf_sha)
  cat >"$STATE" <<TXT
share_id=$SHARE_ID
share_name=$SHARE_NAME
volume_id=$VOL_ID
mount_point=$MP
account=$ACCT_RW
smb_conf_sha_before=$SMB_SHA_PRE
applied_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
TXT
  cp "$STARTUP_DROPIN" "$WORK/logs/reboot-dropin-before.conf" 2>/dev/null
  printf 'RequiresMountsFor: %s\nAfter: %s\n' "$(systemd_requires)" "$(systemd_after)" \
    >"$WORK/logs/reboot-systemd-before.txt"
  printf 'boot_id_before=%s\nuptime_before=%s\n' "$(cat /proc/sys/kernel/random/boot_id)" \
    "$(cut -d' ' -f1 /proc/uptime)" >>"$WORK/logs/reboot-systemd-before.txt"
  echo
  note "现场布置完成。现在重启这台虚拟机，然后跑 post："
  note "  sudo reboot"
  note "  sudo sh $0 post"
  ;;
post)
  echo "== P08.3 重启顺序验收：post（重启之后） =="
  if [ ! -f "$STATE" ]; then
    printf '找不到 %s：先跑 pre 阶段\n' "$STATE" >&2
    exit 2
  fi
  . "$STATE"
  MP=$mount_point
  UPTIME=$(cut -d' ' -f1 /proc/uptime)
  BOOT_ID=$(cat /proc/sys/kernel/random/boot_id)
  note "本次开机已运行 ${UPTIME}s（boot_id=$BOOT_ID）"
  if [ "$(echo "$UPTIME" | cut -d. -f1)" -lt 1800 ]; then
    pass "这是一次新的开机（运行时间 < 30 分钟）"
  else
    fail "这是一次新的开机" "机器已经运行 ${UPTIME}s，看不出这是重启后的状态"
  fi
  PRE_BOOT_ID=$(sed -n 's/^boot_id_before=//p' "$WORK/logs/reboot-systemd-before.txt" 2>/dev/null)
  if [ -n "$PRE_BOOT_ID" ] && [ "$PRE_BOOT_ID" != "$BOOT_ID" ]; then
    pass "boot_id 与重启前不同（确实是新的一次开机）"
  else
    fail "boot_id 与重启前不同" "重启前='$PRE_BOOT_ID' 现在='$BOOT_ID'"
  fi

  MOUNT_UNIT=$(mount_unit_of "$MP")
  expect_eq "本次开机 drop-in 还在" "True" "$([ -f "$STARTUP_DROPIN" ] && echo True || echo False)"
  expect_eq "本次开机 systemd 仍然解析出 RequiresMountsFor" "$MP" "$(systemd_requires)"
  expect_contains "After 里有挂载单元" "$MOUNT_UNIT" "$(systemd_after)"
  expect_eq "smbd 是 active" "active" "$(systemctl is-active smbd.service 2>/dev/null | head -1)"
  expect_eq "挂载单元是 active" "active" "$(systemctl is-active "$MOUNT_UNIT" 2>/dev/null | head -1)"
  expect_eq "独立证据：卷在本次开机里挂上了" "True" "$(is_mounted "$MP")"

  MOUNT_MONO=$(mono_of "$MOUNT_UNIT")
  SMB_MONO=$(mono_of smbd.service)
  note "挂载单元 ActiveEnterTimestampMonotonic = $MOUNT_MONO ($(human_of "$MOUNT_UNIT"))"
  note "smbd      ActiveEnterTimestampMonotonic = $SMB_MONO ($(human_of smbd.service))"
  if [ -n "$MOUNT_MONO" ] && [ -n "$SMB_MONO" ] && [ "$MOUNT_MONO" -gt 0 ] && [ "$SMB_MONO" -gt 0 ]; then
    if [ "$MOUNT_MONO" -le "$SMB_MONO" ]; then
      pass "本次开机里挂载单元先进入 active，smbd 之后才起来（差 $(( (SMB_MONO - MOUNT_MONO) / 1000 ))ms）"
    else
      fail "挂载单元先于 smbd" "挂载晚了 $(( (MOUNT_MONO - SMB_MONO) / 1000 ))ms：smbd 先起来服务了一个空目录"
    fi
  else
    fail "两条单元的 ActiveEnterTimestampMonotonic 都读得到" "mount='$MOUNT_MONO' smbd='$SMB_MONO'"
  fi

  LS_POST=$(smb_ls "$SHARE_NAME")
  printf '%s\n' "$LS_POST" >"$WORK/logs/reboot-post-smbclient.txt"
  expect_contains "重启之后客户端能列出共享里的文件" "probe.txt" "$LS_POST"
  expect_eq "smb.conf 与重启前逐字节相同" "$smb_conf_sha_before" "$(smb_conf_sha)"

  journalctl -b -o short-precise -u smbd.service -u "$MOUNT_UNIT" --no-pager \
    >"$WORK/logs/reboot-journal.txt" 2>&1
  systemctl show smbd.service -p RequiresMountsFor -p After -p DropInPaths -p ActiveEnterTimestamp \
    >"$WORK/logs/reboot-systemd-after.txt" 2>&1
  systemctl show "$MOUNT_UNIT" -p ActiveEnterTimestamp -p Where -p WantedBy \
    >>"$WORK/logs/reboot-systemd-after.txt" 2>&1
  note "journal 与单元属性已存到 $WORK/logs/reboot-{journal,systemd-after}.txt"
  ;;
clean)
  echo "== P08.3 重启顺序验收：clean（收拾现场） =="
  if [ -f "$STATE" ]; then
    . "$STATE"
  fi
  ensure_run_dir
  if start_agent; then pass "代理启动"; else fail "代理启动" "$(tail -5 "$AGENT_LOG")"; fi
  if start_web; then pass "Web 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG")"; fi
  if [ "$(login)" != "200" ]; then
    fail "管理员登录" "登录失败，跳过收拾"
  else
    CSRF=$(jget "$WORK/reboot-login.json" 'd["data"]["csrf_token"]')
    if [ -n "${share_id:-}" ]; then
      api GET "/api/v1/shares/$share_id" >/dev/null
      _v=$(jget "$WORK/reboot-resp.json" 'd["data"]["share"]["version"]')
      if [ -n "$_v" ]; then
        run_action share.delete "{\"share_id\":\"$share_id\",\"version\":\"$_v\"}" "reboot-clean-del$RUN_KEY"
        expect_eq "删掉共享" "succeeded" "$JOB_STATE"
      fi
    fi
    if [ -n "${volume_id:-}" ]; then
      api GET "/api/v1/storage/volumes/$volume_id" >/dev/null
      _vv=$(jget "$WORK/reboot-resp.json" 'd["data"]["version"]')
      if [ -n "$_vv" ]; then
        run_action storage.volume.unmount "{\"volume_id\":\"$volume_id\",\"version\":\"$_vv\"}" "reboot-clean-unmount$RUN_KEY"
        expect_eq "卸载卷" "succeeded" "$JOB_STATE"
      fi
    fi
  fi
  expect_eq "drop-in 已随最后一条共享一起收回" "False" \
    "$([ -f "$STARTUP_DROPIN" ] && echo True || echo False)"
  ;;
esac

echo
printf 'PASS=%s FAIL=%s\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
