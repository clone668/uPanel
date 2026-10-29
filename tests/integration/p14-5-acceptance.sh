#!/bin/sh
# P14.5 Linux 验收脚本：受管理 Linux 账号、authorized_keys 托管区、受限 sudo 模板。
#
# 运行位置：Ubuntu 22.04 验收 VM 内，**必须以 root 运行**。不需要一次性空盘——这一轮
# 不碰分区、不碰挂载，只碰账号、~/.ssh 与 /etc/sudoers.d。
#
# 用法（**必须后台起**，理由与 P14.1–P14.4 相同：visudo、sudo -l、userdel 都可能因为
# 一个坏掉的 sudoers 文件卡住，ssh 管道抖一次就 SIGPIPE 掉整轮验收）：
#   sudo BIN_DIR=/var/tmp/upanel-bin setsid nohup \
#        sh tests/integration/p14-5-acceptance.sh >/var/tmp/p145-run.log 2>&1 &
#   然后轮询 /var/tmp/upanel-accept-p14-5/RESULT
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。NOT RUN 是**按设计**列出的范围外项，
# 单独计数、不影响退出码——与 P14.1–P14.4 一致。
#
# ---------------------------------------------------------------- 这一轮验什么
#
#   A. **口令不进 argv、不进日志**：口令先交给 POST /secrets（purpose=account_password，
#      4 KiB 上限，只进不出）拿回 `sec_` 引用，任务参数里只有引用。执行器把明文从标准
#      输入交给 chpasswd。验收里断言的是**结果**：`passwd -S` 读回 P，而不是面板说成功。
#   B. **uid 是系统分配的，不是页面给的**：建完用 getent passwd 读回，uid 必须落在
#      1000–59999；home 必须在 /home 之下（面板拒绝往别处写 authorized_keys）。
#   C. **托管区只碰自己那几行**：authorized_keys 里先放一条"人手写的外来钥匙"，面板
#      加/撤公钥之后那一条必须**逐字节不变**，托管区里的指纹必须等于登记表，文件权限
#      回读必须是 0600、目录 0700。
#   D. **sudo 的判据是 sudo 自己**：写进 /etc/sudoers.d/upanel-<账号> 之后，`visudo -c`
#      整份通过、`sudo -l -U` 读回的授权逐条等于模板。然后**真的跑一次**（runuser 换到
#      那个账号，`sudo -n` 执行模板里的命令，退出码 0），再跑一条模板外命令（必须被拒）。
#   E. **真机制造一次 visudo 失败**：在 /etc/sudoers.d 放一个坏文件，让**整份** `visudo -c`
#      失败，此时给同一个账号换模板。候选文件自己合格、rename 已经发生，整份校验失败 →
#      执行器必须把旧内容恢复。断言：文件哈希与失败前一致、`sudo -l -U` 仍是旧模板、
#      任务失败原因里带 SUDO_VISUDO_FAILED。移走坏文件再应用 → 成功，这才证明失败是那个
#      坏文件造成的，不是别的东西。
#   F. **面板不覆盖别人的文件**：手工把 /etc/sudoers.d/upanel-alice 改成没有认领标记的
#      内容（模拟"有人手改过"），预检必须 SUDO_FILE_DAMAGED，文件一个字都不动。
#   G. **保留名与账号种类**：`root` 这种系统保留名必须被拒；共享账号拿不到 sudo。
#   H. **禁用是换壳不是 -L**：禁用之后 `passwd` 的登录壳读回是 nologin、`passwd -S`
#      的口令状态一位没变（没用 `usermod -L`）；启用之后回到 rbash。真跑 usermod 前后各
#      读一次。禁用到底拦不拦得住人，见 J 的第 2、3 面。
#   I. **删账号不删数据**：userdel 不带 -r，home 与里面的文件一个不少，sudoers 文件跟着撤掉。
#   J. **（有条件）公钥登录真机的四面**，断言的是"钥匙能用、口令不是门槛、壳才是门槛"：
#      ① bob 没有口令、只配了钥匙 A → 能用这把钥匙登（publickey 认证不看 shadow 的口令
#      字段）；② 面板禁用 bob（换壳成 nologin）→ 钥匙还在文件里，但同一把钥匙登不上；
#      ③ 再启用 → 又能登，把 ② 的拒绝归因到壳上，而不是归因到 sshd 的配置；④ 撤掉公钥
#      → 登不上，托管区确认真在生效。首轮这里写的是"没口令就登不上"，真机否了它：那不是
#      sshd 的行为，key-only 账号在真实服务器上很常见。能在这台机器上验就验，验不了（没有
#      sshd）就记 NOT RUN 并写明原因，不编。
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * **共享账号的 Samba 那一半**（share.account.apply 建系统账号 + passdb）：需要
#     samba-common-bin。这台 VM 上装了就跑（"共享账号不能拿 sudo"用得上），没装就记
#     NOT RUN——不为一条验收去装包（PLAN.md 第 10 节）。
#   * **并发**：两条公钥动作同时落到同一份 authorized_keys 的锁行为由单测
#     internal/accounts/sshkey_test.go 覆盖；真机同时发两个任务会把冲突变成时序问题。
#   * **10 把公钥上限的真机路径**：上限判定在判决层，单测覆盖；真机每加一把都要落一次
#     文件，验到 2 把即可，第 11 把的行为与单测同源。
#   * **磁盘/家目录在别处的账号**（home 指向 NFS、home 是符号链接）：需要额外的磁盘
#     布局。面板对这类账号是**拒绝**写 authorized_keys（SSH_KEY_HOME_INVALID），这条
#     只在单测里造过，真机没有造。
#   * **TOTP / 面板自己的登录策略**：属于 P14.6。

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

WORK=${WORK:-/var/tmp/upanel-accept-p14-5}
TMPD=$WORK/tmp
FACTS=$WORK/facts
PAYLOADS=$WORK/payloads
KEYS=$WORK/keys
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19481}
WEB_ADDR=127.0.0.1
BASE="http://$WEB_ADDR:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-p145-admin-92'
ACCT_PASSWORD='kestrel-ocean-41'
ACCT_PASSWORD_2='kestrel-ocean-42'
JAR=""
CSRF=""
RESULT=$WORK/RESULT

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

ACCOUNTS_PATH=/api/v1/accounts
SECRETS_PATH=/api/v1/secrets
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs

SUDOERS_DIR=/etc/sudoers.d
HOME_BASE=/home
MIN_UID=1000
MAX_UID=59999
BLOCK_BEGIN='# >>> upanel 托管区（整块由面板重写，手改会被覆盖）>>>'
BLOCK_END='# <<< upanel 托管区 <<<'

ACCT=alice
ACCT2=bob

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
# 标识形状（四个前缀之一 + 32 位小写十六进制）。
#
# 只判"非空"不够：jget 取不到时会打印 `ERR:KeyError:'data'` 这种字符串，非空，
# 于是后面每一条断言都以这个垃圾值为输入全部变红，真正的现场反而被埋掉（P14.4 第一轮
# 就是这么红的）。所以标识一律按形状判。
expect_id() {
  if printf '%s' "$2" | grep -Eq '^(sac|ssk|sru|sec|srule)_[0-9a-f]{32}$'; then
    pass "$1"
  else
    fail "$1" "不是 <前缀>_+32 位十六进制：'$2'"
  fi
}

write_result() {
  {
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
pget() { jget "$WORK/preflight.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
joberr() { jobget 'json.dumps(d["data"].get("error") or {})'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"].get(\"$1\")"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

# ---- 进程 ----

start_agent() {
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
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
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

# ---- 账号两侧的清理 ----
#
# 这一轮的卫生比 P14.1–P14.4 更要紧：**账号与 sudoers 是系统级的**。留一个 alice 或者
# 留一份坏的 /etc/sudoers.d 文件，下一轮验收的第一步就可能读到上一轮的现场，甚至让
# 整台机器的 sudo 不可用。所以：
#   * 起手把 /etc/sudoers.d 整份留档，并把上一轮 `upanel-*` 文件与已知的账号清掉；
#   * 收尾把留档放回去、把账号删掉。
SUDOERS_ARCHIVE=""

archive_sudoers() {
  SUDOERS_ARCHIVE=$WORK/logs/sudoers.d-initial
  mkdir -p "$SUDOERS_ARCHIVE"
  cp "$SUDOERS_DIR"/* "$SUDOERS_ARCHIVE"/ 2>/dev/null || true
  note "sudoers.d 留档：$SUDOERS_ARCHIVE"
}

restore_sudoers() {
  # 先把面板的与这一轮造的临时文件都删掉，再把留档放回去：留档里可能本来就有
  # upanel-* 之外的东西（比如别的软件的规则），一个都不能少。
  for f in "$SUDOERS_DIR"/upanel-* "$SUDOERS_DIR"/zz-p145-broken; do
    [ -e "$f" ] || continue
    rm -f "$f"
  done
  if [ -n "$SUDOERS_ARCHIVE" ] && [ -d "$SUDOERS_ARCHIVE" ]; then
    cp "$SUDOERS_ARCHIVE"/* "$SUDOERS_DIR"/ 2>/dev/null || true
  fi
  return 0
}

remove_account_quietly() {
  _name=$1
  userdel "$_name" 2>/dev/null || true
  rm -rf "$HOME_BASE/$_name" 2>/dev/null || true
}

cleanup() {
  stop_web
  stop_agent
  restore_sudoers
  remove_account_quietly "$ACCT"
  remove_account_quietly "$ACCT2"
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
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }
reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$TMPD/pre.json"
  _code=$(curl -s -b "$JAR" -o "$TMPD/resp.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$TMPD/pre.json")
  cp "$TMPD/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}

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
  while [ "$_i" -lt 400 ]; do
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

# 幂等键每次运行都不同：Web 侧的幂等记录是持久的（重复的键会判 IDEMPOTENCY_CONFLICT）。
RUN_KEY="-$(date -u +%Y%m%d%H%M%S)-$$"

run_action() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pget 'd["data"]["allowed"]')
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  PRE_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
  PRE_REAUTH=$(pget 'bool(d["data"].get("requires_reauth"))')
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

# 只跑预检、不提交（用来断言"判决层拒绝"那几条）。
preflight_only() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pget 'd["data"]["allowed"]')
  PRE_BLOCKERS=$(pall)
}

# ---- 面板读的账号 ----

get_accounts() {
  _code=$(api "$JAR" "$CSRF" GET "$ACCOUNTS_PATH")
  cp "$TMPD/resp.json" "$WORK/accounts-$1.json"
  printf '%s' "$_code"
}
aget() { jget "$WORK/accounts-$1.json" "$2"; }
# 一个账号的某一列（按名字找）。找不到时打印空串，不抛异常。
acct_field() { # $1 label, $2 name, $3 python 表达式（对象名为 a）
  aget "$1" "[str($3) for a in d[\"data\"][\"accounts\"] if a[\"name\"]==\"$2\"][0] if any(a[\"name\"]==\"$2\" for a in d[\"data\"][\"accounts\"]) else \"\""
}
acct_id() { acct_field "$1" "$2" 'a["id"]'; }
acct_version() { acct_field "$1" "$2" 'a["version"]'; }
acct_count() { aget "$1" 'len(d["data"]["accounts"])'; }

get_keys() {
  _code=$(api "$JAR" "$CSRF" GET "$ACCOUNTS_PATH/$1/ssh-keys")
  cp "$TMPD/resp.json" "$WORK/keys-$2.json"
  printf '%s' "$_code"
}
kget() { jget "$WORK/keys-$1.json" "$2"; }

# ---- 独立证据（不看面板怎么说） ----

passwd_field() { getent passwd "$1" 2>/dev/null | cut -d: -f"$2"; }
acct_uid() { passwd_field "$1" 3; }
acct_gid() { passwd_field "$1" 4; }
acct_home() { passwd_field "$1" 6; }
acct_shell() { passwd_field "$1" 7; }
# `passwd -S` 的第二列：P=有口令，L=锁定，NP=无口令。
passwd_state() { passwd -S "$1" 2>/dev/null | awk '{print $2}'; }
file_mode() { stat -c '%a' "$1" 2>/dev/null; }
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }
fingerprint_of() { ssh-keygen -lf "$1" 2>/dev/null | awk '{print $2}'; }
# 文件里某个指纹在不在。authorized_keys 可以和别人共用，`ssh-keygen -lf` 一行一把。
fp_listed() {
  ssh-keygen -lf "$1" 2>/dev/null | awk '{print $2}' | grep -qxF "$2" && echo True || echo False
}

authz_of() { cat "$(acct_home "$1")/.ssh/authorized_keys" 2>/dev/null; }
# 托管区正文（只取标记之间、非空、非注释的行）。
managed_lines() {
  authz_of "$1" | awk -v b="$BLOCK_BEGIN" -v e="$BLOCK_END" '
    $0 == b { inside = 1; next }
    $0 == e { inside = 0; next }
    inside && $0 !~ /^[[:space:]]*$/ && $0 !~ /^#/ { print }'
}
managed_count() { managed_lines "$1" | wc -l | tr -d ' '; }
has_block() {
  if authz_of "$1" | grep -qxF "$BLOCK_BEGIN"; then echo True; else echo False; fi
}
# 托管区之外的那一条外来的行是否**逐字节**还在。
foreign_line_intact() { # $1 账号，$2 期望的整行
  if authz_of "$1" | grep -qxF "$2"; then echo True; else echo False; fi
}

sudoers_of() { printf '%s/upanel-%s' "$SUDOERS_DIR" "$1"; }
sudoers_text() { cat "$(sudoers_of "$1")" 2>/dev/null; }
sudoers_has() {
  if sudoers_text "$1" | grep -qF "$2"; then echo True; else echo False; fi
}
visudo_all() { visudo -c 2>&1 | tail -2 | tr '\n' ' '; }
visudo_all_rc() { visudo -c >/dev/null 2>&1; echo $?; }
# `sudo -l -U <账号>` 是 **sudo 自己解析出来的授权**，不是文件内容，也不是面板的读回。
sudo_list() { sudo -l -U "$1" 2>&1; }
sudo_list_has() {
  if sudo_list "$1" | grep -qF "$2"; then echo True; else echo False; fi
}

# ---------------------------------------------------------------------------
echo "== 0. 环境与起点 =="
# ---------------------------------------------------------------------------

for p in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$p" ]; then pass "二进制存在：$p"; else echo "缺少 $p（先交叉编译到 $BIN_DIR）" >&2; exit 2; fi
done
for tool in useradd usermod userdel getent passwd chpasswd visudo sudo ssh-keygen stat python3; do
  if command -v "$tool" >/dev/null 2>&1; then pass "夹具：有 $tool"; else
    echo "缺少 $tool —— 这是环境不满足，不是产品缺陷" >&2; exit 2
  fi
done
# 受限档用的是 rbash：没有它，restricted 这一档建不出来（面板会拒，不是缺陷）。
if [ -x /bin/rbash ]; then pass "夹具：有 /bin/rbash"; else
  echo "没有 /bin/rbash（bash 的受限壳）——这一轮验不了 restricted 档" >&2; exit 2
fi
if [ -x /usr/sbin/nologin ]; then pass "夹具：有 /usr/sbin/nologin"; else
  echo "没有 /usr/sbin/nologin" >&2; exit 2
fi
# 起点：这两个账号不能已经存在（否则分不清是这一轮建的还是上一轮留下的）。
for name in "$ACCT" "$ACCT2"; do
  if id "$name" >/dev/null 2>&1; then
    echo "起点不干净：$name 已经存在（先 userdel -r $name）" >&2; exit 2
  fi
done
pass "起点：$ACCT / $ACCT2 都不存在"

rm -rf "$WORK"
mkdir -p "$WORK" "$TMPD" "$FACTS" "$PAYLOADS" "$WORK/logs" "$KEYS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/logs" "$PAYLOADS" "$FACTS"
chmod 0700 "$TMPD" "$KEYS"
chown -R "$WEB_USER" "$WEB_DB_DIR"
rm -f "$RESULT"

archive_sudoers
# 上一轮留下的面板文件先摘掉，并把账号清掉：留着它们会让"起点干净"的断言变假。
for f in "$SUDOERS_DIR"/upanel-* "$SUDOERS_DIR"/zz-p145-broken; do
  [ -e "$f" ] || continue
  rm -f "$f"
  note "移走上一轮留下的 $(basename "$f")（已留档）"
done
for name in "$ACCT" "$ACCT2"; do remove_account_quietly "$name"; done
expect_eq "起点：sudoers.d 里没有 upanel 文件" "0" \
  "$(find "$SUDOERS_DIR" -maxdepth 1 -name 'upanel-*' | wc -l | tr -d ' ')"
expect_eq "起点：整份 sudo 配置通过 visudo -c" "0" "$(visudo_all_rc)"

# 三把钥匙：两把给面板用，一把当"人手写的外来钥匙"。
ssh-keygen -q -t ed25519 -N '' -C "alice@laptop" -f "$KEYS/key-a" >/dev/null 2>&1
ssh-keygen -q -t ed25519 -N '' -C "alice@desktop" -f "$KEYS/key-b" >/dev/null 2>&1
ssh-keygen -q -t ed25519 -N '' -C "foreign@admin" -f "$KEYS/key-foreign" >/dev/null 2>&1
if [ -f "$KEYS/key-a.pub" ] && [ -f "$KEYS/key-b.pub" ] && [ -f "$KEYS/key-foreign.pub" ]; then
  pass "夹具：三把 ed25519 公钥生成好了"
else
  echo "ssh-keygen 没生出钥匙来" >&2; exit 2
fi
KEY_A_PUB=$(cat "$KEYS/key-a.pub")
KEY_B_PUB=$(cat "$KEYS/key-b.pub")
KEY_FOREIGN_PUB=$(cat "$KEYS/key-foreign.pub")
FP_A=$(fingerprint_of "$KEYS/key-a.pub")
FP_B=$(fingerprint_of "$KEYS/key-b.pub")
FP_FOREIGN=$(fingerprint_of "$KEYS/key-foreign.pub")
expect_ne "钥匙 A 有指纹" "" "$FP_A"

# ---------------------------------------------------------------------------
echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
# ---------------------------------------------------------------------------

install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
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
    "logs": {}
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
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

expect_eq "页面在 /accounts 上有入口（SPA 返回 index.html）" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/accounts")"
expect_eq "页面在 /shares 上仍然有入口" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/shares")"

# ---------------------------------------------------------------------------
echo
echo "== 2. 能力报告与起点（账号列表读的是同一张 managed_accounts） =="
# ---------------------------------------------------------------------------

expect_eq "账号列表可读" "200" "$(get_accounts cap)"
cp "$WORK/accounts-cap.json" "$PAYLOADS/accounts-capability.json"
expect_eq "capability.status 是 available" "available" "$(aget cap 'd["data"]["capability"]["status"]')"
# 这三条锁的是**能力的来源**：账号页读的必须是账号自己的能力（useradd/getent/
# chpasswd、两档壳、visudo），不是 share.accounts 带回来的 Samba 能力。曾经混过
# 一次——这台 VM 没装 Samba，于是账号页显示"不支持账号管理"（DECISIONS Q477/Q478）。
expect_eq "能力报告问的是账号的工具（不是 Samba 的）" "/usr/sbin/useradd"   "$(aget cap 'd["data"]["capability"]["tools"]["useradd"]')"
expect_eq "能力报告没混进 Samba 的工具" "False"   "$(aget cap '"smbd" in d["data"]["capability"]["tools"]')"
expect_eq "两档壳都可用" "['disabled', 'restricted']"   "$(aget cap 'sorted(d["data"]["capability"]["shell_profiles"])')"
expect_eq "visudo 在，sudo 规则可写" "available"   "$(aget cap 'd["data"]["capability"]["sudo_rule_write"]')"
expect_eq "起点：一个受管理账号都没有" "0" "$(acct_count cap)"
expect_eq "起点：列表里没有 $ACCT" "" "$(acct_id cap "$ACCT")"

# ---------------------------------------------------------------------------
echo
echo "== 3. 建普通账号：口令经秘密通道，uid 由系统分配 =="
# ---------------------------------------------------------------------------

# POST /secrets 自己也要求 5 分钟内的再认证（与任务链路的 reauth 是两个入口，同一个窗口）。
expect_eq "秘密通道的再认证" "200" "$(reauth)"
printf '{"purpose":"account_password","value":"%s","reference":"account:%s"}' \
  "$ACCT_PASSWORD" "$ACCT" >"$TMPD/sec.json"
SEC_CODE=$(curl -s -b "$JAR" -o "$WORK/secret.json" -w '%{http_code}' -X POST "$BASE$SECRETS_PATH" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -d @"$TMPD/sec.json")
cp "$WORK/secret.json" "$PAYLOADS/secret.json"
expect_eq "口令交给秘密通道" "201" "$SEC_CODE"
SECRET_REF=$(jget "$WORK/secret.json" 'd["data"]["secret_ref"]')
expect_id "口令拿到了 sec_ 引用" "$SECRET_REF"
expect_eq "响应里只有引用，没有明文" "False" \
  "$(jget "$WORK/secret.json" "any('$ACCT_PASSWORD' in str(v) for v in d['data'].values())")"

DEF_ALICE=$(printf '{"name":"%s","display_name":"爱丽丝的登录账号","type":"regular","disabled":false,"shell_profile":"restricted","password_secret_ref":"%s"}' "$ACCT" "$SECRET_REF")
run_action account.apply "{\"account_id\":null,\"version\":null,\"definition\":$DEF_ALICE}" create-alice
expect_eq "建账号任务终态" "succeeded" "$JOB_STATE"
expect_eq "建账号结果 verified" "verified" "$(joutcome)"
ALICE_ID=$(jobdata account_id)
expect_id "结果里带 sac_ 标识" "$ALICE_ID"

# 独立证据：系统自己怎么说。
_uid=$(acct_uid "$ACCT")
if [ -n "$_uid" ] && [ "$_uid" -ge "$MIN_UID" ] && [ "$_uid" -le "$MAX_UID" ]; then
  pass "独立证据：getent 读出的 uid=$_uid 落在 $MIN_UID–$MAX_UID"
else
  fail "独立证据：uid 落在受管区间" "getent 给的是 '$_uid'"
fi
expect_eq "独立证据：登录壳是 /bin/rbash（restricted 档）" "/bin/rbash" "$(acct_shell "$ACCT")"
ALICE_HOME=$(acct_home "$ACCT")
case $ALICE_HOME in
  "$HOME_BASE/$ACCT") HOME_OK=True ;;
  *) HOME_OK=False ;;
esac
expect_eq "独立证据：home 在 /home 之下" "True" "$HOME_OK"
expect_eq "独立证据：home 目录真的建出来了" "True" \
  "$(test -d "$ALICE_HOME" && echo True || echo False)"
expect_eq "独立证据：passwd -S 读到 P（口令已设）" "P" "$(passwd_state "$ACCT")"
# 建账号这条路报的是 `password_state`（passwd -S 的那一列）与 `has_password`；
# `password_changed` 是**改**账号时才有的字段。第一轮把名字写错，红在这里——
# 断言要跟着接口的键走，不能跟着"我以为它叫什么"走。
expect_eq "执行器读回 password_state=P" "P" "$(jobobserved password_state)"
expect_eq "执行器读回 has_password=true" "True" "$(jobobserved has_password)"
expect_eq "执行器读回 uid 与 getent 一致" "$_uid" "$(jobobserved uid)"

get_accounts created >/dev/null
expect_eq "面板登记了 $ACCT" "1" "$(acct_count created)"
expect_eq "登记的形状是 regular" "regular" "$(acct_field created "$ACCT" 'a["type"]')"
expect_eq "登记的 shell_profile 是 restricted" "restricted" "$(acct_field created "$ACCT" 'a["shell_profile"]')"
expect_eq "登记说 has_password=true" "True" "$(acct_field created "$ACCT" 'a["has_password"]')"
expect_eq "登记里没有密码字段" "False" \
  "$(aget created "any(k in a for a in d[\"data\"][\"accounts\"] for k in (\"password\",\"secret_ref\",\"password_hash\"))")"
ALICE_VER=$(acct_version created "$ACCT")
expect_ne "拿到了 alice 的版本号" "" "$ALICE_VER"

# 名单外的名字拿不到账号：保留名在代理那边有一份闭集。
preflight_only account.apply \
  '{"account_id":null,"version":null,"definition":{"name":"root","type":"regular","disabled":false,"shell_profile":"restricted"}}' reserved-root
expect_eq "root 是保留名，判决层拒绝" "False" "$PRE_ALLOWED"
expect_contains "拒绝码是 ACCOUNT_RESERVED" "ACCOUNT_RESERVED" "$PRE_BLOCKERS"
get_accounts after-root >/dev/null
expect_eq "保留名被拒之后账号列表没变（只多了 alice 那一行）" "1" "$(acct_count after-root)"

preflight_only account.apply \
  '{"account_id":null,"version":null,"definition":{"name":"_apt2","type":"regular","disabled":false,"shell_profile":"restricted"}}' bad-name
expect_eq "下划线开头的名字被拒" "False" "$PRE_ALLOWED"
expect_contains "拒绝码是 ACCOUNT_NAME_INVALID" "ACCOUNT_NAME_INVALID" "$PRE_BLOCKERS"

# ---------------------------------------------------------------------------
echo
echo "== 4. authorized_keys 托管区：只碰自己那几行 =="
# ---------------------------------------------------------------------------

ALICE_HOME=$(acct_home "$ACCT")
install -d -m 0700 "$ALICE_HOME/.ssh"
# 人手写的一条外来钥匙：面板既不能吃掉它，也不能改它。
printf '%s\n' "$KEY_FOREIGN_PUB" >"$ALICE_HOME/.ssh/authorized_keys"
chmod 0600 "$ALICE_HOME/.ssh/authorized_keys"
chown -R "$ACCT" "$ALICE_HOME/.ssh"
FOREIGN_SHA_BEFORE=$(sha_of "$ALICE_HOME/.ssh/authorized_keys")
pass "夹具：先放一条人手写的外来钥匙（$FP_FOREIGN）"

run_action ssh_key.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"key_id\":null,\"public_key\":\"$KEY_A_PUB\"}" key-a
expect_eq "加第一把公钥任务终态" "succeeded" "$JOB_STATE"
expect_eq "加第一把公钥结果 verified" "verified" "$(joutcome)"
expect_eq "执行器读回的指纹是钥匙 A" "$FP_A" "$(jobobserved fingerprint)"
expect_eq "执行器读回托管区里有 1 把" "1" "$(jobobserved managed_keys)"
KEY_A_ID=$(jobdata key_id)
expect_id "结果里带 ssk_ 标识" "$KEY_A_ID"

expect_eq "独立证据：托管区标记存在" "True" "$(has_block "$ACCT")"
expect_eq "独立证据：托管区里有 1 行" "1" "$(managed_count "$ACCT")"
expect_eq "独立证据：托管区里就是钥匙 A" "True" \
  "$(managed_lines "$ACCT" | grep -qxF "$KEY_A_PUB" && echo True || echo False)"
expect_eq "独立证据：人手写的那条外来钥匙逐字节还在" "True" "$(foreign_line_intact "$ACCT" "$KEY_FOREIGN_PUB")"
expect_eq "独立证据：文件权限 0600" "600" "$(file_mode "$ALICE_HOME/.ssh/authorized_keys")"
expect_eq "独立证据：.ssh 目录权限 0700" "700" "$(file_mode "$ALICE_HOME/.ssh")"
# 文件里不止一把钥匙（还有一条外来的），`ssh-keygen -lf` 会一行一把地打出来，
# 所以判据是"这一堆指纹里有钥匙 A"，不是"整份输出等于钥匙 A"。第一轮写成后者，
# 结果拿两行输出比一行，红得毫无信息量。
expect_eq "独立证据：ssh-keygen 从文件里读到的指纹包含钥匙 A" "True" \
  "$(fp_listed "$ALICE_HOME/.ssh/authorized_keys" "$FP_A")"
expect_ne "独立证据：整份文件确实变了（不是没写）" "$FOREIGN_SHA_BEFORE" \
  "$(sha_of "$ALICE_HOME/.ssh/authorized_keys")"

run_action ssh_key.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"key_id\":null,\"public_key\":\"$KEY_B_PUB\"}" key-b
expect_eq "加第二把公钥任务终态" "succeeded" "$JOB_STATE"
KEY_B_ID=$(jobdata key_id)
expect_eq "执行器读回托管区里有 2 把" "2" "$(jobobserved managed_keys)"
expect_eq "独立证据：托管区里有 2 行" "2" "$(managed_count "$ACCT")"
expect_eq "独立证据：两把钥匙都在" "True" \
  "$(managed_lines "$ACCT" | grep -qxF "$KEY_B_PUB" && managed_lines "$ACCT" | grep -qxF "$KEY_A_PUB" && echo True || echo False)"
expect_eq "独立证据：外来钥匙仍然逐字节还在" "True" "$(foreign_line_intact "$ACCT" "$KEY_FOREIGN_PUB")"

# 同一把钥匙再加一次：判决层必须认出重复，不是又写一行。
preflight_only ssh_key.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"key_id\":null,\"public_key\":\"$KEY_A_PUB\"}" key-dup
expect_eq "重复的公钥被拒" "False" "$PRE_ALLOWED"
expect_contains "拒绝码是 SSH_KEY_DUPLICATE" "SSH_KEY_DUPLICATE" "$PRE_BLOCKERS"
expect_eq "重复被拒之后托管区还是 2 行" "2" "$(managed_count "$ACCT")"

KEYS_CODE=$(get_keys "$ALICE_ID" a)
expect_eq "公钥列表可读" "200" "$KEYS_CODE"
expect_eq "列表里有 2 条登记" "2" "$(kget a 'len(d["data"]["ssh_keys"])')"
expect_eq "列表里的指纹是 SSH 的 SHA256 形状" "True" \
  "$(kget a "all(k[\"fingerprint\"].startswith(\"SHA256:\") for k in d[\"data\"][\"ssh_keys\"])")"
expect_eq "列表响应里没有密钥体" "False" \
  "$(kget a 'any("AAAAC3" in str(k) for k in d["data"]["ssh_keys"])')"

run_action ssh_key.delete \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"key_id\":\"$KEY_A_ID\"}" key-a-del
expect_eq "删第一把公钥任务终态" "succeeded" "$JOB_STATE"
expect_eq "执行器读回托管区只剩 1 把" "1" "$(jobobserved managed_keys)"
expect_eq "独立证据：托管区里只剩钥匙 B" "True" \
  "$(managed_lines "$ACCT" | grep -qxF "$KEY_B_PUB" && ! managed_lines "$ACCT" | grep -qxF "$KEY_A_PUB" && echo True || echo False)"
expect_eq "独立证据：外来钥匙仍然逐字节还在" "True" "$(foreign_line_intact "$ACCT" "$KEY_FOREIGN_PUB")"
expect_eq "独立证据：删一把之后权限仍是 0600" "600" "$(file_mode "$ALICE_HOME/.ssh/authorized_keys")"

# ---------------------------------------------------------------------------
echo
echo "== 5. 受限 sudo：写文件 → visudo → sudo -l 读回 → 真跑一次 =="
# ---------------------------------------------------------------------------

run_action sudo_rule.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"template_id\":\"system_status\"}" sudo-status
expect_eq "下发 system_status 任务终态" "succeeded" "$JOB_STATE"
expect_eq "下发结果 verified" "verified" "$(joutcome)"
expect_eq "执行器读回写入了 1 条授权" "1" "$(jobobserved commands)"
expect_eq "执行器读回 sudo -l -U 有 1 条" "1" "$(jobobserved sudo_rules)"

SUDO_FILE=$(sudoers_of "$ACCT")
expect_eq "独立证据：规则文件就在 sudoers.d 里" "True" \
  "$(test -f "$SUDO_FILE" && echo True || echo False)"
expect_eq "独立证据：文件权限 0440" "440" "$(file_mode "$SUDO_FILE")"
expect_eq "独立证据：文件里有认领标记" "True" "$(sudoers_has "$ACCT" '# upanel:')"
expect_eq "独立证据：文件里只有模板那一条命令、没有通配符" "True" \
  "$(sudoers_text "$ACCT" | grep -qF "NOPASSWD: /usr/bin/systemctl status --no-pager" && ! sudoers_text "$ACCT" | grep -qF '*' && echo True || echo False)"
expect_eq "独立证据：整份 sudo 配置通过 visudo -c" "0" "$(visudo_all_rc)"
expect_eq "独立证据：sudo -l -U 里出现模板命令" "True" \
  "$(sudo_list_has "$ACCT" '/usr/bin/systemctl status --no-pager')"
expect_eq "核心：真跑一次模板里的命令（runuser 换到 $ACCT，sudo -n）" "0" \
  "$(runuser -u "$ACCT" -- sudo -n /usr/bin/systemctl status --no-pager >"$FACTS/sudo-status.out" 2>&1; echo $?)"
# 模板外的命令必须被拒：这一条才说明"授权是那一条，不是随便什么"。
#
# sudo 的拒绝有两种说法：没有任何规则匹配时是 "Sorry, user … is not allowed to execute …"，
# 有规则但需要口令时是 "sudo: a password is required"。两种都算拒绝，所以这里断的是
# "退出码非 0 + 明文里说明了理由"，而不是咬死某一句话。
if runuser -u "$ACCT" -- sudo -n /usr/bin/systemctl restart cron >"$FACTS/sudo-restart.out" 2>&1; then
  fail "模板外的命令被拒" "runuser ... systemctl restart cron 竟然成功了"
else
  pass "模板外的命令被拒（退出码非 0）"
fi
REJECT_TEXT=$(cat "$FACTS/sudo-restart.out" 2>/dev/null | tr '\n' ' ')
case $REJECT_TEXT in
  *"not allowed"*|*"password is required"*) pass "被拒的理由来自 sudo 自己" ;;
  *) fail "被拒的理由来自 sudo 自己" "$REJECT_TEXT" ;;
esac
expect_eq "核心：sudo -l -U 里恰好只有模板那 1 条授权" "1" "$(jobobserved sudo_rules)"

# ---------------------------------------------------------------------------
echo
echo "== 6. 真机制造一次 visudo 失败：旧规则必须逐字节保留 =="
# ---------------------------------------------------------------------------

# 一个坏文件让**整份** `visudo -c` 失败。它不在面板的命名空间里（没有 upanel- 前缀），
# 所以面板不会去动它——这正是"别人的文件坏了，面板拒绝往里加一条"的现实场景。
BROKEN=$SUDOERS_DIR/zz-p145-broken
OLD_SHA=$(sha_of "$SUDO_FILE")
OLD_TEXT=$(sudoers_text "$ACCT")
printf 'this is not a sudoers line\n' >"$BROKEN"
chmod 0440 "$BROKEN"
expect_ne "独立证据：整份 visudo -c 现在过不去" "0" "$(visudo_all_rc)"
note "坏文件：$BROKEN（内容是 'this is not a sudoers line'）"

run_action sudo_rule.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"template_id\":\"journal_recent\"}" sudo-broken
expect_eq "坏配置下的下发任务失败（不是成功）" "failed" "$JOB_STATE"
expect_contains "失败原因带 SUDO_VISUDO_FAILED" "SUDO_VISUDO_FAILED" "$(joberr)"
expect_eq "核心：旧规则文件哈希与失败前一致" "$OLD_SHA" "$(sha_of "$SUDO_FILE")"
expect_eq "核心：旧规则内容逐字节没变" "$OLD_TEXT" "$(sudoers_text "$ACCT")"
expect_eq "核心：没有留下候选临时文件" "0" \
  "$(find "$SUDOERS_DIR" -maxdepth 1 -name '.upanel-*' | wc -l | tr -d ' ')"
# 注意：**坏文件还在的时候不能跑 sudo -l** —— sudo 自己会因为整份配置解析失败而拒绝
# 工作，那一步的失败与"面板有没有恢复旧规则"无关，拿它当断言是假红。先断言文件，
# 移走坏文件之后再拿 sudo 读回。

rm -f "$BROKEN"
expect_eq "独立证据：移走坏文件之后 visudo -c 又通过了" "0" "$(visudo_all_rc)"
expect_eq "核心：sudo -l -U 读回仍然是旧模板那一条" "True" \
  "$(sudo_list_has "$ACCT" '/usr/bin/systemctl status --no-pager')"
run_action sudo_rule.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"template_id\":\"journal_recent\"}" sudo-journal
expect_eq "移走坏文件之后同样的下发成功" "succeeded" "$JOB_STATE"
expect_eq "这次的 verdict 是 verified" "verified" "$(joutcome)"
expect_eq "独立证据：sudo -l -U 读回新模板那一条" "True" \
  "$(sudo_list_has "$ACCT" '/usr/bin/journalctl -n 200 --no-pager')"
expect_eq "独立证据：旧模板那一条已经不在" "False" \
  "$(sudo_list_has "$ACCT" '/usr/bin/systemctl status --no-pager')"
expect_eq "核心：真跑一次新模板里的命令" "0" \
  "$(runuser -u "$ACCT" -- sudo -n /usr/bin/journalctl -n 200 --no-pager >"$FACTS/sudo-journal.out" 2>&1; echo $?)"

# ---------------------------------------------------------------------------
echo
echo "== 7. 面板不覆盖别人的文件：手改过的规则文件 =="
# ---------------------------------------------------------------------------

cp "$SUDO_FILE" "$WORK/logs/sudoers-alice-good"
printf '# 管理员手改过这一份，认领标记被删掉了\n%s ALL = (root) NOPASSWD: /bin/true\n' "$ACCT" >"$SUDO_FILE"
chmod 0440 "$SUDO_FILE"
HAND_SHA=$(sha_of "$SUDO_FILE")
if visudo -c >/dev/null 2>&1; then
  pass "手改后的文件本身是合法的 sudoers（所以拒绝只能来自认领标记）"
else
  fail "手改后的文件本身是合法的 sudoers" "$(visudo_all)"
fi
preflight_only sudo_rule.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"template_id\":\"journal_recent\"}" sudo-handedit
expect_eq "手改过的文件：判决层拒绝" "False" "$PRE_ALLOWED"
expect_contains "拒绝码是 SUDO_FILE_DAMAGED" "SUDO_FILE_DAMAGED" "$PRE_BLOCKERS"
expect_eq "核心：手改的文件被逐字节保留" "$HAND_SHA" "$(sha_of "$SUDO_FILE")"
preflight_only sudo_rule.delete \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\"}" sudo-handedit-del
expect_eq "手改过的文件：删除也被拒" "False" "$PRE_ALLOWED"
expect_contains "删除的拒绝码同样是 SUDO_FILE_DAMAGED" "SUDO_FILE_DAMAGED" "$PRE_BLOCKERS"
cp "$WORK/logs/sudoers-alice-good" "$SUDO_FILE"
chmod 0440 "$SUDO_FILE"
expect_eq "复原之后 sudo -l -U 又读到面板那条规则" "True" \
  "$(sudo_list_has "$ACCT" '/usr/bin/journalctl -n 200 --no-pager')"

# ---------------------------------------------------------------------------
echo
echo "== 8. 禁用 / 启用：换壳，不用 -L =="
# ---------------------------------------------------------------------------

get_accounts pre-disable >/dev/null
ALICE_VER=$(acct_version pre-disable "$ACCT")
DEF_DISABLED=$(printf '{"name":"%s","display_name":"爱丽丝的登录账号","type":"regular","disabled":true,"shell_profile":"restricted"}' "$ACCT")
run_action account.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"definition\":$DEF_DISABLED}" disable-alice
expect_eq "禁用任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：登录壳变成 nologin" "/usr/sbin/nologin" "$(acct_shell "$ACCT")"
expect_eq "执行器读回 shell_changed=true" "True" "$(jobobserved shell_changed)"
# 口令**还在**：禁用是换壳，不是 usermod -L（-U 会给从未设过口令的账号写一个空口令）。
expect_eq "独立证据：口令状态仍然是 P（没用 -L）" "P" "$(passwd_state "$ACCT")"
expect_eq "独立证据：公钥文件没被禁用动作带走" "True" "$(has_block "$ACCT")"

get_accounts disabled >/dev/null
ALICE_VER=$(acct_version disabled "$ACCT")
expect_eq "登记的 disabled=true" "True" "$(acct_field disabled "$ACCT" 'a["disabled"]')"
expect_eq "登记的 shell 是 nologin、shell_profile 仍记着 restricted" "True" \
  "$(acct_field disabled "$ACCT" 'a["shell"] == "/usr/sbin/nologin" and a["shell_profile"] == "restricted"')"
DEF_ENABLED=$(printf '{"name":"%s","display_name":"爱丽丝的登录账号","type":"regular","disabled":false,"shell_profile":"restricted"}' "$ACCT")
run_action account.apply \
  "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\",\"definition\":$DEF_ENABLED}" enable-alice
expect_eq "启用任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：登录壳回到 rbash" "/bin/rbash" "$(acct_shell "$ACCT")"

# ---------------------------------------------------------------------------
echo
echo "== 9. 第二个账号：没有口令的那一个（公钥那一面留着第 11 节验） =="
# ---------------------------------------------------------------------------

DEF_BOB=$(printf '{"name":"%s","display_name":"没有口令的账号","type":"regular","disabled":false,"shell_profile":"restricted"}' "$ACCT2")
run_action account.apply "{\"account_id\":null,\"version\":null,\"definition\":$DEF_BOB}" create-bob
expect_eq "建 bob 任务终态" "succeeded" "$JOB_STATE"
BOB_ID=$(jobdata account_id)
expect_id "bob 的 sac_ 标识" "$BOB_ID"
expect_ne "独立证据：passwd -S 不是 P（没有口令）" "P" "$(passwd_state "$ACCT2")"
get_accounts bob >/dev/null
expect_eq "登记说 bob 没有口令" "False" "$(acct_field bob "$ACCT2" 'a["has_password"]')"
BOB_VER=$(acct_version bob "$ACCT2")
run_action ssh_key.apply \
  "{\"account_id\":\"$BOB_ID\",\"version\":\"$BOB_VER\",\"key_id\":null,\"public_key\":\"$KEY_A_PUB\"}" key-bob
expect_eq "给 bob 加公钥任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：bob 的托管区里有 1 把" "1" "$(managed_count "$ACCT2")"

# 共享账号拿不到 sudo。共享账号要靠 Samba 那条动作建，没装 samba 就记 NOT RUN。
if command -v pdbedit >/dev/null 2>&1 && command -v smbpasswd >/dev/null 2>&1; then
  SHARE_ACCT=p145share
  # 共享账号的口令走 purpose=smb_password（用途闭集里它是另一条），不能拿账号口令那份引用顶。
  expect_eq "共享账号口令的再认证" "200" "$(reauth)"
  printf '{"purpose":"smb_password","value":"%s","reference":"share:%s"}' \
    "$ACCT_PASSWORD" "$SHARE_ACCT" >"$TMPD/sec-share.json"
  SHARE_SEC_CODE=$(curl -s -b "$JAR" -o "$WORK/secret-share.json" -w '%{http_code}' -X POST "$BASE$SECRETS_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -d @"$TMPD/sec-share.json")
  expect_eq "共享账号口令交给秘密通道" "201" "$SHARE_SEC_CODE"
  SHARE_SECRET=$(jget "$WORK/secret-share.json" 'd["data"]["secret_ref"]')
  run_action share.account.apply \
    "{\"account_id\":null,\"name\":\"$SHARE_ACCT\",\"secret_ref\":\"$SHARE_SECRET\",\"disabled\":false}" share-acct
  if [ "$JOB_STATE" = "succeeded" ]; then
    get_accounts shareacct >/dev/null
    SHARE_ACCT_ID=$(acct_id shareacct "$SHARE_ACCT")
    SHARE_ACCT_VER=$(acct_version shareacct "$SHARE_ACCT")
    expect_ne "共享账号登记出来了" "" "$SHARE_ACCT_ID"
    preflight_only sudo_rule.apply \
      "{\"account_id\":\"$SHARE_ACCT_ID\",\"version\":\"$SHARE_ACCT_VER\",\"template_id\":\"system_status\"}" sudo-share
    expect_eq "共享账号拿不到 sudo" "False" "$PRE_ALLOWED"
    expect_contains "拒绝码是 SUDO_ACCOUNT_NOT_ELIGIBLE" "SUDO_ACCOUNT_NOT_ELIGIBLE" "$PRE_BLOCKERS"
    run_action account.delete \
      "{\"account_id\":\"$SHARE_ACCT_ID\",\"version\":\"$SHARE_ACCT_VER\"}" delete-share-acct
    expect_eq "清理共享账号" "succeeded" "$JOB_STATE"
  else
    notrun "共享账号不能拿 sudo" "share.account.apply 没成功（$JOB_STATE），这台机器的 Samba 不可用"
  fi
else
  notrun "共享账号不能拿 sudo" "没有 pdbedit/smbpasswd（samba-common-bin 未安装）；不为一条验收去装包"
fi

# ---------------------------------------------------------------------------
echo
echo "== 10. 删账号：userdel 不带 -r，home 里的东西一个不少 =="
# ---------------------------------------------------------------------------

# 先在 home 里放一个文件，删完要能在原地读到。
printf 'keep me\n' >"$ALICE_HOME/user-data.txt"
chown "$ACCT:$ACCT" "$ALICE_HOME/user-data.txt"
DATA_SHA=$(sha_of "$ALICE_HOME/user-data.txt")
get_accounts pre-delete >/dev/null
ALICE_VER=$(acct_version pre-delete "$ACCT")
run_action account.delete "{\"account_id\":\"$ALICE_ID\",\"version\":\"$ALICE_VER\"}" delete-alice
expect_eq "删账号任务终态" "succeeded" "$JOB_STATE"
expect_eq "删账号结果 verified" "verified" "$(joutcome)"
expect_eq "执行器读回 linux_removed=true" "True" "$(jobobserved linux_removed)"
expect_eq "独立证据：getent 读不到 $ACCT 了" "" "$(getent passwd "$ACCT" 2>/dev/null)"
expect_eq "独立证据：home 目录还在" "True" "$(test -d "$ALICE_HOME" && echo True || echo False)"
expect_eq "独立证据：home 里的文件还在、内容没变" "$DATA_SHA" "$(sha_of "$ALICE_HOME/user-data.txt")"
expect_eq "独立证据：sudoers 规则文件跟着撤掉了" "False" \
  "$(test -f "$SUDO_FILE" && echo True || echo False)"
expect_eq "独立证据：整份 sudo 配置仍然通过 visudo -c" "0" "$(visudo_all_rc)"
get_accounts gone >/dev/null
expect_eq "面板记录也删了" "" "$(acct_id gone "$ACCT")"

# ---------------------------------------------------------------------------
echo
echo "== 11. （有条件）公钥登录的三件事：钥匙能用、口令不是门槛、壳才是门槛 =="
# ---------------------------------------------------------------------------
#
# 首轮这里断言的是"没有口令的账号即使配了公钥也登不上"，**真机否了它**：
# publickey 认证根本不看 shadow 的口令字段，一个没有任何口令、只配了钥匙的账号
# （key-only 账号）此刻真的能登进来。这不是缺陷——key-only 是服务器上很常见的
# 用法，面板也不该拦它。真正拦人的是**壳**：禁用档把壳换成 nologin 之后，同一把
# 钥匙立刻失效。所以这一节分四面，而且最后两面把"登不上"归因到壳上，而不是
# 归因到钥匙本身或 sshd 的配置。

SSHD_OK=False
if systemctl is-active ssh >/dev/null 2>&1 || systemctl is-active sshd >/dev/null 2>&1; then
  SSHD_OK=True
fi
if [ "$SSHD_OK" != "True" ]; then
  notrun "只有钥匙、没有口令的账号能用那把钥匙登录" "这台 VM 上 sshd 没在跑（systemctl is-active ssh 不是 active）"
  notrun "禁用（换壳成 nologin）之后同一把钥匙登不上" "同上：没有 sshd 就没有这一面"
  notrun "撤掉公钥之后登不上" "同上：没有 sshd 就没有这一面"
else
  # bob 的 home 与 authorized_keys 已经在第 9 节准备好了；这里只是拿 ssh 去用它。
  SSH_OPTS="-F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o PreferredAuthentications=publickey -o IdentitiesOnly=yes -o ConnectTimeout=5"

  # 面一：bob 没有口令、配着钥匙 A。能登。
  if ssh $SSH_OPTS -i "$KEYS/key-a" "$ACCT2@127.0.0.1" true >"$FACTS/ssh-bob-keyonly.out" 2>&1; then
    pass "只有钥匙、没有口令的账号用钥匙能登（publickey 不看口令字段）"
  else
    fail "只有钥匙、没有口令的账号用钥匙能登"       "$(tail -3 "$FACTS/ssh-bob-keyonly.out" 2>/dev/null)"
  fi

  # 面二：禁用 bob——面板把壳换成 nologin。钥匙还在文件里，但登不上。
  get_accounts bob-off >/dev/null
  BOB_VER=$(acct_version bob-off "$ACCT2")
  DEF_BOB_OFF=$(printf '{"name":"%s","display_name":"没有口令的账号","type":"regular","disabled":true,"shell_profile":"restricted"}' "$ACCT2")
  run_action account.apply     "{\"account_id\":\"$BOB_ID\",\"version\":\"$BOB_VER\",\"definition\":$DEF_BOB_OFF}" bob-disable
  expect_eq "禁用 bob 的任务终态" "succeeded" "$JOB_STATE"
  expect_eq "独立证据：bob 的登录壳换成 nologin" "/usr/sbin/nologin" "$(acct_shell "$ACCT2")"
  expect_eq "独立证据：钥匙还在文件里（禁用没动 authorized_keys）" "1" "$(managed_count "$ACCT2")"
  if ssh $SSH_OPTS -i "$KEYS/key-a" "$ACCT2@127.0.0.1" true >"$FACTS/ssh-bob-disabled.out" 2>&1; then
    fail "核心：禁用之后同一把钥匙登不上" "竟然还登得上去——换壳没有生效"
  else
    pass "核心：禁用之后同一把钥匙登不上（换壳就是那道门）"
    note "sshd 的原文：$(tail -2 "$FACTS/ssh-bob-disabled.out" | tr '\n' ' ')"
  fi

  # 面三：启用回来，壳回到 restricted。同一把钥匙又能登——这一步把面二归因到壳上。
  get_accounts bob-on >/dev/null
  BOB_VER=$(acct_version bob-on "$ACCT2")
  DEF_BOB_ON=$(printf '{"name":"%s","display_name":"没有口令的账号","type":"regular","disabled":false,"shell_profile":"restricted"}' "$ACCT2")
  run_action account.apply     "{\"account_id\":\"$BOB_ID\",\"version\":\"$BOB_VER\",\"definition\":$DEF_BOB_ON}" bob-enable
  expect_eq "启用 bob 的任务终态" "succeeded" "$JOB_STATE"
  expect_eq "独立证据：登录壳回到 /bin/rbash" "/bin/rbash" "$(acct_shell "$ACCT2")"
  if ssh $SSH_OPTS -i "$KEYS/key-a" "$ACCT2@127.0.0.1" true >"$FACTS/ssh-bob-enabled.out" 2>&1; then
    pass "启用之后同一把钥匙又能登（说明上一条的拒绝来自壳）"
  else
    fail "启用之后同一把钥匙又能登" "$(tail -3 "$FACTS/ssh-bob-enabled.out" 2>/dev/null)"
  fi

  # 面四：撤掉公钥之后必须登不上：托管区是真的在生效，不是写着好看。
  get_accounts bob-keydel >/dev/null
  BOB_VER=$(acct_version bob-keydel "$ACCT2")
  get_keys "$BOB_ID" bob >/dev/null
  BOB_KEY_ID=$(kget bob 'd["data"]["ssh_keys"][0]["id"]')
  run_action ssh_key.delete     "{\"account_id\":\"$BOB_ID\",\"version\":\"$BOB_VER\",\"key_id\":\"$BOB_KEY_ID\"}" bob-key-del
  expect_eq "撤掉 bob 的公钥" "succeeded" "$JOB_STATE"
  if ssh $SSH_OPTS -i "$KEYS/key-a" "$ACCT2@127.0.0.1" true >"$FACTS/ssh-bob-nokey.out" 2>&1; then
    fail "撤掉公钥之后登不上" "竟然还能登"
  else
    pass "撤掉公钥之后登不上（托管区在生效）"
  fi
fi

# ---------------------------------------------------------------------------
echo "== 12. 收尾：把这一轮造的东西全撤掉 =="
# ---------------------------------------------------------------------------

get_accounts pre-clean >/dev/null
BOB_VER=$(acct_version pre-clean "$ACCT2")
if [ -n "$BOB_ID" ]; then
  run_action account.delete "{\"account_id\":\"$BOB_ID\",\"version\":\"$BOB_VER\"}" delete-bob
  expect_eq "删 bob 任务终态" "succeeded" "$JOB_STATE"
fi
remove_account_quietly "$ACCT"
remove_account_quietly "$ACCT2"
restore_sudoers
expect_eq "收尾：sudoers.d 里没有 upanel 文件" "0" \
  "$(find "$SUDOERS_DIR" -maxdepth 1 -name 'upanel-*' | wc -l | tr -d ' ')"
expect_eq "收尾：整份 sudo 配置通过 visudo -c" "0" "$(visudo_all_rc)"
get_accounts final >/dev/null
expect_eq "收尾：账号列表里没有这两个名字" "0" \
  "$(aget final "len([a[\"name\"] for a in d[\"data\"][\"accounts\"] if a[\"name\"] in (\"$ACCT\",\"$ACCT2\")])")"
ls -la "$SUDOERS_DIR" >"$FACTS/sudoers.d-final.txt" 2>&1
getent passwd | awk -F: '$3 >= 1000 && $3 <= 59999' >"$FACTS/human-uids-final.txt" 2>&1

# ---------------------------------------------------------------------------
echo
echo "== 13. 结论 =="
# ---------------------------------------------------------------------------

notrun "共享账号 + Samba passdb 的那一半" "本脚本只在装了 samba-common-bin 时才验；共享账号的 SMB 认证属于 P14.3 的验收面"
notrun "两条公钥动作并发写同一份 authorized_keys" "锁行为由 internal/accounts/sshkey_test.go 与 shares 的锁单测覆盖；真机并发会把冲突变成时序问题"
notrun "第 11 把公钥被拒（上限 10）" "上限判定在判决层，单测覆盖；真机验到 2 把即可"
notrun "home 在别处 / home 是符号链接的账号" "面板对这类账号是拒绝写（SSH_KEY_HOME_INVALID），只在单测里造过；真机没有造这种磁盘布局"
notrun "面板自己的 TOTP 与登录策略" "属于 P14.6"

printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
write_result
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
