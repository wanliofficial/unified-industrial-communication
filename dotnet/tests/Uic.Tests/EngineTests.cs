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
        tests.Add(new TestCase("engine/transient_failure_is_retried", TransientFailureIsRetried));
        tests.Add(new TestCase("engine/bad_quality_batch_is_retried", BadQualityBatchIsRetried));
        tests.Add(new TestCase("engine/deadline_is_retried_as_timeout", DeadlineIsRetriedAsTimeout));
        tests.Add(new TestCase("engine/permanent_failure_is_not_retried", PermanentFailureIsNotRetried));
        tests.Add(new TestCase("engine/retries_are_finite", RetriesAreFinite));
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

    /// <summary>
    /// 按脚本应答的假驱动：第 N 次读取执行第 N 个动作，脚本用完后一直走兜底动作。
    /// 重试语义只有「第几次读发生了什么」能说明白，所以把时间轴显式写出来。
    /// </summary>
    private sealed class ScriptedDriver : IDriver
    {
        private readonly List<Func<IReadOnlyList<ReadSpan>, IReadOnlyList<RawSample>>> _steps = new();
        private Func<IReadOnlyList<ReadSpan>, IReadOnlyList<RawSample>> _fallback = Good;

        public int Reads;
        public int Probes;

        /// <summary>这一次读取直接抛异常（连接断、超时等）。</summary>
        public ScriptedDriver Throw(Exception error)
        {
            _steps.Add(_ => throw error);
            return this;
        }

        /// <summary>这一次读取「成功返回」但整批质量都是坏的。</summary>
        public ScriptedDriver BadBatch(Quality quality)
        {
            _steps.Add(spans => Samples(spans, quality, "injected failure"));
            return this;
        }

        /// <summary>每次都抛同一个异常（链路彻底断了）。</summary>
        public ScriptedDriver AlwaysThrow(Exception error)
        {
            Func<IReadOnlyList<ReadSpan>, IReadOnlyList<RawSample>> always = _ => throw error;
            _steps.Add(always);
            _fallback = always;
            return this;
        }

        /// <summary>脚本走完之后一直返回好值。</summary>
        public ScriptedDriver ThenSucceed()
        {
            _fallback = Good;
            return this;
        }

        public DriverMeta Meta { get; } = new()
        {
            Id = "scripted",
            Name = "脚本驱动",
            Transports = new[] { "none" },
            Roles = new[] { DriverRole.Master },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        private static IReadOnlyList<RawSample> Good(IReadOnlyList<ReadSpan> spans)
            => Samples(spans, Quality.Good, null);

        private static IReadOnlyList<RawSample> Samples(
            IReadOnlyList<ReadSpan> spans, Quality quality, string? error)
            => spans.SelectMany(s => s.Points)
                .Select(p => new RawSample(p.Id, DataValue.Number(p.DataType, 1), quality, null, error))
                .ToArray();

        private sealed class Conn : IConn
        {
            private readonly ScriptedDriver _owner;

            public Conn(ScriptedDriver owner) => _owner = owner;

            public Task ProbeAsync(CancellationToken ct)
            {
                Interlocked.Increment(ref _owner.Probes);
                return Task.CompletedTask;
            }

            public Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                var index = Interlocked.Increment(ref _owner.Reads) - 1;
                var step = index < _owner._steps.Count ? _owner._steps[index] : _owner._fallback;
                return Task.FromResult(step(spans));
            }

            public Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
                => Task.FromResult<IReadOnlyList<WriteResult>>(commands
                    .Select(c => new WriteResult(c.Point.Id, true, Quality.Good, DateTimeOffset.UtcNow))
                    .ToArray());

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static Device BuildDevice(int retries = 0, TimeSpan? interval = null) => new()
    {
        Id = "dev-1",
        ChannelId = "ch-1",
        // 默认快速轮询，够并发用例取到多次采样；重试用例传长间隔，只跑一轮才好数「读了几次」。
        Interval = interval ?? TimeSpan.FromMilliseconds(10),
        Retries = retries,
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

    /// <summary>
    /// 临时性失败（超时）要按 retries 重试：偶发丢帧在串口/无线链路上很常见，
    /// 一次读失败就判设备坏会把正常产线画成故障。重试成功后不留失败痕迹。
    /// </summary>
    private static Task TransientFailureIsRetried(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ScriptedDriver()
                .Throw(DriverException.Timeout("scripted", "read", "first read timed out"))
                .ThenSucceed();

            var cache = new PointCache();
            var actor = BuildActor(driver, retries: 2, cache);

            await StartAndSettleAsync(actor, t);

            t.Equal(2, driver.Reads, "the timed-out read is retried exactly once");
            t.Equal(Quality.Good, cache.Get("dev-1", "a")!.Quality, "the good value from the retry wins");
            t.Equal(RuntimeState.Collecting, actor.State, "a recovered retry does not leave the device degraded");
            t.Equal(0L, actor.Stats.ReadsFailed, "the failed attempt is not counted as a failed cycle");
            t.Ok(driver.Probes >= 2, $"the retry reconnects before reading again (probes={driver.Probes})");

            await actor.StopAsync();
        });
    }

    /// <summary>
    /// 驱动也可能「调用成功但整批数据是坏质量」（Modbus 返回异常码就是这样）。
    /// 这类临时失败同样要重试，而且坏值不能被先发布出去——否则界面会闪一下坏值。
    /// </summary>
    private static Task BadQualityBatchIsRetried(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ScriptedDriver()
                .BadBatch(Quality.Timeout)
                .ThenSucceed();

            var cache = new PointCache();
            var actor = BuildActor(driver, retries: 1, cache);

            await StartAndSettleAsync(actor, t);

            t.Equal(2, driver.Reads, "a bad-quality batch is retried");
            t.Equal(Quality.Good, cache.Get("dev-1", "a")!.Quality, "the retry replaces the bad batch");
            t.Equal(1UL, cache.Get("dev-1", "a")!.Seq, "the discarded attempt never reached the cache");

            await actor.StopAsync();
        });
    }

    /// <summary>
    /// 设备级 deadline 到期时，传输层可能只抛了一个没有分类的取消异常。
    /// 这仍然是「超时」，必须能重试、也必须以 timeout 上报，不能被当成 bad。
    /// </summary>
    private static Task DeadlineIsRetriedAsTimeout(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ScriptedDriver()
                .Throw(new OperationCanceledException("simulated deadline"))
                .ThenSucceed();

            var cache = new PointCache();
            var actor = BuildActor(driver, retries: 1, cache);

            await StartAndSettleAsync(actor, t);

            t.Equal(2, driver.Reads, "the deadline is retried like any other timeout");
            t.Equal(Quality.Good, cache.Get("dev-1", "a")!.Quality, "the retry recovers the point");

            await actor.StopAsync();
        });
    }

    /// <summary>
    /// 地址越界、协议错这类失败是确定性的：重试只会延长故障时间。
    /// 所以「不重试」和重试本身一样是需求的一部分。
    /// </summary>
    private static Task PermanentFailureIsNotRetried(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ScriptedDriver()
                .Throw(DriverException.Address("scripted", "read", "register 99999 out of range"))
                .ThenSucceed();

            var cache = new PointCache();
            var actor = BuildActor(driver, retries: 3, cache);

            await StartAndSettleAsync(actor, t);

            t.Equal(1, driver.Reads, "a deterministic address error is not retried");
            t.Equal(Quality.AddressError, cache.Get("dev-1", "a")!.Quality, "the point carries the address error");
            t.Equal(RuntimeState.Collecting, actor.State,
                "an address error is a point-table problem: it does not degrade or trip the device");
            t.Equal("scripted/read Address: register 99999 out of range", actor.Stats.LastError,
                "the error text is kept for the operator");

            await actor.StopAsync();
        });
    }

    /// <summary>
    /// 重试次数是有限的：链路真的断了的时候，必须按 retries 停下并上报失败，
    /// 而不是在一个采集周期里无限打对面。
    /// </summary>
    private static Task RetriesAreFinite(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ScriptedDriver()
                .AlwaysThrow(DriverException.Connection("scripted", "read", "link down"));

            var cache = new PointCache();
            var actor = BuildActor(driver, retries: 2, cache);

            await StartAndSettleAsync(actor, t);

            t.Equal(3, driver.Reads, "one attempt plus two retries, then stop");
            t.Equal(Quality.CommError, cache.Get("dev-1", "a")!.Quality, "the point reports the comm error");
            t.Equal(RuntimeState.Degraded, actor.State, "the device is degraded after the attempts are used up");
            t.Equal(1L, actor.Stats.ReadsFailed, "the whole cycle counts as one failed read");

            await actor.StopAsync();
        });
    }

    private static DeviceActor BuildActor(ScriptedDriver driver, int retries, PointCache cache) => new(
        BuildDevice(retries: retries, interval: TimeSpan.FromSeconds(10)),
        BuildChannel(),
        driver,
        cache,
        new ChannelGate(1),
        NullLogger.Instance,
        new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

    /// <summary>间隔设成 10 秒，所以启动后只会有一次采集；等它跑完再断言。</summary>
    private static Task StartAndSettleAsync(DeviceActor actor, TestContext t)
    {
        actor.Start();
        return Task.Delay(200, t.Ct);
    }

    private static Task RunAsync(Func<Task> body) => body();
}
