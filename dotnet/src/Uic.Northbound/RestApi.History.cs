using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Domain;
using Uic.Northbound.Security;

namespace Uic.Northbound;

public static partial class RestApi
{
    /// <summary>
    /// 历史查询：从 journal sink 的 NDJSON 里按时间范围取样本。
    ///
    /// 时间参数默认最近 1 小时；`interval` 打开降采样（每个点位每桶取最后一条）。
    /// 不引入时序库的取舍见 <see cref="JournalHistory"/> 与 docs/northbound-api.md。
    /// </summary>
    private static void MapHistory(WebApplication app, NorthboundContext ctx)
    {
        app.MapGet("/api/v1/history", async (HttpContext http, CancellationToken ct) =>
        {
            var now = DateTimeOffset.UtcNow;
            var to = ParseTime(http.Request.Query["to"], now, "to");
            var from = ParseTime(http.Request.Query["from"], to - TimeSpan.FromHours(1), "from");

            if (from > to)
            {
                throw ApiException.BadRequest(
                    "request.invalid_range",
                    "'from' must not be after 'to'",
                    new { from = from.ToString("O"), to = to.ToString("O") });
            }

            TimeSpan? interval = ParseInterval(http.Request.Query["interval"]);

            var device = http.Request.Query["device"].FirstOrDefault();
            var point = http.Request.Query["point"].FirstOrDefault();
            if (string.IsNullOrWhiteSpace(device)) device = null;
            if (string.IsNullOrWhiteSpace(point)) point = null;

            var limit = ClampLimit(ParseInt(http.Request.Query["limit"], 100));
            var offset = ParseCursor(http.Request.Query["cursor"]);

            var result = ctx.History is null
                ? new HistoryResult(Array.Empty<SampleDto>(), 0)
                : await ctx.History.QueryAsync(
                    new HistoryQuery(device, point, from, to, interval), ct).ConfigureAwait(false);

            var page = result.Items.Skip(offset).Take(limit).ToList();
            var next = offset + page.Count < result.Total ? EncodeCursor(offset + page.Count) : null;

            return Results.Json(new PagedResponse<SampleDto>(page, next, result.Total), ApiJson.Options);
        }).RequirePermission(Permissions.SystemRead);
    }

    private static DateTimeOffset ParseTime(string? text, DateTimeOffset fallback, string parameter)
        => string.IsNullOrWhiteSpace(text)
            ? fallback
            : JournalHistory.ParseInstant(text, parameter);

    private static TimeSpan? ParseInterval(string? text)
    {
        if (string.IsNullOrWhiteSpace(text)) return null;

        TimeSpan interval;
        try
        {
            interval = DurationConverter.Parse(text);
        }
        catch (FormatException)
        {
            throw ApiException.BadRequest(
                "request.invalid_parameter",
                "'interval' must be a duration such as '5s' or '1m'",
                new { parameter = "interval" });
        }

        if (interval <= TimeSpan.Zero)
        {
            throw ApiException.BadRequest(
                "request.invalid_parameter",
                "'interval' must be greater than zero",
                new { parameter = "interval" });
        }

        return interval;
    }
}
