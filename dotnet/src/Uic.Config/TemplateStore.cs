using System.Text;
using System.Text.Json;
using Uic.Domain;

namespace Uic.Config;

/// <summary>
/// 模板库：<c>{dataDir}/templates/*.json</c>。
///
/// 模板与实例配置分开存储（域模型 §11 的第 4 条规则）：
/// 模板文件变更不会重写已经应用的设备配置，避免「改一个默认值，现场 50 台设备一起变」。
/// </summary>
public sealed class TemplateStore
{
    private readonly string _dir;
    private readonly SemaphoreSlim _lock = new(1, 1);

    public TemplateStore(string dataDir)
    {
        _dir = Path.Combine(dataDir, "templates");
        Directory.CreateDirectory(_dir);
    }

    public string Root => _dir;

    public IReadOnlyList<DeviceTemplate> List()
    {
        var list = new List<DeviceTemplate>();

        if (!Directory.Exists(_dir)) return list;

        foreach (var file in Directory.EnumerateFiles(_dir, "*.json").OrderBy(f => f, StringComparer.Ordinal))
        {
            try
            {
                var template = JsonSerializer.Deserialize<DeviceTemplate>(
                    File.ReadAllText(file), JsonDefaults.Options);

                if (template is not null) list.Add(template);
            }
            catch (JsonException)
            {
                // 单个模板文件坏了不能拖垮整个列表：控制台仍然要能打开，
                // 由 save 校验阻止坏文件继续产生。
            }
        }

        return list;
    }

    public DeviceTemplate? Get(string id)
        => List().FirstOrDefault(t => string.Equals(t.Id, id, StringComparison.Ordinal));

    public async Task<DeviceTemplate> SaveAsync(DeviceTemplate template, CancellationToken ct)
    {
        var problems = Validate(template);
        if (problems.Count > 0)
            throw new ConfigException($"template '{template.Id}' is invalid", problems);

        await _lock.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            var path = Path.Combine(_dir, template.Id + ".json");
            AtomicWrite(path, Encoding.UTF8.GetBytes(
                JsonSerializer.Serialize(template, JsonDefaults.Indented) + "\n"));
            return template;
        }
        finally
        {
            _lock.Release();
        }
    }

    public async Task<bool> DeleteAsync(string id, CancellationToken ct)
    {
        await _lock.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            var path = Path.Combine(_dir, id + ".json");
            if (!File.Exists(path)) return false;
            File.Delete(path);
            return true;
        }
        finally
        {
            _lock.Release();
        }
    }

    public static IReadOnlyList<ValidationProblem> Validate(DeviceTemplate template)
    {
        var problems = new List<ValidationProblem>();
        void Add(string path, string message) => problems.Add(new ValidationProblem(path, message));

        if (template.SchemaVersion != 1)
            Add("schemaVersion", $"unsupported schema version {template.SchemaVersion}");

        if (string.IsNullOrWhiteSpace(template.Id) ||
            !System.Text.RegularExpressions.Regex.IsMatch(template.Id, "^[a-z0-9][a-z0-9._-]{0,63}$"))
        {
            Add("id", "must match ^[a-z0-9][a-z0-9._-]{0,63}$");
        }

        if (template.Device is null)
        {
            Add("device", "required");
            return problems;
        }

        var declared = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        foreach (var parameter in template.Parameters)
        {
            if (string.IsNullOrWhiteSpace(parameter.Name) ||
                !System.Text.RegularExpressions.Regex.IsMatch(parameter.Name, "^[a-z][a-z0-9_.-]{0,31}$"))
            {
                Add("parameters", $"parameter name '{parameter.Name}' is invalid");
                continue;
            }

            if (!declared.Add(parameter.Name))
                Add($"parameters.{parameter.Name}", "duplicate parameter name");

            var type = parameter.Type.ToLowerInvariant();
            if (type is not ("string" or "int" or "bool"))
                Add($"parameters.{parameter.Name}.type", "type must be string, int or bool");

            if (parameter.Required && string.IsNullOrWhiteSpace(parameter.Default))
            {
                // required 且有默认值并不矛盾，但两个同时出现通常意味着作者写错了意图。
                // 这里只提示，不阻断：默认值仍然会被 required 校验优先使用。
            }

            if (!string.IsNullOrWhiteSpace(parameter.Default) && type is "int" or "bool")
            {
                var ok = type == "int"
                    ? long.TryParse(parameter.Default, out _)
                    : bool.TryParse(parameter.Default, out _);

                if (!ok) Add($"parameters.{parameter.Name}.default", $"default is not a valid {type}");
            }
        }

        foreach (var used in TemplateInstantiator.CollectPlaceholders(template))
        {
            if (!declared.Contains(used))
                Add("device", $"placeholder '{{{used}}}' is not declared in parameters");
        }

        // 设备骨架里可能有 <c>"{base}"</c> 这类占位符，直接反序列化会因类型不匹配失败。
        // 用每个参数类型的样例值实例化一次，才是「骨架本身能否变成合法设备」的正确检查。
        var sample = new Dictionary<string, string?>(StringComparer.OrdinalIgnoreCase);
        foreach (var parameter in template.Parameters)
        {
            if (string.IsNullOrWhiteSpace(parameter.Name)) continue;

            sample[parameter.Name] = parameter.Type.ToLowerInvariant() switch
            {
                "int" => "0",
                "bool" => "false",
                _ => "x",
            };
        }

        var probe = TemplateInstantiator.Instantiate(template, sample, Array.Empty<string>());
        if (!probe.Ok)
        {
            foreach (var problem in probe.Problems)
                Add("device" + (problem.Path == "device" ? string.Empty : "." + problem.Path),
                    problem.Message);
        }

        return problems;
    }

    private static void AtomicWrite(string path, byte[] bytes)
    {
        var temp = path + ".tmp-" + Guid.NewGuid().ToString("N")[..8];

        using (var stream = new FileStream(temp, FileMode.Create, FileAccess.Write, FileShare.None))
        {
            stream.Write(bytes, 0, bytes.Length);
            stream.Flush(flushToDisk: true);
        }

        File.Move(temp, path, overwrite: true);
    }
}
