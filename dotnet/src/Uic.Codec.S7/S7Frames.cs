using System.Buffers.Binary;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Codec.S7;

/// <summary>
/// S7comm 线上常量。数值全部来自实际抓包与公开的报文解析资料，
/// 不用「看起来合理」的猜测——这些数字错了，现象是「连上了但读什么都报地址错」。
/// </summary>
public static class S7Constants
{
    /// <summary>TPKT 头固定 4 字节：版本 3、保留 0、总长度（含头）。</summary>
    public const int TpktHeaderLength = 4;

    public const byte TpktVersion = 0x03;

    /// <summary>COTP PDU 类型。</summary>
    public const byte CotpConnectRequest = 0xE0;
    public const byte CotpConnectConfirm = 0xD0;
    public const byte CotpDisconnectRequest = 0x80;
    public const byte CotpData = 0xF0;

    /// <summary>S7 协议标识，永远是 0x32。</summary>
    public const byte ProtocolId = 0x32;

    /// <summary>报文类型（ROSCTR）。</summary>
    public const byte RosctrJob = 0x01;
    public const byte RosctrAck = 0x02;
    public const byte RosctrAckData = 0x03;

    /// <summary>功能码。</summary>
    public const byte FuncSetupCommunication = 0xF0;
    public const byte FuncReadVar = 0x04;
    public const byte FuncWriteVar = 0x05;

    /// <summary>Job 的 S7 头是 10 字节；Ack / Ack_Data 多两个错误字段，共 12 字节。</summary>
    public const int JobHeaderLength = 10;
    public const int AckDataHeaderLength = 12;

    /// <summary>存储区。DB 区的编号在请求项里单独给，所以这里只有区域类型。</summary>
    public const byte AreaPeripheral = 0x80;
    public const byte AreaInput = 0x81;
    public const byte AreaOutput = 0x82;
    public const byte AreaFlag = 0x83;
    public const byte AreaDataBlock = 0x84;
    public const byte AreaLocal = 0x86;
    public const byte AreaCounter = 0x1C;
    public const byte AreaTimer = 0x1D;

    /// <summary>请求项里的「变量类型」（也是传输尺寸的请求侧写法）。</summary>
    public const byte VarBit = 0x01;
    public const byte VarByte = 0x02;

    /// <summary>响应数据项里的传输尺寸。长度字段的单位跟着它变，混用会读出半截数据。</summary>
    public const byte DataBit = 0x03;
    public const byte DataByteWordDword = 0x04;
    public const byte DataInteger = 0x05;
    public const byte DataDint = 0x06;
    public const byte DataReal = 0x07;
    public const byte DataOctetString = 0x09;

    /// <summary>响应项的返回码。</summary>
    public const byte ReturnOk = 0xFF;
    public const byte ReturnHardwareFault = 0x01;
    public const byte ReturnNotAllowed = 0x03;
    public const byte ReturnAddressOutOfRange = 0x05;
    public const byte ReturnTypeNotSupported = 0x06;
    public const byte ReturnTypeInconsistent = 0x07;
    public const byte ReturnObjectNotExist = 0x0A;

    /// <summary>S7-Any 寻址的固定字段。</summary>
    public const byte SpecTypeVariable = 0x12;
    public const byte SyntaxIdS7Any = 0x10;

    /// <summary>请求项里固定为 0x0A 的地址规范长度。</summary>
    public const byte AnyItemLength = 0x0A;

    /// <summary>PDU 协商的几个档位。请求大一点、由 PLC 回它真正的上限，比自己猜安全。</summary>
    public const int PduSmall = 240;
    public const int PduMedium = 480;
    public const int PduLarge = 960;

    /// <summary>把返回码翻成人话。日志与质量码都靠它，号多但只想看这一句。</summary>
    public static string DescribeReturn(byte code) => code switch
    {
        ReturnOk => "ok",
        ReturnHardwareFault => "hardware fault",
        ReturnNotAllowed => "accessing the object is not allowed (CPU protection?)",
        ReturnAddressOutOfRange => "address out of range",
        ReturnTypeNotSupported => "data type not supported",
        ReturnTypeInconsistent => "data type inconsistent",
        ReturnObjectNotExist => "object does not exist",
        _ => $"unknown return code 0x{code:x2}",
    };
}

/// <summary>
/// 一次 S7 访问的物理地址：区域 + DB 号 + 字节偏移 + 位。
///
/// 平台模型里 register 的 <c>offset</c> 是「寄存器偏移」，S7 的地址天生是字节加位
/// （DB1.DBX4.3）。所以 S7 驱动把 <c>offset</c> 解释成字节偏移，并在文档与
/// <c>ConfigFields</c> 里写明白——两种单位混着用会读出「值像是对的但差一半」的数据。
/// </summary>
public readonly record struct S7Address(byte Area, int Db, int ByteOffset, int Bit)
{
    public bool IsBit => Bit >= 0;

    /// <summary>线上地址字段是位偏移，3 字节大端。</summary>
    public int BitOffset => ByteOffset * 8 + Math.Max(0, Bit);

    /// <summary>
    /// 解析配置里的 <c>address.area</c>。接受德系外语混写的常见别名：
    /// 现场既有写 <c>db1</c> 的，也有写 <c>DB1</c> / <c>i</c> / <c>e</c> 的。
    /// </summary>
    public static S7Address Parse(Address address, string protocol = "s7")
    {
        if (address.Kind != AddressKind.Register)
            throw DriverException.Unsupported(protocol, "encode",
                $"address kind '{address.Kind}' is not supported by S7; use register addressing with a byte offset");

        if (address.Offset < 0)
            throw DriverException.Address(protocol, "encode", $"negative byte offset {address.Offset}");

        // S7 的位只有 0..7。平台层的 address.bit 允许到 15（Modbus 的 16 位字），
        // 直接把 8..15 当作 S7 位号会静默跳到下一个字节，读出错位的值。
        if (address.Bit > 7)
            throw DriverException.Address(protocol, "encode",
                $"bit {address.Bit} is out of range for S7 (0..7); byte {address.Offset} only has 8 bits");

        var (area, db) = ParseArea(address.Area, protocol);
        return new S7Address(area, db, address.Offset, address.Bit);
    }

    public static (byte Area, int Db) ParseArea(string? area, string protocol = "s7")
    {
        var key = (area ?? string.Empty).Trim().ToLowerInvariant();
        if (key.Length == 0)
            throw DriverException.Unsupported(protocol, "encode",
                "address.area is required (db<N>, i, q, m, l, p, c, t)");

        // db12 / db / datablock12 —— 编号直接嵌在区域名里，这样不用给 Address 加字段。
        if (key.StartsWith("db", StringComparison.Ordinal))
        {
            var digits = key[2..];
            if (digits.Length == 0) return (S7Constants.AreaDataBlock, 1);
            if (!int.TryParse(digits, out var db) || db is < 1 or > 65535)
                throw DriverException.Address(protocol, "encode",
                    $"'{area}' is not a valid data block; expected db1..db65535");
            return (S7Constants.AreaDataBlock, db);
        }

        return key switch
        {
            "i" or "ib" or "input" or "inputs" or "e" => (S7Constants.AreaInput, 0),
            "q" or "qb" or "output" or "outputs" or "a" => (S7Constants.AreaOutput, 0),
            "m" or "mb" or "flag" or "flags" or "marker" or "merker" => (S7Constants.AreaFlag, 0),
            "l" or "local" => (S7Constants.AreaLocal, 0),
            "p" or "pe" or "peripheral" => (S7Constants.AreaPeripheral, 0),
            "c" or "counter" or "counters" => (S7Constants.AreaCounter, 0),
            "t" or "timer" or "timers" => (S7Constants.AreaTimer, 0),
            _ => throw DriverException.Unsupported(protocol, "encode",
                $"unknown area '{area}'; expected db<N>, i, q, m, l, p, c, t"),
        };
    }

    public string Describe() => Area switch
    {
        S7Constants.AreaDataBlock => IsBit ? $"DB{Db}.DBX{ByteOffset}.{Bit}" : $"DB{Db}.DBB{ByteOffset}",
        S7Constants.AreaInput => IsBit ? $"I{ByteOffset}.{Bit}" : $"IB{ByteOffset}",
        S7Constants.AreaOutput => IsBit ? $"Q{ByteOffset}.{Bit}" : $"QB{ByteOffset}",
        S7Constants.AreaFlag => IsBit ? $"M{ByteOffset}.{Bit}" : $"MB{ByteOffset}",
        S7Constants.AreaLocal => IsBit ? $"L{ByteOffset}.{Bit}" : $"LB{ByteOffset}",
        S7Constants.AreaPeripheral => IsBit ? $"P{ByteOffset}.{Bit}" : $"PB{ByteOffset}",
        S7Constants.AreaCounter => $"C{ByteOffset}",
        S7Constants.AreaTimer => $"T{ByteOffset}",
        _ => $"?0x{Area:x2}:{ByteOffset}",
    };
}

/// <summary>TPKT（RFC 1006）分帧：4 字节头 + 载荷，长度字段含头本身。</summary>
public static class Tpkt
{
    public static byte[] Build(ReadOnlySpan<byte> payload)
    {
        var frame = new byte[S7Constants.TpktHeaderLength + payload.Length];
        frame[0] = S7Constants.TpktVersion;
        frame[1] = 0;
        BinaryPrimitives.WriteUInt16BigEndian(frame.AsSpan(2, 2), (ushort)frame.Length);
        payload.CopyTo(frame.AsSpan(S7Constants.TpktHeaderLength));
        return frame;
    }

    /// <summary>从 4 字节头里取出整帧长度。小于 7 说明对端发了坏帧，不是「还没读完」。</summary>
    public static int TotalLength(ReadOnlySpan<byte> header)
    {
        if (header.Length < S7Constants.TpktHeaderLength)
            throw DriverException.ProtocolError("s7", "tpkt", "TPKT header shorter than 4 bytes");

        if (header[0] != S7Constants.TpktVersion)
            throw DriverException.ProtocolError("s7", "tpkt",
                $"unsupported TPKT version 0x{header[0]:x2} (expected 0x03)");

        var length = BinaryPrimitives.ReadUInt16BigEndian(header.Slice(2, 2));
        if (length < S7Constants.TpktHeaderLength + 3)
            throw DriverException.ProtocolError("s7", "tpkt", $"invalid TPKT length {length}");

        return length;
    }

    /// <summary>去掉 TPKT 头，返回 COTP 及之后的字节。</summary>
    public static ReadOnlySpan<byte> Payload(ReadOnlySpan<byte> frame)
    {
        var length = TotalLength(frame);
        if (frame.Length < length)
            throw DriverException.ProtocolError("s7", "tpkt",
                $"truncated frame: have {frame.Length}, need {length}");
        return frame.Slice(S7Constants.TpktHeaderLength, length - S7Constants.TpktHeaderLength);
    }
}

/// <summary>
/// COTP（ISO 8073）连接管理。只实现 S7 实际用到的三种包：
/// 连接请求、连接确认、数据传输。其余（断开、TPDU 分片）这里不做，
/// 遇到就明确报错，而不是当数据包继续往下解。
/// </summary>
public static class Cotp
{
    public const int ConnectHeaderLength = 7;

    /// <summary>
    /// 连接请求。目的 TSAP 的第二字节编码机架与槽位（rack &lt;&lt; 5 | slot），
    /// 这是 S7 唯一必须配对的「硬件位置」信息：配错的表现是 TCP 通、握手超时。
    /// </summary>
    public static byte[] BuildConnectRequest(int rack, int slot, ushort localTsap = 0x0100)
    {
        var remoteTsap = (ushort)(0x0100 | ((rack & 0x07) << 5) | (slot & 0x1F));

        var cotp = new byte[18];
        cotp[0] = 17;                                     // 长度：后续 17 字节
        cotp[1] = S7Constants.CotpConnectRequest;
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(2, 2), 0x0000); // dst ref
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(4, 2), 0x0001); // src ref
        cotp[6] = 0x00;                                   // class 0
        cotp[7] = 0xC0; cotp[8] = 0x01; cotp[9] = 0x0A;   // TPDU size = 1024
        cotp[10] = 0xC1; cotp[11] = 0x02;
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(12, 2), localTsap);
        cotp[14] = 0xC2; cotp[15] = 0x02;
        BinaryPrimitives.WriteUInt16BigEndian(cotp.AsSpan(16, 2), remoteTsap);

        return Tpkt.Build(cotp);
    }

    /// <summary>DT 包：3 字节头（长度 2、类型 0xF0、EOT 0x80）。</summary>
    public static byte[] BuildData(ReadOnlySpan<byte> s7pdu)
    {
        var cotp = new byte[3 + s7pdu.Length];
        cotp[0] = 0x02;
        cotp[1] = S7Constants.CotpData;
        cotp[2] = 0x80;
        s7pdu.CopyTo(cotp.AsSpan(3));
        return Tpkt.Build(cotp);
    }

    /// <summary>连接确认是否成功。失败时把 PLC 的说法原样带出去。</summary>
    public static void ExpectConnectConfirm(ReadOnlySpan<byte> cotp)
    {
        if (cotp.Length < ConnectHeaderLength)
            throw DriverException.ProtocolError("s7", "connect", "COTP confirm shorter than 7 bytes");

        var type = cotp[1];
        if (type == S7Constants.CotpDisconnectRequest || type == 0x81 /* DR */)
            throw DriverException.Connection("s7", "connect",
                "PLC refused the COTP connection (check rack/slot and that PUT/GET is allowed)");

        if (type != S7Constants.CotpConnectConfirm)
            throw DriverException.ProtocolError("s7", "connect",
                $"expected COTP connect confirm (0xD0), got 0x{type:x2}");
    }

    /// <summary>确认载包携带的 TPDU 尺寸。返回 0 表示对端没给。</summary>
    public static int NegotiatedTpduSize(ReadOnlySpan<byte> cotp)
    {
        if (cotp.Length < ConnectHeaderLength) return 0;

        // 参数区从第 7 字节开始，逐项 [code, len, payload...]。
        var i = ConnectHeaderLength;
        while (i + 2 <= cotp.Length)
        {
            var code = cotp[i];
            var len = cotp[i + 1];
            if (i + 2 + len > cotp.Length) break;

            if (code == 0xC0 && len >= 1)
            {
                // 参数值是指数：0x0A 表示 2^10 = 1024 字节。
                // 上限 13（8192）是 ISO 8073 的规格；超出说明对端发了坏参数。
                var exponent = cotp[i + 2];
                return exponent is >= 7 and <= 13 ? 1 << exponent : 0;
            }

            i += 2 + len;
        }

        return 0;
    }

    /// <summary>数据包必须带 EOT 位：没有它说明 PLC 把 PDU 分了片，本实现不支持。</summary>
    public static ReadOnlySpan<byte> ExpectData(ReadOnlySpan<byte> cotp)
    {
        if (cotp.Length < 3)
            throw DriverException.ProtocolError("s7", "read", "COTP data shorter than 3 bytes");

        if (cotp[1] != S7Constants.CotpData)
            throw DriverException.ProtocolError("s7", "read",
                $"expected COTP data (0xF0), got 0x{cotp[1]:x2}");

        if ((cotp[2] & 0x80) == 0)
            throw DriverException.Unsupported("s7", "read",
                "PLC fragmented the S7 PDU; TPDU reassembly is not implemented");

        return cotp.Slice(3);
    }
}

/// <summary>解出来的 S7 报文。Parameter 与 Data 分开，调用方只关心自己那一段。</summary>
public readonly record struct S7Pdu(
    byte Rosctr,
    ushort PduReference,
    byte ErrorClass,
    byte ErrorCode,
    byte[] Parameter,
    byte[] Data)
{
    public bool IsError => ErrorClass != 0 || ErrorCode != 0;
}

/// <summary>S7 报文头与 PDU 的装配 / 拆解。</summary>
public static class S7Frame
{
    public static byte[] BuildJob(ushort pduReference, ReadOnlySpan<byte> parameter, ReadOnlySpan<byte> data)
    {
        var pdu = new byte[S7Constants.JobHeaderLength + parameter.Length + data.Length];
        pdu[0] = S7Constants.ProtocolId;
        pdu[1] = S7Constants.RosctrJob;
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(2, 2), 0);
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(4, 2), pduReference);
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(6, 2), (ushort)parameter.Length);
        BinaryPrimitives.WriteUInt16BigEndian(pdu.AsSpan(8, 2), (ushort)data.Length);
        parameter.CopyTo(pdu.AsSpan(S7Constants.JobHeaderLength));
        data.CopyTo(pdu.AsSpan(S7Constants.JobHeaderLength + parameter.Length));
        return pdu;
    }

    /// <summary>
    /// 拆解 PLC 的回应。头长度按 ROSCTR 决定，不是常数——
    /// 按 10 字节读 Ack_Data 会把错误码当成参数的第一字节，现象是「参数永远对不上」。
    /// </summary>
    public static S7Pdu Parse(ReadOnlySpan<byte> pdu)
    {
        if (pdu.Length < S7Constants.JobHeaderLength)
            throw DriverException.ProtocolError("s7", "parse", $"S7 PDU shorter than 10 bytes ({pdu.Length})");

        if (pdu[0] != S7Constants.ProtocolId)
            throw DriverException.ProtocolError("s7", "parse",
                $"unexpected protocol id 0x{pdu[0]:x2} (expected 0x32)");

        var rosctr = pdu[1];
        var headerLength = rosctr is S7Constants.RosctrAck or S7Constants.RosctrAckData
            ? S7Constants.AckDataHeaderLength
            : S7Constants.JobHeaderLength;

        if (pdu.Length < headerLength)
            throw DriverException.ProtocolError("s7", "parse",
                $"S7 header truncated: need {headerLength}, have {pdu.Length}");

        var pduReference = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(4, 2));
        var paramLength = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(6, 2));
        var dataLength = BinaryPrimitives.ReadUInt16BigEndian(pdu.Slice(8, 2));

        var errorClass = headerLength == S7Constants.AckDataHeaderLength ? pdu[10] : (byte)0;
        var errorCode = headerLength == S7Constants.AckDataHeaderLength ? pdu[11] : (byte)0;

        var need = headerLength + paramLength + dataLength;
        if (pdu.Length < need)
            throw DriverException.ProtocolError("s7", "parse",
                $"S7 PDU truncated: need {need}, have {pdu.Length}");

        return new S7Pdu(
            rosctr,
            pduReference,
            errorClass,
            errorCode,
            pdu.Slice(headerLength, paramLength).ToArray(),
            pdu.Slice(headerLength + paramLength, dataLength).ToArray());
    }
}

/// <summary>PDU 协商（Setup Communication）。</summary>
public static class S7Setup
{
    public static byte[] BuildRequest(ushort pduReference, ushort requestedPdu = S7Constants.PduMedium)
    {
        var param = new byte[8];
        param[0] = S7Constants.FuncSetupCommunication;
        param[1] = 0x00;
        BinaryPrimitives.WriteUInt16BigEndian(param.AsSpan(2, 2), 0x0001); // max amq caller
        BinaryPrimitives.WriteUInt16BigEndian(param.AsSpan(4, 2), 0x0001); // max amq callee
        BinaryPrimitives.WriteUInt16BigEndian(param.AsSpan(6, 2), requestedPdu);
        return S7Frame.BuildJob(pduReference, param, ReadOnlySpan<byte>.Empty);
    }

    /// <summary>
    /// 读回 PLC 真正的 PDU 上限。以对端给的值为准而不是我们请求的值：
    /// 请求 960、PLC 只支持 240 时按 960 组帧，会被判成协议错。
    /// </summary>
    public static int ParseResponse(S7Pdu pdu)
    {
        if (pdu.IsError)
            throw DriverException.ProtocolError("s7", "setup",
                $"setup communication failed: class 0x{pdu.ErrorClass:x2} code 0x{pdu.ErrorCode:x2}");

        if (pdu.Parameter.Length < 8 || pdu.Parameter[0] != S7Constants.FuncSetupCommunication)
            throw DriverException.ProtocolError("s7", "setup",
                "setup communication response has no parameter block");

        var pduLength = BinaryPrimitives.ReadUInt16BigEndian(pdu.Parameter.AsSpan(6, 2));
        return pduLength is >= 240 and <= 960 ? pduLength : S7Constants.PduSmall;
    }
}

/// <summary>一次读请求里的一项：区域、DB 号、起始字节、字节数或位。</summary>
public readonly record struct S7ReadItem(S7Address Address, int ByteCount, bool IsBit)
{
    /// <summary>请求项 12 字节：0x12 / 0x0A / 0x10 / 类型 / 个数 / DB / 区域 / 位偏移。</summary>
    internal void WriteTo(Span<byte> dst)
    {
        dst[0] = S7Constants.SpecTypeVariable;
        dst[1] = S7Constants.AnyItemLength;
        dst[2] = S7Constants.SyntaxIdS7Any;
        dst[3] = IsBit ? S7Constants.VarBit : S7Constants.VarByte;

        // 传输尺寸是 BYTE 时「个数」就是字节数，BIT 时是位数。
        // 用 BYTE 而不是 WORD 是有意的：奇数字节（字符串、0.5 个字）也能一次读完。
        BinaryPrimitives.WriteUInt16BigEndian(dst.Slice(4, 2),
            (ushort)(IsBit ? 1 : Math.Max(1, ByteCount)));

        BinaryPrimitives.WriteUInt16BigEndian(dst.Slice(6, 2), (ushort)Address.Db);
        dst[8] = Address.Area;
        dst[9] = (byte)(Address.BitOffset >> 16);
        dst[10] = (byte)(Address.BitOffset >> 8);
        dst[11] = (byte)Address.BitOffset;
    }
}

/// <summary>读请求 / 读响应的编解码。</summary>
public static class S7Read
{
    public static byte[] BuildRequest(ushort pduReference, IReadOnlyList<S7ReadItem> items)
    {
        if (items.Count == 0)
            throw DriverException.ProtocolError("s7", "encode", "empty read request");

        // 项数上限由 PDU 长度决定，不在这里写死：驱动会按协商出的 PDU 切片。
        // 这里只拦住参数长度会溢出 16 位的病态输入。
        if (items.Count > 5000)
            throw DriverException.ProtocolError("s7", "encode",
                $"{items.Count} items in one request do not fit in a 16-bit parameter length");

        var param = new byte[2 + items.Count * 12];
        param[0] = S7Constants.FuncReadVar;
        param[1] = (byte)items.Count;

        for (var i = 0; i < items.Count; i++)
            items[i].WriteTo(param.AsSpan(2 + i * 12, 12));

        return S7Frame.BuildJob(pduReference, param, ReadOnlySpan<byte>.Empty);
    }

    /// <summary>
    /// 解出每一项的字节。返回 <c>ok=false</c> 时不抛异常——一项读不到不该让
    /// 同一次请求里的其它项一起失败，调用方按点位逐个标质量码。
    /// </summary>
    public static IReadOnlyList<(bool Ok, byte ReturnCode, byte[] Data)> ParseResponse(
        S7Pdu pdu, int expectedItems)
    {
        if (pdu.IsError)
            throw DriverException.ProtocolError("s7", "read",
                $"read failed: class 0x{pdu.ErrorClass:x2} code 0x{pdu.ErrorCode:x2}");

        if (pdu.Parameter.Length < 2 || pdu.Parameter[0] != S7Constants.FuncReadVar)
            throw DriverException.ProtocolError("s7", "read",
                "read response does not start with function 0x04");

        var count = pdu.Parameter[1];
        if (count != expectedItems)
            throw DriverException.ProtocolError("s7", "read",
                $"read response has {count} item(s), request had {expectedItems}");

        var results = new List<(bool, byte, byte[])>(count);
        var offset = 0;

        for (var i = 0; i < count; i++)
        {
            if (offset + 4 > pdu.Data.Length)
                throw DriverException.ProtocolError("s7", "read",
                    $"read response truncated at item {i} (data {pdu.Data.Length} bytes)");

            var returnCode = pdu.Data[offset];
            var transport = pdu.Data[offset + 1];
            var length = BinaryPrimitives.ReadUInt16BigEndian(pdu.Data.AsSpan(offset + 2, 2));

            // 长度字段的单位由传输尺寸决定：0x04/0x05 是位，0x06/0x07/0x09 是字节。
            // 拿位当字节会读出 8 倍长度的「数据」，而且后面每一项的偏移全错。
            var byteCount = transport switch
            {
                S7Constants.DataBit or S7Constants.DataByteWordDword or S7Constants.DataInteger
                    => (length + 7) / 8,
                _ => length,
            };

            var available = pdu.Data.Length - offset - 4;
            if (byteCount > available)
                throw DriverException.ProtocolError("s7", "read",
                    $"item {i} claims {byteCount} byte(s), only {available} left");

            var ok = returnCode == S7Constants.ReturnOk;
            results.Add((ok, returnCode, pdu.Data.AsSpan(offset + 4, byteCount).ToArray()));

            // 数据项按偶数字节对齐。
            offset += 4 + byteCount + (byteCount % 2);
        }

        return results;
    }
}

/// <summary>一次写请求里的一项。</summary>
public readonly record struct S7WriteItem(S7Address Address, bool IsBit, byte[] Data)
{
    public S7ReadItem AsReadItem() => new(Address, Data.Length, IsBit);
}

/// <summary>写请求 / 写响应的编解码。</summary>
public static class S7Write
{
    public static byte[] BuildRequest(ushort pduReference, IReadOnlyList<S7WriteItem> items)
    {
        if (items.Count == 0)
            throw DriverException.ProtocolError("s7", "encode", "empty write request");

        var param = new byte[2 + items.Count * 12];
        param[0] = S7Constants.FuncWriteVar;
        param[1] = (byte)items.Count;

        var dataSize = items.Sum(i => 4 + i.Data.Length + (i.Data.Length % 2));
        var data = new byte[dataSize];
        var offset = 0;

        for (var i = 0; i < items.Count; i++)
        {
            var item = items[i];
            item.AsReadItem().WriteTo(param.AsSpan(2 + i * 12, 12));

            // 写请求里的返回码字段固定填 0（这一位是给响应用的）。
            data[offset] = 0x00;
            data[offset + 1] = item.IsBit ? S7Constants.DataBit : S7Constants.DataByteWordDword;

            // 与读响应同一条规则：BIT / BYTE 的传输尺寸下长度是位。
            var length = item.IsBit ? 1 : item.Data.Length * 8;
            BinaryPrimitives.WriteUInt16BigEndian(data.AsSpan(offset + 2, 2), (ushort)length);

            item.Data.CopyTo(data.AsSpan(offset + 4));
            offset += 4 + item.Data.Length + (item.Data.Length % 2);
        }

        return S7Frame.BuildJob(pduReference, param, data);
    }

    /// <summary>写响应只有每项一个字节的返回码。</summary>
    public static IReadOnlyList<byte> ParseResponse(S7Pdu pdu, int expectedItems)
    {
        if (pdu.IsError)
            throw DriverException.ProtocolError("s7", "write",
                $"write failed: class 0x{pdu.ErrorClass:x2} code 0x{pdu.ErrorCode:x2}");

        if (pdu.Parameter.Length < 2 || pdu.Parameter[0] != S7Constants.FuncWriteVar)
            throw DriverException.ProtocolError("s7", "write",
                "write response does not start with function 0x05");

        var count = pdu.Parameter[1];
        if (count != expectedItems)
            throw DriverException.ProtocolError("s7", "write",
                $"write response has {count} item(s), request had {expectedItems}");

        if (pdu.Data.Length < count)
            throw DriverException.ProtocolError("s7", "write",
                $"write response carries {pdu.Data.Length} byte(s) for {count} item(s)");

        return pdu.Data.Take(count).ToArray();
    }

    /// <summary>返回码 → 质量码。地址类错误不参与熔断，这一条与其它驱动保持一致。</summary>
    public static Quality QualityFor(byte code) => code switch
    {
        S7Constants.ReturnOk => Quality.Good,
        S7Constants.ReturnAddressOutOfRange or S7Constants.ReturnObjectNotExist => Quality.AddressError,
        S7Constants.ReturnNotAllowed => Quality.Disabled,
        _ => Quality.Bad,
    };
}
