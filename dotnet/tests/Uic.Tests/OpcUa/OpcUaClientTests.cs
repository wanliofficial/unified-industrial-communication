using Microsoft.Extensions.Logging.Abstractions;
using Opc.Ua;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.OpcUa;
using Uic.Obs;
using DomainValue = Uic.Domain.DataValue;

namespace Uic.Tests;

/// <summary>
/// OPC UA 客户端的纯函数验收：NodeId 解析、质量映射、值翻译、端点选择、参数推导。
///
/// 为什么这些不放进 --opcua 回环用例：回环需要证书与绑端口，跑一次几十秒，
/// 而且只能覆盖「None 策略 + 本仓服务端」。真正的翻译错误（少写一个范围检查、
/// 选端点时悄悄降级）恰恰是最难在端到端里看出来的。
/// </summary>
internal static class OpcUaClientTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("opcua/address_parses_node_ids", AddressParsesNodeIds));
        tests.Add(new TestCase("opcua/address_rejects_bad_input", AddressRejectsBadInput));
        tests.Add(new TestCase("opcua/status_maps_to_quality", StatusMapsToQuality));
        tests.Add(new TestCase("opcua/variant_decodes_scalars", VariantDecodesScalars));
        tests.Add(new TestCase("opcua/variant_decodes_arrays", VariantDecodesArrays));
        tests.Add(new TestCase("opcua/variant_rejects_shape_mismatch", VariantRejectsShapeMismatch));
        tests.Add(new TestCase("opcua/encode_writes_values", EncodeWritesValues));
        tests.Add(new TestCase("opcua/sample_keeps_device_timestamp", SampleKeepsDeviceTimestamp));
        tests.Add(new TestCase("opcua/options_from_channel_and_labels", OptionsFromChannelAndLabels));
        tests.Add(new TestCase("opcua/options_reject_bad_config", OptionsRejectsBadConfig));
        tests.Add(new TestCase("opcua/endpoint_selection_never_downgrades", EndpointSelectionNeverDowngrades));
        tests.Add(new TestCase("opcua/batch_size_never_zero_and_follows_operation_limit", BatchSizeNeverZeroAndFollowsOperationLimit));
    }

    // ------------------------------------------------------------------
    // 寻址
    // ------------------------------------------------------------------

    private static Task AddressParsesNodeIds(TestContext t)
    {
        AssertNode(OpcUaAddress.TryParse("ns=2;s=Demo.Temp", 0, out var a, out _), t, "explicit namespace");
        t.Equal((ushort)2, a!.NamespaceIndex, "ns= sets the namespace index");
        t.Equal(IdType.String, a.IdType, "s= is a string identifier");
        t.Equal("Demo.Temp", a.Identifier, "the identifier keeps its dots");

        t.Ok(OpcUaAddress.TryParse("ns=2;i=85", 0, out var b, out _), "numeric identifiers parse");
        t.Equal(IdType.Numeric, b!.IdType, "i= is numeric");
        t.Equal((uint)85, (uint)b.Identifier, "the numeric identifier is 85");

        // 省略 ns= 时用 address.index：点表里常常整台设备都在同一个命名空间。
        t.Ok(OpcUaAddress.TryParse("i=85", 3, out var c, out _), "i= without ns= parses");
        t.Equal((ushort)3, c!.NamespaceIndex, "the default namespace is applied");

        t.Ok(OpcUaAddress.TryParse("Demo.Temp", 4, out var d, out _), "bare text parses");
        t.Equal(IdType.String, d!.IdType, "bare text is treated as a string identifier");
        t.Equal("Demo.Temp", d.Identifier, "bare text keeps its value");
        t.Equal((ushort)4, d.NamespaceIndex, "bare text uses the default namespace");

        // 现场最常见的输入是「复制粘贴带空格」的 NodeId。
        t.Ok(OpcUaAddress.TryParse("  ns=2;s=Demo.Temp  ", 0, out var e, out _), "surrounding spaces are trimmed");
        t.Equal("Demo.Temp", e!.Identifier, "the trimmed identifier is used");

        var guid = Guid.NewGuid();
        t.Ok(OpcUaAddress.TryParse($"ns=2;g={guid}", 0, out var f, out _), "GUID identifiers parse");
        t.Equal(IdType.Guid, f!.IdType, "g= is a GUID");
        t.Equal(guid, f.Identifier, "the GUID survives");

        var bytes = new byte[] { 1, 2, 3 };
        t.Ok(OpcUaAddress.TryParse($"ns=2;b={Convert.ToBase64String(bytes)}", 0, out var g, out _),
            "ByteString identifiers parse");
        t.Equal(IdType.Opaque, g!.IdType, "b= is opaque");
        t.Equal(Convert.ToBase64String(bytes), Convert.ToBase64String((byte[])g.Identifier),
            "the ByteString survives");

        // 配置里的点位必须走符号寻址：寄存器寻址落到 UA 上没有任何含义。
        var wrongKind = new Address { Kind = AddressKind.Register, Area = "holding", Offset = 0 };
        var ex = t.Throws<DriverException>(() => OpcUaAddress.Parse(wrongKind),
            "register addressing is rejected");
        t.Equal(ErrorKind.Address, ex.Kind, "wrong addressing kind is an address error");

        return Task.CompletedTask;
    }

    private static Task AddressRejectsBadInput(TestContext t)
    {
        Rejects(t, null, 0, "address.path");           // 空路径
        Rejects(t, "   ", 0, "address.path");          // 只有空格
        Rejects(t, "nsu=http://example.com/;s=Foo", 0, "numeric namespace index");
        Rejects(t, "svr=1;ns=2;s=Foo", 0, "numeric namespace index");
        Rejects(t, "ns=2", 0, "identifier");           // 缺分号与标识符
        Rejects(t, "ns=abc;s=Foo", 0, "invalid namespace index");
        Rejects(t, "ns=70000;s=Foo", 0, "invalid namespace index");
        Rejects(t, "ns=2;", 0, "no identifier");
        Rejects(t, "ns=2;i=not-a-number", 0, "invalid numeric identifier");
        Rejects(t, "ns=2;s=", 0, "empty string identifier");
        Rejects(t, "ns=2;g=not-a-guid", 0, "invalid GUID identifier");
        Rejects(t, "ns=2;b=!!!", 0, "invalid base64");

        // 默认命名空间越界与文本内容无关，必须在解析前就拒绝。
        var ex = t.Throws<DriverException>(() => OpcUaAddress.Parse(Address.Symbol("s=Foo", index: 70_000)),
            "address.index above 65535 is rejected");
        t.Contains(ex.Message, "0..65535", "the error names the valid range");

        return Task.CompletedTask;
    }

    private static void Rejects(TestContext t, string? text, int ns, string expected)
    {
        string? error = null;
        var ok = OpcUaAddress.TryParse(text, ns, out var node, out error);

        t.Ok(!ok, $"'{text}' must be rejected");
        t.Ok(node is null, $"'{text}' must not produce a NodeId");
        t.Contains(error ?? string.Empty, expected, $"'{text}' explains why");
    }

    // ------------------------------------------------------------------
    // 质量
    // ------------------------------------------------------------------

    private static Task StatusMapsToQuality(TestContext t)
    {
        t.Equal(Quality.Good, OpcUaValues.FromStatus(StatusCodes.Good), "Good stays good");
        t.Equal(Quality.Uncertain, OpcUaValues.FromStatus(StatusCodes.UncertainLastUsableValue),
            "uncertain stays uncertain");

        // 点表写错：重试没有意义，要改配置。
        t.Equal(Quality.AddressError, OpcUaValues.FromStatus(StatusCodes.BadNodeIdUnknown),
            "an unknown node is an address error");
        t.Equal(Quality.AddressError, OpcUaValues.FromStatus(StatusCodes.BadIndexRangeInvalid),
            "a bad index range is an address error");

        // 权限问题：重试同样没有意义，但要做的是授权。
        t.Equal(Quality.Disabled, OpcUaValues.FromStatus(StatusCodes.BadUserAccessDenied),
            "access denied is a permission problem");
        t.Equal(Quality.Disabled, OpcUaValues.FromStatus(StatusCodes.BadNotWritable),
            "a read-only node is a permission problem");

        // 链路问题：会重试、会熔断。
        t.Equal(Quality.Timeout, OpcUaValues.FromStatus(StatusCodes.BadTimeout), "timeouts map to timeout");
        t.Equal(Quality.CommError, OpcUaValues.FromStatus(StatusCodes.BadConnectionClosed),
            "a closed connection is a comm error");

        // 订阅刚建立时服务端还没有采样，这不是故障。
        t.Equal(Quality.Stale, OpcUaValues.FromStatus(StatusCodes.BadWaitingForInitialData),
            "waiting for initial data is stale, not bad");

        t.Equal(Quality.Bad, OpcUaValues.FromStatus(StatusCodes.BadUnexpectedError),
            "anything unclassified is simply bad");

        t.Equal(ErrorKind.Address, OpcUaValues.KindFromStatus(StatusCodes.BadNodeIdUnknown),
            "the address error keeps its kind");
        t.Equal(ErrorKind.Connection, OpcUaValues.KindFromStatus(StatusCodes.BadConnectionClosed),
            "the comm error keeps its kind");
        t.Equal(ErrorKind.Timeout, OpcUaValues.KindFromStatus(StatusCodes.BadTimeout),
            "the timeout keeps its kind");

        t.Contains(OpcUaValues.DescribeStatus(StatusCodes.BadNodeIdUnknown), "BadNodeIdUnknown",
            "status names are readable in error messages");

        return Task.CompletedTask;
    }

    // ------------------------------------------------------------------
    // 值翻译
    // ------------------------------------------------------------------

    private static Task VariantDecodesScalars(TestContext t)
    {
        var f32 = new PointRef("temp", Address.Symbol("ns=2;s=Temp"), DataType.Float32,
            ByteOrder.ABCD, 2);
        t.Close(12.5, OpcUaValues.Decode(new Variant(12.5f), f32).AsNumber(), 1e-6,
            "float32 keeps its value");

        // 整数点位的服务端常给 int16/uint16 的原生类型；只要装得下就该接受。
        var u16 = new PointRef("speed", Address.Symbol("ns=2;s=Speed"), DataType.UInt16,
            ByteOrder.ABCD, 2);
        t.Close(300, OpcUaValues.Decode(new Variant((ushort)300), u16).AsNumber(), 0,
            "uint16 keeps its value");
        t.Close(300, OpcUaValues.Decode(new Variant(300), u16).AsNumber(), 0,
            "an int32 that fits the declared uint16 is accepted");

        var boolean = new PointRef("run", Address.Symbol("ns=2;s=Run"), DataType.Bool,
            ByteOrder.ABCD, 1);
        t.Equal(true, OpcUaValues.Decode(new Variant(true), boolean).AsBool(), "booleans survive");

        var text = new PointRef("name", Address.Symbol("ns=2;s=Name"), DataType.String,
            ByteOrder.ABCD, 0);
        t.Equal("line-1", OpcUaValues.Decode(new Variant("line-1"), text).Text, "strings survive");
        t.Equal("hello", OpcUaValues.Decode(new Variant(new LocalizedText("hello")), text).Text,
            "LocalizedText collapses to its text");

        var stamp = new DateTime(2026, 1, 2, 3, 4, 5, DateTimeKind.Utc);
        var when = new PointRef("at", Address.Symbol("ns=2;s=At"), DataType.DateTime,
            ByteOrder.ABCD, 8);
        var decoded = OpcUaValues.Decode(new Variant(stamp), when);
        t.Ok(decoded.TryGetDateTime(out var at), "the datetime decodes");
        t.Equal(stamp, at.UtcDateTime, "the instant is preserved");
        t.Equal(DateTimeKind.Utc, at.UtcDateTime.Kind, "the timestamp is UTC, not local");

        var blob = new PointRef("blob", Address.Symbol("ns=2;s=Blob"), DataType.Bytes,
            ByteOrder.ABCD, 0);
        var arrayPoint = new PointRef("profile", Address.Symbol("ns=2;s=Profile"), DataType.UInt16,
            ByteOrder.ABCD, 2, Count: 3);
        t.Equal(Convert.ToBase64String(new byte[] { 9, 8, 7 }),
            OpcUaValues.Decode(new Variant(new byte[] { 9, 8, 7 }), blob).Text,
            "ByteStrings keep their bytes");

        // byte[] 同时是 ByteString 和 CLR 数组；数组点位收到 ByteString 不能按元素拆开。
        t.Throws<FormatException>(
            () => OpcUaValues.Decode(new Variant(new byte[] { 1, 2, 3 }), arrayPoint),
            "a ByteString is not read as a numeric array");

        return Task.CompletedTask;
    }

    private static Task VariantDecodesArrays(TestContext t)
    {
        var array = new PointRef("profile", Address.Symbol("ns=2;s=Profile"), DataType.Float32,
            ByteOrder.ABCD, 2, Count: 3);

        var decoded = OpcUaValues.Decode(new Variant(new[] { 1f, 2f, 3f }), array);
        t.Ok(decoded.IsArray, "an array point decodes to an array");
        t.Equal(3, decoded.Array!.Count, "all elements are kept");
        t.Close(2, decoded.NumberAt(1), 1e-6, "the element order is preserved");

        // 服务端给 double[] 读进 float32 数组点位：值域内就接受。
        var widening = OpcUaValues.Decode(new Variant(new[] { 1d, 2d, 3d }), array);
        t.Equal(3, widening.Array!.Count, "a double array can be read into a float32 array point");

        return Task.CompletedTask;
    }

    private static Task VariantRejectsShapeMismatch(TestContext t)
    {
        var scalar = new PointRef("temp", Address.Symbol("ns=2;s=Temp"), DataType.Float32,
            ByteOrder.ABCD, 2);
        var array = new PointRef("profile", Address.Symbol("ns=2;s=Profile"), DataType.Float32,
            ByteOrder.ABCD, 2, Count: 3);
        var u16 = new PointRef("speed", Address.Symbol("ns=2;s=Speed"), DataType.UInt16,
            ByteOrder.ABCD, 2);

        t.Throws<FormatException>(
            () => OpcUaValues.Decode(new Variant(new[] { 1f, 2f }), scalar),
            "an array cannot be read into a scalar point");

        t.Throws<FormatException>(
            () => OpcUaValues.Decode(new Variant(1f), array),
            "a scalar cannot be read into an array point");

        t.Throws<FormatException>(
            () => OpcUaValues.Decode(new Variant(new[] { 1f, 2f }), array),
            "a short array is rejected instead of padded");

        t.Throws<FormatException>(
            () => OpcUaValues.Decode(new Variant("not a number"), scalar),
            "a string cannot be read into a numeric point");

        // 截断是最难查的一种"成功"：70000 存进 uint16 会变成 4464。
        t.Throws<FormatException>(
            () => OpcUaValues.Decode(new Variant(70_000), u16),
            "a value outside the declared range is rejected, not truncated");

        t.Throws<FormatException>(
            () => OpcUaValues.Decode(new Variant(-1), u16),
            "a negative value is rejected for an unsigned point");

        // 订阅回调里必须走 ToSample：坏状态是质量码，不是异常。
        var masked = new Opc.Ua.DataValue { WrappedValue = new Variant(0f) };
        t.Ok(masked.WrappedValue.Value is not null, "the data value carries its wrapped value");

        return Task.CompletedTask;
    }

    private static Task EncodeWritesValues(TestContext t)
    {
        var f32 = new PointRef("temp", Address.Symbol("ns=2;s=Temp"), DataType.Float32,
            ByteOrder.ABCD, 2);

        var encoded = OpcUaValues.Encode(DomainValue.Number(DataType.Float32, 4.5), f32);
        t.Equal(typeof(float), encoded.Value.GetType(), "float32 writes as a CLR float, not a double");
        t.Close(4.5, (float)encoded.Value, 1e-6, "the value survives encoding");

        var u16 = new PointRef("speed", Address.Symbol("ns=2;s=Speed"), DataType.UInt16,
            ByteOrder.ABCD, 2);
        t.Equal(typeof(ushort), OpcUaValues.Encode(DomainValue.Number(DataType.UInt16, 300), u16)
            .Value.GetType(), "uint16 writes as a CLR ushort");

        var outOfRange = t.Throws<DriverException>(
            () => OpcUaValues.Encode(DomainValue.Number(DataType.UInt16, 70_000), u16),
            "an out-of-range write is rejected");
        t.Equal(ErrorKind.Address, outOfRange.Kind, "an out-of-range write is an address error");

        var array = new PointRef("profile", Address.Symbol("ns=2;s=Profile"), DataType.Float32,
            ByteOrder.ABCD, 2, Count: 3);

        var encodedArray = OpcUaValues.Encode(
            DomainValue.Numbers(DataType.Float32, new double[] { 1, 2, 3 }), array);

        t.Equal(typeof(float[]), encodedArray.Value.GetType(),
            "an array writes as float[]; double[] would be BadTypeMismatch on a Float32 node");
        t.Equal(3, ((float[])encodedArray.Value).Length, "all elements are written");

        t.Throws<DriverException>(
            () => OpcUaValues.Encode(DomainValue.Numbers(DataType.Float32, new double[] { 1, 2 }), array),
            "a short array is rejected before it reaches the wire");

        t.Throws<DriverException>(
            () => OpcUaValues.Encode(DomainValue.Number(DataType.Float32, 1), array),
            "a scalar cannot be written into an array point");

        var bcd = new PointRef("bcd", Address.Symbol("ns=2;s=Bcd"), DataType.Bcd16, ByteOrder.ABCD, 2);
        var unsupported = t.Throws<DriverException>(
            () => OpcUaValues.Encode(DomainValue.Number(DataType.Bcd16, 12), bcd),
            "BCD has no OPC UA representation");
        t.Equal(ErrorKind.Unsupported, unsupported.Kind, "BCD is reported as unsupported");

        return Task.CompletedTask;
    }

    private static Task SampleKeepsDeviceTimestamp(TestContext t)
    {
        var point = new PointRef("temp", Address.Symbol("ns=2;s=Temp"), DataType.Float32,
            ByteOrder.ABCD, 2);

        var source = new DateTime(2026, 5, 6, 7, 8, 9, DateTimeKind.Utc);
        var good = new Opc.Ua.DataValue
        {
            WrappedValue = new Variant(21.5f),
            StatusCode = StatusCodes.Good,
            SourceTimestamp = source,
        };

        var sample = OpcUaValues.ToSample(point.Id, good, point);

        t.Equal(Quality.Good, sample.Quality, "a good value keeps good quality");
        t.Close(21.5, sample.Value!.AsNumber(), 1e-6, "the value is carried into the sample");
        var stamp = sample.DeviceTime;
        t.Ok(stamp is not null, "the device timestamp is reported");
        t.Equal(source, stamp!.Value.UtcDateTime, "the source timestamp is used, not the local clock");

        // 服务端只给 ServerTimestamp 时也要有一个时标，否则历史库只能补本机时间。
        var serverOnly = new Opc.Ua.DataValue
        {
            WrappedValue = new Variant(1f),
            StatusCode = StatusCodes.Good,
            ServerTimestamp = source.AddSeconds(1),
        };
        t.Equal(source.AddSeconds(1), OpcUaValues.ToSample(point.Id, serverOnly, point)
            .DeviceTime!.Value.UtcDateTime, "the server timestamp is the fallback");

        var bad = new Opc.Ua.DataValue
        {
            WrappedValue = new Variant(0f),
            StatusCode = StatusCodes.BadNodeIdUnknown,
        };

        var failed = OpcUaValues.ToSample(point.Id, bad, point);
        t.Equal(Quality.AddressError, failed.Quality, "a bad status carries its quality");
        t.Ok(failed.Value is null, "a bad status has no value");
        t.Contains(failed.Error ?? string.Empty, "BadNodeIdUnknown", "the error names the status");

        var mismatch = new Opc.Ua.DataValue
        {
            WrappedValue = new Variant("text"),
            StatusCode = StatusCodes.Good,
        };

        var mistyped = OpcUaValues.ToSample(point.Id, mismatch, point);
        t.Equal(Quality.AddressError, mistyped.Quality,
            "a type mismatch is a point-table problem, not a link failure");

        return Task.CompletedTask;
    }

    // ------------------------------------------------------------------
    // 通道参数与端点选择
    // ------------------------------------------------------------------

    private static Task OptionsFromChannelAndLabels(TestContext t)
    {
        var ctx = Context("10.0.0.5", 4841, t.TempDir);
        var options = OpcUaClientOptions.FromContext(ctx);

        t.Equal("opc.tcp://10.0.0.5:4841", options.EndpointUrl, "the endpoint comes from host and port");
        t.Equal("None", options.SecurityPolicy, "the default policy is None");
        t.Equal(MessageSecurityMode.None, options.SecurityMode, "None policy implies None mode");
        t.Equal(Path.Combine(t.TempDir, "opcua-client-pki"), options.PkiDir,
            "the certificate store lives under the data directory");

        // 省略端口时用 UA 的标准端口，而不是 Modbus 的 502。
        t.Equal("opc.tcp://10.0.0.5:4840", OpcUaClientOptions.FromContext(
            Context("10.0.0.5", 0, t.TempDir)).EndpointUrl, "port 0 falls back to 4840");

        // IPv6 字面量必须带方括号，否则 URI 解析失败。
        t.Equal("opc.tcp://[::1]:4840", OpcUaClientOptions.FromContext(
            Context("::1", 0, t.TempDir)).EndpointUrl, "IPv6 literals are bracketed");

        var withPath = Context("10.0.0.5", 4840, t.TempDir, new() { ["endpointPath"] = "UA/Server" });
        t.Equal("opc.tcp://10.0.0.5:4840/UA/Server",
            OpcUaClientOptions.FromContext(withPath).EndpointUrl,
            "the endpoint path is normalised with a leading slash");

        var secured = Context("10.0.0.5", 4840, t.TempDir,
            new() { ["securityPolicy"] = "basic256sha256" });

        var secureOptions = OpcUaClientOptions.FromContext(secured);
        t.Equal("Basic256Sha256", secureOptions.SecurityPolicy, "policy names are normalised");
        t.Equal(MessageSecurityMode.SignAndEncrypt, secureOptions.SecurityMode,
            "a secure policy defaults to SignAndEncrypt");

        var signed = Context("10.0.0.5", 4840, t.TempDir, new()
        {
            ["securityPolicy"] = "Basic256Sha256",
            ["securityMode"] = "Sign",
        });

        t.Equal(MessageSecurityMode.Sign, OpcUaClientOptions.FromContext(signed).SecurityMode,
            "an explicit mode is honoured");

        var relative = Context("10.0.0.5", 4840, t.TempDir, new() { ["pkiDir"] = "certs/ua" });
        t.Equal(Path.Combine(t.TempDir, "certs", "ua"), OpcUaClientOptions.FromContext(relative).PkiDir,
            "a relative pkiDir is resolved under the data directory");

        var absolute = Path.Combine(t.TempDir, "elsewhere");
        var absoluteCtx = Context("10.0.0.5", 4840, t.TempDir, new() { ["pkiDir"] = absolute });
        t.Equal(absolute, OpcUaClientOptions.FromContext(absoluteCtx).PkiDir,
            "an absolute pkiDir is used as is");

        var tuned = Context("10.0.0.5", 4840, t.TempDir, new()
        {
            ["publishingInterval"] = "250",
            ["sessionTimeout"] = "120000",
        });

        var tunedOptions = OpcUaClientOptions.FromContext(tuned);
        t.Equal(TimeSpan.FromMilliseconds(250), tunedOptions.PublishingInterval,
            "the publishing interval is configurable");
        t.Equal(TimeSpan.FromSeconds(120), tunedOptions.SessionTimeout,
            "the session timeout is configurable");

        return Task.CompletedTask;
    }

    private static Task OptionsRejectsBadConfig(TestContext t)
    {
        // 不在白名单里的策略：明确拒绝，而不是退回 None。
        var unknown = Context("10.0.0.5", 4840, t.TempDir, new() { ["securityPolicy"] = "Basic128Rsa15" });
        var unsupported = t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(unknown),
            "an unsupported policy is rejected");
        t.Equal(ErrorKind.Unsupported, unsupported.Kind, "an unsupported policy is not retried");
        t.Contains(unsupported.Message, "None or Basic256Sha256", "the error lists what is supported");

        // 声称加密却用明文模式：比"连不上"更糟的是连上了却没加密。
        var plaintext = Context("10.0.0.5", 4840, t.TempDir, new()
        {
            ["securityPolicy"] = "Basic256Sha256",
            ["securityMode"] = "None",
        });

        var downgrade = t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(plaintext),
            "a secure policy with sign mode None is rejected");
        t.Equal(ErrorKind.Unsupported, downgrade.Kind, "the downgrade is refused explicitly");

        var noneSigned = Context("10.0.0.5", 4840, t.TempDir, new() { ["securityMode"] = "Sign" });
        t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(noneSigned),
            "None policy with signing is rejected");

        var badMode = Context("10.0.0.5", 4840, t.TempDir, new() { ["securityMode"] = "Encrypt" });
        t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(badMode),
            "an unknown security mode is rejected");

        var badTimeout = Context("10.0.0.5", 4840, t.TempDir, new() { ["sessionTimeout"] = "1" });
        t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(badTimeout),
            "a session timeout below the floor is rejected");

        var badBool = Context("10.0.0.5", 4840, t.TempDir,
            new() { ["acceptUntrustedCertificates"] = "maybe" });
        t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(badBool),
            "a non-boolean label is rejected instead of guessed");

        // 没有主机名就没有端点；此时报地址错误，别走到 DNS 才发现。
        var noHost = new OpenContext(
            new Channel { Id = "ua", Protocol = "opcua", Transport = "tcp" },
            new Device { Id = "plc", ChannelId = "ua" },
            NullLogger.Instance, new MetricsRegistry(), t.TempDir);

        t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(noHost),
            "a channel without a host is rejected");

        // 证书必须落在某个目录里：没有数据目录又没有显式 pkiDir 时不能启动。
        var noStore = new OpenContext(
            new Channel { Id = "ua", Protocol = "opcua", Transport = "tcp", Host = "10.0.0.5", Port = 4840 },
            new Device { Id = "plc", ChannelId = "ua" },
            NullLogger.Instance, new MetricsRegistry());

        t.Throws<DriverException>(() => OpcUaClientOptions.FromContext(noStore),
            "a missing certificate store is rejected");

        return Task.CompletedTask;
    }

    private static Task EndpointSelectionNeverDowngrades(TestContext t)
    {
        var endpoints = new[]
        {
            new EndpointDescription
            {
                EndpointUrl = "opc.tcp://10.0.0.5:4840",
                SecurityPolicyUri = SecurityPolicies.None,
                SecurityMode = MessageSecurityMode.None,
            },
            new EndpointDescription
            {
                EndpointUrl = "opc.tcp://10.0.0.5:4840",
                SecurityPolicyUri = SecurityPolicies.Basic256Sha256,
                SecurityMode = MessageSecurityMode.SignAndEncrypt,
            },
        };

        var chosen = OpcUaClientOptions.SelectEndpoint(endpoints, SecurityPolicies.None, MessageSecurityMode.None);
        t.Equal(SecurityPolicies.None, chosen.SecurityPolicyUri, "the requested policy is selected");

        var secure = OpcUaClientOptions.SelectEndpoint(
            endpoints, SecurityPolicies.Basic256Sha256, MessageSecurityMode.SignAndEncrypt);
        t.Equal(MessageSecurityMode.SignAndEncrypt, secure.SecurityMode,
            "the requested mode is selected");

        // 策略对、模式不对同样不算匹配。
        var failure = t.Throws<DriverException>(
            () => OpcUaClientOptions.SelectEndpoint(
                endpoints, SecurityPolicies.Basic256Sha256, MessageSecurityMode.Sign),
            "a missing policy/mode combination is an error");

        t.Equal(ErrorKind.Unsupported, failure.Kind, "a missing endpoint is not retried blindly");
        t.Contains(failure.Message, "does not offer", "the error says what is missing");
        t.Contains(failure.Message, "None/None", "the error lists what the server offers");
        t.Contains(failure.Message, "Basic256Sha256/SignAndEncrypt", "every offered option is listed");

        // 服务端只有加密端点、配置要求 None：绝不悄悄升级或降级。
        var encryptedOnly = endpoints[1..];
        t.Throws<DriverException>(
            () => OpcUaClientOptions.SelectEndpoint(
                encryptedOnly, SecurityPolicies.None, MessageSecurityMode.None),
            "None is not silently upgraded to encryption");

        t.Throws<DriverException>(
            () => OpcUaClientOptions.SelectEndpoint(
                Array.Empty<EndpointDescription>(), SecurityPolicies.None, MessageSecurityMode.None),
            "an empty endpoint list is an error, not a default");

        t.Equal("Basic256Sha256", OpcUaClientOptions.PolicyName(SecurityPolicies.Basic256Sha256),
            "policy URIs shorten to their name");
        t.Equal("None", OpcUaClientOptions.PolicyName(null), "a missing URI means None");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 单次服务的节点批量（FR-COL-05 的「按 <c>MaxNodesPerRead</c> 分片」）：
    /// 服务端声明的上限比默认批量小时按上限切，否则按默认批量；**任何输入都不得返回 0**。
    ///
    /// 为什么单独立一条：批量大小决定「一轮采集要发几次 Read 服务调用」。这个值若退化成 0，
    /// <c>for (offset += batch)</c> 的循环会原地打转——不是慢，是永久卡住本轮采集。
    /// 服务端不声明上限（内置服务端就是这样）也必须回落到一个正的默认值。
    /// </summary>
    private static Task BatchSizeNeverZeroAndFollowsOperationLimit(TestContext t)
    {
        var fallback = OpcUaClientDriver.DefaultBatchNodes;

        t.Equal(fallback, OpcUaClientDriver.MaxNodes(null),
            "a server that does not advertise a limit falls back to the default batch");
        t.Equal(fallback, OpcUaClientDriver.MaxNodes(0u),
            "zero is not a usable batch; it must not be returned as-is");

        t.Equal(5, OpcUaClientDriver.MaxNodes(5u), "a smaller advertised limit is honoured");
        t.Equal(1, OpcUaClientDriver.MaxNodes(1u), "the smallest usable batch is still 1");

        t.Equal(fallback, OpcUaClientDriver.MaxNodes((uint)fallback),
            "a limit exactly equal to the default keeps the default");
        t.Equal(fallback, OpcUaClientDriver.MaxNodes(100_000u),
            "a limit larger than the default does not inflate the batch");

        foreach (var limit in new uint?[] { null, 0, 1, 5, 499, 500, 501, uint.MaxValue })
            t.Ok(OpcUaClientDriver.MaxNodes(limit) >= 1,
                $"batch is never zero (limit {limit?.ToString() ?? "null"})");

        return Task.CompletedTask;
    }

    private static void AssertNode(bool parsed, TestContext t, string what)
        => t.Ok(parsed, what);

    private static OpenContext Context(
        string host, int port, string dataDir, Dictionary<string, string>? labels = null)
        => new(
            new Channel { Id = "ua", Protocol = "opcua", Transport = "tcp", Host = host, Port = port },
            new Device
            {
                Id = "plc",
                ChannelId = "ua",
                Labels = labels ?? new Dictionary<string, string>(StringComparer.Ordinal),
            },
            NullLogger.Instance,
            new MetricsRegistry(),
            dataDir);
}
