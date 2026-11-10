using Opc.Ua;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Driver.OpcUa;

/// <summary>
/// OPC UA 客户端的连接参数。
///
/// 全部来自通道（host/port）与设备 labels，不从进程环境或全局配置里"顺手"取：
/// 同一台网关要连多台 UA 服务端时，任何隐式默认值都会变成一次现场排查。
/// </summary>
public sealed record OpcUaClientOptions
{
    private const string Protocol = OpcUaValues.Protocol;

    /// <summary>支持的 SecurityPolicy。PRD 只要求这两档，其它的显式拒绝。</summary>
    public static readonly string[] SupportedPolicies = { "None", "Basic256Sha256" };

    public required string EndpointUrl { get; init; }

    /// <summary>客户端证书与信任列表的根目录。Basic256Sha256 必需，None 下也用它放身份证书。</summary>
    public required string PkiDir { get; init; }

    public string SecurityPolicy { get; init; } = "None";
    public MessageSecurityMode SecurityMode { get; init; } = MessageSecurityMode.None;

    /// <summary>
    /// 是否接受服务端的自签证书。默认接受：现场多为自签，而且扫描/拒绝会把
    /// "证书没放进信任列表"变成一句看不出原因的 BadCertificateUntrusted。
    /// 需要严格信任链的部署应置 false，并把对端证书放进 &lt;pkiDir&gt;/trusted。
    /// </summary>
    public bool AcceptUntrustedCertificates { get; init; } = true;

    public string ApplicationName { get; init; } = "uic-gateway-client";
    public string ApplicationUri { get; init; } = "urn:uic:gateway:client";

    public TimeSpan SessionTimeout { get; init; } = TimeSpan.FromSeconds(60);
    public TimeSpan OperationTimeout { get; init; } = TimeSpan.FromSeconds(5);

    /// <summary>订阅的发布周期。轮询周期由调度器决定，这里只管推送。</summary>
    public TimeSpan PublishingInterval { get; init; } = TimeSpan.FromSeconds(1);

    public static OpcUaClientOptions FromContext(OpenContext ctx)
    {
        var channel = ctx.Channel;
        var device = ctx.Device;

        var host = channel.Host?.Trim();
        if (string.IsNullOrEmpty(host))
        {
            throw DriverException.Address(Protocol, "open",
                $"channel '{channel.Id}' needs transport.host (the OPC UA server address)");
        }

        var port = channel.Port == 0 ? 4840 : channel.Port;
        if (port is < 1 or > 65535)
        {
            throw DriverException.Address(Protocol, "open",
                $"channel '{channel.Id}' has an invalid port {port}");
        }

        var path = device.Label("endpointPath")?.Trim();
        if (!string.IsNullOrEmpty(path) && !path.StartsWith('/'))
            path = "/" + path;

        // IPv6 字面量必须加方括号，否则 "opc.tcp://::1:4840" 会被当成非法 URI。
        var authority = host.Contains(':') && !host.StartsWith('[') ? $"[{host}]" : host;
        var endpoint = $"opc.tcp://{authority}:{port}{path}";

        if (!Uri.TryCreate(endpoint, UriKind.Absolute, out _))
        {
            throw DriverException.Address(Protocol, "open",
                $"'{endpoint}' is not a valid opc.tcp URL; check transport.host / transport.port");
        }

        var policy = device.Label("securityPolicy")?.Trim() ?? "None";
        var (policyName, policyUri) = NormalizePolicy(policy);

        var modeText = device.Label("securityMode")?.Trim();
        var mode = string.IsNullOrEmpty(modeText)
            ? (policyUri == SecurityPolicies.None ? MessageSecurityMode.None : MessageSecurityMode.SignAndEncrypt)
            : ParseMode(modeText, policyName);

        if (policyUri == SecurityPolicies.None && mode != MessageSecurityMode.None)
        {
            throw DriverException.Unsupported(Protocol, "open",
                $"securityPolicy None cannot use securityMode {mode}; set labels.securityMode=None");
        }

        if (policyUri != SecurityPolicies.None && mode == MessageSecurityMode.None)
        {
            throw DriverException.Unsupported(Protocol, "open",
                $"securityPolicy {policyName} needs securityMode Sign or SignAndEncrypt; " +
                "None would send the traffic in plaintext while claiming to be secure");
        }

        return new OpcUaClientOptions
        {
            EndpointUrl = endpoint,
            PkiDir = ResolvePkiDir(ctx, device),
            SecurityPolicy = policyName,
            SecurityMode = mode,
            AcceptUntrustedCertificates = ParseBool(device, "acceptUntrustedCertificates", true),
            SessionTimeout = TimeSpan.FromMilliseconds(ParseInt(device, "sessionTimeout", 60_000, 5_000, 3_600_000)),
            OperationTimeout = TimeSpan.FromMilliseconds(ParseInt(device, "operationTimeout", 5_000, 100, 600_000)),
            PublishingInterval = TimeSpan.FromMilliseconds(
                ParseInt(device, "publishingInterval", 1_000, 100, 3_600_000)),
        };
    }

    /// <summary>
    /// 客户端 PKI 目录。证书是"这一台网关"的身份，不是某台设备的，
    /// 所以默认共用数据目录下的一个目录；需要隔离时用 labels.pkiDir 指定。
    /// </summary>
    private static string ResolvePkiDir(OpenContext ctx, Device device)
    {
        var configured = device.Label("pkiDir")?.Trim();

        if (!string.IsNullOrEmpty(configured))
        {
            if (Path.IsPathRooted(configured)) return configured;
            if (ctx.DataDir is { Length: > 0 } dir) return Path.Combine(dir, configured);

            throw DriverException.Unsupported(Protocol, "open",
                $"labels.pkiDir '{configured}' is relative but no data directory is available");
        }

        if (ctx.DataDir is { Length: > 0 } dataDir)
            return Path.Combine(dataDir, "opcua-client-pki");

        throw DriverException.Unsupported(Protocol, "open",
            "OPC UA needs a certificate store; set labels.pkiDir to an absolute path " +
            "(the gateway passes its data directory automatically)");
    }

    /// <summary>策略名归一化（None / Basic256Sha256）。不在白名单里的显式拒绝。</summary>
    public static (string Name, string Uri) NormalizePolicy(string policy)
    {
        if (string.Equals(policy, "None", StringComparison.OrdinalIgnoreCase))
            return ("None", SecurityPolicies.None);

        if (string.Equals(policy, "Basic256Sha256", StringComparison.OrdinalIgnoreCase))
            return ("Basic256Sha256", SecurityPolicies.Basic256Sha256);

        throw DriverException.Unsupported(Protocol, "open",
            $"securityPolicy '{policy}' is not supported by this build; " +
            $"use {string.Join(" or ", SupportedPolicies)}");
    }

    private static MessageSecurityMode ParseMode(string text, string policyName)
    {
        if (Enum.TryParse<MessageSecurityMode>(text, ignoreCase: true, out var mode) &&
            mode is MessageSecurityMode.None or MessageSecurityMode.Sign or MessageSecurityMode.SignAndEncrypt)
        {
            return mode;
        }

        throw DriverException.Unsupported(Protocol, "open",
            $"securityMode '{text}' is not valid for {policyName}; use None, Sign or SignAndEncrypt");
    }

    /// <summary>
    /// 从服务端公布的端点里挑出配置指定的那一档。
    ///
    /// 不能"有安全端点就用安全端点"：配置写 None 却连上 SignAndEncrypt 会
    /// 因为证书不一致而失败；反过来更糟——配置要求加密却被悄悄降级成明文。
    /// 找不到就报错，把服务端实际提供的档位列出来。
    /// </summary>
    public static EndpointDescription SelectEndpoint(
        IEnumerable<EndpointDescription> endpoints, string policyUri, MessageSecurityMode mode)
    {
        var list = endpoints.ToArray();

        foreach (var candidate in list)
        {
            if (string.Equals(candidate.SecurityPolicyUri, policyUri, StringComparison.OrdinalIgnoreCase) &&
                candidate.SecurityMode == mode)
            {
                return candidate;
            }
        }

        var offered = list.Length == 0
            ? "none"
            : string.Join(", ", list.Select(e => $"{PolicyName(e.SecurityPolicyUri)}/{e.SecurityMode}").Distinct());

        throw DriverException.Unsupported(Protocol, "connect",
            $"the server does not offer {PolicyName(policyUri)}/{mode}; it offers {offered} " +
            "(the driver does not silently downgrade)");
    }

    public static string PolicyName(string? uri)
    {
        if (string.IsNullOrEmpty(uri)) return "None";
        var hash = uri.LastIndexOf('#');
        return hash >= 0 && hash + 1 < uri.Length ? uri[(hash + 1)..] : uri;
    }

    private static int ParseInt(Device device, string key, int fallback, int min, int max)
    {
        var raw = device.Label(key);
        if (raw is null) return fallback;

        if (!int.TryParse(raw, out var value) || value < min || value > max)
        {
            throw DriverException.Unsupported(Protocol, "open",
                $"labels.{key} must be {min}..{max}, got '{raw}'");
        }

        return value;
    }

    private static bool ParseBool(Device device, string key, bool fallback)
    {
        var raw = device.Label(key)?.Trim().ToLowerInvariant();
        return raw switch
        {
            null => fallback,
            "true" or "1" or "yes" or "on" => true,
            "false" or "0" or "no" or "off" => false,
            _ => throw DriverException.Unsupported(Protocol, "open",
                $"labels.{key} must be true or false, got '{raw}'"),
        };
    }
}
