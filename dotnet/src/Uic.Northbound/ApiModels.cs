using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Json.Serialization;
using Uic.Domain;

namespace Uic.Northbound;

/// <summary>
/// 线上契约的投影。
///
/// 内部 <see cref="Sample"/> 用 DeviceId/PointId，契约要求 device/point；
/// 这类差异必须集中在一处，散落在各端点里迟早出现两套字段名。
/// </summary>
public sealed record SampleDto(
    [property: JsonPropertyName("device")] string Device,
    [property: JsonPropertyName("point")] string Point,
    [property: JsonPropertyName("value")] DataValue Value,
    [property: JsonPropertyName("quality")] string Quality,
    [property: JsonPropertyName("unit")] string? Unit,
    [property: JsonPropertyName("deviceTime")] DateTimeOffset? DeviceTime,
    [property: JsonPropertyName("collectedAt")] DateTimeOffset CollectedAt,
    [property: JsonPropertyName("seq")] ulong Seq,
    [property: JsonPropertyName("error")] string? Error)
{
    public static SampleDto From(Sample s) => new(
        s.DeviceId,
        s.PointId,
        s.Value,
        JsonEnum.ToWire(s.Quality),
        s.Unit,
        s.DeviceTime,
        s.CollectedAt,
        s.Seq,
        s.Error);
}

public sealed record PagedResponse<T>(IReadOnlyList<T> Items, string? NextCursor, int Total);

public sealed record ErrorBody(string Code, string Message, string? RequestId, object? Details);

public sealed record ErrorEnvelope(ErrorBody Error);

public sealed record DeviceStatsDto(
    long ReadsTotal,
    long ReadsFailed,
    double SuccessRate,
    double P95DurationMs,
    string? LastError,
    DateTimeOffset? LastSuccessAt);

public sealed record DeviceSummaryDto(
    string Id,
    string? Name,
    string Channel,
    string Protocol,
    string ConfiguredState,
    string RuntimeState,
    DateTimeOffset Since,
    int Generation,
    TimeSpan Interval,
    DeviceStatsDto Stats,
    PointsSummaryDto Points);

public sealed record PointsSummaryDto(int Total, int Good, int Bad);

public sealed record HealthDto(
    string Status,
    long UptimeSeconds,
    DevicesHealthDto Devices,
    PointsHealthDto Points,
    OutboxHealthDto Outbox,
    IReadOnlyList<ProblemDto> Problems,
    ResourcesHealthDto? Resources = null);

/// <summary>资源水位。两个读取值都可能为 null：平台不提供磁盘信息时不能谎报成 0。</summary>
public sealed record ResourcesHealthDto(
    string Status,
    bool DiskLow,
    bool MemoryHigh,
    long? FreeBytes,
    long? DiskTotalBytes,
    long? WorkingSetBytes,
    DateTimeOffset Since);

public sealed record DevicesHealthDto(int Total, int Collecting, int Degraded, int Faulted);

public sealed record PointsHealthDto(int Total, int Good, int Stale, int Bad);

public sealed record OutboxHealthDto(long Pending, long DroppedLastHour);

public sealed record ProblemDto(string Device, string State, string? Reason, DateTimeOffset Since);

public sealed record ReadyDto(bool Ready, IReadOnlyList<DependencyCheck> Checks);

public sealed record VersionDto(
    string Name,
    string Version,
    string ApiVersion,
    string SchemaVersion,
    string Site,
    string Runtime,
    DateTimeOffset StartedAt,
    string ConfigVersion);

public sealed record WriteItemDto(string Point, DataValue Value, bool Confirm = false);

public sealed record WriteBatchDto(IReadOnlyList<WriteItemDto> Writes);

public sealed record WriteResultDto(
    string Point,
    bool Ok,
    string Quality,
    DateTimeOffset AppliedAt,
    string? Error = null,
    bool? Verified = null);

public sealed record WriteResponseDto(IReadOnlyList<WriteResultDto> Results);

public sealed record ValidateRequestDto(string? Json, ConfigDocument? Config);

public sealed record ValidateResponseDto(bool Valid, IReadOnlyList<string> Problems);

public sealed record ApplyRequestDto(ConfigDocument? Config, string? Json, string? Comment, bool Confirm = true);

public sealed record ApplyResponseDto(
    string Version,
    bool Applied,
    ChangedCountsDto Changed,
    IReadOnlyList<string> RestartedDevices,
    long DurationMs);

public sealed record ChangedCountsDto(
    int DevicesAdded,
    int DevicesModified,
    int DevicesRemoved,
    int PointsAdded,
    int PointsModified,
    int PointsRemoved);

public sealed record ConfigVersionDto(string Id, DateTimeOffset CreatedAt, string? Comment, int Size, bool Current);

public sealed record DiffEntryDto(string Path, string Change, string? Before, string? After);

public sealed record DiffResponseDto(string From, string To, IReadOnlyList<DiffEntryDto> Entries);

public sealed record RollbackRequestDto(string Version, bool Confirm = true);

public sealed record CommandRequestDto(string Action, Dictionary<string, string>? Params, bool Confirm = false);

public sealed record CommandResponseDto(string Action, bool Ok, string? Detail);

public sealed record LogEntryDto(
    long Seq,
    DateTimeOffset At,
    string Level,
    string Category,
    string Message,
    string? Device,
    string? Exception);

public sealed record TemplateSummaryDto(
    string Id,
    string? Name,
    string? Description,
    string? Protocol,
    IReadOnlyList<TemplateParameter> Parameters,
    string? DeviceId);

public sealed record InstantiateRequestDto(
    IReadOnlyList<Dictionary<string, string?>>? Instances,
    bool Apply,
    string? Comment);

public sealed record InstantiateResponseDto(
    string TemplateId,
    bool Applied,
    string? Version,
    IReadOnlyList<Device> Devices,
    IReadOnlyList<ValidationProblem> Problems);

public sealed record PointsImportRequestDto(string? Csv, bool? DryRun = null, string? Comment = null);

public sealed record PointsImportResponseDto(
    string Device,
    bool Valid,
    bool Applied,
    string? Version,
    IReadOnlyList<string> Added,
    IReadOnlyList<string> Updated,
    IReadOnlyList<Point> Points,
    IReadOnlyList<ValidationProblem> Problems);

public sealed record ConfigExportDto(
    string GatewayVersion,
    DateTimeOffset ExportedAt,
    string ConfigVersion,
    bool Redacted,
    JsonNode? Config);

public sealed record ConfigImportResponseDto(
    bool Valid,
    bool Applied,
    string? Version,
    IReadOnlyList<string> Migrations,
    JsonNode? Config,
    IReadOnlyList<ValidationProblem> Problems);

/// <summary>契约序列化选项：与 <see cref="JsonDefaults"/> 一致，额外忽略 null。</summary>
public static class ApiJson
{
    public static readonly JsonSerializerOptions Options = JsonDefaults.Options;

    public static string Serialize<T>(T value) => JsonSerializer.Serialize(value, Options);
}