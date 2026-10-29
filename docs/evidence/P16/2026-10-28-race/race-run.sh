#!/bin/sh
# 在验收 VM 里跑 -race 全量：只做竞态检测，不改产品代码。
# 记录：每包 ok/FAIL、总量、以及有没有 WARNING: DATA RACE。
set -u
cd /tmp/upanel-src || exit 2
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=1
export GOMODCACHE=/tmp/gomodcache
export GOCACHE=/tmp/gocache
echo "== go version =="; go version
echo "== go env =="; go env GOOS GOARCH CGO_ENABLED CC GOMODCACHE
echo "== 包数 =="; go list ./... | wc -l
echo "== 依赖预热 $(date -u +%FT%TZ) =="
go mod download all 2>&1 | tail -5
echo "== go test -race 开始 $(date -u +%FT%TZ) =="
go test -race -count=1 ./... 2>&1
rc=$?
echo "== go test -race 结束 $(date -u +%FT%TZ) rc=$rc =="
echo "== DATA RACE 计数 =="
