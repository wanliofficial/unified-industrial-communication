using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Virtual;
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
        tests.Add(new TestCase("lifecycle/failed_apply_leaves_running_devices_alone",
            FailedApplyLeavesRunningDevicesAlone));
        tests.Add(new TestCase("lifecycle/unpersisted_apply_rolls_the_devices_back",
            UnpersistedApplyRollsTheDevicesBack));
        tests.Add(new TestCase("lifecycle/bad_mqtt_ca_fails_before_anything_changes",
            BadMqttCaFailsBeforeAnythingChanges));
        tests.Add(new TestCase("lifecycle/channel_concurrency_follows_hot_update",
            ChannelConcurrencyFollowsHotUpdate));
    }

    private static ConfigDocument ConfigWith(params string[] deviceIds) => ConfigOn("noop", deviceIds);

    private static ConfigDocument ConfigOn(string protocol, params string[] deviceIds) => new()
    {
        SchemaVersion = 1,
        Site = "lifecycle-site",
        Channels =
        {
            new Channel
            {
                Id = "ch",
                Protocol = protocol,
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

    /// <summary>
    /// 一份引用了未注册驱动的配置必须整份被拒，不能留下半应用状态。
    ///
    /// <see cref="Scheduler.ApplyAsync"/> 的顺序是「先卸旧的、再建新的」，而驱动是否
    /// 已注册要到 <see cref="Scheduler.CreateActor"/> 里才检查——配置校验只要求
    /// <c>protocol</c> 非空。于是驱动名拼错时，前一台设备已经被 Dispose、新的建不出来，
    /// 而 <see cref="Scheduler.Config"/> 还停在旧值。运行态与配置从此不一致：一台仍在
    /// 配置里的设备永远不出数，再 apply 一次还可能按设备顺序把另一台也拆掉。
    ///
    /// 失败必须是配置错误（带字段路径），不是「应用了一半再报 500」。
    /// </summary>
    private static async Task FailedApplyLeavesRunningDevicesAlone(TestContext t)
    {
        var registry = new DriverRegistry();
        registry.Register(new VirtualDriver());

        await using var engine = new Scheduler(
            registry, new PointCache(), NullLoggerFactory.Instance,
            new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

        engine.Start(ConfigOn("virtual", "d1", "d2"));
        await WaitForSamples(t, engine, 2);

        var d1 = engine.Find("d1");
        var d2 = engine.Find("d2");
        t.Ok(d1 is not null && d2 is not null, "both devices run before the bad apply");

        // "virtualx" 是拼错的驱动名：校验查不出，注册表里没有。
        var bad = ConfigOn("virtualx", "d1", "d2");

        ConfigException? error = null;
        try
        {
            await engine.ApplyAsync(bad);
        }
        catch (Exception ex)
        {
            error = ex as ConfigException;
        }

        // 先钉真正的后果：一台仍在配置里的设备被拆掉、且没有新 actor 顶替。
        t.Ok(ReferenceEquals(d1, engine.Find("d1")),
            "a refused apply does not dispose the device that was already running");
        t.Ok(ReferenceEquals(d2, engine.Find("d2")),
            "nor the one that had not been reached yet");
        t.Equal("virtual", engine.Config.Channels[0].Protocol,
            "the engine keeps the last applied config");

        t.Ok(error is not null, "a config naming an unregistered driver is refused as a config error");
        t.Ok(error?.Problems.Any(p => p.Path == "channels[ch].protocol") == true,
            "the refusal points at the channel protocol, not at an internal exception");

        // 拒绝之后老配置必须还能继续工作：采集没被停在半路。
        var seq = engine.Cache.Get("d1", "p1")?.Seq ?? 0;
        var spin = System.Diagnostics.Stopwatch.StartNew();
        while ((engine.Cache.Get("d1", "p1")?.Seq ?? 0) <= seq &&
               spin.Elapsed < TimeSpan.FromSeconds(10))
        {
            await Task.Delay(10, t.Ct);
        }

        t.Ok((engine.Cache.Get("d1", "p1")?.Seq ?? 0) > seq,
            "the untouched device keeps collecting after the refused apply");
    }

    private static double StaleCount(MetricsRegistry metrics, string device)
        => metrics.CounterValue("uic_watchdog_stale_total",
            new Dictionary<string, string> { ["device"] = device });

    /// <summary>
    /// 新配置已经在设备上生效、但版本写不下盘（磁盘满、只读挂载）时，必须把设备退回旧配置。
    ///
    /// 否则三份状态各说各话：接口回 500（运维据此认为没生效）、设备跑的是新配置、
    /// 磁盘上还是旧版本（重启后悄悄退回）。「apply 失败 = 什么都没变」才是可操作的契约。
    ///
    /// 注入点是真实的文件系统权限：把 <c>config/versions</c> 改成只读，写入必然失败。
    /// 以 root 运行时权限拦不住写入，这时跳过而不是假通过。
    /// </summary>
    private static async Task UnpersistedApplyRollsTheDevicesBack(TestContext t)
    {
        // 用 IsWindows 而不是「非 Linux/非 macOS」：分析器只认这种形状的守卫，
        // 换成反向判断就退化成 CA1416 警告（与 FaultTests 的 chmod 注入保持一致）。
        if (OperatingSystem.IsWindows())
            throw new SkipException("chmod-based permission injection needs a POSIX filesystem");

        using var logs = LoggerFactory.Create(b => { b.ClearProviders(); });
        var ring = new RingBufferLogProvider(128);

        var runtime = await GatewayRuntime.CreateAsync(
            new RuntimeOptions
            {
                DataDir = t.TempDir,
                GatewayVersion = "0.0.0-test",
                Fallback = () => RuntimeConfig(baseline: "42"),
            },
            RuntimeDrivers(),
            logs,
            ring,
            t.Ct);

        try
        {
            await WaitForRuntimeSamples(t, runtime, 1);
            var versionBefore = runtime.ConfigVersion;

            var versions = Path.Combine(t.TempDir, "config", "versions");
            var mode = File.GetUnixFileMode(versions);
            File.SetUnixFileMode(versions, UnixFileMode.UserRead | UnixFileMode.UserExecute);

            try
            {
                // 先确认注入真的生效：root 下写只读目录照样成功，那样后面的断言就无从谈起。
                try
                {
                    await File.WriteAllTextAsync(Path.Combine(versions, "probe"), "x", t.Ct);
                    throw new SkipException("running with privileges that ignore directory permissions");
                }
                catch (UnauthorizedAccessException)
                {
                    // 期望路径：写不进去。
                }

                var edited = RuntimeConfig(baseline: "7");

                Exception? failure = null;
                try
                {
                    await runtime.ApplyConfigAsync(edited, "probe", t.Ct);
                }
                catch (Exception ex)
                {
                    failure = ex;
                }

                t.Ok(failure is not null, "an apply that cannot persist its version fails");
                t.Ok(failure is IOException or UnauthorizedAccessException,
                    $"the failure surfaces the storage problem, got {failure?.GetType().Name}");

                t.Equal(versionBefore, runtime.ConfigVersion, "no version was committed");
                t.Equal("42", runtime.Engine.Config.Devices[0].Labels["baseline"],
                    "the devices were rolled back to the persisted configuration");

                // 回滚后的老配置必须真的还在采：一台被停住的设备等于没回滚。
                var seq = runtime.Engine.Cache.Get("v1", "temp")?.Seq ?? 0;
                var spin = System.Diagnostics.Stopwatch.StartNew();
                while ((runtime.Engine.Cache.Get("v1", "temp")?.Seq ?? 0) <= seq &&
                       spin.Elapsed < TimeSpan.FromSeconds(10))
                {
                    await Task.Delay(20, t.Ct);
                }

                t.Ok((runtime.Engine.Cache.Get("v1", "temp")?.Seq ?? 0) > seq,
                    "the rolled-back device keeps collecting");
            }
            finally
            {
                File.SetUnixFileMode(versions, mode);
            }
        }
        finally
        {
            await runtime.DisposeAsync();
        }
    }

    private static ConfigDocument RuntimeConfig(string baseline) => new()
    {
        SchemaVersion = 1,
        Site = "lifecycle-site",
        Channels = new List<Channel>
        {
            new() { Id = "virtual-bus", Protocol = "virtual", Transport = "none" },
        },
        Devices = new List<Device>
        {
            new()
            {
                Id = "v1",
                ChannelId = "virtual-bus",
                Interval = TimeSpan.FromMilliseconds(200),
                Labels = new Dictionary<string, string>
                {
                    ["mode"] = "const",
                    ["baseline"] = baseline,
                },
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "temp",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.Float64,
                    },
                },
            },
        },
    };

    private static DriverRegistry RuntimeDrivers()
    {
        var registry = new DriverRegistry();
        registry.Register(new VirtualDriver());
        return registry;
    }

    /// <summary>
    /// MQTT 的自签 CA 读不出来时，apply 必须在任何副作用之前失败。
    ///
    /// <c>ConfigureSinks</c> 的顺序是「先摘旧 sink、再建新的」，CA 又是在建新 sink 时才读——
    /// 那一步抛异常时，旧连接已经断了、版本也已经提交：接口回 422，实际却把 MQTT 上报停了，
    /// 磁盘上还多了一个「应用成功过」的版本。提前读一次就能把这类问题挡在副作用之前。
    /// </summary>
    private static async Task BadMqttCaFailsBeforeAnythingChanges(TestContext t)
    {
        using var logs = LoggerFactory.Create(b => { b.ClearProviders(); });
        var ring = new RingBufferLogProvider(128);

        var runtime = await GatewayRuntime.CreateAsync(
            new RuntimeOptions
            {
                DataDir = t.TempDir,
                GatewayVersion = "0.0.0-test",
                Fallback = () => RuntimeConfig(baseline: "42"),
            },
            RuntimeDrivers(),
            logs,
            ring,
            t.Ct);

        try
        {
            await WaitForRuntimeSamples(t, runtime, 1);
            var versionBefore = runtime.ConfigVersion;

            // 文件存在、但不是 PEM：这是「配了 CA 但内容不对」的真实形状。
            var caFile = t.TempFile("garbage-ca.pem");
            await File.WriteAllTextAsync(caFile, "not a certificate\n", t.Ct);

            var baseline = RuntimeConfig(baseline: "42");
            var edited = new ConfigDocument
            {
                SchemaVersion = 1,
                Site = baseline.Site,
                Channels = baseline.Channels,
                Devices = baseline.Devices,
                Northbound = new NorthboundOptions
                {
                    Rest = baseline.Northbound.Rest,
                    OpcUa = baseline.Northbound.OpcUa,
                    Mqtt = baseline.Northbound.Mqtt with
                    {
                        Enabled = true,
                        Tls = new MqttTlsOptions { Enabled = true, CaFile = caFile },
                    },
                },
            };

            ConfigException? error = null;
            try
            {
                await runtime.ApplyConfigAsync(edited, "probe", t.Ct);
            }
            catch (Exception ex)
            {
                error = ex as ConfigException;
            }

            t.Ok(error is not null, "an unreadable MQTT CA is reported as a configuration error");
            t.Ok(error?.Problems.Any(p => p.Path == "northbound.mqtt.tls.caFile") == true,
                "the error points at the CA file field");
            t.Equal(versionBefore, runtime.ConfigVersion,
                "a config refused before its side effects does not commit a version");
            t.Equal(false, runtime.CurrentConfig.Northbound.Mqtt.Enabled,
                "the running configuration keeps MQTT disabled");
        }
        finally
        {
            await runtime.DisposeAsync();
        }
    }

    /// <summary>
    /// 通道并发上限必须能热更新。`ChannelGate` 底层是 `SemaphoreSlim`，建好之后容量改不了，
    /// 所以上限变化只能换一个新的闸门。原来的写法是 `GetOrAdd`：闸门已存在就原样返回，
    /// 改小并发只在重启进程后才生效——配置写着 1、实际仍按 4 路并发打同一条串口/链路，
    /// 这类「配置说一套、跑的是另一套」在现场极难发现，因为两边都没有报错。
    ///
    /// 用真实调度器 + 同通道两台设备：先确认上限 2 时确实会重叠（否则用例本身没测到东西），
    /// 再把上限改成 1，确认重叠消失。
    /// </summary>
    private static async Task ChannelConcurrencyFollowsHotUpdate(TestContext t)
    {
        var driver = new OverlapDriver();

        var registry = new DriverRegistry();
        registry.Register(driver);

        await using var engine = new Scheduler(
            registry, new PointCache(), NullLoggerFactory.Instance,
            new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) });

        engine.Start(TwoDeviceConfig(concurrency: 2));

        // 先证明「上限 2 时真的会重叠」：不重叠的话后面那条断言毫无意义。
        await WaitFor(t, () => driver.MaxInFlight >= 2, "two devices share the channel when the limit allows it");

        // 换闸门的那一瞬间，旧闸门下还有在飞的读——它们不算新上限的证据。
        // 先让这一轮走完再开始计量，否则测的是过渡期而不是稳态。
        t.Equal(1, await PeakAfterApply(t, driver, engine, TwoDeviceConfig(concurrency: 1)),
            "the new concurrency limit takes effect without a process restart");

        // 改大同样要生效，否则「只能改小」还是个半成品。
        t.Equal(2, await PeakAfterApply(t, driver, engine, TwoDeviceConfig(concurrency: 2)),
            "raising the limit also takes effect");
    }

    /// <summary>
    /// 应用一份配置，等旧闸门下的在飞读走完，再从零计量稳态并发峰值。
    /// </summary>
    private static async Task<int> PeakAfterApply(
        TestContext t, OverlapDriver driver, Scheduler engine, ConfigDocument config)
    {
        await engine.ApplyAsync(config);

        // 两轮读的时长（读本身 40ms）足以让旧闸门上的在飞请求落地。
        await Task.Delay(150, t.Ct);

        driver.ResetPeak();

        for (var i = 0; i < 6; i++) await Task.Delay(50, t.Ct);

        return driver.MaxInFlight;
    }

    private static ConfigDocument TwoDeviceConfig(int concurrency) => new()
    {
        SchemaVersion = 1,
        Site = "lifecycle-site",
        Channels = new List<Channel>
        {
            new()
            {
                Id = "ch",
                Protocol = "overlap",
                Transport = "none",
                MaxConcurrency = concurrency,
            },
        },
        Devices = new List<Device>
        {
            TwoDevice("d1"),
            TwoDevice("d2"),
        },
    };

    private static Device TwoDevice(string id) => new()
    {
        Id = id,
        ChannelId = "ch",
        Interval = TimeSpan.FromMilliseconds(50),
        Points = new List<Point>
        {
            new()
            {
                Id = "p1",
                Address = Address.Register("holding", 0),
                DataType = DataType.UInt16,
                Interval = TimeSpan.FromMilliseconds(50),
            },
        },
    };

    private static async Task WaitFor(TestContext t, Func<bool> condition, string what)
    {
        var clock = System.Diagnostics.Stopwatch.StartNew();
        while (!condition() && clock.Elapsed < TimeSpan.FromSeconds(10))
            await Task.Delay(10, t.Ct);

        t.Ok(condition(), what);
    }

    /// <summary>
    /// 记录「同时在读的最大路数」。读故意慢（40ms）：
    /// 快到毫秒级的话并发窗口根本不会重叠，用例就成了摆设。
    /// </summary>
    private sealed class OverlapDriver : IDriver
    {
        private int _inFlight;
        private int _peak;

        public int MaxInFlight => Volatile.Read(ref _peak);

        public void ResetPeak() => Interlocked.Exchange(ref _peak, 0);

        public DriverMeta Meta { get; } = new()
        {
            Id = "overlap",
            Name = "并发计数驱动",
            Transports = new[] { "none" },
            Roles = new[] { DriverRole.Master },
        };

        public IConn Open(OpenContext context) => new Conn(this);

        private void Enter()
        {
            var now = Interlocked.Increment(ref _inFlight);

            // CAS 抬高峰值：多路同时进入时不能互相覆盖。
            int seen;
            while (now > (seen = Volatile.Read(ref _peak)) &&
                   Interlocked.CompareExchange(ref _peak, now, seen) != seen)
            {
            }
        }

        private void Leave() => Interlocked.Decrement(ref _inFlight);

        private sealed class Conn : IConn
        {
            private readonly OverlapDriver _owner;

            public Conn(OverlapDriver owner) => _owner = owner;

            public Task ProbeAsync(CancellationToken ct) => Task.CompletedTask;

            public async Task<IReadOnlyList<RawSample>> ReadAsync(
                IReadOnlyList<ReadSpan> spans, CancellationToken ct)
            {
                _owner.Enter();
                try
                {
                    await Task.Delay(40, ct).ConfigureAwait(false);

                    return spans.SelectMany(s => s.Points)
                        .Select(p => new RawSample(p.Id, DataValue.Number(p.DataType, 1), Quality.Good, null))
                        .ToArray();
                }
                finally
                {
                    _owner.Leave();
                }
            }

            public Task<IReadOnlyList<WriteResult>> WriteAsync(
                IReadOnlyList<WriteCommand> commands, CancellationToken ct)
                => throw new NotSupportedException("本用例只测读路径的并发");

            public ValueTask DisposeAsync() => ValueTask.CompletedTask;
        }
    }

    private static async Task WaitForRuntimeSamples(TestContext t, GatewayRuntime runtime, int count)
    {
        var clock = System.Diagnostics.Stopwatch.StartNew();
        while (runtime.Engine.Cache.All().Count < count && clock.Elapsed < TimeSpan.FromSeconds(10))
            await Task.Delay(20, t.Ct);

        t.Ok(runtime.Engine.Cache.All().Count >= count,
            $"expected {count} sample(s), got {runtime.Engine.Cache.All().Count}");
    }

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
