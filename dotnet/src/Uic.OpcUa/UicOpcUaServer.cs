using Microsoft.Extensions.Logging;
using Opc.Ua;
using Opc.Ua.Configuration;
using Opc.Ua.Server;
using Uic.Domain;
using Uic.Engine;

namespace Uic.OpcUa;

/// <summary>
/// OPC UA 服务端门面：持有 <see cref="ApplicationInstance"/> 与自定义节点管理器。
///
/// 为什么用官方栈而不是自己写二进制协议：OPC UA 的握手、会话、订阅、监控项
/// 有大量边界条件，自研的维护成本远高于集成成本；这里只把「地址空间怎么长」
/// 掌握在自己手里。
/// </summary>
public sealed class UicOpcUaServer : IAsyncDisposable
{
    private readonly ApplicationInstance _app;
    private readonly UicServer _server;
    private readonly OpcUaServerOptions _options;
    private readonly ILogger _log;
    private int _disposed;

    private UicOpcUaServer(
        ApplicationInstance app,
        UicServer server,
        OpcUaServerOptions options,
        ILogger log)
    {
        _app = app;
        _server = server;
        _options = options;
        _log = log;
    }

    public string Endpoint => _options.Endpoint;
    public string NamespaceUri => _options.NamespaceUri;
    public UicNodeManager NodeManager => _server.NodeManager
        ?? throw new OpcUaException("node manager has not been created yet");
    public ServerState State => _server.CurrentState;

    /// <summary>
    /// 启动服务端。配置非法抛 <see cref="OpcUaConfigurationException"/>（宿主按退出码 2 处理）；
    /// 端口被占等运行期问题抛 <see cref="OpcUaException"/>。
    /// </summary>
    public static async Task<UicOpcUaServer> StartAsync(
        OpcUaServerOptions options,
        PointCache cache,
        Scheduler engine,
        ConfigDocument config,
        ILoggerFactory logs,
        CancellationToken ct = default)
    {
        var log = logs.CreateLogger("uic.opcua");
        var appConfig = BuildApplicationConfiguration(options);

        try
        {
            await appConfig.ValidateAsync(ApplicationType.Server).ConfigureAwait(false);
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            throw new OpcUaConfigurationException(
                $"OPC UA application configuration is invalid: {ex.Message}", ex);
        }

        // ApplicationInstance 已弃用的无参构造会自己建一个空遥测上下文；
        // 这里显式传入宿主的 ILoggerFactory，让协议栈日志进入同一套管道。
        var app = new ApplicationInstance(new UicTelemetryContext(logs))
        {
            ApplicationName = "uic-gateway",
            ApplicationType = ApplicationType.Server,
            ApplicationConfiguration = appConfig,
            // 即使 SecurityPolicy=None，客户端仍要求端点描述里带服务器证书；
            // 首次启动会在 PKI 目录生成自签证书。它是身份标识而非加密材料——
            // 本版本不提供基于证书的加密通道。
            DisableCertificateAutoCreation = false,
        };

        // 协议栈要求服务端在 SecurityPolicy=None 下也持有应用实例证书：
        // CreateSession 里会用实例证书做域名校验，证书为空会抛空引用（客户端只看到
        // BadUnexpectedError，极难定位）。首次启动自动生成自签证书，仅作身份标识。
        try
        {
            await app.CheckApplicationInstanceCertificatesAsync(silent: false, lifeTimeInMonths: null, ct)
                .ConfigureAwait(false);
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            throw new OpcUaConfigurationException(
                $"OPC UA application certificate is unavailable under '{options.PkiDir}': {ex.Message}", ex);
        }

        var server = new UicServer(log, options, cache, engine, config);

        try
        {
            await app.StartAsync(server).ConfigureAwait(false);
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            log.LogError(ex, "opc ua server failed to start");

            // Start 可能已经绑定了端口再在后续步骤失败；不清理会留下一个半启动的监听。
            try { await app.StopAsync().AsTask().WaitAsync(TimeSpan.FromSeconds(3)).ConfigureAwait(false); }
            catch { /* 清理失败不掩盖原始错误 */ }

            throw new OpcUaException($"OPC UA server failed to start on {options.Endpoint}: {ex.Message}", ex);
        }

        var instance = new UicOpcUaServer(app, server, options, log);

        var index = server.CurrentInstance?.NamespaceUris.GetIndex(options.NamespaceUri) ?? -1;
        log.LogInformation(
            "opc ua server running at {Endpoint} (namespace index {Index}, anonymous write {Write})",
            options.Endpoint, index, options.AllowAnonymousWrite);

        if (index >= 0 && index != options.ExpectedNamespaceIndex)
        {
            log.LogWarning(
                "opc ua namespace index is {Actual}, configuration expected {Expected}; " +
                "node ids in client tools will use the actual value",
                index, options.ExpectedNamespaceIndex);
        }

        return instance;
    }

    /// <summary>热更新地址空间。失败不会停止服务，只是新配置的点位不可见，调用方已记录日志。</summary>
    public void UpdateConfig(ConfigDocument config) => _server.NodeManager?.UpdateConfig(config);

    /// <summary>
    /// 从选项构造协议栈的 ApplicationConfiguration。
    ///
    /// internal 是为了让安全姿态（只声明 SecurityPolicy.None + 匿名令牌）能被用例钉住：
    /// 端到端用例用匿名会话连上就通过，往端点集合里再塞一个 Basic256Sha256 它照样绿，
    /// 于是「只支持 None」这件事会悄悄破。不绑端口也能断言这条，所以放这里。
    /// </summary>
    internal static ApplicationConfiguration BuildApplicationConfiguration(OpcUaServerOptions options)
    {
        var pki = options.PkiDir;
        Directory.CreateDirectory(pki);
        Directory.CreateDirectory(Path.Combine(pki, "trusted"));
        Directory.CreateDirectory(Path.Combine(pki, "issuer"));
        Directory.CreateDirectory(Path.Combine(pki, "rejected"));

        return new ApplicationConfiguration
        {
            ApplicationName = "uic-gateway",
            // ApplicationUri 不能与自定义命名空间 URI 相同：服务端命名空间表会去重，
            // 相同字符串会复用索引 1，导致 NodeId 变成 ns=1 而不是契约约定的 ns=2。
            ApplicationUri = $"urn:uic:gateway:{options.Site}",
            ApplicationType = ApplicationType.Server,
            ProductUri = "urn:uic:gateway",
            ServerConfiguration = new ServerConfiguration
            {
                BaseAddresses = { options.Endpoint },
                SecurityPolicies = new ServerSecurityPolicyCollection
                {
                    new()
                    {
                        SecurityMode = MessageSecurityMode.None,
                        SecurityPolicyUri = SecurityPolicies.None,
                    },
                },
                UserTokenPolicies = new UserTokenPolicyCollection
                {
                    new(UserTokenType.Anonymous),
                },
                DiagnosticsEnabled = false,
                MaxSessionCount = 100,
                MaxSubscriptionCount = 200,
                MinPublishingInterval = 100,
                MaxPublishingInterval = 3_600_000,
                PublishingResolution = 50,
            },
            TransportQuotas = new TransportQuotas
            {
                OperationTimeout = 15_000,
                MaxStringLength = 65_535,
                MaxArrayLength = 65_535,
                MaxByteStringLength = 1_048_576,
                MaxMessageSize = 4_194_304,
                MaxBufferSize = 65_535,
                ChannelLifetime = 300_000,
                SecurityTokenLifetime = 3_600_000,
            },
            SecurityConfiguration = new SecurityConfiguration
            {
                ApplicationCertificate = new CertificateIdentifier
                {
                    StoreType = "Directory",
                    StorePath = pki,
                    SubjectName = "CN=uic-gateway",
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
                // 本版本没有可信身份，Trust 的意义有限；显式接受是为了让客户端
                // 不会因为一个无法验证的自签证书被卡在门外。
                AutoAcceptUntrustedCertificates = true,
                RejectSHA1SignedCertificates = false,
                MinimumCertificateKeySize = 2048,
            },

            // 协议栈在多处直接读取 TraceConfiguration；留空会在启动期抛空引用。
            // 该属性已被标记过时（新版本改用 ITelemetryContext 输出日志），
            // 但 1.5 分支仍必须提供实例。
#pragma warning disable CS0618
            TraceConfiguration = new TraceConfiguration(),
#pragma warning restore CS0618
        };
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        try
        {
            using var stop = new CancellationTokenSource(TimeSpan.FromSeconds(5));
            await _app.StopAsync().AsTask().WaitAsync(stop.Token).ConfigureAwait(false);
            _log.LogInformation("opc ua server stopped");
        }
        catch (Exception ex)
        {
            _log.LogWarning(ex, "opc ua server stop failed");
        }
    }
}

/// <summary>把宿主的 ILoggerFactory 接进协议栈；TelemetryContextBase 的构造函数是 protected，必须派生。</summary>
internal sealed class UicTelemetryContext : TelemetryContextBase
{
    public UicTelemetryContext(ILoggerFactory factory) : base(factory) { }
}

/// <summary>
/// 把自定义节点管理器挂进服务端。必须在 <see cref="StandardServer.CreateMasterNodeManager"/>
/// 这一层注入——此时命名空间表还没冻结，晚一步注册就拿不到稳定的 ns 索引。
/// </summary>
internal sealed class UicServer : StandardServer
{
    private readonly ILogger _log;
    private readonly OpcUaServerOptions _options;
    private readonly PointCache _cache;
    private readonly Scheduler _engine;
    private readonly ConfigDocument _config;

    public UicServer(
        ILogger log,
        OpcUaServerOptions options,
        PointCache cache,
        Scheduler engine,
        ConfigDocument config)
    {
        _log = log;
        _options = options;
        _cache = cache;
        _engine = engine;
        _config = config;
    }

    public UicNodeManager? NodeManager { get; private set; }

    protected override MasterNodeManager CreateMasterNodeManager(
        IServerInternal server,
        ApplicationConfiguration configuration)
    {
        try
        {
            NodeManager = new UicNodeManager(
                server,
                configuration,
                _log,
                _options.NamespaceUri,
                _options.Site,
                _cache,
                _engine,
                _config,
                _options.AllowAnonymousWrite);

            return new MasterNodeManager(
                server,
                configuration,
                null,
                new INodeManager[] { NodeManager });
        }
        catch (Exception ex)
        {
            _log.LogError(ex, "creating the OPC UA node manager failed");
            throw;
        }
    }

    protected override void OnServerStarted(IServerInternal server)
    {
        base.OnServerStarted(server);
        NodeManager?.Start();
    }
}
