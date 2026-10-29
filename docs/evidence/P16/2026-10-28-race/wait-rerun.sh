#!/bin/sh
# 本机侧的看门狗：每 300 秒查一次 VM 上的 /tmp/race-rerun.log，
# 出现 "全部补跑完成" 就退出 0；超过 60 次（5 小时）退出 3。
# 只做轮询，不产生任何负载（ssh 取一行 tail）。
set -u
KEY=/h/Code/vms/upanel-2404/id_ed25519
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10 -i $KEY -p 2222 upanel@127.0.0.1"
i=0
while [ "$i" -lt 60 ]; do
	i=$((i + 1))
	# 注意：不要用 grep -c 的退出码判断——0 命中时 grep -c 会同时打印 "0" 并返回 1，
	# 一层 `|| echo 0` 会拼出 "0\n0"，于是第一版脚本在第 1 次就误报 FOUND-END。
	out=$($SSH 'if sudo -n grep -q "^== 全部补跑完成" /tmp/race-rerun.log 2>/dev/null; then echo FOUND; else echo NOTYET; fi' 2>/dev/null)
	last=$($SSH 'sudo -n grep "^==" /tmp/race-rerun.log | tail -1' 2>/dev/null)
	echo "tick $i/60 $(date -u +%FT%TZ) done=${out:-?} last=${last:-?}"
	if [ "${out:-NOTYET}" = "FOUND" ]; then
		echo "FOUND-END at tick $i"
		exit 0
	fi
	sleep 300
done
echo "LOOP-EXPIRED after 60 ticks"
exit 3
