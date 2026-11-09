using System.Text;
using System.Text.Json;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Northbound.Security;

namespace Uic.Northbound;

public static partial class RestApi
{
    private const int MaxSseConnections = 50;

    private static void MapStream(WebApplication app, NorthboundContext ctx, SseHub hub)
    {
        app.MapGet("/api/v1/stream", async (HttpContext http, CancellationToken ct) =>
        {
            if (hub.ConnectionCount >= MaxSseConnections)
                throw ApiException.TooManyRequests($"at most {MaxSseConnections} concurrent streams");

            var rawPoints = http.Request.Query["points"].FirstOrDefault();
            var patterns = string.IsNullOrWhiteSpace(rawPoints)
                ? Array.Empty<string>()
                : rawPoints.Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries);

            var quality = http.Request.Query["quality"].FirstOrDefault();
            var heartbeat = ParseInt(http.Request.Query["heartbeat"], 15);
            if (heartbeat is < 1 or > 300)
                throw ApiException.BadRequest("request.invalid_parameter", "heartbeat must be 1..300 seconds");

            http.Response.StatusCode = StatusCodes.Status200OK;
            http.Response.ContentType = "text/event-stream; charset=utf-8";
            http.Response.Headers.CacheControl = "no-cache, no-transform";
            http.Response.Headers["X-Accel-Buffering"] = "no";

            using var subscription = hub.Subscribe(patterns, quality, heartbeat);

            // hello 必须第一个发：客户端据此确认流已建立，而不是等第一个采样点。
            var hello = JsonSerializer.Serialize(new
            {
                server = "uic",
                apiVersion = ApiVersion,
                heartbeatSeconds = heartbeat,
                filters = patterns,
            }, ApiJson.Options);

            await WriteEventAsync(http, "hello", hello, null, ct).ConfigureAwait(false);

            var reader = subscription.Reader;
            var heartbeatSpan = TimeSpan.FromSeconds(heartbeat);

            try
            {
                while (!ct.IsCancellationRequested)
                {
                    var waitRead = reader.WaitToReadAsync(ct).AsTask();
                    var waitHeartbeat = Task.Delay(heartbeatSpan, ct);

                    var completed = await Task.WhenAny(waitRead, waitHeartbeat).ConfigureAwait(false);

                    if (completed == waitHeartbeat)
                    {
                        await WriteEventAsync(http, "heartbeat",
                            JsonSerializer.Serialize(new { ts = DateTimeOffset.UtcNow }, ApiJson.Options),
                            null, ct).ConfigureAwait(false);
                        continue;
                    }

                    if (!await waitRead.ConfigureAwait(false)) break; // hub 已关闭该订阅

                    while (reader.TryRead(out var evt))
                    {
                        await WriteEventAsync(http, evt.Event, evt.Data, evt.Id, ct).ConfigureAwait(false);
                    }
                }
            }
            catch (OperationCanceledException)
            {
                // 客户端断开。
            }
            catch (IOException)
            {
                // 客户端断开。
            }

            return Results.Empty;
        }).RequirePermission(Permissions.SystemRead);
    }

    private static async Task WriteEventAsync(
        HttpContext http,
        string eventName,
        string data,
        string? id,
        CancellationToken ct)
    {
        var sb = new StringBuilder(data.Length + 64);

        if (id is not null) sb.Append("id: ").Append(id).Append('\n');
        sb.Append("event: ").Append(eventName).Append('\n');

        // SSE 规定按行拆分：data 里的换行必须逐行加 "data: " 前缀。
        foreach (var line in data.Split('\n'))
        {
            sb.Append("data: ").Append(line.TrimEnd('\r')).Append('\n');
        }

        sb.Append('\n');

        await http.Response.WriteAsync(sb.ToString(), ct).ConfigureAwait(false);
        await http.Response.Body.FlushAsync(ct).ConfigureAwait(false);
    }
}