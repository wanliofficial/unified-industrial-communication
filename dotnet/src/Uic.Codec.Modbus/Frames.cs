using System.Buffers.Binary;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Codec.Modbus;

/// <summary>
/// 值编解码。字节序四态（ABCD/CDAB/BADC/DCBA）是现场最容易被忽略、
/// 也最难在生产环境发现的问题：同一个浮点数在两种字节序下可能都「看起来正常」。
/// </summary>
public static class ValueCodec
{
    /// <summary>把线上寄存器顺序重排成大端连续字节。自反操作：编码方向再调一次即还原。</summary>
    public static void Normalize(Span<byte> raw, ByteOrder order)
    {
        switch (order)
        {
            case ByteOrder.ABCD:
                return;

            case ByteOrder.CDAB:
                for (var i = 0; i + 3 < raw.Length; i += 4)
                {
                    (raw[i], raw[i + 1], raw[i + 2], raw[i + 3]) =
                        (raw[i + 2], raw[i + 3], raw[i], raw[i + 1]);
                }
                return;

            case ByteOrder.BADC:
                for (var i = 0; i + 1 < raw.Length; i += 2)
                {
                    (raw[i], raw[i + 1]) = (raw[i + 1], raw[i]);
                }
                return;

            case ByteOrder.DCBA:
                raw.Reverse();
                return;

            default:
                throw new ArgumentOutOfRangeException(nameof(order), order, "unknown byte order");
        }
    }

    public static DataValue Decode(ReadOnlySpan<byte> raw, DataType type, ByteOrder order)
    {
        var buf = raw.ToArray();
        Normalize(buf, order);

        return type switch
        {
            DataType.Bool => DataValue.Boolean(buf.Any(b => b != 0)),
            DataType.Int16 => DataValue.Number(type, BinaryPrimitives.ReadInt16BigEndian(buf)),
            DataType.UInt16 => DataValue.Number(type, BinaryPrimitives.ReadUInt16BigEndian(buf)),
            DataType.Int32 => DataValue.Number(type, BinaryPrimitives.ReadInt32BigEndian(buf)),
            DataType.UInt32 => DataValue.Number(type, BinaryPrimitives.ReadUInt32BigEndian(buf)),
            DataType.Int64 => DataValue.Number(type, BinaryPrimitives.ReadInt64BigEndian(buf)),
            DataType.UInt64 => DataValue.Number(type, BinaryPrimitives.ReadUInt64BigEndian(buf)),
            DataType.Float32 => DataValue.Number(type, BinaryPrimitives.ReadSingleBigEndian(buf)),
            DataType.Float64 => DataValue.Number(type, BinaryPrimitives.ReadDoubleBigEndian(buf)),
            DataType.Bcd16 => DataValue.Number(type, DecodeBcd(buf.AsSpan(0, 2))),
            DataType.Bcd32 => DataValue.Number(type, DecodeBcd(buf.AsSpan(0, 4))),
            DataType.String => DataValue.String(
                System.Text.Encoding.ASCII.GetString(buf).TrimEnd('\0', ' ')),
            DataType.Bytes => DataValue.Bytes(buf),
            // 现场常见的「时间寄存器」是 4 个寄存器里的 int64 Unix 毫秒（UTC）。
            DataType.DateTime => DataValue.DateTime(
                DateTimeOffset.FromUnixTimeMilliseconds(BinaryPrimitives.ReadInt64BigEndian(buf))),
            _ => throw new NotSupportedException($"type {type} is not decodable"),
        };
    }

    /// <summary>把值编码成 <paramref name="registers"/> 个寄存器（偶数长度）的线上字节。</summary>
    public static byte[] Encode(DataValue value, DataType type, ByteOrder order, int registers)
    {
        var n = type switch
        {
            DataType.Bool => 2,
            DataType.String or DataType.Bytes => Math.Max(1, registers) * 2,
            _ => Math.Max(type.Registers(), Math.Max(1, registers)) * 2,
        };

        var buf = new byte[n];

        switch (type)
        {
            case DataType.Bool:
                BinaryPrimitives.WriteUInt16BigEndian(buf, value.AsBool() ? (ushort)1 : (ushort)0);
                break;
            case DataType.Int16:
                BinaryPrimitives.WriteInt16BigEndian(buf, (short)value.AsNumber());
                break;
            case DataType.UInt16:
                BinaryPrimitives.WriteUInt16BigEndian(buf, (ushort)value.AsNumber());
                break;
            case DataType.Int32:
                BinaryPrimitives.WriteInt32BigEndian(buf, (int)value.AsNumber());
                break;
            case DataType.UInt32:
                BinaryPrimitives.WriteUInt32BigEndian(buf, (uint)value.AsNumber());
                break;
            case DataType.Int64:
                BinaryPrimitives.WriteInt64BigEndian(buf, (long)value.AsNumber());
                break;
            case DataType.UInt64:
                BinaryPrimitives.WriteUInt64BigEndian(buf, (ulong)value.AsNumber());
                break;
            case DataType.Float32:
                BinaryPrimitives.WriteSingleBigEndian(buf, (float)value.AsNumber());
                break;
            case DataType.Float64:
                BinaryPrimitives.WriteDoubleBigEndian(buf, value.AsNumber());
                break;
            case DataType.Bcd16:
                EncodeBcd(buf.AsSpan(0, 2), (long)value.AsNumber());
                break;
            case DataType.Bcd32:
                EncodeBcd(buf.AsSpan(0, 4), (long)value.AsNumber());
                break;
            case DataType.String:
                var bytes = System.Text.Encoding.ASCII.GetBytes(value.Text ?? string.Empty);
                Array.Copy(bytes, buf, Math.Min(bytes.Length, buf.Length));
                break;
            case DataType.Bytes:
                var raw = Convert.FromBase64String(value.Text ?? string.Empty);
                Array.Copy(raw, buf, Math.Min(raw.Length, buf.Length));
                break;
            case DataType.DateTime:
                // 写不出去的时间宁可报错也不写 0：0 是 1970-01-01，
                // 在趋势图上看起来像一个合法读数，比写入失败更难发现。
                if (!value.TryGetDateTime(out var timestamp))
                    throw new FormatException($"'{value.Text}' is not a valid datetime value");
                BinaryPrimitives.WriteInt64BigEndian(buf, timestamp.ToUnixTimeMilliseconds());
                break;
            default:
                throw new NotSupportedException($"type {type} is not encodable");
        }

        // 编码方向要逆变换：Normalize 把线上顺序转成大端，这里把大端转回线上顺序。
        Normalize(buf, order);
        return buf;
    }

    private static double DecodeBcd(ReadOnlySpan<byte> raw)
    {
        long v = 0;
        foreach (var b in raw)
        {
            var hi = b >> 4;
            var lo = b & 0x0F;
            if (hi > 9 || lo > 9) throw new FormatException($"invalid BCD byte 0x{b:x2}");
            v = v * 100 + hi * 10 + lo;
        }
        return v;
    }

    private static void EncodeBcd(Span<byte> dst, long value)
    {
        if (value < 0) throw new ArgumentOutOfRangeException(nameof(value), "BCD cannot encode negatives");
        for (var i = dst.Length - 1; i >= 0; i--)
        {
            var lo = (int)(value % 10);
            value /= 10;
            var hi = (int)(value % 10);
            value /= 10;
            dst[i] = (byte)((hi << 4) | lo);
        }
        if (value != 0) throw new ArgumentOutOfRangeException(nameof(value), "value does not fit in BCD width");
    }
}

/// <summary>TCP 分帧：MBAP 7 字节 + PDU。</summary>
public static class ModbusTcpFrame
{
    public const int MbapLength = 7;

    public static byte[] Build(ushort transaction, byte unit, ReadOnlySpan<byte> pdu)
    {
        var adu = new byte[MbapLength + pdu.Length];
        BinaryPrimitives.WriteUInt16BigEndian(adu.AsSpan(0, 2), transaction);
        BinaryPrimitives.WriteUInt16BigEndian(adu.AsSpan(2, 2), 0);
        BinaryPrimitives.WriteUInt16BigEndian(adu.AsSpan(4, 2), (ushort)(pdu.Length + 1));
        adu[6] = unit;
        pdu.CopyTo(adu.AsSpan(MbapLength));
        return adu;
    }

    /// <summary>解析完整 ADU；返回事务号、单元号与 PDU。</summary>
    public static (ushort Transaction, ushort Protocol, byte Unit, byte[] Pdu) Parse(ReadOnlySpan<byte> adu)
    {
        if (adu.Length < 8)
            throw DriverException.ProtocolError("modbus", "parse-tcp", "frame shorter than 8 bytes");

        var transaction = BinaryPrimitives.ReadUInt16BigEndian(adu.Slice(0, 2));
        var protocol = BinaryPrimitives.ReadUInt16BigEndian(adu.Slice(2, 2));
        var length = BinaryPrimitives.ReadUInt16BigEndian(adu.Slice(4, 2));

        if (protocol != 0)
            throw DriverException.ProtocolError("modbus", "parse-tcp", $"protocol id {protocol} is not modbus");
        if (length is < 2 or > ModbusConstants.MaxAdu)
            throw DriverException.ProtocolError("modbus", "parse-tcp", $"invalid MBAP length {length}");
        if (adu.Length < 6 + length)
            throw DriverException.ProtocolError("modbus", "parse-tcp",
                $"truncated frame: have {adu.Length}, need {6 + length}");

        var unit = adu[6];
        var pdu = adu.Slice(MbapLength, length - 1).ToArray();
        return (transaction, protocol, unit, pdu);
    }

    /// <summary>从 MBAP 头推算整帧长度（用于流式读取）。</summary>
    public static int TotalLength(ReadOnlySpan<byte> header)
    {
        if (header.Length < 6)
            throw DriverException.ProtocolError("modbus", "parse-tcp", "header shorter than 6 bytes");
        return 6 + BinaryPrimitives.ReadUInt16BigEndian(header.Slice(4, 2));
    }
}

/// <summary>RTU 分帧：站号 + PDU + CRC16（小端）。</summary>
public static class ModbusRtuFrame
{
    public static byte[] Build(byte unit, ReadOnlySpan<byte> pdu)
    {
        var frame = new byte[pdu.Length + 3];
        frame[0] = unit;
        pdu.CopyTo(frame.AsSpan(1));
        var crc = ModbusConstants.Crc16(frame.AsSpan(0, pdu.Length + 1));
        frame[^2] = (byte)(crc & 0xFF);
        frame[^1] = (byte)(crc >> 8);
        return frame;
    }

    public static (byte Unit, byte[] Pdu) Parse(ReadOnlySpan<byte> frame)
    {
        if (frame.Length < 4)
            throw DriverException.ProtocolError("modbus", "parse-rtu", "frame shorter than 4 bytes");

        var crc = ModbusConstants.Crc16(frame[..^2]);
        var got = (ushort)(frame[^2] | (frame[^1] << 8));
        if (crc != got)
            throw DriverException.ProtocolError("modbus", "parse-rtu",
                $"CRC mismatch: got 0x{got:x4}, want 0x{crc:x4}");

        return (frame[0], frame.Slice(1, frame.Length - 3).ToArray());
    }

    /// <summary>已知请求功能码时，推算响应帧总长度（含站号与 CRC）。0 表示无法预判。</summary>
    public static int ExpectedResponseLength(byte requestFunction, byte byteCount)
    {
        if ((requestFunction & 0x80) != 0) return 5;
        return requestFunction switch
        {
            ModbusConstants.FcReadCoils or ModbusConstants.FcReadDiscreteInputs
                or ModbusConstants.FcReadHoldingRegs or ModbusConstants.FcReadInputRegs => 5 + byteCount,
            ModbusConstants.FcWriteSingleCoil or ModbusConstants.FcWriteSingleRegister
                or ModbusConstants.FcWriteMultipleCoils or ModbusConstants.FcWriteMultipleRegs => 8,
            _ => 0,
        };
    }
}

/// <summary>ASCII 分帧：':' + 十六进制 + LRC + CRLF。</summary>
public static class ModbusAsciiFrame
{
    public const byte Start = (byte)':';
    public const int MaxFrameBytes = 513;

    public static string Build(byte unit, ReadOnlySpan<byte> pdu)
    {
        var body = new byte[pdu.Length + 1];
        body[0] = unit;
        pdu.CopyTo(body.AsSpan(1));
        var lrc = ModbusConstants.Lrc(body);

        return ":" + Convert.ToHexString(body) + lrc.ToString("X2") + "\r\n";
    }

    public static (byte Unit, byte[] Pdu) Parse(string frame)
    {
        if (!frame.StartsWith(':') || !frame.EndsWith("\r\n", StringComparison.Ordinal))
            throw DriverException.ProtocolError("modbus", "parse-ascii",
                "frame must start with ':' and end with CRLF");

        var hex = frame[1..^2];
        if (hex.Length == 0 || hex.Length % 2 != 0)
            throw DriverException.ProtocolError("modbus", "parse-ascii", "odd hex length");

        byte[] body;
        try
        {
            body = Convert.FromHexString(hex);
        }
        catch (FormatException ex)
        {
            throw DriverException.ProtocolError("modbus", "parse-ascii", "invalid hex payload", ex);
        }

        if (body.Length < 2)
            throw DriverException.ProtocolError("modbus", "parse-ascii", "frame too short");

        var lrc = ModbusConstants.Lrc(body.AsSpan(0, body.Length - 1));
        if (lrc != body[^1])
            throw DriverException.ProtocolError("modbus", "parse-ascii",
                $"LRC mismatch: got 0x{body[^1]:x2}, want 0x{lrc:x2}");

        return (body[0], body[1..^1]);
    }
}