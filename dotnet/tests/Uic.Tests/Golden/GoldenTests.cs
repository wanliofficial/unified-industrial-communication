using System.Buffers.Binary;
using Uic.Codec.Modbus;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Tests;

/// <summary>
/// 金报文验收（规范 §7.1 / 用例 T14）。
///
/// 每一份 testdata/golden/*.json 都同时验证三件事：
/// 1. 编码器产出的字节与手算/抓包得到的帧**完全一致**（多一个字节都算错）；
/// 2. 解码器能从同一帧里还原出点位值；
/// 3. 封装层（MBAP / CRC / LRC）自洽——帧长度、事务号、单元号都对得上。
///
/// 只用一份 JSON 描述，不做「代码里再抄一遍期望值」：抄一遍就等于放弃比对。
/// </summary>
internal static class GoldenTests
{
    public static void Register(List<TestCase> tests)
    {
        foreach (var (file, golden) in GoldenCorpus.Load())
        {
            var captured = golden;

            tests.Add(new TestCase($"golden/{golden.Name}", t =>
            {
                Run(t, file, captured);
                return Task.CompletedTask;
            }));
        }
    }

    private static void Run(TestContext t, string file, GoldenCase g)
    {
        var order = ParseByteOrder(g.ByteOrder, file);
        var wire = DecodeFrame(t, g);
        var codec = new ModbusCodec("golden");

        if (string.Equals(g.Kind, "write", StringComparison.OrdinalIgnoreCase))
        {
            RunWrite(t, g, wire, codec, order);
            return;
        }

        RunRead(t, g, wire, codec, order);
    }

    // ------------------------------------------------------------------
    // 读
    // ------------------------------------------------------------------

    private static void RunRead(TestContext t, GoldenCase g, Wire wire, ModbusCodec codec, ByteOrder order)
    {
        var requestPdu = wire.RequestPdu;

        t.Ok(requestPdu.Length >= 5, "read request carries a 5-byte PDU");
        t.Ok((requestPdu[0] & 0x80) == 0, "request function code is not an exception code");

        var (areaFromCode, isBits) = AreaForFunction(requestPdu[0]);
        var start = BinaryPrimitives.ReadUInt16BigEndian(requestPdu.AsSpan(1));
        var quantity = BinaryPrimitives.ReadUInt16BigEndian(requestPdu.AsSpan(3));
        var area = g.Area ?? areaFromCode;

        var points = new List<PointRef>(g.Expect.Length);
        var cursor = (int)start;

        foreach (var e in g.Expect)
        {
            var type = JsonEnum.ParseDataType(e.Type);
            var at = e.Offset ?? cursor;
            var length = isBits ? 1 : Math.Max(1, e.Length ?? type.Registers());

            if (isBits) t.Equal(DataType.Bool, type, $"{e.Point}: bit areas only carry bool values");

            points.Add(new PointRef(
                e.Point,
                Address.Register(area, at, isBits ? 0 : length),
                type,
                order,
                length));

            cursor = at + (isBits ? 1 : length);
        }

        t.Ok(points.Count > 0, "fixture lists at least one point");

        // 布局必须与请求窗口严格一致：否则「编码对上了」只是碰巧。
        var window = ModbusCodec.Window(points.ToArray());
        t.Equal((int)start, window.Start, "fixture starts at the request offset");
        t.Equal((int)quantity, window.Quantity, "fixture covers exactly the requested window");
        t.Equal(isBits, window.IsBits, "fixture area agrees with the request function code");

        t.Equal(wire.RequestWire, wire.Render(codec.EncodeRead(points)),
            "encoded request matches the golden frame");

        if (g.Error is not null)
        {
            var kind = ParseErrorKind(g.Error, g.Name);
            var thrown = CatchDriverException(() => codec.DecodeRead(points, wire.ResponsePdu), g.Name);
            t.Equal(kind, thrown.Kind, "exception response maps to the expected error kind");
            return;
        }

        var samples = codec.DecodeRead(points, wire.ResponsePdu);
        t.Equal(points.Count, samples.Count, "one sample per point, in order");

        for (var i = 0; i < samples.Count; i++)
        {
            CompareSample(t, g.Expect[i], samples[i]);
        }
    }

    // ------------------------------------------------------------------
    // 写
    // ------------------------------------------------------------------

    private static void RunWrite(TestContext t, GoldenCase g, Wire wire, ModbusCodec codec, ByteOrder order)
    {
        var requestPdu = wire.RequestPdu;

        t.Ok(requestPdu.Length >= 5, "write request carries a 5-byte PDU");
        t.Equal(1, g.Expect.Length, "write fixture describes exactly one point");

        var e = g.Expect[0];
        var type = JsonEnum.ParseDataType(e.Type);
        var isBitWrite = requestPdu[0] is ModbusConstants.FcWriteSingleCoil or ModbusConstants.FcWriteMultipleCoils;

        var area = g.Area ?? (isBitWrite ? "coil" : "holding");
        var offset = BinaryPrimitives.ReadUInt16BigEndian(requestPdu.AsSpan(1));
        var length = isBitWrite ? 1 : Math.Max(1, e.Length ?? type.Registers());

        if (isBitWrite) t.Equal(DataType.Bool, type, "coil writes require a bool point");

        var point = new PointRef(
            e.Point,
            Address.Register(area, offset, isBitWrite ? 0 : length),
            type,
            order,
            length);

        var command = new WriteCommand(point, ToDataValue(e, type));

        t.Equal(wire.RequestWire, wire.Render(codec.EncodeWrite(command)),
            "encoded write matches the golden frame");

        if (g.Error is not null)
        {
            var kind = ParseErrorKind(g.Error, g.Name);
            var thrown = CatchDriverException(() => codec.DecodeWrite(command, wire.ResponsePdu), g.Name);
            t.Equal(kind, thrown.Kind, "exception response maps to the expected error kind");
            return;
        }

        var result = codec.DecodeWrite(command, wire.ResponsePdu);
        t.Ok(result.Ok, "write response is accepted");
        t.Equal(e.Point, result.PointId, "write result is attributed to the requested point");
    }

    // ------------------------------------------------------------------
    // 封装
    // ------------------------------------------------------------------

    private readonly record struct Wire(
        byte Unit,
        byte[] RequestPdu,
        byte[] ResponsePdu,
        Func<byte[], string> Render,
        string RequestWire);

    private static Wire DecodeFrame(TestContext t, GoldenCase g)
    {
        switch (g.Frame.ToLowerInvariant())
        {
            case "tcp":
            {
                var request = ParseHex(t, g.Request, "request");
                var (transaction, _, unit, requestPdu) = ModbusTcpFrame.Parse(request);

                var response = ParseHex(t, g.Response, "response");
                t.Equal(response.Length, ModbusTcpFrame.TotalLength(response),
                    "MBAP length field matches the actual frame size");

                var (responseTransaction, _, responseUnit, responsePdu) = ModbusTcpFrame.Parse(response);
                t.Equal(transaction, responseTransaction, "response echoes the transaction id");
                t.Equal(unit, responseUnit, "response echoes the unit id");

                return new Wire(
                    unit,
                    requestPdu,
                    responsePdu,
                    pdu => Convert.ToHexString(ModbusTcpFrame.Build(transaction, unit, pdu)),
                    NormalizeHex(g.Request));
            }

            case "rtu":
            {
                var request = ParseHex(t, g.Request, "request");
                var (unit, requestPdu) = ModbusRtuFrame.Parse(request);

                var response = ParseHex(t, g.Response, "response");
                var (responseUnit, responsePdu) = ModbusRtuFrame.Parse(response);
                t.Equal(unit, responseUnit, "response echoes the unit id");

                // 流式读取靠「功能码 + 字节数」预判长度；金报文必须能通过这条路径。
                if (responsePdu[0] == requestPdu[0] && requestPdu.Length >= 5)
                {
                    var quantity = BinaryPrimitives.ReadUInt16BigEndian(requestPdu.AsSpan(3));
                    var byteCount = requestPdu[0] is ModbusConstants.FcReadCoils or ModbusConstants.FcReadDiscreteInputs
                        ? (quantity + 7) / 8
                        : quantity * 2;

                    t.Equal(response.Length,
                        ModbusRtuFrame.ExpectedResponseLength(requestPdu[0], (byte)byteCount),
                        "RTU response length matches the framing prediction");
                }

                return new Wire(
                    unit,
                    requestPdu,
                    responsePdu,
                    pdu => Convert.ToHexString(ModbusRtuFrame.Build(unit, pdu)),
                    NormalizeHex(g.Request));
            }

            case "ascii":
            {
                var (unit, requestPdu) = ModbusAsciiFrame.Parse(g.Request);
                var (responseUnit, responsePdu) = ModbusAsciiFrame.Parse(g.Response);
                t.Equal(unit, responseUnit, "response echoes the unit id");

                return new Wire(
                    unit,
                    requestPdu,
                    responsePdu,
                    pdu => NormalizeAscii(ModbusAsciiFrame.Build(unit, pdu)),
                    NormalizeAscii(g.Request));
            }

            default:
                throw new CheckFailedException($"frame '{g.Frame}' is not one of tcp/rtu/ascii");
        }
    }

    // ------------------------------------------------------------------
    // 工具
    // ------------------------------------------------------------------

    private static (string Area, bool IsBits) AreaForFunction(byte function) => function switch
    {
        ModbusConstants.FcReadCoils => ("coil", true),
        ModbusConstants.FcReadDiscreteInputs => ("discrete", true),
        ModbusConstants.FcReadHoldingRegs or ModbusConstants.FcWriteSingleRegister
            or ModbusConstants.FcWriteMultipleRegs => ("holding", false),
        ModbusConstants.FcReadInputRegs => ("input", false),
        ModbusConstants.FcWriteSingleCoil or ModbusConstants.FcWriteMultipleCoils => ("coil", true),
        _ => throw new CheckFailedException($"function code 0x{function:x2} is not covered by the golden loader"),
    };

    private static DataValue ToDataValue(GoldenExpect e, DataType type) => type switch
    {
        DataType.Bool => DataValue.Boolean(e.Bool ?? false),
        DataType.String or DataType.Bytes => DataValue.String(e.Text ?? string.Empty),
        _ => DataValue.Number(type, e.Num ?? 0),
    };

    private static void CompareSample(TestContext t, GoldenExpect e, RawSample sample)
    {
        t.Equal(e.Point, sample.PointId, "sample point id");

        var expectedQuality = JsonEnum.ParseQuality(e.Quality);
        t.Equal(expectedQuality, sample.Quality, $"{e.Point}: quality");

        if (expectedQuality != Quality.Good)
        {
            t.Ok(sample.Value is null, $"{e.Point}: bad samples carry no value");
            return;
        }

        t.Ok(sample.Value is not null, $"{e.Point}: value is present");

        var type = JsonEnum.ParseDataType(e.Type);
        t.Equal(type, sample.Value!.Type, $"{e.Point}: value type");

        switch (type)
        {
            case DataType.Bool:
                t.Equal(e.Bool ?? false, sample.Value.Bool ?? false, $"{e.Point}: bool value");
                break;

            case DataType.String or DataType.Bytes:
                t.Equal(e.Text ?? string.Empty, sample.Value.Text ?? string.Empty, $"{e.Point}: text value");
                break;

            default:
                t.Close(e.Num ?? 0, sample.Value.AsNumber(), 1e-6, $"{e.Point}: numeric value");
                break;
        }
    }

    private static DriverException CatchDriverException(Action action, string name)
    {
        try
        {
            action();
        }
        catch (DriverException ex)
        {
            return ex;
        }

        throw new CheckFailedException($"{name}: expected a DriverException, the frame decoded successfully");
    }

    private static ErrorKind ParseErrorKind(string text, string name) => text.ToLowerInvariant() switch
    {
        "timeout" => ErrorKind.Timeout,
        "connection" => ErrorKind.Connection,
        "protocol" => ErrorKind.Protocol,
        "address" => ErrorKind.Address,
        "permission" => ErrorKind.Permission,
        "unsupported" => ErrorKind.Unsupported,
        "canceled" => ErrorKind.Canceled,
        "internal" => ErrorKind.Internal,
        _ => throw new CheckFailedException($"{name}: unknown error kind '{text}'"),
    };

    private static ByteOrder ParseByteOrder(string text, string name) => text.ToLowerInvariant() switch
    {
        "abcd" => ByteOrder.ABCD,
        "cdab" => ByteOrder.CDAB,
        "badc" => ByteOrder.BADC,
        "dcba" => ByteOrder.DCBA,
        _ => throw new CheckFailedException($"{name}: unknown byte order '{text}'"),
    };

    private static byte[] ParseHex(TestContext t, string text, string what)
    {
        var clean = new string(text.Where(c => !char.IsWhiteSpace(c)).ToArray());

        if (clean.Length == 0 || clean.Length % 2 != 0)
            t.Fail($"{what}: hex string must have an even number of digits");

        try
        {
            return Convert.FromHexString(clean);
        }
        catch (FormatException)
        {
            throw new CheckFailedException($"{what}: '{text}' is not valid hex");
        }
    }

    private static string NormalizeHex(string text)
        => Convert.ToHexString(Convert.FromHexString(
            new string(text.Where(c => !char.IsWhiteSpace(c)).ToArray())));

    /// <summary>ASCII 帧的比较忽略大小写：CRC 之外，线上大小写没有语义差别。</summary>
    private static string NormalizeAscii(string text) => text.Trim().ToUpperInvariant();
}
