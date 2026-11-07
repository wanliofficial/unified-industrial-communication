using Uic.Domain;
using Uic.Northbound;

namespace Uic.Tests;

/// <summary>
/// journal 历史查询的读取语义：时间过滤、身份过滤、去重、降采样。
///
/// 这些点在真实现场都会踩到：Outbox 重放会写重复行、掉电会留半行、
/// 前端要的是「每桶一个点」而不是整段原始数据。用例直接断言读取层，
/// 不经过 HTTP，才能把分页之外的责任划清楚。
/// </summary>
internal static class HistoryTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("history/filters_by_time_and_identity", FiltersByTimeAndIdentity));
        tests.Add(new TestCase("history/downsamples_each_point_per_bucket", DownsamplesPerBucket));
        tests.Add(new TestCase("history/dedupes_redelivered_lines", DedupesRedeliveredLines));
        tests.Add(new TestCase("history/missing_journal_is_empty", MissingJournalIsEmpty));
        tests.Add(new TestCase("history/skips_malformed_lines", SkipsMalformedLines));
        tests.Add(new TestCase("history/handles_lines_across_read_buffer", HandlesLinesAcrossReadBuffer));
    }

    private static Task FiltersByTimeAndIdentity(TestContext t)
    {
        var dir = t.TempDir;
        var t0 = new DateTimeOffset(2026, 11, 7, 8, 0, 0, TimeSpan.Zero);

        WriteJournal(dir,
            Sample("devA", "p1", 1, t0.AddSeconds(10), 1),
            Sample("devA", "p1", 2, t0.AddMinutes(1), 2),
            Sample("devB", "p1", 3, t0.AddMinutes(2), 1),
            Sample("devA", "p2", 4, t0.AddMinutes(3), 1),
            Sample("devA", "p1", 5, t0.AddMinutes(10), 3));

        return QueryAndAssert(t, dir, new HistoryQuery(
                null, null, t0.AddSeconds(30), t0.AddMinutes(5), null),
            items =>
            {
                t.Equal(3, items.Count, "the 10-second and 10-minute samples fall outside the range");
                t.Equal("devA", items[0].Device, "results are sorted by collectedAt");
                t.Equal("devB", items[1].Device, "the later sample follows");
                t.Equal("p2", items[2].Point, "point identity is preserved");
            },
            _ => { });
    }

    private static Task DownsamplesPerBucket(TestContext t)
    {
        var dir = t.TempDir;
        var t0 = new DateTimeOffset(2026, 11, 7, 8, 0, 0, TimeSpan.Zero);

        // 第一个桶里有 3 条（10s/20s/50s），第二个桶里只有 61s 那一条。
        // 降采样应该留下每个桶的最后一条，而不是平均值或第一条。
        WriteJournal(dir,
            Sample("dev", "p", 1, t0.AddSeconds(10), 10),
            Sample("dev", "p", 2, t0.AddSeconds(20), 20),
            Sample("dev", "p", 3, t0.AddSeconds(50), 50),
            Sample("dev", "p", 4, t0.AddSeconds(61), 61));

        return QueryAndAssert(t, dir, new HistoryQuery(
                null, null, t0, t0.AddMinutes(5), TimeSpan.FromMinutes(1)),
            items =>
            {
                t.Equal(2, items.Count, "one sample per minute bucket");
                t.Equal(50d, items[0].Value.Num!.Value, "the last sample in the first bucket wins");
                t.Equal(61d, items[1].Value.Num!.Value, "the second bucket keeps its only sample");
            },
            _ => { });
    }

    private static Task DedupesRedeliveredLines(TestContext t)
    {
        var dir = t.TempDir;
        var t0 = new DateTimeOffset(2026, 11, 7, 8, 0, 0, TimeSpan.Zero);
        var sample = Sample("dev", "p", 1, t0, 7);

        // 同一行写两次，模拟 Outbox 重启后重放同一批记录。
        WriteJournal(dir, sample, sample);

        return QueryAndAssert(t, dir, new HistoryQuery(null, null, t0.AddMinutes(-1), t0.AddMinutes(1), null),
            items => t.Equal(1, items.Count, "the same (device, point, seq) is delivered twice"),
            _ => { });
    }

    private static Task MissingJournalIsEmpty(TestContext t)
    {
        return QueryAndAssert(t, t.TempDir,
            new HistoryQuery(null, null, DateTimeOffset.UtcNow.AddHours(-1), DateTimeOffset.UtcNow, null),
            items => t.Equal(0, items.Count, "a gateway without journal has no history"),
            total => t.Equal(0, total, "the total is zero rather than an error"));
    }

    private static Task SkipsMalformedLines(TestContext t)
    {
        var dir = t.TempDir;
        var t0 = new DateTimeOffset(2026, 11, 7, 8, 0, 0, TimeSpan.Zero);

        var path = JournalPath(dir);
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);

        // 中间夹一条半行 JSON 和一条空行：查询要继续，不能整次失败。
        File.WriteAllLines(path, new[]
        {
            ApiJson.Serialize(SampleDto.From(Sample("dev", "p", 1, t0, 1))),
            "{\"device\":\"dev\",\"point\":",
            string.Empty,
            ApiJson.Serialize(SampleDto.From(Sample("dev", "p", 2, t0.AddSeconds(1), 2))),
        });

        return QueryAndAssert(t, dir, new HistoryQuery(null, null, t0.AddMinutes(-1), t0.AddMinutes(1), null),
            items => t.Equal(2, items.Count, "the valid lines on both sides of the broken one are kept"),
            _ => { });
    }

    private static Task HandlesLinesAcrossReadBuffer(TestContext t)
    {
        var dir = t.TempDir;
        var t0 = new DateTimeOffset(2026, 11, 7, 8, 0, 0, TimeSpan.Zero);

        // 一行超过读取缓冲区（256 KiB），并且用 CRLF 结尾：
        // 跨缓冲区拼接和回车裁剪都必须正确，否则真实文件里每隔 256 KiB 就会丢一行。
        var unit = new string('x', 300_000);
        var sample = new SampleDto(
            "dev", "p", DataValue.Number(DataType.Float64, 1.5), "good", unit,
            null, t0, 1, null);

        var path = JournalPath(dir);
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);
        File.WriteAllText(path, ApiJson.Serialize(sample) + "\r\n");

        return QueryAndAssert(t, dir, new HistoryQuery(null, null, t0.AddMinutes(-1), t0.AddMinutes(1), null),
            items =>
            {
                t.Equal(1, items.Count, "the long line is read exactly once");
                t.Equal(unit.Length, items[0].Unit?.Length ?? -1, "the whole unit string survives");
            },
            _ => { });
    }

    // ------------------------------------------------------------------
    // 夹具
    // ------------------------------------------------------------------

    private static async Task QueryAndAssert(
        TestContext t,
        string dir,
        HistoryQuery query,
        Action<IReadOnlyList<SampleDto>> assertItems,
        Action<int> assertTotal)
    {
        var result = await new JournalHistory(dir).QueryAsync(query, t.Ct);
        assertItems(result.Items);
        assertTotal(result.Total);
    }

    private static Sample Sample(
        string device, string point, ulong seq, DateTimeOffset at, double value)
        => new(
            device,
            point,
            DataValue.Number(DataType.Float64, value),
            Quality.Good,
            "°C",
            null,
            at,
            seq);

    private static void WriteJournal(string dataDir, params Sample[] samples)
    {
        var path = JournalPath(dataDir);
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);

        File.WriteAllLines(path, samples.Select(s => ApiJson.Serialize(SampleDto.From(s))));
    }

    private static string JournalPath(string dataDir)
        => Path.Combine(dataDir, "journal", "journal.ndjson");
}
