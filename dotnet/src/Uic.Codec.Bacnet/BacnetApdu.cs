using System.Buffers.Binary;
using Uic.Driver;

namespace Uic.Codec.Bacnet;

/// <summary>
/// 应用层服务（APDU）编解码：Who-Is、I-Am、ReadProperty、WriteProperty 及其响应。
///
/// 只实现采集主路径的最小闭环，且都限制在不分段、单帧：
/// - 发现：Who-Is（广播）→ I-Am；
/// - 读：ReadProperty（确认请求）→ ComplexAck（带值）或 Error；
/// - 写：WriteProperty（确认请求）→ SimpleAck 或 Error。
///
/// 帧格式按 ASHRAE 135 来，不做「自定义但两端一致」的简化：
/// 确认请求必须带 invoke-id（对面按它配对响应），服务参数必须带上下文标签
/// （对象标识符是 [0]、属性标识符是 [1]、属性值是 [2] 或 [3] 的构造类型）。
/// 省掉这些的项目在现场表现为「能发出去，但设备不认」——是最难查的一类问题。
///
/// 明确不支持：分段（segmented）、ReadPropertyMultiple、COV、写优先级。
/// </summary>
public static class BacnetApdu
{
    /// <summary>确认请求头：<c>[0]</c> PDU 类型、<c>[1]</c> 分段/最大长度、<c>[2]</c> invoke-id、<c>[3]</c> 服务选择码。</summary>
    private const int ConfirmedRequestHeaderLength = 4;

    /// <summary>上下文标签：对象标识符 [0]、属性标识符 [1]。</summary>
    private const int ContextObjectId = 0;
    private const int ContextPropertyId = 1;

    /// <summary>ReadProperty-ACK 的属性值 [2]、WriteProperty-Request 的属性值 [3]。</summary>
    private const int ContextReadValue = 2;
    private const int ContextWriteValue = 3;

    // ------------------------------------------------------------------
    // 请求（主站侧）
    // ------------------------------------------------------------------

    /// <summary>Who-Is：未确认请求，2 字节，无参数（不按设备实例区间过滤）。</summary>
    public static byte[] BuildWhoIs()
        => new[] { BacnetConstants.ApduUnconfirmedRequest, BacnetConstants.ServiceWhoIs };

    /// <summary>ReadProperty 确认请求：对象标识符 + 属性标识符。</summary>
    public static byte[] BuildReadProperty(BacnetObjectId id, int property, byte invokeId)
    {
        var propertyField = PropertyField(property);
        var apdu = new byte[ConfirmedRequestHeaderLength + 5 + propertyField.Length];

        WriteConfirmedHeader(apdu, BacnetConstants.ServiceReadProperty, invokeId);

        // [0] ObjectIdentifier：上下文标签 0，定长 4 字节。
        apdu[4] = BacnetConstants.ContextTagHeader(ContextObjectId, 4);
        BinaryPrimitives.WriteUInt32BigEndian(apdu.AsSpan(5, 4), id.ToWire());

        propertyField.CopyTo(apdu.AsSpan(9));
        return apdu;
    }

    /// <summary>
    /// WriteProperty 确认请求：对象标识符 + 属性标识符 + 属性值。
    ///
    /// 值由调用方用 <see cref="BacnetTags.EncodeValue"/> 编成应用标签后传入——
    /// 「标签怎么选」属类型层，「字节怎么排」属编码层，这条界线与 ValueCodec 一致。
    /// </summary>
    public static byte[] BuildWriteProperty(
        BacnetObjectId id, int property, ReadOnlySpan<byte> valueWithTag, byte invokeId)
    {
        var propertyField = PropertyField(property);
        var apdu = new byte[ConfirmedRequestHeaderLength + 5 + propertyField.Length + 1 + valueWithTag.Length + 1];

        WriteConfirmedHeader(apdu, BacnetConstants.ServiceWriteProperty, invokeId);

        apdu[4] = BacnetConstants.ContextTagHeader(ContextObjectId, 4);
        BinaryPrimitives.WriteUInt32BigEndian(apdu.AsSpan(5, 4), id.ToWire());

        var offset = 9;
        propertyField.CopyTo(apdu.AsSpan(offset));
        offset += propertyField.Length;

        // [3] 属性值是「打开标签 + 应用标签值 + 关闭标签」。
        apdu[offset++] = BacnetConstants.ContextTagOpening(ContextWriteValue);
        valueWithTag.CopyTo(apdu.AsSpan(offset));
        offset += valueWithTag.Length;
        apdu[offset] = BacnetConstants.ContextTagClosing(ContextWriteValue);

        return apdu;
    }

    private static void WriteConfirmedHeader(byte[] apdu, byte service, byte invokeId)
    {
        apdu[0] = BacnetConstants.ApduConfirmedRequest;
        apdu[1] = BacnetConstants.ConfirmedRequestMaxSegMaxApdu;
        apdu[2] = invokeId;
        apdu[3] = service;
    }

    /// <summary>属性标识符字段：上下文标签 1 + 极小编码的无符号整数（1..4 字节）。</summary>
    private static byte[] PropertyField(int property)
    {
        if (property is < 0 or > 0x3FFFFF)
            throw DriverException.Address("bacnet", "encode",
                $"property identifier {property} out of range (0..4194303)");

        var bytes = property switch
        {
            <= 0xFF => new[] { (byte)property },
            <= 0xFFFF => new[] { (byte)(property >> 8), (byte)property },
            _ => new[] { (byte)(property >> 24), (byte)(property >> 16), (byte)(property >> 8), (byte)property },
        };

        var field = new byte[1 + bytes.Length];
        field[0] = BacnetConstants.ContextTagHeader(ContextPropertyId, bytes.Length);
        bytes.CopyTo(field, 1);
        return field;
    }

    // ------------------------------------------------------------------
    // 响应（从站侧构造，测试与内置从站用）
    // ------------------------------------------------------------------

    /// <summary>I-Am：未确认请求，声明设备对象标识符。</summary>
    /// <param name="maxApduCode">
    /// max-APDU-length-accepted 的**枚举值**，不是字节数：0=50 … 4=1024 … 10=65535。
    /// 传字节数（1024）会写出 0x00 0x04，对面按枚举读成「50 字节」。
    /// </param>
    public static byte[] BuildIAm(int deviceInstance, int maxApduCode = 4, uint vendorId = 0)
    {
        if (deviceInstance is < 0 or > BacnetObjectId.MaxInstance)
            throw DriverException.Address("bacnet", "encode",
                $"device instance {deviceInstance} out of range (0..{BacnetObjectId.MaxInstance})");

        var deviceId = new BacnetObjectId(BacnetConstants.ObjectTypeDevice, deviceInstance);

        var maxApduBytes = Unsigned((ulong)CheckedMaxApduCode(maxApduCode));
        var vendorBytes = Unsigned(vendorId);

        var apdu = new byte[2 + 5 + (1 + maxApduBytes.Length) + (1 + vendorBytes.Length)];
        apdu[0] = BacnetConstants.ApduUnconfirmedRequest;
        apdu[1] = BacnetConstants.ServiceIAm;

        // [0] 设备对象标识符：上下文标签 0，4 字节。
        apdu[2] = BacnetConstants.ContextTagHeader(ContextObjectId, 4);
        BinaryPrimitives.WriteUInt32BigEndian(apdu.AsSpan(3, 4), deviceId.ToWire());

        // [1] Max APDU：应用标签 unsigned，载荷是枚举值。
        apdu[7] = (byte)((BacnetConstants.TagUnsigned << 4) | maxApduBytes.Length);
        maxApduBytes.CopyTo(apdu, 8);

        // [2] Vendor ID：应用标签 unsigned。
        var vendorOffset = 8 + maxApduBytes.Length;
        apdu[vendorOffset] = (byte)((BacnetConstants.TagUnsigned << 4) | vendorBytes.Length);
        vendorBytes.CopyTo(apdu, vendorOffset + 1);

        return apdu;
    }

    private static int CheckedMaxApduCode(int code)
    {
        // 0..10 是标准枚举（50/128/206/480/1024/1476/…/65535）。别的值对面无法解释。
        if (code is < 0 or > 10)
            throw DriverException.Address("bacnet", "encode",
                $"max-APDU enum code {code} out of range (0..10; 4 = 1024 bytes)");
        return code;
    }

    private static byte[] Unsigned(ulong value)
    {
        var bytes = new byte[8];
        BinaryPrimitives.WriteUInt64BigEndian(bytes, value);

        var start = 0;
        while (start < bytes.Length - 1 && bytes[start] == 0) start++;
        return bytes[start..];
    }

    /// <summary>ReadProperty-ACK：ComplexAck，带回对象标识符、属性标识符与属性值。</summary>
    public static byte[] BuildReadPropertyAck(
        byte invokeId, BacnetObjectId id, int property, ReadOnlySpan<byte> valueWithTag)
    {
        var propertyField = PropertyField(property);

        var apdu = new byte[3 + 5 + propertyField.Length + 1 + valueWithTag.Length + 1];
        apdu[0] = BacnetConstants.ApduComplexAck;
        apdu[1] = invokeId;
        apdu[2] = BacnetConstants.ServiceReadProperty;

        apdu[3] = BacnetConstants.ContextTagHeader(ContextObjectId, 4);
        BinaryPrimitives.WriteUInt32BigEndian(apdu.AsSpan(4, 4), id.ToWire());

        var offset = 8;
        propertyField.CopyTo(apdu.AsSpan(offset));
        offset += propertyField.Length;

        apdu[offset++] = BacnetConstants.ContextTagOpening(ContextReadValue);
        valueWithTag.CopyTo(apdu.AsSpan(offset));
        offset += valueWithTag.Length;
        apdu[offset] = BacnetConstants.ContextTagClosing(ContextReadValue);

        return apdu;
    }

    /// <summary>SimpleAck：确认请求成功的响应，只带服务选择码。</summary>
    public static byte[] BuildSimpleAck(byte invokeId, byte service)
        => new[] { BacnetConstants.ApduSimpleAck, invokeId, service };

    /// <summary>Error：确认请求失败的响应，带错误类与错误码（均为应用标签 enumerated）。</summary>
    public static byte[] BuildError(byte invokeId, byte service, int errorClass, int errorCode)
    {
        var apdu = new byte[7];
        apdu[0] = BacnetConstants.ApduError;
        apdu[1] = invokeId;
        apdu[2] = service;
        apdu[3] = (byte)((BacnetConstants.TagEnumerated << 4) | 1);
        apdu[4] = (byte)errorClass;
        apdu[5] = (byte)((BacnetConstants.TagEnumerated << 4) | 1);
        apdu[6] = (byte)errorCode;
        return apdu;
    }

    // ------------------------------------------------------------------
    // 响应解析
    // ------------------------------------------------------------------

    /// <summary>确认请求解析结果：成功的属性值（仍带应用标签）或错误类/码。</summary>
    public readonly record struct AckResult(
        bool Ok,
        byte[]? ValueWithTag,
        int ErrorClass,
        int ErrorCode,
        string Describe,
        ErrorKind Kind)
    {
        public static AckResult Success(byte[] valueWithTag) => new(true, valueWithTag, 0, 0, "ok", ErrorKind.Internal);

        public static AckResult Failure(int errorClass, int errorCode)
            => new(false, null, errorClass, errorCode, DescribeError(errorClass, errorCode),
                KindFor(errorClass, errorCode));

        public static AckResult Failure(string describe, ErrorKind kind = ErrorKind.Protocol)
            => new(false, null, -1, -1, describe, kind);

        /// <summary>
        /// 把「设备说这个对象/属性不存在」与「链路协议坏了」分开。
        ///
        /// 这个分野决定上层要不要熔断：unknown-object / unknown-property / 越权是
        /// 「点表写错」，重试一万次也一样，应落成 address_error 让人去改配置；
        /// 其余（资源不足、服务不支持）才更像链路或设备问题。
        /// </summary>
        private static ErrorKind KindFor(int errorClass, int errorCode) => errorClass switch
        {
            1 => ErrorKind.Address, // object: unknown-object
            2 => ErrorKind.Address, // property: unknown-property / invalid-data-type
            3 => ErrorKind.Address, // resources: 越界分配，属于配置口径
            4 => ErrorKind.Permission, // security: 越权
            _ => ErrorKind.Protocol,
        };
    }

    /// <summary>
    /// 解析 ReadProperty 的响应。
    ///
    /// <paramref name="expectedInvokeId"/> 用于核对响应确实属于本请求：同一条 UDP 流上
    /// 晚到的上一个请求的响应如果被当成这个请求的值，点位质量仍会是 good——
    /// 这是 BACnet 采集里最隐蔽的一类错值。
    /// </summary>
    public static AckResult ParseReadProperty(ReadOnlySpan<byte> apdu, byte expectedInvokeId)
    {
        if (apdu.IsEmpty)
            return AckResult.Failure("empty response");

        var pduType = (byte)(apdu[0] & 0xF0);

        if (pduType == BacnetConstants.ApduError)
            return ParseError(apdu, expectedInvokeId);

        if (pduType == BacnetConstants.ApduReject)
            return AckResult.Failure(
                $"device rejected the request (reject reason 0x{(apdu.Length > 2 ? apdu[2] : 0):x2}); " +
                "the object or property is not addressable by this build's minimal subset");

        if (pduType != BacnetConstants.ApduComplexAck)
            return AckResult.Failure(
                $"unexpected APDU type 0x{apdu[0]:x2} for a ReadProperty response");

        if (apdu.Length < 3)
            return AckResult.Failure("complex-ack shorter than its header");

        // 分段响应（首字节 bit3 = segmented）不重组：假装读到了会把第一段的值当成全值。
        if ((apdu[0] & 0x08) != 0)
            return AckResult.Failure(
                "device returned a segmented complex-ack; this build does not reassemble segments",
                ErrorKind.Unsupported);

        if (apdu[1] != expectedInvokeId)
            return AckResult.Failure(
                $"response carries invoke-id {apdu[1]} but the request used {expectedInvokeId}");

        if (apdu[2] != BacnetConstants.ServiceReadProperty)
            return AckResult.Failure(
                $"complex-ack is for service 0x{apdu[2]:x2}, not ReadProperty");

        var i = 3;

        // [0] 对象标识符：上下文标签 0，定长 4 字节。
        if (i >= apdu.Length || apdu[i] != BacnetConstants.ContextTagHeader(ContextObjectId, 4))
            return AckResult.Failure("complex-ack does not start with an object identifier");
        i += 5;

        // [1] 属性标识符：上下文标签 1，长度 1..4。
        if (i >= apdu.Length ||
            (apdu[i] & BacnetConstants.ContextTagClassBit) == 0 ||
            (apdu[i] >> 4) != ContextPropertyId)
            return AckResult.Failure("complex-ack has no property identifier");
        var propertyLength = apdu[i] & 0x07;
        if (propertyLength is 0 or > 4)
            return AckResult.Failure($"complex-ack property identifier has implausible length {propertyLength}");
        i += 1 + propertyLength;

        // [2] 属性值：打开标签 + 应用标签值 + 关闭标签。
        if (i >= apdu.Length || apdu[i] != BacnetConstants.ContextTagOpening(ContextReadValue))
            return AckResult.Failure("complex-ack has no open tag for the property value");
        i += 1;

        // 值是一段「应用标签 + 载荷」的不透明字节：不能逐字节扫描找关闭标签，
        // 因为载荷是任意字节，完全可能撞上一个合法的关闭标签（例如 real 1.25
        // = 44 3F A0 00 00，头一个载荷字节 0x3F 本身就是「上下文标签 3 的关闭」）。
        // 正确做法是先解出应用标签拿到确切长度，再往后跳过恰好这么多字节，
        // 关闭标签只出现在值之后——它只可能有一个。
        var valueStart = i;
        var (_, valueLen) = OpaqueTaggedLength(apdu, i, out var lengthError);
        if (lengthError is not null)
            return AckResult.Failure(lengthError);
        i += valueLen;

        if (i >= apdu.Length || !BacnetConstants.IsContextClosing(apdu[i]))
            return AckResult.Failure("complex-ack property value is not closed by a context tag");

        i += 1; // 消费关闭标签

        if (i != apdu.Length)
            return AckResult.Failure(
                $"complex-ack has {apdu.Length - i} unexpected trailing byte(s) after the value");

        if (valueLen == 0)
            return AckResult.Failure("complex-ack property value is empty");

        return AckResult.Success(apdu.Slice(valueStart, valueLen).ToArray());
    }

    /// <summary>
    /// 量出「应用标签 + 载荷」占用的总字节数，不解读值本身。
    ///
    /// ReadProperty 的 ACK 里属性值是一段不透明的应用标签字节：标签头给出长度，
    /// 据此跳过恰好这么多字节就是关闭标签的位置。绝不能在值里逐字节找关闭标签——
    /// 载荷可能撞上任意字节序列。
    /// </summary>
    private static (int Tag, int Length) OpaqueTaggedLength(
        ReadOnlySpan<byte> apdu, int offset, out string? error)
    {
        error = null;
        if (offset >= apdu.Length)
        {
            error = "complex-ack property value is empty";
            return (0, 0);
        }

        var header = apdu[offset];
        if ((header & BacnetConstants.ContextTagClassBit) != 0)
        {
            error = "complex-ack property value is a context tag, not an application value";
            return (0, 0);
        }

        var tag = header >> 4;
        var lvt = header & 0x07;
        var cursor = offset + 1;

        // boolean 与 null 的值就在标签头里：lvt 是 0/1（boolean）或 0（null），
        // 后面没有载荷字节。按「1 + lvt」算会把后面的关闭标签当成载荷吞掉。
        if (tag is BacnetConstants.TagNull or BacnetConstants.TagBoolean)
            return (tag, 1);

        if (lvt < 5)
            return (tag, 1 + lvt); // 头 1 字节 + 短载荷

        // LVT=5：后面跟扩展长度（1/254/255 字节，见 BacnetTags）。
        if (cursor >= apdu.Length)
        {
            error = "complex-ack value declares an extended length but it is missing";
            return (0, 0);
        }

        var first = apdu[cursor++];
        int length;
        int consumed;

        if (first <= 253)
        {
            length = first;
            consumed = 0;
        }
        else if (first == 254)
        {
            if (cursor + 2 > apdu.Length)
            {
                error = "complex-ack value declares a 2-byte extended length but it is truncated";
                return (0, 0);
            }
            length = System.Buffers.Binary.BinaryPrimitives.ReadUInt16BigEndian(apdu.Slice(cursor, 2));
            consumed = 2;
        }
        else
        {
            if (cursor + 4 > apdu.Length)
            {
                error = "complex-ack value declares a 4-byte extended length but it is truncated";
                return (0, 0);
            }
            length = (int)System.Buffers.Binary.BinaryPrimitives.ReadUInt32BigEndian(apdu.Slice(cursor, 4));
            consumed = 4;
        }

        cursor += consumed;

        if (cursor + length > apdu.Length)
        {
            error = $"complex-ack value declares {length} payload byte(s) but only {apdu.Length - cursor} remain";
            return (0, 0);
        }

        return (tag, cursor - offset + length);
    }

    private static AckResult ParseError(ReadOnlySpan<byte> apdu, byte expectedInvokeId)
    {
        if (apdu.Length < 7)
            return AckResult.Failure("error PDU is shorter than 7 bytes");

        if (apdu[1] != expectedInvokeId)
            return AckResult.Failure(
                $"error carries invoke-id {apdu[1]} but the request used {expectedInvokeId}");

        // [3]=error-class 标签、[4]=值、[5]=error-code 标签、[6]=值。
        var errorClass = apdu[4];
        var errorCode = apdu[6];
        return AckResult.Failure(errorClass, errorCode);
    }

    /// <summary>解析 SimpleAck：核对 invoke-id 与服务选择码。</summary>
    public static AckResult ParseSimpleAck(ReadOnlySpan<byte> apdu, byte expectedInvokeId, byte expectedService)
    {
        if (apdu.IsEmpty || (apdu[0] & 0xF0) == BacnetConstants.ApduError)
            return ParseReadProperty(apdu, expectedInvokeId);

        if ((apdu[0] & 0xF0) == BacnetConstants.ApduReject)
            return AckResult.Failure(
                $"device rejected the request (reject reason 0x{(apdu.Length > 2 ? apdu[2] : 0):x2})");

        if ((apdu[0] & 0xF0) != BacnetConstants.ApduSimpleAck)
            return AckResult.Failure($"unexpected APDU type 0x{apdu[0]:x2} for a SimpleAck");

        if (apdu.Length < 2 || apdu[1] != expectedInvokeId)
            return AckResult.Failure(
                $"ack carries invoke-id {(apdu.Length > 1 ? apdu[1] : 0)} but the request used {expectedInvokeId}");

        if (apdu.Length >= 3 && apdu[2] != expectedService)
            return AckResult.Failure(
                $"ack is for service 0x{apdu[2]:x2}, expected 0x{expectedService:x2}");

        return AckResult.Success(Array.Empty<byte>());
    }

    /// <summary>判断一条 APDU 是否为 I-Am。</summary>
    public static bool IsIAm(ReadOnlySpan<byte> apdu)
        => apdu.Length >= 2
           && (apdu[0] & 0xF0) == BacnetConstants.ApduUnconfirmedRequest
           && apdu[1] == BacnetConstants.ServiceIAm;

    /// <summary>从 I-Am 里取出设备实例号。只解析第一个参数（设备对象标识符）。</summary>
    public static int ParseIAmDeviceInstance(ReadOnlySpan<byte> apdu)
    {
        if (!IsIAm(apdu))
            throw DriverException.ProtocolError("bacnet", "decode", "response is not an I-Am");

        if (apdu.Length < 3 || apdu[2] != BacnetConstants.ContextTagHeader(ContextObjectId, 4))
            throw DriverException.ProtocolError("bacnet", "decode",
                "I-Am does not start with a context-tagged device object identifier");

        if (apdu.Length < 7)
            throw DriverException.ProtocolError("bacnet", "decode", "I-Am device object identifier is truncated");

        return BacnetObjectId.FromWire(BinaryPrimitives.ReadUInt32BigEndian(apdu.Slice(3, 4))).Instance;
    }

    // ------------------------------------------------------------------
    // 错误描述
    // ------------------------------------------------------------------

    /// <summary>
    /// 把错误类/码翻成运维看得懂的句子。未收录的给数字，不假装认识。
    /// 只列采集现场最常见的几个：对象/属性不存在、越权、类型不符。
    /// </summary>
    public static string DescribeError(int errorClass, int errorCode)
    {
        var classText = errorClass switch
        {
            0 => "device",
            1 => "object",
            2 => "property",
            3 => "resources",
            4 => "security",
            5 => "services",
            _ => $"class#{errorClass}",
        };

        var codeText = errorCode switch
        {
            0 => "other",
            9 => "invalid-data-type",
            27 => "read-access-denied",
            31 => "unknown-object",
            32 => "unknown-property",
            40 => "write-access-denied",
            41 => "invalid-array-index",
            42 => "property-is-not-an-array",
            _ => $"code#{errorCode}",
        };

        return $"bacnet error: {classText}/{codeText} (class {errorClass}, code {errorCode})";
    }
}
