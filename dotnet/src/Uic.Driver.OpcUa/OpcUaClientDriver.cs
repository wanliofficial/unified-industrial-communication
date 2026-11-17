using Microsoft.Extensions.Logging;
using Opc.Ua;
using Opc.Ua.Client;
using Opc.Ua.Configuration;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Driver.OpcUa;

/// <summary>
/// OPC UA 客户端（主站角色：网关去读 PLC/SCADA 的 UA 服务端）。
///
/// 为什么用官方协议栈而不是自己写：OPC UA 的 SecureChannel、会话、订阅、
/// 证书校验有大量边界条件，自研的成本远高于集成成本（北向服务端也是同一取向）。
/// 这一层只负责三件事：把点位翻译成 NodeId、把 Variant 翻译成领域值、
/// 把 StatusCode 翻译成质量码。
///
/// 与 Modbus/S7 的差别在于寻址：OPC UA 的点位是"地址空间里的一个节点"，
/// 没有寄存器的连续窗口概念，因此 <see cref="ReadPlanner"/> 对符号寻址是一个点一个
/// span；读请求仍然把同一轮的节点合并成一次 Read 服务调用。
///
/// 明确不支持（配置校验或运行期显式报错，不静默降级）：
/// - <c>nsu=</c> / <c>svr=</c> 形式的命名空间（要求写数字索引，见 <see cref="OpcUaAddress"/>）；
/// - 用户名/口令身份（本版本的凭据链路只覆盖北向；匿名 + 证书策略已满足 FR-COL-05）；
/// - 结构体/扩展对象类型（Variant 里的 ExtensionObject）；
/// - Basic256Sha256 以外的加密策略（Basic128Rsa15、Aes* 等）。
/// </summary>
public sealed class OpcUaClientDriver : IDriver
{
    /// <summary>驱动 ID / 通道 protocol 值。</summary>
    public const string Protocol = "opcua";

    public DriverMeta Meta { get; } = new()
    {
        // 与 Protocol 常量同源：通道里的 protocol 值就是驱动 ID，写两份迟早漂移。
        Id = Protocol,
        Name = "OPC UA 客户端",
        Version = "1.0.0",
        Roles = new[] { DriverRole.Master },
        Transports = new[] { "tcp" },
        AddressKinds = new[] { AddressKind.Symbol },
        Capabilities = new Capabilities(
            Read: CapState.Supported,
            Write: CapState.Supported,
            Subscribe: CapState.Supported,
            MultiRead: CapState.Supported),
        Description = "OPC UA 客户端：NodeId 符号寻址、批量读、写、监控项订阅；" +
                      "SecurityPolicy None / Basic256Sha256。",
        ConfigFields = new[]
        {
            new ConfigField("transport.host", "主机", "string", Required: true,
                Placeholder: "192.168.1.20", Scope: "channel"),
            new ConfigField("transport.port", "端口", "int", Required: true, Default: 4840, Scope: "channel"),
            new ConfigField("labels.endpointPath", "端点路径", "string", Scope: "device",
                Placeholder: "/UA/Server",
                Help: "服务端 URL 带路径时填写；一般留空"),
            new ConfigField("labels.securityPolicy", "安全策略", "string", Default: "None", Scope: "device",
                Enum: OpcUaClientOptions.SupportedPolicies,
                Help: "Basic256Sha256 需要客户端证书与对端信任"),
            new ConfigField("labels.securityMode", "安全模式", "string", Scope: "device",
                Enum: new[] { "None", "Sign", "SignAndEncrypt" },
                Help: "留空时：None 策略用 None，其余用 SignAndEncrypt"),
            new ConfigField("labels.acceptUntrustedCertificates", "接受自签证书", "bool", Default: true,
                Scope: "device",
                Help: "关闭后必须把服务端证书放进 <pkiDir>/trusted"),
            new ConfigField("labels.pkiDir", "证书目录", "string", Scope: "device",
                Help: "留空时放在数据目录的 opcua-client-pki；相对路径按数据目录解析"),
            new ConfigField("labels.sessionTimeout", "会话超时（毫秒）", "int", Default: 60000, Scope: "device"),
            new ConfigField("labels.publishingInterval", "订阅发布周期（毫秒）", "int", Default: 1000,
                Scope: "device", Help: "只影响 SubscribeAsync；轮询采集不受它影响"),
        },
        PointFields = new[]
        {
            new ConfigField("address.path", "NodeId", "string", Required: true, Scope: "point",
                Placeholder: "ns=2;s=Demo.Temp",
                Help: "OPC UA 的节点文本：ns=<n>;s=<文本> 或 ns=<n>;i=<数字>；省略 ns= 时用 address.index"),
            new ConfigField("address.index", "命名空间索引", "int", Default: 0, Scope: "point",
                Help: "只在 address.path 省略 ns= 时生效"),
        },
    };

    /// <summary>一次 Read/Write 里最多带多少个节点；服务端未声明上限、或声明的上限更大时用它。</summary>
    internal const int DefaultBatchNodes = 500;

    /// <summary>
    /// 单次 Read / Write 服务的节点上限：服务端声明的 <c>MaxNodesPerRead</c> 若比默认批量小就用它，
    /// 否则用默认批量。**返回值始终 ≥1**——调用方按这个步长递增偏移，返回 0 会让分片循环原地打转
    /// （不是慢，是永久卡住本轮采集）。服务端根本不声明上限时也必须回落到正的默认值。
    ///
    /// 放在驱动类上而不是会话类上：它是一条与"怎么连"无关的批处理规则，
    /// 抽成纯函数才能被用例直接钉住（缺失 / 0 / 超大三种输入）。
    /// </summary>
    internal static int MaxNodes(uint? operationLimit)
    {
        var limit = operationLimit ?? 0;
        return limit is > 0 and < DefaultBatchNodes ? (int)limit : DefaultBatchNodes;
    }

    public IConn Open(OpenContext context) => new OpcUaConn(context);
}

/// <summary>一条到 UA 服务端的会话。会话内部不是线程安全的，串行由调度器保证。</summary>
internal sealed class OpcUaConn : ISubscriptionConn
{
    private const string Protocol = OpcUaValues.Protocol;

    /// <summary>
    /// 证书生成与用完就丢的 ApplicationConfiguration 不同：同一目录的证书只能有
    /// 一份，多台设备同时首连会各生成一张，栈会以"证书已被占用"失败。
    /// 初始化串行化，代价只是首连时几十毫秒。
    /// </summary>
    private static readonly SemaphoreSlim CertGate = new(1, 1);

    private readonly OpenContext _ctx;
    private readonly OpcUaClientOptions _options;
    private readonly SemaphoreSlim _sessionLock = new(1, 1);

    private ApplicationConfiguration? _appConfig;
    private ISession? _session;
    private Subscription? _subscription;
    private int _disposed;

    public OpcUaConn(OpenContext ctx)
    {
        _ctx = ctx;
        _options = OpcUaClientOptions.FromContext(ctx);
    }

    public async Task ProbeAsync(CancellationToken ct)
    {
        var session = await EnsureSessionAsync(ct).ConfigureAwait(false);

        // 读 ServerStatus.CurrentTime：它同时证明"会话建立"和"读服务可用"，
        // 且任何符合规范的服务端都有这个节点。只连上不读，会把
        // "能握手但不能读"的服务端当成健康。
        var nodes = new ReadValueIdCollection
        {
            new() { NodeId = VariableIds.Server_ServerStatus_CurrentTime, AttributeId = Attributes.Value },
        };

        ReadResponse response;
        try
        {
            response = await session.ReadAsync(null, 0, TimestampsToReturn.Neither, nodes, ct)
                .ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            throw Translate(ex, "probe");
        }

        var status = (uint)response.Results[0].StatusCode;
        if (StatusCode.IsBad(status))
        {
            throw DriverException.ProtocolError(Protocol, "probe",
                $"reading ServerStatus.CurrentTime returned {OpcUaValues.DescribeStatus(status)}");
        }
    }

    public async Task<IReadOnlyList<RawSample>> ReadAsync(
        IReadOnlyList<ReadSpan> spans, CancellationToken ct)
    {
        var points = spans.SelectMany(s => s.Points).ToArray();
        if (points.Length == 0) return Array.Empty<RawSample>();

        // 先把地址解析一遍：地址写错的点位单独报，不拖累同一轮里正确的点位。
        var samples = new List<RawSample>(points.Length);
        var pending = new List<(PointRef Point, NodeId Node)>(points.Length);

        foreach (var point in points)
        {
            if (TryResolve(point, out var node, out var error))
            {
                pending.Add((point, node!));
            }
            else
            {
                samples.Add(new RawSample(point.Id, null, Quality.AddressError, null, error));
            }
        }

        if (pending.Count == 0) return samples;

        var session = await EnsureSessionAsync(ct).ConfigureAwait(false);
        var batch = OpcUaClientDriver.MaxNodes(session.OperationLimits?.MaxNodesPerRead);

        try
        {
            for (var offset = 0; offset < pending.Count; offset += batch)
            {
                var slice = pending.GetRange(offset, Math.Min(batch, pending.Count - offset));

                var nodes = new ReadValueIdCollection(slice.Count);
                foreach (var (_, node) in slice)
                    nodes.Add(new ReadValueId { NodeId = node, AttributeId = Attributes.Value });

                var response = await session
                    .ReadAsync(null, 0, TimestampsToReturn.Both, nodes, ct)
                    .ConfigureAwait(false);

                for (var i = 0; i < slice.Count; i++)
                {
                    samples.Add(OpcUaValues.ToSample(
                        slice[i].Point.Id, response.Results[i], slice[i].Point));
                }
            }
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            throw Translate(ex, "read");
        }

        return samples;
    }

    public async Task<IReadOnlyList<WriteResult>> WriteAsync(
        IReadOnlyList<WriteCommand> commands, CancellationToken ct)
    {
        var results = new WriteResult?[commands.Count];
        var pending = new List<(int Index, PointRef Point, NodeId Node, Variant Value)>(commands.Count);
        var now = DateTimeOffset.UtcNow;

        for (var i = 0; i < commands.Count; i++)
        {
            var command = commands[i];

            try
            {
                if (!TryResolve(command.Point, out var node, out var error))
                {
                    results[i] = new WriteResult(command.Point.Id, false, Quality.AddressError, now, error);
                    continue;
                }

                var variant = OpcUaValues.Encode(command.Value, command.Point);
                pending.Add((i, command.Point, node!, variant));
            }
            catch (DriverException ex)
            {
                results[i] = new WriteResult(command.Point.Id, false, QualityMap.From(ex.Kind), now, ex.Message);
            }
            catch (Exception ex)
            {
                results[i] = new WriteResult(command.Point.Id, false, Quality.Bad, now, ex.Message);
            }
        }

        if (pending.Count == 0) return results.Select(r => r!).ToArray();

        var session = await EnsureSessionAsync(ct).ConfigureAwait(false);
        var batch = OpcUaClientDriver.MaxNodes(session.OperationLimits?.MaxNodesPerRead);

        try
        {
            for (var offset = 0; offset < pending.Count; offset += batch)
            {
                var slice = pending.GetRange(offset, Math.Min(batch, pending.Count - offset));

                var values = new WriteValueCollection(slice.Count);
                foreach (var entry in slice)
                {
                    values.Add(new WriteValue
                    {
                        NodeId = entry.Node,
                        AttributeId = Attributes.Value,
                        Value = new Opc.Ua.DataValue(entry.Value) { StatusCode = StatusCodes.Good },
                    });
                }

                var response = await session.WriteAsync(null, values, ct).ConfigureAwait(false);

                for (var i = 0; i < slice.Count; i++)
                {
                    var status = (uint)response.Results[i];
                    var quality = OpcUaValues.FromStatus(status);
                    var ok = StatusCode.IsGood(status);

                    results[slice[i].Index] = new WriteResult(
                        slice[i].Point.Id, ok, quality, DateTimeOffset.UtcNow,
                        ok ? null : OpcUaValues.DescribeStatus(status));
                }
            }
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            throw Translate(ex, "write");
        }

        return results.Select(r => r!).ToArray();
    }

    public async Task<ISubscription> SubscribeAsync(
        IReadOnlyList<PointRef> points,
        Func<RawSample, ValueTask> onSample,
        CancellationToken ct)
    {
        if (points.Count == 0)
            throw DriverException.Address(Protocol, "subscribe", "no points to subscribe");

        var session = await EnsureSessionAsync(ct).ConfigureAwait(false);

        if (_subscription is not null)
        {
            throw DriverException.ProtocolError(Protocol, "subscribe",
                "this connection already has a subscription; dispose it before creating another");
        }

        var subscription = new Subscription(session.DefaultSubscription)
        {
            DisplayName = $"uic-{_ctx.Device.Id}",
            PublishingEnabled = true,
            PublishingInterval = (int)_options.PublishingInterval.TotalMilliseconds,
            KeepAliveCount = 10,
            LifetimeCount = 100,
        };

        session.AddSubscription(subscription);

        try
        {
            await subscription.CreateAsync(ct).ConfigureAwait(false);

            foreach (var point in points)
            {
                var node = OpcUaAddress.Parse(point.Address);

                var item = new MonitoredItem(subscription.DefaultItem)
                {
                    DisplayName = point.Id,
                    StartNodeId = node,
                    AttributeId = Attributes.Value,
                    SamplingInterval = (int)_options.PublishingInterval.TotalMilliseconds,
                    QueueSize = 1,
                    DiscardOldest = true,
                };

                // 通知回调在协议栈的发布线程上执行：这里只做翻译和投递，
                // 任何阻塞都会卡住后续通知。投递失败只记日志，不能把异常抛回栈里。
                item.Notification += (_, e) =>
                {
                    if (e.NotificationValue is not MonitoredItemNotification change) return;
                    Deliver(onSample, OpcUaValues.ToSample(point.Id, change.Value, point));
                };

                subscription.AddItem(item);
            }

            await subscription.ApplyChangesAsync(ct).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            await TryDeleteAsync(subscription).ConfigureAwait(false);
            throw;
        }
        catch (Exception ex)
        {
            await TryDeleteAsync(subscription).ConfigureAwait(false);
            throw Translate(ex, "subscribe");
        }

        _subscription = subscription;

        _ctx.Log.LogDebug("opcua subscription to {Endpoint} created: {Count} item(s), publishing {Interval}ms",
            _options.EndpointUrl, points.Count, (int)_options.PublishingInterval.TotalMilliseconds);

        return new LiveSubscription(this, subscription);
    }

    private static void Deliver(Func<RawSample, ValueTask> onSample, RawSample sample)
    {
        try
        {
            var pending = onSample(sample);
            if (!pending.IsCompletedSuccessfully)
            {
                _ = pending.AsTask().ContinueWith(
                    t => t.Exception,
                    CancellationToken.None,
                    TaskContinuationOptions.OnlyOnFaulted,
                    TaskScheduler.Default);
            }
        }
        catch
        {
            // 消费方的异常不得回到协议栈的发布线程。
        }
    }

    internal async ValueTask DetachAsync(Subscription subscription)
    {
        if (!ReferenceEquals(Interlocked.CompareExchange(ref _subscription, null, subscription), subscription))
            return;

        // 先删监控项/订阅，再从会话上摘掉：顺序反过来会让栈在后台发布线程上
        // 继续回调一个已经无人消费的订阅。
        await TryDeleteAsync(subscription).ConfigureAwait(false);

        var session = _session;
        if (session is null) return;

        try { await session.RemoveSubscriptionAsync(subscription).ConfigureAwait(false); }
        catch { /* 会话可能已经断开，摘不掉也不影响关闭 */ }
    }

    private async Task TryDeleteAsync(Subscription subscription)
    {
        try { await subscription.DeleteAsync(false).ConfigureAwait(false); }
        catch { /* 删除失败不能掩盖真正的原因 */ }
    }

    // ------------------------------------------------------------------
    // 会话
    // ------------------------------------------------------------------

    private async Task<ISession> EnsureSessionAsync(CancellationToken ct)
    {
        await _sessionLock.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            if (_session is not null) return _session;

            var session = await CreateSessionAsync(ct).ConfigureAwait(false);
            _session = session;
            return session;
        }
        finally
        {
            _sessionLock.Release();
        }
    }

    private async Task<ISession> CreateSessionAsync(CancellationToken ct)
    {
        var config = await ApplicationConfigAsync(ct).ConfigureAwait(false);

        // 先拿服务端公布的端点，再按配置挑：只依赖"最安全的那个端点"会让
        // 配置里的策略形同虚设。
        EndpointDescriptionCollection endpoints;
        try
        {
            using var discovery = await DiscoveryClient
                .CreateAsync(config, new Uri(_options.EndpointUrl), DiagnosticsMasks.None, ct)
                .ConfigureAwait(false);

            endpoints = await discovery.GetEndpointsAsync(null, ct).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            throw Translate(ex, "discover");
        }

        var (_, policyUri) = OpcUaClientOptions.NormalizePolicy(_options.SecurityPolicy);
        var description = OpcUaClientOptions.SelectEndpoint(endpoints, policyUri, _options.SecurityMode);
        var endpoint = new ConfiguredEndpoint(null, description, EndpointConfiguration.Create(config));

        ISession session;
        try
        {
            // 用工厂的异步重载而不是静态 Session.Create：后者已过时，而且同步
            // 建链会把调度器线程按在网络上。updateBeforeConnect 保持 false——
            // 端点已经在上一步按配置挑好了，再发现一次可能挑到别的策略档位。
            var sessionFactory = new DefaultSessionFactory(
                new DriverTelemetryContext(new SingleLoggerFactory(_ctx.Log)));

            session = await sessionFactory.CreateAsync(
                config,
                endpoint,
                updateBeforeConnect: false,
                sessionName: $"uic-{_ctx.Device.Id}",
                sessionTimeout: (uint)Math.Max(10_000, _options.SessionTimeout.TotalMilliseconds),
                identity: new UserIdentity(),
                preferredLocales: null,
                ct: ct).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            throw Translate(ex, "connect");
        }

        _ctx.Log.LogInformation(
            "opcua session to {Endpoint} established ({Policy}/{Mode}, session {Session})",
            _options.EndpointUrl, _options.SecurityPolicy, _options.SecurityMode, session.SessionName);

        return session;
    }

    private async Task<ApplicationConfiguration> ApplicationConfigAsync(CancellationToken ct)
    {
        if (_appConfig is not null) return _appConfig;

        await CertGate.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            if (_appConfig is not null) return _appConfig;

            var pki = _options.PkiDir;
            Directory.CreateDirectory(pki);
            Directory.CreateDirectory(Path.Combine(pki, "trusted"));
            Directory.CreateDirectory(Path.Combine(pki, "issuer"));
            Directory.CreateDirectory(Path.Combine(pki, "rejected"));

            var config = new ApplicationConfiguration
            {
                ApplicationName = _options.ApplicationName,
                ApplicationUri = _options.ApplicationUri,
                ApplicationType = ApplicationType.Client,
                ProductUri = "urn:uic:gateway",
                ClientConfiguration = new ClientConfiguration
                {
                    DefaultSessionTimeout = (int)Math.Max(10_000, _options.SessionTimeout.TotalMilliseconds),
                    MinSubscriptionLifetime = 10_000,
                },
                TransportQuotas = new TransportQuotas
                {
                    OperationTimeout = (int)Math.Max(100, _options.OperationTimeout.TotalMilliseconds),
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
                        SubjectName = "CN=uic-gateway-client",
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
                    AutoAcceptUntrustedCertificates = _options.AcceptUntrustedCertificates,
                    AddAppCertToTrustedStore = true,
                    RejectSHA1SignedCertificates = false,
                    MinimumCertificateKeySize = 2048,
                },

                // 协议栈在多处直接读取 TraceConfiguration，留空会在建链时抛空引用。
#pragma warning disable CS0618
                TraceConfiguration = new TraceConfiguration(),
#pragma warning restore CS0618
            };

            try
            {
                await config.ValidateAsync(ApplicationType.Client).ConfigureAwait(false);
            }
            catch (Exception ex) when (ex is not OperationCanceledException)
            {
                throw DriverException.Unsupported(Protocol, "open",
                    $"OPC UA client configuration is invalid: {ex.Message}");
            }

            var app = new ApplicationInstance(
                new DriverTelemetryContext(new SingleLoggerFactory(_ctx.Log)))
            {
                ApplicationName = _options.ApplicationName,
                ApplicationType = ApplicationType.Client,
                ApplicationConfiguration = config,
            };

            try
            {
                // 证书是"这台网关"的身份：None 策略下它只用于会话身份标识，
                // Basic256Sha256 下它参与签名与加密。首次运行自动生成自签证书。
                await app.CheckApplicationInstanceCertificatesAsync(
                    silent: false, lifeTimeInMonths: null, ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                throw;
            }
            catch (Exception ex)
            {
                throw DriverException.Unsupported(Protocol, "open",
                    $"OPC UA client certificate is unavailable under '{pki}': {ex.Message}");
            }

            _appConfig = config;
            return config;
        }
        finally
        {
            CertGate.Release();
        }
    }

    private static bool TryResolve(PointRef point, out NodeId? node, out string? error)
    {
        try
        {
            node = OpcUaAddress.Parse(point.Address);
            error = null;
            return true;
        }
        catch (DriverException ex)
        {
            node = null;
            error = ex.Message;
            return false;
        }
    }

    private DriverException Translate(Exception ex, string op)
    {
        if (ex is DriverException driver) return driver;

        if (ex is ServiceResultException sre)
        {
            var quality = OpcUaValues.FromStatus(sre.StatusCode);
            var kind = OpcUaValues.KindFromStatus(sre.StatusCode);
            var temporary = quality is Quality.Timeout or Quality.CommError;

            return new DriverException(kind, Protocol, op,
                $"{OpcUaValues.DescribeStatus(sre.StatusCode)}: {sre.Message}", temporary, sre);
        }

        if (ex is TimeoutException)
            return DriverException.Timeout(Protocol, op, ex.Message, ex);

        if (ex is System.Net.Sockets.SocketException socket)
            return DriverException.Connection(Protocol, op, socket.Message, socket);

        return new DriverException(ErrorKind.Internal, Protocol, op, ex.Message, temporary: false, ex);
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        var subscription = Interlocked.Exchange(ref _subscription, null);
        if (subscription is not null) await TryDeleteAsync(subscription).ConfigureAwait(false);

        var session = Interlocked.Exchange(ref _session, null);
        if (session is not null)
        {
            try
            {
                using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
                await session.CloseAsync(timeout.Token).ConfigureAwait(false);
            }
            catch { /* 关闭期异常忽略 */ }

            session.Dispose();
        }

        _sessionLock.Dispose();
    }

    /// <summary>订阅句柄：释放即删除监控项，不影响会话本身（会话由连接持有）。</summary>
    private sealed class LiveSubscription : ISubscription
    {
        private readonly OpcUaConn _conn;
        private readonly Subscription _subscription;
        private int _disposed;

        public LiveSubscription(OpcUaConn conn, Subscription subscription)
        {
            _conn = conn;
            _subscription = subscription;
        }

        public TimeSpan PublishingInterval => TimeSpan.FromMilliseconds(
            _subscription.CurrentPublishingInterval > 0
                ? _subscription.CurrentPublishingInterval
                : _subscription.PublishingInterval);

        public async ValueTask DisposeAsync()
        {
            if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

            await _conn.DetachAsync(_subscription).ConfigureAwait(false);
        }
    }
}

/// <summary>
/// 协议栈的遥测上下文。协议栈只认 <see cref="ILoggerFactory"/>，而驱动手里只有
/// 一个 <see cref="ILogger"/>；适配一层，协议栈的诊断才会和设备日志落在同一处。
/// </summary>
internal sealed class DriverTelemetryContext : TelemetryContextBase
{
    public DriverTelemetryContext(ILoggerFactory factory) : base(factory) { }
}

/// <summary>把单个 logger 适配成 factory：所有分类都写进同一条设备日志。</summary>
internal sealed class SingleLoggerFactory : ILoggerFactory
{
    private readonly ILogger _log;

    public SingleLoggerFactory(ILogger log) => _log = log;

    public ILogger CreateLogger(string categoryName) => _log;

    /// <summary>协议栈不注册额外 provider；空实现是为了满足接口。</summary>
    public void AddProvider(ILoggerProvider provider) { }

    public void Dispose() { }
}
