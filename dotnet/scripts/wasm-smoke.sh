#!/usr/bin/env bash
#
# WASM 控制台的发布与冒烟验收。
#
# 分两层：
#   静态层（curl）：index.html、引导脚本、SPA 回退、缺少的资源必须 404、REST 可达；
#   运行层（Playwright）：真开浏览器，等 WASM 启动并把 REST 数据渲染成设备列表。
#
# 运行层是重点。裁剪后的 WASM 最容易坏在运行时：源生成序列化漏了类型、启动脚本与
# index.html 的结构约定被改坏、同源地址解析写死到 8080。这些用 curl 全看不出来。
#
# 用法：
#   scripts/wasm-smoke.sh                # 发布（Release）+ 两层都跑
#   scripts/wasm-smoke.sh --keep         # 保留发布产物、日志、截图
#   scripts/wasm-smoke.sh --web <dir>    # 复用已有发布目录，跳过 dotnet publish
#   scripts/wasm-smoke.sh --no-browser   # 只做静态层（没装 playwright 的机器）
#   scripts/wasm-smoke.sh --browser firefox   # 换浏览器（默认 chromium）
#
# 依赖：dotnet 10、curl；运行层额外需要 playwright（全局安装即可，脚本自己找 NODE_PATH）。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEEP=0
WEB=""
BROWSER=1
BROWSER_NAME="chromium"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) KEEP=1 ;;
    --no-browser) BROWSER=0 ;;
    --web) WEB="${2:?--web 需要一个目录}"; shift ;;
    --browser) BROWSER_NAME="${2:?--browser 需要一个名字}"; shift ;;
    *) echo "未知参数：$1" >&2; exit 64 ;;
  esac
  shift
done

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die() { printf '  \033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

command -v dotnet >/dev/null 2>&1 || die "需要 dotnet"
command -v curl >/dev/null 2>&1 || die "需要 curl"

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

TMP="$(mktemp -d)"
HOST_PID=""

cleanup() {
  [[ -n "$HOST_PID" ]] && kill "$HOST_PID" 2>/dev/null || true
  [[ -n "$HOST_PID" ]] && wait "$HOST_PID" 2>/dev/null || true

  if [[ $KEEP -eq 1 ]]; then
    printf '\n现场保留在 %s（发布产物 / host.log / screen.png）\n' "$TMP"
  else
    rm -rf "$TMP"
  fi
}
trap cleanup EXIT

if [[ -z "$WEB" ]]; then
  log "发布 WASM（Release，裁剪）"
  dotnet publish "$ROOT/src/Uic.Ui/Uic.Ui.csproj" \
    -f net10.0-browserwasm -c Release -o "$TMP/pub" -v quiet >/dev/null ||
    die "发布失败"

  WEB="$TMP/pub/wwwroot"
  [[ -f "$WEB/index.html" ]] || die "发布目录里没有 index.html：$WEB"
  ok "发布完成：$(du -sh "$WEB" | cut -f1)，$(find "$WEB" -type f | wc -l) 个文件"
else
  WEB="$(cd "$WEB" && pwd)"
  [[ -f "$WEB/index.html" ]] || die "--web 目录里没有 index.html：$WEB"
  ok "复用已有发布目录：$WEB"
fi

log "启动宿主（静态资源 + REST 同源）"
dotnet build "$ROOT/src/Uic.Host" -v quiet >/dev/null || die "宿主构建失败"

PORT="$(free_port)"
DLL="$ROOT/src/Uic.Host/bin/Debug/net10.0/uic.dll"
[[ -f "$DLL" ]] || die "找不到 $DLL"

dotnet "$DLL" run --data "$TMP/data" --listen "127.0.0.1:$PORT" --web "$WEB" \
  >"$TMP/host.log" 2>&1 &
HOST_PID=$!

for ((i = 0; i < 300; i++)); do
  curl -fsS -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null && break
  sleep 0.1
done
curl -fsS -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null ||
  die "宿主没有起来，见 $TMP/host.log"

BASE="http://127.0.0.1:$PORT"
ok "宿主监听 $BASE"

log "静态层"
status=$(curl -sS -o "$TMP/index.html" -w '%{http_code}' "$BASE/")
[[ "$status" == "200" ]] || die "GET / 返回 $status"
grep -q 'uno-bootstrap.js' "$TMP/index.html" || die "index.html 里没有引导脚本"
ok "GET / 返回启动页"

BOOTSTRAP=$(grep -o 'package_[a-f0-9]*/uno-bootstrap.js' "$TMP/index.html" | head -1)
[[ -n "$BOOTSTRAP" ]] || die "没能从 index.html 里解析出引导脚本路径"

curl -fsS -o /dev/null "$BASE/$BOOTSTRAP" || die "引导脚本拉不到：/$BOOTSTRAP"
ok "引导脚本可下载：/$BOOTSTRAP"

BODY=$(curl -fsS "$BASE/api/v1/health") || die "GET /api/v1/health 失败"
grep -q '"status":"ok"' <<<"$BODY" || die "健康检查没有返回 status=ok：$BODY"
ok "REST 同源可用：/api/v1/health"

status=$(curl -sS -o "$TMP/fallback.html" -w '%{http_code}' "$BASE/devices")
[[ "$status" == "200" ]] || die "SPA 回退返回 $status"
grep -q 'uno-bootstrap.js' "$TMP/fallback.html" || die "SPA 回退没有返回启动页"
ok "深链接回退到 index.html：GET /devices"

status=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/_framework/definitely-not-here.wasm")
[[ "$status" == "404" ]] || die "缺失的框架资源返回 $status（应为 404，不能回退成 HTML）"
ok "缺失的资源返回 404，不会被 SPA 回退吞掉"

if [[ $BROWSER -eq 0 ]]; then
  warn "按参数跳过浏览器层（--no-browser）"
  log "静态层通过"
  exit 0
fi

log "运行层（Playwright · $BROWSER_NAME）"
NODE_PATH="$(npm root -g 2>/dev/null || true)"
export NODE_PATH
export UIC_SMOKE_BROWSER="$BROWSER_NAME"

set +e
node "$ROOT/scripts/wasm-smoke.cjs" "$BASE/" "$TMP/screen.png"
rc=$?
set -e

case "$rc" in
  0) ok "浏览器里 WASM 启动完成，设备列表来自 REST（$BROWSER_NAME）" ;;
  2) warn "本机没有 playwright 或没有 $BROWSER_NAME，运行层跳过；静态层已通过" ;;
  *) die "浏览器冒烟失败（退出码 $rc）" ;;
esac

log "WASM 冒烟通过"
