#!/usr/bin/env bash
#
# 与真实 MQTT broker（mosquitto）的互操作验收。
#
# 单元测试里的 broker 是按规范独立实现的，能证明「客户端拼出来的字节自洽」，
# 但不能证明「别的实现也认」。这个脚本用官方 mosquitto 镜像跑一次真链路：
#
#   1. 连接后 $birth 上线，点位值按 QoS1 持续发布，$state 有 retained 状态；
#   2. broker 重启后客户端自己回去，并补发 $birth / $state（retained 已随重启清空）；
#   3. 通过 broker 下发命令：执行结果回到 $command/result，重复 id 只执行一次，
#      特权动作被拒；
#   4. kill -9 网关，broker 代发遗嘱 $death —— 订阅端看到的「进程被杀」与「正常退出」一致。
#
# 用法：
#   scripts/mqtt-interop.sh          # 跑一遍，结束后清理
#   scripts/mqtt-interop.sh --keep   # 保留临时目录（配置、日志、抓包）便于排查
#
# 依赖：docker 与 python3。镜像不存在时会自动拉取。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${MOSQUITTO_IMAGE:-eclipse-mosquitto:2}"
KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
die() { printf '  \033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "需要 docker"
command -v python3 >/dev/null 2>&1 || die "需要 python3（只用来挑空闲端口）"

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_port() { # host port timeout-seconds
  local host=$1 port=$2 timeout=${3:-20} i
  for ((i = 0; i < timeout * 10; i++)); do
    if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
      exec 3>&-
      return 0
    fi
    sleep 0.1
  done
  return 1
}

wait_line() { # file pattern timeout-seconds
  local file=$1 pattern=$2 timeout=${3:-20} i
  for ((i = 0; i < timeout * 10; i++)); do
    grep -qF -- "$pattern" "$file" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

wait_count() { # file pattern count timeout-seconds
  local file=$1 pattern=$2 count=$3 timeout=${4:-20} i
  for ((i = 0; i < timeout * 10; i++)); do
    [[ "$(grep -cF -- "$pattern" "$file" 2>/dev/null || true)" -ge "$count" ]] && return 0
    sleep 0.1
  done
  return 1
}

TMP="$(mktemp -d)"
BROKER="uic-mosq-$$"
GW_PID=""
SUB_PID=""

cleanup() {
  [[ -n "$GW_PID" ]] && kill -9 "$GW_PID" 2>/dev/null || true
  docker exec "$BROKER" pkill -f mosquitto_sub >/dev/null 2>&1 || true
  [[ -n "$SUB_PID" ]] && kill "$SUB_PID" 2>/dev/null || true
  docker rm -f "$BROKER" >/dev/null 2>&1 || true

  if [[ $KEEP -eq 1 ]]; then
    printf '\n现场保留在 %s（config.json / gateway.log / capture*.txt）\n' "$TMP"
  else
    rm -rf "$TMP"
  fi
}
trap cleanup EXIT

MQTT_PORT="$(free_port)"
REST_PORT="$(free_port)"

cat > "$TMP/mosquitto.conf" <<'CONF'
listener 1883
allow_anonymous true
CONF

cat > "$TMP/config.json" <<JSON
{
  "schemaVersion": 1,
  "site": "interop-site",
  "description": "MQTT 互操作验收：一台虚拟设备 + 真实 broker",
  "channels": [
    { "id": "virtual-bus", "name": "虚拟总线", "protocol": "virtual", "transport": "none" }
  ],
  "devices": [
    {
      "id": "virtual-1",
      "name": "虚拟设备 1",
      "channelId": "virtual-bus",
      "interval": "200ms",
      "labels": { "mode": "sine", "periodSeconds": "10", "amplitude": "100", "baseline": "50" },
      "points": [
        {
          "id": "temperature",
          "name": "温度",
          "address": { "kind": "register", "area": "holding", "offset": 0 },
          "dataType": "float64",
          "unit": "°C"
        },
        {
          "id": "counter",
          "name": "累计计数",
          "address": { "kind": "register", "area": "holding", "offset": 8 },
          "dataType": "uint32"
        }
      ]
    }
  ],
  "northbound": {
    "rest": { "enabled": true, "listen": "127.0.0.1:$REST_PORT" },
    "mqtt": {
      "enabled": true,
      "host": "127.0.0.1",
      "port": $MQTT_PORT,
      "topic": "uic/{site}/{device}/{point}/value",
      "qos": 1,
      "keepAliveSeconds": 10,
      "reconnectDelayMs": 500,
      "commands": true
    },
    "opcUa": { "enabled": false }
  },
  "storage": { "dataDir": "$TMP/data" }
}
JSON

start_broker() {
  docker run -d --rm --name "$BROKER" \
    -p "127.0.0.1:$MQTT_PORT:1883" \
    -v "$TMP/mosquitto.conf:/mosquitto/config/mosquitto.conf:ro" \
    "$IMAGE" >/dev/null

  wait_port 127.0.0.1 "$MQTT_PORT" ||
    die "broker 没有起来，docker logs $BROKER 查看原因"
}

stop_broker() {
  docker exec "$BROKER" pkill -f mosquitto_sub >/dev/null 2>&1 || true
  [[ -n "$SUB_PID" ]] && { kill "$SUB_PID" 2>/dev/null || true; wait "$SUB_PID" 2>/dev/null || true; SUB_PID=""; }
  docker rm -f "$BROKER" >/dev/null 2>&1 || true
}

subscribe() { # capture-file
  : > "$1"
  docker exec "$BROKER" mosquitto_sub -h 127.0.0.1 -p 1883 -t 'uic/#' -v -W 60 -C 2000 >"$1" 2>&1 &
  SUB_PID=$!
}

log "准备镜像与 broker"
docker image inspect "$IMAGE" >/dev/null 2>&1 || {
  echo "  拉取 $IMAGE ..."
  docker pull "$IMAGE" >/dev/null
}
start_broker
ok "mosquitto 监听 127.0.0.1:$MQTT_PORT"

log "构建并启动网关"
dotnet build "$ROOT/src/Uic.Host" -v quiet >/dev/null ||
  die "网关构建失败"

DLL="$ROOT/src/Uic.Host/bin/Debug/net10.0/uic.dll"
[[ -f "$DLL" ]] || die "找不到 $DLL"

dotnet "$DLL" run --config "$TMP/config.json" --data "$TMP/data" \
  >"$TMP/gateway.log" 2>&1 &
GW_PID=$!

wait_port 127.0.0.1 "$REST_PORT" 30 || die "网关没有起来，见 $TMP/gateway.log"

subscribe "$TMP/capture1.txt"

wait_line "$TMP/capture1.txt" 'uic/interop-site/$birth' 20 ||
  die "没有收到 \$birth，broker 与客户端可能没握上手（见 $TMP/gateway.log）"
wait_line "$TMP/capture1.txt" 'uic/interop-site/virtual-1/temperature/value' 20 ||
  die "没有收到点位值"
wait_line "$TMP/capture1.txt" 'uic/interop-site/virtual-1/$state' 20 ||
  die "没有收到设备 \$state"

grep -q '"online":true' "$TMP/capture1.txt" || die "\$birth 载荷里没有 online:true"
grep -q '"schemaVersion":1' "$TMP/capture1.txt" || die "载荷里没有 schemaVersion"
grep -q 'no PUBACK' "$TMP/gateway.log" && die "日志里出现 PUBACK 超时"

VALUES=$(grep -c 'virtual-1/temperature/value' "$TMP/capture1.txt")
ok "上线：\$birth / \$state / 点位值都在，已收 $VALUES 条温度样本"

# 上线/状态是 retained 的：新订阅者不重启网关也应该一问即知。
docker exec "$BROKER" mosquitto_sub -h 127.0.0.1 -p 1883 \
  -t 'uic/interop-site/$birth' -t 'uic/interop-site/+/$state' -v -C 2 -W 10 \
  >"$TMP/retained.txt" 2>&1 || true

wait_line "$TMP/retained.txt" 'uic/interop-site/$birth' 5 ||
  die "retained 的 \$birth 对新订阅者不可见"
wait_line "$TMP/retained.txt" 'uic/interop-site/virtual-1/$state' 5 ||
  die "retained 的 \$state 对新订阅者不可见"
ok "新订阅者一条命令就拿到 retained 的 \$birth 与 \$state"

log "重启 broker，验证自动重连与补发"
stop_broker
start_broker
ok "broker 已重启（retained 随进程消失）"

subscribe "$TMP/capture2.txt"

wait_line "$TMP/capture2.txt" 'uic/interop-site/$birth' 25 ||
  die "broker 重启后客户端没有重连（见 $TMP/gateway.log）"
wait_line "$TMP/capture2.txt" 'uic/interop-site/virtual-1/$state' 25 ||
  die "重连后没有补发 \$state"
wait_line "$TMP/capture2.txt" 'uic/interop-site/virtual-1/temperature/value' 25 ||
  die "重连后数据没有恢复"
ok "重连后补发了 \$birth 与 \$state，数据继续流动"

log "下发命令：执行结果回到 \$command/result"

publish_command() { # json
  docker exec "$BROKER" mosquitto_pub -h 127.0.0.1 -p 1883 -q 1 \
    -t 'uic/interop-site/$command' -m "$1"
}

publish_command '{"id":"interop-1","action":"driver.restart","params":{"device":"virtual-1"}}'
wait_line "$TMP/capture2.txt" 'uic/interop-site/$command/result' 20 ||
  die "没有收到命令结果（见 $TMP/gateway.log）"
grep -q '"id":"interop-1"' "$TMP/capture2.txt" || die "结果里没有回显请求 id"
grep -q '"ok":true' "$TMP/capture2.txt" || die "命令没有执行成功"
grep -q '"replayed":false' "$TMP/capture2.txt" || die "首次结果被误标成 replay"

FIRST_RESULTS="$(grep -cF 'uic/interop-site/$command/result' "$TMP/capture2.txt" || true)"
ok "命令被执行，结果已回到 \$command/result"

# 同一个 id 重投：必须重放首次结果，而不是再重启一次设备。
publish_command '{"id":"interop-1","action":"driver.restart","params":{"device":"virtual-1"}}'
wait_count "$TMP/capture2.txt" 'uic/interop-site/$command/result' "$((FIRST_RESULTS + 1))" 20 ||
  die "重复 id 没有返回结果"
grep -q '"replayed":true' "$TMP/capture2.txt" || die "重复 id 没有被标记为 replay"
ok "重复 id 只执行一次，第二次结果是 replay"

# 匿名明文通道不允许回滚配置：即使带 confirm 也必须拒绝。
publish_command '{"id":"interop-2","action":"config.rollback","params":{"version":"v1"},"confirm":true}'
wait_count "$TMP/capture2.txt" 'uic/interop-site/$command/result' "$((FIRST_RESULTS + 2))" 20 ||
  die "特权命令没有返回结果"
grep -q '"code":"command.forbidden"' "$TMP/capture2.txt" || die "特权命令没有被拒绝"
ok "config.rollback 在 MQTT 通道上被拒绝（command.forbidden）"

log "kill -9 网关，验证 broker 代发遗嘱"
kill -9 "$GW_PID"
wait "$GW_PID" 2>/dev/null || true
GW_PID=""

wait_line "$TMP/capture2.txt" 'uic/interop-site/$death' 20 ||
  die "进程被 kill 后没有收到遗嘱 \$death"
grep -q '"online":false' "$TMP/capture2.txt" || die "\$death 载荷里没有 online:false"
ok "遗嘱 \$death 由 broker 代发，载荷 online:false"

printf '\n\033[32mMQTT 互操作验收通过\033[0m（真实 broker：%s）\n' "$IMAGE"
