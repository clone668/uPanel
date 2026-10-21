#!/usr/bin/env bash
# P04.4 前端页面冒烟（可在开发宿主运行，仅使用回环地址与临时数据库）。
#
# 覆盖：真实 upanel-web（mock 模式）托管 go:embed 的前端产物，浏览器里走一遍
#   未登录 → 登录页（/、/storage 都被挡）
#   登录 → 导航只有已实现模块（概览/存储/任务中心）
#   概览 → 面板服务 + 系统概况 + 存储摘要；代理不可用时必须显示“依赖未安装”，不得显示 0
#   存储 → 依赖未安装 + 错误码 AGENT_UNAVAILABLE，且不出现任何设备卡片
#   深链接 /storage/devices/sda → 不落到“页面不存在”
#   未知路径 → 404 页面
#
# 不做的事：不在这台机器上碰任何磁盘、不做分区/格式化/网络断连；
# mock 二进制没有 agent socket，所以“有设备可渲染”的那些断言会记为 SKIP ——
# 真实 Ubuntu 上的设备渲染验收见 tests/integration/ 下的脚本与 docs/PROGRESS.md 的未验证项。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 1

# mock 模式只能绑定 127.0.0.1:18881（internal/config/config.go 的 ListenMock），
# 显式换端口会被拒绝 —— 这是刻意的隔离约束，脚本不去绕过它。
# 所以本脚本与 tests/smoke/p02-1-auth-smoke.sh 不能同时运行。
PORT=18881
BASE="http://127.0.0.1:${PORT}"
PASSWORD="correct-horse-9"
WORKDIR="${UPANEL_SMOKE_DIR:-$(python -c 'import tempfile;print(tempfile.mkdtemp(prefix="upanel-p044-"))')}"
DB="${WORKDIR}/upanel.db"
WEB_PID=""

DIAG=0
for arg in "$@"; do
	case "$arg" in
		--diagnose) DIAG=1 ;;
	esac
done

cleanup() {
	if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
		kill "$WEB_PID" 2>/dev/null
		wait "$WEB_PID" 2>/dev/null
	fi
	# 诊断信息：屏幕冒烟失败时把关键日志打出来，便于定位。
	if [ "$DIAG" -eq 1 ]; then
		printf '\n== 诊断：web.log 末尾 40 行 ==\n'
		tail -n 40 "$WORKDIR/web.log" 2>/dev/null
	fi
}
trap cleanup EXIT

echo "== 0. 构建（前端 → internal/webui/dist → bin/upanel-web）=="
if [ -x bin/upanel-web ] && [ -x bin/upanelctl ] && [ "${UPANEL_SKIP_BUILD:-0}" = "1" ]; then
	echo "   复用已有 bin/（UPANEL_SKIP_BUILD=1）"
else
	sh scripts/build.sh || { echo "构建失败"; exit 1; }
fi
cp bin/upanel-web bin/upanelctl "$WORKDIR"/ || exit 1

echo "== 1. 初始化管理员 =="
INIT_OUT="$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$WORKDIR/upanelctl" init-admin --db "$DB" --json 2>"$WORKDIR/init.stderr")"
INIT_CODE=$?
if [ "$INIT_CODE" -ne 0 ]; then
	echo "init-admin 失败（exit $INIT_CODE）"; cat "$WORKDIR/init.stderr"; exit 1
fi
echo "   $INIT_OUT"

echo "== 2. 启动 mock 模式 Web 服务（127.0.0.1:${PORT}）=="
if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
	echo "错误: ${BASE} 上已有服务在应答（多半是上一轮遗留的 upanel-web）。先停掉它再跑，避免测到别人的进程。"
	exit 1
fi
"$WORKDIR/upanel-web" --mode mock --db "$DB" >"$WORKDIR/web.log" 2>&1 &
WEB_PID=$!
READY=""
for _ in $(seq 1 100); do
	if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then READY="yes"; break; fi
	if ! kill -0 "$WEB_PID" 2>/dev/null; then break; fi
	sleep 0.2
done
if [ -z "$READY" ]; then
	echo "Web 服务未就绪"
	tail -n 40 "$WORKDIR/web.log" 2>/dev/null
	exit 1
fi
echo "   健康检查: $(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"

echo "== 3. 记录存储接口的可信失败响应（没有 agent socket 时应为 503 AGENT_UNAVAILABLE）=="
# 先拿一个真会话，否则这四个接口只会回答 401，记录不到我们要的证据。
LOGIN_HEADERS="$WORKDIR/login.headers"
LOGIN_BODY="$WORKDIR/login.json"
LOGIN_CODE=$(curl -s -D "$LOGIN_HEADERS" -o "$LOGIN_BODY" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $BASE" \
	-d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
if [ "$LOGIN_CODE" != "200" ]; then
	echo "登录失败（HTTP $LOGIN_CODE）："; cat "$LOGIN_BODY"; exit 1
fi
COOKIE=$(grep -i '^set-cookie:' "$LOGIN_HEADERS" | tr -d '\r' | sed -e 's/^[Ss]et-[Cc]ookie: //' -e 's/;.*//')
for path in /api/v1/system/summary /api/v1/storage/topology /api/v1/storage/devices /api/v1/storage/devices/sda; do
	code=$(curl -s -o "$WORKDIR/resp.json" -w '%{http_code}' "$BASE$path" -H "Cookie: $COOKIE")
	printf '   %-31s %s %s\n' "$path" "$code" "$(cat "$WORKDIR/resp.json")"
done

echo "== 4. 浏览器页面冒烟（Playwright，登录后逐页检查）=="
cd "$ROOT/web" || exit 1
node scripts/smoke.mjs --base "$BASE" --expect mock --no-build \
	--login-user admin --login-pass "$PASSWORD"
SMOKE_CODE=$?
cd "$ROOT" || exit 1

echo
printf '工作目录（含 web.log 与数据库）: %s\n' "$WORKDIR"
if [ "$SMOKE_CODE" -ne 0 ]; then
	DIAG=1
	exit 1
fi
exit 0
