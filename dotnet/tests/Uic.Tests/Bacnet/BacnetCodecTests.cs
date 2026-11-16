using Uic.Codec.Bacnet;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Tests;

/// <summary>
/// BACnet/IP 报文级验收：每一串 hex 都按 ASHRAE 135 的字段布局手算出来，
/// 不是「编码一次再解码一次、两边相等」的自洽断言。
///
/// 这样写的原因：编解码器自己前后一致，可以完全错在同一处（例如 NPDU 少一个版本字节、
/// 服务参数漏掉上下文标签），真实设备却一个字节都不认。手算的黄金帧会把这些错当场暴露。
/// </summary>
internal static class BacnetCodecTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("bacnet/who_is_frame", WhoIsFrame));
        tests.Add(new TestCase("bacnet/bvlc_npdu_wrapping", BvlcNpduWrapping));
        tests.Add(new TestCase("bacnet/read_property_frame", ReadPropertyFrame));
        tests.Add(new TestCase("bacnet/read_property_ack_parses_value", ReadPropertyAckParsesValue));
        tests.Add(new TestCase("bacnet/read_property_ack_rejects_wrong_invoke_id", AckRejectsWrongInvokeId));
        tests.Add(new TestCase("bacnet/error_response_maps_to_address_error", ErrorMapsToAddressError));
        tests.Add(new TestCase("bacnet/segmented_ack_is_unsupported", SegmentedAckIsUnsupported));
        tests.Add(new TestCase("bacnet/write_property_frame", WritePropertyFrame));
        tests.Add(new TestCase("bacnet/ack_value_contains_closing_lookalike", AckValueContainingClosingLookalike));
        tests.Add(new TestCase("bacnet/simple_ack_round_trip", SimpleAckRoundTrip));
        tests.Add(new TestCase("bacnet/i_am_parses_device_instance", IAmParsesDeviceInstance));
        tests.Add(new TestCase("bacnet/object_id_wire_encoding", ObjectIdWireEncoding));
        tests.Add(new TestCase("bacnet/address_validation", AddressValidation));
        tests.Add(new TestCase("bacnet/tags_round_trip", TagsRoundTrip));
        tests.Add(new TestCase("bacnet/tags_reject_invalid", TagsRejectInvalid));
        tests.Add(new TestCase("bacnet/npdu_version_must_be_one", NpduVersionMustBeOne));
    }

    private static string Hex(ReadOnlySpan<byte> bytes) => Convert.ToHexString(bytes);

    private static Task WhoIsFrame(TestContext t)
    {
        // Who-Is：APDU = 未确认请求(0x10) + 服务码 0x08，两字节，无参数。
        var apdu = BacnetApdu.BuildWhoIs();
        t.Equal("1008", Hex(apdu), "who-is apdu is the unconfirmed service choice only");

        // 广播封帧：BVLC(81 0A) + 长度(0008) + NPDU(版本 01 + 控制 00) + APDU(1008)。
        var frame = BacnetFrame.Build(apdu, broadcast: true);
        t.Equal("810A000801001008", Hex(frame), "broadcast who-is frame");
        t.Equal(BacnetConstants.BvllFunctionOriginalBroadcast, BacnetFrame.BvllFunction(frame),
            "the frame is a broadcast");

        var unicast = BacnetFrame.Build(apdu, broadcast: false);
        t.Equal("810B000801001008", Hex(unicast), "unicast uses function 0x0B");
        t.Equal("1008", Hex(BacnetFrame.Payload(unicast)), "payload survives the round trip");

        return Task.CompletedTask;
    }

    private static Task BvlcNpduWrapping(TestContext t)
    {
        var apdu = Convert.FromHexString("AABBCC");
        var frame = BacnetFrame.Build(apdu, broadcast: false);

        // BVLC 的 2 字节长度字段包含 BVLC 自己（4 字节）+ NPDU（2）+ APDU（3）= 9。
        t.Equal(9, frame.Length, "frame length");
        t.Equal(9, (frame[2] << 8) | frame[3], "BVLC length counts the whole frame including itself");
        t.Equal(BacnetConstants.BvllTypeBacnetIp, frame[0], "BVLC type is always 0x81");
        t.Equal(BacnetConstants.NpduVersion, frame[4], "NPDU version must be 1");
        t.Equal(0, (int)frame[5], "NPDU control byte 0 means no routing");

        t.Throws<DriverException>(() => BacnetFrame.Payload(frame[..5]).ToArray(),
            "a truncated frame is rejected");
        t.Throws<DriverException>(() =>
            {
                var bad = frame.ToArray();
                bad[0] = 0x82;
                BacnetFrame.Payload(bad).ToArray();
            }, "a non-BACnet/IP BVLC type is rejected");

        return Task.CompletedTask;
    }

    private static Task ReadPropertyFrame(TestContext t)
    {
        // 手算 ReadProperty(analog-input:1, present-value=85)，invoke-id = 7：
        //   APDU 00 04 07 0C                        确认请求/不分段+1024/invoke-id/服务码
        //       0C 00000001                        [0] 对象标识符 = 类型0<<22 | 1
        //       19 55                              [1] 属性标识符 = 85
        var id = new BacnetObjectId(BacnetConstants.ObjectTypeAnalogInput, 1);
        var apdu = BacnetApdu.BuildReadProperty(id, BacnetConstants.PropertyPresentValue, invokeId: 7);

        t.Equal("0004070C0C000000011955", Hex(apdu), "read-property apdu matches the wire format");

        // 封成帧之后 BVLC 长度 = 4 + 2 + 11 = 17 = 0x11。
        var frame = BacnetFrame.Build(apdu, broadcast: false);
        t.Equal("810B001101000004070C0C000000011955", Hex(frame), "read-property frame");

        // 属性标识符 > 255 时必须用两字节（present-value 是 85，走一字节分支；
        // 111 status-flags 也一字节；这里挑一个两字节的值验证分支没写反）。
        var wide = BacnetApdu.BuildReadProperty(id, 0x0123, 1);
        t.Contains(Hex(wide), "1A0123", "a 16-bit property id is encoded as two bytes");

        return Task.CompletedTask;
    }

    private static Task ReadPropertyAckParsesValue(TestContext t)
    {
        // 从站回 ComplexAck：real 42.5 → 标签 0x44 + IEEE754 42.5 = 42A80000。
        var id = new BacnetObjectId(BacnetConstants.ObjectTypeAnalogInput, 1);
        var value = BacnetTags.EncodeValue(DataValue.Number(DataType.Float32, 42.5), DataType.Float32);
        t.Equal("44422A0000", Hex(value), "real 42.5 with its application tag");

        var ackApdu = BacnetApdu.BuildReadPropertyAck(7, id, BacnetConstants.PropertyPresentValue, value);
        var frame = BacnetFrame.Build(ackApdu, broadcast: false);

        var parsed = BacnetApdu.ParseReadProperty(BacnetFrame.Payload(frame), expectedInvokeId: 7);
        t.Ok(parsed.Ok, $"the ack parses ({parsed.Describe})");

        var (tag, body, consumed) = BacnetTags.DecodeTag(parsed.ValueWithTag!);
        t.Equal((int)BacnetConstants.TagReal, (int)tag, "the value is tagged real");
        t.Equal(value.Length, consumed, "the tag declares exactly the bytes it carries");

        var decoded = BacnetTags.DecodeValue(tag, body, DataType.Float32);
        t.Equal(42.5d, decoded.AsNumber(), "the value round-trips");

        return Task.CompletedTask;
    }

    private static Task AckRejectsWrongInvokeId(TestContext t)
    {
        var id = new BacnetObjectId(BacnetConstants.ObjectTypeAnalogInput, 1);
        var value = BacnetTags.EncodeValue(DataValue.Number(DataType.Float32, 1), DataType.Float32);
        var ack = BacnetApdu.BuildReadPropertyAck(9, id, BacnetConstants.PropertyPresentValue, value);

        var parsed = BacnetApdu.ParseReadProperty(ack, expectedInvokeId: 7);
        t.Ok(!parsed.Ok, "an ack for another request is not accepted");
        t.Contains(parsed.Describe, "invoke-id", "the error names the mismatch");

        return Task.CompletedTask;
    }

    private static Task ErrorMapsToAddressError(TestContext t)
    {
        // 对象不存在：error-class 1 是 object，error-code 31 是 unknown-object。
        // 这种错重试一万次也一样，必须落成 address_error 让人去改点表，而不是 bad。
        var error = BacnetApdu.BuildError(
            invokeId: 3, BacnetConstants.ServiceReadProperty, errorClass: 1, errorCode: 31);

        var parsed = BacnetApdu.ParseReadProperty(error, expectedInvokeId: 3);
        t.Ok(!parsed.Ok, "an error response is not a value");
        t.Equal((int)ErrorKind.Address, (int)parsed.Kind, "unknown-object is an address problem");
        t.Contains(parsed.Describe, "unknown-object", "the reason is preserved for the operator");

        // 越权（security/写拒绝）不是配置错，是权限问题。
        var denied = BacnetApdu.BuildError(4, BacnetConstants.ServiceWriteProperty, errorClass: 4, errorCode: 40);
        var deniedParsed = BacnetApdu.ParseReadProperty(denied, expectedInvokeId: 4);
        t.Equal((int)ErrorKind.Permission, (int)deniedParsed.Kind, "access denied is a permission problem");

        return Task.CompletedTask;
    }

    private static Task SegmentedAckIsUnsupported(TestContext t)
    {
        // 分段位（首字节 bit3）置上：本实现不重组，必须明确拒绝而不是把第一段当全值。
        var ack = BacnetApdu.BuildReadPropertyAck(
            invokeId: 1,
            new BacnetObjectId(BacnetConstants.ObjectTypeAnalogInput, 1),
            BacnetConstants.PropertyPresentValue,
            BacnetTags.EncodeValue(DataValue.Number(DataType.Float32, 1), DataType.Float32));

        ack[0] |= 0x08;

        var parsed = BacnetApdu.ParseReadProperty(ack, expectedInvokeId: 1);
        t.Ok(!parsed.Ok, "a segmented ack is not silently accepted");
        t.Equal((int)ErrorKind.Unsupported, (int)parsed.Kind, "it is reported as unsupported");
        t.Contains(parsed.Describe, "segmented", "the message says why");

        return Task.CompletedTask;
    }

    private static Task WritePropertyFrame(TestContext t)
    {
        // 手算 WriteProperty(analog-output:7, present-value, real 100.0)：
        //   00 04 02 0F           确认请求 / invoke-id 2 / 服务码 0F
        //   0C 00000007           [0] 对象标识符（类型 1 << 22 | 7 = 0x00400007）
        //   19 55                 [1] 属性标识符 85
        //   3E 44 42C80000 3F     [3] 打开标签 + real 100.0 的应用标签 + 关闭标签
        var id = new BacnetObjectId(BacnetConstants.ObjectTypeAnalogOutput, 7);
        var value = BacnetTags.EncodeValue(DataValue.Number(DataType.Float32, 100), DataType.Float32);
        t.Equal("4442C80000", Hex(value), "real 100.0");

        var apdu = BacnetApdu.BuildWriteProperty(id, BacnetConstants.PropertyPresentValue, value, invokeId: 2);

        t.Equal("0004020F0C0040000719553E4442C800003F", Hex(apdu),
            "write-property apdu matches the wire format");
        t.Ok(BacnetConstants.IsContextClosing(apdu[^1]), "the value is closed by a context closing tag");

        return Task.CompletedTask;
    }

    private static Task AckValueContainingClosingLookalike(TestContext t)
    {
        // 回归：属性值中间的字节可能长得像上下文关闭标签。real 1.25 = 3F A0 00 00，
        // 头一个字节 0x3F 低 4 位是 F。解析 ACK 时若只按「低 4 位 = F」判断关闭，
        // 会把 0x3F 误当关闭标签、把值从中间截断（声明 4 字节却只剩 0 字节）。
        // 关闭标签必须落在上下文 class（bit3=1）里。
        var id = new BacnetObjectId(BacnetConstants.ObjectTypeAnalogInput, 0);
        var value = BacnetTags.EncodeValue(DataValue.Number(DataType.Float32, 1.25), DataType.Float32);
        t.Equal("443FA00000", Hex(value), "real 1.25 carries a 0x3F byte in its payload");

        var ack = BacnetApdu.BuildReadPropertyAck(0, id, BacnetConstants.PropertyPresentValue, value);
        var parsed = BacnetApdu.ParseReadProperty(ack, expectedInvokeId: 0);
        t.Ok(parsed.Ok, $"the ack parses ({parsed.Describe})");

        var (tag, body, consumed) = BacnetTags.DecodeTag(parsed.ValueWithTag!);
        t.Equal((int)BacnetConstants.TagReal, (int)tag, "the tag is intact");
        // ValueWithTag 是「标签头 + 载荷」整段；DecodeTag 的 consumed 必须覆盖整段，
        // 否则说明在 0x3F 字节处提前收尾、把值截断了。
        t.Equal(parsed.ValueWithTag!.Length, consumed, "the value is not truncated at the 0x3F byte");
        t.Equal(4, body.Length, "real payload is the full 4 bytes");
        t.Close(1.25, BacnetTags.DecodeValue(tag, body, DataType.Float32).AsNumber(), 1e-6, "the value round-trips");

        return Task.CompletedTask;
    }

    private static Task SimpleAckRoundTrip(TestContext t)
    {
        var ack = BacnetApdu.BuildSimpleAck(invokeId: 5, BacnetConstants.ServiceWriteProperty);
        t.Equal("20050F", Hex(ack), "simple-ack is three bytes");

        var ok = BacnetApdu.ParseSimpleAck(ack, expectedInvokeId: 5,
            BacnetConstants.ServiceWriteProperty);
        t.Ok(ok.Ok, "the ack matches the request");

        var wrongId = BacnetApdu.ParseSimpleAck(ack, 6, BacnetConstants.ServiceWriteProperty);
        t.Ok(!wrongId.Ok, "a different invoke-id is not accepted");

        var wrongService = BacnetApdu.ParseSimpleAck(ack, 5, BacnetConstants.ServiceReadProperty);
        t.Ok(!wrongService.Ok, "a different service is not accepted");

        return Task.CompletedTask;
    }

    private static Task IAmParsesDeviceInstance(TestContext t)
    {
        // I-Am：10 10 (未确认/服务码) + 0C 000003E9 (设备对象标识符，实例 1001)
        //       + 21 04 (max-apdu 枚举码 4 = 1024 字节) + 21 00 (vendor 0)。
        var iAm = BacnetApdu.BuildIAm(deviceInstance: 1001);
        t.Equal("10100C020003E921042100", Hex(iAm), "i-am frame matches the wire format");
        t.Ok(BacnetApdu.IsIAm(iAm), "recognised as an I-Am");
        t.Equal(1001, BacnetApdu.ParseIAmDeviceInstance(iAm), "device instance comes from the object id");

        // Reject I-Am 以外的未确认服务，不能靠「长度够」猜。
        t.Ok(!BacnetApdu.IsIAm(Convert.FromHexString("1008")), "who-is is not an i-am");

        return Task.CompletedTask;
    }

    private static Task ObjectIdWireEncoding(TestContext t)
    {
        // 类型 10 位、实例 22 位，拼成一个 32 位无符号整数。
        var analogInput = new BacnetObjectId(BacnetConstants.ObjectTypeAnalogInput, 1);
        t.Equal(1u, analogInput.ToWire(), "analog-input:1");
        t.Equal("00000001", Hex(Convert.FromHexString("00000001")), "sanity");

        var binaryValue = new BacnetObjectId(BacnetConstants.ObjectTypeBinaryValue, 0x3FFFFF);
        t.Equal(0x017FFFFFu, binaryValue.ToWire(), "type 5 with the maximum instance");

        var parsed = BacnetObjectId.FromWire(binaryValue.ToWire());
        t.Equal(5, parsed.Type, "type survives the round trip");
        t.Equal(0x3FFFFF, parsed.Instance, "instance survives the round trip");

        t.Equal("binary-value:4194303", parsed.Describe(), "the label is human readable");

        return Task.CompletedTask;
    }

    private static Task AddressValidation(TestContext t)
    {
        // 寄存器/符号/文件寻址不能被 BACnet 驱动吞掉：静默当成某个对象会读错点位。
        t.Throws<DriverException>(
            () => BacnetObjectId.From(Address.Register("holding", 0, 1)),
            "register addressing is rejected");

        // objectType 必须是整数：别名（"analog-input"）留给配置层，编解码层不猜。
        t.Throws<DriverException>(
            () => BacnetObjectId.From(Address.Object("analog-input", 1, "85")),
            "a non-numeric object type is rejected");

        // 10 位类型字段与 22 位实例字段都有上限。
        t.Throws<DriverException>(
            () => BacnetObjectId.From(Address.Object("1024", 1, "85")),
            "object type beyond 10 bits is rejected");
        t.Throws<DriverException>(
            () => BacnetObjectId.From(Address.Object("0", 0x400000, "85")),
            "instance beyond 22 bits is rejected");

        // property 缺省是 present-value：采集点位默认读当前值。
        var noProperty = Address.Object("0", 1, "");
        t.Equal(BacnetConstants.PropertyPresentValue, BacnetObjectId.PropertyOf(noProperty),
            "an empty property defaults to present-value");

        t.Throws<DriverException>(
            () => BacnetObjectId.PropertyOf(Address.Object("0", 1, "present-value")),
            "a named property is rejected rather than guessed");

        return Task.CompletedTask;
    }

    private static Task TagsRoundTrip(TestContext t)
    {
        AssertTag(t, DataValue.Number(DataType.UInt16, 300), DataType.UInt16,
            "22012C", "unsigned 300 is two bytes, minimal");
        AssertTag(t, DataValue.Number(DataType.UInt32, 0), DataType.UInt32,
            "2100", "zero is a single zero byte");
        AssertTag(t, DataValue.Number(DataType.Int32, -2), DataType.Int32,
            "31FE", "signed -2 is one byte 0xFE with its sign bit");
        AssertTag(t, DataValue.Boolean(true), DataType.Bool,
            "11", "true is the tag header itself");
        AssertTag(t, DataValue.Boolean(false), DataType.Bool,
            "10", "false is the tag header itself");
        AssertTag(t, DataValue.Number(DataType.Float32, 0.5), DataType.Float32,
            "443F000000", "real 0.5");

        // character-string：载荷 = 1 字节字符集 + 正文。4 个字符 → 5 字节载荷，
        // 已超过短格式上限（4），必须用扩展长度：75 05 03 "test"。
        AssertTag(t, DataValue.String("test"), DataType.String,
            "75050374657374", "character string carries its charset byte");

        // 更长的正文同样走扩展长度：12 字节正文 + 1 字节字符集 = 13 = 0x0D 字节载荷。
        AssertTag(t, DataValue.String("123456789012"), DataType.String,
            "750D03313233343536373839303132", "a 13-byte payload uses the extended length form");

        // 3 个字符 → 4 字节载荷，正好落在短格式上限内（LVT=4，没有扩展长度字节）。
        AssertTag(t, DataValue.String("abc"), DataType.String,
            "7403616263", "a 4-byte payload stays in the short form");

        return Task.CompletedTask;
    }

    private static void AssertTag(TestContext t, DataValue value, DataType type, string expected, string what)
    {
        var encoded = BacnetTags.EncodeValue(value, type);
        t.Equal(expected, Hex(encoded), $"{what}: encoding");

        var (tag, body, consumed) = BacnetTags.DecodeTag(encoded);
        t.Equal(encoded.Length, consumed, $"{what}: the tag declares its own length");

        var decoded = BacnetTags.DecodeValue(tag, body, type);
        t.Equal(value.AsNumber(), decoded.AsNumber(), $"{what}: value round-trips");
    }

    private static Task TagsRejectInvalid(TestContext t)
    {
        // bool 点位读回 real：一定是读错了属性，不能硬转成 0/1。
        var real = BacnetTags.EncodeValue(DataValue.Number(DataType.Float32, 1), DataType.Float32);
        var (tag, body, _) = BacnetTags.DecodeTag(real);
        t.Throws<DriverException>(() => BacnetTags.DecodeValue(tag, body, DataType.Bool),
            "a real value cannot be decoded into bool");

        // 字符串点位读回 unsigned 同理。
        var unsigned = BacnetTags.EncodeValue(DataValue.Number(DataType.UInt16, 7), DataType.UInt16);
        var (utag, ubody, _) = BacnetTags.DecodeTag(unsigned);
        t.Throws<DriverException>(() => BacnetTags.DecodeValue(utag, ubody, DataType.String),
            "an integer value cannot be decoded into a string");

        // 整数装不进声明类型：必须报错，不能截断。
        var big = BacnetTags.EncodeValue(DataValue.Number(DataType.UInt32, 70_000), DataType.UInt32);
        var (btag, bbody, _) = BacnetTags.DecodeTag(big);
        t.Throws<DriverException>(() => BacnetTags.DecodeValue(btag, bbody, DataType.UInt16),
            "70000 does not fit uint16");

        // NaN / Infinity：BACnet 用它们表示无效值，映射成 0 会把「无效」变成「零度」。
        var nan = new byte[] { 0x44, 0x7F, 0xC0, 0x00, 0x00 };
        var (ntag, nbody, _) = BacnetTags.DecodeTag(nan);
        t.Throws<DriverException>(() => BacnetTags.DecodeValue(ntag, nbody, DataType.Float32),
            "a NaN real is rejected");

        // 截断的标签：声明 4 字节但只剩 2 字节。
        t.Throws<DriverException>(() => BacnetTags.DecodeTag(new byte[] { 0x44, 0x00, 0x00 }),
            "a truncated tag is rejected");

        // 上下文标签出现在值的位置：说明报文结构不对，不是应用值。
        t.Throws<DriverException>(() => BacnetTags.DecodeTag(new byte[] { 0x2E, 0x00 }),
            "a context tag is not an application value");

        return Task.CompletedTask;
    }

    private static Task NpduVersionMustBeOne(TestContext t)
    {
        // NPDU 版本不是 1：真设备会把报文当坏帧丢掉。解析端也必须拒绝，
        // 否则「长度自洽但内容错位」的坏帧会被当成正常响应。
        var frame = BacnetFrame.Build(BacnetApdu.BuildWhoIs(), broadcast: true).ToArray();
        frame[BacnetConstants.BvllHeaderLength] = 0x00;

        t.Throws<DriverException>(() => BacnetFrame.Payload(frame).ToArray(),
            "NPDU version 0 is rejected");

        // 带路由信息的控制字节同样拒绝：本实现只处理一跳直连。
        var routed = BacnetFrame.Build(BacnetApdu.BuildWhoIs(), broadcast: true).ToArray();
        routed[BacnetConstants.BvllHeaderLength + 1] = 0x20;

        t.Throws<DriverException>(() => BacnetFrame.Payload(routed).ToArray(),
            "a routed NPDU is not silently mis-parsed");

        return Task.CompletedTask;
    }
}
