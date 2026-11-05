using Uic.Domain;
using Uic.Driver;
using Uic.Engine;

namespace Uic.Tests;

/// <summary>
/// 批量读规划（用例 T4 的「结果数量/顺序」前提，以及 T6 的地址边界）。
///
/// 规划错了不会立刻报错，而是变成「一轮多几个来回」或者「一次请求打翻整台设备」，
/// 所以这里对合并边界、上限、非寄存器寻址分别断言。
/// </summary>
internal static class PlannerTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("planner/merges_adjacent_registers", MergesAdjacentRegisters));
        tests.Add(new TestCase("planner/does_not_merge_across_a_gap", DoesNotMergeAcrossGap));
        tests.Add(new TestCase("planner/merges_across_a_gap_when_allowed", MergesAcrossGapWhenAllowed));
        tests.Add(new TestCase("planner/splits_at_register_limit", SplitsAtRegisterLimit));
        tests.Add(new TestCase("planner/splits_at_bit_limit", SplitsAtBitLimit));
        tests.Add(new TestCase("planner/separates_areas", SeparatesAreas));
        tests.Add(new TestCase("planner/keeps_non_register_addresses_separate", KeepsNonRegisterAddressesSeparate));
        tests.Add(new TestCase("planner/covers_every_point_exactly_once", CoversEveryPointExactlyOnce));
    }

    private static PointRef Reg(string id, string area, int offset, int length = 1, DataType type = DataType.UInt16)
        => new(id, Address.Register(area, offset, length), type, ByteOrder.ABCD, length);

    private static Task MergesAdjacentRegisters(TestContext t)
    {
        var spans = ReadPlanner.Plan(new[]
        {
            Reg("a", "holding", 100),
            Reg("b", "holding", 101),
            Reg("c", "holding", 102),
        });

        t.Equal(1, spans.Count, "one window");
        t.Equal(3, spans[0].Points.Count, "all three points in it");
        t.Equal("span-0", spans[0].Id, "span ids are stable");

        return Task.CompletedTask;
    }

    private static Task DoesNotMergeAcrossGap(TestContext t)
    {
        // 默认 maxGap = 0：100 与 102 之间空了一个寄存器，宁可多发一帧，
        // 也不要把「一个不存在的地址」纳入请求——很多老从站会整帧拒绝。
        var spans = ReadPlanner.Plan(new[]
        {
            Reg("a", "holding", 100),
            Reg("b", "holding", 102),
        });

        t.Equal(2, spans.Count, "two windows");
        t.Equal(100, spans[0].Points[0].Address.Offset, "first window starts at 100");
        t.Equal(102, spans[1].Points[0].Address.Offset, "second window starts at 102");

        return Task.CompletedTask;
    }

    private static Task MergesAcrossGapWhenAllowed(TestContext t)
    {
        var spans = ReadPlanner.Plan(new[]
        {
            Reg("a", "holding", 100),
            Reg("b", "holding", 102),
        }, maxGap: 1);

        t.Equal(1, spans.Count, "one window when a 1-register gap is tolerated");
        t.Equal(2, spans[0].Points.Count, "both points");

        // 跨 2 个寄存器的空隙在 maxGap = 1 下仍然要切开。
        var split = ReadPlanner.Plan(new[]
        {
            Reg("a", "holding", 100),
            Reg("b", "holding", 103),
        }, maxGap: 1);

        t.Equal(2, split.Count, "a 2-register gap is still split");

        return Task.CompletedTask;
    }

    private static Task SplitsAtRegisterLimit(TestContext t)
    {
        // 0..129 共 130 个寄存器，超过 125 的上限，必须切成两帧。
        var points = Enumerable.Range(0, 130).Select(i => Reg($"p{i}", "holding", i)).ToArray();
        var spans = ReadPlanner.Plan(points);

        t.Equal(2, spans.Count, "two windows");

        foreach (var span in spans)
        {
            var start = span.Points.Min(p => p.Address.Offset);
            var end = span.Points.Max(p => p.Address.Offset) + 1;
            t.Ok(end - start <= ReadPlanner.DefaultMaxRegisters, $"window {span.Id} stays within 125 registers");
        }

        return Task.CompletedTask;
    }

    private static Task SplitsAtBitLimit(TestContext t)
    {
        // 线圈按位计数，上限是 2000 而不是 125。
        var points = Enumerable.Range(0, 2001)
            .Select(i => Reg($"c{i}", "coil", i, type: DataType.Bool))
            .ToArray();

        var spans = ReadPlanner.Plan(points);
        t.Equal(2, spans.Count, "2001 bits need two windows");

        // 连续 2000 位必须仍装得下一帧；超过一位就切。
        var small = ReadPlanner.Plan(Enumerable.Range(0, 2000)
            .Select(i => Reg($"c{i}", "coil", i, type: DataType.Bool))
            .ToArray());

        t.Equal(1, small.Count, "2000 contiguous bits still fit in one window");

        return Task.CompletedTask;
    }

    private static Task SeparatesAreas(TestContext t)
    {
        var spans = ReadPlanner.Plan(new[]
        {
            Reg("a", "holding", 0),
            Reg("b", "input", 0),
            Reg("c", "coil", 0, type: DataType.Bool),
        });

        t.Equal(3, spans.Count, "each area gets its own window");

        // 区名大小写与空白不应造成额外窗口。
        var messy = ReadPlanner.Plan(new[]
        {
            Reg("a", "Holding", 0),
            Reg("b", " holding ", 1),
        });

        t.Equal(1, messy.Count, "area names are normalized before grouping");

        return Task.CompletedTask;
    }

    private static Task KeepsNonRegisterAddressesSeparate(TestContext t)
    {
        var symbolic = new PointRef(
            "sym", Address.Symbol("DB1.DBD0"), DataType.UInt32, ByteOrder.ABCD, 2);

        var spans = ReadPlanner.Plan(new[]
        {
            Reg("a", "holding", 0),
            symbolic,
            Reg("b", "holding", 1),
        });

        // 符号寻址无法用偏移窗口合并，必须独占一个 span；寄存器那两点仍应合并。
        t.Equal(2, spans.Count, "one window per non-register point plus the merged register window");
        t.Ok(spans.Any(s => s.Points.Count == 1 && s.Points[0].Id == "sym"), "the symbolic point is alone");
        t.Ok(spans.Any(s => s.Points.Count == 2), "the two registers are merged");

        return Task.CompletedTask;
    }

    private static Task CoversEveryPointExactlyOnce(TestContext t)
    {
        var random = new Random(20261105);
        var points = Enumerable.Range(0, 200)
            .Select(i => Reg($"p{i}", i % 3 == 0 ? "holding" : "input", random.Next(0, 400)))
            .ToArray();

        var spans = ReadPlanner.Plan(points);

        var covered = spans.SelectMany(s => s.Points.Select(p => p.Id)).ToArray();
        t.Equal(points.Length, covered.Length, "no point is emitted twice or dropped");
        t.Equal(points.Length, covered.Distinct(StringComparer.Ordinal).Count(), "all ids are distinct");

        foreach (var span in spans)
        {
            var areas = span.Points.Select(p => p.Address.Area).Distinct(StringComparer.OrdinalIgnoreCase).ToArray();
            t.Equal(1, areas.Length, $"window {span.Id} mixes areas: {string.Join(",", areas)}");
        }

        t.Contains(ReadPlanner.Describe(spans), "covering 200 point(s)", "diagnostic text");

        return Task.CompletedTask;
    }
}
