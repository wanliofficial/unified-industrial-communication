using System.Net;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;

namespace Uic.Tests;

/// <summary>
/// 用例自签证书。TLS 验收必须用真实证书：不装 CA、不 mock 握手，
/// 否则「证书链/主机名校验到底有没有生效」这个问题测不出来。
/// </summary>
internal static class TestCertificates
{
    /// <summary>
    /// 生成一张同时可作服务端与根的自签证书（SAN 含 localhost 与 127.0.0.1）。
    /// 自签 + CA=true 让它既能给 Kestrel/SslStream 用，也能当作
    /// <c>tls.caFile</c> 的自定义信任根，省掉维护两套证书的麻烦。
    /// </summary>
    public static X509Certificate2 Create(string commonName = "localhost")
    {
        using var rsa = RSA.Create(2048);

        var request = new CertificateRequest(
            $"CN={commonName}", rsa, HashAlgorithmName.SHA256, RSASignaturePadding.Pkcs1);

        request.CertificateExtensions.Add(
            new X509BasicConstraintsExtension(certificateAuthority: true, false, 0, critical: true));
        request.CertificateExtensions.Add(
            new X509KeyUsageExtension(
                X509KeyUsageFlags.DigitalSignature | X509KeyUsageFlags.KeyEncipherment |
                X509KeyUsageFlags.KeyCertSign, critical: true));
        request.CertificateExtensions.Add(
            new X509EnhancedKeyUsageExtension(
                new OidCollection { new("1.3.6.1.5.5.7.3.1") }, critical: false));

        var names = new SubjectAlternativeNameBuilder();
        names.AddDnsName("localhost");
        names.AddIpAddress(IPAddress.Loopback);
        request.CertificateExtensions.Add(names.Build());

        return request.CreateSelfSigned(
            DateTimeOffset.UtcNow.AddHours(-1), DateTimeOffset.UtcNow.AddYears(1));
    }

    /// <summary>写出 PFX（带私钥），REST 服务端用。</summary>
    public static string WritePfx(string directory, X509Certificate2 certificate, string? password = null)
    {
        var path = Path.Combine(directory, "rest.pfx");
        File.WriteAllBytes(path, certificate.Export(X509ContentType.Pkcs12, password));
        return path;
    }

    /// <summary>写出 PEM（只含证书，不含私钥），MQTT 自定义 CA 用。</summary>
    public static string WritePem(string directory, X509Certificate2 certificate, string name = "ca.pem")
    {
        var path = Path.Combine(directory, name);
        File.WriteAllText(path, certificate.ExportCertificatePem());
        return path;
    }
}
