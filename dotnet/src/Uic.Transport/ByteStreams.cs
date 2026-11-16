using System.IO.Ports;
using System.Net;
using System.Net.Sockets;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Transport;

/// <summary>
/// 字节流抽象。所有读写都必须带超时——「卡住」比「报错」更难排查。
/// 实现不保证并发安全；串行化由调用方负责（一台设备一条链路）。
/// </summary>
public interface IByteStream : IAsyncDisposable
{
    string Remote { get; }
    bool IsOpen { get; }

    Task WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct);
    Task ReadExactlyAsync(Memory<byte> buffer, CancellationToken ct);
    Task<int> ReadAsync(Memory<byte> buffer, CancellationToken ct);

    /// <summary>丢弃接收缓冲中的残留字节（半双工链路上换设备前必须调用）。</summary>
    void DiscardInBuffer();
}

/// <summary>按通道配置创建字节流。</summary>
public static class ByteStreams
{
    public static async Task<IByteStream> OpenAsync(Channel channel, CancellationToken ct)
    {
        switch (channel.Transport)
        {
            case "tcp":
                return await TcpByteStream.ConnectAsync(
                    channel.Host ?? throw new ArgumentException("channel.host is required"),
                    channel.Port,
                    channel.ConnectTimeout,
                    ct).ConfigureAwait(false);

            // UDP 是数据报：收一次就是一条完整报文，没有「先读 4 字节头再读剩下」这回事。
            // 驱动必须自己知道这一点（见各驱动的帧读取），不能把它当成字节流来切。
            case "udp":
                return await UdpByteStream.ConnectAsync(
                    channel.Host ?? throw new ArgumentException("channel.host is required"),
                    channel.Port,
                    channel.ConnectTimeout,
                    ct).ConfigureAwait(false);

            case "serial":
                var s = channel.Serial ?? throw new ArgumentException("channel.serial is required");
                return SerialByteStream.Open(s);

            default:
                throw new NotSupportedException($"transport '{channel.Transport}' is not supported");
        }
    }
}

/// <summary>TCP 字节流。读满或超时是唯一的成功判据——短读即失败，不做猜测。</summary>
public sealed class TcpByteStream : IByteStream
{
    private readonly TcpClient _client;
    private readonly NetworkStream _stream;
    private readonly TimeSpan _timeout;
    private int _closed;

    private TcpByteStream(TcpClient client, NetworkStream stream, TimeSpan timeout, string remote)
    {
        _client = client;
        _stream = stream;
        _timeout = timeout;
        Remote = remote;
    }

    public string Remote { get; }
    public bool IsOpen => Volatile.Read(ref _closed) == 0 && _client.Connected;

    public static async Task<TcpByteStream> ConnectAsync(string host, int port, TimeSpan timeout, CancellationToken ct)
    {
        var client = new TcpClient { NoDelay = true };
        try
        {
            using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            cts.CancelAfter(timeout);
            await client.ConnectAsync(host, port, cts.Token).ConfigureAwait(false);
            var stream = client.GetStream();
            return new TcpByteStream(client, stream, timeout, $"{host}:{port}");
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            client.Dispose();
            throw DriverException.Timeout("tcp", "connect",
                $"connect to {host}:{port} timed out after {timeout.TotalMilliseconds:0}ms");
        }
        catch (SocketException ex)
        {
            client.Dispose();
            throw DriverException.Connection("tcp", "connect", $"connect to {host}:{port} failed: {ex.Message}", ex);
        }
    }

    public async Task WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct)
    {
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        cts.CancelAfter(_timeout);
        try
        {
            await _stream.WriteAsync(data, cts.Token).ConfigureAwait(false);
            await _stream.FlushAsync(cts.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            throw DriverException.Timeout("tcp", "write", $"write to {Remote} timed out");
        }
        catch (IOException ex)
        {
            throw DriverException.Connection("tcp", "write", ex.Message, ex);
        }
        catch (SocketException ex)
        {
            throw DriverException.Connection("tcp", "write", ex.Message, ex);
        }
    }

    public async Task ReadExactlyAsync(Memory<byte> buffer, CancellationToken ct)
    {
        var read = 0;
        while (read < buffer.Length)
        {
            var n = await ReadAsync(buffer[read..], ct).ConfigureAwait(false);
            if (n == 0)
                throw DriverException.Connection("tcp", "read",
                    $"connection closed by {Remote} after {read}/{buffer.Length} bytes");
            read += n;
        }
    }

    public async Task<int> ReadAsync(Memory<byte> buffer, CancellationToken ct)
    {
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        cts.CancelAfter(_timeout);
        try
        {
            return await _stream.ReadAsync(buffer, cts.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            throw DriverException.Timeout("tcp", "read",
                $"read from {Remote} timed out after {_timeout.TotalMilliseconds:0}ms");
        }
        catch (IOException ex)
        {
            throw DriverException.Connection("tcp", "read", ex.Message, ex);
        }
        catch (SocketException ex)
        {
            throw DriverException.Connection("tcp", "read", ex.Message, ex);
        }
    }

    public void DiscardInBuffer()
    {
        try
        {
            while (_stream.DataAvailable)
            {
                var buf = new byte[256];
                if (_stream.Read(buf, 0, buf.Length) <= 0) break;
            }
        }
        catch (IOException)
        {
            // 丢弃残留失败不应影响主流程，下一次读写会暴露真实错误。
        }
    }

    public ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _closed, 1) != 0) return ValueTask.CompletedTask;
        _stream.Dispose();
        _client.Dispose();
        return ValueTask.CompletedTask;
    }
}

/// <summary>
/// 串口字节流。多个设备共享同一物理串口时，由驱动层的端口表负责复用与事务锁，
/// 这一层只做字节搬运。
/// </summary>
public sealed class SerialByteStream : IByteStream
{
    private readonly SerialPort _port;
    private readonly TimeSpan _timeout;
    private int _closed;

    private SerialByteStream(SerialPort port, TimeSpan timeout, string remote)
    {
        _port = port;
        _timeout = timeout;
        Remote = remote;
    }

    public string Remote { get; }
    public bool IsOpen => Volatile.Read(ref _closed) == 0 && _port.IsOpen;

    public static SerialByteStream Open(SerialOptions options)
    {
        var parity = options.Parity switch
        {
            "odd" => Parity.Odd,
            "even" => Parity.Even,
            "mark" => Parity.Mark,
            "space" => Parity.Space,
            _ => Parity.None,
        };

        var stopBits = options.StopBits switch
        {
            "onePointFive" => StopBits.OnePointFive,
            "two" => StopBits.Two,
            _ => StopBits.One,
        };

        var port = new SerialPort(options.Port, options.Baud, parity, options.DataBits, stopBits)
        {
            ReadTimeout = options.ReadTimeoutMs,
            WriteTimeout = options.ReadTimeoutMs,
        };

        try
        {
            port.Open();
            return new SerialByteStream(port, TimeSpan.FromMilliseconds(options.ReadTimeoutMs), options.Port);
        }
        catch (Exception ex)
        {
            port.Dispose();
            throw DriverException.Connection("serial", "open", $"open {options.Port} failed: {ex.Message}", ex);
        }
    }

    public async Task WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct)
    {
        try
        {
            await _port.BaseStream.WriteAsync(data, ct).ConfigureAwait(false);
            await _port.BaseStream.FlushAsync(ct).ConfigureAwait(false);
        }
        catch (OperationCanceledException)
        {
            throw;
        }
        catch (Exception ex)
        {
            throw DriverException.Connection("serial", "write", ex.Message, ex);
        }
    }

    public async Task ReadExactlyAsync(Memory<byte> buffer, CancellationToken ct)
    {
        var read = 0;
        while (read < buffer.Length)
        {
            var n = await ReadAsync(buffer[read..], ct).ConfigureAwait(false);
            if (n == 0)
                throw DriverException.Timeout("serial", "read", $"read from {Remote} returned 0 bytes");
            read += n;
        }
    }

    public async Task<int> ReadAsync(Memory<byte> buffer, CancellationToken ct)
    {
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        cts.CancelAfter(_timeout);
        try
        {
            return await _port.BaseStream.ReadAsync(buffer, cts.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            throw DriverException.Timeout("serial", "read",
                $"read from {Remote} timed out after {_timeout.TotalMilliseconds:0}ms");
        }
        catch (Exception ex)
        {
            throw DriverException.Connection("serial", "read", ex.Message, ex);
        }
    }

    public void DiscardInBuffer()
    {
        try
        {
            _port.DiscardInBuffer();
        }
        catch (Exception)
        {
            // 同 TcpByteStream：丢弃残留失败不改变主流程语义。
        }
    }

    public ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _closed, 1) != 0) return ValueTask.CompletedTask;
        try
        {
            if (_port.IsOpen) _port.Close();
        }
        finally
        {
            _port.Dispose();
        }
        return ValueTask.CompletedTask;
    }
}

/// <summary>
/// UDP 字节流（数据报）。
///
/// 与 TCP 的关键差别：<see cref="ReadAsync"/> 一次返回**一条完整数据报**的长度，
/// 缓冲区小于数据报时多余字节会被内核丢弃——所以调用方必须给足缓冲区（BACnet/IP
/// 上限 65535）。<see cref="ReadExactlyAsync"/> 只接受「数据报长度正好等于缓冲区」，
/// 不做拼接：把两条数据报拼成一条，得到的是一个不存在的帧。
/// </summary>
public sealed class UdpByteStream : IByteStream
{
    /// <summary>单条 UDP 数据报的理论上限。驱动按这个尺寸收，避免截断。</summary>
    public const int MaxDatagram = 65535;

    private readonly Socket _socket;
    private readonly TimeSpan _timeout;
    private int _closed;

    private UdpByteStream(Socket socket, TimeSpan timeout, string remote)
    {
        _socket = socket;
        _timeout = timeout;
        Remote = remote;
    }

    public string Remote { get; }

    public bool IsOpen => Volatile.Read(ref _closed) == 0;

    public static async Task<UdpByteStream> ConnectAsync(string host, int port, TimeSpan timeout, CancellationToken ct)
    {
        IPAddress address;
        try
        {
            var addresses = await Dns.GetHostAddressesAsync(host, ct).ConfigureAwait(false);
            // 优先 IPv4：现场绝大多数 BACnet 设备只监听 IPv4。
            address = addresses.FirstOrDefault(a => a.AddressFamily == AddressFamily.InterNetwork)
                      ?? addresses.FirstOrDefault()
                      ?? throw DriverException.Connection("udp", "connect", $"host '{host}' resolved to no address");
        }
        catch (SocketException ex)
        {
            throw DriverException.Connection("udp", "connect", $"cannot resolve '{host}': {ex.Message}", ex);
        }

        var endpoint = new IPEndPoint(address, port);
        var socket = new Socket(address.AddressFamily, SocketType.Dgram, ProtocolType.Udp);

        try
        {
            // connect 一个 UDP 套接字只设置默认对端并过滤来源地址：不是「建立连接」，
            // 但能让「别的设备发来的报文」不混进这条链路。
            socket.Connect(endpoint);
            return new UdpByteStream(socket, timeout, $"{address}:{port}");
        }
        catch (SocketException ex)
        {
            socket.Dispose();
            throw DriverException.Connection("udp", "connect",
                $"cannot bind {address}:{port}: {ex.Message}", ex);
        }
    }

    public async Task WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct)
    {
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        cts.CancelAfter(_timeout);
        try
        {
            var sent = await _socket.SendAsync(data, SocketFlags.None, cts.Token).ConfigureAwait(false);
            if (sent != data.Length)
                throw DriverException.ProtocolError("udp", "write",
                    $"sent {sent} of {data.Length} bytes in one datagram");
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            throw DriverException.Timeout("udp", "write", $"send to {Remote} timed out");
        }
        catch (SocketException ex)
        {
            throw DriverException.Connection("udp", "write", ex.Message, ex);
        }
    }

    public async Task<int> ReadAsync(Memory<byte> buffer, CancellationToken ct)
    {
        // 不在流层自设超时：BACnet 驱动的每条请求都有自己的时延预算（见
        // BacnetConn 的 deadline 循环），由调用方通过取消令牌叫停这一读。
        // 若在这里再挂一个 socket 级超时，它会抢先抛出一条泛化的
        // 「no datagram within Nms」，盖掉驱动按错误类/invoke-id 配好的诊断消息，
        // 也会把「点表写错（unknown-object 应落 address_error）」误报成超时。
        try
        {
            return await _socket.ReceiveAsync(buffer, SocketFlags.None, ct).ConfigureAwait(false);
        }
        catch (SocketException ex)
        {
            throw DriverException.Connection("udp", "read", ex.Message, ex);
        }
    }

    public async Task ReadExactlyAsync(Memory<byte> buffer, CancellationToken ct)
    {
        var received = await ReadAsync(buffer, ct).ConfigureAwait(false);
        if (received != buffer.Length)
            throw DriverException.ProtocolError("udp", "read",
                $"datagram carried {received} byte(s) but {buffer.Length} were expected; " +
                "UDP datagrams are read whole, so this response cannot be completed from the stream");
    }

    public void DiscardInBuffer()
    {
        try
        {
            // UDP 没有「缓冲区」，只有内核队列里已到达的数据报。逐个取空即可。
            var scratch = new byte[MaxDatagram];
            while (_socket.Available > 0)
            {
                if (_socket.Receive(scratch) <= 0) break;
            }
        }
        catch (SocketException)
        {
            // 与 TCP 同语义：丢弃失败不影响主流程，下一次读会暴露真实错误。
        }
    }

    public ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _closed, 1) != 0) return ValueTask.CompletedTask;
        _socket.Dispose();
        return ValueTask.CompletedTask;
    }
}

/// <summary>内存流。用于金报文测试与虚拟链路，不依赖真实网络或串口。</summary>
public sealed class LoopbackStream : IByteStream
{
    private readonly Queue<byte> _incoming = new();
    private readonly Func<ReadOnlyMemory<byte>, IEnumerable<byte>>? _responder;
    private readonly object _lock = new();

    public LoopbackStream(Func<ReadOnlyMemory<byte>, IEnumerable<byte>>? responder = null) => _responder = responder;

    public string Remote => "loopback";
    public bool IsOpen { get; private set; } = true;
    public List<byte[]> Writes { get; } = new();

    public void Enqueue(ReadOnlySpan<byte> data)
    {
        lock (_lock)
        {
            foreach (var b in data) _incoming.Enqueue(b);
        }
    }

    public Task WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct)
    {
        lock (_lock)
        {
            Writes.Add(data.ToArray());
            if (_responder is not null)
            {
                foreach (var b in _responder(data)) _incoming.Enqueue(b);
            }
        }
        return Task.CompletedTask;
    }

    public Task ReadExactlyAsync(Memory<byte> buffer, CancellationToken ct)
    {
        lock (_lock)
        {
            if (_incoming.Count < buffer.Length)
                throw DriverException.Timeout("loopback", "read",
                    $"need {buffer.Length} bytes, have {_incoming.Count}");
            for (var i = 0; i < buffer.Length; i++) buffer.Span[i] = _incoming.Dequeue();
        }
        return Task.CompletedTask;
    }

    public Task<int> ReadAsync(Memory<byte> buffer, CancellationToken ct)
    {
        lock (_lock)
        {
            var n = Math.Min(buffer.Length, _incoming.Count);
            for (var i = 0; i < n; i++) buffer.Span[i] = _incoming.Dequeue();
            return Task.FromResult(n);
        }
    }

    public void DiscardInBuffer()
    {
        lock (_lock) _incoming.Clear();
    }

    public ValueTask DisposeAsync()
    {
        IsOpen = false;
        return ValueTask.CompletedTask;
    }
}
