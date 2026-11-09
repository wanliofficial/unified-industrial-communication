using System.Buffers;
using System.Collections.Concurrent;
using System.Net.Security;
using System.Net.Sockets;
using System.Security.Authentication;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using System.Threading.Channels;
using Microsoft.Extensions.Logging;
using Uic.Domain;

namespace Uic.Northbound;

/// <summary>一条入站消息。主题已由 Broker 过滤，这里不再做通配匹配。</summary>
public sealed record MqttMessage(string Topic, byte Qos, bool Retain, byte[] Payload);

/// <summary>
/// 最小 MQTT 3.1.1 客户端：发布 + 订阅。
///
/// 为什么自己写：边缘部署要求单文件、无额外依赖，且我们只需要
/// CONNECT / PUBLISH(QoS0/1) / SUBSCRIBE / PINGREQ / DISCONNECT 几个报文。
/// 引入完整 MQTT 库会带来 WebSocket、持久会话等我们用不到的路径，
/// 那些路径一旦被配置误开，反而变成「以为加密了其实没有」的风险点。
/// TLS 用 <see cref="SslStream"/> 在流这一层包起来，几十行就能做完，
/// 因此不构成引入第三方库的理由。
///
/// 明确不支持：QoS2、WebSocket、持久会话。TLS 只做服务端证书校验（含自定义 CA），
/// 不做客户端证书；MQTT 报文层与明文路径完全相同，加密只发生在流这一层。
///
/// 订阅必须在 <see cref="Start"/> 之前登记：一次连接握手结束后统一发 SUBSCRIBE，
/// 并在每次重连后自动重发；运行中动态改订阅不在这一版的能力范围内。
/// </summary>
public sealed class MqttClient : IAsyncDisposable
{
    private const byte PacketConnect = 0x10;
    private const byte PacketConnack = 0x20;
    private const byte PacketPublish = 0x30;
    private const byte PacketPuback = 0x40;
    private const byte PacketSubscribe = 0x82;
    private const byte PacketSuback = 0x90;
    private const byte PacketPingreq = 0xC0;
    private const byte PacketPingresp = 0xD0;
    private const byte PacketDisconnect = 0xE0;

    private readonly MqttOptions _options;
    private readonly string _site;
    private readonly ILogger _log;
    private readonly CancellationTokenSource _cts = new();
    private readonly SemaphoreSlim _writeLock = new(1, 1);
    private readonly ConcurrentDictionary<ushort, TaskCompletionSource<bool>> _pending = new();
    private readonly ConcurrentDictionary<ushort, TaskCompletionSource<byte[]>> _subackPending = new();
    private readonly ConcurrentDictionary<string, byte> _subscriptions = new(StringComparer.Ordinal);
    private readonly System.Threading.Channels.Channel<MqttMessage> _inbound =
        System.Threading.Channels.Channel.CreateUnbounded<MqttMessage>(
            new UnboundedChannelOptions { SingleReader = true });
    private readonly object _connLock = new();

    private readonly string _dataDir;

    private TcpClient? _client;
    private Stream? _stream;
    private X509Certificate2Collection? _customRoots;
    private Task? _loop;
    private Task? _inboundWorker;
    private int _packetId;
    private long _lastWriteMs;
    private volatile bool _connected;
    private int _started;
    private int _disposed;

    public MqttClient(MqttOptions options, string site, ILogger log, string? dataDir = null)
    {
        _options = options;
        _site = site;
        _log = log;
        _dataDir = dataDir ?? Environment.CurrentDirectory;

        // 告警放在构造时而不是第一次握手里：Broker 连不上时也要在启动日志里
        // 留下「这次部署没有认证对端」的证据，否则 TLS 看起来一直是开着的。
        if (options.Tls.Enabled && options.Tls.InsecureSkipVerify)
        {
            _log.LogWarning(
                "mqtt TLS is accepting any certificate (insecureSkipVerify); " +
                "traffic is encrypted but the broker is not authenticated");
        }
    }

    public bool IsConnected => _connected;

    public string WillTopic => $"{_options.TopicPrefix}/{Segment(_site)}/$death";

    /// <summary>连接成功后的回调：重连时要补发 $birth 与全部 $state。</summary>
    public Func<MqttClient, CancellationToken, Task>? OnConnected { get; set; }

    /// <summary>
    /// 入站消息回调。由独立 worker 串行调用，不在读循环里执行——
    /// 命令处理可能耗时几百毫秒，堵住读循环就会拖慢 PUBACK 与心跳。
    /// </summary>
    public Func<MqttClient, MqttMessage, CancellationToken, Task>? OnMessage { get; set; }

    /// <summary>
    /// 登记订阅。必须在 <see cref="Start"/> 之前调用；重连时自动重发。
    /// </summary>
    public void Subscribe(string topicFilter, int qos = 1)
    {
        if (string.IsNullOrWhiteSpace(topicFilter))
            throw new ArgumentException("topic filter is required", nameof(topicFilter));
        if (Volatile.Read(ref _started) != 0)
            throw new InvalidOperationException("subscriptions must be registered before Start()");

        _subscriptions[topicFilter] = (byte)Math.Clamp(qos, 0, 1);
    }

    public IReadOnlyList<string> Subscriptions => _subscriptions.Keys.ToArray();

    public void Start()
    {
        if (_disposed != 0) return;
        if (Interlocked.Exchange(ref _started, 1) != 0) return;

        _inboundWorker = Task.Run(() => InboundWorkerAsync(_cts.Token));
        _loop = Task.Run(() => RunAsync(_cts.Token));
    }

    private async Task RunAsync(CancellationToken ct)
    {
        var delay = TimeSpan.FromMilliseconds(Math.Max(500, _options.ReconnectDelayMs));

        while (!ct.IsCancellationRequested)
        {
            // 每条连接一份会话令牌：断链时取消它，读循环与心跳循环一起退出。
            using var session = CancellationTokenSource.CreateLinkedTokenSource(ct);
            Task? keepAlive = null;

            try
            {
                await ConnectAsync(session.Token).ConfigureAwait(false);

                _log.LogInformation("mqtt connected to {Host}:{Port}", _options.Host, _options.Port);

                // 订阅在启动读循环之前完成：SUBACK 需要同步读到，否则它会被
                // 读循环当成未知报文丢掉，我们永远不知道订阅是否被 Broker 接受。
                await SubscribeAllAsync(session.Token).ConfigureAwait(false);

                // 读循环必须先跑起来：OnConnected 里要补发 $birth 与全部 $state，
                // 这些是 QoS1、要等 PUBACK，而 PUBACK 只有读循环能消费。
                // 顺序反了的话每次重连都会先卡满 3s 超时，再"补发"。
                var reader = ReadLoopAsync(session.Token);
                keepAlive = KeepAliveAsync(session.Token);

                // 到这里链路才真正可用：订阅已被接受、读循环已在消费 PUBACK。
                // 入站 worker 也在等这个标志，避免命令结果在还没人读确认时发出。
                _connected = true;

                if (OnConnected is not null)
                {
                    try
                    {
                        await OnConnected(this, session.Token).ConfigureAwait(false);
                    }
                    catch (Exception ex)
                    {
                        _log.LogWarning(ex, "mqtt on-connected hook failed");

                        // 钩子失败通常意味着连接已经不可用：关掉流让读循环退出，
                        // 走下面的重连路径，而不是把它挂在这里。
                        await CloseAsync().ConfigureAwait(false);
                    }
                }

                delay = TimeSpan.FromMilliseconds(Math.Max(500, _options.ReconnectDelayMs));
                await reader.ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                return;
            }
            catch (Exception ex)
            {
                _log.LogWarning("mqtt connection lost: {Error}", ex.Message);
            }
            finally
            {
                _connected = false;
                await session.CancelAsync().ConfigureAwait(false);

                if (keepAlive is not null)
                {
                    try { await keepAlive.ConfigureAwait(false); }
                    catch { /* 会话结束时的取消/写失败，均已记录 */ }
                }

                FailPending("connection closed");
                await CloseAsync().ConfigureAwait(false);
            }

            // 指数退避，上限 30s：Broker 重启时不要把它打崩。
            var wait = delay;
            delay = TimeSpan.FromMilliseconds(Math.Min(30_000, delay.TotalMilliseconds * 2));

            try { await Task.Delay(wait, ct).ConfigureAwait(false); }
            catch (OperationCanceledException) { return; }
        }
    }

    private async Task ConnectAsync(CancellationToken ct)
    {
        var client = new TcpClient { NoDelay = true };
        Stream stream;

        using (var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct))
        {
            timeout.CancelAfter(TimeSpan.FromSeconds(10));
            await client.ConnectAsync(_options.Host, _options.Port, timeout.Token).ConfigureAwait(false);
            stream = await OpenStreamAsync(client, timeout.Token).ConfigureAwait(false);
        }

        lock (_connLock)
        {
            _client = client;
            _stream = stream;
        }

        var clientId = string.IsNullOrWhiteSpace(_options.ClientId)
            ? $"uic-{Segment(_site)}"
            : _options.ClientId;

        var body = new List<byte>(64);

        WriteString(body, "MQTT");
        body.Add(4); // protocol level 3.1.1

        byte flags = 0x02; // clean session
        var willPayload = Encoding.UTF8.GetBytes("{\"schemaVersion\":1,\"online\":false}");

        if (_options.Qos >= 0)
        {
            flags |= 0x04;          // will flag
            flags |= 0x20;          // will retain
            flags |= (byte)(Math.Min(1, _options.Qos) << 3); // will qos
        }

        var hasUser = !string.IsNullOrEmpty(_options.Username);
        var hasPass = !string.IsNullOrEmpty(_options.Password);

        if (hasUser) flags |= 0x80;
        if (hasPass) flags |= 0x40;

        body.Add(flags);
        WriteUInt16(body, (ushort)Math.Clamp(_options.KeepAliveSeconds, 5, 600));

        WriteString(body, clientId);

        if (_options.Qos >= 0)
        {
            WriteString(body, WillTopic);
            WriteUInt16(body, (ushort)willPayload.Length);
            body.AddRange(willPayload);
        }

        if (hasUser) WriteString(body, _options.Username!);
        if (hasPass) WriteString(body, _options.Password!);

        await WritePacketAsync(PacketConnect, body.ToArray(), ct).ConfigureAwait(false);

        // CONNACK 必须先读到，否则后面的 PUBLISH 全是在跟空气说话。
        var (header, payload) = await ReadPacketAsync(ct).ConfigureAwait(false);

        if ((header & 0xF0) != PacketConnack || payload.Length < 2)
            throw new InvalidOperationException("expected CONNACK");

        if (payload[1] != 0)
            throw new InvalidOperationException($"broker refused connection: code {payload[1]}");
    }

    /// <summary>
    /// 心跳：连接空闲时按 keepalive 的一半发 PINGREQ。
    ///
    /// 不发的话 broker 会在 1.5×keepalive 后主动断开，表现为「没有数据的时候
    /// 每隔几十秒重连一次」——有流量时看不出来，现场静默时段才会暴露。
    /// 因此只要链路上没有写动作就补一次 ping，而不是无条件定时发。
    /// </summary>
    private async Task KeepAliveAsync(CancellationToken ct)
    {
        var keepAliveSeconds = Math.Max(5, _options.KeepAliveSeconds);
        var interval = TimeSpan.FromMilliseconds(keepAliveSeconds * 1000L / 2);
        var intervalMs = (long)interval.TotalMilliseconds;

        while (!ct.IsCancellationRequested)
        {
            await Task.Delay(interval, ct).ConfigureAwait(false);

            var idleMs = Environment.TickCount64 - Interlocked.Read(ref _lastWriteMs);
            if (idleMs < intervalMs) continue;

            await WritePacketAsync(PacketPingreq, Array.Empty<byte>(), ct).ConfigureAwait(false);
        }
    }

    /// <summary>
    /// 连接建立后统一发 SUBSCRIBE。QoS 取配置值（上限 1），并同步等待 SUBACK。
    /// Broker 用 0x80 拒绝某个主题时不能假装订阅成功——命令通道会静默失效。
    /// </summary>
    private async Task SubscribeAllAsync(CancellationToken ct)
    {
        var topics = _subscriptions.ToArray();
        if (topics.Length == 0) return;

        var packetId = NextPacketId();
        var tcs = new TaskCompletionSource<byte[]>(TaskCreationOptions.RunContinuationsAsynchronously);
        _subackPending[packetId] = tcs;

        var body = new List<byte>(64);
        WriteUInt16(body, packetId);

        foreach (var (topic, qos) in topics)
        {
            WriteString(body, topic);
            body.Add(qos);
        }

        try
        {
            await WritePacketAsync(PacketSubscribe, body.ToArray(), ct).ConfigureAwait(false);

            // 读循环还没启动，这里同步把 SUBACK 读出来；期间万一有 retained
            // PUBLISH 抢先到达，也走同一个分发器，消息先入队不会丢。
            // 但等待必须有上限：Broker 回了一个错误的 packet id 时，
            // 不能把整条连接挂在「等 SUBACK」上。
            using var wait = CancellationTokenSource.CreateLinkedTokenSource(ct);
            wait.CancelAfter(TimeSpan.FromSeconds(10));

            try
            {
                while (!tcs.Task.IsCompleted)
                {
                    var (header, payload) = await ReadPacketAsync(wait.Token).ConfigureAwait(false);
                    await DispatchAsync(header, payload, wait.Token).ConfigureAwait(false);
                }
            }
            catch (OperationCanceledException) when (!ct.IsCancellationRequested)
            {
                throw new TimeoutException("no SUBACK within 10s");
            }

            var granted = await tcs.Task.ConfigureAwait(false);
            var rejected = 0;
            for (var i = 0; i < granted.Length; i++)
            {
                if (granted[i] == 0x80)
                {
                    rejected++;
                    _log.LogError("broker rejected subscription to {Topic}", topics[Math.Min(i, topics.Length - 1)].Key);
                }
            }

            if (granted.Length == 0)
            {
                _log.LogWarning("broker returned an empty SUBACK; subscription state is unknown");
                return;
            }

            if (rejected == granted.Length)
            {
                // 不能因为命令订阅被 ACL 拒绝就把数据通道一起拖进重连循环。
                _log.LogError("broker rejected every subscription; inbound commands are offline");
                return;
            }

            _log.LogInformation("mqtt subscribed to {Topics}",
                string.Join(", ", topics.Select(t => t.Key)));
        }
        finally
        {
            _subackPending.TryRemove(packetId, out _);
        }
    }

    private async Task ReadLoopAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested)
        {
            var (header, payload) = await ReadPacketAsync(ct).ConfigureAwait(false);
            await DispatchAsync(header, payload, ct).ConfigureAwait(false);
        }
    }

    /// <summary>把一条报文分派到正确的等待者/处理器。读循环与 SUBACK 同步等待共用。</summary>
    private async Task DispatchAsync(byte header, byte[] payload, CancellationToken ct)
    {
        switch (header & 0xF0)
        {
            case PacketPuback when payload.Length >= 2:
            {
                var id = (ushort)((payload[0] << 8) | payload[1]);
                if (_pending.TryRemove(id, out var tcs)) tcs.TrySetResult(true);
                break;
            }

            case PacketSuback when payload.Length >= 2:
            {
                var id = (ushort)((payload[0] << 8) | payload[1]);
                if (_subackPending.TryRemove(id, out var tcs)) tcs.TrySetResult(payload[2..]);
                break;
            }

            case PacketPingresp:
                break;

            case PacketPublish:
                await HandlePublishAsync(header, payload, ct).ConfigureAwait(false);
                break;

            default:
                _log.LogDebug("ignoring mqtt packet 0x{Header:x2}", header);
                break;
        }
    }

    /// <summary>
    /// 入站 PUBLISH。QoS1 先入队再回 PUBACK：确认之后 Broker 认为责任已转移，
    /// 而这一版没有持久会话，进程崩溃仍会丢——去重由发布方的 id 兜底。
    /// </summary>
    private async Task HandlePublishAsync(byte header, byte[] payload, CancellationToken ct)
    {
        var qos = (byte)((header >> 1) & 0x03);
        var retain = (header & 0x01) != 0;

        if (qos == 2)
        {
            _log.LogWarning("inbound QoS2 PUBLISH is not supported; dropping");
            return;
        }

        var offset = 0;
        if (payload.Length < 2) return;

        var topicLength = (payload[offset] << 8) | payload[offset + 1];
        offset += 2;

        if (payload.Length < offset + topicLength) return;
        var topic = Encoding.UTF8.GetString(payload, offset, topicLength);
        offset += topicLength;

        ushort packetId = 0;
        if (qos > 0)
        {
            if (payload.Length < offset + 2) return;
            packetId = (ushort)((payload[offset] << 8) | payload[offset + 1]);
            offset += 2;
        }

        _inbound.Writer.TryWrite(new MqttMessage(topic, qos, retain, payload[offset..]));

        if (qos > 0)
        {
            await WritePacketAsync(PacketPuback,
                new[] { (byte)(packetId >> 8), (byte)(packetId & 0xFF) }, ct).ConfigureAwait(false);
        }
    }

    /// <summary>
    /// 入站消息 worker。串行执行 OnMessage，异常只记录不终止——
    /// 一条坏命令不能把整条链路带走。
    /// </summary>
    private async Task InboundWorkerAsync(CancellationToken ct)
    {
        try
        {
            await foreach (var message in _inbound.Reader.ReadAllAsync(ct).ConfigureAwait(false))
            {
                if (OnMessage is null) continue;

                // 消息只会在连接存活时到达；等链路真正可用再处理，
                // 避免命令结果在 PUBACK 还没人读时发出。
                while (!_connected && !ct.IsCancellationRequested)
                    await Task.Delay(10, ct).ConfigureAwait(false);

                if (ct.IsCancellationRequested) return;

                try
                {
                    await OnMessage(this, message, ct).ConfigureAwait(false);
                }
                catch (OperationCanceledException) when (ct.IsCancellationRequested)
                {
                    return;
                }
                catch (Exception ex)
                {
                    _log.LogWarning(ex, "mqtt inbound handler failed for {Topic}", message.Topic);
                }
            }
        }
        catch (OperationCanceledException)
        {
            // 正常停机。
        }
    }

    /// <summary>发布一条消息。QoS1 必须等到 PUBACK 才返回，否则 Outbox 会误以为投递成功。</summary>
    public async Task PublishAsync(string topic, ReadOnlyMemory<byte> payload, bool retain, CancellationToken ct)
    {
        if (!_connected) throw new InvalidOperationException("mqtt is not connected");

        var qos = Math.Clamp(_options.Qos, 0, 1);

        var body = new List<byte>(payload.Length + topic.Length + 8);
        WriteString(body, topic);

        ushort packetId = 0;
        if (qos > 0)
        {
            packetId = NextPacketId();
            body.Add((byte)(packetId >> 8));
            body.Add((byte)(packetId & 0xFF));
        }

        body.AddRange(payload.ToArray());

        var header = (byte)(PacketPublish | (qos << 1) | (retain ? 1 : 0));

        if (qos == 0)
        {
            await WritePacketAsync(header, body.ToArray(), ct).ConfigureAwait(false);
            return;
        }

        // 必须先登记再写：PUBACK 可能在 WritePacketAsync 返回之前就到达，
        // 那时读循环找不到等待者，会把确认丢掉，发布白白等到 3s 超时。
        var tcs = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        _pending[packetId] = tcs;

        try
        {
            await WritePacketAsync(header, body.ToArray(), ct).ConfigureAwait(false);

            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
            timeout.CancelAfter(TimeSpan.FromSeconds(3));

            try
            {
                await tcs.Task.WaitAsync(timeout.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (!ct.IsCancellationRequested)
            {
                throw new TimeoutException($"no PUBACK for '{topic}' within 3s");
            }
        }
        finally
        {
            // 成功时读循环已经摘掉了；失败/取消时由这里兜底，避免等待表泄漏。
            _pending.TryRemove(packetId, out _);
        }
    }

    private ushort NextPacketId()
    {
        var next = Interlocked.Increment(ref _packetId);
        return (ushort)((next % 65535) + 1);
    }

    private async Task WritePacketAsync(byte header, byte[] body, CancellationToken ct)
    {
        var buffer = new ArrayBufferWriter<byte>(body.Length + 8);
        buffer.Write(new[] { header });

        var remaining = body.Length;
        var span = buffer.GetSpan(4);
        var count = 0;

        do
        {
            var b = (byte)(remaining % 128);
            remaining /= 128;
            if (remaining > 0) b |= 0x80;
            span[count++] = b;
        } while (remaining > 0);

        buffer.Advance(count);
        buffer.Write(body);

        await _writeLock.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            var stream = _stream ?? throw new InvalidOperationException("mqtt is not connected");
            await stream.WriteAsync(buffer.WrittenMemory, ct).ConfigureAwait(false);
            await stream.FlushAsync(ct).ConfigureAwait(false);

            // 心跳据此判断「链路上还有没有动作」，写失败时不要刷新时间戳。
            Interlocked.Exchange(ref _lastWriteMs, Environment.TickCount64);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    private async Task<(byte Header, byte[] Payload)> ReadPacketAsync(CancellationToken ct)
    {
        var stream = _stream ?? throw new InvalidOperationException("mqtt is not connected");
        var one = new byte[1];
        await stream.ReadExactlyAsync(one, ct).ConfigureAwait(false);

        // 首字节是报文类型与标志位，必须在读「剩余长度」之前留下：
        // 后面几轮把同一个 one 缓冲覆盖掉了，读循环会因此把所有应答都当成未知报文。
        var header = one[0];

        var multiplier = 1;
        var length = 0;
        var guard = 0;

        while (true)
        {
            await stream.ReadExactlyAsync(one, ct).ConfigureAwait(false);
            length += (one[0] & 0x7F) * multiplier;

            if ((one[0] & 0x80) == 0) break;
            multiplier *= 128;

            if (++guard > 4) throw new InvalidOperationException("malformed remaining length");
        }

        var payload = new byte[length];
        if (length > 0) await stream.ReadExactlyAsync(payload, ct).ConfigureAwait(false);

        return (header, payload);
    }

    private void FailPending(string reason)
    {
        foreach (var (id, tcs) in _pending)
        {
            if (_pending.TryRemove(id, out var removed)) removed.TrySetException(new IOException(reason));
        }

        foreach (var (id, tcs) in _subackPending)
        {
            if (_subackPending.TryRemove(id, out var removed)) removed.TrySetException(new IOException(reason));
        }
    }

    /// <summary>
    /// 建立网络流。TLS 在这一层包起来，上层读循环仍按字节流工作——
    /// MQTT 的报文格式不因为 TLS 改变，加密只影响「字节怎么出去」。
    /// </summary>
    private async Task<Stream> OpenStreamAsync(TcpClient client, CancellationToken ct)
    {
        var network = (Stream)client.GetStream();
        if (!_options.Tls.Enabled) return network;

        var ssl = new SslStream(network, leaveInnerStreamOpen: false, ValidateServerCertificate);

        try
        {
            await ssl.AuthenticateAsClientAsync(new SslClientAuthenticationOptions
            {
                TargetHost = _options.Host,
                EnabledSslProtocols = SslProtocols.Tls12 | SslProtocols.Tls13,
                CertificateRevocationCheckMode = X509RevocationMode.NoCheck,
            }, ct).ConfigureAwait(false);

            return ssl;
        }
        catch
        {
            ssl.Dispose();
            throw;
        }
    }

    private bool ValidateServerCertificate(
        object sender,
        X509Certificate? certificate,
        X509Chain? chain,
        SslPolicyErrors errors)
    {
        if (_options.Tls.InsecureSkipVerify) return true;

        if (errors == SslPolicyErrors.None) return true;

        // 自签 Broker：系统信任库当然不认，用配置里的 CA 重建一次链。
        // 主机名校验不清除，避免「链对了但连错了机器」。
        if ((errors & SslPolicyErrors.RemoteCertificateChainErrors) != 0 && certificate is not null)
        {
            _customRoots ??= Tls.LoadCustomRoots(_options.Tls.CaFile, _dataDir);

            if (_customRoots.Count > 0)
            {
                using var leaf = new X509Certificate2(certificate);
                using var custom = new X509Chain();

                custom.ChainPolicy.TrustMode = X509ChainTrustMode.CustomRootTrust;
                custom.ChainPolicy.CustomTrustStore.AddRange(_customRoots);
                custom.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;

                if (custom.Build(leaf)) errors &= ~SslPolicyErrors.RemoteCertificateChainErrors;
            }
        }

        if (errors == SslPolicyErrors.None) return true;

        _log.LogWarning("mqtt TLS handshake rejected the broker certificate: {Errors}", errors);
        return false;
    }

    private async Task CloseAsync()
    {
        Stream? stream;
        TcpClient? client;

        lock (_connLock)
        {
            stream = _stream;
            client = _client;
            _stream = null;
            _client = null;
        }

        if (stream is not null)
        {
            try
            {
                // 尽力发 DISCONNECT，让 Broker 知道我们是正常离开（不会触发 LWT）。
                await _writeLock.WaitAsync(TimeSpan.FromMilliseconds(500)).ConfigureAwait(false);
                try
                {
                    await stream.WriteAsync(new byte[] { PacketDisconnect, 0 }).ConfigureAwait(false);
                    await stream.FlushAsync().ConfigureAwait(false);
                }
                finally
                {
                    _writeLock.Release();
                }
            }
            catch
            {
                // 已经断了，忽略。
            }

            stream.Dispose();
        }

        client?.Dispose();
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        await _cts.CancelAsync().ConfigureAwait(false);

        var loop = _loop;
        if (loop is not null)
        {
            try { await loop.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }

        _inbound.Writer.TryComplete();

        var worker = _inboundWorker;
        if (worker is not null)
        {
            try { await worker.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }

        await CloseAsync().ConfigureAwait(false);

        _cts.Dispose();
        _writeLock.Dispose();
    }

    // ---- 编码辅助 ----

    private static void WriteString(List<byte> target, string value)
    {
        var bytes = Encoding.UTF8.GetBytes(value);
        if (bytes.Length > ushort.MaxValue)
            throw new ArgumentException("mqtt string is too long", nameof(value));

        WriteUInt16(target, (ushort)bytes.Length);
        target.AddRange(bytes);
    }

    private static void WriteUInt16(List<byte> target, ushort value)
    {
        target.Add((byte)(value >> 8));
        target.Add((byte)(value & 0xFF));
    }

    /// <summary>主题段清洗：/、+、#、NUL 一律替换。一个斜杠就能让消息跑到别人的主题上。</summary>
    public static string Segment(string raw)
    {
        var sb = new StringBuilder(raw.Length);
        foreach (var c in raw)
        {
            sb.Append(c is '/' or '+' or '#' or '\0' ? '_' : c);
        }
        return sb.Length == 0 ? "_" : sb.ToString();
    }
}