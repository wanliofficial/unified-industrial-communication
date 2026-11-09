using System.Collections.Concurrent;
using System.Security.Cryptography;
using System.Text;

namespace Uic.Northbound.Security;

/// <summary>一个登录会话。Token 只保存在进程内存：重启即失效，符合「边缘盒子重启后重新登录」的预期。</summary>
public sealed record Session(string UserId, DateTimeOffset CreatedAt, DateTimeOffset ExpiresAt);

/// <summary>
/// 会话表。按 Token 的 SHA-256 建索引而不是用原始 Token 当 key：
/// 进程内存被导出的概率不高，但把可直接使用的凭据铺在字典键上没有任何好处。
/// </summary>
public sealed class SessionStore
{
    /// <summary>会话有效期。PRD / API 规范要求 12 小时，不做滚动续期——长时间无人值守的浏览器标签页不该永久有效。</summary>
    public static readonly TimeSpan Lifetime = TimeSpan.FromHours(12);

    private readonly ConcurrentDictionary<string, Session> _sessions = new(StringComparer.Ordinal);

    public int Count => _sessions.Count;

    public (string Token, Session Session) Issue(string userId, DateTimeOffset now)
    {
        // 32 字节随机量：足够长，无法枚举；base64url 方便放进 Authorization 头。
        var token = Convert.ToBase64String(RandomNumberGenerator.GetBytes(32))
            .TrimEnd('=').Replace('+', '-').Replace('/', '_');

        var session = new Session(userId, now, now + Lifetime);
        _sessions[KeyOf(token)] = session;
        Prune(now);
        return (token, session);
    }

    public Session? Find(string token, DateTimeOffset now)
    {
        var key = KeyOf(token);
        if (!_sessions.TryGetValue(key, out var session)) return null;

        if (session.ExpiresAt <= now)
        {
            _sessions.TryRemove(key, out _);
            return null;
        }

        return session;
    }

    public void Revoke(string token) => _sessions.TryRemove(KeyOf(token), out _);

    /// <summary>用户被禁用或改密时踢掉其全部会话：改完口令旧 Token 还能用，等于没改。</summary>
    public void RevokeUser(string userId)
    {
        foreach (var (key, session) in _sessions)
        {
            if (string.Equals(session.UserId, userId, StringComparison.Ordinal))
                _sessions.TryRemove(key, out _);
        }
    }

    public void Clear() => _sessions.Clear();

    private void Prune(DateTimeOffset now)
    {
        if (_sessions.Count < 512) return;

        foreach (var (key, session) in _sessions)
        {
            if (session.ExpiresAt <= now) _sessions.TryRemove(key, out _);
        }
    }

    private static string KeyOf(string token)
        => Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(token)));
}
