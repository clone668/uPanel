#!/bin/sh
# P11.2 验收的启动器：只做一件事——把脚本自己的退出码如实写进日志末尾。
# 上一版把 `$?` 写成了字面量 `\0`，于是"FAIL 1"的日志末尾摆着一句 exit=0，
# 那是假的。退出码必须由这里捕获，不能靠读日志的人猜。
{
  BIN_DIR=/var/tmp/upanel-bin sh /var/tmp/p11-2-package-write-acceptance.sh
  echo "exit=$?"
} >>/root/p11-2-run.log 2>&1
