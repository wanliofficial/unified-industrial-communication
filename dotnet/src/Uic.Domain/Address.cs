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
                if (FileId is < 0 or > 0xFFFF) Add($"{path}.fileId", "must be 0..65535");
                if (RecordId is < 0 or > 0xFFFF) Add($"{path}.recordId", "must be 0..65535");
                if (!string.IsNullOrEmpty(Area)) Add($"{path}.area", "not allowed for file addressing");
                // 文件记录寻址没有「记录内偏移」：帧里的引用就是「文件号 + 记录号 + 记录长度」。
                // 放行非 0 的 offset 会让运行期每轮静默报 address_error，配置阶段就该拒绝。
                if (Offset != 0) Add($"{path}.offset", "must be 0 for file addressing; use recordId to move");
                // 记录长度由点位的寄存器数推出（count × 类型宽度，string/bytes 用点位级 length）。
                // 寄存器寻址的窗口宽度 `length` 在这里没有任何作用，静默忽略就是「配了不生效」。
                if (Length != 0) Add($"{path}.length", "not allowed for file addressing; the record length comes from count and the data type");
                // `path`/`objectType` 由 symbol/object 寻址独占，`bit` 由字区的位访问独占。
                // 它们填在 file 上都是抄错了别的寻址，拒绝时把提示写清楚，比「字段没生效」好查。
                if (Bit != -1) Add($"{path}.bit", "not allowed for file addressing; a record holds whole registers");
                if (!string.IsNullOrEmpty(Path)) Add($"{path}.path", "not allowed for file addressing; that field belongs to symbol/object addressing");
                if (!string.IsNullOrEmpty(ObjectType)) Add($"{path}.objectType", "not allowed for file addressing; that field belongs to object addressing");
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

/// <summary>死区判定方式。三种方式解决的是三种现场噪声，不能互相替代。</summary>
public enum DeadbandMode
{
    /// <summary>绝对值：|Δ| &gt; deadband。默认，单位与点位一致。</summary>
    Absolute,

    /// <summary>量程百分比：|Δ| &gt; deadband% × (rangeMax - rangeMin)。要求配置量程。</summary>
    Percent,

    /// <summary>变化率：|Δ| / Δt(秒) &gt; deadband。用于抑制慢漂移、保留突跳。</summary>
    Rate,
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

    /// <summary>死区阈值。含义由 <see cref="DeadbandMode"/> 决定；为空表示不启用死区过滤。</summary>
    public double? Deadband { get; init; }

    /// <summary>死区判定方式。默认绝对值，兼容旧配置。</summary>
    public DeadbandMode DeadbandMode { get; init; } = DeadbandMode.Absolute;

    /// <summary>量程下限。与 <see cref="RangeMax"/> 成对出现，百分比死区与上位机显示都用它。</summary>
    public double? RangeMin { get; init; }

    /// <summary>量程上限。</summary>
    public double? RangeMax { get; init; }

    /// <summary>
    /// 点位自己的采集周期。为空或 0 表示继承设备周期。
    ///
    /// 快变量（流量、位置）和慢变量（液位、温度）挂在同一台 PLC 上很常见，
    /// 全按最快周期读会把链路的预算浪费在慢变量上；全按最慢周期读又会漏掉快变量。
    /// 允许点位覆盖周期，才能在一台设备上做到「100ms 与 10s 并存」。
    /// </summary>
    public TimeSpan? Interval { get; init; }

    /// <summary>有效周期：点位覆盖优先，否则继承设备。0 与空值等价。</summary>
    public TimeSpan EffectiveInterval(Device device)
        => Interval is { } own && own > TimeSpan.Zero ? own : device.Interval;

    public string? Unit { get; init; }
    public bool Writable { get; init; } = true;
    public bool Enabled { get; init; } = true;

    /// <summary>string/bytes 类型的显式长度（字节）；其余类型可留 0。</summary>
    public int Length { get; init; }

    /// <summary>
    /// 数组元素个数。0 或 1 表示标量，&gt;1 表示定长数组。
    ///
    /// 用「元素个数」而不是「占用寄存器数」表达维度：后者随数据类型变化
    /// （uint16 是 1 个寄存器、float64 是 4 个），写点表的人得先自己乘一遍，
    /// 乘错就会读偏整段数据。元素个数是现场唯一不用换算就能核对的值。
    /// </summary>
    public int Count { get; init; }

    public bool IsArray => Count > 1;

    /// <summary>该点位实际占用的寄存器数，规划与编解码共用。</summary>
    public int RegisterCount()
    {
        if (DataType is DataType.String or DataType.Bytes) return Math.Max(1, (Length + 1) / 2);

        var elements = Count > 1 ? Count : 1;
        return Math.Max(1, DataType.Registers() * elements);
    }
}
