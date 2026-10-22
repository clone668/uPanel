#!/bin/sh
# P04.2 Linux 验收脚本：存储拓扑只读（块设备树、设备身份校验、系统盘保护识别）。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并且要用 upanel-web 身份去查 socket）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-2-storage-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P04.2、PLAN §8、CONTRACTS §6/§6.1）：
#   1. capabilities 如实报告 storage.topology，写动作仍然为空
#   2. 设备集合与 /sys 完全一致：/sys/block 下的每台设备、每台设备下带 partition 属性的
#      分区，一个不多一个不少
#   3. 每台设备的 size_bytes / read_only 与 sysfs 逐台对齐；size_bytes 是十进制字符串
#   4. 边：分区到磁盘是 partition_of，方向是“子 → 父”，两端都必须是已知设备，
#      且与内核认的分区一一对应（多一条、少一条都算失败）
#   5. 身份：identity_confidence 与可读性一致（读不到 WWN/序列号就不能报 high），
#      指纹在同一次运行内稳定，不同盘不撞车
#   6. 诚实性：P04.2 没有 SMART，所以 physical disk 的 health.status 在未检查时是 unknown、
#      温度为 null（P04.3 起分区/回环/逻辑设备报 unsupported，判定见第 6 节）；
#      capabilities 一律空数组（不提供任何页面动作入口）
#   7. 系统盘保护：从 /proc/self/mountinfo 独立算出承载 / 的设备，必须被标为系统盘；
#      保护沿边递归向上（子设备受保护 ⇒ 父设备也受保护）；理由非空且可读
#   8. 非系统盘不被保护：空白磁盘不保护；临时挂载一块 loop 镜像后，该 loop 设备不出现在
#      保护名单里（证明不是“凡挂载就保护”，也不是按名字猜）
#   9. 挂载点容量：/ 的 total_bytes 与 statvfs 精确一致，available_bytes 落在实时
#      Bavail 的 0.5% 内（容量按 30 秒采集，窗口内会漂移），不是估算值
#  10. 拓扑快照：interval_ms=5000，collected_at 是最新一份并按间隔前进；
#      内容不变时 snapshot_version 不变，设备出现/消失时它跟着变
#  11. 参数与边界：storage.topology 不接受任何参数（含伪装成命令的字段）
#  12. Web：未登录 401；viewer 可读（V）；Web 与代理给出同一个 snapshot_version；
#      设备详情只带与该设备相关的边；未知设备 404；设备 id 不是命令入口；
#      代理停机后 503 AGENT_UNAVAILABLE，且不返回任何旧数值
#  13. 日志卫生：agent/web 日志里不出现 shell 命令行与私钥

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p04-2}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=19446
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

# capabilities 的断言不再写死整张清单（见 §1 的说明）：每过一个阶段能力表就会变长，
# 写死就只能在每个阶段改期望值，最后断言变成橡皮图章。改为钉“存储查询在、且没有
# storage.* 写动作被登记成可执行”这两条跨阶段不变量。


AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log

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
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
notrun() { NOTRUN=$((NOTRUN + 1)); NOTRUN_NAMES="$NOTRUN_NAMES | $1"; printf 'NOTRUN  %s: %s\n' "$1" "${2:-}"; }
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

echo "== 0. 环境"
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

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/out-web" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/out-web" "$WORK/logs"
chown "$WEB_USER" "$WORK/out-web"
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo
echo "== 1. 启动代理（生产登记表，不带验收动作） =="
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
    "shutdown_timeout_seconds": 10
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
  rm -f "$KEY_FILE"
  if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
  fi
fi

if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

# socket 文件出现不等于已经在 accept：两个采集循环启动的那一瞬间，连接可能还进不了队列。
# 这里明确等到 capabilities 能应答为止（最多 10 秒），等不到才算失败——不靠“再试一次碰运气”。
READY=""
for _ in $(seq 1 50); do
  if [ "$(runuser -u "$WEB_USER" -- curl -s -o /dev/null -w '%{http_code}' --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null)" = "200" ]; then
    READY=1
    break
  fi
  sleep 0.2
done
if [ -n "$READY" ]; then pass "代理在 10 秒内开始应答（capabilities 200）"; else fail "代理在 10 秒内开始应答" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

call web GET /internal/v1/capabilities
expect_eq "capabilities 200" "200" "$CODE"
# 不再钉死整张清单：能力表每过一个阶段就会变长（P05 的服务/进程、P06 的文件/终端、
# P07.2 的分区写动作），钉死清单只会让这个脚本在每个阶段都变红，而“红了就改期望值”
# 等于把断言变成橡皮图章。这里改钉跨阶段真正成立的：
#   * 存储查询在表里；
#   * storage.* 写动作名只能出自四个固定动作（不许凭空多一个名字进来）；
#   * 写动作名不会混进 supported_queries（两个列表不是同一个东西）；
#   * 两张表本身有序且无重复。
expect_eq "capabilities 报告 storage.topology 查询" "True" \
  "$(jget "$RESP" '"storage.topology" in d["data"]["supported_queries"]')"
expect_eq "storage.* 写动作名只能出自四个固定动作" "" \
  "$(jget "$RESP" '",".join(a for a in sorted(set(d["data"]["supported_actions"])) if a.startswith("storage.") and a not in ("storage.partition_table.initialize","storage.partition.create","storage.partition.delete","storage.filesystem.format"))')"
expect_eq "写动作名没有混进 supported_queries" "[]" \
  "$(jget "$RESP" 'sorted(q for q in d["data"]["supported_queries"] if q in ("storage.partition_table.initialize","storage.partition.create","storage.partition.delete","storage.filesystem.format"))')"
expect_eq "supported_actions 有序且无重复" "True" \
  "$(jget "$RESP" 'd["data"]["supported_actions"] == sorted(set(d["data"]["supported_actions"]))')"
expect_eq "supported_queries 有序且无重复" "True" \
  "$(jget "$RESP" 'd["data"]["supported_queries"] == sorted(set(d["data"]["supported_queries"]))')"

echo
echo "== 2. 拉一份拓扑快照 =="
call web POST /internal/v1/queries '{"query":"storage.topology"}'
expect_eq "storage.topology 200" "200" "$CODE"
if [ "$CODE" != "200" ]; then
  fail "拓扑采集失败，后续断言无法进行" "$(head -c 600 "$RESP" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
  exit 1
fi
cp "$RESP" "$WORK/topo-1.json"
T1=$(jget "$WORK/topo-1.json" 'd["data"]["collected_at"]')
V1=$(jget "$WORK/topo-1.json" 'd["data"]["snapshot_version"]')
expect_eq "status=ok" "ok" "$(jget "$WORK/topo-1.json" 'd["data"]["status"]')"
expect_eq "interval_ms=5000（PLAN §8：磁盘计数器每 5 秒）" "5000" \
  "$(jget "$WORK/topo-1.json" 'd["data"]["interval_ms"]')"
expect_eq "collected_at 是最近 15 秒内的快照" "True" "$(ts_age_lt "$T1" 15)"
expect_eq "snapshot_version 是 sha256 指纹" "True" \
  "$(jget "$WORK/topo-1.json" 'len(d["data"]["snapshot_version"]) == 71 and d["data"]["snapshot_version"].startswith("sha256:")')"

echo
echo "== 3. 设备集合与 /sys 完全一致 =="
sysfs_top_ids | sort -u >"$WORK/sysfs-top.txt"
sysfs_partitions >"$WORK/sysfs-parts.txt"
( cat "$WORK/sysfs-top.txt"; awk '{print $1}' "$WORK/sysfs-parts.txt" ) | sort -u >"$WORK/sysfs-ids.txt"
jget "$WORK/topo-1.json" '"\n".join(sorted(n["id"] for n in d["data"]["nodes"]))' >"$WORK/api-ids.txt"
if diff -u "$WORK/sysfs-ids.txt" "$WORK/api-ids.txt" >"$WORK/ids.diff" 2>&1; then
  pass "设备集合与 /sys/block(+partition) 完全一致（$(wc -l <"$WORK/api-ids.txt" | tr -d ' ') 台）"
else
  fail "设备集合与 /sys/block(+partition) 一致" "$(head -20 "$WORK/ids.diff")"
fi

: >"$WORK/sysfs-attrs.txt"
while read -r id; do
  [ -n "$id" ] || continue
  size=$(dev_attr "$id" size)
  ro=$(dev_attr "$id" ro)
  [ -n "$size" ] || continue
  printf '%s\t%s\t%s\n' "$id" "$((size * 512))" "${ro:-0}" >>"$WORK/sysfs-attrs.txt"
done <"$WORK/api-ids.txt"

jgetx "$WORK/topo-1.json" "$WORK/sysfs-attrs.txt" '
want = {}
for line in open(a[0], encoding="utf-8"):
    parts = line.split()
    if len(parts) == 3:
        want[parts[0]] = (int(parts[1]), parts[2] == "1")
bad = []
for n in d["data"]["nodes"]:
    if n["id"] not in want:
        bad.append((n["id"], "missing-in-sysfs"))
        continue
    size, ro = want[n["id"]]
    if str(n["size_bytes"]) != str(size):
        bad.append((n["id"], "size", str(n["size_bytes"]), size))
    if bool(n["read_only"]) != ro:
        bad.append((n["id"], "read_only", bool(n["read_only"]), ro))
print(bad)' >"$WORK/attr-mismatch.txt"
if [ "$(cat "$WORK/attr-mismatch.txt")" = "[]" ]; then
  pass "每台设备的 size_bytes 与 read_only 都和 /sys 精确一致"
else
  fail "size_bytes / read_only 与 /sys 一致" "$(cat "$WORK/attr-mismatch.txt")"
fi

expect_eq "size_bytes 是十进制字符串（不是数字、不是 null）" "True" \
  "$(jget "$WORK/topo-1.json" 'all(isinstance(n["size_bytes"], str) and n["size_bytes"].isdigit() for n in d["data"]["nodes"])')"
expect_eq "磁盘的逻辑扇区 >= 512 且物理扇区是它的整数倍" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["logical_sector_bytes"] >= 512 and n["physical_sector_bytes"] % n["logical_sector_bytes"] == 0 for n in d["data"]["nodes"] if n["kind"] == "disk")')"
# kind 要跟着内核的分区属性走，不能按名字前缀猜：sda1 是分区、sda 不是，
# 而 md0/dm-0 甚至不以数字结尾。
jgetx "$WORK/topo-1.json" "$WORK/sysfs-parts.txt" '
parts = {tuple(line.split())[0] for line in open(a[0], encoding="utf-8") if line.split()}
bad = [n["id"] for n in d["data"]["nodes"] if (n["id"] in parts) != (n["kind"] == "partition")]
print(bad)' >"$WORK/kind-bad.txt"
if [ "$(cat "$WORK/kind-bad.txt")" = "[]" ]; then
  pass "kind=partition 的设备集合与内核 partition 属性完全对应"
else
  fail "kind=partition 与内核 partition 属性对应" "$(cat "$WORK/kind-bad.txt")"
fi

echo
echo "== 4. 边的方向与一致性 =="
expect_eq "边的两端都是已知设备 id" "True" \
  "$(jget "$WORK/topo-1.json" '
ids = {n["id"] for n in d["data"]["nodes"]}
print(all(e["from_device_id"] in ids and e["to_device_id"] in ids for e in d["data"]["edges"]))')"
expect_eq "relation 只有 partition_of / depends_on" "True" \
  "$(jget "$WORK/topo-1.json" 'all(e["relation"] in ("partition_of", "depends_on") for e in d["data"]["edges"])')"
expect_eq "没有重复边" "True" \
  "$(jget "$WORK/topo-1.json" 'len({(e["from_device_id"], e["to_device_id"], e["relation"]) for e in d["data"]["edges"]}) == len(d["data"]["edges"])')"
expect_eq "sysfs 认的每个分区都有 partition_of 边指向它的磁盘" "True" \
  "$(jgetx "$WORK/topo-1.json" "$WORK/sysfs-parts.txt" '
want = {tuple(line.split()) for line in open(a[0], encoding="utf-8") if line.strip()}
got = {(e["from_device_id"], e["to_device_id"]) for e in d["data"]["edges"] if e["relation"] == "partition_of"}
print(want <= got)')"
expect_eq "partition_of 边不会凭空多出来" "True" \
  "$(jgetx "$WORK/topo-1.json" "$WORK/sysfs-parts.txt" '
want = {tuple(line.split()) for line in open(a[0], encoding="utf-8") if line.strip()}
got = {(e["from_device_id"], e["to_device_id"]) for e in d["data"]["edges"] if e["relation"] == "partition_of"}
print(got <= want)')"
expect_eq "没有自环（from 与 to 相同的边）" "True" \
  "$(jget "$WORK/topo-1.json" 'all(e["from_device_id"] != e["to_device_id"] for e in d["data"]["edges"])')"

echo
echo "== 5. 设备身份：诚实、稳定、不撞车 =="
expect_eq "identity_confidence 只在确实读到 WWN/序列号时才报 high" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["identity_confidence"] != "high" or n["wwn"] is not None or n["serial"] is not None for n in d["data"]["nodes"])')"
expect_eq "identity_confidence 取值合法" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["identity_confidence"] in ("high", "medium", "low", "unknown") for n in d["data"]["nodes"])')"
expect_eq "identity_fingerprint 是 sha256 指纹" "True" \
  "$(jget "$WORK/topo-1.json" 'all(len(n["identity_fingerprint"]) == 71 and n["identity_fingerprint"].startswith("sha256:") for n in d["data"]["nodes"])')"
expect_eq "不同设备的身份指纹不撞车" "True" \
  "$(jget "$WORK/topo-1.json" 'len({n["identity_fingerprint"] for n in d["data"]["nodes"]}) == len(d["data"]["nodes"])')"
# 同型号同容量的两块盘必须能被区分开——这说明指纹不是“型号+容量”拼出来的。
jget "$WORK/topo-1.json" '
groups = {}
for n in d["data"]["nodes"]:
    if n["kind"] != "disk":
        continue
    groups.setdefault((n["model"], n["size_bytes"]), []).append(n["identity_fingerprint"])
twins = {k: v for k, v in groups.items() if len(v) > 1}
print("True" if twins and all(len(set(v)) == len(v) for v in twins.values()) else
      ("NONE" if not twins else "False"))' >"$WORK/twin-check.txt"
case $(cat "$WORK/twin-check.txt") in
  True) pass "同型号同容量的磁盘身份指纹互不相同" ;;
  NONE) notrun "同型号同容量的磁盘身份指纹互不相同" "这台 VM 上没有两台同型号同容量的磁盘" ;;
  *) fail "同型号同容量的磁盘身份指纹互不相同" "$(cat "$WORK/twin-check.txt")" ;;
esac

call web POST /internal/v1/queries '{"query":"storage.topology"}'
cp "$RESP" "$WORK/topo-2.json"
expect_eq "同一份内容两次查询，snapshot_version 不变" "$V1" "$(jget "$WORK/topo-2.json" 'd["data"]["snapshot_version"]')"
expect_eq "两次查询的身份指纹逐台一致（身份稳定）" "True" \
  "$(jgetx "$WORK/topo-2.json" "$WORK/topo-1.json" '
p = {n["id"]: n["identity_fingerprint"] for n in json.load(open(a[0]))["data"]["nodes"]}
print(all(p.get(n["id"]) == n["identity_fingerprint"] for n in d["data"]["nodes"]))')"

echo
echo "== 6. 诚实性：没有 SMART 就不许说健康，没有动作就不许给按钮 =="
# P04.3 起分区、回环设备和逻辑设备会如实报 unsupported（“不直接提供 SMART”），
# 所以 P04.2 当时写的“health.status 一律 unknown”已经不成立。这里换成更严格的不变量：
# 状态必须在闭集内；非物理盘必须是 unsupported；物理盘没真的检查过就必须是 unknown。
# 换成这三条不是为了变绿，而是“没检查过就不许说健康”这条诚实性要求在新状态机下仍然被断言。
expect_eq "health.status 在闭集内" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["health"]["status"] in ("ok","warning","critical","unknown","unsupported") for n in d["data"]["nodes"])')"
expect_eq "非物理盘的健康一律 unsupported" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["kind"] == "disk" or n["health"]["status"] == "unsupported" for n in d["data"]["nodes"])')"
expect_eq "没检查过的物理盘一律 unknown（不许凭猜说健康）" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["kind"] != "disk" or n["health"]["checked_at"] is not None or n["health"]["status"] == "unknown" for n in d["data"]["nodes"])')"
expect_eq "未采集温度时 temperature_celsius 是 null（不是 0）" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["health"]["temperature_celsius"] is None for n in d["data"]["nodes"])')"
# P04.3 起物理盘会在启动后立刻被检查一次，所以“checked_at 一律是 null”已经不成立。
# 换成诚实性不变量：**说了不是 unknown，就必须有检查时间**——不许凭猜说健康。
expect_eq "状态不是 unknown 的物理盘必须有 checked_at（说健康就得有检查时间）" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["health"]["status"] in ("unknown","unsupported") or n["health"]["checked_at"] is not None for n in d["data"]["nodes"])')"
# P07.1 起设备会带存储动作的能力投影（available/blocked/unsupported），不再是空数组。
# 不变量：拓扑里的能力只能引用 storage.* 动作，状态必须在闭集内。
expect_eq "设备能力只引用 storage.* 动作" "True" \
  "$(jget "$WORK/topo-1.json" 'all(all(c["action"].startswith("storage.") for c in n.get("capabilities") or []) for n in d["data"]["nodes"])')"
expect_eq "设备能力的状态在闭集内" "True" \
  "$(jget "$WORK/topo-1.json" 'all(all(c["state"] in ("available","blocked","unsupported") for c in n.get("capabilities") or []) for n in d["data"]["nodes"])')"
expect_eq "读不到的字段是 null 而不是空字符串" "True" \
  "$(jget "$WORK/topo-1.json" 'all(n["serial"] is None or n["serial"] != "" for n in d["data"]["nodes"])')"
expect_eq "protection 三个字段都在，且 reasons/sources 是数组" "True" \
  "$(jget "$WORK/topo-1.json" 'all(isinstance(n["protection"]["system_disk"], bool) and isinstance(n["protection"]["reasons"], list) and isinstance(n["protection"]["sources"], list) for n in d["data"]["nodes"])')"

echo
echo "== 7. 系统盘保护：独立地从 /proc/self/mountinfo 算出应被保护的设备 =="
: >"$WORK/expect-protected.txt"
for cp in / /boot /boot/efi /usr /etc /var /var/lib/upanel /var/lib/upanel-agent /etc/upanel; do
  mp=$(longest_mount "$cp")
  [ -n "$mp" ] || continue
  dev=$(mnt_device "$mp")
  [ -n "$dev" ] || continue
  printf '%s %s\n' "$cp" "$dev" >>"$WORK/expect-protected.txt"
done
awk '{print $2}' "$WORK/expect-protected.txt" | sort -u >"$WORK/expect-protected-dev.txt"
if [ -s "$WORK/expect-protected-dev.txt" ]; then
  pass "从 mountinfo 独立算出承载关键路径的设备（$(tr '\n' ' ' <"$WORK/expect-protected-dev.txt")）"
  expect_eq "承载关键路径的设备都必须被标为系统盘" "True" \
    "$(jgetx "$WORK/topo-1.json" "$WORK/expect-protected-dev.txt" '
want = {line.strip() for line in open(a[0], encoding="utf-8") if line.strip()}
got = {n["id"] for n in d["data"]["nodes"] if n["protection"]["system_disk"]}
print(want <= got)')"
else
  notrun "承载关键路径的设备被保护" "mountinfo 里没有任何关键路径落在块设备上"
fi
ROOT_DEV=$(mnt_device /)
if [ -n "$ROOT_DEV" ]; then
  expect_eq "承载 / 的设备（$ROOT_DEV）被标为系统盘" "True" \
    "$(jgetx "$WORK/topo-1.json" "$ROOT_DEV" 'any(n["id"] == a[0] and n["protection"]["system_disk"] for n in d["data"]["nodes"])')"
  expect_eq "该设备的保护理由写明承载根文件系统" "True" \
    "$(jgetx "$WORK/topo-1.json" "$ROOT_DEV" 'any("根文件系统" in r for n in d["data"]["nodes"] if n["id"] == a[0] for r in n["protection"]["reasons"])')"
  expect_eq "受保护设备的理由与来源都非空" "True" \
    "$(jget "$WORK/topo-1.json" 'all(n["protection"]["reasons"] and n["protection"]["sources"] for n in d["data"]["nodes"] if n["protection"]["system_disk"])')"
else
  fail "承载 / 的设备被标为系统盘" "readlink /sys/dev/block 解析失败"
fi
expect_eq "保护沿边递归向上（子受保护 ⇒ 父也受保护）" "True" \
  "$(jget "$WORK/topo-1.json" '
prot = {n["id"]: n["protection"]["system_disk"] for n in d["data"]["nodes"]}
print(all(not prot.get(e["from_device_id"]) or prot.get(e["to_device_id"]) for e in d["data"]["edges"]))')"

# 空白磁盘必须不被保护：它们是“能拿来分区的东西”，保护判定不能一刀切成全部。
jget "$WORK/topo-1.json" '"\n".join(sorted(n["id"] for n in d["data"]["nodes"]
    if n["kind"] == "disk" and not n["protection"]["system_disk"]
    and not (n["filesystem"] and n["filesystem"]["mounts"])))' >"$WORK/spare-disks.txt"
if [ -s "$WORK/spare-disks.txt" ]; then
  expect_eq "无文件系统、无挂载的空白磁盘不被标为系统盘（$(tr '\n' ' ' <"$WORK/spare-disks.txt")）" "True" \
    "$(jgetx "$WORK/topo-1.json" "$WORK/spare-disks.txt" '
spare = {line.strip() for line in open(a[0], encoding="utf-8") if line.strip()}
print(all(not n["protection"]["system_disk"] for n in d["data"]["nodes"] if n["id"] in spare))')"
else
  notrun "空白磁盘不被保护" "这台 VM 没有多余的空白磁盘"
fi

echo
echo "== 8. 临时挂载 loop 镜像：挂载不等于系统盘 =="
IMG=$WORK/spare.img
mkdir -p "$WORK/mnt"
HAVE_LOOP=1
if ! command -v losetup >/dev/null 2>&1; then
  HAVE_LOOP=0
  notrun "loop 镜像挂载实验" "缺少 losetup"
elif ! command -v mkfs.ext4 >/dev/null 2>&1; then
  HAVE_LOOP=0
  notrun "loop 镜像挂载实验" "缺少 mkfs.ext4"
else
  dd if=/dev/zero of="$IMG" bs=1M count=64 status=none 2>/dev/null
  if ! mkfs.ext4 -q -F "$IMG" >"$WORK/mkfs.log" 2>&1; then
    HAVE_LOOP=0
    notrun "loop 镜像挂载实验" "mkfs.ext4 失败：$(tail -2 "$WORK/mkfs.log" 2>/dev/null)"
  elif ! LOOP_DEV=$(losetup --find --show "$IMG" 2>>"$WORK/mkfs.log"); then
    HAVE_LOOP=0
    LOOP_DEV=""
    notrun "loop 镜像挂载实验" "losetup 失败：$(tail -2 "$WORK/mkfs.log" 2>/dev/null)"
  elif ! mount "$LOOP_DEV" "$WORK/mnt" 2>>"$WORK/mkfs.log"; then
    HAVE_LOOP=0
    notrun "loop 镜像挂载实验" "mount 失败：$(tail -2 "$WORK/mkfs.log" 2>/dev/null)"
    losetup -d "$LOOP_DEV" 2>/dev/null
    LOOP_DEV=""
  fi
fi

if [ "$HAVE_LOOP" -eq 1 ]; then
  pass "已把 64 MiB 临时镜像挂到 $WORK/mnt（$LOOP_DEV）"
  LOOP_ID=$(basename "$LOOP_DEV")
  note "等 6 秒跨过一个扫描间隔，让代理看见这次挂载"
  sleep 6
  call web POST /internal/v1/queries '{"query":"storage.topology"}'
  cp "$RESP" "$WORK/topo-loop.json"
  # loop 设备本身在 P04.2 之前就在 /sys/block 里（内核固定预留几块），
  # 所以这里断言的是“挂载出现了”，不是“设备出现了”——后者在验收机上永远为真，测不出东西。
  expect_eq "loop 设备（$LOOP_ID）的 kind 是 loop" "loop" \
    "$(jgetx "$WORK/topo-loop.json" "$LOOP_ID" '[n["kind"] for n in d["data"]["nodes"] if n["id"] == a[0]][0]')"
  expect_eq "它的挂载点里出现了 $WORK/mnt" "True" \
    "$(jgetx "$WORK/topo-loop.json" "$LOOP_ID" "$WORK/mnt" '
n = [n for n in d["data"]["nodes"] if n["id"] == a[0]][0]
print(bool(n["filesystem"]) and any(m["path"] == a[1] for m in n["filesystem"]["mounts"]))')"
  expect_eq "它没有被标成系统盘（挂载不等于系统盘）" "False" \
    "$(jgetx "$WORK/topo-loop.json" "$LOOP_ID" '[n["protection"]["system_disk"] for n in d["data"]["nodes"] if n["id"] == a[0]][0]')"
  expect_eq "它的容量已 statfs 出来，total_bytes 与 statvfs 完全一致" "True" \
    "$(jgetx "$WORK/topo-loop.json" "$WORK/mnt" '
caps = [m["capacity"] for n in d["data"]["nodes"] if n["filesystem"]
        for m in n["filesystem"]["mounts"] if m["path"] == a[0] and m["capacity"]]
import os
st = os.statvfs(a[0])
print(bool(caps) and int(caps[0]["total_bytes"]) == st.f_blocks * st.f_frsize)')"
  # 这里才是严格对账的地方：这块 ext4 镜像空闲、没有任何写入，
  # mkfs 默认给 root 留 5% 的块（64 MiB 上约 3 MiB），Bfree 明显大于 Bavail，
  # 所以 available_bytes 必须与 Bavail 精确相等——取错成 Bfree 一定会看出来。
  LOOPCAP=$(jgetx "$WORK/topo-loop.json" "$WORK/mnt" '
caps = [m["capacity"] for n in d["data"]["nodes"] if n["filesystem"]
        for m in n["filesystem"]["mounts"] if m["path"] == a[0] and m["capacity"]]
import os
st = os.statvfs(a[0])
print(int(caps[0]["available_bytes"]) if caps else -1,
      st.f_bavail * st.f_frsize, st.f_bfree * st.f_frsize)')
  set -- $LOOPCAP
  note "loop 镜像容量对账: available=$1 Bavail=$2 Bfree=$3（保留块差 $(( $3 - $2 )) 字节）"
  expect_eq "它的 available_bytes 与空闲镜像的 Bavail 精确相等" "$2" "$1"
  LOOP_VERSION=$(jget "$WORK/topo-loop.json" 'd["data"]["snapshot_version"]')
  expect_eq "挂载出现后 snapshot_version 变了" "True" \
    "$(jgetx "$WORK/topo-loop.json" "$V1" 'd["data"]["snapshot_version"] != a[0]')"
  umount "$WORK/mnt" 2>/dev/null
  losetup -d "$LOOP_DEV" 2>/dev/null
  LOOP_DEV=""
  note "等 6 秒让下一次扫描看见挂载消失"
  sleep 6
  call web POST /internal/v1/queries '{"query":"storage.topology"}'
  cp "$RESP" "$WORK/topo-after.json"
  expect_eq "卸载后该挂载点从拓扑里消失" "False" \
    "$(jgetx "$WORK/topo-after.json" "$LOOP_ID" "$WORK/mnt" '
n = [n for n in d["data"]["nodes"] if n["id"] == a[0]][0]
print(bool(n["filesystem"]) and any(m["path"] == a[1] for m in n["filesystem"]["mounts"]))')"
  expect_eq "卸载后不再有该挂载点的容量" "False" \
    "$(jgetx "$WORK/topo-after.json" "$WORK/mnt" '
print(any(m["path"] == a[0] for n in d["data"]["nodes"] if n["filesystem"] for m in n["filesystem"]["mounts"]))')"
  expect_eq "挂载消失后 snapshot_version 也变了" "True" \
    "$(jgetx "$WORK/topo-after.json" "$LOOP_VERSION" 'd["data"]["snapshot_version"] != a[0]')"
  if [ "$(jget "$WORK/topo-after.json" 'd["data"]["snapshot_version"]')" = "$V1" ]; then
    pass "挂载消失后 snapshot_version 回到原值（指纹只由内容决定，与时间无关）"
  else
    note "挂载消失后 snapshot_version 与最初不同（内容确实可能不同，例如分区表探测的 warning 变了）"
  fi
fi

echo
echo "== 9. 快照节奏：collected_at 按 5 秒间隔前进 =="
# 基线必须在这里现取：之前用的是 §2 那份快照的 T1，而 §3—§8（回环挂载、statvfs、
# 卸载）在 TCG 客机上要跑好几分钟，于是 T3−T1 早就超过 30 秒——这条断言其实在
# 断言“前面几节跑得快不快”，与产品无关（本轮就因此假红过一次）。
call web POST /internal/v1/queries '{"query":"storage.topology"}'
cp "$RESP" "$WORK/topo-2.json"
T2=$(jget "$WORK/topo-2.json" 'd["data"]["collected_at"]')
sleep 6
call web POST /internal/v1/queries '{"query":"storage.topology"}'
cp "$RESP" "$WORK/topo-3.json"
T3=$(jget "$WORK/topo-3.json" 'd["data"]["collected_at"]')
expect_eq "collected_at 在一个间隔之后前进了（基线：本节现取）" "True" "$(ts_gap_in "$T2" "$T3" 4 30)"

echo
echo "== 10. 挂载点容量与 statvfs 一致 =="
# total_bytes 是文件系统大小，不会自己变，必须精确相等。
expect_eq "/ 的 total_bytes 与 statvfs 完全一致" "True" \
  "$(jgetx "$WORK/topo-1.json" / '
got = [m["capacity"] for n in d["data"]["nodes"] if n["filesystem"]
       for m in n["filesystem"]["mounts"] if m["path"] == a[0]]
import os
st = os.statvfs(a[0])
print(len(got) == 1 and got[0] is not None and int(got[0]["total_bytes"]) == st.f_blocks * st.f_frsize)')"

# available_bytes 不能要求与 Bavail 精确相等：容量按 PLAN §8 每 30 秒采集一次，
# 命中的快照最多比现在旧 30 秒，这段窗口里验收脚本自己就在往 / 写日志和临时文件。
# 这里改成“与实时 Bavail 相差不超过 total 的 0.5%”，并另外证明这条断言有牙：
# ext4 默认给 root 留 5% 的块，Bfree 比 Bavail 大 5%，所以 0.5% 的容差足以
# 区分“取的是 Bavail”和“错取了 Bfree”——后者一定超差。
CAPLINE=$(jgetx "$WORK/topo-1.json" / '
got = [m["capacity"] for n in d["data"]["nodes"] if n["filesystem"]
       for m in n["filesystem"]["mounts"] if m["path"] == a[0]]
import os
st = os.statvfs(a[0])
c = got[0] if len(got) == 1 else None
if c is None:
    print("none")
else:
    print(int(c["total_bytes"]), int(c["available_bytes"]),
          st.f_blocks * st.f_frsize, st.f_bavail * st.f_frsize, st.f_bfree * st.f_frsize)')
set -- $CAPLINE
if [ "$1" = "none" ] || [ "$#" -ne 5 ]; then
  fail "/ 的 available_bytes 取的是 Bavail 而不是 Bfree" "容量缺失或格式异常: $CAPLINE"
else
  note "容量对账: total=$1 available=$2 statvfs.Bavail=$4 statvfs.Bfree=$5"
  expect_eq "/ 的 available_bytes 落在实时 Bavail 的 0.5% 内" "ok" \
    "$(awk -v a="$2" -v t="$1" -v bav="$4" 'BEGIN {
       tol = t * 0.005
       d = a - bav; if (d < 0) d = -d
       if (d <= tol) print "ok"; else printf "bad:差 %d 字节，容差 %d\n", d, tol }')"
  # 容差按采集窗口内的漂移设，可能比本机保留块差还大，那就区分不了 Bavail/Bfree。
  # 这里只如实记录能不能区分，不拿它当断言——严格对账在 §8 的空闲 loop 镜像上做。
  CAPGAP=$(( $5 - $4 ))
  CAPTOL=$(awk -v t="$1" 'BEGIN { printf "%d", t * 0.005 }')
  if [ "$CAPGAP" -gt "$CAPTOL" ]; then
    note "本机保留块差 $CAPGAP 字节 > 容差 $CAPTOL，这条断言能区分两种取法"
  else
    note "本机保留块差 $CAPGAP 字节 ≤ 容差 $CAPTOL，这条断言区分不了 Bavail/Bfree（见 §8）"
  fi
fi
expect_eq "used_percent 落在 0—100" "True" \
  "$(jget "$WORK/topo-1.json" 'all(0 <= m["capacity"]["used_percent"] <= 100 for n in d["data"]["nodes"] if n["filesystem"] for m in n["filesystem"]["mounts"] if m["capacity"] is not None)')"
expect_eq "capacity 自带 collected_at（30 秒缓存，可能比拓扑旧）" "True" \
  "$(jget "$WORK/topo-1.json" 'all(m["capacity"]["collected_at"] for n in d["data"]["nodes"] if n["filesystem"] for m in n["filesystem"]["mounts"] if m["capacity"] is not None)')"

echo
echo "== 11. 查询边界：没有万能入口 =="
call web POST /internal/v1/queries '{"query":"storage.topology","params":{"command":"lsblk"}}'
expect_eq "storage.topology 不接受参数 400" "400" "$CODE"
call web POST /internal/v1/queries '{"query":"storage.topology","params":{"path":"/dev/sda"}}'
expect_eq "不接受任何路径类参数 400" "400" "$CODE"
call web POST /internal/v1/queries '{"query":"storage.inspect","params":{"id":"sda"}}'
expect_eq "未登记的查询名 400" "400" "$CODE"
expect_contains "拒绝时说明未登记" "未登记" "$(cat "$RESP" 2>/dev/null)"
call web POST /internal/v1/queries '{"query":"storage.topology","id":"sda"}'
expect_eq "信封里夹带未知字段 400" "400" "$CODE"

echo
echo "== 12. Web 端透传（权限 V、数值来自代理） =="
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
INIT_RC=$?
expect_eq "init-admin 退出码" "0" "$INIT_RC"
chown "$WEB_USER" "$WEB_DB" 2>/dev/null
for suffix in "" "-wal" "-shm"; do
  [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
done
if start_web; then pass "Web 以 $WEB_USER 身份在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
WEB_CHILD_PID=$(pgrep -u "$WEB_USER" -f "upanel-web --mode production --db $WEB_DB" | head -n 1)
expect_eq "Web 进程以 $WEB_USER 运行" "$(id -u "$WEB_USER")" "$(ps -o uid= -p "${WEB_CHILD_PID:-0}" 2>/dev/null | tr -d ' ')"

ANON=$(curl -s -o "$WORK/anon.json" -w '%{http_code}' "$BASE/api/v1/storage/topology")
expect_eq "未登录读拓扑 401" "401" "$ANON"
expect_eq "未登录错误码" "UNAUTHENTICATED" "$(jget "$WORK/anon.json" 'd["error"]["code"]')"

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/topology")
expect_eq "GET /api/v1/storage/topology 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/web-topo.json"
# 和**代理此刻**的版本比：`/storage/topology` 是只读透传，Web 不该自己再扫一遍、
# 也不该把上一次的结果缓存下来（V 权限的读请求必须反映当前快照）。
call web POST /internal/v1/queries '{"query":"storage.topology"}'
cp "$RESP" "$WORK/topo-at-web.json"
expect_eq "Web 与代理给出同一个 snapshot_version" "$(jget "$WORK/topo-at-web.json" 'd["data"]["snapshot_version"]')" \
  "$(jget "$WORK/web-topo.json" 'd["data"]["snapshot_version"]')"
expect_eq "Web 没有自己重排边" "True" \
  "$(jgetx "$WORK/web-topo.json" "$WORK/topo-at-web.json" 'd["data"]["edges"] == json.load(open(a[0]))["data"]["edges"]')"
expect_eq "Web 没有自己改保护判定" "True" \
  "$(jgetx "$WORK/web-topo.json" "$WORK/topo-at-web.json" '
a0 = {n["id"]: n["protection"] for n in json.load(open(a[0]))["data"]["nodes"]}
print(all(a0.get(n["id"]) == n["protection"] for n in d["data"]["nodes"]))')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/devices")
expect_eq "GET /api/v1/storage/devices 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/web-devices.json"
expect_eq "设备列表与拓扑的节点数一致" "$(jget "$WORK/web-topo.json" 'len(d["data"]["nodes"])')" \
  "$(jget "$WORK/web-devices.json" 'len(d["data"]["devices"])')"
expect_eq "设备列表的 size_bytes 仍是十进制字符串" "True" \
  "$(jget "$WORK/web-devices.json" 'all(isinstance(n["size_bytes"], str) and n["size_bytes"].isdigit() for n in d["data"]["devices"])')"

DETAIL_ID=$(jget "$WORK/web-topo.json" 'sorted(n["id"] for n in d["data"]["nodes"])[0]')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/devices/$DETAIL_ID")
expect_eq "GET /api/v1/storage/devices/$DETAIL_ID 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/web-detail.json"
expect_eq "详情的设备 id 与请求一致" "$DETAIL_ID" "$(jget "$WORK/web-detail.json" 'd["data"]["device"]["id"]')"
expect_eq "详情只带与该设备相关的边" "True" \
  "$(jgetx "$WORK/web-detail.json" "$DETAIL_ID" 'all(e["from_device_id"] == a[0] or e["to_device_id"] == a[0] for e in d["data"]["edges"])')"

for bad in "nonexistent0" "sda%3Bid" "..%2f..%2fetc%2fshadow" "sda%20--force"; do
  code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/devices/$bad")
  expect_eq "设备 id 不是命令入口：devices/$bad 不返回 200" "True" \
    "$([ "$code" = "200" ] && echo False || echo True)"
done
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/devices/nonexistent0")
expect_eq "未知设备 404" "404" "$code"
expect_eq "未知设备的错误码" "NOT_FOUND" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
  "{\"username\":\"alice\",\"display_name\":\"alice\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 201" "201" "$code"
code=$(login alice "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")
code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" GET "/api/v1/storage/topology")
expect_eq "viewer 读拓扑 200（权限 V）" "200" "$code"
code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" GET "/api/v1/storage/devices/$DETAIL_ID")
expect_eq "viewer 读设备详情 200（权限 V）" "200" "$code"

echo
echo "== 13. 代理停机：503 而不是编造数据 =="
stop_agent
expect_eq "代理已停止（socket 不存在）" "False" "$([ -S "$SOCK" ] && echo True || echo False)"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/topology")
expect_eq "代理停机时 /storage/topology 503" "503" "$code"
expect_eq "代理停机时的错误码" "AGENT_UNAVAILABLE" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_eq "代理停机时不返回任何设备" "False" "$(jget "$WORK/resp.json" 'bool(d.get("data"))')"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/devices")
expect_eq "代理停机时 /storage/devices 503" "503" "$code"

if start_agent; then pass "代理恢复启动"; else fail "代理恢复启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/storage/topology")
expect_eq "代理恢复后 /storage/topology 200" "200" "$code"
expect_eq "恢复后仍然是同一份真实拓扑" "True" \
  "$(jget "$WORK/resp.json" 'd["data"]["snapshot_version"].startswith("sha256:") and len(d["data"]["nodes"]) > 0')"

echo
echo "== 14. 日志卫生 =="
if [ -f "$AGENT_LOG" ]; then
  expect_eq "代理日志里没有 shell 命令行痕迹" "0" "$(grep -c 'sh -c' "$AGENT_LOG" 2>/dev/null | head -n 1)"
  expect_eq "代理日志里没有私钥泄漏" "0" "$(grep -c 'BEGIN PRIVATE KEY' "$AGENT_LOG" 2>/dev/null | head -n 1)"
  expect_eq "代理日志里没有 panic" "0" "$(grep -c 'panic' "$AGENT_LOG" 2>/dev/null | head -n 1)"
else
  fail "代理日志存在" "$AGENT_LOG"
fi
if [ -f "$WEB_LOG" ]; then
  expect_eq "Web 日志里没有 panic" "0" "$(grep -c 'panic' "$WEB_LOG" 2>/dev/null | head -n 1)"
else
  fail "Web 日志存在" "$WEB_LOG"
fi

echo
printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$NOTRUN" -ne 0 ]; then
  printf '未执行项:%s\n' "$NOTRUN_NAMES"
fi
if [ "$FAILED" -ne 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
