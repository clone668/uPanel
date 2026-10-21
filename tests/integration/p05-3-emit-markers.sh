#!/bin/sh
# p05-3-emit-markers.sh <单元名> [条数] [标识符]
#
# 在一个瞬时单元里写入一组可定位的日志，并**回读自证**它们确实落在该单元下。
#
# 为什么不是一句句 logger：
#   实测（systemd 255）用 `logger` 在循环里连写 30 条，journald 有 3 条记不住发送者
#   的 cgroup —— 条目进了 journal，但 _SYSTEMD_UNIT 是空的，于是 `journalctl -u <unit>`
#   查不到它们。验收里表现为“30 个标记各出现一次”这条断言随机失败。
#   换成一个常驻的 systemd-cat 进程（一次写多行、进程在整个写入期间都活着）后归属
#   稳定；这里再加一层“回读 + 只补写确实缺的那几条”，把这件事做到确定。
#
# 只补写缺的，不整体重写：整体重写会在 journal 里留下重复条目，而“每个标记恰好出现
# 一次”正是跨页翻页要证明的事。
set -u

unit=${1:?需要单元名}
count=${2:-30}
ident=${3:-upanel-accept}

# 只看这个单元最近十分钟的消息正文。--since= 用等号形式，避免被当成选项。
scan() {
  journalctl -u "$unit" --since=-10min -o cat --no-pager 2>/dev/null
}

has_line() { # has_line <快照> <整行>
  printf '%s\n' "$1" | grep -qxF "$2"
}

emit_line() { # emit_line <正文> [级别]
  msg=$1
  prio=${2:-}
  if [ -n "$prio" ]; then
    printf '%s\n' "$msg" | systemd-cat -t "$ident" -p "$prio"
  else
    printf '%s\n' "$msg" | systemd-cat -t "$ident"
  fi
}

missing_markers() { # missing_markers <快照>
  i=1
  while [ "$i" -le "$count" ]; do
    has_line "$1" "marker-i=$i" || printf 'marker-i=%s\n' "$i"
    i=$((i + 1))
  done
}

ERR_LINE="marker-err-1 拒绝连接"
PASS_LINE="password=hunter2secret 登录失败"
TOKEN_LINE="token=abcdef123456 已下发"

attempt=1
while [ "$attempt" -le 5 ]; do
  snapshot=$(scan)

  pending=$(missing_markers "$snapshot")
  if [ -n "$pending" ]; then
    # 一个进程一次写完缺的那几条：归属稳定，也不会写出重复条目以外的噪声。
    printf '%s\n' "$pending" | systemd-cat -t "$ident"
  fi
  has_line "$snapshot" "$ERR_LINE" || emit_line "$ERR_LINE" err
  has_line "$snapshot" "$PASS_LINE" || emit_line "$PASS_LINE" warning
  has_line "$snapshot" "$TOKEN_LINE" || emit_line "$TOKEN_LINE"

  # journald 是异步落盘的：写完立刻回读会看不到刚写的条目，所以等一下再数。
  sleep 0.3
  snapshot=$(scan)
  if [ -z "$(missing_markers "$snapshot")" ] &&
    has_line "$snapshot" "$ERR_LINE" &&
    has_line "$snapshot" "$PASS_LINE" &&
    has_line "$snapshot" "$TOKEN_LINE"; then
    exit 0
  fi
  attempt=$((attempt + 1))
done

# 写不下去就明说：验收脚本会在 emit.log 里看到这一行，而不是一条莫名其妙的失败断言。
echo "emit-markers: 重试 $((attempt - 1)) 次后仍有条目没进 journald" >&2
exit 1
