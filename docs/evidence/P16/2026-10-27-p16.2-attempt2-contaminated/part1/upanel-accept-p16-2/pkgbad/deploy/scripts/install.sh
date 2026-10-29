#!/bin/sh
# uPanel 0.1 安装脚本（P10.4）。
#
# 这是一个**可审阅**的安装器，不是 `curl | bash`：先把发布包解开、核对校验和、
# 把本文件读完，再执行。脚本里没有任何下载步骤，只做下面列出的事。
#
# 步骤（每步都打印 [n/13] 与结论；失败时打印已完成步骤和重入命令）：
#   1  校验发布包（SHA256SUMS）
#   2  探测系统（Ubuntu 22.04/24.04、架构、systemd、端口、磁盘）
#   3  按功能列出缺失依赖（事实来自 upanelctl host-caps），**不擅自安装**
#   4  创建两个系统账号（upanel-web 无登录 shell；upanel-shell 有真 shell、无 sudo）
#   5  创建目录与权限（/etc/upanel 归 root，配置 0640 root:upanel-web）
#   6  生成面板自签名证书并打印指纹
#   7  写配置并跑 upanelctl check-config（配置的唯一裁判）
#   8  生成代理秘密密钥（已存在则跳过，绝不覆盖）
#   9  升级前备份：两个数据库 + 配置与密钥（库里已有数据时才做）
#  10  安装二进制并以 upanel-web 身份迁移数据库
#  11  创建第一个管理员（密码只经标准输入，不进 argv、不进日志）
#  12  安装并启动 systemd 单元（systemd-analyze verify + enable --now）
#  13  健康检查（HTTPS 校验真证书 + 指纹比对 + Web 不是 root）
#
# 关于第 9 步：升级会换掉二进制、跑数据库迁移，而迁移**只向前**。所以只要
# 这台机器上已经有数据，就在动二进制之前先备份——备份由 backup-state.sh 完成，
# 它停服务、把两个库（连同 -wal/-shm）与 /etc/upanel（含 agent.key）拷成一份目录，
# 再起回服务。第 9 步失败就停下：没有备份就不再往下动数据库。
#
# 用法（解开包后，以 root 执行）：
#   sudo sh deploy/scripts/install.sh --source .
#
# 参数：
#   --source DIR         发布包根目录（含 bin/ 与 SHA256SUMS），默认 .
#   --listen ADDR        面板监听地址，默认 127.0.0.1:9443
#   --admin-user NAME    第一个管理员用户名，默认 admin
#   --admin-display NAME 显示名（默认同用户名）
#   --password-stdin     从标准输入读两行密码（自动化用；交互安装省略即可，会提示输入）
#   --install-deps LIST  逗号分隔：nginx,certbot,samba,docker,smartmontools
#   --write-config       覆盖已存在的配置文件（先备份）
#   --backup-dir DIR     升级前备份放到哪里（默认 /var/backups/upanel/install-<UTC 时间>）
#   --no-backup          不备份就直接升级（库里已有数据时**不推荐**：迁移只向前）
#   --skip-checksums     跳过校验和（只用于本地反复调试安装器）
#
# 退出码：0 安装完成；2 参数或环境不满足、某一步失败（会打印已完成步骤）。

set -u

SELF=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/$(basename -- "$0")

SOURCE="."
LISTEN="127.0.0.1:9443"
ADMIN_USER="admin"
ADMIN_DISPLAY=""
PASSWORD_STDIN=0
INSTALL_DEPS=""
WRITE_CONFIG=0
SKIP_CHECKSUMS=0
BACKUP_TO=""
DO_BACKUP=1

WEB_USER="upanel-web"
SHELL_USER="upanel-shell"
ETC_DIR="/etc/upanel"
TLS_DIR="$ETC_DIR/tls"
CERT="$TLS_DIR/panel.pem"
KEY="$TLS_DIR/panel.key"
KEYFILE="$ETC_DIR/agent.key"
LIB_DIR="/usr/local/lib/upanel"
BIN_DIR="/usr/local/bin"
DOC_DIR="/usr/local/share/doc/upanel"
WEB_DATA="/var/lib/upanel"
AGENT_DATA="/var/lib/upanel-agent"
SHELL_HOME="/var/lib/upanel-shell"
SRV_ROOT="/srv/upanel"
MANUAL_CERT_DIR="/etc/ssl/upanel"
ACME_WEBROOT="/var/www/upanel-acme"
CONFIG_FILE="$ETC_DIR/upanel.json"

TOTAL_STEPS=13
STEP=0
COMPLETED=""

while [ $# -gt 0 ]; do
  case "$1" in
    --source) SOURCE=${2:-}; shift 2 ;;
    --listen) LISTEN=${2:-}; shift 2 ;;
    --admin-user) ADMIN_USER=${2:-}; shift 2 ;;
    --admin-display) ADMIN_DISPLAY=${2:-}; shift 2 ;;
    --password-stdin) PASSWORD_STDIN=1; shift ;;
    --install-deps) INSTALL_DEPS=${2:-}; shift 2 ;;
    --write-config) WRITE_CONFIG=1; shift ;;
    --backup-dir) BACKUP_TO=${2:-}; shift 2 ;;
    --no-backup) DO_BACKUP=0; shift ;;
    --skip-checksums) SKIP_CHECKSUMS=1; shift ;;
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 {exit}' "$SELF"; exit 0 ;;
    *) echo "错误: 未知参数 $1（--help 看用法）" >&2; exit 2 ;;
  esac
done

log()  { printf '%s\n' "$*"; }
step() { STEP=$((STEP + 1)); printf '\n[%d/%d] %s\n' "$STEP" "$TOTAL_STEPS" "$*"; }
ok()   { printf '      OK   %s\n' "$*"; }
warn() { printf '      注意 %s\n' "$*"; }

on_failure() {
  printf '\n==== 安装没有完成（%s）====\n' "$1" >&2
  if [ -n "$COMPLETED" ]; then
    printf '已完成的步骤（不需要重做，安装器是幂等的）：\n' >&2
    printf '%s' "$COMPLETED" | while IFS= read -r line; do
      if [ -n "$line" ]; then printf '  - %s\n' "$line" >&2; fi
    done
  else
    printf '没有任何步骤完成，系统未被修改。\n' >&2
  fi
  printf '\n修好之后用同一条命令重跑：\n  %s --source %s\n' "$SELF" "$SOURCE" >&2
  printf '服务相关日志：journalctl -u upanel-web -u upanel-agent -n 100 --no-pager\n' >&2
}
die() { on_failure "$*"; exit 2; }
done_step() { COMPLETED="${COMPLETED}${1}
"; }

have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- 1. 校验发布包
[ "$(id -u)" = "0" ] || die "必须以 root 执行（sudo sh $SELF ...）"
SOURCE=$(CDPATH= cd -- "$SOURCE" 2>/dev/null && pwd) || die "--source 指向的目录不存在"
[ -f "$SOURCE/bin/upanel-web" ] || die "$SOURCE 看起来不是发布包根目录（缺 bin/upanel-web）"

step "校验发布包"
if [ "$SKIP_CHECKSUMS" = "1" ]; then
  warn "按参数要求跳过校验和（发布给别人的时候不要这么用）"
elif [ -f "$SOURCE/SHA256SUMS" ]; then
  if ( cd "$SOURCE" && sha256sum -c --quiet SHA256SUMS ); then
    ok "包内文件与 SHA256SUMS 一致"
  else
    die "校验和不匹配：包被改过或传输损坏，不要继续安装"
  fi
else
  die "$SOURCE 里没有 SHA256SUMS；请用官方发布包（--skip-checksums 只用于本地调试）"
fi
done_step "1 校验发布包"

# ---------------------------------------------------------------- 2. 系统探测
step "探测系统"
[ -f /etc/os-release ] || die "读不到 /etc/os-release，无法确认发行版"
# 只取需要的三个变量，不 source 整个文件（避免污染当前 shell）。
OS_ID=$(sed -n 's/^ID=//p' /etc/os-release | tr -d '"' | head -n 1)
OS_VER=$(sed -n 's/^VERSION_ID=//p' /etc/os-release | tr -d '"' | head -n 1)
OS_NAME=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"' | head -n 1)
IDVER="$OS_ID:$OS_VER"
case "$IDVER" in
  ubuntu:22.04|ubuntu:24.04) ok "系统 $OS_NAME" ;;
  *) die "0.1 只支持 Ubuntu 22.04 与 24.04，当前是 $IDVER" ;;
esac

[ -d /run/systemd/system ] || die "这不是 systemd 引导的系统（/run/systemd/system 不存在）"
ok "systemd $(systemctl --version | head -n 1 | awk '{print $2}')"

ARCH=$(uname -m)
case "$ARCH" in
  x86_64) WANT_ARCH=amd64 ;;
  aarch64) WANT_ARCH=arm64 ;;
  *) die "不支持的架构 $ARCH（只有 amd64/aarch64）" ;;
esac
# 包里的二进制必须真能在这台机器上跑：拿错架构时给一句人话，
# 而不是等到 systemctl restart 才报 exec format error。
#
# 三种失败要分开说，因为修法完全不同：没有执行权限（包解压时丢了权限位）、
# 架构不符（拿错包）、文件损坏（下载断了）。混成一句“跑不起来”要人多试一遍。
if [ ! -x "$SOURCE/bin/upanelctl" ]; then
  die "包里的 $SOURCE/bin/upanelctl 没有执行权限：解压方式丢了权限位。请 chmod 0755 $SOURCE/bin/upanel-* 后重跑（这也说明这个包不该被这样解压）"
fi
if "$SOURCE/bin/upanelctl" version >/dev/null 2>&1; then
  ok "发布包二进制可在本机执行（$ARCH）"
else
  die "包里的二进制跑不起来（架构不符或文件损坏）：本机是 $ARCH，请用 linux-$WANT_ARCH 的包"
fi

PORT=${LISTEN##*:}
case "$PORT" in
  ''|*[!0-9]*) die "--listen 需要 host:port 形式，当前为 $LISTEN" ;;
esac
if ss -Hltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$PORT\$"; then
  HOLDER=$(ss -Hltnp 2>/dev/null | awk -v p=":$PORT" '$4 ~ p"$"' | head -n 1)
  HOLDER_PID=$(printf '%s' "$HOLDER" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
  OWN_PID=$(systemctl show -p MainPID --value upanel-web.service 2>/dev/null || true)
  # 重跑安装器时，占着这个端口的正是上一次装出来的 upanel-web。那不是“端口被占用”，
  # 那是幂等：判据是“监听者就是这个单元的主进程”，而不是“端口空着才算能装”。
  if [ -n "$HOLDER_PID" ] && [ "$HOLDER_PID" = "$OWN_PID" ]; then
    ok "端口 $PORT 由已安装的 upanel-web（pid $HOLDER_PID）占用：这是重跑，继续"
  else
    die "端口 $PORT 已被占用：$HOLDER"
  fi
else
  ok "端口 $PORT 空闲"
fi

FREE_MB=$(df -Pm /usr /var 2>/dev/null | awk 'NR>1 {if (min=="" || $4<min) min=$4} END {print min+0}')
[ "${FREE_MB:-0}" -ge 300 ] || die "/usr 或 /var 剩余空间不足 300 MiB（当前 ${FREE_MB}MiB）"
ok "磁盘剩余 ${FREE_MB} MiB（/usr 与 /var 取小）"
ok "内存 $(awk '/MemTotal/ {printf "%d", $2/1024}' /proc/meminfo) MiB"
done_step "2 探测系统（$OS_NAME / $ARCH）"

# ---------------------------------------------------------------- 3. 依赖
step "依赖检查（事实来自 upanelctl host-caps，不擅自安装）"
MISSING=""
# 依赖现状只有一份实现：同一个探测器也在面板的“外部程序能力”页与代理接口后面。
# 安装器不再自己 `command -v` 一遍——两处判据迟早会分叉（安装器认为“在 PATH 里就算有”，
# 而代理只在 /usr/sbin 这类固定目录里找，PATH 里可能有当前目录这种可写路径）。
# 拿到的还有版本：升级前知道原来装的是 nginx 1.24 而不是“装了”。
DEPS_TMP=$(mktemp) || die "无法创建临时文件"
DEP_STATE=$(mktemp) || die "无法创建临时文件"
"$SOURCE/bin/upanelctl" host-caps --line > "$DEP_STATE" 2>"$DEPS_TMP"
DEPS_RC=$?
if [ "$DEPS_RC" != "0" ]; then
  warn "依赖现状读不出来（upanelctl host-caps 退出码 $DEPS_RC）：安装继续，但不列缺失清单"
  if [ -s "$DEPS_TMP" ]; then sed 's/^/      /' "$DEPS_TMP"; fi
  warn "装完之后在面板「设置 → 外部程序能力」页可以看到这台机器到底有什么"
else
  # 六列制表符分隔：id、state、version、path、feature、hint（空字段表示“没有”）。
  dep_field() {
    awk -v id="$1" -v col="$2" '{ n = split($0, f, "\t"); if (n >= col && f[1] == id) { print f[col]; exit } }' "$DEP_STATE"
  }
  # $1 功能中文名  $2 host-caps 标识  $3 apt 包名
  check_dep() {
    _state=$(dep_field "$2" 2)
    _version=$(dep_field "$2" 3)
    case "$_state" in
      present)
        if [ -n "$_version" ]; then ok "$1：$2 $_version"
        else ok "$1：$2 已安装（版本读不出，可能是这个程序没有可靠的版本开关）"; fi ;;
      absent)
        MISSING="$MISSING $1=$3"; warn "$1：缺 $2（对应软件包 $3）" ;;
      unknown)
        # 说不清不等于没有，但也不能当成有：按缺列出，同时说明是“读不到”。
        MISSING="$MISSING $1=$3"; warn "$1：$2 的状态说不清（固定目录读不出来），按缺处理" ;;
      '')
        warn "$1：探测报告里没有 $2 这一条（探测器版本比安装器旧？）" ;;
      *) warn "$1：$2 的状态是意外值 $_state" ;;
    esac
  }
  check_dep "日志与终端审计" journalctl systemd
  check_dep "反向代理" nginx nginx
  check_dep "证书签发" certbot certbot
  check_dep "文件共享" samba samba
  check_dep "容器" docker docker.io
  check_dep "磁盘健康" smartctl smartmontools
  ok "依赖清单来自 $SOURCE/bin/upanelctl host-caps（与面板里那一页是同一份事实）"
fi
rm -f "$DEP_STATE" "$DEPS_TMP"

# 这两个是**安装器自己**用的工具，不属于面板功能清单（所以不在 host-caps 的表里）：
# openssl 生成面板证书，journalctl 在失败时取日志，curl 做健康检查。
installer_need() {
  if have "$1"; then ok "$2：$1 已安装"; else
    MISSING="$MISSING $2=$3"; warn "$2：缺 $1（对应软件包 $3）"; fi
}
installer_need openssl "面板证书与证书模块" openssl
installer_need curl "安装期健康检查" curl

if [ -n "$INSTALL_DEPS" ]; then
  PKGS=""
  for item in $(printf '%s' "$INSTALL_DEPS" | tr ',' ' '); do
    case "$item" in
      nginx|certbot|samba|smartmontools) PKGS="$PKGS $item" ;;
      docker|docker.io) PKGS="$PKGS docker.io" ;;
      *) die "--install-deps 里不认识 $item（可用：nginx,certbot,samba,docker,smartmontools）" ;;
    esac
  done
  log "      按要求安装：$PKGS"
  DEBIAN_FRONTEND=noninteractive apt-get update -qq || die "apt-get update 失败"
  # 只装点名的包：不 upgrade 系统，不删除任何东西。
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends $PKGS \
    || die "apt 安装 $PKGS 失败（看上面的 apt 输出）"
  ok "已安装：$PKGS"
else
  if [ -n "$MISSING" ]; then
    warn "缺失清单：$MISSING"
    warn "面板照常启动并在相关页面显示“依赖未安装”；要装就重跑并加：--install-deps ${MISSING# }"
  fi
fi
done_step "3 依赖检查"

# ---------------------------------------------------------------- 4. 账号
step "系统账号"
create_account() {
  # $1 账号  $2 home  $3 shell  $4 说明
  if id -u "$1" >/dev/null 2>&1; then
    ok "$1 已存在（保留现有设置）"
    return 0
  fi
  # “组在、账号不在”是一种真实的半截状态：userdel 只在组里没有别的成员时才顺手删组，
  # 而 useradd --system 看到同名组已经存在会直接报
  #   useradd: group upanel-web exists - if you want to add this user to that group, use -g
  # 并以 9 退出——安装器宣称幂等，就不能被这个状态卡死。但**也不能默默复用这个组**：
  # 它正是 /etc/upanel/tls（0750 root:upanel-web）与面板数据目录的属组，组里多出来的
  # 成员可以直接读面板私钥与会话数据。所以分成两种：
  #   - 组存在但没有任何成员（含把该组当主组的用户）→ 复用它（-g）；
  #   - 组存在且有成员 → 停下来点名，由操作者先处理（安装器不替人清别人的组）。
  if getent group "$1" >/dev/null 2>&1; then
    _gid=$(getent group "$1" | cut -d: -f3)
    _members=$(getent group "$1" | cut -d: -f4 | tr ',' ' ')
    _primary=$(awk -F: -v gid="$_gid" '$4 == gid { printf "%s ", $1 }' /etc/passwd)
    _all=$(printf '%s %s' "$_members" "$_primary" | tr -s ' ' | sed 's/^ //; s/ $//')
    if [ -n "$_all" ]; then
      die "组 $1 已经存在，并且里面有成员（${_all}）：这些成员会获得 $TLS_DIR 与 $WEB_DATA 的读权限。
      先把它们移出（gpasswd -d <账号> $1）再重跑；安装器不动别人的组成员关系"
    fi
    useradd --system --home-dir "$2" --shell "$3" --no-create-home --comment "$4" -g "$1" "$1" || return 1
    ok "创建账号 $1（复用已存在的空组 $1，home $2，shell $3）"
    return 0
  fi
  useradd --system --home-dir "$2" --shell "$3" --no-create-home --comment "$4" "$1" || return 1
  ok "创建账号 $1（home $2，shell $3）"
}
# upanel-web：无登录 shell、无 sudo、不进任何特权组。
create_account "$WEB_USER" "$WEB_DATA" /usr/sbin/nologin "uPanel Web 服务" || die "创建 $WEB_USER 失败"
# upanel-shell：终端要真 shell（产品会拒绝 nologin），但口令锁死、不进 sudo/docker/disk。
create_account "$SHELL_USER" "$SHELL_HOME" /bin/bash "uPanel 终端账号" || die "创建 $SHELL_USER 失败"
passwd -l "$SHELL_USER" >/dev/null 2>&1 || warn "锁定 $SHELL_USER 口令失败（请手工执行 passwd -l $SHELL_USER）"

for u in "$WEB_USER" "$SHELL_USER"; do
  if id -nG "$u" | tr ' ' '\n' | grep -qxE 'sudo|admin|docker|disk|lxd|root'; then
    die "$u 属于特权组（$(id -nG "$u")）：Web 与终端账号都不允许，请先移出再重跑"
  fi
done
done_step "4 系统账号（$WEB_USER、$SHELL_USER）"

# ---------------------------------------------------------------- 5. 目录
step "目录与权限"
install -d -m 0755 -o root -g root "$LIB_DIR"
install -d -m 0755 -o root -g root "$DOC_DIR"
# /etc/upanel 必须 root 拥有：Web 账号能读配置（属组可读）但**不能**替换它。
install -d -m 0755 -o root -g root "$ETC_DIR"
install -d -m 0750 -o root -g "$WEB_USER" "$TLS_DIR"
install -d -m 0750 -o "$WEB_USER" -g "$WEB_USER" "$WEB_DATA"
install -d -m 0700 -o root -g root "$AGENT_DATA"
install -d -m 0700 -o "$SHELL_USER" -g "$SHELL_USER" "$SHELL_HOME"
install -d -m 0755 -o root -g root "$SRV_ROOT"
install -d -m 0700 -o root -g root "$MANUAL_CERT_DIR"
install -d -m 0755 -o root -g root "$ACME_WEBROOT"
ok "$ETC_DIR 0755 root:root（配置 0640 root:$WEB_USER：能读不能改）"
ok "$TLS_DIR 0750 root:$WEB_USER"
ok "$WEB_DATA 0750 $WEB_USER；$AGENT_DATA 0700 root；$SHELL_HOME 0700 $SHELL_USER"
ok "$SRV_ROOT 0755 root；$MANUAL_CERT_DIR 0700 root；$ACME_WEBROOT 0755 root"
done_step "5 目录与权限"

# ---------------------------------------------------------------- 6. 面板证书
step "面板 HTTPS 证书"
if [ -s "$CERT" ] && [ -s "$KEY" ]; then
  ok "沿用已存在的面板证书（换证书：替换这两个文件后 systemctl restart upanel-web）"
else
  HOST_SHORT=$(hostname -s 2>/dev/null || echo upanel)
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$KEY" -out "$CERT" -days 825 -sha256 \
    -subj "/CN=$HOST_SHORT" \
    -addext "subjectAltName=DNS:$HOST_SHORT,DNS:localhost,IP:127.0.0.1" \
    >/dev/null 2>&1 || die "生成自签名证书失败（openssl req）"
  ok "已生成自签名证书（ECDSA P-256，825 天，CN=$HOST_SHORT）"
fi
chown root:"$WEB_USER" "$CERT" "$KEY" || die "设置证书属主失败"
chmod 0644 "$CERT"
chmod 0640 "$KEY"
FINGERPRINT=$(openssl x509 -in "$CERT" -noout -fingerprint -sha256 | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f')
NOT_AFTER=$(openssl x509 -in "$CERT" -noout -enddate | sed 's/^notAfter=//')
[ -n "$FINGERPRINT" ] || die "读不出证书指纹（$CERT 不是有效证书？）"
log "      证书指纹 sha256: $FINGERPRINT"
log "      有效期至:        $NOT_AFTER"
log "      浏览器第一次连接会提示“不受信任”：核对上面这串指纹后再接受。"
done_step "6 面板 HTTPS 证书（指纹 $FINGERPRINT）"

# ---------------------------------------------------------------- 7. 配置
step "配置文件"
if [ -s "$CONFIG_FILE" ] && [ "$WRITE_CONFIG" != "1" ]; then
  ok "$CONFIG_FILE 已存在（保留；要覆盖请加 --write-config，会先备份）"
else
  if [ -s "$CONFIG_FILE" ]; then
    BAK="$CONFIG_FILE.bak.$(date -u +%Y%m%dT%H%M%SZ)"
    cp -p "$CONFIG_FILE" "$BAK"
    warn "原配置已备份到 $BAK"
  fi
  TMP_CONFIG=$(mktemp "$ETC_DIR/.upanel.json.XXXXXX") || die "无法在 $ETC_DIR 建临时文件"
  cat > "$TMP_CONFIG" <<EOF
{
  "web": {
    "mode": "production",
    "listen_address": "$LISTEN",
    "tls_cert_path": "$CERT",
    "tls_key_path": "$KEY",
    "shutdown_timeout_seconds": 10
  },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER",
    "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DATA/agent.db",
    "secret_key_path": "$KEYFILE",
    "storage": { "smart_enabled": true, "smart_interval_minutes": 15 },
    "files": {
      "roots": [
        { "id": "srv", "name": "站点与应用目录", "path": "$SRV_ROOT", "writable": true }
      ]
    },
    "terminal": {
      "shell_user": "$SHELL_USER",
      "home_path": "$SHELL_HOME"
    }
  },
  "database": { "path": "$WEB_DATA/upanel.db", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json", "add_source": false }
}
EOF
  chown root:"$WEB_USER" "$TMP_CONFIG"
  chmod 0640 "$TMP_CONFIG"
  mv -f "$TMP_CONFIG" "$CONFIG_FILE"     # 同目录 rename：不会留下半个配置文件
  ok "已写 $CONFIG_FILE（0640 root:$WEB_USER）"
fi

# check-config 是配置的唯一裁判：它读证书、判 TLS 与监听地址的组合、算允许源。
"$SOURCE/bin/upanelctl" check-config --config "$CONFIG_FILE" | sed 's/^/      /' \
  || die "配置校验没通过（上面是 check-config 的输出）"
ok "upanelctl check-config 通过"

# 后面几步都以“配置里实际生效的值”为准，而不是命令行参数：配置可能是上一次安装留下的，
# --listen 这次未必被采用。--json 的输出是稳定契约，从这里读 scheme 与监听地址。
CFG_JSON=$("$SOURCE/bin/upanelctl" check-config --config "$CONFIG_FILE" --json) \
  || die "check-config --json 失败"
CFG_LISTEN=$(printf '%s\n' "$CFG_JSON" | sed -n 's/.*"web_listen": *"\([^"]*\)".*/\1/p')
[ -n "$CFG_LISTEN" ] || die "无法从 check-config --json 读出 web.listen_address"
if printf '%s\n' "$CFG_JSON" | grep -q '"web_tls"'; then
  CFG_SCHEME=https
  ok "配置里已启用面板 TLS（监听 $CFG_LISTEN）"
else
  CFG_SCHEME=http
  warn "配置里没有 TLS（监听 $CFG_LISTEN）：只允许回环访问，浏览器无法完成登录"
  warn "本次安装生成的自签名证书已就位但没有被配置引用；要用它就把 web.tls_cert_path /"
  warn "web.tls_key_path 指向 $CERT 与 $KEY 后 systemctl restart upanel-web（见 docs/ACCESS.md）"
fi
done_step "7 配置文件"

# 数据库位置按**配置里生效的值**取，不写死 /var/lib/upanel：配置可能早于本次安装，
# 也可能有人把库挪到了别的盘上。check-config --json 是这些值的唯一出口。
PANEL_DB=$(printf '%s\n' "$CFG_JSON" | sed -n 's/.*"database_path": *"\([^"]*\)".*/\1/p' | head -n 1)
AGENT_DB=$(printf '%s\n' "$CFG_JSON" | sed -n 's/.*"agent_database_path": *"\([^"]*\)".*/\1/p' | head -n 1)
[ -n "$PANEL_DB" ] || warn "配置里没有 database.path：第 9 步会跳过面板库备份"
[ -n "$AGENT_DB" ] || warn "配置里没有 agent.database_path：第 9 步会跳过代理库备份"

# ---------------------------------------------------------------- 8. 代理密钥
step "代理秘密密钥"
if [ -s "$KEYFILE" ]; then
  ok "$KEYFILE 已存在（保留：覆盖会让已保存的秘密永久解不开）"
else
  KEY_OUT=$("$SOURCE/bin/upanel-agent" -init-key --config "$CONFIG_FILE" 2>&1) \
    || die "生成代理密钥失败：$KEY_OUT"
  chown root:root "$KEYFILE"
  chmod 0600 "$KEYFILE"
  printf '%s\n' "$KEY_OUT" | sed 's/^/      /'
  ok "已生成 $KEYFILE（root:root 0600）"
  warn "这个文件必须单独离线备份：丢了以后已保存的秘密（共享口令、证书私钥引用）就解不开了"
fi
done_step "8 代理秘密密钥"

# ---------------------------------------------------------------- 9. 升级前备份
step "升级前备份（两数据库 + 配置与密钥）"
# 只有“库真的已经存在”才需要备份：全新安装没有东西可丢，也就没有可备份的内容。
NEED_BACKUP=0
for db in "${PANEL_DB:-}" "${AGENT_DB:-}"; do
  [ -n "$db" ] && [ -s "$db" ] && NEED_BACKUP=1
done
if [ "$NEED_BACKUP" = "0" ]; then
  ok "这台机器上还没有数据库（全新安装），跳过备份"
elif [ "$DO_BACKUP" = "0" ]; then
  warn "按参数要求不备份就直接动二进制与数据库（--no-backup）：迁移只向前，出问题时没有回退点"
elif [ ! -f "$SOURCE/deploy/scripts/backup-state.sh" ]; then
  warn "发布包里没有 backup-state.sh：本次没有备份（升级前请手工备份两个库与 /etc/upanel）"
else
  if [ -z "$BACKUP_TO" ]; then
    BACKUP_TO="/var/backups/upanel/install-$(date -u +%Y%m%dT%H%M%SZ)"
  fi
  # 用包里的 upanelctl 而不是已装的：旧版本没有 agent_database_path 这个字段，
  # 备份脚本靠它定位代理库，用旧二进制会走到“读不到代理库”的分支。
  sh "$SOURCE/deploy/scripts/backup-state.sh" \
    --to "$BACKUP_TO" \
    --config "$CONFIG_FILE" \
    --upanelctl "$SOURCE/bin/upanelctl" | sed 's/^/      /' \
    || die "备份没有完成：没有备份就不再往下动数据库（修好后重跑同一条命令，或用 --no-backup 明确承担风险）"
  ok "备份在 $BACKUP_TO（两个库 + /etc/upanel；整份拷走再升级）"
fi
done_step "9 升级前备份"

# ---------------------------------------------------------------- 10. 二进制/库
step "安装二进制并迁移数据库"
install -m 0755 -o root -g root "$SOURCE/bin/upanel-web" "$BIN_DIR/upanel-web" || die "安装 upanel-web 失败"
install -m 0755 -o root -g root "$SOURCE/bin/upanelctl" "$BIN_DIR/upanelctl" || die "安装 upanelctl 失败"
install -m 0755 -o root -g root "$SOURCE/bin/upanel-agent" "$LIB_DIR/upanel-agent" || die "安装 upanel-agent 失败"
ok "upanel-web、upanelctl → $BIN_DIR；upanel-agent → $LIB_DIR"

# 文档进 /usr/local/share/doc/upanel：出故障的时候，README 与恢复手册必须在本机就有，
# 而不是“去哪个仓库翻一下”。包里有哪份拷哪份，不编造缺失的文件。
DOC_COUNT=0
for doc in README.md VERSION deploy/upanel.example.json \
           docs/INSTALL.md docs/ACCESS.md docs/RECOVERY.md \
           docs/LOCAL_START.md docs/STORAGE_RECOVERY.md docs/SHARES_CLIENT.md; do
  if [ -f "$SOURCE/$doc" ]; then
    install -m 0644 -o root -g root "$SOURCE/$doc" "$DOC_DIR/$(basename -- "$doc")" \
      || die "拷贝文档 $doc 到 $DOC_DIR 失败"
    DOC_COUNT=$((DOC_COUNT + 1))
  fi
done
ok "$DOC_COUNT 份文档已装到 $DOC_DIR"

# 迁移必须以 upanel-web 身份跑：root 建出来的库文件属主是 root，
# Web 服务启动后就再也写不进去（只剩一个看不懂的 SQLITE_READONLY）。
runuser -u "$WEB_USER" -- "$BIN_DIR/upanelctl" migrate --config "$CONFIG_FILE" | sed 's/^/      /' \
  || die "数据库迁移失败（upanelctl migrate）"
ok "数据库已迁移（$WEB_DATA/upanel.db，属主 $WEB_USER）"
done_step "10 二进制与数据库"

# ---------------------------------------------------------------- 11. 管理员
step "第一个管理员"
# 先无密码探测一次：init-admin 在“已有账号”时会在读密码之前就退出，
# 因此这条探测不会创建任何账号，也不会在重跑时白问一遍密码。
PROBE=$(printf '' | runuser -u "$WEB_USER" -- "$BIN_DIR/upanelctl" init-admin \
  --config "$CONFIG_FILE" --username "$ADMIN_USER" 2>&1)
if printf '%s' "$PROBE" | grep -q '已有'; then
  ok "数据库里已有账号，跳过创建（重置密码见 $DOC_DIR/RECOVERY.md）"
  ADMIN_USER="(已存在的账号)"
else
  case "$PROBE" in
    *"读取密码失败"*) : ;;
    *"需要两行密码"*) : ;;
    *) die "init-admin 探测返回了意外结果：$PROBE" ;;
  esac

  ATTEMPT=0
  while : ; do
    ATTEMPT=$((ATTEMPT + 1))
    if [ "$PASSWORD_STDIN" = "1" ]; then
      IFS= read -r PW1 || die "--password-stdin 需要标准输入给两行密码"
      IFS= read -r PW2 || die "--password-stdin 需要两行密码（第二行缺失）"
    else
      printf '管理员 %s 的密码（至少 12 个字符，输入不回显）: ' "$ADMIN_USER"
      stty -echo 2>/dev/null || true
      IFS= read -r PW1 || true
      stty echo 2>/dev/null || true
      printf '\n再次输入: '
      stty -echo 2>/dev/null || true
      IFS= read -r PW2 || true
      stty echo 2>/dev/null || true
      printf '\n'
    fi
    if [ -z "${PW1:-}" ]; then
      unset PW1 PW2
      if [ "$PASSWORD_STDIN" = "1" ]; then die "从标准输入读到空密码"; fi
      warn "没有输入密码"
      if [ "$ATTEMPT" -ge 3 ]; then die "连续 3 次没有输入密码"; fi
      continue
    fi
    if [ "${PW1}" != "${PW2:-}" ]; then
      unset PW1 PW2
      if [ "$PASSWORD_STDIN" = "1" ]; then die "两行密码不一致"; fi
      warn "两次输入不一致"
      if [ "$ATTEMPT" -ge 3 ]; then die "连续 3 次输入不一致"; fi
      continue
    fi
    if [ -n "$ADMIN_DISPLAY" ]; then
      ADMIN_OUT=$(printf '%s\n%s\n' "$PW1" "$PW1" | runuser -u "$WEB_USER" -- \
        "$BIN_DIR/upanelctl" init-admin --config "$CONFIG_FILE" \
        --username "$ADMIN_USER" --display-name "$ADMIN_DISPLAY" 2>&1)
    else
      ADMIN_OUT=$(printf '%s\n%s\n' "$PW1" "$PW1" | runuser -u "$WEB_USER" -- \
        "$BIN_DIR/upanelctl" init-admin --config "$CONFIG_FILE" \
        --username "$ADMIN_USER" 2>&1)
    fi
    ADMIN_RC=$?
    unset PW1 PW2
    if [ "$ADMIN_RC" = "0" ]; then
      printf '%s\n' "$ADMIN_OUT" | sed 's/^/      /'
      ok "已创建管理员 $ADMIN_USER"
      break
    fi
    if printf '%s' "$ADMIN_OUT" | grep -q '已有'; then
      ok "数据库里已有账号，跳过创建"
      break
    fi
    printf '%s\n' "$ADMIN_OUT" | sed 's/^/      /' >&2
    if [ "$PASSWORD_STDIN" = "1" ]; then die "创建管理员失败（原因见上）"; fi
    if [ "$ATTEMPT" -ge 3 ]; then die "连续 3 次没能创建管理员"; fi
    warn "密码没被接受，再试一次"
  done
fi
done_step "11 第一个管理员"

# ---------------------------------------------------------------- 12. 单元
step "systemd 单元"
install -m 0644 -o root -g root "$SOURCE/deploy/systemd/upanel-agent.service" \
  /etc/systemd/system/upanel-agent.service || die "安装 upanel-agent.service 失败"
install -m 0644 -o root -g root "$SOURCE/deploy/systemd/upanel-web.service" \
  /etc/systemd/system/upanel-web.service || die "安装 upanel-web.service 失败"
systemctl daemon-reload || die "systemctl daemon-reload 失败"
systemd-analyze verify /etc/systemd/system/upanel-agent.service \
  /etc/systemd/system/upanel-web.service \
  || die "systemd-analyze verify 报错（单元文件与这台机器的 systemd 不兼容）"
ok "单元文件通过 systemd-analyze verify"
systemctl enable upanel-agent.service upanel-web.service >/dev/null 2>&1 \
  || die "systemctl enable 失败"
systemctl restart upanel-agent.service || {
  journalctl -u upanel-agent -n 20 --no-pager >&2
  die "upanel-agent 启动失败（上面是最近 20 行日志）"
}
systemctl restart upanel-web.service || {
  journalctl -u upanel-web -n 20 --no-pager >&2
  die "upanel-web 启动失败（上面是最近 20 行日志）"
}
ok "两个单元已 enable 并启动"
done_step "12 systemd 单元"

# ---------------------------------------------------------------- 13. 健康检查
step "健康检查"
# 探测走回环地址：自签名证书的 SAN 里固定带了 IP:127.0.0.1，
# 所以 TLS 下这里可以**不加 -k** 校验证书——要证明的正是“证书配得上、指纹是那一串”。
HOST_PART=${CFG_LISTEN%:*}
CFG_PORT=${CFG_LISTEN##*:}
case "$HOST_PART" in
  0.0.0.0|'::'|'[::]'|'') PROBE_HOST=127.0.0.1 ;;
  *) PROBE_HOST=$HOST_PART ;;
esac
HEALTH_URL="$CFG_SCHEME://$PROBE_HOST:$CFG_PORT/api/v1/health/live"

if [ "$CFG_SCHEME" = "https" ]; then
  CURL_TLS="--cacert $CERT"
else
  CURL_TLS=""
fi

i=0
while [ "$i" -lt 60 ]; do
  # shellcheck disable=SC2086  # CURL_TLS 为空或恰好两个词，故意不加引号
  if curl -sS --max-time 3 $CURL_TLS "$HEALTH_URL" >/dev/null 2>&1; then break; fi
  i=$((i + 1))
  sleep 1
done
if [ "$i" -ge 60 ]; then
  journalctl -u upanel-web -n 30 --no-pager >&2
  die "面板 60 秒内没有在 $HEALTH_URL 上响应"
fi
# shellcheck disable=SC2086
HEALTH=$(curl -sS --max-time 3 $CURL_TLS "$HEALTH_URL") || die "健康检查请求失败"
printf '%s' "$HEALTH" | grep -q '"status"' || die "健康检查返回了意外内容：$HEALTH"
ok "$CFG_SCHEME 健康检查通过：$HEALTH"

if [ "$CFG_SCHEME" = "https" ]; then
  SERVED=$(echo | openssl s_client -connect "$PROBE_HOST:$CFG_PORT" \
    -servername "$(hostname -s 2>/dev/null || echo localhost)" 2>/dev/null \
    | openssl x509 -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f')
  [ -n "$SERVED" ] || die "握手后读不到服务端证书指纹（openssl s_client）"
  [ "$SERVED" = "$FINGERPRINT" ] || die "服务端证书指纹 $SERVED ≠ 安装的证书 $FINGERPRINT"
  ok "端口 $CFG_PORT 上服务的正是安装的那张证书"
else
  warn "跳过证书指纹比对：配置没有启用 TLS"
fi

WEB_PID=$(systemctl show -p MainPID --value upanel-web.service)
WEB_UID=$(ps -o uid= -p "${WEB_PID:-0}" 2>/dev/null | tr -d ' ')
if [ "${WEB_UID:-0}" = "0" ]; then
  die "upanel-web 竟然以 root 运行（pid $WEB_PID）：先停下，检查单元文件"
fi
ok "Web 进程 uid=${WEB_UID:-?}（不是 root）"

if [ -S /run/upanel/agent.sock ]; then
  ok "代理 socket 存在；服务状态 $(systemctl is-active upanel-agent.service)"
else
  warn "代理 socket 不存在：$HEALTH 仍可能正常，但系统功能会报 AGENT_UNAVAILABLE"
fi
done_step "13 健康检查"

# ---------------------------------------------------------------- 完成
printf '\n==== 安装完成 ====\n'
if [ "${NEED_BACKUP:-0}" = "1" ] && [ -n "${BACKUP_TO:-}" ]; then
  printf '升级前备份:    %s（两个库 + /etc/upanel，整份拷走再清理）\n' "$BACKUP_TO"
fi
cat <<EOF
面板监听:      $CFG_LISTEN（$CFG_SCHEME）
访问地址:      $CFG_SCHEME://$CFG_LISTEN
证书指纹:      $FINGERPRINT（有效期至 $NOT_AFTER）$( [ "$CFG_SCHEME" = "https" ] || echo '  ← 注意：配置未启用 TLS，这个证书尚未被引用' )
管理员账号:    $ADMIN_USER
配置文件:      $CONFIG_FILE
Web 数据库:    $WEB_DATA/upanel.db（属主 $WEB_USER）
代理数据库:    $AGENT_DATA/agent.db（root 0700）
代理密钥:      $KEYFILE（root 0600，必须离线备份）

只监听回环地址时，从自己的电脑建 SSH 隧道再开浏览器：
  ssh -N -L $CFG_PORT:127.0.0.1:$CFG_PORT 你的用户@这台主机
  然后访问 $CFG_SCHEME://127.0.0.1:$CFG_PORT

要改成对管理网直接可达（会改变暴露面，先读 docs/ACCESS.md 与 docs/INSTALL.md）：
  1) 改 $CONFIG_FILE 里的 web.listen_address（例如 192.0.2.10:9443）
  2) 同时设置 web.allowed_origins（非回环监听必须显式给 https:// 源，否则拒绝启动）
  3) systemctl restart upanel-web

日常检查：
  upanelctl check-config --config $CONFIG_FILE
  systemctl status upanel-web upanel-agent
  journalctl -u upanel-web -u upanel-agent -f

忘了管理员密码、证书过期、代理连不上、数据库损坏、fstab 写错：见
  $DOC_DIR/RECOVERY.md（安装包里同 docs/RECOVERY.md）
EOF
