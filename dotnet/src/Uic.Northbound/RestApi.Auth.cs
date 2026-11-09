using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Northbound.Security;

namespace Uic.Northbound;

public static partial class RestApi
{
    /// <summary>
    /// 认证端点：登录、退出、看自己、改自己的口令。
    ///
    /// 登录不要求先登录，其余三个只要求「已认证」——它们本身就是账号生命周期的一部分，
    /// 不能再要求某个业务权限，否则被强制改密的用户会连改密都进不去。
    /// </summary>
    private static void MapAuth(WebApplication app, NorthboundContext ctx)
    {
        app.MapPost("/api/v1/auth/login", async (HttpContext http, CancellationToken ct) =>
        {
            var auth = RequireAuth(ctx);
            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = ReadJson<LoginRequestDto>(body)
                          ?? throw ApiException.BadRequest("request.invalid", "body is required");

            var now = DateTimeOffset.UtcNow;

            try
            {
                var result = auth.Login(request.Username, request.Password,
                    http.Connection.RemoteIpAddress?.ToString() ?? "unknown", now);

                ctx.Audit?.Write(
                    "auth.login",
                    "ok",
                    result.User.Id,
                    "rest",
                    target: result.User.Id,
                    remote: http.Connection.RemoteIpAddress?.ToString(),
                    requestId: http.Items["requestId"] as string,
                    detail: new { role = result.User.Role });

                return Results.Json(new LoginResponseDto(
                    result.Token,
                    result.ExpiresAt,
                    ToView(result.User)), ApiJson.Options);
            }
            catch (ApiException ex)
            {
                // 失败也要留痕，但不记口令。用户名要记：否则「谁在被尝试」这个问题无人能答。
                ctx.Audit?.Write(
                    "auth.login",
                    "denied",
                    actor: "anonymous",
                    source: "rest",
                    target: http.Request.Path.Value,
                    remote: http.Connection.RemoteIpAddress?.ToString(),
                    requestId: http.Items["requestId"] as string,
                    detail: new { username = request.Username, reason = ex.Code });

                throw;
            }
        });

        app.MapPost("/api/v1/auth/logout", (HttpContext http) =>
        {
            var auth = RequireAuth(ctx);
            var token = BearerToken(http);

            auth.Logout(token);
            WriteAudit(ctx, http, "auth.logout", "ok");

            return Results.NoContent();
        });

        app.MapGet("/api/v1/auth/me", (HttpContext http) =>
        {
            var principal = RequirePrincipal(http);
            return Results.Json(CurrentUser(ctx, principal), ApiJson.Options);
        });

        app.MapPost("/api/v1/auth/password", async (HttpContext http, CancellationToken ct) =>
        {
            var auth = RequireAuth(ctx);
            var principal = RequirePrincipal(http);

            // API Token 不是账号，没有口令可改；让请求失败比悄悄忽略更清楚。
            if (principal.IsApiToken)
                throw ApiException.BadRequest("auth.not_a_user",
                    "the static API token has no password; change it in the configuration");

            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = ReadJson<PasswordChangeRequestDto>(body)
                          ?? throw ApiException.BadRequest("request.invalid", "body is required");

            var check = auth.Users.CheckPassword(principal.Id, request.CurrentPassword ?? string.Empty);
            if (check != LoginCheck.Ok)
                throw ApiException.BadRequest("auth.invalid_password", "current password is incorrect");

            UserStore.ValidatePassword(request.NewPassword, request.CurrentPassword);
            auth.Users.SetPassword(principal.Id, request.NewPassword!, mustChange: false);
            auth.Users.ClearInitialPasswordFile();

            // 改密后旧会话全部作废：否则改密前泄露的 Token 还能继续用 12 小时。
            // 给当前客户端换发一个新 Token，浏览器不必重新登录。
            auth.Sessions.RevokeUser(principal.Id);
            var (token, session) = auth.Sessions.Issue(principal.Id, DateTimeOffset.UtcNow);

            WriteAudit(ctx, http, "auth.password_changed", "ok");

            var user = auth.Users.Find(principal.Id)!;
            return Results.Json(new PasswordChangeResponseDto(
                token, session.ExpiresAt, ToView(user)), ApiJson.Options);
        });

        MapUsers(app, ctx);
    }

    private static CurrentUserResponse CurrentUser(NorthboundContext ctx, Principal principal)
    {
        var user = ctx.Auth?.Users.Find(principal.Id);

        // API Token 没有账号记录，用 principal 本身合成一个视图；
        // kind 字段让前端能区分「真人登录」与「运维令牌」。
        if (user is null)
        {
            return new CurrentUserResponse(
                principal.Id,
                principal.DisplayName,
                principal.Role,
                principal.Permissions.Order(StringComparer.Ordinal).ToArray(),
                Enabled: true,
                MustChangePassword: false,
                principal.IsApiToken ? "api-token" : "session");
        }

        return new CurrentUserResponse(
            user.Id,
            user.DisplayName,
            user.Role,
            Roles.PermissionsOf(user.Role).Order(StringComparer.Ordinal).ToArray(),
            user.Enabled,
            user.MustChangePassword,
            "session");
    }

    private static UserViewDto ToView(UserRecord user) => new(
        user.Id,
        user.DisplayName,
        user.Role,
        Roles.PermissionsOf(user.Role).Order(StringComparer.Ordinal).ToArray(),
        user.Enabled,
        user.MustChangePassword,
        user.CreatedAt,
        user.LastLoginAt);

    private static AuthService RequireAuth(NorthboundContext ctx)
        => ctx.Auth ?? throw ApiException.NotReady("authentication is not configured on this runtime");

    private static Principal RequirePrincipal(HttpContext http)
        => http.Items["principal"] as Principal
           ?? throw ApiException.Unauthorized();

    /// <summary>从请求里取出 Bearer Token；用于退出与改密换发。</summary>
    private static string? BearerToken(HttpContext http)
    {
        var header = http.Request.Headers.Authorization.FirstOrDefault();
        if (string.IsNullOrWhiteSpace(header)) return null;
        if (!header.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase)) return null;

        var token = header["Bearer ".Length..].Trim();
        return token.Length == 0 ? null : token;
    }
}
