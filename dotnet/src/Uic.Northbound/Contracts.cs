using Uic.Config;
using Uic.Domain;
using Uic.Engine;
using Uic.Northbound.Security;
using Uic.Obs;
using OutboxQueue = Uic.Outbox.Outbox;

namespace Uic.Northbound;

/// <summary>
/// 北向需要的控制面能力。由运行时实现，北向只依赖这个接口——
/// 这样 HTTP 层不直接持有调度器内部状态，命令语义集中在一处。
/// </summary>
public interface IGatewayControl
{
    ConfigDocument CurrentConfig { get; }
    string ConfigVersion { get; }

    Task<ApplyOutcome> ApplyConfigAsync(ConfigDocument doc, string? comment, CancellationToken ct);
    Task<ApplyOutcome> RollbackAsync(string versionId, CancellationToken ct);
    Task RestartDeviceAsync(string deviceId, CancellationToken ct);
    Task<bool> SelfTestAsync(string deviceId, CancellationToken ct);
    Task FlushOutboxAsync(string? sink, CancellationToken ct);

    /// <summary>生成诊断包并返回落盘路径。构建逻辑与 HTTP 下载共用同一份实现。</summary>
    Task<string> GenerateDiagnosticsAsync(CancellationToken ct);
    void SetLogLevel(string level);
}

public sealed record ApplyOutcome(
    string Version,
    bool Applied,
    IReadOnlyList<string> RestartedDevices,
    IReadOnlyList<string> AddedDevices,
    IReadOnlyList<string> RemovedDevices,
    long DurationMs,
    string? Comment);

/// <summary>北向依赖集合。显式传入，避免全局服务定位器。</summary>
public sealed record NorthboundContext(
    Scheduler Engine,
    ConfigStore Store,
    OutboxQueue Outbox,
    MetricsRegistry Metrics,
    RingBufferLogProvider Logs,
    IGatewayControl Control,
    RestOptions Rest,
    string Site,
    string GatewayVersion,
    DateTimeOffset StartedAt,
    IResourceStatusSource? Resources = null,
    JournalHistory? History = null,
    AuditLog? Audit = null,
    AuthService? Auth = null);

/// <summary>业务错误。统一由中间件渲染成契约里的 error 信封。</summary>
public sealed class ApiException : Exception
{
    public int Status { get; }
    public string Code { get; }
    public object? Details { get; }

    public ApiException(int status, string code, string message, object? details = null, Exception? inner = null)
        : base(message, inner)
    {
        Status = status;
        Code = code;
        Details = details;
    }

    public static ApiException BadRequest(string code, string message, object? details = null)
        => new(400, code, message, details);

    public static ApiException Unauthorized(string message = "missing or invalid bearer token")
        => new(401, "auth.unauthorized", message);

    public static ApiException Forbidden(string code, string message) => new(403, code, message);

    public static ApiException NotFound(string code, string message, object? details = null)
        => new(404, code, message, details);

    public static ApiException Conflict(string code, string message) => new(409, code, message);

    public static ApiException Unprocessable(string code, string message, object? details = null)
        => new(422, code, message, details);

    public static ApiException NotReady(string message) => new(503, "service.not_ready", message);

    public static ApiException TooManyRequests(string message = "rate limit exceeded")
        => new(429, "request.rate_limited", message);
}

/// <summary>健康检查里的一项依赖。</summary>
public sealed record DependencyCheck(string Name, bool Ok, string? Detail = null);

/// <summary>
/// 写入幂等结果：同一 key 在窗口内重复提交返回首次结果。
/// <para>
/// <c>MonoAt</c> 是写入时刻的单调时刻（归一化到 ticks），不是墙钟：窗口问的是
/// 「过了多久」，一次校时跳变不能让重投的写请求被当成新请求、对设备重复执行。
/// </para>
/// <para>
/// <c>Fingerprint</c> 是首次请求体的规范化 JSON：同一个 key 配上另一份请求体时
/// 必须报冲突，不能把首次结果重放给一个从未执行过的写入。
/// </para>
/// </summary>
public sealed record IdempotentResponse(string Body, long MonoAt, string Fingerprint);