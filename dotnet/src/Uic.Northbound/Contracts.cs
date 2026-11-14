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
    AuthService? Auth = null,
    IdempotencyCache? Idempotency = null)
{
    /// <summary>
    /// 写入幂等台账。刻意不做成 <c>RestApi</c> 上的 <c>static</c> 字段：
    /// 台账里记的是「首次写入时刻的单调时刻」，而这个时刻只能与**同一个**单调时钟
    /// 相减。进程级静态字典会让第二个运行时读到第一个运行时的条目，
    /// 拿两套时钟做减法（差值为负）后 `elapsed &lt; window` 恒成立，
    /// 重投永远被判成重放，返回的还是另一套配置、另一批设备的首次响应。
    /// 由运行时持有并传进来，保证「台账」与「时钟」一一对应。
    /// </summary>
    public IdempotencyCache Writes { get; } = Idempotency ?? new IdempotencyCache();
}
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

/// <summary>
/// 写入幂等台账：key → 首次响应，窗口 5 分钟。
///
/// 为什么是一个有实例的对象而不是 <c>RestApi</c> 上的静态字典：条目里的
/// <see cref="IdempotentResponse.MonoAt"/> 只在**产出它的那个单调时钟**上可解释。
/// 静态字典让同一进程里的第二个运行时读到第一个运行时的条目，两个时钟的读数
/// 相减是负数，`elapsed &lt; window` 永远成立——重投被判成重放，返回的还是另一套
/// 配置、另一批设备的首次响应，而设备上一次都没被写过。现场表现为「换了配置之后
/// 点写按钮没反应」，日志、审计、接口返回值全都正常。
///
/// 台账随运行时一起生死，与它记的时钟同源，这个错误就无从发生。
/// </summary>
public sealed class IdempotencyCache
{
    /// <summary>键的数量由调用方决定，必须有上限；超过就清掉过窗的条目。</summary>
    private const int MaxEntries = 1024;

    private readonly System.Collections.Concurrent.ConcurrentDictionary<string, IdempotentResponse> _entries =
        new(StringComparer.Ordinal);

    internal int Count => _entries.Count;

    internal bool TryGet(string key, long monoNow, long window, out IdempotentResponse cached)
    {
        if (_entries.TryGetValue(key, out var entry) && monoNow - entry.MonoAt < window)
        {
            cached = entry;
            return true;
        }

        cached = null!;
        return false;
    }

    internal void Store(string key, string body, long monoNow, string fingerprint)
    {
        _entries[key] = new IdempotentResponse(body, monoNow, fingerprint);

        if (_entries.Count > MaxEntries) Prune(monoNow);
    }

    private void Prune(long monoNow)
    {
        foreach (var (key, entry) in _entries)
        {
            if (monoNow - entry.MonoAt >= IdempotencyWindowTicks) _entries.TryRemove(key, out _);
        }
    }

    /// <summary>幂等窗口长度。5 分钟足够覆盖客户端超时重试，又不会让缓存无限增长。</summary>
    internal static readonly long IdempotencyWindowTicks = TimeSpan.FromMinutes(5).Ticks;
}