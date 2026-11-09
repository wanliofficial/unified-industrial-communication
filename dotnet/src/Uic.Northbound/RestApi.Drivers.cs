using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Domain;
using Uic.Driver;
using Uic.Northbound.Security;

namespace Uic.Northbound;

/// <summary>
/// 驱动元数据。控制台靠它渲染四态能力徽标，接入方也据此判断「这个协议到底会不会写」。
///
/// 能力是四态而不是布尔：Symbol 寻址在 A 型号支持、B 型号不支持，用 true/false 只能撒谎。
/// </summary>
public static partial class RestApi
{
    private static void MapDrivers(WebApplication app, NorthboundContext ctx)
    {
        app.MapGet("/api/v1/drivers", () =>
        {
            var items = ctx.Engine.Drivers.List().Select(ToDriverView).ToArray();
            return Results.Json(new DriversDto(items), ApiJson.Options);
        }).RequirePermission(Permissions.SystemRead);
    }

    private static DriverDto ToDriverView(DriverMeta meta) => new(
        meta.Id,
        meta.Name,
        meta.Version,
        meta.SdkVersion,
        meta.Roles,
        meta.Transports,
        meta.AddressKinds,
        meta.Capabilities,
        meta.Description);
}

/// <summary>驱动列表。数量很小，不分页。</summary>
public sealed record DriversDto(IReadOnlyList<DriverDto> Items);

public sealed record DriverDto(
    string Id,
    string Name,
    string Version,
    string SdkVersion,
    IReadOnlyList<DriverRole> Roles,
    IReadOnlyList<string> Transports,
    IReadOnlyList<AddressKind> AddressKinds,
    Capabilities Capabilities,
    string? Description);
