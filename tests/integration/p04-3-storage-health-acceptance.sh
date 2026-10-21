#!/bin/sh
# P04.3 Linux 验收脚本：SMART/NVMe 健康、diskstats 速率、不可达网络挂载的隔离。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并且要用 upanel-web 身份去查 socket）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-3-storage-health-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P04.3、PLAN §8、CONTRACTS §6/§6.2）：
#   1. 配置：SMART 间隔与网络容量超时的范围校验真的会拒绝启动（不悄悄钳制）
#   2. health 闭集：物理盘只可能是 smartctl 读出的结论；分区/loop/rom 一律 unsupported 并带理由
#   3. 与 smartctl 交叉验证：自己跑一遍 smartctl，逐盘比 status / temperature / source
#   4. 温度不造假：没有读数就是 null，绝不能出现 0 °C
#   5. SMART 节奏：15 分钟内 checked_at 不变（5 秒的扫描循环不去碰 SMART），
#      且期间没有新的 smartctl 进程
#   6. diskstats：每台设备只拿自己那一行（分区不累加到整盘），计数与 /proc/diskstats 对得上，
#      字节数是十进制字符串
#   7. 速率：第二次采样之后给出速率；计数器归零/换盘/异常窗口时为 null 而不是负值或 0
#   8. 快照版本：io 变化不改变 snapshot_version（P07 的预检靠它判断状态是否变化）
#   9. 网络挂载隔离：NFS 服务停掉后 statfs 会挂在 D 状态；代理必须超时放弃、写 capacity_error、
#      把该挂载点隔离起来（不反复派子进程），而且拓扑扫描不受影响；服务恢复后容量回来
#  10. statfs helper 入口本身只接受一个绝对路径
#  11. Web 透传：viewer 能读，io/health 字段原样出现，字节字段是字符串
#  12. 登录不受采集拖累：statfs 卡住期间 Web 起得来、登录 5 秒内返回 200，
#      且那一刻挂载仍然是不可达的（快不是因为卡点已经过去）
#  13. 日志卫生：不出现 shell 命令行与私钥
#  14. USB 盘（有则验）：拓扑里 transport=usb 可见、纳入逐盘对账；桥不认识时只能是 unknown
#  15. 反向断言：报 healthy 必须有 smartctl 自己的依据（passed=true 或 NVMe 健康日志）
#
# 明确不覆盖（NOT RUN，原因写在文档里，不在脚本里假装通过）：
#   - 睡眠盘不唤醒：QEMU 的 SCSI/NVMe 在这两个 VM 里无法进入 standby
#     （smartctl -s standby,now 之后 sysfs 仍是 running），真机证据拿不到，只有单测。
#   - ATA/SATA 盘的真实输出：VM-E 挂 ide-hd 会让 guest 卡在固件阶段，ATA 分支只有合成输入。
#   - 真实 USB 盘：QEMU 的 usb-storage 桥（0x46f4:0x0001）不被 smartctl 识别，
#     拿到的是“Unknown USB bridge”这个真实反例（夹具已留），不是一块能读出 SMART 的 USB 盘。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p04-3}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=19447
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

# capabilities 的完整闭集。生产代理只要依赖齐备就会登记全部查询与动作，
# 所以这里断言**完全等于**当前应有的集合：少报是隐藏能力，多报是编造能力，两种都算失败。
# 新增查询/动作时同步改这两行（P05.1 加 service.control，P05.2 加 processes.list / process.signal）。
EXPECTED_QUERIES="['processes.list', 'services.list', 'services.unit', 'storage.topology', 'system.summary']"
EXPECTED_ACTIONS="['process.signal', 'service.control']"

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log

# 网络挂载实验：本机 NFS 服务 + 一条 hard 挂载（statfs 会无限重试，正是我们要隔离的行为）。
NFS_SRC=/srv/upanel-nfs
NFS_MNT=/mnt/upanel-nfs
NFS_READY=""
SMARTCTL=""

PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""
NOTRUN_NAMES=""
AGENT_PID=""
WEB_PID=""
CODE=""
RESP=""
LOOP_DEV=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="${FAILED_NAMES:+$FAILED_NAMES | }$1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
notrun() { NOTRUN=$((NOTRUN + 1)); NOTRUN_NAMES="${NOTRUN_NAMES:+$NOTRUN_NAMES | }$1"; printf 'NOTRUN  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }

expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}

# 探针的公共执行体：把表达式跑在 {"d": 解析后的 JSON, "a": 参数列表, "json", "os"} 命名空间里。
#
# 断言写起来有两种形状：
#   - 单表达式，eval 拿返回值（例：d["data"]["status"]）；
#   - 多语句块，自己 print（例：遍历所有节点找反例）。
# 两种统一成“打印一行给 shell 比对”，所以先把 stdout 截下来：
# 语句块 print 了什么就用什么，没 print 才用 eval 的返回值。
# 之前直接 print(eval(...)) 会在语句块上输出 "False\nNone"——值对了但比不过，
# 而且 NameError 会被当成空串静默通过。
_probe() {
  python3 -c 'import contextlib,io,json,os,sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
a = [l for l in open(sys.argv[2], encoding="utf-8").read().split("\n") if l != ""]
ns = {"d": d, "a": a, "json": json, "os": os}
code = open(sys.argv[3], encoding="utf-8").read()
buf = io.StringIO()
with contextlib.redirect_stdout(buf):
    try:
        r = eval(code, ns)
    except SyntaxError:
        exec(code, ns)
        r = None
out = buf.getvalue().rstrip("\n")
if out != "":
    print(out)
elif r is not None:
    print(r)' "$1" "$2" "$3"
}

# jget <文件> <python 表达式，d 为解析后的 JSON>；null 打印 None。
jget() {
  printf '%s\n' "$2" >"$WORK/_probe.py"
  _probe "$1" /dev/null "$WORK/_probe.py"
}

# jgetx <文件> <附加参数…> <表达式>：表达式里用 a[0]、a[1]… 取附加参数。
#
# 最后一个参数是表达式（可以多行），前面的都是要带进 a 的值。这里逐个参数取，
# 不靠 bash 的 ${!#}——验收脚本要在 dash 下跑。参数里不能有换行（都是路径与数值）。
jgetx() {
  f=$1; shift
  total=$#
  i=0
  : >"$WORK/_probe-args.txt"
  expr=""
  for v in "$@"; do
    i=$((i + 1))
    if [ "$i" -eq "$total" ]; then
      expr=$v
    else
      printf '%s\n' "$v" >>"$WORK/_probe-args.txt"
    fi
  done
  printf '%s\n' "$expr" >"$WORK/_probe.py"
  _probe "$f" "$WORK/_probe-args.txt" "$WORK/_probe.py"
}

# Go 的 RFC3339Nano 有 9 位小数秒，Ubuntu 22.04 的 Python 3.10 只认 3/6 位，先截到 6 位。
ts_norm='import re,datetime,sys
def n(x):
    return datetime.datetime.fromisoformat(re.sub(r"\.(\d{6})\d+", r".\1", x).replace("Z","+00:00"))'

ts_age_lt() {
  python3 -c "$ts_norm
now=datetime.datetime.now(datetime.timezone.utc)
print((now-n(sys.argv[1])).total_seconds() < float(sys.argv[2]))" "$1" "$2" 2>/dev/null
}

ts_gap_in() {
  python3 -c "$ts_norm
d=(n(sys.argv[2])-n(sys.argv[1])).total_seconds()
print(float(sys.argv[3]) <= d <= float(sys.argv[4]))" "$1" "$2" "$3" "$4" 2>/dev/null
}

# call <身份> <方法> <路径> [json]：以指定身份访问代理 socket。CODE=状态码，RESP=响应文件。
call() {
  as=$1; method=$2; path=$3; body=${4:-}
  out="$WORK/out-$as/resp.json"
  url="http://agent$path"
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    chmod 0644 "$WORK/req.json"
    args="-sS -o $out -w %{http_code} -X $method -H Content-Type:application/json --data-binary @$WORK/req.json --unix-socket $SOCK $url"
  else
    args="-sS -o $out -w %{http_code} -X $method --unix-socket $SOCK $url"
  fi
  RESP="$out"
  case $as in
    web) CODE=$(runuser -u "$WEB_USER" -- curl $args 2>/dev/null) ;;
    *)   fail "call-args" "未知调用身份 $as"; CODE="" ;;
  esac
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
  runuser -u "$WEB_USER" -- "$WEB" --mode production --db "$WEB_DB" --listen "127.0.0.1:$PORT" \
    >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 100); do
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
  for _ in $(seq 1 100); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
    sleep 0.1
  done
  return 1
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

# 毫秒时钟：用来给登录掐表（不依赖 python 启动时间）。
now_ms() { date +%s%3N; }

# Web 进程以 upanel-web 身份运行，数据库文件必须归它。
chown_web_db() {
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

api() {
  jar=$1; csrf=$2; method=$3; path=$4; body=${5:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
      -d @"$WORK/req.json"
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN"
  fi
}

# dev_attr <id> <属性>：优先走 /sys/class/block（分区也在里面），退回到 /sys/block。
dev_attr() {
  if [ -r "/sys/class/block/$1/$2" ]; then
    cat "/sys/class/block/$1/$2" 2>/dev/null
  elif [ -r "/sys/block/$1/$2" ]; then
    cat "/sys/block/$1/$2" 2>/dev/null
  fi
}

# sysfs_top_ids / sysfs_partitions：内核当前认得的块设备顶层与分区（一行一个，含父盘）。
sysfs_top_ids() {
  for d in /sys/block/*; do
    [ -d "$d" ] || continue
    basename "$d"
  done
}

sysfs_partitions() {
  for d in /sys/block/*; do
    [ -d "$d" ] || continue
    disk=$(basename "$d")
    for c in "$d"/*; do
      [ -e "$c/partition" ] || continue
      printf '%s %s\n' "$(basename "$c")" "$disk"
    done
  done
}

# mnt_device <挂载点>：把 mountinfo 里该挂载点的 major:minor 换成设备名；解析不到就输出空。
mnt_device() {
  mm=$(awk -v p="$1" '$5 == p {print $3; exit}' /proc/self/mountinfo)
  [ -n "$mm" ] || return 0
  link=$(readlink -f "/sys/dev/block/$mm" 2>/dev/null)
  [ -n "$link" ] && basename "$link"
}

# longest_mount <路径>：该路径实际落在哪个挂载点上（最长前缀，按目录边界）。
longest_mount() {
  python3 -c '
import sys
p = sys.argv[1]
best = ""
for line in open("/proc/self/mountinfo", encoding="utf-8"):
    f = line.split()
    if len(f) < 5:
        continue
    mp = f[4].replace("\\040", " ").replace("\\011", "\t").replace("\\134", "\\")
    base = mp.rstrip("/")
    if mp == "/" or p == mp or (base and p.startswith(base + "/")):
        if len(mp) > len(best):
            best = mp
print(best)' "$1" 2>/dev/null
}

cleanup() {
  if mountpoint -q "$WORK/mnt" 2>/dev/null; then
    umount "$WORK/mnt" 2>/dev/null
  fi
  if [ -n "$LOOP_DEV" ]; then
    losetup -d "$LOOP_DEV" 2>/dev/null
  fi
  stop_web
  stop_agent
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

cleanup() {
  if [ -n "${NFS_READY:-}" ]; then
    timeout -k 5 20 umount -l "$NFS_MNT" 2>/dev/null
  fi
  systemctl start nfs-server >/dev/null 2>&1
  stop_web
  stop_agent
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# 把一份配置写到指定路径。第一段是给正常验收用的；坏配置由 bad_cfg 复制后改字段。
write_cfg() {
  dst=$1; interval=$2; nettimeout=$3; smpath=$4
  extra=""
  if [ -n "$smpath" ]; then extra=", \"smartctl_path\": \"$smpath\""; fi
  cat >"$dst" <<JSON
{
  "web": {
    "mode": "production",
    "listen_address": "127.0.0.1:$PORT",
    "shutdown_timeout_seconds": 10
  },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "/var/lib/upanel-agent/agent.db",
    "secret_key_path": "/etc/upanel/agent.key",
    "storage": {
      "smart_enabled": true,
      "smart_interval_minutes": $interval,
      "network_capacity_timeout_seconds": $nettimeout$extra
    }
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
  chmod 0644 "$dst"
}

# topology：拉一份快照到指定文件；失败返回非 0。
topology() {
  call web POST /internal/v1/queries '{"query":"storage.topology"}'
  if [ "$CODE" != "200" ]; then return 1; fi
  cp "$RESP" "$1"
  return 0
}

# disk_counter <文件> <设备名> <字段序号 1..>: 取 /proc/diskstats 某行的某个值字段。
# 字段 1 是读完成次数，3 是读扇区，5 是写完成次数，7 是写扇区。
disk_field() {
  awk -v d="$2" -v n="$3" '$3 == d {print $(3 + n); exit}' "$1"
}

echo "== 0. 环境 =="
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用于解析响应"; else fail "python3 可用于解析响应" "缺少 python3"; fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER（验收环境未预置）"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

for cand in /usr/sbin/smartctl /usr/bin/smartctl /sbin/smartctl /bin/smartctl; do
  if [ -x "$cand" ]; then SMARTCTL=$cand; break; fi
done
if [ -n "$SMARTCTL" ]; then
  pass "本机有 smartctl（$SMARTCTL）：可以做真机交叉验证"
else
  note "本机没有 smartctl：物理盘健康将保持 unknown，交叉验证标为未执行"
fi
if command -v exportfs >/dev/null 2>&1 && command -v mount.nfs4 >/dev/null 2>&1; then
  NFS_TOOLS=1; pass "本机有 NFS 工具（可以做不可达网络挂载实验）"
else
  NFS_TOOLS=0
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/out-web" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/out-web" "$WORK/logs"
chown "$WEB_USER" "$WORK/out-web"
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo
echo "== 1. 配置校验：超范围直接拒绝启动 =="
# 期望：非零退出 + 说出合法范围。悄悄钳制会让运维以为设置生效了。
expect_cfg_reject() {
  label=$1; file=$2; needle=$3
  if timeout 20 "$AGENT" --config "$file" >"$WORK/cfg.out" 2>&1; then
    fail "$label" "配置被接受了（本应拒绝启动）"
  else
    rc=$?
    if [ "$rc" -eq 124 ]; then
      fail "$label" "代理没有在 20 秒内退出"
    elif grep -q -- "$needle" "$WORK/cfg.out"; then
      pass "$label"
    else
      fail "$label" "报错里没有 '$needle'：$(head -2 "$WORK/cfg.out")"
    fi
  fi
}

write_cfg "$WORK/cfg-bad-1.json" 3 10 ""
expect_cfg_reject "smart_interval_minutes=3 被拒绝并指出 5—60" "$WORK/cfg-bad-1.json" "5—60"
write_cfg "$WORK/cfg-bad-2.json" 61 10 ""
expect_cfg_reject "smart_interval_minutes=61 被拒绝" "$WORK/cfg-bad-2.json" "5—60"
write_cfg "$WORK/cfg-bad-3.json" 15 0 ""
expect_cfg_reject "network_capacity_timeout_seconds=0 被拒绝并指出 1—60" "$WORK/cfg-bad-3.json" "1—60"
write_cfg "$WORK/cfg-bad-4.json" 15 10 "relative/smartctl"
expect_cfg_reject "smartctl_path 不是绝对路径被拒绝" "$WORK/cfg-bad-4.json" "绝对路径"
write_cfg "$WORK/cfg-bad-5.json" 15 10 "/usr/sbin/smart?ctl"
expect_cfg_reject "smartctl_path 含 '?' 被拒绝" "$WORK/cfg-bad-5.json" "?"
write_cfg "$WORK/cfg-bad-6.json" 15 61 ""
expect_cfg_reject "network_capacity_timeout_seconds=61 被拒绝" "$WORK/cfg-bad-6.json" "1—60"

echo
echo "== 2. 启动代理（SMART 15 分钟、网络容量超时 2 秒） =="
# 网络容量超时设成 2 秒只是为了验收跑得动：默认 10 秒，语义相同。
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
chmod 0755 "$RUN_DIR"
chown root:root "$RUN_DIR"
write_cfg "$SYS_CFG" 15 2 ""

if [ -f "$KEY_FILE" ]; then
  pass "代理密钥文件已存在（沿用上一轮）"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
  fi
fi
if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

READY=""
for _ in $(seq 1 50); do
  if [ "$(runuser -u "$WEB_USER" -- curl -s -o /dev/null -w '%{http_code}' --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null)" = "200" ]; then
    READY=1; break
  fi
  sleep 0.2
done
if [ -n "$READY" ]; then pass "代理在 10 秒内开始应答（capabilities 200）"; else fail "代理在 10 秒内开始应答" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

call web GET /internal/v1/capabilities
expect_eq "capabilities 200" "200" "$CODE"
expect_eq "capabilities 报告全部已登记查询" "$EXPECTED_QUERIES" "$(jget "$RESP" 'd["data"]["supported_queries"]')"
expect_eq "capabilities 报告的写动作集合与已登记动作一致" "$EXPECTED_ACTIONS" "$(jget "$RESP" 'd["data"]["supported_actions"]')"

echo
echo "== 3. health：闭集、来源、不支持与温度 =="
if ! topology "$WORK/topo-h1.json"; then
  fail "拓扑采集失败，后续断言无法进行" "$(head -c 600 "$RESP" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
  exit 1
fi
expect_eq "所有 health.status 都在闭集内" "True" \
  "$(jget "$WORK/topo-h1.json" 'all(n["health"]["status"] in {"healthy","warning","critical","unknown","unsupported","sleeping"} for n in d["data"]["nodes"])')"
expect_eq "分区/loop/rom 的 health 是 unsupported" "True" \
  "$(jget "$WORK/topo-h1.json" 'all(n["health"]["status"] == "unsupported" for n in d["data"]["nodes"] if n["kind"] in {"partition","loop","rom"})')"
expect_eq "unsupported 都带可读理由" "True" \
  "$(jget "$WORK/topo-h1.json" 'all(n["health"]["details"] for n in d["data"]["nodes"] if n["health"]["status"] == "unsupported")')"
expect_eq "health.source 在 {smartctl, sysfs, none} 内" "True" \
  "$(jget "$WORK/topo-h1.json" 'all(n["health"]["source"] in {"smartctl","sysfs","none"} for n in d["data"]["nodes"])')"
expect_eq "温度没有 0 °C：要么 null 要么 > 0" "True" \
  "$(jget "$WORK/topo-h1.json" 'all(n["health"]["temperature_celsius"] is None or n["health"]["temperature_celsius"] > 0 for n in d["data"]["nodes"])')"
expect_eq "非物理盘不带温度" "True" \
  "$(jget "$WORK/topo-h1.json" 'all(n["health"]["temperature_celsius"] is None for n in d["data"]["nodes"] if n["kind"] != "disk")')"

DISK_COUNT=$(jget "$WORK/topo-h1.json" 'len([n for n in d["data"]["nodes"] if n["kind"] == "disk"])')
if [ "$DISK_COUNT" = "0" ]; then
  notrun "物理盘健康交叉验证" "本机没有物理盘节点"
elif [ -z "$SMARTCTL" ]; then
  notrun "物理盘健康交叉验证" "本机没有 smartctl"
else
  # SMART 探测在独立 goroutine 里，启动后第一次探测可能还没落地：最多等 40 秒。
  PROBED=""
  for _ in $(seq 1 40); do
    topology "$WORK/topo-h2.json" || break
    PROBED=$(jget "$WORK/topo-h2.json" 'all(n["health"]["checked_at"] is not None for n in d["data"]["nodes"] if n["kind"] == "disk")')
    [ "$PROBED" = "True" ] && break
    sleep 1
  done
  expect_eq "每块物理盘都完成了至少一次 SMART 采集（checked_at 非空）" "True" "$PROBED"
  cp "$WORK/topo-h2.json" "$WORK/topo-health.json"

  # 自己跑一遍 smartctl，逐盘对照。抽出来的规则与代码里的分支一一对应。
  mkdir -p "$WORK/smart"
  # 一行一个设备名：jget 打出来的是 Python repr，直接 for 循环会拿到方括号与引号。
  for dev in $(jget "$WORK/topo-health.json" '"\n".join(sorted(n["name"] for n in d["data"]["nodes"] if n["kind"] == "disk"))'); do
    "$SMARTCTL" -j -i -H -A "/dev/$dev" >"$WORK/smart/$dev.json" 2>"$WORK/smart/$dev.err"
    printf '%s' "$?" >"$WORK/smart/$dev.rc"
  done
  cat >"$WORK/cross.py" <<'PY'
import json, os, sys
topo = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
smdir = sys.argv[2]
bad = []
for n in topo["nodes"]:
    if n["kind"] != "disk":
        continue
    dev = n["name"]
    path = os.path.join(smdir, dev + ".json")
    if not os.path.exists(path):
        bad.append(f"{dev}: 没有自己的 smartctl 输出")
        continue
    try:
        rep = json.load(open(path, encoding="utf-8"))
    except Exception as exc:
        bad.append(f"{dev}: smartctl 输出不是 JSON（{exc}）")
        continue
    rc = int(open(os.path.join(smdir, dev + ".rc"), encoding="utf-8").read() or "-1")
    api = n["health"]
    support = rep.get("smart_support") or {}
    if support.get("available") is False:
        if api["status"] != "unsupported":
            bad.append(f"{dev}: smartctl 说不支持 SMART，API 却是 {api['status']}")
        continue
    passed = (rep.get("smart_status") or {}).get("passed")
    log = rep.get("nvme_smart_health_information_log")
    # 反向也要管：说 healthy 必须有自己的依据（smart_status.passed=true 或 NVMe 健康日志），
    # 不能因为“没读到问题”就报健康——USB 桥不认识这类输出恰好没有任何健康字段。
    if api["status"] == "healthy" and passed is not True and log is None:
        bad.append(f"{dev}: API 说 healthy，但 smartctl 自己没给出任何通过结论")
    if rc == 0 and passed is True and api["status"] != "healthy":
        bad.append(f"{dev}: smartctl 结论通过，API 却是 {api['status']}")
    if passed is False and api["status"] != "critical":
        bad.append(f"{dev}: smartctl 结论未通过，API 却是 {api['status']}")
    cur = (rep.get("temperature") or {}).get("current", 0)
    if cur is None or cur <= 0:
        if api["temperature_celsius"] is not None:
            bad.append(f"{dev}: smartctl 没有温度读数，API 给了 {api['temperature_celsius']}")
    else:
        got = api["temperature_celsius"]
        if got is None:
            bad.append(f"{dev}: smartctl 报 {cur} °C，API 却是 null")
        elif abs(got - cur) > 10:
            bad.append(f"{dev}: 温度差太大 smartctl={cur} API={got}")
    if api["source"] != "smartctl":
        bad.append(f"{dev}: source={api['source']}，应当来自 smartctl（设备不在休眠）")
for line in bad:
    print(line)
PY
  # USB 盘（如果有）：拓扑里必须看得见，且它的形状要能被逐盘对账覆盖到。
  # 这里只记录“本机有没有 USB 盘”，结论本身由上面那条逐盘对账负责。
  USB_DEVS=$(jget "$WORK/topo-health.json" '"、".join(sorted(n["name"] for n in d["data"]["nodes"] if (n.get("transport") or "") == "usb"))')
  if [ -n "$USB_DEVS" ]; then
    pass "USB 盘在拓扑里可见（transport=usb：$USB_DEVS），并纳入逐盘对账"
  else
    notrun "USB 盘在拓扑里可见" "本机没有 USB 总线上的块设备"
  fi
  MISMATCH=$(python3 "$WORK/cross.py" "$WORK/topo-health.json" "$WORK/smart")
  if [ -z "$MISMATCH" ]; then
    pass "API 的健康结论与 smartctl 自身输出逐盘一致"
  else
    fail "API 的健康结论与 smartctl 自身输出逐盘一致" "$MISMATCH"
  fi

  # 真实 NVMe（如果有）：必须拿到 healthy + 温度，这是本阶段唯一能真机证实的健康来源。
  if [ "$(jget "$WORK/topo-health.json" 'any(n["name"].startswith("nvme") and n["health"]["status"] == "healthy" for n in d["data"]["nodes"])')" = "True" ]; then
    pass "NVMe 盘给出真实 healthy"
    expect_eq "NVMe 盘给出真实温度（> 0）" "True" \
      "$(jget "$WORK/topo-health.json" 'all(n["health"]["temperature_celsius"] > 0 for n in d["data"]["nodes"] if n["name"].startswith("nvme") and n["health"]["status"] == "healthy")')"
  else
    notrun "NVMe 真实 healthy" "本机没有可读出 healthy 的 NVMe（QEMU SCSI 盘 smart_support.available=false）"
  fi
fi

echo
echo "== 4. SMART 节奏：15 分钟内不重复探测，也不在 5 秒循环里碰盘 =="
topology "$WORK/topo-c1.json" || fail "节奏实验取快照" "见上一条"
CHECKED_BEFORE=$(jget "$WORK/topo-c1.json" '[ (n["name"], n["health"]["checked_at"]) for n in d["data"]["nodes"] if n["kind"] == "disk" ]')
COLLECTED_BEFORE=$(jget "$WORK/topo-c1.json" 'd["data"]["collected_at"]')
: >"$WORK/smart-spawns.txt"
( i=0
  while [ "$i" -lt 12 ]; do
    printf '%s\n' "$(pgrep -cx smartctl 2>/dev/null || echo 0)" >>"$WORK/smart-spawns.txt"
    i=$((i + 1)); sleep 1
  done ) &
SAMPLER=$!
sleep 13
wait "$SAMPLER" 2>/dev/null
topology "$WORK/topo-c2.json" || fail "节奏实验第二次取快照" "见上一条"
COLLECTED_AFTER=$(jget "$WORK/topo-c2.json" 'd["data"]["collected_at"]')
CHECKED_AFTER=$(jget "$WORK/topo-c2.json" '[ (n["name"], n["health"]["checked_at"]) for n in d["data"]["nodes"] if n["kind"] == "disk" ]')

if [ "$COLLECTED_BEFORE" = "$COLLECTED_AFTER" ]; then
  fail "13 秒内拓扑确实在刷新" "collected_at 没变（$COLLECTED_BEFORE）"
else
  pass "13 秒内拓扑确实在刷新（5 秒节奏）"
fi
expect_eq "13 秒内 SMART 的 checked_at 不变（15 分钟节奏）" "$CHECKED_BEFORE" "$CHECKED_AFTER"
SPAWN_PEAK=$(sort -n "$WORK/smart-spawns.txt" 2>/dev/null | tail -1)
expect_eq "5 秒扫描循环里没有启动 smartctl" "0" "${SPAWN_PEAK:-0}"

echo
echo "== 5. diskstats：自己的那一行、速率、null 语义 =="
cat >"$WORK/check_io.py" <<'PY'
import json, sys
topo = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
def load(path):
    out = {}
    for line in open(path, encoding="utf-8"):
        f = line.split()
        if len(f) < 14:
            continue
        out[f[2]] = {
            "reads": int(f[3]), "rsect": int(f[5]),
            "writes": int(f[7]), "wsect": int(f[9]),
        }
    return out
before = load(sys.argv[2])
after = load(sys.argv[3])
bad = []
for n in topo["nodes"]:
    name = n["name"]
    io = n["io"]
    if name not in after:
        if io is not None:
            bad.append(f"{name}: /proc/diskstats 没有这一行，却给了 io")
        continue
    if io is None:
        bad.append(f"{name}: /proc/diskstats 有这一行，io 却是 null")
        continue
    # 采样时刻被夹在 before 与 after 两次本机读数之间（等待采样时间落在窗口内才断言），
    # 所以四个计数都必须落在这个区间里：比 after 大说明不是本机这一行，比 before 小说明是旧值。
    for key, src, mult in (("read_bytes_total", "rsect", 512), ("write_bytes_total", "wsect", 512),
                           ("reads_total", "reads", 1), ("writes_total", "writes", 1)):
        v = io[key]
        if not isinstance(v, str) or not v.isdigit():
            bad.append(f"{name}.{key} = {v!r}，应当是十进制字符串")
            continue
        v = int(v)
        lo = before.get(name, after[name])[src] * mult
        hi = after[name][src] * mult
        if not (lo <= v <= hi):
            bad.append(f"{name}.{key} = {v}，不在采样窗口 [{lo}, {hi}] 内")
    rate = io["rate"]
    if rate is not None:
        if not (1000 <= rate["interval_ms"] <= 60000):
            bad.append(f"{name}.rate.interval_ms = {rate['interval_ms']}，不像真实间隔")
        for key in ("read_bytes_per_second", "write_bytes_per_second"):
            if not isinstance(rate[key], str) or not rate[key].isdigit():
                bad.append(f"{name}.rate.{key} = {rate[key]!r}，应当是十进制字符串")
        for key in ("read_iops", "write_iops"):
            if rate[key] < 0:
                bad.append(f"{name}.rate.{key} < 0")
        for key in ("read_latency_ms", "write_latency_ms"):
            if rate[key] is not None and rate[key] < 0:
                bad.append(f"{name}.rate.{key} < 0")
        # 没有完成的读就不该有平均读延迟：那是“没有样本”，不是“等了 0 毫秒”。
        if rate["read_latency_ms"] is not None and int(io["reads_total"]) == 0:
            bad.append(f"{name}: 读完成次数为 0 却给了读延迟")
# 分区不能把 I/O 累加到整盘上：分区的计数必然 ≤ 所属整盘。
by_id = {n["id"]: n for n in topo["nodes"]}
for e in topo["edges"]:
    if e["relation"] != "partition_of":
        continue
    child, parent = by_id.get(e["from_device_id"]), by_id.get(e["to_device_id"])
    if not child or not parent or not child["io"] or not parent["io"]:
        continue
    if int(child["io"]["reads_total"]) > int(parent["io"]["reads_total"]):
        bad.append(f"{child['id']} 的读次数大于 {parent['id']}：看起来把父子 I/O 相加了")
print("\n".join(bad))
PY

# io.sampled_at 是 5 秒扫描的时刻，而查询返回的是最近一次快照（可能旧一拍）。
# 先记下本机 t0 与计数，等到拿到一份 sampled_at ≥ t0 的快照，再立刻读一次计数：
# 采样时刻必落在 [before, after] 之间，四个计数才有可断言的窗口。
cat >"$WORK/fresh.py" <<'PY'
import datetime, re, sys
def ts(x):
    # Go 的 RFC3339Nano 有 9 位小数秒，3.10 的 fromisoformat 只认 6 位。
    return datetime.datetime.fromisoformat(
        re.sub(r"\.(\d{6})\d+", r".\1", x).replace("Z", "+00:00")).timestamp()
print(ts(sys.argv[1]) >= float(sys.argv[2]))
PY

IO_BAD=""
IO_SYNCED=0
date +%s.%N >"$WORK/io-t0.txt"
cat /proc/diskstats >"$WORK/diskstats-before.txt"
for _try in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
  if ! topology "$WORK/topo-io.json"; then IO_BAD="拓扑采集失败"; break; fi
  MAXS=$(jget "$WORK/topo-io.json" 'max([n["io"]["sampled_at"] for n in d["data"]["nodes"] if n["io"]] or [d["data"]["collected_at"]])')
  if [ "$(python3 "$WORK/fresh.py" "$MAXS" "$(cat "$WORK/io-t0.txt")")" = "True" ]; then
    IO_SYNCED=1
    break
  fi
  sleep 0.5
done
cat /proc/diskstats >"$WORK/diskstats-after.txt"
if [ "$IO_SYNCED" != "1" ] && [ -z "$IO_BAD" ]; then
  fail "每台设备的计数与 /proc/diskstats 自己那一行对得上" \
    "7.5 秒内拿不到采样时间晚于本机读数的快照，采样时刻与扫描节奏对不上"
else
  IO_BAD=$(python3 "$WORK/check_io.py" "$WORK/topo-io.json" "$WORK/diskstats-before.txt" "$WORK/diskstats-after.txt")
  if [ -z "$IO_BAD" ]; then
    pass "每台设备的计数与 /proc/diskstats 自己那一行对得上（且是十进制字符串）"
  else
    fail "每台设备的计数与 /proc/diskstats 自己那一行对得上" "$IO_BAD"
  fi
fi

echo
echo "== 6. io 变化不改变 snapshot_version =="
V1=$(jget "$WORK/topo-io.json" 'd["data"]["snapshot_version"]')
IO1=$(jget "$WORK/topo-io.json" '[ (n["id"], n["io"]["reads_total"] if n["io"] else None) for n in d["data"]["nodes"] ]')
sleep 11
topology "$WORK/topo-v2.json" || fail "版本实验取快照" "见上一条"
V2=$(jget "$WORK/topo-v2.json" 'd["data"]["snapshot_version"]')
IO2=$(jget "$WORK/topo-v2.json" '[ (n["id"], n["io"]["reads_total"] if n["io"] else None) for n in d["data"]["nodes"] ]')
expect_eq "11 秒后 snapshot_version 不变（io 不进指纹）" "$V1" "$V2"
if [ "$IO1" = "$IO2" ]; then
  note "11 秒内计数器没有变化：本机这段时间没有 I/O（不影响版本断言）"
else
  pass "期间 io 确实变化了，而版本没变（证明 io 不参与指纹）"
fi

echo
echo "== 7. statfs helper 入口只接受一个绝对路径 =="
HELPER_OUT=$("$AGENT" --statfs-helper / 2>&1)
HELPER_RC=$?
if [ "$HELPER_RC" -eq 0 ]; then pass "helper 对 / 返回 0"; else fail "helper 对 / 返回 0" "退出码 $HELPER_RC：$HELPER_OUT"; fi
expect_contains "helper 输出带容量" '"capacity"' "$HELPER_OUT"
expect_eq "helper 输出里的总容量是十进制字符串" "True" \
  "$(printf '%s' "$HELPER_OUT" >"$WORK/helper.json"; jget "$WORK/helper.json" 'isinstance(d["capacity"]["total_bytes"], str) and d["capacity"]["total_bytes"].isdigit()')"

for badargs in "" "relative/path" "/ /"; do
  # shellcheck disable=SC2086
  "$AGENT" --statfs-helper $badargs >"$WORK/helper-bad.json" 2>&1
  rc=$?
  if [ "$rc" -ne 2 ]; then
    fail "helper 拒绝非法参数 '$badargs'" "退出码 $rc（期望 2）"
  elif ! jget "$WORK/helper-bad.json" 'd.get("error")' >/dev/null 2>&1; then
    fail "helper 拒绝非法参数 '$badargs'" "没有给出 JSON 错误"
  else
    pass "helper 拒绝非法参数 '$badargs'"
  fi
done

echo
echo "== 8. 不可达网络挂载：超时、隔离、不影响拓扑 =="
if [ "$NFS_TOOLS" != "1" ]; then
  notrun "网络挂载隔离实验" "本机没有 NFS 工具（exportfs / mount.nfs4）"
else
  mkdir -p "$NFS_SRC" "$NFS_MNT"
  printf '%s *(ro,async,no_subtree_check,fsid=0)\n' "$NFS_SRC" >/etc/exports
  exportfs -ra >/dev/null 2>&1
  systemctl restart nfs-server >/dev/null 2>&1
  sleep 2
  if timeout -k 5 20 mount -t nfs4 127.0.0.1:/ "$NFS_MNT" -o hard,timeo=600,retrans=2,ro >"$WORK/nfs-mount.log" 2>&1; then
    NFS_READY=1
    pass "在本机挂起一条 hard NFS4 挂载（statfs 会无限重试）"
  else
    notrun "网络挂载隔离实验" "本机 NFS 自挂载失败：$(tail -2 "$WORK/nfs-mount.log" 2>/dev/null)"
  fi
fi

if [ -n "${NFS_READY:-}" ]; then
  # 网络挂载在快照里是**独立列表**（PLAN.md 第 8 节）：它们是伪设备号，
  # 本机 sysfs 里没有对应的块设备，不可能塞进某台盘的 filesystem.mounts。
  cap_probe='found = [m for m in d["data"]["network_mounts"] if m["path"] == a[0]]
print(found[0]["capacity"] is not None if found else "missing")'
  fstype_probe='found = [m for m in d["data"]["network_mounts"] if m["path"] == a[0]]
print(found[0]["fstype"] if found else "")'
  present_probe='print(any(m["path"] == a[0] for m in d["data"]["network_mounts"]))'
  err_probe='found = [m for m in d["data"]["network_mounts"] if m["path"] == a[0]]
print(found[0].get("capacity_error", "") if found else "")'
  reachable_probe='found = [m for m in d["data"]["network_mounts"] if m["path"] == a[0]]
print(found[0]["reachable"] if found else "missing")'

  # 先确认代理把它当成网络挂载并且容量读得到。
  NFS_CAP=""
  for _ in $(seq 1 20); do
    topology "$WORK/topo-nfs1.json" || break
    NFS_CAP=$(jgetx "$WORK/topo-nfs1.json" "$NFS_MNT" "$cap_probe")
    if [ "$NFS_CAP" = "True" ]; then break; fi
    sleep 1
  done
  expect_eq "NFS 挂载在快照里可见且容量可读" "True" "${NFS_CAP:-False}"
  expect_eq "NFS 挂载的 fstype 是 nfs4（决定走隔离通道）" "nfs4" \
    "$(jgetx "$WORK/topo-nfs1.json" "$NFS_MNT" "$fstype_probe")"
  expect_eq "网络挂载没有被塞进某台盘的 filesystem.mounts" "True" \
    "$(jgetx "$WORK/topo-nfs1.json" "$NFS_MNT" '
print(not any(m["path"] == a[0] for n in d["data"]["nodes"] if n["filesystem"] for m in n["filesystem"]["mounts"]))')"
  expect_eq "读到容量时 reachable 为 true" "True" \
    "$(jgetx "$WORK/topo-nfs1.json" "$NFS_MNT" "$reachable_probe")"

  COLLECTED_PRE_STOP=$(jget "$WORK/topo-nfs1.json" 'd["data"]["collected_at"]')
  systemctl stop nfs-server >/dev/null 2>&1
  note "已停掉本机 NFS 服务：现在对 $NFS_MNT 做 statfs 会挂在内核里"

  # 容量有 30 秒缓存：停服后第一次重查发生在缓存过期的那一刻，超时之后才写 capacity_error。
  # 所以这里等的是“代理自己得出结论”，不是干等一个固定秒数。
  : >"$WORK/helper-spawns.txt"
  rm -f "$WORK/helper-stop"
  ( while [ ! -f "$WORK/helper-stop" ]; do
      printf '%s\n' "$(pgrep -fc 'statfs-helper' 2>/dev/null || echo 0)" >>"$WORK/helper-spawns.txt"
      sleep 1
    done ) &
  SAMPLER=$!
  NFS_CAP=""
  waited=0
  while [ "$waited" -lt 120 ]; do
    topology "$WORK/topo-nfs2.json" || break
    NFS_CAP=$(jgetx "$WORK/topo-nfs2.json" "$NFS_MNT" "$cap_probe")
    [ "$NFS_CAP" = "False" ] && break
    sleep 3
    waited=$((waited + 3))
  done
  : >"$WORK/helper-stop"
  wait "$SAMPLER" 2>/dev/null
  expect_eq "停服后 120 秒内容量变成 null（缓存过期后重查超时）" "False" "${NFS_CAP:-missing}"

  expect_eq "停服后挂载点仍可见" "True" \
    "$(jgetx "$WORK/topo-nfs2.json" "$NFS_MNT" "$present_probe")"
  expect_eq "停服后容量为 null（不是 0，也不是旧数字）" "True" \
    "$(jgetx "$WORK/topo-nfs2.json" "$NFS_MNT" 'found = [m for m in d["data"]["network_mounts"] if m["path"] == a[0]]
print(found[0]["capacity"] is None if found else "missing")')"
  expect_eq "停服后 reachable 为 false（明确的不可达，不是不知道）" "False" \
    "$(jgetx "$WORK/topo-nfs2.json" "$NFS_MNT" "$reachable_probe")"
  NFS_ERR=$(jgetx "$WORK/topo-nfs2.json" "$NFS_MNT" "$err_probe")
  expect_contains "capacity_error 说明是超时" "超时" "$NFS_ERR"
  expect_contains "capacity_error 说明多久之后才重试" "不再重试" "$NFS_ERR"
  COLLECTED_POST_STOP=$(jget "$WORK/topo-nfs2.json" 'd["data"]["collected_at"]')
  if [ "$COLLECTED_PRE_STOP" = "$COLLECTED_POST_STOP" ]; then
    fail "卡住的网络挂载没有拖住 5 秒扫描" "collected_at 停在 $COLLECTED_PRE_STOP"
  else
    pass "卡住的网络挂载没有拖住 5 秒扫描（collected_at 仍在前进）"
  fi
  expect_eq "其余设备的 io 仍然完整（采集没有被一起拖死）" "True" \
    "$(jget "$WORK/topo-nfs2.json" 'all(n["io"] is not None for n in d["data"]["nodes"] if n["kind"] == "disk")')"
  SPAWN_MAX=$(sort -n "$WORK/helper-spawns.txt" 2>/dev/null | tail -1)
  if [ "${SPAWN_MAX:-0}" -le 1 ]; then
    pass "隔离生效：整个停服期间最多只有 1 个 statfs 子进程"
  else
    fail "隔离生效：整个停服期间最多只有 1 个 statfs 子进程" "峰值 $SPAWN_MAX"
  fi

  # STAGES §6 验收项“采集失败不会卡住登录”。
  # 此刻 NFS 服务仍然是停的、挂载点是 hard（statfs 会挂在内核里），正是最容易把
  # Web 拖死的时刻：先把管理员建好、把 Web 起起来，再掐表登录一次。
  # 登录路径不该碰存储采集，所以它必须在几百毫秒内返回，而不是等到 statfs 超时。
  printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
    --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
  expect_eq "init-admin 退出码" "0" "$?"
  chown_web_db
  if start_web; then
    pass "statfs 卡住期间 Web 起得来（$BASE）"
  else
    fail "Web 启动（卡住期间）" "$(tail -5 "$WEB_LOG" 2>/dev/null)"
  fi
  LOGIN_T0=$(now_ms)
  STALL_LOGIN=$(login admin "$PASSWORD" "$WORK/stall-admin.jar" login-stall-admin)
  LOGIN_T1=$(now_ms)
  LOGIN_MS=$((LOGIN_T1 - LOGIN_T0))
  expect_eq "NFS 卡住时登录仍然 200" "200" "$STALL_LOGIN"
  if [ "$LOGIN_MS" -lt 5000 ]; then
    pass "登录耗时 ${LOGIN_MS} 毫秒（< 5000，没有被 statfs 拖住）"
  else
    fail "登录耗时 ${LOGIN_MS} 毫秒" "超过 5 秒：登录路径可能踩到了挂住的挂载点"
  fi
  # 同一时刻再确认挂载还是卡着的：上面那次登录快，不是因为卡点已经过去了。
  if topology "$WORK/topo-stall.json"; then
    expect_eq "登录的那一刻网络挂载仍不可达（不是卡点已过）" "False" \
      "$(jgetx "$WORK/topo-stall.json" "$NFS_MNT" "$cap_probe")"
  else
    fail "登录的那一刻网络挂载仍不可达（不是卡点已过）" "取快照失败"
  fi
  stop_web
  note "已停掉 Web（第 9 段会用同一份数据库重新启动）"

  # 服务恢复后，隔离到期（5 分钟）才会重新尝试。这一步很慢，可用 WAIT_QUARANTINE=0 跳过。
  WAIT_QUARANTINE=${WAIT_QUARANTINE:-360}
  if [ "$WAIT_QUARANTINE" -le 0 ]; then
    notrun "隔离到期后容量自愈" "WAIT_QUARANTINE=0：本轮不等待 5 分钟隔离期"
  else
    systemctl start nfs-server >/dev/null 2>&1
    RECOVERED=""
    waited=0
    while [ "$waited" -lt "$WAIT_QUARANTINE" ]; do
      topology "$WORK/topo-nfs3.json" || break
      RECOVERED=$(jgetx "$WORK/topo-nfs3.json" "$NFS_MNT" "$cap_probe")
      [ "$RECOVERED" = "True" ] && break
      sleep 15
      waited=$((waited + 15))
    done
    expect_eq "NFS 恢复后容量在隔离到期后自动回来" "True" "${RECOVERED:-False}"
  fi

  timeout -k 5 20 umount -l "$NFS_MNT" 2>/dev/null
  systemctl start nfs-server >/dev/null 2>&1
  NFS_READY=""
  note "已卸载 NFS 挂载并恢复 NFS 服务"
else
  # 没有 nfs-kernel-server 就制造不出“statfs 卡住”。仍然要验“登录不被采集拖住”，
  # 但卡住场景单独标 NOT RUN——不拿正常状态冒充卡住状态。
  printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
    --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
  expect_eq "init-admin 退出码" "0" "$?"
  chown_web_db
  if start_web; then
    pass "Web 起得来（$BASE）"
  else
    fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"
  fi
  LOGIN_T0=$(now_ms)
  PLAIN_LOGIN=$(login admin "$PASSWORD" "$WORK/plain-admin.jar" login-plain-admin)
  LOGIN_T1=$(now_ms)
  expect_eq "登录 200" "200" "$PLAIN_LOGIN"
  pass "登录耗时 $((LOGIN_T1 - LOGIN_T0)) 毫秒"
  notrun "NFS 卡住时登录不受影响" "本机没有 nfs-kernel-server，制造不了 statfs 卡住"
  stop_web
  note "已停掉 Web（第 9 段会用同一份数据库重新启动）"
fi

echo
echo "== 9. Web 透传（权限 V、字段原样） =="
# 管理员在第 8 段已经建好（那时要么 NFS 卡着、要么明说没有 NFS）。init-admin 不允许
# 覆盖已有账号，这里断言“第二次执行会被拒绝”，然后直接复用同一份数据库启动 Web。
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init2.out" 2>"$WORK/init2.err"
INIT2=$?
if [ "$INIT2" -ne 0 ] && grep -q "只用于首次初始化" "$WORK/init2.err"; then
  pass "重复 init-admin 被拒绝（不覆盖已有管理员）"
else
  fail "重复 init-admin 被拒绝（不覆盖已有管理员）" "exit=$INIT2 $(head -c 200 "$WORK/init2.err")"
fi
chown_web_db
if start_web; then pass "Web 以 $WEB_USER 身份在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

ANON=$(curl -s -o "$WORK/anon.json" -w '%{http_code}' "$BASE/api/v1/storage/devices")
expect_eq "未登录读设备列表 401" "401" "$ANON"

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")
VIEWER_CSRF=""
if [ "$code" = "200" ]; then
  code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
    "{\"username\":\"viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
  case $code in 200|201|409) : ;; *) fail "创建 viewer" "HTTP $code $(head -c 200 "$WORK/resp.json")" ;; esac
  code=$(login viewer "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
  if [ "$code" = "200" ]; then VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json"); fi
fi

if [ -n "$VIEWER_CSRF" ]; then
  code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" GET "/api/v1/storage/devices")
  expect_eq "viewer 读设备列表 200（权限 V）" "200" "$code"
  cp "$WORK/resp.json" "$WORK/web-devices.json"
  expect_eq "Web 返回的每台设备都带 io 字段（可为 null）" "True" \
    "$(jget "$WORK/web-devices.json" 'all("io" in n for n in d["data"]["devices"])')"
  expect_eq "Web 没有改写 health" "True" \
    "$(jget "$WORK/web-devices.json" 'all(n["health"]["status"] in {"healthy","warning","critical","unknown","unsupported","sleeping"} for n in d["data"]["devices"])')"
  expect_eq "Web 返回的 io 字节字段是字符串" "True" \
    "$(jget "$WORK/web-devices.json" 'all(isinstance(n["io"]["read_bytes_total"], str) for n in d["data"]["devices"] if n["io"])')"
  expect_eq "Web 返回的挂载点容量字段存在（可为 null）" "True" \
    "$(jget "$WORK/web-devices.json" 'all("capacity" in m for n in d["data"]["devices"] if n["filesystem"] for m in n["filesystem"]["mounts"])')"

  # 网络挂载走 /storage/topology：它们不在设备列表里（没有块设备），
  # 但 viewer 也该看到——这是只读信息，权限 V。
  code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" GET "/api/v1/storage/topology")
  expect_eq "viewer 读拓扑 200（权限 V）" "200" "$code"
  cp "$WORK/resp.json" "$WORK/web-topology.json"
  expect_eq "Web 拓扑里有 network_mounts 列表（没有也是 []，不是 null）" "True" \
    "$(jget "$WORK/web-topology.json" 'isinstance(d["data"].get("network_mounts"), list)')"
  expect_eq "拓扑里的网络挂载字段完整" "True" \
    "$(jget "$WORK/web-topology.json" 'all(all(k in m for k in ("path", "source", "fstype", "read_only", "reachable", "capacity")) for m in d["data"]["network_mounts"])')"
else
  fail "viewer 读取" "viewer 未登录成功，Web 断言无法进行"
fi

echo
echo "== 10. 没有 smartctl 时：unknown，不是 healthy =="
stop_agent
write_cfg "$SYS_CFG" 15 2 "/nonexistent/smartctl-for-acceptance"
if start_agent; then
  topology "$WORK/topo-nosmart.json" || fail "没有 smartctl 时取快照" "见上一条"
  expect_eq "配置了不存在的 smartctl_path 时，物理盘仍是 unknown" "True" \
    "$(jget "$WORK/topo-nosmart.json" 'all(n["health"]["status"] == "unknown" for n in d["data"]["nodes"] if n["kind"] == "disk")')"
  expect_eq "unknown 带说明而不是空" "True" \
    "$(jget "$WORK/topo-nosmart.json" 'all(n["health"]["details"] for n in d["data"]["nodes"] if n["kind"] == "disk")')"
  expect_eq "分区仍然是 unsupported（不是 unknown）" "True" \
    "$(jget "$WORK/topo-nosmart.json" 'all(n["health"]["status"] == "unsupported" for n in d["data"]["nodes"] if n["kind"] in {"partition","loop","rom"})')"
else
  fail "用不存在的 smartctl_path 启动代理" "代理没有起来（配置写错不应导致启动失败，只会让健康保持 unknown）"
fi
stop_agent
write_cfg "$SYS_CFG" 15 2 ""
if start_agent; then pass "恢复默认配置后代理重新启动"; else fail "恢复默认配置后代理重新启动" "$(tail -5 "$AGENT_LOG")"; fi

echo
echo "== 11. 日志卫生 =="
if [ -r "$AGENT_LOG" ]; then
  LEAK=$(grep -E "smartctl -j|-n standby|BEGIN OPENSSH|PRIVATE KEY" "$AGENT_LOG" 2>/dev/null | head -3)
  if [ -z "$LEAK" ]; then
    pass "代理日志里没有命令行回显与私钥"
  else
    fail "代理日志里没有命令行回显与私钥" "$LEAK"
  fi
else
  notrun "日志卫生" "没有代理日志"
fi

echo
printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
if [ "$NOTRUN" -gt 0 ]; then
  printf '未执行项：%s\n' "$NOTRUN_NAMES"
fi
exit 0
