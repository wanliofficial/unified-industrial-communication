using System.Buffers.Binary;
using System.Diagnostics;
using System.Net;
using System.Net.Sockets;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Modbus;
using Uic.Engine;
using Uic.Obs;
using Uic.Sim.Modbus;

namespace Uic.Tests;

/// <summary>
/// 主站 ↔ 从站回环（用例 T1/T3/T4/T6/T9/T10/T16）。
///
/// 用内置从站而不是假字节流：这才会同时走 TCP 分帧、事务号匹配、
/// 超时与异常码映射这几条真实路径。假字节流只能验证「我以为的字节」。
///
/// 串口专属的 T15/T17/T18 在 `Serial/SerialTests.cs` 里，跑在真实伪终端上；
/// 这里只保留一条 TCP 侧的沉默从站用例（T16），两条链路都要覆盖超时语义。
/// </summary>
internal static class LoopbackTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("loopback/probe_succeeds", ProbeSucceeds));
        tests.Add(new TestCase("loopback/read_returns_values_in_order", ReadReturnsValuesInOrder));
        tests.Add(new TestCase("loopback/read_maps_exception_to_quality", ReadMapsExceptionToQuality));
        tests.Add(new TestCase("loopback/write_then_read_back", WriteThenReadBack));
        tests.Add(new TestCase("loopback/write_to_missing_register_is_rejected", WriteRejectsMissingRegister));
        tests.Add(new TestCase("loopback/silent_slave_times_out", SilentSlaveTimesOut));
        tests.Add(new TestCase("loopback/cancellation_returns_promptly", CancellationReturnsPromptly));
        tests.Add(new TestCase("loopback/close_is_idempotent", CloseIsIdempotent));
        tests.Add(new TestCase("loopback/planner_windows_round_trip", PlannerWindowsRoundTrip));
        tests.Add(new TestCase("loopback/probe_times_out", ProbeTimesOut));
        tests.Add(new TestCase("loopback/empty_batch_is_a_no_op", EmptyBatchIsANoOp));
        tests.Add(new TestCase("loopback/fragmented_response_is_reassembled", FragmentedResponseIsReassembled));
        tests.Add(new TestCase("loopback/file_record_read_and_write_round_trip", FileRecordRoundTrip));
        tests.Add(new TestCase("loopback/missing_file_record_maps_to_address_error", MissingFileRecordMapsToAddressError));
        tests.Add(new TestCase("loopback/file_offset_is_rejected", FileOffsetIsRejected));
    }

    private static (Channel Channel, Device Device) Setup(int port) => (
        new Channel
        {
            Id = "ch-1",
            Protocol = "modbus-tcp",
            Transport = "tcp",
            Host = "127.0.0.1",
            Port = port,
            Timeout = TimeSpan.FromMilliseconds(800),
            ConnectTimeout = TimeSpan.FromSeconds(2),
            MaxConcurrency = 1,
        },
        new Device
        {
            Id = "slave-1",
            ChannelId = "ch-1",
            Interval = TimeSpan.FromMilliseconds(200),
            UnitId = 1,
            Timeout = TimeSpan.FromMilliseconds(500),
            Points = new List<Point>(),
        });

    private static IConn Open(Channel channel, Device device) => ModbusMasterDriver.Tcp().Open(
        new OpenContext(channel, device, NullLogger.Instance, new MetricsRegistry()));

    private static async Task<ModbusTcpSlaveServer> StartSlaveAsync(TestContext t, ModbusSlaveModel model)
    {
        var server = new ModbusTcpSlaveServer(IPAddress.Loopback, 0, model);
        server.Start();
        t.Ok(server.Port > 0, "the slave picked a port");
        return await Task.FromResult(server);
    }

    private static PointRef U16(string id, int offset) =>
        new(id, Address.Register("holding", offset, 1), DataType.UInt16, ByteOrder.ABCD, 1);

    private static Task ProbeSucceeds(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            await using var conn = Open(channel, device);
            await conn.ProbeAsync(t.Ct);

            // 探活读的是 0 号保持寄存器，值本身不重要，能读通就说明链路可用。
        });
    }

    private static Task ReadReturnsValuesInOrder(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            model.SeedHolding(100, 10, 11, 12);
            model.SeedInput(0, 1, 2);
            model.SeedCoils(0, true, false, true, true);

            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            var points = new[]
            {
                U16("a", 100),
                U16("b", 101),
                U16("c", 102),
                new PointRef("in-0", Address.Register("input", 0, 1), DataType.UInt16, ByteOrder.ABCD, 1),
                new PointRef("coil-0", Address.Register("coil", 0, 1), DataType.Bool, ByteOrder.ABCD, 1),
                new PointRef("coil-3", Address.Register("coil", 3, 1), DataType.Bool, ByteOrder.ABCD, 1),
            };

            await using var conn = Open(channel, device);

            var spans = ReadPlanner.Plan(points);

            // holding 三点连续 → 1 个窗口；input → 1 个；coil 0 与 coil 3 之间有间隔 → 2 个。
            t.Equal(4, spans.Count, "holding / input / non-adjacent coils produce four windows");

            var samples = await conn.ReadAsync(spans, t.Ct);

            // 结果数量必须与点位数量一致，顺序也必须对齐——上层按位置分配，不做按 id 查找。
            t.Equal(points.Length, samples.Count, "one sample per point");

            var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);

            t.Equal(10d, byId["a"].Value!.AsNumber(), "a");
            t.Equal(11d, byId["b"].Value!.AsNumber(), "b");
            t.Equal(12d, byId["c"].Value!.AsNumber(), "c");
            t.Equal(1d, byId["in-0"].Value!.AsNumber(), "input register");
            t.Equal(true, byId["coil-0"].Value!.Bool, "coil 0");
            t.Equal(true, byId["coil-3"].Value!.Bool, "coil 3");

            foreach (var sample in samples)
            {
                t.Equal(Quality.Good, sample.Quality, $"{sample.PointId}: quality");
            }
        });
    }

    private static Task ReadMapsExceptionToQuality(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            // 65535 起读 2 个寄存器越过从站地址空间：从站回异常码 02。
            var bad = new[]
            {
                new PointRef("beyond", Address.Register("holding", 65535, 2), DataType.UInt32, ByteOrder.ABCD, 2),
            };
            var spans = ReadPlanner.Plan(bad);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(spans, t.Ct);

            // 单点失败不抛异常：驱动把错误折成质量码，整轮采集继续。
            t.Equal(1, samples.Count, "the failing point still yields a sample");
            t.Equal(Quality.AddressError, samples[0].Quality, "an out-of-range window maps to address_error");
            t.Ok(samples[0].Value is null, "a bad sample carries no value");

            // 越界在编码阶段就被拦住了（连一帧都不发）：这比让从站回异常码更安全。
            t.Contains(samples[0].Error ?? string.Empty, "address space", "the reason is preserved for the UI");
        });
    }

    private static Task WriteThenReadBack(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            model.SeedHolding(20, 7);

            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            var point = U16("setpoint", 20);
            await using var conn = Open(channel, device);

            var write = await conn.WriteAsync(new[]
            {
                new WriteCommand(point, DataValue.Number(DataType.UInt16, 42)),
            }, t.Ct);

            t.Equal(1, write.Count, "one write result");
            t.Ok(write[0].Ok, "write is accepted");
            t.Equal(Quality.Good, write[0].Quality, "write quality");
            t.Equal((ushort)42, model.Holding[20], "the slave actually holds the new value");

            // T7：写后回读必须一致。
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
            t.Equal(42d, samples[0].Value!.AsNumber(), "read-back matches what was written");
        });
    }

    private static Task WriteRejectsMissingRegister(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            await using var conn = Open(channel, device);

            // 写只读区（输入寄存器）必须在驱动层就被拦下，不能发到线上。
            var readOnly = new PointRef("in", Address.Register("input", 0, 1), DataType.UInt16, ByteOrder.ABCD, 1);
            var results = await conn.WriteAsync(new[]
            {
                new WriteCommand(readOnly, DataValue.Number(DataType.UInt16, 1)),
            }, t.Ct);

            t.Equal(1, results.Count, "the point still gets a result");
            t.Ok(!results[0].Ok, "write is rejected");
            t.Equal(Quality.Disabled, results[0].Quality, "permission failures map to disabled");
            t.Contains(results[0].Error ?? string.Empty, "read-only", "the reason names the read-only area");
        });
    }

    private static Task SilentSlaveTimesOut(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 一个只 accept、永不回包的监听器：模拟地址不存在的从站（T16）。
            var listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();

            var accept = Task.Run(async () =>
            {
                try
                {
                    var client = await listener.AcceptTcpClientAsync();
                    await Task.Delay(TimeSpan.FromSeconds(10));
                    client.Dispose();
                }
                catch
                {
                    // 测试结束时会打断。
                }
            });

            try
            {
                var (channel, device) = Setup(((IPEndPoint)listener.LocalEndpoint).Port);
                channel = new Channel
                {
                    Id = channel.Id,
                    Protocol = channel.Protocol,
                    Transport = channel.Transport,
                    Host = channel.Host,
                    Port = channel.Port,
                    Timeout = TimeSpan.FromMilliseconds(300),
                    ConnectTimeout = channel.ConnectTimeout,
                    MaxConcurrency = 1,
                };

                await using var conn = Open(channel, device);

                var clock = Stopwatch.StartNew();
                var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { U16("p", 0) }), t.Ct);
                clock.Stop();

                t.Equal(Quality.Timeout, samples[0].Quality, "a silent slave maps to timeout");
                t.Ok(clock.Elapsed < TimeSpan.FromSeconds(5),
                    $"the read returns near the configured timeout (took {clock.ElapsedMilliseconds} ms)");
            }
            finally
            {
                listener.Stop();
                await Task.WhenAny(accept, Task.Delay(100));
            }
        });
    }

    private static Task CancellationReturnsPromptly(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 连接到不存在的端口会快速失败；这里要验证的是「已取消的令牌不会被忽略」。
            var model = new ModbusSlaveModel();
            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            await using var conn = Open(channel, device);

            using var cts = new CancellationTokenSource();
            await cts.CancelAsync();

            var clock = Stopwatch.StartNew();
            try
            {
                await conn.ReadAsync(ReadPlanner.Plan(new[] { U16("p", 0) }), cts.Token);
                t.Fail("a cancelled read must not succeed silently");
            }
            catch (OperationCanceledException)
            {
                // 预期路径。
            }

            clock.Stop();
            t.Ok(clock.Elapsed < TimeSpan.FromMilliseconds(500),
                $"cancellation is observed promptly (took {clock.ElapsedMilliseconds} ms)");
        });
    }

    private static Task CloseIsIdempotent(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            var conn = Open(channel, device);
            await conn.ProbeAsync(t.Ct);

            await conn.DisposeAsync();
            await conn.DisposeAsync();
            await conn.DisposeAsync();

            // T10：连续关闭不得抛异常。
        });
    }

    private static Task PlannerWindowsRoundTrip(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            for (var i = 0; i < 200; i++) model.Holding[i] = (ushort)i;

            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            var points = Enumerable.Range(0, 200).Select(i => U16("p" + i, i)).ToArray();
            var spans = ReadPlanner.Plan(points);

            t.Equal(2, spans.Count, "200 registers need two windows");

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(spans, t.Ct);

            t.Equal(200, samples.Count, "every point is read");

            // 每个值都必须落在自己的地址上：窗口切分错了，这里会整体错位。
            foreach (var sample in samples)
            {
                var expected = double.Parse(sample.PointId[1..], System.Globalization.CultureInfo.InvariantCulture);
                t.Equal(expected, sample.Value!.AsNumber(), $"{sample.PointId}");
            }
        });
    }

    private static Task ProbeTimesOut(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 只 accept、不回包：探活必须在超时内失败，并且标记为「可重试」。
            var listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();

            var accept = Task.Run(async () =>
            {
                try
                {
                    var client = await listener.AcceptTcpClientAsync();
                    await Task.Delay(TimeSpan.FromSeconds(10));
                    client.Dispose();
                }
                catch
                {
                    // 测试结束时会打断。
                }
            });

            try
            {
                var (channel, device) = Setup(((IPEndPoint)listener.LocalEndpoint).Port);
                await using var conn = Open(channel, device);

                var clock = Stopwatch.StartNew();
                try
                {
                    await conn.ProbeAsync(t.Ct);
                    t.Fail("probing a silent slave must not succeed");
                }
                catch (DriverException ex)
                {
                    t.Equal(ErrorKind.Timeout, ex.Kind, "a silent slave is a timeout");
                    t.Ok(ex.Temporary, "timeouts are temporary: the caller may retry");
                }

                clock.Stop();
                t.Ok(clock.Elapsed < TimeSpan.FromSeconds(5),
                    $"probe returns near the configured timeout (took {clock.ElapsedMilliseconds} ms)");
            }
            finally
            {
                listener.Stop();
                await Task.WhenAny(accept, Task.Delay(100));
            }
        });
    }

    private static Task EmptyBatchIsANoOp(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            await using var conn = Open(channel, device);

            // T5：空批量既不能抛异常，也不能发一个功能码为 0 的帧出去。
            var samples = await conn.ReadAsync(Array.Empty<ReadSpan>(), t.Ct);
            t.Equal(0, samples.Count, "reading nothing yields nothing");

            var writes = await conn.WriteAsync(Array.Empty<WriteCommand>(), t.Ct);
            t.Equal(0, writes.Count, "writing nothing yields nothing");
        });
    }

    private static Task FragmentedResponseIsReassembled(TestContext t)
    {
        return RunAsync(async () =>
        {
            // T12：响应分片到达。真实网络本来就会这样交付，但很少在测试里发生，
            // 于是「读满一个整包」这条路径常年没人验证。
            var listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();

            var server = Task.Run(async () =>
            {
                try
                {
                    using var client = await listener.AcceptTcpClientAsync();
                    await using var stream = client.GetStream();

                    var mbap = new byte[7];
                    await stream.ReadExactlyAsync(mbap);

                    var length = BinaryPrimitives.ReadUInt16BigEndian(mbap.AsSpan(4, 2));
                    var pdu = new byte[length - 1];
                    await stream.ReadExactlyAsync(pdu);

                    // 读 100..101 两个保持寄存器的响应：值为 10 / 11。
                    var response = Convert.FromHexString("000100000007010304000A000B");

                    // 事务号必须回显，否则驱动会把它当成迟到的旧响应丢弃。
                    mbap.AsSpan(0, 2).CopyTo(response);

                    var cuts = new[] { 0, 3, 9, response.Length };
                    for (var i = 0; i < cuts.Length - 1; i++)
                    {
                        await stream.WriteAsync(response.AsMemory(cuts[i], cuts[i + 1] - cuts[i]));
                        await stream.FlushAsync();
                        await Task.Delay(15);
                    }
                }
                catch
                {
                    // 测试结束时会打断。
                }
            });

            try
            {
                var (channel, device) = Setup(((IPEndPoint)listener.LocalEndpoint).Port);
                await using var conn = Open(channel, device);

                var points = new[] { U16("p1", 100), U16("p2", 101) };
                var samples = await conn.ReadAsync(ReadPlanner.Plan(points), t.Ct);

                t.Equal(2, samples.Count, "both points are decoded from the fragmented response");
                t.Equal(Quality.Good, samples[0].Quality, "quality is good despite the split");
                t.Equal(10d, samples[0].Value!.AsNumber(), "p1");
                t.Equal(11d, samples[1].Value!.AsNumber(), "p2");
            }
            finally
            {
                listener.Stop();
                await Task.WhenAny(server, Task.Delay(100));
            }
        });
    }

    private static PointRef FileU16(string id, int fileId, int recordId, int registers = 1) =>
        new(id, Address.File(fileId, recordId), DataType.UInt16, ByteOrder.ABCD,
            Length: registers, Count: registers > 1 ? registers : 0);

    /// <summary>
    /// FC20/21 走完整主从回环：写进文件记录、当场从从站模型核对、再读回来。
    /// 用内置从站而不是假字节流，才会真的经过 FC21 请求编码、从站记录写入、
    /// FC20 响应解析这条真实路径。
    /// </summary>
    private static Task FileRecordRoundTrip(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            model.SeedFileRecord(1, 0, 100, 200, 300);

            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            // 一条文件记录 3 个寄存器；点位读前 2 个。
            var point = FileU16("recipe-a", 1, 0, registers: 2);

            await using var conn = Open(channel, device);

            // 先读：从站模型里预置的是 100/200。
            var read = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
            t.Equal(1, read.Count, "one sample for a file point");
            t.Equal(Quality.Good, read[0].Quality, "file read quality");
            t.Equal(100d, read[0].Value!.NumberAt(0), "first register of the record");
            t.Equal(200d, read[0].Value!.NumberAt(1), "second register of the record");

            // 再写：整条记录前 2 个寄存器改成 7/8。
            var write = await conn.WriteAsync(new[]
            {
                new WriteCommand(point, DataValue.Numbers(DataType.UInt16, new[] { 7d, 8d })),
            }, t.Ct);

            t.Equal(1, write.Count, "one write result");
            t.Ok(write[0].Ok, $"file write is accepted: {write[0].Error}");
            t.Equal(Quality.Good, write[0].Quality, "file write quality");
            t.Equal((ushort)7, model.GetFileRecord(1, 0)![0], "the slave record holds the new first register");
            t.Equal((ushort)8, model.GetFileRecord(1, 0)![1], "the slave record holds the new second register");
            t.Equal((ushort)300, model.GetFileRecord(1, 0)![2], "the untouched third register is preserved");

            // 写后回读必须一致。
            var again = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
            t.Equal(7d, again[0].Value!.NumberAt(0), "read-back matches what was written");
            t.Equal(8d, again[0].Value!.NumberAt(1), "read-back matches what was written");
        });
    }

    /// <summary>
    /// 不存在的文件/记录必须折成 address_error，而不是「读回一串 0」：
    /// 0 是一个合法读数，运维会把「没有这个记录」当成「记录被清零」。
    /// </summary>
    private static Task MissingFileRecordMapsToAddressError(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            var point = FileU16("ghost", 9, 9);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "the failing point still yields a sample");
            t.Equal(Quality.AddressError, samples[0].Quality, "a missing record maps to address_error");
            t.Ok(samples[0].Value is null, "a bad sample carries no value");
        });
    }

    /// <summary>
    /// 文件寻址没有「记录内偏移」：FC20/21 的引用是「文件号 + 记录号 + 记录长度」。
    /// 配了非 0 的 offset 却静默忽略，读到的是「从记录头开始的一段」——值像对的、位置全错。
    /// </summary>
    private static Task FileOffsetIsRejected(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new ModbusSlaveModel();
            model.SeedFileRecord(1, 0, 11, 22, 33);

            await using var server = await StartSlaveAsync(t, model);
            var (channel, device) = Setup(server.Port);

            var point = new PointRef("shifted", Address.File(1, 0, offset: 1),
                DataType.UInt16, ByteOrder.ABCD, 1);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "the rejected point still yields a sample");
            t.Ok(samples[0].Value is null, "no value is produced for a rejected offset");
            t.Equal(Quality.AddressError, samples[0].Quality, "a non-zero file offset is an address error");
            t.Contains(samples[0].Error ?? string.Empty, "record-internal offset",
                "the reason explains that file addressing has no offset");
        });
    }

    private static Task RunAsync(Func<Task> body) => body();

}
