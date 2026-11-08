using System.Text.Json;
using System.Text.Json.Serialization;

namespace Uic.Domain;

/// <summary>
/// 一次读写的值。数值统一放 <see cref="Num"/>（超过 2^53 的整数用 <see cref="Text"/>），
/// 布尔与字符串各占一个字段，不做隐式混用。
///
/// 数组点位的值放在 <see cref="Array"/> 里，<see cref="Type"/> 仍然是**元素类型**：
/// 这样「元素是什么类型」与「有几个元素」是两个正交维度，不需要为一个浮点数组
/// 新造一个 <c>float32array</c> 类型名，读取方也不必维护「类型名 × 维度」的组合表。
/// </summary>
[JsonConverter(typeof(DataValueConverter))]
public sealed record DataValue
{
    public DataType Type { get; init; }
    public double? Num { get; init; }
    public bool? Bool { get; init; }
    public string? Text { get; init; }

    /// <summary>数组元素；为 null 表示标量。只有固定宽度的数值元素支持数组。</summary>
    public IReadOnlyList<double>? Array { get; init; }

    public static DataValue Number(DataType type, double value) => new() { Type = type, Num = value };
    public static DataValue Boolean(bool value) => new() { Type = DataType.Bool, Bool = value };
    public static DataValue String(string value) => new() { Type = DataType.String, Text = value };
    public static DataValue Bytes(ReadOnlySpan<byte> value) =>
        new() { Type = DataType.Bytes, Text = Convert.ToBase64String(value) };

    /// <summary>数组值。元素类型与点位声明的 <c>dataType</c> 一致。</summary>
    public static DataValue Numbers(DataType elementType, IReadOnlyList<double> values) =>
        new() { Type = elementType, Array = values };

    public bool IsArray => Array is not null;

    /// <summary>元素个数：标量为 1，数组为元素数。</summary>
    public int Count => Array?.Count ?? 1;

    /// <summary>
    /// 时间值统一存成 ISO-8601 UTC 文本（毫秒精度）。用文本而不是 double：
    /// 1970 年至今的毫秒数约 1.8e12，仍在 2^53 以内，但把「UTC 时间」表达成数字
    /// 会让北向消费者必须自己猜时区与单位；文本形态在 REST/MQTT/日志里都能直接读。
    /// </summary>
    public static DataValue DateTime(DateTimeOffset value) => new()
    {
        Type = DataType.DateTime,
        Text = value.ToUniversalTime().ToString(
            "yyyy-MM-ddTHH:mm:ss.fffZ", System.Globalization.CultureInfo.InvariantCulture),
    };

    /// <summary>解析时间值。非法文本返回 false，由调用方决定是报错还是跳过。</summary>
    public bool TryGetDateTime(out DateTimeOffset value)
    {
        value = default;
        if (Type != DataType.DateTime || Text is null) return false;

        return System.DateTimeOffset.TryParse(
            Text,
            System.Globalization.CultureInfo.InvariantCulture,
            System.Globalization.DateTimeStyles.AssumeUniversal |
            System.Globalization.DateTimeStyles.AdjustToUniversal,
            out value);
    }

    /// <summary>
    /// 是否可参与数值运算（缩放、死区）。数组也算：它的元素是数值，
    /// 只是「有几个」不同——若这里判 false，数组会被当成非数值走整体相等比较，
    /// 死区就彻底失效了。
    /// </summary>
    public bool IsNumeric => Array is not null || Num.HasValue;
    public double AsNumber() => Num ?? (Bool == true ? 1d : 0d);
    public bool AsBool() => Bool ?? (Num.HasValue && Math.Abs(Num.Value) > double.Epsilon);

    /// <summary>
    /// 第 <paramref name="index"/> 个元素。标量只有下标 0。
    ///
    /// 刻意不提供 <c>AsNumbers()</c> 返回集合：采集热路径上每个样本都做一次
    /// 「标量 → 单元素列表」的分配毫无意义，调用方按「Count + NumberAt」遍历即可。
    /// </summary>
    public double NumberAt(int index) => Array is null ? AsNumber() : Array[index];

    /// <summary>
    /// 写后回读的比较口径。浮点按相对误差而不是精确相等：写 1500.0 读回 1499.9999
    /// 不应判为失败；类型不同则一律不等，避免把 uint16 的 1 当成 bool true。
    /// 数组逐元素比较，元素个数不同直接不等——少写一个寄存器不该算「写成功」。
    /// </summary>
    public bool Matches(DataValue other, double tolerance = 1e-6)
    {
        if (Type != other.Type) return false;

        if (Array is not null || other.Array is not null)
        {
            if (Array is null || other.Array is null) return false;
            if (Array.Count != other.Array.Count) return false;

            for (var i = 0; i < Array.Count; i++)
            {
                if (!SameNumber(Array[i], other.Array[i], tolerance)) return false;
            }

            return true;
        }

        if (Bool.HasValue || other.Bool.HasValue) return Bool == other.Bool;

        if (Num.HasValue && other.Num.HasValue)
            return SameNumber(Num.Value, other.Num.Value, tolerance);

        return string.Equals(Text, other.Text, StringComparison.Ordinal);
    }

    private static bool SameNumber(double a, double b, double tolerance)
    {
        // NaN 只能等于自身，否则「坏值」会被死区当成「没变化」永远不上报。
        if (double.IsNaN(a) || double.IsNaN(b)) return double.IsNaN(a) && double.IsNaN(b);

        var scale = Math.Max(1, Math.Max(Math.Abs(a), Math.Abs(b)));
        return Math.Abs(a - b) <= tolerance * scale;
    }

    public override string ToString() => Array is null
        ? Type switch
        {
            DataType.Bool => Bool?.ToString() ?? "null",
            DataType.String or DataType.Bytes or DataType.DateTime => Text ?? "null",
            _ => Num?.ToString("R", System.Globalization.CultureInfo.InvariantCulture) ?? "null",
        }
        : "[" + string.Join(",", Array.Select(v =>
            v.ToString("R", System.Globalization.CultureInfo.InvariantCulture))) + "]";
}

/// <summary>
/// 把值序列化成 <c>{"type":"float32","num":42.5}</c> 的稳定形态。
/// 契约稳定性优先：字段名与类型名不随版本变化。
/// </summary>
public sealed class DataValueConverter : JsonConverter<DataValue>
{
    public override DataValue Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
    {
        using var doc = JsonDocument.ParseValue(ref reader);
        var root = doc.RootElement;

        var type = DataType.UInt16;
        if (root.TryGetProperty("type", out var t) && t.ValueKind == JsonValueKind.String)
        {
            type = JsonEnum.ParseDataType(t.GetString()!);
        }

        if (root.TryGetProperty("array", out var arr) && arr.ValueKind == JsonValueKind.Array)
        {
            var values = new List<double>(arr.GetArrayLength());
            foreach (var item in arr.EnumerateArray())
            {
                if (item.ValueKind != JsonValueKind.Number)
                {
                    // 数组里混进 null 或字符串说明生产方有 bug；宁可整体不解析，
                    // 也不要悄悄少一个元素——元素个数是数组语义的一部分。
                    throw new JsonException("array element must be a number");
                }

                values.Add(item.GetDouble());
            }

            return DataValue.Numbers(type, values);
        }

        if (root.TryGetProperty("bool", out var b) && b.ValueKind is JsonValueKind.True or JsonValueKind.False)
        {
            return DataValue.Boolean(b.GetBoolean());
        }

        if (root.TryGetProperty("text", out var s) && s.ValueKind == JsonValueKind.String)
        {
            return new DataValue { Type = type, Text = s.GetString() };
        }

        if (root.TryGetProperty("num", out var n))
        {
            return n.ValueKind switch
            {
                JsonValueKind.Number => DataValue.Number(type, n.GetDouble()),
                JsonValueKind.String when double.TryParse(
                    n.GetString(),
                    System.Globalization.NumberStyles.Float,
                    System.Globalization.CultureInfo.InvariantCulture,
                    out var d) => DataValue.Number(type, d),
                _ => new DataValue { Type = type },
            };
        }

        return new DataValue { Type = type };
    }

    public override void Write(Utf8JsonWriter writer, DataValue value, JsonSerializerOptions options)
    {
        writer.WriteStartObject();
        writer.WriteString("type", JsonEnum.ToWire(value.Type));

        // 数组只写 array，不写 num：两个字段同时出现会让消费方要决定「以谁为准」。
        if (value.Array is { } array)
        {
            writer.WriteStartArray("array");
            foreach (var item in array) writer.WriteNumberValue(item);
            writer.WriteEndArray();
        }
        else
        {
            if (value.Num.HasValue) writer.WriteNumber("num", value.Num.Value);
            if (value.Bool.HasValue) writer.WriteBoolean("bool", value.Bool.Value);
            if (value.Text is not null) writer.WriteString("text", value.Text);
        }

        writer.WriteEndObject();
    }
}

/// <summary>枚举与线上表示（camelCase / snake_case 字符串）之间的稳定映射。</summary>
/// <summary>
/// 配置里的 <c>dataType</c> 与北向值里的 <c>type</c> 必须用同一个名字。
///
/// 默认的 camelCase 枚举写法会产出 <c>"uInt16"</c>，而值与文档约定的是 <c>"uint16"</c>；
/// 两者并存意味着脚本要记住两套拼写，迟早出错。这里统一到后者。
/// </summary>
public sealed class DataTypeConverter : JsonConverter<DataType>
{
    public override DataType Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
        => reader.TokenType == JsonTokenType.String
            ? JsonEnum.ParseDataType(reader.GetString() ?? string.Empty)
            : (DataType)reader.GetInt32();

    public override void Write(Utf8JsonWriter writer, DataType value, JsonSerializerOptions options)
        => writer.WriteStringValue(JsonEnum.ToWire(value));
}

public static class JsonEnum
{
    public static string ToWire(DataType t) => t switch
    {
        DataType.Bool => "bool",
        DataType.Int16 => "int16",
        DataType.UInt16 => "uint16",
        DataType.Int32 => "int32",
        DataType.UInt32 => "uint32",
        DataType.Int64 => "int64",
        DataType.UInt64 => "uint64",
        DataType.Float32 => "float32",
        DataType.Float64 => "float64",
        DataType.String => "string",
        DataType.Bcd16 => "bcd16",
        DataType.Bcd32 => "bcd32",
        DataType.Bytes => "bytes",
        DataType.DateTime => "datetime",
        _ => "uint16",
    };

    // 大小写不敏感：旧版本配置写的是 "uInt16"，不能因为一次改名就拒绝启动。
    public static DataType ParseDataType(string s) => s.Trim().ToLowerInvariant() switch
    {
        "bool" => DataType.Bool,
        "int16" => DataType.Int16,
        "uint16" => DataType.UInt16,
        "int32" => DataType.Int32,
        "uint32" => DataType.UInt32,
        "int64" => DataType.Int64,
        "uint64" => DataType.UInt64,
        "float32" => DataType.Float32,
        "float64" => DataType.Float64,
        "string" => DataType.String,
        "bcd16" => DataType.Bcd16,
        "bcd32" => DataType.Bcd32,
        "bytes" => DataType.Bytes,
        "datetime" => DataType.DateTime,
        _ => DataType.UInt16,
    };

    public static string ToWire(Quality q) => q switch
    {
        Quality.Good => "good",
        Quality.Uncertain => "uncertain",
        Quality.Stale => "stale",
        Quality.Bad => "bad",
        Quality.Timeout => "timeout",
        Quality.CommError => "comm_error",
        Quality.AddressError => "address_error",
        Quality.NotConnected => "not_connected",
        Quality.Disabled => "disabled",
        _ => "bad",
    };

    public static Quality ParseQuality(string s) => s switch
    {
        "good" => Quality.Good,
        "uncertain" => Quality.Uncertain,
        "stale" => Quality.Stale,
        "timeout" => Quality.Timeout,
        "comm_error" => Quality.CommError,
        "address_error" => Quality.AddressError,
        "not_connected" => Quality.NotConnected,
        "disabled" => Quality.Disabled,
        _ => Quality.Bad,
    };

    public static string ToWire(RuntimeState s) => s.ToString().ToLowerInvariant();

    public static string ToWire(ConfiguredState s) => s.ToString().ToLowerInvariant();

    public static string ToWire(DeadbandMode mode) => mode switch
    {
        DeadbandMode.Percent => "percent",
        DeadbandMode.Rate => "rate",
        _ => "absolute",
    };

    // 与 DataType 一样大小写不敏感：CSV 导入与手写配置都不该被大小写卡住。
    public static DeadbandMode ParseDeadbandMode(string s)
        => TryParseDeadbandMode(s, out var mode) ? mode : DeadbandMode.Absolute;

    /// <summary>严格解析：CSV 导入要靠返回值拒绝拼错的模式，而不是静默退回绝对值。</summary>
    public static bool TryParseDeadbandMode(string s, out DeadbandMode mode)
    {
        switch (s.Trim().ToLowerInvariant())
        {
            case "":
            case "absolute":
                mode = DeadbandMode.Absolute;
                return true;
            case "percent":
                mode = DeadbandMode.Percent;
                return true;
            case "rate":
                mode = DeadbandMode.Rate;
                return true;
            default:
                mode = DeadbandMode.Absolute;
                return false;
        }
    }
}
