namespace Uic.Ui;

/// <summary>
/// 前端使用的权限名。刻意与服务端 <c>Permissions</c> 保持同名字符串而不是引用服务端程序集：
/// 控制台是纯前端产物，把整个北向程序集拖进 WASM 只为了九个常量不划算。
/// 名字对不上时最坏结果是「按钮不显示」，而不是越权——服务端始终按自己的能力表校验。
/// </summary>
public static class UiPermissions
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
}

/// <summary>
/// 当前会话的身份与权限快照。
///
/// 只缓存 <c>/auth/me</c> 的结果，不在前端推断角色：服务端已经算好了有效权限集合，
/// 前端照着集合显示即可。用户改了角色但没重新登录时，服务端会在下一次 401/403 时
/// 把页面踢回登录——隐藏按钮从来不是授权边界。
/// </summary>
public static class Session
{
    private static readonly HashSet<string> Granted = new(StringComparer.Ordinal);

    public static MeDto? Current { get; private set; }

    public static bool IsAuthenticated => Current is not null;

    public static string Id => Current?.Id ?? "anonymous";

    public static string Role => Current?.Role ?? "anonymous";

    public static string DisplayName => Current?.DisplayName is { Length: > 0 } name ? name : Id;

    public static bool MustChangePassword => Current?.MustChangePassword ?? false;

    public static bool IsAdmin => string.Equals(Role, "admin", StringComparison.Ordinal);

    public static void Set(MeDto me)
    {
        Current = me;

        Granted.Clear();
        foreach (var permission in me.Permissions) Granted.Add(permission);
    }

    public static void Clear()
    {
        Current = null;
        Granted.Clear();
    }

    public static bool Can(string permission) => Granted.Contains(permission);
}
