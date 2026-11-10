using Opc.Ua;
using Uic.Domain;
using DomainType = Uic.Domain.DataType;
using DomainValue = Uic.Domain.DataValue;
using UaValue = Opc.Ua.DataValue;

namespace Uic.Driver.OpcUa;

/// <summary>
/// OPC UA 的值与质量映射。
///
/// 这一层刻意做成纯函数：协议栈的 <see cref="Variant"/> 与平台的 <see cref="DomainValue"/>
/// 之间只差一次翻译，而翻译错一个类型在现场的表现为「值一直是 0」或者
/// 「写下去的东西 PLC 不认」，都不报错。把这些规则放进可单测的函数里，
/// 比在会话回调里靠日志猜要可靠。
/// </summary>
public static class OpcUaValues
{
    public const string Protocol = "opcua";

    /// <summary>
    /// StatusCode → 平台质量码。
    ///
    /// 分类的口径是「运维看到这个质量码该做什么」：<see cref="Quality.AddressError"/>
    /// 表示点表写错了（改配置），<see cref="Quality.Disabled"/> 表示权限不够（要授权），
    /// <see cref="Quality.Timeout"/> / <see cref="Quality.CommError"/> 表示链路问题
    /// （会重试、会熔断）。
    /// </summary>
    public static Quality FromStatus(uint code)
    {
        if (StatusCode.IsGood(code)) return Quality.Good;
        if (StatusCode.IsUncertain(code)) return Quality.Uncertain;

        return code switch
        {
            StatusCodes.BadNodeIdUnknown or
            StatusCodes.BadNodeIdInvalid or
            StatusCodes.BadNodeIdRejected or
            StatusCodes.BadAttributeIdInvalid or
            StatusCodes.BadIndexRangeInvalid or
            StatusCodes.BadIndexRangeNoData or
            StatusCodes.BadTypeMismatch or
            StatusCodes.BadDataEncodingInvalid or
            StatusCodes.BadDataEncodingUnsupported or
            StatusCodes.BadNoMatch or
            StatusCodes.BadNothingToDo => Quality.AddressError,

            StatusCodes.BadUserAccessDenied or
            StatusCodes.BadSecurityChecksFailed or
            StatusCodes.BadSecurityModeInsufficient or
            StatusCodes.BadSecurityPolicyRejected or
            StatusCodes.BadIdentityTokenInvalid or
            StatusCodes.BadIdentityTokenRejected or
            StatusCodes.BadCertificateInvalid or
            StatusCodes.BadCertificateUntrusted or
            StatusCodes.BadCertificateTimeInvalid or
            StatusCodes.BadCertificateRevoked or
            StatusCodes.BadCertificateUseNotAllowed or
            StatusCodes.BadNotWritable or
            StatusCodes.BadWriteNotSupported => Quality.Disabled,

            StatusCodes.BadTimeout or
            StatusCodes.BadRequestTimeout => Quality.Timeout,

            StatusCodes.BadNotConnected or
            StatusCodes.BadConnectionClosed or
            StatusCodes.BadSessionClosed or
            StatusCodes.BadSessionIdInvalid or
            StatusCodes.BadSecureChannelClosed or
            StatusCodes.BadSecureChannelIdInvalid or
            StatusCodes.BadServerNotConnected or
            StatusCodes.BadShutdown or
            StatusCodes.BadServerHalted or
            StatusCodes.BadCommunicationError or
            StatusCodes.BadNoCommunication => Quality.CommError,

            // "还没有数据"不等于"读失败"：订阅刚建立、服务端还没采样时就是它。
            StatusCodes.BadWaitingForInitialData or
            StatusCodes.BadDataUnavailable or
            StatusCodes.BadNoData => Quality.Stale,

            _ => Quality.Bad,
        };
    }

    /// <summary>StatusCode → 错误分类。写结果与整段失败的上报都用它。</summary>
    public static ErrorKind KindFromStatus(uint code) => FromStatus(code) switch
    {
        Quality.AddressError => ErrorKind.Address,
        Quality.Disabled => ErrorKind.Permission,
        Quality.Timeout => ErrorKind.Timeout,
        Quality.CommError => ErrorKind.Connection,
        Quality.Uncertain or Quality.Stale => ErrorKind.Protocol,
        Quality.Good => ErrorKind.Internal,
        _ => ErrorKind.Protocol,
    };

    /// <summary>StatusCodes 的可读名字，写进样本的错误说明。</summary>
    public static string DescribeStatus(uint code)
        => StatusCodes.GetBrowseName(code);

    // ------------------------------------------------------------------
    // 读：Variant → 领域值
    // ------------------------------------------------------------------

    /// <summary>
    /// 把服务端返回的 <see cref="Variant"/> 转成点位声明的值。
    ///
    /// 刻意不用"服务端给什么类型就存什么类型"：点位声明的 dataType 决定了
    /// 缩放、死区和北向契约，读回一个类型不同的值再悄悄存下去，
    /// 上游看到的类型会随服务端实现漂移。
    /// </summary>
    public static DomainValue Decode(Variant variant, PointRef point)
    {
        var value = variant.Value
            ?? throw new FormatException("the server returned a null value");

        if (point.IsArray)
        {
            // ByteString 在 CLR 里也是 byte[]，但它是一个标量。把它当数组拆开，
            // 「读一个二进制块」会变成「读 N 个数字」，而且数值恰好对得上时还不报错。
            if (value is byte[])
            {
                throw new FormatException(
                    $"the point declares {point.Elements} elements but the server returned a ByteString");
            }

            if (value is not Array array)
            {
                throw new FormatException(
                    $"the point declares {point.Elements} elements but the server returned a scalar " +
                    $"({TypeName(value)})");
            }

            if (array.Length != point.Elements)
            {
                throw new FormatException(
                    $"the server returned {array.Length} element(s), the point declares {point.Elements}");
            }

            if (!SupportsArray(point.DataType))
                throw new FormatException($"{point.DataType} cannot be read as an array from OPC UA");

            var numbers = new double[array.Length];
            for (var i = 0; i < array.Length; i++)
                numbers[i] = ToNumber(array.GetValue(i), point.DataType, i);

            return DomainValue.Numbers(point.DataType, numbers);
        }

        // ByteString 的 CLR 表示就是 byte[]：它不是数组值，走下面的 Bytes 分支。
        if (value is Array elements and not byte[])
        {
            throw new FormatException(
                $"the point is a scalar but the server returned {elements.Length} element(s)");
        }

        switch (point.DataType)
        {
            case DomainType.Bool:
                return value is bool flag
                    ? DomainValue.Boolean(flag)
                    : throw new FormatException($"expected a Boolean, got {TypeName(value)}");

            case DomainType.String:
                return DomainValue.String(ToText(value));

            case DomainType.Bytes:
                return value is byte[] bytes
                    ? DomainValue.Bytes(bytes)
                    : throw new FormatException($"expected a ByteString, got {TypeName(value)}");

            case DomainType.DateTime:
                return DomainValue.DateTime(ToTimestamp(value));

            case DomainType.Bcd16:
            case DomainType.Bcd32:
                throw new FormatException(
                    $"{point.DataType} has no OPC UA representation; use an integer type instead");

            default:
                return DomainValue.Number(point.DataType, Checked(ToNumber(value, point.DataType, 0), point.DataType));
        }
    }

    /// <summary>数值类型是否支持数组读取。字符串/时间/字节串数组没有对应的领域形状。</summary>
    public static bool SupportsArray(DomainType type) => type is not
        (DomainType.String or DomainType.Bytes or DomainType.DateTime or DomainType.Bcd16 or DomainType.Bcd32);

    private static double ToNumber(object? value, DomainType type, int index)
    {
        var raw = value switch
        {
            sbyte v => v,
            byte v => v,
            short v => v,
            ushort v => v,
            int v => v,
            uint v => v,
            long v => v,
            ulong v => (double)v,
            float v => v,
            double v => v,
            _ => double.NaN,
        };

        if (double.IsNaN(raw))
        {
            var where = type is DomainType.Float32 or DomainType.Float64 ? string.Empty : $", expected a number for {type}";
            throw new FormatException(
                $"element {index} is {TypeName(value)}{where}");
        }

        return raw;
    }

    /// <summary>整型必须真的装得下：装不下时截断，读出来的值与现场对不上，比报错难查得多。</summary>
    private static double Checked(double value, DomainType type)
    {
        var (min, max, integral) = Range(type);

        if (!integral) return value;

        if (double.IsNaN(value) || double.IsInfinity(value) || value < min || value > max)
            throw new FormatException($"value {value:R} does not fit {type} ({min}..{max})");

        return value;
    }

    private static (double Min, double Max, bool Integral) Range(DomainType type) => type switch
    {
        DomainType.Int16 => (short.MinValue, short.MaxValue, true),
        DomainType.UInt16 => (ushort.MinValue, ushort.MaxValue, true),
        DomainType.Int32 => (int.MinValue, int.MaxValue, true),
        DomainType.UInt32 => (uint.MinValue, uint.MaxValue, true),
        DomainType.Int64 => (-9.2233720368547758E18, 9.2233720368547758E18, true),
        DomainType.UInt64 => (0, 1.8446744073709552E19, true),
        _ => (double.MinValue, double.MaxValue, false),
    };

    private static string ToText(object value) => value switch
    {
        string s => s,
        LocalizedText text => text.Text ?? string.Empty,
        _ => Convert.ToString(value, System.Globalization.CultureInfo.InvariantCulture) ?? string.Empty,
    };

    private static DateTimeOffset ToTimestamp(object value) => value switch
    {
        DateTime dt => FromOpcUa(dt),
        DateTimeOffset dto => dto.ToUniversalTime(),
        _ => throw new FormatException($"expected a DateTime, got {TypeName(value)}"),
    };

    /// <summary>
    /// OPC UA 的 DateTime 一律是 UTC；协议栈可能把它包成 Local 或 Unspecified，
    /// 用 ToUniversalTime 会把 Unspecified 当本机时区再偏一次。
    /// </summary>
    public static DateTimeOffset FromOpcUa(DateTime value)
        => new(new DateTime(
            value.Ticks, value.Kind == DateTimeKind.Local ? DateTimeKind.Local : DateTimeKind.Utc)
            .ToUniversalTime());

    public static DateTime ToOpcUa(DateTimeOffset value)
        => DateTime.SpecifyKind(value.UtcDateTime, DateTimeKind.Utc);

    private static string TypeName(object? value) => value?.GetType().Name ?? "null";

    // ------------------------------------------------------------------
    // 写：领域值 → Variant
    // ------------------------------------------------------------------

    /// <summary>
    /// 把要写入的值转成服务端能接受的 <see cref="Variant"/>。
    ///
    /// 数组必须造出**具体元素类型**的 CLR 数组：<c>new Variant(new double[]{...})</c>
    /// 写进 Float32 变量会得到 BadTypeMismatch，而 <c>float[]</c> 才对。
    /// </summary>
    public static Variant Encode(DomainValue value, PointRef point)
    {
        if (point.DataType is DomainType.Bcd16 or DomainType.Bcd32)
        {
            throw DriverException.Unsupported(Protocol, "encode",
                $"{point.DataType} has no OPC UA representation; use an integer type instead");
        }

        if (point.IsArray)
        {
            if (value.Array is not { } elements)
            {
                throw DriverException.Address(Protocol, "encode",
                    $"'{point.Id}' declares {point.Elements} elements, got a scalar");
            }

            if (elements.Count != point.Elements)
            {
                throw DriverException.Address(Protocol, "encode",
                    $"'{point.Id}' declares {point.Elements} elements, got {elements.Count}");
            }

            if (!SupportsArray(point.DataType))
            {
                throw DriverException.Unsupported(Protocol, "encode",
                    $"{point.DataType} cannot be written as an array over OPC UA");
            }

            var elementType = ClrType(point.DataType);
            var array = Array.CreateInstance(elementType, elements.Count);

            for (var i = 0; i < elements.Count; i++)
            {
                try
                {
                    array.SetValue(Scalar(DomainValue.Number(point.DataType, elements[i]), point.DataType), i);
                }
                catch (Exception ex) when (ex is FormatException or OverflowException or ArgumentException)
                {
                    throw DriverException.Address(Protocol, "encode",
                        $"'{point.Id}' element {i}: {ex.Message}");
                }
            }

            return new Variant(array);
        }

        if (value.Array is not null)
        {
            throw DriverException.Address(Protocol, "encode",
                $"'{point.Id}' is a scalar point, got {value.Array.Count} element(s)");
        }

        try
        {
            return new Variant(Scalar(value, point.DataType));
        }
        catch (FormatException ex)
        {
            throw DriverException.Address(Protocol, "encode", $"'{point.Id}': {ex.Message}");
        }
        catch (OverflowException)
        {
            throw DriverException.Address(Protocol, "encode",
                $"'{point.Id}': value {value.AsNumber():R} does not fit {point.DataType}");
        }
    }

    private static object Scalar(DomainValue value, DomainType type)
    {
        var number = value.AsNumber();

        return type switch
        {
            DomainType.Bool => value.AsBool(),
            DomainType.String => value.Text ?? string.Empty,
            DomainType.Bytes => Convert.FromBase64String(value.Text ?? string.Empty),
            DomainType.DateTime => value.TryGetDateTime(out var stamp)
                ? ToOpcUa(stamp)
                /* 写不出去的时间宁可报错也不写 0：1970-01-01 在趋势图上像一个合法读数。 */
                : throw new FormatException($"'{value.Text}' is not a valid datetime value"),
            DomainType.Int16 => (short)Integral(number, type),
            DomainType.UInt16 => (ushort)Integral(number, type),
            DomainType.Int32 => (int)Integral(number, type),
            DomainType.UInt32 => (uint)Integral(number, type),
            DomainType.Int64 => (long)Integral(number, type),
            DomainType.UInt64 => (ulong)Integral(number, type),
            DomainType.Float32 => (float)number,
            DomainType.Float64 => number,
            _ => throw new FormatException($"type {type} is not writable over OPC UA"),
        };
    }

    /// <summary>取整并做范围检查。<c>(short)70000</c> 会静默回绕成 4464，那是最坏的一种"成功"。</summary>
    private static long Integral(double value, DomainType type)
    {
        var (min, max, _) = Range(type);

        if (double.IsNaN(value) || double.IsInfinity(value))
            throw new FormatException($"value {value:R} does not fit {type}");

        var rounded = Math.Round(value, MidpointRounding.AwayFromZero);
        if (rounded < min || rounded > max)
            throw new FormatException($"value {value:R} does not fit {type} ({min}..{max})");

        return (long)rounded;
    }

    private static Type ClrType(DomainType type) => type switch
    {
        DomainType.Bool => typeof(bool),
        DomainType.Int16 => typeof(short),
        DomainType.UInt16 => typeof(ushort),
        DomainType.Int32 => typeof(int),
        DomainType.UInt32 => typeof(uint),
        DomainType.Int64 => typeof(long),
        DomainType.UInt64 => typeof(ulong),
        DomainType.Float32 => typeof(float),
        DomainType.Float64 => typeof(double),
        _ => throw new FormatException($"type {type} has no array representation"),
    };

    /// <summary>把驱动的 <see cref="UaValue"/> 转成样本；调用方已按状态码筛掉坏值。</summary>
    public static RawSample ToSample(string pointId, UaValue value, PointRef point)
    {
        var code = (uint)value.StatusCode;
        var quality = FromStatus(code);
        var stamp = TimestampOf(value);

        if (StatusCode.IsBad(code))
        {
            return new RawSample(pointId, null, quality, stamp, DescribeStatus(code));
        }

        try
        {
            return new RawSample(pointId, Decode(value.WrappedValue, point), quality, stamp);
        }
        catch (FormatException ex)
        {
            // 类型对不上是点表问题：报 address_error，别让它算进熔断。
            return new RawSample(pointId, null, Quality.AddressError, stamp, ex.Message);
        }
    }

    /// <summary>设备时标：优先 sourceTimestamp（数据在设备侧产生的时刻），否则用服务端时标。</summary>
    private static DateTimeOffset? TimestampOf(UaValue value)
    {
        if (value.SourceTimestamp != DateTime.MinValue) return FromOpcUa(value.SourceTimestamp);
        if (value.ServerTimestamp != DateTime.MinValue) return FromOpcUa(value.ServerTimestamp);
        return null;
    }
}
