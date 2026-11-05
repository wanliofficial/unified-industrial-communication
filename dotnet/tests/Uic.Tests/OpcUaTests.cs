using System.Diagnostics;
using Microsoft.Extensions.Logging;
using Opc.Ua;
using Opc.Ua.Client;
using Opc.Ua.Configuration;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Modbus;
using Uic.Driver.Virtual;
using Uic.Obs;
using Uic.Runtime;

namespace Uic.Tests;

/// <summary>
/// 北向 OPC UA 的端到端验收：匿名会话连上来之后，浏览、读、写、订阅都必须真的可用。
///
/// 用标准客户端而不是自己拼二进制：OPC UA 的握手（Hello/Ack、OpenSecureChannel、
/// CreateSession、CreateSubscription）一步都不能省，只有第三方栈能证明我们没有
/// 「看起来像 OPC UA」而是真的兼容。
///
/// 默认跳过（--opcua 开启）：它要生成证书、绑端口，启动开销明显高于纯函数用例。
/// </summary>
internal static class OpcUaTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("opcua/anonymous_session_read_write_subscribe", RunAsync, Flag: "opcua"));
    }

    private static async Task RunAsync(TestContext t)
    {
        const string site = "opcua-site";

        var port = Ip.FreePort();
        var endpoint = $"opc.tcp://127.0.0.1:{port}";

        var ring = new RingBufferLogProvider(128);
        using var logs = LoggerFactory.Create(builder =>
        {
            builder.ClearProviders();
            builder.AddProvider(ring);
            builder.SetMinimumLevel(LogLevel.Warning);
        });

        var runtime = await GatewayRuntime.CreateAsync(
            new RuntimeOptions
            {
                DataDir = t.TempDir,
                GatewayVersion = "0.0.0-test",
                Fallback = () => BuildConfig(endpoint, site),
            },
            Drivers(),
            logs,
            ring,
            t.Ct);

        try
        {
            using var session = await ConnectAsync(t, endpoint);

            t.Ok(session.Connected, "the session is connected");

            // ---- 浏览：Objects/UIC/{site}/{device} ----
            var uic = Browse(session, ObjectIds.ObjectsFolder, ReferenceTypeIds.Organizes)
                .FirstOrDefault(r => r.DisplayName.Text == "UIC");
            t.Ok(uic is not null, "Objects/UIC exists");
            if (uic is null) return;

            var uicNode = ExpandedNodeId.ToNodeId(uic.NodeId, session.NamespaceUris);
            t.Equal(2, uicNode.NamespaceIndex, "the UIC namespace is index 2");

            var siteNode = Browse(session, uicNode, ReferenceTypeIds.Organizes)
                .FirstOrDefault(r => r.DisplayName.Text == site);
            t.Ok(siteNode is not null, $"UIC/{site} exists");
            if (siteNode is null) return;

            var devices = Browse(session, ExpandedNodeId.ToNodeId(siteNode.NodeId, session.NamespaceUris),
                ReferenceTypeIds.Organizes);

            t.Equal(1, devices.Count, "one device under the site");
            t.Equal("虚拟设备", devices[0].DisplayName.Text, "device display name comes from the config");
            t.Equal($"{site}.v1", devices[0].NodeId.Identifier?.ToString(), "device node id is site-scoped");

            var points = Browse(session,
                ExpandedNodeId.ToNodeId(devices[0].NodeId, session.NamespaceUris),
                ReferenceTypeIds.HasComponent);

            var pointIds = points.Select(p => p.NodeId.Identifier?.ToString()).ToArray();
            t.Ok(pointIds.Contains($"{site}.v1.temp"), $"the device exposes temp (got {string.Join(", ", pointIds)})");
            t.Ok(pointIds.Contains($"{site}.v1.flag"), "the device exposes flag");

            // ---- 读 ----
            var temperatureId = new NodeId($"{site}.v1.temp", uicNode.NamespaceIndex);
            var value = ReadEventually(session, temperatureId, t);

            t.Ok(StatusCode.IsGood(value.StatusCode), $"read {temperatureId} ({value.StatusCode})");
            t.Close(55, Convert.ToDouble(value.Value, System.Globalization.CultureInfo.InvariantCulture),
                1e-6, "value comes from the const waveform");

            // ---- 写 ----
            var write = new WriteValue
            {
                NodeId = temperatureId,
                AttributeId = Attributes.Value,
                Value = new Opc.Ua.DataValue(new Variant(77.5)) { StatusCode = StatusCodes.Good },
            };

            session.Write(null, new WriteValueCollection { write }, out var statuses, out _);
            t.Ok(StatusCode.IsGood(statuses[0]), $"write {temperatureId} = 77.5 ({statuses[0]})");

            var after = ReadEventually(session, temperatureId, t, expected: 77.5);
            t.Close(77.5, Convert.ToDouble(after.Value, System.Globalization.CultureInfo.InvariantCulture),
                1e-6, "the written value is read back");

            // ---- 订阅 ----
            var subscription = new Subscription(session.DefaultSubscription)
            {
                PublishingInterval = 200,
                PublishingEnabled = true,
                KeepAliveCount = 10,
                LifetimeCount = 100,
            };

            session.AddSubscription(subscription);
            await subscription.CreateAsync().ConfigureAwait(false);

            try
            {
                var tcs = new TaskCompletionSource<object?>(TaskCreationOptions.RunContinuationsAsynchronously);

                var item = new MonitoredItem(subscription.DefaultItem)
                {
                    DisplayName = "temp",
                    StartNodeId = temperatureId,
                    AttributeId = Attributes.Value,
                    SamplingInterval = 100,
                    QueueSize = 10,
                };

                item.Notification += (_, e) =>
                {
                    if (e.NotificationValue is MonitoredItemNotification change)
                    {
                        tcs.TrySetResult(change.Value.Value);
                    }
                };

                subscription.AddItem(item);
                await subscription.ApplyChangesAsync().ConfigureAwait(false);

                var notified = await Task.WhenAny(tcs.Task, Task.Delay(TimeSpan.FromSeconds(10), t.Ct))
                    .ConfigureAwait(false);

                t.Ok(notified == tcs.Task, "a data change notification arrives");
            }
            finally
            {
                await subscription.DeleteAsync(false).ConfigureAwait(false);
            }
        }
        finally
        {
            await runtime.DisposeAsync();
        }
    }

    private static Opc.Ua.DataValue ReadEventually(Session session, NodeId id, TestContext t, double? expected = null)
    {
        var value = session.ReadValue(id);
        var deadline = Stopwatch.StartNew();

        while (deadline.Elapsed < TimeSpan.FromSeconds(10))
        {
            if (StatusCode.IsGood(value.StatusCode) && value.Value is not null)
            {
                if (expected is null) return value;

                var actual = Convert.ToDouble(value.Value, System.Globalization.CultureInfo.InvariantCulture);
                if (Math.Abs(actual - expected.Value) <= 1e-6 * Math.Max(1, Math.Abs(expected.Value))) return value;
            }

            Thread.Sleep(50);
            value = session.ReadValue(id);
        }

        return value;
    }

    private static async Task<Session> ConnectAsync(TestContext t, string endpointUrl)
    {
        var pki = Path.Combine(t.TempDir, "client-pki");

        var config = new ApplicationConfiguration
        {
            ApplicationName = "uic-tests-client",
            ApplicationUri = "urn:uic:tests:client",
            ApplicationType = ApplicationType.Client,
            SecurityConfiguration = new SecurityConfiguration
            {
                ApplicationCertificate = new CertificateIdentifier
                {
                    StoreType = "Directory",
                    StorePath = pki,
                    SubjectName = "CN=uic-tests-client",
                },
                TrustedPeerCertificates = new CertificateTrustList
                {
                    StoreType = "Directory",
                    StorePath = Path.Combine(pki, "trusted"),
                },
                TrustedIssuerCertificates = new CertificateTrustList
                {
                    StoreType = "Directory",
                    StorePath = Path.Combine(pki, "issuer"),
                },
                RejectedCertificateStore = new CertificateTrustList
                {
                    StoreType = "Directory",
                    StorePath = Path.Combine(pki, "rejected"),
                },
                AutoAcceptUntrustedCertificates = true,
            },
            ClientConfiguration = new ClientConfiguration
            {
                DefaultSessionTimeout = 60_000,
                MinSubscriptionLifetime = 10_000,
            },
        };

        await config.ValidateAsync(ApplicationType.Client).ConfigureAwait(false);

        var app = new ApplicationInstance
        {
            ApplicationName = "uic-tests-client",
            ApplicationType = ApplicationType.Client,
            ApplicationConfiguration = config,
        };

        await app.CheckApplicationInstanceCertificatesAsync(false, 0, t.Ct).ConfigureAwait(false);

        var description = CoreClientUtils.SelectEndpoint(config, endpointUrl, useSecurity: false);
        var endpoint = new ConfiguredEndpoint(null, description, EndpointConfiguration.Create(config));

        return await Session.Create(config, endpoint, updateBeforeConnect: false, sessionName: "uic-tests",
            sessionTimeout: 60_000, identity: new UserIdentity(), preferredLocales: null, ct: t.Ct)
            .ConfigureAwait(false);
    }

    private static List<ReferenceDescription> Browse(Session session, NodeId parent, NodeId referenceType)
    {
        var description = new BrowseDescription
        {
            NodeId = parent,
            BrowseDirection = BrowseDirection.Forward,
            ReferenceTypeId = referenceType,
            IncludeSubtypes = true,
            NodeClassMask = 0,
            ResultMask = (uint)BrowseResultMask.All,
        };

        session.Browse(null, null, 0u, new BrowseDescriptionCollection { description },
            out var results, out _);

        var all = new List<ReferenceDescription>();
        var result = results[0];
        if (result.References is not null) all.AddRange(result.References);

        while (result.ContinuationPoint is { Length: > 0 })
        {
            session.BrowseNext(null, false, new ByteStringCollection { result.ContinuationPoint },
                out var next, out _);

            result = next[0];
            if (result.References is not null) all.AddRange(result.References);
        }

        return all;
    }

    private static DriverRegistry Drivers()
    {
        var registry = new DriverRegistry();
        registry.Register(ModbusMasterDriver.Tcp(), ModbusMasterDriver.Rtu(), ModbusMasterDriver.Ascii());
        registry.Register(new VirtualDriver());
        return registry;
    }

    private static ConfigDocument BuildConfig(string endpoint, string site) => new()
    {
        SchemaVersion = 1,
        Site = site,
        Description = "opc ua contract test",
        Channels = new List<Channel>
        {
            new() { Id = "virtual-bus", Protocol = "virtual", Transport = "none" },
        },
        Devices = new List<Device>
        {
            new()
            {
                Id = "v1",
                Name = "虚拟设备",
                ChannelId = "virtual-bus",
                Interval = TimeSpan.FromMilliseconds(200),
                Labels = new Dictionary<string, string>(StringComparer.Ordinal)
                {
                    ["mode"] = "const",
                    ["baseline"] = "55",
                    ["amplitude"] = "1",
                },
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "temp",
                        Name = "温度",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.Float64,
                        Unit = "°C",
                    },
                    new()
                    {
                        Id = "flag",
                        Name = "标志位",
                        Address = Address.Register("coil", 0),
                        DataType = DataType.Bool,
                    },
                },
            },
        },
        Northbound = new NorthboundOptions
        {
            OpcUa = new OpcUaOptions
            {
                Enabled = true,
                Endpoint = endpoint,
                AllowAnonymousRead = true,

                // 验收要覆盖写路径；生产配置默认关闭匿名写（只读更安全）。
                AllowAnonymousWrite = true,
                SecurityPolicy = "None",
            },
        },
    };

    /// <summary>端口分配：绑一次 0 号端口取到系统给的值，再让给服务端。</summary>
    private static class Ip
    {
        public static int FreePort()
        {
            var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
            listener.Start();
            var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
            listener.Stop();
            return port;
        }
    }
}
