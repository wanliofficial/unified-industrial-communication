using System.Buffers.Binary;
using Uic.Driver;

namespace Uic.Codec.Modbus;

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