namespace Uic.Northbound.Security;

/// <summary>
/// 已认证的调用者。所有授权判断只看这里，不再回头读 HTTP 头——
/// 中间件解析一次，处理器只认这个对象，避免「某个端点又自己解析了一遍令牌」。
/// </summary>
public sealed record Principal(
    string Id,
    string? DisplayName,
    string Role,
    IReadOnlySet<string> Permissions,
    bool MustChangePassword,
    bool IsApiToken)
{
    public bool Can(string permission) => Permissions.Contains(permission);
}

public sealed record LoginResult(string Token, DateTimeOffset ExpiresAt, UserRecord User);

/// <summary>
/// 认证服务：账号校验、会话签发、登录限流。
///
/// 登录限流按来源 IP 计 5 次失败/分钟，超限锁定 60 秒。为什么按 IP 而不是按账号：
/// 按账号锁会被用来恶意锁死管理员；按 IP 至少要求攻击者换源，代价更高。
/// 成功登录会清空该 IP 的失败计数，避免共享出口的正常用户被别人的爆破连累太久。
/// </summary>
public sealed class AuthService
{
    private static readonly TimeSpan FailureWindow = TimeSpan.FromMinutes(1);
    private static readonly TimeSpan Lockout = TimeSpan.FromSeconds(60);
    private const int MaxFailures = 5;

    private readonly object _throttleGate = new();
    private readonly Dictionary<string, List<long>> _failures = new(StringComparer.Ordinal);
    private readonly TimeProvider _time;

    /// <param name="time">锁定窗口的时间来源，只用于<b>单调</b>的时长判定。</param>
    public AuthService(string dataDir, Action<string>? warn = null, TimeProvider? time = null)
    {
        Users = new UserStore(dataDir, warn);
        _time = time ?? TimeProvider.System;
    }

    public UserStore Users { get; }

    public SessionStore Sessions { get; } = new();

    /// <summary>
    /// 用口令换会话。失败抛 <see cref="ApiException"/>，让 HTTP 层与审计都拿到统一语义。
    /// </summary>
    public LoginResult Login(string? username, string? password, string remote, DateTimeOffset now)
    {
        var key = string.IsNullOrWhiteSpace(remote) ? "unknown" : remote;

        if (LockedUntil(key, now) is { } until)
        {
            throw new ApiException(429, "request.rate_limited",
                $"too many failed logins; try again after {until:HH:mm:ss}Z",
                new { retryAfterSeconds = (int)Math.Ceiling((until - now).TotalSeconds) });
        }

        if (string.IsNullOrWhiteSpace(username) || string.IsNullOrEmpty(password))
            throw ApiException.BadRequest("auth.missing_credentials", "username and password are required");

        var check = Users.CheckPassword(username, password);
        switch (check)
        {
            case LoginCheck.Ok:
                RecordSuccess(key);
                Users.RecordLogin(username, now);
                var (token, session) = Sessions.Issue(username, now);

                // 重新读一次：登录时间刚被写进去，返回给客户端的应当是当前记录。
                var user = Users.Find(username)!;
                return new LoginResult(token, session.ExpiresAt, user);

            case LoginCheck.Disabled:
                RecordFailure(key);
                throw ApiException.Forbidden("auth.user_disabled", $"user '{username}' is disabled");

            default:
                RecordFailure(key);
                // 不区分「用户不存在」与「口令错误」：错误信息本身会变成一个账号枚举接口。
                throw new ApiException(401, "auth.invalid_credentials", "invalid username or password");
        }
    }

    public Principal? Resolve(string? token, DateTimeOffset now)
    {
        if (string.IsNullOrWhiteSpace(token)) return null;

        var session = Sessions.Find(token, now);
        if (session is null) return null;

        var user = Users.Find(session.UserId);
        if (user is null || !user.Enabled)
        {
            // 用户被删除或停用后，旧 Token 立即失效，不等 12 小时自然过期。
            Sessions.Revoke(token);
            return null;
        }

        return new Principal(
            user.Id,
            user.DisplayName,
            user.Role,
            new HashSet<string>(Roles.PermissionsOf(user.Role), StringComparer.Ordinal),
            user.MustChangePassword,
            IsApiToken: false);
    }

    public void Logout(string? token)
    {
        if (!string.IsNullOrWhiteSpace(token)) Sessions.Revoke(token);
    }

    /// <summary>当前 IP 的锁定截止时间；未锁定时为 null。</summary>
    public DateTimeOffset? LockedUntil(string key, DateTimeOffset now)
    {
        lock (_throttleGate)
        {
            if (!_failures.TryGetValue(key, out var attempts)) return null;

            var monoNow = _time.GetTimestamp();
            attempts.RemoveAll(t => _time.GetElapsedTime(t, monoNow) > FailureWindow);
            if (attempts.Count < MaxFailures)
            {
                if (attempts.Count == 0) _failures.Remove(key);
                return null;
            }

            // 锁定到期用单调时钟算「还剩多久」，再换算成墙钟时刻返回给调用方。
            // 用墙钟算剩余时间，一次校时就能凭空改变锁定的长短。
            var remaining = Lockout - _time.GetElapsedTime(attempts[^1], monoNow);
            return remaining > TimeSpan.Zero ? now.Add(remaining) : null;
        }
    }

    private void RecordFailure(string key)
    {
        lock (_throttleGate)
        {
            if (!_failures.TryGetValue(key, out var attempts))
            {
                attempts = new List<long>();
                _failures[key] = attempts;
            }

            // 只存单调时刻：墙钟在这里出现一次，整条锁定逻辑就跟着校时一起撒谎。
            var monoNow = _time.GetTimestamp();
            attempts.RemoveAll(t => _time.GetElapsedTime(t, monoNow) > FailureWindow);
            attempts.Add(monoNow);

            // 失败记录是攻击者可控的增长点，必须有上限。
            if (_failures.Count > 4096)
            {
                foreach (var stale in _failures.Where(p => _time.GetElapsedTime(p.Value[^1], monoNow) > FailureWindow)
                             .Select(p => p.Key).ToArray())
                {
                    _failures.Remove(stale);
                }
            }
        }
    }

    private void RecordSuccess(string key)
    {
        lock (_throttleGate) _failures.Remove(key);
    }

}
