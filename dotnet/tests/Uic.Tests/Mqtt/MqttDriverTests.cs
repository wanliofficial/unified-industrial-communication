using System.Diagnostics;
using System.Text;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Codec.Mqtt;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Mqtt;
using Uic.Obs;

namespace Uic.Tests;

/// <summary>
/// MQTT 南向订阅驱动的验收：主题匹配/取值是纯函数，订阅与重连用测试内的
/// 最小 Broker 走真回环。
///
/// 这一层刻意分开：取值规则（JSON 字段、类型转换、质量归属）不依赖网络，
/// 回环只证明「订阅登记、缓存、重连后还能收到」这条链路。
/// </summary>
internal static class MqttDriverTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("mqtt/topic_matcher_follows_wildcard_rules", TopicMatcherFollowsWildcardRules));
        tests.Add(new TestCase("mqtt/payload_decodes_fields_and_types", PayloadDecodesFieldsAndTypes));
        tests.Add(new TestCase("mqtt/options_from_channel_and_labels", OptionsFromChannelAndLabels));
        tests.Add(new TestCase("mqtt/options_reject_credentials", OptionsRejectCredentials));
        tests.Add(new TestCase("mqtt/driver_is_stale_before_first_message", DriverIsStaleBeforeFirstMessage));
        tests.Add(new TestCase("mqtt/driver_reads_qos1_message_into_cache", DriverReadsQos1MessageIntoCache));
        tests.Add(new TestCase("mqtt/driver_reads_qos0_message_into_cache", DriverReadsQos0MessageIntoCache));
        tests.Add(new TestCase("mqtt/driver_uses_point_field_override", DriverUsesPointFieldOverride));
        tests.Add(new TestCase("mqtt/driver_reports_bad_payload_and_recovers", DriverReportsBadPayloadAndRecovers));
        tests.Add(new TestCase("mqtt/driver_resubscribes_after_broker_drop", DriverResubscribesAfterBrokerDrop));
        tests.Add(new TestCase("mqtt/driver_marks_value_stale_after_window", DriverMarksValueStaleAfterWindow));
        tests.Add(new TestCase("mqtt/driver_stale_ignores_a_clock_jump", DriverStaleIgnoresAClockJump));
        tests.Add(new TestCase("mqtt/driver_probe_fails_when_subscription_rejected", DriverProbeFailsWhenSubscriptionRejected));
    }

    // ------------------------------------------------------------------
    // 纯函数
    // ------------------------------------------------------------------

    private static Task TopicMatcherFollowsWildcardRules(TestContext t)
    {
        t.Ok(MqttTopic.Matches("plant/line1/temp", "plant/line1/temp"), "an exact topic matches");
        t.Ok(MqttTopic.Matches("plant/+/temp", "plant/line1/temp"), "+ matches exactly one level");
        t.Ok(!MqttTopic.Matches("plant/+/temp", "plant/a/b/temp"), "+ does not match two levels");
        t.Ok(MqttTopic.Matches("plant/#", "plant"), "# matches zero trailing levels");
        t.Ok(MqttTopic.Matches("plant/#", "plant/a/b"), "# matches any trailing depth");
        t.Ok(!MqttTopic.Matches("plant/+", "plant"), "a level wildcard still needs its level");
        t.Ok(!MqttTopic.Matches("plant/+", "plant/a/b"), "a filter never matches a deeper topic");
        t.Ok(!MqttTopic.Matches("plant/line1/temp", "plant/line1/hum"), "different text does not match");
        t.Ok(!MqttTopic.Matches("plant/Temp", "plant/temp"), "topics are case sensitive");

        t.Ok(MqttTopic.TryValidate("plant/line1/temp", out _), "a plain topic is valid");
        t.Ok(MqttTopic.TryValidate("plant/+/temp", out _), "a single-level wildcard is valid");
        t.Ok(MqttTopic.TryValidate("plant/#", out _), "a trailing multi-level wildcard is valid");

        t.Ok(!MqttTopic.TryValidate("", out var empty) && empty is not null, "an empty filter is rejected");
        t.Ok(!MqttTopic.TryValidate("plant//temp", out var hole) && hole is not null,
            "an empty level is rejected");
        t.Ok(!MqttTopic.TryValidate("plant/#/temp", out var hash) && hash is not null,
            "# must be the last level");
        t.Ok(!MqttTopic.TryValidate("plant/te+mp", out var plus) && plus is not null,
            "+ must be a whole level");

        return Task.CompletedTask;
    }

    private static Task PayloadDecodesFieldsAndTypes(TestContext t)
    {
        var number = Decode("""{"value":21.5}""", DataType.Float64);
        t.Equal(Quality.Good, number.Quality, "a matching field is good");
        t.Close(21.5, number.Value!.AsNumber(), 1e-9, "the number survives");

        var nested = Decode("""{"data":{"temp":3}}""", DataType.Int16, field: "data.temp");
        t.Equal(Quality.Good, nested.Quality, "a dotted field path descends into objects");
        t.Close(3, nested.Value!.AsNumber(), 1e-9, "the nested number survives");

        var pointer = Decode("""{"data":{"temp":4}}""", DataType.Int16, field: "/data/temp");
        t.Equal(Quality.Good, pointer.Quality, "a JSON pointer selects the same node");

        var whole = Decode("42.25", DataType.Float64, field: string.Empty);
        t.Equal(Quality.Good, whole.Quality, "an empty field path means the whole payload");
        t.Close(42.25, whole.Value!.AsNumber(), 1e-9, "a bare number payload decodes");

        // 级联网关的载荷就是北向 Sample JSON：值在 value 里，且是 {"type","num"} 信封。
        var envelope = Decode(
            """{"value":{"type":"float64","num":7.5},"quality":"good"}""", DataType.Float64);
        t.Equal(Quality.Good, envelope.Quality, "the northbound value envelope is unwrapped");
        t.Close(7.5, envelope.Value!.AsNumber(), 1e-9, "the enveloped number survives");

        var boolean = Decode("""{"value":1}""", DataType.Bool);
        t.Equal(Quality.Good, boolean.Quality, "0/1 is accepted as a boolean");
        t.Ok(boolean.Value!.AsBool(), "1 becomes true");

        var text = Decode("""{"value":"hello"}""", DataType.String);
        t.Equal(Quality.Good, text.Quality, "a string is accepted");
        t.Equal("hello", text.Value!.Text, "the text survives");

        var array = Decode("""{"value":[1,2,3]}""", DataType.Float64, count: 3);
        t.Equal(Quality.Good, array.Quality, "an array payload decodes");
        t.Equal(3, array.Value!.Array?.Count ?? 0, "all elements are present");

        var missing = Decode("""{"other":1}""", DataType.Float64);
        t.Equal(Quality.AddressError, missing.Quality, "a missing field is an address error");

        var notJson = MqttValues.Decode(Encoding.UTF8.GetBytes("not json"), "value", DataType.Float64, 0);
        t.Equal(Quality.Bad, notJson.Quality, "a non-JSON payload is a bad value");

        var wrongType = Decode("""{"value":"abc"}""", DataType.Float64);
        t.Equal(Quality.AddressError, wrongType.Quality,
            "a payload that cannot be a number is a contract error, not a link error");

        var wrongCount = Decode("""{"value":[1,2]}""", DataType.Float64, count: 3);
        t.Equal(Quality.AddressError, wrongCount.Quality, "array length must match the point");

        return Task.CompletedTask;
    }

    private static MqttValues.DecodeResult Decode(
        string json, DataType type, string field = "value", int count = 0)
        => MqttValues.Decode(Encoding.UTF8.GetBytes(json), field, type, count);

    private static Task OptionsFromChannelAndLabels(TestContext t)
    {
        var (channel, device) = Model("10.0.0.5", 0, new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["payloadField"] = ".",
            ["qos"] = "0",
            ["staleAfterSeconds"] = "7",
            ["clientId"] = "edge-1",
            ["keepAliveSeconds"] = "15",
            ["reconnectDelayMs"] = "750",
            ["tlsEnabled"] = "true",
            ["tlsCaFile"] = "broker.pem",
            ["tlsInsecureSkipVerify"] = "true",
        });

        var options = MqttConnOptions.FromContext(new OpenContext(
            channel, device, NullLogger.Instance, new MetricsRegistry(), t.TempDir));

        t.Equal("10.0.0.5", options.Client.Host, "the channel host is the broker");
        t.Equal(1883, options.Client.Port, "port 0 falls back to 1883");
        t.Equal(string.Empty, options.PayloadField, "'.' means the whole payload");
        t.Equal(0, options.Client.Qos, "the subscribe QoS comes from labels");
        t.Equal(7, options.StaleAfterSeconds, "the freshness window comes from labels");
        t.Equal("edge-1", options.Client.ClientId, "the client id comes from labels");
        t.Equal(15, options.Client.KeepAliveSeconds, "keepalive comes from labels");
        t.Equal(750, options.Client.ReconnectDelayMs, "reconnect delay comes from labels");
        t.Ok(options.Client.Tls.Enabled, "TLS is enabled");
        t.Equal("broker.pem", options.Client.Tls.CaFile, "the CA file is passed through");
        t.Ok(options.Client.Tls.InsecureSkipVerify, "skip-verify is passed through");

        var (plainChannel, plainDevice) = Model("broker.local", 1884);
        var plain = MqttConnOptions.FromContext(new OpenContext(
            plainChannel, plainDevice, NullLogger.Instance, new MetricsRegistry(), t.TempDir));
        t.Equal(1884, plain.Client.Port, "an explicit port wins");
        t.Equal("value", plain.PayloadField, "the default field is value");
        t.Equal(1, plain.Client.Qos, "the default QoS is 1");
        t.Equal("uic-meter-1", plain.Client.ClientId, "the default client id includes the device id");
        t.Ok(!plain.Client.Tls.Enabled, "TLS is off by default");

        return Task.CompletedTask;
    }

    private static Task OptionsRejectCredentials(TestContext t)
    {
        var (channel, device) = Model("broker.local", 1883,
            new Dictionary<string, string>(StringComparer.Ordinal) { ["username"] = "ops" });

        var ex = t.Throws<DriverException>(
            () => MqttConnOptions.FromContext(new OpenContext(
                channel, device, NullLogger.Instance, new MetricsRegistry(), t.TempDir)),
            "southbound credentials are rejected");

        t.Equal(ErrorKind.Unsupported, ex.Kind, "credentials are an unsupported feature");

        var (caChannel, caDevice) = Model("broker.local", 1883,
            new Dictionary<string, string>(StringComparer.Ordinal) { ["tlsCaFile"] = "ca.pem" });

        var ca = t.Throws<DriverException>(
            () => MqttConnOptions.FromContext(new OpenContext(
                caChannel, caDevice, NullLogger.Instance, new MetricsRegistry(), t.TempDir)),
            "a CA without TLS is rejected instead of ignored");

        t.Equal(ErrorKind.Unsupported, ca.Kind, "an unused CA file is a configuration error");

        return Task.CompletedTask;
    }

    // ------------------------------------------------------------------
    // 回环
    // ------------------------------------------------------------------

    private static async Task DriverIsStaleBeforeFirstMessage(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var (conn, device, point) = Connect(t, broker.Port);

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            var sample = await ReadOneAsync(conn, device, point);
            t.Equal(Quality.Stale, sample.Quality, "no message yet means stale");
            t.Ok(sample.Value is null, "a stale point carries no invented value");
            t.Ok(sample.Error is not null, "the reason is reported");
        }
    }

    private static async Task DriverReadsQos1MessageIntoCache(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var (conn, device, point) = Connect(t, broker.Port);

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":21.5}""", qos: 1);

            var sample = await WaitForGoodAsync(t, conn, device, point);
            t.Close(21.5, sample.Value!.AsNumber(), 1e-9, "the published value reaches the point");
            t.Ok(sample.DeviceTime is not null, "the receive time is exposed as the device time");
        }
    }

    private static async Task DriverReadsQos0MessageIntoCache(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var (conn, device, point) = Connect(
            t, broker.Port, new Dictionary<string, string>(StringComparer.Ordinal) { ["qos"] = "0" });

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":8}""", qos: 0);

            var sample = await WaitForGoodAsync(t, conn, device, point);
            t.Close(8, sample.Value!.AsNumber(), 1e-9, "a QoS0 message is collected too");
        }
    }

    private static async Task DriverUsesPointFieldOverride(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var (conn, device, point) = Connect(
            t, broker.Port,
            labels: new Dictionary<string, string>(StringComparer.Ordinal) { ["payloadField"] = "value" },
            field: "data.temp");

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":1,"data":{"temp":9}}""", qos: 1);

            var sample = await WaitForGoodAsync(t, conn, device, point);
            t.Close(9, sample.Value!.AsNumber(), 1e-9, "address.property overrides the device field");
        }
    }

    private static async Task DriverReportsBadPayloadAndRecovers(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var (conn, device, point) = Connect(t, broker.Port);

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            await broker.PublishToClientAsync("plant/line1/temp", "not json", qos: 1);
            var bad = await WaitForQualityAsync(t, conn, device, point, Quality.Bad);
            t.Ok(bad.Value is null, "a broken payload does not keep a made-up value");

            // 字段缺失是点表与发布方的契约问题，与链路无关。
            await broker.PublishToClientAsync("plant/line1/temp", """{"other":2}""", qos: 1);
            var address = await WaitForQualityAsync(t, conn, device, point, Quality.AddressError);
            t.Ok(address.Error is not null, "the missing field is named in the error");

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":5}""", qos: 1);
            var good = await WaitForGoodAsync(t, conn, device, point);
            t.Close(5, good.Value!.AsNumber(), 1e-9, "a good payload recovers the point");
        }
    }

    private static async Task DriverResubscribesAfterBrokerDrop(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var (conn, device, point) = Connect(
            t, broker.Port,
            new Dictionary<string, string>(StringComparer.Ordinal) { ["reconnectDelayMs"] = "500" });

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":1}""", qos: 1);
            await WaitForGoodAsync(t, conn, device, point);

            broker.DropConnections();

            // 重连之后必须重新发 SUBSCRIBE：clean session 下 Broker 不会记得上一次的订阅。
            await WaitUntil(t, () => broker.Connects.Count >= 2 && broker.Subscriptions.Count >= 2,
                "the driver reconnects and re-subscribes");

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":2}""", qos: 1);
            var sample = await WaitForValueAsync(t, conn, device, point, 2);
            t.Close(2, sample.Value!.AsNumber(), 1e-9, "messages flow again after the reconnect");
        }
    }

    private static async Task DriverMarksValueStaleAfterWindow(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var (conn, device, point) = Connect(
            t, broker.Port,
            new Dictionary<string, string>(StringComparer.Ordinal) { ["staleAfterSeconds"] = "1" });

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":3}""", qos: 1);
            await WaitForGoodAsync(t, conn, device, point);

            // 值本身不会消失，只是不再新鲜：过期后质量转 stale，值仍保留以便界面显示最后读数。
            var stale = await WaitForQualityAsync(t, conn, device, point, Quality.Stale, timeoutMs: 10_000);
            t.Close(3, stale.Value!.AsNumber(), 1e-9, "the last value is kept when it turns stale");
            t.Ok(stale.Error is not null, "the reason says the message is too old");
        }
    }

    /// <summary>
    /// 南向过期判定必须用单调时钟，绝不能用墙钟。
    ///
    /// 它回答的是「多久没收到发布方的报文」。用墙钟算：前跳一小时会让所有点位瞬间误报
    /// stale（数据其实刚到）；后跳则让过期点位无限期报 Good——把旧数据当新鲜值，正是
    /// 这套代码反复警惕的那种假象。判定只看真实流逝了多久。
    /// </summary>
    private static async Task DriverStaleIgnoresAClockJump(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var clock = new FakeClock();
        var (conn, device, point) = Connect(
            t, broker.Port,
            new Dictionary<string, string>(StringComparer.Ordinal) { ["staleAfterSeconds"] = "1" },
            time: clock);

        await using (conn)
        {
            await conn.ProbeAsync(t.Ct);

            await broker.PublishToClientAsync("plant/line1/temp", """{"value":3}""", qos: 1);
            await WaitForGoodAsync(t, conn, device, point);

            // 只拨墙钟一小时、单调时钟不动：真实上没过多久，值必须仍然新鲜。
            clock.Advance(TimeSpan.FromHours(1), TimeSpan.Zero);
            var afterJump = await ReadOneAsync(conn, device, point);
            t.Equal(Quality.Good, afterJump.Quality,
                "a wall-clock jump must not make a fresh value look stale");

            // 真实（单调）时间越过过期窗口，这时才应该转 stale。
            clock.Advance(TimeSpan.Zero, TimeSpan.FromSeconds(2));
            var stale = await ReadOneAsync(conn, device, point);
            t.Equal(Quality.Stale, stale.Quality,
                "the value turns stale only after the real window elapses");
        }
    }

    private static async Task DriverProbeFailsWhenSubscriptionRejected(TestContext t)
    {
        await using var broker = new FakeMqttBroker { RefuseSubscription = true };
        var (conn, _, _) = Connect(t, broker.Port);

        await using (conn)
        {
            var ex = await t.ThrowsAsync<DriverException>(() => conn.ProbeAsync(t.Ct),
                "a rejected subscription fails the probe");

            t.Equal(ErrorKind.Permission, ex.Kind,
                "an ACL rejection is a permission problem, not a link failure");
        }
    }

    // ------------------------------------------------------------------
    // 辅助
    // ------------------------------------------------------------------

    private static (Channel Channel, Device Device) Model(
        string host,
        int port,
        Dictionary<string, string>? labels = null,
        string field = "value")
    {
        var channel = new Channel
        {
            Id = "broker",
            Protocol = MqttClientDriver.Protocol,
            Transport = "tcp",
            Host = host,
            Port = port,
        };

        var device = new Device
        {
            Id = "meter-1",
            Name = "MQTT 电表",
            ChannelId = channel.Id,
            Labels = labels ?? new Dictionary<string, string>(StringComparer.Ordinal),
            Points =
            {
                new Point
                {
                    Id = "temp",
                    Address = Address.Symbol("plant/line1/temp") with { Property = field == "value" ? null : field },
                    DataType = DataType.Float64,
                },
            },
        };

        return (channel, device);
    }

    private static (IConn Conn, Device Device, Point Point) Connect(
        TestContext t,
        int port,
        Dictionary<string, string>? labels = null,
        string field = "value",
        TimeProvider? time = null)
    {
        var (channel, device) = Model("127.0.0.1", port, labels, field);
        var conn = new MqttClientDriver().Open(new OpenContext(
            channel, device, NullLogger.Instance, new MetricsRegistry(), t.TempDir, time));

        return (conn, device, device.Points[0]);
    }

    private static async Task<RawSample> ReadOneAsync(IConn conn, Device device, Point point)
    {
        var spans = new[] { new ReadSpan("span-0", new[] { PointRef.Of(device, point) }) };
        var samples = await conn.ReadAsync(spans, CancellationToken.None);
        return samples[0];
    }

    private static async Task<RawSample> WaitForGoodAsync(
        TestContext t, IConn conn, Device device, Point point)
        => await WaitForQualityAsync(t, conn, device, point, Quality.Good);

    private static async Task<RawSample> WaitForQualityAsync(
        TestContext t, IConn conn, Device device, Point point, Quality quality, int timeoutMs = 8000)
    {
        var clock = Stopwatch.StartNew();
        RawSample sample;

        do
        {
            sample = await ReadOneAsync(conn, device, point);
            if (sample.Quality == quality) return sample;
            await Task.Delay(20, t.Ct);
        } while (clock.ElapsedMilliseconds <= timeoutMs);

        t.Fail($"{point.Id} never reached {quality}; last was {sample.Quality} ({sample.Error})");
        return sample;
    }

    private static async Task<RawSample> WaitForValueAsync(
        TestContext t, IConn conn, Device device, Point point, double expected, int timeoutMs = 8000)
    {
        var clock = Stopwatch.StartNew();
        RawSample sample;

        do
        {
            sample = await ReadOneAsync(conn, device, point);
            if (sample.Quality == Quality.Good &&
                sample.Value is not null &&
                Math.Abs(sample.Value.AsNumber() - expected) < 1e-9)
            {
                return sample;
            }

            await Task.Delay(20, t.Ct);
        } while (clock.ElapsedMilliseconds <= timeoutMs);

        t.Fail($"{point.Id} never reached {expected}; last was {sample.Value?.AsNumber()} ({sample.Quality})");
        return sample;
    }

    private static async Task WaitUntil(TestContext t, Func<bool> condition, string what, int timeoutMs = 8000)
    {
        var clock = Stopwatch.StartNew();
        while (!condition())
        {
            if (clock.ElapsedMilliseconds > timeoutMs)
                t.Fail($"{what} (waited {clock.ElapsedMilliseconds} ms)");

            await Task.Delay(20, t.Ct);
        }
    }
}
