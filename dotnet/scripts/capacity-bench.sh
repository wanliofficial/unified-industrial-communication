#!/usr/bin/env bash
#
# 容量与写延迟基准（NFR：200 设备 / 10,000 点；写命令 P95 ≤ 300 ms）。
#
# 为什么是脚本而不是用例：
#   - 要真的起 200 台设备、跑十几秒才谈得上「容量」；
#   - 数字随机器（CPU、磁盘、同时在跑的容器）漂移，放进自包含运行器会让
#     「通过/失败」跟着变，而这恰恰是最容易被忽略的假绿。
# 所以这里只负责把现场搭起来、把数字打出来；是否达标由人对照 PRD 判断
# （脚本在明显不达标时返回非零，方便当 CI 里的冒烟，但数字本身才是重点）。
#
# 用法：
#   scripts/capacity-bench.sh                          # 200 设备 × 50 点 / 1s
#   scripts/capacity-bench.sh --devices 50 --points 20 # 小规模先看一眼
#   scripts/capacity-bench.sh --writes 200 --rate 8    # 改写入次数与速率
#   scripts/capacity-bench.sh --interval 500ms --channels 8  # 改周期与通道数
#   scripts/capacity-bench.sh --confirm                # 写后回读（设备改用常量波形）
#   scripts/capacity-bench.sh --keep                   # 保留配置、数据目录与日志
#
# 依赖：dotnet 10、python3、curl。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

DEVICES=200
POINTS=50
INTERVAL="1s"
CHANNELS=4
WRITES=100
RATE=5
WINDOW=15
BOOT_TIMEOUT=180
CONFIRM=0
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --devices) DEVICES="${2:?--devices 需要一个数字}"; shift 2 ;;
    --points) POINTS="${2:?--points 需要一个数字}"; shift 2 ;;
    --interval) INTERVAL="${2:?--interval 需要时长}"; shift 2 ;;
    --channels) CHANNELS="${2:?--channels 需要一个数字}"; shift 2 ;;
    --writes) WRITES="${2:?--writes 需要一个数字}"; shift 2 ;;
    --rate) RATE="${2:?--rate 需要一个数字}"; shift 2 ;;
    --window) WINDOW="${2:?--window 需要一个数字}"; shift 2 ;;
    --confirm) CONFIRM=1; shift ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,19p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 64 ;;
  esac
done

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die() { printf '  \033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

for pair in "devices:$DEVICES" "points:$POINTS" "channels:$CHANNELS"; do
  [[ "${pair#*:}" =~ ^[0-9]+$ && "${pair#*:}" -gt 0 ]] || die "--${pair%%:*} 需要正整数"
done

[[ "$WRITES" =~ ^[0-9]+$ && "$WRITES" -gt 0 ]] || die "--writes 需要正整数"
[[ "$RATE" =~ ^[0-9]+([.][0-9]+)?$ && "$RATE" != "0" ]] || die "--rate 需要正数"

command -v python3 >/dev/null 2>&1 || die "需要 python3"
command -v curl >/dev/null 2>&1 || die "需要 curl"

# 允许用户用自己的 SDK 位置；没有显式指定时先探测仓库常用的同级路径。
if [[ -z "${DOTNET_ROOT:-}" ]]; then
  for candidate in "$HOME/Development/dotnet-sdk-10" "$HOME/.dotnet"; do
    if [[ -x "$candidate/dotnet" ]]; then
      export DOTNET_ROOT="$candidate"
      export PATH="$candidate:$PATH"
      break
    fi
  done
fi

if [[ -n "${NUGET_PACKAGES:-}" ]]; then
  export NUGET_PACKAGES
elif [[ -d "$HOME/Development/nuget" ]]; then
  export NUGET_PACKAGES="$HOME/Development/nuget"
fi

command -v dotnet >/dev/null 2>&1 || die "找不到 dotnet：请把 SDK 加入 PATH，或设置 DOTNET_ROOT"

WORK="${TMPDIR:-/tmp}/uic-capacity-bench"
DATA="$WORK/data"
CFG="$WORK/gateway.json"
HOST_PID=""

cleanup() {
  if [[ -n "$HOST_PID" ]]; then
    kill "$HOST_PID" 2>/dev/null || true
    wait "$HOST_PID" 2>/dev/null || true
  fi

  if [[ $KEEP -eq 1 ]]; then
    printf '\n现场保留在 %s（配置 / data / host.log）\n' "$WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

rm -rf "$WORK"
mkdir -p "$DATA"

log "生成配置（$DEVICES 设备 × $POINTS 点 = $((DEVICES * POINTS)) 点，周期 $INTERVAL，$CHANNELS 条虚拟通道$( ((CONFIRM)) && echo '，常量波形（写后回读）')）"

python3 - "$CFG" "$DEVICES" "$POINTS" "$INTERVAL" "$CHANNELS" "$CONFIRM" <<'PY'
import json, sys

path, devices, points, interval, channels, confirm = sys.argv[1:7]
devices, points, channels = int(devices), int(points), int(channels)

# 写后回读要求「写进去的值在下一轮读时还在」。虚拟设备默认是正弦波形，
# 每次都重新生成读数；只有常量波形会把写入的值读回来，所以 --confirm 时切过去。
# 代价是值不再变化，缓存与扇出的成本随之下降——两种模式不要混着比较。
def device(d):
    entry = {
        "id": f"dev-{d:03d}",
        "name": f"基准设备 {d:03d}",
        "channelId": f"bench-{d % channels}",
        "interval": interval,
        # 每个点位占一个寄存器：合并读时一台设备正好是一个连续窗口，
        # 这样读请求数取决于设备数而不是点位数，测出来的才是调度与扇出的成本。
        "points": [
            {
                "id": f"p{p:03d}",
                "name": f"点 {p:03d}",
                "address": {"kind": "register", "area": "holding", "offset": p},
                "dataType": "uint16",
            }
            for p in range(points)
        ],
    }
    if confirm == "1":
        entry["labels"] = {"mode": "const"}
    return entry


doc = {
    "schemaVersion": 1,
    "site": "capacity-bench",
    "description": f"容量基准：{devices} 台设备 × {points} 个点位，周期 {interval}",
    "channels": [
        {
            "id": f"bench-{c}",
            "name": f"基准通道 {c}",
            # 虚拟设备不需要链路参数；拆成多条通道是为了让并发上限也参与进来。
            "protocol": "virtual",
            "transport": "none",
        }
        for c in range(channels)
    ],
    "devices": [device(d) for d in range(devices)],
}

with open(path, "w", encoding="utf-8") as f:
    json.dump(doc, f, ensure_ascii=False, indent=2)
    f.write("\n")
PY

PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"

log "启动宿主"
dotnet build "$ROOT/src/Uic.Host" -v quiet --nologo >"$WORK/build.log" 2>&1 ||
  { tail -20 "$WORK/build.log" >&2; die "宿主构建失败"; }

DLL="$ROOT/src/Uic.Host/bin/Debug/net10.0/uic.dll"
[[ -f "$DLL" ]] || die "找不到 $DLL"

BASE="http://127.0.0.1:$PORT"
dotnet "$DLL" run --config "$CFG" --data "$DATA" --listen "127.0.0.1:$PORT" \
  >"$WORK/host.log" 2>&1 &
HOST_PID=$!

for ((i = 0; i < 300; i++)); do
  curl -fsS -o /dev/null "$BASE/healthz" 2>/dev/null && break
  sleep 0.1
done
curl -fsS -o /dev/null "$BASE/healthz" 2>/dev/null ||
  { tail -20 "$WORK/host.log" >&2; die "宿主没有起来"; }

printf '  \033[32m✓\033[0m 监听 %s（pid %s）\n' "$BASE" "$HOST_PID"

log "测量（等全部点位转 good，再取 $WINDOW s 稳态窗口）"

python3 - "$BASE" "$HOST_PID" "$WINDOW" "$WRITES" "$RATE" "$BOOT_TIMEOUT" "$CONFIRM" <<'PY'
import http.client, json, math, os, socket, sys, time

base, pid, window, writes, rate, boot_timeout, confirm = sys.argv[1:8]
window, writes, rate, boot_timeout = float(window), int(writes), float(rate), float(boot_timeout)
confirm = confirm == "1"

host, port = base.removeprefix("http://").split(":")
port = int(port)


def new_conn():
    """
    复用一条连接，并关掉客户端的 Nagle。

    http.client 把请求头和请求体分两次写；客户端 Nagle 开着时，第二个小包会等到
    服务端 delayed ACK（约 40 ms）才发出去，于是偶尔出现 40/80 ms 的假延迟——
    那是本机 TCP 栈的锅，不是网关的。
    """
    c = http.client.HTTPConnection(host, port, timeout=30)
    c.connect()
    c.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    return c


conn = new_conn()


def call(method, path, body=None):
    """一次请求（复用连接，测的是服务端处理时间，不是 TCP 建链）。"""
    global conn
    payload = None if body is None else json.dumps(body).encode()
    headers = {"Content-Type": "application/json"} if payload is not None else {}

    for attempt in (1, 2):
        try:
            conn.request(method, path, body=payload, headers=headers)
            response = conn.getresponse()
            raw = response.read()
            return response.status, raw
        except (http.client.HTTPException, OSError):
            # 连接被对端关掉时重连一次；连不上就直接抛，让脚本失败。
            conn.close()
            conn = new_conn()
            if attempt == 2:
                raise


def get_json(path):
    status, raw = call("GET", path)
    if status != 200:
        raise SystemExit(f"GET {path} -> {status}: {raw[:200]!r}")
    return json.loads(raw)


def health():
    return get_json("/api/v1/health")


def devices():
    return get_json("/api/v1/devices?limit=1000")


def proc_stats():
    """/proc/<pid>/stat 的 utime+stime（clock tick）与 RSS（字节）。"""
    with open(f"/proc/{pid}/stat", "rb") as f:
        fields = f.read().decode().rsplit(")", 1)[1].split()
    ticks = int(fields[11]) + int(fields[12])

    rss = 0
    with open(f"/proc/{pid}/status", "rb") as f:
        for line in f.read().decode().splitlines():
            if line.startswith("VmRSS:"):
                rss = int(line.split()[1]) * 1024
                break
    return ticks, rss


def pct(values, p):
    if not values:
        return float("nan")
    ordered = sorted(values)
    return ordered[max(0, math.ceil(p / 100 * len(ordered)) - 1)]


# ---- 启动到稳态 ----------------------------------------------------------
started = time.perf_counter()
last = health()

while True:
    last = health()
    points = last["points"]
    ready = points["good"] == points["total"] and last["devices"]["collecting"] == last["devices"]["total"]

    if ready:
        break
    if time.perf_counter() - started > boot_timeout:
        print(f"  \033[31m✗\033[0m {boot_timeout:.0f} s 内没有全部转 good："
              f"点位 {points['good']}/{points['total']}，"
              f"设备 {last['devices']['collecting']}/{last['devices']['total']}（faulted {last['devices']['faulted']}）")
        print(f"    健康状态 {last['status']}，problems {len(last.get('problems') or [])} 条")
        raise SystemExit(1)

    time.sleep(0.25)

boot = time.perf_counter() - started
first_points = last["points"]
print(f"  全部点位转 good 用时 {boot:.1f} s（{last['devices']['total']} 台设备，{first_points['total']} 个点位）")

# ---- 稳态窗口：读吞吐、CPU、内存 -----------------------------------------
clk = os.sysconf("SC_CLK_TCK")
ticks0, _ = proc_stats()
reads0 = sum(d["stats"]["readsTotal"] for d in devices()["items"])
t0 = time.perf_counter()

time.sleep(window)

ticks1, rss = proc_stats()
snapshot = devices()
t1 = time.perf_counter()

elapsed = t1 - t0
reads = sum(d["stats"]["readsTotal"] for d in snapshot["items"]) - reads0
cpu = (ticks1 - ticks0) / clk / elapsed * 100

# ---- 写延迟（读负载持续在跑）---------------------------------------------
# REST 的写配额是每 IP 10 次/秒（RateLimiter），超过会 429。那是限流，
# 不是延迟，所以按给定速率发；真出现 429 就单独计数、稍后重试。
device_ids = [d["id"] for d in snapshot["items"]]
latencies = []
rejected = 0
failed = 0
unverified = 0
first_error = None

interval = 1.0 / rate
next_at = time.perf_counter()


def write_once(device, value):
    body = {"value": {"type": "uint16", "num": value}, "confirm": confirm}
    began = time.perf_counter()
    status, raw = call("POST", f"/api/v1/points/{device}/p000/write", body)
    return status, raw, time.perf_counter() - began


# 预热：第一次写会撞上 JIT 与首次连接的冷路径（在本机量到 ~70 ms），
# 把它算进 P95 只会掩盖稳态表现。预热 3 次，不计入统计。
for i in range(3):
    write_once(device_ids[i % len(device_ids)], i)

for i in range(writes):
    now = time.perf_counter()
    if now < next_at:
        time.sleep(next_at - now)
    next_at = max(next_at + interval, time.perf_counter())

    device = device_ids[i % len(device_ids)]
    status, raw, took = write_once(device, i % 1000)

    if status == 429:
        rejected += 1
        time.sleep(0.3)
        continue

    if status != 200:
        failed += 1
        continue

    latencies.append(took * 1000)
    result = json.loads(raw)["results"][0]
    if not result.get("ok"):
        failed += 1
        if first_error is None:
            first_error = f"{device}/p000 -> {result.get('error')}（quality {result.get('quality')}）"
    elif confirm and not result.get("verified"):
        # confirm=true 要求写后回读一致；虚拟驱动必须能通过。
        unverified += 1

final = health()
points = final["points"]

print()
print(f"  读请求            {reads / elapsed:8.1f} /s（{snapshot['total']} 台设备，"
      f"每台每周期一个合并窗口：约 {reads / elapsed * first_points['total'] / max(1, snapshot['total']):8.0f} 点/秒）")
print(f"  点位              {points['good']:8d} / {points['total']} good"
      f"（stale {points['stale']}，bad {points['bad']}）")
print(f"  设备              {final['devices']['collecting']:8d} / {final['devices']['total']} collecting"
      f"（faulted {final['devices']['faulted']}）")
print(f"  CPU               {cpu:8.0f} %（单核 = 100 %，{window:.0f} s 窗口平均）")
print(f"  RSS               {rss / 1024 / 1024:8.1f} MB（工作集，含运行时）")
print(f"  健康              {final['status']:>8}")

print()
if latencies:
    print(f"  写命令 {len(latencies):4d} 次（{rate:.0f} 次/秒，"
          + ("confirm=true 写后回读" if confirm else "confirm=false，只测写入路径")
          + "；预热 3 次不计入）")
    print(f"    P50 {pct(latencies, 50):7.1f} ms   P95 {pct(latencies, 95):7.1f} ms"
          f"   P99 {pct(latencies, 99):7.1f} ms   最大 {max(latencies):7.1f} ms")
    if rejected:
        print(f"    429（写配额）{rejected} 次——按 {rate:.0f} 次/秒发送仍被限流，"
              f"说明配额低于该速率")
    if failed:
        print(f"    \033[31m✗\033[0m {failed} 次返回 ok=false"
              + (f"：例如 {first_error}" if first_error else ""))
    if unverified:
        print(f"    \033[31m✗\033[0m {unverified} 次写后回读不一致")
else:
    print("  \033[31m✗\033[0m 没有成功写入，写延迟无法统计")

print()
print("== 对照 PRD 目标 ==")
capacity_ok = (points["good"] == points["total"] and
               final["devices"]["collecting"] == final["devices"]["total"])
p95 = pct(latencies, 95) if latencies else float("inf")

if capacity_ok:
    print(f"  ✓ 容量 {final['devices']['total']} 设备 / {points['total']} 点全部可采")
else:
    print(f"  ✗ 容量：{points['good']}/{points['total']} 点可采，"
          f"{final['devices']['collecting']}/{final['devices']['total']} 台 collecting")

print(f"  {'✓' if p95 <= 300 else '✗'} 写命令 P95 {p95:.1f} ms（目标 ≤ 300 ms，读负载下）")

if not capacity_ok or p95 > 300 or failed or unverified:
    raise SystemExit(1)
PY

log "完成"
