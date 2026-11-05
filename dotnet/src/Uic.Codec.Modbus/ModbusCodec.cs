using System.Buffers.Binary;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Codec.Modbus;

/// <summary>协议常量与校验算法。都是纯函数，主站、从站、金报文测试共用。</summary>
public static class ModbusConstants
{
    public const byte FcReadCoils = 0x01;
    public const byte FcReadDiscreteInputs = 0x02;
    public const byte FcReadHoldingRegs = 0x03;
    public const byte FcReadInputRegs = 0x04;
    public const byte FcWriteSingleCoil = 0x05;
    public const byte FcWriteSingleRegister = 0x06;
    public const byte FcWriteMultipleCoils = 0x0F;
    public const byte FcWriteMultipleRegs = 0x10;

    /// <summary>单次请求上限（协议规定）。</summary>
    public const int MaxReadRegisters = 125;

    public const int MaxWriteRegisters = 123;
    public const int MaxReadBits = 2000;
    public const int MaxWriteBits = 1968;
    public const int MaxAdu = 260;

    /// <summary>标准 Modbus CRC16。多项式 0xA001，初值 0xFFFF，线上小端。</summary>
    public static ushort Crc16(ReadOnlySpan<byte> data)
    {
        ushort crc = 0xFFFF;
        foreach (var b in data)
        {
            crc ^= b;
            for (var i = 0; i < 8; i++)
            {
                var lsb = (crc & 1) != 0;
                crc >>= 1;
                if (lsb) crc ^= 0xA001;
            }
        }
        return crc;
    }

    /// <summary>ASCII 帧的 LRC：逐字节求和取反加一。</summary>
    public static byte Lrc(ReadOnlySpan<byte> data)
    {
        var sum = 0;
        foreach (var b in data) sum += b;
        return (byte)(-(sbyte)(byte)sum);
    }

    /// <summary>区名 → 读功能码 / 是否按位 / 是否只读。</summary>
    public static (byte Function, bool IsBits, bool IsReadOnly) FunctionForArea(string area) =>
        area.Trim().ToLowerInvariant() switch
        {
            "coil" or "coils" => (FcReadCoils, true, false),
            "discrete" or "discrete_input" or "discreteinput" or "di" => (FcReadDiscreteInputs, true, true),
            "holding" or "holding_register" or "holdingregister" or "hr" => (FcReadHoldingRegs, false, false),
            "input" or "input_register" or "inputregister" or "ir" => (FcReadInputRegs, false, true),
            _ => throw DriverException.Unsupported("modbus", "encode", $"unknown area '{area}'"),
        };

    public static bool IsBitArea(string area) => FunctionForArea(area).IsBits;
}

/// <summary>一次读窗口：区 + 起始 + 数量 + 位/字。</summary>
public readonly record struct SpanWindow(string Area, int Start, int Quantity, bool IsBits)
{
    public int ByteCount => IsBits ? (Quantity + 7) / 8 : Quantity * 2;
}

/// <summary>
/// Modbus 纯函数编解码。
/// 不做 I/O、不读时钟、不持有可变状态——因此主站、从站模拟器与金报文测试共用同一份实现。
/// </summary>
public sealed class ModbusCodec
{
    public string DeviceId { get; }

    public ModbusCodec(string deviceId) => DeviceId = deviceId;

    /// <summary>计算覆盖一组点位的窗口。同一 span 内不允许混区。</summary>
    public static SpanWindow Window(ReadOnlySpan<PointRef> points)
    {
        if (points.Length == 0)
            throw DriverException.ProtocolError("modbus", "encode", "empty span");

        var area = points[0].Address.Area
                   ?? throw DriverException.ProtocolError("modbus", "encode", "area is required");
        var (_, isBits, _) = ModbusConstants.FunctionForArea(area);

        var start = points[0].Address.Offset;
        var end = start + LengthOf(points[0], isBits);

        foreach (var p in points)
        {
            if (p.Address.Kind != AddressKind.Register)
                throw DriverException.Unsupported("modbus", "encode",
                    $"address kind '{p.Address.Kind}' is not supported");

            if (!string.Equals(p.Address.Area, area, StringComparison.OrdinalIgnoreCase))
                throw DriverException.ProtocolError("modbus", "encode", "mixed areas in one span");

            if (p.Address.Offset < 0)
                throw DriverException.Address("modbus", "encode", $"negative offset {p.Address.Offset}");

            start = Math.Min(start, p.Address.Offset);
            end = Math.Max(end, p.Address.Offset + LengthOf(p, isBits));
        }

        var quantity = end - start;
        var limit = isBits ? ModbusConstants.MaxReadBits : ModbusConstants.MaxReadRegisters;

        if (quantity <= 0)
            throw DriverException.ProtocolError("modbus", "encode", "empty window");
        if (quantity > limit)
            throw DriverException.ProtocolError("modbus", "encode",
                $"{quantity} {(isBits ? "bits" : "registers")} exceeds limit {limit}");
        if (start > 0xFFFF || end > 0x10000)
            throw DriverException.Address("modbus", "encode", $"window {start}..{end} exceeds address space");

        return new SpanWindow(area, start, quantity, isBits);
    }

    private static int LengthOf(PointRef p, bool isBits) => isBits ? 1 : Math.Max(1, p.Length);

    public byte[] EncodeRead(IReadOnlyList<PointRef> points)
    {
        var window = Window(points.ToArray());
        var (fc, _, _) = ModbusConstants.FunctionForArea(window.Area);

        var pdu = new byte[5];
        pdu[0] = fc;
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(1, 2), (ushort)window.Start);
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(3, 2), (ushort)window.Quantity);
        return pdu;
    }

    public IReadOnlyList<RawSample> DecodeRead(IReadOnlyList<PointRef> points, ReadOnlySpan<byte> response)
    {
        var arr = points.ToArray();
        var window = Window(arr);
        var (fc, _, _) = ModbusConstants.FunctionForArea(window.Area);

        if (response.Length == 0)
            throw DriverException.ProtocolError("modbus", "decode", "empty response");

        if ((response[0] & 0x80) != 0)
            throw ExceptionError(response, "read");

        if (response[0] != fc)
            throw DriverException.ProtocolError("modbus", "decode",
                $"function echo 0x{response[0]:x2}, want 0x{fc:x2}");

        if (response.Length < 2)
            throw DriverException.ProtocolError("modbus", "decode", "missing byte count");

        var bc = response[1];

        // 字节数与功能码必须自洽：多一个少一个都说明帧边界错了，不能"按需截取"。
        var expectedBytes = window.ByteCount;
        if (bc != expectedBytes)
            throw DriverException.ProtocolError("modbus", "decode",
                $"byte count {bc} does not match window {expectedBytes}");

        if (response.Length < 2 + bc)
            throw DriverException.ProtocolError("modbus", "decode",
                $"truncated payload: have {response.Length - 2}, need {bc}");

        var data = response.Slice(2, bc);
        var result = new List<RawSample>(arr.Length);

        foreach (var p in arr)
        {
            var rel = p.Address.Offset - window.Start;

            if (window.IsBits)
            {
                if (rel < 0 || rel / 8 >= data.Length)
                {
                    result.Add(Bad(p.Id, Quality.AddressError, "bit offset out of range"));
                    continue;
                }
                var v = (data[rel / 8] & (1 << (rel % 8))) != 0;
                result.Add(new RawSample(p.Id, DataValue.Boolean(v), Quality.Good, null));
                continue;
            }

            var n = Math.Max(1, p.Length) * 2;
            var off = rel * 2;
            if (off < 0 || off + n > data.Length)
            {
                result.Add(Bad(p.Id, Quality.AddressError, "register window out of range"));
                continue;
            }

            // 字区里的 Bool：按位取或按字非零。
            if (p.DataType == DataType.Bool)
            {
                var word = BinaryPrimitives.ReadUInt16BigEndian(data.Slice(off, 2));
                var v = p.Address.Bit is >= 0 and <= 15
                    ? (word & (1 << p.Address.Bit)) != 0
                    : word != 0;
                result.Add(new RawSample(p.Id, DataValue.Boolean(v), Quality.Good, null));
                continue;
            }

            try
            {
                var v = ValueCodec.Decode(data.Slice(off, n), p.DataType, p.ByteOrder);
                result.Add(new RawSample(p.Id, v, Quality.Good, null));
            }
            catch (Exception ex)
            {
                result.Add(Bad(p.Id, Quality.Bad, ex.Message));
            }
        }

        return result;
    }

    public byte[] EncodeWrite(WriteCommand cmd)
    {
        var p = cmd.Point;
        if (p.Address.Kind != AddressKind.Register)
            throw DriverException.Unsupported("modbus", "encode", "only register addresses are writable");
        if (p.Address.Offset is < 0 or > 0xFFFF)
            throw DriverException.Address("modbus", "encode", $"offset {p.Address.Offset} out of range");

        var area = (p.Address.Area ?? string.Empty).Trim().ToLowerInvariant();

        if (p.DataType == DataType.Bool)
        {
            if (area is not ("coil" or "coils"))
                throw DriverException.Unsupported("modbus", "encode",
                    $"bool write requires coil area, got '{area}'");

            var pdu = new byte[5];
            pdu[0] = ModbusConstants.FcWriteSingleCoil;
            BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(1, 2), (ushort)p.Address.Offset);
            BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(3, 2),
                cmd.Value.AsBool() ? (ushort)0xFF00 : (ushort)0x0000);
            return pdu;
        }

        var (_, _, isReadOnly) = ModbusConstants.FunctionForArea(area);
        if (isReadOnly)
            throw DriverException.Unsupported("modbus", "encode", $"area '{area}' is read-only");

        var data = ValueCodec.Encode(cmd.Value, p.DataType, p.ByteOrder, Math.Max(1, p.Length));

        if (data.Length == 2)
        {
            var pdu = new byte[5];
            pdu[0] = ModbusConstants.FcWriteSingleRegister;
            BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(1, 2), (ushort)p.Address.Offset);
            pdu[3] = data[0];
            pdu[4] = data[1];
            return pdu;
        }

        var regs = data.Length / 2;
        if (regs > ModbusConstants.MaxWriteRegisters)
            throw DriverException.Unsupported("modbus", "encode",
                $"{regs} registers exceeds write limit {ModbusConstants.MaxWriteRegisters}");

        var multi = new byte[6 + data.Length];
        multi[0] = ModbusConstants.FcWriteMultipleRegs;
        BinaryPrimitives.WriteUInt16BigEndian(multi.AsSpan(1, 2), (ushort)p.Address.Offset);
        BinaryPrimitives.WriteUInt16BigEndian(multi.AsSpan(3, 2), (ushort)regs);
        multi[5] = (byte)data.Length;
        data.CopyTo(multi.AsSpan(6));
        return multi;
    }

    public WriteResult DecodeWrite(WriteCommand cmd, ReadOnlySpan<byte> response)
    {
        if (response.Length == 0)
            throw DriverException.ProtocolError("modbus", "decode-write", "empty response");

        if ((response[0] & 0x80) != 0)
            throw ExceptionError(response, "write");

        var expected = cmd.Point.DataType == DataType.Bool
            ? ModbusConstants.FcWriteSingleCoil
            : cmd.Point.Length <= 1 ? ModbusConstants.FcWriteSingleRegister : ModbusConstants.FcWriteMultipleRegs;

        if (response[0] != expected)
            throw DriverException.ProtocolError("modbus", "decode-write",
                $"function echo 0x{response[0]:x2}, want 0x{expected:x2}");

        if (response.Length < 5)
            throw DriverException.ProtocolError("modbus", "decode-write", "truncated write response");

        // 回显的地址与值必须与请求一致；不一致说明响应串了事务。
        var addr = BinaryPrimitives.ReadUInt16BigEndian(response.Slice(1, 2));
        if (addr != (ushort)cmd.Point.Address.Offset)
            throw DriverException.ProtocolError("modbus", "decode-write",
                $"address echo {addr}, want {cmd.Point.Address.Offset}");

        return new WriteResult(cmd.Point.Id, true, Quality.Good, DateTimeOffset.UtcNow);
    }

    private static DriverException ExceptionError(ReadOnlySpan<byte> response, string op)
    {
        var code = response.Length >= 2 ? response[1] : (byte)0;
        return code switch
        {
            0x01 => DriverException.Unsupported("modbus", op, "exception 01: illegal function"),
            0x02 => DriverException.Address("modbus", op, "exception 02: illegal data address"),
            0x03 => DriverException.ProtocolError("modbus", op, "exception 03: illegal data value"),
            0x04 => new DriverException(ErrorKind.Connection, "modbus", op,
                "exception 04: slave device failure", temporary: true),
            0x05 => new DriverException(ErrorKind.Connection, "modbus", op,
                "exception 05: acknowledge", temporary: true),
            0x06 => new DriverException(ErrorKind.Connection, "modbus", op,
                "exception 06: slave busy", temporary: true),
            0x08 => new DriverException(ErrorKind.Connection, "modbus", op,
                "exception 08: memory parity error", temporary: true),
            0x0A => DriverException.Address("modbus", op, "exception 0A: gateway path unavailable"),
            0x0B => DriverException.Timeout("modbus", op, "exception 0B: gateway target failed to respond"),
            _ => DriverException.ProtocolError("modbus", op, $"exception 0x{code:x2}"),
        };
    }

    private static RawSample Bad(string point, Quality q, string msg)
        => new(point, null, q, null, msg);
}