using System.Collections.Concurrent;
using System.Text;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Obs;

namespace Uic.Northbound;

public static partial class RestApi
{
    /// <summary>
    /// 审计查询与导出。
    ///
    /// 为什么单独开一组端点而不是塞进 /logs：日志是「发生了什么」的调试线索，
    /// 会轮转、会丢；审计是「谁让它发生的」的追责记录，不能被日志量冲掉。
    /// 两者混在一起，现场就会拿日志当审计用，然后在重启后发现什么都没有。
    /// </summary>
    private static void MapAudit(WebApplication app, NorthboundContext ctx)
    {
        app.MapGet("/api/v1/audit", async (HttpContext http, CancellationToken ct) =>
        {
            var query = ReadAuditQuery(http, ctx, defaultLimit: 100, maxLimit: 1000);
            var page = await ctx.Audit!.QueryAsync(query, ct).ConfigureAwait(false);

            var offset = query.Offset + page.Items.Count;
            var next = offset < page.Total ? EncodeCursor(offset) : null;

            return Results.Json(new PagedResponse<AuditEntry>(page.Items, next, page.Total), ApiJson.Options);
        });

        app.MapGet("/api/v1/audit/export", async (HttpContext http, CancellationToken ct) =>
        {
            var query = ReadAuditQuery(http, ctx, defaultLimit: 5000, maxLimit: 100_000);
            var page = await ctx.Audit!.QueryAsync(query, ct).ConfigureAwait(false);

            var text = new StringBuilder();
            foreach (var entry in page.Items)
                text.Append(AuditLog.Serialize(entry)).Append('\n');

            // 导出本身也要留痕：审计文件一旦离开这台机器，谁在什么时候拿走的
            // 是和内容同等重要的信息。
            WriteAudit(ctx, http, "audit.export", "ok", target: "ndjson",
                detail: new
                {
                    query.From,
                    query.To,
                    query.Action,
                    query.Actor,
                    query.Result,
                    query.Target,
                    records = page.Items.Count,
                });

            var name = $"uic-audit-{DateTimeOffset.UtcNow:yyyyMMdd-HHmmss}.ndjson";
            return Results.File(Encoding.UTF8.GetBytes(text.ToString()), "application/x-ndjson", name);
        });
    }

    /// <summary>
    /// 解析审计查询参数。默认最近 24 小时、最新在前：审计最常见的用法是
    /// 「刚才谁改的」，而不是翻一个月前的记录。
    /// </summary>
    private static AuditQuery ReadAuditQuery(
        HttpContext http, NorthboundContext ctx, int defaultLimit, int maxLimit)
    {
        if (ctx.Audit is null)
            throw ApiException.NotReady("audit log is not configured on this runtime");

        var now = DateTimeOffset.UtcNow;
        var to = ParseTime(http.Request.Query["to"], now, "to");
        var from = ParseTime(http.Request.Query["from"], to - TimeSpan.FromHours(24), "from");

        if (from > to)
        {
            throw ApiException.BadRequest(
                "request.invalid_range",
                "'from' must not be after 'to'",
                new { from = from.ToString("O"), to = to.ToString("O") });
        }

        var limit = ClampLimit(ParseInt(http.Request.Query["limit"], defaultLimit), defaultLimit, maxLimit);
        var offset = ParseCursor(http.Request.Query["cursor"]);

        return new AuditQuery(
            Normalize(http.Request.Query["action"].FirstOrDefault()),
            Normalize(http.Request.Query["actor"].FirstOrDefault()),
            Normalize(http.Request.Query["result"].FirstOrDefault()),
            Normalize(http.Request.Query["target"].FirstOrDefault()),
            from,
            to,
            limit,
            offset);
    }

    private static string? Normalize(string? value)
        => string.IsNullOrWhiteSpace(value) ? null : value;

    // ------------------------------------------------------------------
    // 写入侧：所有需要留痕的地方都走这三个小函数
    // ------------------------------------------------------------------

    /// <summary>
    /// 当前调用者的身份。这一版没有账号体系，只有「配了令牌」与「没配令牌」两种；
    /// FR-OPS-02 落地后这里换成账号 id，审计记录的形状不变。
    /// </summary>
    private static string ActorOf(NorthboundContext ctx)
        => string.IsNullOrWhiteSpace(ctx.Rest.Token) ? "anonymous" : "bearer";

    private static void WriteAudit(
        NorthboundContext ctx,
        HttpContext http,
        string action,
        string result,
        string? target = null,
        object? detail = null,
        string source = "rest")
    {
        ctx.Audit?.Write(
            action,
            result,
            ActorOf(ctx),
            source,
            target,
            http.Connection.RemoteIpAddress?.ToString(),
            http.Items["requestId"] as string,
            detail);
    }

    /// <summary>
    /// 执行一次会改变状态的动作，并把成功与失败都记成审计。
    ///
    /// 失败也要记：只记成功的审计恰好帮不上「为什么没生效」这类问题。
    /// </summary>
    private static async Task<T> AuditedAsync<T>(
        NorthboundContext ctx,
        HttpContext http,
        string action,
        Func<T, string?> target,
        Func<T, object?> detail,
        Func<Task<T>> body)
    {
        try
        {
            var value = await body().ConfigureAwait(false);
            WriteAudit(ctx, http, action, "ok", target(value), detail(value));
            return value;
        }
        catch (Exception ex)
        {
            WriteAudit(ctx, http, action, "failed", target: null,
                detail: new { error = ex.Message });
            throw;
        }
    }

    // ------------------------------------------------------------------
    // 认证失败
    // ------------------------------------------------------------------

    private static readonly ConcurrentDictionary<string, DateTimeOffset> AuthFailureSeen = new(StringComparer.Ordinal);

    /// <summary>
    /// 记录一次认证失败。按来源 IP 做 30 秒去重：审计要回答「谁在被拒绝」，
    /// 不是把扫描器的每个 401 都写成一条磁盘记录。
    /// </summary>
    private static void AuditAuthFailure(HttpContext http, NorthboundContext ctx)
    {
        var remote = http.Connection.RemoteIpAddress?.ToString() ?? "unknown";
        var now = DateTimeOffset.UtcNow;

        if (AuthFailureSeen.TryGetValue(remote, out var last) && now - last < TimeSpan.FromSeconds(30))
            return;

        AuthFailureSeen[remote] = now;

        // 来源 IP 可能是伪造的（X-Forwarded-For 不在这一层解析），必须有上限。
        if (AuthFailureSeen.Count > 1024)
        {
            foreach (var (key, seenAt) in AuthFailureSeen)
            {
                if (now - seenAt >= TimeSpan.FromSeconds(30)) AuthFailureSeen.TryRemove(key, out _);
            }
        }

        // 认证失败时还没有可信身份：actor 记 anonymous，而不是「哪个令牌被拒绝了」。
        // detail.hasToken 只是告诉运维「对方带了令牌但不对」，不记录令牌本身。
        ctx.Audit?.Write(
            "auth.failed",
            "denied",
            actor: "anonymous",
            source: "rest",
            target: http.Request.Path.Value,
            remote: http.Connection.RemoteIpAddress?.ToString(),
            requestId: http.Items["requestId"] as string,
            detail: new
            {
                method = http.Request.Method,
                hasToken = !string.IsNullOrWhiteSpace(http.Request.Headers.Authorization.FirstOrDefault()),
            });
    }

    /// <summary>用例需要在同一进程里连续验证 401 审计；去重窗口会跨用例生效。</summary>
    internal static void ResetAuthFailureThrottleForTests() => AuthFailureSeen.Clear();
}
