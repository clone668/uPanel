#!/bin/sh
# P12.1 Linux 验收脚本：restic 本地/SFTP 仓库、秘密引用、SFTP 主机密钥验证、
# 仓库初始化与容量检测。
#
# 要证明的不是"接口返回 200"，而是下面这些**很容易被糊过去**的事实：
#
#   1) **仓库 id 是读回来的，不是编的**：初始化之后任务结果是 verified，而
#      observed_state.repo_id 必须与脚本自己用同样口令、同样 ssh 命令跑
#      `restic cat config` 读到的那一串 64 位十六进制逐字一致。两者对不上时
#      "仓库建好了"只是一句话。
#   2) **只初始化空仓库，不覆盖已有数据**：目标位置已经是仓库时，预检必须给出
#      BACKUP_REPOSITORY_EXISTS 并拒绝——不是靠 restic 自己拒绝，而是预检先拒绝。
#   3) **SFTP 主机密钥是我们核对的**：登记正确指纹能连上；登记错误的指纹必须被
#      拒绝（BACKUP_HOST_KEY_MISMATCH，且错误里同时给出登记值与实际值）；用一把
#      没被授权的私钥必须被拒绝（BACKUP_SFTP_AUTH）。三条各造一个仓库定义跑一遍，
#      不共用结论。
#   4) **秘密按用途把关**：拿一张 TLS 私钥当仓库口令必须是 BACKUP_SECRET_PURPOSE；
#      引用不存在必须是 BACKUP_SECRET_UNRESOLVED。两者都发生在预检，而不是在
#      restic 吐出一堆英文之后。
#   5) **容量是读来的**：本地走 statfs、SFTP 走 statvfs，读到的是真实数字；
#      读不到就报"读不到"，不报 0。
#   6) **默认拒绝**：非 upanel-web 的身份（包括 root）碰不到仓库镜像路由。
#   7) **known_hosts 是我们写的**：初始化之后 StateDir 下出现这次连接的 known_hosts，
#      内容与 ssh-keyscan 扫到的指纹一致。
#
# 用法（root，在验收虚拟机上）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p12-1-backup-acceptance.sh
#
# 环境变量：BIN_DIR（默认 /var/tmp/upanel-bin）、WORK（默认 /var/tmp/upanel-accept-p12-1）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制非 ELF / 没有 restic /
# 没有 ssh / sshd 没监听 / 没有 upanel-web 账号）。
#
# 覆盖的验收点（STAGES §14 P12.1、CONTRACTS §4/§5/§12/§15、PLAN §17）：
#   能力表如实报告 backup.repository.initialize / .state、镜像路由的默认拒绝与乐观并发、
#   非法定义与越界路径在参数层被拒、现状查询、四种预检拒绝、SFTP 与本地两种后端的初始化、
#   仓库 id 独立核对、容量前后对比、已存在仓库不再初始化、口令错的现状查询、
#   未登记 id 与陈旧版本的预检阻塞、动作参数的严格解码。
#
# 明确不覆盖（NOT RUN，原因写在这里，不假装通过）：
#   - **备份与恢复**（写快照、还原、校验一致）：P12.2/P12.3 才实现。BACKUP-01 的
#     "备份后恢复"要到那时才有东西可验。
#   - **备份跑到一半网络断开**：SFTP 那条路径在这里只验"连不上/认证失败时预检拒绝"，
#     "备份中途掉线"属于 P12.2 的备份执行。
#   - **存储满**：要真把验收机的文件系统填满（会连 apt/journal 一起弄坏），或者挂一个
#     loop 设备（要动 /etc/fstab）。该判决由 internal/backup 的 capacityVerdict 单测覆盖。
#   - **目标路径存在但不是目录**：与文件模块的用例重复，由评估逻辑单测覆盖。
#   - **restic 版本差异带来的行为分歧**：22.04 有 0.12.1、24.04 有 0.16.4，脚本把两个
#     版本的原文都记进 payloads/restic-versions.txt，但不把"两个版本行为一致"当验收点。
#   - **浏览器渲染**：VM 上没有浏览器，本阶段也没有 Web 侧备份页面。
#
# 证据卫生：脚本跑完会把 payloads 里含秘密明文的文件就地替换成一行说明——仓库口令与
# 测试私钥都不进留下的证据（product 侧那条"秘密不进 argv/日志"是另一件事，由单测锁）。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
WORK=${WORK:-/var/tmp/upanel-accept-p12-1}
AGENT="$BIN_DIR/upanel-agent"
CTL="$BIN_DIR/upanelctl"
WEB_USER=upanel-web
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
STATE_DIR=/var/lib/upanel-agent/backup
ROOT_PATH=$WORK/data
TMPD=/tmp/p12-1.$$
REPO_PASSWORD='p12-1-correct-horse-9'
WRONG_PASSWORD='p12-1-wrong-horse-1'
REMOTE_DIR=/home/$WEB_USER/p12-1-repo
LOCAL_REL=backups/local-nightly
SSH_DIR=$WORK/ssh

PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""
AGENT_PID=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; printf 'PASS\t%s\n' "$1" >>"$WORK/logs/results.txt"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
  printf 'FAIL\t%s\t%s\n' "$1" "${2:-}" >>"$WORK/logs/results.txt"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; printf 'NOTRUN\t%s\t%s\n' "$1" "$2" >>"$WORK/logs/results.txt"; }
note() { printf '      %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "期望包含 '$2'，实际 '$3'" ;;
  esac
}
expect_ge() { if [ "$2" -ge "$3" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 >= $3，实际 '$2'"; fi; }
expect_le() { if [ "$2" -le "$3" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 <= $3，实际 '$2'"; fi; }

# jget <文件> <python 表达式，d 为解析后的 JSON>
#
# 表达式本身出错时打印 ERR:<类型>:<消息>：空字符串会让"表达式写错"看起来像
# "响应里没这个字段"，而这两件事的下一步完全不同。
jget() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
try:
    print(eval(sys.argv[2], {"d": d, "json": json}))
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))' "$1" "$2" 2>/dev/null
}

cleanup() {
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  rm -rf "$TMPD"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# ---------------------------------------------------------------- 环境检查
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi
for f in "$AGENT" "$CTL"; do
  if [ ! -x "$f" ]; then printf '缺少可执行文件：%s\n' "$f" >&2; exit 2; fi
  # 拿 Windows 编出来的 PE 拷上来时，症状是 `Exec format error`：代理"没起来"，
  # 然后是十几个连锁失败，全都在别处。四字节魔术数字把它按在现场。
  if [ "$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')" != "7f454c46" ]; then
    printf '%s 不是本机的 ELF 可执行文件（拿到的是别的平台的构建产物）\n' "$f" >&2
    exit 2
  fi
done
# 代理把 restic 与 ssh 的路径当常量（backup.DefaultPaths），这两个绝对路径必须存在。
if [ ! -x /usr/bin/restic ]; then
  echo '本机没有 /usr/bin/restic（apt install restic），P12.1 的验收无法进行' >&2
  exit 2
fi
if [ ! -x /usr/bin/ssh ]; then
  echo '本机没有 /usr/bin/ssh，SFTP 后端的验收无法进行' >&2
  exit 2
fi
if ! command -v ssh-keyscan >/dev/null 2>&1; then
  echo '本机没有 ssh-keyscan，拿不到服务端主机密钥指纹' >&2
  exit 2
fi
if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  printf '本机没有 %s 账号（先装一遍 0.1 包）\n' "$WEB_USER" >&2
  exit 2
fi
if ! ss -ltnH 2>/dev/null | grep -q ':22 '; then
  echo '本机 sshd 没有监听 22，SFTP 后端的验收无法进行' >&2
  exit 2
fi
if pgrep -x upanel-agent >/dev/null 2>&1; then
  printf '这台机器上已经有 upanel-agent 进程在跑，先停掉再验收（否则 socket 归属不明）\n' >&2
  exit 2
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/payloads" "$SSH_DIR" "$ROOT_PATH"
chmod 0755 "$WORK"
mkdir -p "$TMPD"
chmod 1777 "$TMPD"   # runuser 要以 upanel-web 的身份在里面写响应文件

RESULTS=$WORK/logs/results.txt
AGENT_LOG=$WORK/logs/agent.log
PAYLOADS=$WORK/payloads
: >"$RESULTS"

# web_curl <方法> <路径> <输出文件> [请求体文件]：回显 HTTP 状态码。
#
# 代理的普通路由只认 upanel-web 的 SO_PEERCRED，所以必须以该用户去连。
web_curl() {
  _m=$1; _p=$2; _o=$3; _b=${4:-}
  _tmp=$TMPD/out.json
  rm -f "$_tmp"
  if [ -n "$_b" ]; then
    CODE=$(runuser -u "$WEB_USER" -- curl -s -o "$_tmp" -w '%{http_code}' -X "$_m" \
      --unix-socket "$SOCK" -H 'Content-Type: application/json' \
      --data-binary "@$_b" "http://agent$_p" 2>/dev/null)
  else
    CODE=$(runuser -u "$WEB_USER" -- curl -s -o "$_tmp" -w '%{http_code}' -X "$_m" \
      --unix-socket "$SOCK" "http://agent$_p" 2>/dev/null)
  fi
  if [ -f "$_tmp" ]; then cp "$_tmp" "$_o" 2>/dev/null; fi
  [ -f "$_o" ] || : >"$_o"
  printf '%s' "$CODE"
}

# json <名字> <内容>：请求体写到 upanel-web 可读的位置并留一份证据，回显路径。
json() {
  _path=$TMPD/$1.json
  printf '%s' "$2" >"$_path"
  chmod 0644 "$_path"
  cp "$_path" "$PAYLOADS/$1.json" 2>/dev/null
  printf '%s' "$_path"
}

# pf <名字> <仓库 id> <版本>：预检一条 backup.repository.initialize，响应存
# $PAYLOADS/preflight-<名字>.json，回显状态码。
pf() {
  _body=$(printf '{"action":"backup.repository.initialize","params":{"repository_id":"%s","version":%s},"actor_context":{"user_id":1,"session_id_hash":"%s","request_id":"p12-1-%s","reauth_at":"%s"}}' \
    "$2" "$3" "$(openssl rand -hex 32)" "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
  _code=$(web_curl POST /internal/v1/preflights "$PAYLOADS/preflight-$1.json" "$(json preflight-$1 "$_body")")
  printf '%s' "$_code"
}
pfget() { jget "$PAYLOADS/preflight-$1.json" "$2"; }
# pfblockers <名字>：预检结论里的阻塞码列表（JSON 数组文本，便于 expect_contains）。
pfblockers() { pfget "$1" 'json.dumps([b["code"] for b in d["data"]["blockers"]])'; }
pfmessages() { pfget "$1" 'json.dumps([b["message"] for b in d["data"]["blockers"]])'; }

# state_of <名字> <仓库 id>：查一次 backup.repository.state，响应存
# $PAYLOADS/state-<名字>.json，回显状态码。
state_of() {
  _body=$(printf '{"query":"backup.repository.state","params":{"repository_id":"%s"}}' "$2")
  _code=$(web_curl POST /internal/v1/queries "$PAYLOADS/state-$1.json" "$(json state-$1 "$_body")")
  printf '%s' "$_code"
}
stget() { jget "$PAYLOADS/state-$1.json" "$2"; }

# create_def <名字> <定义 JSON>：登记一条仓库定义，回显状态码；响应存
# $PAYLOADS/repo-<名字>.json。
create_def() {
  _code=$(web_curl POST /internal/v1/backup/repositories "$PAYLOADS/repo-$1.json" \
    "$(json repo-$1 "{\"definition\":$2}")")
  printf '%s' "$_code"
}
repo_id_of() { jget "$PAYLOADS/repo-$1.json" 'd["data"]["repository"]["id"]'; }

# run_op <名字> <preflight_ref>：提交操作并轮询到终态，响应存
# $PAYLOADS/operation-<名字>.json，回显终态（提交就被拒时回 "submit:<码>"）。
run_op() {
  _opid="op_$(openssl rand -hex 16)"
  _body=$(printf '{"operation_id":"%s","preflight_ref":"%s","confirmation":"","actor_context":{"user_id":1,"session_id_hash":"%s","request_id":"p12-1-%s-op","reauth_at":"%s"}}' \
    "$_opid" "$2" "$(openssl rand -hex 32)" "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
  _code=$(web_curl POST /internal/v1/operations "$TMPD/op-submit.json" "$(json operation-submit-$1 "$_body")")
  case "$_code" in
    200|201|202) : ;;
    *) printf 'submit:%s' "$_code"; return 0 ;;
  esac
  _stage=""
  for _ in $(seq 1 180); do
    web_curl GET "/internal/v1/operations/$_opid" "$PAYLOADS/operation-$1.json" >/dev/null
    _stage=$(opget "$1" 'd["data"]["stage"]')
    case "$_stage" in
      succeeded|failed|canceled|interrupted) break ;;
    esac
    sleep 0.5
  done
  printf '%s' "${_stage:-timeout}"
}
opget() { jget "$PAYLOADS/operation-$1.json" "$2"; }

echo "== 0. 这台机器上的 restic / ssh 现场 =="
note "restic：$(restic version 2>&1 | head -1)"
note "ssh：$(ssh -V 2>&1)"
{
  echo "# apt-cache policy restic"
  apt-cache policy restic 2>&1
  echo
  echo "# restic version"
  restic version 2>&1
  echo
  echo "# ssh -V"
  ssh -V 2>&1
  echo
  echo "# ssh-keyscan -p 22 127.0.0.1"
  ssh-keyscan -p 22 127.0.0.1 2>&1
} >"$PAYLOADS/restic-versions.txt" 2>&1
pass "env-records-versions"

echo
echo "== 1. 启动代理（root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "127.0.0.1:19512", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "$SOCK",
    "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER",
    "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DB",
    "secret_key_path": "$KEY_FILE",
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {},
    "logs": {},
    "files": { "roots": [ { "id": "root1", "name": "验收数据", "path": "$ROOT_PATH", "writable": true } ] }
  },
  "database": { "path": "$WORK/webdata/upanel.db", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"

if [ -f "$KEY_FILE" ]; then
  pass "agent-key-present"
elif "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
  pass "agent-key-created"
else
  fail "agent-key-created" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
fi

# 这里**故意不 rm -f "$SOCK"**：陈旧 socket 由代理自己判断并清理；那个路径上
# 如果真有人在服务，代理会拒绝启动并把理由写进日志——那正是要看到的东西。
"$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 100); do
  if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null \
       --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then
    break
  fi
  sleep 0.1
done
if runuser -u "$WEB_USER" -- curl -fsS -o /dev/null --unix-socket "$SOCK" \
     http://agent/internal/v1/capabilities 2>/dev/null; then
  pass "agent-started"
else
  fail "agent-started" "代理没起来（见 $AGENT_LOG）：$(tail -1 "$AGENT_LOG" 2>/dev/null | cut -c1-200)"
fi
expect_eq "agent-runs-as-root" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"
expect_contains "agent-logs-backup-enabled" "已启用备份仓库动作" "$(cat "$AGENT_LOG" 2>/dev/null)"

echo
echo "== 2. 能力表如实报告一条动作与一条查询 =="
CAP=$PAYLOADS/capabilities.json
CODE=$(web_curl GET /internal/v1/capabilities "$CAP")
expect_eq "capabilities-200" "200" "$CODE"
expect_eq "capabilities-has-initialize" "yes" \
  "$(jget "$CAP" '"yes" if "backup.repository.initialize" in d["data"]["supported_actions"] else "no"')"
expect_eq "capabilities-has-state" "yes" \
  "$(jget "$CAP" '"yes" if "backup.repository.state" in d["data"]["supported_queries"] else "no"')"
note "supported_actions=$(jget "$CAP" 'json.dumps(d["data"]["supported_actions"])')"
note "supported_queries=$(jget "$CAP" 'json.dumps(d["data"]["supported_queries"])')"

echo
echo "== 3. 仓库镜像路由：只有 Web 能写，定义必须过形状校验 =="
CODE=$(curl -s -o "$TMPD/root.json" -w '%{http_code}' --unix-socket "$SOCK" \
  http://agent/internal/v1/backup/repositories 2>/dev/null)
expect_eq "mirror-denies-root" "403" "$CODE"

# ---- 秘密：两条仓库口令（一正一错）、两把私钥（一授权一未授权）、一张 TLS 私钥
#
# 明文只出现在这一次请求里：请求体用完就删，$PAYLOADS 只留响应。
put_secret() { # put_secret <名字> <purpose> <值> <值是文件路径时 1>
  _req=$TMPD/secret.req.json
  if [ "$4" = "1" ]; then
    python3 -c 'import json,sys;print(json.dumps({"purpose":sys.argv[1],"value":open(sys.argv[2]).read(),"reference":"p12.1"}))' \
      "$2" "$3" >"$_req"
  else
    python3 -c 'import json,sys;print(json.dumps({"purpose":sys.argv[1],"value":sys.argv[2],"reference":"p12.1"}))' \
      "$2" "$3" >"$_req"
  fi
  chmod 0644 "$_req"
  _code=$(web_curl POST /internal/v1/secrets "$PAYLOADS/secret-$1.json" "$_req")
  rm -f "$_req"
  printf '%s' "$_code"
}
secret_of() { jget "$PAYLOADS/secret-$1.json" 'd["data"]["secret_ref"]'; }

# 两把 ed25519 私钥：一把塞进 upanel-web 的 authorized_keys，一把不塞。
rm -f "$SSH_DIR"/id_ed25519* "$SSH_DIR"/id_other*
ssh-keygen -q -t ed25519 -N '' -C 'p12-1-accept' -f "$SSH_DIR/id_ed25519" >/dev/null 2>&1
ssh-keygen -q -t ed25519 -N '' -C 'p12-1-unauthorized' -f "$SSH_DIR/id_other" >/dev/null 2>&1
install -d -m 0700 -o "$WEB_USER" -g "$WEB_USER" "/home/$WEB_USER/.ssh"
touch "/home/$WEB_USER/.ssh/authorized_keys"
chown "$WEB_USER:$WEB_USER" "/home/$WEB_USER/.ssh/authorized_keys"
chmod 0600 "/home/$WEB_USER/.ssh/authorized_keys"
if grep -qF "$(cat "$SSH_DIR/id_ed25519.pub")" "/home/$WEB_USER/.ssh/authorized_keys" 2>/dev/null; then
  pass "authorized-key-present"
else
  cat "$SSH_DIR/id_ed25519.pub" >>"/home/$WEB_USER/.ssh/authorized_keys"
  pass "authorized-key-added"
fi

expect_eq "secret-password" "201" "$(put_secret password backup_repository_password "$REPO_PASSWORD" 0)"
SEC_PASS=$(secret_of password)
expect_eq "secret-wrong-password" "201" "$(put_secret wrongpass backup_repository_password "$WRONG_PASSWORD" 0)"
SEC_WRONG=$(secret_of wrongpass)
expect_eq "secret-private-key" "201" "$(put_secret key sftp_private_key "$SSH_DIR/id_ed25519" 1)"
SEC_KEY=$(secret_of key)
expect_eq "secret-other-key" "201" "$(put_secret otherkey sftp_private_key "$SSH_DIR/id_other" 1)"
SEC_OTHER_KEY=$(secret_of otherkey)
# 用途不对的那一条：TLS 私钥拿来当仓库口令。
expect_eq "secret-tls-key" "201" "$(put_secret tlskey tls_private_key "$SSH_DIR/id_other" 1)"
SEC_TLS=$(secret_of tlskey)
expect_contains "secret-ref-shape" "sec_" "$SEC_PASS"
note "仓库口令引用 $SEC_PASS；SFTP 私钥引用 $SEC_KEY"

# 服务端真正对外的指纹：ssh-keyscan 拿的是**客户端看到的**那把，不是本机文件里的。
HOSTKEY_FP=$(ssh-keyscan -p 22 -t ed25519 127.0.0.1 2>/dev/null | ssh-keygen -lf - 2>/dev/null | awk '{print $2}')
if [ -n "$HOSTKEY_FP" ]; then
  pass "host-key-scanned"
  note "127.0.0.1:22 的 ed25519 主机密钥指纹：$HOSTKEY_FP"
else
  fail "host-key-scanned" "ssh-keyscan 没有拿到指纹"
  HOSTKEY_FP="SHA256:missing"
fi
# 形状合法但不同的一份指纹：把最后一个字符换掉（不依赖 sshd 是否配了 RSA 主机密钥）。
WRONG_FP=$(python3 -c '
import sys
fp = sys.argv[1]
print(fp[:-1] + ("A" if fp[-1] != "A" else "B"))' "$HOSTKEY_FP")

# sftp_def <名称> <指纹> <口令引用> <私钥引用> <远端路径>
sftp_def() {
  python3 -c '
import json,sys
name,fp,pass_ref,key_ref,path,user = sys.argv[1:7]
print(json.dumps({"name": name, "backend": "sftp",
  "location": {"host":"127.0.0.1","port":22,"username":user,
               "remote_path": path, "host_key_fingerprint": fp},
  "password_secret_ref": pass_ref, "credential_secret_ref": key_ref}))' \
    "$1" "$2" "$3" "$4" "$5" "$WEB_USER"
}
# local_def <名称> <口令引用> <相对路径>
local_def() {
  python3 -c '
import json,sys
print(json.dumps({"name": sys.argv[1], "backend": "local",
  "location": {"root_id":"root1","relative_path":sys.argv[3]},
  "password_secret_ref": sys.argv[2]}))' "$1" "$2" "$3"
}

# ---- 正确的一份
expect_eq "mirror-create-sftp" "201" "$(create_def sftp "$(sftp_def 'SFTP 验收仓库' "$HOSTKEY_FP" "$SEC_PASS" "$SEC_KEY" "$REMOTE_DIR")")"
REP_SFTP=$(repo_id_of sftp)
expect_contains "mirror-id-shape" "rep_" "$REP_SFTP"
expect_eq "mirror-create-version-1" "1" "$(jget "$PAYLOADS/repo-sftp.json" 'd["data"]["repository"]["version"]')"
expect_eq "mirror-create-frozen-backend" "sftp" "$(jget "$PAYLOADS/repo-sftp.json" 'd["data"]["repository"]["backend"]')"
note "SFTP 仓库定义 id：$REP_SFTP"

# ---- 非法定义：本地后端却带了 host（拼凑两个后端的字段）
BAD_DEF='{"name":"拼凑的","backend":"local","location":{"root_id":"root1","relative_path":"x","host":"evil.example.com"},"password_secret_ref":"'$SEC_PASS'"}'
CODE=$(create_def bad "$BAD_DEF")
expect_eq "mirror-rejects-mixed-backend" "400" "$CODE"
expect_eq "mirror-rejects-mixed-backend-code" "VALIDATION_ERROR" "$(jget "$PAYLOADS/repo-bad.json" 'd["error"]["code"]')"
expect_contains "mirror-rejects-mixed-backend-field" "host" "$(jget "$PAYLOADS/repo-bad.json" 'd["error"]["message"]')"

# ---- 越界路径：这个字符串最终会进 restic 的 argv
TRAV_DEF='{"name":"越界","backend":"local","location":{"root_id":"root1","relative_path":"../../etc"},"password_secret_ref":"'$SEC_PASS'"}'
expect_eq "mirror-rejects-traversal" "400" "$(create_def trav "$TRAV_DEF")"

# ---- 乐观并发：拿旧版本改必须 409
PATCH_BODY=$(python3 -c 'import json,sys;print(json.dumps({"version":99,"definition":json.loads(sys.argv[1])}))' \
  "$(sftp_def 'SFTP 验收仓库' "$HOSTKEY_FP" "$SEC_PASS" "$SEC_KEY" "$REMOTE_DIR")")
CODE=$(web_curl PATCH "/internal/v1/backup/repositories/$REP_SFTP" "$PAYLOADS/repo-patch.json" "$(json repo-patch "$PATCH_BODY")")
expect_eq "mirror-patch-version-conflict" "409" "$CODE"
expect_eq "mirror-patch-version-conflict-code" "VERSION_CONFLICT" "$(jget "$PAYLOADS/repo-patch.json" 'd["error"]["code"]')"

# ---- 单条读取、列表、查不到与 id 形状
CODE=$(web_curl GET "/internal/v1/backup/repositories/$REP_SFTP" "$PAYLOADS/repo-get.json")
expect_eq "mirror-get-200" "200" "$CODE"
expect_eq "mirror-get-same-id" "$REP_SFTP" "$(jget "$PAYLOADS/repo-get.json" 'd["data"]["repository"]["id"]')"
CODE=$(web_curl GET /internal/v1/backup/repositories "$PAYLOADS/repo-list.json")
expect_eq "mirror-list-200" "200" "$CODE"
expect_eq "mirror-list-contains" "yes" \
  "$(jget "$PAYLOADS/repo-list.json" '"yes" if any(r["id"]=="'"$REP_SFTP"'" for r in d["data"]["repositories"]) else "no"')"
expect_eq "mirror-get-unknown-404" "404" \
  "$(web_curl GET /internal/v1/backup/repositories/rep_ffffffffffffffffffffffffffffffff "$TMPD/r404.json")"
expect_eq "mirror-get-malformed-400" "400" \
  "$(web_curl GET /internal/v1/backup/repositories/not-a-repo-id "$TMPD/r400.json")"

echo
echo "== 4. 现状查询：连得上、主机密钥对、还没有仓库、容量读得到 =="
expect_eq "state-query-200" "200" "$(state_of ok "$REP_SFTP")"
expect_eq "state-registered" "True" "$(stget ok 'd["data"]["registered"]')"
expect_eq "state-host-key-verified" "True" "$(stget ok 'd["data"]["state"]["host_key_verified"]')"
expect_eq "state-not-initialized-yet" "False" "$(stget ok 'd["data"]["state"]["initialized"]')"
expect_eq "state-capacity-supported" "True" "$(stget ok 'd["data"]["state"]["capacity"]["supported"]')"
AVAIL_BEFORE=$(stget ok 'd["data"]["state"]["capacity"]["available_bytes"]')
expect_ge "state-capacity-available-positive" "$AVAIL_BEFORE" 1
note "远端可用 $AVAIL_BEFORE 字节 / 总量 $(stget ok 'd["data"]["state"]["capacity"]["total_bytes"]') 字节"
state_of ghost rep_ffffffffffffffffffffffffffffffff >/dev/null
expect_eq "state-unregistered-registered-false" "False" "$(jget "$PAYLOADS/state-ghost.json" 'd["data"]["registered"]')"

echo
echo "== 5. 主机密钥与秘密：四种拒绝各自成立 =="
# (a) 指纹错
expect_eq "mirror-create-badfp" "201" "$(create_def badfp "$(sftp_def '指纹错的仓库' "$WRONG_FP" "$SEC_PASS" "$SEC_KEY" "$REMOTE_DIR-badfp")")"
REP_BADFP=$(repo_id_of badfp)
expect_eq "preflight-badfp-201" "201" "$(pf badfp "$REP_BADFP" 1)"
expect_eq "preflight-badfp-blocked" "False" "$(pfget badfp 'd["data"]["allowed"]')"
expect_contains "preflight-badfp-code" "BACKUP_HOST_KEY_MISMATCH" "$(pfblockers badfp)"
expect_contains "preflight-badfp-shows-registered" "$WRONG_FP" "$(pfmessages badfp)"
expect_contains "preflight-badfp-says-mismatch" "不匹配" "$(pfmessages badfp)"

# (b) 私钥没被授权：指纹对，但钥匙进不了门
expect_eq "mirror-create-badkey" "201" "$(create_def badkey "$(sftp_def '私钥错的仓库' "$HOSTKEY_FP" "$SEC_PASS" "$SEC_OTHER_KEY" "$REMOTE_DIR-badkey")")"
REP_BADKEY=$(repo_id_of badkey)
expect_eq "preflight-badkey-201" "201" "$(pf badkey "$REP_BADKEY" 1)"
expect_eq "preflight-badkey-blocked" "False" "$(pfget badkey 'd["data"]["allowed"]')"
expect_contains "preflight-badkey-code" "BACKUP_SFTP_AUTH" "$(pfblockers badkey)"

# (c) 用途不对：拿 TLS 私钥当仓库口令
expect_eq "mirror-create-purpose" "201" "$(create_def purpose "$(sftp_def '用途错的仓库' "$HOSTKEY_FP" "$SEC_TLS" "$SEC_KEY" "$REMOTE_DIR-purpose")")"
REP_PURPOSE=$(repo_id_of purpose)
expect_eq "preflight-purpose-201" "201" "$(pf purpose "$REP_PURPOSE" 1)"
expect_eq "preflight-purpose-blocked" "False" "$(pfget purpose 'd["data"]["allowed"]')"
expect_contains "preflight-purpose-code" "BACKUP_SECRET_PURPOSE" "$(pfblockers purpose)"
expect_contains "preflight-purpose-names-both" "tls_private_key" "$(pfmessages purpose)"

# (d) 引用不存在：形状合法（sec_ + 32 位十六进制），但库里没有这一条
GHOST='sec_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
expect_eq "mirror-create-ghost" "201" "$(create_def ghost "$(sftp_def '秘密不存在的仓库' "$HOSTKEY_FP" "$GHOST" "$SEC_KEY" "$REMOTE_DIR-ghost")")"
REP_GHOST=$(repo_id_of ghost)
expect_eq "preflight-ghost-201" "201" "$(pf ghost "$REP_GHOST" 1)"
expect_eq "preflight-ghost-blocked" "False" "$(pfget ghost 'd["data"]["allowed"]')"
expect_contains "preflight-ghost-code" "BACKUP_SECRET_UNRESOLVED" "$(pfblockers ghost)"

echo
echo "== 6. 正确的那一份：预检放行、要再认证、不要短语 =="
expect_eq "preflight-ok-201" "201" "$(pf ok "$REP_SFTP" 1)"
expect_eq "preflight-ok-allowed" "True" "$(pfget ok 'd["data"]["allowed"]')"
expect_eq "preflight-ok-no-blockers" "0" "$(pfget ok 'len(d["data"]["blockers"])')"
expect_eq "preflight-ok-requires-reauth" "True" "$(pfget ok 'd["data"]["requires_reauth"]')"
expect_eq "preflight-ok-no-phrase" "False" "$(pfget ok 'd["data"]["requires_phrase"]')"
expect_eq "preflight-ok-cancellable" "True" "$(pfget ok 'd["data"]["cancellable"]')"
expect_eq "preflight-ok-has-lock" "yes" \
  "$(pfget ok '"yes" if any(l.startswith("backup-repo:") for l in d["data"]["locks"]) else "no"')"
expect_eq "preflight-ok-impact-create" "yes" \
  "$(pfget ok '"yes" if any(i["kind"]=="create_repository" for i in d["data"]["impacts"]) else "no"')"
expect_eq "preflight-ok-impact-secret" "yes" \
  "$(pfget ok '"yes" if any(i["kind"]=="secret_use" for i in d["data"]["impacts"]) else "no"')"
expect_eq "preflight-ok-params-are-frozen" "yes" \
  "$(pfget ok '"yes" if set(d["data"]["normalized_params"].keys())=={"repository_id","version"} else "no"')"
PRE_OK=$(pfget ok 'd["data"]["preflight_ref"]')
note "preflight_ref：$PRE_OK"

echo
echo "== 7. 初始化：结果必须是 verified，且仓库 id 与独立读回的一致 =="
expect_eq "operation-stage-succeeded" "succeeded" "$(run_op ok "$PRE_OK")"
expect_eq "operation-outcome-verified" "verified" "$(opget ok 'd["data"]["result"]["outcome"]')"
REPO_ID=$(opget ok 'd["data"]["result"]["data"]["repo_id"]')
expect_eq "operation-repo-id-matches-observed" "$REPO_ID" "$(opget ok 'd["data"]["result"]["observed_state"]["repo_id"]')"
expect_eq "operation-verified-flag" "True" "$(opget ok 'd["data"]["result"]["data"]["verified"]')"
expect_eq "operation-verified-by-restic" "restic cat config" "$(opget ok 'd["data"]["result"]["observed_state"]["verified_by"]')"
expect_eq "operation-repo-id-is-64-hex" "yes" \
  "$(python3 -c 'import re,sys;print("yes" if re.fullmatch(r"[0-9a-f]{64}", sys.argv[1] or "") else "no")' "$REPO_ID")"
note "仓库 id（代理读回）：$REPO_ID"

# known_hosts 是我们写的，内容必须与扫到的指纹一致。
KH=$(find "$STATE_DIR/known_hosts" -type f 2>/dev/null | head -1)
if [ -n "$KH" ]; then
  pass "known_hosts-written"
  note "known_hosts：$KH"
  expect_eq "known_hosts-mode-0600" "600" "$(stat -c %a "$KH" 2>/dev/null)"
  expect_contains "known_hosts-has-host-key" "$HOSTKEY_FP" "$(ssh-keygen -lf "$KH" 2>/dev/null | awk '{print $2}')"
  expect_contains "known_hosts-names-host" "127.0.0.1" "$(cat "$KH" 2>/dev/null)"
else
  fail "known_hosts-written" "$STATE_DIR/known_hosts 下没有文件"
  KH=$TMPD/kh-missing
  : >"$KH"
fi
if grep -rn 'StrictHostKeyChecking=no' "$AGENT_LOG" "$KH" 2>/dev/null | head -1 | grep -q .; then
  fail "no-trust-on-first-use" "日志或 known_hosts 里出现了 StrictHostKeyChecking=no"
else
  pass "no-trust-on-first-use"
fi

# 独立核对：脚本自己用同样的口令、同样那套 ssh 选项读一遍仓库 id。
install -m 0600 "$SSH_DIR/id_ed25519" "$TMPD/id_ed25519"
INDEP=$(RESTIC_PASSWORD="$REPO_PASSWORD" RESTIC_CACHE_DIR="$TMPD/cache" restic \
  -r "sftp:$WEB_USER@127.0.0.1:$REMOTE_DIR" \
  -o "sftp.command=/usr/bin/ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$KH -o GlobalKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -i $TMPD/id_ed25519 -p 22 $WEB_USER@127.0.0.1 -s sftp" \
  cat config 2>"$TMPD/indep.err" | python3 -c 'import json,sys
try: print(json.load(sys.stdin)["id"])
except Exception: print("")')
cp "$TMPD/indep.err" "$PAYLOADS/independent-cat-config.stderr" 2>/dev/null
expect_eq "independent-repo-id-not-empty" "yes" \
  "$(python3 -c 'import sys;print("yes" if sys.argv[1] else "no")' "${INDEP:-}")"
expect_eq "independent-read-repo-id" "$REPO_ID" "$INDEP"

echo
echo "== 8. 容量真的读了，不是 0 =="
AVAIL_AFTER=$(opget ok 'd["data"]["result"]["observed_state"]["capacity_after"]["available_bytes"]')
expect_eq "capacity-after-supported" "True" "$(opget ok 'd["data"]["result"]["observed_state"]["capacity_after"]["supported"]')"
expect_ge "capacity-after-positive" "$AVAIL_AFTER" 1
expect_eq "capacity-before-supported" "True" "$(opget ok 'd["data"]["result"]["observed_state"]["capacity_before"]["supported"]')"
expect_ge "capacity-before-positive" "$(opget ok 'd["data"]["result"]["observed_state"]["capacity_before"]["available_bytes"]')" 1
# 初始化只写几十 KB，同一次执行内的前后两次读数必须单调不减用。
expect_le "capacity-after-not-more-than-before" "$AVAIL_AFTER" \
  "$(opget ok 'd["data"]["result"]["observed_state"]["capacity_before"]["available_bytes"]')"

echo
echo "== 9. 已存在的仓库不许覆盖 =="
expect_eq "preflight-again-201" "201" "$(pf again "$REP_SFTP" 1)"
expect_eq "preflight-again-blocked" "False" "$(pfget again 'd["data"]["allowed"]')"
expect_contains "preflight-again-code" "BACKUP_REPOSITORY_EXISTS" "$(pfblockers again)"
state_of after "$REP_SFTP" >/dev/null
expect_eq "state-now-initialized" "True" "$(jget "$PAYLOADS/state-after.json" 'd["data"]["state"]["initialized"]')"
expect_eq "state-now-reports-repo-id" "$REPO_ID" "$(jget "$PAYLOADS/state-after.json" 'd["data"]["state"]["repo_id"]')"

echo
echo "== 10. 口令错：现状查询要说出口令不对，不假装是空仓库 =="
expect_eq "mirror-create-wrongpass" "201" "$(create_def wrongpass "$(sftp_def '口令错的仓库' "$HOSTKEY_FP" "$SEC_WRONG" "$SEC_KEY" "$REMOTE_DIR")")"
REP_WRONGPASS=$(repo_id_of wrongpass)
expect_eq "state-wrongpass-200" "200" "$(state_of wrongpass "$REP_WRONGPASS")"
expect_eq "state-wrongpass-sees-repository" "True" "$(stget wrongpass 'd["data"]["state"]["initialized"]')"
WERR=$(stget wrongpass 'd["data"]["state"].get("repo_config_error","")')
if [ -n "$WERR" ]; then pass "state-wrongpass-reports-error"; else fail "state-wrongpass-reports-error" "repo_config_error 是空的"; fi
expect_eq "state-wrongpass-no-repo-id" "" "$(stget wrongpass 'd["data"]["state"].get("repo_id","")')"
note "口令不对时的原因原文：$WERR"
case "$WERR" in
  *口令*|*password*|*Password*) pass "state-wrongpass-says-password-wrong" ;;
  *) fail "state-wrongpass-says-password-wrong" "原因里没有指向口令：$WERR" ;;
esac

echo
echo "== 11. 本地后端：受管理根内建仓库 =="
expect_eq "mirror-create-local" "201" "$(create_def local "$(local_def '本地验收仓库' "$SEC_PASS" "$LOCAL_REL")")"
REP_LOCAL=$(repo_id_of local)
expect_eq "preflight-local-201" "201" "$(pf local "$REP_LOCAL" 1)"
expect_eq "preflight-local-allowed" "True" "$(pfget local 'd["data"]["allowed"]')"
expect_eq "preflight-local-no-host-key-blocker" "no" \
  "$(pfget local '"yes" if any(b["code"].startswith("BACKUP_HOST_KEY") for b in d["data"]["blockers"]) else "no"')"
LOCAL_REF=$(pfget local 'd["data"]["preflight_ref"]')
expect_eq "operation-local-succeeded" "succeeded" "$(run_op local "$LOCAL_REF")"
expect_eq "operation-local-verified" "verified" "$(opget local 'd["data"]["result"]["outcome"]')"
LOCAL_ABS="$ROOT_PATH/$LOCAL_REL"
expect_eq "local-absolute-path-reported" "$LOCAL_ABS" "$(opget local 'd["data"]["result"]["observed_state"]["absolute_path"]')"
if [ -f "$LOCAL_ABS/config" ]; then pass "local-repo-config-exists"; else fail "local-repo-config-exists" "$LOCAL_ABS/config 不存在"; fi
expect_eq "local-repo-mode-0700" "700" "$(stat -c %a "$LOCAL_ABS" 2>/dev/null)"
LOCAL_ID=$(opget local 'd["data"]["result"]["data"]["repo_id"]')
LOCAL_INDEP=$(RESTIC_PASSWORD="$REPO_PASSWORD" RESTIC_CACHE_DIR="$TMPD/cache" restic -r "$LOCAL_ABS" cat config 2>/dev/null \
  | python3 -c 'import json,sys
try: print(json.load(sys.stdin)["id"])
except Exception: print("")')
expect_eq "local-repo-id-matches" "$LOCAL_ID" "$LOCAL_INDEP"
expect_ge "local-capacity-positive" "$(opget local 'd["data"]["result"]["observed_state"]["capacity_after"]["available_bytes"]')" 1
expect_eq "preflight-local-again-201" "201" "$(pf local-again "$REP_LOCAL" 1)"
expect_contains "preflight-local-again-code" "BACKUP_REPOSITORY_EXISTS" "$(pfblockers local-again)"

echo
echo "== 12. 未登记的 id 与陈旧版本：预检拒绝，不去评估现场 =="
expect_eq "preflight-ghostrep-201" "201" "$(pf ghostrep rep_ffffffffffffffffffffffffffffffff 1)"
expect_eq "preflight-ghostrep-blocked" "False" "$(pfget ghostrep 'd["data"]["allowed"]')"
expect_contains "preflight-ghostrep-code" "BACKUP_REPOSITORY_NOT_FOUND" "$(pfblockers ghostrep)"
expect_eq "preflight-stale-201" "201" "$(pf stale "$REP_SFTP" 9)"
expect_eq "preflight-stale-blocked" "False" "$(pfget stale 'd["data"]["allowed"]')"
expect_contains "preflight-stale-code" "BACKUP_REPOSITORY_VERSION_STALE" "$(pfblockers stale)"
expect_eq "preflight-stale-still-locks" "yes" \
  "$(pfget stale '"yes" if any(l.startswith("backup-repo:") for l in d["data"]["locks"]) else "no"')"

echo
echo "== 13. 参数层：id 形状与未知字段在解码时就被拒 =="
expect_eq "preflight-malformed-id-400" "400" "$(pf malformed not-a-repo-id 1)"
EXTRA_BODY="{\"action\":\"backup.repository.initialize\",\"params\":{\"repository_id\":\"$REP_SFTP\",\"version\":1,\"path\":\"/tmp/x\"},\"actor_context\":{\"user_id\":1,\"session_id_hash\":\"$(openssl rand -hex 32)\",\"request_id\":\"p12-1-extra\",\"reauth_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}"
expect_eq "preflight-unknown-field-400" "400" \
  "$(web_curl POST /internal/v1/preflights "$PAYLOADS/preflight-extra.json" "$(json preflight-extra "$EXTRA_BODY")")"
expect_eq "preflight-unknown-field-code" "VALIDATION_ERROR" \
  "$(jget "$PAYLOADS/preflight-extra.json" 'd["error"]["code"]')"
expect_contains "preflight-unknown-field-names-it" "path" \
  "$(jget "$PAYLOADS/preflight-extra.json" 'd["error"]["message"]')"

echo
echo "== 证据卫生：把含秘密明文的载荷替换成说明 =="
for f in "$PAYLOADS"/*.json; do
  [ -f "$f" ] || continue
  if grep -qF "$REPO_PASSWORD" "$f" 2>/dev/null || grep -qF "$WRONG_PASSWORD" "$f" 2>/dev/null ||
     grep -q 'BEGIN OPENSSH PRIVATE KEY' "$f" 2>/dev/null; then
    printf '（已清除：这份载荷里含测试口令或测试私钥的明文）\n' >"$f"
  fi
done
pass "evidence-secrets-scrubbed"

echo
echo "== NOT RUN（本阶段不该假装通过的项目） =="
notrun "备份与恢复" "P12.2/P12.3 才实现；BACKUP-01 的『备份后恢复』要到那时才有东西可验"
notrun "备份跑到一半网络断开" "属于 P12.2 的备份执行；本阶段只验连不上/认证失败时预检拒绝"
notrun "存储满" "要真把验收机的文件系统填满或挂 loop 设备；由 internal/backup 的 capacityVerdict 单测覆盖"
notrun "目标路径存在但不是目录" "与文件模块用例重复，由评估逻辑单测覆盖"
notrun "两个 restic 版本行为一致" "22.04 与 24.04 的 restic 版本不同；原文记在 payloads/restic-versions.txt"

printf '\n== 汇总 ==\nPASS %d  FAIL %d  NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
