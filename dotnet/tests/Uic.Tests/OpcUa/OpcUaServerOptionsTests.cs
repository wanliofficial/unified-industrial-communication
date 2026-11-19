using Opc.Ua;
using Uic.Domain;
using Uic.OpcUa;

namespace Uic.Tests;

/// <summary>
/// OPC UA 北向服务端的能力边界验收。
///
/// 为什么不放进 --opcua 回环用例：回环用例用匿名会话连上就通过——往端点集合里再塞
/// 一个 Basic256Sha256 它照样绿，于是「服务端只声明 None + 匿名令牌」会悄悄破掉。
/// 这两条纯函数用例在配置层与 ApplicationConfiguration 层各钉一次，不需要绑端口。
/// </summary>
internal static class OpcUaServerOptionsTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("opcua/server_options_are_anonymous_and_none_only", ServerOptions));
        tests.Add(new TestCase("opcua/server_endpoint_advertises_only_none_and_anonymous", ServerEndpoint));
    }

    private static OpcUaOptions Options(
        string endpoint = "opc.tcp://127.0.0.1:4840",
        string? policy = null,
        bool allowAnonymousRead = true) => new()
    {
        Enabled = true,
        Endpoint = endpoint,
        SecurityPolicy = policy ?? "None",
        AllowAnonymousRead = allowAnonymousRead,
    };

    private static Task ServerOptions(TestContext t)
    {
        var dataDir = t.TempDir;

        var ok = OpcUaServerOptions.FromConfig(Options(), dataDir, "plant-1");

        // 只读是更安全的初始值：匿名写默认关闭，配置里没写就不开。
        t.Ok(ok.AllowAnonymousRead, "anonymous read is allowed by default");
        t.Ok(!ok.AllowAnonymousWrite, "anonymous write is off unless explicitly enabled");
        t.Equal("plant-1", ok.Site, "the site is carried through to the address space");

        // 命名空间 URI 用配置里的值（缺省是 urn:uic:gateway）；只有显式留空才按站点派生。
        t.Equal("urn:uic:gateway", ok.NamespaceUri, "the configured default namespace uri is used");
        t.Equal(Path.Combine(dataDir, "opcua-pki"), ok.PkiDir, "the pki directory sits under the data dir");
        t.Equal(2, ok.ExpectedNamespaceIndex, "the spec namespace index is 2");

        var derived = OpcUaServerOptions.FromConfig(
            new OpcUaOptions
            {
                Enabled = true,
                Endpoint = "opc.tcp://127.0.0.1:4840",
                NamespaceUri = " ",
            }, dataDir, "plant-1");
        t.Equal("urn:uic:site:plant-1", derived.NamespaceUri,
            "a blank namespace uri falls back to urn:uic:site:<site>");

        var custom = OpcUaServerOptions.FromConfig(
            new OpcUaOptions
            {
                Enabled = true,
                Endpoint = "opc.tcp://127.0.0.1:4840",
                NamespaceUri = "urn:acme:plc",
                NamespaceIndex = 3,
            }, dataDir, "plant-1");
        t.Equal("urn:acme:plc", custom.NamespaceUri, "a configured namespace uri wins over the derived one");
        t.Equal(3, custom.ExpectedNamespaceIndex, "the configured namespace index wins");

        // 能力边界在构造选项时就拒绝：与其等客户端握不上手，不如在启动/配置应用前就报出来。
        var disabled = t.Throws<OpcUaConfigurationException>(
            () => OpcUaServerOptions.FromConfig(new OpcUaOptions { Enabled = false }, dataDir, "plant-1"),
            "a disabled endpoint is refused instead of building anything");
        t.Contains(disabled.Message, "disabled", "the refusal says the endpoint is disabled");

        var encrypted = t.Throws<OpcUaConfigurationException>(
            () => OpcUaServerOptions.FromConfig(Options(policy: "Basic256Sha256"), dataDir, "plant-1"),
            "a certificate policy is refused");
        t.Contains(encrypted.Message, "only", "the refusal explains that only None is implemented");

        var noAuth = t.Throws<OpcUaConfigurationException>(
            () => OpcUaServerOptions.FromConfig(Options(allowAnonymousRead: false), dataDir, "plant-1"),
            "disabling anonymous read is refused");
        t.Contains(noAuth.Message, "allowAnonymousRead", "the refusal names the offending field");

        var badScheme = t.Throws<OpcUaConfigurationException>(
            () => OpcUaServerOptions.FromConfig(Options(endpoint: "http://127.0.0.1:4840"), dataDir, "plant-1"),
            "a non-opc.tcp scheme is refused");
        t.Contains(badScheme.Message, "opc.tcp", "the refusal demands the opc.tcp scheme");

        var noPort = t.Throws<OpcUaConfigurationException>(
            () => OpcUaServerOptions.FromConfig(Options(endpoint: "opc.tcp://127.0.0.1"), dataDir, "plant-1"),
            "an endpoint without an explicit port is refused");
        t.Contains(noPort.Message, "explicit port", "the refusal demands a port");

        return Task.CompletedTask;
    }

    private static Task ServerEndpoint(TestContext t)
    {
        var options = OpcUaServerOptions.FromConfig(Options(), t.TempDir, "plant-1");
        var appConfig = UicOpcUaServer.BuildApplicationConfiguration(options);

        var server = appConfig.ServerConfiguration!;

        // 端点只声明 SecurityPolicy.None：签名与加密需要证书生命周期，第一版不做。
        // 再塞一条 Basic256Sha256 端点，客户端就会开始协商它——这条断言就是那道闸。
        t.Equal(1, server.SecurityPolicies.Count, "exactly one security policy is advertised");
        t.Equal(SecurityPolicies.None, server.SecurityPolicies[0].SecurityPolicyUri, "the only policy is None");
        t.Equal(MessageSecurityMode.None, server.SecurityPolicies[0].SecurityMode, "the only mode is None");

        // 只接受匿名令牌：没有用户名/证书两类令牌可挑。
        t.Equal(1, server.UserTokenPolicies.Count, "exactly one user token policy is advertised");
        t.Equal(UserTokenType.Anonymous, server.UserTokenPolicies[0].TokenType, "the only token is anonymous");

        t.Ok(server.BaseAddresses.Contains(options.Endpoint), "the configured endpoint is the listen address");

        // 应用 URI 与地址空间命名空间 URI 必须不同：命名空间表按字符串去重，相同会让
        // 自定义命名空间被复用进索引 1，NodeId 就不再是契约约定的 ns=2。
        t.Ok(appConfig.ApplicationUri != options.NamespaceUri,
            "the application uri differs from the address-space namespace uri");

        return Task.CompletedTask;
    }
}
