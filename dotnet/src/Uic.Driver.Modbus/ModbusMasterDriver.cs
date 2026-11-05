using System.Buffers.Binary;
using System.Text;
using Uic.Codec.Modbus;
using Uic.Domain;
using Uic.Driver;
using Uic.Transport;

namespace Uic.Driver.Modbus;

public enum ModbusLink
{
    Tcp,
    Rtu,
    Ascii,
}

/// <summary>
/// Modbus 主站驱动。三种链路共用同一份 PDU 编解码，只在分帧层不同——
/// 这样异常码分类、部分失败保留好样本、批量写逐项结果等语义不会分叉。
/// </summary>
public sealed class ModbusMasterDriver : IDriver
{
    private static int _txCounter;

    private readonly Func<OpenContext, IModbusFraming> _framingFactory;

    private ModbusMasterDriver(DriverMeta meta, Func<OpenContext, IModbusFraming> framingFactory)
    {
        Meta = meta;
        _framingFactory = framingFactory;
    }

    public DriverMeta Meta { get; }

    public static ModbusMasterDriver Tcp() => new(
        new DriverMeta
        {
            Id = "modbus-tcp",
            Name = "Modbus TCP",
            Version = "1.0.0",
            Roles = new[] { DriverRole.Master },
            Transports = new[] { "tcp" },
            AddressKinds = new[] { AddressKind.Register },
            Capabilities = new Capabilities(
                Read: CapState.Supported,
                Write: CapState.Supported,
                MultiRead: CapState.Supported,
                BitAccess: CapState.Supported),
            Description = "Modbus TCP 主站：FC1/2/3/4/5/6/15/16，位访问与四种字序。",
            ConfigFields = new[]
            {
                new ConfigField("transport.host", "主机", "string", Required: true,
                    Placeholder: "192.168.1.10", Scope: "channel"),
                new ConfigField("transport.port", "端口", "int", Required: true, Default: 502, Scope: "channel"),
            },
        },
        ctx => new TcpFraming(ctx));

    public static ModbusMasterDriver Rtu() => new(
        new DriverMeta
        {
            Id = "modbus-rtu",
            Name = "Modbus RTU",
            Version = "1.0.0",
            Roles = new[] { DriverRole.Master },
            Transports = new[] { "serial" },
            AddressKinds = new[] { AddressKind.Register },
            Capabilities = new Capabilities(
                Read: CapState.Supported,
                Write: CapState.Supported,
                MultiRead: CapState.Supported,
                BitAccess: CapState.Supported),
            Description = "Modbus RTU 主站：CRC 校验、多设备共享同一串口、半双工事务串行。",
            ConfigFields = new[]
            {
                new ConfigField("transport.port", "串口", "string", Required: true,
                    Placeholder: "/dev/ttyUSB0", Scope: "channel"),
                new ConfigField("transport.baud", "波特率", "int", Default: 9600, Scope: "channel"),
            },
        },
        ctx => new RtuFraming(ctx));

    public static ModbusMasterDriver Ascii() => new(
        new DriverMeta
        {
            Id = "modbus-ascii",
            Name = "Modbus ASCII",
            Version = "1.0.0",
            Roles = new[] { DriverRole.Master },
            Transports = new[] { "serial" },
            AddressKinds = new[] { AddressKind.Register },
            Capabilities = new Capabilities(
                Read: CapState.Supported,
                Write: CapState.Supported,
                MultiRead: CapState.Supported,
                BitAccess: CapState.Supported),
            Description = "Modbus ASCII 主站：LRC 校验、冒号/CRLF 边界、可读报文。",
            ConfigFields = new[]
            {
                new ConfigField("transport.port", "串口", "string", Required: true, Scope: "channel"),
                new ConfigField("transport.baud", "波特率", "int", Default: 9600, Scope: "channel"),
            },
        },
        ctx => new AsciiFraming(ctx));

    public IConn Open(OpenContext context) => new ModbusConn(context, _framingFactory(context));

    internal static ushort NextTransaction() => (ushort)Interlocked.Increment(ref _txCounter);

    private sealed class ModbusConn : IConn
    {
        private readonly OpenContext _ctx;
        private readonly IModbusFraming _framing;
        private readonly ModbusCodec _codec;
        private bool _disposed;

        public ModbusConn(OpenContext ctx, IModbusFraming framing)
        {
            _ctx = ctx;
            _framing = framing;
            _codec = new ModbusCodec(ctx.Device.Id);
        }

        public async Task ProbeAsync(CancellationToken ct)
        {
            // 探活读一个保持寄存器：读成功即链路与会话都可用。
            var point = new PointRef(
                "_probe",
                Address.Register("holding", 0, 1),
                DataType.UInt16,
                _ctx.Device.ByteOrder,
                1);

            var pdu = _codec.EncodeRead(new[] { point });
            var response = await _framing.RoundTripAsync(pdu, ct).ConfigureAwait(false);
            var samples = _codec.DecodeRead(new[] { point }, response);

            if (samples.Count != 1 || samples[0].Quality != Quality.Good)
                throw DriverException.ProtocolError(_ctx.Channel.Protocol, "probe",
                    "probe read returned no good sample");
        }

        public async Task<IReadOnlyList<RawSample>> ReadAsync(IReadOnlyList<ReadSpan> spans, CancellationToken ct)
        {
            var all = new List<RawSample>();

            foreach (var span in spans)
            {
                try
                {
                    var pdu = _codec.EncodeRead(span.Points);
                    var response = await _framing.RoundTripAsync(pdu, ct).ConfigureAwait(false);
                    all.AddRange(_codec.DecodeRead(span.Points, response));
                }
                catch (OperationCanceledException) when (ct.IsCancellationRequested)
                {
                    throw;
                }
                catch (DriverException ex)
                {
                    // 单个窗口失败不拖垮整轮：其余窗口照常返回。
                    all.AddRange(span.Points.Select(p =>
                        new RawSample(p.Id, null, QualityMap.From(ex.Kind), null, ex.Message)));
                }
                catch (Exception ex)
                {
                    all.AddRange(span.Points.Select(p =>
                        new RawSample(p.Id, null, Quality.Bad, null, ex.Message)));
                }
            }

            return all;
        }

        public async Task<IReadOnlyList<WriteResult>> WriteAsync(IReadOnlyList<WriteCommand> commands, CancellationToken ct)
        {
            var results = new List<WriteResult>(commands.Count);

            foreach (var cmd in commands)
            {
                try
                {
                    var pdu = _codec.EncodeWrite(cmd);
                    var response = await _framing.RoundTripAsync(pdu, ct).ConfigureAwait(false);
                    results.Add(_codec.DecodeWrite(cmd, response));
                }
                catch (OperationCanceledException) when (ct.IsCancellationRequested)
                {
                    throw;
                }
                catch (DriverException ex)
                {
                    results.Add(new WriteResult(cmd.Point.Id, false, QualityMap.From(ex.Kind),
                        DateTimeOffset.UtcNow, ex.Message));
                }
                catch (Exception ex)
                {
                    results.Add(new WriteResult(cmd.Point.Id, false, Quality.Bad,
                        DateTimeOffset.UtcNow, ex.Message));
                }
            }

            return results;
        }

        public async ValueTask DisposeAsync()
        {
            if (_disposed) return;
            _disposed = true;
            await _framing.DisposeAsync().ConfigureAwait(false);
        }
    }
}

/// <summary>分帧层：一次「写 PDU → 读回完整 PDU」的往返。</summary>
public interface IModbusFraming : IAsyncDisposable
{
    Task<byte[]> RoundTripAsync(byte[] pdu, CancellationToken ct);
}

/// <summary>TCP：MBAP 事务号必须匹配，否则视为「迟到的旧响应」直接失败。</summary>
internal sealed class TcpFraming : IModbusFraming
{
    private readonly OpenContext _ctx;
    private IByteStream? _stream;

    public TcpFraming(OpenContext ctx) => _ctx = ctx;

    public async Task<byte[]> RoundTripAsync(byte[] pdu, CancellationToken ct)
    {
        _stream ??= await ByteStreams.OpenAsync(_ctx.Channel, ct).ConfigureAwait(false);

        var tx = ModbusMasterDriver.NextTransaction();
        var adu = ModbusTcpFrame.Build(tx, (byte)_ctx.Device.UnitId, pdu);

        await _stream.WriteAsync(adu, ct).ConfigureAwait(false);

        var header = new byte[ModbusTcpFrame.MbapLength];
        await _stream.ReadExactlyAsync(header, ct).ConfigureAwait(false);

        var length = BinaryPrimitives.ReadUInt16BigEndian(header.AsSpan(4, 2));
        if (length is < 2 or > ModbusConstants.MaxAdu)
            throw DriverException.ProtocolError("modbus-tcp", "read", $"invalid MBAP length {length}");

        var body = new byte[length - 1];
        await _stream.ReadExactlyAsync(body, ct).ConfigureAwait(false);

        var full = new byte[ModbusTcpFrame.MbapLength + body.Length];
        header.CopyTo(full, 0);
        body.CopyTo(full, ModbusTcpFrame.MbapLength);

        var (gotTx, _, unit, responsePdu) = ModbusTcpFrame.Parse(full);

        // 事务号不匹配说明链路上有别人的响应（或上一次超时的迟到响应），
        // 直接丢弃比「装作成功」安全。
        if (gotTx != tx)
            throw DriverException.ProtocolError("modbus-tcp", "read",
                $"transaction mismatch: got {gotTx}, want {tx}");

        if (unit != (byte)_ctx.Device.UnitId && unit != 0)
            throw DriverException.ProtocolError("modbus-tcp", "read",
                $"unit mismatch: got {unit}, want {_ctx.Device.UnitId}");

        return responsePdu;
    }

    public async ValueTask DisposeAsync()
    {
        if (_stream is not null) await _stream.DisposeAsync().ConfigureAwait(false);
        _stream = null;
    }
}

/// <summary>
/// RTU：半双工。请求前清空接收缓冲，按功能码推算响应长度，
/// 这样不会把下一帧的字节当成当前帧的尾巴读走。
/// </summary>
internal sealed class RtuFraming : IModbusFraming
{
    private readonly OpenContext _ctx;
    private SerialLease? _lease;

    public RtuFraming(OpenContext ctx) => _ctx = ctx;

    public async Task<byte[]> RoundTripAsync(byte[] pdu, CancellationToken ct)
    {
        _lease ??= await SerialPortTable.AcquireAsync(_ctx.Channel).ConfigureAwait(false);

        await _lease.Mutex.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            _lease.Stream.DiscardInBuffer();

            var frame = ModbusRtuFrame.Build((byte)_ctx.Device.UnitId, pdu);
            await _lease.Stream.WriteAsync(frame, ct).ConfigureAwait(false);

            // 站号 + 功能码
            var head = new byte[2];
            await _lease.Stream.ReadExactlyAsync(head, ct).ConfigureAwait(false);

            var fc = head[1];
            byte[] rest;

            if ((fc & 0x80) != 0)
            {
                rest = new byte[3]; // 异常码 + CRC
                await _lease.Stream.ReadExactlyAsync(rest, ct).ConfigureAwait(false);
            }
            else if (fc is >= ModbusConstants.FcReadCoils and <= ModbusConstants.FcReadInputRegs)
            {
                var bc = new byte[1];
                await _lease.Stream.ReadExactlyAsync(bc, ct).ConfigureAwait(false);

                if (bc[0] == 0 || bc[0] > 250)
                    throw DriverException.ProtocolError("modbus-rtu", "read",
                        $"invalid byte count {bc[0]} in response");

                rest = new byte[1 + bc[0] + 2];
                rest[0] = bc[0];
                await _lease.Stream.ReadExactlyAsync(rest.AsMemory(1), ct).ConfigureAwait(false);
            }
            else
            {
                rest = new byte[6]; // addr(2) + value/qty(2) + CRC(2)
                await _lease.Stream.ReadExactlyAsync(rest, ct).ConfigureAwait(false);
            }

            var full = new byte[2 + rest.Length];
            head.CopyTo(full, 0);
            rest.CopyTo(full, 2);

            var (unit, responsePdu) = ModbusRtuFrame.Parse(full);

            if (unit != (byte)_ctx.Device.UnitId)
                throw DriverException.ProtocolError("modbus-rtu", "read",
                    $"unit mismatch: got {unit}, want {_ctx.Device.UnitId}");

            return responsePdu;
        }
        finally
        {
            _lease.Mutex.Release();
        }
    }

    public async ValueTask DisposeAsync()
    {
        if (_lease is not null) await SerialPortTable.ReleaseAsync(_lease).ConfigureAwait(false);
        _lease = null;
    }
}

/// <summary>ASCII：以 CRLF 为帧尾，逐字节读到边界为止。</summary>
internal sealed class AsciiFraming : IModbusFraming
{
    private readonly OpenContext _ctx;
    private SerialLease? _lease;

    public AsciiFraming(OpenContext ctx) => _ctx = ctx;

    public async Task<byte[]> RoundTripAsync(byte[] pdu, CancellationToken ct)
    {
        _lease ??= await SerialPortTable.AcquireAsync(_ctx.Channel).ConfigureAwait(false);

        await _lease.Mutex.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            _lease.Stream.DiscardInBuffer();

            var frame = ModbusAsciiFrame.Build((byte)_ctx.Device.UnitId, pdu);
            await _lease.Stream.WriteAsync(Encoding.ASCII.GetBytes(frame), ct).ConfigureAwait(false);

            var bytes = new List<byte>(64);
            var one = new byte[1];

            // 先等起始冒号，再读到 CRLF：串口上可能先收到噪声字节。
            while (bytes.Count == 0)
            {
                await _lease.Stream.ReadExactlyAsync(one, ct).ConfigureAwait(false);
                if (one[0] == ModbusAsciiFrame.Start) bytes.Add(one[0]);
            }

            while (true)
            {
                await _lease.Stream.ReadExactlyAsync(one, ct).ConfigureAwait(false);
                bytes.Add(one[0]);

                if (bytes.Count >= 2 && bytes[^2] == (byte)'\r' && bytes[^1] == (byte)'\n') break;

                if (bytes.Count > ModbusAsciiFrame.MaxFrameBytes)
                    throw DriverException.ProtocolError("modbus-ascii", "read",
                        $"frame exceeds {ModbusAsciiFrame.MaxFrameBytes} bytes");
            }

            var (unit, responsePdu) = ModbusAsciiFrame.Parse(Encoding.ASCII.GetString(bytes.ToArray()));

            if (unit != (byte)_ctx.Device.UnitId)
                throw DriverException.ProtocolError("modbus-ascii", "read",
                    $"unit mismatch: got {unit}, want {_ctx.Device.UnitId}");

            return responsePdu;
        }
        finally
        {
            _lease.Mutex.Release();
        }
    }

    public async ValueTask DisposeAsync()
    {
        if (_lease is not null) await SerialPortTable.ReleaseAsync(_lease).ConfigureAwait(false);
        _lease = null;
    }
}

/// <summary>
/// 物理串口复用表。
///
/// 一条 RS-485 总线上通常挂多台从站，每台在配置里是独立设备。如果每个设备
/// 各自打开同一个 /dev/ttyUSB0，Linux 上两个 fd 同时读一个 tty 会互相偷字节，
/// 表现是「单台跑得通，多台就随机超时」。因此按路径引用计数复用，事务串行化
/// 交给每个端口自己的一把锁。
/// </summary>
internal static class SerialPortTable
{
    private static readonly Dictionary<string, SharedSerial> Ports = new(StringComparer.Ordinal);
    private static readonly SemaphoreSlim Gate = new(1, 1);

    private sealed class SharedSerial
    {
        public required SerialByteStream Stream { get; init; }
        public required SemaphoreSlim Mutex { get; init; }
        public int Refs;
    }

    public static async Task<SerialLease> AcquireAsync(Channel channel)
    {
        var options = channel.Serial ?? throw new ArgumentException("channel.serial is required");

        await Gate.WaitAsync().ConfigureAwait(false);
        try
        {
            if (!Ports.TryGetValue(options.Port, out var shared))
            {
                shared = new SharedSerial
                {
                    Stream = SerialByteStream.Open(options),
                    Mutex = new SemaphoreSlim(1, 1),
                };
                Ports[options.Port] = shared;
            }

            shared.Refs++;
            return new SerialLease(options.Port, shared.Stream, shared.Mutex);
        }
        finally
        {
            Gate.Release();
        }
    }

    public static async Task ReleaseAsync(SerialLease lease)
    {
        await Gate.WaitAsync().ConfigureAwait(false);
        try
        {
            if (!Ports.TryGetValue(lease.Port, out var shared)) return;

            shared.Refs--;
            if (shared.Refs > 0) return;

            Ports.Remove(lease.Port);
            shared.Mutex.Dispose();
            await shared.Stream.DisposeAsync().ConfigureAwait(false);
        }
        finally
        {
            Gate.Release();
        }
    }
}

internal sealed class SerialLease
{
    public string Port { get; }
    public IByteStream Stream { get; }
    public SemaphoreSlim Mutex { get; }

    public SerialLease(string port, IByteStream stream, SemaphoreSlim mutex)
    {
        Port = port;
        Stream = stream;
        Mutex = mutex;
    }
}