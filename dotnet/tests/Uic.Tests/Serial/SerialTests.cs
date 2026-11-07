using System.Diagnostics;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Modbus;
using Uic.Engine;
using Uic.Obs;
using Uic.Sim.Modbus;

namespace Uic.Tests;

/// <summary>
/// 串口链路专项用例（T15–T18），跑在真实伪终端上。
///
/// 这几条无法用内存流复现：内存流没有内核 tty 层，也就没有 termios、
/// 没有 poller、没有「关闭 fd 打断阻塞读」的行为。伪造出来的通过比留白更糟。
///
/// pty 由 <see cref="PtyPair"/> 直接用 libc 开，不依赖 socat 之类的外部进程；
/// 非 Linux 上这些用例报 skip 而不是失败。
/// </summary>
internal static class SerialTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("serial/rtu_golden_frame_over_pty", RtuGoldenFrame));
        tests.Add(new TestCase("serial/ascii_golden_frame_over_pty", AsciiGoldenFrame));
        tests.Add(new TestCase("serial/silent_slave_times_out", SilentSlaveTimesOut));
        tests.Add(new TestCase("serial/read_returns_near_deadline", ReadReturnsNearDeadline));
        tests.Add(new TestCase("serial/close_interrupts_blocked_read", CloseInterruptsBlockedRead));
        tests.Add(new TestCase("serial/half_duplex_does_not_cross_responses", HalfDuplexDoesNotCrossResponses));
        tests.Add(new TestCase("serial/rtu_waits_between_frames", RtuWaitsBetweenFrames));
    }

    private static Channel SerialChannel(PtyPair pty, string protocol, int readTimeoutMs = 300, int maxConcurrency = 1, int baud = 9600) => new()
    {
        Id = "ser-1",
        Protocol = protocol,
        Transport = "serial",
        MaxConcurrency = maxConcurrency,
        Timeout = TimeSpan.FromMilliseconds(readTimeoutMs + 500),
        ConnectTimeout = TimeSpan.FromSeconds(2),
        Serial = new SerialOptions
        {
            Port = pty.SlavePath,
            Baud = baud,
            DataBits = 8,
            Parity = "none",
            StopBits = "one",
            ReadTimeoutMs = readTimeoutMs,
        },
    };

    private static Device Unit(string id, int unitId, TimeSpan? interval = null) => new()
    {
        Id = id,
        ChannelId = "ser-1",
        UnitId = unitId,
        Interval = interval ?? TimeSpan.FromMilliseconds(20),
        Timeout = TimeSpan.FromMilliseconds(800),
        Points = new List<Point>
        {
            new() { Id = "p1", Address = Address.Register("holding", 100, 1), DataType = DataType.UInt16 },
            new() { Id = "p2", Address = Address.Register("holding", 101, 1), DataType = DataType.UInt16 },
        },
    };

    private static IConn Open(Channel channel, Device device, ModbusLink link)
    {
        var driver = link switch
        {
            ModbusLink.Rtu => ModbusMasterDriver.Rtu(),
            ModbusLink.Ascii => ModbusMasterDriver.Ascii(),
            _ => ModbusMasterDriver.Tcp(),
        };

        return driver.Open(new OpenContext(channel, device, NullLogger.Instance, new MetricsRegistry()));
    }

    private static IReadOnlyList<ReadSpan> Plan(Device device) =>
        ReadPlanner.Plan(device.Points.Select(p => PointRef.Of(device, p)).ToArray());

    private static string Hex(byte[] bytes) => Convert.ToHexString(bytes);

    private static Task RtuGoldenFrame(TestContext t)
    {
        return RunAsync(async () =>
        {
            using var pty = PtyPair.Open();

            var model = new ModbusSlaveModel();
            model.SeedHolding(100, 10, 11);

            using var slave = new RtuSlave(pty, (1, model));
            slave.Start();

            var channel = SerialChannel(pty, "modbus-rtu");
            var device = Unit("unit-1", 1);
            await using var conn = Open(channel, device, ModbusLink.Rtu);

            var samples = await conn.ReadAsync(Plan(device).ToArray(), t.Ct);

            t.Equal(2, samples.Count, "both registers come back over a real tty");
            t.Equal(Quality.Good, samples[0].Quality,
                $"quality is good (error: {samples[0].Error ?? "none"})");
            t.Equal(10d, samples[0].Value!.AsNumber(), "p1");
            t.Equal(11d, samples[1].Value!.AsNumber(), "p2");

            await WaitForAsync(() => slave.RequestCount >= 1, t);

            // 与 testdata/golden/rtu_read_holding_2_regs.json 的 request 逐字节一致。
            t.Equal("01030064000285D4", Hex(slave.Requests[0]),
                "the frame on the wire equals the RTU golden frame");

            t.Ok(slave.Fault is null, $"the fake slave stayed healthy: {slave.Fault}");
        });
    }

    private static Task AsciiGoldenFrame(TestContext t)
    {
        return RunAsync(async () =>
        {
            using var pty = PtyPair.Open();

            var model = new ModbusSlaveModel();
            model.SeedHolding(100, 42, 43);

            using var slave = new AsciiSlave(pty, (1, model));
            slave.Start();

            var channel = SerialChannel(pty, "modbus-ascii");
            var device = Unit("unit-1", 1);
            await using var conn = Open(channel, device, ModbusLink.Ascii);

            var samples = await conn.ReadAsync(Plan(device).ToArray(), t.Ct);

            t.Equal(2, samples.Count, "both registers come back");
            t.Equal(Quality.Good, samples[0].Quality,
                $"quality is good (error: {samples[0].Error ?? "none"})");
            t.Equal(42d, samples[0].Value!.AsNumber(), "p1");
            t.Equal(43d, samples[1].Value!.AsNumber(), "p2");

            await WaitForAsync(() => slave.RequestCount >= 1, t);

            // 与 testdata/golden/ascii_read_holding_2_regs.json 的 request 逐字符一致。
            t.Equal(":01030064000296\r\n", slave.Requests[0],
                "the frame on the wire equals the ASCII golden frame");

            t.Ok(slave.Fault is null, $"the fake slave stayed healthy: {slave.Fault}");
        });
    }

    /// <summary>T16：总线上没有这个站号时必须超时返回，不能挂死。</summary>
    private static Task SilentSlaveTimesOut(TestContext t)
    {
        return RunAsync(async () =>
        {
            using var pty = PtyPair.Open();
            using var slave = new RtuSlave(pty, (1, new ModbusSlaveModel())) { Silent = true };
            slave.Start();

            var channel = SerialChannel(pty, "modbus-rtu", readTimeoutMs: 300);
            var device = Unit("unit-1", 1);
            await using var conn = Open(channel, device, ModbusLink.Rtu);

            var clock = Stopwatch.StartNew();
            try
            {
                await conn.ProbeAsync(t.Ct);
                t.Fail("a silent slave must not answer");
            }
            catch (DriverException ex)
            {
                t.Equal(ErrorKind.Timeout, ex.Kind, "a silent slave is a timeout");
                t.Ok(ex.Temporary, "serial timeouts are temporary: the caller may retry");
            }

            clock.Stop();
            t.Ok(clock.Elapsed < TimeSpan.FromSeconds(2),
                $"the probe returns instead of hanging (took {clock.ElapsedMilliseconds} ms)");
        });
    }

    /// <summary>
    /// T17：无数据时读必须在 deadline 附近返回——不能立刻返回（会把「没读到」当成「读到空」），
    /// 也不能是 timeout 的若干倍（说明在重试或读取长度估算错了）。
    /// </summary>
    private static Task ReadReturnsNearDeadline(TestContext t)
    {
        return RunAsync(async () =>
        {
            const int readTimeoutMs = 400;

            using var pty = PtyPair.Open();
            using var slave = new RtuSlave(pty, (1, new ModbusSlaveModel())) { Silent = true };
            slave.Start();

            var channel = SerialChannel(pty, "modbus-rtu", readTimeoutMs);
            var device = Unit("unit-1", 1);
            await using var conn = Open(channel, device, ModbusLink.Rtu);

            var clock = Stopwatch.StartNew();
            var samples = await conn.ReadAsync(Plan(device).ToArray(), t.Ct);
            clock.Stop();

            t.Equal(2, samples.Count, "a failed window still yields one sample per point");
            t.Equal(Quality.Timeout, samples[0].Quality,
                $"silence maps to a timeout (got {samples[0].Quality}: {samples[0].Error ?? "none"})");

            t.Ok(clock.Elapsed >= TimeSpan.FromMilliseconds(readTimeoutMs * 0.6),
                $"the read does not return instantly (took {clock.ElapsedMilliseconds} ms, deadline {readTimeoutMs} ms)");

            t.Ok(clock.Elapsed <= TimeSpan.FromMilliseconds(readTimeoutMs + 900),
                $"the read returns near the deadline, not a multiple of it (took {clock.ElapsedMilliseconds} ms)");
        });
    }

    /// <summary>T18：关闭端口必须打断阻塞中的读，否则进程退出会卡在最后一次采集上。</summary>
    private static Task CloseInterruptsBlockedRead(TestContext t)
    {
        return RunAsync(async () =>
        {
            using var pty = PtyPair.Open();
            using var slave = new RtuSlave(pty, (1, new ModbusSlaveModel())) { Silent = true };
            slave.Start();

            var channel = SerialChannel(pty, "modbus-rtu", readTimeoutMs: 5000);
            var device = Unit("unit-1", 1);
            var conn = Open(channel, device, ModbusLink.Rtu);

            var read = Task.Run(async () =>
            {
                try
                {
                    await conn.ReadAsync(Plan(device).ToArray(), CancellationToken.None);
                }
                catch
                {
                    // 关闭期的异常类型无关紧要，这里只关心「它有没有返回」。
                }
            });

            await Task.Delay(200, t.Ct);
            t.Ok(!read.IsCompleted, "the read is genuinely blocked on the silent slave");

            var clock = Stopwatch.StartNew();
            await conn.DisposeAsync();

            var finished = await Task.WhenAny(read, Task.Delay(TimeSpan.FromSeconds(2), t.Ct));
            t.Ok(finished == read, "closing the port interrupts the blocked read");
            t.Ok(clock.Elapsed < TimeSpan.FromSeconds(2),
                $"the blocked read returns within 2s of closing (took {clock.ElapsedMilliseconds} ms)");
        });
    }

    /// <summary>
    /// T15：一条 RS-485 总线上挂两台从站，并发采集时响应不能张冠李戴。
    ///
    /// 从站侧在两个方向上都做了检查：值必须来自各自站号的寄存器；并且在响应发出前
    /// 总线上不允许出现第二个请求（真正的半双工）。
    /// </summary>
    private static Task HalfDuplexDoesNotCrossResponses(TestContext t)
    {
        return RunAsync(async () =>
        {
            using var pty = PtyPair.Open();

            var unit1 = new ModbusSlaveModel();
            unit1.SeedHolding(100, 10, 11);

            var unit2 = new ModbusSlaveModel();
            unit2.SeedHolding(100, 20, 21);

            using var slave = new RtuSlave(pty, (1, unit1), (2, unit2));
            slave.Start();

            var channel = SerialChannel(pty, "modbus-rtu", readTimeoutMs: 500, maxConcurrency: 2);
            var cache = new PointCache();
            using var gate = new ChannelGate(2);
            var driver = ModbusMasterDriver.Rtu();
            var options = new SchedulerOptions { Tick = TimeSpan.FromMilliseconds(10) };

            var d1 = Unit("unit-1", 1);
            var d2 = Unit("unit-2", 2);

            await using var actor1 = new DeviceActor(d1, channel, driver, cache, gate, NullLogger.Instance, options);
            await using var actor2 = new DeviceActor(d2, channel, driver, cache, gate, NullLogger.Instance, options);

            actor1.Start();
            actor2.Start();
            await Task.Delay(500, t.Ct);

            t.Ok(slave.RequestCount >= 6,
                $"both devices really collected ({slave.RequestCount} requests on the shared bus)");

            t.Ok(!slave.Overlapped,
                "no request hit the bus while the previous response was still pending");

            t.Ok(slave.Fault is null, $"the fake slave stayed healthy: {slave.Fault}");

            foreach (var (deviceId, want1, want2) in new[] { ("unit-1", 10d, 11d), ("unit-2", 20d, 21d) })
            {
                var s1 = cache.Get(deviceId, "p1");
                var s2 = cache.Get(deviceId, "p2");

                t.Ok(s1 is not null && s2 is not null, $"{deviceId} has samples");
                t.Equal(Quality.Good, s1!.Quality, $"{deviceId}.p1 quality");

                t.Equal(want1, s1.Value!.AsNumber(),
                    $"{deviceId}.p1 comes from its own unit, not the other one");
                t.Equal(want2, s2!.Value!.AsNumber(),
                    $"{deviceId}.p2 comes from its own unit, not the other one");
            }
        });
    }

    /// <summary>
    /// Modbus over Serial Line 要求两帧之间至少 3.5 个字符时间的静默。
    /// 用 300 bps 把这段静默放大到约 128 ms：没有实现时两个事务间隔只有几毫秒，
    /// 有实现时至少 100 ms，断言不会受调度抖动影响。
    /// </summary>
    private static Task RtuWaitsBetweenFrames(TestContext t)
    {
        return RunAsync(async () =>
        {
            using var pty = PtyPair.Open();

            var model = new ModbusSlaveModel();
            model.SeedHolding(100, 10, 11);

            using var slave = new RtuSlave(pty, (1, model)) { ReplyDelay = TimeSpan.Zero };
            slave.Start();

            var channel = SerialChannel(pty, "modbus-rtu", readTimeoutMs: 500, baud: 300);
            var device = Unit("unit-1", 1);
            await using var conn = Open(channel, device, ModbusLink.Rtu);

            var spans = Plan(device).ToArray();

            var first = await conn.ReadAsync(spans, t.Ct);
            var second = await conn.ReadAsync(spans, t.Ct);

            t.Equal(Quality.Good, first[0].Quality, "the first transaction succeeds");
            t.Equal(Quality.Good, second[0].Quality, "the second transaction succeeds");

            await WaitForAsync(() => slave.RequestCount >= 2, t);

            var stamps = slave.RequestStamps;
            var gap = Stopwatch.GetElapsedTime(stamps[0], stamps[1]);

            t.Ok(gap >= TimeSpan.FromMilliseconds(100),
                $"consecutive requests are separated by 3.5 character times at 300 bps " +
                $"(got {gap.TotalMilliseconds:F0} ms)");

            t.Ok(slave.Fault is null, $"the fake slave stayed healthy: {slave.Fault}");
        });
    }

    private static async Task WaitForAsync(Func<bool> condition, TestContext t, int timeoutMs = 2000)
    {
        var clock = Stopwatch.StartNew();
        while (!condition())
        {
            if (clock.ElapsedMilliseconds > timeoutMs)
                t.Fail($"condition was not met within {timeoutMs} ms");

            await Task.Delay(10, t.Ct);
        }
    }

    private static Task RunAsync(Func<Task> body) => body();
}
