#!/usr/bin/env bash
#
# 历史查询性能基准（FR-NOR-08「100 万点 < 2s」）。
#
# 这是耗时验收，不进自包含测试运行器：
#   - 要先生成上百 MB 的 journal，默认每次跑都要几十秒；
#   - 数字依赖机器（CPU、磁盘），不适合当通过/失败断言。
# 因此做成脚本：生成一次数据、多次测量、打印表格，由人判断。
#
# 用法：
#   scripts/history-bench.sh                 # 默认 100 万行
#   scripts/history-bench.sh --lines 200000  # 换数据量
#   scripts/history-bench.sh --keep          # 保留数据与临时工程
#
# 依赖：dotnet 10。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LINES=1000000
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --lines) LINES="${2:?--lines needs a number}"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,20p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

WORK="${TMPDIR:-/tmp}/uic-history-bench"
DATA="$WORK/data"
PROJ="$WORK/bench"

echo "== 生成 $LINES 行 journal =="
mkdir -p "$PROJ" "$DATA/journal"

cat > "$PROJ/bench.csproj" <<EOF
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <OutputType>Exe</OutputType>
    <TargetFramework>net10.0</TargetFramework>
    <Nullable>enable</Nullable>
    <ImplicitUsings>enable</ImplicitUsings>
  </PropertyGroup>
  <ItemGroup>
    <ProjectReference Include="$ROOT/src/Uic.Northbound/Uic.Northbound.csproj" />
  </ItemGroup>
</Project>
EOF

cat > "$PROJ/Program.cs" <<'EOF'
using System.Diagnostics;
using Uic.Domain;
using Uic.Northbound;

// 第一个参数：数据目录；第二个：行数。
var dir = args[0];
var lines = int.Parse(args[1]);
var path = Path.Combine(dir, "journal", "journal.ndjson");
Directory.CreateDirectory(Path.GetDirectoryName(path)!);

if (!File.Exists(path) || new FileInfo(path).Length == 0)
{
    // 50 台设备 × 200 个点位，每 0.1 s 一条，覆盖约 27 小时。
    var t0 = new DateTimeOffset(2026, 11, 1, 0, 0, 0, TimeSpan.Zero);
    using var writer = new StreamWriter(path);
    var sw = Stopwatch.StartNew();

    for (var i = 0; i < lines; i++)
    {
        var at = t0.AddSeconds(i / 10.0);
        var sample = new Sample(
            $"dev-{i % 50:D2}", $"p{i % 200:D3}",
            DataValue.Number(DataType.Float64, i % 1000),
            Quality.Good, "°C", null, at, (ulong)(i + 1));

        writer.Write(ApiJson.Serialize(SampleDto.From(sample)));
        writer.Write('\n');
    }

    Console.WriteLine($"  {lines} 行 / {new FileInfo(path).Length / 1024 / 1024} MB，{sw.ElapsedMilliseconds} ms");
}

var history = new JournalHistory(dir);
var to = new DateTimeOffset(2026, 11, 1, 0, 0, 0, TimeSpan.Zero).AddSeconds(lines / 10.0);
var from = to.AddDays(-3650);

async Task Measure(string label, HistoryQuery query, int rounds = 3)
{
    var best = long.MaxValue;
    var count = 0;

    for (var i = 0; i < rounds; i++)
    {
        var sw = Stopwatch.StartNew();
        var result = await history.QueryAsync(query, default);
        sw.Stop();

        count = result.Items.Count;
        if (sw.ElapsedMilliseconds < best) best = sw.ElapsedMilliseconds;
    }

    Console.WriteLine($"  {label,-34} 返回 {count,7} 条  最快 {best,5} ms");
}

Console.WriteLine("== 查询（每项取 3 次最快值，冷启动不参与）==");
await Measure("全范围 + 5m 降采样", new HistoryQuery(null, null, from, to, TimeSpan.FromMinutes(5)));
await Measure("单设备 + 1m 降采样", new HistoryQuery("dev-07", null, from, to, TimeSpan.FromMinutes(1)));
await Measure("最近 30 分钟 + 1m 降采样", new HistoryQuery(null, null, to.AddMinutes(-30), to, TimeSpan.FromMinutes(1)));
await Measure("全范围不降采样", new HistoryQuery(null, null, from, to, null), rounds: 1);
EOF

# 允许用户用自己的 SDK 位置；没显式指定时先探测仓库常用的同级路径，
# 都没有就直接用 PATH 里的 dotnet（并给出可读的报错）。
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

if ! command -v dotnet >/dev/null 2>&1; then
  echo "找不到 dotnet：请把 SDK 加入 PATH，或设置 DOTNET_ROOT。" >&2
  exit 1
fi

dotnet run --project "$PROJ" -c Release -- "$DATA" "$LINES"

if [[ $KEEP -eq 0 ]]; then
  rm -rf "$WORK"
  echo "（数据与临时工程已删除；加 --keep 保留）"
fi
