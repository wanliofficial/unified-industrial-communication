using System.Text.Json;
using System.Text.Json.Nodes;
using Uic.Domain;

namespace Uic.Config;

/// <summary>
/// 脱敏占位符的还原。
///
/// <c>GET /api/v1/config</c> 与默认导出会把凭据字段（见 <see cref="SecretKeys"/>：
/// <c>token</c>/<c>password</c>/<c>secret</c>/<c>certificatePassword</c>）
/// 替换成 <c>"***"</c>。客户端把读到的配置原样改一处再提交时，占位符必须还原成
/// 当前值——否则一次与安全无关的编辑（比如加一台设备）会把 API 令牌改成字面量
/// <c>"***"</c>，等于把所有人锁在门外。
///
/// 只在「提交值与当前值同路径且提交值是占位符」时还原；其它情况一律以提交值为准，
/// 这样显式清空口令（提交空串）仍然有效。
/// </summary>
public static class RedactedSecrets
{
    public const string Placeholder = "***";

    /// <summary>
    /// 凭据字段的唯一清单。**读侧脱敏与写侧还原必须共用它**。
    ///
    /// 两份清单分开维护过一次，代价是：读接口漏了 <c>certificatePassword</c>
    /// （key 名是组合词，不是字面 <c>password</c>），PFX 口令原样出现在
    /// <c>GET /api/v1/config</c> 里；而因为还原侧同样漏了它，读到明文再原样提交
    /// 也不会触发任何「占位符」分支——明文就被直接写进配置版本。
    /// 两侧都漏等于没有任何一处报错，这类缺陷只能靠「清单只有一份」来防。
    ///
    /// 新增凭据字段时同时改这里和 <see cref="Secrets"/> 的三处调用；
    /// 用例 <c>contract/every_credential_field_is_redacted</c> 会盯着读侧。
    /// </summary>
    private static readonly HashSet<string> SecretKeys = new(StringComparer.Ordinal)
    {
        "token", "password", "secret", "certificatePassword",
    };

    /// <summary>这个 key 名是否是凭据。脱敏侧按 key 名判定，与还原侧共用同一份清单。</summary>
    public static bool IsSecretKey(string key) => SecretKeys.Contains(key);

    /// <summary>
    /// 返回还原后的 JSON 文本。当前配置未知、提交的是非法 JSON 或根不是对象时
    /// 原样返回，让后续的解析与校验给出准确错误。
    /// </summary>
    public static string Restore(string candidateJson, ConfigDocument? current)
    {
        if (current is null) return candidateJson;

        JsonNode? candidate;
        try
        {
            // 与配置迁移链的解析口径保持一致：允许注释与尾逗号，避免「能升级但不能还密钥」。
            candidate = JsonNode.Parse(candidateJson, documentOptions: new JsonDocumentOptions
            {
                CommentHandling = JsonCommentHandling.Skip,
                AllowTrailingCommas = true,
            });
        }
        catch (JsonException)
        {
            return candidateJson;
        }

        if (candidate is not JsonObject candidateRoot) return candidateJson;

        var currentRoot = JsonNode.Parse(JsonSerializer.Serialize(current, JsonDefaults.Options)) as JsonObject;
        if (currentRoot is null) return candidateJson;

        RestoreNode(candidateRoot, currentRoot);

        return candidateRoot.ToJsonString(JsonDefaults.Indented);
    }

    /// <summary>
    /// 差异路径是否指向凭据字段。路径来自 <see cref="ConfigStore.Diff"/>，
    /// 形如 <c>northbound.rest.token</c> 或 <c>channels[0].secret</c>。
    /// </summary>
    public static bool IsSecretPath(string path)
    {
        var lastDot = path.LastIndexOf('.');
        var leaf = lastDot >= 0 ? path[(lastDot + 1)..] : path;

        var bracket = leaf.IndexOf('[');
        if (bracket >= 0) leaf = leaf[..bracket];

        return SecretKeys.Contains(leaf);
    }

    /// <summary>差异值里的凭据同样要脱敏；结构信息（路径与变更类型）保持可见。</summary>
    public static string? MaskIfSecret(string path, string? value)
        => value is not null && IsSecretPath(path) ? Placeholder : value;

    private static void RestoreNode(JsonNode? candidate, JsonNode? current)
    {
        switch (candidate, current)
        {
            case (JsonObject submitted, JsonObject existing):
                foreach (var key in submitted.Select(kv => kv.Key).ToArray())
                {
                    if (SecretKeys.Contains(key) && IsPlaceholder(submitted[key]))
                    {
                        // 当前值不存在（比如首次配置口令）时保持占位符：
                        // 这里没有可以还原的东西，交给上层去拒绝或接受。
                        if (existing[key] is not null) submitted[key] = existing[key]!.DeepClone();
                        continue;
                    }

                    RestoreNode(submitted[key], existing[key]);
                }
                break;

            case (JsonArray submitted, JsonArray existing):
                // 配置里的数组通常带 id，但按位置对齐已经足够：
                // 当前密钥只出现在标量对象（northbound.rest / northbound.mqtt）里。
                for (var i = 0; i < submitted.Count && i < existing.Count; i++)
                {
                    RestoreNode(submitted[i], existing[i]);
                }
                break;
        }
    }

    private static bool IsPlaceholder(JsonNode? node)
        => node is JsonValue value &&
           value.TryGetValue<string>(out var text) &&
           string.Equals(text, Placeholder, StringComparison.Ordinal);
}
