using Microsoft.Extensions.Logging;
using Uic.Domain;

namespace Uic.Driver;

/// <summary>驱动在协议中的角色。</summary>
public enum DriverRole
{
    Master,
    Slave,
    Both,
}

/// <summary>
/// 能力四态。禁止在不确定时声明 <see cref="Supported"/>——
/// 「某协议支持符号寻址」在不同型号上含义不同，布尔能力位只会撒谎。
/// </summary>
public enum CapState
{
    Supported,
    Unsupported,
    RuntimeDependent,
    Experimental,
}

public sealed record Capabilities(
    CapState Read = CapState.Supported,
    CapState Write = CapState.Unsupported,
    CapState Subscribe = CapState.Unsupported,
    CapState Browse = CapState.Unsupported,
    CapState Discovery = CapState.Unsupported,
    CapState MultiRead = CapState.Supported,
    CapState BitAccess = CapState.Unsupported,
    CapState StructTypes = CapState.Unsupported);

/// <summary>用于前端生成配置表单的字段声明。</summary>
public sealed record ConfigField(
    string Name,
    string Label,
    string Type,
    bool Required = false,
    object? Default = null,
    IReadOnlyList<string>? Enum = null,
    string? Placeholder = null,
    string? Help = null,
    string Scope = "device");

/// <summary>驱动的不可变元数据。</summary>
public sealed record DriverMeta
{
    public required string Id { get; init; }
    public required string Name { get; init; }
    public string Version { get; init; } = "1.0.0";
    public string SdkVersion { get; init; } = DriverSdk.Version;
    public IReadOnlyList<DriverRole> Roles { get; init; } = new[] { DriverRole.Master };
    public IReadOnlyList<string> Transports { get; init; } = new[] { "tcp" };
    public IReadOnlyList<AddressKind> AddressKinds { get; init; } = new[] { AddressKind.Register };
    public Capabilities Capabilities { get; init; } = new();
    public IReadOnlyList<ConfigField> ConfigFields { get; init; } = Array.Empty<ConfigField>();
    public IReadOnlyList<ConfigField> PointFields { get; init; } = Array.Empty<ConfigField>();
    public string? Description { get; init; }

    public bool Accepts(AddressKind kind) => AddressKinds.Contains(kind);
}

public static class DriverSdk
{
    public const string Version = "1.0";
}

/// <summary>驱动实例化所需的全部输入。驱动不得读取全局配置。</summary>
/// <param name="DataDir">
/// 数据目录。需要落本地文件（证书、密钥库）的协议用它，而不是去读进程的
/// 当前目录——那会让「同一份配置在两台机器上表现不同」变成常态。
/// 为空表示调用方没有提供，此时只有不落盘的驱动能用。
/// </param>
public sealed record OpenContext(
    Channel Channel,
    Device Device,
    ILogger Log,
    IMetrics Metrics,
    string? DataDir = null);

/// <summary>驱动可用的最小埋点接口。</summary>
public interface IMetrics
{
    void Counter(string name, double delta, IReadOnlyDictionary<string, string>? labels = null);
    void Observe(string name, double value, IReadOnlyDictionary<string, string>? labels = null);
}

/// <summary>一次批量读覆盖的点位集合。批量规划器的输出单位。</summary>
public sealed record ReadSpan(string Id, IReadOnlyList<PointRef> Points)
{
    public IEnumerable<string> PointIds => Points.Select(p => p.Id);
}

/// <summary>驱动读写所需的点位视图。</summary>
public sealed record PointRef(
    string Id,
    Address Address,
    DataType DataType,
    ByteOrder ByteOrder,
    int Length,
    int Count = 0)
{
    public static PointRef Of(Device device, Point point) => new(
        point.Id,
        point.Address,
        point.DataType,
        point.ByteOrder ?? device.ByteOrder,
        point.Length > 0 ? Math.Max(1, (point.Length + 1) / 2) : point.RegisterCount(),
        point.Count);

    public int ByteLength => Length * 2;

    /// <summary>元素个数：0 或 1 表示标量。</summary>
    public int Elements => Count > 1 ? Count : 1;

    public bool IsArray => Count > 1;
}

/// <summary>驱动层写入命令。与 API 层的写入请求分开，避免协议细节泄漏到接口层。</summary>
public sealed record WriteCommand(PointRef Point, DataValue Value, bool Confirm = false);

/// <summary>原始采样：驱动只负责「值 + 质量 + 设备时标」，缩放与死区在上层。</summary>
public sealed record RawSample(
    string PointId,
    DataValue? Value,
    Quality Quality,
    DateTimeOffset? DeviceTime,
    string? Error = null);

/// <summary>驱动入口。实现必须无状态或线程安全：同一驱动会被多台设备共享。</summary>
public interface IDriver
{
    DriverMeta Meta { get; }
    IConn Open(OpenContext context);
}

/// <summary>
/// 一条到设备/站点的连接。同一实例不会被并发调用——串行由调度器保证，
/// 因为协议会话有状态（事务号、半双工、PDU 协商）。
/// </summary>
public interface IConn : IAsyncDisposable
{
    Task ProbeAsync(CancellationToken ct);
    Task<IReadOnlyList<RawSample>> ReadAsync(IReadOnlyList<ReadSpan> spans, CancellationToken ct);
    Task<IReadOnlyList<WriteResult>> WriteAsync(IReadOnlyList<WriteCommand> commands, CancellationToken ct);
}

/// <summary>
/// 可选能力：推送式采集。协议会话主动把数据变化推上来，而不是等调度器轮询。
///
/// 单独一个接口而不是加进 <see cref="IConn"/>：轮询是默认且必须有的路径，
/// 推送只有部分协议有；把它们合成一个接口，所有驱动都得写一个空实现。
/// </summary>
public interface ISubscriptionConn : IConn
{
    /// <summary>
    /// 为这些点位建立监控项。回调可能来自协议栈自己的线程，实现方不得在回调里阻塞；
    /// 返回的句柄释放即取消订阅。
    /// </summary>
    Task<ISubscription> SubscribeAsync(
        IReadOnlyList<PointRef> points,
        Func<RawSample, ValueTask> onSample,
        CancellationToken ct);
}

/// <summary>一条活的订阅。</summary>
public interface ISubscription : IAsyncDisposable
{
    /// <summary>订阅建立时实际协商出的发布周期。诊断用。</summary>
    TimeSpan PublishingInterval { get; }
}

/// <summary>错误分类。上层据此决定「重试 / 熔断 / 立即失败」。</summary>
public enum ErrorKind
{
    Timeout,
    Connection,
    Protocol,
    Address,
    Permission,
    Unsupported,
    Canceled,
    Internal,
}

public sealed class DriverException : Exception
{
    public ErrorKind Kind { get; }
    public bool Temporary { get; }
    public string Protocol { get; }
    public string Op { get; }

    public DriverException(
        ErrorKind kind,
        string protocol,
        string op,
        string message,
        bool temporary = false,
        Exception? inner = null)
        : base($"{protocol}/{op} {kind}: {message}", inner)
    {
        Kind = kind;
        Protocol = protocol;
        Op = op;
        Temporary = temporary;
    }

    public static DriverException Timeout(string protocol, string op, string message, Exception? inner = null)
        => new(ErrorKind.Timeout, protocol, op, message, temporary: true, inner);

    public static DriverException Connection(string protocol, string op, string message, Exception? inner = null)
        => new(ErrorKind.Connection, protocol, op, message, temporary: true, inner);

    public static DriverException ProtocolError(string protocol, string op, string message, Exception? inner = null)
        => new(ErrorKind.Protocol, protocol, op, message, temporary: false, inner);

    public static DriverException Address(string protocol, string op, string message)
        => new(ErrorKind.Address, protocol, op, message, temporary: false);

    public static DriverException Unsupported(string protocol, string op, string message)
        => new(ErrorKind.Unsupported, protocol, op, message, temporary: false);

    public static DriverException Permission(string protocol, string op, string message)
        => new(ErrorKind.Permission, protocol, op, message, temporary: false);
}

/// <summary>错误 → 质量码的统一映射。所有驱动共用一个实现，避免各自造词。</summary>
public static class QualityMap
{
    public static Quality From(ErrorKind kind) => kind switch
    {
        ErrorKind.Timeout => Quality.Timeout,
        ErrorKind.Connection => Quality.CommError,
        ErrorKind.Address => Quality.AddressError,
        ErrorKind.Unsupported or ErrorKind.Permission => Quality.Disabled,
        ErrorKind.Canceled => Quality.NotConnected,
        _ => Quality.Bad,
    };
}
