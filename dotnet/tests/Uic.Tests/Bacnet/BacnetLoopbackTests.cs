using Microsoft.Extensions.Logging.Abstractions;
using Uic.Codec.Bacnet;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Bacnet;
using Uic.Engine;
using Uic.Obs;
using Uic.Sim.Bacnet;

namespace Uic.Tests;

/// <summary>
/// BACnet/IP 主站 ↔ 内置从站回环。
///
/// 用真 UDP 端口而不是假字节流：BVLC 长度、NPDU 版本、invoke-id 配对、数据报边界
/// 这几处只有在真套接字上才会暴露问题。特别是 invoke-id：假流里「按顺序一问一答」
/// 永远是对的，而 UDP 上迟到的响应是常态，配错了就静默读到别的点位的值。
/// </summary>
internal static class BacnetLoopbackTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("bacnet/probe_gets_i_am", ProbeGetsIAm));
        tests.Add(new TestCase("bacnet/read_present_value_as_real", ReadPresentValueAsReal));
        tests.Add(new TestCase("bacnet/read_multiple_objects", ReadMultipleObjects));
        tests.Add(new TestCase("bacnet/read_bool_and_string", ReadBoolAndString));
        tests.Add(new TestCase("bacnet/write_then_read_back", WriteThenReadBack));
        tests.Add(new TestCase("bacnet/unknown_object_is_address_error", UnknownObjectIsAddressError));
        tests.Add(new TestCase("bacnet/unknown_property_is_address_error", UnknownPropertyIsAddressError));
        tests.Add(new TestCase("bacnet/silent_device_times_out", SilentDeviceTimesOut));
        tests.Add(new TestCase("bacnet/stray_response_is_not_taken_as_value", StrayResponseIsNotTaken));
        tests.Add(new TestCase("bacnet/probe_with_wrong_device_instance_times_out", ProbeWithWrongInstance));
        tests.Add(new TestCase("bacnet/metadata_declares_object_addressing", MetadataDeclaresObjectAddressing));
    }

    private static (Channel Channel, Device Device) Setup(int port, Dictionary<string, string>? labels = null) => (
        new Channel
        {
            Id = "bacnet-ch",
            Protocol = "bacnet",
            Transport = "udp",
            Host = "127.0.0.1",
            Port = port,
            Timeout = TimeSpan.FromMilliseconds(700),
            ConnectTimeout = TimeSpan.FromSeconds(2),
            MaxConcurrency = 1,
        },
        new Device
        {
            Id = "ahu-1",
            ChannelId = "bacnet-ch",
            Interval = TimeSpan.FromMilliseconds(200),
            Timeout = TimeSpan.FromMilliseconds(600),
            Labels = labels ?? new Dictionary<string, string>(StringComparer.Ordinal),
            Points = new List<Point>(),
        });

    private static IConn Open(Channel channel, Device device) => new BacnetMasterDriver().Open(
        new OpenContext(channel, device, NullLogger.Instance, new MetricsRegistry()));

    private static PointRef Point(string id, int objectType, int instance, int property, DataType type) =>
        new(id, Address.Object(objectType.ToString(), instance, property.ToString()), type, ByteOrder.ABCD, 1);

    private static Task ProbeGetsIAm(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new BacnetSlaveModel { DeviceInstance = 2002 };
            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port, new Dictionary<string, string>(StringComparer.Ordinal)
            {
                ["deviceInstance"] = "2002",
            });

            await using var conn = Open(channel, device);
            await conn.ProbeAsync(t.Ct);

            t.Ok(model.RequestCount >= 1, $"the probe sent a Who-Is ({model.RequestCount} request(s))");
            t.Contains(server.LastTrace ?? string.Empty, "i-am", "the slave answered with an I-Am");
        });
    }

    private static Task ReadPresentValueAsReal(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new BacnetSlaveModel();
            model.Set(BacnetConstants.ObjectTypeAnalogInput, 1, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 21.5), DataType.Float32);

            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port);
            var point = Point("supply-temp", BacnetConstants.ObjectTypeAnalogInput, 1,
                BacnetConstants.PropertyPresentValue, DataType.Float32);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "one sample per point");
            t.Equal(Quality.Good, samples[0].Quality, $"quality ({samples[0].Error})");
            t.Close(21.5, samples[0].Value!.AsNumber(), 1e-6, "present-value");
        });
    }

    private static Task ReadMultipleObjects(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new BacnetSlaveModel();
            model.Set(BacnetConstants.ObjectTypeAnalogInput, 0, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 1.25), DataType.Float32);
            model.Set(BacnetConstants.ObjectTypeAnalogInput, 1, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 2.5), DataType.Float32);
            model.Set(BacnetConstants.ObjectTypeAnalogValue, 5, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 3.75), DataType.Float32);

            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port);
            var points = new[]
            {
                Point("ai0", BacnetConstants.ObjectTypeAnalogInput, 0, BacnetConstants.PropertyPresentValue, DataType.Float32),
                Point("ai1", BacnetConstants.ObjectTypeAnalogInput, 1, BacnetConstants.PropertyPresentValue, DataType.Float32),
                Point("av5", BacnetConstants.ObjectTypeAnalogValue, 5, BacnetConstants.PropertyPresentValue, DataType.Float32),
            };

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(points), t.Ct);

            var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);
            t.Equal(3, samples.Count, "one sample per point");
            t.Close(1.25, byId["ai0"].Value!.AsNumber(), 1e-6, "analog-input:0");
            t.Close(2.5, byId["ai1"].Value!.AsNumber(), 1e-6, "analog-input:1");
            t.Close(3.75, byId["av5"].Value!.AsNumber(), 1e-6, "analog-value:5");

            foreach (var s in samples) t.Equal(Quality.Good, s.Quality, $"{s.PointId} quality");
        });
    }

    private static Task ReadBoolAndString(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new BacnetSlaveModel();
            model.Set(BacnetConstants.ObjectTypeBinaryInput, 3, BacnetConstants.PropertyPresentValue,
                DataValue.Boolean(true), DataType.Bool);
            model.Set(BacnetConstants.ObjectTypeAnalogValue, 9, BacnetConstants.PropertyObjectName,
                DataValue.String("AHU-1"), DataType.String);

            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port);
            var points = new[]
            {
                Point("fan-run", BacnetConstants.ObjectTypeBinaryInput, 3,
                    BacnetConstants.PropertyPresentValue, DataType.Bool),
                Point("name", BacnetConstants.ObjectTypeAnalogValue, 9,
                    BacnetConstants.PropertyObjectName, DataType.String),
            };

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(points), t.Ct);

            var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);
            t.Equal(true, byId["fan-run"].Value!.Bool, "binary-input present-value");
            t.Equal("AHU-1", byId["name"].Value!.Text, "object-name as a character string");
        });
    }

    private static Task WriteThenReadBack(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new BacnetSlaveModel();
            // 先放一个不同的值：写进去的不一样，才能证明「写生效了」而不是「读到的是旧值」。
            model.Set(BacnetConstants.ObjectTypeAnalogOutput, 2, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 10), DataType.Float32);

            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port);
            var point = Point("damper", BacnetConstants.ObjectTypeAnalogOutput, 2,
                BacnetConstants.PropertyPresentValue, DataType.Float32);

            await using var conn = Open(channel, device);

            var writes = await conn.WriteAsync(new[]
            {
                new WriteCommand(point, DataValue.Number(DataType.Float32, 62.5)),
            }, t.Ct);

            t.Equal(1, writes.Count, "one result");
            t.Ok(writes[0].Ok, $"write accepted ({writes[0].Error})");
            t.Equal(Quality.Good, writes[0].Quality, "write quality");

            // 从站内存里必须真的是新值：对 62.5 的 IEEE754 编码逐字节核对，
            // 这样「驱动写对了」与「驱动以为写对了」才分得开。
            t.Ok(model.TryGet(BacnetConstants.ObjectTypeAnalogOutput, 2,
                BacnetConstants.PropertyPresentValue, out var stored), "the slave stored the value");
            t.Equal("44427A0000", Convert.ToHexString(stored), "real 62.5 with its application tag");

            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
            t.Close(62.5, samples[0].Value!.AsNumber(), 1e-6, "read back");
        });
    }

    private static Task UnknownObjectIsAddressError(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new BacnetSlaveModel();
            // 只放 analog-input:1，点位却指向 99：从站回 unknown-object。
            model.Set(BacnetConstants.ObjectTypeAnalogInput, 1, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 1), DataType.Float32);

            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port);
            var point = Point("typo", BacnetConstants.ObjectTypeAnalogInput, 99,
                BacnetConstants.PropertyPresentValue, DataType.Float32);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "the failing point still yields a sample");
            // 点表写错与链路故障必须分开：前者重试无意义，后者才该熔断。
            t.Equal(Quality.AddressError, samples[0].Quality,
                "an unknown object is a configuration problem, not a comm problem");
            t.Ok(samples[0].Value is null, "a bad sample carries no value");
            t.Contains(samples[0].Error ?? string.Empty, "unknown-object", "the device's reason is preserved");
        });
    }

    private static Task UnknownPropertyIsAddressError(TestContext t)
    {
        return RunAsync(async () =>
        {
            var model = new BacnetSlaveModel();
            model.Set(BacnetConstants.ObjectTypeAnalogInput, 1, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 1), DataType.Float32);

            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port);

            // 对象存在，但属性 36（event-state）没配。
            var point = Point("event", BacnetConstants.ObjectTypeAnalogInput, 1,
                BacnetConstants.PropertyEventState, DataType.UInt16);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(Quality.AddressError, samples[0].Quality, "an unknown property is an address problem");
            t.Contains(samples[0].Error ?? string.Empty, "unknown-property", "the reason is preserved");
        });
    }

    private static Task SilentDeviceTimesOut(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 端口开着但没人回：模拟「IP 通、设备不在」这种最常见的误判场景。
            var socket = new System.Net.Sockets.UdpClient(new System.Net.IPEndPoint(
                System.Net.IPAddress.Loopback, 0));
            var port = ((System.Net.IPEndPoint)socket.Client.LocalEndPoint!).Port;

            try
            {
                var (channel, device) = Setup(port);
                await using var conn = Open(channel, device);

                var point = Point("dead", BacnetConstants.ObjectTypeAnalogInput, 1,
                    BacnetConstants.PropertyPresentValue, DataType.Float32);

                var clock = System.Diagnostics.Stopwatch.StartNew();
                var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);
                clock.Stop();

                t.Equal(1, samples.Count, "one sample");
                t.Equal(Quality.Timeout, samples[0].Quality, "a silent device is a timeout");
                t.Ok(clock.Elapsed < TimeSpan.FromSeconds(5),
                    $"the read returned in {clock.ElapsedMilliseconds}ms instead of hanging");
            }
            finally
            {
                socket.Dispose();
            }
        });
    }

    private static Task StrayResponseIsNotTaken(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 从站先故意回一条 invoke-id 不对的响应，再回正确的。
            // 驱动若只看「收到了一条 ComplexAck」，就会把前一条的值当成这个点位的值——
            // 而质量仍然是 good，现场极难发现。
            var model = new BacnetSlaveModel();
            model.Set(BacnetConstants.ObjectTypeAnalogInput, 1, BacnetConstants.PropertyPresentValue,
                DataValue.Number(DataType.Float32, 77.5), DataType.Float32);

            await using var server = new StrayingServer(model, strayInvokeId: 200);

            var (channel, device) = Setup(server.Port);
            var point = Point("value", BacnetConstants.ObjectTypeAnalogInput, 1,
                BacnetConstants.PropertyPresentValue, DataType.Float32);

            await using var conn = Open(channel, device);
            var samples = await conn.ReadAsync(ReadPlanner.Plan(new[] { point }), t.Ct);

            t.Equal(1, samples.Count, "one sample");
            t.Equal(Quality.Good, samples[0].Quality, $"the real response was used ({samples[0].Error})");
            t.Close(77.5, samples[0].Value!.AsNumber(), 1e-6,
                "the value came from the matching response, not the stray one");
            t.Equal(1, server.StraySent, "the stray response was actually sent");
        });
    }

    private static Task ProbeWithWrongInstance(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 同网段常有多台设备。配置里指定了实例号，就要核对——回错一台同样是「通」。
            var model = new BacnetSlaveModel { DeviceInstance = 1001 };
            await using var server = BacnetUdpSlaveServer.Start(model);

            var (channel, device) = Setup(server.Port, new Dictionary<string, string>(StringComparer.Ordinal)
            {
                ["deviceInstance"] = "9999",
            });

            await using var conn = Open(channel, device);

            var ex = await t.ThrowsAsync<DriverException>(
                () => conn.ProbeAsync(t.Ct), "the wrong device does not satisfy the probe");
            t.Equal((int)ErrorKind.Timeout, (int)ex.Kind, "it reports a timeout, not a protocol error");
            t.Contains(ex.Message, "9999", "the message names the expected instance");
        });
    }

    private static Task MetadataDeclaresObjectAddressing(TestContext t)
    {
        var meta = new BacnetMasterDriver().Meta;

        t.Equal("bacnet", meta.Id, "driver id");
        t.Ok(meta.Accepts(AddressKind.Object), "the driver accepts object addressing");
        t.Ok(!meta.Accepts(AddressKind.Register), "it does not accept register addressing");
        t.Ok(meta.Transports.Contains("udp"), "BACnet/IP is carried over UDP");
        t.Equal((int)CapState.Supported, (int)meta.Capabilities.Read, "read is supported");
        t.Equal((int)CapState.Supported, (int)meta.Capabilities.Write, "write is supported");
        t.Equal((int)CapState.Unsupported, (int)meta.Capabilities.MultiRead,
            "each point needs its own request, so multi-read is not claimed");

        // 三个点位字段必须与编解码层实际读取的字段一致，否则前端生成的表单填了不生效。
        var fields = meta.PointFields.Select(f => f.Name).ToArray();
        foreach (var required in new[] { "address.objectType", "address.instance", "address.property" })
            t.Ok(fields.Contains(required), $"the point field {required} is declared");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 会先发一条「invoke-id 不匹配」响应的从站。
    ///
    /// 这是真实 UDP 链路上会发生的事：上一条请求超时了，它的响应晚到。用它做故障注入，
    /// 验证驱动是按 invoke-id 配对，而不是「收到什么就用什么」。
    /// </summary>
    private sealed class StrayingServer : IAsyncDisposable
    {
        private readonly BacnetSlaveModel _model;
        private readonly byte _strayInvokeId;
        private readonly System.Net.Sockets.UdpClient _socket;
        private readonly CancellationTokenSource _cts = new();
        private readonly Task _loop;

        public StrayingServer(BacnetSlaveModel model, byte strayInvokeId)
        {
            _model = model;
            _strayInvokeId = strayInvokeId;
            _socket = new System.Net.Sockets.UdpClient(new System.Net.IPEndPoint(
                System.Net.IPAddress.Loopback, 0));
            Port = ((System.Net.IPEndPoint)_socket.Client.LocalEndPoint!).Port;
            _loop = Task.Run(() => ServeAsync(_cts.Token));
        }

        public int Port { get; }

        public int StraySent { get; private set; }

        private async Task ServeAsync(CancellationToken ct)
        {
            while (!ct.IsCancellationRequested)
            {
                System.Net.Sockets.UdpReceiveResult received;
                try
                {
                    received = await _socket.ReceiveAsync(ct).ConfigureAwait(false);
                }
                catch (OperationCanceledException) { return; }
                catch (ObjectDisposedException) { return; }
                catch (System.Net.Sockets.SocketException) { continue; }

                try
                {
                    var apdu = BacnetFrame.Payload(received.Buffer);
                    if (apdu.IsEmpty) continue;

                    if ((apdu[0] & 0xF0) == BacnetConstants.ApduConfirmedRequest
                        && apdu.Length >= 4
                        && apdu[3] == BacnetConstants.ServiceReadProperty)
                    {
                        var id = new BacnetObjectId(BacnetConstants.ObjectTypeAnalogInput, 1);

                        // 1) 一条值完全不同的「迟到响应」，invoke-id 故意不对。
                        var strayValue = BacnetTags.EncodeValue(
                            DataValue.Number(DataType.Float32, -1), DataType.Float32);
                        var stray = BacnetFrame.Build(
                            BacnetApdu.BuildReadPropertyAck(_strayInvokeId, id,
                                BacnetConstants.PropertyPresentValue, strayValue),
                            broadcast: false);

                        await _socket.SendAsync(stray, received.RemoteEndPoint, ct).ConfigureAwait(false);
                        StraySent++;

                        // 2) 再回正确的那条。
                        var reply = BacnetSlaveHandler.Handle(_model, received.Buffer, out _);
                        if (reply is not null)
                            await _socket.SendAsync(reply, received.RemoteEndPoint, ct).ConfigureAwait(false);
                        continue;
                    }

                    var other = BacnetSlaveHandler.Handle(_model, received.Buffer, out _);
                    if (other is not null)
                        await _socket.SendAsync(other, received.RemoteEndPoint, ct).ConfigureAwait(false);
                }
                catch (Exception)
                {
                    // 故障注入夹具：坏包丢弃。
                }
            }
        }

        public async ValueTask DisposeAsync()
        {
            await _cts.CancelAsync().ConfigureAwait(false);
            _socket.Dispose();
            try { await _loop.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
            _cts.Dispose();
        }
    }

    private static Task RunAsync(Func<Task> body) => body();
}
