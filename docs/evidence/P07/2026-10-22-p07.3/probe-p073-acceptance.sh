#!/bin/sh
# P07.3 验收脚本设计所需的两个未知量（只动 sdc，一次性盘）：
#   1. 什么样的**合法预检目标**会让 mkfs 以非 0 退出（“错误退出”验收用例）；
#   2. 用忙轮询 SIGSTOP 能不能稳定抓住 mkfs（“禁止取消”与“进程中断”用例的窗口）。
set -u
DISK=${DISK:-/dev/sdc}
LOG=/tmp/probe-p073-accept.log
exec >"$LOG" 2>&1
echo "===== 0. 环境 ====="
mkfs.ext4 -V 2>&1 | head -1
mkfs.xfs -V 2>&1 | head -1
blkid -V
echo
echo "===== 1. 清盘并建 GPT + 三个分区（32M / 64M / 2G） ====="
wipefs -a "$DISK"
sgdisk --zap-all "$DISK" >/dev/null 2>&1
sgdisk --new=1:2048:+32M --new=2:0:+64M --new=3:0:+2G "$DISK" >/dev/null 2>&1
partprobe "$DISK" 2>/dev/null
udevadm settle --timeout=5
lsblk -o NAME,SIZE,TYPE "$DISK"
echo
echo "===== 2. 32 MiB 分区上 mkfs.xfs（合法目标、容量不足）====="
echo "--- mkfs.xfs ${DISK}1"
mkfs.xfs -f "${DISK}1" >/tmp/x.out 2>&1
echo "  rc=$?"
sed -n '1,6p' /tmp/x.out | sed 's/^/  | /'
echo
echo "===== 3. 64 MiB 分区上 mkfs.xfs ====="
mkfs.xfs -f -L probe "${DISK}2" >/tmp/x2.out 2>&1
echo "  rc=$?"
sed -n '1,4p' /tmp/x2.out | sed 's/^/  | /'
echo "  blkid: $(blkid -o export "${DISK}2" | tr '\n' ' ')"
echo
echo "===== 4. 32 MiB 分区上 mkfs.ext4（对照：同样小，ext4 能行）====="
mkfs.ext4 -q -L tiny "${DISK}1" >/tmp/e.out 2>&1
echo "  rc=$?"
echo "  blkid:"
blkid -o export "${DISK}1" | sed 's/^/  | /'
echo
echo "===== 5. 2 GiB 分区上 mkfs.xfs 的耗时（三次）====="
for i in 1 2 3; do
  t0=$(date +%s%N)
  mkfs.xfs -f "${DISK}3" >/dev/null 2>&1
  rc=$?
  t1=$(date +%s%N)
  echo "  #$i rc=$rc elapsed_ms=$(( (t1 - t0) / 1000000 ))"
done
echo
echo "===== 6. 忙轮询能不能抓住 mkfs（三次）====="
grab() {
  # 后台起 mkfs，前台用最小开销的循环反复看 /proc。
  mkfs.xfs -f "${DISK}3" >/dev/null 2>&1 &
  target=$!
  caught=0
  tries=0
  while kill -0 "$target" 2>/dev/null; do
    tries=$((tries + 1))
    # mkfs 会 fork 出子进程干活；SIGSTOP 目标进程本身即可（它在等子进程）。
    if kill -STOP "$target" 2>/dev/null; then
      caught=$((caught + 1))
      sleep 0.3
      kill -CONT "$target" 2>/dev/null
      # 只记第一次成功的时间点，然后继续跑完
      [ "$caught" -eq 1 ] && echo "  caught at try=$tries"
      break
    fi
  done
  wait "$target"
  echo "  mkfs rc=$? tries=$tries caught=$caught"
}
grab
grab
echo
echo "===== 7. 收尾 ====="
wipefs -a "$DISK" >/dev/null 2>&1
sgdisk --zap-all "$DISK" >/dev/null 2>&1
partprobe "$DISK" 2>/dev/null
udevadm settle --timeout=5
lsblk -o NAME,SIZE,TYPE "$DISK"
echo "PROBE_DONE"
