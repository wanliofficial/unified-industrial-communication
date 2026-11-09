using System.Text;
using System.Text.Json;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Domain;
using Uic.Driver;
using Uic.Northbound.Security;

namespace Uic.Northbound;

public static partial class RestApi
{
    private static void MapDevices(WebApplication app, NorthboundContext ctx)
    {
        app.MapGet("/api/v1/devices", (HttpContext http) =>
        {
            var q = http.Request.Query["q"].FirstOrDefault();
            var limit = ClampLimit(ParseInt(http.Request.Query["limit"], 100));
            var offset = ParseCursor(http.Request.Query["cursor"]);

            var all = ctx.Engine.DeviceViews()
                .Where(v => string.IsNullOrWhiteSpace(q) ||
                            v.Id.Contains(q, StringComparison.OrdinalIgnoreCase) ||
                            (v.Name?.Contains(q, StringComparison.OrdinalIgnoreCase) ?? false))
                .Select(ToSummary)
                .ToList();

            var page = all.Skip(offset).Take(limit).ToList();
            var next = offset + page.Count < all.Count ? EncodeCursor(offset + page.Count) : null;

            return Results.Json(new PagedResponse<DeviceSummaryDto>(page, next, all.Count), ApiJson.Options);
        }).RequirePermission(Permissions.SystemRead);

        app.MapGet("/api/v1/devices/{device}", (string device) =>
        {
            var view = ctx.Engine.DeviceViews().FirstOrDefault(v => v.Id == device)
                       ?? throw ApiException.NotFound("device.not_found", $"device '{device}' not found",
                           new { device });

            return Results.Json(ToSummary(view), ApiJson.Options);
        }).RequirePermission(Permissions.SystemRead);

        app.MapGet("/api/v1/devices/{device}/points", (HttpContext http, string device) =>
        {
            var config = ctx.Control.CurrentConfig
                         ?? throw ApiException.NotReady("configuration is not loaded");

            var dev = config.Devices.FirstOrDefault(d => d.Id == device)
                      ?? throw ApiException.NotFound("device.not_found", $"device '{device}' not found",
                          new { device });

            var quality = http.Request.Query["quality"].FirstOrDefault();
            var q = http.Request.Query["q"].FirstOrDefault();
            var limit = ClampLimit(ParseInt(http.Request.Query["limit"], 100));
            var offset = ParseCursor(http.Request.Query["cursor"]);

            var views = dev.Points
                .Where(p => string.IsNullOrWhiteSpace(q) || p.Id.Contains(q, StringComparison.OrdinalIgnoreCase))
                .Select(p => ToPointView(ctx, dev, p))
                .Where(p => string.IsNullOrWhiteSpace(quality) ||
                            string.Equals(p.Quality, quality, StringComparison.OrdinalIgnoreCase))
                .ToList();

            var page = views.Skip(offset).Take(limit).ToList();
            var next = offset + page.Count < views.Count ? EncodeCursor(offset + page.Count) : null;

            return Results.Json(new PagedResponse<PointDto>(page, next, views.Count), ApiJson.Options);
        }).RequirePermission(Permissions.SystemRead);

        app.MapGet("/api/v1/points", (HttpContext http) =>
        {
            var config = ctx.Control.CurrentConfig
                         ?? throw ApiException.NotReady("configuration is not loaded");

            var deviceFilter = http.Request.Query["device"].FirstOrDefault();
            var quality = http.Request.Query["quality"].FirstOrDefault();
            var q = http.Request.Query["q"].FirstOrDefault();
            var limit = ClampLimit(ParseInt(http.Request.Query["limit"], 100));
            var offset = ParseCursor(http.Request.Query["cursor"]);

            var points = new List<PointDto>();

            foreach (var dev in config.Devices)
            {
                if (!string.IsNullOrWhiteSpace(deviceFilter) && dev.Id != deviceFilter) continue;

                foreach (var p in dev.Points)
                {
                    if (!string.IsNullOrWhiteSpace(q) && !p.Id.Contains(q, StringComparison.OrdinalIgnoreCase)) continue;

                    var view = ToPointView(ctx, dev, p);
                    if (!string.IsNullOrWhiteSpace(quality) &&
                        !string.Equals(view.Quality, quality, StringComparison.OrdinalIgnoreCase)) continue;

                    points.Add(view);
                }
            }

            var page = points.Skip(offset).Take(limit).ToList();
            var next = offset + page.Count < points.Count ? EncodeCursor(offset + page.Count) : null;

            return Results.Json(new PagedResponse<PointDto>(page, next, points.Count), ApiJson.Options);
        }).RequirePermission(Permissions.SystemRead);

        app.MapGet("/api/v1/points/{device}/{point}", (string device, string point) =>
        {
            var (dev, cfgPoint) = FindPoint(ctx, device, point);
            return Results.Json(ToPointView(ctx, dev, cfgPoint), ApiJson.Options);
        }).RequirePermission(Permissions.SystemRead);

        app.MapPost("/api/v1/points/{device}/{point}/write",
            async (HttpContext http, string device, string point, CancellationToken ct) =>
        {
            var request = await ReadWriteBodyAsync(http, point, ct).ConfigureAwait(false);

            return await WriteWithIdempotencyAsync(http, ctx, device, $"point:{point}", request.Writes, ct)
                .ConfigureAwait(false);
        }).RequirePermission(Permissions.PointWrite);

        app.MapPost("/api/v1/points/{device}/write",
            async (HttpContext http, string device, CancellationToken ct) =>
        {
            var request = await ReadWriteBodyAsync(http, singlePoint: null, ct).ConfigureAwait(false);

            return await WriteWithIdempotencyAsync(http, ctx, device, "batch", request.Writes, ct)
                .ConfigureAwait(false);
        }).RequirePermission(Permissions.PointWrite);
    }

    /// <summary>
    /// 写请求的幂等外壳。单点与批量共用一份实现：只给批量加幂等键的接口，
    /// 会让调用方误以为单点重投是安全的，而现场恰恰是单点重投最多。
    /// </summary>
    private static async Task<IResult> WriteWithIdempotencyAsync(
        HttpContext http,
        NorthboundContext ctx,
        string device,
        string scope,
        IReadOnlyList<WriteItemDto> items,
        CancellationToken ct)
    {
        var key = http.Request.Headers["X-Idempotency-Key"].FirstOrDefault();
        if (string.IsNullOrWhiteSpace(key))
        {
            var response = await ExecuteWritesAsync(http, ctx, device, items, ct).ConfigureAwait(false);
            return Results.Json(response, ApiJson.Options);
        }

        var cacheKey = device + "\u001f" + scope + "\u001f" + key;
        var now = DateTimeOffset.UtcNow;

        if (Idempotency.TryGetValue(cacheKey, out var cached) &&
            now - cached.At < TimeSpan.FromMinutes(5))
        {
            http.Response.Headers["X-Idempotent-Replay"] = "true";
            return Results.Content(cached.Body, "application/json; charset=utf-8");
        }

        var fresh = await ExecuteWritesAsync(http, ctx, device, items, ct).ConfigureAwait(false);
        var body = JsonSerializer.Serialize(fresh, ApiJson.Options);
        Idempotency[cacheKey] = new IdempotentResponse(body, now);

        // 键的数量由调用方决定，长期运行的网关必须有上限；超过就把过窗的条目清掉。
        if (Idempotency.Count > 1024) PruneIdempotency(now);

        http.Response.Headers["X-Idempotent-Replay"] = "false";
        return Results.Content(body, "application/json; charset=utf-8");
    }

    private static void PruneIdempotency(DateTimeOffset now)
    {
        foreach (var (key, entry) in Idempotency)
        {
            if (now - entry.At >= TimeSpan.FromMinutes(5)) Idempotency.TryRemove(key, out _);
        }
    }

    // ------------------------------------------------------------------
    // 写入
    // ------------------------------------------------------------------

    private sealed record WriteEnvelope(IReadOnlyList<WriteItemDto> Writes);

    private static async Task<WriteEnvelope> ReadWriteBodyAsync(HttpContext http, string? singlePoint, CancellationToken ct)
    {
        string body;
        using (var reader = new StreamReader(http.Request.Body, Encoding.UTF8))
        {
            body = await reader.ReadToEndAsync(ct).ConfigureAwait(false);
        }

        if (string.IsNullOrWhiteSpace(body))
            throw ApiException.BadRequest("request.empty_body", "request body is required");

        using var doc = JsonDocument.Parse(body);
        var root = doc.RootElement;

        var items = new List<WriteItemDto>();

        // 单点写法：{ "value": {...}, "confirm": true }
        if (root.TryGetProperty("value", out var value))
        {
            if (singlePoint is null)
                throw ApiException.BadRequest("request.invalid", "single-value body is only valid on the single-point endpoint");

            var confirm = root.TryGetProperty("confirm", out var c) && c.ValueKind == JsonValueKind.True;
            var parsed = JsonSerializer.Deserialize<DataValue>(value.GetRawText(), JsonDefaults.Options)
                         ?? throw ApiException.BadRequest("request.invalid_value", "value could not be parsed");

            items.Add(new WriteItemDto(singlePoint, parsed, confirm));
        }

        if (root.TryGetProperty("writes", out var writes) && writes.ValueKind == JsonValueKind.Array)
        {
            var parsed = JsonSerializer.Deserialize<List<WriteItemDto>>(writes.GetRawText(), JsonDefaults.Options)
                         ?? new List<WriteItemDto>();
            items.AddRange(parsed);
        }

        if (items.Count == 0)
            throw ApiException.BadRequest("request.invalid", "no writes in request");

        if (items.Count > 100)
            throw ApiException.BadRequest("request.too_many_writes", "at most 100 writes per request");

        return new WriteEnvelope(items);
    }

    private static async Task<WriteResponseDto> ExecuteWritesAsync(
        HttpContext http,
        NorthboundContext ctx,
        string deviceId,
        IReadOnlyList<WriteItemDto> items,
        CancellationToken ct)
    {
        // 旧值必须在写之前取：写入后缓存立刻变成新值，事后回读只能得到「旧值=新值」。
        var before = new Dictionary<string, DataValue?>(StringComparer.Ordinal);
        foreach (var item in items)
            before[item.Point] = ctx.Engine.Cache.Get(deviceId, item.Point)?.Value;

        // 请求级失败（设备不存在、配置未就绪）不会产生逐条结果，但同样是「有人试图写」。
        // 这类记录往往比成功记录更能解释现场问题，不能因为异常路径就跳过。
        WriteResponseDto response;
        try
        {
            response = await ExecuteWritesCoreAsync(ctx, deviceId, items, ct).ConfigureAwait(false);
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            WriteAudit(ctx, http, "point.write", "failed", target: deviceId,
                detail: new
                {
                    device = deviceId,
                    points = items.Select(i => i.Point).ToArray(),
                    error = ex.Message,
                });

            throw;
        }

        // 逐条留痕：写入是不可逆动作，「谁在什么时候把哪个点位改成了什么」必须能回放。
        // 被拒绝的写入（点位不存在 / 不可写）也记，它们往往是配置错误的第一现场。
        foreach (var result in response.Results)
        {
            var item = items.FirstOrDefault(i => string.Equals(i.Point, result.Point, StringComparison.Ordinal));
            before.TryGetValue(result.Point, out var old);

            WriteAudit(ctx, http, "point.write", result.Ok ? "ok" : "failed",
                target: $"{deviceId}/{result.Point}",
                detail: new
                {
                    device = deviceId,
                    point = result.Point,
                    value = item?.Value,
                    oldValue = old,
                    confirm = item?.Confirm ?? false,
                    result.Quality,
                    result.Error,
                    result.Verified,
                });
        }

        return response;
    }

    private static async Task<WriteResponseDto> ExecuteWritesCoreAsync(
        NorthboundContext ctx,
        string deviceId,
        IReadOnlyList<WriteItemDto> items,
        CancellationToken ct)
    {
        var config = ctx.Control.CurrentConfig
                     ?? throw ApiException.NotReady("configuration is not loaded");

        var device = config.Devices.FirstOrDefault(d => d.Id == deviceId)
                     ?? throw ApiException.NotFound("device.not_found", $"device '{deviceId}' not found",
                         new { device = deviceId });

        var results = new List<WriteResultDto>();
        var commands = new List<WriteCommand>();
        var pending = new List<(WriteItemDto Item, Point Point)>();

        foreach (var item in items)
        {
            var point = device.Points.FirstOrDefault(p => p.Id == item.Point);

            if (point is null)
            {
                results.Add(new WriteResultDto(item.Point, false, "bad",
                    DateTimeOffset.UtcNow, $"point.not_found: '{item.Point}' is not configured"));
                continue;
            }

            if (!point.Writable)
            {
                results.Add(new WriteResultDto(item.Point, false, "disabled",
                    DateTimeOffset.UtcNow, "point.not_writable"));
                continue;
            }

            if (item.Value.Type != point.DataType)
            {
                results.Add(new WriteResultDto(item.Point, false, "bad",
                    DateTimeOffset.UtcNow,
                    $"value.type_mismatch: configured {JsonEnum.ToWire(point.DataType)}, got {JsonEnum.ToWire(item.Value.Type)}"));
                continue;
            }

            commands.Add(new WriteCommand(PointRef.Of(device, point), item.Value, item.Confirm));
            pending.Add((item, point));
        }

        if (commands.Count == 0) return new WriteResponseDto(results);

        IReadOnlyList<Domain.WriteResult> outcomes;
        try
        {
            outcomes = await ctx.Engine.WriteAsync(deviceId, commands, ct).ConfigureAwait(false);
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            foreach (var (item, _) in pending)
            {
                results.Add(new WriteResultDto(item.Point, false, "comm_error", DateTimeOffset.UtcNow, ex.Message));
            }
            return new WriteResponseDto(results);
        }

        foreach (var outcome in outcomes)
        {
            var matched = pending.FirstOrDefault(p => p.Item.Point == outcome.PointId);

            if (!outcome.Ok)
            {
                results.Add(new WriteResultDto(outcome.PointId, false,
                    JsonEnum.ToWire(outcome.Quality), outcome.AppliedAt, outcome.Error, outcome.Verified));
                continue;
            }

            if (matched.Item.Confirm)
            {
                // 驱动已做写后回读：有结论就直接用，不再等下一轮采集。
                if (outcome.Verified is true)
                {
                    results.Add(new WriteResultDto(
                        outcome.PointId, true, "good", outcome.AppliedAt, null, true));
                    continue;
                }

                if (outcome.Verified is false)
                {
                    results.Add(new WriteResultDto(
                        outcome.PointId, false, JsonEnum.ToWire(outcome.Quality), outcome.AppliedAt,
                        outcome.Error, false));
                    continue;
                }

                // 兼容没有实现回读的驱动：退回「等缓存出现期望值」的旧路径。
                var confirmed = await ConfirmAsync(ctx, deviceId, matched.Point, matched.Item.Value, ct)
                    .ConfigureAwait(false);

                results.Add(new WriteResultDto(
                    outcome.PointId,
                    confirmed,
                    confirmed ? "good" : "uncertain",
                    outcome.AppliedAt,
                    confirmed ? null : "confirm: readback does not match requested value",
                    confirmed));
                continue;
            }

            results.Add(new WriteResultDto(outcome.PointId, true, "good", outcome.AppliedAt));
        }

        return new WriteResponseDto(results);
    }

    /// <summary>
    /// confirm=true 时回读一次。窗口取 min(2×点位有效周期, 2s)：
    /// 太短会误判失败，太长会让接口在慢设备上失去意义。
    /// 用点位周期而不是设备周期：点位周期覆盖之后，快变量等设备周期会白等，
    /// 慢变量按设备周期等则会误判。
    /// </summary>
    private static async Task<bool> ConfirmAsync(
        NorthboundContext ctx,
        string deviceId,
        Point point,
        DataValue expected,
        CancellationToken ct)
    {
        var actor = ctx.Engine.Find(deviceId);
        var cadence = actor is null ? TimeSpan.FromMilliseconds(250) : point.EffectiveInterval(actor.Device);
        var window = TimeSpan.FromMilliseconds(Math.Min(2000, Math.Max(300, cadence.TotalMilliseconds * 2)));
        var deadline = DateTimeOffset.UtcNow + window;

        while (DateTimeOffset.UtcNow < deadline)
        {
            var sample = ctx.Engine.Cache.Get(deviceId, point.Id);

            if (sample is not null && sample.Quality == Quality.Good && ValuesEqual(sample.Value, expected))
                return true;

            await Task.Delay(50, ct).ConfigureAwait(false);
        }

        return false;
    }

    private static bool ValuesEqual(DataValue a, DataValue b) => a.Matches(b);

    // ------------------------------------------------------------------
    // 视图映射
    // ------------------------------------------------------------------

    private static (Device Device, Point Point) FindPoint(NorthboundContext ctx, string deviceId, string pointId)
    {
        var config = ctx.Control.CurrentConfig
                     ?? throw ApiException.NotReady("configuration is not loaded");

        var device = config.Devices.FirstOrDefault(d => d.Id == deviceId)
                     ?? throw ApiException.NotFound("device.not_found", $"device '{deviceId}' not found",
                         new { device = deviceId });

        var point = device.Points.FirstOrDefault(p => p.Id == pointId)
                    ?? throw ApiException.NotFound("point.not_found",
                        $"point '{pointId}' not found on device '{deviceId}'",
                        new { device = deviceId, point = pointId });

        return (device, point);
    }

    private static PointDto ToPointView(NorthboundContext ctx, Device device, Point point)
    {
        var sample = ctx.Engine.Cache.Get(device.Id, point.Id);

        if (sample is null)
        {
            // 已配置但从没采到：如实返回 not_connected，而不是编一个 0。
            return new PointDto(
                device.Id,
                point.Id,
                point.Name,
                DataValue.Number(point.DataType, 0),
                "not_connected",
                point.Unit,
                null,
                DateTimeOffset.UtcNow,
                0,
                point.Writable,
                point.Deadband,
                JsonEnum.ToWire(point.DeadbandMode),
                point.RangeMin,
                point.RangeMax,
                point.Interval,
                point.EffectiveInterval(device),
                point.Count);
        }

        return new PointDto(
            device.Id,
            point.Id,
            point.Name,
            sample.Value,
            JsonEnum.ToWire(sample.Quality),
            sample.Unit ?? point.Unit,
            sample.DeviceTime,
            sample.CollectedAt,
            sample.Seq,
            point.Writable,
            point.Deadband,
            JsonEnum.ToWire(point.DeadbandMode),
            point.RangeMin,
            point.RangeMax,
            point.Interval,
            point.EffectiveInterval(device),
            point.Count);
    }

    private static int ParseInt(string? text, int fallback)
        => int.TryParse(text, out var v) ? v : fallback;

    /// <summary>游标就是不透明化的偏移量。用 base64 是为了让调用方不要依赖它的语义。</summary>
    private static string EncodeCursor(int offset)
        => Convert.ToBase64String(Encoding.UTF8.GetBytes(offset.ToString()));

    private static int ParseCursor(string? cursor)
    {
        if (string.IsNullOrWhiteSpace(cursor)) return 0;

        try
        {
            var text = Encoding.UTF8.GetString(Convert.FromBase64String(cursor));
            return int.TryParse(text, out var v) && v >= 0 ? v : 0;
        }
        catch (FormatException)
        {
            throw ApiException.BadRequest("request.invalid_cursor", "cursor is not valid");
        }
    }
}

/// <summary>点位视图。quality 用字符串，与 Sample 契约保持一致。</summary>
public sealed record PointDto(
    string Device,
    string Point,
    string? Name,
    DataValue Value,
    string Quality,
    string? Unit,
    DateTimeOffset? DeviceTime,
    DateTimeOffset CollectedAt,
    ulong Seq,
    bool Writable,
    double? Deadband,
    string DeadbandMode,
    double? RangeMin,
    double? RangeMax,
    TimeSpan? Interval,
    TimeSpan EffectiveInterval,
    // 数组点位的元素个数；0 表示标量。北向消费者靠它渲染「N 个元素」，
    // 空值点位（Value 是占位值）也能据此知道期望维度。
    int Count = 0);