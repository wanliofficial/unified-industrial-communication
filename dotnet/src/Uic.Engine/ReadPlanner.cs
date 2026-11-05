using Uic.Domain;
using Uic.Driver;

namespace Uic.Engine;

/// <summary>
/// 批量读规划：把离散点位合并成若干「连续窗口」请求。
/// 窗口越少，一轮采集的往返次数越少；但窗口过大会放大单次失败的代价，
/// 所以默认只合并严格相邻的点位（maxGap = 0）。
/// </summary>
public static class ReadPlanner
{
    public const int DefaultMaxRegisters = 125;
    public const int DefaultMaxBits = 2000;

    private static readonly HashSet<string> BitAreas = new(StringComparer.OrdinalIgnoreCase)
    {
        "coil", "coils", "discrete", "discrete_input", "discreteinput", "di",
    };

    public static bool IsBitArea(string? area) => area is not null && BitAreas.Contains(area.Trim());

    public static IReadOnlyList<ReadSpan> Plan(
        IReadOnlyList<PointRef> points,
        int maxGap = 0,
        int maxRegisters = DefaultMaxRegisters)
    {
        var spans = new List<ReadSpan>();

        // 符号/对象/文件寻址无法用「偏移窗口」合并，一个点一个 span。
        foreach (var p in points.Where(p => p.Address.Kind != AddressKind.Register))
        {
            spans.Add(new ReadSpan($"span-{spans.Count}", new[] { p }));
        }

        var registerPoints = points.Where(p => p.Address.Kind == AddressKind.Register);

        foreach (var group in registerPoints.GroupBy(
                     p => (p.Address.Area ?? string.Empty).Trim().ToLowerInvariant(),
                     StringComparer.Ordinal))
        {
            var isBits = IsBitArea(group.Key);
            var limit = isBits ? DefaultMaxBits : maxRegisters;

            var ordered = group
                .OrderBy(p => p.Address.Offset)
                .ThenBy(p => p.Id, StringComparer.Ordinal)
                .ToList();

            var bucket = new List<PointRef>();
            var windowStart = 0;
            var windowEnd = 0;

            foreach (var p in ordered)
            {
                var length = isBits ? 1 : Math.Max(1, p.Length);
                var start = p.Address.Offset;
                var end = start + length;

                if (bucket.Count == 0)
                {
                    bucket.Add(p);
                    windowStart = start;
                    windowEnd = end;
                    continue;
                }

                var mergedEnd = Math.Max(windowEnd, end);
                var quantity = mergedEnd - windowStart;
                var gap = start - windowEnd;

                if (quantity <= limit && gap <= maxGap)
                {
                    bucket.Add(p);
                    windowEnd = mergedEnd;
                }
                else
                {
                    spans.Add(new ReadSpan($"span-{spans.Count}", bucket.ToArray()));
                    bucket = new List<PointRef> { p };
                    windowStart = start;
                    windowEnd = end;
                }
            }

            if (bucket.Count > 0) spans.Add(new ReadSpan($"span-{spans.Count}", bucket.ToArray()));
        }

        return spans;
    }

    /// <summary>规划结果的诊断字符串：窗口数 + 覆盖点数，日志里比逐个点位好读。</summary>
    public static string Describe(IReadOnlyList<ReadSpan> spans) =>
        $"{spans.Count} window(s) covering {spans.Sum(s => s.Points.Count)} point(s)";
}