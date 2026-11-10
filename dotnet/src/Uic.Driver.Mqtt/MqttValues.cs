using System.Globalization;
using System.Text.Json;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Driver.Mqtt;

/// <summary>
/// 南向 MQTT 载荷取值：从一条 JSON 报文里按字段名 / JSON 指针取出值，转成领域 <see cref="DataValue"/>。
///
/// 为什么取值规则要单独一层：MQTT 报文是「一个主题 + 一段字节」，字节是什么形状没有规范约束，
/// 可能是裸数字（<c>21.5</c>）、可能是 <c>{"value":21.5}</c>、也可能是另一个网关的
/// 北向 Sample JSON（<c>{"value":{"type":"float64","num":21.5}}</c>）。把这段「取哪个字段、
/// 怎么转」的翻译集中在这里，点表里看到的就只是「主题 + 类型」，而不是一份 JSON 解析代码。
///
/// 明确不支持：BCD（那是寄存器世界的编码）、报文里的 <c>quality</c> 字段（南向只按
/// 新鲜度判质量，上游质量是否可信属于级联网关的策略问题，这一版不猜）。
/// </summary>
public static class MqttValues
{
    /// <summary>取值结果。值与质量分离：坏值也带原因，界面与日志能直接定位。</summary>
    public readonly record struct DecodeResult(DataValue? Value, Quality Quality, string? Error)
    {
        public static DecodeResult Ok(DataValue value) => new(value, Quality.Good, null);
        public static DecodeResult Bad(string error) => new(null, Quality.Bad, error);
        public static DecodeResult Address(string error) => new(null, Quality.AddressError, error);
    }

    /// <summary>
    /// 解析一条报文。失败原因分两类，对应不同的处置：
    /// 报文不是 JSON 是 <see cref="Quality.Bad"/>（发布方或 Broker 的问题，重连/重试有意义）；
    /// 字段缺失或类型对不上是 <see cref="Quality.AddressError"/>（点表与发布方契约的问题，
    /// 重试一百次也是同样的结果，不该把设备拖进熔断）。
    /// </summary>
    public static DecodeResult Decode(
        ReadOnlyMemory<byte> payload,
        string fieldPath,
        DataType dataType,
        int count)
    {
        JsonDocument document;
        try
        {
            document = JsonDocument.Parse(payload);
        }
        catch (JsonException ex)
        {
            return DecodeResult.Bad($"payload is not valid JSON: {ex.Message}");
        }

        using (document)
        {
            var node = Select(document.RootElement, fieldPath);
            if (node is null)
                return DecodeResult.Address($"field '{FieldDescription(fieldPath)}' is missing from the payload");

            return ConvertNode(node.Value, dataType, count, fieldPath);
        }
    }

    /// <summary>
    /// 取字段。留空表示整个载荷就是值（发布方只发裸数字/裸字符串时）。
    /// 以 <c>/</c> 开头按 JSON 指针走（<c>/value</c>、<c>/data/temp</c>），
    /// 否则按点号分层（<c>value</c>、<c>data.temp</c>）。
    /// </summary>
    private static JsonElement? Select(JsonElement root, string fieldPath)
    {
        if (string.IsNullOrEmpty(fieldPath)) return root;

        var segments = fieldPath.StartsWith('/')
            ? fieldPath[1..].Split('/')
            : fieldPath.Split('.');

        var current = root;
        foreach (var raw in segments)
        {
            // JSON 指针用 ~1 / ~0 转义 / 与 ~，先还原再查属性名。
            var segment = raw.Replace("~1", "/").Replace("~0", "~");

            if (current.ValueKind != JsonValueKind.Object) return null;
            if (!current.TryGetProperty(segment, out var next)) return null;

            current = next;
        }

        return current;
    }

    private static DecodeResult ConvertNode(JsonElement node, DataType type, int count, string field)
    {
        // 北向 Sample JSON 的值是 {"type":..,"num":..} 形态：解包出裸标量再按点位声明的类型转，
        // 这样上游网关的值形状下游不必懂协议。类型以本地点表为准，不一致会在转换处暴露。
        if (IsEnvelope(node)) node = EnvelopeValue(node);

        try
        {
            if (count > 1) return DecodeResult.Ok(ConvertArray(node, type, count));

            var value = type switch
            {
                DataType.Bool => ConvertBool(node),
                DataType.Int16 or DataType.UInt16 or DataType.Int32 or DataType.UInt32
                    or DataType.Int64 or DataType.UInt64 => ConvertInteger(node, type),
                DataType.Float32 or DataType.Float64 => ConvertFloat(node, type),
                DataType.String => ConvertString(node),
                DataType.Bytes => ConvertBytes(node),
                DataType.DateTime => ConvertDateTime(node),
                _ => throw new FormatException($"{type} cannot be read from a JSON payload"),
            };

            return DecodeResult.Ok(value);
        }
        catch (FormatException ex)
        {
            return DecodeResult.Address($"field '{FieldDescription(field)}': {ex.Message}");
        }
    }

    private static DataValue ConvertArray(JsonElement node, DataType type, int count)
    {
        if (node.ValueKind != JsonValueKind.Array)
            throw new FormatException($"expected a JSON array of {count} numbers");

        var actual = node.GetArrayLength();
        if (actual != count)
            throw new FormatException($"got {actual} element(s), the point declares {count}");

        if (!type.SupportsArray())
            throw new FormatException($"{type} cannot be an array; set count to 1");

        var values = new List<double>(count);
        foreach (var item in node.EnumerateArray())
        {
            RawNumber(item, out var element);
            values.Add(element);
        }

        return DataValue.Numbers(type, values);
    }

    private static DataValue ConvertBool(JsonElement node) => node.ValueKind switch
    {
        JsonValueKind.True => DataValue.Boolean(true),
        JsonValueKind.False => DataValue.Boolean(false),
        // 不少发布方用 0/1 表达布尔；收下但不接受其它数字，避免把 2 静默当成 true。
        JsonValueKind.Number when node.TryGetInt64(out var i) && (i == 0 || i == 1)
            => DataValue.Boolean(i != 0),
        JsonValueKind.String when bool.TryParse(node.GetString(), out var parsed) => DataValue.Boolean(parsed),
        _ => throw new FormatException($"expected a boolean, got {Kind(node)}"),
    };

    private static DataValue ConvertInteger(JsonElement node, DataType type)
    {
        var raw = RawNumber(node, out var value);

        if (double.IsInfinity(value) || value != Math.Floor(value))
            throw new FormatException($"expected an integer, got {raw}");

        return DataValue.Number(type, value);
    }

    private static DataValue ConvertFloat(JsonElement node, DataType type)
    {
        RawNumber(node, out var value);
        return DataValue.Number(type, value);
    }

    private static DataValue ConvertString(JsonElement node) => node.ValueKind switch
    {
        JsonValueKind.String => DataValue.String(node.GetString() ?? string.Empty),
        // 数字/布尔也收成字符串：发布方把数值当文本发很常见，按点表声明的类型落库。
        JsonValueKind.Number or JsonValueKind.True or JsonValueKind.False
            => DataValue.String(node.GetRawText()),
        _ => throw new FormatException($"expected a string, got {Kind(node)}"),
    };

    private static DataValue ConvertBytes(JsonElement node)
    {
        if (node.ValueKind != JsonValueKind.String)
            throw new FormatException($"expected a base64 string, got {Kind(node)}");

        try
        {
            return DataValue.Bytes(Convert.FromBase64String(node.GetString() ?? string.Empty));
        }
        catch (FormatException)
        {
            throw new FormatException("is not valid base64");
        }
    }

    private static DataValue ConvertDateTime(JsonElement node)
    {
        if (node.ValueKind != JsonValueKind.String)
            throw new FormatException($"expected an ISO-8601 string, got {Kind(node)}");

        var text = node.GetString() ?? string.Empty;
        if (!DateTimeOffset.TryParse(
                text,
                CultureInfo.InvariantCulture,
                DateTimeStyles.AssumeUniversal | DateTimeStyles.AdjustToUniversal,
                out var value))
        {
            throw new FormatException($"'{text}' is not a valid ISO-8601 timestamp");
        }

        return DataValue.DateTime(value);
    }

    /// <summary>北向值信封：<c>{"type":..,"num":..|bool|text|array}</c>。有 type 且带值字段才算。</summary>
    private static bool IsEnvelope(JsonElement node)
        => node.ValueKind == JsonValueKind.Object
           && node.TryGetProperty("type", out var type) && type.ValueKind == JsonValueKind.String
           && (node.TryGetProperty("num", out _)
               || node.TryGetProperty("bool", out _)
               || node.TryGetProperty("text", out _)
               || node.TryGetProperty("array", out _));

    private static JsonElement EnvelopeValue(JsonElement node)
    {
        if (node.TryGetProperty("array", out var array) && array.ValueKind == JsonValueKind.Array)
            return array;
        if (node.TryGetProperty("num", out var num)) return num;
        if (node.TryGetProperty("bool", out var boolean)) return boolean;
        if (node.TryGetProperty("text", out var text)) return text;

        return default;
    }

    /// <summary>取数字。JSON 数字直接取；字符串形态的数字也收（发布方当文本发很常见）。</summary>
    private static string RawNumber(JsonElement node, out double value)
    {
        switch (node.ValueKind)
        {
            case JsonValueKind.Number:
                value = node.GetDouble();
                return node.GetRawText();

            case JsonValueKind.String:
                var text = node.GetString();
                if (double.TryParse(text, NumberStyles.Float, CultureInfo.InvariantCulture, out value))
                    return text ?? string.Empty;
                throw new FormatException($"'{text}' is not a number");

            default:
                value = 0;
                throw new FormatException($"expected a number, got {Kind(node)}");
        }
    }

    private static string Kind(JsonElement node)
        => node.ValueKind switch
        {
            JsonValueKind.Object => "an object",
            JsonValueKind.Array => "an array",
            JsonValueKind.String => "a string",
            JsonValueKind.True or JsonValueKind.False => "a boolean",
            JsonValueKind.Null => "null",
            _ => "a non-number",
        };

    private static string FieldDescription(string fieldPath)
        => string.IsNullOrEmpty(fieldPath) ? "(whole payload)" : fieldPath;
}
