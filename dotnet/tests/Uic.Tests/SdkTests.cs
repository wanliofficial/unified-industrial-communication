using Uic.Driver;

namespace Uic.Tests;

/// <summary>
/// 驱动契约用例（FR-SDK-02 / FR-SDK-03）。
///
/// 这两条需求都是「加载时不匹配直接拒绝」：一个驱动如果声明的 SDK 版本和宿主对不上，
/// 或者能力声明和配置对不上，宁可在启动时炸出来，也不能让它带着缺失的方法跑起来。
///
/// 用 <see cref="DriverRegistry"/> 直接测，不走整条 Host：注册本身是纯逻辑，
/// 不需要真连设备。
/// </summary>
internal static class SdkTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("sdk/register_rejects_sdk_version_mismatch", RegisterRejectsSdkVersionMismatch));
        tests.Add(new TestCase("sdk/register_rejects_duplicate_id", RegisterRejectsDuplicateId));
        tests.Add(new TestCase("sdk/register_accepts_matching_version", RegisterAcceptsMatchingVersion));
    }

    /// <summary>
    /// SDK 版本不匹配直接拒绝。
    ///
    /// 版本不匹配意味着接口形状对不上：驱动可能调用了宿主没实现的方法，
    /// 或者宿主传了驱动不认识的上下文。运行时才炸的话，排障成本远高于启动时拒绝。
    /// </summary>
    private static Task RegisterRejectsSdkVersionMismatch(TestContext t)
    {
        var registry = new DriverRegistry();
        var driver = new StubDriver { SdkVersion = "0.9" };

        var ex = t.Throws<InvalidOperationException>(
            () => registry.Register(driver), "SDK 版本不匹配");

        t.Contains(ex.Message, "0.9", "错误消息里说明驱动声明的版本");
        t.Contains(ex.Message, DriverSdk.Version, "错误消息里说明宿主要求的版本");

        // 拒绝后注册表里没有这个驱动：不会「部分注册」留下半残状态。
        t.Ok(!registry.TryGet("stub", out _), "注册被拒绝后驱动不在注册表里");

        return Task.CompletedTask;
    }

    /// <summary>同一个 id 注册两次拒绝：第二个驱动不会悄悄覆盖第一个。</summary>
    private static Task RegisterRejectsDuplicateId(TestContext t)
    {
        var registry = new DriverRegistry();
        var first = new StubDriver { Id = "dup" };
        var second = new StubDriver { Id = "dup" };

        registry.Register(first);

        var ex = t.Throws<InvalidOperationException>(
            () => registry.Register(second), "重复 id");

        t.Contains(ex.Message, "dup", "错误消息里说明是哪个 id 冲突");

        // 注册表里还是第一个驱动：后注册的不覆盖。
        registry.Get("dup");
        t.Ok(true, "Get 取回的是第一个驱动，没有抛出");

        return Task.CompletedTask;
    }

    /// <summary>版本匹配时正常注册，能力声明原样透出（FR-SDK-02）。</summary>
    private static Task RegisterAcceptsMatchingVersion(TestContext t)
    {
        var registry = new DriverRegistry();
        var driver = new StubDriver
        {
            Id = "stub",
            SdkVersion = DriverSdk.Version,
        };

        registry.Register(driver);

        var got = registry.Get("stub");
        t.Ok(got is StubDriver, "Get 取回注册过的驱动");
        t.Equal("stub", got.Meta.Id, "Meta.Id 原样");
        t.Equal("存根驱动", got.Meta.Name, "Meta.Name 原样");
        t.Equal("none", got.Meta.Transports[0], "Meta.Transports 原样");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 最小驱动实现：只满足 IDriver 接口，其余行为与本组用例无关。
    /// </summary>
    private sealed class StubDriver : IDriver
    {
        public string Id { get; init; } = "stub";
        public string Name { get; init; } = "存根驱动";
        public string SdkVersion { get; init; } = DriverSdk.Version;

        public DriverMeta Meta => new()
        {
            Id = Id,
            Name = Name,
            SdkVersion = SdkVersion,
            Transports = new[] { "none" },
            Roles = new[] { DriverRole.Master },
            Capabilities = new Capabilities(),
            ConfigFields = Array.Empty<ConfigField>(),
        };

        public IConn Open(OpenContext context) =>
            throw new NotSupportedException("本用例只测注册，不开连接");
    }
}
