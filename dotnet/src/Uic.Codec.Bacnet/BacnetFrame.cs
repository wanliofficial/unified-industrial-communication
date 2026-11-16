using System.Buffers.Binary;
using Uic.Driver;

namespace Uic.Codec.Bacnet;

/// <summary>
/// BACnet/IP 报文封装：BVLC + NPDU + APDU。
///
/// 分三层与 S7 的 TPKT/COTP/S7 刻意同构：
/// - BVLC（4 字节）：Annex J 的虚拟链路头，类型 + 功能 + 长度；
/// - NPDU（≥2 字节）：网络层头，直连一跳用控制字节 0 即可；
/// - APDU：应用层服务请求/响应。
///
/// 驱动只面对「一条完整 APDU + 目标是否广播」，封帧与拆帧都收在这里。
/// </summary>
public static class BacnetFrame
{
    /// <summary>把 APDU 封成一条完整的 BACnet/IP 报文（单播或广播）。</summary>
    public static byte[] Build(ReadOnlySpan<byte> apdu, bool broadcast)
    {
        var npduLength = BacnetConstants.NpduHeaderLength;
        var total = BacnetConstants.BvllHeaderLength + npduLength + apdu.Length;
        if (total > ushort.MaxValue)
            throw DriverException.ProtocolError("bacnet", "encode",
                $"APDU too large to frame: {apdu.Length} bytes");

        var frame = new byte[total];
        frame[0] = BacnetConstants.BvllTypeBacnetIp;
        frame[1] = broadcast
            ? BacnetConstants.BvllFunctionOriginalBroadcast
            : BacnetConstants.BvllFunctionOriginalUnicast;
        BinaryPrimitives.WriteUInt16BigEndian(frame.AsSpan(2, 2), (ushort)total);
        frame[BacnetConstants.BvllHeaderLength] = BacnetConstants.NpduVersion;
        frame[BacnetConstants.BvllHeaderLength + 1] = BacnetConstants.NpduControlSimple;
        apdu.CopyTo(frame.AsSpan(BacnetConstants.BvllHeaderLength + npduLength));
        return frame;
    }

    /// <summary>从一条完整报文里取出 APDU。校验 BVLC 类型与长度自洽。</summary>
    public static ReadOnlySpan<byte> Payload(ReadOnlySpan<byte> frame)
    {
        if (frame.Length < BacnetConstants.BvllHeaderLength)
            throw DriverException.ProtocolError("bacnet", "bvlc", "frame shorter than the 4-byte BVLC header");

        if (frame[0] != BacnetConstants.BvllTypeBacnetIp)
            throw DriverException.ProtocolError("bacnet", "bvlc",
                $"unsupported BVLC type 0x{frame[0]:x2} (expected 0x{BacnetConstants.BvllTypeBacnetIp:x2} for BACnet/IP)");

        var declared = BinaryPrimitives.ReadUInt16BigEndian(frame.Slice(2, 2));
        if (declared != frame.Length)
            throw DriverException.ProtocolError("bacnet", "bvlc",
                $"BVLC length {declared} does not match the frame size {frame.Length}");
        if (declared < BacnetConstants.BvllHeaderLength + BacnetConstants.NpduHeaderLength)
            throw DriverException.ProtocolError("bacnet", "bvlc", $"implausible BVLC length {declared}");

        // NPDU 版本必须是 1。允许别的值会让「解析出乱码但长度自洽」的坏帧通过，
        // 之后的属性值全部错位。
        var npduVersion = frame[BacnetConstants.BvllHeaderLength];
        if (npduVersion != BacnetConstants.NpduVersion)
            throw DriverException.ProtocolError("bacnet", "npdu",
                $"unsupported NPDU version {npduVersion} (expected {BacnetConstants.NpduVersion})");

        // 控制字节非 0 表示带路由/网络层消息：本实现只处理一跳直连，遇到明确报错。
        var control = frame[BacnetConstants.BvllHeaderLength + 1];
        if (control != BacnetConstants.NpduControlSimple)
            throw DriverException.Unsupported("bacnet", "npdu",
                $"NPDU control byte 0x{control:x2} carries routing or network-layer messages, " +
                "which this build does not parse");

        return frame.Slice(BacnetConstants.BvllHeaderLength + BacnetConstants.NpduHeaderLength);
    }

    /// <summary>BVLC 功能字节，用来区分这是一条单播还是广播报文（Who-Is 走广播）。</summary>
    public static byte BvllFunction(ReadOnlySpan<byte> frame) => frame.Length > 1 ? frame[1] : (byte)0;
}
