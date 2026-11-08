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
    public LoggingOptions Logging { get; init; } = new();

    /// <summary>替换设备列表，其余字段原样保留。模板/导入都靠它构造候选配置。</summary>
    public ConfigDocument WithDevices(IReadOnlyList<Device> devices) => new()
    {
        SchemaVersion = SchemaVersion,
        Site = Site,
        Description = Description,
        Channels = Channels,
        Devices = devices.ToList(),
        Northbound = Northbound,
        Storage = Storage,
        Logging = Logging,
    };
}

/// <summary>北向通道配置。每个通道独立启停，未实现的能力必须显式失败而不是静默降级。</summary>
public sealed class NorthboundOptions
{
    public RestOptions Rest { get; init; } = new();
    public MqttOptions Mqtt { get; init; } = new();
    public OpcUaOptions OpcUa { get; init; } = new();
}

public sealed record RestOptions
{
    public bool Enabled { get; init; } = true;
    public string Listen { get; init; } = "0.0.0.0:8080";

    /// <summary>Bearer Token。为空表示不启用认证（仅限本机调试，启动时会告警）。</summary>
    public string? Token { get; init; }

    /// <summary>静态前端资源目录；为空时由宿主内嵌资源提供。</summary>
    public string? StaticRoot { get; init; }
}

public sealed record MqttOptions
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

    /// <summary>
    /// 是否开放下行命令（订阅 CommandTopic）。默认关闭：明文匿名通道上的
    /// 写操作必须由部署方显式打开。
    /// </summary>
    public bool Commands { get; init; }

    /// <summary>命令主题模板。默认 {prefix}/{site}/$command。</summary>
    public string CommandTopic { get; init; } = "{prefix}/{site}/$command";

    /// <summary>命令结果主题模板。默认 {prefix}/{site}/$command/result。</summary>
    public string CommandResultTopic { get; init; } = "{prefix}/{site}/$command/result";
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

    public AuditOptions Audit { get; init; } = new();

    public int OutboxMaxRecords { get; init; } = 100_000;
    public long OutboxMaxBytes { get; init; } = 256L * 1024 * 1024;
    public TimeSpan OutboxTtl { get; init; } = TimeSpan.FromHours(24);
    public int OutboxBatchSize { get; init; } = 64;
    public TimeSpan OutboxFlushInterval { get; init; } = TimeSpan.FromSeconds(1);

    /// <summary>
    /// 数据目录所在分区的剩余空间低于该值时进入降级：暂停文件日志与 journal，
    /// 健康状态转 degraded，给 Outbox 留出最后一点写盘空间。0 表示不检查。
    /// 没有默认值是因为「多少算少」完全取决于设备：1GB 的盒子与机房虚拟机不是一回事。
    /// </summary>
    public long MinFreeBytes { get; init; }

    /// <summary>恢复到该值以上才解除降级；0 表示取 minFreeBytes 的两倍作为回差。</summary>
    public long ResumeFreeBytes { get; init; }

    /// <summary>进程工作集超过该值时进入降级（只告警，不主动释放内存）。0 表示不检查。</summary>
    public long MaxMemoryBytes { get; init; }

    /// <summary>资源水位检查间隔。1 秒 .. 1 小时。</summary>
    public TimeSpan ResourceCheckInterval { get; init; } = TimeSpan.FromSeconds(10);
}

/// <summary>
/// 审计日志落盘设置。
///
/// 与文件日志不同，审计默认**开启**：文件日志是排障辅助，缺了只是不方便；
/// 审计是「配置是谁改的、写操作是谁下的」的唯一答案，默认关闭等于没有。
/// 目录跟着数据目录走，随配置版本一起搬机器。
/// </summary>
public sealed class AuditOptions
{
    public bool Enabled { get; init; } = true;

    /// <summary>单个文件的上限。写满后开新文件，不会把一条记录切成两半。</summary>
    public long MaxBytes { get; init; } = 8L * 1024 * 1024;

    /// <summary>最多保留几个文件（含当前文件）。审计是有成本的记忆，必须设上限。</summary>
    public int Retain { get; init; } = 6;

    /// <summary>相对数据目录的子目录；也接受绝对路径。</summary>
    public string Directory { get; init; } = "audit";
}

/// <summary>日志配置。默认不落盘：边缘设备磁盘小，没显式配置就不该悄悄写文件。</summary>
public sealed class LoggingOptions
{
    public FileLogOptions File { get; init; } = new();
}

/// <summary>
/// JSON 行文件日志。一行一个对象，方便直接 <c>grep</c> / <c>jq</c>，
/// 也避免多行异常把日志切碎后无法按行恢复。
/// </summary>
public sealed class FileLogOptions
{
    /// <summary>默认关闭。内存环形日志仍然可用，文件是给需要留档的现场用的。</summary>
    public bool Enabled { get; init; }

    /// <summary>相对 <c>storage.dataDir</c> 的目录；绝对路径原样使用。</summary>
    public string Directory { get; init; } = "logs";

    /// <summary>当前活动文件名。轮转后的归档名由它派生。</summary>
    public string FileName { get; init; } = "uic.log";

    /// <summary>单个文件大小上限，超过后轮转。下限 4 KiB，避免每行切一个文件。</summary>
    public long MaxBytes { get; init; } = 10L * 1024 * 1024;

    /// <summary>保留的归档份数（不含当前文件）。</summary>
    public int Retain { get; init; } = 5;

    /// <summary>文件日志独立的最低级别；为空时沿用全局级别。</summary>
    public string? MinimumLevel { get; init; }
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

        // 复合写法（"1h0m0s"、"1h30m"）来自 Go 风格配置与旧示例。
        // 只认「数字 + 单位」的重复序列，不接受任何表达式——配置文件不是脚本。
        if (TryComposite(text, out var composite)) return composite;

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

    private static bool TryComposite(string text, out TimeSpan result)
    {
        result = TimeSpan.Zero;

        var totalMs = 0d;
        var index = 0;
        var segments = 0;

        while (index < text.Length)
        {
            var start = index;

            while (index < text.Length && (char.IsAsciiDigit(text[index]) || text[index] == '.'))
                index++;

            if (index == start) return false;

            if (!double.TryParse(text[start..index],
                    System.Globalization.NumberStyles.Float,
                    System.Globalization.CultureInfo.InvariantCulture, out var value))
            {
                return false;
            }

            if (index >= text.Length) return false;

            string unit;

            if (index + 1 < text.Length &&
                (text.Substring(index, 2).Equals("ms", StringComparison.OrdinalIgnoreCase) ||
                 text.Substring(index, 2).Equals("us", StringComparison.OrdinalIgnoreCase) ||
                 text.Substring(index, 2).Equals("ns", StringComparison.OrdinalIgnoreCase)))
            {
                unit = text.Substring(index, 2).ToLowerInvariant();
                index += 2;
            }
            else
            {
                unit = char.ToLowerInvariant(text[index]).ToString();
                index++;
            }

            totalMs += unit switch
            {
                "ms" => value,
                "us" or "µs" => value / 1000d,
                "ns" => value / 1_000_000d,
                "s" => value * 1000d,
                "m" => value * 60_000d,
                "h" => value * 3_600_000d,
                "d" => value * 86_400_000d,
                _ => double.NaN,
            };

            if (double.IsNaN(totalMs)) return false;

            segments++;
        }

        if (segments == 0) return false;

        result = TimeSpan.FromMilliseconds(totalMs);
        return true;
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
