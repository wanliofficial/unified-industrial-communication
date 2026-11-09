using Uic.Domain;
using Uic.Northbound;
using Uic.Northbound.Security;

namespace Uic.Tests;

/// <summary>
/// 本地账号与会话（FR-OPS-02 / FR-OPS-03）。
///
/// 这些用例只碰安全层，不起 HTTP：口令派生、初始口令、会话过期、锁定与角色表
/// 都是纯逻辑，单独测能更快定位问题；跨 HTTP 的授权行为在 contract/auth_* 里验。
/// </summary>
internal static class AuthTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("auth/passwords_are_salted_and_never_stored", PasswordsAreSaltedAndNeverStored));
        tests.Add(new TestCase("auth/bootstrap_creates_admin_once", BootstrapCreatesAdminOnce));
        tests.Add(new TestCase("auth/password_change_clears_must_change", PasswordChangeClearsMustChange));
        tests.Add(new TestCase("auth/session_expires_after_12h", SessionExpiresAfter12Hours));
        tests.Add(new TestCase("auth/lockout_after_five_failures", LockoutAfterFiveFailures));
        tests.Add(new TestCase("auth/role_matrix", RoleMatrix));
        tests.Add(new TestCase("auth/disabled_user_loses_sessions", DisabledUserLosesSessions));
        tests.Add(new TestCase("auth/last_admin_is_protected", LastAdminIsProtected));
        tests.Add(new TestCase("auth/weak_password_is_rejected", WeakPasswordIsRejected));
    }

    private static string InitialPassword(UserStore store)
    {
        var path = store.InitialPasswordPath ?? throw new CheckFailedException("no initial password file");
        var line = File.ReadAllLines(path).FirstOrDefault(l => l.StartsWith("password:", StringComparison.Ordinal));

        return line is null
            ? throw new CheckFailedException("the initial password file has no password line")
            : line["password:".Length..].Trim();
    }

    private static Task PasswordsAreSaltedAndNeverStored(TestContext t)
    {
        var store = new UserStore(t.TempDir);
        var password = "same-password-42";

        store.Create("alice", null, Roles.Operator, password, mustChange: true);
        store.Create("bob", null, Roles.Viewer, password, mustChange: true);

        var json = File.ReadAllText(Path.Combine(store.Root, "users.json"));

        t.Ok(!json.Contains(password, StringComparison.Ordinal),
            "the plaintext password never appears in the user file");
        t.Contains(json, "pbkdf2-sha256$", "the file stores a PBKDF2 hash");

        var alice = store.Find("alice")!;
        var bob = store.Find("bob")!;

        t.Ok(alice.PasswordHash != bob.PasswordHash,
            "the same password produces different hashes because each account has its own salt");

        t.Equal(LoginCheck.Ok, store.CheckPassword("alice", password), "the password verifies");
        t.Equal(LoginCheck.BadPassword, store.CheckPassword("alice", password + "x"),
            "a wrong password does not verify");
        t.Equal(LoginCheck.NoSuchUser, store.CheckPassword("nobody", password),
            "an unknown account is reported as such internally");
        return Task.CompletedTask;
    }

    private static Task BootstrapCreatesAdminOnce(TestContext t)
    {
        var first = new UserStore(t.TempDir);

        t.Ok(first.CreatedInitialAdmin, "the first start creates the admin account");
        t.Ok(first.InitialPasswordPath is not null && File.Exists(first.InitialPasswordPath),
            "the initial password is written to a file");

        var password = InitialPassword(first);
        t.Ok(password.Length >= 16, "the generated password is long enough");

        var admin = first.Find("admin");
        t.Ok(admin is not null, "the admin account exists");
        t.Equal(Roles.Admin, admin!.Role, "the bootstrap account is an administrator");
        t.Ok(admin.MustChangePassword, "the bootstrap password must be changed on first login");

        var second = new UserStore(t.TempDir);
        t.Ok(!second.CreatedInitialAdmin, "a restart does not create another admin");
        t.Equal(LoginCheck.Ok, second.CheckPassword("admin", password),
            "the initial password survives a restart");
        return Task.CompletedTask;
    }

    private static Task PasswordChangeClearsMustChange(TestContext t)
    {
        var store = new UserStore(t.TempDir);
        var initial = InitialPassword(store);

        store.SetPassword("admin", "a-new-password-9", mustChange: false);
        store.ClearInitialPasswordFile();

        var admin = store.Find("admin")!;
        t.Ok(!admin.MustChangePassword, "changing the password clears the forced-change flag");
        t.Ok(admin.PasswordChangedAt is not null, "the change time is recorded");
        t.Equal(LoginCheck.Ok, store.CheckPassword("admin", "a-new-password-9"), "the new password works");
        t.Equal(LoginCheck.BadPassword, store.CheckPassword("admin", initial), "the initial password stops working");
        t.Ok(store.InitialPasswordPath is null || !File.Exists(store.InitialPasswordPath),
            "the initial password file is removed");
        return Task.CompletedTask;
    }

    private static Task SessionExpiresAfter12Hours(TestContext t)
    {
        var auth = new AuthService(t.TempDir);
        var now = DateTimeOffset.UtcNow;

        var (token, session) = auth.Sessions.Issue("admin", now);

        t.Equal(now + TimeSpan.FromHours(12), session.ExpiresAt, "the session lasts 12 hours");
        t.Ok(auth.Sessions.Find(token, now.AddHours(11)) is not null, "the session is valid before expiry");
        t.Ok(auth.Sessions.Find(token, now.AddHours(12)) is null, "the session is gone at expiry");
        t.Ok(auth.Sessions.Find(token, now.AddHours(13)) is null, "and stays gone");
        return Task.CompletedTask;
    }

    private static Task LockoutAfterFiveFailures(TestContext t)
    {
        var auth = new AuthService(t.TempDir);
        var password = InitialPassword(auth.Users);
        var now = DateTimeOffset.UtcNow;

        for (var i = 0; i < 5; i++)
        {
            t.Throws<ApiException>(
                () => auth.Login("admin", "wrong-" + i, "10.0.0.9", now),
                "a bad password is rejected");
        }

        t.Ok(auth.LockedUntil("10.0.0.9", now) is not null,
            "five failures within a minute lock the address");

        var locked = t.Throws<ApiException>(
            () => auth.Login("admin", password, "10.0.0.9", now),
            "even the correct password is refused while locked");

        t.Equal(429, locked.Status, "the lockout is a 429, not a 401");

        // 另一个来源不应被连坐：共享出口的爆破不该锁死所有办公区。
        t.Ok(auth.Login("admin", password, "10.0.0.10", now).Token.Length > 0,
            "a different address can still log in");

        auth.ResetThrottleForTests();
        t.Ok(auth.Login("admin", password, "10.0.0.9", now.AddSeconds(61)).Token.Length > 0,
            "after the lockout window the address can log in again");
        return Task.CompletedTask;
    }

    private static Task RoleMatrix(TestContext t)
    {
        t.Equal(Permissions.All.Count, Roles.PermissionsOf(Roles.Admin).Count,
            "admin has every permission");

        var operatorRole = Roles.PermissionsOf(Roles.Operator);
        t.Ok(operatorRole.Contains(Permissions.PointWrite), "operator can write points");
        t.Ok(operatorRole.Contains(Permissions.ConfigWrite), "operator can apply configuration");
        t.Ok(!operatorRole.Contains(Permissions.ConfigRollback), "operator cannot roll back");
        t.Ok(!operatorRole.Contains(Permissions.UserManage), "operator cannot manage users");

        var viewer = Roles.PermissionsOf(Roles.Viewer);
        t.Ok(viewer.Contains(Permissions.SystemRead), "viewer can read the system");
        t.Ok(!viewer.Contains(Permissions.PointWrite), "viewer cannot write points");

        var auditor = Roles.PermissionsOf(Roles.Auditor);
        t.Ok(auditor.Contains(Permissions.AuditRead), "auditor can read the audit log");
        t.Ok(!auditor.Contains(Permissions.ConfigWrite), "auditor cannot change configuration");

        t.Ok(!Roles.IsKnown("root"), "unknown roles are rejected");
        t.Equal(0, Roles.PermissionsOf("root").Count, "an unknown role grants nothing");
        return Task.CompletedTask;
    }

    private static Task DisabledUserLosesSessions(TestContext t)
    {
        var auth = new AuthService(t.TempDir);
        auth.Users.Create("alice", null, Roles.Operator, "alice-password-1", mustChange: false);

        var now = DateTimeOffset.UtcNow;
        var login = auth.Login("alice", "alice-password-1", "10.0.0.11", now);
        t.Ok(auth.Resolve(login.Token, now) is not null, "the session resolves while the account is enabled");

        auth.Users.Update("alice", null, null, enabled: false);

        t.Equal(LoginCheck.Disabled, auth.Users.CheckPassword("alice", "alice-password-1"),
            "a disabled account cannot log in");

        t.Ok(auth.Resolve(login.Token, now) is null,
            "an existing session stops resolving as soon as the account is disabled");
        return Task.CompletedTask;
    }

    private static Task LastAdminIsProtected(TestContext t)
    {
        var store = new UserStore(t.TempDir);

        var conflict = t.Throws<ApiException>(
            () => store.Update("admin", null, Roles.Viewer, null),
            "the last enabled admin cannot be demoted");

        t.Equal(409, conflict.Status, "the refusal is a conflict");

        store.Create("admin2", null, Roles.Admin, "second-admin-1", mustChange: true);

        var demoted = store.Update("admin", null, Roles.Viewer, null);
        t.Equal(Roles.Viewer, demoted.Role, "with a second admin the first one can be demoted");

        t.Throws<ApiException>(() => store.Delete("admin2"), "the remaining admin cannot be deleted");
        return Task.CompletedTask;
    }

    private static Task WeakPasswordIsRejected(TestContext t)
    {
        t.Throws<ApiException>(() => UserStore.ValidatePassword("short"),
            "passwords shorter than eight characters are rejected");
        t.Throws<ApiException>(() => UserStore.ValidatePassword("same-password", "same-password"),
            "the new password must differ from the current one");

        UserStore.ValidatePassword("long-enough-1");
        return Task.CompletedTask;
    }
}
