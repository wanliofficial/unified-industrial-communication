using System.Diagnostics;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Engine;

namespace Uic.Tests;

/// <summary>
/// 调度层的不变量（用例 T11 的落点）。
///
/// 「同一设备上的请求永不并发」是协议会话有状态的前提：事务号、半双工、PDU 协商
/// 都经不起两个请求交错。这条约束写在对驱动的要求里，但真正执行它的是设备 actor，
/// 所以这里用一个会计数的假驱动验证「actor 确实做到了」。
/// </summary>
internal static class EngineTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("engine/conn_is_never_called_concurrently", NeverCalledConcurrently));
        tests.Add(new TestCase("engine/actor_reports_state_and_generation", ReportsStateAndGeneration));
    }

    /// <summary>一个「记住自己有没有被并发调用」的假驱动。</summary>
    private sealed class CountingDriver : IDriver
    {
        public int Reads;
        public int Writes;
        public int Probes;
        public int Overlaps;
        private int _inFlight;

        public DriverMeta Meta { get; } = new()
        {
            Id = "counting",
            Name = "计数驱动",
            Transports = new[] { "none" },
            Roles = new[] { DriverRole.Master },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        private void Enter()
        {
            if (Interlocked.Increment(ref _inFlight) > 1) Interlocked.Increment(ref Overlaps);
        }

        private void Leave() => Interlocked.Decrement(ref _inFlight);

        private sealed class Conn : IConn
        {
            private readonly CountingDriver _owner;

            public Conn(CountingDriver owner) => _owner = owner;

            public Task ProbeAsync(CancellationToken ct)
            {
                _owner.Enter();
                try
                {
                    Interlocked.Increment(ref _owner.Probes);
                    return Task.CompletedTask;
                }
                finally
                {
                    _owner.Leave();
                }
            }

            public async Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                _owner.Enter();
                try
                {
                    Interlocked.Increment(ref _owner.Reads);

                    // 故意慢一点：太快的话并发窗口根本不会重叠，测试就成了摆设。
                    await Task.Delay(8, ct).ConfigureAwait(false);

                    return spans.SelectMany(s => s.Points)
                        .Select(p => new RawSample(p.Id, DataValue.Number(p.DataType, 1), Quality.Good, null))
                        .ToArray();
                }
                finally
                {
                    _owner.Leave();
                }
            }

            public async Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
            {
                _owner.Enter();
                try
                {
                    Interlocked.Increment(ref _owner.Writes);
                    await Task.Delay(8, ct).ConfigureAwait(false);

                    return commands
                        .Select(c => new WriteResult(c.Point.Id, true, Quality.Good, DateTimeOffset.UtcNow))
                        .ToArray();
                }
                finally
                {
                    _owner.Leave();
                }
            }

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static Device BuildDevice() => new()
    {
        Id = "dev-1",
        ChannelId = "ch-1",
        Interval = TimeSpan.FromMilliseconds(10),
        Points = new List<Point>
        {
            new() { Id = "a", Address = Address.Register("holding", 0), DataType = DataType.UInt16 },
            new() { Id = "b", Address = Address.Register("holding", 1), DataType = DataType.UInt16 },
        },
    };

    private static Channel BuildChannel() => new()
    {
        Id = "ch-1",
        Protocol = "counting",
        Transport = "none",

        // 通道级并发也是 1：读写都不允许重叠，才能验证 actor + gate 两层都生效。
        MaxConcurrency = 1,
        Timeout = TimeSpan.FromSeconds(2),
        ConnectTimeout = TimeSpan.FromSeconds(2),
    };

    private static Task NeverCalledConcurrently(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new CountingDriver();
            var cache = new PointCache();
            var actor = new DeviceActor(
                BuildDevice(),
                BuildChannel(),
                driver,
                cache,
                new ChannelGate(1),
                NullLogger.Instance,
                new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            actor.Start();

            // 采集在跑的同时下发一次写：写请求会插队，但绝不允许与读并行。
            var write = actor.WriteAsync(
                new[] { new WriteCommand(actor.Spans[0].Points[0], DataValue.Number(DataType.UInt16, 7)) },
                t.Ct);

            await Task.Delay(150, t.Ct);
            var results = await write;
            await actor.StopAsync();

            t.Ok(driver.Reads + driver.Writes >= 3,
                $"the actor actually collected and wrote (reads={driver.Reads}, writes={driver.Writes})");
            t.Equal(0, driver.Overlaps, "no read or write ever overlapped another call on the same connection");
            t.Equal(1, results.Count, "one write result");
            t.Ok(results[0].Ok, "write succeeded");

            t.Ok(cache.Get("dev-1", "a") is not null, "collected values reached the cache");
            t.Equal(Quality.Good, cache.Get("dev-1", "a")!.Quality, "quality is good");
        });
    }

    private static Task ReportsStateAndGeneration(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new CountingDriver();
            var actor = new DeviceActor(
                BuildDevice(),
                BuildChannel(),
                driver,
                new PointCache(),
                new ChannelGate(1),
                NullLogger.Instance,
                new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            t.Equal(RuntimeState.Configured, actor.State, "a fresh actor is configured, not collecting");
            t.Equal(ConfiguredState.Enabled, actor.ConfiguredState, "configured state comes from the config");

            var before = actor.Generation;
            actor.Start();
            await Task.Delay(80, t.Ct);
            t.Equal(RuntimeState.Collecting, actor.State, "the actor is collecting after start");

            await actor.RestartAsync();
            await Task.Delay(80, t.Ct);

            // 重启必须换代次：旧连接的迟到结果不能覆盖新连接的状态。
            t.Ok(actor.Generation > before, $"restart bumps the generation (was {before}, now {actor.Generation})");
            t.Equal(RuntimeState.Collecting, actor.State, "collecting again after restart");

            await actor.StopAsync();
            t.Equal(RuntimeState.Disabled, actor.State, "stop leaves the actor disabled");
        });
    }

    private static Task RunAsync(Func<Task> body) => body();
}
