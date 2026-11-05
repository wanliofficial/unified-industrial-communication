using System.Diagnostics;
using System.Text;
using System.Text.Json.Nodes;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Northbound;
using Uic.Outbox;

namespace Uic.Tests;

/// <summary>
/// MQTT 发布侧的协议级验收。
///
/// 这一层原先只做过代码评审：报文是手工拼的，没有真实 broker 对过。
/// 用例跑在测试内的最小 broker（<see cref="FakeMqttBroker"/>）上，它独立
/// 解析字节、不复用客户端任何代码——这样「客户端拼错了」和「测试照着错」
/// 是两件事，不会互相掩护。
///
/// 仍然不等于与 mosquitto 互操作：这里只覆盖 3.1.1 的发布侧最短路径。
/// </summary>
internal static class MqttTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("mqtt/connect_frame_carries_will_and_credentials", ConnectFrameCarriesWillAndCredentials));
        tests.Add(new TestCase("mqtt/publish_qos1_waits_for_puback", PublishQos1WaitsForPuback));
        tests.Add(new TestCase("mqtt/publish_qos0_has_no_packet_id", PublishQos0HasNoPacketId));
        tests.Add(new TestCase("mqtt/inbound_publish_is_not_mistaken_for_puback", InboundPublishIsNotPuback));
        tests.Add(new TestCase("mqtt/sink_renders_topic_and_injects_schema_version", SinkRendersTopic));
        tests.Add(new TestCase("mqtt/reconnect_republishes_birth_and_state", ReconnectRepublishesBirthAndState));
        tests.Add(new TestCase("mqtt/refused_connack_keeps_retrying_without_connecting", RefusedConnackKeepsRetrying));
        tests.Add(new TestCase("mqtt/idle_connection_sends_pingreq", IdleConnectionSendsPingreq));
    }

    private static MqttOptions Options(int port, int qos = 1, string? username = null, string? password = null, int keepAliveSeconds = 30) => new()
    {
        Enabled = true,
        Host = "127.0.0.1",
        Port = port,
        Qos = qos,
        Retain = false,
        KeepAliveSeconds = keepAliveSeconds,
        ReconnectDelayMs = 500,
        Username = username,
        Password = password,
        ClientId = "uic-test",
        TopicPrefix = "uic",
        Topic = "uic/{site}/{device}/{point}/value",
    };

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

    /// <summary>
    /// CONNECT 最容易出错也最难在事后发现的地方：协议名与级别、clean session、
    /// 遗嘱（主题/载荷/QoS/retain）、凭据。任何一项拼错，broker 只会回一个
    /// 「已拒绝」，现场看不出原因。
    /// </summary>
    private static async Task ConnectFrameCarriesWillAndCredentials(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        await using var client = new MqttClient(
            Options(broker.Port, username: "ops", password: "s3cret"), "plant/1", NullLogger.Instance);

        client.Start();
        await WaitUntil(t, () => client.IsConnected && broker.Connects.Count > 0, "the client connects");

        var connect = broker.Connects[0];

        t.Equal("MQTT", connect.Protocol, "protocol name is MQTT (not MQIsdp)");
        t.Equal((byte)4, connect.Level, "protocol level 4 means 3.1.1");

        t.Ok((connect.Flags & 0x02) != 0, "clean session is set: we have no server-side state to resume");
        t.Ok((connect.Flags & 0x04) != 0, "will flag is set, otherwise a crashed gateway looks online forever");
        t.Ok((connect.Flags & 0x20) != 0, "will is retained, so a late subscriber still sees it");
        t.Equal((byte)0x08, (byte)(connect.Flags & 0x18), "will QoS matches the configured QoS 1");

        t.Equal((ushort)30, connect.KeepAliveSeconds, "keep alive comes from the config");
        t.Equal("uic-test", connect.ClientId, "client id comes from the config");

        // 站点名里的斜杠必须被清洗：否则 /plant/1 会把遗嘱发到 uic/plant/1/$death。
        t.Equal("uic/plant_1/$death", connect.WillTopic, "will topic is the sanitised $death topic");
        t.Ok(connect.WillPayload is not null, "will payload is present");
        t.Contains(connect.WillPayload!, "\"online\":false", "will marks the gateway offline");
        t.Contains(connect.WillPayload!, "\"schemaVersion\":1", "will carries the schema version");

        t.Equal("ops", connect.Username, "username is forwarded");
        t.Equal("s3cret", connect.Password, "password is forwarded");
        t.Ok((connect.Flags & 0xC0) == 0xC0, "username and password flags are both set");
    }

    /// <summary>
    /// QoS1 的语义是「broker 收下了才算投递成功」。如果发布在发出后立刻返回，
    /// Outbox 会推进位点，broker 重启时那一段消息就永久丢了。
    /// </summary>
    private static async Task PublishQos1WaitsForPuback(TestContext t)
    {
        await using var broker = new FakeMqttBroker { PubackDelay = TimeSpan.FromMilliseconds(150) };
        await using var client = new MqttClient(Options(broker.Port, qos: 1), "site-a", NullLogger.Instance);

        client.Start();
        await WaitUntil(t, () => client.IsConnected, "the client connects");

        var payload = Encoding.UTF8.GetBytes("{\"value\":42}");
        var clock = Stopwatch.StartNew();
        await client.PublishAsync("uic/site-a/d1/p1/value", payload, retain: false, t.Ct);
        clock.Stop();

        t.Ok(clock.Elapsed >= TimeSpan.FromMilliseconds(100),
            $"publish waits for the PUBACK (took {clock.ElapsedMilliseconds} ms, broker delay 150 ms)");

        await WaitUntil(t, () => broker.Published.Count > 0, "the broker receives the PUBLISH");
        var published = broker.Published[0];

        t.Equal("uic/site-a/d1/p1/value", published.Topic, "topic is sent verbatim");
        t.Equal("{\"value\":42}", published.Payload, "payload is not re-encoded");
        t.Equal(1, published.Qos, "the PUBLISH header says QoS 1");
        t.Ok(published.PacketId != 0, "a QoS1 PUBLISH carries a non-zero packet id");
        t.Equal(false, published.Retain, "retain follows the config (off)");
    }

    private static async Task PublishQos0HasNoPacketId(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        await using var client = new MqttClient(Options(broker.Port, qos: 0), "site-a", NullLogger.Instance);

        client.Start();
        await WaitUntil(t, () => client.IsConnected, "the client connects");

        var clock = Stopwatch.StartNew();
        await client.PublishAsync("uic/site-a/d1/p1/value", Encoding.UTF8.GetBytes("{}"), retain: true, t.Ct);
        clock.Stop();

        t.Ok(clock.Elapsed < TimeSpan.FromMilliseconds(500),
            $"QoS0 does not wait for an acknowledgement (took {clock.ElapsedMilliseconds} ms)");

        await WaitUntil(t, () => broker.Published.Count > 0, "the broker receives the PUBLISH");
        var published = broker.Published[0];

        t.Equal(0, published.Qos, "the PUBLISH header says QoS 0");
        t.Equal((ushort)0, published.PacketId, "a QoS0 PUBLISH must not carry a packet id");
        t.Equal(true, published.Retain, "retain follows the config (on)");
    }

    /// <summary>
    /// 读循环会把入站报文分流。Broker 发来一条 QoS1 PUBLISH 时，绝不能把它
    /// 当成「发布成功」的确认——否则 PUBACK 丢了也看不出来，Outbox 会推进位点。
    /// </summary>
    private static async Task InboundPublishIsNotPuback(TestContext t)
    {
        await using var broker = new FakeMqttBroker
        {
            StrayPublishOnConnect = true,
            PubackDelay = TimeSpan.FromMilliseconds(150),
        };

        await using var client = new MqttClient(Options(broker.Port, qos: 1), "site-a", NullLogger.Instance);
        client.Start();
        await WaitUntil(t, () => client.IsConnected, "the client connects");

        var clock = Stopwatch.StartNew();
        await client.PublishAsync("uic/site-a/d1/p1/value", Encoding.UTF8.GetBytes("{}"), retain: false, t.Ct);
        clock.Stop();

        t.Ok(clock.Elapsed >= TimeSpan.FromMilliseconds(100),
            $"the publish still waits for the real PUBACK (took {clock.ElapsedMilliseconds} ms)");

        await WaitUntil(t, () => broker.Published.Count > 0, "the broker receives the PUBLISH");
        t.Equal("uic/site-a/d1/p1/value", broker.Published[0].Topic, "the stray PUBLISH is not confused with ours");
    }

    /// <summary>值主题与载荷都来自 Outbox 的记录：主题要渲染，载荷要补 schemaVersion。</summary>
    private static async Task SinkRendersTopic(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        await using var sink = new MqttSink(Options(broker.Port), "plant/1", NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => sink.Client.IsConnected, "the sink connects");

        var record = new OutboxRecord(
            7,
            "{\"device\":\"d1\",\"point\":\"p1\",\"value\":42.5,\"quality\":\"good\"}",
            DateTimeOffset.UtcNow);

        await sink.DeliverAsync(new[] { record }, t.Ct);

        await WaitUntil(t, () => Count(broker, "uic/plant_1/d1/p1/value") > 0, "the broker receives the sample");

        // 不能取 Published[0]：连接建立时 OnConnected 会先补发 $birth，谁先到不确定。
        var published = broker.Published.Last(p => p.Topic == "uic/plant_1/d1/p1/value");

        t.Equal("uic/plant_1/d1/p1/value", published.Topic,
            "the {site}/{device}/{point} placeholders are rendered and sanitised");

        var body = JsonNode.Parse(published.Payload)!.AsObject();
        t.Equal(1, body["schemaVersion"]!.GetValue<int>(), "schemaVersion is injected");
        t.Equal("d1", body["device"]!.GetValue<string>(), "the payload is passed through unchanged");
        t.Close(42.5, body["value"]!.GetValue<double>(), 1e-9, "the value survives the round trip");
    }

    /// <summary>
    /// Broker 重启会丢掉 retained，所以每次重连都必须补发 $birth 与全部已知 $state。
    /// 这条同时验证断线重连本身：客户端要自己回去，而不是等人重启网关。
    /// </summary>
    private static async Task ReconnectRepublishesBirthAndState(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        await using var sink = new MqttSink(Options(broker.Port), "site-a", NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => sink.Client.IsConnected, "the sink connects");
        await WaitUntil(t, () => Count(broker, "uic/site-a/$birth") == 1, "the first $birth is published");

        await sink.PublishStateAsync("d1", "online", previous: null, error: null, t.Ct);
        await WaitUntil(t, () => Count(broker, "uic/site-a/d1/$state") == 1, "the first $state is published");

        broker.DropConnections();
        await WaitUntil(t, () => broker.Connects.Count >= 2, "the client reconnects on its own");

        await WaitUntil(t, () => Count(broker, "uic/site-a/$birth") >= 2,
            "the reconnected client republishes $birth");
        await WaitUntil(t, () => Count(broker, "uic/site-a/d1/$state") >= 2,
            "the reconnected client republishes the last known $state");

        var birth = broker.Published.Last(p => p.Topic == "uic/site-a/$birth");
        t.Equal(true, birth.Retain, "$birth is retained");
        t.Contains(birth.Payload, "\"online\":true", "$birth marks the gateway online");

        var state = broker.Published.Last(p => p.Topic == "uic/site-a/d1/$state");
        t.Equal(true, state.Retain, "$state is retained");
        t.Contains(state.Payload, "\"state\":\"online\"", "$state carries the device state");
    }

    /// <summary>Broker 拒绝连接时不能报告「已连接」，但必须继续重试而不是退出。</summary>
    private static async Task RefusedConnackKeepsRetrying(TestContext t)
    {
        await using var broker = new FakeMqttBroker { ConnackCode = 5 }; // 5 = not authorized
        await using var client = new MqttClient(Options(broker.Port), "site-a", NullLogger.Instance);

        client.Start();

        await WaitUntil(t, () => broker.Connects.Count >= 2, "the client retries after a refused CONNACK");

        t.Ok(!client.IsConnected, "a refused CONNACK must not be reported as connected");
        t.Ok(broker.Published.Count == 0, "no PUBLISH is attempted before the session is accepted");
    }

    /// <summary>
    /// CONNECT 里声明的 keepalive 是承诺：连接空闲时必须发 PINGREQ，
    /// 否则 broker 会在 1.5×keepalive 后单方面断开。有流量时这个缺陷看不出来。
    /// </summary>
    private static async Task IdleConnectionSendsPingreq(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        await using var client = new MqttClient(
            Options(broker.Port, keepAliveSeconds: 5), "site-a", NullLogger.Instance);

        client.Start();
        await WaitUntil(t, () => client.IsConnected, "the client connects");
        t.Equal((ushort)5, broker.Connects[0].KeepAliveSeconds, "keepalive is negotiated as configured");

        // 实现按 keepalive 的一半发心跳，5s 的 keepalive 应该在 ~2.5s 内出现。
        await WaitUntil(t, () => broker.PingCount >= 1, "an idle connection is kept alive", timeoutMs: 6000);

        t.Ok(client.IsConnected, "PINGRESP is consumed and the client stays connected");
    }

    private static int Count(FakeMqttBroker broker, string topic) =>
        broker.Published.Count(p => p.Topic == topic);
}
