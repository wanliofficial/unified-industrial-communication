using System.Diagnostics;
using System.IO.Compression;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Config;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Northbound;

public static partial class RestApi
{
    private static void MapConfig(WebApplication app, NorthboundContext ctx)
    {
        app.MapGet("/api/v1/config", () =>
        {
            var config = ctx.Control.CurrentConfig
                         ?? throw ApiException.NotReady("configuration is not loaded");

            return Results.Json(new
            {
                version = ctx.Control.ConfigVersion,
                config = Redact(config),
            }, ApiJson.Options);
        });

        app.MapPost("/api/v1/config/validate", async (HttpContext http, CancellationToken ct) =>
        {
            var (text, _) = await ReadConfigTextAsync(http, ct).ConfigureAwait(false);

            // 与 apply 走同一条还原路径：客户端提交的通常是 GET /config 读到的脱敏文档，
            // 不还原的话 "***" 会被当成明文凭据，校验结果与真正的 apply 对不上。
            var restored = RedactedSecrets.Restore(text, ctx.Control.CurrentConfig);

            // 这里刻意不抛异常：校验的产出就是「问题清单」，包括 JSON 语法错误。
            var parsed = ConfigStore.TryParse(restored, "request", out var doc, out var problems);

            var all = problems.ToList();

            // 凭据引用必须真的能解析：语法对、但环境变量没注入，apply 一样会失败。
            if (parsed && doc is not null) all.AddRange(Secrets.Validate(doc));

            var valid = parsed && all.Count == 0;

            return Results.Json(new ValidateResponseDto(
                valid,
                all.Select(p => p.ToString()).ToArray()), ApiJson.Options);
        });

        app.MapPost("/api/v1/config/apply", async (HttpContext http, CancellationToken ct) =>
        {
            var (doc, comment) = await ReadConfigAsync(http, ctx.Control.CurrentConfig, ct).ConfigureAwait(false);

            var before = ctx.Control.CurrentConfig;
            var started = Stopwatch.GetTimestamp();

            var outcome = await AuditedAsync(
                ctx, http, "config.apply",
                o => o.Version,
                o =>
                {
                    var changes = CountChanges(before, doc);
                    return new { comment, changes, o.RestartedDevices, o.DurationMs };
                },
                () => ctx.Control.ApplyConfigAsync(doc, comment, ct)).ConfigureAwait(false);

            var counts = CountChanges(before, doc);
            var durationMs = (long)Stopwatch.GetElapsedTime(started).TotalMilliseconds;

            return Results.Json(new ApplyResponseDto(
                outcome.Version,
                outcome.Applied,
                counts,
                outcome.RestartedDevices,
                durationMs), ApiJson.Options);
        });

        app.MapGet("/api/v1/config/versions", () =>
        {
            var current = ctx.Control.ConfigVersion;

            var versions = ctx.Store.Versions()
                .Select(v => new ConfigVersionDto(v.Id, v.CreatedAt, v.Comment, v.Size, v.Id == current))
                .ToArray();

            return Results.Json(new PagedResponse<ConfigVersionDto>(versions, null, versions.Length), ApiJson.Options);
        });

        app.MapGet("/api/v1/config/versions/{id}", (string id) =>
        {
            var doc = ctx.Store.LoadVersion(id)
                      ?? throw ApiException.NotFound("config.version_not_found", $"version '{id}' not found",
                          new { version = id });

            return Results.Json(new { version = id, config = Redact(doc) }, ApiJson.Options);
        });

        app.MapGet("/api/v1/config/diff", (HttpContext http) =>
        {
            var from = http.Request.Query["from"].FirstOrDefault();
            var to = http.Request.Query["to"].FirstOrDefault();

            if (string.IsNullOrWhiteSpace(to)) to = ctx.Control.ConfigVersion;
            if (string.IsNullOrWhiteSpace(from))
                throw ApiException.BadRequest("request.missing_parameter", "'from' is required");

            var before = ctx.Store.LoadVersion(from)
                         ?? throw ApiException.NotFound("config.version_not_found", $"version '{from}' not found");
            var after = ctx.Store.LoadVersion(to)
                        ?? throw ApiException.NotFound("config.version_not_found", $"version '{to}' not found");

            var entries = ConfigStore.Diff(before, after)
                .Select(e => new DiffEntryDto(
                    e.Path,
                    e.Kind,
                    // 差异会展示字段值；凭据字段不能因为「变了」就泄露明文。
                    RedactedSecrets.MaskIfSecret(e.Path, e.Before),
                    RedactedSecrets.MaskIfSecret(e.Path, e.After)))
                .ToArray();

            return Results.Json(new DiffResponseDto(from, to, entries), ApiJson.Options);
        });

        app.MapPost("/api/v1/config/rollback", async (HttpContext http, CancellationToken ct) =>
        {
            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = ReadJson<RollbackRequestDto>(body)
                          ?? throw ApiException.BadRequest("request.invalid", "body is required");

            if (!request.Confirm)
                throw ApiException.BadRequest("request.confirm_required",
                    "rollback requires \"confirm\": true");

            var outcome = await AuditedAsync(
                ctx, http, "config.rollback",
                o => o.Version,
                o => new { requested = request.Version, o.DurationMs, o.RestartedDevices },
                () => ctx.Control.RollbackAsync(request.Version, ct)).ConfigureAwait(false);

            return Results.Json(new ApplyResponseDto(
                outcome.Version,
                outcome.Applied,
                new ChangedCountsDto(0, 0, 0, 0, 0, 0),
                outcome.RestartedDevices,
                outcome.DurationMs), ApiJson.Options);
        });

        app.MapPost("/api/v1/commands", async (HttpContext http, CancellationToken ct) =>
        {
            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = ReadJson<CommandRequestDto>(body)
                          ?? throw ApiException.BadRequest("request.invalid", "body is required");

            var response = await AuditedAsync(
                ctx, http, request.Action,
                _ => null,
                r => new { r.Ok, r.Detail, request.Params, confirm = request.Confirm },
                () => CommandExecutor.ExecuteAsync(ctx.Control, request, CommandScope.All, ct))
                .ConfigureAwait(false);

            return Results.Json(response, ApiJson.Options);
        });
    }

    private static void MapDiagnostics(WebApplication app, NorthboundContext ctx)
    {
        app.MapGet("/api/v1/logs", (HttpContext http) =>
        {
            var device = http.Request.Query["device"].FirstOrDefault();
            var level = http.Request.Query["level"].FirstOrDefault();
            var limit = ClampLimit(ParseInt(http.Request.Query["limit"], 200), 200, 5000);

            var entries = ctx.Logs.Snapshot(level, category: null, deviceId: device, limit: limit)
                .Select(e => new LogEntryDto(e.Seq, e.At, e.Level, e.Category, e.Message, e.DeviceId, e.Exception))
                .ToArray();

            return Results.Json(new PagedResponse<LogEntryDto>(entries, null, entries.Length), ApiJson.Options);
        });

        app.MapGet("/api/v1/diagnostics/bundle", () =>
        {
            var bytes = DiagnosticsBundle.Build(ctx);
            return Results.File(bytes, "application/zip", DiagnosticsBundle.FileName(DateTimeOffset.UtcNow));
        });

        app.MapGet("/api/v1/diagnostics/network", async (HttpContext http, CancellationToken ct) =>
        {
            var deviceId = http.Request.Query["device"].FirstOrDefault()
                           ?? throw ApiException.BadRequest("request.missing_parameter", "'device' is required");

            var config = ctx.Control.CurrentConfig
                         ?? throw ApiException.NotReady("configuration is not loaded");

            var device = config.Devices.FirstOrDefault(d => d.Id == deviceId)
                         ?? throw ApiException.NotFound("device.not_found", $"device '{deviceId}' not found");

            var channel = config.Channels.FirstOrDefault(c => c.Id == device.ChannelId)
                          ?? throw ApiException.NotFound("channel.not_found",
                              $"channel '{device.ChannelId}' not found");

            var result = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                ["device"] = device.Id,
                ["channel"] = channel.Id,
                ["protocol"] = channel.Protocol,
                ["transport"] = channel.Transport,
            };

            if (channel.Transport == "serial")
            {
                var port = channel.Serial?.Port;
                result["port"] = port;
                result["exists"] = !string.IsNullOrEmpty(port) && File.Exists(port);
            }
            else if (channel.Transport == "tcp")
            {
                result["host"] = channel.Host;
                result["port"] = channel.Port;

                var dnsStart = Stopwatch.GetTimestamp();
                try
                {
                    var addresses = await Dns.GetHostAddressesAsync(channel.Host!, ct).ConfigureAwait(false);
                    result["dnsOk"] = true;
                    result["dnsMs"] = (long)Stopwatch.GetElapsedTime(dnsStart).TotalMilliseconds;
                    result["addresses"] = addresses.Select(a => a.ToString()).ToArray();
                }
                catch (Exception ex)
                {
                    result["dnsOk"] = false;
                    result["dnsError"] = ex.Message;
                }

                var tcpStart = Stopwatch.GetTimestamp();
                try
                {
                    using var client = new TcpClient();
                    using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
                    timeout.CancelAfter(TimeSpan.FromSeconds(3));
                    await client.ConnectAsync(channel.Host!, channel.Port, timeout.Token).ConfigureAwait(false);
                    result["tcpOk"] = true;
                    result["tcpMs"] = (long)Stopwatch.GetElapsedTime(tcpStart).TotalMilliseconds;
                }
                catch (Exception ex)
                {
                    result["tcpOk"] = false;
                    result["tcpError"] = ex.Message;
                }
            }

            result["selfTest"] = await ctx.Control.SelfTestAsync(device.Id, ct).ConfigureAwait(false);

            return Results.Json(result, ApiJson.Options);
        });
    }

    /// <summary>脱敏：口令与 token 一律替换，配置本身照常可读。</summary>
    internal static JsonObject Redact(ConfigDocument config)
    {
        var node = JsonNode.Parse(JsonSerializer.Serialize(config, JsonDefaults.Options)) as JsonObject
                   ?? new JsonObject();

        RedactNode(node);
        return node;
    }

    private static void RedactNode(JsonNode? node)
    {
        switch (node)
        {
            case JsonObject obj:
                foreach (var key in obj.Select(kv => kv.Key).ToArray())
                {
                    if (key is "password" or "token" or "secret")
                    {
                        if (obj[key] is not null) obj[key] = "***";
                        continue;
                    }
                    RedactNode(obj[key]);
                }
                break;

            case JsonArray array:
                foreach (var item in array) RedactNode(item);
                break;
        }
    }

    private static async Task<string> ReadBodyAsync(HttpContext http, CancellationToken ct)
    {
        using var reader = new StreamReader(http.Request.Body, Encoding.UTF8);
        var body = await reader.ReadToEndAsync(ct).ConfigureAwait(false);

        if (string.IsNullOrWhiteSpace(body))
            throw ApiException.BadRequest("request.empty_body", "request body is required");

        return body;
    }

    /// <summary>接受 <c>{"config": {...}}</c> 或 <c>{"json": "..."}</c> 两种提交形态。</summary>
    private static async Task<(ConfigDocument Doc, string? Comment)> ReadConfigAsync(
        HttpContext http, ConfigDocument? current, CancellationToken ct)
    {
        var (text, comment) = await ReadConfigTextAsync(http, ct).ConfigureAwait(false);

        // 客户端大概率是把 GET /config 读到的脱敏配置改一处再提交；
        // 先把 "***" 还原成当前密钥，避免无关编辑把令牌改成占位符。
        return (ConfigStore.Parse(RedactedSecrets.Restore(text, current), "request"), comment);
    }

    /// <summary>只取出配置原文，不做校验：校验端点要把问题一次性列出来，而不是抛第一条。</summary>
    private static async Task<(string Text, string? Comment)> ReadConfigTextAsync(HttpContext http, CancellationToken ct)
    {
        var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);

        JsonDocument doc;
        try
        {
            doc = JsonDocument.Parse(body);
        }
        catch (JsonException ex)
        {
            // 请求体本身不是 JSON：与「配置语义非法」共用 422/错误信封，
            // 而不是落进 500 —— 这明显是调用方的问题，不是网关的故障。
            throw new ConfigException(
                $"configuration is not valid JSON: {ex.Message}",
                new[] { new ValidationProblem("$", ex.Message) });
        }

        using var _ = doc;
        var root = doc.RootElement;

        var comment = root.TryGetProperty("comment", out var c) && c.ValueKind == JsonValueKind.String
            ? c.GetString()
            : null;

        if (root.TryGetProperty("config", out var configElement))
        {
            return (configElement.GetRawText(), comment);
        }

        if (root.TryGetProperty("json", out var jsonElement) && jsonElement.ValueKind == JsonValueKind.String)
        {
            return (jsonElement.GetString() ?? string.Empty, comment);
        }

        // 兼容直接提交整份配置的写法。
        return (body, comment);
    }

    /// <summary>按设备/点位粒度统计变化，用于 apply 响应里的 changed 字段。</summary>
    private static ChangedCountsDto CountChanges(ConfigDocument? before, ConfigDocument after)
    {
        if (before is null)
        {
            return new ChangedCountsDto(
                after.Devices.Count, 0, 0,
                after.Devices.Sum(d => d.Points.Count), 0, 0);
        }

        int devicesAdded = 0, devicesModified = 0, devicesRemoved = 0;
        int pointsAdded = 0, pointsModified = 0, pointsRemoved = 0;

        var beforeDevices = before.Devices.ToDictionary(d => d.Id, StringComparer.Ordinal);
        var afterDevices = after.Devices.ToDictionary(d => d.Id, StringComparer.Ordinal);

        foreach (var (id, prev) in beforeDevices)
        {
            if (!afterDevices.TryGetValue(id, out var next))
            {
                devicesRemoved++;
                pointsRemoved += prev.Points.Count;
                continue;
            }

            var prevPoints = prev.Points.ToDictionary(p => p.Id, StringComparer.Ordinal);
            var nextPoints = next.Points.ToDictionary(p => p.Id, StringComparer.Ordinal);

            var deviceChanged = JsonSerializer.Serialize(prev, JsonDefaults.Options) !=
                                JsonSerializer.Serialize(next, JsonDefaults.Options);

            foreach (var pid in prevPoints.Keys)
            {
                if (!nextPoints.ContainsKey(pid))
                {
                    pointsRemoved++;
                    deviceChanged = true;
                }
                else if (JsonSerializer.Serialize(prevPoints[pid], JsonDefaults.Options) !=
                         JsonSerializer.Serialize(nextPoints[pid], JsonDefaults.Options))
                {
                    pointsModified++;
                    deviceChanged = true;
                }
            }

            foreach (var pid in nextPoints.Keys.Where(p => !prevPoints.ContainsKey(p)))
            {
                pointsAdded++;
                deviceChanged = true;
            }

            if (deviceChanged) devicesModified++;
        }

        foreach (var (id, next) in afterDevices)
        {
            if (beforeDevices.ContainsKey(id)) continue;
            devicesAdded++;
            pointsAdded += next.Points.Count;
        }

        return new ChangedCountsDto(devicesAdded, devicesModified, devicesRemoved,
            pointsAdded, pointsModified, pointsRemoved);
    }
}