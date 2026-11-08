using Uic.Config;
using Uic.Domain;

namespace Uic.Tests;

/// <summary>
/// 凭据不能以明文出现在配置里（FR-OPS-09）。
///
/// 这里测的是「读不出来时会不会静默降级」——令牌解析失败若退化成空串，
/// 鉴权就被悄悄关掉了，那是最危险的一种「通过」。所以缺环境变量、密钥不对、
/// 密文被改动都必须抛错，而不是返回 null。
/// </summary>
internal static class SecretTests
{
    private const string EnvName = "UIC_SECRET_TEST_TOKEN";
    private const string Plaintext = "s3cr3t-token";

    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("secret/env_reference_is_resolved", EnvReferenceIsResolved));
        tests.Add(new TestCase("secret/missing_env_fails_loudly", MissingEnvFailsLoudly));
        tests.Add(new TestCase("secret/file_reference_trims_line_ending", FileReferenceTrimsLineEnding));
        tests.Add(new TestCase("secret/plaintext_is_rejected", PlaintextIsRejected));
        tests.Add(new TestCase("secret/empty_secret_is_allowed", EmptySecretIsAllowed));
        tests.Add(new TestCase("secret/sealed_round_trip", SealedRoundTrip));
        tests.Add(new TestCase("secret/tampered_sealed_value_is_rejected", TamperedSealedValueIsRejected));
        tests.Add(new TestCase("secret/unknown_scheme_is_rejected", UnknownSchemeIsRejected));
        tests.Add(new TestCase("secret/describe_never_prints_the_secret", DescribeNeverPrintsTheSecret));
    }

    private static Task EnvReferenceIsResolved(TestContext t)
    {
        var doc = Doc("${env:" + EnvName + "}");

        var resolved = Secrets.Resolve(doc, Env(Plaintext));

        t.Equal(Plaintext, resolved.RestToken, "the environment value becomes the effective token");
        t.Equal(null, resolved.MqttPassword, "an unset password stays unset");

        // 引用本身不能被改写成明文：配置文档是所有导出/差异的来源。
        t.Equal("${env:" + EnvName + "}", doc.Northbound.Rest.Token, "the document keeps the reference");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 这条是安全用例：环境变量拼错时若返回空串，鉴权会静默关闭。
    /// </summary>
    private static Task MissingEnvFailsLoudly(TestContext t)
    {
        var doc = Doc("${env:" + EnvName + "}");

        var ex = t.Throws<ConfigException>(() => Secrets.Resolve(doc, Env(null)),
            "a missing environment variable fails instead of resolving to an empty token");

        t.Equal(1, ex.Problems.Count, "the problem is reported once");
        t.Equal("northbound.rest.token", ex.Problems[0].Path, "the field path is named");
        t.Contains(ex.Problems[0].Message, EnvName, "the message names the variable to set");

        return Task.CompletedTask;
    }

    private static Task FileReferenceTrimsLineEnding(TestContext t)
    {
        var path = t.TempFile("token");
        File.WriteAllText(path, Plaintext + "\r\n");

        var resolved = Secrets.Resolve(Doc("${file:" + path + "}"), Env(null));

        t.Equal(Plaintext, resolved.RestToken,
            "the line ending added by echo/editors is not part of the credential");

        return Task.CompletedTask;
    }

    private static Task PlaintextIsRejected(TestContext t)
    {
        var ex = t.Throws<ConfigException>(
            () => Secrets.Resolve(Doc(Plaintext), Env(null)),
            "a literal token is refused");

        t.Equal("northbound.rest.token", ex.Problems[0].Path, "the offending field is named");
        t.Contains(ex.Problems[0].Message, "plaintext", "the message explains why");
        t.Contains(ex.Problems[0].Message, "${env:", "the message suggests a reference");

        // 密码走同一条规则，不能只保护 token。
        var mqtt = new ConfigDocument
        {
            Site = "secret-site",
            Northbound = new NorthboundOptions
            {
                Mqtt = new MqttOptions { Username = "uic", Password = "hunter2" },
            },
        };

        var problems = Secrets.Validate(mqtt, Env(null));
        t.Equal("northbound.mqtt.password", problems[0].Path, "the MQTT password is checked too");

        return Task.CompletedTask;
    }

    private static Task EmptySecretIsAllowed(TestContext t)
    {
        var resolved = Secrets.Resolve(Doc(null), Env(null));

        t.Equal(null, resolved.RestToken, "no credential is a valid state (auth disabled, with a warning)");
        t.Equal(0, Secrets.Validate(Doc(null), Env(null)).Count, "an empty secret raises no problem");

        return Task.CompletedTask;
    }

    private static Task SealedRoundTrip(TestContext t)
    {
        var key = Secrets.GenerateKey();
        var keyText = Secrets.EncodeKey(key);

        var sealedValue = Secrets.Seal(Plaintext, key);

        t.Ok(sealedValue.StartsWith(Secrets.SealedPrefix, StringComparison.Ordinal),
            "the sealed value carries a version prefix");
        t.Ok(!sealedValue.Contains(Plaintext, StringComparison.Ordinal),
            "the ciphertext does not contain the plaintext");
        t.Ok(!string.Equals(sealedValue, Secrets.Seal(Plaintext, key), StringComparison.Ordinal),
            "each seal uses a fresh nonce");

        var resolved = Secrets.Resolve(Doc(sealedValue), Env(null, keyText));
        t.Equal(Plaintext, resolved.RestToken, "the sealed value decrypts back to the token");

        // 没有密钥时必须报出「缺密钥」，而不是把密文当口令用。
        var ex = t.Throws<ConfigException>(() => Secrets.Resolve(Doc(sealedValue), Env(null)),
            "a sealed secret without a key is refused");
        t.Contains(ex.Problems[0].Message, Secrets.KeyVariable, "the message names the key variable");

        return Task.CompletedTask;
    }

    private static Task TamperedSealedValueIsRejected(TestContext t)
    {
        var key = Secrets.GenerateKey();
        var sealedValue = Secrets.Seal(Plaintext, key);

        // 改一个字节的 base64：GCM 的认证标签必须拒绝它，而不是解出一段乱码。
        var payload = Convert.FromBase64String(sealedValue[Secrets.SealedPrefix.Length..]);
        payload[^1] ^= 0x01;
        var tampered = Secrets.SealedPrefix + Convert.ToBase64String(payload);

        var ex = t.Throws<ConfigException>(
            () => Secrets.Resolve(Doc(tampered), Env(null, Secrets.EncodeKey(key))),
            "a tampered ciphertext is refused");

        t.Contains(ex.Problems[0].Message, "cannot be decrypted", "the message says the value is unusable");

        return Task.CompletedTask;
    }

    private static Task UnknownSchemeIsRejected(TestContext t)
    {
        var unknown = Secrets.Validate(Doc("${vault:path/to/token}"), Env(null));
        t.Equal(1, unknown.Count, "an unknown scheme is one problem");
        t.Contains(unknown[0].Message, "unknown reference scheme", "the message explains the scheme is unknown");

        var malformed = Secrets.Validate(Doc("${env}"), Env(null));
        t.Equal(1, malformed.Count, "a reference without a scheme is one problem");
        t.Contains(malformed[0].Message, "malformed", "the message explains the expected shape");

        var version = Secrets.Validate(Doc("enc:v2:AAAA"), Env(null));
        t.Equal(1, version.Count, "a newer sealed version is not silently accepted");
        t.Contains(version[0].Message, "enc:v1", "the message names the supported version");

        return Task.CompletedTask;
    }

    private static Task DescribeNeverPrintsTheSecret(TestContext t)
    {
        var key = Secrets.GenerateKey();

        var lines = Secrets.Describe(new ConfigDocument
        {
            Site = "secret-site",
            Northbound = new NorthboundOptions
            {
                Rest = new RestOptions { Token = "${env:" + EnvName + "}" },
                Mqtt = new MqttOptions { Username = "uic", Password = Secrets.Seal("hunter2", key) },
            },
        });

        t.Equal(2, lines.Count, "both credential fields are described");
        t.Contains(lines[0], "northbound.rest.token: environment variable " + EnvName,
            "an environment reference is described without its value");
        t.Contains(lines[1], "northbound.mqtt.password: sealed", "a sealed value is described as sealed");

        foreach (var line in lines)
        {
            t.Ok(!line.Contains("hunter2", StringComparison.Ordinal) &&
                 !line.Contains(Plaintext, StringComparison.Ordinal),
                "no line contains a credential: " + line);
        }

        return Task.CompletedTask;
    }

    // ------------------------------------------------------------------
    // 夹具
    // ------------------------------------------------------------------

    private static ConfigDocument Doc(string? token) => new()
    {
        Site = "secret-site",
        Northbound = new NorthboundOptions
        {
            Rest = new RestOptions { Token = token },
        },
    };

    /// <summary>把进程环境换成一个只有 <see cref="EnvName"/> 的假世界。</summary>
    private static SecretLookup Env(string? value, string? keyText = null) => new()
    {
        Environment = name => string.Equals(name, EnvName, StringComparison.Ordinal) ? value : null,
        ReadFile = path => File.Exists(path) ? File.ReadAllText(path) : null,
        KeyText = () => keyText,
    };
}
