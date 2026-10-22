#!/bin/sh
# 在 Ubuntu 验收 VM 内按正确身份依次跑完所有验收脚本。
#
# 为什么需要它：这些脚本对运行身份的要求不同，跑错身份不会报“身份错”，
# 而是会得到看似通过或诡异挂起的结果——
#   * p01-1 / p01-2 / p01-4 / p02-1 / p02-2：**非 root**（脚本自己用 sudo 做特权部分，
#     并会断言“Web 不以 root 运行”“数据库文件属主是运行者”）；
#   * p02-3 / p02-4 / p03-1 / p03-2 / p03-3 / p03-4 / p04-1 / p04-2 / p04-3：**root**（代理只能在 root 下启动，
#     p03-4 还要 kill 代理、替换代理库来制造重启与数据丢失；p04-2 要挂载一块临时 loop 镜像；
#     p04-3 要停掉本机 NFS 服务、等 5 分钟隔离期到期，整脚本约 20 分钟——嫌慢可设 WAIT_QUARANTINE=0，
#     此时“隔离到期后容量自愈”一项如实记为 NOT RUN，其它断言照跑；
#     p07-1 要在三块**一次性空盘**上造分区/文件系统/swap/dm 现场（默认 sdb sdc sdd，
#     可用 SPARE_DISKS 覆盖），它会先算出承载 / 的整盘，一旦有盘与根设备同源就拒绝运行；
#     p07-2 会在同样的三块盘上**真的写盘**（初始化 GPT、建分区、删分区），跑完自己擦回空白；
#     p07-3 更狠，会真的 mkfs（还要在 mkfs 运行期间冻住它、杀掉代理），跑完也自己擦回空白。
#     三者必须按 p07-1 → p07-2 → p07-3 的顺序跑：后一个会把盘写脏，而前一个需要空白盘来验
#     “该拒的都拒”。
#     p07-4 用**两块**一次性盘（默认 sdb sdc）走完“导入 → 挂载 → 写文件 → 卸载 → 重挂载 →
#     开机自动挂载”的整条链，跑完**故意不改回空白**：它最后一步要重启 VM 证明 fstab 真的
#     生效，所以这里只跑第一段，第二段必须重启后手工跑：
#       sudo BIN_DIR=/var/tmp/upanel-bin PHASE=post-boot sh tests/integration/p07-4-storage-volume-acceptance.sh
#     BIN_DIR 必须扛得过重启（/tmp 重启后是空的）且对 upanel-web 可执行（Q203）。
#     第二段结束时会把卷卸载并退出管理，fstab 回到只有托管块标记的状态。
# 典型事故：用 `sudo sh p01-2-acceptance.sh` 跑 p01-2，root 绕过目录权限，
# “数据库目录不可写即拒绝启动”那条永远等不到退出，脚本会一直挂着。
#
# 用法（VM 内）：
#   BIN_DIR=/tmp/upanel-bin sh run-all.sh            # root 或能 sudo 的 upanel 用户都行
#   BIN_DIR=/tmp/upanel-bin sh run-all.sh p03-3-events p03-2-jobs   # 只跑指定几个
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
set -u

BIN_DIR="${BIN_DIR:-/tmp/upanel-bin}"
SRC_DIR="${SRC_DIR:-$(cd "$(dirname "$0")" && pwd)}"
DB_ROOT_BASE=/var/tmp

# p01-4 需要一份 deploy/ 目录（systemd 单元模板）；仓库里自带，按上传位置推导。
# 推导不到就交给脚本自己报“缺少 unit 文件”，不假装通过。
DEPLOY_GUESS="$(cd "$SRC_DIR/../.." 2>/dev/null && pwd)/deploy"
if [ -z "${DEPLOY_DIR:-}" ] && [ -d "$DEPLOY_GUESS" ]; then
    DEPLOY_DIR="$DEPLOY_GUESS"
fi
export DEPLOY_DIR="${DEPLOY_DIR:-}"
if [ -n "$DEPLOY_DIR" ] && [ ! -d "$DEPLOY_DIR/systemd" ]; then
    # 目录结构不对（例如 scp -r 把 deploy/ 塞进了已存在的 deploy/，变成 deploy/deploy），
    # 直接说清楚，别让 p01-4 以七条“单元文件不存在”的形式报出来。
    echo "DEPLOY_DIR=$DEPLOY_DIR 下没有 systemd/，systemd 相关断言不可能通过" >&2
    echo "检查上传结构：$DEPLOY_DIR 应当直接包含 systemd/ 与两个配置样例" >&2
    exit 2
fi

NONROOT_SCRIPTS="p01-1-acceptance p01-2-acceptance p01-4-acceptance p02-1-acceptance p02-2-acceptance"
ROOT_SCRIPTS="p02-3-agent-acceptance p02-4-agent-secrets-acceptance p03-1-preflight-acceptance p03-2-jobs-acceptance p03-3-events-acceptance p03-4-reconcile-acceptance p04-1-system-acceptance p04-2-storage-acceptance p04-3-storage-health-acceptance p05-1-services-acceptance p05-2-processes-acceptance p05-3-logs-acceptance p06-1-files-acceptance p06-2-files-write-acceptance p06-3-terminal-acceptance p07-1-storage-guard-acceptance p07-2-storage-write-acceptance p07-3-storage-format-acceptance p07-4-storage-volume-acceptance"

if [ $# -gt 0 ]; then
    SELECTED="$*"
else
    SELECTED="$NONROOT_SCRIPTS $ROOT_SCRIPTS"
fi

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
    SUDO="sudo"
fi

# 二进制先过一遍：三个入口缺一个或没有执行位，后面的脚本会以几十条“功能失败”的形式
# 报出来（p02-1 曾在 root 下刷出 45 条失败），看起来像产品坏了，其实是没传好文件。
MISSING=""
for bin in upanel-agent upanel-web upanelctl; do
    [ -x "$BIN_DIR/$bin" ] || MISSING="$MISSING $bin"
done
if [ -n "$MISSING" ]; then
    echo "缺少可执行文件（$BIN_DIR）:$MISSING" >&2
    echo "先上传二进制并 chmod 0755（注意 upanelctl 结尾没有连字符，别用 upanel-* 通配）" >&2
    exit 2
fi

# 需要时把命令降到非特权用户执行：优先用盘上真实存在的 upanel 账号。
if [ "$(id -u)" -eq 0 ]; then
    RUN_USER=${RUN_USER:-upanel}
    if ! id -u "$RUN_USER" >/dev/null 2>&1; then
        echo "找不到非特权账号 $RUN_USER，无法运行非 root 验收脚本" >&2
        exit 2
    fi
    RUN_UID=$(id -u "$RUN_USER")
    RUN_GID=$(id -g "$RUN_USER")
fi

run_nonroot() {
    if [ "$(id -u)" -eq 0 ]; then
        setpriv --reuid="$RUN_UID" --regid="$RUN_GID" --clear-groups \
            env BIN_DIR="$BIN_DIR" DEPLOY_DIR="$DEPLOY_DIR" sh "$SRC_DIR/$1.sh"
    else
        env BIN_DIR="$BIN_DIR" DEPLOY_DIR="$DEPLOY_DIR" sh "$SRC_DIR/$1.sh"
    fi
}

run_root() {
    $SUDO env BIN_DIR="$BIN_DIR" DEPLOY_DIR="$DEPLOY_DIR" sh "$SRC_DIR/$1.sh"
}

TOTAL_FAILED=0
TOTAL_RAN=0
SUMMARY=""

# 全部从干净状态开始：残留进程会占住 18881 端口或重造 socket，
# 上一次运行留下的数据库/配置也不该参与本次断言。
$SUDO pkill -f "upanel-agent --config" 2>/dev/null
$SUDO pkill -f "upanel-web --config" 2>/dev/null
sleep 1
$SUDO rm -rf /run/upanel 2>/dev/null
$SUDO rm -rf "$DB_ROOT_BASE"/upanel-accept-* 2>/dev/null

# 每个脚本的日志写在自建目录里，而不是 /tmp 的固定文件名：
# /tmp 有粘滞位，上一次以 root 建的同名文件会让这次的 runuser/setpriv 进程写不进去，
# 结果是脚本全都跑通了、重定向却失败，退出码变成 2，看起来像环境不满足（24.04 上真发生过）。
LOG_DIR=${LOG_DIR:-/var/tmp/upanel-logs}
$SUDO rm -rf "$LOG_DIR" 2>/dev/null
$SUDO mkdir -p "$LOG_DIR" 2>/dev/null
$SUDO chmod 0777 "$LOG_DIR" 2>/dev/null
# 旧版本用的 /tmp 固定文件名留着只会误导人（里面是上一次的结果）。
$SUDO rm -f /tmp/upanel-accept-*.log 2>/dev/null

# 默认配置路径的残留会被“默认路径”类断言读到：p01-2 的 `migrate --db` 与 p01-4 的 mock 段
# 都不传 --config，于是读 /etc/upanel/upanel.json。文件不存在时走内置默认值（正常）；
# 存在但是 0 字节就是“配置错误: 解析配置文件失败 /etc/upanel/upanel.json: EOF”。
# 2026-10-21 被打断的那次回归正好把 p01-4 装的这份配置截成 0 字节，下一轮 3 条断言因此
# 变成失败，看着像产品坏了。这里只如实报告，不替人删现场：/etc/upanel 是真实安装位置。
if [ -e /etc/upanel/upanel.json ] && [ ! -s /etc/upanel/upanel.json ]; then
    echo "警告: /etc/upanel/upanel.json 存在但为 0 字节（多半是上一次安装或被中断的残留）。" >&2
    echo "      默认配置路径的断言会报“解析配置文件失败 /etc/upanel/upanel.json: EOF”；" >&2
    echo "      确认是脏现场后再删除它，别把它记成产品缺陷。" >&2
fi

for name in $SELECTED; do
    script="$SRC_DIR/$name.sh"
    if [ ! -f "$script" ]; then
        echo "缺少脚本: $script" >&2
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        SUMMARY="$SUMMARY\n  $name: 缺失"
        continue
    fi
    case " $NONROOT_SCRIPTS " in
        *" $name "*) identity="非 root" ;;
        *) identity="root" ;;
    esac

    echo "== $name（$identity）"
    if [ "$identity" = "root" ]; then
        run_root "$name" > "$LOG_DIR/$name.log" 2>&1
    else
        run_nonroot "$name" > "$LOG_DIR/$name.log" 2>&1
    fi
    rc=$?
    TOTAL_RAN=$((TOTAL_RAN + 1))
    tail -3 "$LOG_DIR/$name.log"
    if [ "$rc" -ne 0 ]; then
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        SUMMARY="$SUMMARY\n  $name: exit=$rc（日志 $LOG_DIR/$name.log）"
    fi
    echo
done

echo "== 汇总 =="
printf '运行脚本数: %s\n' "$TOTAL_RAN"
printf '失败脚本数: %s\n' "$TOTAL_FAILED"
printf '日志目录: %s\n' "$LOG_DIR"
[ -z "$SUMMARY" ] || printf '%b\n' "$SUMMARY"
[ "$TOTAL_FAILED" -eq 0 ] || exit 1
exit 0
