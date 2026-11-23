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
        tests.Add(new TestCase("engine/devices_collect_in_parallel", DevicesCollectInParallel));
        tests.Add(new TestCase("engine/channel_gate_lets_queued_writes_go_first", ChannelGateLetsQueuedWritesGoFirst));
        tests.Add(new TestCase("engine/actor_reports_state_and_generation", ReportsStateAndGeneration));
        tests.Add(new TestCase("engine/transient_failure_is_retried", TransientFailureIsRetried));
        tests.Add(new TestCase("engine/bad_quality_batch_is_retried", BadQualityBatchIsRetried));
        tests.Add(new TestCase("engine/deadline_is_retried_as_timeout", DeadlineIsRetriedAsTimeout));
        tests.Add(new TestCase("engine/permanent_failure_is_not_retried", PermanentFailureIsNotRetried));
        tests.Add(new TestCase("engine/comm_error_batch_is_retried", CommErrorBatchIsRetried));
        tests.Add(new TestCase("engine/address_error_batch_does_not_retire_the_device", AddressErrorBatchDoesNotRetireTheDevice));
        tests.Add(new TestCase("engine/retries_are_finite", RetriesAreFinite));
        tests.Add(new TestCase("engine/failure_threshold_trips_the_breaker", FailureThresholdTripsTheBreaker));
        tests.Add(new TestCase("engine/write_confirmation_reads_back", WriteConfirmationReadsBack));
        tests.Add(new TestCase("engine/write_confirmation_detects_mismatch", WriteConfirmationDetectsMismatch));
        tests.Add(new TestCase("engine/write_without_confirmation_skips_readback", WriteWithoutConfirmationSkipsReadback));
        tests.Add(new TestCase("engine/write_readback_failure_is_not_a_write_failure", WriteReadbackFailureIsNotAWriteFailure));
        tests.Add(new TestCase("engine/array_write_shape_is_validated_before_the_device", ArrayWriteShapeIsValidated));
        tests.Add(new TestCase("engine/point_interval_overrides_device_period", PointIntervalOverridesDevicePeriod));
        tests.Add(new TestCase("engine/point_interval_failure_is_scoped_to_due_bucket", PointIntervalFailureIsScoped));
    }

    /// <summary>
    /// 通道闸门的「写优先」（FR-RUN-03）：写是人的操作、延迟可感知；读是后台任务，晚一点无所谓。
    /// 已经进入临界区的读不会被抢占（不中断在途请求），但**新来的读**必须给排队中的写让路。
    ///
    /// 为什么单独立一条：<c>engine/conn_is_never_called_concurrently</c> 只验证「不重叠」，
    /// <c>lifecycle/channel_concurrency_follows_hot_update</c> 只验证「换了闸门」。
    /// 把 <c>ChannelGate</c> 里那段 <c>_writesWaiting</c> 自旋整个删掉，这两条都不会失败——
    /// 也就是说矩阵里写着的「写优先」此前没有任何断言。
    /// </summary>
    private static Task ChannelGateLetsQueuedWritesGoFirst(TestContext t)
    {
        return RunAsync(async () =>
        {
            using var gate = new ChannelGate(2);

            // 先把两个槽位占满（用读占：读不抬高 _writesWaiting）。
            var held1 = await gate.EnterReadAsync(t.Ct);
            var held2 = await gate.EnterReadAsync(t.Ct);

            var order = new List<string>();
            var orderLock = new object();
            void Record(string who)
            {
                lock (orderLock) order.Add(who);
            }

            // 写排在信号量的队首。
            var writerReady = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var writer = Task.Run(async () =>
            {
                writerReady.SetResult();
                using (await gate.EnterWriteAsync(t.Ct))
                {
                    Record("write");
                    // 持有槽位一小会儿，制造一个「写进行中」的窗口。
                    await Task.Delay(300, t.Ct);
                }
            });
            await writerReady.Task;
            await Task.Delay(80, t.Ct); // 确保写已 Increment 并阻塞在信号量上

            // 写排队之后才来的读：必须等写完成。
            var readerEntered = 0;
            var reader = Task.Run(async () =>
            {
                using (await gate.EnterReadAsync(t.Ct))
                {
                    Interlocked.Exchange(ref readerEntered, 1);
                    Record("read");
                }
            });
            await Task.Delay(120, t.Ct);

            // 放掉一个槽位：写应当拿到它（它在队列里排第一）。
            held1.Dispose();
            await Task.Delay(120, t.Ct);
            t.Equal(0, Volatile.Read(ref readerEntered),
                "a read that arrived after the write must not slip in while the write runs");

            // 再放掉第二个槽位：写仍在持有（_writesWaiting 还没归零），新读仍不得占用空出来的槽位。
            // 这正是区分「写优先」与「只是并发上限」的地方：没有优先级时，
            // 这个读会立刻拿走第二个空槽位，与写并发。
            held2.Dispose();
            await Task.Delay(150, t.Ct);
            t.Equal(0, Volatile.Read(ref readerEntered),
                "a queued write holds a second free slot back from new reads");

            await writer.WaitAsync(TimeSpan.FromSeconds(10), t.Ct);
            await reader.WaitAsync(TimeSpan.FromSeconds(10), t.Ct);

            string joined;
            lock (orderLock) joined = string.Join(",", order);
            t.Equal("write,read", joined, "the write goes first, the read only after it finished");
        });
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

        /// <summary>回读这一步直接抛异常：写命令已被接受下发，但回读链路读不出来。</summary>
        public bool ThrowOnRead;

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

                if (_owner.ThrowOnRead)
                    throw DriverException.Connection("confirm", "read", "readback link down");

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
    /// 写后确认的回读这一步本身抛异常时，绝不能把它当成「写入失败」上报。
    ///
    /// 为什么单独立一条：写命令已经被驱动接受、已经下发到设备，回读却读不出来——
    /// 这只能说明「这次写没能被确认」，是**不确定**，不是**写失败**。两者现场处置完全相反：
    /// 写失败去查写命令/点表，回读失败去查回读链路。把它们混为一谈，运维会在错误的方向上排查。
    /// 代码里 catch 块因此把质量折成读失败的语义（这里是 Connection→comm_error）、
    /// 错误消息统一打 verify.readback_failed 前缀，与 verify.mismatch/verify.quality 明确区分开。
    ///
    /// 这与 write_confirmation_detects_mismatch（值对不上）是不同分支：那条是
    /// 「读回来了但值不对」，这条是「读这一步就挂了」。前者质量是 Uncertain、这条质量是
    /// 回读异常本身折出的 comm_error——把回读异常也塌缩成 Uncertain 会让运维看不出是链路问题。
    /// </summary>
    private static Task WriteReadbackFailureIsNotAWriteFailure(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ConfirmDriver { ThrowOnRead = true };
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
            // 关键：写命令确实下发到了设备（驱动收到并接受了写），所以这不是「写没出去」。
            t.Equal(1, driver.Writes, "the write command was dispatched to the device");
            t.Equal(1, driver.Reads, "the readback read was attempted (and failed)");

            // 不能确认 = 不算成功：Ok 为 false、Verified 为 false。
            t.Ok(!results[0].Ok, "an unconfirmed write is not reported as success");
            t.Equal(false, results[0].Verified, "the write is not marked verified");
            // 质量是回读异常本身折出的 comm_error，而不是笼统的 Uncertain/Bad——
            // 现场据此判断「去查回读链路」，而不是「去查写命令」。
            t.Equal(Quality.CommError, results[0].Quality,
                "the readback link failure surfaces as comm_error, not a generic uncertain");
            // 错误消息必须明确是回读失败，而不是写成「写入失败」误导排查方向。
            t.Contains(results[0].Error ?? string.Empty, "verify.readback_failed",
                "the error is labeled as a readback/verification problem, not a write failure");

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

    /// <summary>
    /// 跨设备并行（FR-RUN-02 的另一半）：两台设备各自跑自己的采集循环，互不等待。
    ///
    /// 为什么单独立一条：矩阵写的是「每设备串行、跨设备并行」，但此前只有
    /// <c>engine/conn_is_never_called_concurrently</c>——它证明的是「同一连接不并发」，
    /// 属于串行那半。跨设备并发一旦退化成「整台调度器一把锁」，那半句就没有任何红。
    /// 现场影响很直接：一台慢设备会把所有设备一起拖慢，100 台设备等于串成一条队列。
    ///
    /// 用屏障而不是计时：两台设备的读都在屏障前等对方，只有真并行才能各自放行。
    /// 若调度退化成串行，第一台永远等不到第二台，用例以明确失败（而不是偶发超时）收场。
    /// </summary>
    private static Task DevicesCollectInParallel(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new BarrierDriver(parties: 2);

            var config = new ConfigDocument
            {
                SchemaVersion = 1,
                Site = "parallel-site",
                Channels =
                {
                    // 两条通道、各自并发上限 1：跨设备能否并行只能靠「不同通道/设备不互相锁」。
                    new Channel
                    {
                        Id = "ch-a", Protocol = "barrier", Transport = "none",
                        MaxConcurrency = 1, Timeout = TimeSpan.FromSeconds(20),
                    },
                    new Channel
                    {
                        Id = "ch-b", Protocol = "barrier", Transport = "none",
                        MaxConcurrency = 1, Timeout = TimeSpan.FromSeconds(20),
                    },
                },
                Devices =
                {
                    DeviceFor("dev-a", "ch-a"),
                    DeviceFor("dev-b", "ch-b"),
                },
            };

            var registry = new DriverRegistry();
            registry.Register(driver);

            await using var engine = new Scheduler(
                registry, new PointCache(), NullLoggerFactory.Instance,
                new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

            engine.Start(config);

            // 两台设备都通过屏障＝两次读在同一时刻同时在跑。
            // 屏障有超时，串行时不会挂死：会以「没有并发」的明确消息失败。
            var overlapped = await driver.WaitForOverlapAsync(TimeSpan.FromSeconds(15), t.Ct);

            t.Ok(overlapped,
                "two devices on separate channels collect concurrently, not one after another "
                + $"(arrived={driver.Arrived}, overlaps={driver.Overlaps})");
            t.Ok(driver.Overlaps >= 1,
                $"at least one pair of reads was in flight together (overlaps={driver.Overlaps})");

            var a = engine.Find("dev-a");
            var b = engine.Find("dev-b");
            t.Ok(a is not null && b is not null, "both devices were started");
            t.Ok(a!.State is RuntimeState.Collecting or RuntimeState.Connecting,
                $"device a is running (state={a.State})");
            t.Ok(b!.State is RuntimeState.Collecting or RuntimeState.Connecting,
                $"device b is running (state={b.State})");
        });

        static Device DeviceFor(string id, string channelId) => new()
        {
            Id = id,
            ChannelId = channelId,
            Interval = TimeSpan.FromMilliseconds(50),
            Timeout = TimeSpan.FromSeconds(20),
            Points = new List<Point>
            {
                new() { Id = "p", Address = Address.Register("holding", 0), DataType = DataType.UInt16 },
            },
        };
    }

    /// <summary>
    /// 每次读都在屏障前等「另一个设备也来读」。两个都到了才一起放行，
    /// 于是「两个读的存活区间是否重叠」变成一个确定事实，不靠时序去猜。
    /// </summary>
    private sealed class BarrierDriver(int parties) : IDriver
    {
        private readonly SemaphoreSlim _arrived = new(0);
        private int _arrivedCount;
        private int _overlaps;
        private int _inFlight;

        public int Arrived => Volatile.Read(ref _arrivedCount);
        public int Overlaps => Volatile.Read(ref _overlaps);

        public DriverMeta Meta { get; } = new()
        {
            Id = "barrier",
            Name = "屏障驱动",
            Transports = new[] { "none" },
            Roles = new[] { DriverRole.Master },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        /// <summary>等到出现一次「两个读同时在飞」；超时返回 false（说明没有并发）。</summary>
        public async Task<bool> WaitForOverlapAsync(TimeSpan timeout, CancellationToken ct)
        {
            var clock = Stopwatch.StartNew();
            while (Volatile.Read(ref _overlaps) == 0)
            {
                if (clock.Elapsed > timeout) return false;
                await Task.Delay(5, ct);
            }

            return true;
        }

        private async Task ReadAsync(CancellationToken ct)
        {
            var inFlight = Interlocked.Increment(ref _inFlight);
            if (inFlight > 1) Interlocked.Increment(ref _overlaps);

            Interlocked.Increment(ref _arrivedCount);
            _arrived.Release();

            // 等其余设备到齐（最多等超时的一半，留出余量给上层超时）。
            var remaining = parties - inFlight;
            for (var i = 0; i < Math.Max(0, remaining); i++)
            {
                if (!await _arrived.WaitAsync(TimeSpan.FromSeconds(10), ct).ConfigureAwait(false))
                    break;
            }

            // 稍留片刻，确保两次读的存活区间真的重叠，而不是擦肩而过。
            await Task.Delay(30, ct).ConfigureAwait(false);

            Interlocked.Decrement(ref _inFlight);
        }

        private sealed class Conn(BarrierDriver owner) : IConn
        {
            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public async Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                await owner.ReadAsync(ct).ConfigureAwait(false);

                return spans
                    .SelectMany(s => s.Points)
                    .Select(p => new RawSample(p.Id, DataValue.Number(p.DataType, 1), Quality.Good, null))
                    .ToArray();
            }

            public Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
                => Task.FromResult<IReadOnlyList<WriteResult>>(commands
                    .Select(c => new WriteResult(c.Point.Id, true, Quality.Good, DateTimeOffset.UtcNow))
                    .ToArray());

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static Device BuildDevice(int retries = 0, TimeSpan? interval = null) => new()    {
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
    /// 临时性失败的判定只看质量码是不是 <c>Timeout</c> 或 <c>CommError</c>（<c>DeviceActor.IsTemporary</c>），
    /// 与失败是「抛异常」还是「调用成功但返回整批坏值」无关。上一轮已经钉了 Timeout 的返回批重试
    /// （<c>bad_quality_batch_is_retried</c>）；但 CommError 这一档只被「抛异常」路径钉住
    /// （<c>retries_are_finite</c> 抛的是 <c>DriverException.Connection</c>）。两者在判定重试时
    /// 走的是不同的代码分支：前者靠 catch 块把异常折成质量、后者靠读返回样本里的质量字段直接判。
    /// 若有人把 <c>IsTemporary</c> 里 CommError 那一支删掉，只有「返回 CommError 批」会立刻不重试、
    /// 而抛异常路径仍照常重试——正是「连接偶发抖动被当成永久故障、把正常产线钉进降级」的那种回归。
    /// </summary>
    private static Task CommErrorBatchIsRetried(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ScriptedDriver()
                .BadBatch(Quality.CommError)
                .ThenSucceed();

            var cache = new PointCache();
            var actor = BuildActor(driver, retries: 1, cache);

            await StartAndSettleAsync(actor, t);

            t.Equal(2, driver.Reads, "a comm-error batch (returned, not thrown) is retried like a timeout batch");
            t.Equal(Quality.Good, cache.Get("dev-1", "a")!.Quality, "the retry replaces the comm-error batch");
            t.Equal(1UL, cache.Get("dev-1", "a")!.Seq, "the discarded comm-error attempt never reached the cache");

            await actor.StopAsync();
        });
    }

    /// <summary>
    /// 与 <c>comm_error_batch_is_retried</c> 对称的另一半：驱动「调用成功但整批返回
    /// <c>address_error</c>」时，绝不能重试、也绝不能把设备拖进降级/熔断。
    ///
    /// 为什么单独立一条：地址错是点表写错，不是链路故障——重试一万次还是同一个错地址，
    /// 只会延长故障时间；把它算进熔断计数更糟，那会把一台「链路完好、只是某个点配错」的
    /// 设备判成离线（FR-RUN-05 明确 <c>address</c> 类错误不参与熔断）。<c>permanent_failure_is_not_retried</c>
    /// 钉的是「抛异常」那条 catch 分支；这条钉的是「返回坏质量批」那条主循环分支（actor 在
    /// 读到样本后直接读质量字段判，不走 catch），两条是不同的代码路径，互不覆盖。
    ///
    /// 还有一条只有返回批路径才有的语义：返回批里带的坏值会被原样发布到缓存（现场看得到
    /// 「这个点读到的是 1，但质量是 address_error」），而抛异常路径没有值可发、只能落占位 0。
    /// 这条用「缓存里的值 == 驱动返回的 1」钉住——它是返回批路径独有的。
    /// </summary>
    private static Task AddressErrorBatchDoesNotRetireTheDevice(TestContext t)
    {
        return RunAsync(async () =>
        {
            var driver = new ScriptedDriver()
                .BadBatch(Quality.AddressError)
                .ThenSucceed();

            var cache = new PointCache();
            var actor = BuildActor(driver, retries: 3, cache);

            await StartAndSettleAsync(actor, t);

            t.Equal(1, driver.Reads, "a deterministic address error is not retried (retries=3, yet one read)");
            t.Equal(Quality.AddressError, cache.Get("dev-1", "a")!.Quality, "the point carries the address error");
            // 返回批路径：驱动返回的坏值（1）被原样发布，而不是占位 0——这条是返回批独有。
            t.Equal(1, cache.Get("dev-1", "a")!.Value.AsNumber(),
                "the returned bad value is published as-is, not zeroed to a placeholder");
            t.Equal(RuntimeState.Collecting, actor.State,
                "an address error keeps the device Collecting: it is a point-table problem, not a link failure");
            t.Equal(1L, actor.Stats.ReadsFailed,
                "the failed cycle is counted as a failed read, even though it does not advance the breaker");

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

    /// <summary>
    /// 熔断阈值（FR-RUN-05）：连续失败达到 <c>FailureThreshold</c> 判「降级 → 熔断」。
    ///
    /// 为什么单独立一条：此前全仓只有 <c>engine/retries_are_finite</c> 断言到 <c>Degraded</c>——
    /// 那证明的是「一轮用完重试后降级」，既没到阈值，也从未断言过 <c>Faulted</c>。
    /// 熔断态是运维「这台设备别再试了」的唯一信号，它不出现在断言里就等于没人验证过：
    /// 阈值比较写反，或熔断后不关旧连接，用例都发现不了。
    ///
    /// 驱动每轮读取都要用例显式放行，因此「第 N 次失败之后状态是什么」是确定事实，
    /// 不靠 sleep 去猜。失败用非临时错误（协议错），这样连接不会被重连打断：
    /// 重连会把状态改成 <c>Connecting</c>，而我们正要观察的是降级/熔断。
    /// </summary>
    private static Task FailureThresholdTripsTheBreaker(TestContext t)
    {
        return RunAsync(async () =>
        {
            const int threshold = 3;
            var driver = new GatedFailureDriver();

            var device = new Device
            {
                Id = "dev-1",
                ChannelId = "ch-1",
                // 周期 100ms：退避按 2^n 增长，几次失败之间只隔几百毫秒，
                // 用例整体不拖时间；节奏仍由「逐轮放行」控制，不靠这个数去卡时序。
                Interval = TimeSpan.FromMilliseconds(100),
                // 读超时放到足够长：用例要靠「放行」控制节奏，不该被通道超时抢先判成超时。
                Timeout = TimeSpan.FromSeconds(30),
                FailureThreshold = threshold,
                RecoverInterval = TimeSpan.FromMilliseconds(400),
                Points = new List<Point>
                {
                    new() { Id = "a", Address = Address.Register("holding", 0), DataType = DataType.UInt16 },
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

            actor.Start();

            var generationBeforeTrip = 0;

            try
            {
                // 每轮都先拿到「读进入」这个确定的交接点：
                // 此时上一轮的失败已经记完账、这一轮还没开始。
                // 第 1 轮跳过状态断言：那时连接还在建立，状态本来就是 Connecting。
                for (var round = 1; round <= threshold; round++)
                {
                    await driver.WaitForReadAsync(t);

                    if (round > 1)
                    {
                        var failuresSoFar = round - 1;
                        t.Equal(RuntimeState.Degraded, actor.State,
                            $"{failuresSoFar} of {threshold} failures degrades but does not trip the breaker");
                    }

                    if (round == threshold) generationBeforeTrip = actor.Generation;

                    driver.NextReadFails();
                }

                // 达到阈值必须熔断：降级是「有点问题」，熔断是「别再试了」。
                await WaitForStateAsync(t, actor, RuntimeState.Faulted);

                // 紧跟着取一次快照：这一刻还没走完退避，不会混进第 4 轮。
                var failedAtTrip = actor.Stats.ReadsFailed;
                t.Equal((long)threshold, failedAtTrip,
                    "the breaker trips on exactly the threshold-th consecutive failure");

                // 关连接是 fire-and-forget，换代次紧随其后，稍等它落地。
                await WaitForAsync(t, () => actor.Generation > generationBeforeTrip,
                    $"tripping the breaker closes the link so late results are discarded "
                    + $"(generation {generationBeforeTrip} → {actor.Generation})");

                // 熔断后没有独立的「探活定时器」：仍按退避重连去试，
                // 重连成功才会把状态拉回采集。这里验证它确实又开了一次连接。
                await driver.WaitForReadAsync(t);
                t.Ok(driver.Opens >= 2,
                    $"a faulted device keeps probing by reconnecting (opens={driver.Opens})");
            }
            finally
            {
                driver.ReleaseAll();
                await actor.StopAsync();
            }
        });
    }

    /// <summary>等到设备进入指定运行态；超时即失败，避免用例挂死。</summary>
    private static async Task WaitForStateAsync(TestContext t, DeviceActor actor, RuntimeState want)
    {
        var clock = Stopwatch.StartNew();
        while (actor.State != want)
        {
            if (clock.ElapsedMilliseconds > 10_000)
                t.Fail($"the device never reached {want} (still {actor.State} after {clock.ElapsedMilliseconds} ms)");

            await Task.Delay(10, t.Ct);
        }
    }

    /// <summary>等到某个条件成立；不成立就带着当时的实际值失败。</summary>
    private static async Task WaitForAsync(TestContext t, Func<bool> condition, string what)
    {
        var clock = Stopwatch.StartNew();
        while (!condition())
        {
            if (clock.ElapsedMilliseconds > 10_000) t.Fail($"{what} (waited {clock.ElapsedMilliseconds} ms)");

            await Task.Delay(10, t.Ct);
        }
    }

    /// <summary>
    /// 每一轮读取都等用例放行，然后按用例的指令失败。
    ///
    /// 不用「自动连续失败 + sleep 后看状态」是因为那要靠猜：失败之间的退避会随
    /// 失败次数增长，机器一忙就可能只跑了两轮，断言「已经熔断」就会偶发失败。
    /// 这里把每轮的交接点显式化，状态断言就落在确定的位置上。
    /// </summary>
    private sealed class GatedFailureDriver : IDriver
    {
        private readonly SemaphoreSlim _readGate = new(0);
        private readonly SemaphoreSlim _entered = new(0);
        private int _reads;
        private int _opens;

        public int Reads => Volatile.Read(ref _reads);
        public int Opens => Volatile.Read(ref _opens);

        public DriverMeta Meta { get; } = new()
        {
            Id = "gated",
            Name = "逐轮放行驱动",
            Transports = new[] { "none" },
            Roles = new[] { DriverRole.Master },
        };

        public IConn Open(OpenContext context)
        {
            Interlocked.Increment(ref _opens);
            return new Conn(this);
        }

        /// <summary>等下一轮读取进入，超时即失败。</summary>
        public async Task WaitForReadAsync(TestContext t)
        {
            if (!await _entered.WaitAsync(TimeSpan.FromSeconds(10), t.Ct).ConfigureAwait(false))
                t.Fail("the collection loop stopped asking for reads");
        }

        /// <summary>放行当前这轮读取，并让它失败。</summary>
        public void NextReadFails() => _readGate.Release();

        /// <summary>收尾用：不再阻塞任何在途读取。</summary>
        public void ReleaseAll()
        {
            for (var i = 0; i < 64; i++) _readGate.Release();
        }

        private sealed class Conn(GatedFailureDriver owner) : IConn
        {
            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public async Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                Interlocked.Increment(ref owner._reads);
                owner._entered.Release();

                // 等用例放行：这一步让「失败发生在什么状态之后」变成确定顺序。
                await owner._readGate.WaitAsync(ct).ConfigureAwait(false);

                throw DriverException.ProtocolError("gated", "read", "injected read failure");
            }

            public Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
                => Task.FromResult<IReadOnlyList<WriteResult>>(commands
                    .Select(c => new WriteResult(c.Point.Id, true, Quality.Good, DateTimeOffset.UtcNow))
                    .ToArray());

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static DeviceActor BuildActor(ScriptedDriver driver, int retries, PointCache cache) => new(
        BuildDevice(retries: retries, interval: TimeSpan.FromSeconds(10)),
        BuildChannel(),
        driver,
        cache,
        new ChannelGate(1),
        NullLogger.Instance,
        new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(5) });

    /// <summary>
    /// 间隔设成 10 秒，所以启动后只会有一次采集：等「这一轮真的走完」再断言。
    ///
    /// 早先这里是死等 200 ms——机器空闲时够用，`load average` 上到十几个以后
    /// 一次读都可能排不到，于是「重试了一次」被误判成「只读了一次」。
    /// 判据取 <see cref="DeviceStats.ReadsTotal"/>（一轮采集完成才自增），
    /// 与机器快慢无关；上限留着，真挂住时给出明确失败而不是一直等。
    ///
    /// 计数在记账之前自增，所以还要等状态从 `Connecting` 落定——
    /// 否则可能正好撞上「计数已加、状态未写」的那一瞬间。
    /// </summary>
    private static async Task StartAndSettleAsync(DeviceActor actor, TestContext t)
    {
        actor.Start();

        var clock = Stopwatch.StartNew();

        while (actor.Stats.ReadsTotal < 1 || actor.State == RuntimeState.Connecting)
        {
            if (clock.ElapsedMilliseconds > 15_000)
                t.Fail($"no collection cycle settled in {clock.ElapsedMilliseconds} ms "
                       + $"(reads={actor.Stats.ReadsTotal}, state={actor.State})");

            await Task.Delay(10, t.Ct);
        }
    }

    private static Task RunAsync(Func<Task> body) => body();
}
