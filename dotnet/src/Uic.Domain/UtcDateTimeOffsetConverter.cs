using System.Globalization;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace Uic.Domain;

/// <summary>
/// 统一把时间写成 UTC 毫秒精度的 RFC 3339（<c>...Z</c>）。
///
/// 默认序列化会输出 <c>+00:00</c>，消费方（尤其是 SCADA 侧脚本）经常按字面量
/// 解析，混着两种时区表示迟早出错；契约里只允许一种写法。
/// </summary>
public sealed class UtcDateTimeOffsetConverter : JsonConverter<DateTimeOffset>
{
    public const string Format = "yyyy-MM-dd'T'HH:mm:ss.fff'Z'";

    public override DateTimeOffset Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
    {
        if (reader.TokenType == JsonTokenType.String)
        {
            var text = reader.GetString();

            if (!string.IsNullOrEmpty(text) &&
                DateTimeOffset.TryParse(text, CultureInfo.InvariantCulture,
                    DateTimeStyles.AdjustToUniversal | DateTimeStyles.AssumeUniversal, out var parsed))
            {
                return parsed;
            }
        }

        return reader.GetDateTimeOffset();
    }

    public override void Write(Utf8JsonWriter writer, DateTimeOffset value, JsonSerializerOptions options)
        => writer.WriteStringValue(value.ToUniversalTime().ToString(Format, CultureInfo.InvariantCulture));
}