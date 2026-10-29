#!/bin/sh
# 第六轮：不赌偶发，把"采样 now 与读库之间被拖住 1.5 秒"这个条件焊进探针，验机理。
#
# 第五轮的教训：加 4 个忙循环之后 webhook 那条 1/3 复现了，而 expired 那条 3/3 全过
# （甚至慢了 4 倍、单次 59—68 秒），所以"读库超过 1 秒"这个假设**没有被复现支持**。
# 于是改成确定性实验：只改测试文件里的 mutatingPlanStore.Get，在它读 inner 之前先睡 1500ms，
# 精确制造 `now`（operations.go:171）与夹具改写一起发生（Get 内部）之间的 1.5 秒间隔。
#
# 预期（这是判据，跑之前先写下来）：
#   A 组（夹具 -time.Second，余量 1 秒） + 睡 1.5 秒  → **必须红**，且红成 201
#   B 组（夹具 -time.Minute，余量 60 秒）+ 睡 1.5 秒  → **必须绿**
# 如果 A 组绿了，说明机理不成立，本轮结论就是"没能定性"。
# 如果 A 红 B 绿，机理成立：夹具的 1 秒余量确实会随读库延迟翻转，而 60 秒余量不会。
set -u
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=1
export GOMODCACHE=/tmp/gomodcache
export GOCACHE=/tmp/gocache

probe() {
	dir=$1
	sed -i 's|^func (m \*mutatingPlanStore) Get(ctx context.Context, id string) (agentstore.PreflightPlan, error) {|&\n\ttime.Sleep(1500 * time.Millisecond) // 探针：模拟采样 now 与读库之间被抢占 1.5 秒|' \
		"$dir/internal/agent/operations_test.go"
	echo "-- $dir 里插入的那一行："
	grep -n "探针：模拟采样" "$dir/internal/agent/operations_test.go"
	grep -n "p.ExpiresAt = time.Now().UTC().Add(" "$dir/internal/agent/operations_test.go"
}

echo "==================== A 组：夹具 -time.Second（1 秒余量） + 1.5 秒延迟 ===================="
probe /tmp/upanel-src-fix
( cd /tmp/upanel-src-fix && go test -race -count=1 -timeout=0 \
	-run '^TestOperationExpiredPreflightRejected$' -v ./internal/agent/ ) 2>&1 | tail -20

echo "==================== B 组：夹具 -time.Minute（60 秒余量） + 1.5 秒延迟 ===================="
probe /tmp/upanel-src-probe
( cd /tmp/upanel-src-probe && go test -race -count=1 -timeout=0 \
	-run '^TestOperationExpiredPreflightRejected$' -v ./internal/agent/ ) 2>&1 | tail -20

echo "== 第六轮完成 $(date -u +%FT%TZ) =="
