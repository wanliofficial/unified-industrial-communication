using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using Uic.Config;
using Uic.Domain;

namespace Uic.Northbound;

/// <summary>
/// TLS 材料装载。放在北向而不是宿主，是因为它只服务北向 REST 监听：
/// 「证书读不出来」是配置问题，不是运行期偶发，必须在启动阶段失败。
///
/// MQTT 的自定义 CA 不在这里：北向发布与南向订阅要校验同一个自签 Broker，
/// 那份逻辑已下沉到协议层 <c>Uic.Codec.Mqtt.MqttTls</c>，两侧共用一套语义。
/// </summary>
public static class Tls
{
    /// <summary>
    /// 装载 REST 服务端证书。PFX 走口令，PEM 走证书 + 私钥（同一个文件也可以）。
    /// 失败一律抛 <see cref="ConfigException"/>，启动时以「配置非法」退出，
    /// 而不是带着一个可疑的监听端口继续服务。
    /// </summary>
    public static X509Certificate2 LoadServerCertificate(
        RestOptions rest,
        string dataDir,
        ResolvedSecrets secrets)
    {
        var tls = rest.Tls;
        if (!tls.Enabled) throw new InvalidOperationException("TLS is not enabled");

        if (string.IsNullOrWhiteSpace(tls.CertificateFile))
        {
            throw new ConfigException("TLS certificate is not configured", new[]
            {
                new ValidationProblem("northbound.rest.tls.certificateFile",
                    "required when northbound.rest.tls.enabled is true"),
            });
        }

        var certPath = Path.GetFullPath(Secrets.ResolvePath(tls.CertificateFile, dataDir));

        try
        {
            if (LooksLikePem(certPath))
            {
                var keyPath = string.IsNullOrWhiteSpace(tls.KeyFile)
                    ? certPath
                    : Path.GetFullPath(Secrets.ResolvePath(tls.KeyFile, dataDir));

                using var pem = X509Certificate2.CreateFromPemFile(certPath, keyPath);

                // PEM 装载出的证书在 Windows 上可能没有可用的私钥句柄；
                // 重新打包成 PKCS#12 再读一次，跨平台行为就一致了。
                return X509CertificateLoader.LoadPkcs12(pem.Export(X509ContentType.Pkcs12), null);
            }

            return X509CertificateLoader.LoadPkcs12FromFile(
                certPath, secrets.RestCertificatePassword);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException
                                       or CryptographicException or ArgumentException)
        {
            throw new ConfigException("TLS certificate is not usable", new[]
            {
                new ValidationProblem("northbound.rest.tls.certificateFile",
                    $"'{certPath}': {ex.Message}"),
            });
        }
    }

    /// <summary>
    /// PEM 用内容判断，不看扩展名：现场把 PEM 存成 .crt / .cer 很常见，
    /// 而 PKCS#12 是二进制，两种格式必须走不同的装载路径。
    /// </summary>
    private static bool LooksLikePem(string path)
    {
        using var stream = File.OpenRead(path);
        Span<byte> head = stackalloc byte[16];
        var read = stream.Read(head);
        return read > 10 && head[..read].StartsWith("-----BEGIN"u8);
    }
}
