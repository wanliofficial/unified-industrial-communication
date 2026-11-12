using Uic.Codec.S7;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Tests;

/// <summary>
/// S7 报文级验收：帧字节按抓包格式手算，解析再按同一份字节验证。
///
/// 为什么不用「编码一次、解码一次、两边相等」的自洽断言：那只能证明实现前后一致，
/// 证明不了它和真 PLC 说同一种语言。这里的每一串 hex 都是对着协议格式逐字段写出来的
/// （TPKT 长度、COTP 参数、S7 头里 param/data 的长度、S7-Any 的位偏移），
/// 编码器少一个字节或长度单位写错（位 vs 字节）都会立刻失败。
/// </summary>
internal static class S7CodecTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("s7/tpkt_wraps_and_unwraps", TpktRoundTrip));
        tests.Add(new TestCase("s7/connect_request_frame", ConnectRequestFrame));
        tests.Add(new TestCase("s7/connect_request_encodes_rack_and_slot", ConnectRackSlot));
        tests.Add(new TestCase("s7/setup_request_frame", SetupRequestFrame));
        tests.Add(new TestCase("s7/setup_response_takes_plc_pdu", SetupResponseTakesPlcValue));
        tests.Add(new TestCase("s7/read_request_frame", ReadRequestFrame));
        tests.Add(new TestCase("s7/read_response_decodes_values", ReadResponseDecodesValues));
        tests.Add(new TestCase("s7/read_response_length_is_in_bits", ReadResponseLengthIsInBits));
        tests.Add(new TestCase("s7/read_response_error_code_keeps_other_items", ReadResponseErrorKeepsOthers));
        tests.Add(new TestCase("s7/write_request_frame", WriteRequestFrame));
        tests.Add(new TestCase("s7/write_response_returns_codes", WriteResponseReturnsCodes));
        tests.Add(new TestCase("s7/bit_frames", BitFrames));
        tests.Add(new TestCase("s7/address_parses_areas", AddressParsesAreas));
        tests.Add(new TestCase("s7/address_rejects_bad_input", AddressRejectsBadInput));
        tests.Add(new TestCase("s7/tpkt_rejects_bad_version", TpktRejectsBadVersion));
        tests.Add(new TestCase("s7/cotp_rejects_fragmented_pdu", CotpRejectsFragmentedPdu));
    }

    private static Task TpktRoundTrip(TestContext t)
    {
        var payload = Convert.FromHexString("11E00000000100");
        var frame = Tpkt.Build(payload);

        t.Equal("0300000B11E00000000100", Hex(frame), "TPKT header is 4 bytes and counts itself");
        t.Equal(Hex(payload), Hex(Tpkt.Payload(frame)), "payload is returned unchanged");
        t.Equal(11, Tpkt.TotalLength(frame), "total length comes from the header");

        return Task.CompletedTask;
    }

    private static Task ConnectRequestFrame(TestContext t)
    {
        // 手算：TPKT(03 00 00 16) + COTP CR(11 E0) + dst 0000 + src 0001 + class 00
        //      + C0 01 0A（TPDU 1024）+ C1 02 0100（本端 TSAP）
        //      + C2 02 0101（对端 TSAP = 机架 0 / 槽位 1）。
        var frame = Cotp.BuildConnectRequest(rack: 0, slot: 1);

        t.Equal(
            "0300001611E00000000100C0010AC1020100C2020101",
            Hex(frame),
            "connect request exactly matches the wire format");

        // 确认方向也要能解析：回一个同样的参数字段，PDU 尺寸应读成 1024。
        var confirm = Convert.FromHexString("03000016" + "11D00001000000C0010AC1020100C2020101");
        t.Equal(1024, Cotp.NegotiatedTpduSize(Tpkt.Payload(confirm)), "TPDU size comes from the C0 parameter");

        return Task.CompletedTask;
    }

    private static Task ConnectRackSlot(TestContext t)
    {
        // 目的 TSAP 第二字节 = rack << 5 | slot。S7-300 常见 0/2，S7-1200/1500 是 0/1。
        var r0s1 = Cotp.BuildConnectRequest(0, 1);
        var r0s2 = Cotp.BuildConnectRequest(0, 2);
        var r1s3 = Cotp.BuildConnectRequest(1, 3);

        t.Equal(0x0101, LastTsap(r0s1), "rack 0 slot 1");
        t.Equal(0x0102, LastTsap(r0s2), "rack 0 slot 2");
        t.Equal(0x0123, LastTsap(r1s3), "rack 1 slot 3");

        return Task.CompletedTask;
    }

    private static Task SetupRequestFrame(TestContext t)
    {
        // S7 Job 头 10 字节 + 参数 8 字节：
        // 32 01 | 0000 | 0001 | 0008 | 0000 | F0 00 0001 0001 01E0
        t.Equal(
            "32010000000100080000F0000001000101E0",
            Hex(S7Setup.BuildRequest(0x0001, 480)),
            "setup communication job");

        return Task.CompletedTask;
    }

    private static Task SetupResponseTakesPlcValue(TestContext t)
    {
        // PLC 只给 240：驱动必须按 240 组帧，而不是按自己请求的 960。
        var response = S7Frame.Parse(Convert.FromHexString(
            "320300000001000800000000" + "F00000010001" + "00F0"));

        t.Equal(240, S7Setup.ParseResponse(response), "the PLC's answer wins over the requested value");

        return Task.CompletedTask;
    }

    private static Task ReadRequestFrame(TestContext t)
    {
        // 读 DB1.DBB0 起 2 字节。请求项 12 字节：
        // 12（变量规范）0A（长度）10（S7-Any）02（BYTE）0002（2 个）0001（DB1）84（DB 区）000000（位偏移 0）
        var item = new S7ReadItem(new S7Address(S7Constants.AreaDataBlock, 1, 0, -1), 2, false);

        t.Equal(
            "320100000002000E0000" + "0401120A10020002000184000000",
            Hex(S7Read.BuildRequest(0x0002, new[] { item })),
            "read var job for DB1.DBB0, 2 bytes");

        return Task.CompletedTask;
    }

    private static Task ReadResponseDecodesValues(TestContext t)
    {
        // 32 03 | 0000 | 0002 | 000E（参数 14）| 0006（数据 6）| 0000（无错误）
        // 参数：04 01；数据项：FF（成功）04（BYTE）0010（16 位 = 2 字节）1234
        var pdu = S7Frame.Parse(Convert.FromHexString(
            "3203000000020002000600 00".Replace(" ", "") + "0401" + "FF0400101234"));

        var items = S7Read.ParseResponse(pdu, 1);
        t.Equal(1, items.Count, "one item back");
        t.Ok(items[0].Ok, "return code is 0xFF");
        t.Equal("1234", Hex(items[0].Data), "two data bytes");

        var value = ValueCodec.Decode(items[0].Data, DataType.UInt16, ByteOrder.ABCD, 1);
        t.Equal(4660d, value.AsNumber(), "uint16 is big-endian on the wire");

        return Task.CompletedTask;
    }

    private static Task ReadResponseLengthIsInBits(TestContext t)
    {
        // 传输尺寸 0x04 / 0x03 / 0x05 / 0x06 / 0x07 的长度字段都是**位**，只有 0x09 是字节。
        // 把 32 位当 32 字节读，会吃进后面所有的项，而且看起来只是「值不对」。
        var pdu = S7Frame.Parse(Convert.FromHexString(
            "320300000003000200080000" + "0401" +
            "FF04002012345678"));

        var items = S7Read.ParseResponse(pdu, 1);
        t.Equal("12345678", Hex(items[0].Data), "32 bits means 4 bytes, not 32");

        return Task.CompletedTask;
    }

    private static Task ReadResponseErrorKeepsOthers(TestContext t)
    {
        // 两项：第一项地址越界（05），第二项正常。一项失败不能让整包失败——
        // 上层按点位逐个标质量，一次越界不该把同窗的好点一起判死。
        var pdu = S7Frame.Parse(Convert.FromHexString(
            "3203000000050002000A0000" + "0402" +
            "05000000" +
            "FF040010002A"));

        var items = S7Read.ParseResponse(pdu, 2);
        t.Ok(!items[0].Ok, "first item failed");
        t.Equal(S7Constants.ReturnAddressOutOfRange, items[0].ReturnCode, "return code 0x05 is preserved");
        t.Equal(Quality.AddressError, S7Write.QualityFor(items[0].ReturnCode), "0x05 maps to address_error");
        t.Ok(items[1].Ok, "second item still decoded");
        t.Equal("002A", Hex(items[1].Data), "second item's 2 bytes");

        return Task.CompletedTask;
    }

    private static Task WriteRequestFrame(TestContext t)
    {
        // 写 DB1.DBB0 = 12 34。数据项：00（保留返回码）04（BYTE）0010（16 位）1234
        var item = new S7WriteItem(
            new S7Address(S7Constants.AreaDataBlock, 1, 0, -1), false, new byte[] { 0x12, 0x34 });

        t.Equal(
            "320100000003000E0006" + "0501120A10020002000184000000" + "000400101234",
            Hex(S7Write.BuildRequest(0x0003, new[] { item })),
            "write var job for DB1.DBB0, 2 bytes");

        return Task.CompletedTask;
    }

    private static Task WriteResponseReturnsCodes(TestContext t)
    {
        // 参数 05 01，数据只有每项一个字节的返回码。
        var pdu = S7Frame.Parse(Convert.FromHexString("3203000000030002000100000501FF"));

        var codes = S7Write.ParseResponse(pdu, 1);
        t.Equal(1, codes.Count, "one return code");
        t.Equal(S7Constants.ReturnOk, codes[0], "write succeeded");
        return Task.CompletedTask;
    }

    private static Task BitFrames(TestContext t)
    {
        // 读 DB1.DBX0.3：请求项传输尺寸 01（BIT）、个数 1、位偏移 3。
        var item = new S7ReadItem(new S7Address(S7Constants.AreaDataBlock, 1, 0, 3), 1, true);

        t.Equal(
            "320100000004000E0000" + "0401120A10010001000184000003",
            Hex(S7Read.BuildRequest(0x0004, new[] { item })),
            "bit read request uses transport size BIT and a bit offset");

        // 响应：传输尺寸 03（BIT）、长度 0001（1 位）、数据 01，末尾补齐到偶数。
        var pdu = S7Frame.Parse(Convert.FromHexString(
            "3203000000040002000600 00".Replace(" ", "") + "0401" + "FF0300010100"));

        var bits = S7Read.ParseResponse(pdu, 1);
        t.Equal("01", Hex(bits[0].Data), "one bit is one data byte");

        return Task.CompletedTask;
    }

    private static Task AddressParsesAreas(TestContext t)
    {
        static S7Address Parse(string area, int offset = 0, int bit = -1) =>
            S7Address.Parse(Address.Register(area, offset, 0, bit));

        t.Equal(new S7Address(S7Constants.AreaDataBlock, 12, 8, -1), Parse("db12", 8), "db12");
        t.Equal(new S7Address(S7Constants.AreaDataBlock, 1, 0, -1), Parse("DB", 0), "bare db defaults to db1");
        t.Equal(S7Constants.AreaInput, Parse("i").Area, "i");
        t.Equal(S7Constants.AreaInput, Parse("E").Area, "E is the German input alias");
        t.Equal(S7Constants.AreaOutput, Parse("qb").Area, "qb");
        t.Equal(S7Constants.AreaFlag, Parse("merker").Area, "merker");
        t.Equal(S7Constants.AreaLocal, Parse("l").Area, "l");
        t.Equal(S7Constants.AreaPeripheral, Parse("pe").Area, "pe");
        t.Equal(S7Constants.AreaCounter, Parse("c").Area, "c");
        t.Equal(S7Constants.AreaTimer, Parse("t").Area, "t");

        t.Equal("DB12.DBX8.3", Parse("db12", 8, 3).Describe(), "bit addresses are described the way STEP 7 shows them");
        t.Equal("MB4", Parse("m", 4).Describe(), "byte addresses too");

        // 边界本身必须合法：2 097 151 字节 + 第 7 位正好是 0xFFFFFF。
        t.Equal(0xFFFFFF, Parse("db1", 2_097_151, 7).BitOffset, "the last addressable byte still fits the field");

        return Task.CompletedTask;
    }

    private static Task AddressRejectsBadInput(TestContext t)
    {
        var unknown = t.Throws<DriverException>(
            () => S7Address.Parse(Address.Register("xyz", 0)), "unknown area");
        t.Equal(ErrorKind.Unsupported, unknown.Kind, "unknown area is a configuration error, not a comm error");

        var badDb = t.Throws<DriverException>(
            () => S7Address.Parse(Address.Register("dbx1", 0)), "malformed db number");
        t.Equal(ErrorKind.Address, badDb.Kind, "db<non-number> is an address error");

        var negative = t.Throws<DriverException>(
            () => S7Address.Parse(Address.Register("db1", -1)), "negative offset");
        t.Equal(ErrorKind.Address, negative.Kind, "negative offset is an address error");

        var bit = t.Throws<DriverException>(
            () => S7Address.Parse(Address.Register("db1", 0, 0, 8)), "bit 8");
        t.Equal(ErrorKind.Address, bit.Kind, "S7 only has bits 0..7");
        t.Contains(bit.Message, "0..7", "the message names the valid range");

        var symbol = t.Throws<DriverException>(
            () => S7Address.Parse(Address.Symbol("Motor.Speed")), "symbol addressing");
        t.Equal(ErrorKind.Unsupported, symbol.Kind, "symbol addressing is explicitly unsupported, not silently wrong");

        // 线上地址字段是 3 字节的位偏移，最大 0xFFFFFF 位 = 2 097 151 字节。
        // 超过后写帧时的 (byte) 截断会把地址绕回到另一处，点位质量仍然是 good，
        // 读到的却是另一个变量的值——比直接报错难发现得多。
        var tooFar = t.Throws<DriverException>(
            () => S7Address.Parse(Address.Register("db1", 2_097_152)), "offset beyond the wire field");
        t.Equal(ErrorKind.Address, tooFar.Kind, "an offset outside the address field is an address error");
        t.Contains(tooFar.Message, "2097151", "the message names the largest byte offset");

        return Task.CompletedTask;
    }

    private static Task TpktRejectsBadVersion(TestContext t)
    {
        var bad = t.Throws<DriverException>(
            () => Tpkt.TotalLength(Convert.FromHexString("02000010")), "version 2");
        t.Equal(ErrorKind.Protocol, bad.Kind, "a non-TPKT stream is a protocol error");

        var shortFrame = t.Throws<DriverException>(
            () => Tpkt.Payload(Convert.FromHexString("030000200102")), "truncated frame");
        t.Equal(ErrorKind.Protocol, shortFrame.Kind, "a truncated frame is not 'read more'");

        return Task.CompletedTask;
    }

    private static Task CotpRejectsFragmentedPdu(TestContext t)
    {
        // EOT 位为 0 表示 PLC 把 PDU 分了片。本实现不做重组，
        // 必须明确报 unsupported——当成完整 PDU 解会把半截数据当读数。
        var ex = t.Throws<DriverException>(
            () => Cotp.ExpectData(Convert.FromHexString("02F0003201")).ToArray(), "no EOT bit");
        t.Equal(ErrorKind.Unsupported, ex.Kind, "fragmentation is an explicit unsupported case");

        // 断开请求（0x80）要报成连接失败，而不是协议错：现场原因通常是 rack/slot 或 PUT/GET 没开。
        var refused = t.Throws<DriverException>(
            () => Cotp.ExpectConnectConfirm(Convert.FromHexString("06800000000000")), "disconnect");
        t.Equal(ErrorKind.Connection, refused.Kind, "a refused COTP connection is a connection error");

        return Task.CompletedTask;
    }

    // ------------------------------------------------------------------

    private static ushort LastTsap(byte[] frame) =>
        System.Buffers.Binary.BinaryPrimitives.ReadUInt16BigEndian(frame.AsSpan(frame.Length - 2, 2));

    private static string Hex(ReadOnlySpan<byte> bytes) => Convert.ToHexString(bytes);
}
