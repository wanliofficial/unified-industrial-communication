using System.Text.RegularExpressions;

namespace Uic.Domain;

/// <summary>
/// 配置校验的唯一入口。启动、热更新、API 写入与模板实例化都必须走这里，
/// 避免出现「控制台能存、重启起不来」的两套规则。
/// </summary>
public static partial class ModelValidator
{
    [GeneratedRegex("^[a-z][a-z0-9-]{1,31}$")]
    private static partial Regex SiteRe();

    [GeneratedRegex("^[a-z0-9][a-z0-9-]{0,31}$")]
    private static partial Regex ChannelRe();

    [GeneratedRegex("^[a-z0-9][a-z0-9._-]{0,63}$")]
    private static partial Regex ItemRe();

    public static bool IsValidItemId(string? id)
        => !string.IsNullOrWhiteSpace(id) && ItemRe().IsMatch(id);

    public static IReadOnlyList<ValidationProblem> Validate(ConfigDocument doc)
    {
        var problems = new List<ValidationProblem>();
        void Add(string path, string msg) => problems.Add(new ValidationProblem(path, msg));

        if (doc.SchemaVersion != 1) Add("schemaVersion", $"unsupported schema version {doc.SchemaVersion}");
        if (!SiteRe().IsMatch(doc.Site)) Add("site", "must match ^[a-z][a-z0-9-]{1,31}$");

        var channelIds = new HashSet<string>(StringComparer.Ordinal);
        foreach (var ch in doc.Channels)
        {
            if (!ChannelRe().IsMatch(ch.Id)) Add($"channels[{ch.Id}].id", "must match ^[a-z0-9][a-z0-9-]{0,31}$");
            if (!channelIds.Add(ch.Id)) Add($"channels[{ch.Id}].id", "duplicate channel id");
            if (string.IsNullOrWhiteSpace(ch.Protocol)) Add($"channels[{ch.Id}].protocol", "required");

            if (ch.MaxConcurrency < 1) Add($"channels[{ch.Id}].maxConcurrency", "must be >= 1");
            if (ch.Timeout <= TimeSpan.Zero) Add($"channels[{ch.Id}].timeout", "must be > 0");
            if (ch.ConnectTimeout <= TimeSpan.Zero) Add($"channels[{ch.Id}].connectTimeout", "must be > 0");

            switch (ch.Transport)
            {
                case "tcp":
                case "udp":
                    if (string.IsNullOrWhiteSpace(ch.Host)) Add($"channels[{ch.Id}].host", "required for tcp/udp");
                    if (ch.Port is < 1 or > 65535) Add($"channels[{ch.Id}].port", "must be 1..65535");
                    break;
                case "serial":
                    if (ch.Serial is null) Add($"channels[{ch.Id}].serial", "required for serial transport");
                    else ch.Serial.Validate(problems, $"channels[{ch.Id}].serial");
                    break;
                case "none":
                    break;
                default:
                    Add($"channels[{ch.Id}].transport", $"unknown '{ch.Transport}'");
                    break;
            }
        }

        var deviceIds = new HashSet<string>(StringComparer.Ordinal);
        foreach (var dev in doc.Devices)
        {
            var dp = $"devices[{dev.Id}]";
            if (!ItemRe().IsMatch(dev.Id)) Add($"{dp}.id", "must match ^[a-z0-9][a-z0-9._-]{0,63}$");
            if (!deviceIds.Add(dev.Id)) Add($"{dp}.id", "duplicate device id");
            if (!channelIds.Contains(dev.ChannelId)) Add($"{dp}.channelId", $"channel '{dev.ChannelId}' not found");
            if (dev.Interval < TimeSpan.FromMilliseconds(10)) Add($"{dp}.interval", "must be >= 10ms");
            if (dev.UnitId is < 1 or > 255) Add($"{dp}.unitId", "must be 1..255");
            if (dev.FailureThreshold < 1) Add($"{dp}.failureThreshold", "must be >= 1");
            if (dev.Retries is < 0 or > 10) Add($"{dp}.retries", "must be 0..10");

            if (dev.Points.Count == 0) Add($"{dp}.points", "at least one point is required");

            var pointIds = new HashSet<string>(StringComparer.Ordinal);
            foreach (var p in dev.Points)
            {
                var pp = $"{dp}.points[{p.Id}]";
                if (!pointIds.Add(p.Id)) Add($"{pp}.id", "duplicate point id in device");
                ValidatePoint(p, pp, problems);
            }
        }

        // 北向通道：静态校验优于运行时才发现发不出去。
        var mqtt = doc.Northbound.Mqtt;
        if (mqtt.Enabled)
        {
            if (string.IsNullOrWhiteSpace(mqtt.Host))
                Add("northbound.mqtt.host", "required when mqtt is enabled");
            if (mqtt.Port is < 1 or > 65535)
                Add("northbound.mqtt.port", "must be 1..65535");
            if (mqtt.Qos is not (0 or 1))
                Add("northbound.mqtt.qos", "must be 0 or 1; QoS2 is not supported");
            if (!mqtt.Topic.Contains("{device}", StringComparison.Ordinal) ||
                !mqtt.Topic.Contains("{point}", StringComparison.Ordinal))
                Add("northbound.mqtt.topic", "topic template must contain {device} and {point}");
            if (mqtt.Topic.Contains("{site}", StringComparison.Ordinal) is false &&
                !mqtt.Topic.StartsWith(mqtt.TopicPrefix, StringComparison.Ordinal))
                Add("northbound.mqtt.topic", $"topic template must start with '{mqtt.TopicPrefix}' or contain {{site}}");
            if (!string.IsNullOrEmpty(mqtt.Password) && string.IsNullOrEmpty(mqtt.Username))
                Add("northbound.mqtt.username", "username is required when password is set");
            if (mqtt.KeepAliveSeconds is < 5 or > 600)
                Add("northbound.mqtt.keepAliveSeconds", "must be 5..600");

            if (mqtt.Commands)
            {
                if (string.IsNullOrWhiteSpace(mqtt.CommandTopic))
                    Add("northbound.mqtt.commandTopic", "required when commands are enabled");
                if (string.IsNullOrWhiteSpace(mqtt.CommandResultTopic))
                    Add("northbound.mqtt.commandResultTopic", "required when commands are enabled");

                if (!string.IsNullOrWhiteSpace(mqtt.CommandTopic) &&
                    !string.IsNullOrWhiteSpace(mqtt.CommandResultTopic) &&
                    string.Equals(mqtt.CommandTopic, mqtt.CommandResultTopic, StringComparison.Ordinal))
                {
                    Add("northbound.mqtt.commandResultTopic", "must differ from commandTopic");
                }

                foreach (var (path, topic) in new[]
                         {
                             ("northbound.mqtt.commandTopic", mqtt.CommandTopic),
                             ("northbound.mqtt.commandResultTopic", mqtt.CommandResultTopic),
                         })
                {
                    if (string.IsNullOrWhiteSpace(topic)) continue;

                    if (topic.Contains("{device}", StringComparison.Ordinal) ||
                        topic.Contains("{point}", StringComparison.Ordinal))
                    {
                        Add(path, "must not contain {device} or {point}; the command channel is per site");
                    }

                    if (!topic.Contains("{site}", StringComparison.Ordinal) &&
                        !topic.StartsWith(mqtt.TopicPrefix, StringComparison.Ordinal))
                    {
                        Add(path, $"must start with '{mqtt.TopicPrefix}' or contain {{site}}");
                    }
                }
            }
        }

        var opc = doc.Northbound.OpcUa;
        if (opc.Enabled)
        {
            if (!opc.Endpoint.StartsWith("opc.tcp://", StringComparison.OrdinalIgnoreCase))
                Add("northbound.opcUa.endpoint", "must start with opc.tcp://");
            if (!Uri.TryCreate(opc.Endpoint, UriKind.Absolute, out _))
                Add("northbound.opcUa.endpoint", "is not a valid URI");
            if (opc.NamespaceIndex < 1)
                Add("northbound.opcUa.namespaceIndex", "must be >= 1");

            // 能力边界写进静态校验：与其等客户端握手失败，不如在配置评审阶段就拒绝。
            if (!string.Equals(opc.SecurityPolicy, "None", StringComparison.OrdinalIgnoreCase))
                Add("northbound.opcUa.securityPolicy",
                    "must be \"None\"; certificate-based security is not implemented in this build");
            if (!opc.AllowAnonymousRead)
                Add("northbound.opcUa.allowAnonymousRead",
                    "must be true; no user authentication backend is wired to the OPC UA endpoint");
        }

        var rest = doc.Northbound.Rest;
        if (rest.Enabled)
        {
            if (string.IsNullOrWhiteSpace(rest.Listen) || !rest.Listen.Contains(':'))
                Add("northbound.rest.listen", "must be 'host:port'");
            else
            {
                var portText = rest.Listen[(rest.Listen.LastIndexOf(':') + 1)..];
                if (!int.TryParse(portText, out var restPort) || restPort is < 1 or > 65535)
                    Add("northbound.rest.listen", "port must be 1..65535");
            }
        }

        var storage = doc.Storage;
        if (storage.MinFreeBytes < 0)
            Add("storage.minFreeBytes", "must be >= 0; 0 disables the disk watermark");
        if (storage.ResumeFreeBytes < 0)
            Add("storage.resumeFreeBytes", "must be >= 0");
        if (storage.ResumeFreeBytes > 0 && storage.MinFreeBytes == 0)
            Add("storage.resumeFreeBytes", "requires minFreeBytes");
        if (storage.MinFreeBytes > 0 && storage.ResumeFreeBytes > 0 &&
            storage.ResumeFreeBytes <= storage.MinFreeBytes)
            Add("storage.resumeFreeBytes",
                "must be greater than minFreeBytes so recovery does not flap");
        if (storage.MaxMemoryBytes < 0)
            Add("storage.maxMemoryBytes", "must be >= 0; 0 disables the memory watermark");
        if (storage.ResourceCheckInterval < TimeSpan.FromSeconds(1) ||
            storage.ResourceCheckInterval > TimeSpan.FromHours(1))
            Add("storage.resourceCheckInterval", "must be 1s..1h");

        var file = doc.Logging.File;

        // 即使当前 disabled 也校验：配置里留一个错值，等现场打开时就变成启动失败，
        // 而那时往往已经不具备改配置的条件。
        if (string.IsNullOrWhiteSpace(file.Directory))
            Add("logging.file.directory", "required");
        if (string.IsNullOrWhiteSpace(file.FileName) ||
            file.FileName is "." or ".." ||
            file.FileName.IndexOfAny(new[] { '/', '\\' }) >= 0 ||
            file.FileName.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0)
        {
            Add("logging.file.fileName", "must be a bare file name without path separators");
        }

        if (file.MaxBytes < 4096)
            Add("logging.file.maxBytes", "must be >= 4096; smaller files rotate on every line");
        if (file.MaxBytes > 1024L * 1024 * 1024)
            Add("logging.file.maxBytes", "must be <= 1 GiB");
        if (file.Retain is < 1 or > 100)
            Add("logging.file.retain", "must be 1..100");

        if (file.MinimumLevel is not null &&
            !AllowedLogLevels.Contains(file.MinimumLevel))
        {
            Add("logging.file.minimumLevel",
                "must be one of trace/debug/information/warning/error/critical");
        }

        return problems;
    }

    private static readonly HashSet<string> AllowedLogLevels =
        new(StringComparer.OrdinalIgnoreCase)
        {
            "trace", "debug", "information", "info", "warning", "warn", "error", "critical",
        };

    public static void ThrowIfInvalid(ConfigDocument doc)
    {
        var problems = Validate(doc);
        if (problems.Count > 0)
        {
            throw new ConfigException(
                $"configuration is invalid ({problems.Count} problem(s))",
                problems);
        }
    }

    /// <summary>
    /// 点位自身的校验。抽出来是为了让 CSV 导入与配置校验共用同一套规则——
    /// 两处各写一份，迟早出现「CSV 能导入、启动时被拒」。
    /// </summary>
    public static void ValidatePoint(Point p, string pp, List<ValidationProblem> problems)
    {
        void Add(string path, string msg) => problems.Add(new ValidationProblem(path, msg));

        if (!ItemRe().IsMatch(p.Id)) Add($"{pp}.id", "must match ^[a-z0-9][a-z0-9._-]{0,63}$");
        p.Address.Validate(problems, $"{pp}.address");

        if (p.DataType is DataType.String or DataType.Bytes)
        {
            if (p.Length <= 0) Add($"{pp}.length", "required for string/bytes");
            if (p.Length > 1024) Add($"{pp}.length", "must be <= 1024");
        }

        // 寄存器长度必须与数据类型匹配，否则批量窗口会算错。
        var expected = p.DataType.Registers();
        if (p.Address.Kind == AddressKind.Register && p.DataType is not (DataType.String or DataType.Bytes))
        {
            if (p.Address.Length > 0 && p.Address.Length < expected)
                Add($"{pp}.address.length", $"{p.DataType} needs {expected} register(s), got {p.Address.Length}");
        }

        if (p.DataType == DataType.Bool && p.Address.Area is not null)
        {
            var area = p.Address.Area.ToLowerInvariant();
            if (area is "coil" or "coils" or "discrete" or "discrete_input" or "discreteinput")
            {
                if (p.Address.Bit != -1)
                    Add($"{pp}.address.bit", "bit index is only valid for word areas");
            }
        }

        if (p.Deadband is < 0) Add($"{pp}.deadband", "must be >= 0");
        if (double.IsNaN(p.Scale) || p.Scale == 0) Add($"{pp}.scale", "must be non-zero");

        // 量程必须成对出现：只有上限或只有下限都无法解释「百分比」。
        if ((p.RangeMin is null) != (p.RangeMax is null))
            Add($"{pp}.rangeMin", "rangeMin and rangeMax must be provided together");
        else if (p.RangeMin is { } rangeMin && p.RangeMax is { } rangeMax && rangeMin >= rangeMax)
            Add($"{pp}.rangeMax", "must be greater than rangeMin");

        switch (p.DeadbandMode)
        {
            case DeadbandMode.Percent:
                if (p.RangeMin is null || p.RangeMax is null)
                    Add($"{pp}.rangeMin", "required when deadbandMode is 'percent'");
                if (p.Deadband is null or <= 0 or > 100)
                    Add($"{pp}.deadband", "must be in (0, 100] for deadbandMode 'percent'");
                break;

            case DeadbandMode.Rate:
                // 变化率的单位是「每秒」，0 或负数没有意义。
                if (p.Deadband is null or <= 0)
                    Add($"{pp}.deadband", "must be > 0 for deadbandMode 'rate'");
                break;

            default:
                if (!Enum.IsDefined(p.DeadbandMode))
                    Add($"{pp}.deadbandMode", $"unknown '{p.DeadbandMode}'");
                break;
        }
    }
}
