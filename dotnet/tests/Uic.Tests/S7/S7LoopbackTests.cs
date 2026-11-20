using System.Net;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Codec.S7;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.S7;
using Uic.Engine;
using Uic.Obs;
using Uic.Sim.S7;

namespace Uic.Tests;

/// <summary>
/// S7 主站 ↔ 内置从站回环。
///
/// 用真 TCP 而不是假字节流：这样 COTP 握手、PDU 协商、TPKT 分帧、位偏移换算
/// 都走真实路径。握手最容易在这里坏——它有三个来回（连接请求 / 连接确认 /
/// PDU 协商），任何一步的字段写错，现象都是「连上了但读不出值」。
/// </summary>
internal static class S7LoopbackTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("s7/probe_establishes_full_session", ProbeEstablishesFullSession));
        tests.Add(new TestCase("s7/read_db_bytes", ReadDbBytes));
        tests.Add(new TestCase("s7/read_mixed_areas_and_types", ReadMixedAreasAndTypes));
        tests.Add(new TestCase("s7/read_bit_address", ReadBitAddress));
        tests.Add(new TestCase("s7/write_then_read_back", WriteThenReadBack));
        tests.Add(new TestCase("s7/write_bit_toggles_only_that_bit", WriteBitTogglesOnlyThatBit));
        tests.Add(new TestCase("s7/write_to_input_area_is_rejected", WriteToInputIsRejected));
        tests.Add(new TestCase("s7/read_beyond_block_is_address_error", ReadBeyondBlockIsAddressError));
        tests.Add(new TestCase("s7/bool_without_bit_is_rejected", BoolWithoutBitIsRejected));
        tests.Add(new TestCase("s7/array_point_reads_all_elements", ArrayPointReadsAllElements));
        tests.Add(new TestCase("s7/negotiated_pdu_limits_the_window", NegotiatedPduLimitsTheWindow));
        tests.Add(new TestCase("s7/unsplittable_point_is_reported", OversizedSinglePointIsReported));
        tests.Add(new TestCase("s7/silent_plc_times_out", SilentPlcTimesOut));
        tests.Add(new TestCase("s7/reconnect_after_peer_closes", ReconnectAfterPeerCloses));
        tests.Add(new TestCase("s7/metadata_declares_capabilities", MetadataDeclaresCapabilities));
    }

    private static (Channel Channel, Device Device) Setup(int port, Dictionary<string, string>? labels = null) => (
        new Channel
        {
            Id = "s7-ch",
            Protocol = "s7",
            Transport = "tcp",
            Host = "127.0.0.1",
            Port = port,
            Timeout = TimeSpan.FromMilliseconds(900),
            ConnectTimeout = TimeSpan.FromSeconds(2),
            MaxConcurrency = 1,
        },
        new Device
        {
            Id = "plc-1",
            ChannelId = "s7-ch",
            Interval = TimeSpan.FromMilliseconds(200),
            Timeout = TimeSpan.FromMilliseconds(600),
            Labels = labels ?? new Dictionary<string, string>(StringComparer.Ordinal),
            Points = new List<Point>(),
        });

    private static IConn Open(Channel channel, Device device) => new S7MasterDriver().Open(
        new OpenContext(channel, device, NullLogger.Instance, new MetricsRegistry()));

    private static async Task<S7TcpSlaveServer> StartSlaveAsync(S7SlaveModel model)
    {
        var server = new S7TcpSlaveServer(IPAddress.Loopback, 0, model);
        server.Start();
        await Task.Yield();
        return server;
    }

    private static PointRef U16(string id, string area, int offset) =>
        new(id, Address.Register(area, offset, 1), DataType.UInt16, ByteOrder.ABCD, 1);

    private static PointRef I32(string id, string area, int offset) =>
        new(id, Address.Register(area, offset, 2), DataType.Int32, ByteOrder.ABCD, 2);

    private static PointRef Bit(string id, string area, int offset, int bit) =>
        new(id, Address.Register(area, offset, 0, bit), DataType.Bool, ByteOrder.ABCD, 1);

    private static Task ProbeEstablishesFullSession(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            model.Flags[0] = 0x00;
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);
            await using var conn = Open(channel, device);
            await conn.ProbeAsync(t.Ct);

            // 探活至少要打出 COTP 连接 + PDU 协商 + 一次读：三个请求。
            t.Ok(model.RequestCount >= 2, $"the probe talked S7 (requests: {model.RequestCount})");
        });
    }

    private static Task ReadDbBytes(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            model.SeedDataBlock(1, 0, 0x12, 0x34);
            model.SeedDataBlock(1, 4, 0xFF, 0xFE);
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);
            var points = new[] { U16("a", "db1", 0), U16("b", "db1", 4), I32("c", "db1", 8) };
            model.SeedDataBlock(1, 8, 0xFF, 0xFF, 0xFF, 0xFF);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(points), t.Ct);

            var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);
            t.Equal(points.Length, samples.Count, "one sample per point");
            t.Equal(4660d, byId["a"].Value!.AsNumber(), "DB1.DBW0 = 0x1234");
            t.Equal(65534d, byId["b"].Value!.AsNumber(), "DB1.DBW4 = 0xFFFE is unsigned");
            t.Equal(-1d, byId["c"].Value!.AsNumber(), "DB1.DBD8 is a signed 32-bit");
            t.Equal(Quality.Good, byId["a"].Quality, "quality");
        });
    }

    private static Task ReadMixedAreasAndTypes(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            model.Inputs[2] = 0x00;
            model.Inputs[3] = 0x2A;
            model.Outputs[0] = 0x01;
            model.Outputs[1] = 0x02;
            model.Flags[6] = 0x00;
            model.Flags[7] = 0x63;
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);
            var points = new[]
            {
                U16("i", "i", 2),
                U16("q", "q", 0),
                U16("m", "m", 6),
            };

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(points), t.Ct);
            var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);

            t.Equal(42d, byId["i"].Value!.AsNumber(), "IB2/IB3 as a word");
            t.Equal(258d, byId["q"].Value!.AsNumber(), "QB0/QB1 = 0x0102");
            t.Equal(99d, byId["m"].Value!.AsNumber(), "MB6/MB7");

            foreach (var s in samples) t.Equal(Quality.Good, s.Quality, $"{s.PointId} quality");
        });
    }

    private static Task ReadBitAddress(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            model.SeedDataBlock(2, 3, 0b0000_1000);
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);
            var points = new[]
            {
                Bit("on", "db2", 3, 3),
                Bit("off", "db2", 3, 2),
            };

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(points), t.Ct);
            var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);

            t.Equal(true, byId["on"].Value!.Bool, "DB2.DBX3.3");
            t.Equal(false, byId["off"].Value!.Bool, "DB2.DBX3.2");
        });
    }

    private static Task WriteThenReadBack(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);
            var point = I32("setpoint", "db3", 10);

            await using var conn = Open(channel, device);

            var writes = await conn.WriteAsync(new[]
            {
                new WriteCommand(point, DataValue.Number(DataType.Int32, 1234)),
            }, t.Ct);

            t.Equal(1, writes.Count, "one result");
            t.Ok(writes[0].Ok, $"write accepted ({writes[0].Error})");
            t.Equal(Quality.Good, writes[0].Quality, "write quality");

            // 从站内存里必须真的改了：这是「驱动写对了」而不是「驱动以为写对了」。
            var block = model.DataBlock(3);
            t.Equal(0x00, block[10], "DB3.DBB10");
            t.Equal(0x00, block[11], "DB3.DBB11");
            t.Equal(0x04, block[12], "DB3.DBB12");
            t.Equal(0xD2, block[13], "DB3.DBB13 = 1234");

            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
            t.Equal(1234d, samples[0].Value!.AsNumber(), "read back");
        });
    }

    private static Task WriteBitTogglesOnlyThatBit(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            model.SeedDataBlock(4, 0, 0b1111_1111);
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);
            var point = Bit("flag", "db4", 0, 2);

            await using var conn = Open(channel, device);

            var writes = await conn.WriteAsync(
                new[] { new WriteCommand(point, DataValue.Boolean(false)) }, t.Ct);

            t.Ok(writes[0].Ok, $"write accepted ({writes[0].Error})");

            // 清一位不能碰其它七位：位写的典型错误是整字节覆盖。
            t.Equal(0b1111_1011, (int)model.DataBlock(4)[0], "only bit 2 was cleared");
        });
    }

    private static Task WriteToInputIsRejected(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);
            var point = U16("i", "i", 0);

            await using var conn = Open(channel, device);
            var writes = await conn.WriteAsync(
                new[] { new WriteCommand(point, DataValue.Number(DataType.UInt16, 7)) }, t.Ct);

            // 输入区在真实 CPU 上由过程映像写入，写请求一律被拒。
            t.Ok(!writes[0].Ok, "the PLC refused the write");
            t.Contains(writes[0].Error ?? string.Empty, "not allowed", "the reason is preserved");
            t.Equal(0, (int)model.Inputs[0], "the input image was not modified");
        });
    }

    private static Task ReadBeyondBlockIsAddressError(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);

            // 越过 DB 长度：从站回 0x05（地址越界），必须落成 address_error 而不是 bad。
            // 这两个质量码的分野是「点表写错」与「链路出问题」，上层据此决定要不要熔断。
            var point = U16("missing", "db1", 8000);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "the failing point still yields a sample");
            t.Equal(Quality.AddressError, samples[0].Quality,
                "an out-of-range address is a configuration problem, not a comm problem");
            t.Ok(samples[0].Value is null, "a bad sample carries no value");
            t.Contains(samples[0].Error ?? string.Empty, "out of range", "the PLC's reason is preserved");
        });
    }

    private static Task BoolWithoutBitIsRejected(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);

            // bool 不带 bit：读回来只能是「整字节是否为 0」，那种猜测必须拒绝。
            var point = new PointRef("bad", Address.Register("db1", 0, 1), DataType.Bool, ByteOrder.ABCD, 1);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "one sample");
            t.Equal(Quality.AddressError, samples[0].Quality, "a bool point needs address.bit");
            t.Contains(samples[0].Error ?? string.Empty, "address.bit", "the message says what is missing");
        });
    }

    private static Task ArrayPointReadsAllElements(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            model.SeedDataBlock(5, 0, 0x00, 0x0A, 0x00, 0x0B, 0x00, 0x0C);
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);

            // 3 个 uint16 的定长数组：占 6 字节，读回来 3 个元素。
            var point = new PointRef(
                "arr", Address.Register("db5", 0, 3), DataType.UInt16, ByteOrder.ABCD, 3, 3);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            var value = samples[0].Value!;
            t.Ok(value.Array is not null, "the value is an array");
            t.Equal(3, value.Array!.Count, "three elements");
            t.Equal(10d, value.Array![0], "element 0");
            t.Equal(11d, value.Array![1], "element 1");
            t.Equal(12d, value.Array![2], "element 2");
        });
    }

    private static Task NegotiatedPduLimitsTheWindow(TestContext t)
    {
        return RunAsync(async () =>
        {
            // PLC 只给 240 字节 PDU。40 个点一项 12 字节请求项，必然撑爆——
            // 驱动该自己切片，而不是让整窗失败（现场一台 PLC 上百点是常态）。
            var model = new S7SlaveModel { NegotiatedPdu = S7Constants.PduSmall };
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);

            var block = model.DataBlock(1);
            for (var i = 0; i < 40; i++)
            {
                block[i * 2] = 0x00;
                block[i * 2 + 1] = (byte)i;
            }

            var points = Enumerable.Range(0, 40)
                .Select(i => U16($"p{i}", "db1", i * 2))
                .ToArray();

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(points), t.Ct);

            t.Equal(points.Length, samples.Count, "every point yields a sample");

            foreach (var s in samples)
                t.Equal(Quality.Good, s.Quality, $"{s.PointId}: quality ({s.Error})");

            t.Equal(39d, samples[^1].Value!.AsNumber(), "the last point is p39");

            // 40 点 × 18 字节 / 228 字节预算 → 至少两个请求，加握手两帧。
            t.Ok(model.RequestCount >= 4,
                $"the driver split the window instead of failing ({model.RequestCount} requests)");
        });
    }

    private static Task OversizedSinglePointIsReported(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel { NegotiatedPdu = S7Constants.PduSmall };
            await using var server = await StartSlaveAsync(model);

            var (channel, device) = Setup(server.Port);

            // 单个点位就要读 400 字节：切不动，只能明确失败并指出 PDU 上限。
            var point = new PointRef(
                "big", Address.Register("db1", 0, 200), DataType.Bytes, ByteOrder.ABCD, 200);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "one sample");
            t.Equal(Quality.Bad, samples[0].Quality, "an unsplittable point fails");
            t.Contains(samples[0].Error ?? string.Empty, "240", "the error names the PLC's PDU limit");
        });
    }

    private static Task SilentPlcTimesOut(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 只 accept 不回应：模拟「端口开着但 PLC 不理你」这种最常见也最误导人的故障。
            var listener = new System.Net.Sockets.TcpListener(IPAddress.Loopback, 0);
            listener.Start();

            using var cts = CancellationTokenSource.CreateLinkedTokenSource(t.Ct);
            var server = Task.Run(async () =>
            {
                try
                {
                    using var client = await listener.AcceptTcpClientAsync(cts.Token);
                    await Task.Delay(Timeout.Infinite, cts.Token);
                }
                catch
                {
                    // 测试结束时会打断。
                }
            });

            try
            {
                var (channel, device) = Setup(((IPEndPoint)listener.LocalEndpoint).Port);
                device = new Device
                {
                    Id = device.Id,
                    ChannelId = device.ChannelId,
                    Interval = device.Interval,
                    Timeout = TimeSpan.FromMilliseconds(400),
                    Labels = device.Labels,
                    Points = new List<Point>(),
                };

                await using var conn = Open(channel, device);
                var point = U16("p", "db1", 0);

                var clock = System.Diagnostics.Stopwatch.StartNew();
                var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
                clock.Stop();

                t.Equal(1, samples.Count, "one sample");
                t.Equal(Quality.Timeout, samples[0].Quality, "a silent PLC is a timeout");
                t.Ok(clock.Elapsed < TimeSpan.FromSeconds(5),
                    $"the read returned in {clock.ElapsedMilliseconds}ms instead of hanging");
            }
            finally
            {
                await cts.CancelAsync();
                listener.Stop();
                try { await server; } catch { /* 同上 */ }
            }
        });
    }

    private static Task ReconnectAfterPeerCloses(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new S7SlaveModel();
            model.SeedDataBlock(1, 0, 0x00, 0x2A);

            var first = await StartSlaveAsync(model);
            var port = first.Port;

            var (channel, device) = Setup(port);
            var point = U16("p", "db1", 0);

            await using var conn = Open(channel, device);
            var before = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
            t.Equal(42d, before[0].Value!.AsNumber(), "first session reads 42");

            // 从站整个消失：会话失效后下一次读必须自己重建（COTP + 协商都得重来）。
            await first.DisposeAsync();

            var second = new S7TcpSlaveServer(IPAddress.Loopback, port, model);
            second.Start();
            await using (second)
            {
                var clock = System.Diagnostics.Stopwatch.StartNew();
                IReadOnlyList<RawSample> after = Array.Empty<RawSample>();

                // 端口刚放出来时可能被占用一小会儿，重试几次属于正常现象。
                for (var attempt = 0; attempt < 20; attempt++)
                {
                    after = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
                    if (after[0].Quality == Quality.Good) break;
                    await Task.Delay(50, t.Ct);
                }

                clock.Stop();

                t.Equal(Quality.Good, after[0].Quality,
                    $"the driver rebuilt the S7 session ({clock.ElapsedMilliseconds}ms, last error: {after[0].Error})");
                t.Equal(42d, after[0].Value!.AsNumber(), "same value over the new session");
            }
        });
    }

    private static Task RunAsync(Func<Task> body) => body();

    // ------------------------------------------------------------------
    // 驱动元数据
    // ------------------------------------------------------------------

    /// <summary>
    /// S7 的能力声明此前只活在实现里：protocol-status.md 有记载、<c>GET /api/v1/drivers</c>
    /// 会把它发给控制台前端生成表单，但没有任何用例守着它别漂移。这里把 Id / 角色 /
    /// 传输 / 寻址种类 / 能力四态逐一钉住——和 <c>bacnet/*</c> 的 MetadataDeclaresObjectAddressing 同范式。
    /// 重点钉住「位访问 Supported、订阅 Unsupported」：前者是 S7 区别于 Modbus 的点，
    /// 后者是「订阅只走 OPC UA / MQTT」边界的落实处，悄悄改成 Supported 会被当成已支持的功能。
    /// </summary>
    private static Task MetadataDeclaresCapabilities(TestContext t)
    {
        var meta = new S7MasterDriver().Meta;

        t.Equal("s7", meta.Id, "驱动 id");
        t.Equal("S7comm 主站", meta.Name, "驱动名");
        t.Ok(meta.Roles.Count == 1 && meta.Roles[0] == DriverRole.Master, "S7 只做主站");
        t.Ok(meta.Transports.Count == 1 && meta.Transports[0] == "tcp", "S7 走 TCP");
        t.Ok(meta.Accepts(AddressKind.Register), "S7 用 register 寻址（区域 + 字节偏移）");
        t.Ok(!meta.Accepts(AddressKind.Symbol) && !meta.Accepts(AddressKind.Object),
            "S7 不接受符号/对象寻址——符号寻址是 0xB2，明确没做");
        t.Ok(!meta.Accepts(AddressKind.File), "S7 不走文件记录寻址");

        t.Equal((int)CapState.Supported, (int)meta.Capabilities.Read, "读支持");
        t.Equal((int)CapState.Supported, (int)meta.Capabilities.Write, "写支持");
        t.Equal((int)CapState.Supported, (int)meta.Capabilities.MultiRead, "多节点批量读支持");
        t.Equal((int)CapState.Supported, (int)meta.Capabilities.BitAccess,
            "位访问是 S7 的招牌能力，必须 Supported");
        t.Equal((int)CapState.Unsupported, (int)meta.Capabilities.Subscribe, "S7 没有订阅，轮询采集");
        t.Equal((int)CapState.Unsupported, (int)meta.Capabilities.Browse, "不支持节点浏览");
        t.Equal((int)CapState.Unsupported, (int)meta.Capabilities.StructTypes, "不支持结构化类型");

        // 控制台表单按这些字段生成输入项；缺一个，用户就填不了对应的配置。
        var channelFields = meta.ConfigFields.Where(f => f.Scope == "channel").Select(f => f.Name).ToArray();
        t.Ok(channelFields.Contains("transport.host") && channelFields.Contains("transport.port"),
            "通道级字段 host/port 必须声明");
        var deviceFields = meta.ConfigFields.Where(f => f.Scope == "device").Select(f => f.Name).ToArray();
        foreach (var required in new[] { "labels.rack", "labels.slot", "labels.pduSize" })
            t.Ok(deviceFields.Contains(required), $"设备级字段 {required} 必须声明");

        var pointFields = meta.PointFields.Select(f => f.Name).ToArray();
        foreach (var required in new[] { "address.area", "address.offset" })
            t.Ok(pointFields.Contains(required), $"点位字段 {required} 必须声明");

        return Task.CompletedTask;
    }
}
