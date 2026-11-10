using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Codec.Mqtt;
using Uic.Domain;
using Uic.Northbound;

namespace Uic.Tests;

/// <summary>
/// MQTT 下行命令通道的端到端验收。
///
/// 覆盖的不是「能收到消息」，而是运营上真正会出问题的几件事：
/// 结果必须回到约定主题、重复 id 不能执行两次、特权动作不能被匿名通道执行、
/// 断线重连后订阅要自己回来。
/// </summary>
internal static class MqttCommandTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("mqtt/command_executes_and_publishes_result", ExecutesAndPublishesResult));
        tests.Add(new TestCase("mqtt/command_unknown_action_returns_error", UnknownActionReturnsError));
        tests.Add(new TestCase("mqtt/command_duplicate_id_executes_once", DuplicateIdExecutesOnce));
        tests.Add(new TestCase("mqtt/command_privileged_action_is_forbidden", PrivilegedActionIsForbidden));
        tests.Add(new TestCase("mqtt/command_missing_id_is_rejected", MissingIdIsRejected));
        tests.Add(new TestCase("mqtt/command_resubscribes_after_reconnect", ResubscribesAfterReconnect));
        tests.Add(new TestCase("mqtt/command_outbox_flush_forwards_sink", OutboxFlushForwardsSink));
        tests.Add(new TestCase("mqtt/command_diag_bundle_is_whitelisted", DiagBundleIsWhitelisted));
    }

    private static MqttOptions Options(int port) => new()
    {
        Enabled = true,
        Host = "127.0.0.1",
        Port = port,
        Qos = 1,
        Retain = false,
        KeepAliveSeconds = 30,
        ReconnectDelayMs = 500,
        ClientId = "uic-test",
        TopicPrefix = "uic",
        Topic = "uic/{site}/{device}/{point}/value",
        Commands = true,
        CommandTopic = "uic/{site}/$command",
        CommandResultTopic = "uic/{site}/$command/result",
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

    private static string Payload(string id, string action, object? parameters = null, bool confirm = false)
        => JsonSerializer.Serialize(new { id, action, @params = parameters, confirm });

    private static async Task<JsonObject> AwaitResult(
        TestContext t, FakeMqttBroker broker, string topic, int count = 1)
    {
        await WaitUntil(t, () => broker.Published.Count(p => p.Topic == topic) >= count,
            $"the result is published to {topic}");

        var payload = broker.Published.Last(p => p.Topic == topic).Payload;
        return JsonNode.Parse(payload)!.AsObject();
    }

    /// <summary>请求 → 执行 → 结果回到结果主题，字段可被操作员直接消费。</summary>
    private static async Task ExecutesAndPublishesResult(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => sink.Client.IsConnected, "the client connects");
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the command topic is subscribed");

        t.Equal("uic/site-a/$command", commands.Topic, "the default command topic is rendered and sanitised");
        t.Equal("uic/site-a/$command/result", commands.ResultTopic, "the result topic is rendered");
        t.Equal(1, broker.Subscriptions.First(s => s.Topic == commands.Topic).Qos,
            "the subscription asks for QoS 1, otherwise a reconnecting broker may drop commands");

        await broker.PublishToClientAsync(commands.Topic,
            Payload("cmd-1", "driver.restart", new { device = "d1" }));

        var result = await AwaitResult(t, broker, commands.ResultTopic);

        t.Equal("cmd-1", result["id"]!.GetValue<string>(), "the result echoes the request id");
        t.Equal("driver.restart", result["action"]!.GetValue<string>(), "the result echoes the action");
        t.Equal(true, result["ok"]!.GetValue<bool>(), "the command succeeds");
        t.Equal(false, result["replayed"]!.GetValue<bool>(), "a first delivery is not a replay");
        t.Equal(1, result["schemaVersion"]!.GetValue<int>(), "the result carries a schema version");
        t.Ok(result["error"] is null, "a successful result has no error object");

        t.Equal(1, control.Restarts, "the control surface was invoked exactly once");
        t.Equal("d1", control.LastRestartDevice, "the device parameter is forwarded");
    }

    private static async Task UnknownActionReturnsError(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the command topic is subscribed");

        await broker.PublishToClientAsync(commands.Topic, Payload("cmd-2", "device.reboot"));

        var result = await AwaitResult(t, broker, commands.ResultTopic);
        var error = result["error"]!.AsObject();

        t.Equal(false, result["ok"]!.GetValue<bool>(), "an unknown action fails");
        t.Equal("request.unknown_action", error["code"]!.GetValue<string>(), "the error code is machine readable");
        t.Contains(error["message"]!.GetValue<string>(), "device.reboot", "the error names the rejected action");
    }

    /// <summary>
    /// 重连与人工重投都会造成重复。同一个 id 只能执行一次，第二次必须
    /// 拿到首次结果（而不是再重启一次设备）。
    /// </summary>
    private static async Task DuplicateIdExecutesOnce(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the command topic is subscribed");

        var payload = Payload("dup-1", "driver.restart", new { device = "d1" });

        await broker.PublishToClientAsync(commands.Topic, payload);
        await AwaitResult(t, broker, commands.ResultTopic);

        await broker.PublishToClientAsync(commands.Topic, payload);
        await AwaitResult(t, broker, commands.ResultTopic, count: 2);

        t.Equal(1, control.Restarts, "the duplicate id is not executed a second time");

        var results = broker.Published
            .Where(p => p.Topic == commands.ResultTopic)
            .Select(p => JsonNode.Parse(p.Payload)!.AsObject())
            .ToArray();

        t.Equal(false, results[0]["replayed"]!.GetValue<bool>(), "the first result is not marked as replay");
        t.Equal(true, results[1]["replayed"]!.GetValue<bool>(), "the duplicate result is marked as replay");
        t.Equal("dup-1", results[1]["id"]!.GetValue<string>(), "the replay keeps the original id");
    }

    /// <summary>
    /// MQTT 这一版只有匿名明文。回滚会切换整份配置，绝不能和重启设备
    /// 拥有同样的可达性——即使请求带了 confirm。
    /// </summary>
    private static async Task PrivilegedActionIsForbidden(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the command topic is subscribed");

        await broker.PublishToClientAsync(commands.Topic,
            Payload("cmd-3", "config.rollback", new { version = "v1" }, confirm: true));

        var result = await AwaitResult(t, broker, commands.ResultTopic);
        var error = result["error"]!.AsObject();

        t.Equal(false, result["ok"]!.GetValue<bool>(), "the privileged action is refused");
        t.Equal("command.forbidden", error["code"]!.GetValue<string>(), "the refusal has a stable error code");
        t.Equal(0, control.Rollbacks, "the control surface is never reached");
    }

    private static async Task MissingIdIsRejected(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the command topic is subscribed");

        await broker.PublishToClientAsync(commands.Topic,
            JsonSerializer.Serialize(new { action = "driver.restart", @params = new { device = "d1" } }));

        var result = await AwaitResult(t, broker, commands.ResultTopic);

        t.Equal(false, result["ok"]!.GetValue<bool>(), "a command without an id is rejected");
        t.Equal("request.invalid", result["error"]!.AsObject()["code"]!.GetValue<string>(),
            "the rejection is explicitly about the payload");
        t.Equal(0, control.Restarts, "nothing is executed without an idempotency key");
    }

    /// <summary>Broker 重启后客户端要自己重新订阅，否则命令通道会静默失效。</summary>
    private static async Task ResubscribesAfterReconnect(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the first connection subscribes");

        broker.DropConnections();
        await WaitUntil(t, () => broker.Connects.Count >= 2, "the client reconnects on its own");
        await WaitUntil(t, () => broker.Subscriptions.Count(s => s.Topic == commands.Topic) >= 2,
            "the reconnected client re-subscribes to the command topic");

        await broker.PublishToClientAsync(commands.Topic,
            Payload("after-reconnect", "outbox.flush"));

        var result = await AwaitResult(t, broker, commands.ResultTopic);

        t.Equal(true, result["ok"]!.GetValue<bool>(), "commands work after a reconnect");
        t.Equal(1, control.Flushes, "the command reaches the control surface on the new session");
    }

    /// <summary>
    /// outbox.flush 的 sink 参数必须透传：操作员只想补 MQTT 时，
    /// 不能顺手把 journal 也推一遍（那会改变 journal 的位点）。
    /// </summary>
    private static async Task OutboxFlushForwardsSink(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the command topic is subscribed");

        await broker.PublishToClientAsync(commands.Topic,
            Payload("flush-1", "outbox.flush", new { sink = "mqtt" }));

        var result = await AwaitResult(t, broker, commands.ResultTopic);

        t.Equal(true, result["ok"]!.GetValue<bool>(), "the flush succeeds");
        t.Equal(1, control.Flushes, "the control surface is invoked once");
        t.Equal("mqtt", control.LastFlushSink, "the sink filter is forwarded instead of being ignored");
    }

    /// <summary>诊断包是白名单动作，且返回值里要有落盘路径（否则操作员不知道去哪拿）。</summary>
    private static async Task DiagBundleIsWhitelisted(TestContext t)
    {
        await using var broker = new FakeMqttBroker();
        var options = Options(broker.Port);

        await using var sink = new MqttSink(options, "site-a", NullLogger.Instance);
        var control = new StubControl();
        await using var commands = new MqttCommandSink(
            sink.Client, options, "site-a", control, NullLogger.Instance);

        sink.Start();
        await WaitUntil(t, () => broker.Subscriptions.Any(s => s.Topic == commands.Topic),
            "the command topic is subscribed");

        await broker.PublishToClientAsync(commands.Topic, Payload("diag-1", "diag.bundle"));

        var result = await AwaitResult(t, broker, commands.ResultTopic);

        t.Equal(true, result["ok"]!.GetValue<bool>(), "diag.bundle is allowed on the command channel");
        t.Equal(1, control.Bundles, "the bundle is generated exactly once");
        t.Contains(result["detail"]!.GetValue<string>(), "uic-diagnostics", "the detail names the written file");
    }

    /// <summary>只统计被调用的控制面动作，用来证明「没有第二次执行」。</summary>
    private sealed class StubControl : IGatewayControl
    {
        private int _restarts;
        private int _rollbacks;
        private int _flushes;
        private int _selfTests;
        private int _bundles;

        public int Restarts => Volatile.Read(ref _restarts);
        public int Rollbacks => Volatile.Read(ref _rollbacks);
        public int Flushes => Volatile.Read(ref _flushes);
        public int SelfTests => Volatile.Read(ref _selfTests);
        public int Bundles => Volatile.Read(ref _bundles);

        public string? LastFlushSink { get; private set; }
        public string? LastRestartDevice { get; private set; }

        public ConfigDocument CurrentConfig { get; } = new();
        public string ConfigVersion => "stub-1";

        public Task<ApplyOutcome> ApplyConfigAsync(ConfigDocument doc, string? comment, CancellationToken ct)
            => throw new NotSupportedException("not used by the command channel");

        public Task<ApplyOutcome> RollbackAsync(string versionId, CancellationToken ct)
        {
            Interlocked.Increment(ref _rollbacks);
            throw new NotSupportedException("the command channel must refuse this before reaching here");
        }

        public Task RestartDeviceAsync(string deviceId, CancellationToken ct)
        {
            LastRestartDevice = deviceId;
            Interlocked.Increment(ref _restarts);
            return Task.CompletedTask;
        }

        public Task<bool> SelfTestAsync(string deviceId, CancellationToken ct)
        {
            Interlocked.Increment(ref _selfTests);
            return Task.FromResult(true);
        }

        public Task FlushOutboxAsync(string? sink, CancellationToken ct)
        {
            LastFlushSink = sink;
            Interlocked.Increment(ref _flushes);
            return Task.CompletedTask;
        }

        public Task<string> GenerateDiagnosticsAsync(CancellationToken ct)
        {
            Interlocked.Increment(ref _bundles);
            return Task.FromResult("/tmp/uic-diagnostics-test.zip");
        }

        public void SetLogLevel(string level) => throw new NotSupportedException("not used by this test");
    }
}
