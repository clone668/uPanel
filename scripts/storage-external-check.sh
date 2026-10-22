#!/bin/sh
# 存储状态的**外部独立检查**（P07.5）。
#
# 这个脚本不跟面板说话，也不读面板的数据库或日志。它只读这台机器自己的东西：
#   /etc/fstab 的托管块、findmnt、/proc/self/mountinfo、blkid、systemctl show、备份目录。
#
# 存在的理由：面板自己说“挂上了、fstab 写对了、两源核验一致”只是**它的说法**。
# 交付前要能用一条不经过面板的命令把同一批事实再读一遍——两边对不上，就是有一边在说假话。
#
# 只读。它不 mount、不 umount、不写 fstab、不调 daemon-reload；唯一会动的只有终端输出。
#
# 用法：
#   sudo scripts/storage-external-check.sh                      # 检查全部托管条目
#   sudo scripts/storage-external-check.sh --expect /srv/upanel/volumes/data=mounted
#   sudo scripts/storage-external-check.sh --fstab /tmp/fixture.fstab   # 对着夹具检查（不出网）
#
# 退出码：0 = 全部一致；1 = 有对不上的地方；2 = 环境不满足（缺工具/文件读不到/用法错误）。

set -u

FSTAB=/etc/fstab
BACKUP_DIR=/var/lib/upanel-agent/fstab-backups
VOLUME_ROOT=/srv/upanel/volumes
BEGIN_MARKER='# BEGIN upanel managed volumes'
END_MARKER='# END upanel managed volumes'

EXPECT_LIST=''
FAILS=0
CHECKS=0
SKIPS=0

usage() {
    cat <<'USAGE'
用法: storage-external-check.sh [选项]

选项:
  --fstab PATH              要检查的 fstab 文件（默认 /etc/fstab）
  --backup-dir PATH         fstab 备份目录（默认 /var/lib/upanel-agent/fstab-backups）
  --expect MP=mounted       断言某个挂载点当前是挂着的（可重复）
  --expect MP=unmounted     断言某个挂载点当前没挂
  -h, --help                这份说明

退出码: 0 一致 / 1 有对不上的地方 / 2 环境不满足
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --fstab) FSTAB=${2:-}; shift 2 ;;
        --backup-dir) BACKUP_DIR=${2:-}; shift 2 ;;
        --expect)
            if [ $# -lt 2 ] || [ -z "${2:-}" ]; then
                printf 'FAIL 用法: --expect 需要 MP=mounted 或 MP=unmounted\n'
                exit 2
            fi
            EXPECT_LIST="$EXPECT_LIST $2"
            shift 2
            ;;
        -h|--help) usage; exit 0 ;;
        *) printf 'FAIL 用法: 不认识的参数 %s\n' "$1"; usage >&2; exit 2 ;;
    esac
done

fail() {
    FAILS=$((FAILS + 1))
    printf 'FAIL %s\n' "$*"
}

pass() {
    printf 'PASS %s\n' "$*"
}

skip() {
    SKIPS=$((SKIPS + 1))
    printf 'SKIP %s\n' "$*"
}

info() {
    printf 'INFO %s\n' "$*"
}

# 每条断言都要真的跑过一次；counted 只用来看统计行是不是在自欺欺人。
check() {
    CHECKS=$((CHECKS + 1))
}

# 系统盘上的 /etc/fstab 通常 644，但用 sudo 跑也没问题；读不到就只能退出。
if [ ! -r "$FSTAB" ]; then
    printf 'FAIL 环境: 读不到 %s（用 sudo 或换 --fstab）\n' "$FSTAB"
    exit 2
fi

for tool in awk sed findmnt; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        printf 'FAIL 环境: 缺少必需命令 %s\n' "$tool"
        exit 2
    fi
done

printf '== 存储外部独立检查 ==\n'
info "fstab: $FSTAB"
info "时间: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------- 托管块

BEGIN_COUNT=$(grep -c -x -F "$BEGIN_MARKER" "$FSTAB" || true)
END_COUNT=$(grep -c -x -F "$END_MARKER" "$FSTAB" || true)
info "托管块标记: BEGIN=$BEGIN_COUNT END=$END_COUNT"

check
if [ "$BEGIN_COUNT" -eq 0 ] && [ "$END_COUNT" -eq 0 ]; then
    pass "这个文件里没有面板托管块（一个卷都没纳管，或托管块已被全部退出管理）"
elif [ "$BEGIN_COUNT" -ne "$END_COUNT" ]; then
    fail "托管块的 BEGIN/END 不配对（BEGIN=$BEGIN_COUNT END=$END_COUNT）：有人手工动过，面板会拒绝写"
elif [ "$BEGIN_COUNT" -ne 1 ]; then
    fail "托管块出现 $BEGIN_COUNT 次：面板只认唯一一块，多出来的说明有历史残留"
else
    pass "托管块结构完整（是唯一一块，BEGIN/END 配对）"
fi

# 托管块先落到临时文件：用 `while ... done < file` 而不是管道，
# 这样循环里的 FAILS/CHECKS 计数不会被 subshell 吞掉（用管道时汇总行会说谎）。
BLOCK_TMP=$(mktemp "${TMPDIR:-/tmp}/upanel-fstab-block.XXXXXX") || {
    printf 'FAIL 环境: 无法创建临时文件（TMPDIR=%s）
' "${TMPDIR:-/tmp}"
    exit 2
}
trap 'rm -f "$BLOCK_TMP" "$BLOCK_TMP.entries" "$BLOCK_TMP.bad"' EXIT INT TERM HUP
awk -v b="$BEGIN_MARKER" -v e="$END_MARKER" '
    $0 == b {inside = 1; next}
    $0 == e {inside = 0; next}
    inside {print}
' "$FSTAB" > "$BLOCK_TMP"
BLOCK=$(cat "$BLOCK_TMP")

ENTRY_COUNT=$(printf '%s\n' "$BLOCK" | grep -v '^[[:space:]]*$' | grep -v '^[[:space:]]*#' | wc -l | tr -d ' ')
info "托管块内条目: $ENTRY_COUNT"

# 块里的每个字段都要能解析：UUID=、挂载点、类型、选项、dump、pass。
BAD_LINES=$(printf '%s\n' "$BLOCK" | grep -v '^[[:space:]]*$' | grep -v '^[[:space:]]*#' | awk '
    NF != 6 {print; next}
    $1 !~ /^UUID=/ {print; next}
    $2 !~ /^\// {print; next}
    $5 != "0" {print; next}
')
check
if [ -z "$BAD_LINES" ]; then
    pass "托管块内每一行都是 6 列、UUID= 作首列、挂载点是绝对路径、dump 列为 0"
else
    # 逐行打出来，让人能直接照着改；计数从文件里数，不从 subshell 里数。
    printf '%s\n' "$BAD_LINES" > "$BLOCK_TMP.bad"
    bad_count=0
    while IFS= read -r bad; do
        [ -z "$bad" ] && continue
        fail "托管块里有一行不符合形状: $bad"
        bad_count=$((bad_count + 1))
    done < "$BLOCK_TMP.bad"
    info "共有 $bad_count 行不符合形状"
fi

# 块外的内容不该被面板碰过；能证明的只有“标记之外没有重复条目”这条弱形式。
DUPLICATE=$(printf '%s\n' "$BLOCK" | grep -v '^[[:space:]]*$' | grep -v '^[[:space:]]*#' | \
    awk '{print $2}' | sort | uniq -d)
check
if [ -z "$DUPLICATE" ]; then
    pass "托管块内没有重复的挂载点"
else
    fail "托管块内出现重复挂载点: $(printf '%s' "$DUPLICATE" | tr '\n' ' ')"
fi

# ---------------------------------------------------------------- 逐条核对

grep -v '^[[:space:]]*$' "$BLOCK_TMP" | grep -v '^[[:space:]]*#' > "$BLOCK_TMP.entries"
while IFS=' ' read -r uuid_field mountpoint fstype options dump pass; do
    [ -z "${uuid_field:-}" ] && continue
    uuid=${uuid_field#UUID=}

    # 0) UUID 的形状。面板只写标准 UUID 形态（blkid 出来的那种），
    #    形状不对说明这行不是它写的、或者被人手工改过。
    check
    if printf '%s' "$uuid" | grep -Eq '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'; then
        pass "$mountpoint: UUID 形状合法"
    else
        fail "$mountpoint: UUID 形状不对（$uuid）——面板只写标准 UUID 形态，这行可能被手工改过"
    fi

    check
    case "$mountpoint" in
        "$VOLUME_ROOT"/*) pass "挂载点在面板的卷根下: $mountpoint" ;;
        *) info "挂载点不在 $VOLUME_ROOT 下（不是面板生成的行，或者旧版本遗留）: $mountpoint" ;;
    esac

    # 1) fstab 里的 UUID 在系统里真的有一块对应的盘（blkid 是独立证人：
    #    产品按契约不用 blkid，所以它不会跟着产品一起错）。
    if command -v blkid >/dev/null 2>&1; then
        device=$(blkid -U "$uuid" 2>/dev/null || true)
        check
        if [ -n "$device" ]; then
            pass "$mountpoint: UUID $uuid 现在是 $device"
            real_fstype=$(blkid -p -s TYPE -o value "$device" 2>/dev/null || true)
            check
            if [ "$real_fstype" = "$fstype" ]; then
                pass "$mountpoint: 盘上的文件系统类型是 $fstype（与 fstab 一致）"
            else
                fail "$mountpoint: fstab 写的是 $fstype，盘上实际是 ${real_fstype:-读不到}（用 blkid -p 直读超级块）"
            fi
        else
            fail "$mountpoint: 系统里找不到 UUID=$uuid 的设备（盘没插？分区被删了？）"
        fi
    else
        skip "$mountpoint: 没有 blkid，无法独立确认 UUID 对应的设备"
    fi

    # 2) 现在到底挂没挂：findmnt 与 /proc/self/mountinfo 两路各读一遍。
    #
    # 只能用 `findmnt --mountpoint`（问“这个路径本身是不是挂载点”，不是挂载点就 rc!=0），
    # **不能用 `findmnt --target`**：后者问的是“哪个挂载覆盖了这个路径”，任何存在的路径
    # 都会命中上一层（未挂载的 /srv/upanel/volumes/logs 会返回 /dev/sda1），于是每个
    # 没挂的条目都会被读成“挂着”，再拿它跟 UUID 的设备比就是一片假的 FAIL。P07.5 在真机上
    # 撞到的就是这个（Q204）。
    mounted_source=$(findmnt -n -o SOURCE --mountpoint "$mountpoint" 2>/dev/null || true)
    check
    if [ -n "$mounted_source" ]; then
        pass "$mountpoint: 现在挂着（findmnt --mountpoint source=$mounted_source）"
        # 第二路证人：`--target` 与 `--mountpoint` 必须落到同一个挂载点上
        # （TARGET 恰好等于这个挂载点本身），否则“挂着”这个说法只有一个来源。
        target_of=$(findmnt -n -o TARGET --target "$mountpoint" 2>/dev/null || true)
        check
        if [ "$target_of" = "$mountpoint" ]; then
            pass "$mountpoint: --target 也指向它自己（两路读数一致）"
        else
            fail "$mountpoint: --mountpoint 说有挂载，但 --target 落到 ${target_of:-空}——两路读数打架"
        fi
        # 第三路证人：内核自己的挂载表（第 5 列是挂载点，八进制转义过）。
        check
        if awk -v mp="$mountpoint" '{ p = $5; gsub(/\\040/, " ", p); if (p == mp) found = 1 } END { exit found ? 0 : 1 }' /proc/self/mountinfo 2>/dev/null; then
            pass "$mountpoint: 内核挂载表 /proc/self/mountinfo 里有它"
        else
            fail "$mountpoint: findmnt 说有挂载，但 /proc/self/mountinfo 里找不到这个挂载点"
        fi
        case "$options" in
            *noauto*) info "$mountpoint: 条目带 noauto（开机不自动挂，现在挂着只能是手工或面板挂的）" ;;
        esac
        want_dev=$(blkid -U "$uuid" 2>/dev/null || true)
        if [ -n "$want_dev" ]; then
            check
            if [ "$mounted_source" = "$want_dev" ]; then
                pass "$mountpoint: 挂载来源与 UUID 对得上（$want_dev）"
            else
                fail "$mountpoint: 挂载来源是 $mounted_source，但 UUID=$uuid 现在指向 ${want_dev:-未知}"
            fi
        fi
        inode=$(stat -c %d "$mountpoint" 2>/dev/null || echo '')
        parent_inode=$(stat -c %d "$(dirname "$mountpoint")" 2>/dev/null || echo '')
        check
        if [ -n "$inode" ] && [ "$inode" != "$parent_inode" ]; then
            pass "$mountpoint: 它是一个真正的挂载点（设备号与父目录不同）"
        else
            fail "$mountpoint: findmnt 说有挂载，但它的设备号与父目录相同——这是普通目录，不是挂载点"
        fi
    else
        info "$mountpoint: 现在没有挂载"
        check
        if [ -d "$mountpoint" ]; then
            pass "$mountpoint: 挂载点目录存在"
        else
            fail "$mountpoint: 挂载点目录不存在——开机挂载会失败"
        fi
    fi

    # 3) fstab 的选项里必须有 nofail：一块数据盘不在，不该让这台机器起不来。
    check
    case "$options" in
        *nofail*) pass "$mountpoint: 选项里有 nofail（盘不在也不阻塞开机）" ;;
        *) fail "$mountpoint: 选项里没有 nofail（$options）——盘不在时可能卡住或进不了系统" ;;
    esac

    # 4) sixth 列的 fsck 顺序：ext4 用 2，XFS 用 0（XFS 每次开机跑 fsck 只会更糟）。
    check
    case "$fstype" in
        ext4)
            if [ "$pass" = "2" ]; then
                pass "$mountpoint: ext4 的 fsck 顺序是 2"
            else
                fail "$mountpoint: ext4 的 fsck 顺序是 $pass（应为 2）"
            fi
            ;;
        xfs)
            if [ "$pass" = "0" ]; then
                pass "$mountpoint: XFS 的 fsck 顺序是 0"
            else
                fail "$mountpoint: XFS 的 fsck 顺序是 $pass（应为 0，XFS 自己有日志恢复）"
            fi
            ;;
        *)
            info "$mountpoint: 文件系统类型 $fstype 不在面板的支持范围（ext4/xfs），跳过 fsck 顺序断言"
            ;;
    esac

    # 5) systemd 那边有没有这个挂载单元（开机自动挂载靠的是它）。
    if command -v systemctl >/dev/null 2>&1 && command -v systemd-escape >/dev/null 2>&1; then
        unit=$(systemd-escape -p --suffix=mount "$mountpoint" 2>/dev/null || true)
        if [ -n "$unit" ]; then
            state=$(systemctl show -p LoadState --value "$unit" 2>/dev/null || true)
            check
            case "$state" in
                loaded) pass "$mountpoint: systemd 里有 $unit（LoadState=loaded）" ;;
                '') skip "$mountpoint: 读不到 $unit 的 LoadState（不在 systemd 环境里？）" ;;
                *)
                    case "$options" in
                        *noauto*) info "$mountpoint: $unit 的 LoadState=$state（条目带 noauto，这不矛盾）" ;;
                        *) fail "$mountpoint: fstab 里有它，但 systemd 认为 $unit 是 $state" ;;
                    esac
                    ;;
            esac
        fi
    else
        skip "$mountpoint: 没有 systemctl/systemd-escape，无法确认挂载单元"
    fi
done < "$BLOCK_TMP.entries"

# ---------------------------------------------------------------- 期望状态

for item in $EXPECT_LIST; do
    mountpoint=${item%=*}
    want=${item##*=}
    case "$want" in
        mounted|unmounted) ;;
        *) fail "--expect 的值只能是 mounted 或 unmounted（收到 $want）"; continue ;;
    esac
    check
    actual=unmounted
    # 与逐条核对里的判据保持一致：`--mountpoint` 才回答“挂了没有”，
    # `--target` 对任何存在的路径都命中上一层的挂载。
    if [ -n "$(findmnt -n -o SOURCE --mountpoint "$mountpoint" 2>/dev/null || true)" ]; then
        actual=mounted
    fi
    if [ "$actual" = "$want" ]; then
        pass "$mountpoint: 期望 $want，实际 $actual"
    else
        fail "$mountpoint: 期望 $want，实际 $actual"
    fi
done

# ---------------------------------------------------------------- 备份

if [ -d "$BACKUP_DIR" ]; then
    count=$(find "$BACKUP_DIR" -maxdepth 1 -type f -name '*.fstab' 2>/dev/null | wc -l | tr -d ' ')
    info "fstab 备份目录: $BACKUP_DIR（$count 份备份）"
    newest=$(find "$BACKUP_DIR" -maxdepth 1 -type f -name '*.fstab' 2>/dev/null | sort | tail -1)
    if [ -n "$newest" ]; then
        check
        mode=$(stat -c %a "$newest" 2>/dev/null || echo '')
        if [ "$mode" = "600" ]; then
            pass "最新的备份权限是 0600（$(basename "$newest")）"
        else
            fail "最新的备份权限是 ${mode:-读不到}，应为 0600（$(basename "$newest")——里面有整份 fstab）"
        fi
        check
        if grep -q -x -F "$BEGIN_MARKER" "$newest" 2>/dev/null; then
            info "最新备份里已经有托管块（说明它是块写入之后的状态，回滚它会保住那些条目）"
        else
            info "最新备份里还没有托管块（说明它是第一次写入之前的状态）"
        fi
    else
        skip "备份目录存在但没有 .fstab 备份文件"
    fi
else
    skip "没有备份目录 $BACKUP_DIR（面板从没写过 fstab，或者数据目录不在默认位置）"
fi

# ---------------------------------------------------------------- 汇总

printf '== 汇总 ==\n'
info "断言执行 $CHECKS 项，跳过 $SKIPS 项"
if [ "$FAILS" -eq 0 ]; then
    printf 'RESULT 一致：这台机器的 fstab 托管块、挂载事实与设备身份能互相对上\n'
    exit 0
fi
printf 'RESULT 有 %d 处对不上：先别重启、别重复提交，按上面的 FAIL 行逐条查（见 docs/STORAGE_RECOVERY.md）\n' "$FAILS"
exit 1
