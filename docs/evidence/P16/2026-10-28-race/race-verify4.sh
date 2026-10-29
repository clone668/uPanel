#!/bin/sh
# 第五轮：把"三包并行挤爆 2 条 vCPU"这个因**人为造出来**，看两条红能不能复现。
#
# 为什么要这一轮：第四轮已经证明"隔离下 3/3 全过"，但那只说明它不常在，不说明它为什么红。
# 如果同样是这两条测试、同样这份源码、只是旁边多了四个占满 CPU 的忙循环 —— 那就把
# "机器被挤爆 → 本机 TLS 握手没能在预算内完成 / 预检那次读库超过 1 秒"从推测变成演示。
#
# 对照组就是第四轮 A 段（同样的命令、同样的 -count=3，没有忙循环）：3/3 PASS。
set -u
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=1
export GOMODCACHE=/tmp/gomodcache
export GOCACHE=/tmp/gocache
cd /tmp/upanel-src-fix || exit 2

nproc
echo "== 造负载：4 个忙循环打满 2 条 vCPU $(date -u +%FT%TZ) =="
i=0
while [ $i -lt 4 ]; do
	sh -c 'while :; do :; done' &
	i=$((i + 1))
done
sleep 5
uptime

echo "==================== 带负载：两条测试各跑一次（-race，-count=3）========================="
echo "== [loaded-webhook] 开始 $(date -u +%FT%TZ) =="
go test -race -count=3 -timeout=0 -run '^TestWebhookSelfSignedCertIsRejected$' -v ./internal/agent/ 2>&1 | tail -20
echo "== [loaded-expired] 开始 $(date -u +%FT%TZ) =="
go test -race -count=3 -timeout=0 -run '^TestOperationExpiredPreflightRejected$' -v ./internal/agent/ 2>&1 | tail -30

echo "== 撤负载 $(date -u +%FT%TZ) =="
# 只杀我们自己起的忙循环：命令行恰好是 `sh -c while :; do :; done`
pkill -f "while :; do :; done" 2>/dev/null
sleep 3
uptime
echo "== 第五轮完成 $(date -u +%FT%TZ) =="
