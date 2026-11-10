using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.OpcUa;
using Uic.Obs;

namespace Uic.Tests;

/// <summary>
/// OPC UA 客户端驱动的回环验收：驱动连本仓自带的 UA 服务端，走真实的会话与网络。
///
/// 纯函数用例（<see cref="OpcUaClientTests"/>）证明翻译规则对，这里证明「真的能连上、
/// 能批量读、能写、推送能到」。两端都是本仓代码，所以这不能替代与第三方 PLC/SCADA 的
/// 兼容性验证；Basic256Sha256 也一样——本仓服务端只提供 None，攻击面更大的那条路径
/// 只能靠端点选择用例守住「不静默降级」。这两条缺口写进 protocol-status.md。
/// </summary>
internal static class OpcUaClientLoopbackTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("opcua/client_read_write_subscribe", ReadWriteSubscribe, Flag: "opcua"));
        tests.Add(new TestCase("opcua/client_secure_policy_fails_without_match",
            SecurePolicyFailsWithoutMatch, Flag: "opcua"));
    }

    private static async Task ReadWriteSubscribe(TestContext t)
    {
        await using var server = await OpcUaServerFixture.StartAsync(t).ConfigureAwait(false);

        await using var conn = Connect(t, server, "None");

        // 探活必须真的读一次服务端状态：只握手不读，会把「能连不能读」判成健康。
        await conn.ProbeAsync(t.Ct).ConfigureAwait(false);
        t.Ok(true, "probe reads ServerStatus.CurrentTime");

        var temp = Point("temp", server.NodeIdText("v1", "temp"), DataType.Float64);
        var flag = Point("flag", server.NodeIdText("v1", "flag"), DataType.Bool);
        var count = Point("count", server.NodeIdText("v1", "count"), DataType.UInt16);

        // 一个不存在的节点：地址错误必须只影响它自己，不能拖垮同一轮里正确的点位。
        var missing = Point("missing", "ns=2;s=does.not.exist", DataType.Float64);

        var spans = new[]
        {
            new ReadSpan("span-0", new[] { temp }),
            new ReadSpan("span-1", new[] { flag }),
            new ReadSpan("span-2", new[] { count }),
            new ReadSpan("span-3", new[] { missing }),
        };

        var samples = await conn.ReadAsync(spans, t.Ct).ConfigureAwait(false);
        var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);

        t.Equal(4, samples.Count, "every point is reported back");
        t.Equal(Quality.Good, byId["temp"].Quality, "temp reads good");
        t.Close(55, byId["temp"].Value!.AsNumber(), 1e-6, "temp comes from the const waveform");
        t.Equal(Quality.Good, byId["flag"].Quality, "flag reads good");
        t.Equal(Quality.Good, byId["count"].Quality, "count reads good");
        t.Ok(byId["temp"].DeviceTime is not null, "the server timestamp is carried into the sample");

        t.Equal(Quality.AddressError, byId["missing"].Quality,
            "an unknown node is an address error, not a link failure");
        t.Ok(byId["missing"].Value is null, "an unknown node has no value");
        t.Contains(byId["missing"].Error ?? string.Empty, "BadNodeIdUnknown",
            "the sample names the server status");

        // ---- 写 ----
        var write = new WriteCommand(temp, DataValue.Number(DataType.Float64, 77.5));
        var written = await conn.WriteAsync(new[] { write }, t.Ct).ConfigureAwait(false);

        t.Ok(written[0].Ok, $"write temp = 77.5 ({written[0].Error})");
        t.Equal(Quality.Good, written[0].Quality, "a successful write reports good quality");

        // 服务端的点位缓存按设备周期刷新，写完立刻读到的可能还是上一轮的值；
        // 给一两轮采集的时间，而不是把「缓存还没刷新」当成写失败。
        var afterWrite = await ReadEventuallyAsync(conn, temp, 77.5, t).ConfigureAwait(false);

        t.Close(77.5, afterWrite.Value!.AsNumber(), 1e-6, "the written value is read back");

        // 越界值在本地就被拒绝：让它上网只会得到一句 BadOutOfRange，还得回查是哪一段。
        var overflow = await conn.WriteAsync(
            new[] { new WriteCommand(count, DataValue.Number(DataType.UInt16, 70_000)) },
            t.Ct).ConfigureAwait(false);

        t.Ok(!overflow[0].Ok, "an out-of-range write is refused");
        t.Equal(Quality.AddressError, overflow[0].Quality, "an out-of-range write is a point-table problem");

        // 写一个不存在的节点：失败要落在那个点位上，其余点位不受影响。
        var results = await conn.WriteAsync(new[]
        {
            new WriteCommand(missing, DataValue.Number(DataType.Float64, 1)),
            new WriteCommand(temp, DataValue.Number(DataType.Float64, 66)),
        }, t.Ct).ConfigureAwait(false);

        t.Ok(!results[0].Ok, "writing an unknown node fails");
        t.Ok(results[1].Ok, "the other write in the same batch still succeeds");

        // ---- 订阅 ----
        var wave = Point("wave", server.NodeIdText("v2", "wave"), DataType.Float64);

        var received = new TaskCompletionSource<RawSample>(TaskCreationOptions.RunContinuationsAsynchronously);

        await using var subscription = await conn.SubscribeAsync(
            new[] { wave },
            sample =>
            {
                // 回调在协议栈的发布线程上：这里只投递，不做任何阻塞。
                if (sample.Quality == Quality.Good) received.TrySetResult(sample);
                return ValueTask.CompletedTask;
            },
            t.Ct).ConfigureAwait(false);

        t.Ok(subscription.PublishingInterval > TimeSpan.Zero, "the negotiated publishing interval is reported");

        var notified = await Task.WhenAny(received.Task, Task.Delay(TimeSpan.FromSeconds(15), t.Ct))
            .ConfigureAwait(false);

        t.Ok(notified == received.Task, "a subscription notification arrives");
        t.Equal(Quality.Good, received.Task.Result.Quality, "the pushed sample is good");
        t.Ok(received.Task.Result.Value is not null, "the pushed sample carries a value");
    }

    /// <summary>
    /// 配置要求 Basic256Sha256，对端只提供 None：必须在选端点这一层失败并说清原因。
    /// 这是整个「不静默降级」承诺的端到端证据——纯函数用例证明选择逻辑，这里证明
    /// 它真的接在连接路径上。
    /// </summary>
    private static async Task SecurePolicyFailsWithoutMatch(TestContext t)
    {
        await using var server = await OpcUaServerFixture.StartAsync(t).ConfigureAwait(false);

        var labels = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["securityPolicy"] = "Basic256Sha256",
        };

        await using var conn = Connect(t, server, "None", labels);

        var failure = await t.ThrowsAsync<DriverException>(
            () => conn.ProbeAsync(t.Ct), "a missing security endpoint fails the connection").ConfigureAwait(false);

        t.Equal(ErrorKind.Unsupported, failure.Kind, "the mismatch is not retried blindly");
        t.Contains(failure.Message, "does not offer", "the error says the endpoint is missing");
        t.Contains(failure.Message, "Basic256Sha256/SignAndEncrypt",
            "the error names what was asked for");
        t.Contains(failure.Message, "None/None", "the error lists what the server actually offers");
    }

    /// <summary>轮询读到期望值；超时后返回最后一次结果，让断言报出真实的偏差。</summary>
    private static async Task<RawSample> ReadEventuallyAsync(
        IConn conn, PointRef point, double expected, TestContext t)
    {
        var span = new[] { new ReadSpan("poll", new[] { point }) };
        var deadline = DateTimeOffset.UtcNow.AddSeconds(10);

        RawSample sample;
        do
        {
            var samples = await conn.ReadAsync(span, t.Ct).ConfigureAwait(false);
            sample = samples[0];

            if (sample.Value is not null && Math.Abs(sample.Value.AsNumber() - expected) < 1e-6)
                return sample;

            await Task.Delay(100, t.Ct).ConfigureAwait(false);
        }
        while (DateTimeOffset.UtcNow < deadline);

        return sample;
    }

    private static ISubscriptionConn Connect(
        TestContext t,
        OpcUaServerFixture server,
        string policy,
        Dictionary<string, string>? labels = null)
    {
        var dictionary = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["securityPolicy"] = policy,

            // 回环要和 --opcua 的既有用例共享证书目录：一台网关一份客户端证书，
            // 每个用例各放一份只会让 PKI 目录在临时目录里散成几摊。
            ["pkiDir"] = "client-pki",
        };

        if (labels is not null)
        {
            foreach (var (key, value) in labels) dictionary[key] = value;
        }

        var channel = new Channel
        {
            Id = "ua-server",
            Protocol = OpcUaClientDriver.Protocol,
            Transport = "tcp",
            Host = "127.0.0.1",
            Port = server.Port,
        };

        var device = new Device
        {
            Id = "plc",
            Name = "回环 UA 服务端",
            ChannelId = channel.Id,
            Labels = dictionary,
        };

        var driver = new OpcUaClientDriver();

        // 订阅是客户端驱动的可选能力：用接口拿回来，才能同时验证两条契约。
        return (ISubscriptionConn)driver.Open(new OpenContext(
            channel, device, NullLogger.Instance, new MetricsRegistry(), t.TempDir));
    }

    private static PointRef Point(string id, string nodeIdText, DataType type) => new(
        id,
        Address.Symbol(nodeIdText),
        type,
        ByteOrder.ABCD,
        type == DataType.Float64 ? 4 : 2);
}
