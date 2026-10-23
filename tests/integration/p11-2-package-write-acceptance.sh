#!/bin/sh
# P11.2 Linux 验收脚本：APT 写事务（refresh / install / upgrade / remove）在真实
# Ubuntu 上的行为。
#
# ⚠️ 这个脚本会**真的装、真的升、真的卸**：它在机器上安装 cowsay 与 sl、升级一个
#    可升级的包、卸载刚装上的两个包、并且真的跑一次 `apt-get update`。**只在一台
#    一次性的验收虚拟机上跑**，绝不要在开发宿主或日常服务器上跑（PLAN 第 14 节
#    与 docs/STAGES.md 的硬规定）。
#
# 要证明的不是"接口返回 200"，而是下面这些容易被"写完了"骗过去的事实：
#
#   1) **模拟是准入标准**：预检跑一次 `apt-get -s`，保护判定、连带删除、被保留的包
#      全部来自这次模拟的输出，不是手写的依赖推演。删 openssh-server 时 apt 自己
#      是**愿意**删的（退出 0、有 Remv 行），面板必须拦下它——这条是本轮的核心。
#   2) **锁是真的探测的**：另起一个 flock 进程占住 /var/lib/dpkg/lock-frontend，
#      预检必须报 APT_BUSY 并且一步都不执行（dpkg 状态文件的 sha256 不变）。
#   3) **执行后逐包核对**：任务的 `observed_state.verified` 只有在"读回的 dpkg 状态
#      与预检计划里的目标版本逐条对上"时才是 true。apt 的退出码不是结论，脚本两边都查：
#      面板说的版本 vs `dpkg-query` 现场读到的版本。
#   4) **升级不做发行版升级**：升级一个包前后，把全部已安装包的 (名字, 版本) 抓两份
#      做差集，差集必须**只有**那一个包。
#   5) **卸载要短语**：没有短语或短语不对时，包必须还在（短语在代理侧校验，哈希比对）。
#   6) **刷新索引的真假**：正常刷新后 /var/lib/apt/lists 的 mtime 要与任务里的
#      index_after 对上；故意塞一路不可达的源之后，任务必须**失败**（apt-get update
#      对单源失败仍可能退出 0，所以判据是 Err:/E: 行，不是退出码）。
#   7) **拒收面**：`allow_remove_essential` / `dist_upgrade` / 空包名 / 版本语法 /
#      201 个包名 / 给 refresh 塞 packages，全部在代理侧被拒，且不留任务。
#   8) **权限与审计**：viewer 不能预检；anon 401；审计里有 package.* 的 preflight 与
#      job.submit 记录，被拒的也有记录，且不含口令。
#
# 用法（root，在验收虚拟机上）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p11-2-package-write-acceptance.sh
#
# 环境变量：BIN_DIR（默认 /var/tmp/upanel-bin）、WORK（默认 /var/tmp/upanel-accept-p11-2）、
# PORT（默认 19506）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制缺失 / 不是 Debian 系 /
# 没有 apt-get）。
#
# 明确不覆盖（NOT RUN，原因写在这里，不假装通过）：
#   - **APT_LOCK_UNREADABLE**：root 打开 dpkg 的锁文件不会被权限拦（CAP_DAC_OVERRIDE），
#     而挪走 dpkg 的锁文件去造 EACCES 会破坏验收机的包管理器。该分支由
#     internal/apt/locks_test.go 覆盖。
#   - **`apt-get -y` 是否"必要"**：实测 apt-get 在 stdin 不是终端时**不问**就直接装
#     （这一点与常见说法相反）。所以本轮能诚实证明的是"整条链路在无终端环境下跑完、
#     没有停在提问上"，而不是"没有 -y 就会挂"。
#   - **重启执行**（倒计时、再认证、boot_id 前后比对）：P11.3。
#   - **浏览器渲染**：VM 上没有浏览器；这一轮的载荷原样抓在 $WORK/fixtures 里。
#   - **Windows / macOS 宿主**：0.1 不支持。
#   - **apt-get update 真的失败（网络断掉）**：用一个不可达的源代替，网络层中断不在本轮。
#
# 升级与 conffile 用的是**脚本自建的 file: 源**（两个 1.0/2.0 的小包，见 §0）：
# 发行版里"恰好有个可升级的包"是被上一轮验收消费掉的运气，不是夹具；而 24.04 上
# 一次真升级要跑十几分钟。自建的这两个包里各带一个 conffile，装 1.0 之后本地改一行，
# 于是升级时 dpkg 真的面对"本地改过、包也改了"的冲突——`--force-confold` 是否生效，
# 由此从"argv 里的一串字"变成"文件里还剩什么"。收尾时包、源、仓库目录一起删掉。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
WORK=${WORK:-/var/tmp/upanel-accept-p11-2}
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
FIXDIR=$WORK/fixtures
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db

PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS_PATH=/api/v1/jobs

# 被安装 / 卸载的那两个包：无依赖、无 conffile、无服务，装完立即卸干净。
INSTALL_A=cowsay
INSTALL_B=sl
# 承载"连带删除"证据的包：删它 apt 会带走二十多个包（本轮**只预检、不提交**）。
CASCADE_PKG=ca-certificates
# 故意写坏的一路源（刷新索引的负例）。脚本退出时一定删掉。
BROKEN_SOURCE=/etc/apt/sources.list.d/zz-upanel-p11-2-broken.list

AGENT_PID=""
WEB_PID=""
LOCK_PID=""
PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; }
note() { printf '      %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ne() { if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不该等于 '$2'"; fi; }

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

# 预检/任务的载荷读取（文件不存在时给空串，让断言说出"缺文件"而不是崩掉）。
pf() { jget "$PAYLOADS/preflight-$1.json" "$2"; }
jbf() { jget "$PAYLOADS/job-$1.json" "$2"; }

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

dpkg_state() { dpkg-query -W -f='${db:Status-Abbrev}' "$1" 2>/dev/null | tr -d ' \n'; }
dpkg_version() { dpkg-query -W -f='${Version}' "$1" 2>/dev/null | tr -d ' \n'; }
dpkg_present() { [ "$(dpkg_state "$1")" = "ii" ] && printf yes || printf no; }
status_sha() { sha256sum /var/lib/dpkg/status | awk '{print $1}'; }
lists_mtime() { stat -c %Y /var/lib/apt/lists; }

# 时间戳 → epoch（Go 的 time.Time 带纳秒，Python 的 fromisoformat 认不了）。
ISO_PRELUDE='import datetime,re,sys
s=sys.argv[1]
m=re.match(r"^(.*\.)(\d+)([^0-9].*)$", s)
if m: s=m.group(1)+(m.group(2)+"000000")[:6]+m.group(3)
ts=datetime.datetime.fromisoformat(s.replace("Z","+00:00"))'
iso_epoch() { python3 -c "$ISO_PRELUDE
print(int(ts.timestamp()))" "$1" 2>/dev/null; }
iso_age() { python3 -c "$ISO_PRELUDE
print(int((datetime.datetime.now(datetime.timezone.utc)-ts).total_seconds()))" "$1" 2>/dev/null; }

cleanup() {
  # 锁要放干净（第 8 节会解释为什么 kill 掉 flock 不等于放锁）：否则脚本异常退出后
  # 这台机器还会被自己的锁挡住几十秒，下一次运行的"基线预检"就会莫名其妙地 APT_BUSY。
  release_dpkg_lock 2>/dev/null || true
  rm -f "$BROKEN_SOURCE"
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  rm -f "$SOCK"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

chown_web_db() {
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

start_agent() {
  # 上一次被 SIGKILL 的代理会留下 socket 文件，而 [ -S ] 对尸体也返回真。
  # 24.04 上就吃过这一口：start_agent 立刻"成功"，紧接着读日志时新代理还没把
  # 「已启用 APT 写事务」写下来，于是被判定成没启用——断言没错，就绪判据错了。
  # 所以先清掉可能存在的尸体，再要求**能应答**：capabilities 返回 200 才说明
  # 装配完成、路由挂好、日志也已经落盘（服务端 Serve 在所有组件日志之后）。
  rm -f "$SOCK"
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 100); do
    if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null \
         --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  # 没应答也要说清卡在哪：socket 在不在、代理日志最后一句是什么。
  note "代理就绪探针失败：socket=$([ -S "$SOCK" ] && echo 在 || echo 不在)"
  note "代理日志最后一行：$(tail -1 "$AGENT_LOG" 2>/dev/null | cut -c1-200)"
  return 1
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

# submit <名> <preflight_id> <短语或空> <幂等键>：响应存成 job-submit-<名>.json。
submit() {
  if [ -n "$3" ]; then
    _body=$(printf '{"preflight_id":"%s","confirmation_phrase":"%s"}' "$2" "$3")
  else
    _body=$(printf '{"preflight_id":"%s"}' "$2")
  fi
  _code=$(api "$JAR" "$CSRF" POST "$JOBS_PATH" "$_body" "$4")
  cp "$WORK/resp.json" "$PAYLOADS/job-submit-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# wait_job <名> <job_id> <超时秒>：轮询到终态，响应存成 job-<名>.json，回显状态。
#
# 超时按**墙钟**算，不是按轮询次数：24.04 上装 cowsay+sl 实测 6 分半（下载慢），
# 而每轮问一次 Web 本身就要几百毫秒，按次数算的"300 秒"在这种机器上会变成 600 秒以上——
# 一个写着 300 秒却等 400 秒的等待，等到的东西再对也不算数。
wait_job() {
  _deadline=$(( $(date +%s) + ${3:-120} ))
  _st=unknown
  while [ "$(date +%s)" -lt "$_deadline" ]; do
    api "$JAR" "$CSRF" GET "$JOBS_PATH/$2" >/dev/null
    _st=$(jget "$WORK/resp.json" 'd["data"]["status"]')
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
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi
for f in "$AGENT" "$WEB" "$CTL"; do
  if [ ! -x "$f" ]; then
    printf '缺少可执行文件：%s\n' "$f" >&2
    exit 2
  fi
done
for tool in dpkg-query apt-get apt-cache flock python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf '本机缺少 %s，P11.2 的验收无法进行\n' "$tool" >&2
    exit 2
  fi
done
if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  printf '本机没有 %s 账号（先装一遍 0.1 包）\n' "$WEB_USER" >&2
  exit 2
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$FIXDIR" "$WEB_DB_DIR"
chown "$WEB_USER" "$WEB_DB_DIR"

echo "== 0. 这台机器的发行版与包管理现场 =="
DISTRO=$(sed -n 's/^PRETTY_NAME="\(.*\)"$/\1/p' /etc/os-release)
note "发行版：$DISTRO"
note "apt-get：$(apt-get --version | head -1)"
note "dpkg-query：$(command -v dpkg-query)；锁文件：$(printf '%s ' /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock /var/cache/apt/archives/lock)"
pass "env-has-apt-get"

# 这两个包必须一开始**没装**：装/卸的两个方向都要有"从无到有、从有到无"的变化。
for pkg in "$INSTALL_A" "$INSTALL_B"; do
  if [ "$(dpkg_present "$pkg")" = "no" ]; then
    pass "env-$pkg-not-installed"
  else
    note "这台机器已经装着 $pkg（$(dpkg_version "$pkg")），脚本会先把它卸掉再开始"
    apt-get -q -y purge "$pkg" >"$WORK/logs/purge-preexisting.log" 2>&1 || true
    if [ "$(dpkg_present "$pkg")" = "no" ]; then
      pass "env-$pkg-not-installed"
    else
      fail "env-$pkg-not-installed" "$pkg 卸不掉，装/卸的对照无法成立"
    fi
  fi
done

# 升级路径的夹具：本机 file: 仓库里的两个 1.0/2.0 小包。
#
# 为什么不用"发行版里恰好有个可升级的包"：那不是夹具，那是运气。第一次跑完，
# 那个包就被升到最新了；下一轮同一个脚本就会因为"没有可升级的包"而失败——
# 而失败的原因跟被测代码毫无关系。24.04 上一次真升级还要跑十几分钟（zlib1g 超过
# 6 分钟仍未终态），把"点名的包被升、别的包一个都不动"这件事埋在别的噪声里。
#
# 所以这里自己造两个 8 KB 的包（a 与 b），各两个版本：装 1.0，让候选停在 2.0，
# 只升 a。差集里只该有 a —— 这比"看着不像偷偷做了 dist-upgrade"要硬：b 就是
# 放在旁边没被点名的那一个。两个包都没有 maintainer 脚本、没有 conffile、
# 没有服务，装上与卸掉都不留东西。
#
# 索引是脚本自己按 .deb 的实际字节算出来的（大小、md5、sha256），没有拿
# dpkg-scanpackages：24.04 的验收机上没有 dpkg-dev，为这个夹具再装一套工具链
# 不值得，而 Packages 的字段就这几个。
UPGRADE_TARGET=upanel-fixture-a
UPGRADE_BYSTANDER=upanel-fixture-b
FIXREPO=$WORK/fixture-repo
FIXLIST=/etc/apt/sources.list.d/upanel-fixture.list
fixture_deb() { # name version
  _n=$1
  _v=$2
  _d=$WORK/fixture-build/${_n}_${_v}
  rm -rf "$_d"
  install -d "$_d/DEBIAN" "$_d/usr/share/doc/$_n"
  printf 'Package: %s\nVersion: %s\nArchitecture: all\nMaintainer: uPanel acceptance <upanel@localhost>\nSection: misc\nPriority: optional\nInstalled-Size: 8\nDescription: uPanel P11.2 acceptance fixture package\n A do-nothing package used to exercise apt-get install --only-upgrade.\n' \
    "$_n" "$_v" >"$_d/DEBIAN/control"
  printf 'fixture %s %s\n' "$_n" "$_v" >"$_d/usr/share/doc/$_n/version.txt"
  # 每个包里放一个 conffile，内容随版本变：装 1.0 之后本地改一行，再升到 2.0，
  # dpkg 就真的会面对"本地改过、包也改了"的冲突——这是 --force-confold 唯一
  # 能被证明的场合，否则那几个 -o 选项只是 argv 里的一串字。
  #
  # 文件名是 DEBIAN/conffiles（复数）：写成单数 dpkg 一声不响地忽略它，文件就变成
  # 普通文件，升级时直接覆盖——第一次做这个夹具时正是这么错的，而且断言把它抓出来了。
  install -d "$_d/etc"
  printf '# uPanel 验收夹具的 conffile\nversion=%s\nshipped=yes\n' "$_v" >"$_d/etc/${_n}.conf"
  printf '/etc/%s.conf\n' "$_n" >"$_d/DEBIAN/conffiles"
  dpkg-deb --root-owner-group -Zgzip -b "$_d" "$FIXREPO/${_n}_${_v}_all.deb" >/dev/null
}
fixture_index() { # deb
  _deb=$1
  printf 'Package: %s\nVersion: %s\nArchitecture: all\nMaintainer: uPanel acceptance <upanel@localhost>\nInstalled-Size: 8\nFilename: ./%s\nSize: %s\nMD5sum: %s\nSHA256: %s\nSection: misc\nPriority: optional\nDescription: uPanel P11.2 acceptance fixture package\n A do-nothing package used to exercise apt-get install --only-upgrade.\n\n' \
    "$(dpkg-deb -f "$_deb" Package)" "$(dpkg-deb -f "$_deb" Version)" "$(basename "$_deb")" \
    "$(stat -c %s "$_deb")" "$(md5sum "$_deb" | cut -d' ' -f1)" "$(sha256sum "$_deb" | cut -d' ' -f1)"
}
rm -rf "$FIXREPO"
install -d "$FIXREPO"
for fx in "$UPGRADE_TARGET" "$UPGRADE_BYSTANDER"; do
  fixture_deb "$fx" 1.0
  fixture_deb "$fx" 2.0
done
: >"$FIXREPO/Packages"
for fx in "$FIXREPO"/*.deb; do fixture_index "$fx" >>"$FIXREPO/Packages"; done
printf 'deb [trusted=yes] file:%s ./\n' "$FIXREPO" >"$FIXLIST"
# 只刷这一路源：整份 apt-get update 在 24.04 上要几十秒，而这里只需要夹具的索引。
if DEBIAN_FRONTEND=noninteractive apt-get -q update \
     -o Dir::Etc::sourcelist="$FIXLIST" -o Dir::Etc::sourceparts=/dev/null \
     -o APT::Get::List-Cleanup=0 >"$WORK/logs/fixture-update.log" 2>&1; then
  pass "env-fixture-repo-indexed"
else
  fail "env-fixture-repo-indexed" "$(tail -3 "$WORK/logs/fixture-update.log" | tr '\n' ' ')"
fi
if DEBIAN_FRONTEND=noninteractive apt-get -y -q install \
     "$UPGRADE_TARGET=1.0" "$UPGRADE_BYSTANDER=1.0" >"$WORK/logs/fixture-install.log" 2>&1; then
  pass "env-fixture-installed-at-1.0"
else
  fail "env-fixture-installed-at-1.0" "$(tail -3 "$WORK/logs/fixture-install.log" | tr '\n' ' ')"
fi
expect_eq "env-fixture-a-at-1.0" "1.0" "$(dpkg_version "$UPGRADE_TARGET")"
expect_eq "env-fixture-b-at-1.0" "1.0" "$(dpkg_version "$UPGRADE_BYSTANDER")"
# conffile 冲突的前置：本地改一行。装上的 1.0 与候选的 2.0 里这一行不一样，
# 于是升级时 dpkg 真的要在"本地改过的"和"包里新的"之间做选择。
printf 'local_edit=yes\n' >>"/etc/$UPGRADE_TARGET.conf"
expect_eq "env-conffile-locally-edited" "yes" \
  "$(grep -qx 'local_edit=yes' "/etc/$UPGRADE_TARGET.conf" && echo yes || echo no)"
if apt-get -s install --only-upgrade "$UPGRADE_TARGET" 2>/dev/null | grep -q '^Inst '; then
  pass "env-fixture-has-newer-candidate"
  note "升级目标：$UPGRADE_TARGET（1.0 → 候选 $(apt-cache policy "$UPGRADE_TARGET" | sed -n 's/^  Candidate: //p')），旁边不动的是 $UPGRADE_BYSTANDER"
else
  fail "env-fixture-has-newer-candidate" "本机夹具仓库里的 2.0 没有成为候选，升级路径无法验收"
fi

# 连带删除的对照：apt 自己说要删几个。
if apt-cache policy "$CASCADE_PKG" 2>/dev/null | grep -q 'Installed: (none)'; then
  CASCADE_SIM=0
  notrun "env-cascade-package-installed" "$CASCADE_PKG 没装，连带删除的证据改用单测里的真机捕获"
else
  CASCADE_SIM=$(apt-get -s remove "$CASCADE_PKG" 2>&1 | grep -c '^Remv ')
  note "$CASCADE_PKG 的模拟删除行数：$CASCADE_SIM（其中 1 个是被点名的那个，其余是连带删除）"
fi

echo
echo "== 1. 启动代理（root）与 Web（非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
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
  pass "agent-key-present"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
    pass "agent-key-created"
  else
    fail "agent-key-created" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
  fi
fi
if start_agent; then pass "agent-started"; else fail "agent-started" "代理没起来（见 $AGENT_LOG）"; fi
expect_eq "agent-runs-as-root" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"
if grep -q "已启用 APT 写事务" "$AGENT_LOG"; then
  pass "agent-logs-apt-write-enabled"
else
  fail "agent-logs-apt-write-enabled" "代理日志里没有启用 APT 写事务的记录（见 $AGENT_LOG）"
fi

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin" "0" "$?"
chown_web_db
if start_web; then pass "web-started"; else fail "web-started" "Web 没起来（见 $WEB_LOG）"; fi
LISTEN_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
[ -z "$LISTEN_PID" ] && LISTEN_PID=$WEB_PID
WEB_UID=$(ps -o uid= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
  pass "web-not-root"
else
  fail "web-not-root" "pid=${LISTEN_PID:-未知} uid=${WEB_UID:-未知}"
fi

echo
echo "== 2. 能力表报告四个写动作 =="
CAP=$PAYLOADS/capabilities.json
CAP_TMP=/tmp/p11-2-capabilities.$$.json
CODE=$(runuser -u "$WEB_USER" -- curl -s -o "$CAP_TMP" -w '%{http_code}' \
  --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null)
cp "$CAP_TMP" "$CAP" 2>/dev/null
cp "$CAP" "$FIXDIR/capabilities.json" 2>/dev/null
rm -f "$CAP_TMP"
expect_eq "capabilities-200" "200" "$CODE"
note "supported_actions=$(jget "$CAP" 'json.dumps([a for a in d["data"]["supported_actions"] if a.startswith("package.")], ensure_ascii=False)')"
for action in package.refresh package.install package.upgrade package.remove; do
  expect_eq "capabilities-has-$action" "yes" \
    "$(jget "$CAP" "\"$action\" in d[\"data\"][\"supported_actions\"] and \"yes\" or \"no\"")"
done

echo
echo "== 3. 登录与权限 =="
JAR=$WORK/admin.jar
CODE=$(login admin "$PASSWORD" "$JAR" login-admin)
if [ "$CODE" != "200" ]; then
  fail "login-admin" "状态码 $CODE：$(head -c 200 "$WORK/login-admin.json")"
  printf '\n== 汇总 ==\nPASS %d  FAIL %d  NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
  exit 1
fi
pass "login-admin"
CSRF=$(csrf_of "$WORK/login-admin.json")

CODE=$(api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"pkg-watcher\",\"display_name\":\"pkg-watcher\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
case "$CODE" in
  200|201) note "已建 viewer 账号 pkg-watcher" ;;
  *) fail "create-viewer" "状态码 $CODE：$(head -c 200 "$WORK/resp.json")" ;;
esac
VIEWER_JAR=$WORK/viewer.jar
CODE=$(login pkg-watcher "$VIEWER_PASSWORD" "$VIEWER_JAR" login-viewer)
if [ "$CODE" = "200" ]; then
  pass "login-viewer"
  VCSRF=$(csrf_of "$WORK/login-viewer.json")
  CODE=$(api "$VIEWER_JAR" "$VCSRF" POST "$PREFLIGHT_PATH" \
    "{\"action\":\"package.install\",\"params\":{\"packages\":[\"$INSTALL_A\"]}}")
  expect_eq "viewer-forbidden-preflight" "403" "$CODE"
else
  fail "login-viewer" "状态码 $CODE"
fi
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/packages" -H "Origin: $ORIGIN")
expect_eq "anon-401" "401" "$CODE"

echo
echo "== 4. 参数边界：这七种请求必须被拒，且不留任务 =="
# 先证明这四个动作**登记上了**：否则下面那一排 400 也可能是"未登记的动作"给的，
# 八条断言会一起为错误的理由变绿。第一次真机跑就是这么被坑的
# （RegisterProductionActions 少接了一行，代理日志说"已启用 APT 写事务"，
# 而每个预检都回 400「未登记的动作」）。
CODE=$(preflight "baseline-valid" package.install '{"packages":["upanel-no-such-package-p112"]}')
expect_eq "baseline-valid-preflight-200" "200" "$CODE"
if [ "$CODE" != "200" ]; then
  note "预检本身就不通（$(head -c 160 "$PAYLOADS/preflight-baseline-valid.json")）；下面的拒绝断言没有意义，直接停"
  printf '\n== 汇总 ==\nPASS %d  FAIL %d  NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
  exit 1
fi
SHA_BEFORE_REJECTS=$(status_sha)
JOBS_BEFORE_REJECTS=$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')
reject_preflight() {
  _name=$1; _action=$2; _params=$3
  _code=$(preflight "$_name" "$_action" "$_params")
  expect_eq "reject-$_name" "400" "$_code"
}
reject_preflight "install-allow-remove-essential" package.install \
  "{\"packages\":[\"$INSTALL_A\"],\"allow_remove_essential\":true}"
reject_preflight "install-dist-upgrade" package.install \
  "{\"packages\":[\"$INSTALL_A\"],\"dist_upgrade\":true}"
reject_preflight "install-empty-packages" package.install '{"packages":[]}'
reject_preflight "install-version-syntax" package.install '{"packages":["bash=5.2"]}'
reject_preflight "install-option-like" package.install '{"packages":["--force-yes"]}'
reject_preflight "refresh-with-packages" package.refresh "{\"packages\":[\"$INSTALL_A\"]}"
reject_preflight "remove-uppercase-name" package.remove '{"packages":["Bash"]}'
MANY=$(python3 -c 'import json;print(json.dumps({"packages":["pkg%03d" % i for i in range(201)]}))')
reject_preflight "install-too-many" package.install "$MANY"
expect_eq "reject-leaves-dpkg-untouched" "$SHA_BEFORE_REJECTS" "$(status_sha)"
expect_eq "reject-creates-no-jobs" "$JOBS_BEFORE_REJECTS" "$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')"

echo
echo "== 5. 保护名单：apt 愿意删的，面板必须拦（openssh-server） =="
# 先记下 apt 自己的态度：它**愿意**删 openssh-server，退出 0，一条 Remv 行。
apt-get -s remove openssh-server >"$PAYLOADS/host-sim-remove-openssh.txt" 2>&1
note "apt-get -s remove openssh-server 的退出码：$?"
note "模拟里的删除行：$(grep -c '^Remv ' "$PAYLOADS/host-sim-remove-openssh.txt")"

CODE=$(preflight "remove-openssh" package.remove '{"packages":["openssh-server"]}')
expect_eq "protected-openssh-preflight-200" "200" "$CODE"
expect_eq "protected-openssh-blocked" "False" "$(pf remove-openssh 'd["data"]["allowed"]')"
expect_eq "protected-openssh-code" "True" \
  "$(pf remove-openssh 'any(b["code"]=="PACKAGE_PROTECTED" for b in d["data"]["blockers"])')"
note "阻塞项：$(pf remove-openssh 'json.dumps([b["code"] for b in d["data"]["blockers"]])')"
note "消息：$(pf remove-openssh '(d["data"]["blockers"][0]["message"] if d["data"]["blockers"] else "")')"
expect_eq "protected-openssh-says-ssh" "True" \
  "$(pf remove-openssh 'any("ssh" in b["message"] for b in d["data"]["blockers"])')"
expect_eq "protected-openssh-cancellable-false" "False" "$(pf remove-openssh 'd["data"]["cancellable"]')"
PF_ID=$(pf remove-openssh 'd["data"]["preflight_id"]')
CODE=$(submit "remove-openssh" "$PF_ID" "REMOVE WRONG1" "p11-2-blocked-$$")
expect_eq "protected-openssh-submit-blocked" "409" "$CODE"
expect_eq "protected-openssh-error-code" "DEPENDENCY_BLOCKED" \
  "$(jget "$PAYLOADS/job-submit-remove-openssh.json" 'd["error"]["code"]')"
expect_eq "protected-openssh-still-installed" "yes" "$(dpkg_present openssh-server)"

CODE=$(preflight "remove-bash" package.remove '{"packages":["bash"]}')
expect_eq "protected-bash-preflight-200" "200" "$CODE"
expect_eq "protected-bash-blocked" "False" "$(pf remove-bash 'd["data"]["allowed"]')"
expect_eq "protected-bash-code" "True" \
  "$(pf remove-bash 'any(b["code"]=="PACKAGE_PROTECTED" for b in d["data"]["blockers"])')"
expect_eq "protected-bash-says-essential" "True" \
  "$(pf remove-bash 'any("essential" in b["message"] for b in d["data"]["blockers"])')"
expect_eq "protected-bash-still-installed" "yes" "$(dpkg_present bash)"

echo
echo "== 6. 连带删除：预检必须点名，且不提交（只读证据） =="
if [ "${CASCADE_SIM:-0}" -gt 1 ]; then
  CODE=$(preflight "remove-cascade" package.remove "{\"packages\":[\"$CASCADE_PKG\"]}")
  expect_eq "cascade-preflight-200" "200" "$CODE"
  expect_eq "cascade-warns-collateral" "True" \
    "$(pf remove-cascade 'any(w["code"]=="collateral_removal" for w in d["data"]["warnings"])')"
  CASCADE_NAMED=$(pf remove-cascade \
    '[w["message"] for w in d["data"]["warnings"] if w["code"]=="collateral_removal"][0]' 2>/dev/null)
  CASCADE_N=$(python3 -c 'import re,sys
m=re.search(r"删除 (\d+) 个", sys.argv[1] or "")
print(m.group(1) if m else "")' "$CASCADE_NAMED")
  expect_eq "cascade-count-matches-apt-simulation" "$((CASCADE_SIM - 1))" "$CASCADE_N"
  note "预检：$CASCADE_NAMED"
  expect_eq "cascade-impacts-removal" "True" \
    "$(pf remove-cascade 'any(i["kind"]=="software_removal" for i in d["data"]["impacts"])')"
  # 只读：预检不建任务、不动 dpkg。
  expect_eq "cascade-preflight-created-no-job" "0" \
    "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs WHERE action='package.remove'")"
else
  notrun "cascade-warns-collateral" "$CASCADE_PKG 不在本机，连带删除的证据见单测的真机捕获"
fi

echo
echo "== 7. 不成立的操作：找不到的包、没装的包 =="
CODE=$(preflight "install-unmet" package.install '{"packages":["upanel-no-such-package-p112"]}')
expect_eq "unmet-preflight-200" "200" "$CODE"
expect_eq "unmet-blocked" "False" "$(pf install-unmet 'd["data"]["allowed"]')"
expect_eq "unmet-code" "True" \
  "$(pf install-unmet 'any(b["code"]=="PACKAGE_UNMET" for b in d["data"]["blockers"])')"
note "unmet 消息：$(pf install-unmet '(d["data"]["blockers"][0]["message"] if d["data"]["blockers"] else "")' | cut -c1-160)"

CODE=$(preflight "upgrade-not-installed" package.upgrade "{\"packages\":[\"$INSTALL_A\"]}")
expect_eq "upgrade-not-installed-preflight-200" "200" "$CODE"
expect_eq "upgrade-not-installed-blocked" "False" "$(pf upgrade-not-installed 'd["data"]["allowed"]')"
expect_eq "upgrade-not-installed-code" "PACKAGE_NOT_INSTALLED" \
  "$(pf upgrade-not-installed 'd["data"]["blockers"][0]["code"]')"
expect_eq "upgrade-not-installed-explains" "True" \
  "$(pf upgrade-not-installed 'any(w["code"]=="no_change" for w in d["data"]["warnings"])')"

echo
echo "== 8. 资源锁：别人占着 dpkg 前端的锁时，预检必须拦下且一步不做 =="
# 释放这把握住的锁要放**彻底**，否则第 8 节的结论会污染第 9 节。
#
# 这不是理论：上面那版脚本 `kill -TERM $LOCK_PID` 只杀了 flock 进程，而锁挂在**打开的
# 文件描述**上——`flock -c 'sleep N'` 起的子进程继承了同一个 fd，父进程一死，锁还在
# 子进程手里直到它自己退出。22.04 那一轮就是这么失败的：第 9 节的预检诚实地报了
# APT_BUSY（产品没错，是脚本自己没放锁），后面整段跟着失败。
#
# 所以：先杀子进程、再杀 flock、然后**轮询确认这把锁真的能拿到**才往下走。
release_dpkg_lock() {
  [ -n "${LOCK_PID:-}" ] || return 0
  pkill -TERM -P "$LOCK_PID" 2>/dev/null
  kill -TERM "$LOCK_PID" 2>/dev/null
  _i=0
  while [ "$_i" -lt 100 ]; do
    if ! kill -0 "$LOCK_PID" 2>/dev/null \
      && flock -n -x /var/lib/dpkg/lock-frontend -c true 2>/dev/null; then
      LOCK_PID=""
      return 0
    fi
    sleep 0.1
    _i=$((_i + 1))
  done
  return 1
}

flock -x /var/lib/dpkg/lock-frontend -c 'sleep 90' &
LOCK_PID=$!
sleep 1
SHA_BEFORE_LOCK=$(status_sha)
CODE=$(preflight "install-busy" package.install "{\"packages\":[\"$INSTALL_A\"]}")
expect_eq "lock-busy-preflight-200" "200" "$CODE"
expect_eq "lock-busy-blocked" "False" "$(pf install-busy 'd["data"]["allowed"]')"
expect_eq "lock-busy-code" "True" \
  "$(pf install-busy 'any(b["code"]=="APT_BUSY" for b in d["data"]["blockers"])')"
note "阻塞项：$(pf install-busy 'json.dumps([b["code"] for b in d["data"]["blockers"]])')"
note "locks：$(pf install-busy 'json.dumps(d["data"]["locks"])')"
expect_eq "lock-busy-names-the-path" "True" \
  "$(pf install-busy 'any("lock-frontend" in b["message"] for b in d["data"]["blockers"])')"
expect_eq "lock-busy-nothing-executed" "$SHA_BEFORE_LOCK" "$(status_sha)"
expect_eq "lock-busy-package-still-absent" "no" "$(dpkg_present "$INSTALL_A")"

if release_dpkg_lock; then
  pass "lock-released-before-install"
else
  # 没放掉就停：后面每一节都会被这一次残留的锁诚实地拦下来，那些 FAIL 全是假的。
  fail "lock-released-before-install" "第 8 节占的锁没放掉；后面各节会被 APT_BUSY 拦下，结果不可信"
  printf '\n脚本自身没放掉 dpkg 锁，停止执行（不是产品失败）\n' >&2
  cleanup
  exit 1
fi

echo
echo "== 9. 真装：$INSTALL_A + $INSTALL_B（两个包，一次预检） =="
CAND_A=$(apt-cache policy "$INSTALL_A" | sed -n 's/^  Candidate: //p')
CAND_B=$(apt-cache policy "$INSTALL_B" | sed -n 's/^  Candidate: //p')
note "候选版本：$INSTALL_A=$CAND_A  $INSTALL_B=$CAND_B"
CODE=$(preflight "install-two" package.install "{\"packages\":[\"$INSTALL_A\",\"$INSTALL_B\"]}")
expect_eq "install-preflight-200" "200" "$CODE"
expect_eq "install-allowed" "True" "$(pf install-two 'd["data"]["allowed"]')"
expect_eq "install-requires-reauth" "True" "$(pf install-two 'd["data"]["requires_reauth"]')"
expect_eq "install-no-phrase" "None" "$(pf install-two 'd["data"]["confirmation_phrase"]')"
expect_eq "install-not-cancellable" "False" "$(pf install-two 'd["data"]["cancellable"]')"
expect_eq "install-locks-apt-global" "True" \
  "$(pf install-two '"apt-global" in d["data"]["locks"]')"
expect_eq "install-impact-software-change" "True" \
  "$(pf install-two 'any(i["kind"]=="software_change" for i in d["data"]["impacts"])')"
PF_INSTALL=$(pf install-two 'd["data"]["preflight_id"]')

CODE=$(submit "install-two-no-reauth" "$PF_INSTALL" "" "p11-2-install-noreauth-$$")
expect_eq "install-needs-reauth-first" "403" "$CODE"
expect_eq "install-needs-reauth-code" "REAUTH_REQUIRED" \
  "$(jget "$PAYLOADS/job-submit-install-two-no-reauth.json" 'd["error"]["code"]')"
expect_eq "install-aborted-before-reauth" "no" "$(dpkg_present "$INSTALL_A")"

expect_eq "install-reauth" "200" "$(reauth)"
CODE=$(submit "install-two" "$PF_INSTALL" "" "p11-2-install-$$")
expect_eq "install-submit-accepted" "202" "$CODE"
JOB_INSTALL=$(jget "$PAYLOADS/job-submit-install-two.json" 'd["data"]["job_id"]')
note "任务 $JOB_INSTALL 已提交，等待终态（要下载两个小包）"
# 等待预算按"不比产品先放弃"定：代理自己给 apt 的写超时是 20 分钟
# （internal/apt: writeTimeout），这里取 21 分钟，于是到点还没终态就只能是产品卡住了，
# 而不是脚本没耐心。真机依据：24.04 上 cowsay+sl 的安装实测 6 分半（下载慢），
# 22.04 上同一个动作是秒级。
ST=$(wait_job "install-two" "$JOB_INSTALL" 1260)
expect_eq "install-job-succeeded" "succeeded" "$ST"
expect_eq "install-outcome-verified" "verified" "$(jbf install-two 'd["data"]["result"]["outcome"]')"
expect_eq "install-observed-verified" "True" \
  "$(jbf install-two 'd["data"]["result"]["observed_state"]["verified"]')"
expect_eq "install-no-mismatch" "0" "$(jbf install-two 'd["data"]["result"]["observed_state"]["mismatch_count"]')"
expect_eq "install-exit-code" "0" "$(jbf install-two 'd["data"]["result"]["data"]["exit_code"]')"
expect_eq "install-observed-pkg-a" "ii $CAND_A" \
  "$(jbf install-two "d[\"data\"][\"result\"][\"observed_state\"][\"pkg:$INSTALL_A\"]")"
expect_eq "install-observed-pkg-b" "ii $CAND_B" \
  "$(jbf install-two "d[\"data\"][\"result\"][\"observed_state\"][\"pkg:$INSTALL_B\"]")"
# 面板说的版本 == dpkg 现场读到的版本（两边都要查，缺一条就成了自说自话）。
expect_eq "install-dpkg-says-a-installed" "ii" "$(dpkg_state "$INSTALL_A")"
expect_eq "install-dpkg-version-a" "$CAND_A" "$(dpkg_version "$INSTALL_A")"
expect_eq "install-dpkg-version-b" "$CAND_B" "$(dpkg_version "$INSTALL_B")"
expect_eq "install-mismatches-empty" "0" \
  "$(jbf install-two 'len(d["data"]["result"]["data"]["mismatches"] or [])')"

echo
echo "== 10. 不取消：不可取消的动作，取消请求必须被拒 =="
# 任务可能已经跑完（terminal），也可能是执行中；两种情况都该是 409。
CODE=$(api "$JAR" "$CSRF" POST "$JOBS_PATH/$JOB_INSTALL/cancel" '{}')
expect_eq "cancel-not-allowed" "409" "$CODE"
note "取消请求的响应：$(head -c 160 "$WORK/resp.json")"

echo
echo "== 11. 真升：只升点名的包，不做发行版升级 =="
if [ -n "$UPGRADE_TARGET" ]; then
  CAND_UP=$(apt-cache policy "$UPGRADE_TARGET" | sed -n 's/^  Candidate: //p')
  BEFORE_UP=$(dpkg_version "$UPGRADE_TARGET")
  dpkg-query -W -f='${binary:Package}\t${Version}\n' | sort >"$WORK/versions-before.txt"
  CODE=$(preflight "upgrade" package.upgrade "{\"packages\":[\"$UPGRADE_TARGET\"]}")
  expect_eq "upgrade-preflight-200" "200" "$CODE"
  expect_eq "upgrade-allowed" "True" "$(pf upgrade 'd["data"]["allowed"]')"
  expect_eq "upgrade-no-phrase" "None" "$(pf upgrade 'd["data"]["confirmation_phrase"]')"
  PF_UP=$(pf upgrade 'd["data"]["preflight_id"]')
  expect_eq "upgrade-reauth" "200" "$(reauth)"
  CODE=$(submit "upgrade" "$PF_UP" "" "p11-2-upgrade-$$")
  expect_eq "upgrade-submit-accepted" "202" "$CODE"
  JOB_UP=$(jget "$PAYLOADS/job-submit-upgrade.json" 'd["data"]["job_id"]')
  ST=$(wait_job "upgrade" "$JOB_UP" 1260)
  expect_eq "upgrade-job-succeeded" "succeeded" "$ST"
  expect_eq "upgrade-observed-verified" "True" \
    "$(jbf upgrade 'd["data"]["result"]["observed_state"]["verified"]')"
  expect_eq "upgrade-target-version" "ii $CAND_UP" \
    "$(jbf upgrade "d[\"data\"][\"result\"][\"observed_state\"][\"pkg:$UPGRADE_TARGET\"]")"
  expect_eq "upgrade-dpkg-version" "$CAND_UP" "$(dpkg_version "$UPGRADE_TARGET")"
  # 旁边那个同样有 2.0 候选、但没被点名的包必须一动不动：这才是 --only-upgrade
  # 与"顺手把能升的都升了"的分水岭。
  expect_eq "upgrade-bystander-untouched" "1.0" "$(dpkg_version "$UPGRADE_BYSTANDER")"
  note "$UPGRADE_TARGET：$BEFORE_UP → $(dpkg_version "$UPGRADE_TARGET")；$UPGRADE_BYSTANDER 仍是 $(dpkg_version "$UPGRADE_BYSTANDER")"
  # 差集必须只有这一个包：这就是"没有偷偷做 dist-upgrade"的机械证明。
  dpkg-query -W -f='${binary:Package}\t${Version}\n' | sort >"$WORK/versions-after.txt"
  python3 - "$WORK/versions-before.txt" "$WORK/versions-after.txt" "$UPGRADE_TARGET" \
    >"$WORK/version-diff.txt" 2>"$WORK/version-diff.err" <<'PY'
import sys
before = dict(line.rstrip("\n").split("\t", 1) for line in open(sys.argv[1], encoding="utf-8") if "\t" in line)
after = dict(line.rstrip("\n").split("\t", 1) for line in open(sys.argv[2], encoding="utf-8") if "\t" in line)
target = sys.argv[3]
changed = {k for k in set(before) | set(after) if before.get(k) != after.get(k)}
others = sorted(changed - {target})
print(len(changed), len(others))
for name in others[:10]:
    sys.stderr.write("      其他变动：%s %s → %s\n" % (name, before.get(name), after.get(name)))
PY
  CHANGED=$(awk '{print $1}' "$WORK/version-diff.txt")
  CHANGED_OTHERS=$(awk '{print $2}' "$WORK/version-diff.txt")
  cat "$WORK/version-diff.err" 2>/dev/null
  note "版本表差集的行数：$CHANGED（其中与 $UPGRADE_TARGET 无关的：$CHANGED_OTHERS）"
  expect_eq "upgrade-changed-only-the-named-package" "0" "$CHANGED_OTHERS"
  # --force-confold 的真凭据：本地改过的那一行还在，包里新的 2.0 版本没有覆盖它。
  expect_eq "upgrade-conffile-kept-local-edit" "yes" \
    "$(grep -qx 'local_edit=yes' "/etc/$UPGRADE_TARGET.conf" && echo yes || echo no)"
  expect_eq "upgrade-conffile-not-overwritten" "no" \
    "$(grep -qx 'version=2.0' "/etc/$UPGRADE_TARGET.conf" && echo yes || echo no)"
  note "conffile 现场：$(tr '\n' ' ' <"/etc/$UPGRADE_TARGET.conf")；dpkg-dist 备份：$(ls "/etc/$UPGRADE_TARGET.conf".dpkg-* 2>/dev/null | tr '\n' ' ' || true)"
else
  notrun "upgrade-preflight-200" "没有可升级的包"
fi

echo
echo "== 12. 刷新索引：正常一次，坏源一次 =="
CODE=$(preflight "refresh" package.refresh '{}')
expect_eq "refresh-preflight-200" "200" "$CODE"
expect_eq "refresh-allowed" "True" "$(pf refresh 'd["data"]["allowed"]')"
expect_eq "refresh-no-reauth" "False" "$(pf refresh 'd["data"]["requires_reauth"]')"
expect_eq "refresh-no-phrase" "None" "$(pf refresh 'd["data"]["confirmation_phrase"]')"
expect_eq "refresh-not-cancellable" "False" "$(pf refresh 'd["data"]["cancellable"]')"
PF_REFRESH=$(pf refresh 'd["data"]["preflight_id"]')
REFRESH_KEY="p11-2-refresh-$$"
CODE=$(submit "refresh" "$PF_REFRESH" "" "$REFRESH_KEY")
expect_eq "refresh-submit-accepted" "202" "$CODE"
JOB_REFRESH=$(jget "$PAYLOADS/job-submit-refresh.json" 'd["data"]["job_id"]')
# 幂等：同一个键再提交一次，拿到的是同一个任务，不会跑第二遍。
CODE=$(api "$JAR" "$CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$PF_REFRESH\"}" "$REFRESH_KEY")
cp "$WORK/resp.json" "$PAYLOADS/job-submit-refresh-replay.json" 2>/dev/null
REPLAY_ID=$(jget "$PAYLOADS/job-submit-refresh-replay.json" 'd["data"]["job_id"]')
expect_eq "refresh-idempotent-replay-same-job" "$JOB_REFRESH" "$REPLAY_ID"
ST=$(wait_job "refresh" "$JOB_REFRESH" 360)
expect_eq "refresh-job-succeeded" "succeeded" "$ST"
expect_eq "refresh-observed-verified" "True" \
  "$(jbf refresh 'd["data"]["result"]["observed_state"]["verified"]')"
expect_eq "refresh-no-error-lines" "0" \
  "$(jbf refresh 'len(d["data"]["result"]["data"]["refresh_errors"] or [])')"
IDX_AFTER=$(jbf refresh 'd["data"]["result"]["data"]["index_after"]')
IDX_BEFORE=$(jbf refresh 'd["data"]["result"]["data"]["index_before"]')
IDX_ADV=$(jbf refresh 'd["data"]["result"]["data"]["index_advanced"]')
IDX_AGE=$(iso_age "$IDX_AFTER")
note "index_before=$IDX_BEFORE index_after=$IDX_AFTER index_advanced=$IDX_ADV（距现在 ${IDX_AGE:-?} 秒）"
# 一次"没有新东西"的刷新是**正常**的：apt 全是 Hit:，不重写 /var/lib/apt/lists，
# mtime 当然不动。所以判据不是"时间够新"，而是面板对索引的结论与它报出的两个时间自洽：
#   - index_advanced=true  → index_after 必须真往前走（并且不该是几百秒前的旧值）；
#   - index_advanced=false → 必须 before == after，不能一边说"没动"一边报个新时间。
# 第一版脚本在这里断言"index_after 必须距现在 180 秒内"，在 22.04 上把一次**正确**的
# 无变化刷新判成了失败（距现在 1003 秒，因为上一次 update 就是那个时间）——判据错了，不是产品错了。
if [ "$IDX_ADV" = "True" ]; then
  if [ -n "$IDX_AGE" ] && [ "$IDX_AGE" -ge 0 ] && [ "$IDX_AGE" -le 300 ]; then
    pass "refresh-advanced-index-is-fresh"
  else
    fail "refresh-advanced-index-is-fresh" "index_advanced=True 但 index_after 距现在 ${IDX_AGE:-无法解析} 秒"
  fi
elif [ "$IDX_BEFORE" = "$IDX_AFTER" ]; then
  pass "refresh-unchanged-index-is-honest"
else
  fail "refresh-unchanged-index-is-honest" "index_advanced=False，但 before=$IDX_BEFORE 与 after=$IDX_AFTER 不一致"
fi
# apt 真的被调用过：结果里有 apt-get update 自己的输出行（不是"看着像成功"）。
if jbf refresh 'd["data"]["result"]["data"]["stdout"] or ""' | grep -q 'Reading package lists'; then
  pass "refresh-actually-ran-apt"
else
  fail "refresh-actually-ran-apt" "结果里没有 apt-get update 的输出行"
fi
EXPECT_MTIME=$(iso_epoch "$IDX_AFTER")
ACTUAL_MTIME=$(lists_mtime)
if [ "$EXPECT_MTIME" = "$ACTUAL_MTIME" ]; then
  pass "refresh-index-time-matches-lists-mtime"
else
  fail "refresh-index-time-matches-lists-mtime" "index_after=$EXPECT_MTIME，/var/lib/apt/lists 的 mtime=$ACTUAL_MTIME"
fi

# 坏源：apt-get update 对"单源失败"仍可能退出 0，所以判据是 Err:/E: 行与任务终态。
printf 'deb http://127.0.0.1:1/upanel-p11-2-nope ./\n' >"$BROKEN_SOURCE"
CODE=$(preflight "refresh-broken" package.refresh '{}')
expect_eq "refresh-broken-preflight-200" "200" "$CODE"
PF_BROKEN=$(pf refresh-broken 'd["data"]["preflight_id"]')
CODE=$(submit "refresh-broken" "$PF_BROKEN" "" "p11-2-refresh-broken-$$")
expect_eq "refresh-broken-submit-accepted" "202" "$CODE"
JOB_BROKEN=$(jget "$PAYLOADS/job-submit-refresh-broken.json" 'd["data"]["job_id"]')
ST=$(wait_job "refresh-broken" "$JOB_BROKEN" 360)
expect_eq "refresh-broken-job-failed" "failed" "$ST"
# 失败任务的 result 按契约是空对象（CONTRACTS §5：错误写在 error，不塞进 result），
# 所以"哪一路源坏了"只能落在 error.message 里。第一版脚本在这里找 result.data.
# refresh_errors，永远找得到 0——它其实在断言一个契约明文禁止的东西。
expect_eq "refresh-broken-result-is-empty" "0" "$(jbf refresh-broken 'len(d["data"]["result"])')"
BR_CODE=$(jbf refresh-broken 'd["data"]["error"].get("code")')
BR_MSG=$(jbf refresh-broken 'd["data"]["error"].get("message")')
note "坏源刷新的失败原因：$BR_CODE / $BR_MSG"
if printf '%s' "$BR_MSG" | grep -Eq 'Err:|E: |错误行'; then
  pass "refresh-broken-error-message-carries-reason"
else
  fail "refresh-broken-error-message-carries-reason" "任务失败了，但 error.message 里没有 apt 的 Err:/E: 行：$BR_MSG"
fi
rm -f "$BROKEN_SOURCE"
CODE=$(preflight "refresh-recovered" package.refresh '{}')
PF_REC=$(pf refresh-recovered 'd["data"]["preflight_id"]')
CODE=$(submit "refresh-recovered" "$PF_REC" "" "p11-2-refresh-recovered-$$")
JOB_REC=$(jget "$PAYLOADS/job-submit-refresh-recovered.json" 'd["data"]["job_id"]')
ST=$(wait_job "refresh-recovered" "$JOB_REC" 360)
expect_eq "refresh-recovers-after-broken-source-removed" "succeeded" "$ST"

echo
echo "== 13. 真卸：短语是硬门槛 =="
CODE=$(preflight "remove" package.remove "{\"packages\":[\"$INSTALL_A\",\"$INSTALL_B\"]}")
expect_eq "remove-preflight-200" "200" "$CODE"
expect_eq "remove-allowed" "True" "$(pf remove 'd["data"]["allowed"]')"
# 公开 DTO 不暴露 requires_phrase（那是内部协议）：判据是"给了短语"，而短语只在
# 需要短语时才有值——null 与有值都说得清，省略字段才说不清。
expect_eq "remove-requires-phrase" "True" "$(pf remove 'd["data"]["confirmation_phrase"] is not None')"
PHRASE=$(pf remove 'd["data"]["confirmation_phrase"]')
note "确认短语：$PHRASE"
case "$PHRASE" in
  REMOVE\ *) pass "remove-phrase-shape" ;;
  *) fail "remove-phrase-shape" "短语不是 REMOVE + 随机码：$PHRASE" ;;
esac
PF_REMOVE=$(pf remove 'd["data"]["preflight_id"]')
expect_eq "remove-reauth" "200" "$(reauth)"
# 短语在**代理侧**比对（Web 只存摘要、不存明文），错短语按契约回 400
# CONFIRMATION_MISMATCH——不是"202 接受之后再失败"。这一点必须是同步拒绝：
# 先把一个必然失败的卸载任务写进队列，再让执行器慢慢发现，等于给错短语留了一个
# 真的执行窗口。
CODE=$(submit "remove-wrong-phrase" "$PF_REMOVE" "REMOVE WRONG9" "p11-2-remove-wrong-$$")
expect_eq "remove-wrong-phrase-400" "400" "$CODE"
expect_eq "remove-wrong-phrase-code" "CONFIRMATION_MISMATCH"   "$(jget "$PAYLOADS/job-submit-remove-wrong-phrase.json" 'd["error"]["code"]')"
expect_eq "remove-wrong-phrase-nothing-removed" "ii" "$(dpkg_state "$INSTALL_A")"
expect_eq "remove-wrong-phrase-package-b-intact" "ii" "$(dpkg_state "$INSTALL_B")"
note "拒绝原因：$(jget "$PAYLOADS/job-submit-remove-wrong-phrase.json" 'd["error"]["message"]' | cut -c1-160)"
# 一次预检只能提交一次任务：被拒的那次也已经把它用掉了（条件更新发生在调用代理之前），
# 所以同一份预检不能再提交，必须重新预检。这不是遗憾，是设计——否则"改一下短语再试"
# 就能在同一份计划上无限重放。
CODE=$(submit "remove-after-wrong-phrase" "$PF_REMOVE" "$PHRASE" "p11-2-remove-reuse-$$")
expect_eq "remove-preflight-consumed" "409" "$CODE"
expect_eq "remove-preflight-consumed-code" "PREFLIGHT_STALE"   "$(jget "$PAYLOADS/job-submit-remove-after-wrong-phrase.json" 'd["error"]["code"]')"

CODE=$(preflight "remove-ok" package.remove "{\"packages\":[\"$INSTALL_A\",\"$INSTALL_B\"]}")
PF_REMOVE_OK=$(pf remove-ok 'd["data"]["confirmation_phrase"]')
PF_REMOVE_ID=$(pf remove-ok 'd["data"]["preflight_id"]')
expect_eq "remove-second-reauth" "200" "$(reauth)"
CODE=$(submit "remove-ok" "$PF_REMOVE_ID" "$PF_REMOVE_OK" "p11-2-remove-ok-$$")
expect_eq "remove-submit-accepted" "202" "$CODE"
JOB_REMOVE=$(jget "$PAYLOADS/job-submit-remove-ok.json" 'd["data"]["job_id"]')
ST=$(wait_job "remove-ok" "$JOB_REMOVE" 1260)
expect_eq "remove-job-succeeded" "succeeded" "$ST"
expect_eq "remove-observed-verified" "True" \
  "$(jbf remove-ok 'd["data"]["result"]["observed_state"]["verified"]')"
expect_eq "remove-observed-pkg-a" "未安装" \
  "$(jbf remove-ok "d[\"data\"][\"result\"][\"observed_state\"][\"pkg:$INSTALL_A\"]")"
expect_eq "remove-dpkg-a-gone" "" "$(dpkg_state "$INSTALL_A")"
expect_eq "remove-dpkg-b-gone" "" "$(dpkg_state "$INSTALL_B")"
# 没有 conffile 的包卸完不该留下 rc（只剩配置的残留）。
expect_eq "remove-no-leftover-config" "no" \
  "$(dpkg-query -W -f='${db:Status-Abbrev}' "$INSTALL_A" 2>/dev/null | grep -q '^rc' && echo yes || echo no)"

echo
echo "== 13.5 硬杀：apt 在事务中途被杀，面板必须说实话、不自动修复、不重试 =="
# STAGES §13 的验收里有一条"APT 中断不自动强制修复"。它要证明的是**面板的克制**：
# 中断之后不许悄悄跑 `dpkg --configure -a`、`apt-get -f install`，也不许自己重试一次；
# 它只能如实说"结果未知/失败"，把修复留给操作者。
PKG_KILL=figlet
if [ "$(dpkg_present "$PKG_KILL")" = "yes" ]; then
  notrun "kill-mid-flight" "$PKG_KILL 已经装在这台机器上了，不拿它做硬杀样本"
else
  CODE=$(preflight "install-kill" package.install "{\"packages\":[\"$PKG_KILL\"]}")
  expect_eq "kill-preflight-200" "200" "$CODE"
  expect_eq "kill-preflight-allowed" "True" "$(pf install-kill 'd["data"]["allowed"]')"
  PF_KILL=$(pf install-kill 'd["data"]["preflight_id"]')
  expect_eq "kill-reauth" "200" "$(reauth)"
  JOBS_BEFORE_KILL=$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')
  CODE=$(submit "install-kill" "$PF_KILL" "" "p11-2-kill-$$")
  expect_eq "kill-submit-accepted" "202" "$CODE"
  JOB_KILL=$(jget "$PAYLOADS/job-submit-install-kill.json" 'd["data"]["job_id"]')
  # 等它真的开始写系统再杀：apt-get 进程出现之后才叫"事务中途"。
  #
  # 第一版这里是 `sleep 1.2` 然后 pkill：22.04 的 apt 比 24.04 快 28 倍，figlet 在
  # 1.2 秒内就装完了，杀了个空——任务于是 legitimately succeeded+verified，脚本却把它
  # 记成"面板声称已验证"的失败。判据修成轮询：进程一出现就杀，抓不到就如实 NOT RUN。
  KILL_LANDED=no
  KILL_VICTIM=""
  _i=0
  while [ "$_i" -lt 500 ]; do
    # 只杀**真的在装**的那一个。`apt-get -s ...` 是模拟——预检与提交复核都用它，
    # 杀掉它只会让复核退化成告警，安装照样跑完。第一版按 `(^|/)apt-get ` 无差别匹配，
    # 22.04 上杀到的正是复核用的模拟，任务于是 legitimately succeeded，脚本却判
    # "面板声称已验证"。判据改成：排除带 `-s` 的，并且把被杀进程的 argv 记进证据。
    _kp=$(pgrep -af '(^|/)apt-get ' 2>/dev/null | grep -v ' -s ' | awk '{print $1}' | head -1)
    if [ -n "$_kp" ]; then
      KILL_VICTIM=$(ps -o args= -p "$_kp" 2>/dev/null | head -c 200)
      kill -9 "$_kp" 2>/dev/null && KILL_LANDED=yes
      break
    fi
    sleep 0.05
    _i=$((_i + 1))
  done
  note "硬杀的目标进程：${KILL_VICTIM:-（25 秒内没抓到正在执行的 apt-get）}"
  ST=$(wait_job "install-kill" "$JOB_KILL" 360)
  AUDIT_AFTER_KILL=$(dpkg --audit 2>&1)
  if [ "$KILL_LANDED" = "yes" ]; then
    case "$ST" in
      failed|interrupted) pass "kill-job-terminal" ;;
      succeeded) fail "kill-job-terminal" "apt-get 被 SIGKILL 之后任务还是 succeeded：面板不能说一切都好" ;;
      *) fail "kill-job-terminal" "硬杀之后任务的终态是 '$ST'" ;;
    esac
    expect_ne "kill-job-not-verified-claim" "True"       "$(jbf install-kill 'd["data"]["result"]["observed_state"]["verified"]' 2>/dev/null)"
    # 用 `.get(...) or ""`：#jget 在键不存在时打印 "ERR:KeyError:..."，那是"读不到"，
    # 不是"有原因"。第一版把 ERR 当成了非空，于是这条检查在明明没有 message 时也 PASS。
    _kmsg=$(jbf install-kill 'd["data"]["error"].get("message") or ""')
    case "$_kmsg" in
      ""|ERR:*)
        fail "kill-job-real-error" "任务失败了却没给原因（error.message = '$_kmsg'）"
        ;;
      *)
        pass "kill-job-real-error"
        note "失败原因：$(printf '%s' "$_kmsg" | cut -c1-200)"
        # 光"有原因"不够：原因得说清是谁停的。这一条是 22.04 真机抓出来的——
        # 当时面板写的是「退出码 -1：Reading package lists...」，读的人不知道发生过硬杀。
        case "$_kmsg" in
          *"被信号"*SIG*|*"超时"*|*"时限"*)
            pass "kill-reason-names-the-signal"
            ;;
          *)
            fail "kill-reason-names-the-signal" "原因没说清是被信号终止的：$(printf '%s' "$_kmsg" | cut -c1-160)"
            ;;
        esac
        ;;
    esac
  else
    # 25 秒里没见到 apt-get 进程：这次什么都没杀到，"中断表现"无从谈起。
    notrun "kill-mid-flight" "25 秒内没抓到 apt-get 进程（$PKG_KILL 太小或任务已结束），硬杀这条没有证据"
    notrun "kill-job-not-verified-claim" "同上：本次没有真的中断，verified 的取值说明不了什么"
    notrun "kill-job-real-error" "同上：本次没有真的中断"
    notrun "kill-reason-names-the-signal" "同上：本次没有真的中断"
  fi
  # 不自动重试：任务数不再增长（重试会留下第二条 job）。
  sleep 5
  if [ "$KILL_LANDED" = "yes" ]; then
    expect_eq "kill-no-auto-retry" "$JOBS_BEFORE_KILL"     "$(( $(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs') - 1 ))"
    # 不自动修复：dpkg 的现场在 5 秒内不该被面板偷偷改好。
    expect_eq "kill-no-auto-repair" "$AUDIT_AFTER_KILL" "$(dpkg --audit 2>&1)"
  else
    notrun "kill-no-auto-retry" "本次没有真的中断，重试无从观察"
    notrun "kill-no-auto-repair" "本次没有真的中断，现场修复无从观察"
  fi
  note "dpkg --audit：$(printf '%s' "$AUDIT_AFTER_KILL" | head -3 | tr '
' ' ' | cut -c1-200)"
  # 修复交给操作者（脚本在这里扮演操作者），修完 apt 必须还能用。
  DEBIAN_FRONTEND=noninteractive dpkg --configure -a >/dev/null 2>&1
  DEBIAN_FRONTEND=noninteractive apt-get -y -q purge "$PKG_KILL" >/dev/null 2>&1
  expect_eq "kill-recovered-package-gone" "no" "$(dpkg_present "$PKG_KILL")"
  if apt-get -s install "$PKG_KILL" >/dev/null 2>&1; then
    pass "kill-apt-works-again"
  else
    fail "kill-apt-works-again" "硬杀之后 apt 模拟不再可用：$(apt-get -s install "$PKG_KILL" 2>&1 | tail -3 | tr '
' ' ')"
  fi
fi

echo
echo "== 14. 审计：预检、提交、被拒都留痕，且不含口令 =="
expect_eq "audit-has-package-preflights" "yes" \
  "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='operation.preflight' AND target LIKE 'preflight:package.%'")" -gt 0 ] && echo yes || echo no)"
expect_eq "audit-has-four-actions" "4" \
  "$(dbq "$WEB_DB" "SELECT COUNT(DISTINCT target) FROM audit_events WHERE action='operation.preflight' AND target IN ('preflight:package.refresh','preflight:package.install','preflight:package.upgrade','preflight:package.remove')")"
expect_eq "audit-has-job-submits" "yes" \
  "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='job.submit' AND job_id IS NOT NULL")" -gt 0 ] && echo yes || echo no)"
# 被拒也要留痕：outcome 的闭集是 success/failure（没有 denied 这一档），
# 被拒的提交记成 failure 并带上拒绝原因。
expect_eq "audit-has-failed-submit" "yes" \
  "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='job.submit' AND outcome='failure'")" -gt 0 ] && echo yes || echo no)"
if dbq "$WEB_DB" "SELECT group_concat(redacted_details) FROM audit_events" | grep -q "$PASSWORD"; then
  fail "audit-without-password" "审计明文字段里出现管理员口令"
else
  pass "audit-without-password"
fi
note "审计行数：$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM audit_events')；任务数：$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')"

echo
echo "== 15. 收尾：机器回到此前的样子 =="
expect_eq "final-a-gone" "no" "$(dpkg_present "$INSTALL_A")"
expect_eq "final-b-gone" "no" "$(dpkg_present "$INSTALL_B")"
expect_eq "final-ssh-still-there" "yes" "$(dpkg_present openssh-server)"
expect_eq "final-bash-still-there" "yes" "$(dpkg_present bash)"
expect_eq "final-no-broken-source" "no" "$([ -e "$BROKEN_SOURCE" ] && echo yes || echo no)"
# 夹具也要收干净：包、源、仓库目录一个不留，否则下一轮就不是从 1.0 开始了。
DEBIAN_FRONTEND=noninteractive apt-get -y -q purge "$UPGRADE_TARGET" "$UPGRADE_BYSTANDER" >/dev/null 2>&1
rm -f "$FIXLIST"
rm -rf "$FIXREPO" "$WORK/fixture-build"
expect_eq "final-fixture-a-gone" "no" "$(dpkg_present "$UPGRADE_TARGET")"
expect_eq "final-fixture-b-gone" "no" "$(dpkg_present "$UPGRADE_BYSTANDER")"
expect_eq "final-fixture-source-gone" "no" "$([ -e "$FIXLIST" ] && echo yes || echo no)"
expect_eq "final-fixture-repo-gone" "no" "$([ -d "$FIXREPO" ] && echo yes || echo no)"

echo
echo "== 16. 明确未覆盖 =="
notrun "APT_LOCK_UNREADABLE" "root 造不出 EACCES；不挪 dpkg 的锁文件，分支由 internal/apt/locks_test.go 覆盖"
notrun "重启执行与 boot_id 前后确认" "P11.3"
notrun "浏览器渲染" "VM 上没有浏览器；每一步的原始响应在 $PAYLOADS，能力表在 $FIXDIR"
notrun "Windows / macOS 宿主" "0.1 不支持"

echo
echo "== 汇总 =="
printf 'PASS %d  FAIL %d  NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
printf '载荷（原样抓取）：%s\n' "$PAYLOADS"
ls -1 "$PAYLOADS" 2>/dev/null | sed 's/^/      /'
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
