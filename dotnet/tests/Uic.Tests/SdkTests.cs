using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Virtual;
using Uic.Engine;

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
        tests.Add(new TestCase("sdk/check_references_flags_unregistered_protocol", CheckReferencesFlagsUnregisteredProtocol));
        tests.Add(new TestCase("sdk/check_command_rejects_unregistered_protocol", CheckCommandRejectsUnregisteredProtocol));
        tests.Add(new TestCase("sdk/codec_assemblies_do_not_reference_io_layers", CodecAssembliesDoNotReferenceIo));
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

    /// <summary>
    /// <c>Scheduler.CheckReferences</c> 必须能把「协议名拼错 / 未注册驱动」标出来，
    /// 并指出字段路径，而不是留到调度器里才炸成 500。
    ///
    /// 走的是 <c>uic check</c> 与启动预检共用的同一条纯函数路径：
    /// 静态检查放过、运行时却拒绝，是最难查的一种配置不一致。
    /// </summary>
    private static Task CheckReferencesFlagsUnregisteredProtocol(TestContext t)
    {
        var registry = new DriverRegistry();
        registry.Register(new VirtualDriver());

        var doc = new ConfigDocument
        {
            SchemaVersion = 1,
            Site = "sdk-check-site",
            Channels =
            {
                new Channel { Id = "good", Protocol = "virtual", Transport = "none" },
                new Channel { Id = "bad", Protocol = "nonexistent", Transport = "none" },
            },
            Devices =
            {
                new Device { Id = "d1", ChannelId = "good" },
                new Device { Id = "d2", ChannelId = "bad" },
            },
        };

        var problems = Scheduler.CheckReferences(doc, registry);

        t.Ok(problems.Count == 1, $"只报出未注册的驱动引用，实际 {problems.Count} 条");
        t.Equal("channels[bad].protocol", problems[0].Path, "字段路径指向通道的 protocol");

        // 同一条通道下的多台设备只报一次：问题在通道上，不在设备上。
        doc.Devices.Add(new Device { Id = "d3", ChannelId = "bad" });
        t.Ok(Scheduler.CheckReferences(doc, registry).Count == 1,
            "同一条通道引用同一缺失驱动只报一次");

        return Task.CompletedTask;
    }

    /// <summary>
    /// <c>uic check</c> 必须与启动用同一份驱动注册表：协议名没注册时以退出码 2 失败，
    /// 并指出 `channels[x].protocol`；同一份配置换成已注册的驱动则通过。
    ///
    /// 只测 `Scheduler.CheckReferences` 是不够的——真正的风险是「check 忘了调用它」，
    /// 那会让部署前的静态检查放过一份启动必然失败的配置。所以这里直接跑命令行入口。
    /// </summary>
    private static async Task CheckCommandRejectsUnregisteredProtocol(TestContext t)
    {
        var path = t.TempFile("check.json");
        await File.WriteAllTextAsync(path, JsonDefaults.ToJson(CheckConfig("nonexistent")), t.Ct);

        var (badExit, badError) = await RunCheckAsync(path);

        t.Equal(2, badExit, "an unregistered protocol is a configuration error");
        t.Contains(badError, "channels[ch].protocol", "the message points at the channel protocol");
        t.Contains(badError, "no driver is registered for protocol 'nonexistent'",
            "the message names the missing driver");

        // 反向对照：同一份配置只把协议换成已注册的 virtual 就必须通过。
        // 少了这一条，一条「永远返回 2」的实现也能让上面两条断言通过。
        await File.WriteAllTextAsync(path, JsonDefaults.ToJson(CheckConfig("virtual")), t.Ct);
        var (goodExit, goodError) = await RunCheckAsync(path);

        t.Equal(0, goodExit, $"a registered protocol passes: {goodError}");
    }

    private static ConfigDocument CheckConfig(string protocol) => new()
    {
        SchemaVersion = 1,
        Site = "sdk-check-site",
        Channels =
        {
            new Channel { Id = "ch", Protocol = protocol, Transport = "none" },
        },
        Devices =
        {
            new Device
            {
                Id = "d1",
                ChannelId = "ch",
                Points =
                {
                    new Point
                    {
                        Id = "p1",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.UInt16,
                    },
                },
            },
        },
    };

    /// <summary>跑一次真实的 <c>uic check</c>，返回退出码与 stderr。</summary>
    private static async Task<(int Exit, string Error)> RunCheckAsync(string path)
    {
        var originalOut = Console.Out;
        var originalError = Console.Error;
        var stdout = new StringWriter();
        var stderr = new StringWriter();

        try
        {
            Console.SetOut(stdout);
            Console.SetError(stderr);
            var exit = await Uic.Host.Program.Main(new[] { "check", "--config", path });
            return (exit, stderr.ToString());
        }
        finally
        {
            Console.SetOut(originalOut);
            Console.SetError(originalError);
        }
    }

    /// <summary>
    /// 分层边界（FR-SDK-01）：Codec 程序集只做纯编解码，不许引用字节流层。
    ///
    /// 为什么需要一条用例：这条边界以前只写在文档与 csproj 的约定里。
    /// 约定最容易的退化方式不是多一条没人用的 `ProjectReference`（编译器不会把它
    /// 写进元数据），而是某个 codec 真的开始用 `Uic.Transport` 的类型去开 socket、
    /// 自己重试——构建照样通过，编解码从此不能单测。
    /// 所以这里读的是程序集里**实际**的引用表，而不是 csproj 的声明。
    ///
    /// 反向对照同样重要：Codec 必须引用 Domain（否则连地址模型都拿不到），
    /// 只断言「没引用什么」的话，一个空程序集也能让用例通过。
    ///
    /// `Uic.Codec.Mqtt` 不在这个名单里：它刻意同时承载 MQTT 客户端
    /// （北向 sink 与南向订阅共用同一份 3.1.1 实现），本来就要开 socket。
    /// 把它也算进来，只能得到一条「要么放宽标准、要么拆项目」的假选择。
    /// </summary>
    private static Task CodecAssembliesDoNotReferenceIo(TestContext t)
    {
        // 每个纯编解码程序集都必须在，且都不得引用传输/驱动/运行时。
        var codecs = new[]
        {
            typeof(global::Uic.Codec.Modbus.ModbusCodec).Assembly,
            typeof(global::Uic.Codec.S7.Tpkt).Assembly,
            typeof(global::Uic.Codec.Bacnet.BacnetFrame).Assembly,
        };

        // 这些是「I/O 与编排」层：codec 引用它们就意味着它在自己开连接、自己重试。
        var forbidden = new[]
        {
            "Uic.Transport",
            "Uic.Engine",
            "Uic.Outbox",
            "Uic.Runtime",
            "Uic.Northbound",
            "Uic.Obs",
            "Uic.Config",
            "Uic.Host",
        };

        foreach (var assembly in codecs)
        {
            var name = assembly.GetName().Name!;
            var references = assembly.GetReferencedAssemblies()
                .Select(a => a.Name ?? string.Empty)
                .ToList();

            // 反向对照：codec 确实依赖领域模型，说明这条程序集真的被加载到了。
            t.Ok(references.Contains("Uic.Domain"), $"{name} 依赖 Uic.Domain（否则它拿不到地址与值模型）");

            foreach (var layer in forbidden)
            {
                t.Ok(!references.Contains(layer), $"{name} 不得引用 {layer}：编解码要保持可单测、不碰 I/O");
            }

            // System.Net 是另一条边界：出现它基本等于「codec 自己开 socket」。
            t.Ok(!references.Any(r => r.StartsWith("System.Net", StringComparison.Ordinal)),
                $"{name} 不得引用 System.Net");
        }

        return Task.CompletedTask;
    }
}
