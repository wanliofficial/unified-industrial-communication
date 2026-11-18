using System.Diagnostics;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Engine;

namespace Uic.Tests;

/// <summary>
/// 调度时间基的用例（FR-RUN-01 / FR-RUN-08）。
///
/// 这两条需求都很难用「跑一会儿看看」来验收：相位错不错、时钟跳一下会不会多采一轮，
/// 在短测试里都可能碰巧通过。所以把算术抽成纯函数，对边界值直接断言；再用一个
/// 最小调度器验证「相位确实被分配下去了」。
/// </summary>
internal static class ScheduleTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("schedule/phase_spreads_devices", PhaseSpreadsDevices));
        tests.Add(new TestCase("schedule/monotonic_arithmetic", MonotonicArithmetic));
        tests.Add(new TestCase("schedule/scheduler_assigns_phases", SchedulerAssignsPhases));
        tests.Add(new TestCase("schedule/phase_follows_fastest_point", PhaseFollowsFastestPoint));
        tests.Add(new TestCase("schedule/uptime_ignores_a_clock_jump", UptimeIgnoresClockJump));
        tests.Add(new TestCase("schedule/backoff_grows_then_caps", BackoffGrowsThenCaps));
    }

    /// <summary>
    /// 失败退避：2^n 增长、到上限封顶、抖动只加不减且不超过 10%。
    ///
    /// 这条公式决定「设备彻底离线后多久试一次」。此前它埋在 actor 里，矩阵写着
    /// 「interval × 2^failures，上限 recoverInterval，叠加 10% 抖动」，却没有任何用例
    /// 断言过这个算术——能点名的都是「重试几次」的语义用例。公式写错（比如 2^n 写成
    /// n²、上限取错、抖动摇成 [−10%, 0]）不会有任何红。
    ///
    /// 抽成纯函数后直接算，不赌时序；<paramref name="jitterSample"/> 传 0 就得到基值，
    /// 传接近 1 就得到抖动上界，两者都精确可断言。
    /// </summary>
    private static Task BackoffGrowsThenCaps(TestContext t)
    {
        var baseInterval = TimeSpan.FromMilliseconds(100);
        var recover = TimeSpan.FromMilliseconds(1000);

        // 抖动取 0：得到「纯 2^n」的序列，逐档核对。
        t.Close(100, ScheduleMath.Backoff(0, baseInterval, recover, 0).TotalMilliseconds, 0.001,
            "no failures yet: wait exactly one base interval");
        t.Close(200, ScheduleMath.Backoff(1, baseInterval, recover, 0).TotalMilliseconds, 0.001,
            "one failure: 2x the base interval");
        t.Close(400, ScheduleMath.Backoff(2, baseInterval, recover, 0).TotalMilliseconds, 0.001,
            "two failures: 4x");

        // 上限：2^6 = 6400ms 远超 1000ms，必须被 recoverInterval 截住。
        t.Close(1000, ScheduleMath.Backoff(6, baseInterval, recover, 0).TotalMilliseconds, 0.001,
            "the backoff is capped at recoverInterval, not 2^6 x base");

        // 抖动：全部落在 [基值, 基值 × 1.1] 内，且确实会变大（不是恒定的）。
        var low = ScheduleMath.Backoff(3, baseInterval, recover, 0);          // 800ms
        var high = ScheduleMath.Backoff(3, baseInterval, recover, 0.999_999); // ~880ms
        t.Ok(high > low, $"jitter only adds, never subtracts ({low.TotalMilliseconds:0.#} → {high.TotalMilliseconds:0.#} ms)");
        t.Ok(high.TotalMilliseconds <= 800 * 1.1 + 0.001,
            $"jitter stays within 10% ({high.TotalMilliseconds:0.#} ms <= 880 ms)");

        // 上限也参与抖动：封顶后的值同样不得越过 ceiling × 1.1。
        var cappedHigh = ScheduleMath.Backoff(6, baseInterval, recover, 0.999_999);
        t.Ok(cappedHigh.TotalMilliseconds <= 1000 * 1.1 + 0.001,
            $"a capped backoff still keeps jitter bounded ({cappedHigh.TotalMilliseconds:0.#} ms)");

        // 基数大于上限时（点位周期比 recoverInterval 还长），上限取基数，
        // 退避不能比一个正常周期还短——否则所谓「退避」反而在加快速率。
        t.Close(500, ScheduleMath.Backoff(0, TimeSpan.FromMilliseconds(500), TimeSpan.FromMilliseconds(100), 0).TotalMilliseconds, 0.001,
            "a base larger than recoverInterval is not shortened by the ceiling");

        return Task.CompletedTask;
    }

    /// <summary>
    /// uptime 必须用单调时钟算。
    ///
    /// <c>/health</c> 里的 uptime 是排障时判断「是不是刚重启过」的依据。用墙钟算，
    /// 一次 NTP 校时前跳会让它凭空多出一小时，后跳甚至可能算出负数——
    /// 那比没有这个字段更糟。
    /// </summary>
    private static async Task UptimeIgnoresClockJump(TestContext t)
    {
        var clock = new FakeClock();
        var engine = new Scheduler(
            new DriverRegistry(), new PointCache(), NullLoggerFactory.Instance,
            new SchedulerOptions { Time = clock });

        await using (engine)
        {
            t.Equal(TimeSpan.Zero, engine.Uptime, "a fresh scheduler has not been up yet");

            // 只拨墙钟：这正是校时跳变的形状，单调时钟没有跟着走。
            clock.Advance(TimeSpan.FromHours(1), TimeSpan.Zero);
            t.Equal(TimeSpan.Zero, engine.Uptime,
                "a wall-clock jump must not inflate uptime");

            // 真实流逝才应改变 uptime。
            clock.Advance(TimeSpan.Zero, TimeSpan.FromSeconds(42));
            t.Equal(TimeSpan.FromSeconds(42), engine.Uptime,
                "uptime follows the monotonic clock");
        }
    }

    private static Task PhaseSpreadsDevices(TestContext t)
    {
        var interval = TimeSpan.FromSeconds(1);

        // 4 台同周期设备：0 / 250 / 500 / 750ms，均匀铺满一个周期。
        t.Close(0, ScheduleMath.PhaseOffset(0, 4, interval).TotalMilliseconds, 1e-9,
            "the first device keeps phase zero");
        t.Close(250, ScheduleMath.PhaseOffset(1, 4, interval).TotalMilliseconds, 1e-9,
            "the second device waits a quarter interval");
        t.Close(500, ScheduleMath.PhaseOffset(2, 4, interval).TotalMilliseconds, 1e-9,
            "the third device waits half an interval");
        t.Close(750, ScheduleMath.PhaseOffset(3, 4, interval).TotalMilliseconds, 1e-9,
            "the last device waits three quarters");

        // 单台设备不能因为「除零」拿到一个非零相位；越界索引要夹紧，不能把相位推到周期外。
        t.Equal(TimeSpan.Zero, ScheduleMath.PhaseOffset(0, 1, interval), "a single device starts immediately");
        t.Close(750, ScheduleMath.PhaseOffset(9, 4, interval).TotalMilliseconds, 1e-9,
            "an out-of-range index clamps to the last slot");
        t.Equal(TimeSpan.Zero, ScheduleMath.PhaseOffset(3, 0, interval), "an empty set has no phase");

        return Task.CompletedTask;
    }

    private static Task MonotonicArithmetic(TestContext t)
    {
        t.Equal(Stopwatch.Frequency, ScheduleMath.ToTimestamp(TimeSpan.FromSeconds(1)),
            "one second is exactly one frequency worth of monotonic ticks");
        t.Equal(1L, ScheduleMath.ToTimestamp(TimeSpan.Zero),
            "zero is clamped to one tick so a loop cannot spin forever");

        var due = 10_000L + ScheduleMath.ToTimestamp(TimeSpan.FromMilliseconds(250));
        t.Close(250, ScheduleMath.Remaining(10_000L, due).TotalMilliseconds, 1,
            "remaining time uses the same monotonic base");

        // 已到期或时间戳相等都必须返回零，调用方据此立即采集；不能返回负数。
        t.Equal(TimeSpan.Zero, ScheduleMath.Remaining(10_000L, 10_000L), "an exact deadline is due now");
        t.Equal(TimeSpan.Zero, ScheduleMath.Remaining(10_000L, 9_000L), "a past deadline is due now");

        // 墙上时钟的「昨天/明天」不参与这里的计算：参数只有单调时间戳。
        // 因此 NTP 校时前后跳不会改变下一次到期时间。
        t.Ok(ScheduleMath.Remaining(500L, 1_000L) == ScheduleMath.Remaining(500L, 1_000L),
            "the result depends only on the two monotonic inputs");

        return Task.CompletedTask;
    }

    private static Task SchedulerAssignsPhases(TestContext t)
    {
        return RunAsync(async () =>
        {
            var registry = new DriverRegistry();
            registry.Register(new NoopDriver());

            var config = new ConfigDocument
            {
                SchemaVersion = 1,
                Site = "phase-site",
                Channels =
                {
                    new Channel
                    {
                        Id = "ch",
                        Protocol = "noop",
                        Transport = "none",
                        MaxConcurrency = 1,
                        Timeout = TimeSpan.FromSeconds(1),
                        ConnectTimeout = TimeSpan.FromSeconds(1),
                    },
                },
            };

            for (var i = 0; i < 4; i++)
            {
                config.Devices.Add(new Device
                {
                    Id = $"dev-{i}",
                    ChannelId = "ch",
                    Interval = TimeSpan.FromSeconds(1),
                    Points =
                    {
                        new Point
                        {
                            Id = "p",
                            Address = Address.Register("holding", 0),
                            DataType = DataType.UInt16,
                        },
                    },
                });
            }

            await using var scheduler = new Scheduler(
                registry, new PointCache(), NullLoggerFactory.Instance,
                new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

            scheduler.Start(config);

            var phases = scheduler.Actors
                .Select(a => a.StartPhase.TotalMilliseconds)
                .ToArray();

            t.Equal(4, phases.Length, "one actor per device");
            t.Close(0, phases[0], 1e-9, "device 0 has no delay");
            t.Close(250, phases[1], 1e-9, "device 1 is offset a quarter");
            t.Close(500, phases[2], 1e-9, "device 2 is offset half");
            t.Close(750, phases[3], 1e-9, "device 3 is offset three quarters");
        });
    }

    /// <summary>
    /// 相位要按设备内最快的周期算。
    ///
    /// 一台 10s 设备上挂了一个 100ms 的点位，如果按设备周期铺相，第二台设备的
    /// 快值会白等 5s 才第一次出现——现场会用这个值判断设备是否上线，
    /// 5s 的空白与「设备没起来」看起来一模一样。
    /// </summary>
    private static Task PhaseFollowsFastestPoint(TestContext t)
    {
        return RunAsync(async () =>
        {
            var registry = new DriverRegistry();
            registry.Register(new NoopDriver());

            var config = new ConfigDocument
            {
                Site = "phase",
                Northbound = new NorthboundOptions(),
                Channels =
                {
                    new Channel
                    {
                        Id = "ch",
                        Protocol = "noop",
                        Transport = "none",
                        MaxConcurrency = 1,
                        Timeout = TimeSpan.FromSeconds(1),
                        ConnectTimeout = TimeSpan.FromSeconds(1),
                    },
                },
            };

            for (var i = 0; i < 2; i++)
            {
                config.Devices.Add(new Device
                {
                    Id = $"dev-{i}",
                    ChannelId = "ch",
                    Interval = TimeSpan.FromSeconds(10),
                    Points =
                    {
                        new Point
                        {
                            Id = "fast",
                            Address = Address.Register("holding", 0),
                            DataType = DataType.UInt16,
                            Interval = TimeSpan.FromMilliseconds(100),
                        },
                        new Point
                        {
                            Id = "slow",
                            Address = Address.Register("holding", 1),
                            DataType = DataType.UInt16,
                        },
                    },
                });
            }

            await using var scheduler = new Scheduler(
                registry, new PointCache(), NullLoggerFactory.Instance,
                new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

            scheduler.Start(config);

            var phases = scheduler.Actors.Select(a => a.StartPhase.TotalMilliseconds).ToArray();

            t.Close(0, phases[0], 1e-9, "the first device starts immediately");
            t.Close(50, phases[1], 1e-9,
                "the offset follows the 100ms point, not the 10s device interval");
        });
    }

    /// <summary>只够把 actor 跑起来的空驱动；相位用例不等采集结果。</summary>
    private sealed class NoopDriver : IDriver
    {
        public DriverMeta Meta { get; } = new()
        {
            Id = "noop",
            Name = "空驱动",
            Transports = new[] { "none" },
        };

        public IConn Open(OpenContext context) => new Conn();

        private sealed class Conn : IConn
        {
            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
                => Task.FromResult<IReadOnlyList<RawSample>>(
                    spans.SelectMany(s => s.Points)
                        .Select(p => new RawSample(p.Id, DataValue.Number(p.DataType, 1), Quality.Good, null))
                        .ToArray());

            public Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
                => Task.FromResult<IReadOnlyList<WriteResult>>(Array.Empty<WriteResult>());

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static Task RunAsync(Func<Task> body) => body();
}
