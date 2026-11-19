using Uic.Domain;

namespace Uic.OpcUa;

/// <summary>
/// OPC UA 服务端选项。
///
/// 第一版只支持匿名 + SecurityPolicy.None。为什么不做签名与加密：证书生命周期
/// （签发、轮换、吊销、对端信任列表）是一套独立的运维负担，而第一版面向的是
/// 同网段的调试与验收场景。要求「必须加密」的部署应放在安全边界之后，
/// 由网关显式拒绝启动来提醒，而不是悄悄降级成明文。
/// </summary>
public sealed class OpcUaServerOptions
{
    /// <summary>监听端点，形如 opc.tcp://127.0.0.1:4840。</summary>
    public required string Endpoint { get; init; }

    /// <summary>自定义地址空间的命名空间 URI。</summary>
    public required string NamespaceUri { get; init; }

    /// <summary>期望的命名空间索引（spec 约定为 2）。实际值以服务端分配为准。</summary>
    public int ExpectedNamespaceIndex { get; init; } = 2;

    /// <summary>是否允许匿名读。</summary>
    public bool AllowAnonymousRead { get; init; } = true;

    /// <summary>是否允许匿名写。默认关闭：只读是更安全的初始值。</summary>
    public bool AllowAnonymousWrite { get; init; }

    /// <summary>PKI 根目录。证书与受信列表都建在它下面，与数据目录同级管理。</summary>
    public required string PkiDir { get; init; }

    /// <summary>站点标识，写进 NodeId 与地址空间。</summary>
    public required string Site { get; init; }

    /// <summary>
    /// 从配置构造并做「能力边界」校验。不支持的组合必须在这里失败，
    /// 不能等到客户端连上来才发现握手不了。
    /// </summary>
    public static OpcUaServerOptions FromConfig(OpcUaOptions cfg, string dataDir, string site)
    {
        if (!cfg.Enabled)
        {
            throw new OpcUaConfigurationException("opc ua is disabled");
        }

        if (!string.Equals(cfg.SecurityPolicy, "None", StringComparison.OrdinalIgnoreCase))
        {
            throw new OpcUaConfigurationException(
                $"opcUa.securityPolicy '{cfg.SecurityPolicy}' is not supported by this build; " +
                "only \"None\" is implemented (certificate-based security is not available yet)");
        }

        if (!cfg.AllowAnonymousRead)
        {
            throw new OpcUaConfigurationException(
                "opcUa.allowAnonymousRead=false is not supported by this build; " +
                "no user authentication backend is wired to the OPC UA endpoint");
        }

        // 「写要求读」不需要单独一条校验：上一行已经保证 AllowAnonymousRead 必为 true，
        // 所以 write && !read 在这里不可达。组合不合法时用户看到的错误就是上一行的
        // allowAnonymousRead 提示——把它改成 true 即可。

        ValidateEndpoint(cfg.Endpoint);

        return new OpcUaServerOptions
        {
            Endpoint = cfg.Endpoint,
            NamespaceUri = string.IsNullOrWhiteSpace(cfg.NamespaceUri)
                ? $"urn:uic:site:{site}"
                : cfg.NamespaceUri,
            ExpectedNamespaceIndex = cfg.NamespaceIndex,
            AllowAnonymousRead = cfg.AllowAnonymousRead,
            AllowAnonymousWrite = cfg.AllowAnonymousWrite,
            PkiDir = Path.Combine(dataDir, "opcua-pki"),
            Site = site,
        };
    }

    private static void ValidateEndpoint(string endpoint)
    {
        if (!endpoint.StartsWith("opc.tcp://", StringComparison.OrdinalIgnoreCase))
        {
            throw new OpcUaConfigurationException(
                $"opcUa.endpoint '{endpoint}' must use the opc.tcp scheme");
        }

        if (!Uri.TryCreate(endpoint, UriKind.Absolute, out var uri) || uri.Port <= 0)
        {
            throw new OpcUaConfigurationException(
                $"opcUa.endpoint '{endpoint}' is not a valid opc.tcp URL with an explicit port");
        }
    }
}
