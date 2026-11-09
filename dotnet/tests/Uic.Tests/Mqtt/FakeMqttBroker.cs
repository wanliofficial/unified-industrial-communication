using System.Net;
using System.Net.Security;
using System.Net.Sockets;
using System.Security.Authentication;
using System.Security.Cryptography.X509Certificates;
using System.Text;

namespace Uic.Tests;

/// <summary>
/// 一个只在测试里用的最小 MQTT 3.1.1 Broker。
///
/// 它刻意不复用客户端任何代码：拆包、拼包、字段顺序全部按协议规范独立实现，
/// 这样才能把「客户端编码错了」和「测试也照着错」区分开。
///
/// 覆盖 CONNECT / SUBSCRIBE / SUBACK / PUBLISH(QoS0/1) / PUBACK /
/// PINGREQ / PINGRESP / DISCONNECT；传入证书时整条连接走 TLS
/// （握手在测试里完成，报文层与明文路径完全相同）。不支持持久会话。
/// </summary>
internal sealed class FakeMqttBroker : IAsyncDisposable
{
    public sealed record ConnectInfo(
        string Protocol,
        byte Level,
        byte Flags,
        ushort KeepAliveSeconds,
        string ClientId,
        string? WillTopic,
        string? WillPayload,
        string? Username,
        string? Password);

    public sealed record PublishInfo(string Topic, string Payload, int Qos, bool Retain, ushort PacketId);

    public sealed record SubscriptionInfo(string Topic, int Qos, ushort PacketId);

    private sealed class Connection
    {
        public Connection(Stream stream) => Stream = stream;

        public Stream Stream { get; }
        public SemaphoreSlim WriteLock { get; } = new(1, 1);
    }

    private readonly X509Certificate2? _certificate;
    private readonly TcpListener _listener;
    private readonly CancellationTokenSource _cts = new();
    private readonly Task _accept;
    private readonly List<ConnectInfo> _connects = new();
    private readonly List<PublishInfo> _published = new();
    private readonly List<SubscriptionInfo> _subscriptions = new();
    private readonly List<ushort> _pubacks = new();
    private readonly object _lock = new();
    private readonly List<TcpClient> _clients = new();

    private Connection? _connection;
    private int _droppedOnce;
    private int _pings;
    private int _nextPacketId;

    public FakeMqttBroker(X509Certificate2? certificate = null)
    {
        _certificate = certificate;
        _listener = new TcpListener(IPAddress.Loopback, 0);
        _listener.Start();
        Port = ((IPEndPoint)_listener.LocalEndpoint).Port;
        _accept = Task.Run(() => AcceptAsync(_cts.Token));
    }

    public int Port { get; }

    /// <summary>CONNACK 返回码。0 = 接受，其它值用来验证「Broker 拒绝连接」这条路径。</summary>
    public byte ConnackCode { get; init; }

    /// <summary>延迟多久回 PUBACK。用来证明 QoS1 的发布确实在等确认，而不是发完就走。</summary>
    public TimeSpan PubackDelay { get; init; }

    /// <summary>收到第一条 PUBLISH 后断开连接，模拟 Broker 重启。</summary>
    public bool DropAfterFirstPublish { get; init; }

    /// <summary>连接后是否发一条无关主题的消息。</summary>
    public bool StrayPublishOnConnect { get; init; }

    /// <summary>SUBACK 里对所有主题回 0x80，模拟 Broker 的 ACL 拒绝订阅。</summary>
    public bool RefuseSubscription { get; init; }

    public IReadOnlyList<ConnectInfo> Connects
    {
        get { lock (_lock) return _connects.ToArray(); }
    }

    public IReadOnlyList<PublishInfo> Published
    {
        get { lock (_lock) return _published.ToArray(); }
    }

    public IReadOnlyList<SubscriptionInfo> Subscriptions
    {
        get { lock (_lock) return _subscriptions.ToArray(); }
    }

    /// <summary>客户端回给 broker 的 PUBACK（入站 QoS1 消息的确认）。</summary>
    public IReadOnlyList<ushort> Pubacks
    {
        get { lock (_lock) return _pubacks.ToArray(); }
    }

    public bool ClientSentDisconnect { get; private set; }

    /// <summary>收到的 PINGREQ 次数。客户端声明了 keepalive 就必须真的发心跳。</summary>
    public int PingCount => Volatile.Read(ref _pings);

    private async Task AcceptAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested)
        {
            TcpClient client;
            try
            {
                client = await _listener.AcceptTcpClientAsync(ct).ConfigureAwait(false);
            }
            catch
            {
                return;
            }

            lock (_lock) _clients.Add(client);

            // 每条连接独立处理：客户端重连时会有第二条连接。
            _ = Task.Run(() => HandleAsync(client, ct), ct);
        }
    }

    private async Task HandleAsync(TcpClient client, CancellationToken ct)
    {
        Connection? conn = null;

        try
        {
            Stream stream = client.GetStream();

            if (_certificate is not null)
            {
                var ssl = new SslStream(stream, leaveInnerStreamOpen: false);
                await ssl.AuthenticateAsServerAsync(new SslServerAuthenticationOptions
                {
                    ServerCertificate = _certificate,
                    EnabledSslProtocols = SslProtocols.Tls12 | SslProtocols.Tls13,
                    ClientCertificateRequired = false,
                }, ct).ConfigureAwait(false);

                stream = ssl;
            }

            await using var owned = stream;
            conn = new Connection(stream);

            var (header, payload) = await ReadPacketAsync(stream, ct).ConfigureAwait(false);
            if ((header & 0xF0) != 0x10) return;

            var connect = ParseConnect(payload);
            lock (_lock) _connects.Add(connect);

            await SendAsync(conn, 0x20, new byte[] { 0x00, ConnackCode }, ct).ConfigureAwait(false);

            if (ConnackCode != 0) return;

            // 新连接成为「当前连接」，测试通过它向客户端下发命令。
            lock (_lock) _connection = conn;

            if (StrayPublishOnConnect)
            {
                // 一条 QoS1 的下行消息：没有登记订阅时客户端必须忽略而不是当成 PUBACK。
                await SendAsync(conn, 0x32,
                    Concat(StringBytes("uic/other/x/value"), new byte[] { 0x00, 0x07 }, Encoding.UTF8.GetBytes("{}")),
                    ct).ConfigureAwait(false);
            }

            while (!ct.IsCancellationRequested)
            {
                var (h, body) = await ReadPacketAsync(stream, ct).ConfigureAwait(false);

                switch (h & 0xF0)
                {
                    case 0x80: // SUBSCRIBE
                    {
                        if (body.Length < 2) return;
                        var packetId = (ushort)((body[0] << 8) | body[1]);
                        var offset = 2;

                        var granted = new List<byte>();
                        var requested = new List<SubscriptionInfo>();

                        while (offset < body.Length)
                        {
                            var topic = ReadString(body, ref offset);
                            if (offset >= body.Length) return;
                            var qos = body[offset++];

                            // 客户端只发 QoS0/1；收到 2 说明编码有误。
                            if (qos > 1) return;

                            requested.Add(new SubscriptionInfo(topic, qos, packetId));
                            granted.Add(RefuseSubscription ? (byte)0x80 : (byte)qos);
                        }

                        lock (_lock) _subscriptions.AddRange(requested);

                        await SendAsync(conn, 0x90,
                            Concat(new[] { (byte)(packetId >> 8), (byte)packetId }, granted.ToArray()),
                            ct).ConfigureAwait(false);
                        break;
                    }

                    case 0x40: // 客户端对入站 QoS1 的 PUBACK
                    {
                        if (body.Length < 2) return;
                        var packetId = (ushort)((body[0] << 8) | body[1]);
                        lock (_lock) _pubacks.Add(packetId);
                        break;
                    }

                    case 0x30:
                    {
                        var info = ParsePublish(h, body);
                        lock (_lock) _published.Add(info);

                        if (info.Qos == 1)
                        {
                            if (PubackDelay > TimeSpan.Zero)
                                await Task.Delay(PubackDelay, ct).ConfigureAwait(false);

                            await SendAsync(conn, 0x40,
                                new[] { (byte)(info.PacketId >> 8), (byte)(info.PacketId & 0xFF) }, ct)
                                .ConfigureAwait(false);
                        }

                        if (DropAfterFirstPublish && Interlocked.Exchange(ref _droppedOnce, 1) == 0)
                            return; // 直接断链，模拟 Broker 重启

                        break;
                    }

                    case 0xC0:
                        Interlocked.Increment(ref _pings);
                        await SendAsync(conn, 0xD0, Array.Empty<byte>(), ct).ConfigureAwait(false);
                        break;

                    case 0xE0:
                        ClientSentDisconnect = true;
                        return;

                    default:
                        return;
                }
            }
        }
        catch
        {
            // 连接被测试或被客户端关掉，属于正常收尾。
        }
        finally
        {
            lock (_lock)
            {
                if (ReferenceEquals(_connection, conn)) _connection = null;
            }
        }
    }

    /// <summary>
    /// 向客户端下发一条消息（模拟操作员发布命令）。
    /// 返回 QoS1 使用的 packet id，测试据此断言客户端回了 PUBACK。
    /// </summary>
    public async Task<ushort> PublishToClientAsync(
        string topic, string payload, int qos = 1, CancellationToken ct = default)
    {
        Connection conn;
        lock (_lock) conn = _connection ?? throw new InvalidOperationException("no client is connected");

        var packetId = qos > 0 ? (ushort)((Interlocked.Increment(ref _nextPacketId) % 65535) + 1) : (ushort)0;

        var body = qos > 0
            ? Concat(StringBytes(topic),
                new[] { (byte)(packetId >> 8), (byte)packetId },
                Encoding.UTF8.GetBytes(payload))
            : Concat(StringBytes(topic), Encoding.UTF8.GetBytes(payload));

        await SendAsync(conn, (byte)(0x30 | (qos << 1)), body, ct).ConfigureAwait(false);
        return packetId;
    }

    private static ConnectInfo ParseConnect(byte[] payload)
    {
        var offset = 0;

        var protocol = ReadString(payload, ref offset);
        var level = payload[offset++];
        var flags = payload[offset++];
        var keepAlive = (ushort)((payload[offset] << 8) | payload[offset + 1]);
        offset += 2;

        var clientId = ReadString(payload, ref offset);

        string? willTopic = null;
        string? willPayload = null;
        if ((flags & 0x04) != 0)
        {
            willTopic = ReadString(payload, ref offset);

            var len = (payload[offset] << 8) | payload[offset + 1];
            offset += 2;
            willPayload = Encoding.UTF8.GetString(payload, offset, len);
            offset += len;
        }

        string? username = null;
        string? password = null;
        if ((flags & 0x80) != 0) username = ReadString(payload, ref offset);
        if ((flags & 0x40) != 0) password = ReadString(payload, ref offset);

        return new ConnectInfo(protocol, level, flags, keepAlive, clientId, willTopic, willPayload, username, password);
    }

    private static PublishInfo ParsePublish(byte header, byte[] body)
    {
        var qos = (header >> 1) & 0x03;
        var retain = (header & 0x01) != 0;

        var offset = 0;
        var topic = ReadString(body, ref offset);

        ushort packetId = 0;
        if (qos > 0)
        {
            packetId = (ushort)((body[offset] << 8) | body[offset + 1]);
            offset += 2;
        }

        var payload = Encoding.UTF8.GetString(body, offset, body.Length - offset);
        return new PublishInfo(topic, payload, qos, retain, packetId);
    }

    private static string ReadString(byte[] buffer, ref int offset)
    {
        var len = (buffer[offset] << 8) | buffer[offset + 1];
        offset += 2;
        var value = Encoding.UTF8.GetString(buffer, offset, len);
        offset += len;
        return value;
    }

    private static byte[] StringBytes(string value)
    {
        var bytes = Encoding.UTF8.GetBytes(value);
        return Concat(new[] { (byte)(bytes.Length >> 8), (byte)bytes.Length }, bytes);
    }

    private static byte[] Concat(params byte[][] parts)
    {
        var total = parts.Sum(p => p.Length);
        var all = new byte[total];
        var offset = 0;
        foreach (var part in parts)
        {
            part.CopyTo(all, offset);
            offset += part.Length;
        }

        return all;
    }

    private static async Task<(byte Header, byte[] Payload)> ReadPacketAsync(Stream stream, CancellationToken ct)
    {
        var one = new byte[1];
        await stream.ReadExactlyAsync(one, ct).ConfigureAwait(false);
        var header = one[0];

        var multiplier = 1;
        var length = 0;
        for (var i = 0; ; i++)
        {
            if (i > 3) throw new InvalidDataException("malformed remaining length");
            await stream.ReadExactlyAsync(one, ct).ConfigureAwait(false);
            length += (one[0] & 0x7F) * multiplier;
            if ((one[0] & 0x80) == 0) break;
            multiplier *= 128;
        }

        var payload = new byte[length];
        if (length > 0) await stream.ReadExactlyAsync(payload, ct).ConfigureAwait(false);

        return (header, payload);
    }

    private static async Task WritePacketAsync(Stream stream, byte header, byte[] body, CancellationToken ct)
    {
        var packet = new List<byte>(body.Length + 5) { header };

        var remaining = body.Length;
        do
        {
            var b = (byte)(remaining % 128);
            remaining /= 128;
            if (remaining > 0) b |= 0x80;
            packet.Add(b);
        } while (remaining > 0);

        packet.AddRange(body);

        await stream.WriteAsync(packet.ToArray(), ct).ConfigureAwait(false);
        await stream.FlushAsync(ct).ConfigureAwait(false);
    }

    /// <summary>所有写都串行化：下行命令与 ACK 可能同时发生，字节交错会直接毁掉报文。</summary>
    private static async Task SendAsync(Connection conn, byte header, byte[] body, CancellationToken ct)
    {
        await conn.WriteLock.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            await WritePacketAsync(conn.Stream, header, body, ct).ConfigureAwait(false);
        }
        finally
        {
            conn.WriteLock.Release();
        }
    }

    /// <summary>主动断开所有连接，用来模拟「Broker 把客户端踢下线」。</summary>
    public void DropConnections()
    {
        lock (_lock)
        {
            foreach (var client in _clients)
            {
                try { client.Close(); } catch { /* 收尾期忽略 */ }
            }

            _clients.Clear();
            _connection = null;
        }
    }

    public async ValueTask DisposeAsync()
    {
        await _cts.CancelAsync().ConfigureAwait(false);

        try { _listener.Stop(); } catch { /* 收尾期忽略 */ }

        DropConnections();

        try { await _accept.ConfigureAwait(false); } catch { /* 收尾期忽略 */ }

        _cts.Dispose();
    }
}
