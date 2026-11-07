using System.Text.Json;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Config;
using Uic.Domain;
using Uic.Engine;
using Uic.Obs;

namespace Uic.Northbound;

/// <summary>
/// 北向入口：中间件 + 系统级端点。
/// 设备/点位、配置、诊断分别在同名 partial 文件里，避免一个文件膨胀成「什么都改它」。
/// </summary>
public static partial class RestApi
{
    public const string ApiVersion = "1";
    public const string SchemaVersion = "1";

    private static readonly RateLimiter ReadLimiter = new(capacity: 100, refillPerSecond: 100);
    private static readonly RateLimiter WriteLimiter = new(capacity: 10, refillPerSecond: 10);

    /// <summary>写入幂等窗口：key → 首次响应。5 分钟足够覆盖「客户端超时重试」，又不会无限增长。</summary>
    private static readonly System.Collections.Concurrent.ConcurrentDictionary<string, IdempotentResponse> Idempotency =
        new(StringComparer.Ordinal);

    /// <summary>装配全部端点，并把 SSE 接到采集链路上。</summary>
    public static WebApplication UseNorthbound(this WebApplication app, NorthboundContext ctx)
    {
        var hub = new SseHub();
        ctx.Engine.Cache.Subscribe(hub.Publish);

        app.Use(async (http, next) =>
        {
            var requestId = http.Request.Headers["X-Request-Id"].FirstOrDefault();
            if (string.IsNullOrWhiteSpace(requestId)) requestId = "req-" + Guid.NewGuid().ToString("N")[..16];

            http.Response.Headers["X-Request-Id"] = requestId;
            http.Items["requestId"] = requestId;

            try
            {
                if (!Authorize(http, ctx))
                {
                    http.Response.Headers["WWW-Authenticate"] = "Bearer";
                    throw ApiException.Unauthorized();
                }

                if (!EnforceRateLimit(http))
                {
                    throw ApiException.TooManyRequests();
                }

                await next().ConfigureAwait(false);
            }
            catch (ApiException ex)
            {
                await WriteErrorAsync(http, ex.Status, ex.Code, ex.Message, ex.Details, requestId).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (http.RequestAborted.IsCancellationRequested)
            {
                // 客户端断开是正常路径，不写响应。
            }
            catch (ConfigException ex)
            {
                await WriteErrorAsync(http, 422, "config.invalid", ex.Message,
                    ex.Problems.Select(p => new { path = p.Path, message = p.Message }), requestId)
                    .ConfigureAwait(false);
            }
            catch (KeyNotFoundException ex)
            {
                await WriteErrorAsync(http, 404, "resource.not_found", ex.Message, null, requestId)
                    .ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                await WriteErrorAsync(http, 500, "internal.error", ex.Message, null, requestId)
                    .ConfigureAwait(false);
            }
        });

        app.MapGet("/healthz", () => Results.Text("ok", "text/plain"));

        app.MapGet("/readyz", () => Results.Json(Readiness(ctx), ApiJson.Options));

        app.MapGet("/api/v1/version", () => Results.Json(new VersionDto(
            "uic",
            ctx.GatewayVersion,
            ApiVersion,
            SchemaVersion,
            ctx.Site,
            Environment.Version.ToString(),
            ctx.StartedAt,
            ctx.Control.ConfigVersion), ApiJson.Options));

        app.MapGet("/api/v1/health", () => Results.Json(BuildHealth(ctx), ApiJson.Options));

        app.MapGet("/api/v1/metrics", () => Results.Text(
            ctx.Metrics.RenderPrometheus(), "text/plain; version=0.0.4; charset=utf-8"));

        MapDevices(app, ctx);
        MapDrivers(app, ctx);
        MapConfig(app, ctx);
        MapTemplates(app, ctx);
        MapImportExport(app, ctx);
        MapDiagnostics(app, ctx);
        MapStream(app, ctx, hub);

        return app;
    }

    // ------------------------------------------------------------------
    // 认证与限流
    // ------------------------------------------------------------------

    /// <summary>无需认证的路径：存活/就绪/版本/指标。指标之所以放开，是因为它通常由内网抓取器读取。</summary>
    private static bool IsPublicPath(string path) =>
        path is "/healthz" or "/readyz" or "/api/v1/version" or "/api/v1/metrics" ||
        !path.StartsWith("/api/", StringComparison.Ordinal);

    private static bool Authorize(HttpContext http, NorthboundContext ctx)
    {
        var path = http.Request.Path.Value ?? "/";
        if (IsPublicPath(path)) return true;

        var expected = ctx.Rest.Token;
        if (string.IsNullOrWhiteSpace(expected)) return true;

        var header = http.Request.Headers.Authorization.FirstOrDefault();
        if (string.IsNullOrWhiteSpace(header)) return false;
        if (!header.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase)) return false;

        var presented = header["Bearer ".Length..].Trim();
        return CryptographicEquals(presented, expected);
    }

    /// <summary>定长比较，避免用耗时差反推 token。</summary>
    private static bool CryptographicEquals(string a, string b)
    {
        var left = System.Text.Encoding.UTF8.GetBytes(a);
        var right = System.Text.Encoding.UTF8.GetBytes(b);

        if (left.Length != right.Length) return false;

        var diff = 0;
        for (var i = 0; i < left.Length; i++) diff |= left[i] ^ right[i];
        return diff == 0;
    }

    private static bool EnforceRateLimit(HttpContext http)
    {
        // 只对 /api/ 计配额。WASM 首屏会并行拉几百个程序集，
        // 若把静态资源也算进读配额，控制台会在加载阶段就被自己打回 429
        // （浏览器那边表现为一堆 ERR_NETWORK_CHANGED，很难看出真实原因）。
        var path = http.Request.Path.Value ?? "/";
        if (!path.StartsWith("/api/", StringComparison.Ordinal)) return true;

        var ip = http.Connection.RemoteIpAddress?.ToString() ?? "unknown";
        var write = HttpMethods.IsPost(http.Request.Method) || HttpMethods.IsPut(http.Request.Method) ||
                    HttpMethods.IsPatch(http.Request.Method) || HttpMethods.IsDelete(http.Request.Method);

        return write ? WriteLimiter.TryTake("w:" + ip) : ReadLimiter.TryTake("r:" + ip);
    }

    private static async Task WriteErrorAsync(
        HttpContext http,
        int status,
        string code,
        string message,
        object? details,
        string requestId)
    {
        if (http.Response.HasStarted) return;

        http.Response.Clear();
        http.Response.StatusCode = status;
        http.Response.ContentType = "application/json; charset=utf-8";

        // Clear() 会连 WWW-Authenticate 一起抹掉；401 必须带上它，
        // 否则标准客户端不知道应该用哪种凭据。
        if (status == 401) http.Response.Headers.WWWAuthenticate = "Bearer";

        var body = new ErrorEnvelope(new ErrorBody(code, message, requestId, details));
        await http.Response.WriteAsync(JsonSerializer.Serialize(body, ApiJson.Options)).ConfigureAwait(false);
    }

    // ------------------------------------------------------------------
    // 健康
    // ------------------------------------------------------------------

    private static ReadyDto Readiness(NorthboundContext ctx)
    {
        var checks = new List<DependencyCheck>();

        var config = ctx.Control.CurrentConfig;
        checks.Add(new DependencyCheck("config", config is not null,
            config is null ? "not loaded" : $"{config.Devices.Count} device(s)"));

        var storageOk = true;
        string storageDetail;
        try
        {
            var probe = Path.Combine(ctx.Store.Root, ".ready");
            File.WriteAllText(probe, DateTimeOffset.UtcNow.ToString("O"));
            File.Delete(probe);
            storageDetail = "writable";
        }
        catch (Exception ex)
        {
            storageOk = false;
            storageDetail = ex.Message;
        }
        checks.Add(new DependencyCheck("storage", storageOk, storageDetail));

        var actors = ctx.Engine.Actors;
        var collecting = actors.Count(a => a.State == RuntimeState.Collecting);
        checks.Add(new DependencyCheck("drivers", actors.Count > 0,
            $"{actors.Count} device(s), {collecting} collecting"));

        return new ReadyDto(checks.All(c => c.Ok), checks);
    }

    internal static HealthDto BuildHealth(NorthboundContext ctx)
    {
        var stats = ctx.Outbox.Stats();
        var engineHealth = ctx.Engine.Health(stats.Pending, stats.DroppedTotal);
        var resourceStatus = ctx.Resources?.Status;

        var problems = ctx.Engine.Actors
            .Where(a => a.State is RuntimeState.Faulted or RuntimeState.Degraded)
            .Select(a => new ProblemDto(
                a.DeviceId,
                JsonEnum.ToWire(a.State),
                a.Stats.LastError,
                a.ToView(0, 0).Since))
            .ToList();

        if (resourceStatus is { Degraded: true })
        {
            // 资源问题不是设备问题，但必须出现在同一张问题清单里：
            // 运维看见 degraded 却找不到原因，比不报还糟。
            problems.Add(new ProblemDto(
                "$resources",
                "degraded",
                resourceStatus.DiskLow
                    ? "free disk space is below the watermark; history and file logging are paused"
                    : "working set exceeds the configured memory limit",
                resourceStatus.Since));
        }

        // 故障优先于资源告警：设备已经 faulted 时再多一个 degraded 不会增加信息量。
        var status = engineHealth.Status;
        if (resourceStatus is { Degraded: true } && status == "ok") status = "degraded";

        return new HealthDto(
            status,
            engineHealth.UptimeSeconds,
            new DevicesHealthDto(
                engineHealth.DevicesTotal,
                engineHealth.DevicesCollecting,
                engineHealth.DevicesDegraded,
                engineHealth.DevicesFaulted),
            new PointsHealthDto(
                engineHealth.PointsTotal,
                engineHealth.PointsGood,
                engineHealth.PointsStale,
                engineHealth.PointsBad),
            new OutboxHealthDto(stats.Pending, stats.DroppedTotal),
            problems,
            resourceStatus is null
                ? null
                : new ResourcesHealthDto(
                    resourceStatus.Degraded ? "degraded" : "ok",
                    resourceStatus.DiskLow,
                    resourceStatus.MemoryHigh,
                    resourceStatus.Reading.FreeBytes,
                    resourceStatus.Reading.DiskTotalBytes,
                    resourceStatus.Reading.WorkingSetBytes,
                    resourceStatus.Since));
    }

    // ------------------------------------------------------------------
    // 共用小工具
    // ------------------------------------------------------------------

    internal static DeviceSummaryDto ToSummary(DeviceView v) => new(
        v.Id,
        v.Name,
        v.ChannelId,
        v.Protocol,
        JsonEnum.ToWire(v.ConfiguredState),
        JsonEnum.ToWire(v.RuntimeState),
        v.Since,
        v.Generation,
        v.Interval,
        new DeviceStatsDto(
            v.Stats.ReadsTotal,
            v.Stats.ReadsFailed,
            v.Stats.SuccessRate,
            v.Stats.P95DurationMs,
            v.Stats.LastError,
            v.Stats.LastSuccessAt),
        new PointsSummaryDto(v.PointsTotal, v.PointsGood, v.PointsBad));

    private static int ClampLimit(int limit, int fallback = 100, int max = 1000)
        => limit <= 0 ? fallback : Math.Min(limit, max);

    private static T? ReadJson<T>(string json) where T : class
    {
        try
        {
            return JsonSerializer.Deserialize<T>(json, JsonDefaults.Options);
        }
        catch (JsonException ex)
        {
            throw ApiException.BadRequest("request.invalid_json", ex.Message);
        }
    }
}