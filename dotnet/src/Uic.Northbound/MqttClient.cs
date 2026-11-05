using System.Buffers;
using System.Collections.Concurrent;
using System.Net.Sockets;
using System.Text;
using Microsoft.Extensions.Logging;
using Uic.Domain;

namespace Uic.Northbound;

/// <summary>
/// 最小 MQTT 3.1.1 客户端（仅发布侧）。
///
/// 为什么自己写：边缘部署要求单文件、无额外依赖，且我们只需要
/// CONNECT / PUBLISH(QoS0/1) / PINGREQ / DISCONNECT 五个报文。
/// 引入完整 MQTT 库会带来订阅、TLS、WebSocket 等我们用不到的路径，
/// 那些路径一旦被配置误开，反而变成「以为加密了其实没有」的风险点。
///
/// 明确不支持：QoS2、TLS、WebSocket、订阅、持久会话。配置校验阶段直接拒绝。
/// </summary>
public sealed class MqttClient : IAsyncDisposable
{
    private const byte PacketConnect = 0x10;
    private const byte PacketConnack = 0x20;
    private const byte PacketPublish = 0x30;
    private const byte PacketPuback = 0x40;
    private const byte PacketPingreq = 0xC0;
    private const byte PacketPingresp = 0xD0;
    private const byte PacketDisconnect = 0xE0;

    private readonly MqttOptions _options;
    private readonly string _site;
    private readonly ILogger _log;
    private readonly CancellationTokenSource _cts = new();
    private readonly SemaphoreSlim _writeLock = new(1, 1);
    private readonly ConcurrentDictionary<ushort, TaskCompletionSource<bool>> _pending = new();
    private readonly object _connLock = new();

    private TcpClient? _client;
    private NetworkStream? _stream;
    private Task? _loop;
    private int _packetId;
    private volatile bool _connected;
    private int _disposed;

    public MqttClient(MqttOptions options, string site, ILogger log)
    {
        _options = options;
        _site = site;
        _log = log;
    }

    public bool IsConnected => _connected;

    public string WillTopic => $"{_options.TopicPrefix}/{Segment(_site)}/$death";

    /// <summary>连接成功后的回调：重连时要补发 $birth 与全部 $state。</summary>
    public Func<MqttClient, CancellationToken, Task>? OnConnected { get; set; }

    public void Start()
    {
        if (_disposed != 0) return;
        _loop ??= Task.Run(() => RunAsync(_cts.Token));
    }

    private async Task RunAsync(CancellationToken ct)
    {
        var delay = TimeSpan.FromMilliseconds(Math.Max(500, _options.ReconnectDelayMs));

        while (!ct.IsCancellationRequested)
        {
            try
            {
                await ConnectAsync(ct).ConfigureAwait(false);

                _connected = true;
                _log.LogInformation("mqtt connected to {Host}:{Port}", _options.Host, _options.Port);

                if (OnConnected is not null)
                {
                    try { await OnConnected(this, ct).ConfigureAwait(false); }
                    catch (Exception ex) { _log.LogWarning(ex, "mqtt on-connected hook failed"); }
                }

                delay = TimeSpan.FromMilliseconds(Math.Max(500, _options.ReconnectDelayMs));
                await ReadLoopAsync(ct).ConfigureAwait(false);
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

        using (var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct))
        {
            timeout.CancelAfter(TimeSpan.FromSeconds(5));
            await client.ConnectAsync(_options.Host, _options.Port, timeout.Token).ConfigureAwait(false);
        }

        var stream = client.GetStream();

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

    private async Task ReadLoopAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested)
        {
            var (header, payload) = await ReadPacketAsync(ct).ConfigureAwait(false);

            switch (header & 0xF0)
            {
                case PacketPuback when payload.Length >= 2:
                {
                    var id = (ushort)((payload[0] << 8) | payload[1]);
                    if (_pending.TryRemove(id, out var tcs)) tcs.TrySetResult(true);
                    break;
                }

                case PacketPingresp:
                    break;

                case PacketPublish:
                    // 我们不订阅，收到 PUBLISH 说明 Broker 配置有问题；忽略但记录。
                    _log.LogDebug("ignoring inbound PUBLISH (subscriptions are not supported)");
                    break;

                default:
                    _log.LogDebug("ignoring mqtt packet 0x{Header:x2}", header);
                    break;
            }
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
        await WritePacketAsync(header, body.ToArray(), ct).ConfigureAwait(false);

        if (qos == 0) return;

        var tcs = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        _pending[packetId] = tcs;

        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
        timeout.CancelAfter(TimeSpan.FromSeconds(3));

        try
        {
            await tcs.Task.WaitAsync(timeout.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            _pending.TryRemove(packetId, out _);
            throw new TimeoutException($"no PUBACK for '{topic}' within 3s");
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

        return (one[0], payload);
    }

    private void FailPending(string reason)
    {
        foreach (var (id, tcs) in _pending)
        {
            if (_pending.TryRemove(id, out var removed)) removed.TrySetException(new IOException(reason));
        }
    }

    private async Task CloseAsync()
    {
        NetworkStream? stream;
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