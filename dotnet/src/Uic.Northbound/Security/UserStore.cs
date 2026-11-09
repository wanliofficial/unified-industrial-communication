using System.Security.Cryptography;
using System.Text;
using Uic.Domain;

namespace Uic.Northbound.Security;

/// <summary>一个本地账号。口令只保存 PBKDF2 派生值，明文从不落盘。</summary>
public sealed class UserRecord
{
    public string Id { get; init; } = string.Empty;
    public string? DisplayName { get; init; }
    public string Role { get; init; } = Roles.Viewer;
    public string PasswordHash { get; init; } = string.Empty;
    public bool Enabled { get; init; } = true;

    /// <summary>首次登录必须改密。初始口令与管理员重置后的口令都带这个标记。</summary>
    public bool MustChangePassword { get; init; }

    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;
    public DateTimeOffset? PasswordChangedAt { get; init; }
    public DateTimeOffset? LastLoginAt { get; init; }
}

/// <summary>磁盘形状。单独一层是为了以后能加字段而不动账号逻辑。</summary>
public sealed class UserFile
{
    public int Version { get; init; } = 1;
    public List<UserRecord> Users { get; init; } = new();
}

/// <summary>
/// 本地账号库：<c>data/security/users.json</c>。
///
/// 为什么不用数据库：网关要能拷一个二进制到盒子上就跑，账号数量是个位数，
/// 一个 JSON 文件足够；为什么不放进配置版本：账号是运行期状态，
/// 回滚配置不应该把口令也回滚回去。
///
/// 口令派生用 PBKDF2-SHA256（每账号独立随机盐）。没有引入外部库，
/// 因为框架自带的 <see cref="Rfc2898DeriveBytes"/> 已经够用，也少一个供应链面。
/// </summary>
public sealed class UserStore
{
    /// <summary>迭代次数。取一个在 ARM 网关上约 100 ms 的量级：足够拖慢离线爆破，又不至于让登录明显变慢。</summary>
    private const int Iterations = 210_000;
    private const int SaltBytes = 16;
    private const int HashBytes = 32;

    /// <summary>初始口令的字符集刻意去掉 0/O、1/l/I：运维要能从屏幕上抄进终端而不猜。</summary>
    private const string PasswordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";

    private readonly object _gate = new();
    private readonly string _root;
    private readonly string _path;
    private readonly List<UserRecord> _users;

    public UserStore(string dataDir, Action<string>? warn = null)
    {
        _root = Path.Combine(dataDir, "security");
        _path = Path.Combine(_root, "users.json");

        Directory.CreateDirectory(_root);
        TryRestrictDirectory(_root, warn);

        if (File.Exists(_path))
        {
            _users = Load();
            return;
        }

        // 首次启动没有账号库：生成一个随机 admin 口令，写进只有属主可读的文件。
        // 口令不打印进日志——日志会被收集、转发、贴进工单。
        _users = new List<UserRecord>();
        var password = GeneratePassword(16);

        _users.Add(new UserRecord
        {
            Id = "admin",
            DisplayName = "管理员",
            Role = Roles.Admin,
            PasswordHash = Hash(password),
            Enabled = true,
            MustChangePassword = true,
            CreatedAt = DateTimeOffset.UtcNow,
        });

        Save();

        InitialPasswordPath = Path.Combine(_root, "initial-admin-password.txt");
        File.WriteAllText(InitialPasswordPath,
            $"user: admin\npassword: {password}\n" +
            "这个口令只在首次启动时生成；登录后必须立即修改，改密成功后本文件会被删除。\n");
        TryRestrictFile(InitialPasswordPath, warn);

        CreatedInitialAdmin = true;
    }

    /// <summary>首次启动生成的口令文件；没有生成过时为 null。</summary>
    public string? InitialPasswordPath { get; }

    /// <summary>本次进程是否刚创建了 admin 账号（用于启动日志提示）。</summary>
    public bool CreatedInitialAdmin { get; }

    public string Root => _root;

    public IReadOnlyList<UserRecord> All()
    {
        lock (_gate) return _users.Select(Clone).ToArray();
    }

    public UserRecord? Find(string id)
    {
        lock (_gate)
        {
            var user = _users.FirstOrDefault(u => string.Equals(u.Id, id, StringComparison.Ordinal));
            return user is null ? null : Clone(user);
        }
    }

    /// <summary>
    /// 校验口令。找不到用户与口令错误返回同一个失败：不要让攻击者用响应区分
    /// 「这个账号存在」。禁用账号单独区分，因为运维需要知道「不是口令忘了，是账号被停了」。
    /// </summary>
    public LoginCheck CheckPassword(string username, string password)
    {
        UserRecord? user;
        lock (_gate)
        {
            user = _users.FirstOrDefault(u => string.Equals(u.Id, username, StringComparison.Ordinal));
        }

        // 账号不存在时也跑一次派生：否则响应时间会泄露账号是否存在。
        if (user is null)
        {
            Verify(password, Hash("dummy-" + username));
            return LoginCheck.NoSuchUser;
        }

        if (!Verify(password, user.PasswordHash)) return LoginCheck.BadPassword;
        if (!user.Enabled) return LoginCheck.Disabled;

        return LoginCheck.Ok;
    }

    public void RecordLogin(string id, DateTimeOffset at)
    {
        lock (_gate)
        {
            var index = IndexOf(id);
            if (index < 0) return;

            _users[index] = Copy(_users[index], lastLoginAt: at);
            Save();
        }
    }

    public UserRecord Create(string id, string? displayName, string role, string password, bool mustChange)
    {
        ValidateId(id);
        if (!Roles.IsKnown(role))
            throw ApiException.BadRequest("user.invalid_role",
                $"unknown role '{role}'; expected one of {string.Join(", ", Roles.Names)}");

        var record = new UserRecord
        {
            Id = id,
            DisplayName = displayName,
            Role = role,
            PasswordHash = Hash(password),
            Enabled = true,
            MustChangePassword = mustChange,
            CreatedAt = DateTimeOffset.UtcNow,
            PasswordChangedAt = mustChange ? null : DateTimeOffset.UtcNow,
        };

        lock (_gate)
        {
            if (IndexOf(id) >= 0)
                throw ApiException.Conflict("user.exists", $"user '{id}' already exists");

            _users.Add(record);
            Save();
        }

        return Clone(record);
    }

    public UserRecord Update(string id, string? displayName, string? role, bool? enabled)
    {
        lock (_gate)
        {
            var index = IndexOf(id);
            if (index < 0) throw ApiException.NotFound("user.not_found", $"user '{id}' not found");

            var current = _users[index];

            if (role is not null && !Roles.IsKnown(role))
                throw ApiException.BadRequest("user.invalid_role",
                    $"unknown role '{role}'; expected one of {string.Join(", ", Roles.Names)}");

            // 不允许把最后一个可用的管理员降级或停用：否则下一次改配置就没人有权限了，
            // 只能停机改文件。这条约束在服务端强制，不能指望 UI 记得隐藏按钮。
            var losingAdmin = current.Role == Roles.Admin && current.Enabled &&
                              (role is not null && role != Roles.Admin || enabled == false);

            if (losingAdmin && CountEnabledAdmins() <= 1)
                throw ApiException.Conflict("user.last_admin",
                    "cannot demote or disable the last enabled administrator");

            _users[index] = Copy(current,
                displayName: displayName ?? current.DisplayName,
                role: role ?? current.Role,
                enabled: enabled ?? current.Enabled);

            Save();
            return Clone(_users[index]);
        }
    }

    /// <summary>设置新口令。管理员重置时 <paramref name="mustChange"/> 为 true，用户自己改密为 false。</summary>
    public void SetPassword(string id, string password, bool mustChange)
    {
        lock (_gate)
        {
            var index = IndexOf(id);
            if (index < 0) throw ApiException.NotFound("user.not_found", $"user '{id}' not found");

            _users[index] = Copy(_users[index],
                passwordHash: Hash(password),
                mustChange: mustChange,
                passwordChangedAt: DateTimeOffset.UtcNow);

            Save();
        }
    }

    public void Delete(string id)
    {
        lock (_gate)
        {
            var index = IndexOf(id);
            if (index < 0) throw ApiException.NotFound("user.not_found", $"user '{id}' not found");

            if (_users[index].Role == Roles.Admin && _users[index].Enabled && CountEnabledAdmins() <= 1)
                throw ApiException.Conflict("user.last_admin",
                    "cannot delete the last enabled administrator");

            _users.RemoveAt(index);
            Save();
        }
    }

    /// <summary>改密成功后删除初始口令文件：它还留在磁盘上等于口令从未改过。</summary>
    public void ClearInitialPasswordFile()
    {
        if (InitialPasswordPath is null) return;

        try
        {
            if (File.Exists(InitialPasswordPath)) File.Delete(InitialPasswordPath);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            // 删不掉不影响改密本身，但要让运维知道「那个文件还在」。
            Console.Error.WriteLine(
                $"warning: could not remove {InitialPasswordPath}: {ex.Message}; delete it manually");
        }
    }

    public static string GeneratePassword(int length = 16)
    {
        var chars = new char[length];
        for (var i = 0; i < length; i++)
            chars[i] = PasswordAlphabet[RandomNumberGenerator.GetInt32(PasswordAlphabet.Length)];

        return new string(chars);
    }

    /// <summary>口令策略：至少 8 位。上限防止一次登录把 CPU 占满（PBKDF2 的输入长度也会影响时间）。</summary>
    public static void ValidatePassword(string? password, string? current = null)
    {
        if (string.IsNullOrWhiteSpace(password) || password.Length < 8)
            throw ApiException.BadRequest("auth.weak_password", "password must be at least 8 characters");

        if (password.Length > 128)
            throw ApiException.BadRequest("auth.weak_password", "password must be at most 128 characters");

        if (current is not null && string.Equals(password, current, StringComparison.Ordinal))
            throw ApiException.BadRequest("auth.weak_password", "new password must differ from the current one");
    }

    // ------------------------------------------------------------------
    // 口令派生
    // ------------------------------------------------------------------

    private static string Hash(string password)
    {
        var salt = RandomNumberGenerator.GetBytes(SaltBytes);
        var hash = Rfc2898DeriveBytes.Pbkdf2(
            Encoding.UTF8.GetBytes(password), salt, Iterations, HashAlgorithmName.SHA256, HashBytes);

        return $"pbkdf2-sha256${Iterations}${Convert.ToBase64String(salt)}${Convert.ToBase64String(hash)}";
    }

    private static bool Verify(string password, string encoded)
    {
        var parts = encoded.Split('$');
        if (parts.Length != 4 || parts[0] != "pbkdf2-sha256") return false;
        if (!int.TryParse(parts[1], out var iterations) || iterations <= 0) return false;

        byte[] salt, expected;
        try
        {
            salt = Convert.FromBase64String(parts[2]);
            expected = Convert.FromBase64String(parts[3]);
        }
        catch (FormatException)
        {
            return false;
        }

        var actual = Rfc2898DeriveBytes.Pbkdf2(
            Encoding.UTF8.GetBytes(password), salt, iterations, HashAlgorithmName.SHA256, expected.Length);

        return CryptographicOperations.FixedTimeEquals(actual, expected);
    }

    // ------------------------------------------------------------------
    // 持久化
    // ------------------------------------------------------------------

    private List<UserRecord> Load()
    {
        try
        {
            var file = JsonDefaults.FromJson<UserFile>(File.ReadAllText(_path));
            if (file?.Users is { Count: > 0 }) return file.Users;

            throw new InvalidDataException("the file contains no users");
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or System.Text.Json.JsonException
                                       or InvalidDataException)
        {
            // 账号库坏了不能悄悄重建：那会把现有账号全部抹掉，再用一个新的随机口令
            // 把运维挡在门外。直接让启动失败，保留现场。
            throw new ConfigException(
                $"cannot read user store '{_path}'",
                new[] { new ValidationProblem("security.users", ex.Message) });
        }
    }

    private void Save()
    {
        var json = JsonDefaults.ToJson(new UserFile { Users = _users }, indented: true);
        var temp = _path + ".tmp";

        // 先写临时文件再原子替换：进程在写一半时被杀，账号库仍然是上一份完整内容。
        File.WriteAllText(temp, json);
        TryRestrictFile(temp, null);
        File.Move(temp, _path, overwrite: true);
    }

    private int IndexOf(string id)
        => _users.FindIndex(u => string.Equals(u.Id, id, StringComparison.Ordinal));

    private int CountEnabledAdmins()
        => _users.Count(u => u.Enabled && u.Role == Roles.Admin);

    private static void ValidateId(string id)
    {
        if (string.IsNullOrWhiteSpace(id) || id.Length > 64)
            throw ApiException.BadRequest("user.invalid_id", "user id must be 1..64 characters");

        foreach (var c in id)
        {
            if (!char.IsLetterOrDigit(c) && c is not ('.' or '_' or '-'))
                throw ApiException.BadRequest("user.invalid_id",
                    "user id may only contain letters, digits, '.', '_' or '-'");
        }
    }

    private static UserRecord Clone(UserRecord user) => Copy(user);

    private static UserRecord Copy(
        UserRecord user,
        string? displayName = null,
        string? role = null,
        bool? enabled = null,
        string? passwordHash = null,
        bool? mustChange = null,
        DateTimeOffset? passwordChangedAt = null,
        DateTimeOffset? lastLoginAt = null)
        => new()
        {
            Id = user.Id,
            DisplayName = displayName ?? user.DisplayName,
            Role = role ?? user.Role,
            PasswordHash = passwordHash ?? user.PasswordHash,
            Enabled = enabled ?? user.Enabled,
            MustChangePassword = mustChange ?? user.MustChangePassword,
            CreatedAt = user.CreatedAt,
            PasswordChangedAt = passwordChangedAt ?? user.PasswordChangedAt,
            LastLoginAt = lastLoginAt ?? user.LastLoginAt,
        };

    private static void TryRestrictDirectory(string path, Action<string>? warn)
    {
        if (OperatingSystem.IsWindows()) return;

        const UnixFileMode ownerOnly =
            UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute;

        try
        {
            File.SetUnixFileMode(path, ownerOnly);
            if (File.GetUnixFileMode(path) != ownerOnly)
            {
                warn?.Invoke(
                    $"{path} does not keep unix permissions (the filesystem ignores them); " +
                    "the user store may be readable by other local users");
            }
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or PlatformNotSupportedException)
        {
            warn?.Invoke($"could not restrict permissions on {path}: {ex.Message}");
        }
    }

    internal static void TryRestrictFile(string path, Action<string>? warn)
    {
        if (OperatingSystem.IsWindows()) return;

        const UnixFileMode ownerOnly = UnixFileMode.UserRead | UnixFileMode.UserWrite;

        try
        {
            File.SetUnixFileMode(path, ownerOnly);
            if (File.GetUnixFileMode(path) != ownerOnly)
            {
                warn?.Invoke(
                    $"{path} does not keep unix permissions (the filesystem ignores them); " +
                    "any local user may read the initial password");
            }
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or PlatformNotSupportedException)
        {
            warn?.Invoke($"could not restrict permissions on {path}: {ex.Message}");
        }
    }
}

public enum LoginCheck
{
    Ok,
    NoSuchUser,
    BadPassword,
    Disabled,
}
