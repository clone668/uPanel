#!/bin/sh
# 第四轮：把补跑里 agent 包那两条红**单独**跑，判它们是不是"三包并行把 2 条 vCPU 挤爆"造成的。
#
# 背景：补跑（race-rerun.sh）把 agent/api/auth 三个包并行放在 2 条 TCG vCPU 上，
# agent 包红两条：
#   ① TestWebhookSelfSignedCertIsRejected   —— 期望"自签证书 → 不可重试"，实得 TLS handshake timeout（可重试）
#   ② TestOperationExpiredPreflightRejected —— 期望 409 PREFLIGHT_EXPIRED，实得 201（预检被当成没过期）
#
# 第 ① 条的判据：本机 httptest 起一个 TLS 服务端、同一台机器上 POST 过去，握手本该在毫秒级完成；
#   拿到 handshake timeout 只可能是服务端 goroutine 根本没被调度到。**单独跑如果绿，就是并行挤爆。**
#
# 第 ② 条的判据要更小心。读 internal/agent/operations.go:171/175——`now` 是在**读库之前**采样的：
#       now := s.nowUTC().Truncate(time.Second)      // 171
#       plan, err := s.opts.Plans.Get(...)          // 175
#   而测试的夹具是 `p.ExpiresAt = time.Now().UTC().Add(-time.Second)`，即在 Get 里按**读的那一刻**算过期。
#   于是只要这次 Get 的耗时超过 1 秒，`Get 时刻 - 1 秒` 就落到 `now` 之后，夹具想造的"已过期"
#   反而变成"还没到期"。→ 单独跑如果仍是红，那就不是并行的问题，而是**夹具的 1 秒余量太窄**。
#   为了不靠推理下结论，脚本里另做一棵探针树：只把那一行的 -time.Second 改成 -time.Minute
#   （断言一个字不改，仍然要求 409 PREFLIGHT_EXPIRED），同样条件再跑。绿 → 因果坐实。
set -u
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=1
export GOMODCACHE=/tmp/gomodcache
export GOCACHE=/tmp/gocache

run() {
	label=$1
	dir=$2
	shift 2
	echo "== [$label] 开始 $(date -u +%FT%TZ) (cwd=$dir) $* =="
	( cd "$dir" && go test "$@" ) 2>&1 | tail -25
	echo "== [$label] 结束 $(date -u +%FT%TZ) =="
}

echo "==================== A. 当前源码树，两条测试单独跑 ===================="
run webhook-isolated /tmp/upanel-src-fix -race -count=3 -timeout=0 \
	-run '^TestWebhookSelfSignedCertIsRejected$' -v ./internal/agent/
run expired-isolated /tmp/upanel-src-fix -race -count=3 -timeout=0 \
	-run '^TestOperationExpiredPreflightRejected$' -v ./internal/agent/

echo "==================== B. 探针树：只把夹具的 1 秒余量改成 60 秒 ===================="
rm -rf /tmp/upanel-src-probe
cp -a /tmp/upanel-src-fix /tmp/upanel-src-probe
sed -i 's|p.ExpiresAt = time.Now().UTC().Add(-time.Second)|p.ExpiresAt = time.Now().UTC().Add(-time.Minute)|' \
	/tmp/upanel-src-probe/internal/agent/operations_test.go
echo "探针树里的那一行现在是："
grep -n 'p.ExpiresAt = time.Now().UTC().Add(' /tmp/upanel-src-probe/internal/agent/operations_test.go
run expired-probe /tmp/upanel-src-probe -race -count=3 -timeout=0 \
	-run '^TestOperationExpiredPreflightRejected$' -v ./internal/agent/

echo "== 第四轮完成 $(date -u +%FT%TZ) =="
