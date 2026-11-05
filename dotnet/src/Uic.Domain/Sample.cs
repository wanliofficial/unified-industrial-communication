namespace Uic.Domain;

/// <summary>一次采集的结果：值 + 质量 + 三个时标 + 设备内单调序号。</summary>
public sealed record Sample(
    string DeviceId,
    string PointId,
    DataValue Value,
    Quality Quality,
    string? Unit,
    DateTimeOffset? DeviceTime,
    DateTimeOffset CollectedAt,
    ulong Seq,
    string? Error = null);

/// <summary>一次写入请求。</summary>
public sealed record WriteRequest(string PointId, DataValue Value, bool Confirm = false);

/// <summary>一次写入结果。逐项返回，部分失败不用整体 500。</summary>
public sealed record WriteResult(
    string PointId,
    bool Ok,
    Quality Quality,
    DateTimeOffset AppliedAt,
    string? Error = null);

/// <summary>设备运行统计。configuredState 与 runtimeState 必须分开返回。</summary>
public sealed record DeviceStats(
    long ReadsTotal,
    long ReadsFailed,
    double SuccessRate,
    double P95DurationMs,
    string? LastError,
    DateTimeOffset? LastSuccessAt,
    int Generation);

/// <summary>设备快照视图（API 用）。</summary>
public sealed record DeviceView(
    string Id,
    string? Name,
    string ChannelId,
    string Protocol,
    ConfiguredState ConfiguredState,
    RuntimeState RuntimeState,
    DateTimeOffset Since,
    int Generation,
    TimeSpan Interval,
    DeviceStats Stats,
    int PointsTotal,
    int PointsGood,
    int PointsBad);

/// <summary>点位当前值视图（API 用）。</summary>
public sealed record PointView(
    string DeviceId,
    string PointId,
    string? Name,
    DataValue Value,
    Quality Quality,
    string? Unit,
    DateTimeOffset? DeviceTime,
    DateTimeOffset CollectedAt,
    ulong Seq,
    bool Writable);

/// <summary>健康聚合。</summary>
public sealed record HealthView(
    string Status,
    long UptimeSeconds,
    int DevicesTotal,
    int DevicesCollecting,
    int DevicesDegraded,
    int DevicesFaulted,
    int PointsTotal,
    int PointsGood,
    int PointsStale,
    int PointsBad,
    long OutboxPending,
    long OutboxDroppedLastHour);

/// <summary>配置校验问题。字段路径 + 原因，直接可定位。</summary>
public sealed record ValidationProblem(string Path, string Message)
{
    public override string ToString() => $"{Path}: {Message}";
}

/// <summary>配置非法异常。带上全部问题，而不是只报第一条。</summary>
public sealed class ConfigException : Exception
{
    public IReadOnlyList<ValidationProblem> Problems { get; }

    public ConfigException(string message, IReadOnlyList<ValidationProblem> problems)
        : base(message)
    {
        Problems = problems;
    }

    public string ToDetailedString() =>
        string.Join(Environment.NewLine, Problems.Select(p => "  - " + p));
}
