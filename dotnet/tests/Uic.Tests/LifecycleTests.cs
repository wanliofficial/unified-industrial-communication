using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Engine;
using Uic.Obs;
using Uic.Runtime;

namespace Uic.Tests;

/// <summary>
/// 设备下线后的状态清理。
///
/// 热更新删掉一台设备时，运行态之外还留着三份副本：补发用的上一状态、停滞告警的
/// 去重时刻、以及 broker 上 retained 的 <c>$state</c>。前两份不清理会随反复改配置
/// 无界增长，并且让「删了又加回来」的设备既不重推状态、也吞掉第一轮停滞告警；
/// 第三份会让新订阅者读到一台配置里已经不存在的设备。
///
/// 这三条都不是崩溃级问题，但都属于「监控在撒谎」，而监控撒谎比没有监控更难查——
/// 运维会照着一个不存在的设备去现场，或者反过来，一台真的卡死的设备因为
/// 命中了一个早已过期的去重窗口而一声不响。
/// </summary>
internal static class LifecycleTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("lifecycle/removed_device_is_forgotten", RemovedDeviceIsForgotten));
        tests.Add(new TestCase("lifecycle/readded_device_republishes_its_state", ReaddedDeviceRepublishesItsState));
        tests.Add(new TestCase("lifecycle/hung_read_does_not_block_shutdown", HungReadDoesNotBlockShutdown));
    }

    private static ConfigDocument ConfigWith(params string[] deviceIds) => new()
    {
        SchemaVersion = 1,
        Site = "lifecycle-site",
        Channels =
        {
            new Channel
            {
                Id = "ch",
                Protocol = "noop",
                Transport = "none",
                MaxConcurrency = 1,
            },
        },
        Devices = deviceIds.Select(id => new Device
        {
            Id = id,
            ChannelId = "ch",
            Interval = TimeSpan.FromMilliseconds(100),
            Points =
            {
                new Point
                {
                    Id = "p1",
                    Address = Address.Register("holding", 0),
                    DataType = DataType.UInt16,
                    Interval = TimeSpan.FromMilliseconds(100),
                },
            },
        }).ToList(),
    };

    /// <summary>
    /// 设备被下线后，不再有任何观测状态为它保留——包括停滞告警的去重时刻。
    ///
    /// 用「首轮后假死」的驱动让两台设备真的停滞，这样去重表里才有条目；
    /// 只验证字典被清空的话，看不出「删了又加回来会吞掉第一轮告警」这个后果。
    /// </summary>
    private static async Task RemovedDeviceIsForgotten(TestContext t)
    {
        var registry = new DriverRegistry();
        var driver = new WedgeAfterFirstReadDriver();
        registry.Register(driver);

        var clock = new FakeClock();
        await using var engine = new Scheduler(
            registry, new PointCache(), NullLoggerFactory.Instance,
            new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

        engine.Start(ConfigWith("d1", "d2"));

        var metrics = new MetricsRegistry();
        await using var watcher = new DeviceStateWatcher(engine, () => null, NullLogger.Instance, metrics)
        {
            MinimumStaleAge = TimeSpan.FromSeconds(1),
            Time = clock,
        };

        await WaitForSamples(t, engine, 2);

        // 第一轮只建立观察起点，不能立刻报警。
        await watcher.TickAsync(t.Ct);
        t.Equal(2, watcher.TrackedDeviceCount, "both devices are tracked");

        // 真实时间走过 5 秒，两台设备都不再产出样本。
        clock.Advance(TimeSpan.FromSeconds(5), TimeSpan.FromSeconds(5));
        await watcher.TickAsync(t.Ct);

        // 计数器带 device 标签：用 null 查只会得到 0，看不出到底报没报。
        t.Equal(1d, StaleCount(metrics, "d1"), "d1 reports a stall");
        t.Equal(1d, StaleCount(metrics, "d2"), "d2 reports a stall");

        var applied = await engine.ApplyAsync(ConfigWith("d2"));
        t.Equal(1, applied.Removed.Count, "d1 is removed by the hot update");

        await watcher.TickAsync(t.Ct);
        t.Equal(1, watcher.TrackedDeviceCount, "the removed device leaves no entry behind");

        // 关键后果：把 d1 原样加回来，它必须能重新报警，而不是被旧去重窗口吞掉。
        driver.Release();
        await engine.ApplyAsync(ConfigWith("d1", "d2"));
        await WaitForSamples(t, engine, 2);
        await watcher.TickAsync(t.Ct);   // 建立新 actor 的观察起点
        clock.Advance(TimeSpan.FromSeconds(5), TimeSpan.FromSeconds(5));
        await watcher.TickAsync(t.Ct);

        t.Equal(2d, StaleCount(metrics, "d1"),
            "a re-added device alarms again instead of hitting a stale dedup window");
    }

    /// <summary>
    /// 驱动卡在一条不响应取消的读里时，下线这台设备不能把配置应用挂住。
    ///
    /// <see cref="Scheduler.ApplyAsync"/> 的上限是 2 秒，而 <see cref="DeviceActor.StopAsync"/>
    /// 原本是「无限等采集循环退出」。一个死锁的协议栈会让「改一台设备」变成一次
    /// 无限等待，关机也会卡在这里——这两件事在现场都是灾难。
    /// </summary>
    private static async Task HungReadDoesNotBlockShutdown(TestContext t)
    {
        var registry = new DriverRegistry();
        var driver = new WedgeAfterFirstReadDriver();
        registry.Register(driver);

        await using var engine = new Scheduler(
            registry, new PointCache(), NullLoggerFactory.Instance,
            new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

        engine.Start(ConfigWith("d1"));
        await WaitForSamples(t, engine, 1);

        // 等它真正卡在第二次读里。等不到就说明这条用例没验证到目标路径，
        // 与其「碰巧通过」，不如直接判失败。
        var entered = await Task.WhenAny(driver.Hung, Task.Delay(TimeSpan.FromSeconds(10), t.Ct)) == driver.Hung;
        t.Ok(entered, "the collection loop is actually blocked inside a read");

        var clock = System.Diagnostics.Stopwatch.StartNew();

        // 不调用 driver.Release()：读会一直挂在那里，正是要验证的场景。
        var applied = await engine.ApplyAsync(ConfigWith());

        clock.Stop();
        t.Equal(1, applied.Removed.Count, "the device is removed despite the hung read");
        t.Ok(clock.Elapsed < TimeSpan.FromSeconds(30),
            $"removing a hung device must not block: took {clock.ElapsedMilliseconds} ms");

        driver.Release();
    }

    /// <summary>
    /// 设备删了又原样加回来，必须重新推一次 <c>$state</c>。
    ///
    /// 这钉的是「上一状态没清」：还留着旧值就等于「状态没变」，看门狗不会推，
    /// 而 broker 上那条 retained 已经被我们清掉了，新订阅者将什么也拿不到。
    /// </summary>
    private static async Task ReaddedDeviceRepublishesItsState(TestContext t)
    {
        var registry = new DriverRegistry();
        var driver = new WedgeAfterFirstReadDriver();
        registry.Register(driver);

        var clock = new FakeClock();
        await using var engine = new Scheduler(
            registry, new PointCache(), NullLoggerFactory.Instance,
            new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

        engine.Start(ConfigWith("d1"));

        await using var watcher = new DeviceStateWatcher(
            engine, () => null, NullLogger.Instance, new MetricsRegistry())
        {
            Time = clock,
        };

        await watcher.TickAsync(t.Ct);
        t.Equal(1, watcher.TrackedDeviceCount, "the device is tracked initially");

        await engine.ApplyAsync(ConfigWith());
        await watcher.TickAsync(t.Ct);
        t.Equal(0, watcher.TrackedDeviceCount, "removal clears the entry");

        await engine.ApplyAsync(ConfigWith("d1"));
        await watcher.TickAsync(t.Ct);
        t.Equal(1, watcher.TrackedDeviceCount,
            "a re-added device is tracked again, so its $state is re-published");

        driver.Release();
    }

    private static double StaleCount(MetricsRegistry metrics, string device)
        => metrics.CounterValue("uic_watchdog_stale_total",
            new Dictionary<string, string> { ["device"] = device });

    private static async Task WaitForSamples(TestContext t, Scheduler engine, int count)
    {
        var clock = System.Diagnostics.Stopwatch.StartNew();
        while (engine.Cache.All().Count < count && clock.Elapsed < TimeSpan.FromSeconds(10))
            await Task.Delay(10, t.Ct);

        t.Ok(engine.Cache.All().Count >= count,
            $"expected {count} device(s) to produce a sample, got {engine.Cache.All().Count}");
    }

    /// <summary>
    /// 首轮读到值之后卡住且**不响应取消**的驱动：这正是采集停滞的形状。
    ///
    /// 用「一直返回新样本」的驱动测不出停滞——每轮样本时标都在变，看门狗看到的
    /// 永远是「刚更新的新鲜数据」。必须让样本源真的停下来。
    /// </summary>
    private sealed class WedgeAfterFirstReadDriver : IDriver
    {
        private readonly TaskCompletionSource _release = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource _hung = new(TaskCreationOptions.RunContinuationsAsynchronously);

        public DriverMeta Meta { get; } = new()
        {
            Id = "noop",
            Name = "首轮后假死",
            Transports = new[] { "none" },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        /// <summary>
        /// 读**已经**卡在挂起里了。
        ///
        /// 用例必须等这个信号而不是「等一会儿」：采集循环可能正停在两轮之间的
        /// delay 上，此时取消立刻生效，看上去「下线没被挂住」，
        /// 而真正要验证的路径一次都没走到。
        /// </summary>
        public Task Hung => _hung.Task;

        /// <summary>放行卡住的读，让用例能正常收尾。</summary>
        public void Release() => _release.TrySetResult();

        private sealed class Conn(WedgeAfterFirstReadDriver owner) : IConn
        {
            private int _reads;

            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public async Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                // 刻意不看 ct：真实的假死线程也不会因为「有人喊停」就回来。
                if (Interlocked.Increment(ref _reads) > 1)
                {
                    owner._hung.TrySetResult();
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
}
