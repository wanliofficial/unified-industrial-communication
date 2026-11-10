using Microsoft.Extensions.Logging;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Modbus;
using Uic.Driver.Virtual;
using Uic.Obs;
using Uic.Runtime;

namespace Uic.Tests;

/// <summary>
/// 起一个完整的网关（虚拟设备 + 北向 OPC UA 服务端），给客户端驱动的回环用例当对端。
///
/// 为什么用自家的服务端而不是另找一个 UA 服务端：仓库里没有一个可以离线启动的第三方
/// 服务端，而这里的目的是验证「客户端驱动与服务端说同一种语言」，自家服务端已经由
/// <see cref="OpcUaTests"/> 用标准客户端证明过兼容性。缺口（Basic256Sha256 没有
/// 仓内端到端）在文档里如实写明，而不是假装覆盖了。
/// </summary>
internal sealed class OpcUaServerFixture : IAsyncDisposable
{
    public const string Site = "opcua-client-site";

    private readonly ILoggerFactory _logs;
    private readonly GatewayRuntime _runtime;

    private OpcUaServerFixture(int port, ILoggerFactory logs, GatewayRuntime runtime)
    {
        Port = port;
        Endpoint = $"opc.tcp://127.0.0.1:{port}";
        _logs = logs;
        _runtime = runtime;
    }

    public int Port { get; }

    public string Endpoint { get; }

    /// <summary>北向地址空间的命名空间索引（服务端约定为 2，见 <see cref="OpcUaOptions"/>）。</summary>
    public ushort NamespaceIndex => 2;

    public static async Task<OpcUaServerFixture> StartAsync(TestContext t)
    {
        var port = FreePort();

        var ring = new RingBufferLogProvider(128);
        var logs = LoggerFactory.Create(builder =>
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
                Fallback = () => BuildConfig(port),
            },
            Drivers(),
            logs,
            ring,
            t.Ct).ConfigureAwait(false);

        return new OpcUaServerFixture(port, logs, runtime);
    }

    /// <summary>点位对应的节点文本，形如 <c>opcua-client-site.v1.temp</c>。</summary>
    public string NodeText(string device, string point) => $"{Site}.{device}.{point}";

    public string NodeIdText(string device, string point) => $"ns=2;s={NodeText(device, point)}";

    private static ConfigDocument BuildConfig(int port) => new()
    {
        SchemaVersion = 1,
        Site = Site,
        Description = "opc ua client loopback",
        Channels = new List<Channel>
        {
            new() { Id = "virtual-bus", Protocol = "virtual", Transport = "none" },
        },
        Devices = new List<Device>
        {
            // v1 用常量波形：写下去的值会被回放，才能断言「写什么读什么」。
            ConstDevice(),
            // v2 用正弦波形：订阅用例需要一个会持续变化的节点。
            SineDevice(),
        },
        Northbound = new NorthboundOptions
        {
            OpcUa = new OpcUaOptions
            {
                Enabled = true,
                Endpoint = $"opc.tcp://127.0.0.1:{port}",
                AllowAnonymousRead = true,

                // 回环要覆盖写路径；生产配置默认关闭匿名写（只读更安全）。
                AllowAnonymousWrite = true,
                SecurityPolicy = "None",
            },
        },
    };

    private static Device ConstDevice() => new()
    {
        Id = "v1",
        Name = "常量设备",
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
            },
            new()
            {
                Id = "flag",
                Name = "标志位",
                Address = Address.Register("coil", 0),
                DataType = DataType.Bool,
            },
            new()
            {
                Id = "count",
                Name = "计数",
                Address = Address.Register("holding", 20),
                DataType = DataType.UInt16,
            },
        },
    };

    private static Device SineDevice() => new()
    {
        Id = "v2",
        Name = "正弦设备",
        ChannelId = "virtual-bus",
        Interval = TimeSpan.FromMilliseconds(200),
        Labels = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["mode"] = "sine",
            ["baseline"] = "50",
            ["amplitude"] = "10",
            ["periodSeconds"] = "5",
        },
        Points = new List<Point>
        {
            new()
            {
                Id = "wave",
                Name = "波形",
                Address = Address.Register("holding", 0),
                DataType = DataType.Float64,
            },
        },
    };

    private static DriverRegistry Drivers()
    {
        var registry = new DriverRegistry();
        registry.Register(ModbusMasterDriver.Tcp(), ModbusMasterDriver.Rtu(), ModbusMasterDriver.Ascii());
        registry.Register(new VirtualDriver());
        return registry;
    }

    /// <summary>端口分配：绑一次 0 号端口取到系统给的值，再让给服务端。</summary>
    private static int FreePort()
    {
        var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
        listener.Start();
        var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
        listener.Stop();
        return port;
    }

    public async ValueTask DisposeAsync()
    {
        await _runtime.DisposeAsync().ConfigureAwait(false);
        _logs.Dispose();
    }
}
