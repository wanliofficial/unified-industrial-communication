using Microsoft.Extensions.Logging;
using Uic.Codec.Mqtt;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Driver.Mqtt;

/// <summary>
/// MQTT 南向订阅驱动（网关作为订阅方，从 Broker 取现场数据）。
///
/// 为什么是「订阅 + 缓存」而不是每次 Read 都去问 Broker：MQTT 没有「读一个值」的
/// 请求/应答语义，只有发布/订阅。把采集建模成「维持订阅、缓存最近一条」，调度器的
/// 轮询就只是在取缓存——这与 OPC UA 的监控项是同一套思路，也不需要改采集引擎。
///
/// 寻址：<c>address.path</c> 是主题过滤器（可含 <c>+</c>/<c>#</c>），
/// <c>address.property</c> 是载荷里的 JSON 字段路径，留空时用设备标签
/// <c>payloadField</c>（默认 <c>value</c>）。所以「一条主题、多个字段」可以拆成多个点位，
/// 「一条主题、整个载荷就是一个数」也能读。
///
/// 质量语义：从未收到报文、或超过 <c>labels.staleAfterSeconds</c> 没有新报文，
/// 点位报 <see cref="Quality.Stale"/>；载荷不是 JSON 报 <see cref="Quality.Bad"/>；
/// 字段缺失/类型对不上报 <see cref="Quality.AddressError"/>。
///
/// 明确不支持：QoS2、持久会话、客户端证书、用户名/口令（见 <see cref="MqttConnOptions"/>）、
/// 写点位（按 MQTT 的语义应当发命令主题，这一版没有南向命令通道）。
/// </summary>
public sealed class MqttClientDriver : IDriver
{
    public const string Protocol = MqttConnOptions.Protocol;

    public DriverMeta Meta { get; } = new()
    {
        Id = Protocol,
        Name = "MQTT 订阅",
        Version = "1.0.0",
        Roles = new[] { DriverRole.Master },
        Transports = new[] { "tcp" },
        AddressKinds = new[] { AddressKind.Symbol },
        Capabilities = new Capabilities(
            Read: CapState.Supported,
            Write: CapState.Unsupported,
            // 协议本身是订阅型；采集主路径仍是调度器的 ReadAsync（读的是驱动内缓存）。
            Subscribe: CapState.Supported,
            MultiRead: CapState.Supported),
        Description = "MQTT 3.1.1 订阅方：主题寻址、JSON 字段取值、QoS0/1、TLS；只读。",
        ConfigFields = new[]
        {
            new ConfigField("transport.host", "主机", "string", Required: true,
                Placeholder: "192.168.1.30", Scope: "channel"),
            new ConfigField("transport.port", "端口", "int", Required: true, Default: 1883, Scope: "channel"),
            new ConfigField("labels.payloadField", "默认取值字段", "string", Default: "value", Scope: "device",
                Placeholder: "value / data.temp / .",
                Help: "JSON 字段名或 JSON 指针（/data/temp）；填 . 表示整个载荷就是值"),
            new ConfigField("labels.clientId", "客户端 ID", "string", Scope: "device",
                Help: "留空时用 uic-<设备 id>；同一 Broker 上不能与其它客户端重复"),
            new ConfigField("labels.qos", "订阅 QoS", "int", Default: 1, Scope: "device",
                Help: "0 或 1；QoS2 不支持"),
            new ConfigField("labels.keepAliveSeconds", "心跳周期（秒）", "int", Default: 30, Scope: "device"),
            new ConfigField("labels.reconnectDelayMs", "重连起始间隔（毫秒）", "int", Default: 2000,
                Scope: "device", Help: "断线后按指数退避，上限 30 秒"),
            new ConfigField("labels.staleAfterSeconds", "过期秒数", "int", Default: 0, Scope: "device",
                Help: "超过这么久没有新报文就把值判为 stale；0 表示不按时间过期"),
            new ConfigField("labels.tlsEnabled", "启用 TLS", "bool", Default: false, Scope: "device"),
            new ConfigField("labels.tlsCaFile", "CA 证书", "string", Scope: "device",
                Help: "PEM 文件；相对路径按数据目录解析。自签 Broker 必须填写"),
            new ConfigField("labels.tlsInsecureSkipVerify", "跳过证书校验", "bool", Default: false,
                Scope: "device", Help: "只用于联调；开启后连接加密但不认证对端"),
        },
        PointFields = new[]
        {
            new ConfigField("address.path", "主题", "string", Required: true, Scope: "point",
                Placeholder: "plant/line1/temp",
                Help: "要订阅的主题或主题过滤器，可含 + 与 #"),
            new ConfigField("address.property", "取值字段", "string", Scope: "point",
                Placeholder: "value / data.temp / .",
                Help: "留空时用设备标签 payloadField；填 . 表示整个载荷就是值"),
        },
    };

    public IConn Open(OpenContext context) => new MqttConn(context);
}

/// <summary>
/// 一条到 MQTT Broker 的订阅连接。订阅在 <see cref="ProbeAsync"/> 首次调用时启动，
/// 之后由协议层自己维持重连；<see cref="ReadAsync"/> 只读本地缓存。
/// </summary>
internal sealed class MqttConn : IConn
{
    private const string Protocol = MqttConnOptions.Protocol;

    private readonly OpenContext _ctx;
    private readonly MqttConnOptions _options;
    private readonly MqttClient _client;
    private readonly object _gate = new();

    /// <summary>点位 → 主题/字段。构造后只读，消息回调并发遍历是安全的。</summary>
    private readonly Dictionary<string, Watch> _watches = new(StringComparer.Ordinal);

    /// <summary>最近一条报文解出来的值。键是点位 id。</summary>
    private readonly Dictionary<string, Entry> _cache = new(StringComparer.Ordinal);

    /// <summary>需要向 Broker 登记的主题（去重后）。</summary>
    private readonly string[] _topics;

    private int _started;
    private int _disposed;

    public MqttConn(OpenContext ctx)
    {
        _ctx = ctx;
        _options = MqttConnOptions.FromContext(ctx);

        // 发遗嘱需要一个站点名，但南向订阅方不发遗嘱（announceWill: false）；
        // 这里传设备 id，只是为了协议层默认客户端 ID 有个人类可读的形态。
        _client = new MqttClient(
            _options.Client,
            ctx.Device.Id,
            ctx.Log,
            ctx.DataDir,
            caField: "labels.tlsCaFile",
            announceWill: false);

        _client.OnMessage = OnMessageAsync;

        foreach (var point in ctx.Device.Points.Where(p => p.Enabled))
        {
            _watches[point.Id] = BuildWatch(point, _options.PayloadField, ctx.Log);
        }

        _topics = _watches.Values
            .Where(w => w.Topic is not null)
            .Select(w => w.Topic!)
            .Distinct(StringComparer.Ordinal)
            .ToArray();

        foreach (var topic in _topics)
            _client.Subscribe(topic, _options.Client.Qos);
    }

    public async Task ProbeAsync(CancellationToken ct)
    {
        StartClient();

        // 探活不等到「收到第一条数据」：发布方可能本来就没上线，那属于数据面，
        // 不是链路故障。这里只验证「连上 Broker」且「订阅被接受」。
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
        timeout.CancelAfter(_ctx.Channel.ConnectTimeout);

        try
        {
            while (true)
            {
                var pending = false;

                foreach (var topic in _topics)
                {
                    switch (_client.SubscriptionGranted(topic))
                    {
                        case false:
                            throw DriverException.Permission(Protocol, "probe",
                                $"broker rejected the subscription to '{topic}' (SUBACK 0x80); " +
                                "check the broker ACL for this client");
                        case null:
                            pending = true;
                            break;
                    }
                }

                if (!pending && _client.IsConnected) return;

                await Task.Delay(25, timeout.Token).ConfigureAwait(false);
            }
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            throw DriverException.Connection(Protocol, "probe",
                $"no usable MQTT session within {_ctx.Channel.ConnectTimeout.TotalSeconds:0}s; " +
                $"check {_options.Client.Host}:{_options.Client.Port}" +
                (_options.Client.Tls.Enabled ? " and the TLS settings" : string.Empty));
        }
    }

    public Task<IReadOnlyList<RawSample>> ReadAsync(
        IReadOnlyList<ReadSpan> spans, CancellationToken ct)
    {
        var points = spans.SelectMany(s => s.Points).ToArray();
        if (points.Length == 0) return Task.FromResult<IReadOnlyList<RawSample>>(Array.Empty<RawSample>());

        var now = DateTimeOffset.UtcNow;
        var samples = new List<RawSample>(points.Length);

        foreach (var point in points)
        {
            if (!_watches.TryGetValue(point.Id, out var watch))
            {
                samples.Add(new RawSample(point.Id, null, Quality.AddressError, null,
                    $"point '{point.Id}' is not in this device's point list"));
                continue;
            }

            if (watch.Error is not null)
            {
                samples.Add(new RawSample(point.Id, null, Quality.AddressError, null, watch.Error));
                continue;
            }

            samples.Add(FromCache(point.Id, watch.Topic!, now));
        }

        return Task.FromResult<IReadOnlyList<RawSample>>(samples);
    }

    public Task<IReadOnlyList<WriteResult>> WriteAsync(
        IReadOnlyList<WriteCommand> commands, CancellationToken ct)
    {
        var now = DateTimeOffset.UtcNow;

        var results = commands
            .Select(c => new WriteResult(
                c.Point.Id,
                false,
                Quality.Disabled,
                now,
                "the MQTT southbound driver is read-only; publish a command on the broker instead"))
            .ToArray();

        return Task.FromResult<IReadOnlyList<WriteResult>>(results);
    }

    private RawSample FromCache(string pointId, string topic, DateTimeOffset now)
    {
        lock (_gate)
        {
            if (!_cache.TryGetValue(pointId, out var entry))
            {
                return new RawSample(pointId, null, Quality.Stale, null,
                    $"no message received yet on '{topic}'");
            }

            if (entry.Value is null)
            {
                // 收到过报文但一个值都解不出来（坏 JSON / 字段缺失）：报报文自身的质量，
                // 而不是「从未收到」——后者会把「发布方发了错格式」说成「发布方没上线」。
                var quality = entry.Quality == Quality.Good ? Quality.Bad : entry.Quality;
                return new RawSample(pointId, null, quality, entry.At, entry.Error);
            }

            var resultQuality = entry.Quality;
            var error = entry.Error;

            // 只在值本身是 Good 时判过期：已经报错的点位保留错误原因，
            // 否则「字段缺失」会在过期窗口后被改写成「没有新报文」，把真正的问题藏起来。
            if (resultQuality == Quality.Good &&
                _options.StaleAfterSeconds > 0 &&
                now - entry.At > TimeSpan.FromSeconds(_options.StaleAfterSeconds))
            {
                resultQuality = Quality.Stale;
                error = $"last message on '{topic}' is older than {_options.StaleAfterSeconds}s";
            }

            return new RawSample(pointId, entry.Value, resultQuality, entry.At, error);
        }
    }

    /// <summary>
    /// 入站分发。由协议层的单线程 worker 调用，不在读循环里——
    /// 这里只做 JSON 解析和缓存写入，不碰网络。
    /// </summary>
    private Task OnMessageAsync(MqttClient client, MqttMessage message, CancellationToken ct)
    {
        List<Watch>? hits = null;

        // 同一个主题可能被多个点位共用（不同字段），也可能被多个过滤器命中
        // （plant/# 与 plant/line1/temp 同时存在），所以逐条匹配而不是只取第一个。
        foreach (var watch in _watches.Values)
        {
            if (watch.Topic is null) continue;
            if (!MqttTopic.Matches(watch.Topic, message.Topic)) continue;

            (hits ??= new List<Watch>()).Add(watch);
        }

        if (hits is null) return Task.CompletedTask;

        var at = DateTimeOffset.UtcNow;

        foreach (var watch in hits)
        {
            var decoded = MqttValues.Decode(
                message.Payload, watch.Field, watch.Point.DataType, watch.Point.Count);

            lock (_gate)
            {
                if (!_cache.TryGetValue(watch.Point.Id, out var entry))
                {
                    entry = new Entry();
                    _cache[watch.Point.Id] = entry;
                }

                // 坏报文也要刷新 At：链路是通的，问题在载荷。过期判定看的是
                // 「多久没听到发布方」，不是「多久没解出一个好值」。
                entry.At = at;

                if (decoded.Value is not null) entry.Value = decoded.Value;
                entry.Quality = decoded.Quality;
                entry.Error = decoded.Error;
            }
        }

        return Task.CompletedTask;
    }

    private void StartClient()
    {
        if (Interlocked.Exchange(ref _started, 1) != 0) return;
        _client.Start();
    }

    private static Watch BuildWatch(Point point, string defaultField, ILogger log)
    {
        if (point.Address.Kind != AddressKind.Symbol)
        {
            return new Watch(point, null, string.Empty,
                $"point '{point.Id}' must use symbol addressing (address.path is the MQTT topic)");
        }

        var topic = point.Address.Path?.Trim();
        if (!MqttTopic.TryValidate(topic, out var error))
        {
            return new Watch(point, null, string.Empty, $"point '{point.Id}': {error}");
        }

        // 点位级 address.property 覆盖设备级 payloadField。留空（或 "." / "$"）
        // 表示整个载荷就是值，用于发布方只发裸数字/裸字符串的主题。
        var field = string.IsNullOrWhiteSpace(point.Address.Property)
            ? defaultField
            : MqttConnOptions.NormalizeField(point.Address.Property);

        log.LogDebug("mqtt point {Point} subscribes to {Topic}, field {Field}",
            point.Id, topic, field.Length == 0 ? "(whole payload)" : field);

        return new Watch(point, topic, field);
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;
        await _client.DisposeAsync().ConfigureAwait(false);
    }

    /// <summary>一个点位的订阅视图。<see cref="Error"/> 非空表示地址配置有问题，不会订阅。</summary>
    private sealed record Watch(Point Point, string? Topic, string Field, string? Error = null);

    /// <summary>缓存项：最近一个好值 + 最近一条报文的质量/时标。</summary>
    private sealed class Entry
    {
        public DataValue? Value { get; set; }
        public DateTimeOffset At { get; set; }
        public Quality Quality { get; set; } = Quality.Stale;
        public string? Error { get; set; }
    }
}
