#!/bin/sh
# 全量 -race 之后的补跑。全部以 root，源码是同一份 /tmp/upanel-src
# （已打上本轮三处测试修正：scriptaction_test.go 的轮询锁、journal_test.go 的轮询锁、
#   retention_test.go 的真实目录描述符、以及 ctl 的 Q517）。
#
# 为什么要补跑：
#   1) internal/agent / internal/api / internal/auth 三个包在全量那一轮里都撞到 -timeout=90m
#      （5403s / 5407s / 5405s），**都不是死锁**：panic 栈显示超时那一刻分别在跑
#      TestFileActionsRejectUnknownFields（50s）、TestAppLogsSystemdRejectsContainerParams（3m29s）、
#      TestLoginIssueAndAuthenticate（35s），三条都在正常推进。原因有两个：
#      2 个 TCG vCPU 太慢；以及 -race 把 Argon2id 单次校验放大到 **91 秒**（不带 -race 是 0.6 秒），
#      而 auth 包里的登录类用例每条都要做一次校验。
#      补跑：三个包**并行**、-timeout=0，让它们把 2 条 vCPU 吃满。
#   2) cmd/upanelctl / internal/journal / internal/backup —— 全量那一轮里它们红在**测试自身**
#      的缺陷上（Q517 / Q518 / 轮询数据竞争），已修，补跑读的是改正后的源码。
#   3) 最后一组是判据实验：backup 的 SSH 重试用例带 -race 与不带 -race 各跑一次，
#      用来判断那条红是"检测器把 600 毫秒握手预算吃掉"还是"重试逻辑本身有问题"。
set -u
cd /tmp/upanel-src-fix || exit 2
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=1
export GOMODCACHE=/tmp/gomodcache
export GOCACHE=/tmp/gocache

one() {
	name=$1
	shift
	body=/tmp/race-re-$name.body.log
	echo "== [$name] 开始 $(date -u +%FT%TZ) $* =="
	go test "$@" >"$body" 2>&1
	rc=$?
	echo "== [$name] 结束 $(date -u +%FT%TZ) rc=$rc =="
	tail -30 "$body"
	echo -n "== [$name] WARNING: DATA RACE 条数: "; grep -c 'WARNING: DATA RACE' "$body"
	echo -n "== [$name] FAIL 用例数: "; grep -cE '^--- FAIL' "$body"
	echo -n "== [$name] 包级结果: "; grep -E "^(ok|FAIL)\s" "$body" | tr '\n' ' '; echo
}

# 三个大包并行（包内用例本来就串行，并行只是把两条 vCPU 用满）
one agent -race -count=1 -timeout=0 ./internal/agent &
one api   -race -count=1 -timeout=0 ./internal/api &
one auth  -race -count=1 -timeout=0 ./internal/auth &
wait

# 三个红在测试自身缺陷上的包
one ctl     -race -count=1 -timeout=0 ./cmd/upanelctl
one journal -race -count=1 -timeout=0 ./internal/journal
one backup  -race -count=1 -timeout=0 ./internal/backup
one storage -race -count=1 -timeout=0 ./internal/storage

# 判据实验：storage 那条"隔离期内不许等"的 5 毫秒墙钟上界，带 -race 连跑 5 次看抖动
one cap5race -race -count=5 -run 'TestOSCapacityReaderNetworkTimeoutQuarantines' -v ./internal/storage/
one cap5norace -count=5 -run 'TestOSCapacityReaderNetworkTimeoutQuarantines' -v ./internal/storage/

# 判据实验：SSH 重试用例，带 -race / 不带 -race
one sshrace -race -count=1 -run 'TestDialRetriesOnceWhenHandshakeStalls' -v ./internal/backup/
one sshnorace -count=1 -run 'TestDialRetriesOnceWhenHandshakeStalls' -v ./internal/backup/

# 判据实验：计时用例，不带 -race（同一份源码、同一台机器）
one timing -count=1 -run 'TestDefaultPasswordParamsTiming' -v ./internal/auth/

echo "== 全部补跑完成 $(date -u +%FT%TZ) =="
