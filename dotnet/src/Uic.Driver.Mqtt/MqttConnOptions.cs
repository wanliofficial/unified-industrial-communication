using Uic.Codec.Mqtt;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Driver.Mqtt;

/// <summary>
/// 南向 MQTT 订阅方的连接参数。
///
/// 全部来自通道（host/port）与设备 labels。与北向发布方共用同一份
/// <see cref="MqttOptions"/>，但这里只填「订阅」需要的那几项：主题模板、命令主题、
/// 遗嘱这些发布侧概念一律留空，避免把两套语义串在一起。
///
/// 明确不支持用户名/口令：本版本的凭据链路只覆盖北向，南向设备标签是明文落盘的，
/// 把 Broker 口令写进 labels 等于把凭据抄进配置文件。需要认证时应在 Broker 侧
/// 用 TLS + 网络 ACL 把订阅方限定到固定网段（与 OPC UA 的匿名身份策略一致）。
/// </summary>
public sealed record MqttConnOptions
{
    public const string Protocol = "mqtt";

    /// <summary>传给协议层的客户端参数。南向只用到 host/port/qos/keepalive/reconnect/tls。</summary>
    public required MqttOptions Client { get; init; }

    /// <summary>
    /// 默认 JSON 字段路径。点位没有写 <c>address.property</c> 时用它。
    /// 默认 <c>value</c>：北向发布的 Sample JSON 里值就在这个字段上，网关可以级联。
    /// </summary>
    public required string PayloadField { get; init; }

    /// <summary>
    /// 超过多少秒没有收到新报文就把缓存值判为 stale。0 表示不按时间过期，
    /// 只区分「收到过」与「从未收到」。
    /// </summary>
    public required int StaleAfterSeconds { get; init; }

    public static MqttConnOptions FromContext(OpenContext ctx)
    {
        var channel = ctx.Channel;
        var device = ctx.Device;

        // 匿名 only：显式拒绝而不是静默忽略，否则运维会以为口令生效了。
        if (device.Label("username") is not null || device.Label("password") is not null)
        {
            throw DriverException.Unsupported(Protocol, "open",
                "this build does not support MQTT credentials on the southbound side; " +
                "remove labels.username / labels.password and restrict access on the broker " +
                "(use TLS plus a network ACL)");
        }

        var host = channel.Host?.Trim();
        if (string.IsNullOrEmpty(host))
        {
            throw DriverException.Address(Protocol, "open",
                $"channel '{channel.Id}' needs transport.host (the MQTT broker address)");
        }

        var port = channel.Port == 0 ? 1883 : channel.Port;
        if (port is < 1 or > 65535)
        {
            throw DriverException.Address(Protocol, "open",
                $"channel '{channel.Id}' has an invalid port {port}");
        }

        var tlsEnabled = ParseBool(device, "tlsEnabled", false);
        var caFile = device.Label("tlsCaFile")?.Trim();

        if (!tlsEnabled && caFile is not null)
        {
            throw DriverException.Unsupported(Protocol, "open",
                "labels.tlsCaFile is set but labels.tlsEnabled is false; " +
                "the certificate would never be used, so this is rejected instead of silently ignored");
        }

        var qos = ParseInt(device, "qos", 1, 0, 1);

        return new MqttConnOptions
        {
            PayloadField = NormalizeField(device.Label("payloadField") ?? "value"),
            StaleAfterSeconds = ParseInt(device, "staleAfterSeconds", 0, 0, 86_400),
            Client = new MqttOptions
            {
                Host = host,
                Port = port,
                ClientId = device.Label("clientId")?.Trim() ?? $"uic-{device.Id}",
                Qos = qos,
                KeepAliveSeconds = ParseInt(device, "keepAliveSeconds", 30, 5, 600),
                ReconnectDelayMs = ParseInt(device, "reconnectDelayMs", 2_000, 500, 300_000),
                Tls = new MqttTlsOptions
                {
                    Enabled = tlsEnabled,
                    CaFile = caFile,
                    InsecureSkipVerify = tlsEnabled && ParseBool(device, "tlsInsecureSkipVerify", false),
                },
            },
        };
    }

    /// <summary>
    /// 把「整个载荷就是值」的写法归一成空串。用 <c>.</c> / <c>$</c> 做哨兵而不是空字符串，
    /// 是因为设备标签留空与「没配」在 <see cref="Device.Label"/> 里不可区分。
    /// </summary>
    public static string NormalizeField(string field)
    {
        var trimmed = field.Trim();
        return trimmed is "." or "$" ? string.Empty : trimmed;
    }

    internal static int ParseInt(Device device, string key, int fallback, int min, int max)
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

    internal static bool ParseBool(Device device, string key, bool fallback)
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
