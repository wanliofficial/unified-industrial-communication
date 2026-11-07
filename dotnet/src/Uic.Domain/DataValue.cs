using System.Text.Json;
using System.Text.Json.Serialization;

namespace Uic.Domain;

/// <summary>
/// 一次读写的值。数值统一放 <see cref="Num"/>（超过 2^53 的整数用 <see cref="Text"/>），
/// 布尔与字符串各占一个字段，不做隐式混用。
/// </summary>
[JsonConverter(typeof(DataValueConverter))]
public sealed record DataValue
{
    public DataType Type { get; init; }
    public double? Num { get; init; }
    public bool? Bool { get; init; }
    public string? Text { get; init; }

    public static DataValue Number(DataType type, double value) => new() { Type = type, Num = value };
    public static DataValue Boolean(bool value) => new() { Type = DataType.Bool, Bool = value };
    public static DataValue String(string value) => new() { Type = DataType.String, Text = value };
    public static DataValue Bytes(ReadOnlySpan<byte> value) =>
        new() { Type = DataType.Bytes, Text = Convert.ToBase64String(value) };

    public bool IsNumeric => Num.HasValue;
    public double AsNumber() => Num ?? (Bool == true ? 1d : 0d);
    public bool AsBool() => Bool ?? (Num.HasValue && Math.Abs(Num.Value) > double.Epsilon);

    /// <summary>
    /// 写后回读的比较口径。浮点按相对误差而不是精确相等：写 1500.0 读回 1499.9999
    /// 不应判为失败；类型不同则一律不等，避免把 uint16 的 1 当成 bool true。
    /// </summary>
    public bool Matches(DataValue other, double tolerance = 1e-6)
    {
        if (Type != other.Type) return false;
        if (Bool.HasValue || other.Bool.HasValue) return Bool == other.Bool;

        if (Num.HasValue && other.Num.HasValue)
        {
            var scale = Math.Max(1, Math.Max(Math.Abs(Num.Value), Math.Abs(other.Num.Value)));
            return Math.Abs(Num.Value - other.Num.Value) <= tolerance * scale;
        }

        return string.Equals(Text, other.Text, StringComparison.Ordinal);
    }


    public override string ToString() => Type switch
    {
        DataType.Bool => Bool?.ToString() ?? "null",
        DataType.String or DataType.Bytes => Text ?? "null",
        _ => Num?.ToString("R", System.Globalization.CultureInfo.InvariantCulture) ?? "null",
    };
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
        if (value.Num.HasValue) writer.WriteNumber("num", value.Num.Value);
        if (value.Bool.HasValue) writer.WriteBoolean("bool", value.Bool.Value);
        if (value.Text is not null) writer.WriteString("text", value.Text);
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
