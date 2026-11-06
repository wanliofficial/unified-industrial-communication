using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace Uic.Domain;

/// <summary>模板参数。类型只有 string/int/bool 三种：够用，且不会把配置变成脚本。</summary>
public sealed class TemplateParameter
{
    public required string Name { get; init; }
    public string? Label { get; init; }

    /// <summary>string | int | bool</summary>
    public string Type { get; init; } = "string";

    public bool Required { get; init; }
    public string? Default { get; init; }
    public string? Description { get; init; }
}

/// <summary>
/// 设备模板：一个可参数化的设备骨架。
///
/// 模板里保存的是**原始 JSON** 而不是 <see cref="Device"/>：
/// 参数替换发生在字面量上（例如 offset 写成 <c>"{base}"</c>），
/// 先反序列化再替换会丢掉「这里是占位符」的信息，也没法在类型不匹配时给出准确定位。
/// </summary>
public sealed class DeviceTemplate
{
    public int SchemaVersion { get; init; } = 1;
    public required string Id { get; init; }
    public string? Name { get; init; }
    public string? Description { get; init; }
    public string? Protocol { get; init; }
    public List<TemplateParameter> Parameters { get; init; } = new();
    public required JsonObject Device { get; init; }
}

/// <summary>一次实例化的结果。失败时 <see cref="Device"/> 为 null，problems 至少一条。</summary>
public sealed record TemplateInstantiation(
    Device? Device,
    IReadOnlyList<ValidationProblem> Problems)
{
    public bool Ok => Device is not null && Problems.Count == 0;
}

/// <summary>
/// 模板实例化器。规则刻意保守：
/// 1. 只替换字符串字面量里的 <c>{name}</c>，不执行任何表达式；
/// 2. 整串就是一个占位符且参数类型是 int/bool 时，产出真正的 JSON number/bool；
/// 3. 实例化结果必须通过完整配置校验，等价于手工写的设备。
/// </summary>
public static partial class TemplateInstantiator
{
    [GeneratedRegex(@"\{([a-z][a-z0-9_.-]*)\}", RegexOptions.IgnoreCase)]
    private static partial Regex PlaceholderRe();

    private static readonly Regex TemplateIdRe = new("^[a-z0-9][a-z0-9._-]{0,63}$", RegexOptions.Compiled);

    public static TemplateInstantiation Instantiate(
        DeviceTemplate template,
        IReadOnlyDictionary<string, string?> supplied,
        IReadOnlyList<string> knownChannels)
    {
        var problems = new List<ValidationProblem>();

        if (template.SchemaVersion != 1)
        {
            problems.Add(new ValidationProblem("schemaVersion",
                $"unsupported template schema version {template.SchemaVersion}"));
            return new TemplateInstantiation(null, problems);
        }

        if (!TemplateIdRe.IsMatch(template.Id))
            problems.Add(new ValidationProblem("id", "template id must match ^[a-z0-9][a-z0-9._-]{0,63}$"));

        var values = new Dictionary<string, TemplateValue>(StringComparer.OrdinalIgnoreCase);

        foreach (var parameter in template.Parameters)
        {
            var name = parameter.Name;
            var type = parameter.Type.ToLowerInvariant();

            if (type is not ("string" or "int" or "bool"))
            {
                problems.Add(new ValidationProblem($"parameters.{name}.type",
                    "type must be string, int or bool"));
                continue;
            }

            if (!supplied.TryGetValue(name, out var raw) || string.IsNullOrWhiteSpace(raw))
                raw = parameter.Default;

            if (string.IsNullOrWhiteSpace(raw))
            {
                if (parameter.Required)
                    problems.Add(new ValidationProblem($"parameters.{name}", "required parameter is missing"));
                else
                    values[name] = new TemplateValue(type, null);
                continue;
            }

            switch (type)
            {
                case "int":
                    if (!long.TryParse(raw, System.Globalization.NumberStyles.Integer,
                            System.Globalization.CultureInfo.InvariantCulture, out var number))
                    {
                        problems.Add(new ValidationProblem($"parameters.{name}",
                            $"'{raw}' is not an integer"));
                        continue;
                    }
                    values[name] = new TemplateValue(type, raw) { Number = number };
                    break;

                case "bool":
                    if (!bool.TryParse(raw, out var flag))
                    {
                        problems.Add(new ValidationProblem($"parameters.{name}",
                            $"'{raw}' is not a boolean (true/false)"));
                        continue;
                    }
                    values[name] = new TemplateValue(type, raw) { Flag = flag };
                    break;

                default:
                    values[name] = new TemplateValue(type, raw);
                    break;
            }
        }

        foreach (var key in supplied.Keys)
        {
            if (template.Parameters.Any(p => string.Equals(p.Name, key, StringComparison.OrdinalIgnoreCase)))
                continue;

            problems.Add(new ValidationProblem($"parameters.{key}",
                "unknown parameter; check the template definition"));
        }

        if (problems.Count > 0) return new TemplateInstantiation(null, problems);

        var node = template.Device.DeepClone();
        Substitute(node, values, problems, path: "device");

        if (problems.Count > 0) return new TemplateInstantiation(null, problems);

        Device? device;
        try
        {
            device = node.Deserialize<Device>(JsonDefaults.Options);
        }
        catch (JsonException ex)
        {
            problems.Add(new ValidationProblem("device", $"instantiated device is not valid: {ex.Message}"));
            return new TemplateInstantiation(null, problems);
        }

        if (device is null)
        {
            problems.Add(new ValidationProblem("device", "instantiated device is null"));
            return new TemplateInstantiation(null, problems);
        }

        // 通道必须真实存在；否则会生成一台「看起来合法但永远连不上」的设备。
        if (knownChannels.Count > 0 && !knownChannels.Contains(device.ChannelId, StringComparer.Ordinal))
        {
            problems.Add(new ValidationProblem("device.channel",
                $"channel '{device.ChannelId}' does not exist"));
        }

        return new TemplateInstantiation(device, problems);
    }

    /// <summary>模板里出现的全部占位符名（去重）。保存模板时据此检查「用了没声明的参数」。</summary>
    public static IReadOnlyList<string> CollectPlaceholders(DeviceTemplate template)
    {
        var found = new SortedSet<string>(StringComparer.OrdinalIgnoreCase);
        Walk(template.Device);
        return found.ToArray();

        void Walk(JsonNode? node)
        {
            switch (node)
            {
                case JsonObject obj:
                    foreach (var (_, value) in obj)
                    {
                        if (value is JsonValue v && v.TryGetValue<string>(out var text))
                        {
                            foreach (Match match in PlaceholderRe().Matches(text))
                                found.Add(match.Groups[1].Value);
                        }
                        else
                        {
                            Walk(value);
                        }
                    }
                    break;

                case JsonArray array:
                    foreach (var item in array)
                    {
                        if (item is JsonValue v && v.TryGetValue<string>(out var text))
                        {
                            foreach (Match match in PlaceholderRe().Matches(text))
                                found.Add(match.Groups[1].Value);
                        }
                        else
                        {
                            Walk(item);
                        }
                    }
                    break;
            }
        }
    }

    private static void Substitute(
        JsonNode? node,
        IReadOnlyDictionary<string, TemplateValue> values,
        List<ValidationProblem> problems,
        string path)
    {
        switch (node)
        {
            case JsonObject obj:
                foreach (var key in obj.Select(kv => kv.Key).ToArray())
                {
                    var child = obj[key];
                    if (child is JsonValue childValue && childValue.TryGetValue<string>(out var text))
                    {
                        obj[key] = Render(text, values, problems, $"{path}.{key}");
                    }
                    else
                    {
                        Substitute(child, values, problems, $"{path}.{key}");
                    }
                }
                break;

            case JsonArray array:
                for (var i = 0; i < array.Count; i++)
                {
                    var child = array[i];
                    if (child is JsonValue childValue && childValue.TryGetValue<string>(out var text))
                    {
                        array[i] = Render(text, values, problems, $"{path}[{i}]");
                    }
                    else
                    {
                        Substitute(child, values, problems, $"{path}[{i}]");
                    }
                }
                break;
        }
    }

    private static JsonNode? Render(
        string text,
        IReadOnlyDictionary<string, TemplateValue> values,
        List<ValidationProblem> problems,
        string path)
    {
        // 整串是一个占位符：保留参数的原生类型，而不是全部变成字符串。
        var whole = PlaceholderRe().Match(text);
        if (whole.Success && whole.Length == text.Length)
        {
            var name = whole.Groups[1].Value;
            if (!values.TryGetValue(name, out var value))
            {
                problems.Add(new ValidationProblem(path, $"unknown placeholder '{{{name}}}'"));
                return JsonValue.Create(text);
            }

            if (value.Text is null)
            {
                problems.Add(new ValidationProblem(path, $"parameter '{name}' has no value"));
                return JsonValue.Create(text);
            }

            return value.Type switch
            {
                "int" => JsonValue.Create(value.Number),
                "bool" => JsonValue.Create(value.Flag),
                _ => JsonValue.Create(value.Text),
            };
        }

        var rendered = PlaceholderRe().Replace(text, match =>
        {
            var name = match.Groups[1].Value;
            if (!values.TryGetValue(name, out var value))
            {
                problems.Add(new ValidationProblem(path, $"unknown placeholder '{{{name}}}'"));
                return match.Value;
            }

            if (value.Text is null)
            {
                problems.Add(new ValidationProblem(path, $"parameter '{name}' has no value"));
                return match.Value;
            }

            return value.Text;
        });

        return JsonValue.Create(rendered);
    }

    private sealed class TemplateValue
    {
        public TemplateValue(string type, string? text)
        {
            Type = type;
            Text = text;
        }

        public string Type { get; }
        public string? Text { get; }
        public long Number { get; init; }
        public bool Flag { get; init; }
    }
}
