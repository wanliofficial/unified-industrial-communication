namespace Uic.Domain;

/// <summary>
/// 四类寻址的统一表达。同一条地址只填写与 <see cref="Kind"/> 对应的字段，
/// 其余字段必须留空——混填是「跨协议配置被复制粘贴」时最常见的错误。
/// </summary>
public sealed record Address
{
    public AddressKind Kind { get; init; }

    // register
    public string? Area { get; init; }
    public int Offset { get; init; }
    public int Length { get; init; }
    public int Bit { get; init; } = -1;

    // symbol
    public string? Path { get; init; }
    public int Index { get; init; }

    // object
    public string? ObjectType { get; init; }
    public int Instance { get; init; }
    public string? Property { get; init; }
    public int PropIndex { get; init; }

    // file
    public int FileId { get; init; }
    public int RecordId { get; init; }

    /// <summary>
    /// 寄存器寻址。<paramref name="length"/> 为 0 表示「按点位的数据类型推导」，
    /// 只有需要覆盖默认宽度时才显式填写（例如一次读多个连续寄存器）。
    /// </summary>
    public static Address Register(string area, int offset, int length = 0, int bit = -1) => new()
    {
        Kind = AddressKind.Register,
        Area = area,
        Offset = offset,
        Length = length,
        Bit = bit,
    };

    public static Address Symbol(string path, int index = 0) => new()
    {
        Kind = AddressKind.Symbol,
        Path = path,
        Index = index,
    };

    public static Address Object(string objectType, int instance, string property, int propIndex = 0) => new()
    {
        Kind = AddressKind.Object,
        ObjectType = objectType,
        Instance = instance,
        Property = property,
        PropIndex = propIndex,
    };

    public static Address File(int fileId, int recordId, int offset = 0) => new()
    {
        Kind = AddressKind.File,
        FileId = fileId,
        RecordId = recordId,
        Offset = offset,
    };

    public void Validate(List<ValidationProblem> problems, string path)
    {
        void Add(string p, string m) => problems.Add(new ValidationProblem(p, m));

        switch (Kind)
        {
            case AddressKind.Register:
                if (string.IsNullOrWhiteSpace(Area)) Add($"{path}.area", "required for register addressing");
                if (Offset < 0) Add($"{path}.offset", "must be >= 0");
                if (Length < 0 || Length > 125) Add($"{path}.length", "must be 0..125");
                if (Bit < -1 || Bit > 15) Add($"{path}.bit", "must be -1..15");
                if (!string.IsNullOrEmpty(Path)) Add($"{path}.path", "not allowed for register addressing");
                if (!string.IsNullOrEmpty(ObjectType)) Add($"{path}.objectType", "not allowed for register addressing");
                if (FileId != 0) Add($"{path}.fileId", "not allowed for register addressing");
                break;

            case AddressKind.Symbol:
                if (string.IsNullOrWhiteSpace(Path)) Add($"{path}.path", "required for symbol addressing");
                if (Index < 0) Add($"{path}.index", "must be >= 0");
                if (!string.IsNullOrEmpty(Area)) Add($"{path}.area", "not allowed for symbol addressing");
                if (FileId != 0) Add($"{path}.fileId", "not allowed for symbol addressing");
                break;

            case AddressKind.Object:
                if (string.IsNullOrWhiteSpace(ObjectType)) Add($"{path}.objectType", "required for object addressing");
                if (string.IsNullOrWhiteSpace(Property)) Add($"{path}.property", "required for object addressing");
                if (Instance < 0) Add($"{path}.instance", "must be >= 0");
                if (!string.IsNullOrEmpty(Area)) Add($"{path}.area", "not allowed for object addressing");
                break;

            case AddressKind.File:
                if (FileId < 0) Add($"{path}.fileId", "must be >= 0");
                if (RecordId < 0) Add($"{path}.recordId", "must be >= 0");
                if (!string.IsNullOrEmpty(Area)) Add($"{path}.area", "not allowed for file addressing");
                break;

            default:
                Add($"{path}.kind", $"unknown value {(int)Kind}");
                break;
        }
    }

    /// <summary>用于排序与去重的稳定键；批量规划依赖它。</summary>
    public string StableKey() => Kind switch
    {
        AddressKind.Register => $"r:{Area}:{Offset}:{Length}:{Bit}",
        AddressKind.Symbol => $"s:{Path}:{Index}",
        AddressKind.Object => $"o:{ObjectType}:{Instance}:{Property}:{PropIndex}",
        AddressKind.File => $"f:{FileId}:{RecordId}:{Offset}",
        _ => "?",
    };
}

/// <summary>串口参数。只在 <c>transport=serial</c> 时生效。</summary>
public sealed record SerialOptions
{
    public string Port { get; init; } = "/dev/ttyUSB0";
    public int Baud { get; init; } = 9600;
    public int DataBits { get; init; } = 8;
    public string Parity { get; init; } = "none";
    public string StopBits { get; init; } = "one";
    public int ReadTimeoutMs { get; init; } = 1000;

    public void Validate(List<ValidationProblem> problems, string path)
    {
        void Add(string p, string m) => problems.Add(new ValidationProblem(p, m));

        if (string.IsNullOrWhiteSpace(Port)) Add($"{path}.port", "required");
        if (Baud is < 300 or > 921600) Add($"{path}.baud", "must be 300..921600");
        if (DataBits is < 5 or > 8) Add($"{path}.dataBits", "must be 5..8");
        if (Parity is not ("none" or "odd" or "even" or "mark" or "space"))
            Add($"{path}.parity", "must be none|odd|even|mark|space");
        if (StopBits is not ("one" or "onePointFive" or "two"))
            Add($"{path}.stopBits", "must be one|onePointFive|two");
        if (ReadTimeoutMs is < 10 or > 60000) Add($"{path}.readTimeoutMs", "must be 10..60000");
    }
}

/// <summary>一条物理或逻辑链路。并发上限、超时与故障边界都在这一层。</summary>
public sealed class Channel
{
    public required string Id { get; init; }
    public string? Name { get; init; }
    public required string Protocol { get; init; }
    public bool Enabled { get; init; } = true;

    /// <summary>tcp | serial | udp</summary>
    public string Transport { get; init; } = "tcp";
    public string? Host { get; init; }
    public int Port { get; init; }
    public SerialOptions? Serial { get; init; }

    /// <summary>通道级并发上限。写请求会插队到读之前。</summary>
    public int MaxConcurrency { get; init; } = 4;
    public TimeSpan Timeout { get; init; } = TimeSpan.FromSeconds(3);
    public TimeSpan ConnectTimeout { get; init; } = TimeSpan.FromSeconds(5);
    public TimeSpan IdleTimeout { get; init; } = TimeSpan.FromSeconds(30);
}

/// <summary>设备（从站/站点）。同一设备串行采集，跨设备并行。</summary>
public sealed class Device
{
    public required string Id { get; init; }
    public string? Name { get; init; }
    public required string ChannelId { get; init; }
    public bool Enabled { get; init; } = true;

    public TimeSpan Interval { get; init; } = TimeSpan.FromSeconds(1);
    public TimeSpan? Timeout { get; init; }
    public int Retries { get; init; }
    public int UnitId { get; init; } = 1;
    public ByteOrder ByteOrder { get; init; } = ByteOrder.ABCD;

    /// <summary>连续失败多少次后进入降级（熔断），之后按探活周期恢复。</summary>
    public int FailureThreshold { get; init; } = 3;
    public TimeSpan RecoverInterval { get; init; } = TimeSpan.FromSeconds(30);

    /// <summary>驱动私有参数（波形、故障注入、方言开关等）。键名由驱动 ConfigFields 声明。</summary>
    public Dictionary<string, string> Labels { get; init; } = new(StringComparer.Ordinal);

    /// <summary>实例化来源模板。只用于追溯，模板后续变更不会自动改写设备。</summary>
    public string? TemplateId { get; init; }

    public List<Point> Points { get; init; } = new();

    public string? Label(string key) =>
        Labels.TryGetValue(key, out var v) && !string.IsNullOrWhiteSpace(v) ? v : null;

    /// <summary>替换点位列表，其余字段原样保留（点表导入用）。</summary>
    public Device WithPoints(IReadOnlyList<Point> points) => new()
    {
        Id = Id,
        Name = Name,
        ChannelId = ChannelId,
        Enabled = Enabled,
        Interval = Interval,
        Timeout = Timeout,
        Retries = Retries,
        UnitId = UnitId,
        ByteOrder = ByteOrder,
        FailureThreshold = FailureThreshold,
        RecoverInterval = RecoverInterval,
        Labels = Labels,
        TemplateId = TemplateId,
        Points = points.ToList(),
    };

    /// <summary>记录实例来源模板。</summary>
    public Device WithTemplateId(string templateId) => new()
    {
        Id = Id,
        Name = Name,
        ChannelId = ChannelId,
        Enabled = Enabled,
        Interval = Interval,
        Timeout = Timeout,
        Retries = Retries,
        UnitId = UnitId,
        ByteOrder = ByteOrder,
        FailureThreshold = FailureThreshold,
        RecoverInterval = RecoverInterval,
        Labels = Labels,
        TemplateId = templateId,
        Points = Points,
    };
}

/// <summary>点位。地址 + 类型 + 缩放 + 死区 + 单位。</summary>
public sealed class Point
{
    public required string Id { get; init; }
    public string? Name { get; init; }
    public required Address Address { get; init; }
    public DataType DataType { get; init; } = DataType.UInt16;
    public ByteOrder? ByteOrder { get; init; }

    public double Scale { get; init; } = 1;
    public double Offset { get; init; }

    /// <summary>死区（绝对值）。为空表示不启用死区过滤。</summary>
    public double? Deadband { get; init; }

    public string? Unit { get; init; }
    public bool Writable { get; init; } = true;
    public bool Enabled { get; init; } = true;

    /// <summary>string/bytes 类型的显式长度（字节）；其余类型可留 0。</summary>
    public int Length { get; init; }

    /// <summary>该点位实际占用的寄存器数，规划与编解码共用。</summary>
    public int RegisterCount() => DataType switch
    {
        DataType.String or DataType.Bytes => Math.Max(1, (Length + 1) / 2),
        _ => Math.Max(1, DataType.Registers()),
    };
}
