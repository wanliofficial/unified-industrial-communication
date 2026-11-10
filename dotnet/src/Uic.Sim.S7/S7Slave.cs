using System.Buffers.Binary;
using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using Uic.Codec.S7;
using Uic.Driver;

namespace Uic.Sim.S7;

/// <summary>
/// S7 从站内存模型。DB 号 → 字节数组；I/Q/M 各一块固定大小的过程映像。
///
/// 用字节数组而不是「寄存器数组」：S7 的地址是字节加位，DB1.DBB3 与 DB1.DBW3
/// 落在同一个字节上。按字组织会在奇数字节偏移（现场常见）上做二次换算，
/// 那正是「Modbus 思维」搬过来最容易出错的地方。
/// </summary>
public sealed class S7SlaveModel
{
    private const int AreaSize = 8192;

    private readonly ConcurrentDictionary<int, byte[]> _dataBlocks = new();
    private readonly byte[] _inputs = new byte[AreaSize];
    private readonly byte[] _outputs = new byte[AreaSize];
    private readonly byte[] _flags = new byte[AreaSize];

    /// <summary>已处理的请求数。用例用它断言「确实发过帧」。</summary>
    public int RequestCount;

    /// <summary>
    /// Setup Communication 里回应给主站的 PDU 上限。默认 480；
    /// 调低它就能验证「主站按对端给的值组帧」而不是按自己请求的值。
    /// </summary>
    public int NegotiatedPdu { get; set; } = S7Constants.PduMedium;

    public byte[] DataBlock(int db, int size = 4096) => _dataBlocks.GetOrAdd(db, _ => new byte[size]);
    public byte[] Inputs => _inputs;
    public byte[] Outputs => _outputs;
    public byte[] Flags => _flags;

    internal byte[]? Area(byte area, int db) => area switch
    {
        S7Constants.AreaDataBlock => DataBlock(db),
        S7Constants.AreaInput => _inputs,
        S7Constants.AreaOutput => _outputs,
        S7Constants.AreaFlag => _flags,
        _ => null,
    };

    public void SeedDataBlock(int db, int offset, params byte[] values)
    {
        var block = DataBlock(db);
        if (offset + values.Length > block.Length)
            throw new ArgumentOutOfRangeException(nameof(offset), "seed exceeds the data block");
        values.CopyTo(block, offset);
    }

    public void SeedDataBlock(int db, int offset, ushort value)
        => SeedDataBlock(db, offset, (byte)(value >> 8), (byte)(value & 0xFF));

    public void SeedFlagBit(int byteOffset, int bit, bool value)
    {
        if (value) _flags[byteOffset] |= (byte)(1 << bit);
        else _flags[byteOffset] &= (byte)~(1 << bit);
    }
}

/// <summary>
/// 把 S7 请求 PDU 变成 S7 响应 PDU。纯函数：给定请求与内存模型返回响应字节，
/// 不碰套接字。这样同一份逻辑能被 TCP 从站、单测与黄金帧校验共用——
/// 与 Modbus 的 <c>ModbusSlaveHandler</c> 是同一种分层。
/// </summary>
public static class S7SlaveHandler
{
    public static byte[] Handle(S7SlaveModel model, ReadOnlySpan<byte> request)
    {
        var pdu = S7Frame.Parse(request);
        if (pdu.Rosctr != S7Constants.RosctrJob)
            throw DriverException.ProtocolError("s7", "slave",
                $"expected a job PDU, got ROSCTR 0x{pdu.Rosctr:x2}");

        Interlocked.Increment(ref model.RequestCount);

        if (pdu.Parameter.Length == 0) return Error(pdu, 0x00, 0x00, Array.Empty<byte>());

        return pdu.Parameter[0] switch
        {
            S7Constants.FuncSetupCommunication => Setup(model, pdu),
            S7Constants.FuncReadVar => Read(model, pdu),
            S7Constants.FuncWriteVar => Write(model, pdu),
            // 不认识的函数绝不能「当成功回」：主站会以为改到了 PLC。
            var fc => Error(pdu, 0x81, fc, pdu.Parameter),
        };
    }

    private static byte[] Setup(S7SlaveModel model, S7Pdu pdu)
    {
        // 回应里把 PDU 长度改成从站自己的档位：主站按回应为准，
        // 这正是「协商」而非「声明」要验的行为。
        var param = pdu.Parameter.ToArray();
        if (param.Length >= 8)
            BinaryPrimitives.WriteUInt16BigEndian(param.AsSpan(6, 2), (ushort)model.NegotiatedPdu);

        return AckData(pdu.PduReference, 0x00, 0x00, param, Array.Empty<byte>());
    }

    private static byte[] Read(S7SlaveModel model, S7Pdu pdu)
    {
        var count = pdu.Parameter.Length > 1 ? pdu.Parameter[1] : (byte)0;
        var data = new List<byte>(count * 8);

        for (var i = 0; i < count; i++)
        {
            var item = pdu.Parameter.AsSpan(2 + i * 12, 12);
            var isBit = item[3] == S7Constants.VarBit;
            var (returnCode, bytes) = ReadItem(model, item);

            data.Add(returnCode);

            // 传输尺寸要跟**这一项**的请求一致：请求 BIT 就回 BIT（长度单位是位），
            // 请求 BYTE 就回 BYTE/WORD/DWORD（长度单位也是位，按字节数 × 8）。
            // 不能用「本包里有没有位请求」这种全局判断：一次混合读会写错长度单位。
            data.Add(isBit ? S7Constants.DataBit : S7Constants.DataByteWordDword);

            var length = bytes is null
                ? 0
                : isBit ? 1 : bytes.Length * 8;

            data.Add((byte)(length >> 8));
            data.Add((byte)(length & 0xFF));
            if (bytes is not null) data.AddRange(bytes);

            // 数据项按偶数字节对齐。
            if (bytes is not null && bytes.Length % 2 == 1) data.Add(0x00);
        }

        var param = new[] { S7Constants.FuncReadVar, count };
        return AckData(pdu.PduReference, 0x00, 0x00, param, data.ToArray());
    }

    private static (byte ReturnCode, byte[]? Bytes) ReadItem(S7SlaveModel model, ReadOnlySpan<byte> item)
    {
        var area = item[8];
        var db = BinaryPrimitives.ReadUInt16BigEndian(item.Slice(6, 2));
        var bitOffset = (item[9] << 16) | (item[10] << 8) | item[11];
        var count = BinaryPrimitives.ReadUInt16BigEndian(item.Slice(4, 2));
        var isBit = item[3] == S7Constants.VarBit;

        var memory = model.Area(area, db);
        if (memory is null) return (S7Constants.ReturnObjectNotExist, null);

        if (isBit)
        {
            var index = bitOffset / 8;
            if (index >= memory.Length) return (S7Constants.ReturnAddressOutOfRange, null);
            var value = (memory[index] >> (bitOffset % 8)) & 0x01;
            return (S7Constants.ReturnOk, new[] { (byte)value });
        }

        var start = bitOffset / 8;
        var size = Math.Max(1, (int)count);
        if (start + size > memory.Length) return (S7Constants.ReturnAddressOutOfRange, null);

        return (S7Constants.ReturnOk, memory.AsSpan(start, size).ToArray());
    }

    private static byte[] Write(S7SlaveModel model, S7Pdu pdu)
    {
        var count = pdu.Parameter.Length > 1 ? pdu.Parameter[1] : (byte)0;
        var returns = new byte[count];
        var offset = 0;

        for (var i = 0; i < count; i++)
        {
            var item = pdu.Parameter.AsSpan(2 + i * 12, 12);
            var isBit = item[3] == S7Constants.VarBit;

            var transport = pdu.Data.Length > offset + 1 ? pdu.Data[offset + 1] : (byte)0;
            var length = pdu.Data.Length > offset + 3
                ? BinaryPrimitives.ReadUInt16BigEndian(pdu.Data.AsSpan(offset + 2, 2))
                : 0;

            // 与读响应同一条规则：BIT / BYTE 的长度单位是位。
            var byteCount = transport is S7Constants.DataBit or S7Constants.DataByteWordDword
                ? (length + 7) / 8
                : length;

            var available = Math.Max(0, pdu.Data.Length - offset - 4);
            if (byteCount > available) byteCount = available;

            var payload = pdu.Data.AsSpan(offset + 4, byteCount);
            returns[i] = WriteItem(model, item, payload);

            offset += 4 + byteCount + (byteCount % 2);
        }

        var param = new[] { S7Constants.FuncWriteVar, count };
        return AckData(pdu.PduReference, 0x00, 0x00, param, returns);
    }

    private static byte WriteItem(S7SlaveModel model, ReadOnlySpan<byte> item, ReadOnlySpan<byte> payload)
    {
        var area = item[8];
        var db = BinaryPrimitives.ReadUInt16BigEndian(item.Slice(6, 2));
        var bitOffset = (item[9] << 16) | (item[10] << 8) | item[11];
        var isBit = item[3] == S7Constants.VarBit;

        var memory = model.Area(area, db);
        if (memory is null) return S7Constants.ReturnObjectNotExist;

        // 输入区在真实 CPU 上由过程映像写入，外部写请求一律拒绝。
        if (area == S7Constants.AreaInput) return S7Constants.ReturnNotAllowed;

        if (isBit)
        {
            var index = bitOffset / 8;
            if (index >= memory.Length) return S7Constants.ReturnAddressOutOfRange;

            var bit = bitOffset % 8;
            var on = payload.Length > 0 && (payload[0] & 0x01) != 0;
            if (on) memory[index] |= (byte)(1 << bit);
            else memory[index] &= (byte)~(1 << bit);

            return S7Constants.ReturnOk;
        }

        var start = bitOffset / 8;
        if (start + payload.Length > memory.Length) return S7Constants.ReturnAddressOutOfRange;

        payload.CopyTo(memory.AsSpan(start));
        return S7Constants.ReturnOk;
    }

    private static byte[] Error(S7Pdu request, byte errorClass, byte errorCode, ReadOnlySpan<byte> param)
        => AckData(request.PduReference, errorClass, errorCode, param.ToArray(), Array.Empty<byte>());

    /// <summary>Ack_Data 头的装配。放成 public 是为了黄金帧用例能直接比对完整线上字节。</summary>
    public static byte[] AckData(
        ushort pduReference, byte errorClass, byte errorCode, byte[] param, byte[] data)
    {
        var pdu = new byte[S7Constants.AckDataHeaderLength + param.Length + data.Length];
        pdu[0] = S7Constants.ProtocolId;
        pdu[1] = S7Constants.RosctrAckData;
        pdu[2] = 0x00;
        pdu[3] = 0x00;
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(4, 2), pduReference);
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(6, 2), (ushort)param.Length);
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(8, 2), (ushort)data.Length);
        pdu[10] = errorClass;
        pdu[11] = errorCode;
        param.CopyTo(pdu, S7Constants.AckDataHeaderLength);
        data.CopyTo(pdu, S7Constants.AckDataHeaderLength + param.Length);
        return pdu;
    }
}

/// <summary>
/// TCP 从站：TPKT/COTP 分帧 + 上面的处理器。
///
/// 一条连接上一次只服务一个请求，与真实 CPU 的行为一致（S7 是请求/响应协议，
/// 并发靠多条连接，不靠流水线）。这样用例里「发一帧、读一帧」的推理才成立。
/// </summary>
public sealed class S7TcpSlaveServer : IAsyncDisposable
{
    private readonly TcpListener _listener;
    private readonly S7SlaveModel _model;
    private readonly CancellationTokenSource _cts = new();
    private readonly List<Task> _clients = new();
    private Task? _acceptLoop;
    private int _closed;

    public S7TcpSlaveServer(IPAddress address, int port, S7SlaveModel model)
    {
        _listener = new TcpListener(address, port);
        _model = model;
    }

    public S7SlaveModel Model => _model;

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
            catch (Exception ex) when (ex is OperationCanceledException or ObjectDisposedException or SocketException)
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
            var header = new byte[S7Constants.TpktHeaderLength];

            while (!ct.IsCancellationRequested)
            {
                var frame = await ReadFrameAsync(stream, header, ct).ConfigureAwait(false);
                if (frame is null) return;

                var cotp = Tpkt.Payload(frame);

                // 连接请求走 COTP 确认，其余按 DT 数据包处理。
                if (cotp.Length > 1 && cotp[1] == S7Constants.CotpConnectRequest)
                {
                    var confirm = BuildConnectConfirm(cotp);
                    await stream.WriteAsync(confirm, ct).ConfigureAwait(false);
                    continue;
                }

                if (cotp.Length < 3 || cotp[1] != S7Constants.CotpData) continue;

                byte[] response;
                try
                {
                    response = S7SlaveHandler.Handle(_model, cotp[3..]);
                }
                catch (DriverException)
                {
                    // 坏帧不改协议状态：丢掉这一条，连接继续。
                    continue;
                }

                await stream.WriteAsync(Cotp.BuildData(response), ct).ConfigureAwait(false);
            }
        }
    }

    private static async Task<byte[]?> ReadFrameAsync(
        NetworkStream stream, byte[] header, CancellationToken ct)
    {
        var read = 0;
        while (read < header.Length)
        {
            var n = await stream.ReadAsync(header.AsMemory(read), ct).ConfigureAwait(false);
            if (n == 0) return null;
            read += n;
        }

        var total = BinaryPrimitives.ReadUInt16BigEndian(header.AsSpan(2, 2));
        if (total < 7 || total > 8192) return null;

        var frame = new byte[total];
        header.CopyTo(frame, 0);
        read = header.Length;

        while (read < total)
        {
            var n = await stream.ReadAsync(frame.AsMemory(read), ct).ConfigureAwait(false);
            if (n == 0) return null;
            read += n;
        }

        return frame;
    }

    /// <summary>回一个最小可用的 COTP 连接确认：把主站的目的 TSAP 原样回显。</summary>
    private static byte[] BuildConnectConfirm(ReadOnlySpan<byte> request)
    {
        var localTsap = (ushort)0x0100;
        var remoteTsap = (ushort)0x0100;

        // 请求里的参数区（从第 7 字节起）逐项 [code, len, payload]；C1 是本端 TSAP，C2 是对端。
        var i = 7;
        while (i + 2 <= request.Length)
        {
            var code = request[i];
            var len = request[i + 1];
            if (i + 2 + len > request.Length) break;

            if (code == 0xC1 && len == 2) remoteTsap = BinaryPrimitives.ReadUInt16BigEndian(request.Slice(i + 2, 2));
            if (code == 0xC2 && len == 2) localTsap = BinaryPrimitives.ReadUInt16BigEndian(request.Slice(i + 2, 2));

            i += 2 + len;
        }

        var cotp = new byte[18];
        cotp[0] = 17;
        cotp[1] = S7Constants.CotpConnectConfirm;

        // 连接确认把请求里的引用号对调：对方的 dst-ref 就是我们的 src-ref。
        // 驱动目前不校验引用号，但抓包对照真机时这一处对不上会很显眼。
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(2, 2), 0x0001);
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(4, 2), 0x0000);
        cotp[6] = 0x00;
        cotp[7] = 0xC0; cotp[8] = 0x01; cotp[9] = 0x0A;
        cotp[10] = 0xC1; cotp[11] = 0x02;
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(12, 2), localTsap);
        cotp[14] = 0xC2; cotp[15] = 0x02;
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(16, 2), remoteTsap);

        return Tpkt.Build(cotp);
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

        Task[] clients;
        lock (_clients) clients = _clients.ToArray();
        try { await Task.WhenAll(clients).ConfigureAwait(false); } catch { /* 同上 */ }

        _cts.Dispose();
    }
}
