using System.Buffers.Binary;
using System.Net;
using System.Net.Sockets;
using Uic.Codec.Modbus;

namespace Uic.Sim.Modbus;

/// <summary>
/// Modbus 从站内存模型。主站与从站共用同一份 PDU 编解码，
/// 避免「主站能读、从站写错」这类语义漂移。
/// </summary>
public sealed class ModbusSlaveModel
{
    public const int Size = 65536;

    public ushort[] Holding { get; } = new ushort[Size];
    public ushort[] Input { get; } = new ushort[Size];

    // 位区实际常用前 8K 点；按需增长比一次开 64K 布尔更省内存。
    private bool[] _coils = new bool[8192];
    private bool[] _discrete = new bool[8192];

    public bool[] Coils => _coils;
    public bool[] Discrete => _discrete;

    public void EnsureBits(int count)
    {
        if (count <= _coils.Length) return;
        var next = Math.Max(count, _coils.Length * 2);
        _coils = new bool[next];
        _discrete = new bool[next];
    }

    public void SeedHolding(int offset, params ushort[] values)
    {
        for (var i = 0; i < values.Length; i++) Holding[offset + i] = values[i];
    }

    public void SeedInput(int offset, params ushort[] values)
    {
        for (var i = 0; i < values.Length; i++) Input[offset + i] = values[i];
    }

    public void SeedCoils(int offset, params bool[] values)
    {
        EnsureBits(offset + values.Length);
        for (var i = 0; i < values.Length; i++) _coils[offset + i] = values[i];
    }

    public void SeedDiscrete(int offset, params bool[] values)
    {
        EnsureBits(offset + values.Length);
        for (var i = 0; i < values.Length; i++) _discrete[offset + i] = values[i];
    }
}

/// <summary>
/// 从站 PDU 处理器。纯函数：给定请求 PDU 与内存模型，返回响应 PDU。
/// 这样同一份逻辑可以被 TCP 从站、RTU 从站与单元测试共用。
/// </summary>
public sealed class ModbusSlaveHandler
{
    private readonly ModbusSlaveModel _model;
    private readonly object _lock;

    public ModbusSlaveHandler(ModbusSlaveModel model, object? gate = null)
    {
        _model = model;
        _lock = gate ?? new object();
    }

    public byte[] Handle(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length == 0) return Exception(0, 0x03);

        var fc = pdu[0];
        switch (fc)
        {
            case ModbusConstants.FcReadCoils:
                return ReadBits(pdu, _model.Coils);
            case ModbusConstants.FcReadDiscreteInputs:
                return ReadBits(pdu, _model.Discrete);
            case ModbusConstants.FcReadHoldingRegs:
                return ReadRegisters(pdu, _model.Holding);
            case ModbusConstants.FcReadInputRegs:
                return ReadRegisters(pdu, _model.Input);
            case ModbusConstants.FcWriteSingleCoil:
                return WriteSingleCoil(pdu);
            case ModbusConstants.FcWriteSingleRegister:
                return WriteSingleRegister(pdu);
            case ModbusConstants.FcWriteMultipleCoils:
                return WriteMultipleCoils(pdu);
            case ModbusConstants.FcWriteMultipleRegs:
                return WriteMultipleRegisters(pdu);
            default:
                return Exception(fc, 0x01);
        }
    }

    private byte[] ReadBits(ReadOnlySpan<byte> pdu, bool[] area)
    {
        if (pdu.Length < 5) return Exception(pdu[0], 0x03);

        var start = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(1, 2));
        var qty = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));

        if (qty is < 1 or > ModbusConstants.MaxReadBits) return Exception(pdu[0], 0x03);

        lock (_lock)
        {
            if (start + qty > area.Length) return Exception(pdu[0], 0x02);

            var bytes = (qty + 7) / 8;
            var resp = new byte[2 + bytes];
            resp[0] = pdu[0];
            resp[1] = (byte)bytes;

            for (var i = 0; i < qty; i++)
            {
                if (area[start + i]) resp[2 + i / 8] |= (byte)(1 << (i % 8));
            }
            return resp;
        }
    }

    private byte[] ReadRegisters(ReadOnlySpan<byte> pdu, ushort[] area)
    {
        if (pdu.Length < 5) return Exception(pdu[0], 0x03);

        var start = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(1, 2));
        var qty = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));

        if (qty is < 1 or > ModbusConstants.MaxReadRegisters) return Exception(pdu[0], 0x03);

        lock (_lock)
        {
            if (start + qty > area.Length) return Exception(pdu[0], 0x02);

            var resp = new byte[2 + qty * 2];
            resp[0] = pdu[0];
            resp[1] = (byte)(qty * 2);

            for (var i = 0; i < qty; i++)
            {
                BinaryPrimitives.WriteUInt16BigEndian(resp.AsSpan(2 + i * 2, 2), area[start + i]);
            }
            return resp;
        }
    }

    private byte[] WriteSingleCoil(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length < 5) return Exception(pdu[0], 0x03);

        var addr = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(1, 2));
        var value = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));

        if (value is not (0x0000 or 0xFF00)) return Exception(pdu[0], 0x03);

        lock (_lock)
        {
            if (addr >= _model.Coils.Length) return Exception(pdu[0], 0x02);
            _model.Coils[addr] = value == 0xFF00;
        }

        return pdu[..5].ToArray();
    }

    private byte[] WriteSingleRegister(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length < 5) return Exception(pdu[0], 0x03);

        var addr = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(1, 2));
        lock (_lock)
        {
            if (addr >= _model.Holding.Length) return Exception(pdu[0], 0x02);
            _model.Holding[addr] = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));
        }

        return pdu[..5].ToArray();
    }

    private byte[] WriteMultipleCoils(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length < 6) return Exception(pdu[0], 0x03);

        var start = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(1, 2));
        var qty = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));
        var bc = pdu[5];

        if (qty is < 1 or > ModbusConstants.MaxWriteBits) return Exception(pdu[0], 0x03);
        if (bc < (qty + 7) / 8 || pdu.Length < 6 + bc) return Exception(pdu[0], 0x03);

        lock (_lock)
        {
            if (start + qty > _model.Coils.Length) return Exception(pdu[0], 0x02);

            for (var i = 0; i < qty; i++)
            {
                _model.Coils[start + i] = (pdu[6 + i / 8] & (1 << (i % 8))) != 0;
            }
        }

        return pdu[..5].ToArray();
    }

    private byte[] WriteMultipleRegisters(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length < 6) return Exception(pdu[0], 0x03);

        var start = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(1, 2));
        var qty = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));
        var bc = pdu[5];

        if (qty is < 1 or > ModbusConstants.MaxWriteRegisters) return Exception(pdu[0], 0x03);
        if (bc != qty * 2 || pdu.Length < 6 + bc) return Exception(pdu[0], 0x03);

        lock (_lock)
        {
            if (start + qty > _model.Holding.Length) return Exception(pdu[0], 0x02);

            for (var i = 0; i < qty; i++)
            {
                _model.Holding[start + i] = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(6 + i * 2, 2));
            }
        }

        return pdu[..5].ToArray();
    }

    private static byte[] Exception(byte fc, byte code) => new[] { (byte)(fc | 0x80), code };
}

/// <summary>
/// 内置 Modbus TCP 从站。用于无真机开发、自测与替代设备；
/// 它与主站共用 codec，因此主从语义不会各自漂移。
/// </summary>
public sealed class ModbusTcpSlaveServer : IAsyncDisposable
{
    private readonly TcpListener _listener;
    private readonly ModbusSlaveHandler _handler;
    private readonly CancellationTokenSource _cts = new();
    private readonly List<Task> _clients = new();
    private Task? _acceptLoop;
    private int _closed;

    public ModbusTcpSlaveServer(IPAddress address, int port, ModbusSlaveModel model, object? gate = null)
    {
        _listener = new TcpListener(address, port);
        _handler = new ModbusSlaveHandler(model, gate);
        Model = model;
    }

    public ModbusSlaveModel Model { get; }

    public int Port => ((IPEndPoint)_listener.LocalEndpoint).Port;

    public void Start()
    {
        _listener.Start();
        _acceptLoop = Task.Run(() => AcceptLoopAsync(_cts.Token));
    }

    private async Task AcceptLoopAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested)
        {
            TcpClient client;
            try
            {
                client = await _listener.AcceptTcpClientAsync(ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                return;
            }
            catch (ObjectDisposedException)
            {
                return;
            }
            catch (SocketException)
            {
                return;
            }

            var task = Task.Run(() => ServeAsync(client, ct), ct);
            lock (_clients) _clients.Add(task);
        }
    }

    private async Task ServeAsync(TcpClient client, CancellationToken ct)
    {
        using (client)
        {
            client.NoDelay = true;
            var stream = client.GetStream();
            var header = new byte[ModbusTcpFrame.MbapLength];

            try
            {
                while (!ct.IsCancellationRequested)
                {
                    await stream.ReadExactlyAsync(header, ct).ConfigureAwait(false);

                    var tx = BinaryPrimitives.ReadUInt16BigEndian(header.AsSpan(0, 2));
                    var proto = BinaryPrimitives.ReadUInt16BigEndian(header.AsSpan(2, 2));
                    var length = BinaryPrimitives.ReadUInt16BigEndian(header.AsSpan(4, 2));
                    var unit = header[6];

                    // 非法 MBAP 直接断链：继续读只会把后续帧读错位。
                    if (proto != 0 || length is < 2 or > ModbusConstants.MaxAdu) return;

                    var pdu = new byte[length - 1];
                    await stream.ReadExactlyAsync(pdu, ct).ConfigureAwait(false);

                    var response = _handler.Handle(pdu);
                    var adu = ModbusTcpFrame.Build(tx, unit, response);
                    await stream.WriteAsync(adu, ct).ConfigureAwait(false);
                }
            }
            catch (OperationCanceledException)
            {
                // 关闭期正常路径。
            }
            catch (EndOfStreamException)
            {
                // 客户端断开。
            }
            catch (IOException)
            {
                // 客户端断开。
            }
            catch (SocketException)
            {
                // 客户端断开。
            }
            catch (ObjectDisposedException)
            {
                // 关闭期正常路径。
            }
        }
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _closed, 1) != 0) return;

        await _cts.CancelAsync().ConfigureAwait(false);
        _listener.Stop();

        if (_acceptLoop is not null)
        {
            try { await _acceptLoop.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }

        Task[] pending;
        lock (_clients) pending = _clients.ToArray();

        foreach (var t in pending)
        {
            try { await t.ConfigureAwait(false); } catch { /* 同上 */ }
        }

        _cts.Dispose();
    }
}