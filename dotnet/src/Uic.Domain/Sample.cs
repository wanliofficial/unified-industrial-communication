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
    string? Error = null,
    /// <summary>
    /// 采集发生的单调时刻，已归一化到 <see cref="TimeSpan.Ticks"/>（<c>null</c> 表示未知）。
    /// 变化率死区要用它算 Δt：墙钟会被 NTP 校时拨动，用 <see cref="CollectedAt"/> 算速率
    /// 会让一次校时凭空造出尖峰或把尖峰抹平。
    /// 归一化到固定单位而不是存原始 <c>GetTimestamp()</c>，是因为时间戳的频率由具体
    /// <see cref="TimeProvider"/> 决定（注入的可拨时钟频率可以和系统不同），下游若按系统频率
    /// 换算就会算错 Δt。为 null 时判定回退到墙钟（见 Pipeline.ExceedsRate）。
    /// </summary>
    long? MonoAt = null);

/// <summary>一次写入请求。</summary>
public sealed record WriteRequest(string PointId, DataValue Value, bool Confirm = false);

/// <summary>一次写入结果。逐项返回，部分失败不用整体 500。</summary>
public sealed record WriteResult(
    string PointId,
    bool Ok,
    Quality Quality,
    DateTimeOffset AppliedAt,
    string? Error = null,
    bool? Verified = null);

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
