using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Engine;
using Uic.Obs;
using Uic.Runtime;
using OutboxQueue = Uic.Outbox.Outbox;
using Uic.Outbox;

namespace Uic.Tests;

/// <summary>
/// 环境级故障注入：磁盘满 / 时钟跳变 / 进程假死。
///
/// 这三类此前一直记在「未自动化」里，理由是「需要环境」。实际上它们各自有一个
/// 诚实的注入点，不需要 root、也不需要真的把磁盘写满：
/// - 磁盘满：把目标文件软链到 <c>/dev/full</c>，内核写时返回 ENOSPC（见 OutboxTests）；
/// - 时钟跳变：注入可拨的 <see cref="TimeProvider"/>，让墙钟与单调时钟分开走；
/// - 进程假死：驱动卡在一个不响应取消的 Task 上，actor 就真的不再产出样本。
///
/// 三者都不依赖 sleep 到阈值，因此不会因为机器忙而假失败。
/// </summary>
internal static class FaultTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("fault/collection_stall_is_reported", CollectionStallIsReported));
        tests.Add(new TestCase("fault/clock_jump_does_not_fake_a_stall", ClockJumpDoesNotFakeStall));
        tests.Add(new TestCase("fault/stall_threshold_follows_the_slowest_point", StallThresholdFollowsSlowestPoint));
        tests.Add(new TestCase("fault/outbox_ttl_ignores_a_clock_jump", OutboxTtlIgnoresClockJump));
        tests.Add(new TestCase("fault/outbox_permission_error_is_counted", OutboxPermissionErrorIsCounted));
    }

    /// <summary>
    /// 可拨的时钟：墙钟与单调时间分开推进。
    ///
    /// 现场这两者由不同机制提供——墙钟会被 NTP/手工校时拨动，单调时钟只随真实流逝走。
    /// 把它们绑在一起就永远分不开「校时」与「真的不动」。实现在
    /// <see cref="FakeClock"/>，几个测试文件共用。
    /// </summary>

    /// <summary>
    /// 一台「假死」的设备：第一次读到值，之后卡住且**不响应取消**。
    ///
    /// 这就是进程假死与「读超时」的区别：超时是链路还活着、只是慢；假死是采集线程
    /// 再也不回来，所以 actor 一直停在 Collecting、点位永远是最旧的那个样本。
    /// </summary>
    private sealed class WedgeDriver : IDriver
    {
        private readonly TaskCompletionSource _release = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private int _calls;

        public DriverMeta Meta { get; } = new()
        {
            Id = "wedge",
            Name = "假死驱动",
            Transports = new[] { "none" },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        /// <summary>放行卡住的读，让用例能正常收尾。</summary>
        public void Release() => _release.TrySetResult();

        private sealed class Conn(WedgeDriver owner) : IConn
        {
            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public async Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                if (Interlocked.Increment(ref owner._calls) > 1)
                {
                    // 刻意不看 ct：真实的假死线程也不会因为「有人喊停」就回来。
                    await owner._release.Task.ConfigureAwait(false);
                }

                return spans.SelectMany(s => s.Points)
                    .Select(p => new RawSample(p.Id, DataValue.Number(p.DataType, 1), Quality.Good, null))
                    .ToArray();
            }

            public Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
                => Task.FromResult<IReadOnlyList<WriteResult>>(Array.Empty<WriteResult>());

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static Task<(Scheduler Engine, DeviceActor Actor, WedgeDriver Driver, FakeClock Clock)> StartWedgeAsync(
        TestContext t)
        => StartWedgeAsync(t, TimeSpan.FromMilliseconds(100));

    private static async Task<(Scheduler Engine, DeviceActor Actor, WedgeDriver Driver, FakeClock Clock)> StartWedgeAsync(
        TestContext t, TimeSpan pointInterval)
    {
        var registry = new DriverRegistry();
        var driver = new WedgeDriver();
        registry.Register(driver);

        var config = new ConfigDocument
        {
            SchemaVersion = 1,
            Site = "fault-site",
            Channels =
            {
                new Channel
                {
                    Id = "ch",
                    Protocol = "wedge",
                    Transport = "none",
                    MaxConcurrency = 1,
                    Timeout = TimeSpan.FromMilliseconds(200),
                    ConnectTimeout = TimeSpan.FromMilliseconds(500),
                },
            },
            Devices =
            {
                new Device
                {
                    Id = "d1",
                    ChannelId = "ch",
                    Interval = pointInterval,
                    Timeout = TimeSpan.FromMilliseconds(200),
                    Retries = 0,
                    Points =
                    {
                        new Point
                        {
                            Id = "p1",
                            Address = Address.Register("holding", 0),
                            DataType = DataType.UInt16,
                            Interval = pointInterval,
                        },
                    },
                },
            },
        };

        var engine = new Scheduler(
            registry, new PointCache(), NullLoggerFactory.Instance,
            new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

        engine.Start(config);

        // 等第一个样本进缓存：之后驱动卡死，_points 就只剩这一个。
        var clock = System.Diagnostics.Stopwatch.StartNew();
        while (engine.Cache.ForDevice("d1").Count == 0 && clock.Elapsed < TimeSpan.FromSeconds(10))
            await Task.Delay(10, t.Ct);

        var actor = engine.Find("d1") ?? throw new CheckFailedException("the device actor was not started");
        return (engine, actor, driver, new FakeClock());
    }

    /// <summary>
    /// 进程假死：设备一直报 Collecting，但样本不再更新 → 必须被看门狗指出来。
    ///
    /// 「不更新」由墙钟与单调时钟同时推进来证明：真实时间确实过去了，样本却还是旧的。
    /// 这是运维唯一的早期信号——点位质量仍然是 Good，界面看起来一切正常。
    /// </summary>
    private static async Task CollectionStallIsReported(TestContext t)
    {
        var (engine, actor, driver, clock) = await StartWedgeAsync(t);
        var metrics = new MetricsRegistry();

        await using var watcher = new DeviceStateWatcher(engine, () => null, NullLogger.Instance, metrics)
        {
            MinimumStaleAge = TimeSpan.FromSeconds(1),
            Time = clock,
        };

        var labels = new Dictionary<string, string> { ["device"] = "d1" };

        t.Equal(RuntimeState.Collecting, actor.State, "a hung device still reports collecting");

        // 第一轮只建立观察起点，不能立刻报警。
        await watcher.TickAsync(t.Ct);
        t.Equal(0d, metrics.CounterValue("uic_watchdog_stale_total", labels),
            "the first observation is the baseline, not an alarm");

        // 真实时间走过 5 秒，样本没动。
        clock.Advance(TimeSpan.FromSeconds(5), TimeSpan.FromSeconds(5));
        await watcher.TickAsync(t.Ct);
        t.Equal(1d, metrics.CounterValue("uic_watchdog_stale_total", labels),
            "a stalled collection is reported");

        // 5 分钟内的重复轮询不再刷屏，否则告警通道会被同一台设备灌满。
        await watcher.TickAsync(t.Ct);
        t.Equal(1d, metrics.CounterValue("uic_watchdog_stale_total", labels),
            "the same stall is not counted twice within the dedup window");

        clock.Advance(TimeSpan.FromMinutes(6), TimeSpan.FromMinutes(6));
        await watcher.TickAsync(t.Ct);
        t.Equal(2d, metrics.CounterValue("uic_watchdog_stale_total", labels),
            "a persistent stall is re-reported after the dedup window");

        driver.Release();
        await engine.DisposeAsync();
    }

    /// <summary>
    /// 停滞阈值取 <c>max(30 秒, 10 × 设备内最慢点位周期)</c>，而不是固定的 30 秒（FR-RUN-10）。
    ///
    /// 为什么单独立一条：另外两条看门狗用例都把 <c>MinimumStaleAge</c> 设成 1 秒好让用例跑得快，
    /// 于是「10 × 周期」那一支从未被执行过——它们证明的是「能报警」，不是「阈值按周期放宽」。
    /// 一台只挂 10 秒慢变量的设备，正常采集时两次样本之间就有 10 秒空档；按 30 秒判，
    /// 任何一次采集稍有延迟都会误报，运维从此不再信这个指标。
    ///
    /// 用默认的 30 秒下限 + 10 秒点位周期，阈值应是 100 秒：停 60 秒不该报，停 110 秒必须报。
    /// 若阈值退化成固定的 30 秒，第一次断言就会失败。
    /// </summary>
    private static async Task StallThresholdFollowsSlowestPoint(TestContext t)
    {
        // 点位周期 10s ⇒ 阈值 = max(30s, 100s) = 100s。用例不等待真实时间，
        // 只拨 FakeClock（墙钟与单调时钟一起拨，模拟「真的过了这么久」）。
        var (engine, actor, driver, clock) = await StartWedgeAsync(t, TimeSpan.FromSeconds(10));
        var metrics = new MetricsRegistry();

        await using var watcher = new DeviceStateWatcher(engine, () => null, NullLogger.Instance, metrics)
        {
            // 保持默认的 30s 下限：要验的正是「周期更长时阈值跟着放宽」。
            Time = clock,
        };

        var labels = new Dictionary<string, string> { ["device"] = "d1" };

        t.Equal(TimeSpan.FromSeconds(10), actor.SlowestInterval,
            "the slowest point drives the threshold, not the fast device cycle");
        t.Equal(RuntimeState.Collecting, actor.State, "the device is collecting");

        // 建立观察起点。
        await watcher.TickAsync(t.Ct);

        // 60 秒：超过 30 秒下限，但还没到 10 × 周期的 100 秒 → 不能报。
        clock.Advance(TimeSpan.FromSeconds(60), TimeSpan.FromSeconds(60));
        await watcher.TickAsync(t.Ct);
        t.Equal(0d, metrics.CounterValue("uic_watchdog_stale_total", labels),
            "60s is past the 30s floor but inside 10 × the 10s point cycle");

        // 再往前 50 秒（累计 110 秒）：越过 100 秒 → 必须报。
        clock.Advance(TimeSpan.FromSeconds(50), TimeSpan.FromSeconds(50));
        await watcher.TickAsync(t.Ct);
        t.Equal(1d, metrics.CounterValue("uic_watchdog_stale_total", labels),
            "past 10 × the slowest cycle the stall is reported");

        driver.Release();
        await engine.DisposeAsync();
    }

    /// <summary>
    /// 时钟跳变：NTP 把墙钟往前拨一小时，而设备其实在正常采集 → 不能报停滞。
    ///
    /// 只看墙钟的实现在这里必然假警：样本的 CollectedAt 还是「一小时前」。
    /// 单调时钟没有跳，因此「我们观察了多久」仍然是零，两个条件不同时成立就不报警。
    /// </summary>
    private static async Task ClockJumpDoesNotFakeStall(TestContext t)
    {
        var (engine, actor, driver, clock) = await StartWedgeAsync(t);
        var metrics = new MetricsRegistry();

        await using var watcher = new DeviceStateWatcher(engine, () => null, NullLogger.Instance, metrics)
        {
            MinimumStaleAge = TimeSpan.FromSeconds(1),
            Time = clock,
        };

        var labels = new Dictionary<string, string> { ["device"] = "d1" };
        t.Equal(RuntimeState.Collecting, actor.State, "the device is collecting");

        await watcher.TickAsync(t.Ct);

        // 只拨墙钟，单调时钟（真实流逝）不动：这正是校时跳变的形状。
        clock.Advance(TimeSpan.FromHours(1), TimeSpan.Zero);
        await watcher.TickAsync(t.Ct);

        t.Equal(0d, metrics.CounterValue("uic_watchdog_stale_total", labels),
            "a wall-clock jump must not be mistaken for a stalled device");

        driver.Release();
        await engine.DisposeAsync();
    }

    /// <summary>
    /// 投递队列的 TTL 也不能用墙钟算：往前拨 30 天会把整条积压判成过期并清空。
    ///
    /// TTL 想表达的是「在盘上放了多久」，这只能用单调时钟度量。
    /// </summary>
    private static async Task OutboxTtlIgnoresClockJump(TestContext t)
    {
        var clock = new FakeClock();
        var options = new OutboxOptions
        {
            Ttl = TimeSpan.FromHours(24),
            FlushInterval = TimeSpan.FromMilliseconds(20),
        };

        await using var queue = new OutboxQueue(t.TempDir, options, clock);

        queue.Enqueue("journal", "old-1");
        queue.Enqueue("journal", "old-2");

        clock.Advance(TimeSpan.FromDays(30), TimeSpan.Zero);
        queue.Enqueue("journal", "after-jump");

        t.Equal(3L, queue.Stats().Pending, "a 30-day clock jump does not expire the backlog");
        t.Equal(0L, queue.Stats().DroppedTotal, "nothing was dropped by the clock jump");

        // 真实时间超过 TTL：这时才应该淘汰，且淘汰的是真的放久了的记录。
        clock.Advance(TimeSpan.Zero, TimeSpan.FromHours(25));
        queue.Enqueue("journal", "after-ttl");

        t.Equal(3L, queue.Stats().DroppedTotal, "records older than the TTL are dropped");
        t.Equal(1L, queue.Stats().Pending, "only the newest record survives");
    }

    /// <summary>
    /// 数据目录不可写：样本必须被计数丢弃，而不是无声消失。
    ///
    /// 这条钉住的是一个真实的坑：<see cref="UnauthorizedAccessException"/> 不继承
    /// <see cref="IOException"/>，只捕获后者会让权限错误把每条样本都静默丢掉，
    /// 而 <c>uic_outbox_enqueue_errors_total</c> 一直是 0。
    /// </summary>
    private static Task OutboxPermissionErrorIsCounted(TestContext t)
    {
        // 用 IsWindows 而不是「非 Linux/非 macOS」：分析器只认这种形状的守卫，
        // 换成反向判断就退化成 CA1416 警告。
        if (OperatingSystem.IsWindows())
            throw new SkipException("chmod-based permission injection needs a POSIX filesystem");

        var outboxRoot = Path.Combine(t.TempDir, "outbox");
        var queue = new OutboxQueue(t.TempDir);

        var cache = new PointCache();
        var metrics = new MetricsRegistry();
        using var fanout = new SampleFanout(cache, queue, metrics, new[] { "journal" });
        fanout.Start();

        try
        {
            // 只读目录：分区子目录建不出来，Enqueue 只能抛 UnauthorizedAccessException。
            File.SetUnixFileMode(outboxRoot,
                UnixFileMode.UserRead | UnixFileMode.UserExecute);

            cache.Publish(new Sample(
                "d1", "p1", DataValue.Number(DataType.UInt16, 7), Quality.Good,
                null, null, DateTimeOffset.UtcNow, 1), deadband: null);
        }
        finally
        {
            // 恢复权限，否则临时目录删不掉（Cleanup 只吞 IOException）。
            File.SetUnixFileMode(outboxRoot,
                UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);
        }

        var labels = new Dictionary<string, string> { ["sink"] = "journal" };
        t.Equal(1d, metrics.CounterValue("uic_outbox_enqueue_errors_total", labels),
            "a permission error is counted, not silently swallowed");

        return Task.CompletedTask;
    }
}
