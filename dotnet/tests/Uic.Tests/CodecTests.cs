using System.Text;
using Uic.Codec.Modbus;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Tests;

/// <summary>
/// 编解码器与分帧的单元断言。金报文覆盖「正常路径的字节」，
/// 这里覆盖「坏帧、越界、异常码」——那些不会出现在正常抓包里的路径。
/// </summary>
internal static class CodecTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("codec/crc16_known_vectors", Crc16));
        tests.Add(new TestCase("codec/lrc_known_vectors", Lrc));
        tests.Add(new TestCase("codec/byte_order_encodes_as_expected", ByteOrderEncodesAsExpected));
        tests.Add(new TestCase("codec/byte_order_roundtrip", ByteOrderRoundtrip));
        tests.Add(new TestCase("codec/bcd_roundtrip", BcdRoundtrip));
        tests.Add(new TestCase("codec/datetime_is_unix_milliseconds", DateTimeIsUnixMilliseconds));
        tests.Add(new TestCase("codec/window_rejects_invalid_spans", WindowRejectsInvalidSpans));
        tests.Add(new TestCase("codec/window_merges_and_measures", WindowMergesAndMeasures));
        tests.Add(new TestCase("codec/write_rejects_readonly_and_bit_mismatch", WriteRejectsReadOnlyAndBitMismatch));
        tests.Add(new TestCase("codec/exception_codes_map_to_kinds", ExceptionCodesMapToKinds));
        tests.Add(new TestCase("codec/decode_rejects_inconsistent_frames", DecodeRejectsInconsistentFrames));
        tests.Add(new TestCase("codec/framing_rejects_corruption", FramingRejectsCorruption));
        tests.Add(new TestCase("codec/read_limits", ReadLimits));
        tests.Add(new TestCase("codec/array_roundtrip_per_element_byte_order", ArrayRoundtripPerElementByteOrder));
        tests.Add(new TestCase("codec/array_encode_rejects_wrong_element_count", ArrayEncodeRejectsWrongElementCount));
        tests.Add(new TestCase("codec/array_decode_requires_enough_bytes", ArrayDecodeRequiresEnoughBytes));
        tests.Add(new TestCase("codec/array_rejects_non_numeric_element_types", ArrayRejectsNonNumericTypes));
    }

    private static Task Crc16(TestContext t)
    {
        // 业界通用的校验向量：ASCII "123456789" 的 Modbus CRC16 = 0x4B37。
        t.Equal((ushort)0x4B37, ModbusConstants.Crc16(Encoding.ASCII.GetBytes("123456789")), "CRC of 123456789");
        t.Equal((ushort)0xFFFF, ModbusConstants.Crc16(ReadOnlySpan<byte>.Empty), "CRC of empty input");

        // 与金报文同源：01 03 0064 0002 → 线上 85 D4（小端）。
        var request = Convert.FromHexString("010300640002");
        t.Equal((ushort)0xD485, ModbusConstants.Crc16(request), "CRC of the golden read request");

        return Task.CompletedTask;
    }

    private static Task Lrc(TestContext t)
    {
        t.Equal((byte)0x96, ModbusConstants.Lrc(Convert.FromHexString("010300640002")), "LRC of the golden read request");
        t.Equal((byte)0xE3, ModbusConstants.Lrc(Convert.FromHexString("010304000A000B")), "LRC of the golden read response");

        // LRC 是补码：任意帧「内容 + LRC」逐字节求和必须为 0。
        var body = Convert.FromHexString("010304000A000B");
        t.Equal(0, (body.Sum(b => b) + ModbusConstants.Lrc(body)) & 0xFF, "body + LRC sums to zero");

        return Task.CompletedTask;
    }

    private static Task ByteOrderEncodesAsExpected(TestContext t)
    {
        var value = DataValue.Number(DataType.Float32, 1.5);

        t.Equal("3FC00000", Hex(ValueCodec.Encode(value, DataType.Float32, ByteOrder.ABCD, 2)), "ABCD");
        t.Equal("00003FC0", Hex(ValueCodec.Encode(value, DataType.Float32, ByteOrder.CDAB, 2)), "CDAB");
        t.Equal("C03F0000", Hex(ValueCodec.Encode(value, DataType.Float32, ByteOrder.BADC, 2)), "BADC");
        t.Equal("0000C03F", Hex(ValueCodec.Encode(value, DataType.Float32, ByteOrder.DCBA, 2)), "DCBA");

        return Task.CompletedTask;
    }

    private static Task ByteOrderRoundtrip(TestContext t)
    {
        foreach (var order in Enum.GetValues<ByteOrder>())
        {
            foreach (var (type, value) in new (DataType, DataValue)[]
                     {
                         (DataType.Int16, DataValue.Number(DataType.Int16, -1234)),
                         (DataType.UInt16, DataValue.Number(DataType.UInt16, 65000)),
                         (DataType.Int32, DataValue.Number(DataType.Int32, -2_000_000)),
                         (DataType.UInt32, DataValue.Number(DataType.UInt32, 4_000_000_000d)),
                         (DataType.Float32, DataValue.Number(DataType.Float32, 3.25)),
                         (DataType.Float64, DataValue.Number(DataType.Float64, -0.125)),
                     })
            {
                var raw = ValueCodec.Encode(value, type, order, type.Registers());
                var decoded = ValueCodec.Decode(raw, type, order);

                t.Equal(type, decoded.Type, $"{order}/{type}: type");
                t.Close(value.AsNumber(), decoded.AsNumber(), 1e-6, $"{order}/{type}: value");
            }
        }

        return Task.CompletedTask;
    }

    private static Task BcdRoundtrip(TestContext t)
    {
        t.Equal("1234", Hex(ValueCodec.Encode(DataValue.Number(DataType.Bcd16, 1234), DataType.Bcd16, ByteOrder.ABCD, 1)), "bcd16 1234");
        t.Equal("99999999", Hex(ValueCodec.Encode(DataValue.Number(DataType.Bcd32, 99_999_999), DataType.Bcd32, ByteOrder.ABCD, 2)), "bcd32");

        t.Equal(1234d, ValueCodec.Decode(Convert.FromHexString("1234"), DataType.Bcd16, ByteOrder.ABCD).AsNumber(), "decode bcd16");

        // 非 BCD 半字节必须报错，不能静默造一个数出来。
        t.Throws<FormatException>(
            () => ValueCodec.Decode(Convert.FromHexString("1A34"), DataType.Bcd16, ByteOrder.ABCD),
            "bcd with a non-decimal nibble");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 时间型点位按现场最常见的约定编解码：4 个寄存器里的 int64 Unix 毫秒（UTC）。
    /// 用 Montgomery 之外的固定时刻断言字节，确保「大端 + 毫秒」不会被悄悄改成秒或本地时间。
    /// </summary>
    private static Task DateTimeIsUnixMilliseconds(TestContext t)
    {
        var local = new DateTimeOffset(2026, 11, 5, 20, 47, 46, 123, TimeSpan.FromHours(8));
        var value = DataValue.DateTime(local);

        t.Equal(DataType.DateTime, value.Type, "datetime factory sets the type");
        t.Equal("2026-11-05T12:47:46.123Z", value.Text, "the value is normalized to ISO-8601 UTC");
        t.Equal(4, DataType.DateTime.Registers(), "datetime occupies four registers");
        t.Ok(!DataType.DateTime.IsNumeric(), "datetime is not a numeric type, so scaling does not apply");

        var raw = ValueCodec.Encode(value, DataType.DateTime, ByteOrder.ABCD, 4);
        t.Equal("000001A1ABC041CB", Hex(raw), "int64 unix milliseconds, big-endian");

        var decoded = ValueCodec.Decode(raw, DataType.DateTime, ByteOrder.ABCD);
        t.Equal("2026-11-05T12:47:46.123Z", decoded.Text, "decode returns the same instant");
        t.Ok(decoded.Matches(value), "encoder and decoder agree");

        // 四态字节序同样适用于 64 位时间值；只验证往返，具体排列与 Int64 一致。
        foreach (var order in Enum.GetValues<ByteOrder>())
        {
            var round = ValueCodec.Decode(
                ValueCodec.Encode(value, DataType.DateTime, order, 4), DataType.DateTime, order);
            t.Ok(round.Matches(value), $"{order}: datetime round-trips");
        }

        // 文本坏了必须报错：写成 0 会变成 1970-01-01，在趋势图上看起来像合法读数。
        t.Throws<FormatException>(
            () => ValueCodec.Encode(
                new DataValue { Type = DataType.DateTime, Text = "not-a-timestamp" },
                DataType.DateTime, ByteOrder.ABCD, 4),
            "an invalid datetime is rejected instead of encoded as the epoch");

        return Task.CompletedTask;
    }

    private static Task WindowRejectsInvalidSpans(TestContext t)
    {
        PointRef Point(string id, string area, int offset, DataType type = DataType.UInt16, int length = 1)
            => new(id, Address.Register(area, offset, length), type, ByteOrder.ABCD, length);

        t.Throws<DriverException>(() => ModbusCodec.Window(Array.Empty<PointRef>()), "empty span");

        // 混合区不能合并：同一帧里既有线圈又有保持寄存器，从站会按第一个功能码整帧拒绝。
        t.Throws<DriverException>(
            () => ModbusCodec.Window(new[] { Point("a", "holding", 0), Point("b", "coil", 0) }),
            "mixed areas");

        t.Throws<DriverException>(
            () => ModbusCodec.Window(new[] { Point("a", "holding", -1) }),
            "negative offset");

        // 126 个寄存器超过协议上限 125。
        t.Throws<DriverException>(
            () => ModbusCodec.Window(new[]
            {
                Point("a", "holding", 0, DataType.UInt16, 1),
                Point("b", "holding", 125, DataType.UInt16, 1),
            }),
            "window larger than 125 registers");

        var noArea = new PointRef("a", new Address { Kind = AddressKind.Register }, DataType.UInt16, ByteOrder.ABCD, 1);
        t.Throws<DriverException>(() => ModbusCodec.Window(new[] { noArea }), "missing area");

        return Task.CompletedTask;
    }

    private static Task WindowMergesAndMeasures(TestContext t)
    {
        PointRef Point(string id, string area, int offset, int length = 1)
            => new(id, Address.Register(area, offset, length), DataType.UInt16, ByteOrder.ABCD, length);

        var merged = ModbusCodec.Window(new[] { Point("a", "holding", 10), Point("b", "holding", 12) });
        t.Equal(10, merged.Start, "start");
        t.Equal(3, merged.Quantity, "quantity covers the gap");
        t.Equal(6, merged.ByteCount, "byte count");

        var bits = ModbusCodec.Window(new[]
        {
            new PointRef("a", Address.Register("coil", 0), DataType.Bool, ByteOrder.ABCD, 1),
            new PointRef("b", Address.Register("coil", 9), DataType.Bool, ByteOrder.ABCD, 1),
        });
        t.Equal(10, bits.Quantity, "bit quantity");
        t.Equal(2, bits.ByteCount, "two bytes for ten bits");

        return Task.CompletedTask;
    }

    private static Task WriteRejectsReadOnlyAndBitMismatch(TestContext t)
    {
        var codec = new ModbusCodec("t");

        var inputPoint = new PointRef("p", Address.Register("input", 0), DataType.UInt16, ByteOrder.ABCD, 1);
        t.Throws<DriverException>(
            () => codec.EncodeWrite(new WriteCommand(inputPoint, DataValue.Number(DataType.UInt16, 1))),
            "write to input registers");

        var holdingBool = new PointRef("p", Address.Register("holding", 0), DataType.Bool, ByteOrder.ABCD, 1);
        t.Throws<DriverException>(
            () => codec.EncodeWrite(new WriteCommand(holdingBool, DataValue.Boolean(true))),
            "bool write outside coil area");

        // 32 位写必须走 FC16，且寄存器数不能超限。
        var wide = new PointRef("p", Address.Register("holding", 4), DataType.UInt64, ByteOrder.ABCD, 4);
        t.Equal("1000040004", Hex(codec.EncodeWrite(new WriteCommand(wide, DataValue.Number(DataType.UInt64, 1)))[..5]),
            "64-bit write uses FC16");

        return Task.CompletedTask;
    }

    private static Task ExceptionCodesMapToKinds(TestContext t)
    {
        var codec = new ModbusCodec("t");
        var points = new[]
        {
            new PointRef("p", Address.Register("holding", 0), DataType.UInt16, ByteOrder.ABCD, 1),
        };

        var expected = new (byte Code, ErrorKind Kind)[]
        {
            (0x01, ErrorKind.Unsupported),
            (0x02, ErrorKind.Address),
            (0x03, ErrorKind.Protocol),
            (0x04, ErrorKind.Connection),
            (0x05, ErrorKind.Connection),
            (0x06, ErrorKind.Connection),
            (0x08, ErrorKind.Connection),
            (0x0A, ErrorKind.Address),
            (0x0B, ErrorKind.Timeout),
        };

        foreach (var (code, kind) in expected)
        {
            var response = new byte[] { 0x83, code };

            try
            {
                codec.DecodeRead(points, response);
                t.Fail($"exception 0x{code:x2} should throw");
            }
            catch (DriverException ex)
            {
                t.Equal(kind, ex.Kind, $"exception 0x{code:x2}: kind");
            }
        }

        return Task.CompletedTask;
    }

    private static Task DecodeRejectsInconsistentFrames(TestContext t)
    {
        var codec = new ModbusCodec("t");
        var points = new[]
        {
            new PointRef("a", Address.Register("holding", 0), DataType.UInt16, ByteOrder.ABCD, 1),
            new PointRef("b", Address.Register("holding", 1), DataType.UInt16, ByteOrder.ABCD, 1),
        };

        // 字节数比窗口少：帧边界错了，不能「按需截取」。
        t.Throws<DriverException>(
            () => codec.DecodeRead(points, Convert.FromHexString("03020001")),
            "byte count smaller than window");

        // 功能码回显不对：说明响应串了事务。
        t.Throws<DriverException>(
            () => codec.DecodeRead(points, Convert.FromHexString("040400010002")),
            "function echo mismatch");

        // 有效载荷被截断。
        t.Throws<DriverException>(
            () => codec.DecodeRead(points, Convert.FromHexString("03040001")),
            "truncated payload");

        // 写响应回显的地址不一致。
        var point = new PointRef("a", Address.Register("holding", 5), DataType.UInt16, ByteOrder.ABCD, 1);
        var command = new WriteCommand(point, DataValue.Number(DataType.UInt16, 7));
        t.Throws<DriverException>(
            () => codec.DecodeWrite(command, Convert.FromHexString("0600060007")),
            "write address echo mismatch");

        return Task.CompletedTask;
    }

    private static Task FramingRejectsCorruption(TestContext t)
    {
        t.Throws<DriverException>(
            () => ModbusTcpFrame.Parse(Convert.FromHexString("00010000000601")),
            "short TCP frame");

        t.Throws<DriverException>(
            () => ModbusTcpFrame.Parse(Convert.FromHexString("000100010006010300640002")),
            "non-zero protocol id");

        t.Throws<DriverException>(
            () => ModbusTcpFrame.Parse(Convert.FromHexString("000100000001010300640002")),
            "MBAP length below the minimum");

        // CRC 故意改一位。
        t.Throws<DriverException>(
            () => ModbusRtuFrame.Parse(Convert.FromHexString("010304000A000B9BF7")),
            "RTU CRC mismatch");

        t.Throws<DriverException>(
            () => ModbusAsciiFrame.Parse(":010304000A000BE4\r\n"),
            "ASCII LRC mismatch");

        t.Throws<DriverException>(
            () => ModbusAsciiFrame.Parse("010304000A000BE3"),
            "ASCII frame without guard characters");

        return Task.CompletedTask;
    }

    private static Task ReadLimits(TestContext t)
    {
        var codec = new ModbusCodec("t");

        // 单次读 125 个寄存器是上限，再多必须由规划层切开。
        var atLimit = new[]
        {
            new PointRef("a", Address.Register("holding", 0), DataType.UInt16, ByteOrder.ABCD, 1),
            new PointRef("b", Address.Register("holding", 124), DataType.UInt16, ByteOrder.ABCD, 1),
        };

        var pdu = codec.EncodeRead(atLimit);
        t.Equal("030000007D", Hex(pdu), "125-register read request");

        var bits = new[]
        {
            new PointRef("a", Address.Register("coil", 0), DataType.Bool, ByteOrder.ABCD, 1),
            new PointRef("b", Address.Register("coil", 1999), DataType.Bool, ByteOrder.ABCD, 1),
        };

        t.Equal("01000007D0", Hex(codec.EncodeRead(bits)), "2000-bit read request");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 数组编解码：每个元素单独套一次字节序变换。
    ///
    /// 这条用例的重点不是「能往返」，而是「元素边界不能被字节序搅乱」：
    /// 如果实现把整段字节整体反转（DCBA 的朴素做法），两个 float 会互相交换高低字，
    /// 解出来的值仍然「像正常读数」，比直接报错更难发现。
    /// </summary>
    private static Task ArrayRoundtripPerElementByteOrder(TestContext t)
    {
        var values = new[] { 1.5, -2.25, 0.125 };

        foreach (var order in Enum.GetValues<ByteOrder>())
        {
            var value = DataValue.Numbers(DataType.Float32, values);
            var raw = ValueCodec.Encode(value, DataType.Float32, order, 2 * values.Length);
            var decoded = ValueCodec.Decode(raw, DataType.Float32, order, values.Length);

            t.Ok(decoded.IsArray, $"{order}: decoded value is an array");
            t.Equal(values.Length, decoded.Count, $"{order}: element count");

            for (var i = 0; i < values.Length; i++)
            {
                t.Close(values[i], decoded.NumberAt(i), 1e-6, $"{order}: element {i}");
            }
        }

        // ABCD 的线上字节必须是逐元素的大端拼接，与手工计算一致。
        t.Equal("3FC00000C01000003E000000", Hex(ValueCodec.Encode(
            DataValue.Numbers(DataType.Float32, values), DataType.Float32, ByteOrder.ABCD, 6)),
            "ABCD array bytes");

        // CDAB 只交换每个元素内部的两个字，元素顺序不变。
        t.Equal("00003FC00000C01000003E00", Hex(ValueCodec.Encode(
            DataValue.Numbers(DataType.Float32, values), DataType.Float32, ByteOrder.CDAB, 6)),
            "CDAB array bytes are swapped per element");

        // uint16 数组占 3 个寄存器，读窗口按元素个数算。
        var words = DataValue.Numbers(DataType.UInt16, new double[] { 10, 11, 12 });
        t.Equal("000A000B000C", Hex(ValueCodec.Encode(words, DataType.UInt16, ByteOrder.ABCD, 3)),
            "uint16 array bytes");

        return Task.CompletedTask;
    }

    private static Task ArrayEncodeRejectsWrongElementCount(TestContext t)
    {
        // 少一个元素就是少写一个寄存器：静默补齐会写成「3 个值 + 1 个 0」，
        // 回读时才发现，现场很难判断是写入错了还是设备截断了。
        t.Throws<FormatException>(
            () => ValueCodec.Encode(
                DataValue.Numbers(DataType.UInt16, new double[] { 1, 2 }),
                DataType.UInt16, ByteOrder.ABCD, registers: 3),
            "element count does not match the declared registers");

        return Task.CompletedTask;
    }

    private static Task ArrayDecodeRequiresEnoughBytes(TestContext t)
    {
        t.Throws<FormatException>(
            () => ValueCodec.Decode(Convert.FromHexString("000A000B"), DataType.UInt16, ByteOrder.ABCD, 3),
            "truncated array payload");

        return Task.CompletedTask;
    }

    private static Task ArrayRejectsNonNumericTypes(TestContext t)
    {
        // bool 按位寻址、string/bytes 靠显式长度、datetime 的线上值不是数字：
        // 它们都没有「定宽元素」这个概念，不允许当数组。
        foreach (var type in new[]
                 {
                     DataType.Bool, DataType.String, DataType.Bytes, DataType.DateTime,
                 })
        {
            t.Ok(!type.SupportsArray(), $"{type} is not an array element type");

            t.Throws<NotSupportedException>(
                () => ValueCodec.Decode(new byte[16], type, ByteOrder.ABCD, 2),
                $"{type}: array decode is rejected");
        }

        foreach (var type in new[]
                 {
                     DataType.Int16, DataType.UInt16, DataType.Int32, DataType.UInt32,
                     DataType.Int64, DataType.UInt64, DataType.Float32, DataType.Float64,
                     DataType.Bcd16, DataType.Bcd32,
                 })
        {
            t.Ok(type.SupportsArray(), $"{type} supports arrays");
        }

        return Task.CompletedTask;
    }

    private static string Hex(byte[] bytes) => Convert.ToHexString(bytes);
}
