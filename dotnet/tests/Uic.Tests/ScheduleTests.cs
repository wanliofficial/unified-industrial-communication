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
