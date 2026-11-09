using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Northbound.Security;

namespace Uic.Northbound;

public static partial class RestApi
{
    /// <summary>
    /// 账号管理。只有 <c>user.manage</c> 能进——这一组端点是 RBAC 的「操作级」
    /// 示例：页面可以藏起来，但真正的边界在这里。
    ///
    /// 口令永远不以可读形式返回，除了「管理员新建成或重置时生成的那一次」：
    /// 一次性临时口令必须让管理员抄给用户，否则流程走不通。它带强制改密标记，
    /// 用一次就作废。
    /// </summary>
    private static void MapUsers(WebApplication app, NorthboundContext ctx)
    {
        app.MapGet("/api/v1/users", (HttpContext http) =>
        {
            var auth = RequireAuth(ctx);
            var users = auth.Users.All()
                .OrderBy(u => u.Id, StringComparer.Ordinal)
                .Select(ToView)
                .ToArray();

            return Results.Json(new PagedResponse<UserViewDto>(users, null, users.Length), ApiJson.Options);
        }).RequirePermission(Permissions.UserManage);

        app.MapPost("/api/v1/users", async (HttpContext http, CancellationToken ct) =>
        {
            var auth = RequireAuth(ctx);
            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = ReadJson<CreateUserRequestDto>(body)
                          ?? throw ApiException.BadRequest("request.invalid", "body is required");

            if (string.IsNullOrWhiteSpace(request.Id))
                throw ApiException.BadRequest("user.invalid_id", "'id' is required");

            var role = string.IsNullOrWhiteSpace(request.Role) ? Roles.Viewer : request.Role;

            // 没给口令就生成一个：管理员不该被逼着替别人想口令。
            var generated = string.IsNullOrWhiteSpace(request.Password);
            var password = generated ? UserStore.GeneratePassword() : request.Password!;
            UserStore.ValidatePassword(password);

            var user = auth.Users.Create(request.Id, request.DisplayName, role, password,
                mustChange: true);

            WriteAudit(ctx, http, "user.create", "ok", target: user.Id,
                detail: new { role = user.Role, generatedPassword = generated });

            return Results.Json(new CreateUserResponseDto(
                ToView(user),
                generated ? password : null), ApiJson.Options);
        }).RequirePermission(Permissions.UserManage);

        app.MapPut("/api/v1/users/{id}", async (HttpContext http, string id, CancellationToken ct) =>
        {
            var auth = RequireAuth(ctx);
            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = ReadJson<UpdateUserRequestDto>(body)
                          ?? throw ApiException.BadRequest("request.invalid", "body is required");

            var before = auth.Users.Find(id)
                         ?? throw ApiException.NotFound("user.not_found", $"user '{id}' not found");

            var user = auth.Users.Update(id, request.DisplayName, request.Role, request.Enabled);

            // 停用或降级后立刻踢掉旧会话：否则权限变更要等会话自然过期才生效。
            if (!user.Enabled || user.Role != before.Role) auth.Sessions.RevokeUser(id);

            WriteAudit(ctx, http, "user.update", "ok", target: id,
                detail: new
                {
                    before = new { before.DisplayName, before.Role, before.Enabled },
                    after = new { user.DisplayName, user.Role, user.Enabled },
                });

            return Results.Json(ToView(user), ApiJson.Options);
        }).RequirePermission(Permissions.UserManage);

        app.MapPost("/api/v1/users/{id}/reset-password",
            async (HttpContext http, string id, CancellationToken ct) =>
        {
            var auth = RequireAuth(ctx);
            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = ReadJson<ResetPasswordRequestDto>(body)
                          ?? throw ApiException.BadRequest("request.invalid", "body is required");

            var user = auth.Users.Find(id)
                       ?? throw ApiException.NotFound("user.not_found", $"user '{id}' not found");

            var generated = string.IsNullOrWhiteSpace(request.Password);
            var password = generated ? UserStore.GeneratePassword() : request.Password!;
            UserStore.ValidatePassword(password);

            auth.Users.SetPassword(id, password, mustChange: true);
            auth.Sessions.RevokeUser(id);

            WriteAudit(ctx, http, "user.reset_password", "ok", target: id,
                detail: new { generatedPassword = generated });

            return Results.Json(new ResetPasswordResponseDto(
                user.Id, generated ? password : null, MustChangePassword: true), ApiJson.Options);
        }).RequirePermission(Permissions.UserManage);

        app.MapDelete("/api/v1/users/{id}", (HttpContext http, string id) =>
        {
            var auth = RequireAuth(ctx);
            var principal = RequirePrincipal(http);

            if (string.Equals(principal.Id, id, StringComparison.Ordinal))
                throw ApiException.Conflict("user.cannot_delete_self", "you cannot delete your own account");

            auth.Users.Delete(id);
            auth.Sessions.RevokeUser(id);

            WriteAudit(ctx, http, "user.delete", "ok", target: id);

            return Results.NoContent();
        }).RequirePermission(Permissions.UserManage);
    }
}
