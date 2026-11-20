using System.Buffers.Binary;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Codec.Bacnet;

/// <summary>
/// BACnet 应用标签（Application Tags，ASHRAE 135 §20.2）编解码。
///
/// 标签头是 1 字节：高 4 位标签号、bit3 是 class（应用标签为 0）、低 3 位是
/// length/value/type（LVT）：
/// - 0..4：载荷就是这么多字节；
/// - 5：后面跟扩展长度（1 字节 ≤253，254 后跟 2 字节，255 后跟 4 字节）；
/// - 6/7：构造类型的打开/关闭标签，只对上下文标签合法。
///
/// 长度 ≥5 时**必须**走扩展形式。把它当成「最多 4 字节」是这里最容易犯的错：
/// 4 个字符以上的字符串、6 字节以上的 octet-string 全部编错，
/// 而解析端遇到 LVT=5 当成非法会把自己发的报文也判坏。
///
/// 与 <see cref="Uic.Codec.ValueCodec"/> 的分工：ValueCodec 管「平台 DataType ↔ 裸字节」，
/// 本类管「裸字节 ↔ BACnet 标签」。编码时按 DataType 选标签，解码时先剥标签再按类型校验。
///
/// 不实现：bit-string、日期/时间、构造类型的应用值。遇到明确报错，不猜长度——
/// 猜错的表现是「读到后面一个属性」或「值错位但质量仍是 good」。
/// </summary>
public static class BacnetTags
{
    /// <summary>
    /// 把「应用标签号 + 载荷」封装成完整字节序列。供 WriteProperty 的值、
    /// I-Am 的 max-apdu/vendor 等所有应用标签字段使用，避免各处自己拼头字节。
    /// </summary>
    public static byte[] ApplicationTag(byte tagNumber, ReadOnlySpan<byte> body)
    {
        var headerLength = body.Length switch
        {
            <= 4 => 1,
            <= 253 => 2,
            <= 65535 => 4,
            _ => 6,
        };

        var result = new byte[headerLength + body.Length];
        var offset = 0;

        if (body.Length <= 4)
        {
            result[offset++] = (byte)((tagNumber << 4) | body.Length);
        }
        else if (body.Length <= 253)
        {
            result[offset++] = (byte)((tagNumber << 4) | 5);
            result[offset++] = (byte)body.Length;
        }
        else if (body.Length <= 65535)
        {
            result[offset++] = (byte)((tagNumber << 4) | 5);
            result[offset++] = 254;
            result[offset++] = (byte)(body.Length >> 8);
            result[offset++] = (byte)body.Length;
        }
        else
        {
            result[offset++] = (byte)((tagNumber << 4) | 5);
            result[offset++] = 255;
            result[offset++] = (byte)(body.Length >> 24);
            result[offset++] = (byte)(body.Length >> 16);
            result[offset++] = (byte)(body.Length >> 8);
            result[offset++] = (byte)body.Length;
        }

        body.CopyTo(result.AsSpan(offset));
        return result;
    }

    /// <summary>把一个平台值编成「标签头 + 载荷」的字节序列，供 WriteProperty 使用。</summary>
    public static byte[] EncodeValue(DataValue value, DataType dataType)
    {
        return dataType switch
        {
            // boolean 的值在标签头的 LVT 位里：0x10 = false，0x11 = true，没有载荷。
            DataType.Bool => new[]
            {
                (byte)((BacnetConstants.TagBoolean << 4) | (value.AsBool() ? 1 : 0)),
            },
            DataType.UInt16 or DataType.UInt32 => ApplicationTag(
                BacnetConstants.TagUnsigned, MinimalUnsigned(RequireUnsigned(value.AsNumber()))),
            DataType.Int16 or DataType.Int32 => ApplicationTag(
                BacnetConstants.TagSigned, MinimalSigned(RequireSigned(value.AsNumber()))),
            DataType.Float32 => ApplicationTag(BacnetConstants.TagReal, Real((float)value.AsNumber())),
            DataType.String => ApplicationTag(
                BacnetConstants.TagCharacterString, CharacterString(value.Text ?? string.Empty)),
            _ => throw DriverException.Unsupported("bacnet", "encode",
                $"data type {dataType} has no BACnet application tag in this build"),
        };
    }

    private static ulong RequireUnsigned(double number)
    {
        if (number < 0 || number > uint.MaxValue || Math.Abs(number % 1) > double.Epsilon)
            throw DriverException.Address("bacnet", "encode",
                $"value {number} is not an unsigned 32-bit integer; BACnet unsigned tags carry uint32");
        return (ulong)number;
    }

    private static long RequireSigned(double number)
    {
        if (number < int.MinValue || number > int.MaxValue || Math.Abs(number % 1) > double.Epsilon)
            throw DriverException.Address("bacnet", "encode",
                $"value {number} is not a signed 32-bit integer; BACnet signed tags carry int32");
        return (long)number;
    }

    private static byte[] Real(float value)
    {
        var bytes = new byte[4];
        BinaryPrimitives.WriteSingleBigEndian(bytes, value);
        return bytes;
    }

    /// <summary>character-string 载荷：1 字节字符集（3 = UTF-8）+ 正文。</summary>
    private static byte[] CharacterString(string text)
    {
        var body = System.Text.Encoding.UTF8.GetBytes(text);
        var payload = new byte[body.Length + 1];
        payload[0] = 3; // 字符集：ISO 10646 (UTF-8)
        body.CopyTo(payload, 1);
        return payload;
    }

    /// <summary>
    /// 剥掉一个应用标签，返回「标签号、载荷、消耗的字节数」。
    ///
    /// 消耗字节数是必需的：ReadProperty 的 ACK 里值后面还有关闭标签，
    /// 按自己以为的长度去切会把关闭标签当成值的一部分。
    /// </summary>
    public static (byte Tag, byte[] Body, int Consumed) DecodeTag(ReadOnlySpan<byte> tagged)
    {
        if (tagged.IsEmpty)
            throw DriverException.ProtocolError("bacnet", "decode", "empty tagged value");

        var header = tagged[0];
        var tag = (byte)(header >> 4);
        var lvt = header & 0x07;

        if ((header & BacnetConstants.ContextTagClassBit) != 0)
            throw DriverException.ProtocolError("bacnet", "decode",
                $"expected an application tag, got context tag {tag}");

        switch (tag)
        {
            case BacnetConstants.TagNull:
                return (tag, Array.Empty<byte>(), 1);

            // boolean 的载荷就在 LVT 里：0 = false，非 0 = true，没有后续字节。
            case BacnetConstants.TagBoolean:
                return (tag, new[] { lvt == 0 ? (byte)0 : (byte)1 }, 1);
        }

        // 应用标签没有构造形式，LVT=6/7 一定是对端发错了。
        if (lvt > 5)
            throw DriverException.ProtocolError("bacnet", "decode",
                $"application tag {tag} uses opening/closing LVT {lvt}, which is only valid for context tags");

        var offset = 1;
        int length;

        if (lvt < 5)
        {
            length = lvt;
        }
        else
        {
            length = ReadExtendedLength(tagged, ref offset);
        }

        if (tagged.Length < offset + length)
            throw DriverException.ProtocolError("bacnet", "decode",
                $"tagged value declares {length} byte(s) of payload but only " +
                $"{tagged.Length - offset} remain");

        return (tag, tagged.Slice(offset, length).ToArray(), offset + length);
    }

    private static int ReadExtendedLength(ReadOnlySpan<byte> tagged, ref int offset)
    {
        if (tagged.Length <= offset)
            throw DriverException.ProtocolError("bacnet", "decode",
                "extended length (LVT=5) is missing its length byte");

        var first = tagged[offset++];

        if (first <= 253) return first;

        if (first == 254)
        {
            if (tagged.Length < offset + 2)
                throw DriverException.ProtocolError("bacnet", "decode",
                    "extended length (254) is missing its 2-byte length");
            var value = BinaryPrimitives.ReadUInt16BigEndian(tagged.Slice(offset, 2));
            offset += 2;
            return value;
        }

        // first == 255
        if (tagged.Length < offset + 4)
            throw DriverException.ProtocolError("bacnet", "decode",
                "extended length (255) is missing its 4-byte length");

        var wide = BinaryPrimitives.ReadUInt32BigEndian(tagged.Slice(offset, 4));
        offset += 4;

        if (wide > int.MaxValue)
            throw DriverException.ProtocolError("bacnet", "decode",
                $"extended length {wide} exceeds what this build can buffer");

        return (int)wide;
    }

    /// <summary>
    /// 把剥掉标签后的载荷按点位声明的数据类型还原成 DataValue。
    ///
    /// 按「标签能表达什么」而不是「点位声称什么」解码：BACnet 的模拟量对象一律是 REAL，
    /// 点位配成 Int32 很常见。这里允许 REAL → 整数（带整数性检查），但拒绝把字符串读成数字——
    /// 前者是配置口径，后者一定是读错了属性。
    /// </summary>
    public static DataValue DecodeValue(byte tag, ReadOnlySpan<byte> body, DataType dataType)
    {
        switch (tag)
        {
            case BacnetConstants.TagReal:
                if (body.Length != 4)
                    throw DriverException.ProtocolError("bacnet", "decode",
                        $"real value has {body.Length} byte(s), expected 4");
                return DecodeReal(BinaryPrimitives.ReadSingleBigEndian(body), dataType);

            case BacnetConstants.TagDouble:
                if (body.Length != 8)
                    throw DriverException.ProtocolError("bacnet", "decode",
                        $"double value has {body.Length} byte(s), expected 8");
                return DecodeReal((float)BinaryPrimitives.ReadDoubleBigEndian(body), dataType);

            case BacnetConstants.TagUnsigned:
                return DecodeUnsignedInteger(ReadUnsigned(body), dataType);

            case BacnetConstants.TagSigned:
                return DecodeSignedInteger(ReadSigned(body), dataType);

            case BacnetConstants.TagEnumerated:
                return DecodeUnsignedInteger(ReadUnsigned(body), dataType);

            case BacnetConstants.TagBoolean:
                if (dataType != DataType.Bool)
                    throw DriverException.ProtocolError("bacnet", "decode",
                        $"device returned a boolean value but the point declares {dataType}");
                return DataValue.Boolean(body.Length > 0 && body[0] != 0);

            case BacnetConstants.TagCharacterString:
                if (dataType != DataType.String)
                    throw DriverException.ProtocolError("bacnet", "decode",
                        $"device returned a character string but the point declares {dataType}");
                return DataValue.String(ReadCharacterString(body));

            case BacnetConstants.TagNull:
                throw DriverException.ProtocolError("bacnet", "decode",
                    "device returned a null value; the object or property does not carry a value");

            default:
                throw DriverException.Unsupported("bacnet", "decode",
                    $"application tag {tag} has no decoder in this build");
        }
    }

    private static DataValue DecodeReal(float real, DataType dataType)
    {
        // BACnet 用 NaN / ±Inf 表示「无效值」。映射成 0 会把「无效」变成「零度/零压」，
        // 这种假值会一路进历史库与报警判定。
        if (float.IsNaN(real) || float.IsInfinity(real))
            throw DriverException.ProtocolError("bacnet", "decode",
                $"device returned a non-finite real value ({real}); BACnet encodes an invalid value this way");

        if (dataType is DataType.Int16 or DataType.Int32 or DataType.UInt16 or DataType.UInt32)
        {
            var rounded = Math.Round(real, MidpointRounding.AwayFromZero);
            if (Math.Abs(real - rounded) > 0.0001)
                throw DriverException.Address("bacnet", "decode",
                    $"device value {real} is fractional but the point declares integer {dataType}");
            return DecodeSignedInteger((long)rounded, dataType);
        }

        if (dataType == DataType.Bool)
            throw DriverException.ProtocolError("bacnet", "decode",
                $"device returned a real value but the point declares {dataType}");

        return DataValue.Number(DataType.Float32, real);
    }

    private static DataValue DecodeUnsignedInteger(ulong value, DataType dataType)
    {
        var (min, max) = IntegerRange(dataType);
        if (min < 0 || value > (ulong)max)
            throw DriverException.Address("bacnet", "decode",
                $"device value {value} does not fit {dataType} ({min}..{max})");

        if (dataType == DataType.Bool) return DataValue.Boolean(value != 0);
        return DataValue.Number(dataType, value);
    }

    private static DataValue DecodeSignedInteger(long value, DataType dataType)
    {
        var (min, max) = IntegerRange(dataType);
        if (value < min || value > max)
            throw DriverException.Address("bacnet", "decode",
                $"device value {value} does not fit {dataType} ({min}..{max})");

        if (dataType == DataType.Bool) return DataValue.Boolean(value != 0);
        return DataValue.Number(dataType, value);
    }

    private static (long Min, long Max) IntegerRange(DataType dataType) => dataType switch
    {
        DataType.Int16 => (short.MinValue, short.MaxValue),
        DataType.UInt16 => (0L, ushort.MaxValue),
        DataType.Int32 => (int.MinValue, int.MaxValue),
        DataType.UInt32 => (0L, uint.MaxValue),
        DataType.Bool => (0L, 1L),
        DataType.Float32 => (long.MinValue, long.MaxValue),
        _ => throw DriverException.ProtocolError("bacnet", "decode",
            $"integer value cannot be decoded into data type {dataType}"),
    };

    private static string ReadCharacterString(ReadOnlySpan<byte> body)
    {
        if (body.Length == 0)
            throw DriverException.ProtocolError("bacnet", "decode",
                "character string carries no character-set byte");

        var charset = body[0];
        var text = body.Slice(1);

        return charset switch
        {
            0 => System.Text.Encoding.ASCII.GetString(text),
            3 => System.Text.Encoding.UTF8.GetString(text),
            _ => throw DriverException.Unsupported("bacnet", "decode",
                $"character set {charset} is not supported (only 0 = ANSI X3.4 and 3 = UTF-8)"),
        };
    }

    /// <summary>
    /// 量出 <paramref name="buffer"/> 里从 <paramref name="offset"/> 开始的「应用标签 + 载荷」
    /// 占用多少字节，不解读值本身。
    ///
    /// 这是 ReadProperty-ACK、WriteProperty 请求与 ReadPropertyMultiple 三处共用的唯一实现：
    /// 值是一段不透明的应用标签字节，必须先按标签头给出的确切长度跳过，才能找到其后的
    /// 关闭标签。逐字节扫描找关闭标签是错的——载荷是任意字节，完全可能撞上一个合法的
    /// 关闭标签（real 1.25 = 44 3F A0 00 00，第二个字节 0x3F 就是「上下文标签 3 的关闭」）。
    ///
    /// boolean 与 null 的值就在标签头里（LVT 即值），没有载荷字节，按「1 + lvt」算会把
    /// 后面的关闭标签当成载荷吞掉。
    /// </summary>
    public static bool TryMeasureApplicationValue(
        ReadOnlySpan<byte> buffer, int offset, out int tag, out int length, out string? error)
    {
        tag = 0;
        length = 0;
        error = null;

        if (offset >= buffer.Length)
        {
            error = "value is empty";
            return false;
        }

        var header = buffer[offset];
        if ((header & BacnetConstants.ContextTagClassBit) != 0)
        {
            error = "value is a context tag, not an application value";
            return false;
        }

        tag = header >> 4;
        var lvt = header & 0x07;
        var cursor = offset + 1;

        if (tag is BacnetConstants.TagNull or BacnetConstants.TagBoolean)
        {
            length = 1;
            return true;
        }

        if (lvt < 5)
        {
            length = 1 + lvt; // 头 1 字节 + 短载荷
            return true;
        }

        if (cursor >= buffer.Length)
        {
            error = "value declares an extended length but it is missing";
            return false;
        }

        var first = buffer[cursor++];
        int payload;

        if (first <= 253)
        {
            payload = first;
        }
        else if (first == 254)
        {
            if (cursor + 2 > buffer.Length)
            {
                error = "value declares a 2-byte extended length but it is truncated";
                return false;
            }
            payload = BinaryPrimitives.ReadUInt16BigEndian(buffer.Slice(cursor, 2));
            cursor += 2;
        }
        else
        {
            if (cursor + 4 > buffer.Length)
            {
                error = "value declares a 4-byte extended length but it is truncated";
                return false;
            }
            var wide = BinaryPrimitives.ReadUInt32BigEndian(buffer.Slice(cursor, 4));
            if (wide > int.MaxValue)
            {
                error = $"extended length {wide} exceeds what this build can buffer";
                return false;
            }
            payload = (int)wide;
            cursor += 4;
        }

        if (cursor + payload > buffer.Length)
        {
            error = $"value declares {payload} payload byte(s) but only {buffer.Length - cursor} remain";
            return false;
        }

        length = cursor - offset + payload;
        return true;
    }

    // ---- 极小编码：线上整数不带前导零（BACnet 允许任意长度，最短是惯例）----

    private static byte[] MinimalUnsigned(ulong number)
    {
        var bytes = new byte[8];
        BinaryPrimitives.WriteUInt64BigEndian(bytes, number);
        return TrimLeadingZeros(bytes);
    }

    private static byte[] MinimalSigned(long number)
    {
        var bytes = new byte[8];
        BinaryPrimitives.WriteInt64BigEndian(bytes, number);

        var start = 0;
        while (start < bytes.Length - 1)
        {
            var next = bytes[start + 1];
            if (bytes[start] == 0x00 && (next & 0x80) == 0) { start++; continue; }
            if (bytes[start] == 0xFF && (next & 0x80) != 0) { start++; continue; }
            break;
        }
        return bytes[start..];
    }

    private static byte[] TrimLeadingZeros(byte[] bytes)
    {
        var start = 0;
        while (start < bytes.Length - 1 && bytes[start] == 0) start++;
        return bytes[start..];
    }

    private static ulong ReadUnsigned(ReadOnlySpan<byte> body)
        => body.Length switch
        {
            0 => 0UL,
            1 => body[0],
            2 => BinaryPrimitives.ReadUInt16BigEndian(body),
            3 => ((ulong)body[0] << 16) | ((ulong)body[1] << 8) | body[2],
            4 => BinaryPrimitives.ReadUInt32BigEndian(body),
            8 => BinaryPrimitives.ReadUInt64BigEndian(body),
            _ => throw DriverException.ProtocolError("bacnet", "decode",
                $"unsigned payload of {body.Length} byte(s) is not 1/2/3/4/8"),
        };

    private static long ReadSigned(ReadOnlySpan<byte> body)
    {
        var unsigned = ReadUnsigned(body);
        var bits = body.Length * 8;
        if (bits == 0) return 0;

        // 符号扩展：最高位为 1 表示负数。
        var signBit = 1UL << (bits - 1);
        if (bits < 64 && (unsigned & signBit) != 0)
            return (long)(unsigned | (~0UL << bits));
        return (long)unsigned;
    }
}
