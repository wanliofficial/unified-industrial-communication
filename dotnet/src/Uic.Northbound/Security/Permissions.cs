using Microsoft.AspNetCore.Builder;

namespace Uic.Northbound.Security;

/// <summary>
/// 权限名。这里刻意用「资源.动作」而不是布尔开关：
/// 端点标注的是它需要的能力，角色表决定谁有这份能力，两边不必互相认识。
/// </summary>
public static class Permissions
{
    public const string SystemRead = "system.read";
    public const string SystemControl = "system.control";
    public const string DeviceControl = "device.control";
    public const string PointWrite = "point.write";
    public const string ConfigRead = "config.read";
    public const string ConfigWrite = "config.write";
    public const string ConfigRollback = "config.rollback";
    public const string AuditRead = "audit.read";
    public const string UserManage = "user.manage";

    public static readonly IReadOnlyList<string> All = new[]
    {
        SystemRead, SystemControl, DeviceControl, PointWrite,
        ConfigRead, ConfigWrite, ConfigRollback, AuditRead, UserManage,
    };
}

/// <summary>
/// 内置角色。角色只描述「能做什么」，不绑定页面：页面是否出现在导航里
/// 由前端按权限推导，服务端始终按能力校验——隐藏按钮不是授权。
/// </summary>
public static class Roles
{
    public const string Admin = "admin";
    public const string Operator = "operator";
    public const string Viewer = "viewer";
    public const string Auditor = "auditor";

    private static readonly Dictionary<string, IReadOnlyList<string>> Table =
        new(StringComparer.Ordinal)
        {
            [Admin] = Permissions.All,

            // 现场操作员：能采、能写、能改配置，但不动回滚与账号。
            // 回滚会把整份配置切回旧版本，属于「需要第二个人确认」的动作。
            [Operator] = new[]
            {
                Permissions.SystemRead, Permissions.SystemControl, Permissions.DeviceControl,
                Permissions.PointWrite, Permissions.ConfigRead, Permissions.ConfigWrite,
            },

            // 只读：看设备、点位、配置，不能改任何东西。
            [Viewer] = new[]
            {
                Permissions.SystemRead, Permissions.ConfigRead,
            },

            // 审计员：只多一项「看审计」。不能改配置，也不能改审计文件本身。
            [Auditor] = new[]
            {
                Permissions.SystemRead, Permissions.ConfigRead, Permissions.AuditRead,
            },
        };

    public static IReadOnlyList<string> Names { get; } = Table.Keys.ToArray();

    public static bool IsKnown(string? role) => role is not null && Table.ContainsKey(role);

    public static IReadOnlyList<string> PermissionsOf(string role)
        => Table.TryGetValue(role, out var permissions) ? permissions : Array.Empty<string>();

    /// <summary>
    /// 静态 API Token 的身份。它是给运维脚本用的长期凭据，不是某个人的账号，
    /// 所以不参与「首次改密」，也不需要会话过期。
    /// </summary>
    public static readonly IReadOnlySet<string> ApiTokenPermissions =
        new HashSet<string>(Permissions.All, StringComparer.Ordinal);
}

/// <summary>
/// 端点声明的必需权限。中间件读不到这个标记就拒绝：漏标端点必须是 403，
/// 而不是「忘了标 = 谁都能进」——后者在评审里几乎看不出来。
/// </summary>
[AttributeUsage(AttributeTargets.Class | AttributeTargets.Method)]
public sealed class RequirePermissionAttribute : Attribute
{
    public RequirePermissionAttribute(string permission) => Permission = permission;

    public string Permission { get; }
}

public static class PermissionEndpointExtensions
{
    public static TBuilder RequirePermission<TBuilder>(this TBuilder builder, string permission)
        where TBuilder : IEndpointConventionBuilder
        => builder.WithMetadata(new RequirePermissionAttribute(permission));
}
