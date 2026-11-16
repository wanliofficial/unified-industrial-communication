using System.Buffers.Binary;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Codec.Bacnet;

/// <summary>
/// BACnet/IP (ASHRAE 135 Annex J) 线上常量。
///
/// 数值来自 ASHRAE 135 标准与公开的报文解析资料，不用「看起来合理」的猜测——
/// 这些编号错了，现象是「连上了但读回的对象不存在」或「属性解析成乱码」。
/// 本轮只实现采集主路径用得上的最小子集：Who-Is / I-Am（发现）、
/// ReadProperty（读单个属性）、WriteProperty（写单个属性）。COV、分段、
/// 读写多个属性的 ReadPropertyMultiple 都不做，遇到明确报错。
/// </summary>
public static class BacnetConstants
{
    // ---- BVLC（BACnet Virtual Link Control，Annex J）----
    /// <summary>BVLC 类型。BACnet/IP 永远是 0x81。</summary>
    public const byte BvllTypeBacnetIp = 0x81;

    /// <summary>原始单播 NPDU。主站向某台设备发请求用它。</summary>
    public const byte BvllFunctionOriginalUnicast = 0x0B;

    /// <summary>原始广播 NPDU。Who-Is 用它在本网段广播。</summary>
    public const byte BvllFunctionOriginalBroadcast = 0x0A;

    public const int BvllHeaderLength = 4;

    // ---- NPDU ----
    /// <summary>
    /// NPDU 版本字节。BACnet 135 规定固定为 1；写 0 或省略会让对端把报文当坏帧丢弃，
    /// 现象是「发出去了但没人理」。
    /// </summary>
    public const byte NpduVersion = 0x01;

    /// <summary>
    /// NPDU 控制字节 = 0：无目的/源路由信息、无网络层消息。
    /// 主站与直连设备之间一跳到底，用不到路由；非 0 会引出一段没法验证的字段解析。
    /// </summary>
    public const byte NpduControlSimple = 0x00;

    /// <summary>NPDU 头：版本 + 控制字节（无路由、无网络层消息时没有后续字段）。</summary>
    public const int NpduHeaderLength = 2;

    // ---- APDU（应用层）----
    /// <summary>确认请求 PDU 类型（bit7=0，bit6..4=000，PDU 类型=0 表示 confirmed-request）。</summary>
    public const byte ApduConfirmedRequest = 0x00;

    /// <summary>未确认请求 PDU 类型。I-Am 用它（PDU 类型=1，bit6..4 另含分段位，此处取简单未确认）。</summary>
    public const byte ApduUnconfirmedRequest = 0x10;

    /// <summary>简单确认响应 PDU 类型（PDU 类型=2，bit6..4=000）。ReadProperty 成功回它。</summary>
    public const byte ApduSimpleAck = 0x20;

    /// <summary>复杂 ACK 响应 PDU 类型（PDU 类型=3）。带回属性值，ReadProperty 成功带值时用它。</summary>
    public const byte ApduComplexAck = 0x30;

    /// <summary>错误响应 PDU 类型（PDU 类型=5）。对象/属性不存在、越权等回它。</summary>
    public const byte ApduError = 0x50;

    /// <summary>拒绝响应 PDU 类型（PDU 类型=6）。设备根本不认这条请求时回它。</summary>
    public const byte ApduReject = 0x60;

    // ---- APDU 分段与最大长度位 ----
    /// <summary>
    /// 确认请求第 2 字节：高 3 位 = max-segments-accepted（0 = 不分段），
    /// 低 4 位 = max-APDU-size-accepted（4 = 1024 字节）。本轮所有请求都声明不分段 + 1024。
    /// 写错这字节的后果很隐蔽：对面会按它决定分段，而本实现不重组分段，响应会被截断。
    /// </summary>
    public const byte ConfirmedRequestMaxSegMaxApdu = 0x04;

    // ---- 服务选择码（confirmed / unconfirmed service choice）----
    public const byte ServiceWhoIs = 0x08;
    public const byte ServiceIAm = 0x10;
    public const byte ServiceReadProperty = 0x0C;
    public const byte ServiceWriteProperty = 0x0F;

    // ---- 应用标签号（BACnet Application Tags，ASHRAE 135 §20.2）----
    public const byte TagNull = 0;
    public const byte TagBoolean = 1;
    public const byte TagUnsigned = 2;
    public const byte TagSigned = 3;
    public const byte TagReal = 4;
    public const byte TagDouble = 5;
    public const byte TagOctetString = 6;
    public const byte TagCharacterString = 7;
    public const byte TagBitString = 8;
    public const byte TagEnumerated = 9;
    public const byte TagObjectIdentifier = 12;

    /// <summary>上下文标签的「打开」标志（class 位 = 1、LVT = 6）。</summary>
    public const byte ContextTagClassBit = 0x08;

    /// <summary>上下文标签的打开头：<c>(tag &lt;&lt; 4) | 0x0E</c>。</summary>
    public static byte ContextTagOpening(int tag) => (byte)((tag << 4) | 0x0E);

    /// <summary>
    /// 上下文标签的关闭头：<c>(tag &lt;&lt; 4) | 0x0F</c>。
    ///
    /// 关闭标签**带标签号**：值标签是 [2] 时关闭标签是 0x2F，不是 0x0F。
    /// 写成固定的 0x0F 会被对端读成「关闭标签 0」——它的构造还没打开，
    /// 于是整条 APDU 变成语法错误。本端解析端如果也照 0x0F 匹配，两个错误正好互相
    /// 抵消，回环测试全绿、真实设备全红。
    /// </summary>
    public static byte ContextTagClosing(int tag) => (byte)((tag << 4) | 0x0F);

    /// <summary>判断一个字节是否为上下文关闭标签（class 位 + LVT=7）。</summary>
    public static bool IsContextClosing(byte b) => (b & 0x0F) == 0x0F;

    /// <summary>上下文标签的短格式头（长度 0..14）：<c>(tag &lt;&lt; 4) | 0x08 | length</c>。</summary>
    public static byte ContextTagHeader(int tag, int length)
        => (byte)((tag << 4) | 0x08 | length);

    // ---- 对象类型（只列采集会用到的；完整表在 ASHRAE 135 §12）----
    public const int ObjectTypeAnalogInput = 0;
    public const int ObjectTypeAnalogOutput = 1;
    public const int ObjectTypeAnalogValue = 2;
    public const int ObjectTypeBinaryInput = 3;
    public const int ObjectTypeBinaryOutput = 4;
    public const int ObjectTypeBinaryValue = 5;
    public const int ObjectTypeDevice = 8;
    public const int ObjectTypeMultiStateInput = 13;
    public const int ObjectTypeMultiStateOutput = 14;
    public const int ObjectTypeMultiStateValue = 19;

    // ---- 属性标识符（只列采集会用到的）----
    public const int PropertyPresentValue = 85;
    public const int PropertyObjectName = 77;
    public const int PropertyObjectIdentifier = 75;
    public const int PropertyObjectType = 79;
    public const int PropertyStatusFlags = 111;
    public const int PropertyEventState = 36;

    /// <summary>
    /// 把属性/对象类型的数字翻成人话。日志与错误消息都靠它。
    /// 只覆盖本轮用到的；未列出的给「#数字」，不假装认识。
    /// </summary>
    public static string DescribeObject(int type) => type switch
    {
        ObjectTypeAnalogInput => "analog-input",
        ObjectTypeAnalogOutput => "analog-output",
        ObjectTypeAnalogValue => "analog-value",
        ObjectTypeBinaryInput => "binary-input",
        ObjectTypeBinaryOutput => "binary-output",
        ObjectTypeBinaryValue => "binary-value",
        ObjectTypeDevice => "device",
        ObjectTypeMultiStateInput => "multi-state-input",
        ObjectTypeMultiStateOutput => "multi-state-output",
        ObjectTypeMultiStateValue => "multi-state-value",
        _ => $"object-type#{type}",
    };

    public static string DescribeProperty(int property) => property switch
    {
        PropertyPresentValue => "present-value",
        PropertyObjectName => "object-name",
        PropertyObjectIdentifier => "object-identifier",
        PropertyObjectType => "object-type",
        PropertyStatusFlags => "status-flags",
        PropertyEventState => "event-state",
        _ => $"property#{property}",
    };
}

/// <summary>
/// 平台 Address.Object 到 BACnet 对象标识符的映射。
///
/// <c>objectType</c> 直接是 BACnet 对象类型号（0=analog-input 等），<c>instance</c> 是
/// 实例号（0..4194303，22 位线上字段）。这与 S7 把 <c>offset</c> 解释成字节偏移同理：
/// 平台模型里 object 的三个字段天生就是 BACnet 的「对象类型 + 实例 + 属性」，
/// 不需要额外发明。映射在文档与 ConfigFields 里写明白。
/// </summary>
public readonly record struct BacnetObjectId(int Type, int Instance)
{
    public const int MaxInstance = 0x3FFFFF; // 22 位

    /// <summary>
    /// 从平台 Address 构造对象标识符。objectType 必须是整数（配置里写数字，
    /// 与 BACnet 线上一致；别名映射留给驱动的配置解析层，不在这里做）。
    /// </summary>
    public static BacnetObjectId From(Address address)
    {
        if (address.Kind != AddressKind.Object)
            throw DriverException.Unsupported("bacnet", "encode",
                $"address kind '{address.Kind}' is not supported by BACnet; use object addressing " +
                "(objectType as an integer, instance, property)");

        if (address.ObjectType is null)
            throw DriverException.Address("bacnet", "encode", "objectType is required (integer object type)");

        if (!int.TryParse(address.ObjectType, out var typeNumber))
            throw DriverException.Address("bacnet", "encode",
                $"objectType '{address.ObjectType}' is not an integer; BACnet object types are integers " +
                $"(0=analog-input, 1=analog-output, ...)");
        if (typeNumber is < 0 or > 1023)
            throw DriverException.Address("bacnet", "encode",
                $"objectType {typeNumber} out of range (0..1023, 10-bit wire field)");

        var instance = address.Instance;
        if (instance is < 0 or > MaxInstance)
            throw DriverException.Address("bacnet", "encode",
                $"instance {instance} out of range (0..{MaxInstance}, 22-bit wire field)");

        return new BacnetObjectId(typeNumber, instance);
    }

    /// <summary>属性标识符来自 address.property。缺省 present-value：采集点位默认读当前值。</summary>
    public static int PropertyOf(Address address)
    {
        if (string.IsNullOrWhiteSpace(address.Property)) return BacnetConstants.PropertyPresentValue;
        if (!int.TryParse(address.Property, out var property))
            throw DriverException.Address("bacnet", "encode",
                $"property '{address.Property}' is not an integer; BACnet property identifiers are integers " +
                $"(85=present-value, 77=object-name, ...)");
        if (property is < 0 or > 0x3FFFFF)
            throw DriverException.Address("bacnet", "encode", $"property {property} out of range");
        return property;
    }

    /// <summary>对象标识符在线上是 4 字节：类型（10 位）<< 22 | 实例（22 位）。</summary>
    public uint ToWire() => ((uint)Type << 22) | (uint)Instance;

    public static BacnetObjectId FromWire(uint wire) => new((int)(wire >> 22), (int)(wire & MaxInstance));

    public string Describe() => $"{BacnetConstants.DescribeObject(Type)}:{Instance}";
}
