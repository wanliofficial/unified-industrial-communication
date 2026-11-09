using System.Diagnostics;
using System.Net;
using System.Security.Cryptography.X509Certificates;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Config;
using Uic.Domain;
using Uic.Northbound;

namespace Uic.Tests;

/// <summary>
/// TLS 终结（FR-OPS-10）。
///
/// 这里的用例都用真实证书、真实握手：REST 走进程内 Kestrel，MQTT 走测试 broker
/// 的 SslStream。用 mock 或「只查配置字段」都验不到这条需求最关键的语义——
/// 客户端到底有没有在校验证书链与主机名。
/// </summary>
internal static class TlsTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("tls/rest_certificate_is_required_and_readable", RestCertificateIsRequiredAndReadable));
        tests.Add(new TestCase("tls/certificate_password_must_be_a_reference", CertificatePasswordMustBeAReference));
        tests.Add(new TestCase("tls/mqtt_insecure_skip_verify_requires_enabled", MqttInsecureSkipVerifyRequiresEnabled));
        tests.Add(new TestCase("contract/tls_rest_serves_https", TlsRestServesHttps));
        tests.Add(new TestCase("mqtt/tls_publishes_over_custom_ca", MqttPublishesOverCustomCa));
        tests.Add(new TestCase("mqtt/tls_rejects_untrusted_broker", MqttRejectsUntrustedBroker));
    }

    /// <summary>
    /// 证书是 TLS 的唯一起点：缺路径、路径写错、文件读不出来，都必须在
    /// 配置阶段失败。带着一个「看起来在监听」的明文端口继续服务是最坏的结果。
    /// </summary>
    private static Task RestCertificateIsRequiredAndReadable(TestContext t)
    {
        // 1. 打开 TLS 却没给证书：静态校验与装载器都必须拒绝。
        var missing = Doc(t, restTls: new RestTlsOptions { Enabled = true });

        var problems = ModelValidator.Validate(missing);
        t.Ok(problems.Any(p => p.Path == "northbound.rest.tls.certificateFile"),
            "validation names the missing certificate file");

        var ex = t.Throws<ConfigException>(
            () => Tls.LoadServerCertificate(missing.Northbound.Rest, t.TempDir, ResolvedSecrets.Empty),
            "loading the server certificate fails loudly when nothing is configured");

        t.Equal("northbound.rest.tls.certificateFile", ex.Problems[0].Path, "the failure names the field");

        // 2. 路径存在性由 Secrets.Validate 负责（与运行时解析相对路径的基准一致）。
        var absent = Doc(t, restTls: new RestTlsOptions
        {
            Enabled = true,
            CertificateFile = "missing.pfx",
        });

        var fileProblems = Secrets.Validate(absent);
        t.Ok(fileProblems.Any(p => p.Path == "northbound.rest.tls.certificateFile"),
            "a certificate path that does not exist is reported");

        // 3. 证书路径不是凭据：写成引用只会在装载时得到一条不存在的路径。
        var asReference = Doc(t, restTls: new RestTlsOptions
        {
            Enabled = true,
            CertificateFile = "${env:UIC_TLS_CERT}",
        });

        t.Ok(ModelValidator.Validate(asReference)
                .Any(p => p.Path == "northbound.rest.tls.certificateFile" && p.Message.Contains("file path")),
            "a credential reference is not accepted as a certificate path");

        // 4. 正常路径：真实 PFX 能装载出带私钥的服务端证书。
        var certificate = TestCertificates.Create();
        var pfx = TestCertificates.WritePfx(t.TempDir, certificate);

        var valid = Doc(t, restTls: new RestTlsOptions
        {
            Enabled = true,
            CertificateFile = pfx,
        });

        t.Equal(0, ModelValidator.Validate(valid).Count, "a valid TLS shape passes validation");
        t.Equal(0, Secrets.Validate(valid).Count, "a readable certificate passes the secret checks");

        var loaded = Tls.LoadServerCertificate(valid.Northbound.Rest, t.TempDir, ResolvedSecrets.Empty);
        t.Ok(loaded.HasPrivateKey, "the server certificate carries its private key");

        return Task.CompletedTask;
    }

    /// <summary>
    /// PFX 口令与其它凭据同一条规则：只能写成引用。明文口令落盘等于证书私钥
    /// 也不设防——配置文件本来就是要被复制、备份、贴进工单的。
    /// </summary>
    private static Task CertificatePasswordMustBeAReference(TestContext t)
    {
        var certificate = TestCertificates.Create();
        var pfx = TestCertificates.WritePfx(t.TempDir, certificate, "pfx-pass");

        var plaintext = Doc(t, restTls: new RestTlsOptions
        {
            Enabled = true,
            CertificateFile = pfx,
            CertificatePassword = "pfx-pass",
        });

        var problems = Secrets.Validate(plaintext);
        t.Ok(problems.Any(p => p.Path == "northbound.rest.tls.certificatePassword" &&
                               p.Message.Contains("plaintext")),
            "a literal PFX password is refused at validation time");

        t.Throws<ConfigException>(
            () => Secrets.Resolve(plaintext, SecretLookup.Default),
            "resolving a plaintext PFX password fails instead of silently dropping it");

        // 引用可以解析，且解析出的就是装载 PFX 需要的口令。
        const string Variable = "UIC_TLS_PFX_TEST_PASSWORD";
        Environment.SetEnvironmentVariable(Variable, "pfx-pass");

        try
        {
            var reference = Doc(t, restTls: new RestTlsOptions
            {
                Enabled = true,
                CertificateFile = pfx,
                CertificatePassword = "${env:" + Variable + "}",
            });

            t.Equal(0, Secrets.Validate(reference).Count, "a reference is accepted");

            var secrets = Secrets.Resolve(reference, SecretLookup.Default);
            t.Equal("pfx-pass", secrets.RestCertificatePassword, "the reference resolves to the password");

            var loaded = Tls.LoadServerCertificate(
                reference.Northbound.Rest, t.TempDir, secrets);
            t.Ok(loaded.HasPrivateKey, "the password-protected PFX loads with the resolved password");
        }
        finally
        {
            Environment.SetEnvironmentVariable(Variable, null);
        }

        return Task.CompletedTask;
    }

    /// <summary>
    /// insecureSkipVerify 只在 TLS 打开时有意义。默认关着，打开时它会把
    /// 「加密」降级成「加密但不认证对端」，因此不能和「没开 TLS」混在一起静默通过。
    /// </summary>
    private static Task MqttInsecureSkipVerifyRequiresEnabled(TestContext t)
    {
        var wrong = Doc(t, mqttTls: new MqttTlsOptions { Enabled = false, InsecureSkipVerify = true });

        t.Ok(ModelValidator.Validate(wrong)
                .Any(p => p.Path == "northbound.mqtt.tls.insecureSkipVerify"),
            "insecureSkipVerify without tls.enabled is rejected");

        var enabled = Doc(t, mqttTls: new MqttTlsOptions { Enabled = true, InsecureSkipVerify = true });
        t.Equal(0, ModelValidator.Validate(enabled).Count,
            "the same switch is valid once TLS is on");

        // CA 同样是文件路径；写成引用只会在握手时得到一条不存在的路径。
        var asReference = Doc(t, mqttTls: new MqttTlsOptions
        {
            Enabled = true,
            CaFile = "${env:UIC_MQTT_CA}",
        });

        t.Ok(ModelValidator.Validate(asReference)
                .Any(p => p.Path == "northbound.mqtt.tls.caFile"),
            "a credential reference is not accepted as a CA path");

        return Task.CompletedTask;
    }

    /// <summary>
    /// REST 真的能用 HTTPS 起来：客户端钉住这张自签证书，请求成功即证明
    /// Kestrel 返回的正是配置里那张证书，而不是退回明文或用了别的证书。
    /// </summary>
    private static async Task TlsRestServesHttps(TestContext t)
    {
        await using var host = await ContractTests.Host.StartAsync(t, tls: true);

        t.Equal("https", host.Client.BaseAddress!.Scheme, "the host is listening on https");

        using var health = await host.Client.GetAsync("/healthz", t.Ct);
        t.Equal(HttpStatusCode.OK, health.StatusCode, "a request over TLS reaches the public endpoint");

        using var version = await host.Client.GetAsync("/api/v1/version", t.Ct);
        t.Equal(HttpStatusCode.OK, version.StatusCode, "the version endpoint is served over TLS");
    }

    /// <summary>
    /// 自签 broker + 配置里的 CA：客户端要能建立 TLS、发布并收到 PUBACK。
    /// 这条同时验证「自定义信任根」这条路径真的被走到了（系统信任库不认自签证书）。
    /// </summary>
    private static async Task MqttPublishesOverCustomCa(TestContext t)
    {
        var certificate = TestCertificates.Create();
        var caFile = TestCertificates.WritePem(t.TempDir, certificate);

        await using var broker = new FakeMqttBroker(certificate);
        await using var client = new MqttClient(
            Options(broker.Port, new MqttTlsOptions { Enabled = true, CaFile = caFile }),
            "site-a", NullLogger.Instance, t.TempDir);

        client.Start();
        await WaitUntil(t, () => client.IsConnected, "the client completes the TLS handshake");

        await client.PublishAsync(
            "uic/site-a/d1/p1/value", "{\"value\":7}"u8.ToArray(), retain: false, t.Ct);

        await WaitUntil(t, () => broker.Published.Count > 0, "the broker receives the PUBLISH over TLS");

        t.Equal("uic/site-a/d1/p1/value", broker.Published[0].Topic, "the topic survives the TLS wrapper");
        t.Equal("{\"value\":7}", broker.Published[0].Payload, "the payload survives the TLS wrapper");
    }

    /// <summary>
    /// 信任根不匹配时必须拒绝连接。如果这里「连上了」，说明证书校验被绕过，
    /// 加密就只剩下防窃听、不防中间人。
    /// </summary>
    private static async Task MqttRejectsUntrustedBroker(TestContext t)
    {
        var brokerCertificate = TestCertificates.Create("broker.local");
        var otherCa = TestCertificates.WritePem(t.TempDir, TestCertificates.Create("other.local"), "other.pem");

        await using var broker = new FakeMqttBroker(brokerCertificate);
        await using var client = new MqttClient(
            Options(broker.Port, new MqttTlsOptions { Enabled = true, CaFile = otherCa }),
            "site-a", NullLogger.Instance, t.TempDir);

        client.Start();

        // 握手失败会在 SslStream 里抛错，客户端进入退避重连；给它足够时间证明
        // 「不是慢，是永远不会连上」。
        var clock = Stopwatch.StartNew();
        while (clock.Elapsed < TimeSpan.FromSeconds(2))
        {
            if (client.IsConnected) break;
            await Task.Delay(50, t.Ct);
        }

        t.Ok(!client.IsConnected, "a broker signed by an untrusted CA is rejected");
        t.Equal(0, broker.Connects.Count, "the MQTT CONNECT frame is never sent over an unauthenticated link");
    }

    private static MqttOptions Options(int port, MqttTlsOptions tls) => new()
    {
        Enabled = true,
        Host = "127.0.0.1",
        Port = port,
        Qos = 1,
        Retain = false,
        KeepAliveSeconds = 30,
        ReconnectDelayMs = 500,
        ClientId = "uic-tls-test",
        TopicPrefix = "uic",
        Topic = "uic/{site}/{device}/{point}/value",
        Tls = tls,
    };

    private static ConfigDocument Doc(
        TestContext t,
        RestTlsOptions? restTls = null,
        MqttTlsOptions? mqttTls = null) => new()
    {
        SchemaVersion = 1,
        Site = "tls-site",
        Northbound = new NorthboundOptions
        {
            Rest = new RestOptions
            {
                Enabled = true,
                Listen = "127.0.0.1:18080",
                Tls = restTls ?? new RestTlsOptions(),
            },
            Mqtt = new MqttOptions
            {
                Enabled = true,
                Tls = mqttTls ?? new MqttTlsOptions(),
            },
        },
        Storage = new StorageOptions { DataDir = t.TempDir },
    };

    private static async Task WaitUntil(TestContext t, Func<bool> condition, string what, int timeoutMs = 10_000)
    {
        var clock = Stopwatch.StartNew();
        while (!condition())
        {
            if (clock.ElapsedMilliseconds > timeoutMs)
                t.Fail($"{what} (waited {clock.ElapsedMilliseconds} ms)");

            await Task.Delay(20, t.Ct);
        }
    }
}
