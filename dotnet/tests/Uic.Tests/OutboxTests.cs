using Uic.Outbox;
using OutboxQueue = Uic.Outbox.Outbox;

namespace Uic.Tests;

/// <summary>
/// 持久化上报队列。这里断言的是「不丢」与「不无限长」这两条互相拉扯的底线：
/// 成功才推进位点、重启后能重放、超限时丢最旧并计数。
///
/// 这些行为看见时通常已经在事故现场了，所以必须靠用例钉住。
/// </summary>
internal static class OutboxTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("outbox/advances_only_on_success", AdvancesOnlyOnSuccess));
        tests.Add(new TestCase("outbox/replays_after_restart", ReplaysAfterRestart));
        tests.Add(new TestCase("outbox/batches_respect_batch_size", BatchesRespectBatchSize));
        tests.Add(new TestCase("outbox/partitions_are_independent", PartitionsAreIndependent));
        tests.Add(new TestCase("outbox/drops_oldest_when_full", DropsOldestWhenFull));
        tests.Add(new TestCase("outbox/dropped_last_hour_falls_out_of_the_window", DroppedLastHourFallsOutOfTheWindow));
        tests.Add(new TestCase("outbox/drops_expired_records", DropsExpiredRecords));
        tests.Add(new TestCase("outbox/tolerates_a_torn_last_line", ToleratesTornLastLine));
        tests.Add(new TestCase("outbox/stats_report_pending_and_delivered", StatsReportPendingAndDelivered));
        tests.Add(new TestCase("outbox/disk_full_keeps_records_pending", DiskFullKeepsRecordsPending));
        tests.Add(new TestCase("outbox/compaction_does_not_reuse_ids", CompactionDoesNotReuseIds));
        tests.Add(new TestCase("outbox/a_failed_delivery_does_not_redeliver_earlier_records",
            AFailedDeliveryDoesNotRedeliverEarlierRecords));
    }

    /// <summary>可编程的投递目标：按脚本成功或失败，并记录每次拿到的批次。</summary>
    private sealed class RecordingSink : IOutboxSink
    {
        public RecordingSink(string name) => Name = name;

        public string Name { get; }

        public List<string[]> Batches { get; } = new();

        /// <summary>为 true 时下一次投递抛异常，用来模拟对端不可用。</summary>
        public bool Fail { get; set; }

        public Task DeliverAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
        {
            // 先判失败再记账：失败的尝试不算「送达」，否则测试会把重试数成重复投递。
            if (Fail) throw new InvalidOperationException($"{Name} is down");

            Batches.Add(batch.Select(r => r.Payload).ToArray());
            return Task.CompletedTask;
        }

        // RecordingSink 整批一起送达，直接报批次末条。
        public Task<long> DeliverPartialAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
            => DeliverAsync(batch, ct).ContinueWith(_ => batch.Count > 0 ? batch[^1].Id : -1L);

        public string[] Delivered() => Batches.SelectMany(b => b).ToArray();
    }

    private static OutboxOptions FastOptions(int batchSize = 64, int maxRecords = 100_000, TimeSpan? ttl = null) => new()
    {
        BatchSize = batchSize,
        MaxRecords = maxRecords,
        Ttl = ttl ?? TimeSpan.FromHours(24),
        FlushInterval = TimeSpan.FromMilliseconds(20),
        CompactThresholdBytes = long.MaxValue,
    };

    private static Task AdvancesOnlyOnSuccess(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            await using var queue = new OutboxQueue(t.TempDir, FastOptions());
            var sink = new RecordingSink("journal");
            queue.RegisterSink(sink);

            queue.Enqueue("journal", "one");
            queue.Enqueue("journal", "two");

            sink.Fail = true;
            await t.ThrowsAsync<InvalidOperationException>(
                () => queue.FlushAsync(t.Ct), "a failing sink must surface the failure");

            // 整批失败时位点不得推进：数据必须还在。
            t.Equal(2L, queue.Stats().Pending, "both records are still pending after a failure");

            sink.Fail = false;
            await queue.FlushAsync(t.Ct);

            t.Equal(0L, queue.Stats().Pending, "queue drains once delivery succeeds");
            t.Equal(2, sink.Delivered().Length, "both records are delivered");
            t.Equal("one", sink.Delivered()[0], "order is preserved");
            t.Equal("two", sink.Delivered()[1], "order is preserved");
        });
    }

    private static Task ReplaysAfterRestart(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            var dir = t.TempDir;

            await using (var first = new OutboxQueue(dir, FastOptions()))
            {
                first.Enqueue("journal", "before-crash-1");
                first.Enqueue("journal", "before-crash-2");
                // 故意不 Flush：模拟进程被 kill 的时刻。
            }

            await using var second = new OutboxQueue(dir, FastOptions());
            var sink = new RecordingSink("journal");
            second.RegisterSink(sink);

            t.Equal(2L, second.Stats().Pending, "records survive a restart");

            await second.FlushAsync(t.Ct);

            t.Equal("before-crash-1", sink.Delivered()[0], "replay keeps the original order");
            t.Equal("before-crash-2", sink.Delivered()[1], "replay keeps the original order");
            t.Equal(0L, second.Stats().Pending, "queue is empty after replay");
        });
    }

    private static Task BatchesRespectBatchSize(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            await using var queue = new OutboxQueue(t.TempDir, FastOptions(batchSize: 2));
            var sink = new RecordingSink("journal");
            queue.RegisterSink(sink);

            for (var i = 1; i <= 5; i++) queue.Enqueue("journal", "r" + i);

            await queue.FlushAsync(t.Ct);

            // 5 条、每批 2 条 → 2/2/1。批次大小是「一次网络往返的上限」，不是建议。
            t.Equal(3, sink.Batches.Count, "five records split into three batches");
            t.Equal(2, sink.Batches[0].Length, "first batch is full");
            t.Equal(2, sink.Batches[1].Length, "second batch is full");
            t.Equal(1, sink.Batches[2].Length, "third batch carries the remainder");
        });
    }

    private static Task PartitionsAreIndependent(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            await using var queue = new OutboxQueue(t.TempDir, FastOptions());
            var good = new RecordingSink("mqtt");
            var bad = new RecordingSink("journal") { Fail = true };

            queue.RegisterSink(good);
            queue.RegisterSink(bad);

            queue.Enqueue("mqtt", "m1");
            queue.Enqueue("journal", "j1");

            await t.ThrowsAsync<InvalidOperationException>(
                () => queue.FlushAsync(t.Ct), "the broken sink reports its failure");

            // 一个通道挂掉不能拖住另一个：这是分区存在的唯一理由。
            t.Equal(1, good.Delivered().Length, "the healthy sink delivered its record");
            t.Equal("m1", good.Delivered()[0], "correct payload");

            bad.Fail = false;
            await queue.FlushAsync(t.Ct);

            t.Equal(0L, queue.Stats().Pending, "the lagging partition catches up");
        });
    }

    private static Task DropsOldestWhenFull(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            await using var queue = new OutboxQueue(t.TempDir, FastOptions(maxRecords: 3));

            for (var i = 1; i <= 5; i++) queue.Enqueue("journal", "r" + i);

            var stats = queue.Stats();
            t.Equal(3L, stats.Pending, "the queue is capped");
            t.Equal(2L, stats.DroppedTotal, "drops are counted, not silently swallowed");

            // 丢的是最旧的：现场更关心「刚才发生了什么」。
            var sink = new RecordingSink("journal");
            queue.RegisterSink(sink);
            await queue.FlushAsync(t.Ct);
            t.Equal("r3", sink.Delivered()[0], "oldest records are the ones dropped");
        });
    }

    private static Task DropsExpiredRecords(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            await using var queue = new OutboxQueue(t.TempDir, FastOptions(ttl: TimeSpan.FromMilliseconds(50)));

            queue.Enqueue("journal", "stale");
            await Task.Delay(120, t.Ct);
            queue.Enqueue("journal", "fresh");

            var stats = queue.Stats();
            t.Equal(1L, stats.Pending, "the expired record is gone");
            t.Equal(1L, stats.DroppedTotal, "expiry counts as a drop");

            var sink = new RecordingSink("journal");
            queue.RegisterSink(sink);
            await queue.FlushAsync(t.Ct);

            t.Equal("fresh", sink.Delivered()[0], "only the fresh record is delivered");
        });
    }

    private static Task ToleratesTornLastLine(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            var dir = t.TempDir;

            await using (var first = new OutboxQueue(dir, FastOptions()))
            {
                first.Enqueue("journal", "intact");
            }

            // 掉电时只写了半行：加载必须跳过它，而不是让整个队列起不来。
            var file = Directory.EnumerateFiles(Path.Combine(dir, "outbox"), "records.ndjson", SearchOption.AllDirectories)
                .Single();
            await File.AppendAllTextAsync(file, "{\"id\":2,\"payl", t.Ct);

            await using var second = new OutboxQueue(dir, FastOptions());
            var sink = new RecordingSink("journal");
            second.RegisterSink(sink);

            t.Equal(1L, second.Stats().Pending, "the intact record is kept");

            await second.FlushAsync(t.Ct);
            t.Equal("intact", sink.Delivered()[0], "the intact record is delivered");
        });
    }

    private static Task StatsReportPendingAndDelivered(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            await using var queue = new OutboxQueue(t.TempDir, FastOptions());
            var sink = new RecordingSink("journal");
            queue.RegisterSink(sink);

            t.Equal(0L, queue.Stats().Pending, "a fresh queue is empty");
            t.Equal(0, queue.Stats().Partitions, "no partitions before the first enqueue");

            queue.Enqueue("journal", "x");
            t.Equal(1, queue.Stats().Partitions, "one partition per sink");
            t.Ok(queue.Stats().Bytes > 0, "byte accounting is non-zero");

            await queue.FlushAsync(t.Ct);

            // Delivered 是累计量，Pending 是瞬时量：监控面板两个都要。
            t.Equal(1L, queue.Stats().Delivered, "delivered counter");
            t.Equal(0L, queue.Stats().Pending, "pending counter");
        });
    }

    /// <summary>
    /// 健康接口报的是「最近一小时丢弃数」，不是累计值。
    ///
    /// 累计值只会变大：一次磁盘打满之后，`droppedLastHour` 会永远停在那个数上，
    /// 运维没法从接口回答「现在还在丢吗」——而这正是健康检查唯一的用途。
    /// 窗口必须按单调时钟走：墙钟跳变既不能把窗口瞬间清空，也不能让它停住。
    /// </summary>
    private static Task DroppedLastHourFallsOutOfTheWindow(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            var clock = new FakeClock();
            await using var queue = new OutboxQueue(t.TempDir, FastOptions(maxRecords: 3), clock);

            for (var i = 1; i <= 5; i++) queue.Enqueue("journal", "r" + i);

            var fresh = queue.Stats();
            t.Equal(2L, fresh.DroppedTotal, "the total counts every drop");
            t.Equal(2L, fresh.DroppedLastHour, "fresh drops are inside the window");

            // 半小时后仍在窗口内。只拨单调时钟：窗口不依赖墙钟。
            clock.Advance(TimeSpan.Zero, TimeSpan.FromMinutes(30));
            t.Equal(2L, queue.Stats().DroppedLastHour, "drops stay in the window for an hour");

            // 再走 31 分钟，最初始的两条滑出窗口；累计值不变。
            clock.Advance(TimeSpan.Zero, TimeSpan.FromMinutes(31));
            var later = queue.Stats();
            t.Equal(2L, later.DroppedTotal, "the total keeps the history");
            t.Equal(0L, later.DroppedLastHour, "the window forgets drops older than an hour");

            // 窗口不会因为一小时的空档就永久失效：新的丢弃必须重新进来。
            queue.Enqueue("journal", "r6");
            t.Equal(1L, queue.Stats().DroppedLastHour, "drops after the window still count");
        });
    }


    ///
    /// 注入方式是真的：把 journal 的目标文件软链到 <c>/dev/full</c>，内核在写入时返回
    /// ENOSPC。不用假 sink 模拟异常，是因为这里要验证的正是「真实 IOException 的形状」——
    /// <see cref="Uic.Runtime.SampleFanout"/> 只捕获 IOException，其余类型会漏出去。
    ///
    /// 顺带钉住退出路径：关机预算 10 秒里刷盘失败不能抛，否则后面的清理全被顶掉。
    /// </summary>
    private static Task DiskFullKeepsRecordsPending(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            // 没有 /dev/full 的环境（macOS、Windows、精简容器）跳过而不是假装通过。
            if (!OperatingSystem.IsLinux() || !File.Exists("/dev/full"))
                throw new SkipException("/dev/full is not available on this platform");

            var dir = t.TempDir;
            var journal = Path.Combine(dir, "journal.ndjson");
            File.CreateSymbolicLink(journal, "/dev/full");

            await using var queue = new OutboxQueue(dir, FastOptions());
            var broken = new JournalSink(dir);
            queue.RegisterSink(broken);

            queue.Enqueue("journal", "j1");
            queue.Enqueue("journal", "j2");

            var ex = await t.ThrowsAsync<IOException>(
                () => queue.FlushAsync(t.Ct),
                "ENOSPC must surface as an IOException, that is what the caller catches");
            t.Contains(ex.Message, "No space left", "the message names the real cause");

            // 整批失败 → 位点不推进。这里若变成 1 条或 0 条，就是静默丢数据。
            t.Equal(2L, queue.Stats().Pending, "both records stay pending after ENOSPC");
            t.Equal(0L, queue.Stats().Delivered, "nothing is counted as delivered");

            // 退出路径：刷不出去的缓冲区只能在关闭时丢弃，不能抛。
            await broken.DisposeAsync();
            queue.RemoveSink("journal");

            // 磁盘恢复：换成真文件后重新挂 sink，积压要原样补投。
            File.Delete(journal);
            var healthy = new JournalSink(dir);
            queue.RegisterSink(healthy);

            await queue.FlushAsync(t.Ct);

            t.Equal(0L, queue.Stats().Pending, "the backlog drains once the disk is back");
            t.Equal(2L, queue.Stats().Delivered, "both records were delivered, none lost twice");
            var lines = File.ReadAllLines(journal);
            t.Equal(2, lines.Length, "the journal has exactly the two records");
            t.Equal("j1", lines[0], "order is preserved");
            t.Equal("j2", lines[1], "order is preserved");
        });
    }

    /// <summary>
    /// 压缩过的分区在重启后必须继续往上发号。
    ///
    /// 压缩会把 <c>records.ndjson</c> 清空，而确认位点 <c>acked.id</c> 保留着。
    /// 若发号器只从数据文件里的记录恢复，清空后它就从 1 重新开始；重启加载时
    /// 这些「新」记录的 id 都小于等于已确认位点，会被当成早就投递过的行直接跳过——
    /// <c>Enqueue</c> 明明成功写入磁盘，重启后却什么都不剩。
    ///
    /// 这是最糟的一类丢失：调用方收到了受理，数据也落过盘，却在重启时凭 id 被判死刑，
    /// 没有任何错误或计数。所以发号器的下界必须同时考虑确认位点。
    /// </summary>
    private static Task CompactionDoesNotReuseIds(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            var dir = t.TempDir;
            var options = FastOptions() with { CompactThresholdBytes = 0 };

            await using (var first = new OutboxQueue(dir, options))
            {
                var sink = new RecordingSink("journal");
                first.RegisterSink(sink);

                for (var i = 1; i <= 3; i++) first.Enqueue("journal", "r" + i);
                await first.FlushAsync(t.Ct);

                t.Equal(3L, sink.Delivered().Length, "the first three records are delivered");
                t.Equal(0L, first.Stats().Pending, "the queue is empty");
            }

            // 压缩后数据文件为空、位点是 3。此时入队一条，然后模拟掉电（不 Flush）。
            await using (var second = new OutboxQueue(dir, options))
            {
                second.Enqueue("journal", "after-compaction");
            }

            await using var third = new OutboxQueue(dir, options);
            var replay = new RecordingSink("journal");
            third.RegisterSink(replay);

            t.Equal(1L, third.Stats().Pending,
                "a record enqueued after compaction survives a restart");

            await third.FlushAsync(t.Ct);
            t.Equal("after-compaction", replay.Delivered()[0],
                "the record is delivered instead of being filtered out as already-acked");
        });
    }

    /// <summary>
    /// 逐条投递的 sink 中途失败时，已经投递出去的记录不得被重发。
    ///
    /// `Outbox` 的语义是「整批成功才推进位点」——这是 at-least-once 的正确选择：
    /// 部分成功不能推进，否则会把没发出去的记录当成已送达丢掉。但「整批」
    /// 说的是队列这一侧的分批，不是 sink 内部的循环。像 `MqttSink` 那样逐条
    /// `PublishAsync` 的 sink，一条失败会把同一批里**已经发到 broker** 的几条
    /// 一起拉回来重发：broker 的 retained/订阅者会收到重复，而队列因为
    /// 位点没推进，下次 flush 必然从头重发这一批。
    ///
    /// 所以 sink 必须自己报告「本批实际投递到哪一条」，让队列只回退到那条之前。
    /// 用例用「前 3 条成功、第 4 条失败」的 sink 钉住：修复前第 1..3 条在重试后
    /// 会被再发一次（FAIL）；修复后只有第 4 条失败、其余全部补投且不重发。
    /// </summary>
    private static Task AFailedDeliveryDoesNotRedeliverEarlierRecords(TestContext t)
    {
        return RunAsync(t, async () =>
        {
            await using var queue = new OutboxQueue(t.TempDir, FastOptions(batchSize: 8));
            var sink = new FailAfterSink("journal", succeedThrough: 3);
            queue.RegisterSink(sink);

            for (var i = 1; i <= 5; i++) queue.Enqueue("journal", "r" + i);

            // 第 4 条失败：前 3 条已投递，第 4 条抛 PartialDeliveryException，第 5 条没轮到。
            await t.ThrowsAsync<PartialDeliveryException>(
                () => queue.FlushAsync(t.Ct), "a mid-batch failure must surface");

            // 前 3 条成功送达，第 4 条尝试到一半失败，第 5 条未轮到。
            t.Equal(4, sink.Attempted, "the first three records reached the sink and the fourth was attempted");

            // 恢复后重试：前 3 条不得重发，第 4、5 条补投，队列清空。
            sink.FailAfter = int.MaxValue;
            await queue.FlushAsync(t.Ct);

            t.Equal(0L, queue.Stats().Pending, "queue drains once delivery succeeds");
            t.Equal(2, sink.DeliveredThisFlush, "already-delivered records are not retried");
            t.Equal("r4", sink.Delivered()[3], "the record that failed is delivered on retry");
            t.Equal("r5", sink.Delivered()[4], "the record after the failure is delivered");
        });
    }

    /// <summary>
    /// 逐条投递的 sink：把「投递到哪一条」如实报回队列（<see cref="DeliverPartialAsync"/>），
    /// 这样队列能只回退失败点之前，而不是整批重发。
    /// </summary>
    private sealed class FailAfterSink : IOutboxSink
    {
        private readonly int _succeedThrough;

        public FailAfterSink(string name, int succeedThrough)
        {
            Name = name;
            _succeedThrough = succeedThrough;
        }

        public string Name { get; }

        /// <summary>失败点（第几条，从 1 计）。调大即「全部成功」。</summary>
        public int FailAfter { get; set; }

        /// <summary>本进程累计投递尝试次数（含失败那次）。</summary>
        public int Attempted { get; private set; }

        /// <summary>最近一次 flush 里成功送达的记录数。</summary>
        public int DeliveredThisFlush { get; private set; }

        public string[] Delivered() => _delivered.ToArray();
        private readonly List<string> _delivered = new();

        public async Task DeliverAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
        {
            await DeliverPartialAsync(batch, ct).ConfigureAwait(false);
        }

        public async Task<long> DeliverPartialAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
        {
            DeliveredThisFlush = 0;
            var lastId = -1L;
            for (var i = 0; i < batch.Count; i++)
            {
                var ordinal = i + 1;
                Attempted++;

                if (ordinal > _succeedThrough && ordinal >= FailAfter)
                {
                    // 从这一条起失败：模拟 broker 在发到一半时断连。
                    // 把已送达的最后一条 id 交给队列，让它只对未送达部分重试。
                    throw new PartialDeliveryException(lastId,
                        $"{Name} failed at record {ordinal}");
                }

                await Task.Yield();
                _delivered.Add(batch[i].Payload);
                lastId = batch[i].Id;
                DeliveredThisFlush++;
            }

            return lastId;
        }
    }

    /// <summary>
    /// 运行器里的用例体是同步签名 + 返回 Task。异步用例统一走这个包装，
    /// 保证 <c>await</c> 期间的异常仍被运行器捕获。
    /// </summary>
    private static Task RunAsync(TestContext t, Func<Task> body) => body();
}
