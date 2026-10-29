#!/bin/sh
# 第二轮：**以 root** 跑 -race。
# 为什么必须 root：internal/agentstore 打开代理数据库前会校验数据目录属主必须是 root，
# 用普通用户跑，agentstore / agent / cmd/upanel-agent / cmd/upanelctl 里几十个用例
# 全在 "打开代理数据库失败" 上红（第一轮就是这样，见 race-run1-user.log）。
# -timeout=90m：go test 每包默认 10 分钟，P04.3 在 KVM 上 internal/api 的 -race 已用 273 秒，
# 这台是 2 个 TCG vCPU，不放大就会撞成 "panic: test timed out"。
set -u
cd /tmp/upanel-src || exit 2
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=1
export GOMODCACHE=/tmp/gomodcache
export GOCACHE=/tmp/gocache
BODY=/tmp/race-root.body.log

echo "== 身份 =="; id
echo "== go version =="; go version
echo "== 包数 =="; go list ./... | wc -l
echo "== 开始 $(date -u +%FT%TZ) =="
go test -race -count=1 -timeout=90m ./... >"$BODY" 2>&1
rc=$?
cat "$BODY"
echo "== 结束 $(date -u +%FT%TZ) rc=$rc =="
echo -n "== ok 包数: "; grep -cE '^ok ' "$BODY"
echo -n "== FAIL 包数: "; grep -cE '^FAIL\s' "$BODY"
echo -n "== 无测试文件包数: "; grep -cE '^\?' "$BODY"
echo -n "== WARNING: DATA RACE 条数: "; grep -c 'WARNING: DATA RACE' "$BODY"
echo -n "== 单测超时条数: "; grep -c 'test timed out' "$BODY"
