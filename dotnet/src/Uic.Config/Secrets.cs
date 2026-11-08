using System.Security.Cryptography;
using System.Text;
using System.Text.RegularExpressions;
using Uic.Domain;

namespace Uic.Config;

/// <summary>解析后的凭据。只有真正需要明文的地方（鉴权、MQTT 握手）才用得到它。</summary>
public sealed record ResolvedSecrets(string? RestToken, string? MqttPassword)
{
    public static readonly ResolvedSecrets Empty = new(null, null);
}

/// <summary>
/// 解析凭据引用所需的外部世界。默认实现读进程环境变量与文件系统；
/// 用例注入自己的实现，避免为了测「环境变量缺失」去改进程的全局状态。
/// </summary>
public sealed class SecretLookup
{
    public static readonly SecretLookup Default = new();

    public Func<string, string?> Environment { get; init; } = System.Environment.GetEnvironmentVariable;

    /// <summary>读密钥文件；不存在或不可读返回 null。</summary>
    public Func<string, string?> ReadFile { get; init; } = DefaultReadFile;

    /// <summary>密封密钥原文（base64 / hex）；没有配置返回 null。</summary>
    public Func<string?> KeyText { get; init; } = DefaultKeyText;

    private static string? DefaultReadFile(string path)
    {
        try
        {
            return File.Exists(path) ? File.ReadAllText(path) : null;
        }
        catch (IOException)
        {
            return null;
        }
        catch (UnauthorizedAccessException)
        {
            return null;
        }
    }

    private static string? DefaultKeyText()
    {
        var inline = System.Environment.GetEnvironmentVariable(Secrets.KeyVariable);
        if (!string.IsNullOrWhiteSpace(inline)) return inline;

        var path = System.Environment.GetEnvironmentVariable(Secrets.KeyFileVariable);
        if (string.IsNullOrWhiteSpace(path)) return null;

        return DefaultReadFile(path);
    }
}

/// <summary>
/// 配置里的凭据不能是明文。
///
/// 支持三种写法：
/// <list type="bullet">
/// <item><c>${env:NAME}</c> — 从环境变量读取；compose / systemd / K8s 都能这样注入；</item>
/// <item><c>${file:/run/secrets/x}</c> — 从文件读取（Docker secret 的挂载方式），去掉行尾换行；</item>
/// <item><c>enc:v1:...</c> — AES-256-GCM 密文，密钥来自 <c>UIC_SECRET_KEY</c> 或
///   <c>UIC_SECRET_KEY_FILE</c>，用于「配置文件本身要能拷走」的场景。</item>
/// </list>
///
/// 为什么解析失败必须硬失败：令牌取不到时若退化成空串，鉴权会**静默关闭**——
/// 那是最坏的一种失败。宁可起不来。
/// </summary>
public static partial class Secrets
{
    public const string SealedPrefix = "enc:v1:";
    public const string KeyVariable = "UIC_SECRET_KEY";
    public const string KeyFileVariable = "UIC_SECRET_KEY_FILE";

    /// <summary>AAD 固定成常量：密文不允许被挪到别的字段上复用。</summary>
    private static readonly byte[] Aad = Encoding.UTF8.GetBytes("uic:secret:v1");

    [GeneratedRegex("^[A-Za-z_][A-Za-z0-9_]*$")]
    private static partial Regex EnvName();

    public static bool IsReference(string? value)
        => value is not null &&
           (value.StartsWith("${", StringComparison.Ordinal) ||
            value.StartsWith(SealedPrefix, StringComparison.Ordinal));

    /// <summary>
    /// 校验并解析全部凭据字段。任何一条不可用都抛异常——错误信息带字段路径，
    /// 现场能直接改对地方。
    /// </summary>
    public static ResolvedSecrets Resolve(ConfigDocument doc, SecretLookup? lookup = null)
    {
        lookup ??= SecretLookup.Default;

        var problems = Validate(doc, lookup);
        if (problems.Count > 0)
            throw new ConfigException($"credentials are not usable ({problems.Count} problem(s))", problems);

        return new ResolvedSecrets(
            ResolveValue(doc.Northbound.Rest.Token, "northbound.rest.token", lookup),
            ResolveValue(doc.Northbound.Mqtt.Password, "northbound.mqtt.password", lookup));
    }

    /// <summary>只做校验，返回问题清单（配置校验端点与 <c>uic check</c> 用）。</summary>
    public static IReadOnlyList<ValidationProblem> Validate(ConfigDocument doc, SecretLookup? lookup = null)
    {
        lookup ??= SecretLookup.Default;

        var problems = new List<ValidationProblem>();

        Check("northbound.rest.token", doc.Northbound.Rest.Token, problems, lookup);
        Check("northbound.mqtt.password", doc.Northbound.Mqtt.Password, problems, lookup);

        return problems;
    }

    /// <summary>凭据字段的人类可读摘要，供 <c>uic check</c> 打印（绝不输出明文）。</summary>
    public static IReadOnlyList<string> Describe(ConfigDocument doc)
    {
        var lines = new List<string>();

        void Add(string path, string? value)
        {
            if (string.IsNullOrEmpty(value))
            {
                lines.Add($"{path}: not set");
                return;
            }

            if (value.StartsWith("${env:", StringComparison.Ordinal))
                lines.Add($"{path}: environment variable {value[6..^1]}");
            else if (value.StartsWith("${file:", StringComparison.Ordinal))
                lines.Add($"{path}: file {value[7..^1]}");
            else if (value.StartsWith(SealedPrefix, StringComparison.Ordinal))
                lines.Add($"{path}: sealed (enc:v1)");
            else
                lines.Add($"{path}: PLAINTEXT");
        }

        Add("northbound.rest.token", doc.Northbound.Rest.Token);
        Add("northbound.mqtt.password", doc.Northbound.Mqtt.Password);

        return lines;
    }

    /// <summary>把明文封装成 <c>enc:v1:</c> 密文。密钥长度必须是 32 字节。</summary>
    public static string Seal(string plaintext, byte[] key)
    {
        if (key.Length != 32) throw new ArgumentException("the key must be 32 bytes (AES-256)", nameof(key));

        var plain = Encoding.UTF8.GetBytes(plaintext);
        var nonce = RandomNumberGenerator.GetBytes(12);
        var tag = new byte[16];
        var cipher = new byte[plain.Length];

        using (var aes = new AesGcm(key, tag.Length))
        {
            aes.Encrypt(nonce, plain, cipher, tag, Aad);
        }

        var payload = new byte[nonce.Length + tag.Length + cipher.Length];
        nonce.CopyTo(payload, 0);
        tag.CopyTo(payload, nonce.Length);
        cipher.CopyTo(payload, nonce.Length + tag.Length);

        return SealedPrefix + Convert.ToBase64String(payload);
    }

    /// <summary>解封。失败时返回 false 并给出可操作的原因，绝不返回半截明文。</summary>
    public static bool TryOpen(
        string reference,
        SecretLookup? lookup,
        out string plaintext,
        out string? error)
    {
        lookup ??= SecretLookup.Default;
        plaintext = string.Empty;

        if (!reference.StartsWith(SealedPrefix, StringComparison.Ordinal))
        {
            error = "not a sealed secret";
            return false;
        }

        if (!TryParseKey(lookup.KeyText(), out var key, out var keyError))
        {
            error = keyError;
            return false;
        }

        byte[] payload;
        try
        {
            payload = Convert.FromBase64String(reference[SealedPrefix.Length..]);
        }
        catch (FormatException)
        {
            error = "sealed secret is not valid base64";
            return false;
        }

        if (payload.Length < 12 + 16)
        {
            error = "sealed secret is truncated";
            return false;
        }

        var nonce = payload.AsSpan(0, 12);
        var tag = payload.AsSpan(12, 16);
        var cipher = payload.AsSpan(28);
        var plain = new byte[cipher.Length];

        try
        {
            using var aes = new AesGcm(key, 16);
            aes.Decrypt(nonce, cipher, tag, plain, Aad);
        }
        catch (CryptographicException)
        {
            error = "sealed secret cannot be decrypted with the configured key " +
                    "(wrong key, or the value was tampered with)";
            return false;
        }

        plaintext = Encoding.UTF8.GetString(plain);
        error = null;
        return true;
    }

    /// <summary>解析 base64（44 字符）或 hex（64 字符）形式的 32 字节密钥。</summary>
    public static bool TryParseKey(string? text, out byte[] key, out string? error)
    {
        key = Array.Empty<byte>();

        if (string.IsNullOrWhiteSpace(text))
        {
            error = $"a sealed secret needs a key: set {KeyVariable} (base64) or {KeyFileVariable} (path)";
            return false;
        }

        var trimmed = text.Trim();

        byte[] bytes;
        try
        {
            bytes = trimmed.Length == 64 && IsHex(trimmed)
                ? Convert.FromHexString(trimmed)
                : Convert.FromBase64String(trimmed);
        }
        catch (FormatException)
        {
            error = $"{KeyVariable} is neither base64 nor hex";
            return false;
        }

        if (bytes.Length != 32)
        {
            error = $"{KeyVariable} must decode to 32 bytes (AES-256), got {bytes.Length}";
            return false;
        }

        key = bytes;
        error = null;
        return true;
    }

    public static byte[] GenerateKey() => RandomNumberGenerator.GetBytes(32);

    public static string EncodeKey(byte[] key) => Convert.ToBase64String(key);

    private static bool IsHex(string text)
        => text.All(c => c is >= '0' and <= '9' or >= 'a' and <= 'f' or >= 'A' and <= 'F');

    private static void Check(
        string path,
        string? value,
        List<ValidationProblem> problems,
        SecretLookup lookup)
    {
        // 空 = 未设置。端口上没有凭据比「有但读不出来」安全。
        if (string.IsNullOrEmpty(value)) return;

        if (value.StartsWith("${", StringComparison.Ordinal))
        {
            if (!value.EndsWith('}') || value.Length < 6)
            {
                problems.Add(new ValidationProblem(path, "malformed reference; expected ${env:NAME} or ${file:PATH}"));
                return;
            }

            var body = value[2..^1];
            var colon = body.IndexOf(':');
            if (colon <= 0)
            {
                problems.Add(new ValidationProblem(path, "malformed reference; expected ${env:NAME} or ${file:PATH}"));
                return;
            }

            var scheme = body[..colon];
            var argument = body[(colon + 1)..];

            switch (scheme)
            {
                case "env":
                    if (!EnvName().IsMatch(argument))
                    {
                        problems.Add(new ValidationProblem(path,
                            $"environment variable name '{argument}' is invalid"));
                        return;
                    }

                    if (lookup.Environment(argument) is null)
                        problems.Add(new ValidationProblem(path,
                            $"environment variable '{argument}' is not set"));
                    return;

                case "file":
                    if (argument.Length == 0)
                    {
                        problems.Add(new ValidationProblem(path, "file reference needs a path"));
                        return;
                    }

                    if (lookup.ReadFile(argument) is null)
                        problems.Add(new ValidationProblem(path,
                            $"secret file '{argument}' is missing or unreadable"));
                    return;

                default:
                    problems.Add(new ValidationProblem(path,
                        $"unknown reference scheme '{scheme}'; use env or file"));
                    return;
            }
        }

        if (value.StartsWith("enc:", StringComparison.Ordinal))
        {
            if (!value.StartsWith(SealedPrefix, StringComparison.Ordinal))
            {
                problems.Add(new ValidationProblem(path,
                    "unsupported sealed secret version; this build understands enc:v1"));
                return;
            }

            if (!TryOpen(value, lookup, out _, out var error))
                problems.Add(new ValidationProblem(path, error!));
            return;
        }

        problems.Add(new ValidationProblem(path,
            "plaintext credentials are not allowed; use ${env:NAME}, ${file:PATH} or enc:v1:... " +
            "(see `uic secret seal`)"));
    }

    private static string? ResolveValue(string? value, string path, SecretLookup lookup)
    {
        if (string.IsNullOrEmpty(value)) return null;

        if (value.StartsWith("${env:", StringComparison.Ordinal))
            return lookup.Environment(value[6..^1]) ?? value;

        if (value.StartsWith("${file:", StringComparison.Ordinal))
        {
            var raw = lookup.ReadFile(value[7..^1]);
            // 密钥文件末尾的换行是编辑器与 echo 的副产物，不是口令的一部分。
            return raw?.TrimEnd('\r', '\n');
        }

        if (value.StartsWith(SealedPrefix, StringComparison.Ordinal))
            return TryOpen(value, lookup, out var plaintext, out _) ? plaintext : value;

        // 不该走到这里：Validate 已经拒绝明文。保留原文返回，让调用方看到真实值，
        // 而不是一个悄悄变成 null 的凭据。
        _ = path;
        return value;
    }
}
