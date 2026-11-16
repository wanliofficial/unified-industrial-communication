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
        // 没有声明的驱动不输出空数组：null 在契约里统一表示「没有这一项」，
        // 免得接入方要区分「空数组」和「字段缺失」两种同样无内容的状态。
        meta.ConfigFields.Count == 0 ? null : meta.ConfigFields.Select(ToFieldView).ToArray(),
        meta.PointFields.Count == 0 ? null : meta.PointFields.Select(ToFieldView).ToArray(),
        meta.Description);

    private static FieldDto ToFieldView(ConfigField field) => new(
        // SDK 内部叫 Name，线上叫 key：配置树里的参数名就是 labels.xxx / address.xxx，
        // 接入方拿它直接写配置；叫 name 会和「驱动的 Name」混淆。
        field.Name,
        field.Label,
        field.Type,
        field.Required,
        field.Default,
        field.Enum,
        field.Placeholder,
        field.Help,
        field.Scope);
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
    IReadOnlyList<FieldDto>? ConfigFields,
    IReadOnlyList<FieldDto>? PointFields,
    string? Description);

/// <summary>
/// 一条表单元数据。由驱动自己声明，网关只做投影：控制台的「驱动」页与外部接入方
/// 据此知道某个协议要填哪些参数、哪些必填、默认值是多少，不必各自抄一份协议手册。
/// </summary>
public sealed record FieldDto(
    string Key,
    string Label,
    string Type,
    bool Required,
    object? Default,
    IReadOnlyList<string>? Enum,
    string? Placeholder,
    string? Help,
    string Scope);
