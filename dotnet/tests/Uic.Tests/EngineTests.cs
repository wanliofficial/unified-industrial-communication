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
        tests.Add(new TestCase("engine/write_confirmation_reads_back", WriteConfirmationReadsBack));
        tests.Add(new TestCase("engine/write_confirmation_detects_mismatch", WriteConfirmationDetectsMismatch));
        tests.Add(new TestCase("engine/write_without_confirmation_skips_readback", WriteWithoutConfirmationSkipsReadback));
        tests.Add(new TestCase("engine/array_write_shape_is_validated_before_the_device", ArrayWriteShapeIsValidated));
        tests.Add(new TestCase("engine/point_interval_overrides_device_period", PointIntervalOverridesDevicePeriod));
        tests.Add(new TestCase("engine/point_interval_failure_is_scoped_to_due_bucket", PointIntervalFailureIsScoped));
    }

    /// <summary>
    /// 可回读的假驱动。<c>LieOnRead</c> 模拟「写命令被接受但设备没保持住」——
    /// 这恰恰是写后校验要抓的场景。
    /// </summary>
    private sealed class ConfirmDriver : IDriver
    {
        private readonly Dictionary<string, DataValue> _stored = new(StringComparer.Ordinal);

        public int Reads;
        public int Writes;
        public bool LieOnRead;

        public DriverMeta Meta { get; } = new()
        {
            Id = "confirm",
            Name = "回读驱动",
            Transports = new[] { "none" },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        private sealed class Conn : IConn
        {
            private readonly ConfirmDriver _owner;

            public Conn(ConfirmDriver owner) => _owner = owner;

            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                Interlocked.Increment(ref _owner.Reads);

                var samples = spans.SelectMany(s => s.Points).Select(p =>
                {
                    if (_owner.LieOnRead)
                    {
                        return new RawSample(p.Id, DataValue.Number(p.DataType, 999), Quality.Good, null);
                    }

                    var value = _owner._stored.TryGetValue(p.Id, out var stored)
                        ? stored
                        : DataValue.Number(p.DataType, 1);

                    return new RawSample(p.Id, value, Quality.Good, null);
                }).ToArray();

                return Task.FromResult<IReadOnlyList<RawSample>>(samples);
            }

            public Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
            {
                Interlocked.Increment(ref _owner.Writes);

                foreach (var command in commands)
                {
                    _owner._stored[command.Point.Id] = command.Value;
                }

                return Task.FromResult<IReadOnlyList<WriteResult>>(
                    commands.Select(c => new WriteResult(c.Point.Id, true, Quality.Good, DateTimeOffset.UtcNow))
                        .ToArray());
            }

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static Task WriteConfirmationReadsBack(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ConfirmDriver();
            var actor = new DeviceActor(
                BuildDevice(), BuildChannel(), driver, new PointCache(), new ChannelGate(1),
                NullLogger.Instance, new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            var results = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(actor.Spans[0].Points[0], DataValue.Number(DataType.UInt16, 7),
                        Confirm: true),
                },
                t.Ct);

            t.Equal(1, results.Count, "one result");
            t.Ok(results[0].Ok, "the write succeeds");
            t.Equal(true, results[0].Verified, "confirm=true triggers a readback");
            t.Equal(1, driver.Writes, "the driver wrote once");
            t.Equal(1, driver.Reads, "the driver was read back exactly once");
            t.Equal(null, results[0].Error, "a matching readback has no error");

            await actor.DisposeAsync();
        });
    }

    private static Task WriteConfirmationDetectsMismatch(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ConfirmDriver { LieOnRead = true };
            var actor = new DeviceActor(
                BuildDevice(), BuildChannel(), driver, new PointCache(), new ChannelGate(1),
                NullLogger.Instance, new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            var results = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(actor.Spans[0].Points[0], DataValue.Number(DataType.UInt16, 7),
                        Confirm: true),
                },
                t.Ct);

            t.Equal(1, results.Count, "one result");
            t.Ok(!results[0].Ok, "a mismatch is not reported as success");
            t.Equal(false, results[0].Verified, "the mismatch is explicit");
            t.Equal(Quality.Uncertain, results[0].Quality,
                "the command was applied but could not be confirmed");
            t.Contains(results[0].Error ?? string.Empty, "verify.mismatch",
                "the error says the readback did not match");
            t.Contains(results[0].Error ?? string.Empty, "wrote 7", "the expected value is in the error");
            t.Contains(results[0].Error ?? string.Empty, "read 999", "the actual value is in the error");

            await actor.DisposeAsync();
        });
    }

    private static Task WriteWithoutConfirmationSkipsReadback(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ConfirmDriver();
            var actor = new DeviceActor(
                BuildDevice(), BuildChannel(), driver, new PointCache(), new ChannelGate(1),
                NullLogger.Instance, new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            var results = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(actor.Spans[0].Points[0], DataValue.Number(DataType.UInt16, 7)),
                },
                t.Ct);

            t.Ok(results[0].Ok, "the write succeeds");
            t.Equal(null, results[0].Verified, "without confirm there is no verification verdict");
            t.Equal(0, driver.Reads, "the driver is not read back");

            await actor.DisposeAsync();
        });
    }

    /// <summary>
    /// 数组写入的形状校验必须发生在建链之前。
    ///
    /// 现场最容易出的事故是「少填一个元素」：如果放任它下到驱动，Modbus 会写偏半段
    /// 寄存器，而回读只比较前几个元素，接口仍然报「写成功」。这类错误必须在引擎层
    /// 一次拦掉，不能寄希望于每个驱动各自「顺手」检查。
    /// </summary>
    private static Task ArrayWriteShapeIsValidated(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ConfirmDriver();
            var device = new Device
            {
                Id = "dev-1",
                ChannelId = "ch-1",
                Interval = TimeSpan.FromMilliseconds(10),
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "batch",
                        Address = Address.Register("holding", 0, 3),
                        DataType = DataType.UInt16,
                        Count = 3,
                    },
                },
            };

            var actor = new DeviceActor(
                device, BuildChannel(), driver, new PointCache(), new ChannelGate(1),
                NullLogger.Instance, new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            var point = actor.Spans[0].Points[0];
            t.Equal(3, point.Elements, "the point exposes its element count to the driver layer");

            var results = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(point, DataValue.Numbers(DataType.UInt16, new double[] { 1, 2 })),
                },
                t.Ct);

            t.Equal(1, results.Count, "the rejected write still gets a result");
            t.Ok(!results[0].Ok, "a short array is rejected");
            t.Contains(results[0].Error ?? string.Empty, "needs 3 elements",
                "the error states the expected element count");
            t.Equal(0, driver.Writes, "the driver is never asked to write a malformed value");

            // 标量点位收到数组同样拒绝：允许「单元素数组」会让线上出现两种等价写法。
            var scalar = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(point, DataValue.Number(DataType.UInt16, 1)),
                },
                t.Ct);

            t.Ok(!scalar[0].Ok, "an array point rejects a scalar value");
            t.Contains(scalar[0].Error ?? string.Empty, "value.shape", "the error names the shape problem");

            // 形状正确时必须放行，且结果按请求顺序对齐。
            var ok = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(point, DataValue.Numbers(DataType.UInt16, new double[] { 1, 2, 3 })),
                },
                t.Ct);

            t.Ok(ok[0].Ok, "a well-formed array is written");
            t.Equal(1, driver.Writes, "only the well-formed write reached the driver");

            // 混合批量：被拒项留在原位，后面的成功项不整体前移。
            var mixed = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(point, DataValue.Numbers(DataType.UInt16, new double[] { 1, 2 })),
                    new WriteCommand(point, DataValue.Numbers(DataType.UInt16, new double[] { 4, 5, 6 })),
                },
                t.Ct);

            t.Equal(2, mixed.Count, "one result per command");
            t.Ok(!mixed[0].Ok, "the first (short) command is rejected");
            t.Ok(mixed[1].Ok, "the second (valid) command succeeds");
            t.Equal("batch", mixed[1].PointId, "results keep the request order");

            // 同一点位在一次批量里出现两次是合法的（以最后一次写入为准）。
            // 合并结果时必须仍返回两条，而不是因为 id 重复整批失败。
            var duplicate = await actor.WriteAsync(
                new[]
                {
                    new WriteCommand(point, DataValue.Numbers(DataType.UInt16, new double[] { 7, 8, 9 })),
                    new WriteCommand(point, DataValue.Numbers(DataType.UInt16, new double[] { 4, 5, 6 })),
                },
                t.Ct);

            t.Equal(2, duplicate.Count, "a repeated point still yields one result per command");
            t.Ok(duplicate.All(r => r.Ok), "both writes to the same point succeed");

            await actor.DisposeAsync();
        });
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

    /// <summary>
    /// FR-COL-10：点位周期可以覆盖设备周期。
    ///
    /// 「快变量 100ms、慢变量 10s 并存」是这条需求的验收标准，而它同时受两个约束：
    /// 每个点位按自己的节奏读，同一设备上的请求仍然串行。用例用 1s 的设备周期
    /// 加一个 100ms 的点位，数一数两种节奏确实分开了。
    /// </summary>
    private static Task PointIntervalOverridesDevicePeriod(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new BucketDriver();
            var device = new Device
            {
                Id = "dev-1",
                ChannelId = "ch-1",
                Interval = TimeSpan.FromSeconds(1),
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "fast",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.UInt16,
                        Interval = TimeSpan.FromMilliseconds(100),
                    },
                    new()
                    {
                        Id = "slow",
                        Address = Address.Register("holding", 1),
                        DataType = DataType.UInt16,
                    },
                },
            };

            var actor = new DeviceActor(
                device,
                BuildChannel(),
                driver,
                new PointCache(),
                new ChannelGate(1),
                NullLogger.Instance,
                new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            t.Equal(2, actor.CollectionIntervals.Count, "the points fall into two cadences");
            t.Equal(TimeSpan.FromMilliseconds(100), actor.CollectionIntervals[0],
                "the fast cadence comes first");
            t.Equal(TimeSpan.FromSeconds(1), actor.CollectionIntervals[1],
                "the point without an interval inherits the device period");
            t.Equal(TimeSpan.FromSeconds(1), actor.SlowestInterval,
                "the watchdog threshold follows the slowest cadence");

            actor.Start();
            await Task.Delay(700, t.Ct);
            await actor.StopAsync();

            var fast = driver.ReadsOf("fast");
            var slow = driver.ReadsOf("slow");

            t.Ok(fast >= 5, $"the fast point really runs at 100ms (reads={fast})");
            t.Equal(1, slow, "the slow point is read once within 700ms");
            t.Ok(fast > slow * 3, $"the two cadences are separated (fast={fast}, slow={slow})");
            t.Equal(0, driver.Overlaps, "per-point cadences still never overlap on one connection");
        });
    }

    /// <summary>
    /// 快周期那个桶读失败时，只有它自己的点位变坏。
    ///
    /// 如果失败把整台设备的点位都标成坏值，现场会看到「慢变量也故障」的假象，
    /// 而这台设备其实只是快变量那条窗口出了通信问题。
    /// </summary>
    private static Task PointIntervalFailureIsScoped(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new BucketDriver { FailFastAfterFirstRead = true };
            var device = new Device
            {
                Id = "dev-1",
                ChannelId = "ch-1",
                Interval = TimeSpan.FromSeconds(1),
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "fast",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.UInt16,
                        Interval = TimeSpan.FromMilliseconds(100),
                    },
                    new()
                    {
                        Id = "slow",
                        Address = Address.Register("holding", 1),
                        DataType = DataType.UInt16,
                    },
                },
            };

            var cache = new PointCache();
            var actor = new DeviceActor(
                device,
                BuildChannel(),
                driver,
                cache,
                new ChannelGate(1),
                NullLogger.Instance,
                new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            actor.Start();
            await Task.Delay(450, t.Ct);
            await actor.StopAsync();

            t.Ok(driver.ReadsOf("fast") >= 2, "the fast bucket was attempted again after the failure");
            t.Equal(Quality.CommError, cache.Get("dev-1", "fast")!.Quality,
                "the failing bucket reports the communication error");

            t.Equal(Quality.Good, cache.Get("dev-1", "slow")!.Quality,
                "the slow point keeps its last good value");
            t.Equal(1UL, cache.Get("dev-1", "slow")!.Seq,
                "the slow point is not re-read by the fast bucket's schedule");
            t.Equal(1, driver.ReadsOf("slow"), "the slow point was only read on the merged first round");
        });
    }

    /// <summary>
    /// 记录每个点位被读了多少次，并可选地让「含 fast 的读取」在首轮之后失败。
    /// 周期用例要的是「哪个点位在什么时候被读到」，只有驱动自己数得清。
    /// </summary>
    private sealed class BucketDriver : IDriver
    {
        private readonly Dictionary<string, int> _reads = new(StringComparer.Ordinal);
        private int _calls;
        private int _inFlight;

        public bool FailFastAfterFirstRead { get; init; }
        public int Overlaps;

        public int ReadsOf(string point)
        {
            lock (_reads) return _reads.TryGetValue(point, out var n) ? n : 0;
        }

        public DriverMeta Meta { get; } = new()
        {
            Id = "bucket",
            Name = "周期驱动",
            Transports = new[] { "none" },
            Roles = new[] { DriverRole.Master },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        private sealed class Conn : IConn
        {
            private readonly BucketDriver _owner;

            public Conn(BucketDriver owner) => _owner = owner;

            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                if (Interlocked.Increment(ref _owner._inFlight) > 1)
                    Interlocked.Increment(ref _owner.Overlaps);

                try
                {
                    var call = Interlocked.Increment(ref _owner._calls);
                    var ids = spans.SelectMany(s => s.Points).Select(p => p.Id).ToArray();

                    lock (_owner._reads)
                    {
                        foreach (var id in ids)
                            _owner._reads[id] = (_owner._reads.TryGetValue(id, out var n) ? n : 0) + 1;
                    }

                    if (_owner.FailFastAfterFirstRead &&
                        call > 1 &&
                        ids.Contains("fast", StringComparer.Ordinal))
                    {
                        throw DriverException.Connection("bucket", "read", "fast window link down");
                    }

                    IReadOnlyList<RawSample> raw = spans.SelectMany(s => s.Points)
                        .Select(p => new RawSample(p.Id, DataValue.Number(p.DataType, 1), Quality.Good, null))
                        .ToArray();

                    return Task.FromResult(raw);
                }
                finally
                {
                    Interlocked.Decrement(ref _owner._inFlight);
                }
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

            // 等到采集确实跑起来再收敛，而不是「固定 150 ms 内至少几轮」：
            // 机器一忙，墙钟等待就会变成假失败。真正的断言是下面的 Overlaps == 0。
            var clock = Stopwatch.StartNew();
            while (driver.Reads + driver.Writes < 3 && clock.Elapsed < TimeSpan.FromSeconds(10))
                await Task.Delay(5, t.Ct);

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
