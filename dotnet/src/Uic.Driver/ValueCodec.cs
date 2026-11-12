using System.Buffers.Binary;
using Uic.Domain;

namespace Uic.Driver;

/// <summary>
/// 值编解码。字节序四态（ABCD/CDAB/BADC/DCBA）是现场最容易被忽略、
/// 也最难在生产环境发现的问题：同一个浮点数在两种字节序下可能都「看起来正常」。
///
/// 放在驱动契约层而不是某个协议包里：Modbus、S7 以及后面要接的协议在线上都是
/// 「大端 16 位寄存器序列」，字节序重排与类型解释的规则必须只有一份——
/// 每个协议各抄一遍，迟早出现「Modbus 的 float32 对得上、S7 的对不上」。
/// </summary>
public static class ValueCodec
{
    /// <summary>
    /// 哪些类型真的受字节序影响。
    ///
    /// 只有「由多个 16 位寄存器拼出来的数值」需要重排：Int32/Float64/DateTime 这类。
    /// String、Bytes 是**原样字节流**，字节序对它们没有意义——但设备级 byteOrder 常常
    /// 只是为 float32 设成 cdab，一设上去该设备上所有字符串点位就会错位（"ABCD" 读成
    /// "CDAB"），而且读回来仍然「像是有值」，比直接报错难发现得多；写方向则更危险，
    /// 错位的字节会真的下发给设备。
    /// </summary>
    public static bool ReordersBytes(DataType type) => type is not (DataType.String or DataType.Bytes);

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
        => Decode(raw, type, order, 1);

    /// <summary>
    /// 解码。<paramref name="count"/> &gt; 1 时把字节按元素宽度切成定长数组。
    ///
    /// 每个元素单独套一次 <see cref="Normalize"/>：现场设备对「一组 float32」通常
    /// 是逐元素交换字内字节，而不是把整段字节整体反转——后者会把相邻元素搅在一起，
    /// 得到的数值仍然「像正常读数」，比直接报错更难发现。
    /// </summary>
    public static DataValue Decode(ReadOnlySpan<byte> raw, DataType type, ByteOrder order, int count)
    {
        var elements = Math.Max(1, count);

        // 字符串/字节流不受字序影响：设备级 byteOrder 为 float32 设成 cdab 时，
        // 这里的顺序必须是「不重排」，否则整台设备的文本点位会逐字符错位。
        var reorder = ReordersBytes(type);

        if (elements == 1)
        {
            var single = raw.ToArray();
            if (reorder) Normalize(single, order);
            return DecodeElement(single, type);
        }

        if (!type.SupportsArray())
            throw new NotSupportedException($"type {type} cannot be decoded as an array");

        var unitBytes = ElementBytes(type);
        if (raw.Length < unitBytes * elements)
            throw new FormatException(
                $"array needs {unitBytes * elements} bytes, got {raw.Length}");

        var values = new double[elements];
        for (var i = 0; i < elements; i++)
        {
            var slice = raw.Slice(i * unitBytes, unitBytes).ToArray();
            if (reorder) Normalize(slice, order);
            values[i] = DecodeElement(slice, type).AsNumber();
        }

        return DataValue.Numbers(type, values);
    }

    /// <summary>单个元素的线上字节宽度。</summary>
    public static int ElementBytes(DataType type) => type switch
    {
        DataType.Bool or DataType.Int16 or DataType.UInt16 or DataType.Bcd16 => 2,
        DataType.Int32 or DataType.UInt32 or DataType.Float32 or DataType.Bcd32 => 4,
        DataType.Int64 or DataType.UInt64 or DataType.Float64 or DataType.DateTime => 8,
        _ => throw new NotSupportedException($"type {type} has no fixed element width"),
    };

    private static DataValue DecodeElement(byte[] buf, DataType type) => type switch
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

    /// <summary>
    /// 把值编码成 <paramref name="registers"/> 个寄存器（偶数长度）的线上字节。
    /// 值的 <see cref="DataValue.Array"/> 非空时按数组编码，每个元素各自套一次字节序变换。
    /// </summary>
    public static byte[] Encode(DataValue value, DataType type, ByteOrder order, int registers)
    {
        if (value.Array is { } array) return EncodeArray(array, type, order, registers);

        var n = type switch
        {
            DataType.Bool => 2,
            DataType.String or DataType.Bytes => Math.Max(1, registers) * 2,
            _ => Math.Max(type.Registers(), Math.Max(1, registers)) * 2,
        };

        var buf = new byte[n];
        EncodeElement(buf, value, type);

        // 编码方向要逆变换：Normalize 把线上顺序转成大端，这里把大端转回线上顺序。
        // 字符串/字节流按原样下发——重排等于把内容改成了另一个值。
        if (ReordersBytes(type)) Normalize(buf, order);
        return buf;
    }

    private static byte[] EncodeArray(
        IReadOnlyList<double> values, DataType type, ByteOrder order, int registers)
    {
        if (!type.SupportsArray())
            throw new NotSupportedException($"type {type} cannot be encoded as an array");

        var unit = ElementBytes(type);
        var n = values.Count * unit;

        // 请求里的元素个数必须与点位声明的寄存器数一致。不一致就报错：
        // 按任意一方补齐都会写出「多一个 0 或少一个值」的帧，回读时才发现。
        if (registers > 0 && registers * 2 != n)
            throw new FormatException(
                $"array of {values.Count} x {type} needs {n / 2} registers, got {registers}");

        var buf = new byte[n];
        for (var i = 0; i < values.Count; i++)
        {
            var slice = buf.AsSpan(i * unit, unit);
            EncodeElement(slice, DataValue.Number(type, values[i]), type);
            Normalize(slice, order);
        }

        return buf;
    }

    /// <summary>把单个元素写进 <paramref name="buf"/>（大端，尚未套字节序变换）。</summary>
    private static void EncodeElement(Span<byte> buf, DataValue value, DataType type)
    {
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
                EncodeBcd(buf[..2], (long)value.AsNumber());
                break;
            case DataType.Bcd32:
                EncodeBcd(buf[..4], (long)value.AsNumber());
                break;
            case DataType.String:
                var bytes = System.Text.Encoding.ASCII.GetBytes(value.Text ?? string.Empty);
                bytes.AsSpan(0, Math.Min(bytes.Length, buf.Length)).CopyTo(buf);
                break;
            case DataType.Bytes:
                var raw = Convert.FromBase64String(value.Text ?? string.Empty);
                raw.AsSpan(0, Math.Min(raw.Length, buf.Length)).CopyTo(buf);
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
