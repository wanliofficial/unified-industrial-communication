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

    /// <summary>
    /// 文件记录区：文件号 → 记录号 → 寄存器数组。
    /// 用嵌套字典而不是定长数组：现场文件号/记录号是稀疏的（1、4、9…），
    /// 一次开满 65536 × N 的记录表只为几个记录付全部内存。
    /// 记录长度按需增长，首次写入时定型——后续写入不能超出它。
    /// </summary>
    private readonly Dictionary<ushort, Dictionary<ushort, ushort[]>> _files = new();

    public bool[] Coils => _coils;
    public bool[] Discrete => _discrete;

    /// <summary>文件记录内容；不存在返回 null（调用方按「非法地址」处理）。</summary>
    public ushort[]? GetFileRecord(ushort fileId, ushort recordId)
        => _files.TryGetValue(fileId, out var records) && records.TryGetValue(recordId, out var record)
            ? record
            : null;

    /// <summary>
    /// 写入文件记录。记录长度在首次写入时定型；后续写入不能超出已有长度——
    /// 允许变长会让「读回比写进少一截」变成常态，而那正是最该被发现的数据错位。
    /// 但**允许写记录的前一段**：现场记录长度由设备固定，一次只更新前几个寄存器是常态。
    /// </summary>
    public bool TryWriteFileRecord(ushort fileId, ushort recordId, ReadOnlySpan<ushort> values)
    {
        if (!_files.TryGetValue(fileId, out var records))
        {
            records = new Dictionary<ushort, ushort[]>();
            _files[fileId] = records;
        }

        if (records.TryGetValue(recordId, out var existing))
        {
            if (values.Length > existing.Length) return false;
            values.CopyTo(existing);
            return true;
        }

        records[recordId] = values.ToArray();
        return true;
    }

    /// <summary>预置一条文件记录（测试与演示用）。</summary>
    public void SeedFileRecord(ushort fileId, ushort recordId, params ushort[] values)
    {
        if (!_files.TryGetValue(fileId, out var records))
        {
            records = new Dictionary<ushort, ushort[]>();
            _files[fileId] = records;
        }

        records[recordId] = values.ToArray();
    }

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
            case ModbusConstants.FcReadFileRecord:
                return ReadFileRecord(pdu);
            case ModbusConstants.FcWriteFileRecord:
                return WriteFileRecord(pdu);
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

    /// <summary>
    /// FC20 读文件记录。请求：功能码 + 字节数 + 子请求[参考类型 + 文件号 + 记录号 + 记录长度]。
    ///
    /// 不存在的文件/记录回 exception 02（非法数据地址）而不是「全 0」：
    /// 返回 0 会让主站把「没这个记录」读成「记录全是 0」，运维看到的是合法读数。
    /// </summary>
    private byte[] ReadFileRecord(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length < 8) return Exception(pdu[0], 0x03);

        var byteCount = pdu[1];
        if (byteCount < 7 || pdu.Length < 2 + byteCount) return Exception(pdu[0], 0x03);

        if (pdu[2] != ModbusConstants.FileReferenceType) return Exception(pdu[0], 0x03);

        var fileId = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));
        var recordId = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(5, 2));
        var registers = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(7, 2));

        if (registers == 0 || registers * 2 > ModbusConstants.MaxFileRecordData)
            return Exception(pdu[0], 0x03);

        lock (_lock)
        {
            var record = _model.GetFileRecord(fileId, recordId);
            if (record is null) return Exception(pdu[0], 0x02);

            // 记录长度不足：请求要的比记录实际拥有的多，按非法地址拒绝。
            if (registers > record.Length) return Exception(pdu[0], 0x02);

            // 响应：功能码 + 总字节数 + 参考类型 + 数据字节数 + 数据
            var dataBytes = registers * 2;
            var resp = new byte[4 + dataBytes];
            resp[0] = pdu[0];
            resp[1] = (byte)(2 + dataBytes);
            resp[2] = ModbusConstants.FileReferenceType;
            resp[3] = (byte)dataBytes;

            for (var i = 0; i < registers; i++)
            {
                BinaryPrimitives.WriteUInt16BigEndian(resp.AsSpan(4 + i * 2, 2), record[i]);
            }

            return resp;
        }
    }

    /// <summary>
    /// FC21 写文件记录。请求结构与 FC20 相同，只是多带数据：
    /// 功能码 + 字节数 + 子请求[参考类型 + 文件号 + 记录号 + 记录长度 + 数据]。
    ///
    /// 记录不存在时**新建**（长度由本次写入定型）：要求现场先「声明记录长度」
    /// 才能写，等于多一步无处可配的初始化，很多设备本身就是首次写入即建立。
    /// 已存在的记录只允许写前一段，超出已有长度则拒绝，避免静默改长度导致回读错位。
    /// </summary>
    private byte[] WriteFileRecord(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length < 10) return Exception(pdu[0], 0x03);

        var byteCount = pdu[1];
        if (byteCount < 7 || pdu.Length < 2 + byteCount) return Exception(pdu[0], 0x03);

        if (pdu[2] != ModbusConstants.FileReferenceType) return Exception(pdu[0], 0x03);

        var fileId = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(3, 2));
        var recordId = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(5, 2));
        var registers = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(7, 2));

        if (registers == 0 || registers * 2 > ModbusConstants.MaxFileRecordData)
            return Exception(pdu[0], 0x03);

        // 字节数字段必须与「子请求头 + 数据」自洽：多了少了都说明帧异常。
        if (byteCount != 7 + registers * 2) return Exception(pdu[0], 0x03);

        var values = new ushort[registers];
        for (var i = 0; i < registers; i++)
        {
            values[i] = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(9 + i * 2, 2));
        }

        lock (_lock)
        {
            if (!_model.TryWriteFileRecord(fileId, recordId, values))
                return Exception(pdu[0], 0x02);
        }

        // FC21 响应回显整个请求（与原请求逐字节一致）。
        return pdu[..(2 + byteCount)].ToArray();
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