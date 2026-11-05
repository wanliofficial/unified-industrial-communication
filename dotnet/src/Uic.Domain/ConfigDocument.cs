using System.Text.Json;
using System.Text.Json.Serialization;

namespace Uic.Domain;

/// <summary>整份配置。配置即制品：有版本、可 diff、可回滚。</summary>
public sealed class ConfigDocument
{
    public int SchemaVersion { get; init; } = 1;
    public string Site { get; init; } = "default";
    public string? Description { get; init; }
    public List<Channel> Channels { get; init; } = new();
    public List<Device> Devices { get; init; } = new();
    public NorthboundOptions Northbound { get; init; } = new();
    public StorageOptions Storage { get; init; } = new();
}

/// <summary>北向通道配置。每个通道独立启停，未实现的能力必须显式失败而不是静默降级。</summary>
public sealed class NorthboundOptions
{
    public RestOptions Rest { get; init; } = new();
    public MqttOptions Mqtt { get; init; } = new();
    public OpcUaOptions OpcUa { get; init; } = new();
}

public sealed class RestOptions
{
    public bool Enabled { get; init; } = true;
    public string Listen { get; init; } = "0.0.0.0:8080";

    /// <summary>Bearer Token。为空表示不启用认证（仅限本机调试，启动时会告警）。</summary>
    public string? Token { get; init; }

    /// <summary>静态前端资源目录；为空时由宿主内嵌资源提供。</summary>
    public string? StaticRoot { get; init; }
}

public sealed class MqttOptions
{
    public bool Enabled { get; init; }
    public string Host { get; init; } = "127.0.0.1";
    public int Port { get; init; } = 1883;
    public string? Username { get; init; }
    public string? Password { get; init; }
    public string ClientId { get; init; } = "uic-gateway";
    public string TopicPrefix { get; init; } = "uic";

    /// <summary>值主题模板，必须同时包含 {device} 与 {point}。默认 uic/{site}/{device}/{point}/value。</summary>
    public string Topic { get; init; } = "uic/{site}/{device}/{point}/value";

    public int Qos { get; init; } = 1;
    public bool Retain { get; init; }
    public int KeepAliveSeconds { get; init; } = 30;
    public int ReconnectDelayMs { get; init; } = 2000;
    public bool PublishedSnapshotOnStart { get; init; } = true;
}

public sealed class OpcUaOptions
{
    public bool Enabled { get; init; }
    public string Endpoint { get; init; } = "opc.tcp://0.0.0.0:4840";
    public string ApplicationName { get; init; } = "UIC Gateway";
    public string NamespaceUri { get; init; } = "urn:uic:gateway";
    public int NamespaceIndex { get; init; } = 2;

    /// <summary>匿名只读访问。写操作始终需要认证会话。</summary>
    public bool AllowAnonymousRead { get; init; } = true;
    public bool AllowAnonymousWrite { get; init; }

    /// <summary>安全策略。None 为唯一切实可用的默认值；签名/加密需证书。</summary>
    public string SecurityPolicy { get; init; } = "None";
}

public sealed class StorageOptions
{
    public string DataDir { get; init; } = "./data";

    /// <summary>本地 journal 兜底（NDJSON）。不是北向协议，默认关闭。</summary>
    public bool Journal { get; init; }

    public int OutboxMaxRecords { get; init; } = 100_000;
    public long OutboxMaxBytes { get; init; } = 256L * 1024 * 1024;
    public TimeSpan OutboxTtl { get; init; } = TimeSpan.FromHours(24);
    public int OutboxBatchSize { get; init; } = 64;
    public TimeSpan OutboxFlushInterval { get; init; } = TimeSpan.FromSeconds(1);
}

/// <summary>把 <c>"1s"</c> / <c>"500ms"</c> / <c>"2m"</c> 解析为 TimeSpan；纯数字按毫秒处理。</summary>
public sealed class DurationConverter : JsonConverter<TimeSpan>
{
    public override TimeSpan Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
    {
        if (reader.TokenType == JsonTokenType.Number)
        {
            return TimeSpan.FromMilliseconds(reader.GetDouble());
        }

        var text = reader.GetString();
        if (string.IsNullOrWhiteSpace(text)) return TimeSpan.Zero;
        return Parse(text);
    }

    public static TimeSpan Parse(string text)
    {
        text = text.Trim();
        if (text.Length == 0) throw new FormatException("empty duration");

        // 兼容 TimeSpan 的标准写法（"1.00:00:00"）。配置文件可能由人写、由旧版本写出，
        // 也可能从 .NET 的默认序列化直接拷来，这条路径必须能读回去。
        if (text.Contains(':') &&
            TimeSpan.TryParse(text, System.Globalization.CultureInfo.InvariantCulture, out var span))
        {
            return span;
        }

        static double Num(string s) => double.Parse(s, System.Globalization.CultureInfo.InvariantCulture);

        if (text.EndsWith("ms", StringComparison.OrdinalIgnoreCase)) return TimeSpan.FromMilliseconds(Num(text[..^2]));
        if (text.EndsWith("us", StringComparison.OrdinalIgnoreCase)) return TimeSpan.FromMicroseconds(Num(text[..^2]));
        if (text.EndsWith("ns", StringComparison.OrdinalIgnoreCase)) return TimeSpan.FromMicroseconds(Num(text[..^2]) / 1000d);
        if (text.EndsWith('s')) return TimeSpan.FromSeconds(Num(text[..^1]));
        if (text.EndsWith('m')) return TimeSpan.FromMinutes(Num(text[..^1]));
        if (text.EndsWith('h')) return TimeSpan.FromHours(Num(text[..^1]));
        if (text.EndsWith('d')) return TimeSpan.FromDays(Num(text[..^1]));

        // 无单位时按毫秒处理，避免把 "1000" 误解成 1000 秒。
        return TimeSpan.FromMilliseconds(Num(text));
    }

    public override void Write(Utf8JsonWriter writer, TimeSpan value, JsonSerializerOptions options)
        => writer.WriteStringValue(Format(value));

    public static string Format(TimeSpan v)
    {
        if (v == TimeSpan.Zero) return "0s";
        if (v.TotalMilliseconds < 1000 && v.TotalMilliseconds == Math.Floor(v.TotalMilliseconds))
            return $"{v.TotalMilliseconds:0}ms";
        if (v.TotalSeconds < 60 && v.TotalSeconds == Math.Floor(v.TotalSeconds))
            return $"{v.TotalSeconds:0}s";
        if (v.TotalMinutes < 60 && v.TotalMinutes == Math.Floor(v.TotalMinutes))
            return $"{v.TotalMinutes:0}m";
        if (v.TotalHours < 24 && v.TotalHours == Math.Floor(v.TotalHours))
            return $"{v.TotalHours:0}h";
        if (v.TotalDays == Math.Floor(v.TotalDays))
            return $"{v.TotalDays:0}d";

        // 兜底：小数秒。绝不输出 TimeSpan 的 "d.hh:mm:ss" 形式——那与本转换器的读入语法不一致，
        // 会让「写出去再读回来」失败。
        return $"{v.TotalSeconds.ToString("0.###", System.Globalization.CultureInfo.InvariantCulture)}s";
    }
}

/// <summary>全局 JSON 约定：camelCase、枚举用 camelCase 字符串、时长用 <c>"1s"</c>。</summary>
public static class JsonDefaults
{
    public static readonly JsonSerializerOptions Options = Create();
    public static readonly JsonSerializerOptions Indented = Create(writeIndented: true);

    private static JsonSerializerOptions Create(bool writeIndented = false)
    {
        var o = new JsonSerializerOptions
        {
            PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
            PropertyNameCaseInsensitive = true,
            DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
            WriteIndented = writeIndented,
            ReadCommentHandling = JsonCommentHandling.Skip,
            AllowTrailingCommas = true,
        };
        o.Converters.Add(new DataTypeConverter());
        o.Converters.Add(new JsonStringEnumConverter(JsonNamingPolicy.CamelCase, allowIntegerValues: false));
        o.Converters.Add(new DurationConverter());
        o.Converters.Add(new UtcDateTimeOffsetConverter());
        return o;
    }

    public static string ToJson<T>(T value, bool indented = false) =>
        JsonSerializer.Serialize(value, indented ? Indented : Options);

    public static T? FromJson<T>(string json) => JsonSerializer.Deserialize<T>(json, Options);
}

/// <summary>把已解析配置转成稳定 JSON，用于计算内容哈希。</summary>
public static class CanonicalJson
{
    public static string Of(ConfigDocument doc) => JsonSerializer.Serialize(doc, Options);

    public static readonly JsonSerializerOptions Options =
        new(JsonDefaults.Options) { WriteIndented = false };
}
