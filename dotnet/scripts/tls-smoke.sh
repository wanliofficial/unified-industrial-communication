#!/usr/bin/env bash
#
# 内置 TLS 的外部验收（FR-OPS-10）。
#
# 自包含用例已经覆盖了「装载器 + 进程内 Kestrel」的语义，但那是用
# HttpMessageHandler 钉住证书的进程内客户端。这里换成真正的进程与真正的
# openssl/curl：
#
#   1. 自签证书 + PFX 能起 HTTPS；curl 用同一张证书作 CA 访问成功
#   2. 同一个端口上明文 HTTP 必须连不上（没有「顺手也开了 80」这种事）
#   3. 证书口令不对 / 文件缺失 / 证书缺失 → 退出码 2，且不监听端口
#   4. `uic check` 能只看配置就报出问题（部署前静态检查）
#   5. MQTT 的 mqtts:// 前缀与 CA 路径同样在 check 里被解释
#
# 为什么不用 docker：TLS 是本地密码学与监听装配的问题，跟镜像无关；
# 少一个外部依赖，任何装了 openssl + curl 的机器都能跑。
#
# 用法：
#   scripts/tls-smoke.sh          # 跑一遍并清理
#   scripts/tls-smoke.sh --keep   # 保留证书与 host.log 便于排查
#
# 依赖：dotnet 10、openssl、curl。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die() { printf '  \033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

command -v dotnet >/dev/null 2>&1 || die "需要 dotnet"
command -v openssl >/dev/null 2>&1 || die "需要 openssl"
command -v curl >/dev/null 2>&1 || die "需要 curl"

TMP="$(mktemp -d)"
HOST_PID=""

cleanup() {
  [[ -n "$HOST_PID" ]] && kill "$HOST_PID" 2>/dev/null || true
  [[ -n "$HOST_PID" ]] && wait "$HOST_PID" 2>/dev/null || true

  if [[ $KEEP -eq 1 ]]; then
    printf '\n现场保留在 %s（证书 / 配置 / host.log）\n' "$TMP"
  else
    rm -rf "$TMP"
  fi
}
trap cleanup EXIT

PORT="${UIC_TLS_PORT:-$(( 20000 + RANDOM % 20000 ))}"
PFX_PASSWORD="tls-smoke-$(openssl rand -hex 8)"
BASE="https://127.0.0.1:$PORT"

log "生成自签证书（CA=true、SAN localhost + 127.0.0.1）"
mkdir -p "$TMP/data/security"

openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -keyout "$TMP/data/security/gateway.key" \
  -out "$TMP/data/security/gateway.crt" \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
  -addext "basicConstraints=critical,CA:TRUE" >/dev/null 2>&1 ||
  die "openssl req 失败"

openssl pkcs12 -export \
  -inkey "$TMP/data/security/gateway.key" \
  -in "$TMP/data/security/gateway.crt" \
  -out "$TMP/data/security/gateway.pfx" \
  -passout "pass:$PFX_PASSWORD" >/dev/null 2>&1 ||
  die "openssl pkcs12 失败"

ok "证书与 PFX 已生成（口令不落盘，只在环境变量里）"

write_config() {
  # $1 = 输出路径，$2 = certificateFile，$3 = certificatePassword（引用或空串）
  #
  # 口令行用 @PASSWORD@ 占位再替换：引用本身形如 ${env:NAME}，直接写进
  # 未加引号的 heredoc 会被 shell 展开成空值——配置看起来正常，实际没填。
  local out="$1" file="$2" ref="$3"
  cat >"$out" <<JSON
{
  "schemaVersion": 1,
  "site": "tls-smoke",
  "channels": [
    { "id": "virtual-bus", "name": "虚拟总线", "protocol": "virtual", "transport": "none" }
  ],
  "devices": [
    {
      "id": "v1",
      "name": "虚拟设备",
      "channelId": "virtual-bus",
      "interval": "500ms",
      "labels": { "mode": "const", "baseline": "42", "amplitude": "1" },
      "points": [
        {
          "id": "temp",
          "name": "温度",
          "address": { "kind": "register", "area": "holding", "offset": 0 },
          "dataType": "float64"
        }
      ]
    }
  ],
  "northbound": {
    "rest": {
      "enabled": true,
      "listen": "127.0.0.1:$PORT",
      "tls": {
        "enabled": true,
        "certificateFile": "$file",
        "certificatePassword": "@PASSWORD@"
      }
    },
    "mqtt": {
      "enabled": true,
      "host": "broker.local",
      "port": 8883,
      "tls": { "enabled": true, "caFile": "security/gateway.crt" }
    }
  },
  "storage": { "dataDir": "./data" }
}
JSON

  # 空串表示「这个用例不提供口令」：Secrets 把空值当未设置，不会误判成明文。
  sed -i "s|@PASSWORD@|$ref|" "$out"
}

log "构建宿主"
dotnet build "$ROOT/src/Uic.Host" -v quiet >/dev/null || die "宿主构建失败"
DLL="$ROOT/src/Uic.Host/bin/Debug/net10.0/uic.dll"
[[ -f "$DLL" ]] || die "找不到 $DLL"

write_config "$TMP/gateway.json" "security/gateway.pfx" '${env:UIC_TLS_SMOKE_PASSWORD}'
export UIC_TLS_SMOKE_PASSWORD="$PFX_PASSWORD"
log "uic check（静态检查，不启动进程）"
CHECK="$(cd "$TMP" && dotnet "$DLL" check --config gateway.json 2>&1)" ||
  die "uic check 失败：$CHECK"

printf '%s\n' "$CHECK" | sed 's/^/    /'
printf '%s\n' "$CHECK" | grep -q "https://127.0.0.1:$PORT" ||
  die "check 没有报告 https 监听"
printf '%s\n' "$CHECK" | grep -q "mqtts(broker.local:8883)" ||
  die "check 没有报告 mqtts"
printf '%s\n' "$CHECK" | grep -q "UIC_TLS_SMOKE_PASSWORD" ||
  die "check 没有报告 PFX 口令的来源"
ok "check 报告了 https / mqtts 与口令来源"

log "启动宿主（HTTPS）"
# exec 让子 shell 被 dotnet 替换掉：否则 $! 是 bash 的 pid，
# kill 只杀壳、宿主继续占着端口，后面的「端口未监听」断言会假失败。
(cd "$TMP" && exec dotnet "$DLL" run --config gateway.json --data ./data >host.log 2>&1) &
HOST_PID=$!

for ((i = 0; i < 300; i++)); do
  curl -fsS --cacert "$TMP/data/security/gateway.crt" -o /dev/null "$BASE/healthz" 2>/dev/null && break
  sleep 0.1
done

grep -q "listening on https://127.0.0.1:$PORT" "$TMP/host.log" ||
  die "启动日志没有 https 监听：$(tail -5 "$TMP/host.log")"
ok "启动日志写明 https://127.0.0.1:$PORT"

log "TLS 层"
curl -fsS --cacert "$TMP/data/security/gateway.crt" "$BASE/healthz" >/dev/null ||
  die "用证书作 CA 的 HTTPS 请求失败"
ok "curl --cacert 通过证书校验，/healthz 可达"

# 关掉校验就必须还能连上——否则上面的成功可能只是「根本没走 TLS」。
curl -fsSk "$BASE/api/v1/version" | grep -q '"site":"tls-smoke"' ||
  die "关闭校验的请求也失败了"
ok "关闭校验也能取到 /api/v1/version（同样的端点，不同的校验策略）"

# 明文必须连不上：同一端口没有 HTTP fallback。
if curl -fsS -m 3 -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null; then
  die "同一个端口居然接受明文 HTTP"
fi
ok "同一端口拒绝明文 HTTP"

# 用错 CA 必须失败：证明客户端校验真的在起作用。
if curl -fsS -m 5 --cacert "$TMP/data/security/gateway.key" -o /dev/null "$BASE/healthz" 2>/dev/null; then
  die "错误的 CA 竟然通过了校验"
fi
ok "错误的 CA 被拒绝"

# openssl s_client 看一眼协商出来的协议与证书主题，作为独立第三方视角。
S_CLIENT="$(echo | openssl s_client -connect "127.0.0.1:$PORT" \
  -CAfile "$TMP/data/security/gateway.crt" -servername localhost 2>/dev/null || true)"

printf '%s\n' "$S_CLIENT" | grep -q "CN *= *localhost" ||
  die "s_client 没有看到 CN=localhost"
ok "openssl s_client 看到 CN=localhost"

printf '%s\n' "$S_CLIENT" | grep -Eq "TLSv1\.[23]" ||
  warn "s_client 没有明确报告 TLSv1.2/1.3（可能是版本字符串格式变了）"

kill "$HOST_PID" 2>/dev/null || true
wait "$HOST_PID" 2>/dev/null || true
HOST_PID=""

log "失败路径：配置非法时退出码 2，且不监听端口"

expect_exit_2() {
  # $1 = 说明，$2 = 配置路径，$3 = 期望出现在日志里的字段路径
  #
  # 用独立空数据目录：启动优先级是「已生效的配置 > --config 指定的文件」，
  # 复用主运行目录会把坏配置整个忽略掉，测的是另一份配置。
  # 数据目录里必须有证书，证书路径才能相对它解析。
  local label="$1" config="$2" field="$3" code=0
  local data="./fail-data"
  rm -rf "$TMP/fail-data"
  # 独立数据目录里也要有证书：caFile / certificateFile 都是文件路径，
  # 缺了会在那一项上失败，把真正想测的字段挡在后面。
  mkdir -p "$TMP/fail-data/security"
  cp "$TMP/data/security/gateway.crt" "$TMP/fail-data/security/" 2>/dev/null || true
  cp "$TMP/data/security/gateway.pfx" "$TMP/fail-data/security/" 2>/dev/null || true
  (cd "$TMP" && dotnet "$DLL" run --config "$config" --data "$data" >bad.log 2>&1) || code=$?
  [[ "$code" -eq 2 ]] || die "$label：期望退出码 2，实际 $code"

  # 说明必须落在具体字段上：只说「配置非法」等于让运维自己去猜。
  grep -q "$field" "$TMP/bad.log" || die "$label：日志里没有指出 $field"

  # 端口必须没被占用：监听没有发生。
  if curl -fsSk -m 2 -o /dev/null "$BASE/healthz" 2>/dev/null; then
    die "$label：端口仍然可达"
  fi
  ok "$label：退出码 2，端口未监听"
}

# 失败配置的证书路径相对配置里的 storage.dataDir（./data）解析，
# 与主运行同一个目录，因此相对路径写法与主配置一致。
write_config "$TMP/no-cert.json" "" ""
expect_exit_2 "证书路径为空" no-cert.json "northbound.rest.tls.certificateFile"

write_config "$TMP/missing.json" "security/absent.pfx" '${env:UIC_TLS_SMOKE_PASSWORD}'
expect_exit_2 "证书文件不存在" missing.json "northbound.rest.tls.certificateFile"

write_config "$TMP/wrong-pass.json" "security/gateway.pfx" '${env:UIC_TLS_SMOKE_WRONG}'
export UIC_TLS_SMOKE_WRONG="definitely-not-the-password"
expect_exit_2 "PFX 口令不对" wrong-pass.json "certificate data cannot be read"

# 明文口令（不是引用）在静态检查阶段就该被拒。
write_config "$TMP/plain-pass.json" "security/gateway.pfx" "plain-text-password"
code=0
(cd "$TMP" && dotnet "$DLL" check --config plain-pass.json >plain.log 2>&1) || code=$?
[[ "$code" -eq 2 ]] || die "明文 PFX 口令：期望 check 退出码 2，实际 $code"
grep -q "northbound.rest.tls.certificatePassword" "$TMP/plain.log" ||
  die "明文 PFX 口令：没有指出字段路径"
ok "明文 PFX 口令被 check 拒绝并指出字段"

log "TLS 冒烟通过"
