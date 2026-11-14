using System.Text.Json;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Config;
using Uic.Domain;
using Uic.Engine;
using Uic.Northbound.Security;
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

    // 可换（而不是 readonly）只为测试：100 次/秒的读配额在本地回环上很难在 1 秒内
    // 确定性地打满，靠 sleep 等令牌恢复又会让用例变慢、还受机器负载影响。
    // 生产只有下面这两处构造点，测试结束后必须换回来。
    private static volatile RateLimiter ReadLimiter = new(capacity: 100, refillPerSecond: 100);
    private static volatile RateLimiter WriteLimiter = new(capacity: 10, refillPerSecond: 10);

    /// <summary>装配全部端点，并把 SSE 接到采集链路上。</summary>
    public static WebApplication UseNorthbound(this WebApplication app, NorthboundContext ctx)
    {
        var hub = new SseHub();
        ctx.Engine.Cache.Subscribe(hub.Publish);

        // 连接数只有 hub 自己知道，指标必须在这里注册：放在 GatewayRuntime 里
        // 只能读到 hub 不存在时的常量 0，运维会看到「一条流都没连」而实际上已连满。
        ctx.Metrics.Gauge("uic_sse_connections", () => hub.ConnectionCount,
            "active SSE streams");

        app.Use(async (http, next) =>
        {
            var requestId = http.Request.Headers["X-Request-Id"].FirstOrDefault();
            if (string.IsNullOrWhiteSpace(requestId)) requestId = "req-" + Guid.NewGuid().ToString("N")[..16];

            http.Response.Headers["X-Request-Id"] = requestId;
            http.Items["requestId"] = requestId;

            try
            {
                var path = http.Request.Path.Value ?? "/";

                if (!IsPublicPath(path))
                {
                    var principal = Authenticate(http, ctx);
                    if (principal is null)
                    {
                        AuditAuthFailure(http, ctx);
                        throw ApiException.Unauthorized();
                    }

                    http.Items["principal"] = principal;

                    // 首次登录必须先改密。除改密、看自己、退出之外一律 403，
                    // 否则「强制改密」只是一句提示，客户端换个端点就绕过去了。
                    if (principal.MustChangePassword && !IsPasswordChangePath(path))
                    {
                        AuditDenied(http, ctx, principal, "auth.password_change_required");
                        throw ApiException.Forbidden("auth.password_change_required",
                            "the initial password must be changed before using the API");
                    }

                    if (IsApiPath(path) && !IsAuthPath(path))
                    {
                        // 默认拒绝：端点没声明权限就是配置错误，不能当作「公开」。
                        var required = http.GetEndpoint()?.Metadata.GetMetadata<RequirePermissionAttribute>();

                        if (required is null || !principal.Can(required.Permission))
                        {
                            AuditDenied(http, ctx, principal, "auth.forbidden",
                                required?.Permission ?? "(undeclared)");
                            throw ApiException.Forbidden("auth.forbidden",
                                required is null
                                    ? "this endpoint has no permission declaration"
                                    : $"permission '{required.Permission}' is required");
                        }
                    }
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
        MapHistory(app, ctx);
        MapAudit(app, ctx);
        MapAuth(app, ctx);
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

    /// <summary>
    /// 无需认证的路径：存活/就绪/版本/指标/健康。这些端点是给探针与抓取器用的，
    /// 不含凭据，也不改变任何状态。登录本身当然不能要求先登录。
    ///
    /// 比较**必须**不区分大小写：ASP.NET Core 的路由匹配本身就不区分，
    /// <c>/API/v1/devices</c> 会命中同一个端点。这里若按 Ordinal 判定，大写变体
    /// 会被当成「非 /api/ 的静态资源」直接放行——一次大小写改写就是完整的鉴权绕过
    /// （这是扫描器试探的第一条路径）。
    /// </summary>
    private static bool IsPublicPath(string path) =>
        path.Equals("/healthz", StringComparison.OrdinalIgnoreCase) ||
        path.Equals("/readyz", StringComparison.OrdinalIgnoreCase) ||
        path.Equals("/api/v1/version", StringComparison.OrdinalIgnoreCase) ||
        path.Equals("/api/v1/metrics", StringComparison.OrdinalIgnoreCase) ||
        path.Equals("/api/v1/health", StringComparison.OrdinalIgnoreCase) ||
        path.Equals("/api/v1/auth/login", StringComparison.OrdinalIgnoreCase) ||
        !IsApiPath(path);

    /// <summary>
    /// 是否为需要鉴权/限流的 API 路径。与路由一致地忽略大小写，理由见 <see cref="IsPublicPath"/>。
    /// </summary>
    private static bool IsApiPath(string path)
        => path.StartsWith("/api/", StringComparison.OrdinalIgnoreCase);

    private static bool IsAuthPath(string path)
        => path.StartsWith("/api/v1/auth/", StringComparison.OrdinalIgnoreCase);

    private static bool IsPasswordChangePath(string path)
        => path.Equals("/api/v1/auth/me", StringComparison.OrdinalIgnoreCase) ||
           path.Equals("/api/v1/auth/password", StringComparison.OrdinalIgnoreCase) ||
           path.Equals("/api/v1/auth/logout", StringComparison.OrdinalIgnoreCase);

    /// <summary>
    /// 解析调用者。两条来源：配置里的静态 API Token（运维脚本，全权限、不过期）
    /// 与会话 Token（登录签发，12 小时）。没有「不配令牌就不鉴权」这条路径了：
    /// 账号库始终存在，未认证的请求一律 401。
    /// </summary>
    private static Principal? Authenticate(HttpContext http, NorthboundContext ctx)
    {
        var header = http.Request.Headers.Authorization.FirstOrDefault();
        if (string.IsNullOrWhiteSpace(header)) return null;
        if (!header.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase)) return null;

        var presented = header["Bearer ".Length..].Trim();
        if (presented.Length == 0) return null;

        if (!string.IsNullOrEmpty(ctx.Rest.Token) && CryptographicEquals(presented, ctx.Rest.Token))
        {
            return new Principal(
                "api-token",
                "API Token",
                Roles.Admin,
                Roles.ApiTokenPermissions,
                MustChangePassword: false,
                IsApiToken: true);
        }

        return ctx.Auth?.Resolve(presented, DateTimeOffset.UtcNow);
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
        if (!IsApiPath(path)) return true;

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

        // 报「最近一小时」而不是累计值：累计值只会变大，一次磁盘打满之后健康接口
        // 会永久显示正在丢弃，运维分不清「现在还在丢」和「早就恢复了」。
        var engineHealth = ctx.Engine.Health(stats.Pending, stats.DroppedLastHour);
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
            new OutboxHealthDto(stats.Pending, stats.DroppedLastHour),
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