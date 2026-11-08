using Uic.Domain;
using Uic.Obs;

namespace Uic.Tests;

/// <summary>
/// 审计日志（FR-OPS-04）。
///
/// 这里测的是「能不能在重启后仍然回答谁改了什么」：写进去、查得出来、
/// 文件切分与保留不会把最新记录删掉、被断电截断的半行不会毁掉整份文件。
/// </summary>
internal static class AuditTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("audit/entries_are_queryable", EntriesAreQueryable));
        tests.Add(new TestCase("audit/survives_restart", SurvivesRestart));
        tests.Add(new TestCase("audit/rotates_and_prunes", RotatesAndPrunes));
        tests.Add(new TestCase("audit/disabled_writes_nothing", DisabledWritesNothing));
        tests.Add(new TestCase("audit/corrupt_tail_is_ignored", CorruptTailIsIgnored));
        tests.Add(new TestCase("audit/file_date_skips_unrelated_archives", FileDateSkipsUnrelatedArchives));
        tests.Add(new TestCase("audit/paging_walks_newest_first", PagingWalksNewestFirst));
    }

    private static DateTimeOffset From => DateTimeOffset.UtcNow.AddMinutes(-5);
    private static DateTimeOffset To => DateTimeOffset.UtcNow.AddMinutes(5);

    private static AuditQuery All(int limit = 100, int offset = 0)
        => new(null, null, null, null, From, To, limit, offset);

    private static async Task EntriesAreQueryable(TestContext t)
    {
        using var log = new AuditLog(t.TempDir);

        log.Write("point.write", "ok", "bearer", "rest", target: "v1/temp",
            remote: "10.0.0.7", requestId: "req-1", detail: new { value = 42, oldValue = 41 });
        log.Write("config.apply", "ok", "bearer", "rest", target: "v1");
        log.Write("point.write", "failed", "anonymous", "rest", target: "v1/temp");

        var page = await log.QueryAsync(All());

        t.Equal(3, page.Total, "all three records are found");

        var newest = page.Items[0];
        t.Equal("point.write", newest.Action, "the newest record comes first");
        t.Equal("failed", newest.Result, "the result is preserved");
        t.Equal("anonymous", newest.Actor, "the actor is preserved");

        // 按动作 + 结果过滤：排障时最常见的两个维度。
        var writes = await log.QueryAsync(new AuditQuery("point.write", null, null, null, From, To, 10));
        t.Equal(2, writes.Total, "filtering by action works");

        var failed = await log.QueryAsync(new AuditQuery("point.write", null, "failed", null, From, To, 10));
        t.Equal(1, failed.Total, "filtering by result works");

        // detail 必须原样回来：写入新旧值全靠它。
        var detail = page.Items[^1].Detail!;
        t.Equal("42", detail["value"]!.ToJsonString(), "detail survives the round-trip");
        t.Equal("41", detail["oldValue"]!.ToJsonString(), "the old value is recorded too");

        t.Equal("10.0.0.7", page.Items[^1].Remote, "the remote address is recorded");
        t.Equal("req-1", page.Items[^1].RequestId, "the request id links the audit row to the log line");
    }

    private static async Task SurvivesRestart(TestContext t)
    {
        var dir = t.TempDir;
        long first;

        using (var log = new AuditLog(dir))
        {
            first = log.Write("config.apply", "ok", "bearer", "rest", target: "v1").Seq;
        }

        // 新实例模拟进程重启：seq 不能回到 1，否则同一份文件里会出现两个 seq=1。
        using (var again = new AuditLog(dir))
        {
            var second = again.Write("config.rollback", "ok", "bearer", "rest", target: "v0").Seq;
            t.Ok(second > first, $"sequence keeps increasing across restarts ({first} -> {second})");

            var page = await again.QueryAsync(All());
            t.Equal(2, page.Total, "records written before the restart are still queryable");
        }
    }

    private static async Task RotatesAndPrunes(TestContext t)
    {
        var options = new AuditOptions { MaxBytes = 1024, Retain = 2 };

        using var log = new AuditLog(t.TempDir, options);

        for (var i = 0; i < 100; i++)
        {
            log.Write("point.write", "ok", "bearer", "rest", target: $"v1/p{i}",
                detail: new { value = i, note = new string('x', 40) });
        }

        t.Ok(log.Files().Count <= 2, $"only {options.Retain} files are kept, got {log.Files().Count}");

        // 保留策略只能删旧文件：最新的那批必须还在。
        var page = await log.QueryAsync(All(limit: 1000));
        t.Ok(page.Total > 0, "the newest records are still readable");
        t.Equal("v1/p99", page.Items[0].Target, "the last written record is not pruned");
    }

    private static async Task DisabledWritesNothing(TestContext t)
    {
        using var log = new AuditLog(t.TempDir, new AuditOptions { Enabled = false });

        var entry = log.Write("point.write", "ok", "bearer", "rest", target: "v1/temp");

        t.Ok(entry.Seq > 0, "the entry is still handed back to the caller");
        t.Equal(0, log.Files().Count, "nothing is written to disk when auditing is disabled");

        var page = await log.QueryAsync(All());
        t.Equal(0, page.Total, "there is nothing to query");
    }

    private static async Task CorruptTailIsIgnored(TestContext t)
    {
        var dir = t.TempDir;
        string path;

        using (var log = new AuditLog(dir))
        {
            log.Write("point.write", "ok", "bearer", "rest", target: "v1/temp");
            path = log.ActivePath!;
        }

        // 进程被 kill -9 时最后一行可能只写了一半。
        await File.AppendAllTextAsync(path, "{\"ts\":\"2026-01-01T00:00:00Z\",\"action\":", t.Ct);

        using var again = new AuditLog(dir);
        again.Write("config.apply", "ok", "bearer", "rest", target: "v1");

        var page = await again.QueryAsync(All());
        t.Equal(2, page.Total, "the corrupt line is skipped, not fatal");
    }

    private static async Task FileDateSkipsUnrelatedArchives(TestContext t)
    {
        var dir = Path.Combine(t.TempDir, "audit");
        Directory.CreateDirectory(dir);

        // 归档的日期在文件名里；查询范围不覆盖那一天时整份文件跳过。
        var yesterday = DateTimeOffset.UtcNow.AddDays(-1);
        var archive = new AuditEntry(
            99, DateTimeOffset.UtcNow, "archive.old", "ok", "system", "test",
            null, null, null, null);

        File.WriteAllText(
            Path.Combine(dir, $"audit-{yesterday:yyyyMMdd}-120000-000.ndjson"),
            AuditLog.Serialize(archive) + "\n");

        using var log = new AuditLog(t.TempDir);
        log.Write("fresh", "ok", "bearer", "rest");

        var narrow = await log.QueryAsync(All());
        t.Equal(1, narrow.Total, "a file named for another day is skipped");

        var wide = await log.QueryAsync(new AuditQuery(
            null, null, null, null, yesterday.AddDays(-1), To, 100));
        t.Equal(2, wide.Total, "widening the range includes the archive");
    }

    private static async Task PagingWalksNewestFirst(TestContext t)
    {
        using var log = new AuditLog(t.TempDir);

        for (var i = 0; i < 5; i++)
            log.Write("point.write", "ok", "bearer", "rest", target: $"v1/p{i}");

        var first = await log.QueryAsync(All(limit: 2));
        t.Equal(5, first.Total, "the total ignores paging");
        t.Equal(2, first.Items.Count, "one page is bounded");

        var second = await log.QueryAsync(All(limit: 2, offset: 2));
        t.Equal("v1/p2", second.Items[0].Target, "the next page continues where the last one stopped");
    }
}
